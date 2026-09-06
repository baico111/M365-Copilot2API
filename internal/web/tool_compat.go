package web

import (
	"encoding/json"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file ports the tool-adapter methodology from the sibling project
// cnb2api (a DeepSeek-V4 reverse proxy). The serialization formats differ per
// model family, so we adopt the RESILIENCE LOGIC — fuzzy name resolution,
// argument alias normalization, lenient JSON repair, default back-fill and
// graceful degradation — while keeping our own GPT/M365 fenced-block format.
//
// Without these layers a perfectly reasonable model output is rejected as
// "undeclared tool"/"not a JSON object", the repair round (blind, without the
// precise error list) rarely recovers, and the turn aborts — which agents
// perceive as the assistant "stopping dead".

// ---------- fuzzy tool-name resolution ----------

// toolCategoryAliases maps a normalized category to the model-habit spellings
// that belong to it. Keys/labels mirror cnb2api's toolCategoryMap, adapted to
// the tool names Claude Code / OpenCode / Cursor actually declare.
var toolCategoryAliases = map[string][]string{
	"bash": {"bash", "shell", "execute", "exec", "run", "run_bash", "runbash", "run_command", "runcommand",
		"run_terminal_cmd", "runterminalcmd", "execute_command", "executecommand", "execute_bash", "executebash",
		"run_command_in_terminal", "terminal", "command", "cmd", "powershell", "shell_command", "launchprocess"},
	"read": {"read", "read_file", "readfile", "cat", "view", "view_file", "list_dir", "listdir", "list", "show",
		"open_file", "openfile", "get_file_content", "readfilecontent", "fileread"},
	"write": {"write", "write_file", "writefile", "create", "create_file", "createfile", "save", "save_file",
		"new_file", "write_to_file", "str_replace_editor"},
	"edit": {"edit", "edit_file", "editfile", "string_replace", "stringreplace", "multiedit", "multi_edit",
		"str_replace", "strreplace", "apply_patch", "applypatch", "patch", "modify", "replace", "replace_in_file",
		"edit_notebook", "notebookedit"},
	"search": {"grep", "search", "glob", "glob_file_search", "globfilesearch", "codebase_search", "codesearch",
		"find", "ripgrep", "search_files", "searchfiles", "searchcode", "search_in_files", "searchcontext"},
	"web":  {"webfetch", "web_fetch", "fetch", "browse", "openurl", "websearch", "web_search", "http"},
	"plan": {"todo", "todowrite", "todo_write", "todoedit", "task", "subtask", "plan"},
}

func normalizeToolToken(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// declaredToolNames returns sorted client tool names (deterministic).
func declaredToolNames(tools []map[string]any) []string {
	var out []string
	for _, t := range tools {
		if f, ok := t["function"].(map[string]any); ok {
			if n, ok := f["name"].(string); ok && n != "" {
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// lookupToolName resolves a model-written tool name to the client's declared
// name. Order: exact → case/underscore-insensitive → declared alias category
// match. Returns "" when no reasonable mapping exists (caller must reject).
func lookupToolName(name string, tools []map[string]any) string {
	if name == "" {
		return ""
	}
	declared := declaredToolNames(tools)
	for _, t := range declared {
		if t == name {
			return t
		}
	}
	norm := normalizeToolToken(name)
	if norm == "" {
		return ""
	}
	for _, t := range declared {
		if normalizeToolToken(t) == norm {
			return t
		}
	}
	// Category aliases last: bash↔Bash, read_file↔Read, grep↔Glob…
	for _, cat := range []string{"bash", "read", "write", "edit", "search", "web", "plan"} {
		matched := false
		for _, a := range toolCategoryAliases[cat] {
			if normalizeToolToken(a) == norm {
				matched = true
				break
			}
		}
		if !matched {
			continue // word not in this category — try the next one
		}
		for _, t := range declared { // lowest declared name wins → deterministic
			tn := normalizeToolToken(t)
			for _, a2 := range toolCategoryAliases[cat] {
				if normalizeToolToken(a2) == tn {
					return t
				}
			}
		}
		// word belongs to this category but no declared tool in it — keep
		// scanning other categories instead of bailing out
	}
	return ""
}

// ---------- lenient JSON argument repair ----------

// repairJSONNewlines escapes raw newlines/tabs/CR that appear INSIDE JSON
// string values. Models hand-writing a tool call whose command/content spans
// multiple lines almost always emit real line breaks; strict json.Unmarshal
// then fails and the call is rejected. The state machine only touches bytes
// inside a properly opened string; escaped sequences pass through.
func repairJSONNewlines(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 16)
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
				b.WriteByte(c)
			case c == '\\':
				escaped = true
				b.WriteByte(c)
			case c == '"':
				inString = false
				b.WriteByte(c)
			case c == '\n':
				b.WriteString(`\n`)
			case c == '\r':
				b.WriteString(`\r`)
			case c == '\t':
				b.WriteString(`\t`)
			default:
				b.WriteByte(c)
			}
			continue
		}
		if c == '"' {
			inString = true
		}
		b.WriteByte(c)
	}
	return b.String()
}

func tryUnmarshalMap(s string, m *map[string]any) bool {
	return json.Unmarshal([]byte(s), m) == nil
}

// lenientParseObject parses a tool-call argument block with escalating
// salvage: strict → newline repair → outer-brace extraction (each way).
func lenientParseObject(raw string) (map[string]any, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return map[string]any{}, true
	}
	var m map[string]any
	if tryUnmarshalMap(s, &m) && m != nil {
		return m, true
	}
	if strings.TrimSpace(s) == "null" {
		return map[string]any{}, true // "arguments": null == no arguments
	}
	fixed := repairJSONNewlines(s)
	if tryUnmarshalMap(fixed, &m) && m != nil {
		return m, true
	}
	if start := strings.IndexByte(s, '{'); start >= 0 {
		body := s[start:]
		if end := strings.LastIndexByte(body, '}'); end > 0 {
			cand := body[:end+1]
			var m2 map[string]any
			if tryUnmarshalMap(cand, &m2) && m2 != nil {
				return m2, true
			}
			m2 = nil
			if tryUnmarshalMap(repairJSONNewlines(cand), &m2) && m2 != nil {
				return m2, true
			}
		}
	}
	return nil, false
}

// ---------- argument aliasing + default back-fill ----------

// argCanonicalAliases: for each CANONICAL declared parameter, the model-habit
// spellings. Ports cnb2api argAliases. Renaming only ever happens for a
// REQUIRED field that is otherwise missing, so exact names always win first.
var argCanonicalAliases = map[string][]string{
	"path":       {"file_path", "filePath", "filepath", "filename", "file", "fileName", "path"},
	"command":    {"cmd", "script", "code", "input", "shell_command", "command"},
	"pattern":    {"query", "regex", "glob", "search", "search_string", "searchTerm", "pattern"},
	"content":    {"text", "data", "body", "file_content", "new_content", "content"},
	"old_string": {"oldString", "oldStr", "old_str", "old", "find", "search"},
	"new_string": {"newString", "newStr", "new_str", "new", "replace", "replacement", "replace_in_range"},
	"url":        {"uri", "link", "href", "url"},
	"prompt":     {"text", "query", "prompt"},
}

func schemaProperties(fn map[string]any) map[string]any {
	params, _ := fn["parameters"].(map[string]any)
	if params == nil {
		return nil
	}
	props, _ := params["properties"].(map[string]any)
	return props
}

func schemaRequired(fn map[string]any) []string {
	params, _ := fn["parameters"].(map[string]any)
	if params == nil {
		return nil
	}
	return extractRequiredList(params["required"])
}

// normalizeToolArgs renames alias keys onto missing canonical required
// parameters (deterministic: canonical list sorted, candidates by key order).
func normalizeToolArgs(args map[string]any, props map[string]any, required []string) {
	var reqs []string
	for _, r := range required {
		if _, has := args[r]; !has {
			if props != nil {
				if _, declared := props[r]; !declared {
					continue // only trust the tool's own schema
				}
			}
			reqs = append(reqs, r)
		}
	}
	if len(reqs) == 0 {
		return
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	used := map[string]bool{}
	for _, r := range reqs {
		cands := argCanonicalAliases[r]
		if len(cands) == 0 {
			cands = []string{r}
		}
		candSet := map[string]bool{normalizeToolToken(r): true}
		for _, a := range cands {
			candSet[normalizeToolToken(a)] = true
		}
		for _, k := range keys {
			if k == r || used[k] {
				continue
			}
			if _, take := args[r]; take {
				break
			}
			if candSet[normalizeToolToken(k)] {
				args[r] = args[k]
				delete(args, k)
				used[k] = true
				log.Printf("[tool-validation] mapped argument alias %q -> %q", k, r)
				break
			}
		}
	}
}

// fillRequiredDefaults back-fills missing required parameters ONLY where the
// fill cannot change what the tool does: explicit schema default, the first
// enum value, or a purely descriptive "description"-style field. Semantic
// core fields (command/content/path/...) are NEVER invented.
func fillRequiredDefaults(args map[string]any, props map[string]any, required []string) {
	for _, r := range required {
		if _, has := args[r]; has {
			continue
		}
		p, _ := props[r].(map[string]any)
		if p == nil {
			continue
		}
		if dv, ok := p["default"]; ok {
			args[r] = dv
			log.Printf("[tool-validation] default-filled required arg %q", r)
			continue
		}
		if enum, ok := p["enum"].([]any); ok && len(enum) > 0 {
			args[r] = enum[0]
			log.Printf("[tool-validation] enum-default-filled required arg %q", r)
			continue
		}
		norm := normalizeToolToken(r)
		if norm == "description" || norm == "label" || norm == "title" {
			args[r] = "Auto-generated"
			continue
		}
	}
}

// coerceNumericTypes converts numeric-looking strings for fields the schema
// types as number/integer ("timeout":"10000" is a classic model habit).
func coerceNumericTypes(args map[string]any, props map[string]any) {
	for k, v := range args {
		s, ok := v.(string)
		if !ok {
			continue
		}
		p, _ := props[k].(map[string]any)
		if p == nil {
			continue
		}
		t, _ := p["type"].(string)
		if t != "number" && t != "integer" {
			continue
		}
		str := strings.TrimSpace(s)
		f, err := strconv.ParseFloat(str, 64)
		if err != nil {
			continue
		}
		if t == "integer" && f != float64(int64(f)) {
			continue // fractional value cannot satisfy an integer schema
		}
		args[k] = f // store float64: downstream schemaValid expects it
	}
}

// ---------- degraded-answer prose cleanup ----------

var degradeFenceRe = regexp.MustCompile("(?s)```([A-Za-z0-9_-]+)\\s*\\n(.*?)\\n?```")
var degradeToolTagRe = regexp.MustCompile("(?s)<m365-tool-call>.*?</m365-tool-call>")
var degradeBareCallRe = regexp.MustCompile("(?m)^[ \\t]*([A-Za-z_][A-Za-z0-9_-]*)[ \\t]*\\{.*\\}[ \\t]*$")

// sanitizeToolCallText removes tool-call syntax (fenced, XML-tagged, bare
// name{...} lines) from a response used as the DEGRADED prose fallback, so a
// failed tool call never leaks raw JSON or placeholder tags to the user.
func sanitizeToolCallText(text string, tools []map[string]any) string {
	// NOTE: `declared` must contain ONLY the client's real tool names. Mixing
	// the bash-alias dictionary into it made toolCategoryMatchesBash treat
	// every ```bash explanation fence as a consumed call and strip it.
	declared := map[string]bool{}
	for _, n := range declaredToolNames(tools) {
		declared[normalizeToolToken(n)] = true
	}
	out := degradeToolTagRe.ReplaceAllString(text, "")
	out = degradeFenceRe.ReplaceAllStringFunc(out, func(block string) string {
		m := degradeFenceRe.FindStringSubmatch(block)
		if m == nil {
			return block // keep non-matching fences (real code samples)
		}
		info := normalizeToolToken(m[1])
		// A fenced block is a CONSUMED call attempt (thus stripped) only when
		// its info string names a declared tool AND its body is a JSON object.
		// Fences that merely look similar (python/markdown examples, bash
		// snippets pasted for explanation) must stay in the degraded prose.
		if declared[info] {
			var probe map[string]any
			if p2, ok := lenientParseObject(m[2]); ok {
				probe = p2
			}
			if probe != nil {
				return ""
			}
		}
		if toolCategoryMatchesBash(info, declared) {
			if _, ok := lenientParseObject(m[2]); ok {
				// bash-word fence WITH a JSON body while a bash tool is
				// declared: a consumed call attempt, strip it.
				return ""
			}
		}
		return block
	})
	out = degradeBareCallRe.ReplaceAllStringFunc(out, func(line string) string {
		m := degradeBareCallRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			return ""
		}
		if declared[normalizeToolToken(m[1])] {
			return ""
		}
		return line
	})
	out = strings.TrimSpace(out)
	return out
}

func toolCategoryMatchesBash(info string, declared map[string]bool) bool {
	// true only when `info` is a bash-family word AND the client actually
	// declared a bash-family tool (checked against the REAL declared set,
	// not the alias-expanded dictionary).
	isBashWord := false
	for _, a := range toolCategoryAliases["bash"] {
		if normalizeToolToken(a) == info {
			isBashWord = true
			break
		}
	}
	if !isBashWord {
		return false
	}
	for _, n := range toolCategoryAliases["bash"] {
		if declared[normalizeToolToken(n)] {
			return true
		}
	}
	return false
}

// extractRequiredList pulls the required[] list out of a JSON schema map.
func extractRequiredList(v any) []string {
	switch val := v.(type) {
	case []string:
		out := make([]string, 0, len(val))
		for _, s := range val {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range val {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
