package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// Adversarial cases: the validator must never panic and must degrade sanely.
func TestValidateDetectedToolCallsAdversarial(t *testing.T) {
	tools := []map[string]any{
		toolSpec("Bash", map[string]any{
			"command": map[string]any{"type": "string"},
			"timeout": map[string]any{"type": "integer"},
		}, "command"),
	}
	cases := []struct {
		name    string
		call    detectedToolCall
		wantRej bool
	}{
		{"empty name", detectedToolCall{Name: "", Arguments: json.RawMessage(`{}`)}, true},
		{"nil args", detectedToolCall{Name: "Bash", Arguments: nil}, true}, // becomes {} → missing required command
		{"null args", detectedToolCall{Name: "Bash", Arguments: json.RawMessage(`null`)}, true},
		{"array args", detectedToolCall{Name: "Bash", Arguments: json.RawMessage(`[1,2]`)}, true},
		{"string args", detectedToolCall{Name: "Bash", Arguments: json.RawMessage(`"hello"`)}, true},
		{"truncated json", detectedToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"echo`)}, true},
		{"wrong type object for command", detectedToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":{"nested":true}}`)}, true},
		// empty string is a PRESENT value (cnb2api hasField semantics): the
		// validator only rejects MISSING required fields, empty ones are the
		// client's problem to execute.
		{"alias maps but empty value", detectedToolCall{Name: "bash", Arguments: json.RawMessage(`{"cmd":""}`)}, false},
		{"negative timeout ok", detectedToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"x","timeout":-1}`)}, false},
		{"string timeout fractional rejected", detectedToolCall{Name: "Bash", Arguments: json.RawMessage(`{"command":"x","timeout":"1.5"}`)}, true},
		{"empty fence info name", detectedToolCall{Name: "", Arguments: json.RawMessage(`{}`)}, true},
		{"json null string args", detectedToolCall{Name: "bash", Arguments: json.RawMessage(`null`)}, true},
	}
	for _, tc := range cases {
		valid, rejected := validateDetectedToolCalls([]detectedToolCall{tc.call}, tools, "auto")
		gotRej := len(rejected) > 0
		if gotRej != tc.wantRej {
			t.Errorf("%q: rejected=%v want %v (rej=%+v valid=%+v)", tc.name, gotRej, tc.wantRej, rejected, valid)
		}
	}
}

// The full degraded prose pipeline: no panic, no JSON leakage, prose preserved.
func TestDegradePipelineNoLeak(t *testing.T) {
	tools := []map[string]any{toolSpec("Bash", map[string]any{"command": map[string]any{"type": "string"}}, "command")}
	messy := "I'll fix that.\n```Bash\n{\"command\":\"mak"
	prose := sanitizeToolCallText(messy, tools)
	if strings.Contains(prose, `"command"`) && strings.Contains(prose, "```") {
		t.Logf("truncated fence kept as-is (unparseable): %q", prose) // acceptable: unparseable body stays prose
	}
	complete := "Working on it.\n```Bash\n{\"command\":\"make\"}\n```\nDone."
	out := sanitizeToolCallText(complete, tools)
	if strings.Contains(out, `"command"`) {
		t.Fatalf("complete call leaked: %q", out)
	}
	if !strings.Contains(out, "Working on it.") || !strings.Contains(out, "Done.") {
		t.Fatalf("prose lost: %q", out)
	}
}

// The exact scenario reported by the user: lowercase name + multiline command
// + string timeout, exercised through the stream-repair degradation path.
func TestUserScenarioRepairThenDegrade(t *testing.T) {
	// The real Claude Code Bash tool declares optional `timeout`; the fixture
	// must include it or the string timeout cannot be coerced (no schema).
	tools := []map[string]any{toolSpec("Bash", map[string]any{
		"command": map[string]any{"type": "string"},
		"timeout": map[string]any{"type": "integer"},
	}, "command")}
	// model emitted a lowercase-name fenced call with raw newlines and string timeout
	raw := detectedToolCall{Name: "bash", Arguments: json.RawMessage("{\"command\":\"cat > toor.sh <<'EOF'\n#!/bin/bash\njava -jar server.jar\nEOF\",\"timeout\":\"30\"}")}
	valid, rejected := validateDetectedToolCalls([]detectedToolCall{raw}, tools, "auto")
	if len(rejected) > 0 {
		t.Fatalf("user scenario should now validate: %+v", rejected)
	}
	if valid[0].Name != "Bash" {
		t.Fatalf("expected fuzzy map to Bash, got %q", valid[0].Name)
	}
	var args map[string]any
	_ = json.Unmarshal(valid[0].Arguments, &args)
	cmd, _ := args["command"].(string)
	if !strings.Contains(cmd, "#!/bin/bash") {
		t.Fatalf("multiline command mangled: %q", cmd)
	}
	if args["timeout"] != float64(30) {
		t.Fatalf("timeout coercion failed: %#v", args["timeout"])
	}
	// if repair had failed, the degrade path must produce clean prose
	prose := sanitizeToolCallText("```bash\n{\"command\":\"cat toor.sh\"}\n```", tools)
	if strings.Contains(prose, "{") {
		t.Fatalf("degraded prose leaks call JSON: %q", prose)
	}
}
