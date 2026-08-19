package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// EinoChatModel 把 OpenAI 兼容的 chat completions 接口适配成 Eino 的
// model.BaseChatModel，从而能塞进 adk.ChatModelAgent。
//
// 为什么不复用 ChatModel：
// ChatModel 只做「一句话进、一句话出」的补全，既不支持 tool calling
// 也不支持流式。Agent 两者都必须有 —— 没有 tool calling 就不是 Agent，
// 没有流式则首字延迟等于整段生成时间。二者的请求/响应结构差异很大，
// 硬塞进同一个类型会让两边都变难读，因此另起一个。
//
// ⚠️ 接口形状经 eino@v0.9.14 源码核对：
//   - model.BaseChatModel 是**类型别名** `BaseModel[*schema.Message]`，
//     实现 Generate + Stream 即可满足
//   - ChatModelAgentConfig.Model 的类型是 BaseModel[M]，**不是**
//     ToolCallingChatModel。工具不通过 WithTools 绑定，而是每次调用时
//     由 Agent 通过 model.WithTools 选项传入 —— 所以 Generate/Stream
//     必须从 opts 里读工具列表。
type EinoChatModel struct {
	apiKey  string
	baseURL string
	model   string

	temperature float32
	client      *http.Client

	// tools 是通过 WithTools 预绑定的工具。
	// Agent 走的是「调用时传选项」的路径，此字段仅为兼容
	// ToolCallingChatModel 接口而存在，调用选项优先级更高。
	tools []*schema.ToolInfo
}

// 编译期断言：两个接口都要满足。
// BaseChatModel 是 Agent 实际要求的；ToolCallingChatModel 是为了
// 让本类型也能用在直接绑定工具的场景（如 Phase 4 的 MCP）。
var (
	_ model.BaseChatModel        = (*EinoChatModel)(nil)
	_ model.ToolCallingChatModel = (*EinoChatModel)(nil)
)

// defaultAgentTimeout 比 defaultChatTimeout 长。
// Agent 的一次生成可能要输出几百字，且 ReAct 循环里每轮都要调模型，
// 60 秒对流式场景偏紧。
const defaultAgentTimeout = 180 * time.Second

type EinoChatOption func(*EinoChatModel)

func WithEinoTemperature(t float32) EinoChatOption {
	return func(c *EinoChatModel) { c.temperature = t }
}

func WithEinoHTTPClient(h *http.Client) EinoChatOption {
	return func(c *EinoChatModel) {
		if h != nil {
			c.client = h
		}
	}
}

func NewEinoChatModel(apiKey, baseURL, modelName string, opts ...EinoChatOption) *EinoChatModel {
	c := &EinoChatModel{
		apiKey:  apiKey,
		baseURL: baseURL,
		model:   modelName,
		// Agent 场景不能像精排那样固定 0：温度 0 会让模型在
		// 「该调工具还是该回答」上过于死板，且回答读起来很生硬。
		temperature: 0.7,
		client:      &http.Client{Timeout: defaultAgentTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithTools 返回绑定了工具的副本。
//
// ⚠️ 必须返回**副本**而非修改自身：eino 的接口语义是
// 「返回一个新的模型」，若原地修改，多个 Agent 共享同一实例时
// 会互相覆盖工具列表。
func (c *EinoChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	clone := *c
	clone.tools = tools
	return &clone, nil
}

// ---- 线路格式 ----

type agentToolCall struct {
	// Index 用于流式增量拼接：同一个工具调用的参数会分多个 chunk 到达，
	// 靠 index 而非 id 归并（首个 chunk 之后的 chunk 往往没有 id）。
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type agentMessage struct {
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []agentToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 只在 role=tool 时出现，用于把工具结果关联回请求
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

type agentToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

type agentRequest struct {
	Model       string         `json:"model"`
	Messages    []agentMessage `json:"messages"`
	Temperature float32        `json:"temperature"`
	Stream      bool           `json:"stream,omitempty"`
	Tools       []agentToolDef `json:"tools,omitempty"`
}

type agentChoice struct {
	Message      agentMessage `json:"message"`
	Delta        agentMessage `json:"delta"`
	FinishReason string       `json:"finish_reason"`
}

type agentResponse struct {
	Choices []agentChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Generate 一次性生成，实现 model.BaseChatModel。
func (c *EinoChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	body, err := c.buildRequest(input, opts, false)
	if err != nil {
		return nil, err
	}

	raw, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}

	var parsed agentResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode response: %w (raw: %s)", err, truncate(string(raw), 300))
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("api error: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("response contained no choices")
	}

	return toSchemaMessage(parsed.Choices[0].Message), nil
}

// Stream 流式生成，实现 model.BaseChatModel。
//
// 返回的 StreamReader 中每个元素是**增量片段**而非累计内容 ——
// 这是 eino 的约定，上层用 schema.ConcatMessages 或自行拼接。
func (c *EinoChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	body, err := c.buildRequest(input, opts, true)
	if err != nil {
		return nil, err
	}

	req, err := c.newHTTPRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	// SSE 需要显式声明，某些网关据此决定是否分块传输
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(errBody), 300))
	}

	sr, sw := schema.Pipe[*schema.Message](streamBuffer)

	// ⚠️ resp.Body 的关闭必须在这个 goroutine 里，不能用 defer 放在
	// Stream 函数上 —— 函数会立刻返回，而 body 还要继续读。
	go func() {
		defer resp.Body.Close()
		defer sw.Close()
		c.pumpSSE(ctx, resp.Body, sw)
	}()

	return sr, nil
}

// streamBuffer 是流式管道的缓冲大小。
// 给 16 而非 1：消费端（SSE handler）偶尔会被网络阻塞，
// 有缓冲可以让上游继续接收，避免反压到 HTTP 连接上。
const streamBuffer = 16

// pumpSSE 解析 SSE 流并把增量消息推进管道。
func (c *EinoChatModel) pumpSSE(ctx context.Context, body io.Reader, sw *schema.StreamWriter[*schema.Message]) {
	scanner := bufio.NewScanner(body)
	// 默认 64KB 上限对单条 SSE 行足够，但工具调用的参数可能很长，
	// 放宽到 1MB 以防超长行被截断成非法 JSON。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		// context 取消时立刻停止，不再往管道里写
		if ctx.Err() != nil {
			return
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return
		}

		var chunk agentResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 单个畸形分片不该终止整个流 —— 后面的分片可能是好的。
			// 但也不能静默，否则内容悄悄少一段没人知道。
			sw.Send(nil, fmt.Errorf("malformed SSE chunk: %w", err))
			continue
		}
		if chunk.Error != nil {
			sw.Send(nil, fmt.Errorf("api error: %s", chunk.Error.Message))
			return
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta
		// 跳过纯粹的心跳/占位分片，避免下游收到一堆空消息
		if delta.Content == "" && len(delta.ToolCalls) == 0 {
			continue
		}
		if closed := sw.Send(toSchemaMessage(delta), nil); closed {
			return // 下游已放弃，停止解析
		}
	}

	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		sw.Send(nil, fmt.Errorf("read stream: %w", err))
	}
}

// buildRequest 组装请求体。
//
// ⚠️ 工具来源有两处，优先级不能搞反：
// 调用选项（Agent 走这条）> WithTools 预绑定。
// 反过来的话，Agent 每轮传入的工具会被旧的预绑定覆盖，
// 表现为模型突然「忘记」自己有工具。
func (c *EinoChatModel) buildRequest(input []*schema.Message, opts []model.Option, stream bool) ([]byte, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("input must contain at least one message")
	}

	common := model.GetCommonOptions(&model.Options{}, opts...)

	tools := c.tools
	if len(common.Tools) > 0 {
		tools = common.Tools
	}

	temperature := c.temperature
	if common.Temperature != nil {
		temperature = *common.Temperature
	}
	modelName := c.model
	if common.Model != nil && *common.Model != "" {
		modelName = *common.Model
	}

	msgs := make([]agentMessage, 0, len(input))
	for _, m := range input {
		if m == nil {
			continue
		}
		msgs = append(msgs, fromSchemaMessage(m))
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input contained only nil messages")
	}

	req := agentRequest{
		Model:       modelName,
		Messages:    msgs,
		Temperature: temperature,
		Stream:      stream,
	}

	for _, t := range tools {
		def, err := toToolDef(t)
		if err != nil {
			return nil, err
		}
		req.Tools = append(req.Tools, def)
	}

	return json.Marshal(req)
}

func (c *EinoChatModel) newHTTPRequest(ctx context.Context, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return req, nil
}

func (c *EinoChatModel) do(ctx context.Context, body []byte) ([]byte, error) {
	req, err := c.newHTTPRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	return raw, nil
}

// ---- 类型转换 ----

func toToolDef(t *schema.ToolInfo) (agentToolDef, error) {
	var def agentToolDef
	if t == nil {
		return def, fmt.Errorf("tool info must not be nil")
	}
	def.Type = "function"
	def.Function.Name = t.Name
	def.Function.Description = t.Desc

	// ParamsOneOf 是内嵌字段且内部字段不导出，只能通过 ToJSONSchema 取出。
	// 无参数工具的 ParamsOneOf 为 nil，此时给一个空 object ——
	// 省略 parameters 字段会被部分网关判为非法。
	if t.ParamsOneOf == nil {
		def.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		return def, nil
	}
	js, err := t.ParamsOneOf.ToJSONSchema()
	if err != nil {
		return def, fmt.Errorf("tool %q: convert params: %w", t.Name, err)
	}
	def.Function.Parameters = js
	return def, nil
}

func fromSchemaMessage(m *schema.Message) agentMessage {
	out := agentMessage{
		Role:       string(m.Role),
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	for _, tc := range m.ToolCalls {
		var c agentToolCall
		c.Index = tc.Index
		c.ID = tc.ID
		c.Type = tc.Type
		if c.Type == "" {
			c.Type = "function"
		}
		c.Function.Name = tc.Function.Name
		c.Function.Arguments = tc.Function.Arguments
		out.ToolCalls = append(out.ToolCalls, c)
	}
	return out
}

func toSchemaMessage(m agentMessage) *schema.Message {
	role := schema.RoleType(m.Role)
	if role == "" {
		// 流式分片的 delta 通常只在首片带 role，后续片没有。
		// 缺省按 assistant 处理 —— 模型输出只可能是这个角色。
		role = schema.Assistant
	}

	out := &schema.Message{
		Role:       role,
		Content:    m.Content,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	}
	for _, tc := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, schema.ToolCall{
			Index: tc.Index,
			ID:    tc.ID,
			Type:  tc.Type,
			Function: schema.FunctionCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	return out
}
