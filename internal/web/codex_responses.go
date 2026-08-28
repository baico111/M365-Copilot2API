package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// writeResponsesResult projects an internal OpenAI-style result into the
// Responses events and completion shape consumed by Codex.
func writeResponsesResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	id := firstNonEmpty(fmt.Sprint(src["m365_response_id"]), "resp_"+uuid.NewString())
	msg, _ := openAIChoice(src)
	sanitizePublicAssistantMessage(msg, model)
	var output []any
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if tc["type"] == "custom" {
				input := customToolInput(fn["arguments"])
				output = append(output, map[string]any{"type": "custom_tool_call", "id": "ctc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "input": input, "status": "completed"})
				log.Printf("[responses-output] type=custom_tool_call name=%s call_id=%v input_len=%d", fn["name"], tc["id"], len(input))
				continue
			}
			args := ensureJSONString(fn["arguments"])
			output = append(output, map[string]any{"type": "function_call", "id": "fc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "arguments": args, "status": "completed"})
			log.Printf("[responses-output] type=function_call name=%s call_id=%v args_len=%d", fn["name"], tc["id"], len(args))
		}
	} else {
		text, _ := msg["content"].(string)
		messageID := "msg_" + uuid.NewString()
		output = append(output, map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
	}
	usage, _ := src["usage"].(map[string]any)
	usageSource, _ := src["m365_usage_source"].(string)
	if usage == nil {
		estimate := estimateResponsesUsage(model, nil, nil, nil, fmt.Sprint(msg["content"]))
		usage = estimate.Values
		usageSource = estimate.Source
	}
	if usageSource == "" {
		usageSource = usageSourceHeuristic
	}
	resp := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage, "m365": localUsageMetadata(usageSource)}
	if !stream {
		jsonOut(w, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	aborted := false
	emit := func(name string, v any) {
		if aborted {
			return
		}
		if err := sseWriteFrame(w, f, name, v); err != nil {
			aborted = true
		}
	}
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})
	for i, item := range output {
		m, _ := item.(map[string]any)
		addedItem := item
		if m["type"] == "function_call" {
			// Arguments arrive in function_call_arguments.delta. Including them
			// here too would make conforming clients append duplicate JSON.
			added := make(map[string]any, len(m))
			for k, v := range m {
				added[k] = v
			}
			added["arguments"] = ""
			added["status"] = "in_progress"
			addedItem = added
		}
		emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": addedItem})
		if m["type"] == "message" {
			content, _ := m["content"].([]any)
			if len(content) > 0 {
				c, _ := content[0].(map[string]any)
				emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": i, "content_index": 0, "delta": c["text"]})
			}
		} else if m["type"] == "function_call" {
			args, _ := m["arguments"].(string)
			emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": m["id"], "delta": args})
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": m["id"], "arguments": args})
		} else if m["type"] == "custom_tool_call" {
			input, _ := m["input"].(string)
			emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": m["id"], "delta": input})
			emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i, "item_id": m["id"], "input": input})
		}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func customToolInput(arguments any) string {
	if s, ok := arguments.(string); ok {
		var v struct {
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			return v.Input
		}
	}
	return ""
}

// ensureJSONString guarantees that arguments is a valid JSON string.
// If the value is already a string, it is returned as-is if it parses as
// JSON; otherwise it is wrapped in braces. Non-string values are marshalled.
func ensureJSONString(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return "{}"
		}
		var probe any
		if json.Unmarshal([]byte(s), &probe) == nil {
			return s
		}
		b, _ := json.Marshal(s)
		return string(b)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
