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
)

var (
	sbMu          sync.Mutex
	sbConfig      *SingBoxConfig
	sbProcess     *exec.Cmd
	sbClients     *Clients         // clients pointed at the local sing-box SOCKS5 (urltest / fallback)
	sbNodeClients map[int]*Clients // per-node clients: node index → Clients
	sbNodeList    []string         // node names for status reporting
	sbNodePorts   map[int]int      // node index → local SOCKS5 port
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

	// Stop existing process and clear stale clients
	sbMu.Lock()
	if sbProcess != nil && sbProcess.Process != nil {
		_ = sbProcess.Process.Signal(os.Interrupt)
		_ = sbProcess.Process.Kill()
	}
	sbProcess = nil
	sbClients = nil
	sbNodeClients = nil
	sbNodePorts = nil
	sbMu.Unlock()

	// Start sing-box
	cmd := exec.Command(cfg.BinaryPath, "run", "-c", filepath.Join(cfg.ConfigDir, "config.json"))
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

	sbMu.Lock()
	sbConfig = cfg
	sbProcess = cmd
	sbNodeList = nodeNames(selected)
	// Main clients (urltest auto-select) + per-node clients
	sbClients = buildLocalSOCKS5Clients(cfg.LocalPort)
	sbNodeClients = nodeClients
	sbNodePorts = nodePorts
	sbMu.Unlock()

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
		sbClients = nil
		sbNodeClients = nil
		sbNodePorts = nil
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
		// Start new sing-box process FIRST, then kill old one
		// This avoids a window where no proxy is available.
		reloadCmd := exec.Command(cfg.BinaryPath, "run", "-c", filepath.Join(cfg.ConfigDir, "config.json"))
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
		for i := range selected {
			port := cfg.LocalPort + 1 + i
			nodeClients[i] = buildLocalSOCKS5Clients(port)
			nodePorts[i] = port
		}
		// New process is ready — kill old one
		sbMu.Lock()
		oldCmd := sbProcess
		sbProcess = reloadCmd
		sbNodeList = nodeNames(selected)
		sbClients = buildLocalSOCKS5Clients(cfg.LocalPort)
		sbNodeClients = nodeClients
		sbNodePorts = nodePorts
		sbMu.Unlock()
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
// node, correctly handling vless, vmess, and ss (shadowsocks) protocols.
// The key issue this solves: previously all nodes were hardcoded as "vless"
// type, which caused sing-box to crash with "unknown network: ws" when the
// subscription contained ss or vmess nodes, or when the network field was
// placed at the wrong level in the config.
func buildSingBoxOutbound(tag string, n vlessNode) map[string]any {
	// Normalize the protocol: default to vless for backward compat.
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
		// ss does not use "network" or transport fields in sing-box config.
		return ob

	case "vmess":
		ob["type"] = "vmess"
		ob["uuid"] = n.UUID
		// vmess in sing-box uses "network" at the outbound level.
		ob["network"] = n.Network

	case "vless":
		ob["type"] = "vless"
		ob["uuid"] = n.UUID
		// vless in sing-box uses "network" at the outbound level.
		ob["network"] = n.Network

	default:
		// Unknown protocol — try vless as a fallback.
		ob["type"] = "vless"
		ob["uuid"] = n.UUID
		ob["network"] = n.Network
	}

	// TLS configuration (applies to vless and vmess).
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

	// WebSocket transport configuration.
	// In sing-box, when network is "ws", the ws settings go under the
	// "ws" key (not as a top-level "network" string).
	if n.Network == "ws" {
		wsConf := map[string]any{
			"path": n.Path,
		}
		if n.Host != "" {
			wsConf["host"] = n.Host
		}
		ob["ws"] = wsConf
	}

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

// ---- Public API (replaces old proxy pool) ----

// SingBoxStatus returns info about the running sing-box instance.
func SingBoxStatus() []map[string]any {
	sbMu.Lock()
	defer sbMu.Unlock()
	if sbConfig == nil {
		return []map[string]any{}
	}
	status := []map[string]any{
		{
			"subscription": sbConfig.SubscriptionURL,
			"local_port":   sbConfig.LocalPort,
			"binary":       sbConfig.BinaryPath,
			"node_count":   len(sbNodeList),
			"nodes":        sbNodeList,
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

// SingBoxNodeCount returns the number of available per-node clients.
func SingBoxNodeCount() int {
	sbMu.Lock()
	defer sbMu.Unlock()
	return len(sbNodeClients)
}

// SingBoxNodeClient returns the Clients for a specific node index.
// Falls back to the main urltest clients if the index is out of range.
func SingBoxNodeClient(index int) *Clients {
	sbMu.Lock()
	defer sbMu.Unlock()
	if c, ok := sbNodeClients[index]; ok {
		return c
	}
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
