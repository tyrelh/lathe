package pitest

import (
	"encoding/json"
	"testing"
)

// Message returns one newline-terminated assistant message_end event.
func Message(t testing.TB, text string, tokens int) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "message_end",
		"message": map[string]any{
			"role": "assistant", "stopReason": "stop",
			"usage":   map[string]any{"totalTokens": tokens, "cost": map[string]any{"total": 0.001}},
			"content": []map[string]string{{"type": "text", "text": text}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// Report returns a fenced JSON report in one assistant message_end event.
func Report(t testing.TB, report map[string]any, tokens int) string {
	t.Helper()
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return Message(t, "```json\n"+string(b)+"\n```", tokens)
}
