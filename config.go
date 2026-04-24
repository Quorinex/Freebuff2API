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
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ListenAddr       string
	UpstreamBaseURL  string
	AuthTokens       []string
	AuthTokenDir     string
	RotationInterval time.Duration
	RequestTimeout   time.Duration
	UserAgent        string
	APIKeys          []string
	HTTPProxy        string
	ConfigPath       string
	ConfigFormat     string
	LoadedAt         time.Time
}

type rawConfig struct {
	ListenAddr       string   `json:"LISTEN_ADDR" yaml:"LISTEN_ADDR"`
	UpstreamBaseURL  string   `json:"UPSTREAM_BASE_URL" yaml:"UPSTREAM_BASE_URL"`
	AuthTokens       []string `json:"AUTH_TOKENS" yaml:"AUTH_TOKENS"`
	AuthTokenDir     string   `json:"AUTH_TOKEN_DIR" yaml:"AUTH_TOKEN_DIR"`
	RotationInterval string   `json:"ROTATION_INTERVAL" yaml:"ROTATION_INTERVAL"`
	RequestTimeout   string   `json:"REQUEST_TIMEOUT" yaml:"REQUEST_TIMEOUT"`
	APIKeys          []string `json:"API_KEYS" yaml:"API_KEYS"`
	HTTPProxy        string   `json:"HTTP_PROXY" yaml:"HTTP_PROXY"`
}

type ConfigStore struct {
	current atomic.Value
}

func NewConfigStore(cfg Config) *ConfigStore {
	store := &ConfigStore{}
	store.Update(cfg)
	return store
}

func (s *ConfigStore) Current() Config {
	value := s.current.Load()
	if value == nil {
		return Config{}
	}
	return value.(Config)
}

func (s *ConfigStore) Update(cfg Config) {
	s.current.Store(cfg)
}

func resolveConfigPath(configPath string) (string, error) {
	if strings.TrimSpace(configPath) != "" {
		resolved, err := filepath.Abs(strings.TrimSpace(configPath))
		if err != nil {
			return "", fmt.Errorf("resolve config path: %w", err)
		}
		return resolved, nil
	}

	for _, candidate := range []string{"config.yaml", "config.yml", "config.json"} {
		if _, err := os.Stat(candidate); err == nil {
			resolved, err := filepath.Abs(candidate)
			if err != nil {
				return "", fmt.Errorf("resolve config path: %w", err)
			}
			return resolved, nil
		}
	}

	return "", nil
}

func loadConfig(configPath string) (Config, error) {
	cfg, err := loadRawConfig(configPath)
	if err != nil {
		return Config{}, err
	}

	rotationInterval, err := time.ParseDuration(strings.TrimSpace(cfg.RotationInterval))
	if err != nil {
		return Config{}, fmt.Errorf("parse rotation interval: %w", err)
	}

	requestTimeout, err := time.ParseDuration(strings.TrimSpace(cfg.RequestTimeout))
	if err != nil {
		return Config{}, fmt.Errorf("parse request timeout: %w", err)
	}

	authTokenDir := strings.TrimSpace(cfg.AuthTokenDir)
	if authTokenDir != "" && !filepath.IsAbs(authTokenDir) {
		baseDir := "."
		if strings.TrimSpace(configPath) != "" {
			baseDir = filepath.Dir(configPath)
		}
		authTokenDir = filepath.Clean(filepath.Join(baseDir, authTokenDir))
	}

	authTokens := dedupeStrings(cfg.AuthTokens)
	if tokenDir := authTokenDir; tokenDir != "" {
		tokensFromDir, err := loadAuthTokensFromDir(tokenDir)
		if err != nil {
			return Config{}, fmt.Errorf("load auth tokens from dir: %w", err)
		}
		authTokens = dedupeStrings(append(authTokens, tokensFromDir...))
	}

	finalCfg := Config{
		ListenAddr:       strings.TrimSpace(cfg.ListenAddr),
		UpstreamBaseURL:  normalizeUpstreamBaseURL(cfg.UpstreamBaseURL),
		AuthTokens:       authTokens,
		AuthTokenDir:     authTokenDir,
		RotationInterval: rotationInterval,
		RequestTimeout:   requestTimeout,
		UserAgent:        generateUserAgent(),
		APIKeys:          dedupeStrings(cfg.APIKeys),
		HTTPProxy:        strings.TrimSpace(cfg.HTTPProxy),
		ConfigPath:       strings.TrimSpace(configPath),
		ConfigFormat:     configFormat(configPath),
		LoadedAt:         time.Now().UTC(),
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

	applyEnvConfig(&cfg)

	if strings.TrimSpace(configPath) != "" {
		overlay, err := parseConfigFile(configPath)
		if err != nil {
			return rawConfig{}, err
		}
		mergeRawConfig(&cfg, overlay)
	}

	return cfg, nil
}

func parseConfigFile(configPath string) (rawConfig, error) {
	path, err := filepath.Abs(configPath)
	if err != nil {
		return rawConfig{}, fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return rawConfig{}, fmt.Errorf("read config file: %w", err)
	}

	var cfg rawConfig
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return rawConfig{}, fmt.Errorf("parse config file: %w", err)
		}
	default:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return rawConfig{}, fmt.Errorf("parse config file: %w", err)
		}
	}

	return cfg, nil
}

func mergeRawConfig(dst *rawConfig, src rawConfig) {
	if strings.TrimSpace(src.ListenAddr) != "" {
		dst.ListenAddr = src.ListenAddr
	}
	if strings.TrimSpace(src.UpstreamBaseURL) != "" {
		dst.UpstreamBaseURL = src.UpstreamBaseURL
	}
	if len(src.AuthTokens) > 0 {
		dst.AuthTokens = src.AuthTokens
	}
	if strings.TrimSpace(src.AuthTokenDir) != "" {
		dst.AuthTokenDir = src.AuthTokenDir
	}
	if strings.TrimSpace(src.RotationInterval) != "" {
		dst.RotationInterval = src.RotationInterval
	}
	if strings.TrimSpace(src.RequestTimeout) != "" {
		dst.RequestTimeout = src.RequestTimeout
	}
	if len(src.APIKeys) > 0 {
		dst.APIKeys = src.APIKeys
	}
	if strings.TrimSpace(src.HTTPProxy) != "" {
		dst.HTTPProxy = src.HTTPProxy
	}
}

func applyEnvConfig(cfg *rawConfig) {
	overrideString(&cfg.ListenAddr, "LISTEN_ADDR")
	overrideString(&cfg.UpstreamBaseURL, "UPSTREAM_BASE_URL")
	overrideString(&cfg.AuthTokenDir, "AUTH_TOKEN_DIR")
	overrideString(&cfg.RotationInterval, "ROTATION_INTERVAL")
	overrideString(&cfg.RequestTimeout, "REQUEST_TIMEOUT")
	overrideCSV(&cfg.AuthTokens, "AUTH_TOKENS")
	overrideCSV(&cfg.APIKeys, "API_KEYS")
	overrideString(&cfg.HTTPProxy, "HTTP_PROXY")
}

func loadAuthTokensFromDir(dir string) ([]string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	resolved, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve auth token dir: %w", err)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, fmt.Errorf("read auth token dir: %w", err)
	}

	tokens := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(resolved, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read auth token file %s: %w", path, err)
		}
		tokens = append(tokens, extractTokensFromBlob(path, data)...)
	}

	return dedupeStrings(tokens), nil
}

func extractTokensFromBlob(path string, data []byte) []string {
	var decoded any
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &decoded); err == nil {
			if tokens := collectAuthTokens(decoded); len(tokens) > 0 {
				return dedupeStrings(tokens)
			}
		}
	default:
		if err := json.Unmarshal(data, &decoded); err == nil {
			if tokens := collectAuthTokens(decoded); len(tokens) > 0 {
				return dedupeStrings(tokens)
			}
		}
		if err := yaml.Unmarshal(data, &decoded); err == nil {
			if tokens := collectAuthTokens(decoded); len(tokens) > 0 {
				return dedupeStrings(tokens)
			}
		}
	}

	return splitList(string(data))
}

func collectAuthTokens(value any) []string {
	var tokens []string
	collectAuthTokensInto(value, "", &tokens)
	return dedupeStrings(tokens)
}

func collectAuthTokensInto(value any, key string, tokens *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for childKey, childValue := range typed {
			collectAuthTokensInto(childValue, childKey, tokens)
		}
	case map[any]any:
		for rawKey, childValue := range typed {
			collectAuthTokensInto(childValue, fmt.Sprint(rawKey), tokens)
		}
	case []any:
		for _, childValue := range typed {
			collectAuthTokensInto(childValue, key, tokens)
		}
	case []string:
		if isAuthTokenListKey(key) {
			*tokens = append(*tokens, compactStrings(typed)...)
		}
	case string:
		if isAuthTokenScalarKey(key) {
			*tokens = append(*tokens, strings.TrimSpace(typed))
		}
	}
}

func isAuthTokenScalarKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authtoken", "auth_token", "token":
		return true
	default:
		return false
	}
}

func isAuthTokenListKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authtokens", "auth_tokens", "tokens":
		return true
	default:
		return false
	}
}

func configFormat(configPath string) string {
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(configPath))) {
	case ".yaml", ".yml":
		return "yaml"
	case ".json":
		return "json"
	default:
		return ""
	}
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
// official SDK: Math.random().toString(36).substring(2, 15) -> a ~13-char
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
