package config

import (
	"os"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Debug  bool         `toml:"debug"`
	Server ServerConfig `toml:"server"`
	DB     DBConfig     `toml:"db"`
	Redis  RedisConfig  `toml:"redis"`
	Milvus MilvusConfig `toml:"milvus"`
	Kafka  KafkaConfig  `toml:"kafka"`
	LLM    LLMConfig    `toml:"llm"`
	MCP    MCPConfig    `toml:"mcp"`
}

type ServerConfig struct {
	Port int `toml:"port"`
}

type DBConfig struct {
	DSN string `toml:"dsn"` // PostgreSQL DSN
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
	Provider string `toml:"provider"` // openai / deepseek
	APIKey   string `toml:"api_key"`
	BaseURL  string `toml:"base_url"`
	Model    string `toml:"model"`

	EmbeddingModel string `toml:"embedding_model"`
}

type MCPConfig struct {
	Transport string `toml:"transport"` // stdio / sse
	Port      int    `toml:"port"`      // SSE 模式端口
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// 设置默认值
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	if cfg.Milvus.Collection == "" {
		cfg.Milvus.Collection = "kaho_documents"
	}
	if cfg.Kafka.Topic == "" {
		cfg.Kafka.Topic = "document-processing"
	}
	if cfg.Kafka.GroupID == "" {
		cfg.Kafka.GroupID = "kahoagent-worker"
	}
	if cfg.MCP.Transport == "" {
		cfg.MCP.Transport = "stdio"
	}

	return &cfg, nil
}
