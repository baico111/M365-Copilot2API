package web

import (
	"strings"
	"testing"
)

func intentTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{"name": "Bash", "description": "run", "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []any{"command"}}}},
		{"type": "function", "function": map[string]any{"name": "Read", "description": "read", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}}}},
	}
}

func TestHasToolCallIntentBareShell(t *testing.T) {
	if !hasToolCallIntent("ls -la /tmp", intentTools()) {
		t.Fatal("bare shell command must be intent")
	}
	if !hasToolCallIntent("grep -rn foo .", intentTools()) {
		t.Fatal("bare grep must be intent")
	}
}

func TestHasToolCallIntentActionPromise(t *testing.T) {
	if !hasToolCallIntent("好的，我开始执行。", intentTools()) {
		t.Fatal("Chinese action promise must be intent")
	}
	if !hasToolCallIntent("Let me check line 4.", intentTools()) {
		t.Fatal("English action promise must be intent")
	}
	if hasToolCallIntent("我已经完成了全部修改。", intentTools()) {
		t.Fatal("completed statement must NOT be intent")
	}
}

func TestHasToolCallIntentLonePath(t *testing.T) {
	if !hasToolCallIntent("/Users/a/b/main.go", intentTools()) {
		t.Fatal("lone path line must be intent")
	}
	if hasToolCallIntent("已修改 /Users/a/b/main.go 中的逻辑，请检查。", intentTools()) {
		t.Fatal("prose containing a path must NOT be intent")
	}
}

func TestRecoverBareShell(t *testing.T) {
	calls := recoverNaturalLanguageToolCalls("ls -la /tmp", intentTools(), "auto")
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("want 1 Bash call, got %#v", calls)
	}
}

func TestRecoverBareFilePath(t *testing.T) {
	calls := recoverNaturalLanguageToolCalls("/Users/a/b/main.go", intentTools(), "auto")
	if len(calls) != 1 || calls[0].Name != "Read" {
		t.Fatalf("want 1 Read call, got %#v", calls)
	}
}

func TestRecoverSourceCodeNotShell(t *testing.T) {
	calls := recoverNaturalLanguageToolCalls("def foo():\n    return 1", intentTools(), "auto")
	if len(calls) != 0 {
		t.Fatalf("source code must not become a bash call: %#v", calls)
	}
}

func TestChinesePlanningDetected(t *testing.T) {
	if !hasChinesePlanning("让我先看看这个文件") {
		t.Fatal("plan must be detected")
	}
	if hasChinesePlanning("我已经查看了这个文件，结论如下") {
		t.Fatal("completed statement must not be plan")
	}
}

func TestRecoverStripsPlanningPrefix(t *testing.T) {
	calls := recoverNaturalLanguageToolCalls("让我先看看这个目录\n\nls -la /tmp", intentTools(), "auto")
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("want 1 Bash call after stripping plan prefix, got %#v", calls)
	}
}

func TestRecoverStandaloneToolName(t *testing.T) {
	tools := append(intentTools(), map[string]any{"type": "function", "function": map[string]any{"name": "todo", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}})
	calls := recoverNaturalLanguageToolCalls("todo", tools, "auto")
	if len(calls) != 1 || calls[0].Name != "todo" {
		t.Fatalf("want 1 zero-arg todo call, got %#v", calls)
	}
	if string(calls[0].Arguments) != "{}" {
		t.Fatalf("zero-arg call must have empty args, got %s", calls[0].Arguments)
	}
}

func TestStandaloneNameNotProse(t *testing.T) {
	if extractStandaloneToolName("Bash", intentTools()) != nil {
		t.Fatal("bash-family name must not become a zero-arg call")
	}
	if extractStandaloneToolName("please read the file", intentTools()) != nil {
		t.Fatal("prose must not become a zero-arg call")
	}
}

func TestStripPlaceholderEchoes(t *testing.T) {
	in := "Here is the result:\n(assistant called tools)\nAll done."
	out := stripPlaceholderEchoes(in)
	if strings.Contains(out, "assistant called tool") {
		t.Fatalf("placeholder echo not stripped: %q", out)
	}
	if !strings.Contains(out, "All done.") {
		t.Fatalf("content lost: %q", out)
	}
}

func TestFencedFallbackRecoversRouterCall(t *testing.T) {
	// A router model that ignores the JSON envelope and emits a fenced call
	// (fence language = tool name, body = arguments) must still be recovered by
	// fencedToolCalls, which the router now falls back to.
	text := "I'll run that.\n```Bash\n{\"command\":\"ls -la\"}\n```"
	calls := fencedToolCalls(text, intentTools(), "auto")
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("fenced router call not recovered: %#v", calls)
	}
	if _, ok := parseModelToolDecision(text, intentTools(), "auto"); ok {
		t.Fatal("expected JSON decision parser to miss the fenced form")
	}
}

func TestBareJSONCommandRecovered(t *testing.T) {
	text := "Running it now.\n{\"command\":\"pwd\"}"
	calls := fencedToolCalls(text, intentTools(), "auto")
	if len(calls) != 1 || calls[0].Name != "Bash" {
		t.Fatalf("bare JSON command not recovered: %#v", calls)
	}
}
