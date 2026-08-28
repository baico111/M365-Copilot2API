package web

import "fmt"

// validateToolConversation enforces the OpenAI tool protocol without making
// assumptions about what a tool does. Multiple consecutive assistant tool-call
// messages are allowed (the Responses API / Codex may emit several function_call
// items in one turn); results must eventually resolve every pending id.
func validateToolConversation(messages []oaiMsg) error {
	if len(messages) > 0 {
		first := messages[0].Role
		if first != "system" && first != "developer" && first != "user" && first != "assistant" {
			return fmt.Errorf("first message must have role system, developer, user, or assistant, got %q", first)
		}
	}
	pending := map[string]bool{}
	completed := map[string]bool{}
	// Track unresolved assistant indices for diagnostics.
	unresolvedAssistantIdx := []int{}
	for i, m := range messages {
		switch m.Role {
		case "assistant":
			// Record unresolved calls from previous assistant turns.
			if len(pending) > 0 {
				unresolvedAssistantIdx = append(unresolvedAssistantIdx, i)
			}
			for _, call := range m.ToolCalls {
				id, _ := call["id"].(string)
				if id == "" {
					return fmt.Errorf("assistant tool call missing id at index %d", i)
				}
				if pending[id] || completed[id] {
					return fmt.Errorf("duplicate tool call id: %s", id)
				}
				pending[id] = true
			}
		case "tool":
			if m.ToolCallID == "" {
				return fmt.Errorf("tool_call_id required at index %d", i)
			}
			if !pending[m.ToolCallID] {
				return fmt.Errorf("unexpected tool result: %s", m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			completed[m.ToolCallID] = true
		}
	}
	if len(pending) > 0 {
		for id := range pending {
			return fmt.Errorf("missing tool result for tool_call_id: %s (unresolved assistant turns at indices: %v)", id, unresolvedAssistantIdx)
		}
	}
	return nil
}
