package web

import (
	"fmt"
	"log"
	"strings"
)

// validateToolConversation enforces the OpenAI tool protocol without making
// assumptions about what a tool does. Multiple consecutive assistant tool-call
// messages are allowed (the Responses API / Codex may emit several function_call
// items in one turn); results must eventually resolve every pending id.
//
// Tool results whose call_id has no matching assistant tool call are
// **repaired** by synthesizing a minimal assistant(tool_calls) message and
// inserting it just before the orphaned tool message. This happens when
// Responses API clients (OpenCode, Codex) carry function_call_output items
// whose originating function_call was in a previous response that is no
// longer in the message array. Without the repair the upstream backend
// (M365 Copilot ChatHub) would reject the request because it requires
// every tool message to be preceded by a matching assistant tool call.
//
// Assistant tool calls whose results are missing are tolerated: the Responses
// API allows a function_call to appear in input without a corresponding
// function_call_output when the output was delivered in a prior turn that
// the client does not replay.
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
	// Pending tool calls without results are tolerated — see comment above.
	return nil
}

// repairOrphanedToolResults scans the message list for tool messages whose
// tool_call_id has no preceding assistant tool call, and inserts a synthetic
// assistant(tool_calls) message just before each orphaned tool message. The
// synthetic message carries a single function call with the matching id so
// that strict backends (M365 Copilot ChatHub) accept the conversation.
//
// The function returns the repaired slice (which may be longer than the
// input) and a count of inserted messages for diagnostics.
func repairOrphanedToolResults(messages []oaiMsg) ([]oaiMsg, int) {
	// First pass: collect all known tool call ids from assistant messages.
	knownCallIDs := map[string]bool{}
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			if id, _ := call["id"].(string); id != "" {
				knownCallIDs[id] = true
			}
		}
	}

	// Second pass: build the repaired slice, inserting synthetic assistant
	// messages before orphaned tool messages.
	var out []oaiMsg
	inserted := 0
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != "" && !knownCallIDs[m.ToolCallID] {
			// Synthesize a minimal assistant tool call for this orphaned result.
			synthetic := oaiMsg{
				Role: "assistant",
				ToolCalls: []map[string]any{
					{
						"id":   m.ToolCallID,
						"type": "function",
						"function": map[string]any{
							"name":      "exec",
							"arguments": "{}",
						},
					},
				},
			}
			out = append(out, synthetic)
			inserted++
			log.Printf("[tool-repair] inserted synthetic assistant tool_call id=%s before orphaned tool result", m.ToolCallID)
		}
		out = append(out, m)
	}
	return out, inserted
}

// completeMissingToolResults scans for assistant tool_calls that have no
// corresponding tool message and appends synthetic tool results for each.
// This is required by strict backends (M365 Copilot ChatHub) that expect
// every tool_call to be followed by a tool result in the same conversation.
func completeMissingToolResults(messages []oaiMsg) ([]oaiMsg, int) {
	pending := map[string]bool{}
	for _, m := range messages {
		switch m.Role {
		case "assistant":
			for _, call := range m.ToolCalls {
				if id, _ := call["id"].(string); id != "" {
					pending[id] = true
				}
			}
		case "tool":
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) == 0 {
		return messages, 0
	}
	out := make([]oaiMsg, 0, len(messages)+len(pending))
	out = append(out, messages...)
	added := 0
	for id := range pending {
		out = append(out, oaiMsg{Role: "tool", ToolCallID: id, Content: "[result from previous turn]"})
		added++
		log.Printf("[tool-repair] appended synthetic tool result for unresolved call_id=%s", id)
	}
	return out, added
}

// validateAndRepairToolConversation runs validateToolConversation on the
// messages; if it fails with "unexpected tool result", it repairs the
// orphaned tool messages and re-validates. Returns the (possibly repaired)
// message slice and an error if validation still fails after repair.
func validateAndRepairToolConversation(messages []oaiMsg) ([]oaiMsg, error) {
	if err := validateToolConversation(messages); err != nil {
		// Only attempt repair for "unexpected tool result" errors; other
		// validation failures (missing id, duplicate id, etc.) are real errors.
		if !strings.Contains(err.Error(), "unexpected tool result") {
			return messages, err
		}
		repaired, inserted := repairOrphanedToolResults(messages)
		if inserted > 0 {
			log.Printf("[tool-repair] repaired %d orphaned tool messages, re-validating", inserted)
			if err2 := validateToolConversation(repaired); err2 != nil {
				log.Printf("[tool-repair] re-validation FAILED after repair: %s", err2.Error())
				for i, m := range repaired {
					toolCallIDs := []string{}
					for _, tc := range m.ToolCalls {
						id, _ := tc["id"].(string)
						toolCallIDs = append(toolCallIDs, id)
					}
					log.Printf("[tool-repair] msg[%d] role=%s tool_call_id=%q tool_calls=%v content_len=%d", i, m.Role, m.ToolCallID, toolCallIDs, len(fmt.Sprint(m.Content)))
				}
				return repaired, err2
			}
			return repaired, nil
		}
		return messages, err
	}
	// Even when validation passes, complete any missing tool results so
	// the upstream backend (ChatHub) doesn't reject the conversation.
	completed, added := completeMissingToolResults(messages)
	if added > 0 {
		log.Printf("[tool-repair] completed %d missing tool results", added)
	}
	return completed, nil
}
