package main

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestMapAcquireErrorWaitingRoom(t *testing.T) {
	response := mapAcquireError(&waitingRoomError{RetryAfter: 7 * time.Second}, "server_error")
	if response.Code != "waiting_room_queued" {
		t.Fatalf("expected waiting_room_queued, got %q", response.Code)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", response.StatusCode)
	}
	if response.RetryAfter < 7*time.Second {
		t.Fatalf("expected retry-after >= 7s, got %s", response.RetryAfter)
	}
}

func TestMapAcquireErrorSwitchInProgress(t *testing.T) {
	response := mapAcquireError(&modelSwitchError{CurrentModel: "z-ai/glm-5.1", TargetModel: "minimax/minimax-m2.7", RetryAfter: 2 * time.Second}, "server_error")
	if response.Code != "session_switch_in_progress" {
		t.Fatalf("expected session_switch_in_progress, got %q", response.Code)
	}
	if response.RetryAfter < time.Second {
		t.Fatalf("expected retry-after >= 1s, got %s", response.RetryAfter)
	}
}

func TestMapAcquireErrorFallsBackToTokenPoolUnavailable(t *testing.T) {
	response := mapAcquireError(errors.New("boom"), "server_error")
	if response.Code != "token_pool_unavailable" {
		t.Fatalf("expected token_pool_unavailable, got %q", response.Code)
	}
}

func TestSummarizeTokenSnapshots(t *testing.T) {
	summary := summarizeTokenSnapshots([]tokenSnapshot{
		{State: "active"},
		{State: "queued"},
		{State: "banned"},
		{State: "cooling_down"},
	})

	if ready, ok := summary["service_ready"].(bool); !ok || !ready {
		t.Fatalf("expected service_ready=true, got %#v", summary["service_ready"])
	}
	if got := summary["healthy"].(int); got != 1 {
		t.Fatalf("expected healthy=1, got %d", got)
	}
	if got := summary["queued"].(int); got != 1 {
		t.Fatalf("expected queued=1, got %d", got)
	}
	if got := summary["banned"].(int); got != 1 {
		t.Fatalf("expected banned=1, got %d", got)
	}
}
