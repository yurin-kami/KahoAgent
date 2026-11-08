// Package config 负责加载与校验运行配置。
//
// 配置来源优先级：环境变量 > config.toml > 默认值。
// API Key 优先读环境变量，避免密钥写进文件被误提交。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Debug     bool            `toml:"debug"`
	Server    ServerConfig    `toml:"server"`
	DB        DBConfig        `toml:"db"`
	Redis     RedisConfig     `toml:"redis"`
	Milvus    MilvusConfig    `toml:"milvus"`
	Kafka     KafkaConfig     `toml:"kafka"`
	LLM       LLMConfig       `toml:"llm"`
	MCP       MCPConfig       `toml:"mcp"`
	CloudKaho CloudKahoConfig `toml:"cloudkaho"`
	OTel      OTelConfig      `toml:"otel"`
}

// OTelConfig 是链路追踪的配置。
//
// 默认关闭：本地开发不该被迫起一个 collector。关闭时全局 provider
// 保持 OTel 的 noop 实现，埋点调用是空操作。
type OTelConfig struct {
	Enabled bool `toml:"enabled"`
	// Endpoint 不带 scheme（如 127.0.0.1:4318）。
	// 带 http:// 会让 OTLP exporter 报 "invalid endpoint"。
	Endpoint    string  `toml:"endpoint"`
	Insecure    bool    `toml:"insecure"`
	ServiceName string  `toml:"service_name"`
	SampleRatio float64 `toml:"sample_ratio"`
}

// CloudKahoConfig 是云盘服务的连接配置。
//
// Token 优先读环境变量：它是用户凭证，写进配置文件容易误提交。
// 留空时文件类工具会被禁用（而不是报错退出）——
// 检索能力不依赖云盘，没配也应该能正常问答。
type CloudKahoConfig struct {
	BaseURL string `toml:"base_url"`
	Token   string `toml:"token"`
}

type ServerConfig struct {
	Port int `toml:"port"`
}

type DBConfig struct {
	DSN string `toml:"dsn"`
}

type RedisConfig struct {
	Addr     string `toml:"addr"`
	Password string `toml:"password"`
	DB       int    `toml:"db"`
}

type MilvusConfig struct {
	Addr       string `toml:"addr"`
	Collection string `toml:"collection"`
}

type KafkaConfig struct {
	Brokers []string `toml:"brokers"`
	Topic   string   `toml:"topic"`
	GroupID string   `toml:"group_id"`

	// DLQTopic 是死信 topic。留空时默认为 topic + ".dlq"；
	// 显式设为 "-" 可关闭死信（退化为无限重试的旧行为）。
	DLQTopic string `toml:"dlq_topic"`

	// MaxAttempts 是进死信前的最大尝试次数。
	MaxAttempts int `toml:"max_attempts"`
}

type LLMConfig struct {
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
	BaseURL  string `toml:"base_url"`
	Model    string `toml:"model"`

	// RerankModel 是精排使用的模型，默认与 Model 相同。
	//
	// 为什么要能单独配：推理模型（如 deepseek-flash）会把 token 花在
	// reasoning 上，而精排只需要一个 0~10 的数字 —— 用推理模型既慢又
	// 容易在 max_tokens 内拿不到 content。精排换一个非推理模型（如
	// deepseek-chat / qwen-turbo）能显著降低延迟。
	RerankModel string `toml:"rerank_model"`

	EmbeddingModel string `toml:"embedding_model"`

	// EmbeddingAPIKey / EmbeddingBaseURL 允许 embedding 走**另一家**服务。
	//
	// 为什么需要：DeepSeek 只提供 chat，没有 embedding 接口。
	// 把对话切到 DeepSeek 之后，向量化仍然要指向 DashScope / SiliconFlow
	// 这类有 embedding 的服务。留空时回退到主模型的 key / base_url，
	// 兼容原来的单供应商配置。
	EmbeddingAPIKey  string `toml:"embedding_api_key"`
	EmbeddingBaseURL string `toml:"embedding_base_url"`

	// EmbeddingDim 必须与 EmbeddingModel 的实际输出维度一致。
	// Milvus 建表时用它定义 FloatVector 字段，不一致会导致插入失败。
	EmbeddingDim int `toml:"embedding_dim"`

	// Fallback* 是可选的主备切换配置。
	//
	// 只在主模型返回 429 / 5xx 时启用：DashScope 的 qwen-plus 与
	// qwen-turbo 是独立的并发配额，换模型往往能立刻恢复。
	// 留空则不启用，行为与之前完全一致。
	FallbackModel   string `toml:"fallback_model"`
	FallbackBaseURL string `toml:"fallback_base_url"`
	FallbackAPIKey  string `toml:"fallback_api_key"`

	// MaxHistoryChars 限制送入模型的历史字符数（近似 token 预算）。
	//
	// 中文大致 1 字 ≈ 1 token，12000 字约等于一次请求 12k token 的
	// 输入上限。超限时**从最旧的消息开始丢**，而不是报错 ——
	// 用户不该因为聊得久而突然不能用。
	MaxHistoryChars int `toml:"max_history_chars"`
}

type MCPConfig struct {
	Transport string `toml:"transport"`
	Port      int    `toml:"port"`
}

// apiKeyEnvVars 按顺序尝试的环境变量名。
// 配置文件里 api_key 为空时从这里回退。
var apiKeyEnvVars = []string{
	"DASHSCOPE_API_KEY",
	"OPENAI_API_KEY",
	"DEEPSEEK_API_KEY",
}

// DefaultPath 返回配置文件路径：优先取 KAHO_CONFIG，否则用工作目录下的
// config.toml。
//
// ⚠️ 容器里必须能改这个路径。config.toml 含 API Key，绝不打进镜像，
// 只能挂载进来 —— 而挂载点通常不是工作目录（/app 里除了二进制什么都没有）。
// 路径写死的话，镜像就只能靠「把文件挂到 /app/config.toml」这一种方式跑，
// Kubernetes 的 ConfigMap/Secret 一般挂在 /etc/kaho 之类的位置。
//
// 不放进 Load 里：Load 是纯函数，给什么路径读什么路径，
// 测试才能稳定地指向自己的固件；环境变量只在 main 的入口处生效。
func DefaultPath() string {
	if p := os.Getenv("KAHO_CONFIG"); p != "" {
		return p
	}
	return "config.toml"
}

// Load 从 path 读取配置，应用默认值与环境变量回退，并校验必填项。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	cfg.resolveAPIKey()

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Milvus.Addr == "" {
		c.Milvus.Addr = "127.0.0.1:19530"
	}
	if c.Milvus.Collection == "" {
		c.Milvus.Collection = "kaho_documents"
	}
	if c.Kafka.Topic == "" {
		c.Kafka.Topic = "document-processing"
	}
	if c.Kafka.GroupID == "" {
		c.Kafka.GroupID = "kahoagent-worker"
	}
	if c.Kafka.DLQTopic == "" {
		c.Kafka.DLQTopic = c.Kafka.Topic + ".dlq"
	}
	if c.Kafka.MaxAttempts <= 0 {
		c.Kafka.MaxAttempts = 5
	}
	if len(c.Kafka.Brokers) == 0 {
		c.Kafka.Brokers = []string{"127.0.0.1:9092"}
	}
	if c.MCP.Transport == "" {
		c.MCP.Transport = "stdio"
	}
	if c.MCP.Port == 0 {
		c.MCP.Port = 3000
	}
	if c.CloudKaho.BaseURL == "" {
		// CloudKaho 硬编码监听 :8080（cmd/main.go），不可配置
		c.CloudKaho.BaseURL = "http://127.0.0.1:8080"
	}
	if c.OTel.Endpoint == "" {
		// 默认指向本机 Jaeger 的 OTLP/HTTP 端口，并配套明文传输。
		// 只有「没配 endpoint」时才自动开 insecure；用户显式配了远端地址，
		// insecure 就按他自己的值来，不替他做安全相关的决定。
		c.OTel.Endpoint = "127.0.0.1:4318"
		c.OTel.Insecure = true
	}
	if c.OTel.ServiceName == "" {
		c.OTel.ServiceName = "kahoagent"
	}
	if c.OTel.SampleRatio == 0 {
		c.OTel.SampleRatio = 1.0
	}
}

// resolveAPIKey 在配置未提供 api_key 时按序尝试环境变量。
func (c *Config) resolveAPIKey() {
	if c.LLM.APIKey == "" {
		for _, name := range apiKeyEnvVars {
			if v := os.Getenv(name); v != "" {
				c.LLM.APIKey = v
				break
			}
		}
	}
	// CloudKaho token 同理：它是用户凭证，优先从环境变量取
	if c.CloudKaho.Token == "" {
		c.CloudKaho.Token = os.Getenv("CLOUDKAHO_TOKEN")
	}
	// ⚠️ 环境变量**覆盖**配置文件里的 base_url，与上面几项「留空才回退」
	// 的规则相反。理由：指向哪个 CloudKaho 是**部署环境**的属性，
	// 不是代码的属性 —— 容器里、CI 里、测试里各不相同，
	// 而 config.toml 是同一份。若沿用「留空才回退」，
	// 配置文件一旦写了值就再也没法从外部改。
	if v := os.Getenv("CLOUDKAHO_BASE_URL"); v != "" {
		c.CloudKaho.BaseURL = v
	}
}

// validate 检查缺失会导致运行时才暴露的必填项。
// 早失败好过跑到一半才报错。
func (c *Config) validate() error {
	if c.LLM.APIKey == "" {
		return fmt.Errorf("llm.api_key is empty and none of %v is set", apiKeyEnvVars)
	}
	if c.LLM.BaseURL == "" {
		return fmt.Errorf("llm.base_url is required")
	}
	if c.LLM.Model == "" {
		return fmt.Errorf("llm.model is required")
	}
	if c.LLM.EmbeddingModel == "" {
		return fmt.Errorf("llm.embedding_model is required")
	}
	if c.LLM.EmbeddingDim <= 0 {
		return fmt.Errorf("llm.embedding_dim must be > 0 (must match the embedding model output)")
	}
	// DeepSeek 只提供 chat，没有 /embeddings。如果向量化仍然指向它，
	// 启动时就明确报错 —— 否则要等到第一次入库/检索才 404，
	// 而且错误信息只会是 "HTTP 404"，很难联想到是供应商能力缺失。
	if isDeepSeekHost(c.LLM.EmbeddingURL()) {
		return fmt.Errorf(
			"llm.embedding_base_url 指向 DeepSeek，但 DeepSeek 不提供 embedding 接口；" +
				"请配置 embedding_base_url / embedding_api_key（如 DashScope、SiliconFlow）")
	}
	return nil
}

// isDeepSeekHost 判断 URL 是否指向 DeepSeek 官方域名。
func isDeepSeekHost(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "deepseek.com" || strings.HasSuffix(host, ".deepseek.com")
}

// Fallback 返回备用上游的完整参数。
//
// 未配置 fallback_model 时 ok=false，调用方应保持单上游行为。
// base_url / api_key 留空时回退到主模型的对应值 ——
// 最常见的用法就是「同供应商换个更小的模型」。
func (c *LLMConfig) Fallback() (model, baseURL, apiKey string, ok bool) {
	if c.FallbackModel == "" {
		return "", "", "", false
	}
	baseURL = c.FallbackBaseURL
	if baseURL == "" {
		baseURL = c.BaseURL
	}
	apiKey = c.FallbackAPIKey
	if apiKey == "" {
		apiKey = c.APIKey
	}
	return c.FallbackModel, baseURL, apiKey, true
}

// HistoryBudget 返回历史字符预算，未配置时用默认值。
func (c *LLMConfig) HistoryBudget() int {
	if c.MaxHistoryChars > 0 {
		return c.MaxHistoryChars
	}
	return 12000
}

// RerankModelName 返回精排模型名，未配置时回退到主模型。
func (c *LLMConfig) RerankModelName() string {
	if c.RerankModel != "" {
		return c.RerankModel
	}
	return c.Model
}

// EmbeddingKey 返回 embedding 使用的 API Key。
//
// 未单独配置时回退到主 key —— 单供应商场景（DashScope 同时做 chat 与
// embedding）不需要多写一行配置。
func (c *LLMConfig) EmbeddingKey() string {
	if c.EmbeddingAPIKey != "" {
		return c.EmbeddingAPIKey
	}
	return c.APIKey
}

// EmbeddingURL 返回 embedding 服务的 base_url。
func (c *LLMConfig) EmbeddingURL() string {
	if c.EmbeddingBaseURL != "" {
		return c.EmbeddingBaseURL
	}
	return c.BaseURL
}
