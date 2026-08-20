// Package mcpserver 把 Phase 3 的 Tool 通过 MCP 协议暴露给外部 AI 客户端。
//
// ⚠️ 本包**不包含任何业务逻辑**，只做协议转换与安全防护。
// 判断标准：如果在这里写了业务判断（比如"top_k 大于 20 就截断"），
// 说明设计错了 —— 应该回去改 internal/agent/tools 里的 Tool，
// 否则同一份规则会在 Agent 路径和 MCP 路径上各写一遍，迟早不一致。
package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/semaphore"
)

const (
	// defaultMaxConcurrent 是全局并发上限。
	//
	// 为什么要限：MCP Server 面向**不可信**的 AI 客户端。模型可能因为
	// prompt 注入或自身错误而疯狂调用工具，没有防护会打爆下游的
	// Milvus / PostgreSQL / CloudKaho —— 而它们是和主服务共用的。
	defaultMaxConcurrent = 20

	// defaultTimeout 是单次调用上限。
	//
	// 光有并发限制不够：一个卡死的调用会永久占住一个令牌，
	// 20 个卡死就等于整个 MCP Server 不可用。超时是并发控制的前提。
	defaultTimeout = 30 * time.Second
)

// Invoker 是被保护的调用，形状与 eino 的 InvokableRun 一致。
type Invoker func(ctx context.Context, argsJSON string) (string, error)

// Guard 为每个 tool 调用提供并发限制、超时与审计。
type Guard struct {
	sem     *semaphore.Weighted
	timeout time.Duration
	logger  *slog.Logger
}

type GuardOption func(*Guard)

func WithMaxConcurrent(n int) GuardOption {
	return func(g *Guard) {
		if n > 0 {
			g.sem = semaphore.NewWeighted(int64(n))
		}
	}
}

func WithTimeout(d time.Duration) GuardOption {
	return func(g *Guard) {
		if d > 0 {
			g.timeout = d
		}
	}
}

func WithGuardLogger(l *slog.Logger) GuardOption {
	return func(g *Guard) {
		if l != nil {
			g.logger = l
		}
	}
}

func NewGuard(opts ...GuardOption) *Guard {
	g := &Guard{
		sem:     semaphore.NewWeighted(defaultMaxConcurrent),
		timeout: defaultTimeout,
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// Wrap 给一次工具调用套上并发、超时与审计。
func (g *Guard) Wrap(name string, fn Invoker) Invoker {
	return func(ctx context.Context, argsJSON string) (string, error) {
		start := time.Now()

		// 1. 并发控制：拿不到令牌就**排队**，而不是直接拒绝。
		//
		// 为什么排队而非快速失败：调用方是 AI 客户端，收到"服务繁忙"
		// 多半会立刻重试，反而加重负载；而工具调用本来就是秒级的，
		// 排队几百毫秒对用户无感。Acquire 会在 ctx 取消时返回，
		// 所以不会无限等下去。
		if err := g.sem.Acquire(ctx, 1); err != nil {
			g.audit(name, argsJSON, time.Since(start), "rejected", err)
			return "", fmt.Errorf("服务繁忙，请稍后重试")
		}
		defer g.sem.Release(1)

		// 2. 超时：防止单个慢调用占住并发额度。
		//
		// ⚠️ 必须在拿到令牌**之后**才开始计时。放在前面的话，
		// 排队时间会被算进超时预算，负载高时所有调用都会超时 ——
		// 明明只是排队慢，却表现为工具全线故障。
		callCtx, cancel := context.WithTimeout(ctx, g.timeout)
		defer cancel()

		result, err := fn(callCtx, argsJSON)

		status := "ok"
		if err != nil {
			status = "error"
		}
		g.audit(name, argsJSON, time.Since(start), status, err)
		return result, err
	}
}

// audit 记录一次调用。
//
// ⚠️ 日志必须走 stderr。stdio 模式下 stdout 是 JSON-RPC 协议通道，
// 往里写任何非协议内容都会让客户端解析失败 —— 这个坑在 Spike S4
// 已经踩过一次，见 scripts/spike/mcp/main.go。
func (g *Guard) audit(name, argsJSON string, elapsed time.Duration, status string, err error) {
	attrs := []any{
		"tool", name,
		"duration_ms", elapsed.Milliseconds(),
		"status", status,
		// ⚠️ 参数只记哈希，不记原文。
		// 用户的查询词可能包含个人信息、内部文件名甚至误粘贴的密码，
		// 审计日志通常保留很久且访问面更广，不该成为泄露源。
		// 哈希仍可用于「同样的调用出现了多少次」这类排查。
		"args_hash", hashArgs(argsJSON),
	}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	g.logger.Info("tool_call", attrs...)
}

// hashArgs 取参数的短哈希，用于关联同类调用而不泄露内容。
func hashArgs(argsJSON string) string {
	sum := sha256.Sum256([]byte(argsJSON))
	return hex.EncodeToString(sum[:8])
}
