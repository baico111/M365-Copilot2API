package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
)

// 模拟 openaiChat 返回的工具调用响应时，writeResponsesResult 输出的完整 JSON
func TestE2EResponsesToolCallID(t *testing.T) {
	// 模拟 writeToolResponse 输出的 Chat Completions JSON
	// 这是 openaiChat 通过 httptest.Recorder 返回给 runOpenAIAdapter 的内容
	chatCompletion := map[string]any{
		"id": "chatcmpl-test-123",
		"object": "chat.completion",
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_abc123",
							"type": "function",
							"function": map[string]any{
								"name":      "read_file",
								"arguments": `{"path":"/tmp/test.txt"}`,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}

	// 模拟 responses handler 中的处理
	out, _ := json.Marshal(chatCompletion)
	var parsed map[string]any
	json.Unmarshal(out, &parsed)

	// 模拟 responses handler 设置 m365_response_id
	if _, ok := parsed["id"].(string); ok {
		parsed["m365_response_id"] = "resp_test_456"
	}

	// 调用 writeResponsesResult
	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "gpt-5.6-luna", false, parsed)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v\nbody: %s", err, rr.Body.String())
	}

	// 检查顶层 id
	topID, ok := resp["id"]
	if !ok {
		t.Fatal("missing top-level id")
	}
	if _, ok := topID.(string); !ok {
		t.Fatalf("top-level id is not string, got %T: %v", topID, topID)
	}
	t.Logf("top-level id: %v (type: %T)", topID, topID)

	// 检查 output items
	output, ok := resp["output"].([]any)
	if !ok {
		t.Fatalf("output is not array: %T", resp["output"])
	}
	for i, item := range output {
		m, _ := item.(map[string]any)
		// 检查 item id
		itemID, hasID := m["id"]
		if !hasID {
			t.Errorf("output[%d] missing id field", i)
		} else if _, ok := itemID.(string); !ok {
			t.Errorf("output[%d] id is not string, got %T: %v", i, itemID, itemID)
		}
		// 检查 call_id
		callID, hasCallID := m["call_id"]
		if !hasCallID {
			t.Errorf("output[%d] missing call_id field", i)
		} else if _, ok := callID.(string); !ok {
			t.Errorf("output[%d] call_id is not string, got %T: %v", i, callID, callID)
		}
		t.Logf("output[%d] type=%s id=%v (type:%T) call_id=%v (type:%T)", i, m["type"], itemID, itemID, callID, callID)
	}

	fmt.Println("Full response:")
	fmt.Println(rr.Body.String())
}
