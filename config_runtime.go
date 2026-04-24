package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const configReloadPollInterval = 15 * time.Second

type ConfigReloader struct {
	configPath string
	logger     *log.Logger
	apply      func(Config)

	mu      sync.Mutex
	current Config
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

func NewConfigReloader(initial Config, logger *log.Logger, apply func(Config)) *ConfigReloader {
	return &ConfigReloader{
		configPath: initial.ConfigPath,
		logger:     logger,
		apply:      apply,
		current:    initial,
		stopCh:     make(chan struct{}),
	}
}

func (r *ConfigReloader) Start() {
	if strings.TrimSpace(r.configPath) == "" && strings.TrimSpace(r.current.AuthTokenDir) == "" {
		return
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.run()
	}()
}

func (r *ConfigReloader) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

func (r *ConfigReloader) run() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		r.logger.Printf("config reloader disabled: %v", err)
		return
	}
	defer watcher.Close()

	r.watchCurrentPaths(watcher)

	ticker := time.NewTicker(configReloadPollInterval)
	defer ticker.Stop()

	for {
		select {
		case event := <-watcher.Events:
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) || event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
				r.reloadIfChanged(watcher, event.Name)
			}
		case err := <-watcher.Errors:
			if err != nil {
				r.logger.Printf("config watcher error: %v", err)
			}
		case <-ticker.C:
			r.reloadIfChanged(watcher, "")
		case <-r.stopCh:
			return
		}
	}
}

func (r *ConfigReloader) watchCurrentPaths(watcher *fsnotify.Watcher) {
	r.mu.Lock()
	cfg := r.current
	r.mu.Unlock()

	seen := make(map[string]struct{})
	for _, path := range []string{cfg.ConfigPath, cfg.AuthTokenDir} {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}

		target := path
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			target = filepath.Dir(path)
		}
		target, err = filepath.Abs(target)
		if err != nil {
			continue
		}
		if _, exists := seen[target]; exists {
			continue
		}
		if err := watcher.Add(target); err != nil {
			r.logger.Printf("config watcher add %s failed: %v", target, err)
			continue
		}
		seen[target] = struct{}{}
	}
}

func (r *ConfigReloader) reloadIfChanged(watcher *fsnotify.Watcher, changedPath string) {
	r.mu.Lock()
	current := r.current
	r.mu.Unlock()

	if !r.isRelevantChange(current, changedPath) {
		return
	}

	cfg, err := loadConfig(current.ConfigPath)
	if err != nil {
		r.logger.Printf("config reload failed: %v", err)
		return
	}
	if configSignature(cfg) == configSignature(current) {
		return
	}

	r.mu.Lock()
	r.current = cfg
	r.mu.Unlock()
	r.watchCurrentPaths(watcher)
	r.apply(cfg)
	r.logger.Printf("config reloaded from %s (%d auth tokens, %d api keys)", displayConfigPath(cfg.ConfigPath), len(cfg.AuthTokens), len(cfg.APIKeys))
}

func (r *ConfigReloader) isRelevantChange(cfg Config, changedPath string) bool {
	changedPath = strings.TrimSpace(changedPath)
	if changedPath == "" {
		return true
	}

	changedAbs, err := filepath.Abs(changedPath)
	if err != nil {
		return true
	}

	if cfg.ConfigPath != "" {
		configAbs, err := filepath.Abs(cfg.ConfigPath)
		if err == nil && strings.EqualFold(configAbs, changedAbs) {
			return true
		}
	}

	if cfg.AuthTokenDir != "" {
		tokenDirAbs, err := filepath.Abs(cfg.AuthTokenDir)
		if err == nil {
			rel, relErr := filepath.Rel(tokenDirAbs, changedAbs)
			if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}

	return false
}

func configSignature(cfg Config) string {
	return strings.Join([]string{
		cfg.ListenAddr,
		cfg.UpstreamBaseURL,
		cfg.AuthTokenDir,
		cfg.RotationInterval.String(),
		cfg.RequestTimeout.String(),
		cfg.HTTPProxy,
		strings.Join(cfg.AuthTokens, ","),
		strings.Join(cfg.APIKeys, ","),
	}, "|")
}

func displayConfigPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return "environment"
	}
	return path
}
