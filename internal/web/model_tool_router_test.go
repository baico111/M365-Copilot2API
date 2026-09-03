package web

import (
	"strings"
	"testing"
)

func TestParseModelToolDecisionAutoAndParallel(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"Beijing"}},{"name":"get_time","arguments":{"city":"Beijing"}}]}`, testTools(), "auto")
	if !ok || len(calls) != 2 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestParseModelToolDecisionNoCall(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[]}`, testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestModelToolRouterPromptMarksCompletedResults(t *testing.T) {
	p := modelToolRouterPrompt(`assistant tool_calls: [...]
tool[call_x]: 2026-07-18`, testTools(), "auto")
	if !strings.Contains(p, "Completed evidence must not be repeated") || !strings.Contains(p, "tool[call_x]: 2026-07-18") || !strings.Contains(p, "unfinished work remains") {
		t.Fatalf("missing multi-turn evidence constraint: %s", p)
	}
}

func TestParseModelToolDecisionRejectsBadSchema(t *testing.T) {
	// When EVERY emitted call fails schema validation there is no usable
	// decision: parsed must be false so the caller runs its repair round
	// instead of silently dropping the tool intent (or 502-ing on
	// tool_choice=required without a schema-informed retry).
	calls, ok := parseModelToolDecision("```json\n{\"calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":2}}]}\n```", testTools(), "auto")
	if ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v (want zero calls, unparsed so repair fires)", calls, ok)
	}
	// Explicit empty-call envelope is a legitimate "no tool needed" decision.
	calls, ok = parseModelToolDecision("```json\n{\"calls\":[]}\n```", testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("empty calls should parse as no-tool-needed: calls=%v ok=%v", calls, ok)
	}
	// Partial validity keeps the good calls.
	calls, ok = parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":2}},{"name":"get_time","arguments":{}}]}`, testTools(), "auto")
	if !ok || len(calls) != 1 {
		t.Fatalf("calls=%v ok=%v (want 1 surviving call)", calls, ok)
	}
}
