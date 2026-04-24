package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigSupportsYAMLAndTokenDir(t *testing.T) {
	tempDir := t.TempDir()
	tokenDir := filepath.Join(tempDir, "tokens.d")
	if err := os.MkdirAll(tokenDir, 0o755); err != nil {
		t.Fatalf("mkdir token dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tokenDir, "plain.txt"), []byte("token-from-text\n"), 0o644); err != nil {
		t.Fatalf("write plain token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "json.json"), []byte(`{"authToken":"token-from-json"}`), 0o644); err != nil {
		t.Fatalf("write json token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "yaml.yaml"), []byte("default:\n  authToken: token-from-yaml\n"), 0o644); err != nil {
		t.Fatalf("write yaml token: %v", err)
	}

	configPath := filepath.Join(tempDir, "config.yaml")
	configBody := []byte("" +
		"LISTEN_ADDR: \":18080\"\n" +
		"UPSTREAM_BASE_URL: \"https://codebuff.com\"\n" +
		"AUTH_TOKENS:\n" +
		"  - inline-token\n" +
		"AUTH_TOKEN_DIR: \"tokens.d\"\n" +
		"ROTATION_INTERVAL: \"2h\"\n" +
		"REQUEST_TIMEOUT: \"45s\"\n" +
		"API_KEYS:\n" +
		"  - key-1\n")
	if err := os.WriteFile(configPath, configBody, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}

	if got := cfg.UpstreamBaseURL; got != "https://www.codebuff.com" {
		t.Fatalf("expected normalized base url, got %q", got)
	}
	if got := cfg.AuthTokenDir; got != tokenDir {
		t.Fatalf("expected absolute token dir %q, got %q", tokenDir, got)
	}
	if len(cfg.AuthTokens) != 4 {
		t.Fatalf("expected 4 auth tokens, got %d (%v)", len(cfg.AuthTokens), cfg.AuthTokens)
	}
	if !containsString(cfg.AuthTokens, "inline-token") || !containsString(cfg.AuthTokens, "token-from-json") || !containsString(cfg.AuthTokens, "token-from-yaml") || !containsString(cfg.AuthTokens, "token-from-text") {
		t.Fatalf("unexpected auth tokens: %v", cfg.AuthTokens)
	}
}
