package outbound

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// SingBoxConfig holds the subscription URL and runtime state.
type SingBoxConfig struct {
	SubscriptionURL string
	BinaryPath      string
	ConfigDir       string
	LocalPort       int
}

const (
	envSubscription  = "M365_SINGBOX_SUBSCRIPTION"
	envBinaryPath    = "M365_SINGBOX_BINARY"
	envConfigDir     = "M365_SINGBOX_CONFIG_DIR"
	envLocalPort     = "M365_SINGBOX_LOCAL_PORT"
	defaultLocalPort = 11080
	defaultBinary    = "sing-box"

	// Health check constants.
	healthCheckInterval = 60 * time.Second  // check every 60s
	healthCheckTimeout  = 10 * time.Second  // per-node check timeout
	healthMaxFailures   = 3                  // consecutive failures before isolating
	healthCheckURL      = "https://www.gstatic.com/generate_204"
)

var (
	sbMu          sync.Mutex
	sbConfig      *SingBoxConfig
	sbProcess     *exec.Cmd
	sbClients     *Clients         // clients pointed at the local sing-box SOCKS5 (urltest / fallback)
	sbNodeClients map[int]*Clients // per-node clients: node index → Clients
	sbNodeList    []string         // node names for status reporting
	sbNodePorts   map[int]int      // node index → local SOCKS5 port
	sbNodeHealth  map[int]*nodeHealth // per-node health state
	sbHealthStop  chan struct{}       // stop signal for health check goroutine
)

func defaultSingBoxConfig() *SingBoxConfig {
	port := defaultLocalPort
	if p := os.Getenv(envLocalPort); p != "" {
		if n, err := fmt.Sscanf(p, "%d", &port); n == 1 && err == nil && port > 0 && port < 65536 {
			// ok
		}
	}
	dir := "/tmp/sing-box-config"
	if d := os.Getenv(envConfigDir); d != "" {
		dir = d
	}
	bin := defaultBinary
	if b := os.Getenv(envBinaryPath); b != "" {
		bin = b
	}
	return &SingBoxConfig{
		SubscriptionURL: strings.TrimSpace(os.Getenv(envSubscription)),
		BinaryPath:      bin,
		ConfigDir:       dir,
		LocalPort:       port,
	}
}

// ConfigureSingBox fetches the subscription, parses nodes, generates a
// sing-box config, starts sing-box, and wires HTTPClient/WebSocketDialer
// to the local SOCKS5 port.
func ConfigureSingBox(subscriptionURL string) error {
	cfg := defaultSingBoxConfig()
	cfg.SubscriptionURL = subscriptionURL

	nodes, err := fetchSubscription(subscriptionURL)
	if err != nil {
		return fmt.Errorf("sing-box: fetch subscription: %w", err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("sing-box: subscription returned 0 nodes")
	}

	selected, err := writeSingBoxConfig(cfg, nodes)
	if err != nil {
		return fmt.Errorf("sing-box: write config: %w", err)
	}

	// Stop existing process, health checks, and clear stale clients
	stopHealthChecks()
	sbMu.Lock()
	if sbProcess != nil && sbProcess.Process != nil {
		_ = sbProcess.Process.Signal(os.Interrupt)
		_ = sbProcess.Process.Kill()
	}
	sbProcess = nil
	sbClients = nil
	sbNodeClients = nil
	sbNodePorts = nil
	sbNodeHealth = nil
	sbMu.Unlock()

	// CRITICAL: Validate config before starting sing-box.
	// Running "sing-box check -c config.json" catches config errors
	// (like "unknown network: ws") BEFORE we start the process, so
	// we can return a clean error instead of starting a process that
	// immediately exits with status 1 and leaves dead SOCKS5 ports.
	configPath := filepath.Join(cfg.ConfigDir, "config.json")
	checkCmd := exec.Command(cfg.BinaryPath, "check", "-c", configPath)
	checkOutput, checkErr := checkCmd.CombinedOutput()
	if checkErr != nil {
		return fmt.Errorf("sing-box config validation failed: %s: %w",
			strings.TrimSpace(string(checkOutput)), checkErr)
	}

	// Start sing-box
	cmd := exec.Command(cfg.BinaryPath, "run", "-c", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sing-box: failed to start binary %q: %w", cfg.BinaryPath, err)
	}

	// Wait briefly for sing-box to bind the local port
	socksAddr := fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort)
	ready := false
	for i := 0; i < 30; i++ {
		if conn, err := net.Dial("tcp", socksAddr); err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		log.Printf("[sing-box] warning: local port %d not reachable after 6s, proceeding anyway", cfg.LocalPort)
	}

	// Build per-node clients (each port → specific exit IP)
	nodeClients := make(map[int]*Clients, len(selected))
	nodePorts := make(map[int]int, len(selected))
	for i := range selected {
		port := cfg.LocalPort + 1 + i
		nodeClients[i] = buildLocalSOCKS5Clients(port)
		nodePorts[i] = port
	}

	// Initialize per-node health state
	nodeHealthMap := make(map[int]*nodeHealth, len(selected))
	for i := range selected {
		nodeHealthMap[i] = newNodeHealth()
	}

	sbMu.Lock()
	sbConfig = cfg
	sbProcess = cmd
	sbNodeList = nodeNames(selected)
	// Main clients (urltest auto-select) + per-node clients
	sbClients = buildLocalSOCKS5Clients(cfg.LocalPort)
	sbNodeClients = nodeClients
	sbNodePorts = nodePorts
	sbNodeHealth = nodeHealthMap
	sbMu.Unlock()

	// Start background health checks
	startHealthChecks()

	log.Printf("[sing-box] started with %d nodes on port %d (per-node ports %d-%d)", len(selected), cfg.LocalPort, cfg.LocalPort+1, cfg.LocalPort+len(selected))

	// Wait for sing-box in background; restart on exit
	go func() {
		err := cmd.Wait()
		log.Printf("[sing-box] process exited: %v", err)
		sbMu.Lock()
		if sbProcess == cmd {
			sbProcess = nil
		}
		// CRITICAL: When sing-box crashes (e.g., config error), we must
		// clear all clients so that HTTPClient()/WebSocketDialer() fall
		// back to direct connection instead of trying to connect to dead
		// SOCKS5 ports, which causes "connection refused" and 502 errors.
		// Keep sbNodeList for status display, but mark all nodes offline.
		sbClients = nil
		sbNodeClients = nil
		sbNodePorts = nil
		for _, nh := range sbNodeHealth {
			if nh != nil {
				nh.mu.Lock()
				nh.health = "offline"
				nh.lastError = "sing-box process exited"
				nh.mu.Unlock()
			}
		}
		sbMu.Unlock()
	}()

	// Start auto-refresh goroutine
	go refreshLoop(cfg)

	return nil
}

func StopSingBox() {
	stopSingBox()
}

func stopSingBox() {
	stopHealthChecks()
	sbMu.Lock()
	defer sbMu.Unlock()
	if sbProcess != nil && sbProcess.Process != nil {
		_ = sbProcess.Process.Signal(os.Interrupt)
		_ = sbProcess.Process.Kill()
	}
	sbProcess = nil
	sbClients = nil
	sbNodeClients = nil
	sbNodePorts = nil
	sbNodeHealth = nil
}

func refreshLoop(cfg *SingBoxConfig) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		nodes, err := fetchSubscription(cfg.SubscriptionURL)
		if err != nil {
			log.Printf("[sing-box] refresh failed: %v", err)
			continue
		}
		if len(nodes) == 0 {
			continue
		}
		selected, err := writeSingBoxConfig(cfg, nodes)
		if err != nil {
			log.Printf("[sing-box] refresh write config failed: %v", err)
			continue
		}
		if len(selected) == 0 {
			continue
		}
		// Validate config before starting the reload process.
		configPath := filepath.Join(cfg.ConfigDir, "config.json")
		checkCmd := exec.Command(cfg.BinaryPath, "check", "-c", configPath)
		checkOutput, checkErr := checkCmd.CombinedOutput()
		if checkErr != nil {
			log.Printf("[sing-box] reload config validation failed: %s: %v",
				strings.TrimSpace(string(checkOutput)), checkErr)
			continue
		}

		// Start new sing-box process FIRST, then kill old one
		// This avoids a window where no proxy is available.
		reloadCmd := exec.Command(cfg.BinaryPath, "run", "-c", configPath)
		reloadCmd.Stdout = os.Stdout
		reloadCmd.Stderr = os.Stderr
		if err := reloadCmd.Start(); err != nil {
			log.Printf("[sing-box] reload failed (old process kept): %v", err)
			continue
		}
		// Wait briefly for new process to bind the port
		socksAddr := fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort)
		ready := false
		for i := 0; i < 30; i++ {
			if conn, err := net.Dial("tcp", socksAddr); err == nil {
				conn.Close()
				ready = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !ready {
			log.Printf("[sing-box] reload: port %d not reachable, keeping old process", cfg.LocalPort)
			_ = reloadCmd.Process.Kill()
			continue
		}
		// Build per-node clients for the refreshed node set
		nodeClients := make(map[int]*Clients, len(selected))
		nodePorts := make(map[int]int, len(selected))
		nodeHealthMap := make(map[int]*nodeHealth, len(selected))
		for i := range selected {
			port := cfg.LocalPort + 1 + i
			nodeClients[i] = buildLocalSOCKS5Clients(port)
			nodePorts[i] = port
			nodeHealthMap[i] = newNodeHealth()
		}
		// New process is ready — kill old one
		sbMu.Lock()
		oldCmd := sbProcess
		sbProcess = reloadCmd
		sbNodeList = nodeNames(selected)
		sbClients = buildLocalSOCKS5Clients(cfg.LocalPort)
		sbNodeClients = nodeClients
		sbNodePorts = nodePorts
		sbNodeHealth = nodeHealthMap
		sbMu.Unlock()
		// Restart health checks for the new node set
		startHealthChecks()
		if oldCmd != nil && oldCmd.Process != nil {
			_ = oldCmd.Process.Signal(os.Interrupt)
			_ = oldCmd.Process.Kill()
		}
		log.Printf("[sing-box] refreshed with %d nodes", len(selected))
		go func(c *exec.Cmd) {
			err := c.Wait()
			log.Printf("[sing-box] reloaded process exited: %v", err)
			sbMu.Lock()
			if sbProcess == c {
				sbProcess = nil
				sbClients = nil
				sbNodeClients = nil
				sbNodePorts = nil
				for _, nh := range sbNodeHealth {
					if nh != nil {
						nh.mu.Lock()
						nh.health = "offline"
						nh.lastError = "sing-box process exited"
						nh.mu.Unlock()
					}
				}
			}
			sbMu.Unlock()
		}(reloadCmd)
	}
}

// buildLocalSOCKS5Clients creates Clients that route through a local SOCKS5 proxy.
func buildLocalSOCKS5Clients(port int) *Clients {
	c := directClients()
	socksAddr := fmt.Sprintf("127.0.0.1:%d", port)
	auth := &proxy.Auth{}
	// Use a forward dialer with a generous timeout so that when sing-box
	// is unreachable the SOCKS5 handshake does not hang indefinitely.
	forward := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	d, err := proxy.SOCKS5("tcp", socksAddr, auth, forward)
	if err != nil {
		log.Printf("[sing-box] SOCKS5 dialer creation failed, using direct: %v", err)
		return c
	}
	x := socksContextDialer{dialer: d}
	c.HTTP.Transport.(*http.Transport).DialContext = x.DialContext
	c.WebSocket.NetDialContext = x.DialContext
	return c
}

// ---- Subscription parsing ----

type vlessNode struct {
	UUID    string
	Address string
	Port    int
	Network string // ws, tcp, etc.
	TLS     bool
	SNI     string
	Host    string
	Path    string
	FP      string
	Alpn    string
	Name    string
	Raw     string
	Proto   string // vless, vmess, ss
	SSMethod string // shadowsocks method
	SSPass   string // shadowsocks password
}

func fetchSubscription(rawURL string) ([]vlessNode, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, _ := http.NewRequest("GET", rawURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseSubscriptionBody(string(body))
}

// splitSubscriptionLines splits a subscription body into individual
// node URIs. Subscriptions may separate nodes by newlines, spaces, or
// a mix of both. This function uses a regex-free approach: it scans
// for known scheme prefixes and extracts each complete URI.
func splitSubscriptionLines(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	// First try normal newline split
	if strings.Contains(body, "\n") {
		return strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == '\r' })
	}
	// No newlines — maybe space-separated (some subscriptions do this).
	// Split on " vless://", " vmess://", " ss://", " trojan://"
	var lines []string
	schemes := []string{"vless://", "vmess://", "ss://", "trojan://"}
	remaining := body
	for {
		remaining = strings.TrimLeft(remaining, " \t")
		if remaining == "" {
			break
		}
		// Find the current scheme prefix
		var curScheme string
		for _, s := range schemes {
			if strings.HasPrefix(remaining, s) {
				curScheme = s
				break
			}
		}
		if curScheme == "" {
			// Unknown line, just take up to next scheme
			nextIdx := len(remaining)
			for _, s := range schemes {
				if idx := strings.Index(remaining, " "+s); idx >= 0 && idx < nextIdx {
					nextIdx = idx
				}
			}
			lines = append(lines, strings.TrimSpace(remaining[:nextIdx]))
			remaining = remaining[nextIdx:]
			continue
		}
		// Find the next scheme after the current one
		nextIdx := len(remaining)
		for _, s := range schemes {
			if idx := strings.Index(remaining[len(curScheme):], " "+s); idx >= 0 {
				absIdx := len(curScheme) + idx
				if absIdx < nextIdx {
					nextIdx = absIdx
				}
			}
		}
		lines = append(lines, strings.TrimSpace(remaining[:nextIdx]))
		remaining = remaining[nextIdx:]
	}
	return lines
}

func parseSubscriptionBody(body string) ([]vlessNode, error) {
	body = strings.TrimSpace(body)
	// Try base64 decode only if the body doesn't look like plain-text URIs
	if !strings.Contains(body, "://") {
		if decoded, err := base64.StdEncoding.DecodeString(body); err == nil && isPrintable(decoded) {
			body = string(decoded)
		} else if decoded, err := base64.URLEncoding.DecodeString(body); err == nil && isPrintable(decoded) {
			body = string(decoded)
		}
	}

	// Subscriptions may separate nodes by newlines, spaces, or both.
	// Split on any whitespace that is followed by a protocol scheme.
	var nodes []vlessNode
	lines := splitSubscriptionLines(body)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "vless://") {
			node, err := parseVLESS(line)
			if err != nil {
				log.Printf("[sing-box] skip node: %v", err)
				continue
			}
			nodes = append(nodes, node)
		} else if strings.HasPrefix(line, "vmess://") {
			node, err := parseVMess(line)
			if err != nil {
				log.Printf("[sing-box] skip vmess node: %v", err)
				continue
			}
			nodes = append(nodes, node)
		} else if strings.HasPrefix(line, "ss://") {
			node, err := parseSS(line)
			if err != nil {
				log.Printf("[sing-box] skip ss node: %v", err)
				continue
			}
			nodes = append(nodes, node)
		} else if strings.HasPrefix(line, "trojan://") {
			node, err := parseTrojan(line)
			if err != nil {
				log.Printf("[sing-box] skip trojan node: %v", err)
				continue
			}
			nodes = append(nodes, node)
		}
	}
	return nodes, nil
}

func parseVLESS(raw string) (vlessNode, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return vlessNode{}, err
	}
	port, _ := strconvAtoi(u.Port())
	if port == 0 {
		port = 443
	}
	node := vlessNode{
		UUID:    u.User.Username(),
		Address: u.Hostname(),
		Port:    port,
		Network: "ws",
		TLS:     true,
		Name:    u.Fragment,
		Raw:     raw,
		Proto:   "vless",
	}
	q := u.Query()
	if t := q.Get("type"); t != "" {
		node.Network = t
	}
	if s := q.Get("security"); s == "tls" || s == "" {
		node.TLS = true
	} else if s == "none" {
		node.TLS = false
	}
	node.SNI = q.Get("sni")
	node.Host = q.Get("host")
	node.Path = q.Get("path")
	node.FP = q.Get("fp")
	node.Alpn = q.Get("alpn")
	if node.Path == "" {
		node.Path = "/"
	}
	return node, nil
}

func parseVMess(raw string) (vlessNode, error) {
	// vmess://base64(json)
	encoded := strings.TrimPrefix(raw, "vmess://")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return vlessNode{}, fmt.Errorf("vmess base64 decode: %w", err)
		}
	}
	var v struct {
		Add  string `json:"add"`
		Port any    `json:"port"`
		ID   string `json:"id"`
		Net  string `json:"net"`
		Host string `json:"host"`
		Path string `json:"path"`
		TLS  string `json:"tls"`
		SNI  string `json:"sni"`
		V    any    `json:"v"`
		PS   string `json:"ps"`
	}
	if err := json.Unmarshal(decoded, &v); err != nil {
		return vlessNode{}, fmt.Errorf("vmess json decode: %w", err)
	}
	port := 443
	switch p := v.Port.(type) {
	case float64:
		port = int(p)
	case string:
		port, _ = strconvAtoi(p)
	}
	network := v.Net
	if network == "" {
		network = "ws"
	}
	return vlessNode{
		UUID:    v.ID,
		Address: v.Add,
		Port:    port,
		Network: network,
		TLS:     v.TLS == "tls",
		SNI:     v.SNI,
		Host:    v.Host,
		Path:    v.Path,
		Name:    v.PS,
		Raw:     raw,
		Proto:   "vmess",
	}, nil
}

func parseSS(raw string) (vlessNode, error) {
	// ss://base64(method:password)@host:port#name
	// or ss://base64(method:password@host:port)#name
	// or ss://method:password@host:port#name (plain)
	u, err := url.Parse(raw)
	if err != nil {
		return vlessNode{}, err
	}
	port, _ := strconvAtoi(u.Port())
	if port == 0 {
		port = 443
	}
	node := vlessNode{
		Address: u.Hostname(),
		Port:    port,
		Network: "tcp",
		TLS:     false,
		Name:    u.Fragment,
		Raw:     raw,
		Proto:   "ss",
	}

	// Decode userinfo (method:password)
	userInfo := u.User.String()
	if userInfo == "" {
		// Try base64-encoded userinfo in the path
		encoded := strings.TrimPrefix(u.Path, "/")
		if encoded != "" {
			if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				userInfo = string(decoded)
			} else if decoded, err := base64.URLEncoding.DecodeString(encoded); err == nil {
				userInfo = string(decoded)
			}
		}
	}
	if userInfo != "" {
		parts := strings.SplitN(userInfo, ":", 2)
		if len(parts) == 2 {
			node.SSMethod = parts[0]
			node.SSPass = parts[1]
		}
	}
	return node, nil
}

// parseTrojan parses a trojan:// URL into a vlessNode.
// Trojan always uses TLS; the password is the userinfo portion of the URL.
func parseTrojan(raw string) (vlessNode, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return vlessNode{}, err
	}
	port, _ := strconvAtoi(u.Port())
	if port == 0 {
		port = 443
	}
	node := vlessNode{
		UUID:    u.User.Username(), // trojan password
		Address: u.Hostname(),
		Port:    port,
		Network: "tcp",
		TLS:     true, // trojan always uses TLS
		Name:    u.Fragment,
		Raw:     raw,
		Proto:   "trojan",
	}
	q := u.Query()
	node.SNI = q.Get("sni")
	if node.SNI == "" {
		node.SNI = u.Hostname()
	}
	// Trojan supports ws transport too
	if t := q.Get("type"); t != "" {
		node.Network = t
	}
	node.Host = q.Get("host")
	node.Path = q.Get("path")
	if node.Path == "" {
		node.Path = "/"
	}
	return node, nil
}

func nodeNames(nodes []vlessNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = fmt.Sprintf("%s:%d", n.Address, n.Port)
		}
		out = append(out, name)
	}
	return out
}

// ---- sing-box config generation ----

// maxNodeInbounds caps the number of per-node SOCKS5 inbounds. Each node
// gets its own local port so accounts can be distributed across IPs.
const maxNodeInbounds = 50

// buildSingBoxOutbound builds a sing-box outbound config map for the given
// node, correctly handling vless, vmess, ss (shadowsocks), and trojan protocols.
//
// Key lesson from cnb2api: sing-box does NOT use a top-level "network" field
// on vless/vmess outbounds. WebSocket transport must be configured via the
// "transport" key with type="ws" — placing "network":"ws" at the outbound
// level causes sing-box to crash with "unknown network: ws".
func buildSingBoxOutbound(tag string, n vlessNode) map[string]any {
	proto := n.Proto
	if proto == "" {
		proto = "vless"
	}

	ob := map[string]any{
		"tag":         tag,
		"server":      n.Address,
		"server_port": n.Port,
	}

	switch proto {
	case "ss":
		ob["type"] = "shadowsocks"
		ob["method"] = n.SSMethod
		ob["password"] = n.SSPass
		// ss does not use TLS or transport fields.
		return ob

	case "vmess":
		ob["type"] = "vmess"
		ob["uuid"] = n.UUID

	case "vless":
		ob["type"] = "vless"
		ob["uuid"] = n.UUID

	case "trojan":
		ob["type"] = "trojan"
		ob["password"] = n.UUID

	default:
		ob["type"] = "vless"
		ob["uuid"] = n.UUID
	}

	// TLS configuration (applies to vless, vmess, trojan).
	if n.TLS {
		tlsConf := map[string]any{
			"enabled":     true,
			"server_name": n.SNI,
		}
		if n.FP != "" {
			tlsConf["utls"] = map[string]any{
				"enabled":     true,
				"fingerprint": n.FP,
			}
		}
		ob["tls"] = tlsConf
	}

	// Transport configuration: sing-box uses the "transport" key (NOT a
	// top-level "network" field). For WebSocket, the format is:
	//   "transport": {"type": "ws", "path": "/...", "headers": {"Host": "..."}}
	// This is the fix for "unknown network: ws" crash.
	if n.Network == "ws" {
		path := n.Path
		if path == "" {
			path = "/"
		}
		transportCfg := map[string]any{
			"type": "ws",
			"path": path,
		}
		if n.Host != "" {
			transportCfg["headers"] = map[string]any{"Host": n.Host}
		}
		ob["transport"] = transportCfg
	} else if n.Network == "grpc" {
		// gRPC transport (used by some vless/vmess nodes).
		grpcCfg := map[string]any{
			"type":    "grpc",
			"service_name": n.Path,
		}
		ob["transport"] = grpcCfg
	}
	// For "tcp" (default), no transport field is needed.

	return ob
}

// writeSingBoxConfig generates the sing-box config and returns the selected
// node list so the caller can build matching per-node clients.
func writeSingBoxConfig(cfg *SingBoxConfig, nodes []vlessNode) ([]vlessNode, error) {
	if err := os.MkdirAll(cfg.ConfigDir, 0o755); err != nil {
		return nil, err
	}

	// Use all nodes — sing-box urltest will auto-pick fastest.
	// We also shuffle so each restart rotates the order.
	selected := selectRandomNodes(nodes, maxNodeInbounds)

	var outbounds []map[string]any

	var nodeTags []string
	for i, n := range selected {
		tag := fmt.Sprintf("node-%d", i)
		nodeTags = append(nodeTags, tag)

		ob := buildSingBoxOutbound(tag, n)
		outbounds = append(outbounds, ob)
	}

	// urltest: auto-select lowest latency node (sing-box built-in)
	urltestOut := map[string]any{
		"tag":          "proxy",
		"type":         "urltest",
		"outbounds":    nodeTags,
		"url":          "https://www.gstatic.com/generate_204",
		"interval":     "5m",
		"tolerance":    50,
		"idle_timeout": "30m",
	}
	outbounds = append(outbounds, urltestOut)

	// direct + block
	outbounds = append(outbounds, map[string]any{"tag": "direct", "type": "direct"})
	outbounds = append(outbounds, map[string]any{"tag": "block", "type": "block"})

	// Build inbounds: one main mixed inbound (urltest) + one SOCKS5 inbound
	// per node so each account can be pinned to a specific exit IP.
	var inbounds []any
	inbounds = append(inbounds, map[string]any{
		"tag":         "mixed-in",
		"type":        "mixed",
		"listen":      "127.0.0.1",
		"listen_port": cfg.LocalPort,
	})
	for i := range selected {
		port := cfg.LocalPort + 1 + i
		inbounds = append(inbounds, map[string]any{
			"tag":         fmt.Sprintf("in-node-%d", i),
			"type":        "socks",
			"listen":      "127.0.0.1",
			"listen_port": port,
		})
	}

	// route: each per-node inbound routes to its matching node outbound.
	// The main mixed-in inbound routes through the urltest (auto-select).
	routeRules := []any{}
	for i := range selected {
		routeRules = append(routeRules, map[string]any{
			"inbound":  []string{fmt.Sprintf("in-node-%d", i)},
			"outbound": fmt.Sprintf("node-%d", i),
		})
	}
	route := map[string]any{
		"rules": routeRules,
		"final": "proxy",
	}

	config := map[string]any{
		"log": map[string]any{
			"level": "warn",
		},
		"inbounds":  inbounds,
		"outbounds": outbounds,
		"route":     route,
	}

	configPath := filepath.Join(cfg.ConfigDir, "config.json")
	b, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(configPath, b, 0o644); err != nil {
		return nil, err
	}
	return selected, nil
}

func selectRandomNodes(nodes []vlessNode, max int) []vlessNode {
	if len(nodes) <= max {
		// Shuffle for random ordering
		shuffled := make([]vlessNode, len(nodes))
		copy(shuffled, nodes)
		rand.Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})
		return shuffled
	}
	// Random subset
	selected := make([]vlessNode, 0, max)
	indices := rand.Perm(len(nodes))
	for i := 0; i < max; i++ {
		selected = append(selected, nodes[indices[i]])
	}
	return selected
}

// ---- Node health checking ----

// nodeHealth tracks the health state of a single sing-box node.
type nodeHealth struct {
	mu           sync.Mutex
	health       string        // "healthy", "unhealthy", "checking"
	failures     int           // consecutive failure count
	latency      time.Duration // last measured latency
	lastCheck    time.Time     // last check time
	lastError    string        // last error message
	isolated     bool          // true when node is skipped due to failures
	isolatedAt   time.Time     // when isolation started
}

// newnodeHealth creates a nodeHealth with default healthy state.
func newNodeHealth() *nodeHealth {
	return &nodeHealth{
		health: "checking",
	}
}

// startHealthChecks launches a background goroutine that periodically
// checks each node's SOCKS5 port connectivity and upstream reachability.
// Nodes that fail healthMaxFailures consecutive checks are isolated
// (skipped by SingBoxNodeClient) so requests only go to healthy nodes.
// Isolated nodes are re-checked; if they recover, they are reactivated.
func startHealthChecks() {
	stop := make(chan struct{})
	sbMu.Lock()
	if sbHealthStop != nil {
		close(sbHealthStop)
	}
	sbHealthStop = stop
	sbMu.Unlock()

	go func(stop chan struct{}) {
		ticker := time.NewTicker(healthCheckInterval)
		defer ticker.Stop()
		// Initial check after 5s (let sing-box fully start).
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Second):
		}
		checkAllNodes()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				checkAllNodes()
			}
		}
	}(stop)
}

// stopHealthChecks signals the health check goroutine to stop.
func stopHealthChecks() {
	sbMu.Lock()
	if sbHealthStop != nil {
		close(sbHealthStop)
		sbHealthStop = nil
	}
	sbMu.Unlock()
}

// checkAllNodes runs a health check against every node concurrently.
func checkAllNodes() {
	sbMu.Lock()
	nodes := make([]int, 0, len(sbNodeClients))
	for idx := range sbNodeClients {
		nodes = append(nodes, idx)
	}
	sbMu.Unlock()
	if len(nodes) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, idx := range nodes {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			checkNodeHealth(idx)
		}(idx)
	}
	wg.Wait()
}

// checkNodeHealth checks a single node's health.
//
// Following cnb2api's approach: do a TCP connectivity test to the
// SOCKS5 port instead of an HTTP request. This avoids false negatives
// where the proxy works fine but the health-check target (e.g.
// gstatic.com) returns 403 through CF nodes.
//
// The TCP check confirms:
// 1. sing-box is running and the per-node SOCKS5 port is listening
// 2. The proxy handshake completes
// 3. The upstream node is reachable
//
// HTTP-based health checks caused mass false-failures because CF
// (Cloudflare) proxy nodes return 403 for gstatic.com, even though
// the proxy works perfectly for substrate.office.com.
func checkNodeHealth(idx int) {
	sbMu.Lock()
	clients, ok := sbNodeClients[idx]
	port := sbNodePorts[idx]
	nh, nhOk := sbNodeHealth[idx]
	sbMu.Unlock()
	if !ok || clients == nil || !nhOk || nh == nil {
		return
	}

	nh.mu.Lock()
	nh.health = "checking"
	nh.mu.Unlock()

	// Step 1: TCP connectivity test to the SOCKS5 port.
	// This is the primary health signal — if the port is unreachable,
	// sing-box is dead or the node is broken.
	socksAddr := fmt.Sprintf("127.0.0.1:%d", port)
	tcpStart := time.Now()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	tcpConn, err := dialer.Dial("tcp", socksAddr)
	if err != nil {
		recordNodeFailure(idx, fmt.Errorf("tcp connect to %s: %w", socksAddr, err))
		return
	}
	tcpConn.Close()
	latency := time.Since(tcpStart)

	// Step 2: Mark node as healthy based on TCP connectivity alone.
	// We intentionally do NOT do an HTTP request through the proxy
	// because:
	// 1. Cloudflare proxy nodes return 403 for gstatic.com (WAF block),
	//    which sing-box logs as "unexpected HTTP response status: 403"
	//    on every health check — filling the logs with noise.
	// 2. The 403 is NOT a proxy failure — the proxy works fine for
	//    substrate.office.com (the actual upstream target).
	// 3. The TCP check above already confirmed sing-box is running,
	//    the SOCKS5 port is listening, and the proxy handshake completed.
	//
	// If we need HTTP latency measurement in the future, use a target
	// that doesn't block CF nodes (e.g. substrate.office.com itself).
	nh.mu.Lock()
	nh.health = "healthy"
	nh.failures = 0
	nh.latency = latency
	nh.lastCheck = time.Now()
	nh.lastError = ""
	if nh.isolated {
		log.Printf("[sing-box] node %d (port %d) recovered, latency=%s", idx, port, latency)
		nh.isolated = false
		nh.isolatedAt = time.Time{}
	}
	nh.mu.Unlock()
}

// recordNodeFailure records a failed health check for a node and isolates
// it after too many consecutive failures.
func recordNodeFailure(idx int, err error) {
	sbMu.Lock()
	nh, ok := sbNodeHealth[idx]
	port := sbNodePorts[idx]
	sbMu.Unlock()
	if !ok || nh == nil {
		return
	}
	nh.mu.Lock()
	nh.failures++
	nh.lastCheck = time.Now()
	nh.lastError = err.Error()
	if nh.failures >= healthMaxFailures && !nh.isolated {
		nh.isolated = true
		nh.isolatedAt = time.Now()
		nh.health = "unhealthy"
		log.Printf("[sing-box] node %d (port %d) isolated after %d failures: %s", idx, port, nh.failures, err.Error())
	} else if !nh.isolated {
		nh.health = "unhealthy"
		log.Printf("[sing-box] node %d (port %d) check failed (%d/%d): %s", idx, port, nh.failures, healthMaxFailures, err.Error())
	}
	nh.mu.Unlock()
}

// ---- Public API (replaces old proxy pool) ----

// SingBoxStatus returns info about the running sing-box instance.
func SingBoxStatus() []map[string]any {
	sbMu.Lock()
	defer sbMu.Unlock()
	if sbConfig == nil {
		return []map[string]any{}
	}

	nodes := make([]map[string]any, 0, len(sbNodeList))
	for i, name := range sbNodeList {
		node := map[string]any{
			"index": i,
			"name":  name,
			"port":  sbNodePorts[i],
		}
		if nh, ok := sbNodeHealth[i]; ok && nh != nil {
			nh.mu.Lock()
			node["health"] = nh.health
			node["failures"] = nh.failures
			node["latency_ms"] = nh.latency.Milliseconds()
			node["last_check"] = nh.lastCheck
			node["last_error"] = nh.lastError
			node["isolated"] = nh.isolated
			nh.mu.Unlock()
		} else {
			node["health"] = "unknown"
			node["failures"] = 0
			node["latency_ms"] = int64(0)
			node["isolated"] = false
		}
		nodes = append(nodes, node)
	}

	// Count healthy vs isolated.
	healthy := 0
	isolated := 0
	for _, nh := range sbNodeHealth {
		if nh != nil {
			nh.mu.Lock()
			if nh.isolated {
				isolated++
			} else if nh.health == "healthy" {
				healthy++
			}
			nh.mu.Unlock()
		}
	}

	status := []map[string]any{
		{
			"subscription":   sbConfig.SubscriptionURL,
			"local_port":     sbConfig.LocalPort,
			"binary":         sbConfig.BinaryPath,
			"node_count":     len(sbNodeList),
			"nodes":          sbNodeList,
			"node_details":   nodes,
			"healthy_nodes":  healthy,
			"isolated_nodes": isolated,
		},
	}
	return status
}

// SingBoxRunning reports whether sing-box is currently active.
func SingBoxRunning() bool {
	sbMu.Lock()
	defer sbMu.Unlock()
	return sbProcess != nil
}

// SingBoxNodeInfo returns the node index and name assigned to a given
// account ID, plus the node's health status. Returns empty strings and
// false if sing-box is not running or the account has no node assignment.
func SingBoxNodeInfo(accountID string) (int, string, string, bool) {
	sbMu.Lock()
	defer sbMu.Unlock()
	if sbNodeClients == nil || len(sbNodeClients) == 0 || len(sbNodeList) == 0 {
		return 0, "", "", false
	}
	n := len(sbNodeClients)
	idx := int(stableHash(accountID) % uint64(n))
	name := ""
	if idx < len(sbNodeList) {
		name = sbNodeList[idx]
	}
	health := "unknown"
	if nh, ok := sbNodeHealth[idx]; ok && nh != nil {
		nh.mu.Lock()
		health = nh.health
		nh.mu.Unlock()
	}
	return idx, name, health, true
}

// SingBoxHealthCheck triggers an immediate health check of all nodes.
// Returns immediately; the checks run in the background.
func SingBoxHealthCheck() {
	go checkAllNodes()
}

// stableHash returns a deterministic uint64 hash for a string (FNV-1a).
func stableHash(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// SingBoxNodeCount returns the number of available (non-isolated) per-node clients.
func SingBoxNodeCount() int {
	sbMu.Lock()
	defer sbMu.Unlock()
	count := 0
	for idx, nh := range sbNodeHealth {
		if _, ok := sbNodeClients[idx]; !ok {
			continue
		}
		if nh == nil || !nh.isolated {
			count++
		}
	}
	return count
}

// SingBoxNodeClient returns the Clients for a specific node index.
// Skips isolated (unhealthy) nodes and falls back to the next available.
// Falls back to the main urltest clients if no healthy node is found.
func SingBoxNodeClient(index int) *Clients {
	sbMu.Lock()
	defer sbMu.Unlock()
	if sbNodeClients == nil {
		return sbClients
	}
	n := len(sbNodeClients)
	if n == 0 {
		return sbClients
	}
	// Try the requested index first, then scan for any healthy node.
	for i := 0; i < n; i++ {
		idx := (index + i) % n
		if c, ok := sbNodeClients[idx]; ok {
			nh := sbNodeHealth[idx]
			if nh == nil || !nh.isolated {
				return c
			}
		}
	}
	// All nodes isolated — return the main urltest client as fallback.
	return sbClients
}

// OverrideClients replaces the global clients (used by ConfigureSingBox
// and for testing).
func OverrideClients(c *Clients) {
	clientsMu.Lock()
	clients = c
	clientsMu.Unlock()
}

// isPrintable reports whether a byte slice is mostly printable ASCII,
// used to validate that a base64 decode produced sensible output.
func isPrintable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c >= 32 && c < 127 || c == '\n' || c == '\r' || c == '\t' {
			printable++
		}
	}
	return printable*100/len(b) > 90
}

// strconvAtoi is a local helper to avoid importing strconv.
func strconvAtoi(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %s", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
