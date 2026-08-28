package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"m365-copilot2api/internal/chathub"
)

// responsesRequest is the OpenAI Responses API request subset supported by the gateway.
type responsesRequest struct {
	Model              string           `json:"model"`
	AccountID          string           `json:"accountId,omitempty"`
	Instructions       string           `json:"instructions,omitempty"`
	Input              any              `json:"input"`
	Tools              []map[string]any `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	User               string           `json:"user,omitempty"`
	Reasoning          *reasoningConfig `json:"reasoning,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Conversation       string           `json:"conversation,omitempty"`
	NewConversation    bool             `json:"new_conversation,omitempty"`
	Temperature        *float64         `json:"temperature,omitempty"`
	TopP               *float64         `json:"top_p,omitempty"`
	MaxOutputTokens    *int             `json:"max_output_tokens,omitempty"`
}

const customExecWorkspaceInstruction = `You are operating through the caller's local OpenCode execution bridge. Never use, request, or mention Microsoft 365/Copilot native tools. The only permitted execution tool is the caller-provided custom exec tool. The executor already starts in the caller-selected project workspace. Use relative paths only; never guess, cd to, or write under /root, /workspace, /tmp, or any other absolute project path. Inspect pwd and ls before changes. Do not create files outside the current working directory. Never claim a file was created, modified, or verified until custom exec returns a successful result. After every execution, use custom exec to verify the result.`

func (r responsesRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, AccountID: r.AccountID, Stream: r.Stream, ToolChoice: r.ToolChoice, User: r.User}
	if r.Temperature != nil {
		o.Temperature = r.Temperature
	}
	if r.TopP != nil {
		o.TopP = r.TopP
	}
	if r.MaxOutputTokens != nil {
		o.MaxCompletionTokens = r.MaxOutputTokens
	}
	if instructions := strings.TrimSpace(r.Instructions); instructions != "" {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: instructions})
	}
	if r.Reasoning != nil {
		o.Reasoning = r.Reasoning
		o.ReasoningEffort = r.Reasoning.Effort
	}
	switch v := r.Input.(type) {
	case string:
		if v == "" {
			return o, fmt.Errorf("input required")
		}
		o.Messages = append(o.Messages, oaiMsg{Role: "user", Content: v})
	case []any:
		// First pass: collect all call_ids from tool outputs so we can detect
		// which pending tool calls still have results coming.
		outputCallIDs := map[string]bool{}
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ != "function_call_output" && typ != "custom_tool_call_output" {
				continue
			}
			if id, _ := m["call_id"].(string); strings.TrimSpace(id) != "" {
				outputCallIDs[strings.TrimSpace(id)] = true
			}
		}

		// Buffer consecutive function_call/custom_tool_call items into a single
		// assistant message with multiple tool_calls, mirroring the Chat
		// Completions protocol where one assistant turn can carry several
		// parallel tool invocations.  Also defer any non-tool-call messages that
		// appear between an assistant(tool_calls) and its tool results so the
		// strict adjacency required by many backends is preserved.
		var pendingToolCalls []map[string]any
		var pendingToolCallIDs []string
		awaitingToolOutputs := map[string]bool{}
		var deferredMsgs []oaiMsg

		flushPendingToolCalls := func() {
			if len(pendingToolCalls) == 0 {
				return
			}
			o.Messages = append(o.Messages, oaiMsg{Role: "assistant", ToolCalls: pendingToolCalls})
			for _, id := range pendingToolCallIDs {
				if strings.TrimSpace(id) != "" {
					awaitingToolOutputs[id] = true
				}
			}
			pendingToolCalls = pendingToolCalls[:0]
			pendingToolCallIDs = pendingToolCallIDs[:0]
		}
		flushDeferred := func() {
			for _, msg := range deferredMsgs {
				o.Messages = append(o.Messages, msg)
			}
			deferredMsgs = deferredMsgs[:0]
		}
		hasAwaitingOutput := func() bool {
			for id := range awaitingToolOutputs {
				if outputCallIDs[id] {
					return true
				}
			}
			return false
		}

		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)

			// Non tool-call items flush pending tool calls first
			if typ != "function_call" && typ != "custom_tool_call" {
				flushPendingToolCalls()
			}

			switch typ {
			case "function_call_progress":
				// Progress is deliberately not converted into an assistant/tool
				// message. It is transport metadata from a long-running client-side
				// executor and must not trigger a model turn or tool completion.
				if _, ok := parseToolProgress(m); !ok {
					return o, fmt.Errorf("invalid function_call_progress")
				}
				continue
			case "function_call_output":
				id, _ := m["call_id"].(string)
				if strings.TrimSpace(id) == "" {
					return o, fmt.Errorf("function_call_output missing call_id")
				}
				id = strings.TrimSpace(id)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: flattenToolOutput(m["output"])})
				delete(awaitingToolOutputs, id)
				if len(awaitingToolOutputs) == 0 && len(deferredMsgs) > 0 {
					flushDeferred()
				}
			case "custom_tool_call_output":
				id, _ := m["call_id"].(string)
				if strings.TrimSpace(id) == "" {
					return o, fmt.Errorf("custom_tool_call_output missing call_id")
				}
				id = strings.TrimSpace(id)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: flattenToolOutput(m["output"])})
				delete(awaitingToolOutputs, id)
				if len(awaitingToolOutputs) == 0 && len(deferredMsgs) > 0 {
					flushDeferred()
				}
			case "function_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args := m["arguments"]
				if s, ok := args.(string); ok {
					var x any
					if json.Unmarshal([]byte(s), &x) == nil {
						args = x
					}
				}
				pendingToolCalls = append(pendingToolCalls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": mustJSON(args)}})
				pendingToolCallIDs = append(pendingToolCallIDs, id)
			case "custom_tool_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				input, _ := m["input"].(string)
				pendingToolCalls = append(pendingToolCalls, map[string]any{"id": id, "type": "custom", "function": map[string]any{"name": name, "arguments": mustJSON(map[string]any{"input": input})}})
				pendingToolCallIDs = append(pendingToolCallIDs, id)
			default:
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				// Responses input items use input_text/input_image/input_file/
				// input_audio blocks. Keep the blocks intact so flattenPromptMessages
				// can extract every attachment into the ChatHub payload.
				content := m["content"]
				if content == nil {
					content = []any{m}
				}
				msg := oaiMsg{Role: role, Content: content}
				// Defer messages that appear while we're still waiting for tool
				// outputs to preserve assistant(tool_calls) -> tool(result) adjacency.
				if hasAwaitingOutput() {
					deferredMsgs = append(deferredMsgs, msg)
				} else {
					o.Messages = append(o.Messages, msg)
				}
			}
		}
		flushPendingToolCalls()
		flushDeferred()
	default:
		return o, fmt.Errorf("input must be string or array")
	}
	hasCustomExec := false
	for _, t := range r.Tools {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		if typ == "custom" && name == "exec" {
			hasCustomExec = true
			break
		}
	}
	toolNames := map[string]bool{}
	for _, t := range r.Tools {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		if hasCustomExec && !(typ == "custom" && name == "exec") {
			continue
		}
		f := map[string]any{"name": t["name"], "description": t["description"], "parameters": t["parameters"]}
		if typ == "custom" && name == "exec" {
			// ChatHub accepts JSON function arguments while Codex exec accepts a
			// grammar-constrained raw input string. Preserve the distinction in
			// Tool.Type and bridge the input through a single string field.
			f["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
		} else if typ != "function" {
			continue
		}
		if toolNames[name] {
			continue
		}
		toolNames[name] = true
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: typ, Function: b})
	}
	if hasCustomExec {
		o.Messages = append([]oaiMsg{{Role: "system", Content: customExecWorkspaceInstruction}}, o.Messages...)
	}
	return o, nil
}

// flattenToolOutput normalizes a tool output value that may be a plain
// string, an array of content parts ({"type":"input_text","text":...}),
// or a non-string scalar into a single text payload for a Chat Completions
// tool message. Mirrors responsesToolOutputText from CLIProxyAPI.
func flattenToolOutput(v any) any {
	switch val := v.(type) {
	case string:
		return val
	case []any:
		var b strings.Builder
		for _, part := range val {
			if s, ok := part.(string); ok {
				b.WriteString(s)
				continue
			}
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		return v
	}
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}
type anthropicRequest struct {
	Model      string             `json:"model"`
	System     any                `json:"system,omitempty"`
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice any                `json:"tool_choice,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	MaxTokens  int                `json:"max_tokens,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
}

func (r anthropicRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, Stream: r.Stream}
	if r.MaxTokens > 0 {
		mt := r.MaxTokens
		o.MaxCompletionTokens = &mt
	}
	if len(r.StopSequences) > 0 {
		o.Stop = r.StopSequences
	}
	if r.System != nil {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		if s, ok := m.Content.(string); ok {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: s})
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			return o, fmt.Errorf("invalid anthropic content")
		}
		var text []any
		var calls []map[string]any
		for _, raw := range blocks {
			b, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := b["type"].(string)
			switch typ {
			case "text":
				text = append(text, b)
			case "image":
				source, _ := b["source"].(map[string]any)
				if source != nil {
					srcType, _ := source["type"].(string)
					switch srcType {
					case "base64":
						data, _ := source["data"].(string)
						media, _ := source["media_type"].(string)
						if data != "" {
							if media == "" {
								media = "application/octet-stream"
							}
							text = append(text, map[string]any{
								"type":      "input_image",
								"image_url": "data:" + media + ";base64," + data,
							})
						}
					case "url":
						url, _ := source["url"].(string)
						if url != "" {
							text = append(text, map[string]any{
								"type":      "input_image",
								"image_url": url,
							})
						}
					}
				}
			case "tool_use":
				calls = append(calls, map[string]any{"id": b["id"], "type": "function", "function": map[string]any{"name": b["name"], "arguments": mustJSON(b["input"])}})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: b["content"]})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: text, ToolCalls: calls})
		}
	}
	for _, t := range r.Tools {
		f := map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: b})
	}
	if c, ok := r.ToolChoice.(map[string]any); ok {
		switch c["type"] {
		case "auto":
			o.ToolChoice = "auto"
		case "any":
			o.ToolChoice = "required"
		case "none":
			o.ToolChoice = "none"
		case "tool":
			o.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": c["name"]}}
		}
	}
	return o, nil
}
