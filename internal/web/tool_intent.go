package web

// This file ports the natural-language tool-intent recovery layer from the
// sibling project cnb2api. When a model describes an action in prose instead of
// emitting a real tool call ("let me read that file", a bare shell command, a
// lone file path, or an English action promise), the turn stalls. These helpers
// detect that intent so the pipeline can trigger a targeted correction round.

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ---------- shared vocabulary ----------

// bareShellCommands is the set of command words recognised as a bare shell
// command at the start of a line.
var bareShellCommands = map[string]bool{
	"rg": true, "grep": true, "find": true, "cat": true, "ls": true, "pwd": true, "cd": true,
	"lsof": true, "stat": true, "file": true, "watch": true, "nohup": true, "readlink": true,
	"head": true, "tail": true, "wc": true, "awk": true, "sed": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "xargs": true, "tee": true, "echo": true,
	"git": true, "curl": true, "wget": true, "ping": true, "ssh": true, "scp": true,
	"diff": true, "patch": true, "tar": true, "zip": true, "unzip": true, "mkdir": true,
	"rm": true, "cp": true, "mv": true, "touch": true, "chmod": true, "chown": true,
	"ps": true, "top": true, "kill": true, "which": true, "env": true, "export": true,
	"pip": true, "npm": true, "node": true, "python": true, "python3": true, "go": true,
	"docker": true, "kubectl": true, "jq": true, "yq": true, "ag": true, "ack": true,
	"date": true, "uptime": true, "free": true, "pgrep": true, "pkill": true, "whoami": true,
	"hostname": true, "id": true, "df": true, "du": true, "ss": true, "netstat": true,
	"systemctl": true, "journalctl": true, "history": true, "type": true,
	"make": true, "cargo": true, "openssl": true, "base64": true, "md5sum": true, "sha256sum": true,
}

var (
	bareShellCommandRe = regexp.MustCompile(`^\s*[\$>]?\s*([a-z][a-z0-9]*)\s+(.+)$`)
	barePathCommandRe  = regexp.MustCompile(`^\s*[\$>]?\s*((?:\.{0,2}/)[\w.\-]+(?:/[\w.\-]+)*)(?:\s+(.*))?$`)
	shellProseRe       = regexp.MustCompile(`[。，；：！？、]|(?:然后|接着|这是|说明|结果|因为|所以|可以|需要|建议|下面|上面|将会|已经)`)
	shellSyntaxRe      = regexp.MustCompile(`;\s*\S|&&\s*\S|\|\|\s*\S|\|\s*\S|(?:^|[\w"')\]])\s*>>?\s*[\w/$]`)
	shellForLoopRe     = regexp.MustCompile(`(?:^|;|\s)for\s+[\w$]+\s+in\s`)
	shellDoneRe        = regexp.MustCompile(`\bdo\b|\bdone\b`)
	englishProseRe     = regexp.MustCompile(`(?i)(?:[.?!]\s|\b(?:the|this|that|these|those|you|your|we|our|was|were|been|has|have|had|should|would|could|will|may|might|must|but|what|how|please|following)\b|^\s*\|)`)
	protocolResidueRe  = regexp.MustCompile(`(?i)<\s*/?\s*(?:DSML|` + nativeParamTag + `|antml:)|^\s*[\{[]`)
	angleTagRe         = regexp.MustCompile(`<[^>]+>`)
	commonLangNames    = map[string]bool{
		"python": true, "python3": true, "javascript": true, "js": true, "typescript": true, "ts": true,
		"go": true, "rust": true, "java": true, "c": true, "cpp": true, "c++": true, "csharp": true,
		"php": true, "ruby": true, "swift": true, "kotlin": true, "scala": true, "r": true,
		"html": true, "css": true, "scss": true, "json": true, "yaml": true, "yml": true,
		"xml": true, "sql": true, "markdown": true, "md": true, "text": true, "txt": true,
		"diff": true, "patch": true, "ini": true, "toml": true,
		"makefile": true, "dockerfile": true, "graphql": true, "protobuf": true,
	}
)

const nativeParamTag = "param" + "eter"

var (
	bareFilePathTokenRe = regexp.MustCompile(`^(?:\.{0,2}/)?[\p{Han}\w.\-]+(?:/[\p{Han}\w.\-]+)+$`)
	bareFilePathExtRe   = regexp.MustCompile(`\.[A-Za-z][A-Za-z0-9]{0,5}$`)
	absPathRe           = regexp.MustCompile(`/[\w.\-/]{8,}`)
)

// ---------- plan words + suffix promise ----------

var (
	tailAnchorRe      = regexp.MustCompile(`(开始|现在|马上|立即|这就|接下来|下面|动手|先|让我|我来|我看|我查|我想|我要|我去)`)
	tailImmediacyRe   = regexp.MustCompile(`(立刻|立即|马上|这就|现在就|直接)`)
	tailActionRe      = regexp.MustCompile(`(执行|修改|编辑|替换|删除|创建|写入|运行|修复|处理|调用|改动|改造|动手|操作|改|读取|读|看|跑|查|验证|测试|检查|搜索|打开|获取|列出|更新|恢复|还原|改回|加回|安装|配置|部署|提交|推送|发送|拉取)`)
	tailAdviceRe      = regexp.MustCompile(`(即可|就行|就可以|便可|建议|不妨|可以试试|欢迎)`)
	tailCompletedRe   = regexp.MustCompile(`(执行|修改|编辑|替换|删除|创建|写入|运行|修复|处理|调用|改动|改造|读取|读|看|跑|查|验证|测试|检查|搜索|打开|获取|列出|更新|恢复|还原|改回|加回|安装|配置|部署|提交|推送|发送|拉取)(?:了|过|完)`)
	enActionPromiseRe = regexp.MustCompile(`(?i)\b(let me|i['’ ]*(?:'|ll|will)|i am going to|i'm going to|now i|first,? i|next,? i|i need to|i should|we'll|i'll just)\b.*\b(check|read|look|inspect|verify|review|edit|fix|update|replace|delete|remove|run|execute|open|list|show|search|test|fetch|scan|apply|grab|pull|add|write|try|see)\b`)
	actionRe          = regexp.MustCompile(`(查看|看看|看一下|看|确认|了解|读|读取|分析|检查|搜索|定位|获取|执行|运行|修改|替换|改造|编写|实现|验证|测试|调用|抓取|打开|列出|梳理|排查|修复)`)
	completedRe       = regexp.MustCompile(`(查看了|看了|确认了|了解了|读了|读取了|分析了|检查了|搜索了|获取了|执行了|运行了|修改了|替换了|改造了|编写了|实现了|验证了|测试了|调用了|抓取了|打开了|列出了|修复了)`)
	bareStartRe       = regexp.MustCompile(`^(?:现在|马上|立即|这就)?(?:就)?开始$`)
)

// ---------- tool name resolution (strict + alias) ----------

// sortedCategories returns category keys in deterministic order.
func sortedCategories(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// findToolByCategory finds the first declared tool matching any alias of the
// category (case-insensitive), using sorted names for determinism.
func findToolByCategory(tools []map[string]any, aliases []string) string {
	allowed := allowedToolNames(tools)
	names := declaredToolNames(tools)
	for _, alias := range aliases {
		if allowed[alias] {
			return alias
		}
		for _, name := range names {
			if strings.EqualFold(name, alias) {
				return name
			}
		}
	}
	return ""
}

// resolveToolNameStrict matches exact (case-insensitive) + whole-word category
// aliases, without substring matching (XML tags are false-positive sensitive).
func resolveToolNameStrict(emitted string, tools []map[string]any) string {
	allowed := allowedToolNames(tools)
	if allowed[emitted] {
		return emitted
	}
	lower := strings.ToLower(emitted)
	if allowed[lower] {
		return lower
	}
	for _, name := range declaredToolNames(tools) {
		if strings.EqualFold(name, emitted) {
			return name
		}
	}
	if aliases, ok := toolCategoryAliases[lower]; ok {
		return findToolByCategory(tools, aliases)
	}
	return ""
}

// resolveToolName is the fuzzy resolver: exact → case-insensitive → category
// word → substring containment (length >= 4).
func resolveToolName(emitted string, tools []map[string]any) string {
	allowed := allowedToolNames(tools)
	lower := strings.ToLower(emitted)
	names := declaredToolNames(tools)
	if allowed[emitted] {
		return emitted
	}
	if allowed[lower] {
		return lower
	}
	for _, name := range names {
		if strings.EqualFold(name, emitted) {
			return name
		}
	}
	if commonLangNames[lower] {
		return ""
	}
	for _, category := range sortedCategories(toolCategoryAliases) {
		for _, alias := range toolCategoryAliases[category] {
			if normalizeToolToken(alias) == normalizeToolToken(lower) {
				if resolved := findToolByCategory(tools, toolCategoryAliases[category]); resolved != "" {
					return resolved
				}
			}
		}
	}
	if len(lower) >= 4 {
		for _, name := range names {
			nl := strings.ToLower(name)
			if nl == "" {
				continue
			}
			if strings.Contains(nl, lower) || strings.Contains(lower, nl) {
				return name
			}
		}
	}
	return ""
}

// ---------- Chinese planning ----------

// hasChinesePlanning reports whether text is a short first-person plan
// ("let me check…") rather than a final answer.
func hasChinesePlanning(text string) bool {
	trimmed := strings.TrimSpace(text)
	if utf8.RuneCountInString(trimmed) > 300 {
		return false
	}
	if !actionRe.MatchString(trimmed) {
		return false
	}
	for _, p := range []string{"让我", "我来", "我先"} {
		if strings.Contains(trimmed, p) {
			return true
		}
	}
	if completedRe.MatchString(trimmed) {
		return false
	}
	if utf8.RuneCountInString(trimmed) <= 30 {
		for _, p := range []string{"先", "首先", "然后", "接下来"} {
			if strings.HasPrefix(trimmed, p) {
				return true
			}
		}
	}
	if utf8.RuneCountInString(trimmed) <= 20 {
		for _, p := range []string{"先", "首先", "然后", "接下来"} {
			if strings.Contains(trimmed, p) {
				return true
			}
		}
	}
	return false
}

// stripChinesePlanningPrefix removes a leading plan paragraph so the tool
// call that follows can be parsed.
var planningPrefixRes = []*regexp.Regexp{
	regexp.MustCompile(`(?s)^[让我我来].*?(?:\n\n|\r\n\r\n)`),
	regexp.MustCompile(`(?s)^[先正接下].*?(?:\n\n|\r\n\r\n)`),
	regexp.MustCompile(`(?s)^好的[，,].*?(?:\n\n|\r\n\r\n)`),
	regexp.MustCompile(`(?s)^[我你他她它].*?(?:需要改造|开始实施|按推荐).*?(?:\n\n|\r\n\r\n)`),
}

func stripChinesePlanningPrefix(text string) string {
	for _, re := range planningPrefixRes {
		if loc := re.FindStringIndex(text); loc != nil {
			stripped := strings.TrimSpace(text[loc[1]:])
			if stripped != "" {
				return stripped
			}
		}
	}
	return text
}

// ---------- suffix action promise ----------

// endsWithActionPromise reports whether the last clause is a self action
// promise ("start editing now") with no real tool call.
func endsWithActionPromise(text string) bool {
	trimmed := strings.TrimRight(strings.TrimSpace(text), "。！!？?…\n \t")
	last := trimmed
	if idx := strings.LastIndexAny(trimmed, "\n。！!？？"); idx >= 0 {
		r, _ := utf8.DecodeRuneInString(trimmed[idx:])
		last = strings.TrimSpace(trimmed[idx+utf8.RuneLen(r):])
	}
	runes := utf8.RuneCountInString(last)
	if runes == 0 || runes > 60 {
		return false
	}
	if strings.Contains(last, "你") || tailAdviceRe.MatchString(last) {
		return false
	}
	if tailCompletedRe.MatchString(last) {
		return false
	}
	if bareStartRe.MatchString(last) {
		return true
	}
	if runes <= 24 && tailAnchorRe.MatchString(last) && tailActionRe.MatchString(last) {
		return true
	}
	if tailImmediacyRe.MatchString(last) && tailActionRe.MatchString(last) {
		return true
	}
	if tailActionRe.MatchString(last) && (strings.HasSuffix(last, ":") || strings.HasSuffix(last, "：")) {
		return true
	}
	return false
}

// endsWithEnglishActionPromise reports whether the last non-empty line is an
// English action promise ("Let me check line 4.").
func endsWithEnglishActionPromise(text string) bool {
	lines := strings.Split(strings.TrimRight(text, " \t\r\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > 200 {
			return false
		}
		return enActionPromiseRe.MatchString(line)
	}
	return false
}

// ---------- bare paths ----------

// isBareFilePathLike reports whether the whole string is a file path token with
// a file extension (CJK included).
func isBareFilePathLike(t string) bool {
	if t == "" || len(t) > 240 || !strings.Contains(t, "/") {
		return false
	}
	if strings.Contains(t, "://") || bareShellCommands[strings.ToLower(t)] {
		return false
	}
	if !bareFilePathTokenRe.MatchString(t) {
		return false
	}
	return bareFilePathExtRe.MatchString(t[strings.LastIndex(t, "/")+1:])
}

// isLonePathLine reports whether the whole trimmed line is a single absolute
// file path (a strong "target file" signal).
func isLonePathLine(t string) bool {
	t = strings.TrimSpace(t)
	if t == "" || strings.ContainsAny(t, " \t") {
		return false
	}
	return isBareFilePathLike(t)
}

// endsWithAbsPath reports whether s's last whitespace-separated token is
// exactly an absolute path.
func endsWithAbsPath(s string) bool {
	s = strings.TrimRight(s, " \t")
	i := strings.LastIndexAny(s, " \t")
	tok := s[i+1:]
	if !strings.HasPrefix(tok, "/") {
		return false
	}
	return absPathRe.FindString(tok) == tok
}

// looksLikeOrphanedArgs reports whether the text is a malformed call whose
// argument values leaked as plain prose, ending with a bare absolute path.
func looksLikeOrphanedArgs(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if utf8.RuneCountInString(trimmed) > 220 {
		return false
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) > 10 {
		return false
	}
	if strings.ContainsAny(trimmed, "，；,;!?") || strings.Contains(trimmed, "？") {
		return false
	}
	if !endsWithAbsPath(trimmed[strings.LastIndex(trimmed, "\n")+1:]) {
		return false
	}
	for _, terminator := range []string{"。", "！", "？", ".", "!", "?", "，", ", "} {
		if strings.HasSuffix(trimmed, terminator) {
			return false
		}
	}
	return true
}

// ---------- bare shell commands ----------

// lineLooksLikeShell reports whether a single line is a bare shell command.
func lineLooksLikeShell(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if t == "" || shellProseRe.MatchString(t) || protocolResidueRe.MatchString(t) {
		return "", false
	}
	if m := bareShellCommandRe.FindStringSubmatch(t); m != nil && bareShellCommands[m[1]] {
		return t, true
	}
	if !strings.ContainsAny(t, " \t") && bareShellCommands[strings.ToLower(t)] {
		return t, true
	}
	if m := barePathCommandRe.FindStringSubmatch(t); m != nil && m[1] != "" {
		last := m[1][strings.LastIndex(m[1], "/")+1:]
		if i := strings.LastIndex(last, "."); i > 0 {
			if ext := strings.ToLower(last[i+1:]); ext != "sh" && ext != "bash" {
				return "", false
			}
		}
		return t, true
	}
	if !angleTagRe.MatchString(t) && !englishProseRe.MatchString(stripQuotedSpansForProse(t)) && !looksLikeSourceCode(t) && (shellSyntaxRe.MatchString(t) || (shellForLoopRe.MatchString(t) && shellDoneRe.MatchString(t))) {
		return t, true
	}
	return "", false
}

var trailingWordRunRe = regexp.MustCompile(`(?:\s+[A-Za-z][A-Za-z0-9._\-']*){3,}$`)

var shellTailKeepWords = map[string]bool{
	"in": true, "and": true, "the": true, "of": true, "to": true, "for": true,
	"with": true, "that": true, "this": true, "all": true, "every": true, "each": true,
	"show": true, "display": true, "verify": true, "ensure": true, "see": true, "note": true,
}

// stripShellTrailingProse cuts English explanation prose hanging off the end of
// a command line.
func stripShellTrailingProse(line string) string {
	loc := trailingWordRunRe.FindStringIndex(line)
	if loc == nil {
		return line
	}
	head := line[:loc[0]]
	words := strings.Fields(line[loc[0]:])
	cutAt := -1
	for i, w := range words {
		r, _ := utf8.DecodeRuneInString(w)
		if r >= 'A' && r <= 'Z' {
			cutAt = i
			break
		}
	}
	if cutAt < 0 {
		for i, w := range words {
			if shellTailKeepWords[strings.ToLower(w)] {
				cutAt = i
				break
			}
		}
	}
	if cutAt < 0 {
		return line
	}
	if cutAt == 0 {
		return strings.TrimRight(head, " ")
	}
	return head + " " + strings.Join(words[:cutAt], " ")
}

var bareShellContinuationRe = regexp.MustCompile(`^[/\-|.>\d]`)

// isBareShellContinuationLine reports whether a line continues a wrapped
// command (single token starting with a path/flag/operator).
func isBareShellContinuationLine(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t") {
		return false
	}
	if strings.ContainsAny(s, "。，；：！？") {
		return false
	}
	return bareShellContinuationRe.MatchString(s)
}

var shellLeadProseRe = regexp.MustCompile(`然后|接着|这是|说明|结果|因为|所以|可以|需要|建议|将会|已经`)

// isShellLeadInLine reports whether the line is a short lead-in/label ("run
// this now:") that introduces a command and is never executed itself.
func isShellLeadInLine(t string) bool {
	if t == "" || utf8.RuneCountInString(t) > 60 {
		return false
	}
	if strings.ContainsAny(t, "。！？，、;；&`|<>\"'") {
		return false
	}
	return !shellLeadProseRe.MatchString(t)
}

// extractBareShellCommands recovers bare shell commands from a whole reply.
// Consecutive command/continuation lines merge into one command; a lead-in line
// closes the current group; any prose/fence/protocol-residue line aborts.
func extractBareShellCommands(text string) []detectedToolCall {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	var out []detectedToolCall
	var cur string
	var groupLead bool
	var groupCmdLines int
	knownToolAlias := func(block string) bool {
		if m := bareShellCommandRe.FindStringSubmatch(block); m != nil {
			switch m[1] {
			case "read_file", "write_file", "bash", "glob":
				return true
			}
		}
		return false
	}
	push := func() bool {
		if cur == "" {
			return true
		}
		if knownToolAlias(cur) || (groupLead && groupCmdLines >= 2) {
			return false
		}
		args, _ := json.Marshal(map[string]any{"command": cur})
		out = append(out, detectedToolCall{ID: callID("bash", string(args), len(out)), Name: "bash", Arguments: args, Type: "function"})
		cur = ""
		groupLead = false
		groupCmdLines = 0
		return true
	}
	for _, ln := range strings.Split(trimmed, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if _, ok := lineLooksLikeShell(t); ok {
			t = stripShellTrailingProse(t)
			if cur != "" {
				cur += " " + t
			} else {
				cur = t
			}
			groupCmdLines++
			continue
		}
		if isBareShellContinuationLine(t) && cur != "" {
			cur += " " + t
			continue
		}
		if isShellLeadInLine(t) {
			if !push() {
				return nil
			}
			groupLead = true
			continue
		}
		return nil
	}
	if !push() {
		return nil
	}
	return out
}

// extractBareFilePaths routes bare file-path lines to the client's read tool
// (script paths go to bash instead).
func extractBareFilePaths(text string, tools []map[string]any) []detectedToolCall {
	readTool := findToolByCategory(tools, toolCategoryAliases["read"])
	var out []detectedToolCall
	for _, ln := range strings.Split(strings.TrimSpace(text), "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if !isBareFilePathLike(t) {
			if isShellLeadInLine(t) {
				continue
			}
			return nil
		}
		switch {
		case strings.HasSuffix(t, ".sh") || strings.HasSuffix(t, ".bash"):
			args, _ := json.Marshal(map[string]any{"command": t})
			out = append(out, detectedToolCall{ID: callID("bash", string(args), len(out)), Name: "bash", Arguments: args, Type: "function"})
		case readTool == "":
			return nil
		default:
			param := primaryParamName(tools, readTool)
			if param == "" {
				param = "path"
			}
			args, _ := json.Marshal(map[string]any{param: t})
			out = append(out, detectedToolCall{ID: callID(readTool, string(args), 0), Name: readTool, Arguments: args, Type: "function"})
		}
	}
	return out
}

// ---------- source-code / prose helpers ----------

var (
	sourceCodeLeadRe = regexp.MustCompile(`^(?:def |class |import |from |package |func |public |private |protected |return |if |for |while |const |var |let )`)
	sourceAssignRe   = regexp.MustCompile(`^[A-Za-z_][\w.]*\s*=\s*[^=]`)
)

// looksLikeSourceCode reports whether a line is likely source code rather than
// a shell command (so it is never executed).
func looksLikeSourceCode(t string) bool {
	trimmed := strings.TrimSpace(t)
	if trimmed == "" {
		return false
	}
	if sourceCodeLeadRe.MatchString(trimmed) {
		return strings.HasSuffix(trimmed, ":") || strings.ContainsAny(trimmed, "{}()=") || strings.Contains(trimmed, "=>")
	}
	if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
		return true
	}
	// A bare assignment (no shell metacharacters) is source, not a command.
	if sourceAssignRe.MatchString(trimmed) && !strings.ContainsAny(trimmed, ";|&") {
		return true
	}
	return false
}

// stripQuotedSpansForProse removes quoted spans so English prose detection does
// not fire on quoted shell arguments.
func stripQuotedSpansForProse(s string) string {
	var b strings.Builder
	inSingle, inDouble := false, false
	for _, r := range s {
		switch {
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case !inSingle && !inDouble:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---------- intent aggregator ----------

// hasToolCallIntent reports whether text carries tool-call intent even though
// structured parsing failed, so the pipeline can trigger a correction round.
func hasToolCallIntent(text string, tools []map[string]any) bool {
	lower := strings.ToLower(text)

	for _, m := range xmlTagOpenRe.FindAllStringIndex(text, -1) {
		tagName := text[m[0]+1 : m[1]-1]
		closeTag := "</" + tagName + ">"
		if !strings.Contains(text[m[1]:], closeTag) {
			if resolveToolName(tagName, tools) != "" || isShellName(tagName) {
				return true
			}
		}
	}

	for _, m := range toolCallBraceRe.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		if resolveToolName(name, tools) != "" || isShellName(name) {
			return true
		}
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Check the shell form BEFORE the lead-in form: a bare command such as
		// "ls -la /tmp" has no prose punctuation and would otherwise be
		// misclassified as an inert lead-in label.
		if _, ok := lineLooksLikeShell(trimmed); ok {
			return true
		}
		if isShellLeadInLine(trimmed) {
			continue
		}
		break
	}

	for _, m := range toolCallBraceRe.FindAllStringSubmatchIndex(text, -1) {
		name := text[m[2]:m[3]]
		if resolveToolName(name, tools) == "" && !isShellName(name) && !bareShellCommands[name] {
			if !commonLangNames[strings.ToLower(name)] {
				return true
			}
		}
	}

	for _, p := range []string{"[tool call:", "call_tool:", "function_call:", "tc_start", "tool_call:", "CALL_TOOL:"} {
		if strings.Contains(lower, p) {
			return true
		}
	}

	if strings.Contains(lower, "(assistant called tool") {
		return true
	}
	// A reply that is exactly one declared tool name is a zero-arg call in prose.
	if extractStandaloneToolName(text, tools) != nil {
		return true
	}
	if endsWithActionPromise(text) || endsWithEnglishActionPromise(text) {
		return true
	}
	if looksLikeOrphanedArgs(text) {
		return true
	}
	for _, ln := range strings.Split(text, "\n") {
		if isLonePathLine(ln) {
			return true
		}
	}
	return false
}

// isShellName reports whether name is a bash-family tool spelling.
func isShellName(name string) bool {
	switch strings.ToLower(name) {
	case "bash", "sh", "shell", "cmd", "powershell":
		return true
	}
	return false
}

// recoverNaturalLanguageToolCalls is the last-resort extractor run when
// structured parsing yields nothing but the reply clearly carries tool intent:
// bare shell commands, lone file paths, and bare zero-arg tool names are
// converted into real calls using the client's declared tools.
//
// It returns nil when the reply is prose/fenced/doc content that must NOT be
// executed.
func recoverNaturalLanguageToolCalls(text string, tools []map[string]any, choice any) []detectedToolCall {
	if len(tools) == 0 {
		return nil
	}
	// tool_choice=none disables recovery entirely.
	if s, ok := choice.(string); ok && strings.EqualFold(s, "none") {
		return nil
	}
	// Strip a leading Chinese planning paragraph ("let me take a look…")
	// before scanning, so the plan sentence does not shadow the real command.
	scan := text
	if hasChinesePlanning(scan) {
		if stripped := stripChinesePlanningPrefix(scan); stripped != "" {
			scan = stripped
		}
	}

	shellTool := findToolByCategory(tools, toolCategoryAliases["bash"])

	if shellTool != "" {
		if calls := extractBareShellCommands(scan); len(calls) > 0 {
			for i := range calls {
				calls[i].Name = shellTool
			}
			return calls
		}
	}
	if calls := extractBareFilePaths(scan, tools); len(calls) > 0 {
		return calls
	}
	// Zero-arg tool emitted as just its name ("todo" / "list_dir").
	if call := extractStandaloneToolName(scan, tools); call != nil {
		return []detectedToolCall{*call}
	}
	return nil
}

// extractStandaloneToolName recognises a reply that is exactly one known tool
// name with no arguments — the lazy zero-arg call form. Only strict name
// matching is used so ordinary prose/fragments cannot trigger it.
func extractStandaloneToolName(text string, tools []map[string]any) *detectedToolCall {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.ContainsAny(trimmed, " \t\n{") {
		return nil
	}
	switch strings.ToLower(trimmed) {
	case "bash", "sh", "shell", "cmd", "powershell":
		return nil
	}
	resolved := resolveToolNameStrict(trimmed, tools)
	if resolved == "" {
		r := resolveToolName(trimmed, tools)
		if r == "" || !allowedToolNames(tools)[r] {
			return nil
		}
		resolved = r
	}
	return &detectedToolCall{ID: callID(resolved, "{}", 0), Name: resolved, Arguments: json.RawMessage("{}"), Type: "function"}
}

// toolIntentCorrectionPrompt teaches the model to re-emit a real tool call for
// the action it only described in prose.
func toolIntentCorrectionPrompt(original, emitted string, tools []map[string]any) string {
	names := declaredToolNames(tools)
	return "You described an action in prose but did not emit a tool call. " +
		"Re-emit the SAME action as exactly one real tool call using ONLY these declared tools: " +
		strings.Join(names, ", ") + ".\n" +
		"Rules: output ONLY the tool call (tool_name {\"param\":\"value\"} or a fenced block); " +
		"do NOT describe, do NOT explain, do NOT apologize.\n\n" +
		"User request:\n" + truncateForPrompt(original, 4000) + "\n\n" +
		"Your previous reply (unparsed):\n" + truncateForPrompt(emitted, 4000)
}

func truncateForPrompt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// stripPlaceholderEchoes removes echoed history placeholders ("(assistant
// called tools)").
func stripPlaceholderEchoes(text string) string {
	return placeholderEchoRe.ReplaceAllString(text, "")
}

var placeholderEchoRe = regexp.MustCompile(`(?m)^[ \t]*\(assistant called tools?[^\)]*\)[ \t]*$`)

// ---------- shared parse regexes ----------

var (
	xmlTagOpenRe    = regexp.MustCompile(`<([A-Za-z_][A-Za-z0-9_-]*)>`)
	toolCallBraceRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)\s*\{`)
)

// primaryParamName returns the first required field of a tool schema (or the
// first declared property when there is no required list).
func primaryParamName(tools []map[string]any, toolName string) string {
	for _, t := range tools {
		f, _ := t["function"].(map[string]any)
		if f == nil {
			continue
		}
		if n, _ := f["name"].(string); n != toolName {
			continue
		}
		params, _ := f["parameters"].(map[string]any)
		if params == nil {
			return ""
		}
		if req := extractRequiredList(params["required"]); len(req) > 0 {
			return req[0]
		}
		props, _ := params["properties"].(map[string]any)
		for name := range props {
			return name
		}
	}
	return ""
}
