package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadExpandsEnvironmentAndAppliesDefaults(t *testing.T) {
	t.Setenv("TEST_ZABBIX_API_KEY", "test-api-key")
	path := writeConfig(t, `
zabbix:
  url: https://zabbix.example.com/api_jsonrpc.php
  api_key: ${TEST_ZABBIX_API_KEY}
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Zabbix.APIKey != "test-api-key" {
		t.Fatalf("API key was not expanded: %q", cfg.Zabbix.APIKey)
	}
	if cfg.Prometheus.Pull.Port != 9110 || cfg.Prometheus.Pull.Path != "/metrics" {
		t.Fatalf("unexpected pull defaults: %+v", cfg.Prometheus.Pull)
	}
	if cfg.Collector.Workers < 1 || cfg.Cache.Shards < 1 {
		t.Fatalf("expected positive runtime defaults: collector=%+v cache=%+v", cfg.Collector, cfg.Cache)
	}
}

func TestLoadRejectsMissingAuthentication(t *testing.T) {
	path := writeConfig(t, `
zabbix:
  url: https://zabbix.example.com/api_jsonrpc.php
`)

	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "zabbix.username is required") {
		t.Fatalf("Load() error = %v, want missing authentication error", err)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
