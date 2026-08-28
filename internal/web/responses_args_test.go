package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestResponsesFunctionCallArgumentsFormat(t *testing.T) {
	// Simulate what openaiChat returns: tool_calls with arguments as JSON string
	src := map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_abc",
							"type": "function",
							"function": map[string]any{
								"name":      "new_tab",
								"arguments": `{"url":"https://github.com/HEXUXIU/M365-Copilot2API"}`,
							},
						},
					},
				},
			},
		},
	}

	rr := httptest.NewRecorder()
	writeResponsesResult(rr, "test-model", false, src)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v\nbody: %s", err, rr.Body.String())
	}

	output := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected 1 output item, got %d", len(output))
	}

	call := output[0].(map[string]any)
	if call["type"] != "function_call" {
		t.Fatalf("expected function_call, got %s", call["type"])
	}

	args, ok := call["arguments"].(string)
	if !ok {
		t.Fatalf("arguments should be string, got %T: %v", call["arguments"], call["arguments"])
	}

	// Verify arguments is valid JSON
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments is not valid JSON: %v, args=%q", err, args)
	}

	t.Logf("arguments type: %T", call["arguments"])
	t.Logf("arguments value: %q", args)
	t.Logf("full output: %s", rr.Body.String())

	fmt.Println("Test passed! Arguments format is correct.")
}
