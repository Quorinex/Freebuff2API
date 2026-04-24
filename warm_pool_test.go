package main

import (
	"log"
	"testing"
	"time"
)

func TestDesiredWarmCounts(t *testing.T) {
	got := desiredWarmCounts(4, []string{"z-ai/glm-5.1", "minimax/minimax-m2.7"})
	if got["z-ai/glm-5.1"] != 2 || got["minimax/minimax-m2.7"] != 2 {
		t.Fatalf("unexpected desired counts: %#v", got)
	}

	got = desiredWarmCounts(3, []string{"a", "b"})
	if got["a"] != 2 || got["b"] != 1 {
		t.Fatalf("unexpected desired counts for uneven pool: %#v", got)
	}
}

func TestHotModelsDropsStaleDemand(t *testing.T) {
	manager := &RunManager{
		logger:            log.New(ioDiscard{}, "", 0),
		recentModelDemand: make(map[string]modelDemand),
	}

	now := time.Now()
	manager.recentModelDemand["stale-model"] = modelDemand{
		Count:         99,
		LastRequested: now.Add(-warmPoolRecentWindow - time.Minute),
	}
	manager.recentModelDemand["recent-low"] = modelDemand{
		Count:         1,
		LastRequested: now.Add(-time.Minute),
	}
	manager.recentModelDemand["recent-high"] = modelDemand{
		Count:         3,
		LastRequested: now.Add(-2 * time.Minute),
	}

	hot := manager.hotModels(2)
	if len(hot) != 2 {
		t.Fatalf("expected 2 hot models, got %d (%#v)", len(hot), hot)
	}
	if hot[0] != "recent-high" || hot[1] != "recent-low" {
		t.Fatalf("unexpected hot model ordering: %#v", hot)
	}
	if _, ok := manager.recentModelDemand["stale-model"]; ok {
		t.Fatalf("expected stale demand to be pruned")
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) {
	return len(p), nil
}
