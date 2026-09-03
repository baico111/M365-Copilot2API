package web

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/outbound"
)

const defaultAccountConcurrency = 8

type accountConcurrency struct {
	mu       sync.Mutex
	limit    int
	inflight map[string]int
	changed  chan struct{}
}

func newAccountConcurrency() *accountConcurrency {
	limit := defaultAccountConcurrency
	if raw := strings.TrimSpace(os.Getenv("M365_ACCOUNT_DEFAULT_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return &accountConcurrency{limit: limit, inflight: map[string]int{}, changed: make(chan struct{})}
}

func (c *accountConcurrency) Available(accountID string) bool {
	if c == nil || accountID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID] < c.limit
}

func (c *accountConcurrency) Acquire(ctx context.Context, accountID string) (func(), error) {
	if c == nil || accountID == "" {
		return func() {}, nil
	}
	for {
		c.mu.Lock()
		if c.inflight[accountID] < c.limit {
			c.inflight[accountID]++
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					if c.inflight[accountID] <= 1 {
						delete(c.inflight, accountID)
					} else {
						c.inflight[accountID]--
					}
					close(c.changed)
					c.changed = make(chan struct{})
					c.mu.Unlock()
				})
			}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (c *accountConcurrency) Snapshot() map[string]any {
	if c == nil {
		return map[string]any{"limit": defaultAccountConcurrency, "inflight": map[string]int{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	inflight := make(map[string]int, len(c.inflight))
	for accountID, count := range c.inflight {
		inflight[accountID] = count
	}
	return map[string]any{"limit": c.limit, "inflight": inflight}
}

func (c *accountConcurrency) Inflight(accountID string) int {
	if c == nil || accountID == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID]
}

func (s *Server) accountAvailable(accountID string) bool {
	if s.tokens != nil && !s.tokens.ScheduleEnabled(accountID) {
		return false
	}
	return s.accountPool.Available(accountID) && s.accountConcurrency.Available(accountID)
}

func (s *Server) accountClient(accountID string) *chathub.Client {
	// If the account has an explicitly bound proxy, use it.
	if acc, ok := s.tokens.Get(accountID); ok && acc.BoundProxy != "" {
		return s.clientForProxy(acc.BoundProxy)
	}
	// If sing-box has per-node clients, distribute accounts across nodes
	// by hashing the account ID. This ensures each account always uses
	// the same exit IP (sticky per account) while different accounts get
	// different IPs.
	//
	// NOTE: We intentionally do NOT use a connection pool when routing
	// through sing-box SOCKS5 proxies. SOCKS5 proxies (especially
	// sing-box's VLESS/VMess tunnels) may silently drop idle WebSocket
	// connections, and a pooled connection that looks alive can fail
	// mid-stream — truncating SignalR frames so tool call chunks are
	// lost, producing empty call_id / id fields and triggering
	// "Expected 'id' to be a string" errors on Codex/OpenCode clients.
	// A fresh dial per request avoids this entire class of bugs.
	if n := outbound.SingBoxNodeCount(); n > 0 {
		nodeIdx := int(stableHash(accountID) % uint64(n))
		clients := outbound.SingBoxNodeClient(nodeIdx)
		return &chathub.Client{
			HTTPHeader: s.chat.HTTPHeader,
			HTTPClient: clients.HTTP,
			Dialer:     clients.WebSocket,
			Pool:       nil, // no pool when using SOCKS5 proxy
			Trace:      s.chat.Trace,
		}
	}
	return s.chat
}

// stableHash returns a deterministic uint64 hash for a string.
// Uses FNV-1a for simplicity and good distribution.
func stableHash(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func (s *Server) chatWithAccount(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, opts ...chatCallOption) (chathub.Result, error) {
	cfg := applyChatCallOptions(opts)
	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).Chat(ctx, account, request)
	s.markAccountResult(accountID, err)

	// Transport-level retry for non-streaming requests. Since there is
	// no streaming callback, there is no risk of duplicate content.
	// Retry any transport-level error with a different account — but only
	// for context-free requests (see requestHasConversationState).
	if err != nil && IsRetryable(err) && !cfg.pinned && !requestHasConversationState(request) {
		tried := map[string]bool{accountID: true}
		for attempt := 0; attempt < maxTransportRetries; attempt++ {
			next, nerr := s.nextHealthyAccountMulti(tried)
			if nerr != nil {
				break
			}
			tried[next.ID] = true
			log.Printf("[transport-retry] chat: account %s -> %s (attempt %d) err=%v", accountID, next.ID, attempt+1, err)
			retryCtx, retryCancel := s.retryContext(ctx)
			release2, acqErr := s.accountConcurrency.Acquire(retryCtx, next.ID)
			if acqErr != nil {
				retryCancel()
				continue
			}
			if s.accountPool != nil {
				s.accountPool.MarkCall(next.ID)
			}
			nextAccount := chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}
			result2, err2 := s.accountClient(next.ID).Chat(retryCtx, nextAccount, request)
			retryCancel()
			release2()
			s.markAccountResult(next.ID, err2)
			if err2 == nil {
				if !outbound.IsProxyIsolated(err) {
					s.accountPool.MarkFailure(accountID, err, s.getRateLimitCooldown())
				}
				s.accountPool.MarkSuccess(next.ID)
				if cfg.servingAccountID != nil {
					*cfg.servingAccountID = next.ID
				}
				return result2, nil
			}
			if !outbound.IsProxyIsolated(err2) {
				s.accountPool.MarkFailure(next.ID, err2, s.getRateLimitCooldown())
			}
			if IsRetryable(err2) {
				err = err2
				continue
			}
			return result2, err2
		}
	}

	return result, err
}

// maxTransportRetries limits how many times we retry the upstream request
// with a different account when a transport-level error (proxy drop, WS
// timeout, etc.) occurs BEFORE any text has been streamed to the client.
// Once text has been streamed, we cannot retry without duplicating content,
// so the retry path only activates when streamedLen == 0.
const maxTransportRetries = 2

// chatCallOption customizes transport-level retry behavior for the
// chatWithAccount* family. Existing call sites are unaffected (defaults
// keep the historical behavior for context-free one-shot calls).
type chatCallOption func(*chatCallConfig)

type chatCallConfig struct {
	// servingAccountID, when non-nil, receives the ID of the account that
	// actually served the request after a transport-level retry switched
	// accounts. Callers that bind conversations to accounts MUST pass this
	// (via withServingAccount) and re-resolve the account when it differs
	// from the requested one — otherwise the conversation gets bound to an
	// account that never saw it and the next turn silently loses context.
	servingAccountID *string
	// pinned disables account switching entirely. Used for explicitly
	// account-bound requests where honoring the caller's account choice
	// matters more than transport resilience.
	pinned bool
}

// withServingAccount tracks the account that actually served the request.
func withServingAccount(id *string) chatCallOption {
	return func(c *chatCallConfig) { c.servingAccountID = id }
}

// withPinnedAccount forbids switching to a different account on retry.
func withPinnedAccount() chatCallOption {
	return func(c *chatCallConfig) { c.pinned = true }
}

func applyChatCallOptions(opts []chatCallOption) chatCallConfig {
	var cfg chatCallConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// requestHasConversationState reports whether the request is bound to an
// existing ChatHub conversation. Such requests must NOT switch accounts on
// transport retry: another account cannot see that conversation, so a
// switch silently loses all accumulated context. Those requests rely on
// the server-layer failover (which clears ConversationID when switching).
func requestHasConversationState(request chathub.Request) bool {
	return request.ConversationID != "" || request.SessionID != ""
}

// retryContext returns a context for a transport-retry attempt. Reusing the
// caller's context directly would inherit a mostly-consumed deadline — a
// first attempt that hung until its WS timeout leaves the retry only
// seconds to live, defeating the purpose. When the remaining budget is
// under half the configured chat timeout, derive a fresh deadline; client
// disconnects (context.Canceled) still cancel the retry, while a
// DeadlineExceeded from the exhausted first attempt does not (the retry
// deserves its own full window, matching the server-layer failover which
// always builds a fresh WithTimeout off the request context).
func (s *Server) retryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := time.Duration(s.settings.get().ChatTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 8 * time.Minute
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) >= timeout/2 {
		return ctx, func() {}
	}
	base, cancelBase := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				cancelBase() // client went away mid-retry
			}
		case <-base.Done():
		}
	}()
	timed, cancelTimed := context.WithTimeout(base, timeout)
	return timed, func() {
		cancelTimed()
		cancelBase()
	}
}

func (s *Server) chatWithAccountEvents(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error, opts ...chatCallOption) (chathub.Result, error) {
	cfg := applyChatCallOptions(opts)
	// Track whether any text has been streamed to the client. If the
	// upstream connection fails before any text flows, we can safely retry
	// with a different account (and thus a different exit IP) without
	// producing duplicate content.
	var streamedLen int64
	wrappedOnEvent := func(ev chathub.StreamEvent) error {
		if ev.Kind == "text" && ev.Text != "" {
			atomic.AddInt64(&streamedLen, int64(len(ev.Text)))
		}
		return onEvent(ev)
	}

	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).ChatWithEvents(ctx, account, request, wrappedOnEvent)
	s.markAccountResult(accountID, err)

	// Transport-level retry: if the error is retryable (proxy drop, timeout,
	// etc.) and NO text was streamed, retry with a different account.
	// This is transparent to the client because nothing was sent yet.
	// Conversation-bound requests never switch here (context loss), and
	// pinned (explicitly selected) accounts are respected.
	if err != nil && atomic.LoadInt64(&streamedLen) == 0 && IsRetryable(err) && !cfg.pinned && !requestHasConversationState(request) {
		tried := map[string]bool{accountID: true}
		for attempt := 0; attempt < maxTransportRetries; attempt++ {
			next, nerr := s.nextHealthyAccountMulti(tried)
			if nerr != nil {
				break
			}
			tried[next.ID] = true
			log.Printf("[transport-retry] events: account %s -> %s (attempt %d) err=%v", accountID, next.ID, attempt+1, err)
			retryCtx, retryCancel := s.retryContext(ctx)
			release2, acqErr := s.accountConcurrency.Acquire(retryCtx, next.ID)
			if acqErr != nil {
				retryCancel()
				continue
			}
			if s.accountPool != nil {
				s.accountPool.MarkCall(next.ID)
			}
			nextAccount := chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}
			// Reset streamedLen for the new attempt — no text was sent.
			atomic.StoreInt64(&streamedLen, 0)
			result2, err2 := s.accountClient(next.ID).ChatWithEvents(retryCtx, nextAccount, request, wrappedOnEvent)
			retryCancel()
			release2()
			s.markAccountResult(next.ID, err2)
			if err2 == nil {
				// Success: mark original account's failure (if not proxy-isolated).
				if !outbound.IsProxyIsolated(err) {
					s.accountPool.MarkFailure(accountID, err, s.getRateLimitCooldown())
				}
				s.accountPool.MarkSuccess(next.ID)
				if cfg.servingAccountID != nil {
					*cfg.servingAccountID = next.ID
				}
				return result2, nil
			}
			// This attempt also failed. If still no text streamed, try next account.
			if atomic.LoadInt64(&streamedLen) == 0 && IsRetryable(err2) {
				if !outbound.IsProxyIsolated(err2) {
					s.accountPool.MarkFailure(next.ID, err2, s.getRateLimitCooldown())
				}
				err = err2
				continue
			}
			// Text was streamed or error is not retryable — return this error.
			if !outbound.IsProxyIsolated(err2) {
				s.accountPool.MarkFailure(next.ID, err2, s.getRateLimitCooldown())
			}
			return result2, err2
		}
	}

	return result, err
}

func (s *Server) chatWithAccountReasoning(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onDelta, onReasoning func(string) error, opts ...chatCallOption) (chathub.Result, error) {
	cfg := applyChatCallOptions(opts)
	// Track whether any text/reasoning has been streamed to the client.
	// If the upstream connection fails before any text flows, we can
	// safely retry with a different account.
	var streamedLen int64
	wrappedOnDelta := func(content string) error {
		if content != "" {
			atomic.AddInt64(&streamedLen, int64(len(content)))
		}
		return onDelta(content)
	}
	wrappedOnReasoning := func(reasoning string) error {
		if reasoning != "" {
			atomic.AddInt64(&streamedLen, int64(len(reasoning)))
		}
		return onReasoning(reasoning)
	}

	release, err := s.accountConcurrency.Acquire(ctx, accountID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).ChatWithReasoning(ctx, account, request, wrappedOnDelta, wrappedOnReasoning)
	s.markAccountResult(accountID, err)

	// Transport-level retry: same logic as chatWithAccountEvents.
	if err != nil && atomic.LoadInt64(&streamedLen) == 0 && IsRetryable(err) && !cfg.pinned && !requestHasConversationState(request) {
		tried := map[string]bool{accountID: true}
		for attempt := 0; attempt < maxTransportRetries; attempt++ {
			next, nerr := s.nextHealthyAccountMulti(tried)
			if nerr != nil {
				break
			}
			tried[next.ID] = true
			log.Printf("[transport-retry] reasoning: account %s -> %s (attempt %d) err=%v", accountID, next.ID, attempt+1, err)
			retryCtx, retryCancel := s.retryContext(ctx)
			release2, acqErr := s.accountConcurrency.Acquire(retryCtx, next.ID)
			if acqErr != nil {
				retryCancel()
				continue
			}
			if s.accountPool != nil {
				s.accountPool.MarkCall(next.ID)
			}
			nextAccount := chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}
			atomic.StoreInt64(&streamedLen, 0)
			result2, err2 := s.accountClient(next.ID).ChatWithReasoning(retryCtx, nextAccount, request, wrappedOnDelta, wrappedOnReasoning)
			retryCancel()
			release2()
			s.markAccountResult(next.ID, err2)
			if err2 == nil {
				if !outbound.IsProxyIsolated(err) {
					s.accountPool.MarkFailure(accountID, err, s.getRateLimitCooldown())
				}
				s.accountPool.MarkSuccess(next.ID)
				if cfg.servingAccountID != nil {
					*cfg.servingAccountID = next.ID
				}
				return result2, nil
			}
			if atomic.LoadInt64(&streamedLen) == 0 && IsRetryable(err2) {
				if !outbound.IsProxyIsolated(err2) {
					s.accountPool.MarkFailure(next.ID, err2, s.getRateLimitCooldown())
				}
				err = err2
				continue
			}
			if !outbound.IsProxyIsolated(err2) {
				s.accountPool.MarkFailure(next.ID, err2, s.getRateLimitCooldown())
			}
			return result2, err2
		}
	}

	return result, err
}
