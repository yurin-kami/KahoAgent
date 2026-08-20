//go:build loadtest

// 压测 HTTP 与 MCP 两条路径。
//
//	go run -tags=loadtest ./scripts/bench/load.go -mode=http -rate=50 -duration=30s
//	go run -tags=loadtest ./scripts/bench/load.go -mode=mcp -concurrency=50 -requests=500
//
// 为什么不用 vegeta（计划里写的工具）：
//
//  1. /api/chat 是 SSE 长连接。vegeta 测的是「请求到响应结束」的总时长，
//     而 SSE 的响应要等整段生成完才结束 —— 测出来的是生成耗时，
//     不是服务的吞吐能力，两者完全不是一回事。
//  2. 真正需要知道分位数的是**首字延迟**，那要求边收边计时，
//     vegeta 给不了。
//
// 所以自己写：按固定速率发压（开环，不等上一个请求返回），
// 分别记首字延迟和完成延迟。开环很重要 —— 闭环压测在服务变慢时
// 会自动降低发压速率，把过载现象掩盖掉。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	var (
		mode        = flag.String("mode", "http", "http 或 mcp")
		target      = flag.String("target", "http://127.0.0.1:8080", "服务地址")
		rate        = flag.Int("rate", 20, "http 模式：每秒发起的请求数")
		duration    = flag.Duration("duration", 30*time.Second, "http 模式：持续时间")
		concurrency = flag.Int("concurrency", 50, "mcp 模式：并发数")
		requests    = flag.Int("requests", 500, "mcp 模式：总请求数")
		out         = flag.String("out", "", "结果输出文件（默认 stdout）")
	)
	flag.Parse()

	var (
		report string
		err    error
	)
	switch *mode {
	case "http":
		report, err = runHTTP(*target, *rate, *duration)
	case "mcp":
		report, err = runMCP(*concurrency, *requests)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}

	if *out == "" {
		fmt.Print(report)
		return
	}
	if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("written to %s\n", *out)
}

// ---- 统计 ----

type sample struct {
	firstToken time.Duration
	total      time.Duration
	err        error
}

type stats struct {
	values []time.Duration
}

func (s *stats) add(d time.Duration) { s.values = append(s.values, d) }

// percentile 用 nearest-rank 法，与 Phase 2 的评估脚本保持一致 ——
// 换算法会让两处的 P99 不可比。
func (s *stats) percentile(p float64) time.Duration {
	if len(s.values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), s.values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	rank := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func (s *stats) mean() time.Duration {
	if len(s.values) == 0 {
		return 0
	}
	var sum time.Duration
	for _, v := range s.values {
		sum += v
	}
	return sum / time.Duration(len(s.values))
}

func ms(d time.Duration) string {
	return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
}

// ---- HTTP ----

var httpQueries = []string{
	"缓存穿透怎么解决",
	"Kafka 消息怎么保证不丢",
	"Redis 的持久化机制有哪些",
	"什么是缓存雪崩",
	"布隆过滤器的原理",
}

func runHTTP(target string, rate int, duration time.Duration) (string, error) {
	url := strings.TrimRight(target, "/") + "/api/chat"

	// 先探活，避免压了半天才发现服务没起来
	if err := ping(strings.TrimRight(target, "/") + "/healthz"); err != nil {
		return "", fmt.Errorf("service is not reachable at %s: %w", target, err)
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	results := make(chan sample, rate*int(duration.Seconds())+rate)

	var wg sync.WaitGroup
	ticker := time.NewTicker(time.Second / time.Duration(rate))
	defer ticker.Stop()

	deadline := time.After(duration)
	start := time.Now()
	var issued int

loop:
	for {
		select {
		case <-deadline:
			break loop
		case <-ticker.C:
			// ⚠️ 开环：不等上一个请求返回就发下一个。
			// 闭环压测在服务变慢时会自动降速，把过载现象掩盖掉 ——
			// 而线上真实流量是不会「体谅」你的。
			q := httpQueries[issued%len(httpQueries)]
			issued++
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- oneChat(client, url, q)
			}()
		}
	}
	wg.Wait()
	close(results)
	elapsed := time.Since(start)

	var (
		first, total stats
		failures     int
	)
	for r := range results {
		if r.err != nil {
			failures++
			continue
		}
		first.add(r.firstToken)
		total.add(r.total)
	}

	ok := len(total.values)
	var sb strings.Builder
	fmt.Fprintf(&sb, "# HTTP 压测（/api/chat，SSE）\n\n")
	fmt.Fprintf(&sb, "目标速率  %d req/s，持续 %v\n", rate, duration)
	fmt.Fprintf(&sb, "实际发出  %d 个请求，耗时 %v\n", issued, elapsed.Round(time.Millisecond))
	fmt.Fprintf(&sb, "成功      %d，失败 %d\n", ok, failures)
	if ok > 0 {
		fmt.Fprintf(&sb, "实际吞吐  %.1f req/s（完成数 / 总耗时）\n\n", float64(ok)/elapsed.Seconds())
		fmt.Fprintf(&sb, "首字延迟  P50 %s  P95 %s  P99 %s  mean %s\n",
			ms(first.percentile(50)), ms(first.percentile(95)),
			ms(first.percentile(99)), ms(first.mean()))
		fmt.Fprintf(&sb, "完成延迟  P50 %s  P95 %s  P99 %s  mean %s\n",
			ms(total.percentile(50)), ms(total.percentile(95)),
			ms(total.percentile(99)), ms(total.mean()))
	}
	return sb.String(), nil
}

func ping(url string) error {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

// oneChat 发一轮对话并边收边计时。
func oneChat(client *http.Client, url, query string) sample {
	body, _ := json.Marshal(map[string]string{"message": query})

	start := time.Now()
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return sample{err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return sample{err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 120))}
	}

	var s sample
	var gotFirst bool
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
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "delta" && !gotFirst {
			gotFirst = true
			s.firstToken = time.Since(start)
		}
		if ev.Type == "error" {
			s.err = fmt.Errorf("stream error: %s", ev.Text)
		}
	}
	if err := sc.Err(); err != nil {
		return sample{err: err}
	}
	if !gotFirst && s.err == nil {
		s.err = fmt.Errorf("no content received")
	}
	s.total = time.Since(start)
	return s
}

// ---- MCP ----

func runMCP(concurrency, requests int) (string, error) {
	ctx := context.Background()

	// 用 stdio 起子进程，与 Claude Desktop 相同的路径
	cmd := mcpCommand()
	client := mcp.NewClient(&mcp.Implementation{Name: "loadtest", Version: "0.1.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return "", fmt.Errorf("connect to the MCP server: %w", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("list tools: %w", err)
	}

	sem := make(chan struct{}, concurrency)
	results := make(chan sample, requests)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			callStart := time.Now()
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name: "rag_query",
				Arguments: map[string]any{
					"query": httpQueries[i%len(httpQueries)],
					"top_k": 3,
				},
			})
			s := sample{total: time.Since(callStart)}
			switch {
			case err != nil:
				s.err = err
			case res.IsError:
				s.err = fmt.Errorf("tool reported an error")
			}
			results <- s
		}(i)
	}
	wg.Wait()
	close(results)
	elapsed := time.Since(start)

	var st stats
	var failures int
	for r := range results {
		if r.err != nil {
			failures++
			continue
		}
		st.add(r.total)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# MCP 压测（rag_query over stdio）\n\n")
	fmt.Fprintf(&sb, "工具数    %d\n", len(tools.Tools))
	fmt.Fprintf(&sb, "并发      %d，总请求 %d\n", concurrency, requests)
	fmt.Fprintf(&sb, "耗时      %v\n", elapsed.Round(time.Millisecond))
	fmt.Fprintf(&sb, "成功      %d，失败 %d\n", len(st.values), failures)
	if len(st.values) > 0 {
		fmt.Fprintf(&sb, "吞吐      %.1f call/s\n\n", float64(len(st.values))/elapsed.Seconds())
		fmt.Fprintf(&sb, "调用延迟  P50 %s  P95 %s  P99 %s  mean %s\n",
			ms(st.percentile(50)), ms(st.percentile(95)),
			ms(st.percentile(99)), ms(st.mean()))
		fmt.Fprintf(&sb, "\n注：并发上限由 Guard 控制（默认 20），"+
			"超出部分排队 —— 所以 P99 里包含排队时间，这是刻意的：\n"+
			"    用户感知到的就是「从发起到拿到结果」，排队也是等待。\n")
	}
	return sb.String(), nil
}

// mcpCommand 构造启动 MCP Server 的子进程。
//
// 用 `go run` 而不是预编译的二进制：压测脚本应当能直接跑，
// 不要求使用者先记得去 build。首次启动会慢几秒（编译），
// 但那发生在计时开始之前。
func mcpCommand() *exec.Cmd {
	cmd := exec.Command("go", "run", "./cmd/mcp")
	// 日志走 stderr（stdout 是协议通道），转发到本进程的 stderr
	// 便于观察 Guard 的审计输出
	cmd.Stderr = os.Stderr
	return cmd
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
