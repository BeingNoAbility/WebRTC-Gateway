package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

const (
	DefaultMaxGlobalStreams   = 512
	MaxConcurrentSessions     = 128
	MaxHandshakingSessions    = 32
	MaxHandshakingPerIP       = 4
	MaxOfferConcurrency       = 32
	UnauthSessionTimeout      = 10 * time.Second
	MaxStagedCandBytesSession = 32 * 1024
	MaxStagedCandBytesGlobal  = 1 * 1024 * 1024
	MaxStagedCandCountSession = 128
	DisconnectGraceTimeout    = 30 * time.Second
)

// Config configures the Intranet Agent.
type Config struct {
	NodeID              string
	SignalURL           string
	SignalSecret        string
	AllowInsecureSignal bool // Fail-Closed: required to send SignalSecret over non-loopback ws://
	ForceRekey          bool
	AccessSecret        string
	AllowPublic         bool // Fail-Closed: required if AccessSecret == ""
	Identity            *Identity
	StateFile           string // Optional explicit capability revocation persistence path
	HTTPProxy           *proxy.HTTPProxy
	WSProxy             *proxy.WSProxy
	ICEMDNS             bool // Default true: obfuscate local LAN/Docker/VPN IPs via mDNS
	STUNServers         []string
	TURNURL             string
	TURNUser            string
	TURNPass            string
	MaxGlobalStream     int
	PoolSize            int
	SendSignal          func(msg any) error // Injectable signaling sender for tests
}

type SessionState int

const (
	StateHandshaking SessionState = iota
	StateAuthenticated
	StateClosed
)

// VisitorSession tracks one visitor's WebRTC PeerConnection, PoolMux, SPAKE2+ auth state,
// and capability scope.
//
// LOCK HIERARCHY INVARIANT: ALWAYS acquire `a.sessMu` BEFORE `sess.mu`, NEVER reverse!
type VisitorSession struct {
	mu              sync.Mutex
	signalSessionID string
	clientIP        string
	state           SessionState
	authDone        atomic.Bool // Fast atomic gate blocking pre-auth Pool & WS DC frames!
	role            string      // "owner", "capability", or "public"

	visitorFP      string
	agentFP        string
	visitorNonce   string
	agentNonce     string
	spakeSess      *SPAKE2PVerifierSession
	challengeReady bool // True ONLY after DTLS cert FP verified and challenge state populated.
	authAttempt    bool
	authDispatched atomic.Bool
	capInFlight    atomic.Int32

	capScope  *proxy.CapabilityScope
	capJTI    string
	capExp    int64 // 0 = no expiry
	cookieKey string
	cookieJar *proxy.ProxyCookieJar

	pc          *webrtc.PeerConnection
	mux         *mux.Mux
	unauthTimer *time.Timer
	discTimer   *time.Timer

	offerInFlight bool
	stagedCands   []webrtc.ICECandidateInit
	stagedBytes   int64
	candCh        chan webrtc.ICECandidateInit
	candStop      chan struct{}
	candStopOnce  sync.Once
}

// Agent orchestrates signaling registration, WebRTC handshakes, SPAKE2+ / Ed25519 auth,
// and Pure Pipe HTTP/WS stream dispatching.
type Agent struct {
	cfg           Config
	webrtcAPI     *webrtc.API
	webrtcConfig  webrtc.Configuration
	spakeVerifier *SPAKE2PVerifierRecord
	globalMem     *mux.MemoryBudget
	cookieMgr     *proxy.SessionCookieManager
	authLimiter   *VisitorAuthLimiter
	capStore      *CapabilityStore
	metrics       *Metrics

	rootCtx       context.Context
	rootCancel    context.CancelFunc
	activeStreams atomic.Int64
	sigRegistered atomic.Bool
	stagedGlobal  atomic.Int64
	offerSem      chan struct{}

	sigMu   sync.RWMutex
	sigSend func(msg any) error

	// LOCK HIERARCHY: a.sessMu -> sess.mu (NEVER acquire a.sessMu while holding sess.mu!).
	sessMu   sync.Mutex
	sessions map[string]*VisitorSession // signalSessionID -> VisitorSession
}

func New(cfg Config) (*Agent, error) {
	normNode, ok := signaling.NormalizeAndValidateNodeID(cfg.NodeID)
	if !ok {
		return nil, fmt.Errorf("agent: invalid node_id %q", cfg.NodeID)
	}
	cfg.NodeID = normNode

	if cfg.AccessSecret == "" && !cfg.AllowPublic {
		return nil, errors.New("agent: ACCESS_SECRET is required unless -allow-public is explicitly set (Fail-Closed)")
	}
	if cfg.AccessSecret != "" && cfg.AllowPublic {
		return nil, errors.New("agent: ACCESS_SECRET and -allow-public are mutually exclusive (Fail-Closed)")
	}
	if cfg.Identity == nil {
		return nil, errors.New("agent: persistent Ed25519 Identity is required")
	}
	if cfg.HTTPProxy == nil {
		return nil, errors.New("agent: HTTPProxy is required")
	}
	if cfg.MaxGlobalStream <= 0 {
		cfg.MaxGlobalStream = DefaultMaxGlobalStreams
	}
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = protocol.DefaultPoolSize
	}

	if cfg.SignalSecret != "" && strings.TrimSpace(cfg.SignalSecret) == "" {
		return nil, errors.New("agent: SIGNAL_SECRET cannot be whitespace-only (Fail-Closed)")
	}
	if cfg.AccessSecret != "" && strings.TrimSpace(cfg.AccessSecret) == "" {
		return nil, errors.New("agent: ACCESS_SECRET cannot be whitespace-only (Fail-Closed)")
	}

	// Refuse sending SIGNAL_SECRET over plaintext non-loopback ws:// unless explicitly opted in.
	// Always validate trimmed SignalURL Fail-Closed (matching BuildAgentSignalURL).
	if strings.TrimSpace(cfg.SignalURL) != "" {
		builtURL, _, berr := BuildAgentSignalURL(cfg.SignalURL, cfg.NodeID)
		if berr != nil {
			return nil, fmt.Errorf("agent: invalid signal URL: %w", berr)
		}
		if cfg.SignalSecret != "" && !cfg.AllowInsecureSignal {
			u, uerr := url.Parse(builtURL)
			if uerr != nil {
				return nil, fmt.Errorf("agent: invalid parsed signal URL: %w", uerr)
			}
			sch := strings.ToLower(u.Scheme)
			if (sch == "ws" || sch == "http") && !signaling.IsLoopbackBindAddr(u.Host) && !isLoopbackHost(u.Hostname()) {
				return nil, fmt.Errorf("agent: refusing to send SIGNAL_SECRET over plaintext %s to non-loopback host %q (use wss:// or -allow-insecure-signal)", sch, u.Hostname())
			}
		}
	}

	// Precompute V7 SPAKE2+ verifier (w0, L=w1*G) salted with (nodeID, agentPKB64) ONCE at startup.
	var spakeRec *SPAKE2PVerifierRecord
	if cfg.AccessSecret != "" {
		rec, err := DeriveSPAKE2PVerifier(cfg.AccessSecret, cfg.NodeID, cfg.Identity.PublicKeyBase64())
		if err != nil {
			return nil, fmt.Errorf("agent: derive SPAKE2+ verifier: %w", err)
		}
		spakeRec = rec
		cfg.AccessSecret = "" // Wipe plaintext secret from memory after deriving verifier!
	}

	// Persistent capability revocation store (defaults to sibling of persistent Identity key).
	statePath := cfg.StateFile
	if statePath == "" && cfg.Identity.KeyPath != "" {
		statePath = cfg.Identity.KeyPath + ".state.json"
	}
	capStore, err := NewCapabilityStoreWithFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("agent: load capability state: %w", err)
	}

	se := webrtc.SettingEngine{}
	if cfg.ICEMDNS {
		se.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryAndGather)
	} else {
		se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))

	var iceServers []webrtc.ICEServer
	if len(cfg.STUNServers) > 0 {
		iceServers = append(iceServers, webrtc.ICEServer{URLs: cfg.STUNServers})
	}
	if cfg.TURNURL != "" {
		iceServers = append(iceServers, webrtc.ICEServer{
			URLs:           []string{cfg.TURNURL},
			Username:       cfg.TURNUser,
			Credential:     cfg.TURNPass,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	a := &Agent{
		cfg:           cfg,
		webrtcAPI:     api,
		webrtcConfig:  webrtc.Configuration{ICEServers: iceServers},
		spakeVerifier: spakeRec,
		globalMem:     mux.NewMemoryBudget(mux.DefaultGlobalMemoryBudget),
		cookieMgr:     proxy.NewSessionCookieManager(proxy.MaxCookieSessions),
		authLimiter:   NewVisitorAuthLimiter(),
		capStore:      capStore,
		metrics:       &Metrics{},
		rootCtx:       rootCtx,
		rootCancel:    rootCancel,
		offerSem:      make(chan struct{}, MaxOfferConcurrency),
		sigSend:       cfg.SendSignal,
		sessions:      make(map[string]*VisitorSession),
	}
	go a.expirySweepLoop()
	return a, nil
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(h); err == nil && ip.IsLoopback() {
		return true
	}
	return false
}

func (a *Agent) Identity() *Identity         { return a.cfg.Identity }
func (a *Agent) Metrics() *Metrics           { return a.metrics }
func (a *Agent) GlobalMemUsed() int64        { return a.globalMem.Used() }
func (a *Agent) RequiresVisitorAuth() bool   { return a.spakeVerifier != nil }
func (a *Agent) IsSignalingRegistered() bool { return a.sigRegistered.Load() }

func (a *Agent) sendSignal(msg any) error {
	a.sigMu.RLock()
	fn := a.sigSend
	a.sigMu.RUnlock()
	if fn == nil {
		return errors.New("agent: signaling sender not attached")
	}
	return fn(msg)
}

func (a *Agent) sendSignalError(sessionID, code, msg string) {
	_ = a.sendSignal(signaling.ErrorDTO{
		Type:      signaling.TypeError,
		NodeID:    a.cfg.NodeID,
		SessionID: sessionID,
		Code:      code,
		Error:     msg,
	})
}

// expirySweepLoop actively disconnects expired or revoked capability sessions and prunes stale records.
func (a *Agent) expirySweepLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.rootCtx.Done():
			return
		case now := <-ticker.C:
			// Reload external CLI changes OUTSIDE a.sessMu so no disk I/O or flock runs under session locks.
			a.capStore.MaybeReload()
			nowUnix := now.Unix()
			var victims []*VisitorSession
			a.sessMu.Lock()
			for _, s := range a.sessions {
				s.mu.Lock()
				expired := s.state == StateAuthenticated && s.capExp > 0 && nowUnix >= s.capExp
				revoked := s.state == StateAuthenticated && s.capJTI != "" && a.capStore.IsRevoked(s.capJTI)
				if expired || revoked {
					victims = append(victims, s)
				}
				s.mu.Unlock()
			}
			a.sessMu.Unlock()
			for _, s := range victims {
				a.closeSession(s)
			}
			a.capStore.PruneExpired(now)
		}
	}
}

// Close shuts down the Agent and all active visitor PeerConnections cleanly.
func (a *Agent) Close() {
	a.rootCancel()
	a.sessMu.Lock()
	all := make([]*VisitorSession, 0, len(a.sessions))
	for _, s := range a.sessions {
		all = append(all, s)
	}
	a.sessions = make(map[string]*VisitorSession)
	a.sessMu.Unlock()

	// Notify active sessions via GOAWAY before tearing down SCTP/DTLS sockets so browsers
	// can begin silent reconnect immediately without waiting on ICE consent timeouts.
	sentGoAway := false
	for _, s := range all {
		s.mu.Lock()
		m := s.mux
		closed := s.state == StateClosed
		s.mu.Unlock()
		if !closed && m != nil {
			if m.SendGoAway(protocol.ErrCodeGoingAway) == nil {
				sentGoAway = true
			}
		}
	}
	if sentGoAway {
		time.Sleep(50 * time.Millisecond)
	}

	for _, s := range all {
		a.closeSession(s)
	}
}

func (a *Agent) closeSession(sess *VisitorSession) {
	a.sessMu.Lock()
	sess.mu.Lock()
	if sess.state == StateClosed {
		sess.mu.Unlock()
		a.sessMu.Unlock()
		return
	}
	wasAuth := sess.state == StateAuthenticated
	sess.state = StateClosed
	sess.authDone.Store(false)
	sigID := sess.signalSessionID
	capJTI := sess.capJTI
	cookieKey := sess.cookieKey
	if sess.unauthTimer != nil {
		sess.unauthTimer.Stop()
		sess.unauthTimer = nil
	}
	if sess.discTimer != nil {
		sess.discTimer.Stop()
		sess.discTimer = nil
	}
	if sess.stagedBytes > 0 {
		a.stagedGlobal.Add(-sess.stagedBytes)
		sess.stagedBytes = 0
		sess.stagedCands = nil
	}
	if sess.candStop != nil {
		sess.candStopOnce.Do(func() { close(sess.candStop) })
	}
	m := sess.mux
	pc := sess.pc
	sess.mux = nil
	sess.pc = nil
	sess.spakeSess = nil
	sess.mu.Unlock()

	if cur, ok := a.sessions[sigID]; ok && cur == sess {
		delete(a.sessions, sigID)
	}
	a.sessMu.Unlock()

	if wasAuth {
		a.metrics.ActiveSessions.Add(-1)
	}
	if capJTI != "" {
		a.capStore.ReleaseConn(capJTI)
	}
	if cookieKey != "" {
		a.cookieMgr.Release(cookieKey)
	}
	if m != nil {
		m.Close()
	}
	if pc != nil {
		_ = pc.Close()
	}
}

func (a *Agent) disconnectCapabilitySessions(jti string) {
	var victims []*VisitorSession
	a.sessMu.Lock()
	for _, s := range a.sessions {
		s.mu.Lock()
		if s.capJTI == jti {
			victims = append(victims, s)
		}
		s.mu.Unlock()
	}
	a.sessMu.Unlock()

	for _, v := range victims {
		a.closeSession(v)
	}
}
