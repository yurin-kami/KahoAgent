//go:build e2e

// Phase 3 的端到端验收。
//
//	go test -tags=e2e -v -timeout 20m ./internal/app/
//
// 跑的是与生产**同一份**装配（app.New），接的是真实的 Milvus / PG /
// Redis / DashScope —— 只有 CloudKaho 用 httptest 替身，因为它当前
// 起不来。替身实现的是真实的 wire 格式（code 为字符串 "0"、
// share 收绝对时间戳），所以它验证的仍是真实的协议假设。
//
// 为什么不写成 shell 脚本：
// 计划里写的是 chat_test.sh，但那需要先手工起服务、手工起 CloudKaho
// 替身，还要用 curl+jq 解析 SSE —— 每一步都可能因环境差异而失败，
// 且断言能力弱（「多轮上下文生效」这种要跨请求比对状态）。
// 用 Go test 则能 httptest.NewServer 起完整应用，断言直接写在同一
// 进程里，还能被 CI 无差别执行。验收项一条不少，见下面各 Test。
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yurin-kami/KahoAgent/internal/config"
)

// ---------- CloudKaho 替身 ----------

// stubCloudKaho 复刻 CloudKaho 的真实响应格式。
//
// ⚠️ code 是**字符串** "0" 而不是整数 0，且没有统一信封 ——
// 这是从真实服务端代码核对来的。替身若"修正"成更规整的格式，
// 就测不出客户端的解析假设是否正确了。
type stubCloudKaho struct {
	failShare atomic.Bool
	calls     atomic.Int32
}

func (s *stubCloudKaho) files() []map[string]any {
	// created_at 用固定值，避免断言随时间漂移
	return []map[string]any{
		{"file_id": 101, "file_name": "redis-cache.md", "file_size": 2048, "created_at": 1755600000},
		{"file_id": 102, "file_name": "kafka-guide.md", "file_size": 24576, "created_at": 1755600000},
		{"file_id": 103, "file_name": "milvus-hnsw.md", "file_size": 8192, "created_at": 1755600000},
	}
}

func (s *stubCloudKaho) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/api/file/search", func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0", "total": len(s.files()), "files": s.files(),
		})
	})

	mux.HandleFunc("/api/file/metadata/", func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		id := strings.TrimPrefix(r.URL.Path, "/api/file/metadata/")
		for _, f := range s.files() {
			if fmt.Sprint(f["file_id"]) == id {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "file": f})
				return
			}
		}
		// 真实服务对"不存在"和"没权限"统一返回 404，
		// 区分开会变成一个 file_id 枚举探测接口
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "1", "error": "file not found"})
	})

	mux.HandleFunc("/api/file/share", func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		if s.failShare.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "1", "error": "s3 unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0", "share_url": "https://s3.example.com/presigned/abc123",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ---------- 测试环境 ----------

type env struct {
	url   string
	cloud *stubCloudKaho
	app   *App
}

// repoRoot 向上找 go.mod —— 测试的工作目录是包目录，不是仓库根
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod")
		}
		dir = parent
	}
}

func setup(t *testing.T) *env {
	t.Helper()

	root := repoRoot(t)
	cfg, err := config.Load(filepath.Join(root, "config.toml"))
	if err != nil {
		t.Skipf("config.toml is unavailable (%v); this suite needs the real stack", err)
	}

	cloud := &stubCloudKaho{}
	stub := cloud.server(t)
	cfg.CloudKaho.BaseURL = stub.URL
	cfg.CloudKaho.Token = "e2e-token" // 非空才会注册文件工具

	level := slog.LevelWarn
	if testing.Verbose() {
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a, err := New(ctx, cfg, logger)
	if err != nil {
		t.Skipf("could not build the app (%v); is the stack up? scripts/pull-images.ps1", err)
	}
	t.Cleanup(a.Close)

	mux := http.NewServeMux()
	mux.Handle("/api/chat", a.Handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &env{url: srv.URL + "/api/chat", cloud: cloud, app: a}
}

// ---------- SSE 客户端 ----------

type reply struct {
	text       string
	tools      []string
	errs       []string
	firstToken time.Duration // 从发出请求到收到第一个 delta
	total      time.Duration
	chunks     int
}

func (r reply) usedTool(name string) bool {
	for _, t := range r.tools {
		if t == name {
			return true
		}
	}
	return false
}

// ask 发一轮对话并流式解析 SSE。
//
// ⚠️ 必须**边收边解析**：用 io.ReadAll 等整段结束再算首字延迟的话，
// 测出来的就是总耗时，流式是否真的生效完全测不出来。
func (e *env) ask(t *testing.T, sessionID, message string) reply {
	t.Helper()

	body, _ := json.Marshal(map[string]string{
		"session_id": sessionID, "message": message,
	})
	req, err := http.NewRequest(http.MethodPost, e.url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected an SSE content type, got %q", ct)
	}

	var out reply
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("malformed SSE payload %q: %v", line, err)
		}
		switch ev.Type {
		case "delta":
			if out.chunks == 0 {
				out.firstToken = time.Since(start)
			}
			out.chunks++
			sb.WriteString(ev.Text)
		case "tool":
			out.tools = append(out.tools, ev.Tool)
		case "error":
			out.errs = append(out.errs, ev.Text)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	out.text = sb.String()
	out.total = time.Since(start)
	if testing.Verbose() {
		t.Logf("Q: %s\nA: %s\n(tools=%v chunks=%d first=%v total=%v)",
			message, out.text, out.tools, out.chunks, out.firstToken.Round(time.Millisecond),
			out.total.Round(time.Millisecond))
	}
	return out
}

// session 生成隔离的会话 ID，并在测试结束时清掉它。
// 不隔离的话，重跑一次就会读到上一次的历史，
// 「多轮上下文」这类断言会莫名其妙地时好时坏。
func (e *env) session(t *testing.T) string {
	t.Helper()
	id := fmt.Sprintf("e2e-%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.app.Memory.Clear(ctx, id); err != nil {
			t.Logf("could not clear session %s: %v", id, err)
		}
	})
	return id
}

// ---------- 验收项 ----------

// 验收 1：「帮我找关于 Redis 缓存的文档」→ 调用 rag_query → 答案 + 引用来源
func TestE2E_RAGQueryWithCitations(t *testing.T) {
	e := setup(t)

	r := e.ask(t, e.session(t), "帮我找关于 Redis 缓存的文档，并说明缓存穿透是怎么回事")

	if len(r.errs) > 0 {
		t.Fatalf("the stream reported errors: %v", r.errs)
	}
	if !r.usedTool("rag_query") {
		t.Errorf("expected rag_query to be called, got tools=%v", r.tools)
	}
	if r.text == "" {
		t.Fatal("expected an answer")
	}
	// 引用来源：系统提示要求标注出处，回答里应出现来源痕迹
	if !mentionsSource(r.text) {
		t.Errorf("the answer should cite its sources, got: %s", r.text)
	}
}

// mentionsSource 判断回答里是否带了出处。
// 放宽到多种写法：模型不会每次都用同一个措辞。
func mentionsSource(s string) bool {
	for _, marker := range []string{"来源", "出处", "引用", "文档", ".md", "根据"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// 验收 2：「第二个文件多大」→ 调用 file_metadata（**证明多轮上下文生效**）
//
// 这一项的关键在于：第二轮的问题里**没有任何文件名或 ID**。
// 模型只能从上一轮的 file_search 结果里知道"第二个"指的是
// file_id=102，否则就是猜的。所以它同时验证了历史存取与工具调用。
func TestE2E_MultiTurnContext(t *testing.T) {
	e := setup(t)
	sid := e.session(t)

	first := e.ask(t, sid, "帮我列一下我的文件")
	if !first.usedTool("file_search") {
		t.Fatalf("the first turn should call file_search, got tools=%v", first.tools)
	}
	if !strings.Contains(first.text, "kafka-guide.md") {
		t.Fatalf("the first turn should list the files, got: %s", first.text)
	}

	second := e.ask(t, sid, "第二个文件多大？")

	if len(second.errs) > 0 {
		t.Fatalf("the stream reported errors: %v", second.errs)
	}
	// 24576 B = 24.0 KB，是 kafka-guide.md（列表第二个）的大小。
	// 答对这个数就证明它记住了上一轮的列表顺序。
	if !strings.Contains(second.text, "24") {
		t.Errorf("expected the size of the second file (24 KB / 24576 B); "+
			"multi-turn context appears broken. Got: %s", second.text)
	}
	if strings.Contains(second.text, "2.0 KB") || strings.Contains(second.text, "2048") {
		t.Errorf("it answered with the *first* file's size — the ordinal was misresolved: %s", second.text)
	}
}

// 验收 3：「给我一个分享链接」→ 调用 file_share → 返回 URL
func TestE2E_FileShareReturnsURL(t *testing.T) {
	e := setup(t)
	sid := e.session(t)

	e.ask(t, sid, "帮我列一下我的文件")
	r := e.ask(t, sid, "给第一个文件生成一个分享链接")

	if len(r.errs) > 0 {
		t.Fatalf("the stream reported errors: %v", r.errs)
	}
	if !r.usedTool("file_share") {
		t.Errorf("expected file_share to be called, got tools=%v", r.tools)
	}
	if !strings.Contains(r.text, "https://s3.example.com/presigned/abc123") {
		t.Errorf("expected the share URL in the answer, got: %s", r.text)
	}
}

// 验收 4：连续 12 轮对话不 OOM、不丢上下文（截断+摘要生效）
//
// 12 轮会越过 maxTurns=10 的阈值，触发摘要压缩。
// 压缩之后**最早**说过的信息必须仍然可被追溯 —— 这正是摘要的意义，
// 若只做截断，第 1 轮的内容会彻底消失。
func TestE2E_TwelveTurnsKeepContext(t *testing.T) {
	e := setup(t)
	sid := e.session(t)

	// 第 1 轮埋一个只在这里出现过的事实
	const secret = "紫水晶"
	e.ask(t, sid, "记住：我给这个项目起的代号是「"+secret+"」。之后我会问你。")

	for i := 2; i <= 11; i++ {
		r := e.ask(t, sid, fmt.Sprintf("第 %d 个问题：简单说一句关于缓存的话。", i))
		if len(r.errs) > 0 {
			t.Fatalf("turn %d reported errors: %v", i, r.errs)
		}
		if r.text == "" {
			t.Fatalf("turn %d returned nothing", i)
		}
	}

	// 第 12 轮回头问第 1 轮的事实。
	// 先把此刻的历史打出来 —— 断言失败时要能直接看出
	// 到底是摘要没保住事实，还是保住了但模型没用上。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgs, err := e.app.Memory.Load(ctx, sid)
	if err != nil {
		t.Fatalf("load history: %v", err)
	}
	t.Logf("history going into the final turn: %d messages", len(msgs))
	for i, msg := range msgs {
		t.Logf("  [%d] %-9s %.200s", i, msg.Role, msg.Content)
	}

	if len(msgs) >= 22 { // 11 轮 = 22 条，压缩后必然更少
		t.Errorf("history was never compacted: %d messages after 11 turns", len(msgs))
	}

	var carried strings.Builder
	for _, msg := range msgs {
		carried.WriteString(msg.Content)
	}
	if !strings.Contains(carried.String(), secret) {
		t.Errorf("the summary dropped the fact from turn 1 — " +
			"the model cannot possibly answer the follow-up")
	}

	last := e.ask(t, sid, "我一开始说的项目代号是什么？")
	if !strings.Contains(last.text, secret) {
		t.Errorf("after 12 turns the model lost the fact from turn 1 "+
			"(summarisation should have preserved it). Got: %s", last.text)
	}
}

// 验收 5：SSE 逐 token 输出，首字延迟 <1.5s
//
// ⚠️ 用**不需要工具**的问题来测。带工具的请求首字延迟里包含了
// 一整轮检索（Phase 2 实测精排就要 1.4s），那测的是检索速度而非
// 流式是否生效 —— 两回事。
func TestE2E_FirstTokenLatency(t *testing.T) {
	e := setup(t)

	const budget = 1500 * time.Millisecond
	r := e.ask(t, e.session(t), "用一句话打个招呼，不要调用任何工具。")

	if r.chunks < 2 {
		t.Errorf("expected incremental output, got %d chunk(s) — streaming may be broken", r.chunks)
	}
	if r.firstToken > budget {
		t.Errorf("first-token latency %v exceeds the %v budget", r.firstToken.Round(time.Millisecond), budget)
	}
	t.Logf("first token in %v, %d chunks, %v total",
		r.firstToken.Round(time.Millisecond), r.chunks, r.total.Round(time.Millisecond))
}

// 验收 6：异常处理 —— 故意让 Tool 报错，Agent 能优雅回复而非崩溃
//
// ⚠️ 这一项测的是本项目最容易踩的坑：eino 在 compose/tool_node.go
// 对任何非中断类的工具 error 都会**中止整个节点**，于是一次
// 后端抖动会让整个请求崩掉。所以工具必须把失败转成文本观察。
// 这里注入的是真实的 HTTP 500，走的是完整的错误路径。
func TestE2E_ToolFailureDegradesGracefully(t *testing.T) {
	e := setup(t)
	sid := e.session(t)

	e.ask(t, sid, "帮我列一下我的文件")

	e.cloud.failShare.Store(true)
	r := e.ask(t, sid, "给第一个文件生成一个分享链接")

	// 崩掉的表现：没有回答，只有 error 事件
	if r.text == "" {
		t.Fatalf("the agent produced no answer when the tool failed (errs=%v)", r.errs)
	}
	if len(r.errs) > 0 {
		t.Errorf("a tool failure should not surface as a stream error: %v", r.errs)
	}
	// 不能编造一个链接
	if strings.Contains(r.text, "https://s3.example.com") {
		t.Errorf("the agent fabricated a share URL despite the failure: %s", r.text)
	}
	// 要如实说明失败
	if !mentionsFailure(r.text) {
		t.Errorf("the agent should tell the user the operation failed, got: %s", r.text)
	}
	t.Logf("graceful degradation: %s", r.text)
}

func mentionsFailure(s string) bool {
	for _, marker := range []string{"失败", "错误", "无法", "不可用", "出错", "稍后", "抱歉"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
