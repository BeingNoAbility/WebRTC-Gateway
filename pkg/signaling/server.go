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
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	maxHTTPHeaderBytes    = 64 * 1024
	maxSDPMidLen          = 16
	maxErrorCodeLen       = 64
	maxErrorMsgLen        = 256
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
		BridgeJS:  "/__p2p_gw__/bridge.js",
		PatchJS:   "/__p2p_gw__/patch.js",
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
}

// NewSignalServer validates all Fail-Closed invariants and builds a SignalServer.
func NewSignalServer(opts ServerOptions) (*SignalServer, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		opts.Addr = "127.0.0.1:8888"
	}
	if (opts.TLSCert != "") != (opts.TLSKey != "") {
		return nil, errors.New("signaling: both -tls-cert and -tls-key must be provided together (Fail-Closed)")
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

	// Default allowForceRekey=false under shared cluster SIGNAL_SECRET to prevent lateral node hijacking;
	// permit by default when strictly isolated per-node secrets are configured without a shared secret.
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
	srv.loadAssetManifest()
	return srv, nil
}

func (s *SignalServer) loadAssetManifest() {
	raw, err := s.readStaticFile("__p2p_gw__/manifest.json")
	if err != nil {
		return
	}
	var m assetManifest
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if strings.HasPrefix(m.PortalJS, "/__p2p_gw__/") {
		s.manifest.PortalJS = m.PortalJS
	}
	if strings.HasPrefix(m.PortalCSS, "/__p2p_gw__/") {
		s.manifest.PortalCSS = m.PortalCSS
	}
	if strings.HasPrefix(m.BridgeJS, "/__p2p_gw__/") {
		s.manifest.BridgeJS = m.BridgeJS
	}
	if strings.HasPrefix(m.PatchJS, "/__p2p_gw__/") {
		s.manifest.PatchJS = m.PatchJS
	}
	if strings.HasPrefix(m.SWJS, "/__p2p_gw__/") {
		s.manifest.SWJS = m.SWJS
	}
}

// Handler returns the root HTTP handler (usable directly with httptest.NewServer).
func (s *SignalServer) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *SignalServer) Shutdown() {
	s.rootCancel()
	s.Broker.CloseAll()
}

func (s *SignalServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 1: Single-pass canonicalization on r.URL.EscapedPath() (prevents double URL decode bypasses).
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
	if s.Policy.AllowSameSiteSandbox {
		w.Header().Set("Origin-Agent-Cluster", "?1")
	}
	if s.Policy.IsHTTPS(r) && !s.Policy.LocalDevMode {
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
	nodeID, ok := NormalizeAndValidateNodeID(parts[1])
	if !ok {
		http.Error(w, "Bad Request: invalid node_id", http.StatusBadRequest)
		return
	}

	clientIP := ExtractClientIP(r, s.trustedProxies, s.clientIPHeader)
	if !s.Limiter.AllowUpgrade(clientIP) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	switch kind {
	case "agent":
		s.handleAgentWS(w, r, nodeID, clientIP)
	case "client":
		s.handleClientWS(w, r, nodeID, clientIP)
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

func (s *SignalServer) verifyAgentUpgradeSecret(nodeID, authHdr string) (required bool, verified bool) {
	nodeSecret, hasNodeSecret := s.nodeSecrets[nodeID]
	clusterSecret := s.opts.SignalSecret
	if !hasNodeSecret && clusterSecret == "" {
		if len(s.nodeSecrets) > 0 {
			// Node-secrets-file is configured without a global fallback: fail-closed for unlisted nodes!
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
		// Per-node secret strictly overrides global cluster secret for this node (prevents lateral takeover).
		return true, subtle.ConstantTimeCompare(got, []byte(nodeSecret)) == 1
	}
	return true, subtle.ConstantTimeCompare(got, []byte(clusterSecret)) == 1
}

func (s *SignalServer) handleAgentWS(w http.ResponseWriter, r *http.Request, nodeID, clientIP string) {
	// HTTP Upgrade stage secret verification (rejects before spawning WS goroutines).
	secretVerified := false
	needsAuth := s.opts.SignalSecret != "" || len(s.nodeSecrets) > 0
	if needsAuth {
		if s.Limiter.IsAuthLocked(clientIP) {
			http.Error(w, "Too Many Requests: IP temporarily locked", http.StatusTooManyRequests)
			return
		}
		_, ok := s.verifyAgentUpgradeSecret(nodeID, r.Header.Get("Authorization"))
		if !ok {
			s.Limiter.RecordFailedAuth(clientIP)
			http.Error(w, "Unauthorized: invalid or missing Bearer secret", http.StatusUnauthorized)
			return
		}
		s.Limiter.ResetFailedAuth(clientIP)
		secretVerified = true
	}

	if r.Header.Get("Origin") != "" && !s.Policy.CheckWSOrigin(r, false) {
		http.Error(w, "Forbidden: origin rejected", http.StatusForbidden)
		return
	}
	if !s.Limiter.AcquireConn(clientIP) {
		http.Error(w, "Too Many Requests: IP connection limit reached", http.StatusTooManyRequests)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin & Host already strictly verified above!
	})
	if err != nil {
		s.Limiter.ReleaseConn(clientIP)
		return
	}
	conn.SetReadLimit(MaxWSMessageBytes)

	sess := NewSession(s.rootCtx, conn, RoleAgent, nodeID, newSessionID(), clientIP)
	defer func() {
		s.Broker.CleanupSession(sess)
		sess.Close(websocket.StatusNormalClosure, "")
		s.Limiter.ReleaseConn(clientIP)
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

	// Step 2: Steady-state Agent signaling loop.
	for {
		_, raw, err := conn.Read(sess.Context())
		if err != nil {
			return
		}
		if !s.Limiter.AllowMessage(clientIP) {
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
			if env.SessionID == "" || env.Desc == nil || env.Desc.Validate(false) != nil || env.PK == "" || env.Sig == "" {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: env.SessionID, Code: "INVALID_ANSWER", Error: "invalid answer descriptor or signature fields"})
				continue
			}
			// Enforce answer PK matches this registered Agent's verified Ed25519 identity.
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
			if env.SessionID == "" || len(env.Candidate) > MaxCandidateBytes || len(env.SDPMid) > maxSDPMidLen {
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
	if !s.Policy.CheckWSOrigin(r, false) {
		http.Error(w, "Forbidden: origin rejected", http.StatusForbidden)
		return
	}
	if !s.Limiter.AcquireConn(clientIP) {
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

	sess := NewSession(s.rootCtx, conn, RoleClient, nodeID, newSessionID(), clientIP)
	defer func() {
		s.Broker.CleanupSession(sess)
		sess.Close(websocket.StatusNormalClosure, "")
		s.Limiter.ReleaseConn(clientIP)
	}()

	online, reqAuth, agentPK, err := s.Broker.JoinClient(sess)
	if err != nil {
		code := "WAITING_QUEUE_FULL"
		if errors.Is(err, ErrNodeClientLimitFull) {
			code = "NODE_CLIENT_LIMIT"
		}
		sess.SendErrorAndClose(code, err.Error(), websocket.StatusCode(4029))
		return
	}
	if err := sess.TrySend(JoinedDTO{
		Type:               TypeJoined,
		NodeID:             nodeID,
		SessionID:          sess.ID,
		Online:             online,
		RequireVisitorAuth: reqAuth,
		AgentPK:            agentPK,
	}); err != nil {
		return
	}

	for {
		_, raw, err := conn.Read(sess.Context())
		if err != nil {
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
			allowed, prevHasOffer := sess.RecordOffer(time.Now())
			if !allowed {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "OFFER_RATE_LIMITED", Error: "too many offers"})
				continue
			}
			agent := s.Broker.GetAgent(nodeID)
			if agent == nil {
				sess.RollbackOffer(prevHasOffer)
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
				sess.RollbackOffer(prevHasOffer)
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "AGENT_BUSY", Error: "agent signaling queue busy"})
			}
		case TypeCandidate:
			if len(env.Candidate) > MaxCandidateBytes || len(env.SDPMid) > maxSDPMidLen {
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
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
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
			"default-src 'none'; script-src 'self'; connect-src 'self'; frame-ancestors %s; base-uri 'none'; object-src 'none'",
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
		// Prevent malicious sandbox pages from registering bridge.js as a shadowing Service Worker.
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Service-Worker")), "script") {
			http.Error(w, "Forbidden: bridge script cannot be registered as a Service Worker", http.StatusForbidden)
			return
		}
		s.serveStaticBinary(w, r, strings.TrimPrefix(s.manifest.BridgeJS, "/"), "application/javascript; charset=utf-8", assetCacheControl(s.manifest.BridgeJS))
	case s.manifest.PatchJS:
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Service-Worker")), "script") {
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

func (s *SignalServer) injectPortalMeta(htmlDoc string, r *http.Request) string {
	cfg := map[string]any{
		"portalDomain":  s.Policy.PortalDomain,
		"sandboxMode":   string(s.Policy.Mode),
		"sandboxDomain": s.Policy.SandboxDomain,
		"sameSiteMode":  s.Policy.AllowSameSiteSandbox,
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
	if idx := strings.Index(strings.ToLower(htmlDoc), "<head>"); idx >= 0 {
		insertAt := idx + len("<head>")
		return htmlDoc[:insertAt] + "\n  " + metaTag + htmlDoc[insertAt:]
	}
	return metaTag + "\n" + htmlDoc
}

func (s *SignalServer) injectBridgeMeta(htmlDoc string, r *http.Request) string {
	origins := s.Policy.AllowedPortalOriginsList(r)
	rawJSON, _ := json.Marshal(origins)
	metaTag := fmt.Sprintf(`<meta name="p2p-portal-origins" content="%s">`, html.EscapeString(string(rawJSON)))
	if idx := strings.Index(strings.ToLower(htmlDoc), "<head>"); idx >= 0 {
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

// RunHealthCheck probes /healthz locally (IPv4 & IPv6 :: safe) or a custom URL.
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
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: isLocal && tlsEnabled, // #nosec G402 -- loopback-only healthcheck against local self-signed TLS
		},
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
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
	case s.Policy.AllowSameSiteSandbox:
		mode = "tier2-same-site-wildcard (Origin-Agent-Cluster)"
	}
	authMode := "SIGNAL_SECRET (Upgrade Bearer)"
	if len(s.nodeSecrets) > 0 && s.opts.SignalSecret == "" {
		authMode = fmt.Sprintf("PER-NODE SECRETS (%d nodes)", len(s.nodeSecrets))
	} else if s.opts.SignalSecret == "" {
		authMode = "ANONYMOUS (-allow-anonymous-agents)"
	}
	ipHeaderMode := "xff-only"
	if s.clientIPHeader != "" {
		ipHeaderMode = s.clientIPHeader
	}
	log.Printf("[Signal %s] Addr=%s | SandboxMode=%s | AgentAuth=%s | Limits=%d conns/IP, %d msgs/10s/IP | ClientIPHeader=%s",
		s.opts.Version, s.opts.Addr, mode, authMode, s.opts.MaxIPConns, s.opts.MaxIPMessages, ipHeaderMode)
}
