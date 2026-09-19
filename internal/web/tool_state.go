package web

import "fmt"

// sanitizeToolConversation repairs a tool conversation that upstream clients
// (e.g. an agent that compresses/truncates history) have left malformed.
//
// A strict validator must reject such histories, but rejecting the whole
// request is the wrong response when the only damage is an assistant turn
// whose tool_calls lost their matching tool results (or an orphan tool result
// whose call was dropped). Historically this surfaced as long agent sessions
// failing every turn with 400 tool_protocol_error once context compression
// kicked in.
//
// It returns the cleaned message list and whether anything was dropped.
// Rules, applied in order:
//   - assistant tool_calls with an empty id are dropped;
//   - a tool_call id already pending or completed is dropped (dedupe);
//   - tool results with no matching pending call are dropped;
//   - tool_calls still pending when the next assistant turn starts are dropped
//     from the earlier assistant message, and their results (if any) are
//     treated as orphans.
func sanitizeToolConversation(messages []oaiMsg) ([]oaiMsg, bool) {
	if len(messages) == 0 {
		return messages, false
	}

	changed := false
	pending := map[string]bool{}
	completed := map[string]bool{}
	out := make([]oaiMsg, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "assistant":
			if len(pending) > 0 {
				// Previous turn never completed its tool results. Flush the
				// stale pending ids so this turn can proceed; the now-orphan
				// results that follow will be dropped below.
				for id := range pending {
					delete(pending, id)
				}
				changed = true
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, call := range m.ToolCalls {
					id, _ := call["id"].(string)
					if id == "" || pending[id] || completed[id] {
						changed = true
						continue
					}
					pending[id] = true
					calls = append(calls, call)
				}
				m.ToolCalls = calls
			}
			out = append(out, m)

		case "tool":
			if m.ToolCallID == "" || !pending[m.ToolCallID] {
				changed = true
				continue
			}
			delete(pending, m.ToolCallID)
			completed[m.ToolCallID] = true
			out = append(out, m)

		default:
			out = append(out, m)
		}
	}

	return out, changed
}

// validateToolConversation enforces the OpenAI tool protocol without making
// assumptions about what a tool does. Every assistant call must be followed by
// exactly one matching tool result before another model turn is requested.
func validateToolConversation(messages []oaiMsg) error {
	if len(messages) > 0 {
		first := messages[0].Role
		if first != "system" && first != "developer" && first != "user" && first != "assistant" {
			return fmt.Errorf("first message must have role system, developer, user, or assistant, got %q", first)
		}
	}
	pending := map[string]bool{}
	completed := map[string]bool{}
	for i, m := range messages {
		switch m.Role {
		case "assistant":
			if len(pending) > 0 {
				return fmt.Errorf("tool results missing before assistant message at index %d", i)
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
			return fmt.Errorf("missing tool result for tool_call_id: %s", id)
		}
	}
	return nil
}
