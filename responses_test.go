package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertResponsesCreateRequestToOpenAI(t *testing.T) {
	store := newResponseStore()
	store.Put("resp_prev", []map[string]any{
		{
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": "Previous answer"},
			},
		},
	})

	body := []byte(`{
		"model": "z-ai/glm-5.1",
		"previous_response_id": "resp_prev",
		"instructions": "Be concise",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
			{"type":"function_call_output","call_id":"call_123","output":"done"}
		],
		"tools": [{"type":"function","name":"shell_command","description":"Run shell","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}],
		"tool_choice": "auto",
		"stream": true,
		"max_output_tokens": 123
	}`)

	payload, model, stream, conversation, err := convertResponsesCreateRequestToOpenAI(body, store)
	if err != nil {
		t.Fatalf("convertResponsesCreateRequestToOpenAI returned error: %v", err)
	}

	if model != "z-ai/glm-5.1" {
		t.Fatalf("unexpected model: %s", model)
	}
	if !stream {
		t.Fatalf("expected stream=true")
	}
	if payload["max_tokens"] != 123 {
		t.Fatalf("expected max_tokens=123, got %#v", payload["max_tokens"])
	}

	messages := payload["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(messages))
	}

	systemMessage := messages[1].(map[string]any)
	if systemMessage["role"] != "system" {
		t.Fatalf("expected system message at index 1, got %#v", systemMessage["role"])
	}

	if len(conversation) != 4 {
		t.Fatalf("expected 4 conversation items, got %d", len(conversation))
	}

	tools := payload["tools"].([]any)
	tool := tools[0].(map[string]any)
	function := tool["function"].(map[string]any)
	if function["name"] != "shell_command" {
		t.Fatalf("expected tool name shell_command, got %#v", function["name"])
	}
}

func TestConvertResponsesCreateRequestToOpenAISupportsDeveloperRole(t *testing.T) {
	store := newResponseStore()
	body := []byte(`{
		"model": "z-ai/glm-5.1",
		"input": [
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Always answer with OK."}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`)

	payload, _, _, conversation, err := convertResponsesCreateRequestToOpenAI(body, store)
	if err != nil {
		t.Fatalf("convertResponsesCreateRequestToOpenAI returned error: %v", err)
	}

	if got := conversation[0]["role"]; got != "developer" {
		t.Fatalf("expected stored role developer, got %#v", got)
	}

	messages := payload["messages"].([]any)
	developerMessage := messages[0].(map[string]any)
	if got := developerMessage["role"]; got != "system" {
		t.Fatalf("expected developer role to map to system upstream, got %#v", got)
	}
}

func TestWriteResponsesStream(t *testing.T) {
	upstreamStream := strings.Join([]string{
		"data: {\"id\":\"chatcmpl_1\",\"model\":\"z-ai/glm-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello\"}}]}",
		"",
		"data: {\"id\":\"chatcmpl_1\",\"model\":\"z-ai/glm-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell_command\",\"arguments\":\"{\\\"command\\\":\\\"pwd\\\"}\"}}]}}]}",
		"",
		"data: [DONE]",
		"",
	}, "\n")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(upstreamStream)),
	}

	recorder := httptest.NewRecorder()
	store := newResponseStore()
	if err := writeResponsesStream(recorder, resp, "z-ai/glm-5.1", nil, store); err != nil {
		t.Fatalf("writeResponsesStream returned error: %v", err)
	}

	body := recorder.Body.String()
	expectedFragments := []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.output_item.done",
		"event: response.completed",
		"\"output_text\":\"Hello\"",
		"\"name\":\"shell_command\"",
	}
	for _, fragment := range expectedFragments {
		if !strings.Contains(body, fragment) {
			t.Fatalf("expected stream body to contain %q, body=%s", fragment, body)
		}
	}
}
