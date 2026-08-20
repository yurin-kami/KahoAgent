// Package tokenizer 提供中文分词，供 BM25 全文检索使用。
package tokenizer

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yanyiwu/gojieba"
)

// dictDirEnv 指定词典目录，用于容器等「源码不在运行环境里」的场景。
//
// ⚠️ 这个开关是被迫加的，不是为了灵活性。
// gojieba 的默认词典路径是 init() 里用 runtime.Caller 算出来的
// **编译机源码路径**（$GOMODCACHE/github.com/yanyiwu/gojieba@v1.4.7/deps/...）。
// 多阶段构建的运行镜像里没有 Go module 缓存，于是一调用 NewJieba 就 panic：
//
//	panic: Dictionary file does not exist: github.com/yanyiwu/gojieba@.../jieba.dict.utf8
//
// 更隐蔽的是：加了 -trimpath 之后这个路径会变成相对路径，
// 报错信息看起来像「词典文件缺失」，而实际原因是路径基准整个错了。
// 编译期完全无法发现 —— 只有真正启动容器才会暴露。
const dictDirEnv = "KAHO_JIEBA_DICT_DIR"

// Jieba 封装 gojieba 做中英文混合分词。
//
// 为什么需要它：PostgreSQL 自带的分词器不认识中文，
// 「Redis缓存穿透」会被当成一个不可分割的词元，
// 于是查「缓存穿透」完全匹配不上 —— BM25 那一路直接失效。
//
// 解法（IMPLEMENTATION.md L2.2 选定）：在 Go 侧用 gojieba 切好词，
// 存成空格分隔的字符串，PG 用 'simple' 分词器按空格切即可。
// 相比装 pg_jieba 扩展，省掉了编译安装 PG 插件的麻烦。
//
// ⚠️ gojieba 基于 cgo，且 NewJieba 会加载约 100MB 词典 ——
// 必须复用同一个实例，不能每次调用都新建。
type Jieba struct {
	seg *gojieba.Jieba
}

var (
	shared     *Jieba
	sharedOnce sync.Once
)

// Shared 返回进程内共享的分词器。
//
// 用单例是因为词典加载昂贵（约 100MB 内存 + 秒级耗时）。
// gojieba 的分词方法本身是并发安全的，可以多 goroutine 共用。
//
// 若设置了 KAHO_JIEBA_DICT_DIR，改从该目录加载词典（见 dictDirEnv 说明）。
func Shared() *Jieba {
	sharedOnce.Do(func() {
		shared = &Jieba{seg: newSegmenter()}
	})
	return shared
}

// newSegmenter 按需切换词典目录。
//
// 传空串给 gojieba 表示「用该项默认值」，所以未设环境变量时
// 传五个空串即可完全保持原行为 —— 不给本地开发增加任何配置负担。
func newSegmenter() *gojieba.Jieba {
	dir := os.Getenv(dictDirEnv)
	if dir == "" {
		return gojieba.NewJieba()
	}
	// 顺序固定：dict / hmm / user / idf / stop_words，与 gojieba 的入参一致。
	// 写错顺序不会报错，只会让分词结果悄悄劣化 —— 所以这里不做「智能匹配」。
	return gojieba.NewJieba(
		filepath.Join(dir, "jieba.dict.utf8"),
		filepath.Join(dir, "hmm_model.utf8"),
		filepath.Join(dir, "user.dict.utf8"),
		filepath.Join(dir, "idf.utf8"),
		filepath.Join(dir, "stop_words.utf8"),
	)
}

// Tokenize 把文本切成词元，返回空格连接的字符串，可直接存入 PG。
//
// 用 CutForSearch（搜索引擎模式）而非 Cut：它会在长词基础上
// 额外切出子串，例如「布隆过滤器」→ [布隆 过滤 滤器 过滤器]。
// 这样查「过滤器」也能命中，召回率明显更高 —— 对检索场景正是想要的。
func (j *Jieba) Tokenize(text string) string {
	if text == "" {
		return ""
	}
	words := j.seg.CutForSearch(text, true)

	// 过滤纯空白词元：gojieba 会把原文里的空格也作为词返回，
	// 留着会在结果里产生连续空格，虽不影响 to_tsvector，
	// 但会让存储的 tokens 难以阅读和调试。
	out := make([]string, 0, len(words))
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return strings.Join(out, " ")
}

// TokenizeQuery 把查询切词并转成 PG 的 tsquery 语法。
//
// 用 OR（`|`）而非 AND（`&`）连接：
// 查询「Redis 缓存穿透怎么解决」切出 6 个词，若要求全部命中，
// 几乎没有文档能满足 —— 召回率会趋近于 0。用 OR 召回任一词命中的文档，
// 再由 ts_rank 按匹配程度排序，命中词越多的排越前。
// 这正是「召回宽、排序准」的两阶段思路。
func (j *Jieba) TokenizeQuery(query string) string {
	tokens := j.Tokenize(query)
	if tokens == "" {
		return ""
	}

	parts := strings.Fields(tokens)
	escaped := make([]string, 0, len(parts))
	for _, p := range parts {
		// tsquery 有自己的语法字符（& | ! ( ) : *），
		// 用单引号包裹并转义内部单引号，避免用户输入破坏查询结构。
		escaped = append(escaped, "'"+strings.ReplaceAll(p, "'", "''")+"'")
	}
	return strings.Join(escaped, " | ")
}
