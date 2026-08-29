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
	// This is now tolerated: the Responses API allows function_call items
	// without corresponding function_call_output when the output was
	// delivered in a prior turn.
	msgs := []oaiMsg{
		{Role: "user", Content: "do stuff"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "A", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "B", "type": "function", "function": map[string]any{"name": "g", "arguments": "{}"}}}},
		// B has no tool result
		{Role: "tool", ToolCallID: "A", Content: "result A"},
	}
	if err := validateToolConversation(msgs); err != nil {
		t.Fatalf("expected tolerance for missing tool result B, got: %v", err)
	}
	// validateAndRepairToolConversation should auto-complete the missing result.
	repaired, err := validateAndRepairToolConversation(msgs)
	if err != nil {
		t.Fatalf("expected auto-complete to succeed, got: %v", err)
	}
	if len(repaired) != len(msgs)+1 {
		t.Fatalf("expected %d messages after auto-complete, got %d", len(msgs)+1, len(repaired))
	}
	last := repaired[len(repaired)-1]
	if last.Role != "tool" || last.ToolCallID != "B" {
		t.Fatalf("expected auto-completed tool result for B, got %+v", last)
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

func TestValidateAndRepairOrphanedToolResult(t *testing.T) {
	// Simulates a cross-turn history where the function_call_output is present
	// but the originating function_call (assistant tool_calls) is missing.
	msgs := []oaiMsg{
		{Role: "user", Content: "do stuff"},
		{Role: "tool", ToolCallID: "call_orphan", Content: "result"},
		{Role: "user", Content: "next"},
	}
	// validateToolConversation should reject this.
	if err := validateToolConversation(msgs); err == nil {
		t.Fatal("expected error from validateToolConversation")
	}
	// validateAndRepairToolConversation should fix it by inserting a synthetic assistant message.
	repaired, err := validateAndRepairToolConversation(msgs)
	if err != nil {
		t.Fatalf("expected repair to succeed, got: %v", err)
	}
	if len(repaired) != len(msgs)+1 {
		t.Fatalf("expected %d messages after repair, got %d", len(msgs)+1, len(repaired))
	}
	// The inserted message should be an assistant with the matching tool call.
	if repaired[1].Role != "assistant" || len(repaired[1].ToolCalls) != 1 {
		t.Fatalf("expected assistant tool_calls at index 1, got %+v", repaired[1])
	}
	if id, _ := repaired[1].ToolCalls[0]["id"].(string); id != "call_orphan" {
		t.Fatalf("expected tool call id call_orphan, got %v", repaired[1].ToolCalls[0]["id"])
	}
	// Re-validation should pass.
	if err := validateToolConversation(repaired); err != nil {
		t.Fatalf("re-validation failed: %v", err)
	}
}
