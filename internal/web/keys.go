package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type apiKeyRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"hash"`
	Raw        string     `json:"raw,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool       `json:"revoked"`
}
type apiKeyStore struct {
	mu      sync.Mutex
	Path    string         `json:"-"` // never deserialized: loading a hostile/corrupt file must not redirect where the store writes
	Keys    []apiKeyRecord `json:"keys"`
	persist *persistStore
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{Path: path}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func openAPIKeys() *apiKeyStore {
	p := strings.TrimSpace(os.Getenv("M365_API_KEYS"))
	if p == "" {
		h, _ := os.UserHomeDir()
		p = filepath.Join(h, ".config", "m365-copilot2api", "api-keys.json")
	}
	s := newAPIKeyStore(p)
	b, e := os.ReadFile(p)
	if e == nil && json.Unmarshal(b, s) == nil {
		migrated := false
		for i := range s.Keys {
			if s.Keys[i].Raw != "" {
				if s.Keys[i].Hash == "" {
					s.Keys[i].Hash = keyHash(s.Keys[i].Raw)
				}
				s.Keys[i].Raw = ""
				migrated = true
			}
		}
		if migrated {
			_ = s.flush()
		}
	}
	return s
}
func (s *apiKeyStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(s.Path, b, 0600)
}
func keyHash(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }
func (s *apiKeyStore) create(name string) (apiKeyRecord, string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return apiKeyRecord{}, "", e
	}
	raw := "m365_" + hex.EncodeToString(b)
	r := apiKeyRecord{ID: hex.EncodeToString(b[:8]), Name: name, Prefix: raw[:12], Hash: keyHash(raw), CreatedAt: time.Now()}
	s.mu.Lock()
	s.Keys = append(s.Keys, r)
	s.mu.Unlock()
	if err := s.persist.flushNowBlocking(); err != nil {
		// Roll back by ID: a concurrent create/delete may have moved the
		// tail since our append, so s.Keys[:len-1] could drop the wrong key.
		s.mu.Lock()
		for i := range s.Keys {
			if s.Keys[i].ID == r.ID {
				s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
		return apiKeyRecord{}, "", err
	}
	r.Hash = ""
	r.Raw = ""
	return r, raw, nil
}
func (s *apiKeyStore) list() []apiKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiKeyRecord, len(s.Keys))
	copy(out, s.Keys)
	for i := range out {
		out[i].Hash = ""
		out[i].Raw = ""
	}
	return out
}
func (s *apiKeyStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	found := false
	for i := range s.Keys {
		if s.Keys[i].ID == id && !s.Keys[i].Revoked {
			s.Keys[i].Revoked = true
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		return false, nil
	}
	if err := s.persist.flushNowBlocking(); err != nil {
		// Roll back by ID — index i from before the unlock may point at a
		// different record after concurrent create/delete.
		s.mu.Lock()
		for i := range s.Keys {
			if s.Keys[i].ID == id {
				s.Keys[i].Revoked = false
				break
			}
		}
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}

// delete physically removes a key record, rolling back on persistence failure.
func (s *apiKeyStore) delete(id string) (bool, error) {
	s.mu.Lock()
	var removed *apiKeyRecord
	for i := range s.Keys {
		if s.Keys[i].ID == id {
			cp := s.Keys[i]
			removed = &cp
			s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	if removed == nil {
		return false, nil
	}
	if err := s.persist.flushNowBlocking(); err != nil {
		// Re-add by value: the original index is stale after concurrent
		// mutations, and slicing s.Keys[:i] with a stale i can panic.
		s.mu.Lock()
		exists := false
		for i := range s.Keys {
			if s.Keys[i].ID == removed.ID {
				exists = true
				break
			}
		}
		if !exists {
			s.Keys = append(s.Keys, *removed)
		}
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}

func (s *apiKeyStore) update(id, name string, revoked *bool) (bool, error) {
	s.mu.Lock()
	found := false
	var oldName string
	var oldRevoked bool
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		oldName = s.Keys[i].Name
		oldRevoked = s.Keys[i].Revoked
		if name != "" {
			s.Keys[i].Name = name
		}
		if revoked != nil {
			s.Keys[i].Revoked = *revoked
		}
		found = true
		break
	}
	s.mu.Unlock()
	if !found {
		return false, nil
	}
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		for i := range s.Keys {
			if s.Keys[i].ID == id {
				s.Keys[i].Name = oldName
				s.Keys[i].Revoked = oldRevoked
				break
			}
		}
		s.mu.Unlock()
		return false, err
	}
	return true, nil
}
func (s *apiKeyStore) valid(raw string) bool {
	s.mu.Lock()
	h := keyHash(raw)
	found := false
	for i := range s.Keys {
		if s.Keys[i].Hash == h && !s.Keys[i].Revoked {
			now := time.Now()
			s.Keys[i].LastUsedAt = &now
			found = true
			break
		}
	}
	s.mu.Unlock()
	if found {
		s.persist.markDirty()
	}
	return found
}
