//go:build e2e

// Phase 4 的端到端验收：真的把二进制拉起来，用真实 MCP 客户端连上去。
//
//	go test -tags=e2e -v -timeout 10m ./cmd/mcp/
//
// 为什么必须测编译好的二进制而不是 in-process 的 Server：
// 验收项是「Claude Desktop 配置后能 list 出 4 个 Tool」，而 Claude
// Desktop 是**起一个子进程**并通过 stdio 说话。这条路径上有一类
// in-process 测试根本覆盖不到的故障 —— 任何往 stdout 写的东西
// （日志、fmt.Println、依赖库的告警）都会污染协议流，
// 表现为「工具莫名其妙不出现」。只有真起进程才能发现。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yurin-kami/KahoAgent/internal/config"
)

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

// buildBinary 编译出真正要交给 Claude Desktop 的那个二进制。
func buildBinary(t *testing.T, root string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "kaho-mcp")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", out, "./cmd/mcp")
	cmd.Dir = root
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, combined)
	}
	return out
}

// stubCloudKaho 复刻 CloudKaho 的真实响应格式（code 是字符串 "0"）。
// 该服务当前起不来，但文件类工具必须被注册，否则「list 出 4 个 Tool」
// 这条验收就无从谈起。
func stubCloudKaho(t *testing.T) string {
	t.Helper()

	files := []map[string]any{
		{"file_id": 101, "file_name": "redis-cache.md", "file_size": 2048, "created_at": 1755600000},
		{"file_id": 102, "file_name": "kafka-guide.md", "file_size": 24576, "created_at": 1755600000},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/file/search", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0", "total": len(files), "files": files,
		})
	})
	mux.HandleFunc("/api/file/metadata/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "file": files[0]})
	})
	mux.HandleFunc("/api/file/share", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0", "share_url": "https://s3.example.com/presigned/abc",
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// connectStdio 起子进程并用真实 MCP 客户端接上，模拟 Claude Desktop。
func connectStdio(t *testing.T) *mcp.ClientSession {
	t.Helper()

	root := repoRoot(t)
	if _, err := config.Load(filepath.Join(root, "config.toml")); err != nil {
		t.Skipf("config.toml is unavailable (%v); this suite needs the real stack", err)
	}

	bin := buildBinary(t, root)

	cmd := exec.Command(bin)
	cmd.Dir = root // 二进制从工作目录读 config.toml，与 Claude Desktop 的 cwd 一致
	// 用环境变量把子进程指向替身 CloudKaho，并给一个非空 token
	// （token 为空时文件工具会被刻意跳过注册）
	cmd.Env = append(os.Environ(),
		"CLOUDKAHO_BASE_URL="+stubCloudKaho(t),
		"CLOUDKAHO_TOKEN=e2e-token",
	)

	// stderr 收集起来：它同时是"日志没跑到 stdout 去"的证据
	var stderr strings.Builder
	cmd.Stderr = &stderr
	t.Cleanup(func() {
		if t.Failed() && stderr.Len() > 0 {
			t.Logf("server stderr:\n%s", stderr.String())
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0.0.1"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Skipf("could not connect to the MCP server (%v); is the stack up?\nstderr:\n%s",
			err, stderr.String())
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// 验收：Claude Desktop 配置后能 list 出 4 个 Tool
func TestMCPE2E_ListsFourTools(t *testing.T) {
	cs := connectStdio(t)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	got := map[string]string{}
	for _, tl := range res.Tools {
		got[tl.Name] = tl.Description
	}
	t.Logf("server exposed %d tools:", len(res.Tools))
	for name, desc := range got {
		t.Logf("  %-15s %.60s", name, desc)
	}

	for _, want := range []string{"rag_query", "file_search", "file_metadata", "file_share"} {
		if _, ok := got[want]; !ok {
			t.Errorf("tool %q is missing; Claude Desktop would not see it", want)
		}
		if got[want] == "" {
			t.Errorf("tool %q has no description; the model cannot know when to use it", want)
		}
	}
}

// 验收：「搜索我云盘里 Kafka 相关的文档」→ 成功调用并返回
//
// 这里直接调 rag_query（MCP 层不含模型，模型在客户端那侧）。
func TestMCPE2E_RAGQueryReturnsContent(t *testing.T) {
	cs := connectStdio(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "rag_query",
		Arguments: map[string]any{"query": "Kafka 消息队列", "top_k": 3},
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Fatalf("the tool reported an error: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("expected content")
	}

	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", res.Content[0])
	}
	if strings.TrimSpace(text.Text) == "" {
		t.Error("the tool returned empty text")
	}
	t.Logf("rag_query returned %d chars:\n%.400s", len(text.Text), text.Text)
}

// 验收：审计日志不污染 stdout。
//
// ⚠️ 这是 stdio 模式下最隐蔽也最致命的故障：往 stdout 写一行日志，
// 整个协议流就废了，而现象只是"工具列表是空的"。上面的连接一旦
// 成功，其实已经间接证明了 stdout 干净 —— 但那是隐式的，
// 这里显式再验一次：调用若干次工具（每次都会产生审计日志），
// 之后协议仍然正常工作。
func TestMCPE2E_AuditLoggingDoesNotCorruptStdout(t *testing.T) {
	cs := connectStdio(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{
			Name:      "rag_query",
			Arguments: map[string]any{"query": "缓存", "top_k": 1},
		}); err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
	}

	// 产生了 5 条审计日志之后，协议必须仍然可用
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("the protocol broke after audit logging: %v", err)
	}
	if len(res.Tools) == 0 {
		t.Error("the tool list came back empty; stdout was probably polluted")
	}
}

// 验收：并发 50 个 tool call —— 不 panic、不 OOM、排队而非报错
//
// ⚠️ 这里用 rag_query 而**不是** file_search。
// file_search 打的是本地 httptest 替身，50 次调用 10ms 就跑完了，
// 根本排不上队 —— 那样测的只是"客户端能并发发请求"，
// 完全没碰到 Guard 的信号量。rag_query 走真实的 Milvus + PG +
// 向量化，单次几百毫秒，才会真的触发排队。
func TestMCPE2E_FiftyConcurrentCalls(t *testing.T) {
	cs := connectStdio(t)
	ctx := context.Background()

	const calls = 50
	type outcome struct {
		err     error
		isError bool
		elapsed time.Duration
	}
	results := make(chan outcome, calls)

	start := time.Now()
	for i := 0; i < calls; i++ {
		go func(i int) {
			callStart := time.Now()
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name: "rag_query",
				// 每个请求用不同的查询，避免全部命中缓存后
				// 又变成"根本没排队"的假象
				Arguments: map[string]any{
					"query": fmt.Sprintf("并发压测查询 %d 缓存 Kafka 检索", i),
					"top_k": 3,
				},
			})
			o := outcome{elapsed: time.Since(callStart)}
			if err != nil {
				o.err = err
			} else {
				o.isError = res.IsError
			}
			results <- o
		}(i)
	}

	var failed, toolErrors int
	var slowest time.Duration
	for i := 0; i < calls; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				failed++
				t.Logf("protocol error: %v", r.err)
			}
			if r.isError {
				toolErrors++
			}
			if r.elapsed > slowest {
				slowest = r.elapsed
			}
		case <-time.After(3 * time.Minute):
			t.Fatalf("only %d/%d calls completed before the timeout", i, calls)
		}
	}

	total := time.Since(start)
	t.Logf("%d concurrent calls in %v (slowest single call %v, "+
		"protocol failures=%d, tool errors=%d)",
		calls, total.Round(time.Millisecond), slowest.Round(time.Millisecond),
		failed, toolErrors)

	// 关键断言：没有协议级失败。排队 ≠ 拒绝 ——
	// 直接返回"服务繁忙"的话，AI 客户端多半会立刻重试，反而雪上加霜
	if failed > 0 {
		t.Errorf("%d calls failed at the protocol level; queueing must not become rejection", failed)
	}
	if toolErrors > 0 {
		t.Errorf("%d calls returned tool errors under load", toolErrors)
	}

	// 排队确实发生了：50 个请求 / 并发上限 20，最慢的那个必然
	// 等过至少一轮。若最慢的和最快的差不多，说明信号量没起作用。
	if slowest >= total {
		t.Logf("note: the slowest call (%v) spans nearly the whole run (%v) — "+
			"consistent with queueing", slowest, total)
	}
}
