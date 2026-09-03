package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client is an MCP client that connects to an MCP server via HTTP SSE.
// It implements the MCP client protocol: discover tools, invoke tools.
var nextRequestID int64

type Client struct {
	serverURL  string
	httpClient *http.Client
	sessionID  string
	mu         sync.Mutex
	connected  bool
	pending    map[int64]chan json.RawMessage
	done       chan struct{}
	sseCancel  context.CancelFunc
	sseBody    io.Closer
}

// NewClient creates a new MCP client that connects to the given server URL.
func NewClient(serverURL string) *Client {
	// The JSON-RPC POST client keeps a 30s ceiling. The SSE GET must NOT use
	// it: http.Client.Timeout covers the whole response-body lifetime and
	// killed every MCP session exactly 30s after connect (the "silent stream
	// drop" symptom), so the SSE path uses its own transport with a header
	// timeout plus caller-supplied context cancellation.
	return &Client{
		serverURL:  serverURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		pending:    map[int64]chan json.RawMessage{},
		done:       make(chan struct{}),
	}
}

func (c *Client) sseHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 15 * time.Second}}
}

func (c *Client) isConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.connected = v
	c.mu.Unlock()
}

// Connect establishes the SSE connection to the MCP server and initializes the session.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	sseURL := c.serverURL
	if !strings.Contains(sseURL, "/sse") {
		sseURL = strings.TrimRight(sseURL, "/") + "/sse"
	}

	// Create the cancellation context BEFORE the request so Close() can
	// actually abort the in-flight stream, and use a transport without the
	// 30s whole-response client timeout: that timeout killed every SSE
	// session 30s after connect (silent EOF → scanner loop exits →
	// setConnected(false) with no log), which clients saw as mysterious
	// mid-task tool failures.
	sseCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(sseCtx, http.MethodGet, sseURL, nil)
	if err != nil {
		cancel()
		return fmt.Errorf("sse request: %w", err)
	}

	resp, err := c.sseHTTPClient().Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("sse connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return fmt.Errorf("sse status: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	var sessionID string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			fmt.Printf("[mcp-client] sse data: %s\n", data)
		}
		if strings.HasPrefix(line, "event: endpoint") {
			continue
		}
		if strings.Contains(line, "sessionId=") {
			data := strings.TrimPrefix(line, "data: ")
			if idx := strings.Index(data, "sessionId="); idx >= 0 {
				sessionID = data[idx+10:]
			}
			break
		}
	}

	if sessionID == "" {
		resp.Body.Close()
		cancel()
		return fmt.Errorf("no session ID received from MCP server")
	}

	c.mu.Lock()
	c.sessionID = sessionID
	c.sseCancel = cancel
	c.sseBody = resp.Body
	c.connected = true
	c.mu.Unlock()

	go c.readSSE(resp.Body)

	_, initCh, err := c.sendRequest(sseCtx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "m365-copilot2api-mcp-client", "version": "0.1.0"},
	})
	if err != nil {
		c.Close()
		return fmt.Errorf("initialize: %w", err)
	}
	select {
	case <-initCh:
	case <-time.After(10 * time.Second):
		c.Close()
		return fmt.Errorf("timeout waiting for initialize response")
	}

	_ = c.sendNotification("notifications/initialized", nil)

	return nil
}

func (c *Client) readSSE(body io.ReadCloser) {
	defer body.Close()
	defer c.setConnected(false)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var meta struct {
				ID *int64 `json:"id"`
			}
			if json.Unmarshal([]byte(data), &meta) == nil && meta.ID != nil {
				// The lookup AND the send must happen under c.mu: Close()
				// closes pending channels while holding that same lock, and
				// sending outside the lock raced with Close → "send on
				// closed channel" panic that took down the whole process
				// (this goroutine is outside the HTTP recover middleware).
				c.mu.Lock()
				if ch := c.pending[*meta.ID]; ch != nil {
					select {
					case ch <- json.RawMessage(data):
					default:
					}
				}
				c.mu.Unlock()
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[mcp-client] SSE stream ended with error: %v", err)
	}
}

// Close closes the MCP client connection.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var result struct {
		Tools []Tool `json:"tools"`
	}
	id, ch, err := c.sendRequest(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	select {
	case msg := <-ch:
		var resp struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(msg, &resp); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, err
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout waiting for tools/list response")
	}
	return result.Tools, nil
}

// CallTool invokes a tool on the MCP server.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (CallResult, error) {
	var result CallResult
	id, ch, err := c.sendRequest(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return result, err
	}
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	select {
	case msg := <-ch:
		var resp struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(msg, &resp); err != nil {
			return result, err
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return result, err
		}
	case <-ctx.Done():
		return result, ctx.Err()
	case <-time.After(30 * time.Second):
		return result, fmt.Errorf("timeout waiting for tools/call response")
	}
	return result, nil
}

func (c *Client) sendRequest(ctx context.Context, method string, params any) (int64, chan json.RawMessage, error) {
	id := atomic.AddInt64(&nextRequestID, 1)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	body, _ := json.Marshal(req)

	ch := make(chan json.RawMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	messageURL := fmt.Sprintf("%s/message?sessionId=%s", strings.TrimRight(strings.Split(c.serverURL, "/sse")[0], "/"), c.sessionID)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, messageURL, strings.NewReader(string(body)))
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return 0, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return 0, nil, fmt.Errorf("http status: %s", resp.Status)
	}
	return id, ch, nil
}

func (c *Client) sendNotification(method string, params any) error {
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	body, _ := json.Marshal(req)

	messageURL := fmt.Sprintf("%s/message?sessionId=%s", strings.TrimRight(strings.Split(c.serverURL, "/sse")[0], "/"), c.sessionID)

	httpReq, err := http.NewRequest(http.MethodPost, messageURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Close closes the MCP client connection.
func (c *Client) Close() error {
	c.mu.Lock()
	c.connected = false
	cancel := c.sseCancel
	body := c.sseBody
	c.sseCancel = nil
	c.sseBody = nil
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if body != nil {
		body.Close()
	}
	return nil
}
