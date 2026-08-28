package web

import "testing"

func TestValidateToolConversationConsecutiveAssistant(t *testing.T) {
	// Codex sends consecutive function_call items without interleaved
	// function_call_output. After conversion to OpenAI format:
	//   [0] system
	//   [1] user
	//   [2] assistant (tool_calls: [A])
	//   [3] tool (A)
	//   [4] assistant (tool_calls: [B])
	//   [5] assistant (tool_calls: [C])   ← was rejected before fix
	//   [6] tool (B)
	//   [7] tool (C)
	//   [8] user
	msgs := []oaiMsg{
		{Role: "system", Content: "test"},
		{Role: "user", Content: "do stuff"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "A", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "A", Content: "result A"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "B", "type": "function", "function": map[string]any{"name": "g", "arguments": "{}"}}}},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "C", "type": "function", "function": map[string]any{"name": "h", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "B", Content: "result B"},
		{Role: "tool", ToolCallID: "C", Content: "result C"},
		{Role: "user", Content: "next"},
	}
	if err := validateToolConversation(msgs); err != nil {
		t.Fatalf("expected pass for consecutive assistant tool calls, got: %v", err)
	}
}

func TestValidateToolConversationMissingResult(t *testing.T) {
	// Last assistant has tool_calls but no matching tool results.
	msgs := []oaiMsg{
		{Role: "user", Content: "do stuff"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "A", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "B", "type": "function", "function": map[string]any{"name": "g", "arguments": "{}"}}}},
		// B has no tool result
		{Role: "tool", ToolCallID: "A", Content: "result A"},
	}
	if err := validateToolConversation(msgs); err == nil {
		t.Fatal("expected error for missing tool result B")
	}
}

func TestValidateToolConversationAllResolved(t *testing.T) {
	// Multiple consecutive assistant calls, all resolved eventually.
	msgs := []oaiMsg{
		{Role: "user", Content: "do stuff"},
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "A", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
			{"id": "B", "type": "function", "function": map[string]any{"name": "g", "arguments": "{}"}},
		}},
		{Role: "tool", ToolCallID: "A", Content: "result A"},
		{Role: "tool", ToolCallID: "B", Content: "result B"},
	}
	if err := validateToolConversation(msgs); err != nil {
		t.Fatalf("expected pass, got: %v", err)
	}
}
