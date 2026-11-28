package signaling

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const (
	agentHandshakeTimeout = 10 * time.Second
	staticRequestDeadline = 15 * time.Second
	maxHTTPHeaderBytes    = 64 * 1024
	maxSDPMidLen          = 16
	maxAnswerSigLen       = 128
	maxErrorCodeLen       = 64
	maxErrorMsgLen        = 256
	MinSignalSecretBytes  = 16 // 128-bit minimum entropy for machine secrets
)

// ServerOptions configures the authoritative Go SignalServer.
type ServerOptions struct {
	Addr                 string
	StaticDir            string
	StaticFS             fs.FS
	TLSAuto              bool
	TLSCert              string
	TLSKey               string
	SignalSecret         string
	NodeSecrets          map[string]string // Optional per-node secrets (prevents cross-node hijacking)
	AllowForceRekey      *bool             // Optional override; default false for shared secret, true for per-node secrets
	AllowAnonymousAgents bool              // Fail-Closed: required if SignalSecret == "" && len(NodeSecrets) == 0
	BoundKeysFile        string            // Optional JSON file persisting Ed25519 node bindings
	PortalOrigin         string            // Explicit Portal origin, e.g. https://gw.example.com
	SandboxOrigin        string            // Explicit Sandbox origin, e.g. https://*.sandbox.example.net (Tier 1) or https://app.example.net (Tier 2)
	DevMode              bool              // Explicit local loopback dev mode (*.localhost)
	AllowSameSiteSandbox bool              // Tier 2 opt-in when wildcard sandbox shares eTLD+1 with portal
	AllowAllOrigins      bool
	MaxIPConns           int
	MaxIPMessages        int
	TrustedProxies       string
	ClientIPHeader       string // Optional single-value header from trusted proxies
	Version              string
}

// SignalServer is the authoritative HTTP/WebSocket signaling broker and static Portal/Sandbox host.
type SignalServer struct {
	opts            ServerOptions
	Broker          *SignalBroker
	Limiter         *IPRateLimiter
	Policy          *DomainPolicy
	manifest        assetManifest
	assets          assetStore
	trustedProxies  []netip.Prefix
	clientIPHeader  string
	nodeSecrets     map[string]string
	allowForceRekey bool
	messagesTotal   atomic.Uint64
	rootCtx         context.Context
	rootCancel      context.CancelFunc
	wsWG            sync.WaitGroup
}

// NewSignalServer validates all Fail-Closed invariants, pre-renders static assets, and builds a SignalServer.
func NewSignalServer(opts ServerOptions) (*SignalServer, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		opts.Addr = "127.0.0.1:8888"
	}
	if (opts.TLSCert != "") != (opts.TLSKey != "") {
		return nil, errors.New("signaling: both -tls-cert and -tls-key must be provided together (Fail-Closed)")
	}
	if opts.TLSAuto && (opts.TLSCert != "" || opts.TLSKey != "") {
		return nil, errors.New("signaling: -tls-auto cannot be combined with -tls-cert/-tls-key (Fail-Closed)")
	}
	if opts.SignalSecret != "" {
		if strings.TrimSpace(opts.SignalSecret) == "" {
			return nil, errors.New("signaling: SIGNAL_SECRET cannot be whitespace-only (Fail-Closed)")
		}
		if len(opts.SignalSecret) < MinSignalSecretBytes {
			return nil, fmt.Errorf("signaling: SIGNAL_SECRET must be at least %d bytes (Fail-Closed)", MinSignalSecretBytes)
		}
	}
	normNodeSecrets := make(map[string]string, len(opts.NodeSecrets))
	for k, v := range opts.NodeSecrets {
		nk, ok := NormalizeAndValidateNodeID(k)
		if !ok || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("signaling: invalid node_secrets entry for %q", k)
		}
		if len(v) < MinSignalSecretBytes {
			return nil, fmt.Errorf("signaling: node_secrets entry for %q must be at least %d bytes (Fail-Closed)", nk, MinSignalSecretBytes)
		}
		if _, dup := normNodeSecrets[nk]; dup {
			return nil, fmt.Errorf("signaling: duplicate normalized node_secrets entry for %q (Fail-Closed)", nk)
		}
		normNodeSecrets[nk] = v
	}
	if opts.SignalSecret == "" && len(normNodeSecrets) == 0 && !opts.AllowAnonymousAgents {
		return nil, errors.New("signaling: SIGNAL_SECRET (or node_secrets_file) is required unless -allow-anonymous-agents is explicitly enabled (Fail-Closed)")
	}
	trusted, err := ParseTrustedProxies(opts.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("signaling: %w", err)
	}
	clientIPHeader, err := NormalizeClientIPHeader(opts.ClientIPHeader)
	if err != nil {
		return nil, fmt.Errorf("signaling: %w", err)
	}
	tlsEnabled := opts.TLSAuto || (opts.TLSCert != "" && opts.TLSKey != "")
	policy, err := NewDomainPolicy(
		opts.Addr,
		opts.PortalOrigin,
		opts.SandboxOrigin,
		opts.DevMode,
		tlsEnabled,
		opts.AllowSameSiteSandbox,
		opts.AllowAllOrigins,
	)
	if err != nil {
		return nil, err
	}

	broker, err := NewSignalBrokerWithFile(opts.BoundKeysFile)
	if err != nil {
		return nil, fmt.Errorf("signaling: load bound keys: %w", err)
	}

	allowRekey := len(normNodeSecrets) > 0 && opts.SignalSecret == ""
	if opts.AllowForceRekey != nil {
		allowRekey = *opts.AllowForceRekey
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	srv := &SignalServer{
		opts:            opts,
		Broker:          broker,
		Limiter:         NewIPRateLimiter(opts.MaxIPConns, opts.MaxIPMessages),
		Policy:          policy,
		manifest:        defaultAssetManifest(),
		trustedProxies:  trusted,
		clientIPHeader:  clientIPHeader,
		nodeSecrets:     normNodeSecrets,
		allowForceRekey: allowRekey,
		rootCtx:         rootCtx,
		rootCancel:      rootCancel,
	}
	if err := srv.loadAndPreRenderAssets(); err != nil {
		rootCancel()
		broker.CloseAll()
		return nil, err
	}
	return srv, nil
}

// Handler returns the root HTTP handler (usable directly with httptest.NewServer).
func (s *SignalServer) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

// Shutdown closes active sessions cleanly (1001 GoingAway + single bound-key flush),
// then cancels rootCtx and waits boundedly for active WS handlers to exit their defers.
func (s *SignalServer) Shutdown() {
	s.Broker.CloseAll()
	s.rootCancel()

	done := make(chan struct{})
	go func() {
		s.wsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func isValidSDPMLineIndex(idx *int) bool {
	if idx == nil {
		return true
	}
	return *idx >= 0 && *idx <= math.MaxUint16
}

func applyStaticDeadlines(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(staticRequestDeadline)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	// Prevent net/http finishRequest() from blocking indefinitely draining an unexpected body.
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		w.Header().Set("Connection", "close")
	}
}

func clearStaticDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}

func headerContainsToken(h http.Header, key, want string) bool {
	for _, val := range h.Values(key) {
		for _, part := range strings.Split(val, ",") {
			if strings.EqualFold(strings.TrimSpace(part), want) {
				return true
			}
		}
	}
	return false
}

func isValidWSUpgradeRequest(r *http.Request) bool {
	if !r.ProtoAtLeast(1, 1) {
		return false
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") {
		return false
	}
	if !headerContainsToken(r.Header, "Upgrade", "websocket") {
		return false
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	return len(keys) == 1 && strings.TrimSpace(keys[0]) != ""
}

func (s *SignalServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Apply bounded read/write deadlines by default for ALL requests (cleared only right
	// before a validated RFC 6455 WebSocket upgrade) so failed upgrades or slow body drains
	// can never stall goroutines or file descriptors indefinitely.
	applyStaticDeadlines(w, r)

	// Step 1: Single-pass canonicalization on r.URL.EscapedPath().
	canonPath, err := CanonicalizeHTTPPath(r.URL.EscapedPath())
	if err != nil {
		http.Error(w, "Bad Request: invalid path", http.StatusBadRequest)
		return
	}

	// Step 2: Health check (GET/HEAD only).
	if canonPath == "/healthz" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			ver := s.opts.Version
			if ver == "" {
				ver = "dev"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": ver})
		}
		return
	}

	// Step 3: Strict Host allowlist (blocks DNS rebinding, direct IP bypass, sibling subdomain bypass).
	hostClass := s.Policy.ClassifyHost(r.Host)
	if hostClass == HostRejected {
		http.Error(w, "Misdirected Request: unauthorized Host header", http.StatusMisdirectedRequest)
		return
	}

	// Common security headers.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.Policy.IsSameSite {
		w.Header().Set("Origin-Agent-Cluster", "?1")
	}
	if s.Policy.IsHTTPS() && !s.Policy.LocalDevMode {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}

	// Step 4: Split WebSocket endpoints (/ws/agent/{node} & /ws/client/{node}).
	if strings.HasPrefix(canonPath, "/ws/") {
		if hostClass != HostPortal {
			http.Error(w, "Forbidden: signaling WebSocket disabled on sandbox host", http.StatusForbidden)
			return
		}
		s.handleWSRoute(w, r, canonPath)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Step 5: Strict Portal vs Sandbox static asset air-gap.
	switch hostClass {
	case HostPortal:
		s.servePortalAsset(w, r, canonPath)
	case HostSandbox:
		s.serveSandboxAsset(w, r, canonPath)
	}
}

func (s *SignalServer) handleWSRoute(w http.ResponseWriter, r *http.Request, canonPath string) {
	rest := strings.TrimPrefix(canonPath, "/ws/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	kind := parts[0]
	if kind != "agent" && kind != "client" {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	nodeID, ok := NormalizeAndValidateNodeID(parts[1])
	if !ok {
		http.Error(w, "Bad Request: invalid node_id", http.StatusBadRequest)
		return
	}

	// Validate Origin, method, absence of body, and RFC 6455 Upgrade headers BEFORE
	// burning AllowUpgrade or auth lockout quotas so cross-origin no-cors fetch floods
	// cannot exhaust a victim's NAT rate limit.
	if !s.Policy.CheckWSOrigin(r) {
		http.Error(w, "Forbidden: origin rejected", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "Bad Request: WebSocket upgrade must not carry a body", http.StatusBadRequest)
		return
	}
	if !isValidWSUpgradeRequest(r) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		http.Error(w, "Upgrade Required: valid WebSocket upgrade required", http.StatusUpgradeRequired)
		return
	}

	clientIP := ExtractClientIP(r, s.trustedProxies, s.clientIPHeader)
	if !s.Limiter.AllowUpgrade(clientIP) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	if kind == "agent" {
		s.handleAgentWS(w, r, nodeID, clientIP)
	} else {
		s.handleClientWS(w, r, nodeID, clientIP)
	}
}

// NewHTTPServer creates the hardened HTTP/HTTPS server for graceful shutdown.
func (s *SignalServer) NewHTTPServer() (*http.Server, error) {
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    maxHTTPHeaderBytes,
	}

	aeadCipherSuites := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}

	if s.opts.TLSCert != "" && s.opts.TLSKey != "" {
		prov, err := NewReloadingCertProvider(s.opts.TLSCert, s.opts.TLSKey)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			CipherSuites:   aeadCipherSuites,
			GetCertificate: prov.GetCertificate,
		}
	} else if s.opts.TLSAuto {
		var extraDNS []string
		if s.Policy.PortalHost != "" {
			extraDNS = append(extraDNS, s.Policy.PortalHost)
		}
		if s.Policy.SandboxHost != "" {
			extraDNS = append(extraDNS, s.Policy.SandboxHost)
			if s.Policy.Mode == SandboxModeWildcard {
				extraDNS = append(extraDNS, "*."+s.Policy.SandboxHost)
			}
		}
		cert, err := GenerateSelfSignedCert(extraDNS...)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			CipherSuites: aeadCipherSuites,
			Certificates: []tls.Certificate{cert},
		}
	}
	return srv, nil
}

// RunHealthCheck probes /healthz locally (IPv4 & IPv6 :: safe, bypassing HTTP_PROXY) or a custom URL.
func RunHealthCheck(addr string, tlsEnabled bool, customURL string) error {
	targetURL := strings.TrimSpace(customURL)
	isLocal := false
	if targetURL == "" {
		isLocal = true
		scheme := "http"
		if tlsEnabled {
			scheme = "https"
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			host = "127.0.0.1"
			port = "8888"
		}
		switch host {
		case "", "0.0.0.0":
			host = "127.0.0.1"
		case "::":
			host = "::1"
		}
		targetURL = fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, port))
	} else {
		if _, err := url.Parse(targetURL); err != nil {
			return err
		}
	}

	tr := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: isLocal && tlsEnabled, // #nosec G402 -- loopback-only healthcheck against local self-signed TLS
		},
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(targetURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// PrintStartupSummary logs clean operational diagnostics.
func (s *SignalServer) PrintStartupSummary() {
	mode := "tier1-cross-site-wildcard"
	switch {
	case s.Policy.LocalDevMode:
		mode = "local-dev (*.localhost)"
	case s.Policy.Mode == SandboxModeFixed:
		mode = "tier2-restricted-fixed-sandbox"
	case s.Policy.IsSameSite:
		mode = "tier2-same-site-wildcard (Origin-Agent-Cluster)"
	}
	var authMode string
	switch {
	case len(s.nodeSecrets) > 0 && s.opts.SignalSecret != "":
		authMode = fmt.Sprintf("PER-NODE SECRETS (%d nodes) + SIGNAL_SECRET fallback", len(s.nodeSecrets))
	case len(s.nodeSecrets) > 0:
		authMode = fmt.Sprintf("PER-NODE SECRETS (%d nodes)", len(s.nodeSecrets))
	case s.opts.SignalSecret != "":
		authMode = "SIGNAL_SECRET (Upgrade Bearer)"
	default:
		authMode = "ANONYMOUS (-allow-anonymous-agents)"
	}
	ipHeaderMode := "xff-only"
	if s.clientIPHeader != "" {
		ipHeaderMode = s.clientIPHeader
	}
	log.Printf("[Signal %s] Addr=%s | SandboxMode=%s | AgentAuth=%s | Limits=%d conns/IP, %d msgs/10s/IP | ClientIPHeader=%s",
		s.opts.Version, s.opts.Addr, mode, authMode, s.opts.MaxIPConns, s.opts.MaxIPMessages, ipHeaderMode)
}
