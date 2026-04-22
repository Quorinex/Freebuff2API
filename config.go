package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	ListenAddr       string
	UpstreamBaseURL  string
	AuthTokens       []string
	RotationInterval time.Duration
	RequestTimeout   time.Duration
	UserAgent        string
	APIKeys          []string
	HTTPProxy        string
}

type rawConfig struct {
	ListenAddr       string   `json:"LISTEN_ADDR"`
	UpstreamBaseURL  string   `json:"UPSTREAM_BASE_URL"`
	AuthTokens       []string `json:"AUTH_TOKENS"`
	RotationInterval string   `json:"ROTATION_INTERVAL"`
	RequestTimeout   string   `json:"REQUEST_TIMEOUT"`
	APIKeys          []string `json:"API_KEYS"`
	HTTPProxy        string   `json:"HTTP_PROXY"`
}

func loadConfig(configPath string) (Config, error) {
	cfg, err := loadRawConfig(configPath)
	if err != nil {
		return Config{}, err
	}

	overrideString(&cfg.ListenAddr, "LISTEN_ADDR")
	overrideString(&cfg.UpstreamBaseURL, "UPSTREAM_BASE_URL")
	overrideString(&cfg.RotationInterval, "ROTATION_INTERVAL")
	overrideString(&cfg.RequestTimeout, "REQUEST_TIMEOUT")
	overrideCSV(&cfg.AuthTokens, "AUTH_TOKENS")
	overrideCSV(&cfg.APIKeys, "API_KEYS")
	overrideString(&cfg.HTTPProxy, "HTTP_PROXY")

	rotationInterval, err := time.ParseDuration(strings.TrimSpace(cfg.RotationInterval))
	if err != nil {
		return Config{}, fmt.Errorf("parse rotation interval: %w", err)
	}

	requestTimeout, err := time.ParseDuration(strings.TrimSpace(cfg.RequestTimeout))
	if err != nil {
		return Config{}, fmt.Errorf("parse request timeout: %w", err)
	}

	finalCfg := Config{
		ListenAddr:       strings.TrimSpace(cfg.ListenAddr),
		UpstreamBaseURL:  normalizeUpstreamBaseURL(cfg.UpstreamBaseURL),
		AuthTokens:       dedupeStrings(cfg.AuthTokens),
		RotationInterval: rotationInterval,
		RequestTimeout:   requestTimeout,
		UserAgent:        generateUserAgent(),
		APIKeys:          dedupeStrings(cfg.APIKeys),
		HTTPProxy:        strings.TrimSpace(cfg.HTTPProxy),
	}

	switch {
	case finalCfg.ListenAddr == "":
		return Config{}, errors.New("LISTEN_ADDR cannot be empty")
	case finalCfg.UpstreamBaseURL == "":
		return Config{}, errors.New("UPSTREAM_BASE_URL cannot be empty")
	case len(finalCfg.AuthTokens) == 0:
		return Config{}, errors.New("at least one AUTH_TOKENS is required")
	case finalCfg.RotationInterval <= 0:
		return Config{}, errors.New("ROTATION_INTERVAL must be greater than zero")
	case finalCfg.RequestTimeout <= 0:
		return Config{}, errors.New("REQUEST_TIMEOUT must be greater than zero")
	}

	return finalCfg, nil
}

func normalizeUpstreamBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if strings.EqualFold(parsed.Host, "codebuff.com") {
		parsed.Host = "www.codebuff.com"
	}

	return strings.TrimRight(parsed.String(), "/")
}

func loadRawConfig(configPath string) (rawConfig, error) {
	cfg := rawConfig{
		ListenAddr:       ":8080",
		UpstreamBaseURL:  "https://www.codebuff.com",
		RotationInterval: "6h",
		RequestTimeout:   "15m",
	}

	if configPath != "" {
		path, err := filepath.Abs(configPath)
		if err != nil {
			return rawConfig{}, fmt.Errorf("resolve config path: %w", err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return rawConfig{}, fmt.Errorf("read config file: %w", err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return rawConfig{}, fmt.Errorf("parse config file: %w", err)
		}
	}

	return cfg, nil
}

func overrideString(target *string, envName string) {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		*target = value
	}
}

func overrideCSV(target *[]string, envName string) {
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return
	}
	*target = splitList(value)
}

func splitList(value string) []string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	return compactStrings(fields)
}

func compactStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range compactStrings(values) {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func generateUserAgent() string {
	return "ai-sdk/openai-compatible/1.0.25/codebuff"
}

// generateClientSessionId generates a per-request session ID matching the
// official SDK: Math.random().toString(36).substring(2, 15) — a ~13-char
// base-36 alphanumeric string.
func generateClientSessionId() string {
	buf := make([]byte, 10)
	if _, err := rand.Read(buf); err != nil {
		buf = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 13)
	for i := range out {
		out[i] = alphabet[buf[i%len(buf)]%36]
	}
	return string(out)
}
