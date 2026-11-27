package signaling

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
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
)

var (
	manifestPathRegex      = regexp.MustCompile(`^/__p2p_gw__/(?:assets/)?[A-Za-z0-9._-]+$`)
	manifestSubAssetPathRe = regexp.MustCompile(`^/__p2p_gw__/assets/[A-Za-z0-9._-]+$`)
)

type assetManifest struct {
	PortalJS  string `json:"portal_js"`
	PortalCSS string `json:"portal_css"`
	BridgeJS  string `json:"bridge_js"`
	PatchJS   string `json:"patch_js"`
	SWJS      string `json:"sw_js"`
}

func defaultAssetManifest() assetManifest {
	return assetManifest{
		PortalJS:  "/__p2p_gw__/portal.js",
		PortalCSS: "/__p2p_gw__/portal.css",
		BridgeJS:  "/__p2p_gw__/assets/bridge.js",
		PatchJS:   "/__p2p_gw__/assets/patch.js",
		SWJS:      "/__p2p_gw__/sw.js",
	}
}

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
	PortalDomain         string
	SandboxDomain        string // Tier 2/3 wildcard sandbox base domain
	SandboxHost          string // Tier 1 fixed single sandbox host (no wildcard cert needed)
	AllowSameSiteSandbox bool   // Tier 2 explicit opt-in (-allow-same-site-sandbox)
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
	trustedProxies  []netip.Prefix
	clientIPHeader  string
	nodeSecrets     map[string]string
	allowForceRekey bool
	messagesTotal   atomic.Uint64
	rootCtx         context.Context
	rootCancel      context.CancelFunc
	wsWG            sync.WaitGroup
}

// NewSignalServer validates all Fail-Closed invariants and builds a SignalServer.
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
	if opts.SignalSecret != "" && strings.TrimSpace(opts.SignalSecret) == "" {
		return nil, errors.New("signaling: SIGNAL_SECRET cannot be whitespace-only (Fail-Closed)")
	}
	normNodeSecrets := make(map[string]string, len(opts.NodeSecrets))
	for k, v := range opts.NodeSecrets {
		nk, ok := NormalizeAndValidateNodeID(k)
		if !ok || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("signaling: invalid node_secrets entry for %q", k)
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
	policy, err := NewDomainPolicy(
		opts.Addr,
		opts.PortalDomain,
		opts.SandboxDomain,
		opts.SandboxHost,
		opts.AllowSameSiteSandbox,
		opts.AllowAllOrigins,
		trusted,
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
	if err := srv.loadAssetManifest(); err != nil {
		rootCancel()
		broker.CloseAll()
		return nil, err
	}
	return srv, nil
}

func (s *SignalServer) loadAssetManifest() error {
	raw, err := s.readStaticFile("__p2p_gw__/manifest.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("signaling: read asset manifest: %w", err)
	}
	var m assetManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("signaling: invalid asset manifest JSON (Fail-Closed): %w", err)
	}
	apply := func(field, val string, re *regexp.Regexp, dst *string) error {
		if val == "" {
			return nil
		}
		if !re.MatchString(val) || strings.Contains(val, "..") {
			return fmt.Errorf("signaling: invalid manifest path for %s: %q", field, val)
		}
		*dst = val
		return nil
	}
	if err := apply("portal_js", m.PortalJS, manifestPathRegex, &s.manifest.PortalJS); err != nil {
		return err
	}
	if err := apply("portal_css", m.PortalCSS, manifestPathRegex, &s.manifest.PortalCSS); err != nil {
		return err
	}
	if err := apply("bridge_js", m.BridgeJS, manifestSubAssetPathRe, &s.manifest.BridgeJS); err != nil {
		return err
	}
	if err := apply("patch_js", m.PatchJS, manifestSubAssetPathRe, &s.manifest.PatchJS); err != nil {
		return err
	}
	if err := apply("sw_js", m.SWJS, manifestPathRegex, &s.manifest.SWJS); err != nil {
		return err
	}
	return nil
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
	// Prevent net/http finishRequest() from blocking indefinitely draining an unexpected body on GET/HEAD.
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		w.Header().Set("Connection", "close")
	}
}

func (s *SignalServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1: Single-pass canonicalization on r.URL.EscapedPath() (prevents double URL decode bypasses).
	canonPath, err := CanonicalizeHTTPPath(r.URL.EscapedPath())
	if err != nil {
		applyStaticDeadlines(w, r)
		http.Error(w, "Bad Request: invalid path", http.StatusBadRequest)
		return
	}

	// Step 2: Health check (GET/HEAD only).
	if canonPath == "/healthz" {
		applyStaticDeadlines(w, r)
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
		applyStaticDeadlines(w, r)
		http.Error(w, "Misdirected Request: unauthorized Host header", http.StatusMisdirectedRequest)
		return
	}

	// Common security headers.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.Policy.IsSameSite {
		w.Header().Set("Origin-Agent-Cluster", "?1")
	}
	if s.Policy.IsHTTPS(r) && !s.Policy.LocalDevMode {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}

	// Step 4: Split WebSocket endpoints (/ws/agent/{node} & /ws/client/{node}).
	if strings.HasPrefix(canonPath, "/ws/") {
		if hostClass != HostPortal {
			applyStaticDeadlines(w, r)
			http.Error(w, "Forbidden: signaling WebSocket disabled on sandbox host", http.StatusForbidden)
			return
		}
		s.handleWSRoute(w, r, canonPath)
		return
	}

	// All non-WS endpoints get bounded read/write deadlines to defeat Slow-Body-Drain & Slow-Read DoS.
	applyStaticDeadlines(w, r)

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
		applyStaticDeadlines(w, r)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	kind := parts[0]
	nodeID, ok := NormalizeAndValidateNodeID(parts[1])
	if !ok {
		applyStaticDeadlines(w, r)
		http.Error(w, "Bad Request: invalid node_id", http.StatusBadRequest)
		return
	}

	clientIP := ExtractClientIP(r, s.trustedProxies, s.clientIPHeader)
	if !s.Limiter.AllowUpgrade(clientIP) {
		applyStaticDeadlines(w, r)
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	switch kind {
	case "agent":
		s.handleAgentWS(w, r, nodeID, clientIP)
	case "client":
		s.handleClientWS(w, r, nodeID, clientIP)
	default:
		applyStaticDeadlines(w, r)
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

func (s *SignalServer) verifyAgentUpgradeSecret(nodeID, authHdr string) (required bool, verified bool) {
	nodeSecret, hasNodeSecret := s.nodeSecrets[nodeID]
	clusterSecret := s.opts.SignalSecret
	if !hasNodeSecret && clusterSecret == "" {
		if len(s.nodeSecrets) > 0 {
			return true, false
		}
		return false, false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHdr, prefix) {
		return true, false
	}
	got := []byte(authHdr[len(prefix):])
	if hasNodeSecret {
		return true, subtle.ConstantTimeCompare(got, []byte(nodeSecret)) == 1
	}
	return true, subtle.ConstantTimeCompare(got, []byte(clusterSecret)) == 1
}

func (s *SignalServer) handleAgentWS(w http.ResponseWriter, r *http.Request, nodeID, clientIP string) {
	// Reject forbidden browser Origin BEFORE auth failure accounting so cross-origin web pages
	// cannot lock out a victim's corporate NAT or IPv6 /64 via 10 unauthenticated WS probes!
	if len(r.Header.Values("Origin")) > 0 && !s.Policy.CheckWSOrigin(r) {
		applyStaticDeadlines(w, r)
		http.Error(w, "Forbidden: origin rejected", http.StatusForbidden)
		return
	}

	// HTTP Upgrade stage secret verification (rejects before spawning WS goroutines).
	secretVerified := false
	needsAuth := s.opts.SignalSecret != "" || len(s.nodeSecrets) > 0
	if needsAuth {
		if s.Limiter.IsAuthLocked(clientIP) {
			applyStaticDeadlines(w, r)
			http.Error(w, "Too Many Requests: IP temporarily locked", http.StatusTooManyRequests)
			return
		}
		_, ok := s.verifyAgentUpgradeSecret(nodeID, r.Header.Get("Authorization"))
		if !ok {
			s.Limiter.RecordFailedAuth(clientIP)
			applyStaticDeadlines(w, r)
			http.Error(w, "Unauthorized: invalid or missing Bearer secret", http.StatusUnauthorized)
			return
		}
		s.Limiter.ResetFailedAuth(clientIP)
		secretVerified = true
	}

	if !s.Limiter.AcquireAgentConn(clientIP) {
		applyStaticDeadlines(w, r)
		http.Error(w, "Too Many Requests: Agent connection limit reached", http.StatusTooManyRequests)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin & Host already strictly verified above!
	})
	if err != nil {
		s.Limiter.ReleaseAgentConn(clientIP)
		return
	}
	conn.SetReadLimit(MaxWSMessageBytes)

	s.wsWG.Add(1)
	defer s.wsWG.Done()

	sess := NewSession(s.rootCtx, conn, RoleAgent, nodeID, newSessionID(), clientIP)
	defer func() {
		s.Broker.CleanupSession(sess)
		sess.Close(websocket.StatusNormalClosure, "")
		s.Limiter.ReleaseAgentConn(clientIP)
	}()

	// Step 1: Send Ed25519 ownership challenge and enforce 10s handshake timeout.
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		sess.Close(websocket.StatusInternalError, "rng failure")
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	if err := sess.TrySend(ChallengeDTO{Type: TypeChallenge, NodeID: nodeID, Nonce: nonce}); err != nil {
		return
	}

	hctx, hcancel := context.WithTimeout(sess.Context(), agentHandshakeTimeout)
	_, rawReg, err := conn.Read(hctx)
	hcancel()
	if err != nil {
		sess.SendErrorAndClose("HANDSHAKE_TIMEOUT", "agent registration timed out", websocket.StatusCode(4008))
		return
	}

	var regEnv InboundEnvelope
	if err := json.Unmarshal(rawReg, &regEnv); err != nil || regEnv.Type != TypeRegister {
		sess.SendErrorAndClose("INVALID_REGISTER", "expected register message", websocket.StatusPolicyViolation)
		return
	}
	pk, err := VerifyAgentRegistrationSignature(nodeID, nonce, regEnv.PK, regEnv.Sig)
	if err != nil {
		sess.SendErrorAndClose("INVALID_SIGNATURE", err.Error(), websocket.StatusCode(4003))
		return
	}
	allowRekey := secretVerified && s.allowForceRekey && regEnv.ForceRekey
	if err := s.Broker.RegisterAgent(sess, pk, regEnv.RequireVisitorAuth, allowRekey); err != nil {
		code := "REGISTER_FAILED"
		if errors.Is(err, ErrNodeOwnershipConflict) {
			code = "NODE_OWNERSHIP_CONFLICT"
		} else if errors.Is(err, ErrBoundKeyCapacityFull) {
			code = "REGISTRY_FULL"
		}
		sess.SendErrorAndClose(code, err.Error(), websocket.StatusCode(4003))
		return
	}
	if err := sess.TrySend(RegisteredDTO{Type: TypeRegistered, NodeID: nodeID}); err != nil {
		return
	}

	// Authenticated Agent gets its own high-throughput per-session message limiter (decoupled from visitor IP buckets).
	agentMsgLim := NewSessionMessageLimiter(DefaultMaxAgentSessionMsgs)

	// Step 2: Steady-state Agent signaling loop.
	for {
		_, raw, err := conn.Read(sess.Context())
		if err != nil {
			return
		}
		// Immediately stop forwarding if this Agent was evicted/replaced during async close window.
		if sess.IsEvicted() || s.Broker.GetAgent(nodeID) != sess {
			return
		}
		if !agentMsgLim.Allow(time.Now()) {
			_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, Code: "RATE_LIMITED", Error: "rate limit exceeded"})
			continue
		}
		s.messagesTotal.Add(1)

		var env InboundEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			sess.SendErrorAndClose("INVALID_JSON", "malformed JSON frame", websocket.StatusPolicyViolation)
			return
		}
		switch env.Type {
		case TypePing:
			_ = sess.TrySend(PongDTO{Type: TypePong})
		case TypeAnswer:
			if env.SessionID == "" || env.Desc == nil || env.Desc.Validate(false) != nil || env.PK == "" || env.Sig == "" || len(env.Sig) > maxAnswerSigLen {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: env.SessionID, Code: "INVALID_ANSWER", Error: "invalid answer descriptor or signature fields"})
				continue
			}
			if env.PK != sess.AgentPKBase64() {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: env.SessionID, Code: "PK_MISMATCH", Error: "answer pk does not match registered agent identity"})
				continue
			}
			client := s.Broker.GetClientForNode(nodeID, env.SessionID)
			if client == nil {
				continue
			}
			client.ClearOfferPending()
			_ = client.TrySend(AnswerRelayDTO{
				Type:      TypeAnswer,
				NodeID:    nodeID,
				SessionID: client.ID,
				Desc:      env.Desc,
				PK:        env.PK,
				Sig:       env.Sig,
			})
		case TypeCandidate:
			if env.SessionID == "" || len(env.Candidate) > MaxCandidateBytes || len(env.SDPMid) > maxSDPMidLen || !isValidSDPMLineIndex(env.SDPMLineIndex) {
				continue
			}
			client := s.Broker.GetClientForNode(nodeID, env.SessionID)
			if client == nil {
				continue
			}
			_ = client.TrySend(CandidateRelayDTO{
				Type:          TypeCandidate,
				NodeID:        nodeID,
				SessionID:     client.ID,
				Candidate:     env.Candidate,
				SDPMid:        env.SDPMid,
				SDPMLineIndex: env.SDPMLineIndex,
			})
		case TypeError:
			if env.SessionID == "" {
				continue
			}
			if len(env.Code) > maxErrorCodeLen {
				env.Code = truncateUTF8Bytes(env.Code, maxErrorCodeLen)
			}
			if len(env.Error) > maxErrorMsgLen {
				env.Error = truncateUTF8Bytes(env.Error, maxErrorMsgLen)
			}
			client := s.Broker.GetClientForNode(nodeID, env.SessionID)
			if client == nil {
				continue
			}
			client.ClearOfferPending()
			_ = client.TrySend(ErrorDTO{
				Type:      TypeError,
				NodeID:    nodeID,
				SessionID: client.ID,
				Code:      env.Code,
				Error:     env.Error,
			})
		default:
			_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, Code: "UNSUPPORTED_TYPE", Error: "unsupported agent message type"})
		}
	}
}

func (s *SignalServer) handleClientWS(w http.ResponseWriter, r *http.Request, nodeID, clientIP string) {
	if !s.Policy.CheckWSOrigin(r) {
		applyStaticDeadlines(w, r)
		http.Error(w, "Forbidden: origin rejected", http.StatusForbidden)
		return
	}
	if !s.Limiter.AcquireConn(clientIP) {
		applyStaticDeadlines(w, r)
		http.Error(w, "Too Many Requests: IP connection limit reached", http.StatusTooManyRequests)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		s.Limiter.ReleaseConn(clientIP)
		return
	}
	conn.SetReadLimit(MaxWSMessageBytes)

	s.wsWG.Add(1)
	defer s.wsWG.Done()

	sess := NewSession(s.rootCtx, conn, RoleClient, nodeID, newSessionID(), clientIP)
	defer func() {
		s.Broker.CleanupSession(sess)
		sess.Close(websocket.StatusNormalClosure, "")
		s.Limiter.ReleaseConn(clientIP)
	}()

	// JoinClient atomically enqueues JoinedDTO inside b.mu before any PeerStatusDTO can race ahead.
	if _, _, _, err := s.Broker.JoinClient(sess); err != nil {
		code := "WAITING_QUEUE_FULL"
		if errors.Is(err, ErrNodeClientLimitFull) {
			code = "NODE_CLIENT_LIMIT"
		} else if errors.Is(err, ErrBrokerClosed) {
			code = "SERVER_SHUTTING_DOWN"
		}
		sess.SendErrorAndClose(code, err.Error(), websocket.StatusCode(4029))
		return
	}

	for {
		_, raw, err := conn.Read(sess.Context())
		if err != nil {
			return
		}
		if sess.IsEvicted() {
			return
		}
		if !s.Limiter.AllowMessage(clientIP) {
			_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "RATE_LIMITED", Error: "rate limit exceeded"})
			continue
		}
		s.messagesTotal.Add(1)

		var env InboundEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			sess.SendErrorAndClose("INVALID_JSON", "malformed JSON frame", websocket.StatusPolicyViolation)
			return
		}

		switch env.Type {
		case TypePing:
			_ = sess.TrySend(PongDTO{Type: TypePong})
		case TypeOffer:
			if env.Desc == nil || env.Desc.Validate(true) != nil || len(env.Nonce) < 16 || len(env.Nonce) > 128 {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "INVALID_OFFER", Error: "invalid offer descriptor or nonce"})
				continue
			}
			allowed, prevHasOffer, prevOfferPend := sess.RecordOffer(time.Now())
			if !allowed {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "OFFER_RATE_LIMITED", Error: "too many offers"})
				continue
			}
			agent := s.Broker.GetAgent(nodeID)
			if agent == nil {
				sess.RollbackOffer(prevHasOffer, prevOfferPend)
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "PEER_OFFLINE", Error: "agent offline"})
				continue
			}
			err := agent.TrySend(OfferRelayDTO{
				Type:      TypeOffer,
				NodeID:    nodeID,
				SessionID: sess.ID,
				ClientIP:  clientIP,
				Desc:      env.Desc,
				Nonce:     env.Nonce,
			})
			if err != nil {
				sess.RollbackOffer(prevHasOffer, prevOfferPend)
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "AGENT_BUSY", Error: "agent signaling queue busy"})
			}
		case TypeCandidate:
			if len(env.Candidate) > MaxCandidateBytes || len(env.SDPMid) > maxSDPMidLen || !isValidSDPMLineIndex(env.SDPMLineIndex) {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "CANDIDATE_TOO_LARGE", Error: "candidate or sdp_mid exceeds limit"})
				continue
			}
			allowed, preOffer := sess.AllowCandidate()
			if !allowed {
				code := "CANDIDATE_LIMIT"
				msg := "candidate quota exceeded"
				if preOffer {
					code = "PRE_OFFER_CANDIDATE"
					msg = "candidate rejected before offer"
				}
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: code, Error: msg})
				continue
			}
			sess.TouchActivity(time.Now())
			agent := s.Broker.GetAgent(nodeID)
			if agent == nil {
				continue
			}
			if err := agent.TrySend(CandidateRelayDTO{
				Type:          TypeCandidate,
				NodeID:        nodeID,
				SessionID:     sess.ID,
				Candidate:     env.Candidate,
				SDPMid:        env.SDPMid,
				SDPMLineIndex: env.SDPMLineIndex,
			}); err != nil {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "AGENT_BUSY", Error: "agent signaling queue busy"})
			}
		default:
			_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "UNSUPPORTED_TYPE", Error: "unsupported client message type"})
		}
	}
}

func assetCacheControl(urlPath string) string {
	// Content-hashed filenames (e.g. /__p2p_gw__/portal.ABC12345.js) are safely immutable.
	base := filepath.Base(urlPath)
	if strings.Count(base, ".") >= 2 {
		return "public, max-age=31536000, immutable"
	}
	return "no-cache"
}

func isForbiddenWorkerScriptRequest(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Service-Worker")), "script") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest"))) {
	case "serviceworker", "worker", "sharedworker":
		return true
	}
	return false
}

func (s *SignalServer) servePortalAsset(w http.ResponseWriter, r *http.Request, canonPath string) {
	switch canonPath {
	case "/", "/index.html":
		data, err := s.readStaticFile("index.html")
		if err != nil {
			http.Error(w, "Portal index.html not found", http.StatusNotFound)
			return
		}
		injected := s.injectPortalMeta(string(data), r)
		csp := fmt.Sprintf(
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-src %s; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'self'",
			s.Policy.SandboxFrameSrc(r),
		)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, injected)
		}
	case s.manifest.PortalJS:
		s.serveStaticBinary(w, r, strings.TrimPrefix(s.manifest.PortalJS, "/"), "application/javascript; charset=utf-8", assetCacheControl(s.manifest.PortalJS))
	case s.manifest.PortalCSS:
		s.serveStaticBinary(w, r, strings.TrimPrefix(s.manifest.PortalCSS, "/"), "text/css; charset=utf-8", assetCacheControl(s.manifest.PortalCSS))
	case "/__p2p_gw__/favicon.svg", "/favicon.svg":
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("X-Frame-Options", "DENY")
		s.serveStaticBinary(w, r, "favicon.svg", "image/svg+xml", "public, max-age=3600")
	default:
		// Strict Zero-SW air-gap on Portal origin: /sw.js, /__p2p_gw__/sw.js, /__p2p_gw__/bridge.html etc. MUST 404.
		http.NotFound(w, r)
	}
}

func (s *SignalServer) serveSandboxAsset(w http.ResponseWriter, r *http.Request, canonPath string) {
	switch canonPath {
	case s.manifest.SWJS:
		data, err := s.readStaticFile(strings.TrimPrefix(s.manifest.SWJS, "/"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		originsJSON, _ := json.Marshal(s.Policy.AllowedPortalOriginsList(r))
		patchJSON, _ := json.Marshal(s.manifest.PatchJS)
		preamble := fmt.Sprintf(
			"self.__P2P_GW_PORTAL_ORIGINS__=%s;self.__P2P_GW_PATCH_URL__=%s;\n",
			originsJSON,
			patchJSON,
		)
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'")
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Add("Vary", "Host, X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, preamble)
			_, _ = w.Write(data)
		}
	case "/__p2p_gw__/bridge.html":
		data, err := s.readStaticFile("__p2p_gw__/bridge.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		injected := s.injectBridgeMeta(string(data), r)
		csp := fmt.Sprintf(
			"default-src 'none'; script-src 'self'; connect-src 'self'; frame-ancestors %s; base-uri 'none'; object-src 'none'; form-action 'none'",
			s.Policy.PortalFrameAncestors(r),
		)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, injected)
		}
	case s.manifest.BridgeJS:
		// Always vary on Service-Worker & Sec-Fetch-Dest so immutable HTTP/CDN caches can NEVER
		// serve bridge.js to a ServiceWorker registration probe (defense-in-depth alongside /assets/ scope containment).
		w.Header().Set("Vary", "Service-Worker, Sec-Fetch-Dest")
		if isForbiddenWorkerScriptRequest(r) {
			http.Error(w, "Forbidden: bridge script cannot be registered as a Service Worker", http.StatusForbidden)
			return
		}
		s.serveStaticBinary(w, r, strings.TrimPrefix(s.manifest.BridgeJS, "/"), "application/javascript; charset=utf-8", assetCacheControl(s.manifest.BridgeJS))
	case s.manifest.PatchJS:
		w.Header().Set("Vary", "Service-Worker, Sec-Fetch-Dest")
		if isForbiddenWorkerScriptRequest(r) {
			http.Error(w, "Forbidden: patch script cannot be registered as a Service Worker", http.StatusForbidden)
			return
		}
		s.serveStaticBinary(w, r, strings.TrimPrefix(s.manifest.PatchJS, "/"), "application/javascript; charset=utf-8", assetCacheControl(s.manifest.PatchJS))
	default:
		http.NotFound(w, r)
	}
}

func (s *SignalServer) serveStaticBinary(w http.ResponseWriter, r *http.Request, relPath, fallbackMime, cacheCtrl string) {
	data, err := s.readStaticFile(relPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := fallbackMime
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(relPath))
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", cacheCtrl)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (s *SignalServer) readStaticFile(relPath string) ([]byte, error) {
	cleanRel := filepath.ToSlash(filepath.Clean(relPath))
	if strings.HasPrefix(cleanRel, "..") || strings.HasPrefix(cleanRel, "/") {
		return nil, os.ErrNotExist
	}
	if s.opts.StaticDir != "" {
		return os.ReadFile(filepath.Join(s.opts.StaticDir, filepath.FromSlash(cleanRel)))
	}
	if s.opts.StaticFS != nil {
		return fs.ReadFile(s.opts.StaticFS, "dist/"+cleanRel)
	}
	return nil, os.ErrNotExist
}

// indexASCIILower finds ASCII-only needle (lowercase) inside s without allocating or altering UTF-8 byte offsets.
func indexASCIILower(s, needle string) int {
	n := len(needle)
	if n == 0 || len(s) < n {
		return -1
	}
	for i := 0; i <= len(s)-n; i++ {
		match := true
		for j := 0; j < n; j++ {
			c := s[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 32
			}
			if c != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func (s *SignalServer) injectPortalMeta(htmlDoc string, r *http.Request) string {
	cfg := map[string]any{
		"portalDomain":  s.Policy.PortalDomain,
		"sandboxMode":   string(s.Policy.Mode),
		"sandboxDomain": s.Policy.SandboxDomain,
		"sameSiteMode":  s.Policy.IsSameSite,
		"localDevMode":  s.Policy.LocalDevMode,
		"version":       s.opts.Version,
	}
	if s.Policy.Mode == SandboxModeFixed {
		cfg["sandboxHost"] = s.Policy.SandboxDomain
	}
	if s.Policy.LocalDevMode {
		_, port, _ := splitHostPortLower(r.Host)
		cfg["localPort"] = port
	}
	rawJSON, _ := json.Marshal(cfg)
	metaTag := fmt.Sprintf(`<meta name="p2p-config" content="%s">`, html.EscapeString(string(rawJSON)))
	if idx := indexASCIILower(htmlDoc, "<head>"); idx >= 0 {
		insertAt := idx + len("<head>")
		return htmlDoc[:insertAt] + "\n  " + metaTag + htmlDoc[insertAt:]
	}
	return metaTag + "\n" + htmlDoc
}

func (s *SignalServer) injectBridgeMeta(htmlDoc string, r *http.Request) string {
	origins := s.Policy.AllowedPortalOriginsList(r)
	rawJSON, _ := json.Marshal(origins)
	metaTag := fmt.Sprintf(`<meta name="p2p-portal-origins" content="%s">`, html.EscapeString(string(rawJSON)))
	if idx := indexASCIILower(htmlDoc, "<head>"); idx >= 0 {
		insertAt := idx + len("<head>")
		return htmlDoc[:insertAt] + "\n  " + metaTag + htmlDoc[insertAt:]
	}
	return metaTag + "\n" + htmlDoc
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
	mode := "tier3-cross-site-wildcard"
	switch {
	case s.Policy.LocalDevMode:
		mode = "local-dev (*.localhost)"
	case s.Policy.Mode == SandboxModeFixed:
		mode = "tier1-fixed-dual-domain"
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
