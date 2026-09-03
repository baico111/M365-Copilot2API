package web

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type conversation struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"accountId"`
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	Title          string    `json:"title,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type sessionStore struct {
	mu      sync.Mutex
	path    string
	data    map[string]conversation
	persist *persistStore
}

func openSessionStore() *sessionStore {
	// M365_SESSION_CACHE belongs to sessionResolver (sessions.json, a JSON
	// array). This legacy store is a JSON object — sharing the file would
	// make the two writers clobber each other on every flush, silently
	// wiping one side's bindings after restart.
	path := os.Getenv("M365_SESSION_BINDINGS")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-copilot2api-sessions.json")
	}
	s := &sessionStore{path: path, data: map[string]conversation{}}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			log.Printf("[sessions] failed to unmarshal %s: %v", path, err)
		}
	}
	return s
}

// flush 在锁内生成快照，锁外写盘。
func (s *sessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *sessionStore) list() []conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]conversation, 0, len(s.data))
	for _, v := range s.data {
		out = append(out, v)
	}
	return out
}

func (s *sessionStore) get(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok
}

func (s *sessionStore) upsert(v conversation) conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	s.data[v.ID] = v
	s.persist.markDirty()
	return v
}

func (s *sessionStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; !ok {
		return false
	}
	delete(s.data, id)
	s.persist.markDirty()
	return true
}

// removeByConversation drops every legacy session_key binding pointing at a
// cloud conversation that was deleted; a stale binding would pin requests to
// a dead conversation and surface as 502/empty upstream responses.
func (s *sessionStore) removeByConversation(convID string) {
	if convID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k, v := range s.data {
		if v.ConversationID == convID {
			delete(s.data, k)
			changed = true
		}
	}
	if changed {
		s.persist.markDirty()
	}
}

type userSession struct {
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	AccountID      string    `json:"accountId"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
}

type userSessionStore struct {
	mu      sync.Mutex
	path    string
	data    map[string]userSession
	ttl     time.Duration
	persist *persistStore
}

func openUserSessionStore(ttl time.Duration) *userSessionStore {
	path := os.Getenv("M365_USER_SESSION_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-copilot2api-user-sessions.json")
	}
	s := &userSessionStore{path: path, data: map[string]userSession{}, ttl: ttl}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			log.Printf("[user-sessions] failed to unmarshal %s: %v", path, err)
		}
	}
	s.evictLocked()
	return s
}

// flush 在锁内生成快照，锁外写盘。
func (s *userSessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *userSessionStore) evictLocked() {
	if s.ttl <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.ttl)
	for k, v := range s.data {
		if v.LastUsedAt.Before(cutoff) {
			delete(s.data, k)
		}
	}
}

// userKey namespaces the client-supplied `user` field by tenant so two API
// keys that pass the same `user` value can never resume each other's
// conversation. The stored key is opaque and never returned to a caller.
func userKey(tenant, user string) string { return tenant + "\x00" + user }

// sessionScope namespaces the legacy client-supplied `sessionKey` by tenant.
// Without the prefix two API keys that pass the same session_key string hit
// the same record, and the AccountID/ConversationID pinned inside it route
// requests straight into the other tenant's cloud conversation.
func sessionScope(r *http.Request, sessionKey string) string {
	return tenantFromRequest(r) + "\x00" + sessionKey
}

func (s *userSessionStore) Get(tenant, user string) (userSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	key := userKey(tenant, user)
	v, ok := s.data[key]
	if ok {
		v.LastUsedAt = time.Now().UTC()
		s.data[key] = v
		s.persist.markDirty()
	}
	return v, ok
}

func (s *userSessionStore) Put(tenant, user, conversationID, sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[userKey(tenant, user)] = userSession{
		ConversationID: conversationID,
		SessionID:      sessionID,
		AccountID:      accountID,
		LastUsedAt:     time.Now().UTC(),
	}
	s.persist.markDirty()
}

func (s *userSessionStore) Delete(tenant, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, userKey(tenant, user))
	s.persist.markDirty()
}

// ActiveConversations returns conversation IDs whose owning user used the
// session within the given window. The auto-cleanup skips these so a user's
// in-flight conversation is never removed while still in use.
func (s *userSessionStore) ActiveConversations(window time.Duration) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().Add(-window)
	out := map[string]bool{}
	for _, v := range s.data {
		if v.LastUsedAt.After(cutoff) {
			out[v.ConversationID] = true
		}
	}
	return out
}

// RemoveByConversation drops every user→session binding that points at the
// given cloud conversation. Called when the conversation is deleted (admin or
// auto-cleanup) so the next request cannot resume a dead conversation and get
// upstream errors that surface as 502/empty replies.
func (s *userSessionStore) RemoveByConversation(convID string) {
	if convID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k, v := range s.data {
		if v.ConversationID == convID {
			delete(s.data, k)
			changed = true
		}
	}
	if changed {
		s.persist.markDirty()
	}
}
