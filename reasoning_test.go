package main

import "testing"

func TestNormalizeReasoningEffort(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "none", input: "none", want: "none", ok: true},
		{name: "minimal downgraded", input: "minimal", want: "low", ok: true},
		{name: "xhigh downgraded", input: "xhigh", want: "high", ok: true},
		{name: "max downgraded", input: "max", want: "high", ok: true},
		{name: "auto omitted", input: "auto", want: "", ok: false},
		{name: "enabled omitted", input: "enabled", want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeReasoningEffort(tt.input)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("normalizeReasoningEffort(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestConvertClaudeMessagesRequestToOpenAINormalizesReasoningEffort(t *testing.T) {
	body := []byte(`{
		"model": "z-ai/glm-5.1",
		"thinking": {"type":"adaptive"},
		"output_config": {"effort":"max"},
		"messages": [{"role":"user","content":"hello"}]
	}`)

	payload, _, _, err := convertClaudeMessagesRequestToOpenAI(body)
	if err != nil {
		t.Fatalf("convertClaudeMessagesRequestToOpenAI returned error: %v", err)
	}

	if got := payload["reasoning_effort"]; got != "high" {
		t.Fatalf("expected reasoning_effort=high, got %#v", got)
	}
}

func TestConvertResponsesCreateRequestToOpenAINormalizesReasoningEffort(t *testing.T) {
	store := newResponseStore()
	body := []byte(`{
		"model": "z-ai/glm-5.1",
		"reasoning": {"effort":"xhigh"},
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]
	}`)

	payload, _, _, _, err := convertResponsesCreateRequestToOpenAI(body, store)
	if err != nil {
		t.Fatalf("convertResponsesCreateRequestToOpenAI returned error: %v", err)
	}

	if got := payload["reasoning_effort"]; got != "high" {
		t.Fatalf("expected reasoning_effort=high, got %#v", got)
	}
}
