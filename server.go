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
	cfg     Config
	logger  *log.Logger
	client   *UpstreamClient
	runs     *RunManager
	registry *ModelRegistry
	started  time.Time
}

func NewServer(cfg Config, logger *log.Logger, registry *ModelRegistry) *Server {
	client := NewUpstreamClient(cfg)
	runManager := NewRunManager(cfg, client, logger)

	return &Server{
		cfg:     cfg,
		logger:  logger,
		client:   client,
		runs:     runManager,
		registry: registry,
		started:  time.Now(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
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
		if len(s.cfg.APIKeys) > 0 && !s.authorized(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid proxy api key", "authentication_error", "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		return false
	}
	apiKey := strings.TrimSpace(strings.TrimPrefix(authorization, prefix))
	return containsString(s.cfg.APIKeys, apiKey)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
		return
	}

	response := map[string]any{
		"ok":          true,
		"started_at":  s.started.UTC(),
		"uptime_sec":  int(time.Since(s.started).Seconds()),
		"token_state": s.runs.Snapshots(),
	}
	writeJSON(w, http.StatusOK, response)
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
			"owned_by":   "Freebuff-Go",
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
	agentID, ok := s.registry.AgentForModel(requestedModel)
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("unsupported model %q", requestedModel), "invalid_request_error", "model_not_found")
		return
	}

	startTime := time.Now()

	for attempt := 0; attempt < 2; attempt++ {
		lease, err := s.runs.Acquire(r.Context(), agentID)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "no healthy upstream auth token available", "server_error", "")
			return
		}

		s.logger.Printf("[%s] Routing request (model: %s) via run: %s", lease.pool.name, requestedModel, lease.run.id)

		upstreamBody, err := s.injectUpstreamMetadata(payload, requestedModel, lease.run.id)
		if err != nil {
			s.runs.Release(lease)
			writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}

		resp, errorBody, err := s.client.ChatCompletions(r.Context(), lease.pool.token, upstreamBody)
		if err != nil {
			s.runs.Release(lease)
			writeOpenAIError(w, http.StatusBadGateway, err.Error(), "server_error", "")
			return
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			defer resp.Body.Close()
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			if err := copyResponseBody(w, resp.Body); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Printf("[%s] proxy response copy failed: %v", lease.pool.name, err)
			}
			s.logger.Printf("[%s] Request completed successfully in %v (status: %d)", lease.pool.name, time.Since(startTime).Round(time.Millisecond), resp.StatusCode)
			s.runs.Release(lease)
			return
		}

		if isRunInvalid(resp.StatusCode, errorBody) {
			s.logger.Printf("%s: run %s invalid, rotating and retrying", lease.pool.name, lease.run.id)
			s.runs.Invalidate(lease, strings.TrimSpace(string(errorBody)))
			s.runs.Release(lease)
			continue
		}

		if resp.StatusCode == http.StatusUnauthorized {
			s.runs.Cooldown(lease, 30*time.Minute, "upstream auth rejected token")
		}

		s.runs.Release(lease)
		s.logger.Printf("[%s] upstream error response: %s", lease.pool.name, string(errorBody))
		writePassthroughError(w, resp.StatusCode, errorBody)
		return
	}

	writeOpenAIError(w, http.StatusBadGateway, "upstream run expired twice in a row", "server_error", "")
}

func (s *Server) injectUpstreamMetadata(payload map[string]any, requestedModel, runID string) ([]byte, error) {
	cloned := cloneMap(payload)
	cloned["model"] = requestedModel

	metadata, ok := cloned["codebuff_metadata"].(map[string]any)
	if !ok || metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["run_id"] = runID
	metadata["cost_mode"] = "free"
	metadata["client_id"] = generateClientSessionId()
	cloned["codebuff_metadata"] = metadata

	body, err := json.Marshal(cloned)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream request: %w", err)
	}
	return body, nil
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
