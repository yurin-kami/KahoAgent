package handler

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func msg(role, content string) *schema.Message {
	if role == "user" {
		return schema.UserMessage(content)
	}
	return schema.AssistantMessage(content, nil)
}

// TestTrimHistory_UnderBudget 预算充足时原样返回。
func TestTrimHistory_UnderBudget(t *testing.T) {
	in := []*schema.Message{msg("user", "你好"), msg("assistant", "你好，有什么可以帮你？")}
	out := trimHistory(in, 1000)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
}

// TestTrimHistory_DropsOldest 超预算时丢最旧的，保留最新的。
func TestTrimHistory_DropsOldest(t *testing.T) {
	var in []*schema.Message
	for i := 0; i < 6; i++ {
		in = append(in, msg("user", strings.Repeat("问", 50)))
		in = append(in, msg("assistant", strings.Repeat("答", 50)))
	}
	// 每轮 100 字，共 600 字；预算 250 → 最多保留 2 条完整消息
	out := trimHistory(in, 250)

	if len(out) == 0 {
		t.Fatal("history must not become empty")
	}
	total := 0
	for _, m := range out {
		total += len(m.Content)
	}
	if total > 250 {
		t.Fatalf("kept %d chars, want <= 250", total)
	}
	// 必须保留最新的那条（最后一条），否则等于把上下文截断在了过去
	if out[len(out)-1].Content != in[len(in)-1].Content {
		t.Fatal("newest message was dropped")
	}
}

// TestTrimHistory_KeepsLastEvenIfOverBudget 单条超预算时仍保留。
//
// 用户粘贴了一大段文档时，丢掉它意味着这轮对话直接失效；
// 宁可让上游报 context length 错误（用户能看懂），
// 也不要静默地把问题丢掉。
func TestTrimHistory_KeepsLastEvenIfOverBudget(t *testing.T) {
	in := []*schema.Message{
		msg("user", "旧问题"),
		msg("assistant", "旧回答"),
		msg("user", strings.Repeat("长", 500)),
	}
	out := trimHistory(in, 100)
	if len(out) != 1 || out[0].Content != in[2].Content {
		t.Fatalf("out = %d messages, want only the newest", len(out))
	}
}

// TestTrimHistory_StartsAtUser 切口落在 assistant 上时继续前移。
func TestTrimHistory_StartsAtUser(t *testing.T) {
	in := []*schema.Message{
		msg("user", strings.Repeat("a", 100)),
		msg("assistant", "旧回答"),
		msg("user", strings.Repeat("b", 100)),
		msg("assistant", "新回答"),
	}
	// 预算只够最后两条（user + assistant）
	out := trimHistory(in, 210)
	if len(out) == 0 {
		t.Fatal("empty result")
	}
	if out[0].Role != schema.User {
		t.Fatalf("history starts with role %q, want user", out[0].Role)
	}
}

// TestTrimHistory_NoBudget 预算为 0 表示不限制。
func TestTrimHistory_NoBudget(t *testing.T) {
	in := []*schema.Message{msg("user", "a"), msg("assistant", "b")}
	if got := trimHistory(in, 0); len(got) != 2 {
		t.Fatalf("len = %d, want 2 (unlimited)", len(got))
	}
}
