package web

import (
	"strings"

	"m365-copilot2api/internal/chathub"
)

type atomKind string

const (
	kindSystem atomKind = "SYSTEM"
	kindUser   atomKind = "USER"
	kindTool   atomKind = "ATOM_TOOL"
	kindAssist atomKind = "ASSIST"
	kindAnchor atomKind = "ANCHOR"
)

type contextAtom struct {
	Kind   atomKind
	Msgs   []oaiMsg
	Tokens int
	Start  int
	End    int
}

func estimateBudgetTokens(text string) int {
	if enc, err := getGPTTokenizer(); err == nil {
		if ids, _, e := enc.Encode(text); e == nil {
			return len(ids)
		}
	}
	return heuristicTokenCount(text)
}

func estimateMessageTokens(m oaiMsg, counter func(string) int) int {
	if counter == nil {
		counter = estimateBudgetTokens
	}
	tokens := messageProtocolTokens
	tokens += counter(m.Role)
	tokens += counter(m.Name)
	tokens += counter(m.ToolCallID)
	tokens += serializedTokenCount(m.Content, counter)
	for _, call := range m.ToolCalls {
		tokens += serializedTokenCount(call, counter)
	}
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func buildAtoms(messages []oaiMsg) []contextAtom {
	if len(messages) == 0 {
		return nil
	}
	counter, _ := tokenEstimator("gpt-4")
	if counter == nil {
		counter = estimateBudgetTokens
	}
	var atoms []contextAtom
	i := 0
	for i < len(messages) {
		m := messages[i]
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			start := i
			var msgs []oaiMsg
			total := 0
			for i < len(messages) {
				r := strings.ToLower(strings.TrimSpace(messages[i].Role))
				if r != "system" && r != "developer" {
					break
				}
				msgs = append(msgs, messages[i])
				total += estimateMessageTokens(messages[i], counter)
				i++
			}
			atoms = append(atoms, contextAtom{Kind: kindSystem, Msgs: msgs, Tokens: total, Start: start, End: i})
			continue
		}
		if role == "assistant" && len(m.ToolCalls) > 0 {
			start := i
			var msgs []oaiMsg
			total := 0
			msgs = append(msgs, m)
			total += estimateMessageTokens(m, counter)
			i++
			for i < len(messages) && strings.ToLower(strings.TrimSpace(messages[i].Role)) == "tool" {
				msgs = append(msgs, messages[i])
				total += estimateMessageTokens(messages[i], counter)
				i++
			}
			atoms = append(atoms, contextAtom{Kind: kindTool, Msgs: msgs, Tokens: total, Start: start, End: i})
			continue
		}
		if role == "tool" {
			start := i
			var msgs []oaiMsg
			total := 0
			for i < len(messages) && strings.ToLower(strings.TrimSpace(messages[i].Role)) == "tool" {
				msgs = append(msgs, messages[i])
				total += estimateMessageTokens(messages[i], counter)
				i++
			}
			atoms = append(atoms, contextAtom{Kind: kindTool, Msgs: msgs, Tokens: total, Start: start, End: i})
			continue
		}
		if role == "user" {
			atoms = append(atoms, contextAtom{Kind: kindUser, Msgs: []oaiMsg{m}, Tokens: estimateMessageTokens(m, counter), Start: i, End: i + 1})
			i++
			continue
		}
		if role == "assistant" {
			atoms = append(atoms, contextAtom{Kind: kindAssist, Msgs: []oaiMsg{m}, Tokens: estimateMessageTokens(m, counter), Start: i, End: i + 1})
			i++
			continue
		}
		atoms = append(atoms, contextAtom{Kind: kindUser, Msgs: []oaiMsg{m}, Tokens: estimateMessageTokens(m, counter), Start: i, End: i + 1})
		i++
	}
	for idx, a := range atoms {
		if a.Kind == kindUser {
			atoms[idx].Kind = kindAnchor
			break
		}
	}
	return atoms
}

func flattenAtoms(atoms []contextAtom, attachments []chathub.Attachment) (string, []chathub.Attachment) {
	var msgs []oaiMsg
	for _, a := range atoms {
		msgs = append(msgs, a.Msgs...)
	}
	return flattenPromptMessages(msgs, attachments)
}

func flattenPromptMessagesWithBudget(messages []oaiMsg, attachments []chathub.Attachment, budget int) (string, []chathub.Attachment, bool, error) {
	truncatedMsgs, truncated, err := slidingWindow(messages, budget)
	if err != nil {
		return "", attachments, false, err
	}
	prompt, atts := flattenPromptMessages(truncatedMsgs, attachments)
	return prompt, atts, truncated, nil
}

// budget for slidingWindow: B = ContextWindow - MaxOutput - 512
func slidingWindow(messages []oaiMsg, budget int) ([]oaiMsg, bool, error) {
	if budget <= 0 {
		budget = 1024
	}
	atoms := buildAtoms(messages)
	if len(atoms) == 0 {
		return messages, false, nil
	}
	total := 0
	for _, a := range atoms {
		total += a.Tokens
	}
	total += requestProtocolTokens + replyPrimingTokens
	if total <= budget {
		return messages, false, nil
	}
	var p0Indices []int
	anchorIdx := -1
	for idx, a := range atoms {
		if a.Kind == kindSystem {
			p0Indices = append(p0Indices, idx)
		}
		if a.Kind == kindAnchor && anchorIdx == -1 {
			anchorIdx = idx
		}
	}
	var p1Indices []int
	for idx := len(atoms) - 1; idx >= 0; idx-- {
		if atoms[idx].Kind != kindTool {
			break
		}
		p1Indices = append([]int{idx}, p1Indices...)
	}
	sumP0P1 := requestProtocolTokens + replyPrimingTokens
	for _, idx := range p0Indices {
		sumP0P1 += atoms[idx].Tokens
	}
	for _, idx := range p1Indices {
		sumP0P1 += atoms[idx].Tokens
	}
	if anchorIdx != -1 {
		sumP0P1 += atoms[anchorIdx].Tokens
	}
	// The pinned set (system + trailing tool atom + current task) can exceed
	// the whole budget on its own — e.g. an agent session whose history grew
	// past the window while the system prompt and latest tool result are each
	// large. Hard-failing here returns 400 on every subsequent turn, which the
	// client sees as a permanent failure and retries forever. Instead, shed
	// the pinned parts that can be dropped, in reverse priority order:
	//   1. the trailing tool atom (its content is the least load-bearing),
	//   2. the anchor, but only if system+tool alone already fits,
	//   3. nothing else — keep at least the newest anchor and one system block.
	if sumP0P1 > budget {
		for len(p1Indices) > 0 && sumP0P1 > budget {
			last := p1Indices[len(p1Indices)-1]
			sumP0P1 -= atoms[last].Tokens
			p1Indices = p1Indices[:len(p1Indices)-1]
		}
	}
	if sumP0P1 > budget && anchorIdx != -1 && len(p1Indices) == 0 {
		// Even system + tool fits nowhere with the anchor pinned. Drop the
		// oldest system blocks first; keep the most recent one if possible.
		for len(p0Indices) > 1 && sumP0P1 > budget {
			oldest := p0Indices[0]
			sumP0P1 -= atoms[oldest].Tokens
			p0Indices = p0Indices[1:]
		}
	}
	if sumP0P1 > budget {
		// As a last resort, keep only the current task (anchor) and the newest
		// system block, trimming the system text itself rather than failing.
		minimal := make([]oaiMsg, 0, 2)
		budgetForPinned := budget - requestProtocolTokens - replyPrimingTokens
		if budgetForPinned < 1 {
			budgetForPinned = 1
		}
		if anchorIdx != -1 {
			minimal = append(minimal, trimMessagesToBudget(atoms[anchorIdx].Msgs, budgetForPinned)...)
		} else if len(atoms) > 0 {
			minimal = append(minimal, trimMessagesToBudget(atoms[len(atoms)-1].Msgs, budgetForPinned)...)
		}
		return minimal, true, nil
	}
	remaining := budget - sumP0P1
	selected := make(map[int]bool)
	for _, idx := range p0Indices {
		selected[idx] = true
	}
	for _, idx := range p1Indices {
		selected[idx] = true
	}
	if anchorIdx != -1 {
		selected[anchorIdx] = true
	}
	for idx := len(atoms) - 1; idx >= 0; idx-- {
		if selected[idx] {
			continue
		}
		tok := atoms[idx].Tokens
		if tok <= remaining {
			selected[idx] = true
			remaining -= tok
		}
	}
	var out []oaiMsg
	for idx, a := range atoms {
		if selected[idx] {
			out = append(out, a.Msgs...)
		}
	}
	truncated := len(selected) < len(atoms)
	if len(out) == 0 && len(atoms) > 0 {
		last := atoms[len(atoms)-1]
		out = append(out, last.Msgs...)
		truncated = true
	}
	return out, truncated, nil
}

// trimMessagesToBudget returns the input messages with their text content cut
// down so the total estimated tokens fit budget. It is the last-resort path
// when even a minimal pinned set exceeds the window; it never fails, always
// returning at least the newest message content (possibly trimmed).
func trimMessagesToBudget(msgs []oaiMsg, budget int) []oaiMsg {
	if len(msgs) == 0 {
		return msgs
	}
	if budget < 1 {
		budget = 1
	}
	out := make([]oaiMsg, len(msgs))
	copy(out, msgs)

	// Drop older messages entirely while over budget, keeping the last one.
	for len(out) > 1 {
		total := 0
		for _, m := range out {
			total += estimateMessageTokens(m, nil)
		}
		if total <= budget {
			break
		}
		out = out[1:]
	}

	// Trim the remaining (newest) message content if it alone still overflows.
	if len(out) == 1 {
		content := messageContentString(out[0].Content)
		tokens := estimateBudgetTokens(content)
		if tokens > budget {
			// Keep the tail: the end of a tool result / task is usually the
			// most relevant part.
			runes := []rune(content)
			ratio := float64(budget) / float64(tokens)
			keep := int(float64(len(runes)) * ratio)
			if keep < 1 {
				keep = 1
			}
			if keep < len(runes) {
				out[0].Content = string(runes[len(runes)-keep:])
			}
		}
	}
	return out
}

// messageContentString renders a message's Content field as a plain string for
// token estimation/trimming, regardless of whether it holds a string or a
// structured content-part array.
func messageContentString(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return mustJSON(v)
	}
}
