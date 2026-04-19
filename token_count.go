package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tiktoken-go/tokenizer"
)

func countOpenAIPayloadTokens(model string, payload map[string]any) (int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal payload for token counting: %w", err)
	}

	encoder, err := tokenizerForModel(model)
	if err != nil {
		return 0, fmt.Errorf("initialize tokenizer: %w", err)
	}

	return countOpenAIChatTokens(encoder, body)
}

func tokenizerForModel(model string) (tokenizer.Codec, error) {
	sanitized := strings.ToLower(strings.TrimSpace(model))
	switch {
	case sanitized == "":
		return tokenizer.Get(tokenizer.Cl100kBase)
	case strings.HasPrefix(sanitized, "gpt-5"):
		return tokenizer.ForModel(tokenizer.GPT5)
	case strings.HasPrefix(sanitized, "gpt-4.1"):
		return tokenizer.ForModel(tokenizer.GPT41)
	case strings.HasPrefix(sanitized, "gpt-4o"):
		return tokenizer.ForModel(tokenizer.GPT4o)
	case strings.HasPrefix(sanitized, "gpt-4"):
		return tokenizer.ForModel(tokenizer.GPT4)
	case strings.HasPrefix(sanitized, "gpt-3.5"), strings.HasPrefix(sanitized, "gpt-3"):
		return tokenizer.ForModel(tokenizer.GPT35Turbo)
	case strings.HasPrefix(sanitized, "o1"):
		return tokenizer.ForModel(tokenizer.O1)
	case strings.HasPrefix(sanitized, "o3"):
		return tokenizer.ForModel(tokenizer.O3)
	case strings.HasPrefix(sanitized, "o4"):
		return tokenizer.ForModel(tokenizer.O4Mini)
	default:
		return tokenizer.Get(tokenizer.O200kBase)
	}
}

func countOpenAIChatTokens(encoder tokenizer.Codec, payload []byte) (int64, error) {
	if encoder == nil {
		return 0, fmt.Errorf("encoder is nil")
	}
	if len(payload) == 0 {
		return 0, nil
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return 0, fmt.Errorf("decode payload: %w", err)
	}

	segments := make([]string, 0, 32)

	collectOpenAIMessagesForCount(sliceValue(root["messages"]), &segments)
	collectOpenAIToolsForCount(root["tools"], &segments)
	collectOpenAIToolChoiceForCount(root["tool_choice"], &segments)
	collectOpenAIResponseFormatForCount(root["response_format"], &segments)
	addSegment(&segments, stringValue(root["input"]))
	addSegment(&segments, stringValue(root["prompt"]))

	joined := strings.TrimSpace(strings.Join(segments, "\n"))
	if joined == "" {
		return 0, nil
	}

	count, err := encoder.Count(joined)
	if err != nil {
		return 0, fmt.Errorf("count tokens: %w", err)
	}
	return int64(count), nil
}

func collectOpenAIMessagesForCount(messages []any, segments *[]string) {
	for _, rawMessage := range messages {
		message := mapValue(rawMessage)
		if message == nil {
			continue
		}

		addSegment(segments, stringValue(message["role"]))
		addSegment(segments, stringValue(message["name"]))
		collectOpenAIContentForCount(message["content"], segments)
		collectOpenAIToolCallsForCount(sliceValue(message["tool_calls"]), segments)
	}
}

func collectOpenAIContentForCount(content any, segments *[]string) {
	switch typed := content.(type) {
	case string:
		addSegment(segments, typed)
	case []any:
		for _, rawPart := range typed {
			part := mapValue(rawPart)
			if part == nil {
				if encoded, err := json.Marshal(rawPart); err == nil {
					addSegment(segments, string(encoded))
				}
				continue
			}

			switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
			case "text", "input_text", "output_text":
				addSegment(segments, stringValue(part["text"]))
			case "image_url":
				if imageURL := mapValue(part["image_url"]); imageURL != nil {
					addSegment(segments, stringValue(imageURL["url"]))
				}
			case "tool_result":
				addSegment(segments, stringValue(part["name"]))
				collectOpenAIContentForCount(part["content"], segments)
			default:
				encoded, err := json.Marshal(part)
				if err == nil {
					addSegment(segments, string(encoded))
				}
			}
		}
	default:
		if encoded, err := json.Marshal(content); err == nil && string(encoded) != "null" {
			addSegment(segments, string(encoded))
		}
	}
}

func collectOpenAIToolCallsForCount(toolCalls []any, segments *[]string) {
	for _, rawToolCall := range toolCalls {
		toolCall := mapValue(rawToolCall)
		if toolCall == nil {
			continue
		}

		addSegment(segments, stringValue(toolCall["id"]))
		addSegment(segments, stringValue(toolCall["type"]))
		if function := mapValue(toolCall["function"]); function != nil {
			addSegment(segments, stringValue(function["name"]))
			addSegment(segments, stringValue(function["description"]))
			addSegment(segments, stringValue(function["arguments"]))
			if encoded, err := json.Marshal(function["parameters"]); err == nil && string(encoded) != "null" {
				addSegment(segments, string(encoded))
			}
		}
	}
}

func collectOpenAIToolsForCount(rawTools any, segments *[]string) {
	switch typed := rawTools.(type) {
	case []any:
		for _, rawTool := range typed {
			tool := mapValue(rawTool)
			if tool == nil {
				continue
			}
			addSegment(segments, stringValue(tool["type"]))
			addSegment(segments, stringValue(tool["name"]))
			addSegment(segments, stringValue(tool["description"]))
			if function := mapValue(tool["function"]); function != nil {
				addSegment(segments, stringValue(function["name"]))
				addSegment(segments, stringValue(function["description"]))
				if encoded, err := json.Marshal(function["parameters"]); err == nil && string(encoded) != "null" {
					addSegment(segments, string(encoded))
				}
			}
			if encoded, err := json.Marshal(tool["input_schema"]); err == nil && string(encoded) != "null" {
				addSegment(segments, string(encoded))
			}
		}
	case map[string]any:
		collectOpenAIToolsForCount([]any{typed}, segments)
	}
}

func collectOpenAIToolChoiceForCount(toolChoice any, segments *[]string) {
	switch typed := toolChoice.(type) {
	case string:
		addSegment(segments, typed)
	case map[string]any:
		if encoded, err := json.Marshal(typed); err == nil {
			addSegment(segments, string(encoded))
		}
	}
}

func collectOpenAIResponseFormatForCount(responseFormat any, segments *[]string) {
	responseFormatMap := mapValue(responseFormat)
	if responseFormatMap == nil {
		return
	}

	addSegment(segments, stringValue(responseFormatMap["type"]))
	addSegment(segments, stringValue(responseFormatMap["name"]))
	if encoded, err := json.Marshal(responseFormatMap["json_schema"]); err == nil && string(encoded) != "null" {
		addSegment(segments, string(encoded))
	}
	if encoded, err := json.Marshal(responseFormatMap["schema"]); err == nil && string(encoded) != "null" {
		addSegment(segments, string(encoded))
	}
}

func addSegment(segments *[]string, value string) {
	if segments == nil {
		return
	}
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		*segments = append(*segments, trimmed)
	}
}
