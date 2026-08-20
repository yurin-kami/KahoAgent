package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	einoretriever "github.com/cloudwego/eino/components/retriever"
	"github.com/cloudwego/eino/schema"
)

// quietLogger 静音日志：失败路径会记 warn，测试输出里不需要看到
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRetriever 记录收到的参数并返回预设结果。
type fakeRetriever struct {
	docs []*schema.Document
	err  error

	gotQuery string
	gotTopK  int
}

func (f *fakeRetriever) Retrieve(_ context.Context, query string, opts ...einoretriever.Option) ([]*schema.Document, error) {
	f.gotQuery = query
	o := einoretriever.GetCommonOptions(&einoretriever.Options{}, opts...)
	if o.TopK != nil {
		f.gotTopK = *o.TopK
	}
	return f.docs, f.err
}

func doc(content string, meta map[string]any) *schema.Document {
	return &schema.Document{Content: content, MetaData: meta}
}

func TestRAGQuery_ReturnsFormattedSnippets(t *testing.T) {
	f := &fakeRetriever{docs: []*schema.Document{
		doc("布隆过滤器可以拦截不存在的 key", map[string]any{
			"heading_path": "缓存三兄弟 > 缓存穿透",
			"source":       "cache.md",
		}),
	}}
	tl, err := NewRAGQueryTool(f)
	if err != nil {
		t.Fatalf("new tool: %v", err)
	}

	out, err := tl.InvokableRun(context.Background(), `{"query":"缓存穿透"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out, "布隆过滤器") {
		t.Errorf("the snippet content must be present, got: %s", out)
	}
	// 来源是答案溯源的基础，必须出现在给模型的文本里
	if !strings.Contains(out, "缓存穿透") || !strings.Contains(out, "cache.md") {
		t.Errorf("the source must be present, got: %s", out)
	}
	if f.gotQuery != "缓存穿透" {
		t.Errorf("expected the query to be forwarded, got %q", f.gotQuery)
	}
}

func TestRAGQuery_TopKDefaultsAndClamps(t *testing.T) {
	// 模型经常传离谱的 top_k。收敛到合法区间而不是报错 ——
	// 能干活就别打断 ReAct 循环。
	cases := []struct {
		name string
		args string
		want int
	}{
		{"omitted", `{"query":"x"}`, defaultTopK},
		{"zero", `{"query":"x","top_k":0}`, defaultTopK},
		{"negative", `{"query":"x","top_k":-5}`, defaultTopK},
		{"absurd", `{"query":"x","top_k":1000}`, maxTopK},
		{"valid", `{"query":"x","top_k":3}`, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRetriever{docs: []*schema.Document{doc("内容", nil)}}
			tl, _ := NewRAGQueryTool(f)

			if _, err := tl.InvokableRun(context.Background(), tc.args); err != nil {
				t.Fatalf("run: %v", err)
			}
			if f.gotTopK != tc.want {
				t.Errorf("expected topK %d, got %d", tc.want, f.gotTopK)
			}
		})
	}
}

func TestRAGQuery_NoResultsSaysSoExplicitly(t *testing.T) {
	// ⚠️ 空结果必须明确说「没找到」而非返回空串。
	// 空串会让模型以为工具坏了，进而重复调用直到撞上 MaxIterations。
	f := &fakeRetriever{docs: nil}
	tl, _ := NewRAGQueryTool(f)

	out, err := tl.InvokableRun(context.Background(), `{"query":"不存在的东西"}`)
	if err != nil {
		t.Fatalf("an empty result must not be an error: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("must not return an empty string for no results")
	}
	if !strings.Contains(out, "未找到") {
		t.Errorf("expected an explicit not-found message, got: %s", out)
	}
}

func TestRAGQuery_EmptyQueryReportedAsObservation(t *testing.T) {
	// 空 query 要说清「缺什么」，且不能说成「没找到」——
	// 后者会让模型以为搜过了，从而不再重试。
	//
	// 返回 observation 而非 error：eino 对工具 error 会中止整个节点，
	// 模型就没有纠错机会了。
	f := &fakeRetriever{}
	tl, _ := NewRAGQueryTool(f, WithLogger(quietLogger()))

	for _, args := range []string{`{"query":""}`, `{"query":"   "}`, `{}`} {
		out, err := tl.InvokableRun(context.Background(), args)
		if err != nil {
			t.Errorf("%s: must not return an error (eino aborts the run): %v", args, err)
		}
		if !strings.Contains(out, "query") {
			t.Errorf("%s: should say which parameter is missing, got: %s", args, out)
		}
		if strings.Contains(out, "未找到") {
			t.Errorf("%s: must not be phrased as 'not found', got: %s", args, out)
		}
	}
}

func TestRAGQuery_MalformedArgsReportedAsObservation(t *testing.T) {
	// 参数错误交给模型让它下一轮修正，而不是中止整个请求
	f := &fakeRetriever{}
	tl, _ := NewRAGQueryTool(f, WithLogger(quietLogger()))

	out, err := tl.InvokableRun(context.Background(), `{"query": broken`)
	if err != nil {
		t.Fatalf("must not return an error (eino aborts the run): %v", err)
	}
	if !strings.Contains(out, "参数") {
		t.Errorf("the observation should mention the parameter problem, got: %s", out)
	}
}

func TestRAGQuery_RetrievalFailureReportedAsObservation(t *testing.T) {
	// ⚠️ 这是验收项「Tool 报错时 Agent 能优雅回复而非崩溃」的关键。
	//
	// eino 在 compose/tool_node.go:1203 对任何非中断类工具错误都会
	// 中止整个节点。若这里返回 error，一次 Milvus 抖动就会让整个请求
	// 崩掉，用户看到连接断开而不是一句人话。
	f := &fakeRetriever{err: errors.New("milvus down")}
	tl, _ := NewRAGQueryTool(f, WithLogger(quietLogger()))

	out, err := tl.InvokableRun(context.Background(), `{"query":"x"}`)
	if err != nil {
		t.Fatalf("must not return an error (eino aborts the run): %v", err)
	}
	// 要让模型知道是「服务不可用」而非「没有这份文档」
	if !strings.Contains(out, "不可用") {
		t.Errorf("the model must learn the service failed, got: %s", out)
	}
	// 且要明确禁止编造 —— 否则模型可能用通用知识冒充文档内容
	if !strings.Contains(out, "不要编造") {
		t.Errorf("the observation should forbid fabrication, got: %s", out)
	}
	// 原始错误要带上，便于用户/运维定位
	if !strings.Contains(out, "milvus down") {
		t.Errorf("the underlying cause should be included, got: %s", out)
	}
}

func TestRAGQuery_TruncatesLongContent(t *testing.T) {
	// 不截断的话 5 个块就能撑爆上下文
	long := strings.Repeat("字", 2000)
	f := &fakeRetriever{docs: []*schema.Document{doc(long, nil)}}
	tl, _ := NewRAGQueryTool(f)

	out, err := tl.InvokableRun(context.Background(), `{"query":"x"}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len([]rune(out)); n > snippetLimit+100 {
		t.Errorf("expected truncation near %d runes, got %d", snippetLimit, n)
	}
}

func TestRAGQuery_SourceFallsBackWhenHeadingMissing(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		want string
	}{
		{"heading only", map[string]any{"heading_path": "A > B"}, "A > B"},
		{"source only", map[string]any{"source": "f.md"}, "f.md"},
		{"doc_id fallback", map[string]any{"doc_id": "doc-1"}, "doc-1"},
		{"nothing", nil, "未知来源"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRetriever{docs: []*schema.Document{doc("内容", tc.meta)}}
			tl, _ := NewRAGQueryTool(f)

			out, err := tl.InvokableRun(context.Background(), `{"query":"x"}`)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected source %q, got: %s", tc.want, out)
			}
		})
	}
}

func TestRAGQuery_InfoDescribesWhenToUse(t *testing.T) {
	// Description 决定模型会不会调用这个工具。
	// 断言它说明了「什么时候用」而不只是「我是什么」。
	tl, _ := NewRAGQueryTool(&fakeRetriever{})
	info, err := tl.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}

	if info.Name != "rag_query" {
		t.Errorf("unexpected name: %q", info.Name)
	}
	if info.ParamsOneOf == nil {
		t.Fatal("the tool must declare its parameters")
	}
	js, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	if js == nil {
		t.Fatal("expected a JSON schema for the parameters")
	}
	// 描述里要有触发场景的关键词
	for _, want := range []string{"当用户", "使用此工具"} {
		if !strings.Contains(info.Desc, want) {
			t.Errorf("the description should explain when to use the tool (missing %q): %s", want, info.Desc)
		}
	}
}

func TestNewRAGQueryTool_RejectsNilRetriever(t *testing.T) {
	if _, err := NewRAGQueryTool(nil); err == nil {
		t.Fatal("expected an error for a nil retriever")
	}
}
