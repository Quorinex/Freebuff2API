package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RunManager struct {
	logger *log.Logger
	client *UpstreamClient

	mu    sync.RWMutex
	cfg   Config
	pools []*tokenPool

	next atomic.Uint64

	stopCh chan struct{}
	wg     sync.WaitGroup
}

type tokenPool struct {
	name   string
	token  string
	cfg    Config
	client *UpstreamClient
	logger *log.Logger

	mu               sync.Mutex
	runs             map[string]*managedRun
	draining         []*managedRun
	session          *cachedSession
	sessionRefreshCh chan struct{}
	lastError        string
	cooldownUntil    time.Time
	disabled         bool
}

type managedRun struct {
	id           string
	agentID      string
	model        string
	startedAt    time.Time
	inflight     int
	requestCount int
	finishing    bool
}

type runLease struct {
	pool *tokenPool
	run  *managedRun
}

type tokenSnapshot struct {
	Name              string        `json:"name"`
	Runs              []runSnapshot `json:"runs"`
	DrainingRuns      int           `json:"draining_runs"`
	SessionModel      string        `json:"session_model,omitempty"`
	SessionStatus     string        `json:"session_status,omitempty"`
	SessionInstanceID string        `json:"session_instance_id,omitempty"`
	SessionExpiresAt  time.Time     `json:"session_expires_at,omitempty"`
	SessionPosition   int           `json:"session_position,omitempty"`
	SessionQueueDepth int           `json:"session_queue_depth,omitempty"`
	SessionPollAt     time.Time     `json:"session_poll_at,omitempty"`
	CooldownUntil     time.Time     `json:"cooldown_until,omitempty"`
	LastError         string        `json:"last_error,omitempty"`
	Disabled          bool          `json:"disabled,omitempty"`
	State             string        `json:"state"`
}

type runSnapshot struct {
	AgentID      string    `json:"agent_id"`
	Model        string    `json:"model,omitempty"`
	RunID        string    `json:"run_id"`
	StartedAt    time.Time `json:"started_at"`
	Inflight     int       `json:"inflight"`
	RequestCount int       `json:"request_count"`
}

type waitingRoomError struct {
	Token      string
	Position   int
	QueueDepth int
	RetryAfter time.Duration
}

func (e *waitingRoomError) Error() string {
	if e == nil {
		return "freebuff waiting room queued"
	}

	message := "freebuff waiting room queued"
	if e.Token != "" {
		message += " for " + e.Token
	}
	if e.Position > 0 {
		if e.QueueDepth >= e.Position {
			message += fmt.Sprintf(" (position %d/%d)", e.Position, e.QueueDepth)
		} else {
			message += fmt.Sprintf(" (position %d)", e.Position)
		}
	}
	if e.RetryAfter > 0 {
		message += fmt.Sprintf(", retry in about %s", e.RetryAfter.Round(time.Second))
	}
	return message
}

func NewRunManager(cfg Config, client *UpstreamClient, logger *log.Logger) *RunManager {
	manager := &RunManager{
		cfg:    cfg,
		logger: logger,
		client: client,
		stopCh: make(chan struct{}),
	}
	manager.pools = manager.buildPools(cfg, nil)
	return manager
}

func (m *RunManager) Start(ctx context.Context, agentIDs []string) {
	go m.prewarm(agentIDs)

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				cfg := m.currentConfig()
				maintainCtx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
				for _, pool := range m.snapshotPools() {
					if err := pool.maintain(maintainCtx); err != nil {
						m.logger.Printf("%s: maintenance failed: %v", pool.name, err)
					}
				}
				cancel()
			case <-m.stopCh:
				return
			}
		}
	}()
}

func (m *RunManager) ApplyConfig(cfg Config) {
	m.mu.Lock()
	existing := make(map[string]*tokenPool, len(m.pools))
	for _, pool := range m.pools {
		pool.cfg = cfg
		existing[pool.token] = pool
	}

	m.cfg = cfg
	m.pools = m.buildPools(cfg, existing)
	removed := make([]*tokenPool, 0, len(existing))
	for _, pool := range existing {
		removed = append(removed, pool)
	}
	m.mu.Unlock()

	for _, pool := range removed {
		go func(pool *tokenPool) {
			ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
			defer cancel()
			if err := pool.shutdown(ctx); err != nil {
				m.logger.Printf("%s: shutdown removed token failed: %v", pool.name, err)
			}
		}(pool)
	}
}

func (m *RunManager) buildPools(cfg Config, existing map[string]*tokenPool) []*tokenPool {
	pools := make([]*tokenPool, 0, len(cfg.AuthTokens))
	for index, token := range cfg.AuthTokens {
		if pool := existing[token]; pool != nil {
			pool.name = fmt.Sprintf("token-%d", index+1)
			pool.cfg = cfg
			pools = append(pools, pool)
			delete(existing, token)
			continue
		}
		pools = append(pools, &tokenPool{
			name:   fmt.Sprintf("token-%d", index+1),
			token:  token,
			cfg:    cfg,
			client: m.client,
			runs:   make(map[string]*managedRun),
			logger: m.logger,
		})
	}
	return pools
}

func (m *RunManager) prewarm(agentIDs []string) {
	cfg := m.currentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
	defer cancel()

	for _, pool := range m.snapshotPools() {
		if pool.isDisabled() {
			continue
		}
		for _, agentID := range agentIDs {
			if err := pool.rotateAgent(ctx, agentID, ""); err != nil {
				m.logger.Printf("%s: prewarm %s failed: %v", pool.name, agentID, err)
			} else {
				m.logger.Printf("%s: prewarmed %s", pool.name, agentID)
			}
		}
	}
}

func (m *RunManager) Close(ctx context.Context) {
	close(m.stopCh)
	m.wg.Wait()
	for _, pool := range m.snapshotPools() {
		if err := pool.shutdown(ctx); err != nil {
			m.logger.Printf("%s: shutdown failed: %v", pool.name, err)
		}
	}
}

func (m *RunManager) Acquire(ctx context.Context, agentID, model string) (*runLease, error) {
	pools := m.snapshotPools()
	if len(pools) == 0 {
		return nil, errors.New("no auth tokens configured")
	}

	startIndex := int(m.next.Add(1)-1) % len(pools)
	var errs []string
	var waiting []*waitingRoomError
	var switching []*modelSwitchError
	for offset := 0; offset < len(pools); offset++ {
		pool := pools[(startIndex+offset)%len(pools)]
		lease, err := pool.acquire(ctx, agentID, model)
		if err == nil {
			return lease, nil
		}
		var waitingErr *waitingRoomError
		if errors.As(err, &waitingErr) {
			waiting = append(waiting, waitingErr)
		}
		var switchErr *modelSwitchError
		if errors.As(err, &switchErr) {
			switching = append(switching, switchErr)
		}
		errs = append(errs, fmt.Sprintf("%s: %v", pool.name, err))
	}

	if len(waiting) == len(pools) && len(waiting) > 0 {
		best := waiting[0]
		for _, candidate := range waiting[1:] {
			if candidate != nil && (best == nil || (candidate.Position > 0 && candidate.Position < best.Position)) {
				best = candidate
			}
		}
		if best != nil {
			return nil, best
		}
	}

	if len(switching) == len(pools) && len(switching) > 0 {
		best := switching[0]
		for _, candidate := range switching[1:] {
			if candidate != nil && candidate.RetryAfter > 0 && (best == nil || best.RetryAfter <= 0 || candidate.RetryAfter < best.RetryAfter) {
				best = candidate
			}
		}
		if best != nil {
			return nil, best
		}
	}

	return nil, fmt.Errorf("unable to acquire run from any token (%s)", strings.Join(errs, "; "))
}

func (m *RunManager) Release(lease *runLease) {
	if lease == nil || lease.pool == nil || lease.run == nil {
		return
	}
	lease.pool.release(lease.run)
}

func (m *RunManager) Invalidate(lease *runLease, reason string) {
	if lease == nil || lease.pool == nil || lease.run == nil {
		return
	}
	lease.pool.invalidate(lease.run, reason)
}

func (m *RunManager) Cooldown(lease *runLease, duration time.Duration, reason string) {
	if lease == nil || lease.pool == nil {
		return
	}
	lease.pool.markCooldown(duration, reason)
}

func (m *RunManager) Snapshots() []tokenSnapshot {
	pools := m.snapshotPools()
	snapshots := make([]tokenSnapshot, 0, len(pools))
	for _, pool := range pools {
		snapshots = append(snapshots, pool.snapshot())
	}
	return snapshots
}

func (m *RunManager) currentConfig() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *RunManager) snapshotPools() []*tokenPool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pools := make([]*tokenPool, len(m.pools))
	copy(pools, m.pools)
	return pools
}

func (p *tokenPool) acquire(ctx context.Context, agentID, model string) (*runLease, error) {
	p.mu.Lock()
	if p.disabled {
		lastError := p.lastError
		p.mu.Unlock()
		if lastError == "" {
			lastError = "token disabled"
		}
		return nil, errors.New(lastError)
	}
	p.mu.Unlock()

	if err := p.prepareModel(ctx, model); err != nil {
		return nil, err
	}

	p.mu.Lock()
	if now := time.Now(); now.Before(p.cooldownUntil) {
		cooldownUntil := p.cooldownUntil
		p.mu.Unlock()
		return nil, fmt.Errorf("token cooling down until %s", cooldownUntil.Format(time.RFC3339))
	}
	run := p.runs[agentID]
	needsRotate := run == nil || run.model != model || time.Since(run.startedAt) >= p.cfg.RotationInterval
	p.mu.Unlock()

	if needsRotate {
		if err := p.rotateAgent(ctx, agentID, model); err != nil {
			return nil, err
		}
	}

	if _, err := p.ensureSession(ctx, model); err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	run = p.runs[agentID]
	if run == nil {
		return nil, errors.New("run missing after rotation")
	}
	run.inflight++
	run.requestCount++
	return &runLease{pool: p, run: run}, nil
}

func (p *tokenPool) maintain(ctx context.Context) error {
	if p.isDisabled() {
		return nil
	}
	if model := p.currentSessionModel(); model != "" {
		if _, err := p.ensureSession(ctx, model); err != nil {
			p.logger.Printf("%s: refresh free session failed: %v", p.name, err)
		}
	}
	if p.isDisabled() {
		return nil
	}

	p.mu.Lock()
	var toRotate []string
	for agentID, run := range p.runs {
		if time.Since(run.startedAt) >= p.cfg.RotationInterval {
			toRotate = append(toRotate, agentID)
		}
	}
	draining := append([]*managedRun(nil), p.draining...)
	p.mu.Unlock()

	for _, agentID := range toRotate {
		model := ""
		p.mu.Lock()
		if run := p.runs[agentID]; run != nil {
			model = run.model
		}
		p.mu.Unlock()
		if err := p.rotateAgent(ctx, agentID, model); err != nil {
			p.logger.Printf("%s: rotate agent %s failed: %v", p.name, agentID, err)
		}
	}

	for _, run := range draining {
		if err := p.finishIfReady(run); err != nil {
			p.logger.Printf("%s: finish draining run %s failed: %v", p.name, run.id, err)
		}
	}
	return nil
}

func (p *tokenPool) shutdown(ctx context.Context) error {
	p.mu.Lock()
	var allRuns []*managedRun
	for _, run := range p.runs {
		allRuns = append(allRuns, run)
	}
	allRuns = append(allRuns, p.draining...)
	p.runs = make(map[string]*managedRun)
	p.draining = nil
	p.mu.Unlock()

	var errs []string
	for _, run := range allRuns {
		if err := p.client.FinishRun(ctx, p.token, run.id, run.requestCount); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if err := p.endSession(ctx); err != nil {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (p *tokenPool) rotateAgent(ctx context.Context, agentID, model string) error {
	p.mu.Lock()
	if p.disabled {
		lastError := p.lastError
		p.mu.Unlock()
		if lastError == "" {
			lastError = "token disabled"
		}
		return errors.New(lastError)
	}
	if now := time.Now(); now.Before(p.cooldownUntil) {
		cooldownUntil := p.cooldownUntil
		p.mu.Unlock()
		return fmt.Errorf("token cooling down until %s", cooldownUntil.Format(time.RFC3339))
	}
	p.mu.Unlock()

	runID, err := p.client.StartRun(ctx, p.token, agentID)
	if err != nil {
		if isBannedErrorMessage(err.Error()) {
			p.disable("upstream token banned")
			return err
		}
		p.mu.Lock()
		p.lastError = err.Error()
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	oldRun := p.runs[agentID]
	p.runs[agentID] = &managedRun{
		id:        runID,
		agentID:   agentID,
		model:     model,
		startedAt: time.Now(),
	}
	p.lastError = ""
	if oldRun != nil {
		p.draining = append(p.draining, oldRun)
	}
	p.mu.Unlock()

	if oldRun != nil {
		go func(run *managedRun) {
			if err := p.finishIfReady(run); err != nil {
				p.logger.Printf("%s: finish rotated run %s (agent %s) failed: %v", p.name, run.id, run.agentID, err)
			}
		}(oldRun)
	}
	return nil
}

func (p *tokenPool) release(run *managedRun) {
	if run == nil {
		return
	}

	p.mu.Lock()
	if run.inflight > 0 {
		run.inflight--
	}
	p.mu.Unlock()

	if err := p.finishIfReady(run); err != nil {
		p.logger.Printf("%s: finish released run %s failed: %v", p.name, run.id, err)
	}
}

func (p *tokenPool) finishIfReady(run *managedRun) error {
	p.mu.Lock()
	if run == nil || run.inflight > 0 || run.finishing {
		p.mu.Unlock()
		return nil
	}
	if current, ok := p.runs[run.agentID]; ok && current == run {
		p.mu.Unlock()
		return nil
	}
	run.finishing = true
	timeout := p.cfg.RequestTimeout
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := p.client.FinishRun(ctx, p.token, run.id, run.requestCount); err != nil {
		p.mu.Lock()
		run.finishing = false
		p.lastError = err.Error()
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	filtered := p.draining[:0]
	for _, drainingRun := range p.draining {
		if drainingRun != run {
			filtered = append(filtered, drainingRun)
		}
	}
	p.draining = filtered
	p.mu.Unlock()
	return nil
}

func (p *tokenPool) invalidate(run *managedRun, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if current, ok := p.runs[run.agentID]; ok && current == run {
		delete(p.runs, run.agentID)
	}

	filtered := p.draining[:0]
	for _, drainingRun := range p.draining {
		if drainingRun != run {
			filtered = append(filtered, drainingRun)
		}
	}
	p.draining = filtered
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) markCooldown(duration time.Duration, reason string) {
	if duration <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownUntil = time.Now().Add(duration)
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) snapshot() tokenSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	snapshot := tokenSnapshot{
		Name:          p.name,
		DrainingRuns:  len(p.draining),
		CooldownUntil: p.cooldownUntil,
		LastError:     p.lastError,
		Disabled:      p.disabled,
	}
	if p.session != nil {
		snapshot.SessionModel = p.session.model
		snapshot.SessionStatus = string(p.session.status)
		snapshot.SessionInstanceID = p.session.instanceID
		snapshot.SessionExpiresAt = p.session.expiresAt
		snapshot.SessionPosition = p.session.position
		snapshot.SessionQueueDepth = p.session.queueDepth
		snapshot.SessionPollAt = p.session.pollAt
	}
	for agentID, run := range p.runs {
		snapshot.Runs = append(snapshot.Runs, runSnapshot{
			AgentID:      agentID,
			Model:        run.model,
			RunID:        run.id,
			StartedAt:    run.startedAt,
			Inflight:     run.inflight,
			RequestCount: run.requestCount,
		})
	}
	snapshot.State = classifyTokenState(snapshot)
	return snapshot
}

func (p *tokenPool) disable(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disabled = true
	p.session = nil
	p.cooldownUntil = time.Time{}
	if reason != "" {
		p.lastError = reason
	}
}

func (p *tokenPool) isDisabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disabled
}

func classifyTokenState(snapshot tokenSnapshot) string {
	now := time.Now()
	switch {
	case snapshot.Disabled && strings.Contains(strings.ToLower(snapshot.LastError), "banned"):
		return "banned"
	case snapshot.Disabled:
		return "disabled"
	case !snapshot.CooldownUntil.IsZero() && now.Before(snapshot.CooldownUntil):
		return "cooling_down"
	case snapshot.SessionStatus == string(sessionStatusQueued):
		return "queued"
	case snapshot.SessionStatus == string(sessionStatusActive):
		return "active"
	case snapshot.SessionStatus == string(sessionStatusDisabled):
		return "disabled"
	default:
		return "idle"
	}
}

func isBannedErrorMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, `"status":"banned"`) || strings.Contains(message, `"status": "banned"`) || strings.Contains(message, "status\":\"banned")
}
