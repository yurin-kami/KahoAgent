package tokenizer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanyiwu/gojieba"
)

func TestTokenize_SplitsChineseCompounds(t *testing.T) {
	// 核心用途：PG 自带分词器会把「Redis缓存穿透」当成一个词，
	// 导致查「缓存穿透」匹配不上。分词后必须能切开。
	got := Shared().Tokenize("Redis缓存穿透怎么解决")

	for _, want := range []string{"Redis", "缓存", "穿透"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected token %q in %q", want, got)
		}
	}
}

func TestTokenize_SearchModeProducesSubwords(t *testing.T) {
	// CutForSearch 会在长词基础上额外切子串，
	// 这样查「过滤器」也能命中「布隆过滤器」
	got := Shared().Tokenize("布隆过滤器")

	if !strings.Contains(got, "过滤器") {
		t.Errorf("expected the full word 过滤器 in %q", got)
	}
	if !strings.Contains(got, "布隆") {
		t.Errorf("expected 布隆 in %q", got)
	}
}

func TestTokenize_HandlesMixedChineseEnglish(t *testing.T) {
	got := Shared().Tokenize("使用 gojieba 对中文进行分词")

	if !strings.Contains(got, "gojieba") {
		t.Errorf("English token lost: %q", got)
	}
	if !strings.Contains(got, "分词") {
		t.Errorf("Chinese token lost: %q", got)
	}
}

func TestTokenize_NoRedundantWhitespace(t *testing.T) {
	// gojieba 会把原文空格也当词返回，必须过滤掉
	got := Shared().Tokenize("使用 gojieba 对中文进行分词")

	if strings.Contains(got, "  ") {
		t.Errorf("consecutive spaces in output: %q", got)
	}
	if got != strings.TrimSpace(got) {
		t.Errorf("output has leading/trailing space: %q", got)
	}
	for _, f := range strings.Split(got, " ") {
		if f == "" {
			t.Errorf("empty token in output: %q", got)
		}
	}
}

func TestTokenize_Empty(t *testing.T) {
	if got := Shared().Tokenize(""); got != "" {
		t.Errorf("expected empty output, got %q", got)
	}
	if got := Shared().Tokenize("   "); got != "" {
		t.Errorf("whitespace-only input should produce no tokens, got %q", got)
	}
}

func TestTokenizeQuery_UsesOrNotAnd(t *testing.T) {
	// 用 AND 连接会让召回率趋近于 0 —— 几乎没有文档能同时含全部词
	got := Shared().TokenizeQuery("Redis 缓存穿透怎么解决")

	if !strings.Contains(got, "|") {
		t.Errorf("expected OR-joined tsquery, got %q", got)
	}
	if strings.Contains(got, "&") {
		t.Errorf("tsquery must not use AND, got %q", got)
	}
}

func TestTokenizeQuery_QuotesEveryToken(t *testing.T) {
	got := Shared().TokenizeQuery("缓存 穿透")

	for _, part := range strings.Split(got, "|") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, "'") || !strings.HasSuffix(part, "'") {
			t.Errorf("token %q is not quoted in %q", part, got)
		}
	}
}

func TestTokenizeQuery_EscapesTsqueryOperators(t *testing.T) {
	// tsquery 的语法字符不能破坏查询结构，否则会是注入风险
	for _, input := range []string{
		"缓存 & 穿透", "a | b", "test:*", "(foo)", "!bar", "it's",
	} {
		got := Shared().TokenizeQuery(input)
		if got == "" {
			continue
		}
		// 每个词元都被单引号包裹，运算符只可能出现在引号之间
		for _, part := range strings.Split(got, "|") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.HasPrefix(part, "'") || !strings.HasSuffix(part, "'") {
				t.Errorf("input %q produced an unquoted token %q (full: %q)", input, part, got)
			}
		}
	}
}

func TestTokenizeQuery_Empty(t *testing.T) {
	if got := Shared().TokenizeQuery(""); got != "" {
		t.Errorf("expected empty tsquery, got %q", got)
	}
}

func TestShared_ReturnsTheSameInstance(t *testing.T) {
	// 词典约 100MB，每次新建会迅速吃光内存
	if Shared() != Shared() {
		t.Error("Shared() must return a single cached instance")
	}
}

func TestTokenize_IsConcurrencySafe(t *testing.T) {
	// 检索路径会并发调用，gojieba 的分词方法必须能共用同一实例
	const n = 50
	done := make(chan string, n)

	for range n {
		go func() { done <- Shared().Tokenize("Redis缓存穿透怎么解决") }()
	}

	first := <-done
	for range n - 1 {
		if got := <-done; got != first {
			t.Fatalf("concurrent tokenization differs:\n  %q\n  %q", first, got)
		}
	}
}

// 下面两个测试守护 KAHO_JIEBA_DICT_DIR 这个开关。
//
// 背景：gojieba 默认从**编译机源码路径**加载词典，容器里没有 Go module
// 缓存，NewJieba 直接 panic。这个故障编译期毫无征兆，本地跑测试也永远
// 复现不了 —— 只有真正启动容器才暴露（Phase 6 实测踩到）。
// 所以必须有测试在本地就能证明「切换目录这件事真的生效」，
// 否则下次有人重构掉这段逻辑，仍然要等到部署才发现。
//
// 不测 Shared()：它有 sync.Once 缓存，一旦被别的测试先调用过就没法
// 再观察不同环境变量下的行为，所以直接测 newSegmenter。

func TestNewSegmenter_UsesCustomDictDir(t *testing.T) {
	// 用 gojieba 自己暴露的默认目录当「自定义目录」：
	// 路径确实存在，于是能验证「显式传五个路径」这条分支拼得对 ——
	// 拼错任何一个文件名都会 panic。
	t.Setenv(dictDirEnv, gojieba.DICT_DIR)

	seg := newSegmenter()
	defer seg.Free()

	got := strings.Join(seg.CutForSearch("Redis缓存穿透怎么解决", true), " ")
	for _, want := range []string{"Redis", "缓存", "穿透"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected token %q in %q", want, got)
		}
	}
}

func TestNewSegmenter_HonorsTheEnvVar(t *testing.T) {
	// 反证：指向一个不存在的目录必须失败。
	// 少了这条，上一个测试无法区分「环境变量生效了」和
	// 「环境变量被忽略、走的还是默认路径」—— 两种情况结果完全一样。
	//
	// gojieba 用 panic 而非返回 error 报告词典缺失，所以只能用 recover 断言。
	t.Setenv(dictDirEnv, filepath.Join(t.TempDir(), "nonexistent"))

	defer func() {
		if recover() == nil {
			t.Error("expected a panic: the env var was silently ignored")
		}
	}()

	seg := newSegmenter()
	seg.Free()
}
