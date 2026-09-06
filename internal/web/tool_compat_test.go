package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func toolSpec(name string, props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "parameters": map[string]any{"type": "object", "properties": props, "required": required},
	}}
}

func TestLookupToolNameFuzzy(t *testing.T) {
	tools := []map[string]any{
		toolSpec("Bash", map[string]any{"command": map[string]any{"type": "string"}}, "command"),
		toolSpec("Read", map[string]any{"file_path": map[string]any{"type": "string"}}, "file_path"),
		toolSpec("Grep", map[string]any{"pattern": map[string]any{"type": "string"}}, "pattern"),
	}
	cases := map[string]string{"bash": "Bash", "BASH": "Bash", "run_command": "Bash", "execute": "Bash",
		"read_file": "Read", "cat": "Read", "view": "Read", "grep": "Grep", "search_files": "Grep",
		"totally_unrelated_thing": ""}
	for in, want := range cases {
		if got := lookupToolName(in, tools); got != want {
			t.Errorf("lookupToolName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRepairJSONNewlinesRoundTrip(t *testing.T) {
	raw := "{\"command\":\"make -j8\ncd build\n\"}"
	m, ok := lenientParseObject(raw)
	if !ok {
		t.Fatal("lenient parse of raw-newline JSON must succeed")
	}
	cmd, _ := m["command"].(string)
	if !strings.Contains(cmd, "make -j8\ncd build") || !strings.Contains(cmd, "\n") {
		t.Fatalf("command lost real newlines: %q", cmd)
	}
}

func TestLenientParseOuterGarbage(t *testing.T) {
	m, ok := lenientParseObject("here you go:\n{\"path\":\"a.txt\"} done")
	if !ok || m["path"] != "a.txt" {
		t.Fatalf("salvage failed: %v %v", m, ok)
	}
}

func TestNormalizeToolArgsAlias(t *testing.T) {
	tools := []map[string]any{toolSpec("Read", map[string]any{"file_path": map[string]any{"type": "string"}}, "file_path")}
	fn := toolFunction("Read", tools)
	args := map[string]any{"filePath": "x.go"}
	normalizeToolArgs(args, schemaProperties(fn), schemaRequired(fn))
	if v, _ := args["file_path"].(string); v != "x.go" {
		t.Fatalf("alias rename failed: %v", args)
	}
}

func TestValidateDetectedToolCallsEndToEndRepair(t *testing.T) {
	tools := []map[string]any{toolSpec("Bash", map[string]any{
		"command": map[string]any{"type": "string"},
		"timeout": map[string]any{"type": "integer"},
	}, "command")}
	call := detectedToolCall{Name: "bash", Arguments: json.RawMessage("{\"command\":\"ls\\n-la\",\"timeout\":\"5000\"}")}
	valid, rejected := validateDetectedToolCalls([]detectedToolCall{call}, tools, "auto")
	if len(rejected) != 0 || len(valid) != 1 {
		t.Fatalf("expected 1 valid, got valid=%v rejected=%v", valid, rejected)
	}
	if valid[0].Name != "Bash" {
		t.Fatalf("name not fuzzed: %q", valid[0].Name)
	}
	var args map[string]any
	_ = json.Unmarshal(valid[0].Arguments, &args)
	if args["timeout"] != float64(5000) {
		t.Fatalf("timeout should coerce to number 5000, got %#v", args["timeout"])
	}
	cmd, _ := args["command"].(string)
	if !strings.Contains(cmd, "\n") {
		t.Fatalf("newlines lost: %q", cmd)
	}
}

func TestValidateUnknownNameCarriesAvailableTools(t *testing.T) {
	tools := []map[string]any{toolSpec("Bash", map[string]any{"command": map[string]any{"type": "string"}}, "command")}
	_, rejected := validateDetectedToolCalls([]detectedToolCall{{Name: "mysterious_tool", Arguments: json.RawMessage(`{}`)}}, tools, "auto")
	if len(rejected) != 1 || !strings.Contains(rejected[0].Reason, "Available tools: Bash") {
		t.Fatalf("expected rejection with available list: %+v", rejected)
	}
}

func TestSanitizeToolCallText(t *testing.T) {
	tools := []map[string]any{toolSpec("Bash", map[string]any{"command": map[string]any{"type": "string"}}, "command")}
	text := "Let me run the build now.\n```Bash\n{\"command\":\"make\"}\n```\nAll good."
	got := sanitizeToolCallText(text, tools)
	if strings.Contains(got, "```Bash") || strings.Contains(got, "\"command\"") {
		t.Fatalf("tool block leaked into prose: %q", got)
	}
	if !strings.Contains(got, "All good.") {
		t.Fatalf("prose lost: %q", got)
	}
	// genuine code fences of other languages stay untouched
	text2 := "```bash\nsystemctl restart x\n```"
	if !strings.Contains(sanitizeToolCallText(text2, tools), "systemctl restart x") {
		t.Fatalf("non-call bash example stripped but should stay (not a call: no json body)")
	}
}

func TestFillRequiredDefaultsEnum(t *testing.T) {
	prop := map[string]any{"output_format": map[string]any{"type": "string", "enum": []any{"image", "code"}}}
	args := map[string]any{}
	fillRequiredDefaults(args, prop, []string{"output_format"})
	if args["output_format"] != "image" {
		t.Fatalf("enum first value expected: %v", args)
	}
}

func TestBuildToolRepairRuleCarriesDetails(t *testing.T) {
	tools := []map[string]any{
		toolSpec("Bash", map[string]any{"command": map[string]any{"type": "string"}}, "command"),
		toolSpec("Read", map[string]any{"file_path": map[string]any{"type": "string"}}, "file_path"),
	}
	rej := []rejectedToolCall{
		{Name: "bash", Reason: "missing required parameter 'file_path'"},
		{Name: "mysterious", Reason: "no such tool declared. Available tools: Bash, Read"},
	}
	rule := buildToolRepairRule(rej, tools)
	for _, want := range []string{"bash: missing required", "mysterious", "DECLARED TOOL NAMES", "Bash, Read", "Never return unknown_tool"} {
		if !strings.Contains(rule, want) {
			t.Fatalf("repair rule missing %q:\n%s", want, rule)
		}
	}
	// the raw `\n` sequences must be REAL newlines for the upstream prompt
	if !strings.Contains(rule, "\nREPAIR RULE:") {
		t.Fatal("repair rule must start with a real newline")
	}
}
