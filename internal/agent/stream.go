package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// Chunk 是消费事件流时产出的一个片段。
//
// 把 adk 的事件翻译成这个扁平结构，是为了让 handler 层
// 不必理解 TypedAgentEvent 的泛型嵌套。
type Chunk struct {
	// Text 是模型输出的增量文本（可能为空）
	Text string

	// ToolName 非空表示这是一次工具调用的结果
	ToolName string

	// Err 非空表示出错。出错后迭代即终止。
	Err error
}

// IsToolResult 区分工具结果与模型输出。
// handler 通常只把模型输出推给用户，工具结果仅用于日志。
func (c Chunk) IsToolResult() bool { return c.ToolName != "" }

// Consume 遍历事件流，对每个片段调用 fn。
//
// fn 返回 error 时停止消费并返回该错误（用于客户端断开等场景）。
//
// ⚠️ 事件结构经 eino@v0.9.14 源码核对：
//   - 迭代用 iter.Next() (T, bool)，没有 Recv/Close
//   - 文本在 event.Output.MessageOutput 里，不在 event.Message
//     （README 里的 event.Message.Content 是过时写法，编译不过）
//   - 流式时读 MessageStream.Recv() 直到 io.EOF，每片是**增量**
//   - 执行错误在 event.Err，不通过返回值传递 —— 不检查就会被静默吞掉
func Consume(ctx context.Context, iter *adk.AsyncIterator[*adk.AgentEvent], fn func(Chunk) error) error {
	if iter == nil {
		return fmt.Errorf("iterator must not be nil")
	}

	for {
		// 客户端断开时立刻停止，不再拉取后续事件
		if err := ctx.Err(); err != nil {
			return err
		}

		event, ok := iter.Next()
		if !ok {
			return nil // 正常结束
		}
		if event == nil {
			continue
		}
		if event.Err != nil {
			// 把错误交给 fn 让上层有机会告知用户，然后终止
			_ = fn(Chunk{Err: event.Err})
			return event.Err
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			// 有些事件只带 Action（如 Exit），没有输出内容
			continue
		}

		mo := event.Output.MessageOutput
		toolName := ""
		if mo.Role == schema.Tool {
			toolName = mo.ToolName
		}

		if !mo.IsStreaming {
			if mo.Message == nil || mo.Message.Content == "" {
				continue
			}
			if err := fn(Chunk{Text: mo.Message.Content, ToolName: toolName}); err != nil {
				return err
			}
			continue
		}

		if err := drain(ctx, mo.MessageStream, toolName, fn); err != nil {
			return err
		}
	}
}

// drain 消费一个消息流。
func drain(ctx context.Context, sr *schema.StreamReader[*schema.Message], toolName string, fn func(Chunk) error) error {
	if sr == nil {
		return nil
	}
	// ⚠️ 流必须关闭，否则上游 goroutine 会一直阻塞在 Send 上泄漏。
	// 提前 return 的路径也要走到这里，所以用 defer。
	defer sr.Close()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		msg, err := sr.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			_ = fn(Chunk{Err: err})
			return err
		}
		if msg == nil || msg.Content == "" {
			continue
		}
		if err := fn(Chunk{Text: msg.Content, ToolName: toolName}); err != nil {
			return err
		}
	}
}

// Collect 消费整个事件流并返回拼接后的最终回答。
//
// 只收集模型输出，丢弃工具结果 —— 后者是给模型看的中间产物，
// 不该出现在给用户的答案里。
func Collect(ctx context.Context, iter *adk.AsyncIterator[*adk.AgentEvent]) (string, error) {
	var sb strings.Builder
	err := Consume(ctx, iter, func(c Chunk) error {
		if c.Err == nil && !c.IsToolResult() {
			sb.WriteString(c.Text)
		}
		return nil
	})
	return sb.String(), err
}
