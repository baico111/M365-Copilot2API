package web

import "testing"

// FuzzRouterDecisionChain asserts the router decision parser never panics and
// never accepts an undeclared tool from arbitrary model output.
func FuzzRouterDecisionChain(f *testing.F) {
	for _, s := range []string{
		`{"calls":[{"name":"Bash","arguments":{"command":"ls"}}]}`,
		"CALL_TOOL: Bash({\"command\":\"ls\"})",
		"NO_TOOL_NEEDED",
		"```json\n{\"calls\":[]}\n```",
		"{\"calls\":[{\"name\":\"evil\",\"arguments\":{}}]}",
		"",
	} {
		f.Add(s)
	}
	tools := intentTools()
	f.Fuzz(func(t *testing.T, text string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %q: %v", text, r)
			}
		}()
		calls, _ := parseModelToolDecision(text, tools, "auto")
		for _, c := range calls {
			if lookupToolName(c.Name, tools) == "" {
				t.Fatalf("decision returned undeclared tool %q from %q", c.Name, text)
			}
		}
	})
}

// FuzzValidateToolConversation asserts the client-supplied message validator
// never panics on arbitrary histories.
func FuzzValidateToolConversation(f *testing.F) {
	f.Add("user")
	f.Fuzz(func(t *testing.T, role string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on role %q: %v", role, r)
			}
		}()
		msgs := []oaiMsg{{Role: role, Content: "x"}}
		_ = validateToolConversation(msgs)
	})
}
