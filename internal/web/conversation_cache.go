package web

import (
	"crypto/sha256"
	"encoding/hex"
	"m365-copilot2api/internal/chathub"
	"net/http"
	"strings"
	"sync"
	"time"
)

type cachedConversation struct {
	ConversationID string
	SessionID      string
	Tone           string
	TurnCount      int
	MessageCount   int
	CreatedAt      time.Time
	LastUsedAt     time.Time
	SystemPrompt   string
}

type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*cachedConversation
	maxAge  time.Duration
}

func newConversationCache() *conversationCache {
	return &conversationCache{
		entries: make(map[string]*cachedConversation),
		maxAge:  2 * time.Hour,
	}
}

func (c *conversationCache) key(sessionKey, accountID, model string) string {
	return sessionKey + "|" + accountID + "|" + model
}

func (c *conversationCache) Lookup(sessionKey, accountID, model string) *cachedConversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.key(sessionKey, accountID, model)
	entry := c.entries[k]
	if entry == nil {
		return nil
	}
	if time.Since(entry.LastUsedAt) > c.maxAge {
		delete(c.entries, k)
		return nil
	}
	return entry
}

func (c *conversationCache) Store(sessionKey, accountID, model string, conv *cachedConversation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv.LastUsedAt = time.Now()
	c.entries[c.key(sessionKey, accountID, model)] = conv
}

func (c *conversationCache) Invalidate(sessionKey, accountID, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, c.key(sessionKey, accountID, model))
}

func (c *conversationCache) GC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.entries {
		if now.Sub(v.LastUsedAt) > c.maxAge {
			delete(c.entries, k)
		}
	}
}

func (c *conversationCache) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"cached_conversations": len(c.entries)}
}

func systemPromptHash(messages []oaiMsg) string {
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			text := contentToString(m.Content)
			h := sha256.Sum256([]byte(text))
			return hex.EncodeToString(h[:])
		}
	}
	return ""
}

func extractLastUserMessage(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return contentToString(messages[i].Content)
		}
	}
	return ""
}

func (s *Server) storeConvCache(sessionKey, accID, model string, res chathub.Result, tone string, messages []oaiMsg, reused bool) {
	if res.ConversationID == "" {
		return
	}
	cached := s.convCache.Lookup(sessionKey, accID, model)
	entry := &cachedConversation{
		ConversationID: res.ConversationID,
		SessionID:      res.SessionID,
		Tone:           tone,
		MessageCount:   len(messages),
		SystemPrompt:   systemPromptHash(messages),
	}
	if cached != nil && cached.ConversationID == res.ConversationID {
		entry.TurnCount = cached.TurnCount + 1
	} else {
		entry.TurnCount = 1
	}
	s.convCache.Store(sessionKey, accID, model, entry)
}

func (s *Server) invalidateConvCache(sessionKey, accID, model string) {
	s.convCache.Invalidate(sessionKey, accID, model)
}

// convSessionKey derives a stable isolation key for the conversation cache.
// It combines the caller's tenant (API-key hash) with a client dimension
// (explicit session id > user field > IP+UA fingerprint) so that two
// different clients hitting the same account+model never share the same
// cached M365 conversation. This prevents cross-session context leakage
// when multiple tools or users call the API concurrently.
//
// Priority: X-M365-Session-Id > body.User > IP+UA fingerprint.
// The tenant (derived from the API key) is always prepended so that two
// different API keys are isolated even if they present the same session id
// or come from the same IP.
func convSessionKey(r *http.Request, body *oaiReq) string {
	tenant := tenantFromRequest(r)

	// Explicit client session id is the strongest signal.
	if explicit := strings.TrimSpace(r.Header.Get("X-M365-Session-Id")); explicit != "" {
		return tenant + "|" + explicit
	}

	// The OpenAI "user" field is the next best signal.
	if body != nil && strings.TrimSpace(body.User) != "" {
		h := sha256.Sum256([]byte(body.User))
		return tenant + "|u:" + hex.EncodeToString(h[:16])
	}

	// Fall back to IP+UA fingerprint for anonymous callers.
	return tenant + "|" + clientIPFingerprint(r)
}
