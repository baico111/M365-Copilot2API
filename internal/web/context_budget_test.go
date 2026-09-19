package web

import (
	"strings"
	"testing"
)

// When the pinned set (system + trailing tool atom + anchor) exceeds the whole
// budget, slidingWindow must degrade gracefully instead of returning a hard
// error: an error here makes every subsequent agent turn fail with 400 while
// the client retries forever.
func TestSlidingWindowDegradesWhenPinnedExceedsBudget(t *testing.T) {
	huge := strings.Repeat("word ", 20000)
	msgs := []oaiMsg{
		{Role: "system", Content: huge},
		{Role: "user", Content: "first task"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: huge},
		{Role: "assistant", Content: "thinking"},
		{Role: "user", Content: "current task: keep going " + huge},
	}

	out, truncated, err := slidingWindow(msgs, 2048)
	if err != nil {
		t.Fatalf("slidingWindow returned hard error: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncation when pinned context exceeds budget")
	}
	if len(out) == 0 {
		t.Fatalf("expected a non-empty degraded window")
	}
}

// The common case: the newest user turn must survive truncation so the model
// still sees the current task.
func TestSlidingWindowKeepsNewestUserTurn(t *testing.T) {
	var msgs []oaiMsg
	msgs = append(msgs, oaiMsg{Role: "system", Content: "you are a bot"})
	for i := 0; i < 50; i++ {
		msgs = append(msgs, oaiMsg{Role: "user", Content: strings.Repeat("old ", 500)})
		msgs = append(msgs, oaiMsg{Role: "assistant", Content: strings.Repeat("reply ", 500)})
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "UNIQUE_CURRENT_TASK"})

	out, truncated, err := slidingWindow(msgs, 4096)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncation")
	}
	found := false
	for _, m := range out {
		if m.Role == "user" && strings.Contains(contentString(m.Content), "UNIQUE_CURRENT_TASK") {
			found = true
		}
	}
	if !found {
		t.Fatalf("newest user turn was dropped; window=%v", out)
	}
}

func contentString(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	return ""
}
