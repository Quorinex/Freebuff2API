package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	cfgStore  *ConfigStore
	logger    *log.Logger
	client    *UpstreamClient
	runs      *RunManager
	registry  *ModelRegistry
	responses *responseStore
	started   time.Time
}

type proxyErrorResponse struct {
	StatusCode int
	Message    string
	ErrorType  string
	Code       string
	RetryAfter time.Duration
}

func NewServer(cfg Config, logger *log.Logger, registry *ModelRegistry) *Server {
	cfgStore := NewConfigStore(cfg)
	client := NewUpstreamClient(cfgStore)
	runManager := NewRunManager(cfg, client, logger)

	return &Server{
		cfgStore:  cfgStore,
		logger:    logger,
		client:    client,
		runs:      runManager,
		registry:  registry,
		responses: newResponseStore(),
		started:   time.Now(),
	}
}

func (s *Server) ApplyConfig(cfg Config) {
	current := s.cfgStore.Current()
	if current.ListenAddr != "" && cfg.ListenAddr != current.ListenAddr {
		s.logger.Printf("LISTEN_ADDR changed from %s to %s but requires restart; keeping current listener", current.ListenAddr, cfg.ListenAddr)
		cfg.ListenAddr = current.ListenAddr
	}
	s.cfgStore.Update(cfg)
	s.runs.ApplyConfig(cfg)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/messages", s.handleClaudeMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleClaudeCountTokens)
	return s.withMiddleware(mux)
}

func (s *Server) Start(ctx context.Context) {
	s.runs.Start(ctx, s.registry.AgentIDs())
}

func (s *Server) Shutdown(ctx context.Context) {
	s.runs.Close(ctx)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfgStore.Current()
		if len(cfg.APIKeys) > 0 && !s.authorized(r, cfg.APIKeys) {
			if isClaudeRequestPath(r.URL.Path) {
				writeClaudeErrorDetailed(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error", "invalid_api_key")
			} else {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error", "invalid_api_key")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request, apiKeys []string) bool {
	if apiKey := strings.TrimSpace(r.Header.Get("x-api-key")); apiKey != "" {
		if containsString(apiKeys, apiKey) {
			return true
		}
	}

	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return false
	}
	apiKey := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	return containsString(apiKeys, apiKey)
}

func isClaudeRequestPath(path string) bool {
	return strings.HasPrefix(path, "/v1/messages")
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	response := map[string]any{
		"ok":         true,
		"started_at": s.started.UTC(),
		"uptime_sec": int(time.Since(s.started).Seconds()),
		"summary":    summarizeTokenSnapshots(s.runs.Snapshots()),
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	cfg := s.cfgStore.Current()
	snapshots := s.runs.Snapshots()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"started_at":       s.started.UTC(),
		"uptime_sec":       int(time.Since(s.started).Seconds()),
		"summary":          summarizeTokenSnapshots(snapshots),
		"available_models": s.registry.Models(),
		"token_state":      snapshots,
		"config": map[string]any{
			"listen_addr":        cfg.ListenAddr,
			"upstream_base_url":  cfg.UpstreamBaseURL,
			"rotation_interval":  cfg.RotationInterval.String(),
			"request_timeout":    cfg.RequestTimeout.String(),
			"auth_token_count":   len(cfg.AuthTokens),
			"api_key_count":      len(cfg.APIKeys),
			"config_path":        cfg.ConfigPath,
			"config_format":      cfg.ConfigFormat,
			"auth_token_dir":     cfg.AuthTokenDir,
			"loaded_at":          cfg.LoadedAt,
			"hot_reload_enabled": cfg.ConfigPath != "" || cfg.AuthTokenDir != "",
		},
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	created := s.started.Unix()
	modelsList := s.registry.Models()
	models := make([]map[string]any, 0, len(modelsList))
	for _, model := range modelsList {
		models = append(models, map[string]any{
			"id":         model,
			"object":     "model",
			"created":    created,
			"owned_by":   "Freebuff2API",
			"root":       model,
			"permission": []any{},
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   models,
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "")
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(requestBody, &payload); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body must be valid JSON", "invalid_request_error", "")
		return
	}

	requestedModel, _ := payload["model"].(string)
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		writeOpenAIError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "")
		return
	}

	s.proxyChatRequest(
		w,
		r,
		payload,
		requestedModel,
		"invalid_request_error",
		"server_error",
		writeOpenAIError,
		writePassthroughError,
		writeOpenAISuccessResponse,
	)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "")
		return
	}

	payload, requestedModel, stream, conversation, err := convertResponsesCreateRequestToOpenAI(requestBody, s.responses)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}

	if !s.registry.HasModel(requestedModel) {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), "invalid_request_error", "model_not_found")
		return
	}

	s.proxyChatRequest(
		w,
		r,
		payload,
		requestedModel,
		"invalid_request_error",
		"server_error",
		writeOpenAIError,
		writePassthroughError,
		func(w http.ResponseWriter, resp *http.Response) error {
			return writeResponsesSuccessResponse(w, resp, requestedModel, stream, conversation, s.responses)
		},
	)
}

func (s *Server) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeClaudeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}

	payload, requestedModel, stream, err := convertClaudeMessagesRequestToOpenAI(requestBody)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	if _, ok := s.registry.AgentForModel(requestedModel); !ok {
		writeClaudeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), "invalid_request_error")
		return
	}

	s.proxyChatRequest(
		w,
		r,
		payload,
		requestedModel,
		"invalid_request_error",
		"api_error",
		func(w http.ResponseWriter, statusCode int, message, errorType, _ string) {
			writeClaudeErrorDetailed(w, statusCode, message, errorType, "")
		},
		writeClaudePassthroughError,
		func(w http.ResponseWriter, resp *http.Response) error {
			return writeClaudeSuccessResponse(w, resp, requestedModel, stream)
		},
	)
}

func (s *Server) handleClaudeCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeClaudeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	requestBody, err := io.ReadAll(r.Body)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}

	payload, requestedModel, _, err := convertClaudeMessagesRequestToOpenAI(requestBody)
	if err != nil {
		writeClaudeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	if !s.registry.HasModel(requestedModel) {
		writeClaudeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), "invalid_request_error")
		return
	}

	count, err := countOpenAIPayloadTokens(requestedModel, payload)
	if err != nil {
		s.logger.Printf("count_tokens failed for model %s: %v", requestedModel, err)
		writeClaudeError(w, http.StatusBadGateway, "failed to estimate input tokens", "api_error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": count,
	})
}

func (s *Server) proxyChatRequest(
	w http.ResponseWriter,
	r *http.Request,
	payload map[string]any,
	requestedModel string,
	invalidRequestType string,
	serverErrorType string,
	writeError func(http.ResponseWriter, int, string, string, string),
	writeUpstreamError func(http.ResponseWriter, int, []byte),
	writeSuccess func(http.ResponseWriter, *http.Response) error,
) {
	startTime := time.Now()
	isClaude := isClaudeRequestPath(r.URL.Path)

	agentID, ok := s.registry.AgentForModel(requestedModel)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), invalidRequestType, "model_not_found")
		return
	}

	cfg := s.cfgStore.Current()
	maxAttempts := len(cfg.AuthTokens) + 1
	if maxAttempts < 2 {
		maxAttempts = 2
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		lease, err := s.runs.Acquire(r.Context(), agentID, requestedModel)
		if err != nil {
			s.writeProxyError(w, isClaude, mapAcquireError(err, serverErrorType))
			return
		}

		s.logger.Printf("[%s] Routing request (model: %s) via run: %s", lease.pool.name, requestedModel, lease.run.id)

		sessionInstanceID, err := lease.pool.ensureSession(r.Context(), requestedModel)
		if err != nil {
			s.runs.Release(lease)
			s.writeProxyError(w, isClaude, mapSessionAcquireError(err, serverErrorType))
			return
		}

		upstreamBody, err := s.injectUpstreamMetadata(payload, requestedModel, lease.run.id, sessionInstanceID)
		if err != nil {
			s.runs.Release(lease)
			writeError(w, http.StatusBadRequest, err.Error(), invalidRequestType, "")
			return
		}

		resp, errorBody, err := s.client.ChatCompletions(r.Context(), lease.pool.token, upstreamBody)
		if err != nil {
			s.runs.Release(lease)
			s.logger.Printf("[%s] upstream request failed: %v", lease.pool.name, err)
			s.writeProxyError(w, isClaude, proxyErrorResponse{
				StatusCode: http.StatusBadGateway,
				Message:    "upstream request failed",
				ErrorType:  serverErrorType,
				Code:       "upstream_request_failed",
			})
			return
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			defer resp.Body.Close()
			if err := writeSuccess(w, resp); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("[%s] proxy response copy failed: %v", lease.pool.name, err)
			}
			s.logger.Printf("[%s] Request completed successfully in %v (status: %d)", lease.pool.name, time.Since(startTime).Round(time.Millisecond), resp.StatusCode)
			s.runs.Release(lease)
			return
		}

		message, _, code := extractUpstreamError(errorBody)
		if isBannedErrorMessage(string(errorBody)) {
			s.logger.Printf("%s: upstream token banned, disabling token", lease.pool.name)
			lease.pool.disable("upstream token banned")
			s.runs.Release(lease)
			continue
		}
		if strings.TrimSpace(code) == "session_model_mismatch" {
			s.logger.Printf("%s: session model mismatch on run %s, rotating run and refreshing session", lease.pool.name, lease.run.id)
			lease.pool.invalidateSession(strings.TrimSpace(message))
			s.runs.Invalidate(lease, strings.TrimSpace(message))
			s.runs.Release(lease)
			continue
		}

		if isSessionInvalid(resp.StatusCode, errorBody) {
			s.logger.Printf("%s: free session invalid, refreshing and retrying", lease.pool.name)
			lease.pool.invalidateSession(strings.TrimSpace(string(errorBody)))
			s.runs.Release(lease)
			continue
		}

		if isRunInvalid(resp.StatusCode, errorBody) {
			s.logger.Printf("%s: run %s invalid, rotating and retrying", lease.pool.name, lease.run.id)
			s.runs.Invalidate(lease, strings.TrimSpace(string(errorBody)))
			s.runs.Release(lease)
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			s.runs.Cooldown(lease, 30*time.Minute, "upstream auth rejected token")
			lease.pool.invalidateSession("upstream auth rejected token")
		}

		s.runs.Release(lease)
		s.logger.Printf("[%s] upstream error response: %s", lease.pool.name, string(errorBody))
		_ = writeUpstreamError
		s.writeProxyError(w, isClaude, mapUpstreamProxyError(resp, errorBody, serverErrorType))
		return
	}

	_ = writeError
	s.writeProxyError(w, isClaude, proxyErrorResponse{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "upstream session is still switching models",
		ErrorType:  serverErrorType,
		Code:       "session_switch_in_progress",
		RetryAfter: 3 * time.Second,
	})
}

func (s *Server) writeProxyError(w http.ResponseWriter, isClaude bool, response proxyErrorResponse) {
	if response.StatusCode == 0 {
		response.StatusCode = http.StatusBadGateway
	}
	if response.ErrorType == "" {
		if isClaude {
			response.ErrorType = normalizeClaudeErrorType(response.StatusCode, "")
		} else {
			response.ErrorType = "upstream_error"
		}
	}
	if response.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%.0f", maxDuration(response.RetryAfter, time.Second).Seconds()))
	}
	if isClaude {
		writeClaudeErrorDetailed(w, response.StatusCode, response.Message, response.ErrorType, response.Code)
		return
	}
	writeOpenAIError(w, response.StatusCode, response.Message, response.ErrorType, response.Code)
}

func mapAcquireError(err error, serverErrorType string) proxyErrorResponse {
	var waitingErr *waitingRoomError
	if errors.As(err, &waitingErr) {
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "all free sessions are queued in the Freebuff waiting room",
			ErrorType:  serverErrorType,
			Code:       "waiting_room_queued",
			RetryAfter: maxDuration(waitingErr.RetryAfter, time.Second),
		}
	}
	var switchErr *modelSwitchError
	if errors.As(err, &switchErr) {
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "upstream session is still switching models",
			ErrorType:  serverErrorType,
			Code:       "session_switch_in_progress",
			RetryAfter: maxDuration(switchErr.RetryAfter, time.Second),
		}
	}
	return proxyErrorResponse{
		StatusCode: http.StatusServiceUnavailable,
		Message:    "no healthy upstream auth token available",
		ErrorType:  serverErrorType,
		Code:       "token_pool_unavailable",
		RetryAfter: 5 * time.Second,
	}
}

func mapSessionAcquireError(err error, serverErrorType string) proxyErrorResponse {
	var waitingErr *waitingRoomError
	if errors.As(err, &waitingErr) {
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "all free sessions are queued in the Freebuff waiting room",
			ErrorType:  serverErrorType,
			Code:       "waiting_room_queued",
			RetryAfter: maxDuration(waitingErr.RetryAfter, time.Second),
		}
	}
	var switchErr *modelSwitchError
	if errors.As(err, &switchErr) {
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "upstream session is still switching models",
			ErrorType:  serverErrorType,
			Code:       "session_switch_in_progress",
			RetryAfter: maxDuration(switchErr.RetryAfter, time.Second),
		}
	}
	return proxyErrorResponse{
		StatusCode: http.StatusBadGateway,
		Message:    "failed to acquire upstream free session",
		ErrorType:  serverErrorType,
		Code:       "token_pool_unavailable",
	}
}

func mapUpstreamProxyError(resp *http.Response, errorBody []byte, serverErrorType string) proxyErrorResponse {
	message, _, code := extractUpstreamError(errorBody)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "upstream auth token rejected the request",
			ErrorType:  serverErrorType,
			Code:       "upstream_auth_rejected",
			RetryAfter: 30 * time.Minute,
		}
	case resp.StatusCode == http.StatusTooManyRequests:
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "upstream rate limit reached",
			ErrorType:  serverErrorType,
			Code:       "upstream_rate_limited",
			RetryAfter: maxDuration(retryAfterDuration(resp.Header.Get("Retry-After")), 30*time.Second),
		}
	case strings.TrimSpace(code) == "waiting_room_queued":
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "all free sessions are queued in the Freebuff waiting room",
			ErrorType:  serverErrorType,
			Code:       "waiting_room_queued",
			RetryAfter: maxDuration(retryAfterDuration(resp.Header.Get("Retry-After")), 5*time.Second),
		}
	case strings.TrimSpace(code) == "session_model_mismatch":
		return proxyErrorResponse{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "upstream session is still switching models",
			ErrorType:  serverErrorType,
			Code:       "session_switch_in_progress",
			RetryAfter: 3 * time.Second,
		}
	default:
		if strings.TrimSpace(message) == "" {
			message = "upstream request failed"
		} else {
			message = "upstream request failed"
		}
		return proxyErrorResponse{
			StatusCode: http.StatusBadGateway,
			Message:    message,
			ErrorType:  serverErrorType,
			Code:       "upstream_request_failed",
		}
	}
}

func summarizeTokenSnapshots(snapshots []tokenSnapshot) map[string]any {
	summary := map[string]any{
		"total_tokens":  len(snapshots),
		"active":        0,
		"queued":        0,
		"disabled":      0,
		"banned":        0,
		"cooling_down":  0,
		"idle":          0,
		"healthy":       0,
		"service_ready": false,
	}

	for _, snapshot := range snapshots {
		state := snapshot.State
		if state == "" {
			state = classifyTokenState(snapshot)
		}
		if _, ok := summary[state]; ok {
			summary[state] = summary[state].(int) + 1
		}
		if state == "active" || state == "idle" {
			summary["healthy"] = summary["healthy"].(int) + 1
			summary["service_ready"] = true
		}
	}

	summary["message"] = fmt.Sprintf("%d healthy, %d queued, %d disabled", summary["healthy"], summary["queued"], summary["disabled"].(int)+summary["banned"].(int))
	return summary
}

func writeOpenAISuccessResponse(w http.ResponseWriter, resp *http.Response) error {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	return copyResponseBody(w, resp.Body)
}

func (s *Server) injectUpstreamMetadata(payload map[string]any, requestedModel, runID, sessionInstanceID string) ([]byte, error) {
	cloned := cloneMap(payload)
	cloned["model"] = requestedModel

	// Normalize tool parameter schemas into a conservative subset the upstream
	// backend can parse. This keeps LobeChat-style schemas working without
	// changing non-tool requests.
	if tools, ok := cloned["tools"].([]any); ok {
		normalizeToolSchemas(tools)
	}

	metadata, ok := cloned["codebuff_metadata"].(map[string]any)
	if !ok || metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["run_id"] = runID
	metadata["cost_mode"] = "free"
	metadata["client_id"] = generateClientSessionId()
	if strings.TrimSpace(sessionInstanceID) != "" {
		metadata["freebuff_instance_id"] = sessionInstanceID
	}
	cloned["codebuff_metadata"] = metadata

	body, err := json.Marshal(cloned)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream request: %w", err)
	}
	return body, nil
}

func isSessionInvalid(statusCode int, errorBody []byte) bool {
	if statusCode < 400 {
		return false
	}
	_, _, code := extractUpstreamError(errorBody)
	switch strings.TrimSpace(code) {
	case "freebuff_update_required", "waiting_room_required", "waiting_room_queued", "session_superseded", "session_expired", "session_model_mismatch":
		return true
	default:
		return false
	}
}

// normalizeToolSchemas rewrites tool parameter schemas into a conservative JSON
// Schema subset. Today that means resolving local $ref values and simplifying
// common nullable constructs emitted by clients like LobeChat.
func normalizeToolSchemas(tools []any) {
	for _, tool := range tools {
		toolMap, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := toolMap["function"].(map[string]any)
		if !ok {
			continue
		}
		params, ok := fn["parameters"].(map[string]any)
		if !ok {
			continue
		}
		fn["parameters"] = normalizeSchemaMap(params, extractDefinitions(params), 12)
	}
}

// extractDefinitions returns the combined definitions map from "definitions" and "$defs".
func extractDefinitions(schema map[string]any) map[string]any {
	merged := make(map[string]any)
	if d, ok := schema["definitions"].(map[string]any); ok {
		for key, value := range d {
			merged[key] = value
		}
	}
	if d, ok := schema["$defs"].(map[string]any); ok {
		for key, value := range d {
			merged[key] = value
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func mergeDefinitions(parent, local map[string]any) map[string]any {
	if len(parent) == 0 {
		return local
	}
	if len(local) == 0 {
		return parent
	}
	merged := make(map[string]any, len(parent)+len(local))
	for key, value := range parent {
		merged[key] = value
	}
	for key, value := range local {
		merged[key] = value
	}
	return merged
}

func normalizeSchemaValue(value any, defs map[string]any, maxDepth int) any {
	switch typed := value.(type) {
	case map[string]any:
		return normalizeSchemaMap(typed, defs, maxDepth)
	case []any:
		return normalizeSchemaSlice(typed, defs, maxDepth)
	default:
		return value
	}
}

func normalizeSchemaMap(node map[string]any, defs map[string]any, maxDepth int) map[string]any {
	if maxDepth <= 0 {
		return cloneMap(node)
	}

	defs = mergeDefinitions(defs, extractDefinitions(node))
	if replaced := tryResolveRef(node, defs); replaced != nil {
		if replacedMap, ok := replaced.(map[string]any); ok {
			return normalizeSchemaMap(replacedMap, defs, maxDepth-1)
		}
		return cloneMap(node)
	}

	normalized := make(map[string]any, len(node))
	for key, value := range node {
		normalized[key] = normalizeSchemaValue(value, defs, maxDepth-1)
	}

	delete(normalized, "definitions")
	delete(normalized, "$defs")
	delete(normalized, "nullable")

	normalized = simplifyNullableCombinator(normalized, "anyOf")
	normalized = simplifyNullableCombinator(normalized, "oneOf")
	normalizeTypeField(normalized)
	normalizeEnumField(normalized)
	normalizeConstField(normalized)

	return normalized
}

func normalizeSchemaSlice(slice []any, defs map[string]any, maxDepth int) []any {
	if maxDepth <= 0 {
		return cloneSlice(slice)
	}
	normalized := make([]any, len(slice))
	for i, value := range slice {
		normalized[i] = normalizeSchemaValue(value, defs, maxDepth-1)
	}
	return normalized
}

func simplifyNullableCombinator(schema map[string]any, key string) map[string]any {
	rawOptions, ok := schema[key].([]any)
	if !ok {
		return schema
	}

	filtered := make([]any, 0, len(rawOptions))
	for _, option := range rawOptions {
		if optionMap, ok := option.(map[string]any); ok && isNullSchema(optionMap) {
			continue
		}
		filtered = append(filtered, option)
	}

	if len(filtered) == 0 {
		delete(schema, key)
		return schema
	}

	if len(filtered) == 1 {
		if optionMap, ok := filtered[0].(map[string]any); ok {
			merged := make(map[string]any, len(schema)+len(optionMap))
			for existingKey, existingValue := range schema {
				if existingKey == key {
					continue
				}
				merged[existingKey] = existingValue
			}
			for optionKey, optionValue := range optionMap {
				merged[optionKey] = optionValue
			}
			return merged
		}
	}

	schema[key] = filtered
	return schema
}

func normalizeTypeField(schema map[string]any) {
	rawType, ok := schema["type"]
	if !ok {
		return
	}
	if _, ok := rawType.(string); ok {
		return
	}
	types, ok := rawType.([]any)
	if !ok {
		return
	}
	nonNullTypes := make([]string, 0, len(types))
	for _, entry := range types {
		typeName, ok := entry.(string)
		if !ok || typeName == "null" || strings.TrimSpace(typeName) == "" {
			continue
		}
		nonNullTypes = append(nonNullTypes, typeName)
	}
	switch len(nonNullTypes) {
	case 0:
		delete(schema, "type")
	case 1:
		schema["type"] = nonNullTypes[0]
	default:
		// Upstream expects a single primitive type. Keep the first non-null type
		// rather than failing the whole request.
		schema["type"] = nonNullTypes[0]
	}
}

func normalizeEnumField(schema map[string]any) {
	enumValues, ok := schema["enum"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(enumValues))
	seen := make(map[string]struct{}, len(enumValues))
	for _, entry := range enumValues {
		if entry == nil {
			continue
		}
		key := fmt.Sprintf("%T:%v", entry, entry)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		delete(schema, "enum")
		return
	}
	schema["enum"] = filtered
}

func normalizeConstField(schema map[string]any) {
	if value, ok := schema["const"]; ok && value == nil {
		delete(schema, "const")
	}
}

func isNullSchema(schema map[string]any) bool {
	if typeName, ok := schema["type"].(string); ok && typeName == "null" {
		return true
	}
	if constValue, ok := schema["const"]; ok && constValue == nil {
		return true
	}
	if enumValues, ok := schema["enum"].([]any); ok && len(enumValues) == 1 && enumValues[0] == nil {
		return true
	}
	return false
}

// tryResolveRef checks if a node is a $ref object like {"$ref": "#/definitions/Foo"}
// and returns the cloned definition if found.
func tryResolveRef(node map[string]any, defs map[string]any) any {
	ref, ok := node["$ref"].(string)
	if !ok || len(node) != 1 {
		return nil
	}
	// Support both "#/definitions/X" and "#/$defs/X"
	var name string
	if strings.HasPrefix(ref, "#/definitions/") {
		name = strings.TrimPrefix(ref, "#/definitions/")
	} else if strings.HasPrefix(ref, "#/$defs/") {
		name = strings.TrimPrefix(ref, "#/$defs/")
	}
	if name == "" {
		return nil
	}
	def, ok := defs[name]
	if !ok {
		return nil
	}
	// Clone to avoid mutating the original definition
	if defMap, ok := def.(map[string]any); ok {
		return cloneMap(defMap)
	}
	return def
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[key] = cloneMap(typed)
		case []any:
			output[key] = cloneSlice(typed)
		default:
			output[key] = value
		}
	}
	return output
}

func cloneSlice(input []any) []any {
	output := make([]any, len(input))
	for index, value := range input {
		switch typed := value.(type) {
		case map[string]any:
			output[index] = cloneMap(typed)
		case []any:
			output[index] = cloneSlice(typed)
		default:
			output[index] = value
		}
	}
	return output
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyResponseBody(w http.ResponseWriter, body io.Reader) error {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func isRunInvalid(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(string(body))
	return strings.Contains(message, "runid not found") || strings.Contains(message, "runid not running")
}

func writePassthroughError(w http.ResponseWriter, statusCode int, body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		message, errorType, code := extractUpstreamError(trimmed)
		writeOpenAIError(w, statusCode, message, errorType, code)
		return
	}
	writeOpenAIError(w, statusCode, strings.TrimSpace(string(trimmed)), "upstream_error", "")
}

func extractUpstreamError(body []byte) (message, errorType, code string) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return strings.TrimSpace(string(body)), "upstream_error", ""
	}

	errorType = "upstream_error"

	if rawError, ok := payload["error"]; ok {
		switch typed := rawError.(type) {
		case string:
			code = typed
		case map[string]any:
			if value, ok := typed["message"].(string); ok && strings.TrimSpace(value) != "" {
				message = value
			}
			if value, ok := typed["type"].(string); ok && strings.TrimSpace(value) != "" {
				errorType = value
			}
			if value, ok := typed["code"].(string); ok && strings.TrimSpace(value) != "" {
				code = value
			}
		}
	}

	if value, ok := payload["message"].(string); ok && strings.TrimSpace(value) != "" {
		message = value
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	return message, errorType, code
}

func writeOpenAIError(w http.ResponseWriter, statusCode int, message, errorType, code string) {
	if message == "" {
		message = http.StatusText(statusCode)
	}
	payload := map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
		},
	}
	if code != "" {
		payload["error"].(map[string]any)["code"] = code
	}
	writeJSON(w, statusCode, payload)
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":{"message":"failed to encode response","type":"server_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_, _ = w.Write(body)
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
