package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const freeSessionPollInterval = 5 * time.Second
const freeSessionRetryDelay = 10 * time.Second

type sessionStatus string

const (
	sessionStatusDisabled   sessionStatus = "disabled"
	sessionStatusNone       sessionStatus = "none"
	sessionStatusQueued     sessionStatus = "queued"
	sessionStatusActive     sessionStatus = "active"
	sessionStatusEnded      sessionStatus = "ended"
	sessionStatusSuperseded sessionStatus = "superseded"
)

type freeSessionResponse struct {
	Status                 string `json:"status"`
	InstanceID             string `json:"instanceId"`
	ExpiresAt              string `json:"expiresAt"`
	RemainingMs            int64  `json:"remainingMs"`
	EstimatedWaitMs        int64  `json:"estimatedWaitMs"`
	GracePeriodRemainingMs int64  `json:"gracePeriodRemainingMs"`
	Message                string `json:"message"`
}

type cachedSession struct {
	status     sessionStatus
	instanceID string
	expiresAt  time.Time
}

func (p *tokenPool) ensureSession(ctx context.Context) (string, error) {
	for {
		p.mu.Lock()
		if instanceID, ready := p.readySessionLocked(time.Now()); ready {
			p.mu.Unlock()
			return instanceID, nil
		}
		if ch := p.sessionRefreshCh; ch != nil {
			p.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-ch:
				continue
			}
		}
		ch := make(chan struct{})
		p.sessionRefreshCh = ch
		p.mu.Unlock()

		session, instanceID, err := p.refreshSession(ctx)

		p.mu.Lock()
		if err != nil {
			p.session = nil
			p.lastError = err.Error()
		} else {
			p.session = session
			p.lastError = ""
		}
		close(p.sessionRefreshCh)
		p.sessionRefreshCh = nil
		p.mu.Unlock()

		if err == nil && session != nil && session.status == sessionStatusActive {
			p.watchSessionExpiry(session.instanceID, session.expiresAt)
		}

		return instanceID, err
	}
}

func (p *tokenPool) ensureSessionAsync(reason string) {
	p.mu.Lock()
	if now := time.Now(); now.Before(p.cooldownUntil) {
		p.mu.Unlock()
		return
	}
	if p.sessionRefreshCh != nil || p.sessionRebuildScheduled {
		p.mu.Unlock()
		return
	}
	p.sessionRebuildScheduled = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			p.sessionRebuildScheduled = false
			p.mu.Unlock()
		}()

		if reason != "" {
			p.logger.Printf("%s: rebuilding free session in background (%s)", p.name, reason)
		}
		p.prewarmSession(context.Background())
	}()
}

func (p *tokenPool) readySessionLocked(now time.Time) (string, bool) {
	if p.session == nil {
		return "", false
	}
	switch p.session.status {
	case sessionStatusDisabled:
		return "", true
	case sessionStatusActive:
		if p.session.instanceID == "" {
			return "", false
		}
		if p.session.expiresAt.IsZero() || now.Before(p.session.expiresAt.Add(-5*time.Second)) {
			return p.session.instanceID, true
		}
	}
	return "", false
}

func (p *tokenPool) hasReadySession() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ready := p.readySessionLocked(time.Now())
	return ready
}

func (p *tokenPool) refreshSession(ctx context.Context) (*cachedSession, string, error) {
	state, err := p.client.CreateOrRefreshSession(ctx, p.token)
	if err != nil {
		return nil, "", fmt.Errorf("start free session: %w", err)
	}

	for {
		switch sessionStatus(strings.TrimSpace(state.Status)) {
		case sessionStatusDisabled:
			return &cachedSession{status: sessionStatusDisabled}, "", nil
		case sessionStatusActive:
			instanceID := strings.TrimSpace(state.InstanceID)
			if instanceID == "" {
				return nil, "", fmt.Errorf("free session active response missing instanceId")
			}
			expiresAt, err := parseOptionalTime(state.ExpiresAt)
			if err != nil {
				return nil, "", fmt.Errorf("parse free session expiry: %w", err)
			}
			return &cachedSession{
				status:     sessionStatusActive,
				instanceID: instanceID,
				expiresAt:  expiresAt,
			}, instanceID, nil
		case sessionStatusQueued:
			instanceID := strings.TrimSpace(state.InstanceID)
			if instanceID == "" {
				return nil, "", fmt.Errorf("free session queued response missing instanceId")
			}
			if err := sleepWithContext(ctx, queuedPollDelay(state)); err != nil {
				return nil, "", err
			}
			state, err = p.client.GetSession(ctx, p.token, instanceID)
			if err != nil {
				return nil, "", fmt.Errorf("poll free session: %w", err)
			}
		case sessionStatusNone, sessionStatusEnded, sessionStatusSuperseded:
			state, err = p.client.CreateOrRefreshSession(ctx, p.token)
			if err != nil {
				return nil, "", fmt.Errorf("refresh free session: %w", err)
			}
		default:
			return nil, "", fmt.Errorf("unexpected free session status %q", state.Status)
		}
	}
}

func (p *tokenPool) invalidateSession(reason string) {
	p.mu.Lock()
	p.session = nil
	if reason != "" {
		p.lastError = reason
	}
	p.mu.Unlock()

	p.ensureSessionAsync(reason)
}

func (p *tokenPool) currentSessionInstanceID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == nil {
		return ""
	}
	return p.session.instanceID
}

func (p *tokenPool) prewarmSession(ctx context.Context) {
	for {
		if _, err := p.ensureSession(ctx); err == nil {
			return
		} else {
			p.logger.Printf("%s: session prewarm failed: %v", p.name, err)
		}

		if err := sleepWithContext(ctx, freeSessionRetryDelay); err != nil {
			return
		}
	}
}

func (p *tokenPool) watchSessionExpiry(instanceID string, expiresAt time.Time) {
	if instanceID == "" || expiresAt.IsZero() {
		return
	}

	go func() {
		if err := sleepWithContext(context.Background(), time.Until(expiresAt.Add(time.Second))); err != nil {
			return
		}

		for {
			p.mu.Lock()
			current := p.session
			if current == nil || current.status != sessionStatusActive || current.instanceID != instanceID {
				p.mu.Unlock()
				return
			}
			if p.hasInflightRequestsLocked() {
				p.mu.Unlock()
				if err := sleepWithContext(context.Background(), time.Second); err != nil {
					return
				}
				continue
			}
			p.session = nil
			p.mu.Unlock()

			p.ensureSessionAsync("expired")
			return
		}
	}()
}

func (p *tokenPool) hasInflightRequestsLocked() bool {
	for _, run := range p.runs {
		if run != nil && run.inflight > 0 {
			return true
		}
	}
	for _, run := range p.draining {
		if run != nil && run.inflight > 0 {
			return true
		}
	}
	return false
}

func (p *tokenPool) endSession(ctx context.Context) error {
	p.mu.Lock()
	session := p.session
	p.session = nil
	p.mu.Unlock()

	if session == nil || session.status == sessionStatusDisabled || session.instanceID == "" {
		return nil
	}
	if err := p.client.EndSession(ctx, p.token); err != nil {
		return fmt.Errorf("end free session: %w", err)
	}
	return nil
}

func (c *UpstreamClient) CreateOrRefreshSession(ctx context.Context, authToken string) (freeSessionResponse, error) {
	return c.doSessionRequest(ctx, http.MethodPost, authToken, "")
}

func (c *UpstreamClient) GetSession(ctx context.Context, authToken, instanceID string) (freeSessionResponse, error) {
	return c.doSessionRequest(ctx, http.MethodGet, authToken, instanceID)
}

func (c *UpstreamClient) EndSession(ctx context.Context, authToken string) error {
	requestURL, err := url.JoinPath(c.baseURL, "/api/v1/freebuff/session")
	if err != nil {
		return fmt.Errorf("build free session url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create free session delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send free session delete request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("free session delete failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func (c *UpstreamClient) doSessionRequest(ctx context.Context, method, authToken, instanceID string) (freeSessionResponse, error) {
	requestURL, err := url.JoinPath(c.baseURL, "/api/v1/freebuff/session")
	if err != nil {
		return freeSessionResponse{}, fmt.Errorf("build free session url: %w", err)
	}

	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	}

	req, err := http.NewRequestWithContext(ctx, method, requestURL, body)
	if err != nil {
		return freeSessionResponse{}, fmt.Errorf("create free session request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodGet && instanceID != "" {
		req.Header.Set("x-freebuff-instance-id", instanceID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return freeSessionResponse{}, fmt.Errorf("send free session request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return freeSessionResponse{Status: string(sessionStatusDisabled)}, nil
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return freeSessionResponse{}, fmt.Errorf("read free session response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return freeSessionResponse{}, fmt.Errorf("free session request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}

	var parsed freeSessionResponse
	if err := json.Unmarshal(responseBody, &parsed); err != nil {
		return freeSessionResponse{}, fmt.Errorf("decode free session response: %w", err)
	}
	if strings.TrimSpace(parsed.Status) == "" {
		return freeSessionResponse{}, fmt.Errorf("free session response missing status")
	}
	return parsed, nil
}

func queuedPollDelay(state freeSessionResponse) time.Duration {
	if state.EstimatedWaitMs <= 0 {
		return freeSessionPollInterval
	}
	delay := time.Duration(state.EstimatedWaitMs) * time.Millisecond
	if delay < time.Second {
		return time.Second
	}
	if delay > freeSessionPollInterval {
		return freeSessionPollInterval
	}
	return delay
}

func parseOptionalTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
