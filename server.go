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

		upstreamBody, err := s.injectUpstreamMetadata(payload, requestedModel, lease.run.id, lease.pool.currentSessionInstanceID())
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
		writePassthroughError(w, resp.StatusCode, errorBody)
		return
	}

	writeOpenAIError(w, http.StatusBadGateway, "upstream run expired twice in a row", "server_error", "")
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
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(errorBody, &payload); err != nil {
		return false
	}
	switch strings.TrimSpace(payload.Error) {
	case "freebuff_update_required", "waiting_room_required", "waiting_room_queued", "session_superseded", "session_expired":
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
