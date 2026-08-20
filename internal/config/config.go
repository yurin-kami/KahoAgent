// Package config 负责加载与校验运行配置。
//
// 配置来源优先级：环境变量 > config.toml > 默认值。
// API Key 优先读环境变量，避免密钥写进文件被误提交。
package config

import (
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Debug      bool             `toml:"debug"`
	Server     ServerConfig     `toml:"server"`
	DB         DBConfig         `toml:"db"`
	Redis      RedisConfig      `toml:"redis"`
	Milvus     MilvusConfig     `toml:"milvus"`
	Kafka      KafkaConfig      `toml:"kafka"`
	LLM        LLMConfig        `toml:"llm"`
	MCP        MCPConfig        `toml:"mcp"`
	CloudKaho  CloudKahoConfig  `toml:"cloudkaho"`
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
}

type LLMConfig struct {
	Provider string `toml:"provider"`
	APIKey   string `toml:"api_key"`
	BaseURL  string `toml:"base_url"`
	Model    string `toml:"model"`

	EmbeddingModel string `toml:"embedding_model"`

	// EmbeddingDim 必须与 EmbeddingModel 的实际输出维度一致。
	// Milvus 建表时用它定义 FloatVector 字段，不一致会导致插入失败。
	EmbeddingDim int `toml:"embedding_dim"`
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
	return nil
}
