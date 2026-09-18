package web

import (
	"testing"
)

// FuzzToolIntentChain asserts the natural-language tool parsing pipeline never
// panics and never returns a call whose name is not declared. The inputs are
// model output (untrusted), so robustness against arbitrary bytes is required.
func FuzzToolIntentChain(f *testing.F) {
	seeds := []string{
		"ls -la /tmp",
		"让我先看看这个文件\n\ncat /etc/hosts",
		"/Users/a/b/main.go",
		"todo",
		"```bash\nrm -rf /\n```",
		"{not json",
		"CALL_TOOL: Bash({\"command\":\"ls\"})",
		"<｜｜DSML｜｜invoke name=\"Bash\">",
		"<m365-tool-call>{\"name\":\"Bash\"}</m365-tool-call>",
		"def foo():\n    return 1",
		"",
		"\x00\xff\xfe",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	tools := intentTools()
	f.Fuzz(func(t *testing.T, text string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on %q: %v", text, r)
			}
		}()
		_ = hasToolCallIntent(text, tools)
		calls := recoverNaturalLanguageToolCalls(text, tools, "auto")
		for _, c := range calls {
			if lookupToolName(c.Name, tools) == "" {
				t.Fatalf("recovered undeclared tool %q from %q", c.Name, text)
			}
		}
		fc := fencedToolCalls(text, tools, "auto")
		for _, c := range fc {
			if lookupToolName(c.Name, tools) == "" {
				t.Fatalf("fenced undeclared tool %q from %q", c.Name, text)
			}
		}
		valid, _ := validateDetectedToolCalls(calls, tools, "auto")
		for _, c := range valid {
			if lookupToolName(c.Name, tools) == "" {
				t.Fatalf("validated undeclared tool %q", c.Name)
			}
		}
	})
}
