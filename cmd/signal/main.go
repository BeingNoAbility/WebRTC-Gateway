package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"webrtc-gateway/pkg/signaling"
	"webrtc-gateway/web"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow cross-origin signaling from Cloudflare Pages / Portal
	},
}

var (
	defaultPingPeriod  = 25 * time.Second
	defaultReadTimeout = 60 * time.Second
)

// SetKeepaliveIntervalsForTest adjusts the ping period and read timeout for tests.
func SetKeepaliveIntervalsForTest(ping, read time.Duration) {
	defaultPingPeriod = ping
	defaultReadTimeout = read
}

type Session struct {
	ID       string
	Role     string
	NodeID   string
	RemoteIP string
	Conn     *websocket.Conn
	mu       sync.Mutex
}

func (s *Session) Send(msg *signaling.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Conn == nil {
		return nil
	}
	_ = s.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.Conn.WriteJSON(msg)
}

func (s *Session) Ping() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Conn == nil {
		return nil
	}
	_ = s.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.Conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
}

type SignalBroker struct {
	mu              sync.RWMutex
	serverSecret    string              // Global server secret (if set via -signal-secret / SIGNAL_SECRET)
	agents          map[string]*Session // node_id -> Session
	nodeSecrets     map[string]string   // node_id -> registered secret (anti-hijack)
	nodeVisitorAuth map[string]bool     // node_id -> whether visitors require authentication
	clients         map[string]*Session // session_id -> Session
}

func NewSignalBroker(serverSecret string) *SignalBroker {
	return &SignalBroker{
		serverSecret:    serverSecret,
		agents:          make(map[string]*Session),
		nodeSecrets:     make(map[string]string),
		nodeVisitorAuth: make(map[string]bool),
		clients:         make(map[string]*Session),
	}
}

func (b *SignalBroker) RegisterAgent(nodeID, secret string, requireVisitorAuth *bool, s *Session) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 1. Verify global server secret if server was started with -signal-secret
	if b.serverSecret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(b.serverSecret)) != 1 {
		return fmt.Errorf("unauthorized: server secret mismatch")
	}

	// 2. Node ownership protection (anti-hijack)
	boundSecret, hasBoundSecret := b.nodeSecrets[nodeID]
	if hasBoundSecret && boundSecret != "" {
		if subtle.ConstantTimeCompare([]byte(secret), []byte(boundSecret)) != 1 {
			return fmt.Errorf("unauthorized: node_id %q is protected by a different secret", nodeID)
		}
	} else if _, isConnected := b.agents[nodeID]; isConnected && secret != "" {
		// An active agent session exists without a secret. Prohibit an anonymous connection from injecting a secret mid-way to hijack it.
		return fmt.Errorf("unauthorized: node_id %q is actively in use without cluster secret; hijacking prohibited", nodeID)
	}

	if old, exists := b.agents[nodeID]; exists {
		_ = old.Send(&signaling.Message{Type: signaling.TypeError, Error: "replaced by new agent connection"})
		if old.Conn != nil {
			_ = old.Conn.Close()
		}
	}
	b.agents[nodeID] = s
	if secret != "" {
		// Cap nodeSecrets capacity to prevent memory exhaustion
		const maxNodeSecrets = 10000
		if len(b.nodeSecrets) >= maxNodeSecrets {
			if _, exists := b.nodeSecrets[nodeID]; !exists {
				// Evict orphaned secrets (nodes not currently connected)
				for k := range b.nodeSecrets {
					if _, connected := b.agents[k]; !connected {
						delete(b.nodeSecrets, k)
						if len(b.nodeSecrets) < maxNodeSecrets {
							break
						}
					}
				}
			}
		}
		b.nodeSecrets[nodeID] = secret
	}
	if requireVisitorAuth != nil {
		b.nodeVisitorAuth[nodeID] = *requireVisitorAuth
	} else {
		// Fallback for legacy agents: if secret is provided, assume visitor auth is required
		b.nodeVisitorAuth[nodeID] = (secret != "")
	}
	log.Printf("[Broker] Agent registered: node_id=%s (cluster auth: %v, visitor auth: %v)", nodeID, secret != "", b.nodeVisitorAuth[nodeID])
	return nil
}

func (b *SignalBroker) RequiresVisitorAuth(nodeID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.nodeVisitorAuth[nodeID]
}

func (b *SignalBroker) HasSecret(nodeID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sec, exists := b.nodeSecrets[nodeID]
	return exists && sec != ""
}

func (b *SignalBroker) RemoveAgent(nodeID string, s *Session) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if curr, exists := b.agents[nodeID]; exists && curr == s {
		delete(b.agents, nodeID)
		delete(b.nodeVisitorAuth, nodeID)
		log.Printf("[Broker] Agent disconnected: node_id=%s", nodeID)
	}
}

func (b *SignalBroker) RegisterClient(s *Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[s.ID] = s
	log.Printf("[Broker] Client joined: session_id=%s", s.ID)
}

func (b *SignalBroker) RemoveClient(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, sessionID)
	log.Printf("[Broker] Client disconnected: session_id=%s", sessionID)
}

func (b *SignalBroker) GetAgent(nodeID string) *Session {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.agents[nodeID]
}

func (b *SignalBroker) GetClient(sessionID string) *Session {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.clients[sessionID]
}

func (b *SignalBroker) NotifyWaitingClients(nodeID string) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	reqAuth := b.nodeVisitorAuth[nodeID]
	for _, client := range b.clients {
		if client.NodeID == nodeID {
			_ = client.Send(&signaling.Message{
				Type:           signaling.TypePeerStatus,
				NodeID:         nodeID,
				SessionID:      client.ID,
				Status:         "online",
				RequiresSecret: reqAuth,
			})
		}
	}
}

func (b *SignalBroker) AgentCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.agents)
}

func (b *SignalBroker) ClientCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}

var signalingMessagesTotal uint64

var defaultTrustedProxyCIDRs = []string{
	"127.0.0.0/8",    // IPv4 Loopback
	"::1/128",        // IPv6 Loopback
	"10.0.0.0/8",     // RFC 1918 Class A
	"172.16.0.0/12",  // RFC 1918 Class B
	"192.168.0.0/16", // RFC 1918 Class C
	"100.64.0.0/10",  // RFC 6598 Carrier-Grade NAT (CGNAT, Tailscale, Docker overlays)
	"169.254.0.0/16", // IPv4 Link-Local
	"fe80::/10",      // IPv6 Link-Local
	"fc00::/7",       // IPv6 Unique Local Address (ULA)
}

// DefaultTrustedProxies returns the pre-compiled list of default trusted proxy prefixes.
func DefaultTrustedProxies() []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(defaultTrustedProxyCIDRs))
	for _, cidr := range defaultTrustedProxyCIDRs {
		p, err := netip.ParsePrefix(cidr)
		if err == nil {
			prefixes = append(prefixes, p)
		}
	}
	return prefixes
}

// ParseTrustedProxies parses a comma-separated list of CIDRs or IP addresses into []netip.Prefix.
// If s is empty or "default", DefaultTrustedProxies() is returned.
// If s is "none", an empty slice is returned (disabling trusted proxy headers entirely).
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "default") {
		return DefaultTrustedProxies(), nil
	}
	if strings.EqualFold(s, "none") {
		return []netip.Prefix{}, nil
	}

	var prefixes []netip.Prefix
	parts := strings.Split(s, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			prefix, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", part, err)
			}
			prefixes = append(prefixes, prefix)
		} else {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("invalid IP address %q: %w", part, err)
			}
			bits := 32
			if addr.Is6() {
				bits = 128
			}
			prefixes = append(prefixes, netip.PrefixFrom(addr, bits))
		}
	}
	return prefixes, nil
}

// isTrustedProxy returns true if remoteAddr's IP belongs to any of the trusted prefixes.
func isTrustedProxy(remoteAddr string, prefixes []netip.Prefix) bool {
	if len(prefixes) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	// In IPv4-mapped IPv6 (e.g. ::ffff:127.0.0.1), unmap to IPv4 for accurate matching
	addr = addr.Unmap()
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

type IPRateLimiter struct {
	mu              sync.Mutex
	maxConns        int
	trustedPrefixes []netip.Prefix
	connsPerIP      map[string]int
	msgTimes        map[string][]time.Time
	failedAuths     map[string][]time.Time
	rejectedCount   uint64
	stopCh          chan struct{}
}

func NewIPRateLimiter(maxConnsPerIP int, trustedPrefixes ...[]netip.Prefix) *IPRateLimiter {
	if maxConnsPerIP <= 0 {
		maxConnsPerIP = 64
	}
	var prefixes []netip.Prefix
	if len(trustedPrefixes) > 0 && trustedPrefixes[0] != nil {
		prefixes = trustedPrefixes[0]
	} else {
		prefixes = DefaultTrustedProxies()
	}
	limiter := &IPRateLimiter{
		maxConns:        maxConnsPerIP,
		trustedPrefixes: prefixes,
		connsPerIP:      make(map[string]int),
		msgTimes:        make(map[string][]time.Time),
		failedAuths:     make(map[string][]time.Time),
		stopCh:          make(chan struct{}),
	}
	go limiter.janitorLoop(2 * time.Minute)
	return limiter
}

// janitorLoop periodically cleans up expired IP entries from memory maps to prevent chronic memory leaks.
func (l *IPRateLimiter) janitorLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.pruneExpired()
		case <-l.stopCh:
			return
		}
	}
}

// pruneExpired removes expired message rate limit and failed authentication records,
// and deletes IP keys from the map once their slices become empty.
func (l *IPRateLimiter) pruneExpired() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	msgCutoff := now.Add(-10 * time.Second)
	authCutoff := now.Add(-1 * time.Minute)

	for ip, times := range l.msgTimes {
		validCount := 0
		for _, t := range times {
			if t.After(msgCutoff) {
				validCount++
			}
		}
		if validCount == 0 {
			delete(l.msgTimes, ip)
		} else if validCount < len(times) {
			valid := make([]time.Time, 0, validCount)
			for _, t := range times {
				if t.After(msgCutoff) {
					valid = append(valid, t)
				}
			}
			l.msgTimes[ip] = valid
		}
	}

	for ip, times := range l.failedAuths {
		validCount := 0
		for _, t := range times {
			if t.After(authCutoff) {
				validCount++
			}
		}
		if validCount == 0 {
			delete(l.failedAuths, ip)
		} else if validCount < len(times) {
			valid := make([]time.Time, 0, validCount)
			for _, t := range times {
				if t.After(authCutoff) {
					valid = append(valid, t)
				}
			}
			l.failedAuths[ip] = valid
		}
	}
}

// Close gracefully stops the background janitor goroutine.
func (l *IPRateLimiter) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.stopCh:
		// already closed
	default:
		close(l.stopCh)
	}
}

func (l *IPRateLimiter) GetClientIP(r *http.Request) string {
	return getClientIP(r, l.trustedPrefixes)
}

func (l *IPRateLimiter) AcquireConn(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	current := l.connsPerIP[ip]
	if l.maxConns > 0 && current >= l.maxConns {
		atomic.AddUint64(&l.rejectedCount, 1)
		return false
	}
	l.connsPerIP[ip] = current + 1
	return true
}

func (l *IPRateLimiter) ReleaseConn(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := l.connsPerIP[ip]; c > 1 {
		l.connsPerIP[ip] = c - 1
	} else {
		delete(l.connsPerIP, ip)
	}
}

func (l *IPRateLimiter) AllowMessage(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-10 * time.Second)

	times := l.msgTimes[ip]
	valid := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= 120 {
		atomic.AddUint64(&l.rejectedCount, 1)
		l.msgTimes[ip] = valid
		return false
	}
	l.msgTimes[ip] = append(valid, now)
	return true
}

func (l *IPRateLimiter) RecordFailedAuth(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-1 * time.Minute)

	times := l.failedAuths[ip]
	valid := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	valid = append(valid, now)
	l.failedAuths[ip] = valid
	if len(valid) >= 10 {
		atomic.AddUint64(&l.rejectedCount, 1)
		return true
	}
	return false
}

func (l *IPRateLimiter) IsLocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-1 * time.Minute)
	validCount := 0
	for _, t := range l.failedAuths[ip] {
		if t.After(cutoff) {
			validCount++
		}
	}
	return validCount >= 10
}

func (l *IPRateLimiter) ResetFailedAuth(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failedAuths, ip)
}

func (l *IPRateLimiter) Rejections() uint64 {
	return atomic.LoadUint64(&l.rejectedCount)
}

// getClientIP extracts the real client IP address from the HTTP request.
// If remoteAddr belongs to a trusted proxy (e.g. Docker bridge, Nginx, Cloudflare):
// 1. CF-Connecting-IP (overwritten by Cloudflare CDN/Worker edge, cannot be forged)
// 2. X-Forwarded-For (first client IP)
// 3. X-Real-IP
// If remoteAddr is NOT from a trusted proxy (e.g. direct raw connection from public Internet),
// proxy headers are strictly ignored to prevent IP spoofing against rate limiting.
func getClientIP(r *http.Request, trustedPrefixes []netip.Prefix) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	if isTrustedProxy(r.RemoteAddr, trustedPrefixes) {
		// 1. CF-Connecting-IP (Cloudflare Edge CDN / Worker headers, cannot be forged when coming from Cloudflare)
		if cfIP := r.Header.Get("CF-Connecting-IP"); cfIP != "" {
			ip := strings.TrimSpace(cfIP)
			if ip != "" {
				return ip
			}
		}

		// 2. X-Forwarded-For: client, proxy1, proxy2...
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if len(parts) > 0 {
				ip := strings.TrimSpace(parts[0])
				if ip != "" {
					return ip
				}
			}
		}

		// 3. X-Real-IP
		if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
			ip := strings.TrimSpace(xrip)
			if ip != "" {
				return ip
			}
		}
	}

	// Untrusted proxy or raw direct connection: strictly use the underlying socket IP
	return remoteHost
}

func exportServerMetrics(broker *SignalBroker, limiter *IPRateLimiter) string {
	var b strings.Builder
	b.WriteString("# HELP webrtc_gateway_registered_nodes Number of currently registered agent nodes\n")
	b.WriteString("# TYPE webrtc_gateway_registered_nodes gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_registered_nodes %d\n\n", broker.AgentCount()))

	b.WriteString("# HELP webrtc_gateway_active_clients Number of currently connected visitor clients\n")
	b.WriteString("# TYPE webrtc_gateway_active_clients gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_active_clients %d\n\n", broker.ClientCount()))

	b.WriteString("# HELP webrtc_gateway_signaling_messages_total Total signaling messages processed\n")
	b.WriteString("# TYPE webrtc_gateway_signaling_messages_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_signaling_messages_total %d\n\n", atomic.LoadUint64(&signalingMessagesTotal)))

	b.WriteString("# HELP webrtc_gateway_ratelimit_rejected_total Total connection or message requests rejected by rate limiting\n")
	b.WriteString("# TYPE webrtc_gateway_ratelimit_rejected_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_ratelimit_rejected_total %d\n\n", limiter.Rejections()))

	return b.String()
}

// generateSelfSignedCert generates an in-memory ECDSA TLS certificate valid for localhost and local interface IPs.
func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Zero-Client P2P WebRTC Gateway"},
			CommonName:   "webrtc-gateway-selfsigned",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
	}

	// Auto-discover local interface IPs to add to certificate SAN
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if addrs, err := iface.Addrs(); err == nil {
				for _, addr := range addrs {
					if ipNet, ok := addr.(*net.IPNet); ok {
						if ip := ipNet.IP.To4(); ip != nil && !ip.IsLoopback() {
							template.IPAddresses = append(template.IPAddresses, ip)
						}
					}
				}
			}
		}
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal ec private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// runHealthCheck performs a lightweight HTTP/HTTPS probe against the local signal server.
// Returns 0 if healthy, 1 if unhealthy.
func runHealthCheck(addr string, preferTLS bool, customURL string) int {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   3 * time.Second,
	}

	urls := make([]string, 0, 2)
	if customURL != "" {
		urls = append(urls, customURL)
	} else {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			if strings.HasPrefix(addr, ":") {
				host = "127.0.0.1"
				port = strings.TrimPrefix(addr, ":")
			} else {
				host = "127.0.0.1"
				port = addr
			}
		}
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		httpURL := fmt.Sprintf("http://%s:%s/healthz", host, port)
		httpsURL := fmt.Sprintf("https://%s:%s/healthz", host, port)
		if preferTLS {
			urls = append(urls, httpsURL, httpURL)
		} else {
			urls = append(urls, httpURL, httpsURL)
		}
	}

	var lastErr error
	for _, targetURL := range urls {
		req, err := http.NewRequest(http.MethodGet, targetURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "WebRTC-Gateway-HealthCheck/1.0")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Printf("Health check OK: HTTP %d from %s\n", resp.StatusCode, targetURL)
			return 0
		}
		lastErr = fmt.Errorf("unexpected HTTP status %d from %s", resp.StatusCode, targetURL)
	}

	fmt.Fprintf(os.Stderr, "Health check FAILED: %v\n", lastErr)
	return 1
}

// Version represents the current release version of WebRTC Gateway Signal Server.
var Version = "v2.0.2"

func printSignalUsage(out io.Writer) {
	fmt.Fprintf(out, "WebRTC Gateway Signal (%s)\n", Version)
	fmt.Fprintf(out, "Zero-Client WebRTC P2P Central Signaling & Web Portal Server\n\n")
	fmt.Fprintf(out, "Usage:\n")
	fmt.Fprintf(out, "  signal [options]\n\n")
	fmt.Fprintf(out, "Server & Network Options:\n")
	fmt.Fprintf(out, "  -addr string\n")
	fmt.Fprintf(out, "        Signaling HTTP/WS listening address (env: ADDR or PORT, default: :8888)\n")
	fmt.Fprintf(out, "  -config string\n")
	fmt.Fprintf(out, "        Path to YAML or JSON configuration file (env: CONFIG_FILE)\n")
	fmt.Fprintf(out, "  -static string\n")
	fmt.Fprintf(out, "        Path to external web portal static assets; falls back to embedded portal if empty or not found\n")
	fmt.Fprintf(out, "        (env: STATIC_DIR, default: ./web/dist)\n\n")
	fmt.Fprintf(out, "TLS & HTTPS Security Options:\n")
	fmt.Fprintf(out, "  -tls-auto\n")
	fmt.Fprintf(out, "        Automatically generate an in-memory self-signed ECDSA certificate covering localhost and LAN IPs\n")
	fmt.Fprintf(out, "        to provide a browser Service Worker Secure Context (HTTPS) (env: TLS_AUTO)\n")
	fmt.Fprintf(out, "  -tls-cert string\n")
	fmt.Fprintf(out, "        Path to TLS certificate PEM file (e.g. cert.pem or fullchain.pem, env: TLS_CERT)\n")
	fmt.Fprintf(out, "  -tls-key string\n")
	fmt.Fprintf(out, "        Path to TLS private key PEM file (e.g. key.pem or privkey.pem, env: TLS_KEY)\n\n")
	fmt.Fprintf(out, "Security & Access Control Options:\n")
	fmt.Fprintf(out, "  -signal-secret string\n")
	fmt.Fprintf(out, "        Server-wide pre-shared secret required for agent registration and anti-hijacking (env: SIGNAL_SECRET)\n")
	fmt.Fprintf(out, "  -trusted-proxies string\n")
	fmt.Fprintf(out, "        Comma-separated trusted reverse proxy CIDRs/IPs for real client IP extraction via X-Forwarded-For\n")
	fmt.Fprintf(out, "        (env: TRUSTED_PROXIES, default: loopback & RFC1918 private CIDRs; set to 'none' to disable)\n")
	fmt.Fprintf(out, "  -max-ip-conns int\n")
	fmt.Fprintf(out, "        Maximum concurrent WebSocket connections per client IP (env: MAX_IP_CONNS, default: 64)\n\n")
	fmt.Fprintf(out, "Monitoring & Health Options:\n")
	fmt.Fprintf(out, "  -health\n")
	fmt.Fprintf(out, "        Run local health check probe and exit with code 0 (healthy) or 1 (unhealthy)\n")
	fmt.Fprintf(out, "  -health-url string\n")
	fmt.Fprintf(out, "        Custom URL for health check probe (default: probe -addr on /healthz)\n")
	fmt.Fprintf(out, "  -metrics-addr string\n")
	fmt.Fprintf(out, "        Optional dedicated address to serve Prometheus metrics (e.g. :9090, env: METRICS_ADDR, default: /metrics on main -addr)\n\n")
	fmt.Fprintf(out, "Help & Version Options:\n")
	fmt.Fprintf(out, "  -h, -help\n")
	fmt.Fprintf(out, "        Show this help message and exit\n")
	fmt.Fprintf(out, "  -v, -version\n")
	fmt.Fprintf(out, "        Show version information and exit\n\n")
	fmt.Fprintf(out, "Configuration Precedence:\n")
	fmt.Fprintf(out, "  1. CLI Flags (Highest Priority)\n")
	fmt.Fprintf(out, "  2. Environment Variables\n")
	fmt.Fprintf(out, "  3. Configuration File (server.yaml / server.yml / server.json)\n")
	fmt.Fprintf(out, "  4. Built-in Defaults (Lowest Priority)\n\n")
	fmt.Fprintf(out, "Examples:\n")
	fmt.Fprintf(out, "  # Start signaling server with embedded web portal on port 8888\n")
	fmt.Fprintf(out, "  signal -addr :8888\n\n")
	fmt.Fprintf(out, "  # Start with in-memory self-signed TLS for local Service Worker Secure Context\n")
	fmt.Fprintf(out, "  signal -addr :8443 -tls-auto\n\n")
	fmt.Fprintf(out, "  # Production with custom TLS certificate and cluster registration secret\n")
	fmt.Fprintf(out, "  signal -addr :443 -tls-cert /etc/ssl/fullchain.pem -tls-key /etc/ssl/privkey.pem -signal-secret ClusterToken123\n\n")
	fmt.Fprintf(out, "  # Deploy behind reverse proxy (e.g. Nginx, Cloudflare) with trusted proxy CIDRs\n")
	fmt.Fprintf(out, "  signal -addr :8888 -trusted-proxies 127.0.0.1/32,10.0.0.0/8 -signal-secret ClusterToken123\n\n")
	fmt.Fprintf(out, "  # Run Docker/Kubernetes container health check probe\n")
	fmt.Fprintf(out, "  signal -health\n")
}

func main() {
	flag.Usage = func() {
		printSignalUsage(flag.CommandLine.Output())
	}

	// 0. Early check for help / version / deprecated flags
	for _, arg := range os.Args[1:] {
		if arg == "-secret" || strings.HasPrefix(arg, "-secret=") || arg == "--secret" || strings.HasPrefix(arg, "--secret=") {
			fmt.Fprintln(os.Stderr, "Error: flag -secret has been deprecated and removed.")
			fmt.Fprintln(os.Stderr, "  - Use -signal-secret (SIGNAL_SECRET) for signaling cluster admission.")
			os.Exit(1)
		}
		if arg == "help" {
			printSignalUsage(os.Stdout)
			os.Exit(0)
		}
		if arg == "version" {
			fmt.Printf("WebRTC Gateway Signal %s\n", Version)
			os.Exit(0)
		}
	}

	// 1. Defaults (Tier 1: Defaults)
	cfgAddr := ":8888"
	cfgStatic := "./web/dist"
	cfgTLSCert := ""
	cfgTLSKey := ""
	cfgTLSAuto := false
	cfgSignalSecret := ""
	cfgMaxIPConns := 64
	cfgTrustedProxies := ""
	cfgMetricsAddr := ""

	// 2. Load Config File (Tier 2: Config File)
	configPath, explicitConfig := FindServerConfigFile(os.Args[1:], []string{"server.yaml", "server.yml", "server.json"})
	if configPath != "" {
		fileCfg, err := LoadServerConfigFile(configPath)
		if err != nil {
			if explicitConfig {
				log.Fatalf("[Error] Failed to load configuration file %q: %v", configPath, err)
			} else {
				log.Fatalf("[Error] Failed to parse auto-detected configuration file %q: %v", configPath, err)
			}
		}
		log.Printf("[Config] Loaded configuration from %s", configPath)
		if fileCfg.Addr != "" {
			cfgAddr = fileCfg.Addr
		}
		if fileCfg.SignalSecret != "" {
			cfgSignalSecret = fileCfg.SignalSecret
		}
		if fileCfg.StaticDir != "" {
			cfgStatic = fileCfg.StaticDir
		}
		if fileCfg.TLSCert != "" {
			cfgTLSCert = fileCfg.TLSCert
		}
		if fileCfg.TLSKey != "" {
			cfgTLSKey = fileCfg.TLSKey
		}
		if fileCfg.TLSAuto != nil {
			cfgTLSAuto = *fileCfg.TLSAuto
		}
		if fileCfg.MaxIPConns > 0 {
			cfgMaxIPConns = fileCfg.MaxIPConns
		}
		if fileCfg.TrustedProxies != "" {
			cfgTrustedProxies = fileCfg.TrustedProxies
		}
		if fileCfg.MetricsAddr != "" {
			cfgMetricsAddr = fileCfg.MetricsAddr
		}
	}

	// 3. Override with Environment Variables (Tier 3: ENV)
	if env := os.Getenv("ADDR"); env != "" {
		cfgAddr = env
	} else if env := os.Getenv("PORT"); env != "" {
		cfgAddr = ":" + env
	}
	if env := os.Getenv("STATIC_DIR"); env != "" {
		cfgStatic = env
	}
	if env := os.Getenv("TLS_CERT"); env != "" {
		cfgTLSCert = env
	}
	if env := os.Getenv("TLS_KEY"); env != "" {
		cfgTLSKey = env
	}
	if env := os.Getenv("TLS_AUTO"); env == "true" || env == "1" {
		cfgTLSAuto = true
	}
	if env := os.Getenv("SIGNAL_SECRET"); env != "" {
		cfgSignalSecret = env
	}
	if env := os.Getenv("MAX_IP_CONNS"); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v > 0 {
			cfgMaxIPConns = v
		}
	}
	if env := os.Getenv("TRUSTED_PROXIES"); env != "" {
		cfgTrustedProxies = env
	}
	if env := os.Getenv("METRICS_ADDR"); env != "" {
		cfgMetricsAddr = env
	}

	// 4. CLI Flags (Tier 4: CLI Flags - Highest Priority)
	addr := flag.String("addr", cfgAddr, "Signaling HTTP/WS listening address (env: ADDR or PORT)")
	staticDir := flag.String("static", cfgStatic, "Path to web portal static assets; falls back to embedded portal if empty or not found (env: STATIC_DIR)")
	tlsCert := flag.String("tls-cert", cfgTLSCert, "Path to TLS certificate PEM file (e.g. cert.pem or fullchain.pem, env: TLS_CERT)")
	tlsKey := flag.String("tls-key", cfgTLSKey, "Path to TLS private key PEM file (e.g. key.pem or privkey.pem, env: TLS_KEY)")
	tlsAuto := flag.Bool("tls-auto", cfgTLSAuto, "Automatically generate in-memory self-signed ECDSA certificate for local/LAN HTTPS Secure Context (env: TLS_AUTO)")
	signalSecret := flag.String("signal-secret", cfgSignalSecret, "Server-wide pre-shared secret for agent registration & anti-hijacking (env: SIGNAL_SECRET)")
	configFlag := flag.String("config", "", "Path to configuration file (.yaml, .yml, or .json, env: CONFIG_FILE)")
	maxIPConns := flag.Int("max-ip-conns", cfgMaxIPConns, "Maximum concurrent WebSocket connections per client IP (env: MAX_IP_CONNS)")
	trustedProxies := flag.String("trusted-proxies", cfgTrustedProxies, "Comma-separated trusted proxy CIDRs/IPs for real client IP extraction (env: TRUSTED_PROXIES)")
	metricsAddr := flag.String("metrics-addr", cfgMetricsAddr, "Optional dedicated address to serve Prometheus metrics (e.g. :9090, env: METRICS_ADDR, default /metrics)")
	healthCheck := flag.Bool("health", false, "Run local health check probe and exit with code 0 (healthy) or 1 (unhealthy)")
	healthURL := flag.String("health-url", "", "Custom URL for health check probe (default: probe -addr / ADDR on /healthz)")
	versionFlag := flag.Bool("version", false, "Display version information and exit")
	vFlag := flag.Bool("v", false, "Display version information and exit (shorthand)")
	_ = configFlag
	flag.Parse()

	if *versionFlag || *vFlag {
		fmt.Printf("WebRTC Gateway Signal %s\n", Version)
		os.Exit(0)
	}

	if *healthCheck {
		os.Exit(runHealthCheck(*addr, *tlsAuto || (*tlsCert != "" && *tlsKey != ""), *healthURL))
	}

	finalSecret := *signalSecret

	trustedPrefixes, err := ParseTrustedProxies(*trustedProxies)
	if err != nil {
		log.Fatalf("Invalid -trusted-proxies configuration: %v", err)
	}
	if *trustedProxies != "" && strings.ToLower(*trustedProxies) != "default" {
		log.Printf("[Server] Custom trusted proxies configured (%d prefixes): %s", len(trustedPrefixes), *trustedProxies)
	}

	broker := NewSignalBroker(finalSecret)
	limiter := NewIPRateLimiter(*maxIPConns, trustedPrefixes)
	defer limiter.Close()

	mux := http.NewServeMux()

	// Health check endpoints for Docker / Kubernetes / load balancers
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(exportServerMetrics(broker, limiter)))
	})

	if *metricsAddr != "" {
		go func() {
			mMux := http.NewServeMux()
			mMux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
				_, _ = w.Write([]byte(exportServerMetrics(broker, limiter)))
			})
			mMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("OK\n"))
			})
			log.Printf("[Server] Dedicated Prometheus metrics listening on http://%s/metrics", *metricsAddr)
			mServer := &http.Server{
				Addr:              *metricsAddr,
				Handler:           mMux,
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       90 * time.Second,
			}
			if err := mServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[Server] Dedicated metrics server error: %v", err)
			}
		}()
	}

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		clientIP := limiter.GetClientIP(r)
		if !limiter.AcquireConn(clientIP) {
			log.Printf("[WS] Connection rejected for IP %s: max concurrent connections limit (%d) reached", clientIP, *maxIPConns)
			http.Error(w, "429 Too Many Requests: Concurrent WebSocket connections limit exceeded", http.StatusTooManyRequests)
			return
		}
		defer limiter.ReleaseConn(clientIP)

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] Upgrade failed: %v", err)
			return
		}
		// Security hardening: limit inbound signaling message size to 256KB (F-08)
		conn.SetReadLimit(256 * 1024)

		sessionID := uuid.New().String()
		session := &Session{
			ID:       sessionID,
			RemoteIP: clientIP,
			Conn:     conn,
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		defer func() {
			cancel()
			session.mu.Lock()
			_ = conn.Close()
			session.Conn = nil
			session.mu.Unlock()
			if session.Role == "agent" && session.NodeID != "" {
				broker.RemoveAgent(session.NodeID, session)
			} else {
				broker.RemoveClient(session.ID)
			}
		}()

		// Keepalive: synchronized Pong handler to extend ReadDeadline
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
			return nil
		})

		// Keepalive: synchronized Ping handler to reply Pong under session.mu
		conn.SetPingHandler(func(appData string) error {
			session.mu.Lock()
			defer session.mu.Unlock()
			if session.Conn == nil {
				return nil
			}
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
			_ = conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
			return err
		})

		// Server-side Ping Pump: send Ping periodically to keep connection alive and refresh read deadlines
		go func() {
			ticker := time.NewTicker(defaultPingPeriod)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := session.Ping(); err != nil {
						return
					}
				}
			}
		}()

		for {
			_ = conn.SetReadDeadline(time.Now().Add(defaultReadTimeout))
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}
			atomic.AddUint64(&signalingMessagesTotal, 1)

			if !limiter.AllowMessage(clientIP) {
				log.Printf("[WS] Rate limit exceeded for IP %s", clientIP)
				_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: "rate limit exceeded, please slow down"})
				time.Sleep(50 * time.Millisecond)
				continue
			}

			switch msg.Type {
			case signaling.TypeRegister:
				// Agent registers NodeID
				if msg.NodeID == "" {
					_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: "node_id is required"})
					return
				}
				if limiter.IsLocked(clientIP) {
					log.Printf("[Broker] Preemptive lockout: IP %s blocked due to excessive failed attempts", clientIP)
					_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: "too many failed authentication attempts, IP throttled"})
					return
				}
				if err := broker.RegisterAgent(msg.NodeID, msg.Secret, msg.RequireVisitorAuth, session); err != nil {
					log.Printf("[Broker] Registration rejected for node_id=%s from IP %s: %v", msg.NodeID, clientIP, err)
					if limiter.RecordFailedAuth(clientIP) {
						_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: "too many failed authentication attempts, IP throttled"})
					} else {
						_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: err.Error()})
					}
					return
				}
				limiter.ResetFailedAuth(clientIP)
				session.Role = "agent"
				session.NodeID = msg.NodeID
				_ = session.Send(&signaling.Message{Type: signaling.TypeRegistered, NodeID: msg.NodeID})

				// Broadcast to any clients currently waiting for this NodeID
				broker.NotifyWaitingClients(msg.NodeID)

			case signaling.TypeJoin:
				// Visitor browser joins with NodeID
				session.Role = "client"
				session.NodeID = msg.NodeID
				broker.RegisterClient(session)

				agent := broker.GetAgent(msg.NodeID)
				requiresSecret := broker.RequiresVisitorAuth(msg.NodeID)
				if agent == nil {
					_ = session.Send(&signaling.Message{
						Type:           signaling.TypePeerStatus,
						NodeID:         msg.NodeID,
						Status:         "offline",
						RequiresSecret: requiresSecret,
						Error:          "agent is currently offline",
					})
				} else {
					_ = session.Send(&signaling.Message{
						Type:           signaling.TypePeerStatus,
						NodeID:         msg.NodeID,
						SessionID:      session.ID,
						Status:         "online",
						RequiresSecret: requiresSecret,
					})
				}

			case signaling.TypeOffer:
				// Client sends Offer to Agent
				msg.SessionID = session.ID
				msg.ClientIP = session.RemoteIP
				agent := broker.GetAgent(msg.NodeID)
				if agent == nil {
					_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: "target agent offline"})
				} else {
					_ = agent.Send(&msg)
				}

			case signaling.TypeAnswer:
				// Agent returns Answer to specific Client (Strict node attribution validation)
				if session.Role == "agent" {
					client := broker.GetClient(msg.SessionID)
					if client != nil && client.NodeID == session.NodeID {
						_ = client.Send(&msg)
					}
				}

			case signaling.TypeCandidate:
				// Relay ICE candidates
				if session.Role == "agent" {
					client := broker.GetClient(msg.SessionID)
					if client != nil && client.NodeID == session.NodeID {
						_ = client.Send(&msg)
					}
				} else {
					msg.SessionID = session.ID
					agent := broker.GetAgent(msg.NodeID)
					if agent != nil {
						_ = agent.Send(&msg)
					}
				}

			case signaling.TypeError:
				// Relay error messages (e.g. auth rejection from agent to client with attribution validation)
				if session.Role == "agent" && msg.SessionID != "" {
					client := broker.GetClient(msg.SessionID)
					if client != nil && client.NodeID == session.NodeID {
						_ = client.Send(&msg)
					}
				}
			}
		}
	})

	// Determine static asset source:
	// 1. If explicit or default -static dir exists on disk, serve from disk (allows hot-reloading in dev)
	// 2. Otherwise, serve embedded static files from web.DistFS
	var staticSource string
	var staticHandler http.Handler
	if *staticDir != "" {
		if abs, err := filepath.Abs(*staticDir); err == nil {
			if stat, err := os.Stat(abs); err == nil && stat.IsDir() {
				staticSource = fmt.Sprintf("Local Directory (%s)", abs)
				staticHandler = http.FileServer(http.Dir(abs))
			}
		}
	}
	if staticHandler == nil {
		if sub, err := fs.Sub(web.DistFS, "dist"); err == nil {
			staticSource = fmt.Sprintf("Embedded (%s)", Version)
			staticHandler = http.FileServer(http.FS(sub))
		}
	}

	if staticHandler != nil {
		mux.Handle("/", staticHandler)
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	var isTLS bool
	if (*tlsCert != "" && *tlsKey == "") || (*tlsCert == "" && *tlsKey != "") {
		log.Fatalf("[Server] Both -tls-cert and -tls-key must be provided together.")
	}

	if *tlsCert != "" && *tlsKey != "" {
		isTLS = true
		server.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	} else if *tlsAuto {
		cert, err := generateSelfSignedCert()
		if err != nil {
			log.Fatalf("[Server] Failed to generate self-signed certificate: %v", err)
		}
		isTLS = true
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	scheme := "http"
	wsScheme := "ws"
	tlsInfo := "Plain HTTP"
	if isTLS {
		scheme = "https"
		wsScheme = "wss"
		if *tlsCert != "" {
			tlsInfo = fmt.Sprintf("HTTPS Custom cert (%s)", *tlsCert)
		} else {
			tlsInfo = "HTTPS (Auto SAN cert)"
		}
	}

	host := "localhost"
	port := *addr
	if !strings.HasPrefix(*addr, ":") {
		if h, p, err := net.SplitHostPort(*addr); err == nil {
			if h != "" && h != "0.0.0.0" && h != "::" && h != "[::]" {
				host = h
			}
			port = ":" + p
		}
	}
	portalURL := fmt.Sprintf("%s://%s%s/", scheme, host, port)
	wsURL := fmt.Sprintf("%s://%s%s/ws", wsScheme, host, port)

	clusterAuthStr := "Disabled (Public)"
	if *signalSecret != "" {
		clusterAuthStr = "Enabled (Protected)"
	}

	trustedProxiesStr := "Default (RFC1918 Private CIDRs)"
	if *trustedProxies != "" {
		if *trustedProxies == "none" {
			trustedProxiesStr = "Disabled (None)"
		} else {
			trustedProxiesStr = *trustedProxies
		}
	}

	log.Printf("==================================================")
	log.Printf("  WebRTC Gateway Signal Server %s", Version)
	log.Printf("  Listening: %s (%s)", *addr, tlsInfo)
	log.Printf("  Web Portal: %s", portalURL)
	log.Printf("  Signaling WS: %s", wsURL)
	log.Printf("  Cluster Registration Auth: %s", clusterAuthStr)
	log.Printf("  Static Assets: %s", staticSource)
	log.Printf("  Limits: Max %d Conns/IP | Trusted Proxies: %s", *maxIPConns, trustedProxiesStr)
	log.Printf("==================================================")

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[Server] Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	if isTLS {
		if *tlsCert != "" && *tlsKey != "" {
			err = server.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = server.ListenAndServeTLS("", "")
		}
	} else {
		err = server.ListenAndServe()
	}

	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("[Server] Listen error: %v", err)
	}
}
