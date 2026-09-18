package web

import (
	"encoding/json"
	"testing"

	"m365-copilot2api/internal/chathub"
)

// FuzzNativeToolCalls asserts the native ChatHub event walker never panics and
// never returns a call whose name was not declared, for arbitrary event JSON.
func FuzzNativeToolCalls(f *testing.F) {
	seeds := []string{
		`{"type":1,"arguments":{"pluginName":"get_current_time","arguments":{}}}`,
		`{"id":"Bash","arguments":{"command":"ls"}}`,
		`{"name":"Bash","arguments":{"command":"ls"}}`,
		`[{"name":"Read","parameters":{"path":"/x"}}]`,
		`{"nested":{"deep":[{"functionName":"Bash","args":{}}]}}`,
		`null`,
		`"str"`,
		`123`,
		`{`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	tools := []chathub.Tool{
		{Type: "function", Function: json.RawMessage(`{"name":"Bash","parameters":{"type":"object"}}`)},
		{Type: "function", Function: json.RawMessage(`{"name":"Read","parameters":{"type":"object"}}`)},
	}
	f.Fuzz(func(t *testing.T, raw string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %q: %v", raw, r)
			}
		}()
		calls := nativeToolCalls([]json.RawMessage{json.RawMessage(raw)}, tools)
		for _, c := range calls {
			if c.Name != "Bash" && c.Name != "Read" {
				t.Fatalf("undeclared tool %q from %q", c.Name, raw)
			}
		}
	})
}
