package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type responseStore struct {
	mu    sync.RWMutex
	items map[string]storedResponse
}

type storedResponse struct {
	Conversation []map[string]any
}

func newResponseStore() *responseStore {
	return &responseStore{
		items: make(map[string]storedResponse),
	}
}

func (s *responseStore) GetConversation(id string) ([]map[string]any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.items[strings.TrimSpace(id)]
	if !ok {
		return nil, false
	}
	return cloneResponseItems(entry.Conversation), true
}

func (s *responseStore) Put(id string, conversation []map[string]any) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = storedResponse{Conversation: cloneResponseItems(conversation)}
}

func convertResponsesCreateRequestToOpenAI(body []byte, store *responseStore) (map[string]any, string, bool, []map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, "", false, nil, fmt.Errorf("request body must be valid JSON")
	}

	modelName := strings.TrimSpace(stringValue(root["model"]))
	if modelName == "" {
		return nil, "", false, nil, fmt.Errorf("model is required")
	}

	conversation := make([]map[string]any, 0, 16)
	if previousID := strings.TrimSpace(stringValue(root["previous_response_id"])); previousID != "" {
		previousConversation, ok := store.GetConversation(previousID)
		if !ok {
			return nil, "", false, nil, fmt.Errorf("unknown previous_response_id %q", previousID)
		}
		conversation = append(conversation, previousConversation...)
	}

	if instructions := normalizeResponsesInstructions(root["instructions"]); instructions != nil {
		conversation = append(conversation, instructions)
	}

	inputItems, err := normalizeResponsesInputItems(root["input"])
	if err != nil {
		return nil, "", false, nil, err
	}
	if len(inputItems) == 0 {
		return nil, "", false, nil, fmt.Errorf("input is required")
	}
	conversation = append(conversation, inputItems...)

	messages := make([]any, 0, len(conversation)+2)
	for _, item := range conversation {
		var appendErr error
		messages, appendErr = appendResponsesItemToOpenAIMessages(messages, item)
		if appendErr != nil {
			return nil, "", false, nil, appendErr
		}
	}

	payload := map[string]any{
		"model":    modelName,
		"messages": messages,
	}

	stream := boolValue(root["stream"])
	payload["stream"] = stream

	if maxOutputTokens, ok := intValue(root["max_output_tokens"]); ok && maxOutputTokens > 0 {
		payload["max_tokens"] = maxOutputTokens
	} else if maxTokens, ok := intValue(root["max_completion_tokens"]); ok && maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	} else if maxTokens, ok := intValue(root["max_tokens"]); ok && maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}

	if temperature, ok := floatValue(root["temperature"]); ok {
		payload["temperature"] = temperature
	}
	if topP, ok := floatValue(root["top_p"]); ok {
		payload["top_p"] = topP
	}
	if userValue := strings.TrimSpace(stringValue(root["user"])); userValue != "" {
		payload["user"] = userValue
	}

	if reasoning := mapValue(root["reasoning"]); reasoning != nil {
		if effort := strings.TrimSpace(stringValue(reasoning["effort"])); effort != "" {
			if normalized, ok := normalizeReasoningEffort(effort); ok {
				payload["reasoning_effort"] = normalized
			}
		}
	}

	if tools := convertResponsesToolsToOpenAI(root["tools"]); len(tools) > 0 {
		payload["tools"] = tools
	}
	if toolChoice, ok := convertResponsesToolChoiceToOpenAI(root["tool_choice"]); ok {
		payload["tool_choice"] = toolChoice
	}

	return payload, modelName, stream, conversation, nil
}

func normalizeResponsesInstructions(value any) map[string]any {
	text := strings.TrimSpace(stringValue(value))
	if text == "" {
		return nil
	}
	return map[string]any{
		"type": "message",
		"role": "system",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": text,
			},
		},
	}
}

func normalizeResponsesInputItems(value any) ([]map[string]any, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return nil, nil
		}
		return []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": text,
					},
				},
			},
		}, nil
	case []any:
		items := make([]map[string]any, 0, len(typed))
		for _, rawItem := range typed {
			item, err := normalizeResponsesInputItem(rawItem)
			if err != nil {
				return nil, err
			}
			if item != nil {
				items = append(items, item)
			}
		}
		return items, nil
	case map[string]any:
		item, err := normalizeResponsesInputItem(typed)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return nil, nil
		}
		return []map[string]any{item}, nil
	default:
		text := strings.TrimSpace(fmt.Sprint(value))
		if text == "" {
			return nil, nil
		}
		return []map[string]any{
			{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": text,
					},
				},
			},
		}, nil
	}
}

func normalizeResponsesInputItem(value any) (map[string]any, error) {
	item := mapValue(value)
	if item == nil {
		if text, ok := value.(string); ok {
			text = strings.TrimSpace(text)
			if text == "" {
				return nil, nil
			}
			return map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": text},
				},
			}, nil
		}
		return nil, fmt.Errorf("input items must be objects")
	}

	itemType := strings.TrimSpace(stringValue(item["type"]))
	if itemType == "" && strings.TrimSpace(stringValue(item["role"])) != "" {
		itemType = "message"
	}

	switch itemType {
	case "message":
		role := strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
		if role == "" {
			role = "user"
		}
		if role != "user" && role != "assistant" && role != "system" && role != "developer" {
			return nil, fmt.Errorf("unsupported input role %q", role)
		}
		content := normalizeResponsesContent(item["content"], role)
		if len(content) == 0 {
			return nil, nil
		}
		return map[string]any{
			"type":    "message",
			"role":    role,
			"content": content,
		}, nil

	case "function_call":
		name := strings.TrimSpace(stringValue(item["name"]))
		if name == "" {
			return nil, fmt.Errorf("function_call name is required")
		}
		callID := strings.TrimSpace(stringValue(item["call_id"]))
		itemID := strings.TrimSpace(stringValue(item["id"]))
		if callID == "" && itemID != "" {
			callID = responseCallIDFromValue(itemID)
		}
		if callID == "" {
			callID = responseCallIDFromValue(name)
		}
		if itemID == "" {
			itemID = responseFunctionIDFromCallID(callID)
		}
		return map[string]any{
			"type":      "function_call",
			"id":        itemID,
			"call_id":   callID,
			"name":      name,
			"arguments": normalizeJSONString(item["arguments"]),
		}, nil

	case "function_call_output":
		callID := strings.TrimSpace(stringValue(item["call_id"]))
		if callID == "" {
			return nil, fmt.Errorf("function_call_output call_id is required")
		}
		return map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  normalizeResponseOutputValue(item["output"]),
		}, nil

	default:
		return nil, fmt.Errorf("unsupported input item type %q", itemType)
	}
}

func normalizeResponsesContent(value any, role string) []any {
	textType := "input_text"
	if role == "assistant" {
		textType = "output_text"
	}

	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": textType, "text": text}}
	case []any:
		parts := make([]any, 0, len(typed))
		for _, rawPart := range typed {
			part := mapValue(rawPart)
			if part == nil {
				text := strings.TrimSpace(fmt.Sprint(rawPart))
				if text == "" {
					continue
				}
				parts = append(parts, map[string]any{"type": textType, "text": text})
				continue
			}

			partType := strings.ToLower(strings.TrimSpace(stringValue(part["type"])))
			switch partType {
			case "", "text", "input_text", "output_text":
				text := stringValue(part["text"])
				if strings.TrimSpace(text) == "" {
					continue
				}
				parts = append(parts, map[string]any{"type": textType, "text": text})
			case "input_image", "image":
				if imageURL := firstResponseImageURL(part); imageURL != "" && role != "assistant" {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": imageURL})
				}
			}
		}
		return parts
	default:
		text := strings.TrimSpace(fmt.Sprint(value))
		if text == "" {
			return nil
		}
		return []any{map[string]any{"type": textType, "text": text}}
	}
}

func firstResponseImageURL(part map[string]any) string {
	if url := strings.TrimSpace(stringValue(part["image_url"])); url != "" {
		return url
	}
	imageURL := mapValue(part["image_url"])
	if imageURL != nil {
		if url := strings.TrimSpace(stringValue(imageURL["url"])); url != "" {
			return url
		}
	}
	if source := mapValue(part["source"]); source != nil {
		if strings.EqualFold(stringValue(source["type"]), "url") {
			return strings.TrimSpace(stringValue(source["url"]))
		}
		if strings.EqualFold(stringValue(source["type"]), "base64") {
			mediaType := strings.TrimSpace(stringValue(source["media_type"]))
			data := strings.TrimSpace(stringValue(source["data"]))
			if mediaType != "" && data != "" {
				return "data:" + mediaType + ";base64," + data
			}
		}
	}
	return ""
}

func appendResponsesItemToOpenAIMessages(messages []any, item map[string]any) ([]any, error) {
	switch strings.TrimSpace(stringValue(item["type"])) {
	case "message":
		role := strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
		if role == "developer" {
			role = "system"
		}
		contentParts := responsesContentPartsToOpenAI(item["content"], role)
		if len(contentParts) == 0 {
			return messages, nil
		}
		messages = append(messages, map[string]any{
			"role":    role,
			"content": normalizeOpenAIContent(contentParts),
		})
		return messages, nil

	case "function_call":
		name := strings.TrimSpace(stringValue(item["name"]))
		if name == "" {
			return messages, fmt.Errorf("function_call name is required")
		}
		callID := strings.TrimSpace(stringValue(item["call_id"]))
		if callID == "" {
			callID = responseCallIDFromValue(name)
		}
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{
				map[string]any{
					"id":   callID,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": normalizeJSONString(item["arguments"]),
					},
				},
			},
		})
		return messages, nil

	case "function_call_output":
		callID := strings.TrimSpace(stringValue(item["call_id"]))
		if callID == "" {
			return messages, fmt.Errorf("function_call_output call_id is required")
		}
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      normalizeResponseOutputValue(item["output"]),
		})
		return messages, nil
	}

	return messages, nil
}

func responsesContentPartsToOpenAI(value any, role string) []any {
	rawParts := sliceValue(value)
	if len(rawParts) == 0 {
		return nil
	}

	contentParts := make([]any, 0, len(rawParts))
	for _, rawPart := range rawParts {
		part := mapValue(rawPart)
		if part == nil {
			continue
		}

		switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
		case "input_text", "output_text", "text":
			text := stringValue(part["text"])
			if strings.TrimSpace(text) == "" {
				continue
			}
			contentParts = append(contentParts, map[string]any{
				"type": "text",
				"text": text,
			})
		case "input_image":
			if role == "assistant" {
				continue
			}
			imageURL := strings.TrimSpace(stringValue(part["image_url"]))
			if imageURL == "" {
				continue
			}
			contentParts = append(contentParts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": imageURL,
				},
			})
		}
	}
	return contentParts
}

func convertResponsesToolsToOpenAI(value any) []any {
	rawTools := sliceValue(value)
	if len(rawTools) == 0 {
		return nil
	}

	tools := make([]any, 0, len(rawTools))
	for _, rawTool := range rawTools {
		tool := mapValue(rawTool)
		if tool == nil {
			continue
		}

		toolType := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" && toolType == "function" {
			name = strings.TrimSpace(stringValue(tool["name"]))
		}
		if name == "" {
			name = toolType
		}
		if name == "" {
			continue
		}

		parameters := mapValue(tool["parameters"])
		if parameters == nil {
			parameters = mapValue(tool["input_schema"])
		}
		if parameters == nil {
			parameters = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}

		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": strings.TrimSpace(stringValue(tool["description"])),
				"parameters":  parameters,
			},
		})
	}
	return tools
}

func convertResponsesToolChoiceToOpenAI(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "", "auto":
			return "auto", true
		case "required":
			return "required", true
		case "none":
			return "none", true
		default:
			return typed, true
		}
	case map[string]any:
		choiceType := strings.ToLower(strings.TrimSpace(stringValue(typed["type"])))
		switch choiceType {
		case "", "auto":
			return "auto", true
		case "required":
			return "required", true
		case "none":
			return "none", true
		default:
			name := strings.TrimSpace(stringValue(typed["name"]))
			if name == "" {
				name = choiceType
			}
			if name == "" {
				return nil, false
			}
			return map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": name,
				},
			}, true
		}
	default:
		return nil, false
	}
}

func writeResponsesSuccessResponse(w http.ResponseWriter, resp *http.Response, requestedModel string, stream bool, conversation []map[string]any, store *responseStore) error {
	if stream {
		return writeResponsesStream(w, resp, requestedModel, conversation, store)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read upstream response: %w", err)
	}

	converted, responseID, storedConversation, err := convertOpenAINonStreamResponseToResponses(body, requestedModel, conversation)
	if err != nil {
		return err
	}
	if responseID != "" {
		store.Put(responseID, storedConversation)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(converted)
	return err
}

func convertOpenAINonStreamResponseToResponses(body []byte, requestedModel string, conversation []map[string]any) ([]byte, string, []map[string]any, error) {
	var response openAIChatCompletion
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, "", nil, fmt.Errorf("decode upstream response: %w", err)
	}

	responseObject, conversationItems := buildResponsesObjectFromOpenAI(response, requestedModel)
	storedConversation := append(cloneResponseItems(conversation), conversationItems...)
	encoded, err := json.Marshal(responseObject)
	if err != nil {
		return nil, "", nil, fmt.Errorf("encode responses response: %w", err)
	}
	return encoded, strings.TrimSpace(stringValue(responseObject["id"])), storedConversation, nil
}

func buildResponsesObjectFromOpenAI(response openAIChatCompletion, requestedModel string) (map[string]any, []map[string]any) {
	modelName := strings.TrimSpace(response.Model)
	if modelName == "" {
		modelName = requestedModel
	}

	responseID := normalizeResponseID(response.ID)
	outputItems := make([]any, 0, 4)
	storedItems := make([]map[string]any, 0, 4)
	outputTexts := make([]string, 0, 2)
	finishReason := ""
	var usage map[string]any

	if len(response.Choices) > 0 {
		choice := response.Choices[0]
		finishReason = strings.TrimSpace(choice.FinishReason)

		messageParts := make([]any, 0, 4)
		for _, block := range convertOpenAIContentToClaudeBlocks(choice.Message.Content) {
			blockMap := mapValue(block)
			if blockMap == nil || !strings.EqualFold(stringValue(blockMap["type"]), "text") {
				continue
			}
			text := stringValue(blockMap["text"])
			if strings.TrimSpace(text) == "" {
				continue
			}
			messageParts = append(messageParts, map[string]any{
				"type": "output_text",
				"text": text,
			})
			outputTexts = append(outputTexts, text)
		}

		if len(messageParts) > 0 || len(choice.Message.ToolCalls) == 0 {
			messageItem := map[string]any{
				"id":      normalizeResponseMessageID(responseID),
				"type":    "message",
				"status":  "completed",
				"role":    "assistant",
				"content": messageParts,
			}
			outputItems = append(outputItems, messageItem)
			storedItems = append(storedItems, map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": cloneGenericSlice(messageParts),
			})
		}

		for _, toolCall := range choice.Message.ToolCalls {
			callID := responseCallIDFromValue(toolCall.ID)
			functionID := responseFunctionIDFromCallID(callID)
			toolItem := map[string]any{
				"id":        functionID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   callID,
				"name":      toolCall.Function.Name,
				"arguments": normalizeJSONString(toolCall.Function.Arguments),
			}
			outputItems = append(outputItems, toolItem)
			storedItems = append(storedItems, map[string]any{
				"type":      "function_call",
				"id":        functionID,
				"call_id":   callID,
				"name":      toolCall.Function.Name,
				"arguments": normalizeJSONString(toolCall.Function.Arguments),
			})
		}
	}

	if response.Usage != nil {
		usage = mapOpenAIUsageToResponses(response.Usage)
	} else {
		usage = map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		}
	}

	responseObject := map[string]any{
		"id":           responseID,
		"object":       "response",
		"status":       "completed",
		"created_at":   time.Now().Unix(),
		"model":        modelName,
		"output":       outputItems,
		"output_text":  strings.Join(outputTexts, ""),
		"usage":        usage,
		"finish_reason": finishReason,
	}

	return responseObject, storedItems
}

func mapOpenAIUsageToResponses(usage *openAIUsage) map[string]any {
	inputTokens, outputTokens, cachedTokens := extractOpenAIUsage(usage)
	totalTokens := inputTokens + outputTokens + cachedTokens
	if usage != nil && usage.TotalTokens > 0 {
		totalTokens = usage.TotalTokens
	}
	out := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  totalTokens,
	}
	if cachedTokens > 0 {
		out["input_tokens_details"] = map[string]any{
			"cached_tokens": cachedTokens,
		}
	}
	return out
}

type responseStreamState struct {
	responseID      string
	model           string
	message         *responseOutputMessageState
	toolCalls       map[int]*responseOutputToolState
	outputOrder     []responseOutputEntry
	usage           *openAIUsage
	sequence        int
}

type responseOutputEntry struct {
	kind  string
	index int
}

type responseOutputMessageState struct {
	ID          string
	OutputIndex int
	Text        strings.Builder
	Started     bool
}

type responseOutputToolState struct {
	Index       int
	ID          string
	CallID      string
	Name        string
	Arguments   strings.Builder
	OutputIndex int
	Started     bool
}

func writeResponsesStream(w http.ResponseWriter, resp *http.Response, requestedModel string, conversation []map[string]any, store *responseStore) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	state := &responseStreamState{
		responseID: normalizeResponseID(""),
		model:      requestedModel,
		toolCalls:  make(map[int]*responseOutputToolState),
		outputOrder: make([]responseOutputEntry, 0, 4),
	}

	if err := writeResponsesSSEEvent(w, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         state.responseID,
			"object":     "response",
			"status":     "in_progress",
			"created_at": time.Now().Unix(),
			"model":      state.model,
			"output":     []any{},
		},
		"sequence_number": state.nextSequence(),
	}); err != nil {
		return err
	}
	if err := writeResponsesSSEEvent(w, "response.in_progress", map[string]any{
		"type": "response.in_progress",
		"response": map[string]any{
			"id":         state.responseID,
			"object":     "response",
			"status":     "in_progress",
			"created_at": time.Now().Unix(),
			"model":      state.model,
			"output":     []any{},
		},
		"sequence_number": state.nextSequence(),
	}); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}

	reader := bufio.NewReader(resp.Body)
	for {
		payload, err := readNextSSEDataBlock(reader)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		trimmed := bytes.TrimSpace(payload)
		if len(trimmed) == 0 {
			continue
		}
		if bytes.Equal(trimmed, []byte("[DONE]")) {
			break
		}

		var chunk openAIChatCompletion
		if err := json.Unmarshal(trimmed, &chunk); err != nil {
			return fmt.Errorf("decode upstream stream chunk: %w", err)
		}

		if strings.TrimSpace(chunk.Model) != "" {
			state.model = chunk.Model
		}
		if chunk.Usage != nil {
			state.usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := state.emitTextDelta(w, choice.Delta.Content); err != nil {
					return err
				}
			}
			for _, toolCall := range choice.Delta.ToolCalls {
				if err := state.emitToolDelta(w, toolCall); err != nil {
					return err
				}
			}
		}

		if flusher != nil {
			flusher.Flush()
		}
	}

	outputItems, storedItems, outputText, err := state.finishStream(w)
	if err != nil {
		return err
	}

	responseObject := map[string]any{
		"id":          state.responseID,
		"object":      "response",
		"status":      "completed",
		"created_at":  time.Now().Unix(),
		"model":       state.model,
		"output":      outputItems,
		"output_text": outputText,
		"usage":       mapOpenAIUsageToResponses(state.usage),
	}
	if err := writeResponsesSSEEvent(w, "response.completed", map[string]any{
		"type":            "response.completed",
		"response":        responseObject,
		"sequence_number": state.nextSequence(),
	}); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}

	store.Put(state.responseID, append(cloneResponseItems(conversation), storedItems...))
	return nil
}

func (s *responseStreamState) emitTextDelta(w http.ResponseWriter, delta string) error {
	if s.message == nil {
		s.message = &responseOutputMessageState{
			ID:          normalizeResponseMessageID(s.responseID),
			OutputIndex: len(s.outputOrder),
			Started:     true,
		}
		s.outputOrder = append(s.outputOrder, responseOutputEntry{kind: "message", index: -1})
		if err := writeResponsesSSEEvent(w, "response.output_item.added", map[string]any{
			"type": "response.output_item.added",
			"item": map[string]any{
				"id":      s.message.ID,
				"type":    "message",
				"status":  "in_progress",
				"content": []any{},
				"role":    "assistant",
			},
			"output_index":    s.message.OutputIndex,
			"sequence_number": s.nextSequence(),
		}); err != nil {
			return err
		}
		if err := writeResponsesSSEEvent(w, "response.content_part.added", map[string]any{
			"type": "response.content_part.added",
			"part": map[string]any{
				"type": "output_text",
				"text": "",
			},
			"item_id":         s.message.ID,
			"output_index":    s.message.OutputIndex,
			"content_index":   0,
			"sequence_number": s.nextSequence(),
		}); err != nil {
			return err
		}
	}

	s.message.Text.WriteString(delta)
	return writeResponsesSSEEvent(w, "response.output_text.delta", map[string]any{
		"type": "response.output_text.delta",
		"delta": delta,
		"item_id":         s.message.ID,
		"output_index":    s.message.OutputIndex,
		"content_index":   0,
		"sequence_number": s.nextSequence(),
	})
}

func (s *responseStreamState) emitToolDelta(w http.ResponseWriter, toolCall openAIStreamToolCall) error {
	state, ok := s.toolCalls[toolCall.Index]
	if !ok {
		callID := responseCallIDFromValue(toolCall.ID)
		state = &responseOutputToolState{
			Index:       toolCall.Index,
			ID:          responseFunctionIDFromCallID(callID),
			CallID:      callID,
			OutputIndex: len(s.outputOrder),
			Started:     true,
		}
		s.toolCalls[toolCall.Index] = state
		s.outputOrder = append(s.outputOrder, responseOutputEntry{kind: "tool", index: toolCall.Index})
	}

	if strings.TrimSpace(toolCall.ID) != "" {
		state.CallID = responseCallIDFromValue(toolCall.ID)
		state.ID = responseFunctionIDFromCallID(state.CallID)
	}
	if strings.TrimSpace(toolCall.Function.Name) != "" {
		state.Name = toolCall.Function.Name
	}

	if state.Started {
		state.Started = false
		if err := writeResponsesSSEEvent(w, "response.output_item.added", map[string]any{
			"type": "response.output_item.added",
			"item": map[string]any{
				"id":        state.ID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   state.CallID,
				"name":      state.Name,
				"arguments": "",
			},
			"output_index":    state.OutputIndex,
			"sequence_number": s.nextSequence(),
		}); err != nil {
			return err
		}
	}

	if toolCall.Function.Arguments == "" {
		return nil
	}
	state.Arguments.WriteString(toolCall.Function.Arguments)
	return writeResponsesSSEEvent(w, "response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta",
		"delta": toolCall.Function.Arguments,
		"item_id":         state.ID,
		"output_index":    state.OutputIndex,
		"sequence_number": s.nextSequence(),
	})
}

func (s *responseStreamState) finishStream(w http.ResponseWriter) ([]any, []map[string]any, string, error) {
	outputItems := make([]any, 0, len(s.outputOrder))
	storedItems := make([]map[string]any, 0, len(s.outputOrder))
	outputTexts := make([]string, 0, 2)

	for _, entry := range s.outputOrder {
		switch entry.kind {
		case "message":
			if s.message == nil {
				continue
			}
			text := s.message.Text.String()
			item := map[string]any{
				"id":     s.message.ID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": text,
					},
				},
			}
			if strings.TrimSpace(text) == "" {
				item["content"] = []any{}
			} else {
				outputTexts = append(outputTexts, text)
			}
			if err := writeResponsesSSEEvent(w, "response.output_item.done", map[string]any{
				"type":            "response.output_item.done",
				"item":            item,
				"output_index":    s.message.OutputIndex,
				"sequence_number": s.nextSequence(),
			}); err != nil {
				return nil, nil, "", err
			}
			outputItems = append(outputItems, item)
			storedItems = append(storedItems, map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": cloneGenericSlice(item["content"].([]any)),
			})

		case "tool":
			toolState := s.toolCalls[entry.index]
			if toolState == nil {
				continue
			}
			item := map[string]any{
				"id":        toolState.ID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   toolState.CallID,
				"name":      toolState.Name,
				"arguments": toolState.Arguments.String(),
			}
			if err := writeResponsesSSEEvent(w, "response.output_item.done", map[string]any{
				"type":            "response.output_item.done",
				"item":            item,
				"output_index":    toolState.OutputIndex,
				"sequence_number": s.nextSequence(),
			}); err != nil {
				return nil, nil, "", err
			}
			outputItems = append(outputItems, item)
			storedItems = append(storedItems, map[string]any{
				"type":      "function_call",
				"id":        toolState.ID,
				"call_id":   toolState.CallID,
				"name":      toolState.Name,
				"arguments": toolState.Arguments.String(),
			})
		}
	}

	return outputItems, storedItems, strings.Join(outputTexts, ""), nil
}

func (s *responseStreamState) nextSequence() int {
	current := s.sequence
	s.sequence++
	return current
}

func writeResponsesSSEEvent(w http.ResponseWriter, eventName string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: "+eventName+"\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(encoded); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}

func readNextSSEDataBlock(reader *bufio.Reader) ([]byte, error) {
	var data bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			if data.Len() > 0 {
				return data.Bytes(), nil
			}
			return nil, err
		}

		trimmedLine := bytes.TrimRight(line, "\r\n")
		if len(trimmedLine) == 0 {
			if data.Len() > 0 {
				return data.Bytes(), nil
			}
			if err != nil {
				return nil, err
			}
			continue
		}

		if bytes.HasPrefix(trimmedLine, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmedLine, []byte("data:")))
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(payload)
		}

		if err != nil {
			if data.Len() > 0 {
				return data.Bytes(), nil
			}
			return nil, err
		}
	}
}

func normalizeResponseOutputValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(encoded)
	}
}

func normalizeJSONString(value any) string {
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
		encoded, err := json.Marshal(value)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	}
}

func normalizeResponseID(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return fmt.Sprintf("resp_%d", time.Now().UnixNano())
	case strings.HasPrefix(value, "resp_"):
		return value
	default:
		return "resp_" + value
	}
}

func normalizeResponseMessageID(responseID string) string {
	base := strings.TrimPrefix(strings.TrimSpace(responseID), "resp_")
	if base == "" {
		base = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "msg_" + base
}

func responseCallIDFromValue(value string) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return fmt.Sprintf("call_%d", time.Now().UnixNano())
	case strings.HasPrefix(value, "call_"):
		return value
	case strings.HasPrefix(value, "fc_"):
		return "call_" + strings.TrimPrefix(value, "fc_")
	default:
		return "call_" + value
	}
}

func responseFunctionIDFromCallID(callID string) string {
	callID = strings.TrimSpace(callID)
	switch {
	case callID == "":
		return fmt.Sprintf("fc_%d", time.Now().UnixNano())
	case strings.HasPrefix(callID, "fc_"):
		return callID
	case strings.HasPrefix(callID, "call_"):
		return "fc_" + strings.TrimPrefix(callID, "call_")
	default:
		return "fc_" + callID
	}
}

func cloneResponseItems(items []map[string]any) []map[string]any {
	if len(items) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, cloneMap(item))
	}
	return out
}

func cloneGenericSlice(values []any) []any {
	if len(values) == 0 {
		return nil
	}
	out := make([]any, len(values))
	for i, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			out[i] = cloneMap(typed)
		case []any:
			out[i] = cloneGenericSlice(typed)
		default:
			out[i] = typed
		}
	}
	return out
}
