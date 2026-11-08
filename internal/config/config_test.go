package config

import "testing"

func TestDefaultPath(t *testing.T) {
	// 默认值必须是 config.toml：本地开发不该被迫设环境变量
	t.Run("falls back to the working directory", func(t *testing.T) {
		t.Setenv("KAHO_CONFIG", "")
		if got := DefaultPath(); got != "config.toml" {
			t.Errorf("expected config.toml, got %q", got)
		}
	})

	// 容器场景：config.toml 含密钥不进镜像，只能挂载进来，
	// 而挂载点通常不在工作目录（K8s 一般挂到 /etc/... 下）
	t.Run("honors KAHO_CONFIG", func(t *testing.T) {
		t.Setenv("KAHO_CONFIG", "/etc/kaho/config.toml")
		if got := DefaultPath(); got != "/etc/kaho/config.toml" {
			t.Errorf("expected the override to win, got %q", got)
		}
	})
}

// TestEmbeddingAuth_Fallback 验证 embedding 未单独配置时回退到主模型。
//
// 单供应商（DashScope 同时做 chat 与 embedding）是最常见的部署方式，
// 不能强迫用户为它多写两行配置。
func TestEmbeddingAuth_Fallback(t *testing.T) {
	c := LLMConfig{APIKey: "primary", BaseURL: "https://dashscope.example/v1"}
	if got := c.EmbeddingKey(); got != "primary" {
		t.Fatalf("EmbeddingKey = %q, want primary", got)
	}
	if got := c.EmbeddingURL(); got != "https://dashscope.example/v1" {
		t.Fatalf("EmbeddingURL = %q, want primary url", got)
	}
}

// TestEmbeddingAuth_Override 验证对话与 embedding 可以分属两家。
func TestEmbeddingAuth_Override(t *testing.T) {
	c := LLMConfig{
		APIKey: "deepseek-key", BaseURL: "https://api.deepseek.com",
		EmbeddingAPIKey: "dash-key", EmbeddingBaseURL: "https://dashscope.example/v1",
	}
	if got := c.EmbeddingKey(); got != "dash-key" {
		t.Fatalf("EmbeddingKey = %q, want dash-key", got)
	}
	if got := c.EmbeddingURL(); got != "https://dashscope.example/v1" {
		t.Fatalf("EmbeddingURL = %q, want dashscope", got)
	}
}

// TestValidate_RejectsDeepSeekEmbedding 验证「DeepSeek 没有 embedding」
// 会在启动期被拦住，而不是等到第一次入库才报 404。
func TestValidate_RejectsDeepSeekEmbedding(t *testing.T) {
	base := LLMConfig{
		APIKey: "k", BaseURL: "https://api.deepseek.com",
		Model: "deepseek-chat", EmbeddingModel: "e", EmbeddingDim: 1024,
	}
	c := &Config{LLM: base}
	if err := c.validate(); err == nil {
		t.Fatal("expected an error when embedding points at DeepSeek")
	}

	// 配上独立的 embedding 供应商后应当通过
	c.LLM.EmbeddingBaseURL = "https://dashscope.example/v1"
	c.LLM.EmbeddingAPIKey = "dash"
	if err := c.validate(); err != nil {
		t.Fatalf("validate with separate embedding provider: %v", err)
	}
}
