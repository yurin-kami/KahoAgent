//go:build e2e

// 用真实 LLM 检验摘要提示词是否保住了关键信息。
//
//	go test -tags=e2e -v -run TestSummarize ./internal/agent/
//
// 为什么必须用真实模型：摘要的质量**完全**取决于提示词怎么写，
// 而 stub summarizer 返回的是写死的字符串 —— 提示词写得再糟，
// 单元测试也照样全绿。这类"只有真跑才会暴露"的缺陷，
// 正是端到端验收存在的意义。
package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/yurin-kami/KahoAgent/internal/config"
	"github.com/yurin-kami/KahoAgent/pkg/llm"
)

func realSummarizer(t *testing.T) Summarizer {
	t.Helper()
	cfg, err := config.Load(findConfig(t))
	if err != nil {
		t.Skipf("config.toml is unavailable: %v", err)
	}
	return llm.NewChatModel(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model)
}

func findConfig(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"../../config.toml", "../config.toml", "config.toml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("could not locate config.toml")
	return ""
}

// TestSummarize_KeepsFactsTheUserAskedToRemember 是提示词的**冒烟检查**，
// 不是那个缺陷的回归防线 —— 这点必须说清楚，否则会给人虚假的安全感。
//
// 背景：12 轮验收曾失败，原因是提示词只要求保留「文件名、主题、结论」，
// 用户第 1 轮说的项目代号三者都不沾边，于是被名正言顺地省掉了。
// 修复见 memory.go 的 summarize()。
//
// ⚠️ 为什么这里不是回归测试：我试过用旧提示词跑本测试，
// 4 轮 / 6 轮 / 10 轮填充、真实 RAG 回答做噪声，**都照样通过**
// （旧提示词 3/3 次都保住了代号）。摘要质量对输入长度和措辞
// 极其敏感，脱离完整链路就复现不出当时的信噪比。
//
// 真正的防线是 internal/app 的 TestE2E_TwelveTurnsKeepContext ——
// 它跑真实的 11 轮 RAG 对话，是唯一实际抓住过这个缺陷、
// 也实际验证过修复的测试。本测试只保证提示词没被改回
// 「不提用户事实」的形态，成本低就顺手留着。
func TestSummarize_KeepsFactsTheUserAskedToRemember(t *testing.T) {
	m := &SessionMemory{summarizer: realSummarizer(t), logger: quietLogger()}

	msgs := []*schema.Message{
		user("记住：我给这个项目起的代号是「紫水晶」。之后我会问你。"),
		assistant("已记住：项目代号为「紫水晶」。"),
	}
	// 复刻真实压缩时的输入轮数：maxTurns=10 / keepTurns=6，
	// 第 11 轮触发压缩，被卷进摘要的是第 1~5 轮。
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			user("简单说一句关于缓存的话。"),
			assistant(ragNoise[i%len(ragNoise)]))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	summary, err := m.summarize(ctx, msgs)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	t.Logf("summary: %s", summary)

	if !strings.Contains(summary, "紫水晶") {
		t.Errorf("the summary dropped a fact the user explicitly asked to remember.\n"+
			"This is what breaks multi-turn recall after compaction.\nGot: %s", summary)
	}
}

// ragNoise 取自真实运行中模型给出的回答，用来复现真实的信噪比。
var ragNoise = []string{
	"缓存一致性需权衡实时性与性能，常见策略包括「先删缓存再更新 DB」（Cache-Aside）" +
		"或「更新 DB 后异步刷新缓存」，但无法绝对强一致" +
		"（来源：Redis 八股总集 > 四、缓存一致性 > 1. 如何保证缓存与数据库一致？）。",
	"缓存更新策略中，「先更新数据库，再删除缓存」（Write-Through + Cache Invalidation）" +
		"是较稳妥的方案，可降低缓存与 DB 不一致的概率" +
		"（来源：Redis 八股总集 > 四、缓存一致性 > 2. 更新缓存还是删除缓存？）。",
	"缓存预热是在系统上线或低峰期主动加载热点数据到缓存中，" +
		"避免冷启动时大量请求直接冲击数据库" +
		"（来源：Redis 八股总集 > 五、缓存预热与降级 > 1. 什么是缓存预热？）。",
	"缓存降级是在缓存不可用或响应超时时，自动切换至数据库直查或返回兜底数据，" +
		"保障系统可用性（来源：Redis 八股总集 > 五、缓存预热与降级 > 2. 什么是缓存降级？）。",
	"缓存穿透、击穿、雪崩三类问题本质不同：穿透是查不存在的数据，" +
		"击穿是热点 Key 失效，雪崩是大量 Key 集体失效" +
		"（来源：Redis 八股总集 > 三、缓存穿透、击穿、雪崩 > 开篇定义）。",
	"布隆过滤器（Bloom Filter）可用于高效判断「某数据是否可能存在」，" +
		"是防御缓存穿透的常用前置组件" +
		"（来源：Redis 八股总集 > 三、缓存穿透、击穿、雪崩 > 1. 缓存穿透）。",
}

// 摘要必须保留具体文件名 —— 后续「第二个文件多大」全靠它
func TestSummarize_KeepsFileNames(t *testing.T) {
	m := &SessionMemory{summarizer: realSummarizer(t), logger: quietLogger()}

	msgs := []*schema.Message{
		user("列一下我的文件"),
		assistant("你有 3 个文件：redis-cache.md（2.0 KB）、kafka-guide.md（24.0 KB）、milvus-hnsw.md（8.0 KB）。"),
	}
	for i := 0; i < 6; i++ {
		msgs = append(msgs, user("再说说缓存"), assistant("缓存需要考虑穿透、击穿、雪崩。"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	summary, err := m.summarize(ctx, msgs)
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	t.Logf("summary: %s", summary)

	if !strings.Contains(summary, "kafka-guide.md") {
		t.Errorf("the summary dropped the file names; ordinal follow-ups "+
			"like 「第二个文件多大」 will break. Got: %s", summary)
	}
}
