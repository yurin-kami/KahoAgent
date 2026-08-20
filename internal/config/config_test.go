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
