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
	Model                  string `json:"model"`
	Position               int    `json:"position"`
	QueueDepth             int    `json:"queueDepth"`
	QueuedAt               string `json:"queuedAt"`
	ExpiresAt              string `json:"expiresAt"`
	RemainingMs            int64  `json:"remainingMs"`
	EstimatedWaitMs        int64  `json:"estimatedWaitMs"`
	GracePeriodRemainingMs int64  `json:"gracePeriodRemainingMs"`
	Message                string `json:"message"`
}

type cachedSession struct {
	status     sessionStatus
	instanceID string
	model      string
	expiresAt  time.Time
	position   int
	queueDepth int
	pollAt     time.Time
	retryAfter time.Duration
}

type modelSwitchError struct {
	CurrentModel string
	TargetModel  string
	RetryAfter   time.Duration
}

func (e *modelSwitchError) Error() string {
	if e == nil {
		return "session switch in progress"
	}
	if e.CurrentModel == "" || e.TargetModel == "" {
		return "session switch in progress"
	}
	return fmt.Sprintf("token is switching from %s to %s", e.CurrentModel, e.TargetModel)
}

func (p *tokenPool) ensureSession(ctx context.Context, model string) (string, error) {
	model = strings.TrimSpace(model)
	for {
		p.mu.Lock()
		if instanceID, ready := p.readySessionLocked(time.Now(), model); ready {
			p.mu.Unlock()
			return instanceID, nil
		}
		if waitingErr := waitingRoomErrorFromSession(p.name, p.session, time.Now()); waitingErr != nil && p.sessionMatchesModelLocked(model) {
			p.mu.Unlock()
			return "", waitingErr
		}
		if p.session != nil && !p.sessionMatchesModelLocked(model) {
			p.mu.Unlock()
			if err := p.prepareModel(ctx, model); err != nil {
				return "", err
			}
			continue
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

		session, instanceID, err := p.refreshSession(ctx, model)

		p.mu.Lock()
		if session != nil {
			p.session = session
		}
		if err != nil {
			p.session = nil
			p.lastError = err.Error()
			if isBannedErrorMessage(err.Error()) {
				p.disabled = true
			}
		} else if waitingErr := waitingRoomErrorFromSession(p.name, session, time.Now()); waitingErr != nil {
			p.lastError = waitingErr.Error()
		} else {
			p.lastError = ""
		}
		close(p.sessionRefreshCh)
		p.sessionRefreshCh = nil
		p.mu.Unlock()

		if err == nil {
			if waitingErr := waitingRoomErrorFromSession(p.name, session, time.Now()); waitingErr != nil {
				return "", waitingErr
			}
		}
		return instanceID, err
	}
}

func (p *tokenPool) readySessionLocked(now time.Time, model string) (string, bool) {
	if p.session == nil {
		return "", false
	}
	if !p.sessionMatchesModelLocked(model) {
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

func (p *tokenPool) refreshSession(ctx context.Context, model string) (*cachedSession, string, error) {
	model = strings.TrimSpace(model)
	p.mu.Lock()
	current := p.session
	p.mu.Unlock()

	var (
		state freeSessionResponse
		err   error
	)
	if current != nil && current.status == sessionStatusQueued && strings.TrimSpace(current.instanceID) != "" {
		state, err = p.client.GetSession(ctx, p.token, current.instanceID)
		if err != nil {
			return nil, "", fmt.Errorf("poll free session: %w", err)
		}
	} else {
		state, err = p.client.CreateOrRefreshSession(ctx, p.token, model)
		if err != nil {
			return nil, "", fmt.Errorf("start free session: %w", err)
		}
	}

	for {
		switch sessionStatus(strings.TrimSpace(state.Status)) {
		case sessionStatusDisabled:
			return &cachedSession{status: sessionStatusDisabled, model: model}, "", nil
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
				model:      firstNonEmptyTrimmedString(strings.TrimSpace(state.Model), model),
				expiresAt:  expiresAt,
			}, instanceID, nil
		case sessionStatusQueued:
			instanceID := strings.TrimSpace(state.InstanceID)
			if instanceID == "" {
				return nil, "", fmt.Errorf("free session queued response missing instanceId")
			}
			p.logQueuePosition(state)
			delay := queuedPollDelay(state)
			return &cachedSession{
				status:     sessionStatusQueued,
				instanceID: instanceID,
				model:      firstNonEmptyTrimmedString(strings.TrimSpace(state.Model), model),
				position:   maxInt(state.Position, 1),
				queueDepth: maxInt(state.QueueDepth, maxInt(state.Position, 1)),
				pollAt:     time.Now().Add(delay),
				retryAfter: delay,
			}, "", nil
		case sessionStatusNone, sessionStatusEnded, sessionStatusSuperseded:
			state, err = p.client.CreateOrRefreshSession(ctx, p.token, model)
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
	defer p.mu.Unlock()
	p.session = nil
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) currentSessionInstanceID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == nil {
		return ""
	}
	return p.session.instanceID
}

func (p *tokenPool) currentSessionModel() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.session == nil {
		return ""
	}
	return p.session.model
}

func (p *tokenPool) sessionMatchesModelLocked(model string) bool {
	if p.session == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" || strings.TrimSpace(p.session.model) == "" {
		return true
	}
	return p.session.model == model
}

func (p *tokenPool) prepareModel(ctx context.Context, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}

	p.mu.Lock()
	currentModel := ""
	if p.session != nil {
		currentModel = strings.TrimSpace(p.session.model)
	}
	if currentModel == "" {
		for _, run := range p.runs {
			if strings.TrimSpace(run.model) != "" {
				currentModel = strings.TrimSpace(run.model)
				break
			}
		}
	}
	if currentModel == "" || currentModel == model {
		p.mu.Unlock()
		return nil
	}

	for _, run := range p.runs {
		if run.inflight > 0 {
			p.mu.Unlock()
			return &modelSwitchError{
				CurrentModel: currentModel,
				TargetModel:  model,
				RetryAfter:   3 * time.Second,
			}
		}
	}
	for _, run := range p.draining {
		if run.inflight > 0 {
			p.mu.Unlock()
			return &modelSwitchError{
				CurrentModel: currentModel,
				TargetModel:  model,
				RetryAfter:   3 * time.Second,
			}
		}
	}

	session := p.session
	var allRuns []*managedRun
	for _, run := range p.runs {
		allRuns = append(allRuns, run)
	}
	allRuns = append(allRuns, p.draining...)
	p.runs = make(map[string]*managedRun)
	p.draining = nil
	p.session = nil
	p.lastError = ""
	p.mu.Unlock()

	var errs []string
	for _, run := range allRuns {
		if err := p.client.FinishRun(ctx, p.token, run.id, run.requestCount); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if session != nil && session.status != sessionStatusDisabled && session.instanceID != "" {
		if err := p.client.EndSession(ctx, p.token); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("switch token from model %s to %s: %s", currentModel, model, strings.Join(errs, "; "))
	}
	return nil
}

func waitingRoomErrorFromSession(token string, session *cachedSession, now time.Time) *waitingRoomError {
	if session == nil || session.status != sessionStatusQueued {
		return nil
	}
	if !session.pollAt.IsZero() && now.Before(session.pollAt) {
		return &waitingRoomError{
			Token:      token,
			Position:   session.position,
			QueueDepth: session.queueDepth,
			RetryAfter: time.Until(session.pollAt),
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (p *tokenPool) logQueuePosition(state freeSessionResponse) {
	var parts []string

	if state.QueueDepth > 0 {
		parts = append(parts, fmt.Sprintf("position %d/%d", state.Position, state.QueueDepth))
	} else if state.Position > 0 {
		parts = append(parts, fmt.Sprintf("position %d", state.Position))
	}

	if state.EstimatedWaitMs > 0 {
		parts = append(parts, "~"+formatWaitDuration(time.Duration(state.EstimatedWaitMs)*time.Millisecond)+" remaining")
	}

	if state.QueuedAt != "" {
		if queuedAt, err := time.Parse(time.RFC3339, state.QueuedAt); err == nil {
			parts = append(parts, "elapsed "+formatElapsedDuration(time.Since(queuedAt)))
		}
	}

	if len(parts) > 0 {
		p.logger.Printf("%s: waiting room: %s", p.name, strings.Join(parts, ", "))
	} else {
		p.logger.Printf("%s: waiting room: queued", p.name)
	}
}

func formatWaitDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Minute {
		return "< 1 min"
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%d min", minutes)
}

func formatElapsedDuration(d time.Duration) string {
	d = d.Round(time.Second)
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	if minutes > 0 {
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
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

func (c *UpstreamClient) CreateOrRefreshSession(ctx context.Context, authToken, model string) (freeSessionResponse, error) {
	return c.doSessionRequest(ctx, http.MethodPost, authToken, "", model)
}

func (c *UpstreamClient) GetSession(ctx context.Context, authToken, instanceID string) (freeSessionResponse, error) {
	return c.doSessionRequest(ctx, http.MethodGet, authToken, instanceID, "")
}

func (c *UpstreamClient) EndSession(ctx context.Context, authToken string) error {
	cfg := c.cfgStore.Current()
	requestURL, err := url.JoinPath(cfg.UpstreamBaseURL, "/api/v1/freebuff/session")
	if err != nil {
		return fmt.Errorf("build free session url: %w", err)
	}

	requestCtx, cancel := c.requestContext(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodDelete, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create free session delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cfg.UserAgent)

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

func (c *UpstreamClient) doSessionRequest(ctx context.Context, method, authToken, instanceID, model string) (freeSessionResponse, error) {
	cfg := c.cfgStore.Current()
	requestURL, err := url.JoinPath(cfg.UpstreamBaseURL, "/api/v1/freebuff/session")
	if err != nil {
		return freeSessionResponse{}, fmt.Errorf("build free session url: %w", err)
	}

	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	}

	requestCtx, cancel := c.requestContext(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, method, requestURL, body)
	if err != nil {
		return freeSessionResponse{}, fmt.Errorf("create free session request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", cfg.UserAgent)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		if strings.TrimSpace(model) != "" {
			req.Header.Set("x-freebuff-model", strings.TrimSpace(model))
		}
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

func firstNonEmptyTrimmedString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
