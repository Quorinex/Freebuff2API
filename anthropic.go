package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

type claudeMessageParts struct {
	ContentParts   []any
	BeforeMessages []any
	AfterMessages  []any
	ToolCalls      []any
	Reasoning      string
}

func convertClaudeMessagesRequestToOpenAI(body []byte) (map[string]any, string, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, "", false, fmt.Errorf("request body must be valid JSON")
	}

	modelName := strings.TrimSpace(stringValue(root["model"]))
	if modelName == "" {
		return nil, "", false, fmt.Errorf("model is required")
	}

	stream := boolValue(root["stream"])
	out := map[string]any{
		"model":    modelName,
		"messages": []any{},
		"stream":   stream,
	}

	if maxTokens, ok := intValue(root["max_tokens"]); ok && maxTokens > 0 {
		out["max_tokens"] = maxTokens
	}

	if temperature, ok := floatValue(root["temperature"]); ok {
		out["temperature"] = temperature
	} else if topP, ok := floatValue(root["top_p"]); ok {
		out["top_p"] = topP
	}

	if len(stringSliceValue(root["stop_sequences"])) > 0 {
		stops := stringSliceValue(root["stop_sequences"])
		if len(stops) == 1 {
			out["stop"] = stops[0]
		} else {
			out["stop"] = stops
		}
	}

	if reasoningEffort, ok := mapClaudeThinkingToReasoningEffort(root); ok {
		out["reasoning_effort"] = reasoningEffort
	}

	messages := make([]any, 0, 8)
	if system := root["system"]; system != nil {
		if systemMessage := convertClaudeSystemToOpenAIMessage(system); systemMessage != nil {
			messages = append(messages, systemMessage)
		}
	}

	rawMessages := sliceValue(root["messages"])
	if rawMessages == nil {
		return nil, "", false, fmt.Errorf("messages must be an array")
	}

	for _, rawMessage := range rawMessages {
		message := mapValue(rawMessage)
		if message == nil {
			continue
		}

		role := strings.TrimSpace(stringValue(message["role"]))
		if role == "" {
			continue
		}

		parts := convertClaudeMessageContent(role, message["content"])
		for _, toolResult := range parts.BeforeMessages {
			messages = append(messages, toolResult)
		}

		switch role {
		case "assistant":
			if len(parts.ContentParts) == 0 && len(parts.ToolCalls) == 0 && parts.Reasoning == "" {
				continue
			}

			openAIMessage := map[string]any{"role": "assistant"}
			if len(parts.ContentParts) > 0 {
				openAIMessage["content"] = normalizeOpenAIContent(parts.ContentParts)
			} else {
				openAIMessage["content"] = ""
			}
			if parts.Reasoning != "" {
				openAIMessage["reasoning_content"] = parts.Reasoning
			}
			if len(parts.ToolCalls) > 0 {
				openAIMessage["tool_calls"] = parts.ToolCalls
			}
			messages = append(messages, openAIMessage)

		case "user":
			if len(parts.ContentParts) == 0 {
				continue
			}
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": normalizeOpenAIContent(parts.ContentParts),
			})
		}

		for _, toolResult := range parts.AfterMessages {
			messages = append(messages, toolResult)
		}
	}

	out["messages"] = messages

	builtinToolKinds := make(map[string]string)
	if rawTools := sliceValue(root["tools"]); len(rawTools) > 0 {
		tools := make([]any, 0, len(rawTools))
		for _, rawTool := range rawTools {
			tool := mapValue(rawTool)
			if tool == nil {
				continue
			}

			mappedTool, builtinKind := convertClaudeToolDefinitionToOpenAI(tool)
			if mappedTool == nil {
				continue
			}

			if builtinKind != "" {
				if toolName := strings.TrimSpace(stringValue(tool["name"])); toolName != "" {
					builtinToolKinds[toolName] = builtinKind
				}
			}

			tools = append(tools, mappedTool)
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}

	if toolChoice := mapValue(root["tool_choice"]); toolChoice != nil {
		if mappedToolChoice, ok := convertClaudeToolChoiceToOpenAI(toolChoice, builtinToolKinds); ok {
			out["tool_choice"] = mappedToolChoice
		}
	}

	if userValue := strings.TrimSpace(stringValue(root["user"])); userValue != "" {
		out["user"] = userValue
	}

	return out, modelName, stream, nil
}

func convertClaudeToolDefinitionToOpenAI(tool map[string]any) (map[string]any, string) {
	toolType := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
	if mappedType, ok := mapClaudeBuiltinToolType(toolType); ok {
		mapped := cloneMap(tool)
		mapped["type"] = mappedType
		delete(mapped, "name")
		return mapped, mappedType
	}

	function := map[string]any{
		"name":        stringValue(tool["name"]),
		"description": stringValue(tool["description"]),
	}

	if inputSchema := tool["input_schema"]; inputSchema != nil {
		function["parameters"] = inputSchema
	}

	return map[string]any{
		"type":     "function",
		"function": function,
	}, ""
}

func convertClaudeToolChoiceToOpenAI(toolChoice map[string]any, builtinToolKinds map[string]string) (any, bool) {
	switch strings.ToLower(strings.TrimSpace(stringValue(toolChoice["type"]))) {
	case "none":
		return "none", true
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "tool":
		toolName := strings.TrimSpace(stringValue(toolChoice["name"]))
		if toolName == "" {
			return nil, false
		}
		if builtinType := builtinToolKinds[toolName]; builtinType != "" {
			return map[string]any{"type": builtinType}, true
		}
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": toolName,
			},
		}, true
	default:
		return nil, false
	}
}

func mapClaudeBuiltinToolType(toolType string) (string, bool) {
	switch toolType {
	case "web_search_20250305", "web_search":
		return "web_search", true
	default:
		return "", false
	}
}

func convertClaudeSystemToOpenAIMessage(system any) map[string]any {
	switch typed := system.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return nil
		}
		return map[string]any{
			"role":    "system",
			"content": text,
		}
	case []any:
		contentParts := make([]any, 0, len(typed))
		for _, rawPart := range typed {
			part := mapValue(rawPart)
			if part == nil {
				continue
			}
			if strings.EqualFold(stringValue(part["type"]), "text") {
				text := stringValue(part["text"])
				if strings.TrimSpace(text) == "" {
					continue
				}
				contentParts = append(contentParts, map[string]any{
					"type": "text",
					"text": text,
				})
			}
		}
		if len(contentParts) == 0 {
			return nil
		}
		return map[string]any{
			"role":    "system",
			"content": normalizeOpenAIContent(contentParts),
		}
	default:
		return nil
	}
}

func convertClaudeMessageContent(role string, content any) claudeMessageParts {
	result := claudeMessageParts{
		ContentParts:   make([]any, 0),
		BeforeMessages: make([]any, 0),
		AfterMessages:  make([]any, 0),
		ToolCalls:      make([]any, 0),
	}

	switch typed := content.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return result
		}
		result.ContentParts = append(result.ContentParts, map[string]any{"type": "text", "text": typed})
		return result
	case []any:
		reasoningParts := make([]string, 0)

		for _, rawPart := range typed {
			part := mapValue(rawPart)
			if part == nil {
				continue
			}

			switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
			case "text":
				text := stringValue(part["text"])
				if strings.TrimSpace(text) == "" {
					continue
				}
				result.ContentParts = append(result.ContentParts, map[string]any{
					"type": "text",
					"text": text,
				})

			case "image":
				if imagePart := convertClaudeImagePartToOpenAI(part); imagePart != nil {
					result.ContentParts = append(result.ContentParts, imagePart)
				}

			case "tool_use", "server_tool_use":
				if role != "assistant" {
					continue
				}
				toolCallID := sanitizeClaudeToolID(stringValue(part["id"]))
				result.ToolCalls = append(result.ToolCalls, map[string]any{
					"id":   toolCallID,
					"type": "function",
					"function": map[string]any{
						"name":      stringValue(part["name"]),
						"arguments": marshalJSONObject(part["input"]),
					},
				})

			case "tool_result":
				if toolMessage := buildOpenAIToolResultMessage(part["tool_use_id"], part["content"]); toolMessage != nil {
					result.BeforeMessages = append(result.BeforeMessages, toolMessage)
				}

			case "thinking":
				if role != "assistant" {
					continue
				}
				thinkingText := strings.TrimSpace(firstNonEmptyString(part["thinking"], part["text"]))
				if thinkingText != "" {
					reasoningParts = append(reasoningParts, thinkingText)
				}

			default:
				partType := strings.ToLower(strings.TrimSpace(stringValue(part["type"])))
				switch {
				case strings.HasSuffix(partType, "_tool_use"):
					if role != "assistant" {
						continue
					}
					toolCallID := sanitizeClaudeToolID(firstNonEmptyString(part["tool_use_id"], part["id"]))
					result.ToolCalls = append(result.ToolCalls, map[string]any{
						"id":   toolCallID,
						"type": "function",
						"function": map[string]any{
							"name":      firstNonEmptyString(part["name"], part["tool_name"]),
							"arguments": marshalJSONObject(part["input"]),
						},
					})
				case strings.HasSuffix(partType, "_tool_result"):
					toolContent := any(cloneMap(part))
					if contentValue, ok := part["content"]; ok {
						toolContent = contentValue
					}
					if toolMessage := buildOpenAIToolResultMessage(firstNonEmptyString(part["tool_use_id"], part["id"]), toolContent); toolMessage != nil {
						if role == "assistant" {
							result.AfterMessages = append(result.AfterMessages, toolMessage)
						} else {
							result.BeforeMessages = append(result.BeforeMessages, toolMessage)
						}
					}
				default:
					if fallbackPart := convertClaudeUnknownContentPartToOpenAI(part); fallbackPart != nil {
						result.ContentParts = append(result.ContentParts, fallbackPart)
					}
				}
			}
		}

		result.Reasoning = strings.Join(reasoningParts, "\n\n")
		return result
	default:
		return result
	}
}

func buildOpenAIToolResultMessage(toolUseID any, content any) map[string]any {
	rawToolUseID := strings.TrimSpace(stringValue(toolUseID))
	if rawToolUseID == "" {
		return nil
	}
	sanitizedToolUseID := sanitizeClaudeToolID(rawToolUseID)

	return map[string]any{
		"role":         "tool",
		"tool_call_id": sanitizedToolUseID,
		"content":      convertClaudeToolResultContentToOpenAI(content),
	}
}

func convertClaudeUnknownContentPartToOpenAI(part map[string]any) map[string]any {
	encoded, err := json.Marshal(part)
	if err != nil || len(encoded) == 0 {
		return nil
	}

	return map[string]any{
		"type": "text",
		"text": string(encoded),
	}
}

func convertClaudeImagePartToOpenAI(part map[string]any) map[string]any {
	imageURL := ""

	if source := mapValue(part["source"]); source != nil {
		switch strings.ToLower(strings.TrimSpace(stringValue(source["type"]))) {
		case "base64":
			data := stringValue(source["data"])
			if data != "" {
				mediaType := stringValue(source["media_type"])
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				imageURL = "data:" + mediaType + ";base64," + data
			}
		case "url":
			imageURL = stringValue(source["url"])
		}
	}

	if imageURL == "" {
		imageURL = stringValue(part["url"])
	}
	if strings.TrimSpace(imageURL) == "" {
		return nil
	}

	return map[string]any{
		"type": "image_url",
		"image_url": map[string]any{
			"url": imageURL,
		},
	}
}

func convertClaudeToolResultContentToOpenAI(content any) any {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		contentParts := make([]any, 0, len(typed))
		hasStructuredPart := false

		for _, rawItem := range typed {
			switch item := rawItem.(type) {
			case string:
				text := item
				parts = append(parts, text)
				contentParts = append(contentParts, map[string]any{"type": "text", "text": text})
			case map[string]any:
				switch strings.ToLower(strings.TrimSpace(stringValue(item["type"]))) {
				case "text":
					text := stringValue(item["text"])
					parts = append(parts, text)
					contentParts = append(contentParts, map[string]any{"type": "text", "text": text})
				case "image":
					if imagePart := convertClaudeImagePartToOpenAI(item); imagePart != nil {
						contentParts = append(contentParts, imagePart)
						hasStructuredPart = true
					}
				default:
					hasStructuredPart = true
					encoded, _ := json.Marshal(item)
					parts = append(parts, string(encoded))
				}
			default:
				encoded, _ := json.Marshal(item)
				parts = append(parts, string(encoded))
			}
		}

		if hasStructuredPart {
			if len(contentParts) > 0 {
				return normalizeOpenAIContent(contentParts)
			}
			encoded, _ := json.Marshal(typed)
			return string(encoded)
		}

		if len(contentParts) > 0 {
			return normalizeOpenAIContent(contentParts)
		}
		return strings.Join(parts, "\n\n")
	case map[string]any:
		switch strings.ToLower(strings.TrimSpace(stringValue(typed["type"]))) {
		case "text":
			return stringValue(typed["text"])
		case "image":
			if imagePart := convertClaudeImagePartToOpenAI(typed); imagePart != nil {
				return []any{imagePart}
			}
		}
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	default:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
}

func mapClaudeThinkingToReasoningEffort(root map[string]any) (string, bool) {
	thinking := mapValue(root["thinking"])
	if thinking == nil {
		return "", false
	}

	switch strings.ToLower(strings.TrimSpace(stringValue(thinking["type"]))) {
	case "disabled":
		return "none", true
	case "enabled":
		if budget, ok := intValue(thinking["budget_tokens"]); ok {
			return budgetToReasoningEffort(budget), true
		}
		return "auto", true
	case "adaptive", "auto":
		outputConfig := mapValue(root["output_config"])
		effort := strings.ToLower(strings.TrimSpace(stringValue(outputConfig["effort"])))
		switch effort {
		case "", "auto":
			return "auto", true
		case "low", "medium", "high":
			return effort, true
		case "max":
			return "xhigh", true
		default:
			return "auto", true
		}
	default:
		return "", false
	}
}

func budgetToReasoningEffort(budget int) string {
	switch {
	case budget <= 0:
		return "none"
	case budget <= 512:
		return "minimal"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	default:
		return "xhigh"
	}
}

func normalizeOpenAIContent(contentParts []any) any {
	if len(contentParts) == 0 {
		return ""
	}

	if len(contentParts) == 1 {
		if part := mapValue(contentParts[0]); part != nil && strings.EqualFold(stringValue(part["type"]), "text") {
			return stringValue(part["text"])
		}
	}

	return contentParts
}

func marshalJSONObject(value any) string {
	switch typed := value.(type) {
	case nil:
		return "{}"
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "{}"
		}
		if json.Valid([]byte(trimmed)) {
			return trimmed
		}
		encoded, _ := json.Marshal(trimmed)
		return string(encoded)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil || len(encoded) == 0 {
			return "{}"
		}
		return string(encoded)
	}
}

func sanitizeClaudeToolID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "toolu_" + generateClientSessionId()
	}

	var builder strings.Builder
	for _, ch := range id {
		switch {
		case ch >= 'a' && ch <= 'z':
			builder.WriteRune(ch)
		case ch >= 'A' && ch <= 'Z':
			builder.WriteRune(ch)
		case ch >= '0' && ch <= '9':
			builder.WriteRune(ch)
		case ch == '_' || ch == '-':
			builder.WriteRune(ch)
		}
	}

	if builder.Len() == 0 {
		return "toolu_" + generateClientSessionId()
	}
	return builder.String()
}

func writeClaudeSuccessResponse(w http.ResponseWriter, resp *http.Response, requestedModel string, stream bool) error {
	if stream {
		return writeClaudeStreamingResponse(w, resp, requestedModel)
	}
	return writeClaudeNonStreamResponse(w, resp)
}

func writeClaudeNonStreamResponse(w http.ResponseWriter, resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	converted, err := convertOpenAINonStreamResponseToClaude(body)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(converted)
	return err
}

func writeClaudeStreamingResponse(w http.ResponseWriter, resp *http.Response, requestedModel string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(resp.Body)
	state := &claudeStreamState{
		Model:               requestedModel,
		TextContentBlockIdx: -1,
		ThinkingBlockIdx:    -1,
		ToolBlocks:          make(map[int]*claudeToolCallState),
		ToolBlockIndexes:    make(map[int]int),
	}

	sawDone := false
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 && !bytes.HasPrefix(trimmed, []byte(":")) && bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[5:])
				if bytes.Equal(payload, []byte("[DONE]")) {
					sawDone = true
				}

				events, convErr := convertOpenAIStreamPayloadToClaudeEvents(payload, state)
				if convErr != nil {
					return convErr
				}
				if err := writeClaudeSSEEvents(w, events); err != nil {
					return err
				}
				if flusher != nil && len(events) > 0 {
					flusher.Flush()
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	if !sawDone {
		events, err := finalizeClaudeStream(state)
		if err != nil {
			return err
		}
		if err := writeClaudeSSEEvents(w, events); err != nil {
			return err
		}
		if flusher != nil && len(events) > 0 {
			flusher.Flush()
		}
	}

	return nil
}

type openAIChatCompletion struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

type openAIChoice struct {
	Index        int           `json:"index"`
	FinishReason string        `json:"finish_reason"`
	Message      openAIMessage `json:"message"`
	Delta        openAIDelta   `json:"delta"`
}

type openAIMessage struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
	ReasoningContent json.RawMessage  `json:"reasoning_content,omitempty"`
}

type openAIDelta struct {
	Role             string                 `json:"role"`
	Content          string                 `json:"content"`
	ToolCalls        []openAIStreamToolCall `json:"tool_calls,omitempty"`
	ReasoningContent json.RawMessage        `json:"reasoning_content,omitempty"`
}

type openAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIStreamToolCall struct {
	Index    int            `json:"index"`
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIUsage struct {
	PromptTokens        int64                     `json:"prompt_tokens"`
	CompletionTokens    int64                     `json:"completion_tokens"`
	TotalTokens         int64                     `json:"total_tokens"`
	PromptTokensDetails *openAIPromptTokensDetail `json:"prompt_tokens_details,omitempty"`
}

type openAIPromptTokensDetail struct {
	CachedTokens int64 `json:"cached_tokens"`
}

func convertOpenAINonStreamResponseToClaude(body []byte) ([]byte, error) {
	var response openAIChatCompletion
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode upstream response: %w", err)
	}

	message := map[string]any{
		"id":            response.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         response.Model,
		"content":       []any{},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}

	hasToolCall := false
	if len(response.Choices) > 0 {
		choice := response.Choices[0]
		for _, text := range collectReasoningTexts(choice.Message.ReasoningContent) {
			message["content"] = append(message["content"].([]any), map[string]any{
				"type":     "thinking",
				"thinking": text,
			})
		}
		for _, block := range convertOpenAIContentToClaudeBlocks(choice.Message.Content) {
			message["content"] = append(message["content"].([]any), block)
		}
		for _, toolCall := range choice.Message.ToolCalls {
			hasToolCall = true
			message["content"] = append(message["content"].([]any), map[string]any{
				"type":  "tool_use",
				"id":    sanitizeClaudeToolID(toolCall.ID),
				"name":  toolCall.Function.Name,
				"input": parseJSONObject(toolCall.Function.Arguments),
			})
		}
		if choice.FinishReason != "" {
			message["stop_reason"] = mapOpenAIFinishReasonToClaude(choice.FinishReason)
		}
	}

	if response.Usage != nil {
		inputTokens, outputTokens, cachedTokens := extractOpenAIUsage(response.Usage)
		usage := message["usage"].(map[string]any)
		usage["input_tokens"] = inputTokens
		usage["output_tokens"] = outputTokens
		if cachedTokens > 0 {
			usage["cache_read_input_tokens"] = cachedTokens
		}
	}

	if message["stop_reason"] == "end_turn" && hasToolCall {
		message["stop_reason"] = "tool_use"
	}

	return json.Marshal(message)
}

type claudeStreamState struct {
	MessageID            string
	Model                string
	MessageStarted       bool
	TextContentStarted   bool
	TextContentBlockIdx  int
	ThinkingStarted      bool
	ThinkingBlockIdx     int
	NextBlockIdx         int
	ToolBlocks           map[int]*claudeToolCallState
	ToolBlockIndexes     map[int]int
	FinishReason         string
	SawToolCall          bool
	MessageDeltaSent     bool
	MessageStopSent      bool
	ContentBlocksStopped bool
}

type claudeToolCallState struct {
	ID        string
	Name      string
	Started   bool
	Arguments strings.Builder
}

type claudeSSEEvent struct {
	Name    string
	Payload []byte
}

func convertOpenAIStreamPayloadToClaudeEvents(payload []byte, state *claudeStreamState) ([]claudeSSEEvent, error) {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return finalizeClaudeStream(state)
	}

	var chunk openAIChatCompletion
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, fmt.Errorf("decode upstream stream chunk: %w", err)
	}

	events := make([]claudeSSEEvent, 0, 8)
	if state.MessageID == "" {
		state.MessageID = chunk.ID
	}
	if state.MessageID == "" {
		state.MessageID = "msg_" + generateClientSessionId()
	}
	if strings.TrimSpace(chunk.Model) != "" {
		state.Model = chunk.Model
	}

	if !state.MessageStarted {
		messageStartPayload, err := json.Marshal(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            state.MessageID,
				"type":          "message",
				"role":          "assistant",
				"model":         state.Model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		})
		if err != nil {
			return nil, err
		}
		events = append(events, claudeSSEEvent{Name: "message_start", Payload: messageStartPayload})
		state.MessageStarted = true
	}

	if len(chunk.Choices) == 0 {
		return events, nil
	}

	choice := chunk.Choices[0]
	for _, text := range collectReasoningTexts(choice.Delta.ReasoningContent) {
		stopTextContentBlock(state, &events)
		if !state.ThinkingStarted {
			index := nextClaudeBlockIndex(state, &state.ThinkingBlockIdx)
			payload, err := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":     "thinking",
					"thinking": "",
				},
			})
			if err != nil {
				return nil, err
			}
			events = append(events, claudeSSEEvent{Name: "content_block_start", Payload: payload})
			state.ThinkingStarted = true
		}

		payload, err := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": state.ThinkingBlockIdx,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": text,
			},
		})
		if err != nil {
			return nil, err
		}
		events = append(events, claudeSSEEvent{Name: "content_block_delta", Payload: payload})
	}

	if choice.Delta.Content != "" {
		stopThinkingContentBlock(state, &events)
		if !state.TextContentStarted {
			index := nextClaudeBlockIndex(state, &state.TextContentBlockIdx)
			payload, err := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			if err != nil {
				return nil, err
			}
			events = append(events, claudeSSEEvent{Name: "content_block_start", Payload: payload})
			state.TextContentStarted = true
		}

		payload, err := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": state.TextContentBlockIdx,
			"delta": map[string]any{
				"type": "text_delta",
				"text": choice.Delta.Content,
			},
		})
		if err != nil {
			return nil, err
		}
		events = append(events, claudeSSEEvent{Name: "content_block_delta", Payload: payload})
	}

	for _, toolCall := range choice.Delta.ToolCalls {
		state.SawToolCall = true
		stopThinkingContentBlock(state, &events)
		stopTextContentBlock(state, &events)

		accumulator := state.ToolBlocks[toolCall.Index]
		if accumulator == nil {
			accumulator = &claudeToolCallState{}
			state.ToolBlocks[toolCall.Index] = accumulator
		}

		if strings.TrimSpace(toolCall.ID) != "" {
			accumulator.ID = toolCall.ID
		}
		if strings.TrimSpace(toolCall.Function.Name) != "" {
			accumulator.Name = toolCall.Function.Name
		}
		if toolCall.Function.Arguments != "" {
			accumulator.Arguments.WriteString(toolCall.Function.Arguments)
		}

		if !accumulator.Started && accumulator.Name != "" {
			blockIndex := toolCallBlockIndex(state, toolCall.Index)
			payload, err := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    sanitizeClaudeToolID(accumulator.ID),
					"name":  accumulator.Name,
					"input": map[string]any{},
				},
			})
			if err != nil {
				return nil, err
			}
			events = append(events, claudeSSEEvent{Name: "content_block_start", Payload: payload})
			accumulator.Started = true
		}
	}

	if choice.FinishReason != "" {
		state.FinishReason = choice.FinishReason
		var err error
		events, err = appendClaudeFinalContentEvents(events, state)
		if err != nil {
			return nil, err
		}
	}

	if state.FinishReason != "" && chunk.Usage != nil {
		var err error
		events, err = appendClaudeMessageDeltaAndStop(events, state, chunk.Usage)
		if err != nil {
			return nil, err
		}
	}

	return events, nil
}

func finalizeClaudeStream(state *claudeStreamState) ([]claudeSSEEvent, error) {
	events := make([]claudeSSEEvent, 0, 6)
	var err error
	events, err = appendClaudeFinalContentEvents(events, state)
	if err != nil {
		return nil, err
	}
	return appendClaudeMessageDeltaAndStop(events, state, nil)
}

func appendClaudeFinalContentEvents(events []claudeSSEEvent, state *claudeStreamState) ([]claudeSSEEvent, error) {
	if state.ContentBlocksStopped {
		return events, nil
	}

	stopThinkingContentBlock(state, &events)
	stopTextContentBlock(state, &events)

	indexes := make([]int, 0, len(state.ToolBlocks))
	for index := range state.ToolBlocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	for _, index := range indexes {
		toolCall := state.ToolBlocks[index]
		if !toolCall.Started {
			continue
		}

		blockIndex := toolCallBlockIndex(state, index)
		partialJSON := strings.TrimSpace(toolCall.Arguments.String())
		if partialJSON != "" {
			payload, err := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": partialJSON,
				},
			})
			if err != nil {
				return nil, err
			}
			events = append(events, claudeSSEEvent{Name: "content_block_delta", Payload: payload})
		}

		stopPayload, err := json.Marshal(map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, claudeSSEEvent{Name: "content_block_stop", Payload: stopPayload})
	}

	state.ContentBlocksStopped = true
	return events, nil
}

func appendClaudeMessageDeltaAndStop(events []claudeSSEEvent, state *claudeStreamState, usage *openAIUsage) ([]claudeSSEEvent, error) {
	if !state.MessageDeltaSent {
		inputTokens, outputTokens, cachedTokens := int64(0), int64(0), int64(0)
		if usage != nil {
			inputTokens, outputTokens, cachedTokens = extractOpenAIUsage(usage)
		}

		usagePayload := map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		}
		if cachedTokens > 0 {
			usagePayload["cache_read_input_tokens"] = cachedTokens
		}

		payload, err := json.Marshal(map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   mapOpenAIFinishReasonToClaude(effectiveOpenAIFinishReason(state)),
				"stop_sequence": nil,
			},
			"usage": usagePayload,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, claudeSSEEvent{Name: "message_delta", Payload: payload})
		state.MessageDeltaSent = true
	}

	if !state.MessageStopSent {
		payload, err := json.Marshal(map[string]any{"type": "message_stop"})
		if err != nil {
			return nil, err
		}
		events = append(events, claudeSSEEvent{Name: "message_stop", Payload: payload})
		state.MessageStopSent = true
	}

	return events, nil
}

func effectiveOpenAIFinishReason(state *claudeStreamState) string {
	if state.SawToolCall {
		return "tool_calls"
	}
	if strings.TrimSpace(state.FinishReason) == "" {
		return "stop"
	}
	return state.FinishReason
}

func nextClaudeBlockIndex(state *claudeStreamState, slot *int) int {
	if *slot >= 0 {
		return *slot
	}
	index := state.NextBlockIdx
	state.NextBlockIdx++
	*slot = index
	return index
}

func toolCallBlockIndex(state *claudeStreamState, toolIndex int) int {
	if index, ok := state.ToolBlockIndexes[toolIndex]; ok {
		return index
	}
	index := state.NextBlockIdx
	state.NextBlockIdx++
	state.ToolBlockIndexes[toolIndex] = index
	return index
}

func stopThinkingContentBlock(state *claudeStreamState, events *[]claudeSSEEvent) {
	if !state.ThinkingStarted {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": state.ThinkingBlockIdx,
	})
	if err == nil {
		*events = append(*events, claudeSSEEvent{Name: "content_block_stop", Payload: payload})
	}
	state.ThinkingStarted = false
	state.ThinkingBlockIdx = -1
}

func stopTextContentBlock(state *claudeStreamState, events *[]claudeSSEEvent) {
	if !state.TextContentStarted {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": state.TextContentBlockIdx,
	})
	if err == nil {
		*events = append(*events, claudeSSEEvent{Name: "content_block_stop", Payload: payload})
	}
	state.TextContentStarted = false
	state.TextContentBlockIdx = -1
}

func writeClaudeSSEEvents(w http.ResponseWriter, events []claudeSSEEvent) error {
	for _, event := range events {
		if _, err := io.WriteString(w, "event: "+event.Name+"\n"); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "data: "); err != nil {
			return err
		}
		if _, err := w.Write(event.Payload); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n\n"); err != nil {
			return err
		}
	}
	return nil
}

func convertOpenAIContentToClaudeBlocks(raw json.RawMessage) []any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}

	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil && strings.TrimSpace(text) != "" {
			return []any{map[string]any{"type": "text", "text": text}}
		}
		return nil
	}

	if raw[0] != '[' {
		return nil
	}

	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}

	blocks := make([]any, 0, len(items))
	for _, item := range items {
		switch strings.ToLower(strings.TrimSpace(stringValue(item["type"]))) {
		case "text":
			text := stringValue(item["text"])
			if strings.TrimSpace(text) == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		case "reasoning":
			text := strings.TrimSpace(firstNonEmptyString(item["text"], item["thinking"]))
			if text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": text})
		case "tool_calls":
			for _, rawToolCall := range sliceValue(item["tool_calls"]) {
				toolCall := mapValue(rawToolCall)
				if toolCall == nil {
					continue
				}
				function := mapValue(toolCall["function"])
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    sanitizeClaudeToolID(stringValue(toolCall["id"])),
					"name":  stringValue(function["name"]),
					"input": parseJSONObject(stringValue(function["arguments"])),
				})
			}
		}
	}

	return blocks
}

func collectReasoningTexts(raw json.RawMessage) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}

	texts := make([]string, 0)
	collectReasoningTextValues(value, &texts)
	return texts
}

func collectReasoningTextValues(value any, out *[]string) {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) != "" {
			*out = append(*out, typed)
		}
	case []any:
		for _, item := range typed {
			collectReasoningTextValues(item, out)
		}
	case map[string]any:
		if text := strings.TrimSpace(firstNonEmptyString(typed["text"], typed["thinking"])); text != "" {
			*out = append(*out, text)
		}
	}
}

func parseJSONObject(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}

	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return map[string]any{}
	}

	if object, ok := value.(map[string]any); ok {
		return object
	}
	return map[string]any{}
}

func extractOpenAIUsage(usage *openAIUsage) (int64, int64, int64) {
	if usage == nil {
		return 0, 0, 0
	}

	inputTokens := usage.PromptTokens
	outputTokens := usage.CompletionTokens
	cachedTokens := int64(0)
	if usage.PromptTokensDetails != nil {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
	}

	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}

	return inputTokens, outputTokens, cachedTokens
}

func mapOpenAIFinishReasonToClaude(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func writeClaudePassthroughError(w http.ResponseWriter, statusCode int, body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && json.Valid(trimmed) {
		message, errorType, _ := extractUpstreamError(trimmed)
		writeClaudeError(w, statusCode, message, normalizeClaudeErrorType(statusCode, errorType))
		return
	}
	writeClaudeError(w, statusCode, strings.TrimSpace(string(trimmed)), normalizeClaudeErrorType(statusCode, ""))
}

func writeClaudeError(w http.ResponseWriter, statusCode int, message, errorType string) {
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(statusCode)
	}
	if strings.TrimSpace(errorType) == "" {
		errorType = normalizeClaudeErrorType(statusCode, "")
	}

	writeJSON(w, statusCode, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errorType,
			"message": message,
		},
	})
}

func normalizeClaudeErrorType(statusCode int, upstreamType string) string {
	switch strings.TrimSpace(upstreamType) {
	case "invalid_request_error", "authentication_error", "permission_error", "not_found_error", "rate_limit_error", "api_error", "overloaded_error":
		return upstreamType
	}

	switch statusCode {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func mapValue(value any) map[string]any {
	typed, _ := value.(map[string]any)
	return typed
}

func sliceValue(value any) []any {
	typed, _ := value.([]any)
	return typed
}

func stringSliceValue(value any) []string {
	values := sliceValue(value)
	if len(values) == 0 {
		return nil
	}

	out := make([]string, 0, len(values))
	for _, entry := range values {
		text := strings.TrimSpace(stringValue(entry))
		if text == "" {
			continue
		}
		out = append(out, text)
	}
	return out
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

func boolValue(value any) bool {
	typed, _ := value.(bool)
	return typed
}

func intValue(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	default:
		return 0, false
	}
}

func floatValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		text := strings.TrimSpace(stringValue(value))
		if text != "" {
			return text
		}
	}
	return ""
}
