package outbound

import (
	"context"
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
	"sort"
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
	healthCheckInterval = 60 * time.Second // check every 60s
	healthCheckTimeout  = 8 * time.Second  // per-node egress probe timeout
	healthMaxFailures   = 3                // consecutive soft failures before banning
	// restartCooldown is the minimum spacing between sing-box restarts so a
	// crash loop cannot spawn processes in a hot loop.
	restartCooldown = 10 * time.Second
	// healthProbeDefault is the upstream the gateway actually dials. Probing
	// the real M365 egress host (instead of gstatic) means an HTTP response
	// of ANY status < 500 proves the TCP+TLS+proxy path to Microsoft works,
	// while 403/429 from the CDN means this exit IP is blocked.
	healthProbeDefault = "https://substrate.office.com/"
)

// healthProbeURL returns the egress probe target, overridable by operators.
func healthProbeURL() string {
	if v := strings.TrimSpace(os.Getenv("M365_PROXY_HEALTH_URL")); v != "" {
		return v
	}
	return healthProbeDefault
}

var (
	sbMu          sync.Mutex
	sbLifecycleMu sync.Mutex // serialises all process start/stop (configure/refresh/replace/stop)
	sbConfig      *SingBoxConfig
	sbProcess     *exec.Cmd
	sbClients     *Clients            // clients pointed at the local sing-box SOCKS5 (urltest / fallback)
	sbNodeClients map[int]*Clients    // per-node clients: node index → Clients
	sbNodeList    []string            // node names for status reporting
	sbNodePorts   map[int]int         // node index → local SOCKS5 port
	sbNodeHealth  map[int]*nodeHealth // per-node health state
	sbHealthStop  chan struct{}       // stop signal for health check goroutine
	sbRefreshStop chan struct{}       // stop signal for the auto-refresh goroutine
	sbLastStart   time.Time           // last sing-box start (restart debounce)
	// sbReplacing is the coalescing latch for replacement restarts: while a
	// replace is scheduled or in flight, further bans fold into it instead of
	// stacking additional subscription fetches + process swaps. Guarded by sbMu.
	sbReplacing bool

	// bannedEgresses persists banned upstream exit addresses across sing-box
	// restarts so the same dead IP is never re-selected for a port.
	sbBannedEgresses map[string]bool
	// sbEgressAddrs maps node index → upstream Address, used to record a ban
	// against the address rather than the volatile port/index.
	sbEgressAddrs map[int]string
)

// onNodeBannedFn is the ban联动 callback (set via SetOnNodeBanned). It is
// invoked asynchronously with the banned node index and reason so callers can
// rebind accounts/credentials that were pinned to the dead egress.
var (
	onNodeBannedMu sync.Mutex
	onNodeBannedFn func(idx int, reason string)
)

// SetOnNodeBanned registers the ban联动 callback.
func SetOnNodeBanned(fn func(idx int, reason string)) {
	onNodeBannedMu.Lock()
	onNodeBannedFn = fn
	onNodeBannedMu.Unlock()
}

func defaultSingBoxConfig() *SingBoxConfig {
	port := defaultLocalPort
	if p := os.Getenv(envLocalPort); p != "" {
		if n, err := fmt.Sscanf(p, "%d", &port); n == 1 && err == nil && port > 0 && port < 65536 {
			// ok
		}
	}
	dir := os.Getenv(envConfigDir)
	if dir == "" {
		// Per-instance private temp dir: a fixed /tmp path plus world-readable
		// config would expose plaintext proxy credentials.
		if d, err := os.MkdirTemp("", "singbox-"); err == nil {
			dir = d
		} else {
			dir = filepath.Join(os.TempDir(), fmt.Sprintf("sing-box-config-%d", time.Now().UnixNano()))
		}
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
// to the local SOCKS5 port. It serialises against every other process
// lifecycle operation (refresh, replacement, stop) so concurrent callers can
// never fight over the shared local port.
func ConfigureSingBox(subscriptionURL string) error {
	sbLifecycleMu.Lock()
	defer sbLifecycleMu.Unlock()
	return configureSingBoxLocked(subscriptionURL)
}

func configureSingBoxLocked(subscriptionURL string) error {
	cfg := defaultSingBoxConfig()
	cfg.SubscriptionURL = subscriptionURL

	nodes, err := fetchSubscription(subscriptionURL)
	if err != nil {
		return fmt.Errorf("sing-box: fetch subscription: %w", err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("sing-box: subscription returned 0 nodes")
	}

	selected, err := writeSingBoxConfig(cfg, nodes, nil)
	if err != nil {
		return fmt.Errorf("sing-box: write config: %w", err)
	}

	// Stop existing process, health checks, and clear stale clients
	stopHealthChecks()
	sbMu.Lock()
	oldProc := sbProcess
	sbProcess = nil
	sbClients = nil
	sbNodeClients = nil
	sbNodePorts = nil
	sbNodeHealth = nil
	sbBannedEgresses = make(map[string]bool)
	sbEgressAddrs = make(map[int]string)
	sbLastStart = time.Now()
	sbMu.Unlock()
	if oldProc != nil && oldProc.Process != nil {
		_ = oldProc.Process.Signal(os.Interrupt)
		_ = oldProc.Process.Kill()
	}
	// The previous implementation did not wait for the old process to exit.
	// When sing-box is reconfigured while a previous instance is still
	// winding down, the fresh instance fails to bind the shared local port
	// (EADDRINUSE) and the whole proxy silently stays offline. Release it.
	waitPortReleased(fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort), 3*time.Second)

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
	egressAddrs := make(map[int]string, len(selected))
	for i := range selected {
		port := cfg.LocalPort + 1 + i
		nodeClients[i] = buildLocalSOCKS5Clients(port)
		nodePorts[i] = port
		egressAddrs[i] = selected[i].Address
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
	sbEgressAddrs = egressAddrs
	sbMu.Unlock()

	// Start background health checks
	startHealthChecks()

	log.Printf("[sing-box] started with %d nodes on port %d (per-node ports %d-%d)", len(selected), cfg.LocalPort, cfg.LocalPort+1, cfg.LocalPort+len(selected))

	// Wait for sing-box in background; restart on exit
	go watchSingBoxProcess(cmd)

	// Start auto-refresh goroutine (stops the previous one so repeated
	// reconfigures do not leak refresh loops).
	startRefreshLoop(cfg)

	return nil
}

// watchSingBoxProcess waits for a sing-box process to exit and, if it was the
// current one, clears the client/health state (so callers fall back instead of
// dialing dead SOCKS5 ports) and triggers a debounced crash restart. It is the
// single watcher used by configure/reload/rebuild so crash handling is uniform.
func watchSingBoxProcess(cmd *exec.Cmd) {
	err := cmd.Wait()
	log.Printf("[sing-box] process exited: %v", err)
	sbMu.Lock()
	isCurrent := sbProcess == cmd
	if isCurrent {
		sbProcess = nil
		// CRITICAL: When sing-box crashes (e.g., config error), we must clear
		// all clients so HTTPClient()/WebSocketDialer() fall back to direct
		// connection instead of dialing dead SOCKS5 ports. Keep sbNodeList
		// for status display, but mark all nodes offline.
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
	if isCurrent {
		tryRestartSingBox()
	}
}

// startRefreshLoop (re)starts the 10-minute subscription refresh goroutine,
// signalling any previous loop to exit first.
func startRefreshLoop(cfg *SingBoxConfig) {
	stop := make(chan struct{})
	sbMu.Lock()
	if sbRefreshStop != nil {
		close(sbRefreshStop)
	}
	sbRefreshStop = stop
	sbMu.Unlock()
	go refreshLoop(cfg, stop)
}

// waitPortReleased polls until nothing listens on addr or timeout passes.
// The check runs on localhost only, so dial latency is negligible; a refused
// dial means the kernel has torn the listener down (or it never existed).
func waitPortReleased(addr string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
}

// stopProcessAndWaitPort signals a sing-box process to stop, force-kills it
// after a grace period, and then waits until its listening port is released.
// It deliberately does NOT call cmd.Wait(): the process always has a dedicated
// watchSingBoxProcess goroutine that owns Wait(), and a second concurrent
// Wait() on the same *exec.Cmd races on ProcessState. Port release is a
// sufficient signal that the listener is gone.
func stopProcessAndWaitPort(cmd *exec.Cmd, socksAddr string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	// Give graceful shutdown a moment, then force-kill.
	time.Sleep(150 * time.Millisecond)
	_ = cmd.Process.Kill()
	// Wait for the port to be released (bounded).
	waitPortReleased(socksAddr, 4*time.Second)
}

func StopSingBox() {
	stopSingBox()
}

func stopSingBox() {
	sbLifecycleMu.Lock()
	defer sbLifecycleMu.Unlock()
	stopHealthChecks()
	sbMu.Lock()
	if sbRefreshStop != nil {
		close(sbRefreshStop)
		sbRefreshStop = nil
	}
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
	sbEgressAddrs = nil
	// Clear the replacement latch: a pending replace would otherwise block all
	// future replacement restarts after this stop.
	sbReplacing = false
	// Remove the per-instance config dir (Start recreates it): it holds
	// plaintext proxy credentials.
	if sbConfig != nil && sbConfig.ConfigDir != "" {
		_ = os.RemoveAll(sbConfig.ConfigDir)
	}
}

// tryRestartSingBox reconfigures sing-box from the last subscription after a
// crash, respecting restartCooldown so a crash loop cannot spawn processes in a
// hot loop. It is a no-op when no subscription is known.
func tryRestartSingBox() {
	sbMu.Lock()
	cfg := sbConfig
	lastStart := sbLastStart
	sbMu.Unlock()
	if cfg == nil || cfg.SubscriptionURL == "" {
		return
	}
	if !lastStart.IsZero() && time.Since(lastStart) < restartCooldown {
		log.Printf("[sing-box] skip crash restart (debounce: %s since last start)", time.Since(lastStart).Round(time.Millisecond))
		return
	}
	sbLifecycleMu.Lock()
	defer sbLifecycleMu.Unlock()
	log.Printf("[sing-box] attempting crash restart from subscription")
	if err := configureSingBoxLocked(cfg.SubscriptionURL); err != nil {
		log.Printf("[sing-box] crash restart failed: %v", err)
	}
}

func refreshLoop(cfg *SingBoxConfig, stop chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		nodes, err := fetchSubscription(cfg.SubscriptionURL)
		if err != nil {
			log.Printf("[sing-box] refresh failed: %v", err)
			continue
		}
		if len(nodes) == 0 {
			continue
		}
		// Serialise against replacement/configure: only one process swap may run
		// at a time or they fight over the shared local port.
		sbLifecycleMu.Lock()
		reloadOnce(cfg, nodes)
		sbLifecycleMu.Unlock()
	}
}

// reloadOnce performs a single subscription refresh reload. It assumes the
// caller holds the sbReplacing latch.
func reloadOnce(cfg *SingBoxConfig, nodes []vlessNode) {
	banned := sbBannedSnapshot()
	selected, err := writeSingBoxConfig(cfg, nodes, banned)
	if err != nil {
		log.Printf("[sing-box] refresh write config failed: %v", err)
		return
	}
	if len(selected) == 0 {
		return
	}
	// Validate config before starting the reload process.
	configPath := filepath.Join(cfg.ConfigDir, "config.json")
	checkCmd := exec.Command(cfg.BinaryPath, "check", "-c", configPath)
	checkOutput, checkErr := checkCmd.CombinedOutput()
	if checkErr != nil {
		log.Printf("[sing-box] reload config validation failed: %s: %v",
			strings.TrimSpace(string(checkOutput)), checkErr)
		return
	}

	// The old code started the new process BEFORE killing the old one
	// "to avoid a gap" — but both bind the same LocalPort, so the new
	// process always died on EADDRINUSE while the readiness probe
	// happily connected to the OLD listener. The swap then killed the
	// old owner and left sbClients pointing at a dead socket until the
	// next 10-minute refresh: a guaranteed proxy-wide outage window.
	// Correct order: stop old -> confirm the port is released -> start
	// new -> verify it actually owns the port -> swap in clients.
	socksAddr := fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort)
	sbMu.Lock()
	oldCmd := sbProcess
	sbProcess = nil
	sbClients = nil
	sbNodeClients = nil
	sbNodePorts = nil
	sbMu.Unlock()
	stopProcessAndWaitPort(oldCmd, socksAddr)
	// Wait until nothing is listening on the port any more.
	released := false
	for i := 0; i < 40; i++ {
		conn, derr := net.Dial("tcp", socksAddr)
		if derr != nil {
			released = true
			break
		}
		conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if !released {
		log.Printf("[sing-box] reload: port %d still busy after old process stop; skipping this refresh", cfg.LocalPort)
		return
	}
	reloadCmd := exec.Command(cfg.BinaryPath, "run", "-c", configPath)
	reloadCmd.Stdout = os.Stdout
	reloadCmd.Stderr = os.Stderr
	if err := reloadCmd.Start(); err != nil {
		log.Printf("[sing-box] reload start failed (proxy left offline): %v", err)
		return
	}
	// Wait briefly for the NEW process to bind the port.
	ready := false
	for i := 0; i < 30; i++ {
		if conn, derr := net.Dial("tcp", socksAddr); derr == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		_ = reloadCmd.Process.Kill()
		_ = reloadCmd.Wait()
		log.Printf("[sing-box] reload: new process failed to bind port %d; proxy offline until next refresh", cfg.LocalPort)
		return
	}
	// Build per-node clients for the refreshed node set
	nodeClients := make(map[int]*Clients, len(selected))
	nodePorts := make(map[int]int, len(selected))
	nodeHealthMap := make(map[int]*nodeHealth, len(selected))
	egressAddrs := make(map[int]string, len(selected))
	for i := range selected {
		port := cfg.LocalPort + 1 + i
		nodeClients[i] = buildLocalSOCKS5Clients(port)
		nodePorts[i] = port
		nodeHealthMap[i] = newNodeHealth()
		egressAddrs[i] = selected[i].Address
	}
	sbMu.Lock()
	sbProcess = reloadCmd
	sbNodeList = nodeNames(selected)
	sbClients = buildLocalSOCKS5Clients(cfg.LocalPort)
	sbNodeClients = nodeClients
	sbNodePorts = nodePorts
	sbNodeHealth = nodeHealthMap
	sbEgressAddrs = egressAddrs
	sbLastStart = time.Now()
	sbMu.Unlock()
	// Restart health checks for the new node set
	startHealthChecks()
	log.Printf("[sing-box] refreshed with %d nodes", len(selected))
	go watchSingBoxProcess(reloadCmd)
}

// rebuildSingBoxWithout fetches the current subscription fresh, drops nodes
// whose upstream Address is in banned, rewrites the config, restarts sing-box
// and rewires the per-node clients/health maps. It is called when nodes are
// banned so the bad ports are released and healthy spares take over.
func rebuildSingBoxWithout(cfg *SingBoxConfig, banned map[string]bool) error {
	nodes, err := fetchSubscription(cfg.SubscriptionURL)
	if err != nil {
		return fmt.Errorf("fetch subscription: %w", err)
	}
	if len(nodes) == 0 {
		return fmt.Errorf("subscription returned 0 nodes")
	}
	selected, err := writeSingBoxConfig(cfg, nodes, banned)
	if err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if len(selected) == 0 {
		return fmt.Errorf("no nodes left after excluding banned nodes")
	}

	configPath := filepath.Join(cfg.ConfigDir, "config.json")
	if out, err := exec.Command(cfg.BinaryPath, "check", "-c", configPath).CombinedOutput(); err != nil {
		return fmt.Errorf("config validation failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Stop the current process and wait for the shared port to be released.
	stopHealthChecks()
	sbMu.Lock()
	oldCmd := sbProcess
	sbProcess = nil
	sbClients = nil
	sbNodeClients = nil
	sbNodePorts = nil
	sbMu.Unlock()
	socksAddr := fmt.Sprintf("127.0.0.1:%d", cfg.LocalPort)
	stopProcessAndWaitPort(oldCmd, socksAddr)
	for i := 0; i < 40; i++ {
		conn, derr := net.Dial("tcp", socksAddr)
		if derr != nil {
			break
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}

	cmd := exec.Command(cfg.BinaryPath, "run", "-c", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}
	ready := false
	for i := 0; i < 30; i++ {
		if conn, derr := net.Dial("tcp", socksAddr); derr == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("new sing-box failed to bind port %d", cfg.LocalPort)
	}

	nodeClients := make(map[int]*Clients, len(selected))
	nodePorts := make(map[int]int, len(selected))
	nodeHealthMap := make(map[int]*nodeHealth, len(selected))
	egressAddrs := make(map[int]string, len(selected))
	for i := range selected {
		port := cfg.LocalPort + 1 + i
		nodeClients[i] = buildLocalSOCKS5Clients(port)
		nodePorts[i] = port
		nodeHealthMap[i] = newNodeHealth()
		egressAddrs[i] = selected[i].Address
	}
	sbMu.Lock()
	sbProcess = cmd
	sbNodeList = nodeNames(selected)
	sbClients = buildLocalSOCKS5Clients(cfg.LocalPort)
	sbNodeClients = nodeClients
	sbNodePorts = nodePorts
	sbNodeHealth = nodeHealthMap
	sbEgressAddrs = egressAddrs
	sbLastStart = time.Now()
	sbMu.Unlock()
	startHealthChecks()

	go watchSingBoxProcess(cmd)

	log.Printf("[sing-box] rebuilt with %d nodes after banning %d", len(selected), len(banned))
	return nil
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
	UUID     string
	Address  string
	Port     int
	Network  string // ws, tcp, etc.
	TLS      bool
	SNI      string
	Host     string
	Path     string
	FP       string
	Alpn     string
	Name     string
	Raw      string
	Proto    string // vless, vmess, ss
	SSMethod string // shadowsocks method
	SSPass   string // shadowsocks password
}

func fetchSubscription(rawURL string) ([]vlessNode, error) {
	// The URL comes from the operator config field; refuse file:// and any
	// scheme the http client would otherwise resolve unexpectedly.
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("subscription URL must be http(s)")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	// Err must not be ignored: a malformed subscription URL returns a nil req
	// and the subsequent client.Do(req) would panic on the nil request.
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
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
			"type":         "grpc",
			"service_name": n.Path,
		}
		ob["transport"] = grpcCfg
	}
	// For "tcp" (default), no transport field is needed.

	return ob
}

// writeSingBoxConfig generates the sing-box config and returns the selected
// node list so the caller can build matching per-node clients. Nodes whose
// upstream Address is in banned are dropped so their ports are reclaimed by
// other healthy nodes.
func writeSingBoxConfig(cfg *SingBoxConfig, nodes []vlessNode, banned map[string]bool) ([]vlessNode, error) {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return nil, err
	}

	// Use all nodes — sing-box urltest will auto-pick fastest.
	// We also shuffle so each restart rotates the order. Banned egress
	// addresses are removed from the candidate pool entirely.
	selected := selectRandomNodes(nodes, maxNodeInbounds)
	if len(banned) > 0 {
		filtered := selected[:0]
		for _, n := range selected {
			if banned[n.Address] {
				continue
			}
			filtered = append(filtered, n)
		}
		selected = filtered
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("sing-box: no nodes left after excluding banned nodes")
	}

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
		"url":          healthProbeURL(),
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
	if err := os.WriteFile(configPath, b, 0o600); err != nil {
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
	mu         sync.Mutex
	health     string        // "healthy", "unhealthy", "checking", "banned"
	failures   int           // consecutive failure count
	latency    time.Duration // last measured latency
	lastCheck  time.Time     // last check time
	lastError  string        // last error message
	isolated   bool          // true when node is skipped due to failures/ban
	isolatedAt time.Time     // when isolation started
	banned     bool          // true when the exit IP is CDN-blocked (403/429)
}

// newnodeHealth creates a nodeHealth with default healthy state.
func newNodeHealth() *nodeHealth {
	return &nodeHealth{
		health: "checking",
	}
}

// probeKind classifies the outcome of an egress probe.
type probeKind int

const (
	probeOK          probeKind = iota // response arrived from the probe target (<500)
	probeCDNBlock                     // 403/429: exit IP blocked by the CDN
	probeUpstreamErr                  // 5xx from the probe target
	probeTransport                    // TCP/TLS/timeout: node could not carry traffic
)

type probeResult struct {
	kind    probeKind
	status  int
	latency time.Duration
	err     error
}

// probeCnbRequest performs one small, non-keep-alive GET through the node's
// SOCKS5 client. It is deliberately bounded to healthCheckTimeout and never
// reuses connections so a half-dead tunnel cannot masquerade as healthy by
// serving a pooled response.
func probeCnbRequest(client *http.Client) probeResult {
	if client == nil {
		return probeResult{kind: probeTransport, err: fmt.Errorf("nil http client")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthProbeURL(), nil)
	if err != nil {
		return probeResult{kind: probeTransport, err: err}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	// Clone the transport so DisableKeepAlives applies to this probe only and
	// does not mutate the shared per-node transport.
	base := client.Transport
	var tr http.RoundTripper
	if t, ok := base.(*http.Transport); ok && t != nil {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		tr = clone
	} else {
		tr = base
	}
	probeClient := &http.Client{Transport: tr, Timeout: healthCheckTimeout}

	start := time.Now()
	resp, err := probeClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return probeResult{kind: probeTransport, latency: latency, err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return probeResult{kind: probeCDNBlock, status: resp.StatusCode, latency: latency}
	case resp.StatusCode < 500:
		return probeResult{kind: probeOK, status: resp.StatusCode, latency: latency}
	default:
		return probeResult{kind: probeUpstreamErr, status: resp.StatusCode, latency: latency}
	}
}

// probeEgress is the two-step health test for one node:
//  1. TCP connect to the node's local SOCKS5 port (proves the sing-box
//     process is alive and the inbound is listening);
//  2. a real egress GET through that port to the M365 upstream.
//
// Step 2 distinguishes a genuinely working tunnel from an inbound that
// accepts connections while the upstream is dead.
func probeEgress(idx int, clients *Clients, port int) probeResult {
	socksAddr := fmt.Sprintf("127.0.0.1:%d", port)
	tcpStart := time.Now()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.Dial("tcp", socksAddr)
	if err != nil {
		return probeResult{kind: probeTransport, latency: time.Since(tcpStart), err: fmt.Errorf("tcp connect to %s: %w", socksAddr, err)}
	}
	_ = conn.Close()

	res := probeCnbRequest(clients.HTTP)
	if res.err != nil {
		res.err = fmt.Errorf("egress probe through node %d (port %d) failed for %s: %w", idx, port, healthProbeURL(), res.err)
	}
	return res
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

// checkAllNodes runs a health check against every node concurrently. When a
// node is banned (CDN-blocked exit IP) the whole sing-box config is rebuilt
// without that node so its port is released and a healthy node takes over.
func checkAllNodes() {
	sbMu.Lock()
	nodes := make([]int, 0, len(sbNodeClients))
	for idx := range sbNodeClients {
		nodes = append(nodes, idx)
	}
	clientsSnapshot := make(map[int]*Clients, len(sbNodeClients))
	portSnapshot := make(map[int]int, len(sbNodePorts))
	for idx, c := range sbNodeClients {
		clientsSnapshot[idx] = c
	}
	for idx, p := range sbNodePorts {
		portSnapshot[idx] = p
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
			checkNodeHealth(idx, clientsSnapshot[idx], portSnapshot[idx])
		}(idx)
	}
	wg.Wait()
}

// checkNodeHealth runs the two-step health test for a single node and returns
// a non-empty ban reason when the node's exit IP must be dropped immediately.
//
// Step 1 — TCP connect to the node's local SOCKS5 port (sing-box alive).
// Step 2 — a real egress GET through that port to the M365 upstream:
//   - 403/429 means the CDN is blocking this exit IP → ban immediately;
//   - any other HTTP status < 500 means the request reached M365 → healthy;
//   - transport error / timeout → soft failure, counted toward healthMaxFailures.
func checkNodeHealth(idx int, clients *Clients, port int) string {
	sbMu.Lock()
	nh, nhOk := sbNodeHealth[idx]
	bannedAddr := sbBannedEgresses[sbEgressAddrs[idx]]
	sbMu.Unlock()
	if clients == nil || !nhOk || nh == nil {
		return ""
	}

	nh.mu.Lock()
	nh.health = "checking"
	nh.mu.Unlock()

	res := probeEgress(idx, clients, port)
	switch res.kind {
	case probeOK:
		nh.mu.Lock()
		// A CDN-blocked node gets 403/429 on the probe target; if it somehow
		// returns <500 it must NOT be resurrected on this path alone, because
		// the ban is keyed on the exit Address and persisted. Only clear the
		// ban when the address is no longer in the persistent ban set.
		nh.health = "healthy"
		nh.failures = 0
		nh.latency = res.latency
		nh.lastCheck = time.Now()
		nh.lastError = ""
		if nh.isolated && !bannedAddr {
			nh.isolated = false
			nh.isolatedAt = time.Time{}
			nh.banned = false
			log.Printf("[sing-box] node %d (port %d) recovered, latency=%s", idx, port, res.latency)
		}
		nh.mu.Unlock()
		return ""

	case probeCDNBlock:
		reason := fmt.Sprintf("exit IP blocked by CDN (HTTP %d)", res.status)
		banNodeLocked(idx, reason)
		return reason

	case probeUpstreamErr:
		// A 5xx means the request REACHED the upstream through this tunnel —
		// the proxy path works, the CDN merely errored. This is not a proxy
		// failure, so it must not count toward a ban; only refresh latency.
		nh.mu.Lock()
		nh.latency = res.latency
		nh.lastCheck = time.Now()
		nh.lastError = fmt.Sprintf("upstream returned HTTP %d", res.status)
		if nh.health == "checking" {
			nh.health = "healthy"
		}
		nh.mu.Unlock()
		return ""

	default: // probeTransport
		err := res.err
		if err == nil {
			err = fmt.Errorf("egress probe transport failure")
		}
		recordNodeFailure(idx, err)
		return ""
	}
}

// banNodeLocked is the single ban entry point: it flags a node as banned so
// it is immediately skipped by the picker, persists the ban against the
// upstream Address (so a restart never re-selects the same dead IP), records
// the reason, fires the 联动 callback, and schedules a throttled replacement
// restart that frees the bad port for a healthy node.
func banNodeLocked(idx int, reason string) {
	sbMu.Lock()
	nh := sbNodeHealth[idx]
	port := sbNodePorts[idx]
	addr := sbEgressAddrs[idx]
	if nh == nil {
		sbMu.Unlock()
		return
	}
	persistAddr := addr != "" && !sbBannedEgresses[addr]
	if persistAddr {
		sbBannedEgresses[addr] = true
	}
	sbMu.Unlock()

	nh.mu.Lock()
	already := nh.banned
	nh.banned = true
	nh.isolated = true
	nh.health = "banned"
	nh.failures = healthMaxFailures
	nh.lastError = reason
	nh.lastCheck = time.Now()
	if !already {
		nh.isolatedAt = time.Now()
	}
	nh.mu.Unlock()
	if !already {
		log.Printf("[sing-box] node %d (port %d) BANNED: %s", idx, port, reason)
	}
	if persistAddr {
		log.Printf("[sing-box] egress IP banned (persistent): %s", addr)
		go scheduleReplacementRestart(reason)
	}
	onNodeBanned(idx, reason)
}

// onNodeBanned invokes the registered ban联动 callback asynchronously.
func onNodeBanned(idx int, reason string) {
	onNodeBannedMu.Lock()
	fn := onNodeBannedFn
	onNodeBannedMu.Unlock()
	if fn == nil {
		log.Printf("[sing-box] ban callback: node=%d reason=%s", idx, reason)
		return
	}
	go fn(idx, reason)
}

// scheduleReplacementRestart replaces bad egresses: after the restart debounce
// window it re-selects nodes (skipping bannedEgresses) and restarts sing-box so
// the freed ports carry spare nodes. A replacement node that is itself bad will
// be banned by the next health round and trigger this again, replacing until the
// set stabilises or spares run out. The sbReplacing latch ensures only one
// replacement restart is ever in flight, so concurrent bans cannot fight over
// the shared local port.
func scheduleReplacementRestart(reason string) {
	sbMu.Lock()
	cfg := sbConfig
	lastStart := sbLastStart
	// Coalesce concurrent bans: if a replacement restart is already scheduled
	// or in flight, fold this ban into it instead of stacking another full
	// subscription fetch + process restart (each restart briefly drops the
	// shared port).
	if sbReplacing {
		sbMu.Unlock()
		log.Printf("[sing-box] replacement restart already pending; coalescing ban (%s)", reason)
		return
	}
	sbReplacing = true
	banned := make(map[string]bool, len(sbBannedEgresses))
	for a := range sbBannedEgresses {
		banned[a] = true
	}
	sbMu.Unlock()
	if cfg == nil {
		sbMu.Lock()
		sbReplacing = false
		sbMu.Unlock()
		return
	}
	wait := restartCooldown + 2*time.Second - time.Since(lastStart)
	if wait > 0 {
		time.Sleep(wait)
	}

	// Serialise against refresh/configure/stop: only one process swap may run
	// at a time or they fight over the shared local port.
	sbLifecycleMu.Lock()
	defer sbLifecycleMu.Unlock()
	defer func() {
		sbMu.Lock()
		sbReplacing = false
		sbMu.Unlock()
	}()
	// Re-read the ban set: bans recorded while we waited must be honoured.
	sbMu.Lock()
	for a := range sbBannedEgresses {
		banned[a] = true
	}
	sbMu.Unlock()
	log.Printf("[sing-box] replacing banned egresses (%s)", reason)
	if err := replaceBannedNodes(cfg, banned); err != nil {
		log.Printf("[sing-box] replacement restart failed: %v", err)
	}
}

// replaceBannedNodes re-selects nodes from the subscription while skipping
// addresses in banned, then rebuilds if at least one usable node remains.
func replaceBannedNodes(cfg *SingBoxConfig, banned map[string]bool) error {
	nodes, err := fetchSubscription(cfg.SubscriptionURL)
	if err != nil {
		return fmt.Errorf("fetch subscription: %w", err)
	}
	usable := 0
	for _, n := range nodes {
		if !banned[n.Address] {
			usable++
		}
	}
	if usable == 0 {
		return fmt.Errorf("no usable nodes left (%d banned)", len(banned))
	}
	return rebuildSingBoxWithout(cfg, banned)
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
	if nh.isolated {
		nh.mu.Unlock()
		return
	}
	nh.failures++
	nh.lastCheck = time.Now()
	nh.lastError = err.Error()
	reached := nh.failures >= healthMaxFailures
	failures := nh.failures
	if !reached {
		nh.health = "unhealthy"
	}
	nh.mu.Unlock()
	if reached {
		// Reached the soft-failure threshold: route through the unified ban
		// entry so the node is dropped and its port reclaimed by a healthy one.
		banNodeLocked(idx, fmt.Sprintf("soft failures reached %d: %s", healthMaxFailures, err.Error()))
		return
	}
	log.Printf("[sing-box] node %d (port %d) check failed (%d/%d): %s", idx, port, failures, healthMaxFailures, err.Error())
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
			node["banned"] = nh.banned
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
	bannedEgresses := 0
	for _, nh := range sbNodeHealth {
		if nh != nil {
			nh.mu.Lock()
			if nh.isolated {
				isolated++
			}
			if nh.banned {
				bannedEgresses++
			}
			if !nh.isolated && nh.health == "healthy" {
				healthy++
			}
			nh.mu.Unlock()
		}
	}

	persistentBans := make([]string, 0, len(sbBannedEgresses))
	for a := range sbBannedEgresses {
		persistentBans = append(persistentBans, a)
	}
	sort.Strings(persistentBans)

	status := []map[string]any{
		{
			"subscription":    sbConfig.SubscriptionURL,
			"local_port":      sbConfig.LocalPort,
			"binary":          sbConfig.BinaryPath,
			"node_count":      len(sbNodeList),
			"nodes":           sbNodeList,
			"node_details":    nodes,
			"healthy_nodes":   healthy,
			"isolated_nodes":  isolated,
			"banned_nodes":    bannedEgresses,
			"banned_egresses": persistentBans,
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

// PickNodeForAccount is the SINGLE source of truth for account → node
// affinity. Both request routing (web.accountClient) and the admin UI
// (SingBoxNodeInfo) must go through it. The previous two-path implementation
// hashed accountID modulo the NON-isolated count in one place and modulo the
// TOTAL count in the other: every time a single node was isolated, every
// account's home node drifted at once (mass simultaneous exit-IP churn — the
// exact pattern M365 fraud systems punish), and the UI showed nodes the
// account never actually used.
//
// home = hash % total is stable across isolation state; when the home node is
// isolated the picker walks forward to the nearest healthy node, so ONLY the
// accounts homed on that one node move (minimal churn), and they walk back to
// their original home as soon as it recovers. When every per-node inbound is
// banned the urltest selector is used if sing-box is still alive; only when
// sing-box itself is gone does the picker allow a DIRECT fallback (controlled
// by M365_ALLOW_DIRECT_FALLBACK, default on). ok=false means no per-node pool
// (or sing-box dead): callers fall back to their existing behaviour.
func PickNodeForAccount(accountID string) (clients *Clients, idx int, name, health string, ok bool) {
	sbMu.Lock()
	total := len(sbNodeClients)
	if total == 0 {
		selector := sbClients
		sbMu.Unlock()
		if selector != nil {
			return selector, -1, "urltest-fallback", "fallback", true
		}
		return directFallback(accountID)
	}
	home := int(stableHash(accountID) % uint64(total))
	for i := 0; i < total; i++ {
		node := (home + i) % total
		c := sbNodeClients[node]
		if c == nil {
			continue
		}
		nh := sbNodeHealth[node]
		if nh != nil && nh.isolated {
			continue
		}
		h := "unknown"
		if nh != nil {
			nh.mu.Lock()
			h = nh.health
			nh.mu.Unlock()
		}
		nm := ""
		if node < len(sbNodeList) {
			nm = sbNodeList[node]
		}
		sbMu.Unlock()
		return c, node, nm, h, true
	}
	selector := sbClients
	sbMu.Unlock()
	// All per-node inbounds banned/unhealthy: prefer the urltest selector (it
	// still egresses through sing-box, just picks the best tunnel itself).
	if selector != nil {
		log.Printf("[sing-box] all per-node candidates isolated; account %s falls back to urltest selector", accountID)
		return selector, -1, "urltest-fallback", "fallback", true
	}
	// sing-box itself is gone: allow direct egress (opt-out via env).
	return directFallback(accountID)
}

// directFallback returns the direct clients when M365_ALLOW_DIRECT_FALLBACK is
// not disabled, otherwise ok=false so the caller fails closed. Returning direct
// egress exposes the host's real IP to M365 and is only used when every proxy
// node is unusable.
func directFallback(accountID string) (*Clients, int, string, string, bool) {
	if v := strings.TrimSpace(os.Getenv("M365_ALLOW_DIRECT_FALLBACK")); v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no") {
		log.Printf("[sing-box] no healthy node and direct fallback disabled; account %s has no egress", accountID)
		return nil, -1, "", "", false
	}
	log.Printf("[sing-box] ALL nodes banned and sing-box down; account %s falling back to DIRECT egress", accountID)
	return directClients(), -1, "direct-fallback", "direct", true
}

// SingBoxNodeInfo returns the node ACTUALLY selected for an account (same
// algorithm as request routing) plus its health, for admin display.
func SingBoxNodeInfo(accountID string) (int, string, string, bool) {
	c, idx, name, health, ok := PickNodeForAccount(accountID)
	if !ok || c == nil {
		return 0, "", "", false
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
