package main

import "strings"

func normalizeReasoningEffort(raw string) (string, bool) {
	effort := strings.ToLower(strings.TrimSpace(raw))
	switch effort {
	case "":
		return "", false
	case "none", "false", "disabled", "off":
		return "none", true
	case "minimal":
		return "low", true
	case "low", "medium", "high":
		return effort, true
	case "xhigh", "max":
		return "high", true
	case "auto", "true", "enabled", "on":
		return "", false
	default:
		return "", false
	}
}
