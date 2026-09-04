package web

import (
	"context"
	"sync"
	"time"
)

// conversationGate serializes gateway turns against the same M365 cloud
// conversation.
//
// A ChatHub conversation (session/conversation id pair) is a strictly
// sequential upstream resource. But several reuse paths — the legacy
// session-key store, user-sessions, the content-key session resolver and the
// conversation cache — can hand the SAME conversation id to two concurrent
// /v1 requests at the same moment (think Claude Code plus OpenCode pointed at
// one API key and model, or one tool firing parallel turns). When both posts
// race into one thread, M365 queues/interleaves them, the turns answer each
// other's prompts, throttling state mixes, and the persisted threads pollute
// both clients' subsequent context (the observed "replies truncated, need
// continue under concurrent use").
//
// The gate makes that impossible: while a request owns a (account,
// conversation) pair, any other request that reused the same conversation
// either briefly waits or gracefully Degrades to a fresh conversation instead
// of interleaving on the shared thread.
type conversationGate struct {
	mu   sync.Mutex
	held map[string]chan struct{}
}

func newConversationGate() *conversationGate {
	return &conversationGate{held: make(map[string]chan struct{})}
}

// acquire takes exclusive ownership of key. If another holder is active it
// waits up to waitBudget for the in-flight turn to finish. ok=false means the
// caller must not use the (still busy) conversation: clear the reuse and fall
// back to a fresh one. An empty key is a no-op acquire (fresh conversations
// carry a unique upstream-generated id and cannot collide).
func (g *conversationGate) acquire(ctx context.Context, key string, waitBudget time.Duration) (func(), bool) {
	if key == "" {
		return func() {}, true
	}
	if release, ok := g.tryAcquire(key); ok {
		return release, true
	}
	timer := time.NewTimer(waitBudget)
	defer timer.Stop()
	g.mu.Lock()
	ch := g.held[key]
	g.mu.Unlock()
	if ch != nil {
		select {
		case <-ch:
		case <-timer.C:
			return noRelease, false
		case <-ctx.Done():
			return noRelease, false
		}
	}
	return g.tryAcquire(key)
}

// noRelease keeps acquire's failure contract safe for callers that unconditionally
// defer the returned closure.
func noRelease() {}

func (g *conversationGate) tryAcquire(key string) (func(), bool) {
	g.mu.Lock()
	if _, busy := g.held[key]; busy {
		g.mu.Unlock()
		return noRelease, false
	}
	ch := make(chan struct{})
	g.held[key] = ch
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if cur, ok := g.held[key]; ok && cur == ch {
				delete(g.held, key)
				close(ch)
			}
			g.mu.Unlock()
		})
	}, true
}
