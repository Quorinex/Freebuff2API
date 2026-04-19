package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "", "path to a JSON config file (default: config.json if present)")
	flag.Parse()

	logger := log.New(os.Stdout, "[Freebuff2API] ", log.LstdFlags|log.Lmsgprefix)

	// Auto-detect config.json in CWD when no flag is given
	if *configPath == "" {
		if _, err := os.Stat("config.json"); err == nil {
			*configPath = "config.json"
		}
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		logger.Fatalf("load config: %v", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.HTTPProxy != "" {
		importURL, _ := url.Parse(cfg.HTTPProxy)
		transport.Proxy = http.ProxyURL(importURL)
	}
	httpClient := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	
	registry := NewModelRegistry(httpClient, logger)
	registry.Start(context.Background())
	defer registry.Stop()

	server := NewServer(cfg, logger, registry)
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	server.Start(runCtx)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		logger.Printf("listening on %s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("listen: %v", err)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("http shutdown error: %v", err)
	}
	cancelRun()
	server.Shutdown(shutdownCtx)
}
