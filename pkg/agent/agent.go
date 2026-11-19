package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

type Agent struct {
	NodeID       string
	SignalURL    string
	SignalSecret string
	AccessSecret string
	Target       string
	ProxyURL     string
	SNI          string
	InsecureTLS  bool

	wsConn *websocket.Conn
	wsMu   sync.Mutex

	pingInterval time.Duration
	readTimeout  time.Duration

	cookieManager *proxy.SessionCookieManager
	httpProxy     *proxy.HTTPProxy
	wsProxy       *proxy.WSProxy
	fileProxy     *proxy.FileProxy

	webrtcConfig webrtc.Configuration
	sessionMu    sync.RWMutex
	sessions     map[string]*AgentSession

	maxConcurrentStreams int
	activeStreams        int64
	streamRejections     uint64
	rxBytesTotal         uint64
	txBytesTotal         uint64
	activePeers          int64
}

type countedTransport struct {
	*webrtc.DataChannel
	txCounter *uint64
}

func (c *countedTransport) Send(b []byte) error {
	if c.txCounter != nil {
		atomic.AddUint64(c.txCounter, uint64(len(b)))
	}
	return c.DataChannel.Send(b)
}

func New(nodeID, signalURL, signalSecret, accessSecret, target string) *Agent {
	cm := proxy.NewSessionCookieManager()
	var hp *proxy.HTTPProxy
	var wp *proxy.WSProxy
	if target != "" {
		var err error
		hp, err = proxy.NewHTTPProxy(target, cm)
		if err != nil {
			log.Fatalf("[Agent] Failed to init HTTPProxy: %v", err)
		}
		wp, err = proxy.NewWSProxy(target, cm)
		if err != nil {
			log.Fatalf("[Agent] Failed to init WSProxy: %v", err)
		}
	}

	return &Agent{
		NodeID:        nodeID,
		SignalURL:     signalURL,
		SignalSecret:  signalSecret,
		AccessSecret:  accessSecret,
		Target:        target,
		cookieManager: cm,
		httpProxy:     hp,
		wsProxy:       wp,
		webrtcConfig: webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{URLs: []string{"stun:stun.qq.com:18302"}},
				{URLs: []string{"stun:stun.miwifi.com:3478"}},
				{URLs: []string{"stun:stun.bilibili.com:3478"}},
				{URLs: []string{"stun:stun.cloudflare.com:3478"}},
				{URLs: []string{"stun:stun.l.google.com:19302"}},
			},
		},
		sessions:             make(map[string]*AgentSession),
		maxConcurrentStreams: 512,
		pingInterval:         25 * time.Second,
		readTimeout:          60 * time.Second,
	}
}

// SetProxy configures an explicit proxy (http://, https://, or socks5://) strictly for signaling connections.
func (a *Agent) SetProxy(proxyURL string) {
	a.ProxyURL = proxyURL
}

// SetAllowedProxyDomains configures allowed domain patterns for cross-origin proxying.
func (a *Agent) SetAllowedProxyDomains(domains []string) {
	if a.httpProxy != nil {
		a.httpProxy.SetAllowedDomains(domains)
	}
	if a.wsProxy != nil {
		a.wsProxy.SetAllowedDomains(domains)
	}
}

// SetSTUN configures custom STUN servers for WebRTC ICE candidate discovery.
func (a *Agent) SetSTUN(stunServers []string) {
	if len(stunServers) == 0 {
		return
	}
	var servers []webrtc.ICEServer
	for _, s := range stunServers {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.HasPrefix(s, "stun:") {
			s = "stun:" + s
		}
		servers = append(servers, webrtc.ICEServer{URLs: []string{s}})
	}
	// Preserve any configured TURN servers
	for _, s := range a.webrtcConfig.ICEServers {
		for _, u := range s.URLs {
			if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
				servers = append(servers, s)
				break
			}
		}
	}
	a.webrtcConfig.ICEServers = servers
}

// SetTURN configures a TURN relay server (UDP/TCP/TLS) to bypass symmetric NAT.
func (a *Agent) SetTURN(turnURL, turnUser, turnPass string) {
	turnURL = strings.TrimSpace(turnURL)
	if turnURL == "" {
		return
	}
	if !strings.HasPrefix(turnURL, "turn:") && !strings.HasPrefix(turnURL, "turns:") {
		turnURL = "turn:" + turnURL
	}
	server := webrtc.ICEServer{
		URLs: []string{turnURL},
	}
	if turnUser != "" {
		server.Username = turnUser
		server.Credential = turnPass
		server.CredentialType = webrtc.ICECredentialTypePassword
	}
	a.webrtcConfig.ICEServers = append(a.webrtcConfig.ICEServers, server)
	log.Printf("[Agent] Configured TURN relay: %s (user: %s)", turnURL, turnUser)
}

// SetSingleFile configures the agent to serve a single file directly in memory over WebRTC DataChannels.
func (a *Agent) SetSingleFile(filePath string) error {
	fp, err := proxy.NewSingleFileProxy(filePath)
	if err != nil {
		return err
	}
	a.fileProxy = fp
	return nil
}

// SetDirectory configures the agent to serve a local directory tree directly in memory over WebRTC DataChannels.
func (a *Agent) SetDirectory(dirPath string) error {
	fp, err := proxy.NewDirProxy(dirPath)
	if err != nil {
		return err
	}
	a.fileProxy = fp
	return nil
}

// FileProxy returns the active FileProxy if running in file/directory serving mode.
func (a *Agent) FileProxy() *proxy.FileProxy {
	return a.fileProxy
}

// SetMaxConcurrentStreams sets the maximum number of concurrent multiplexed streams (default 512).
func (a *Agent) SetMaxConcurrentStreams(limit int) {
	if limit > 0 {
		a.maxConcurrentStreams = limit
	}
}

// MaxConcurrentStreams returns the configured concurrent streams limit.
func (a *Agent) MaxConcurrentStreams() int {
	return a.maxConcurrentStreams
}

// ActiveStreams returns the current number of active proxy streams.
func (a *Agent) ActiveStreams() int64 {
	return atomic.LoadInt64(&a.activeStreams)
}

// StreamRejections returns the total count of rejected streams due to capacity limits.
func (a *Agent) StreamRejections() uint64 {
	return atomic.LoadUint64(&a.streamRejections)
}

// TrafficBytes returns the total RX and TX bytes transferred over WebRTC DataChannels.
func (a *Agent) TrafficBytes() (rx, tx uint64) {
	return atomic.LoadUint64(&a.rxBytesTotal), atomic.LoadUint64(&a.txBytesTotal)
}

// ActivePeers returns the count of active WebRTC peer connections.
func (a *Agent) ActivePeers() int64 {
	return atomic.LoadInt64(&a.activePeers)
}

// ExportPrometheusMetrics renders the current agent metrics in standard Prometheus text format.
func (a *Agent) ExportPrometheusMetrics() string {
	rx, tx := a.TrafficBytes()
	var b strings.Builder
	b.WriteString("# HELP webrtc_gateway_active_peers Current number of active WebRTC peer connections\n")
	b.WriteString("# TYPE webrtc_gateway_active_peers gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_active_peers{node_id=%q} %d\n\n", a.NodeID, a.ActivePeers()))

	b.WriteString("# HELP webrtc_gateway_active_streams Current number of active P2PMux logical streams\n")
	b.WriteString("# TYPE webrtc_gateway_active_streams gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_active_streams{node_id=%q} %d\n\n", a.NodeID, a.ActiveStreams()))

	b.WriteString("# HELP webrtc_gateway_max_streams Configured maximum concurrent streams threshold\n")
	b.WriteString("# TYPE webrtc_gateway_max_streams gauge\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_max_streams{node_id=%q} %d\n\n", a.NodeID, a.maxConcurrentStreams))

	b.WriteString("# HELP webrtc_gateway_stream_rejections_total Total number of streams rejected due to capacity limit\n")
	b.WriteString("# TYPE webrtc_gateway_stream_rejections_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_stream_rejections_total{node_id=%q} %d\n\n", a.NodeID, a.StreamRejections()))

	b.WriteString("# HELP webrtc_gateway_traffic_bytes_total Total traffic bytes transferred over WebRTC DataChannels\n")
	b.WriteString("# TYPE webrtc_gateway_traffic_bytes_total counter\n")
	b.WriteString(fmt.Sprintf("webrtc_gateway_traffic_bytes_total{node_id=%q,direction=\"rx\"} %d\n", a.NodeID, rx))
	b.WriteString(fmt.Sprintf("webrtc_gateway_traffic_bytes_total{node_id=%q,direction=\"tx\"} %d\n\n", a.NodeID, tx))

	return b.String()
}

// SetInsecureTLS enables or disables skipping TLS certificate verification (useful for self-signed certificates).
func (a *Agent) SetInsecureTLS(insecure bool) {
	a.InsecureTLS = insecure
	if a.httpProxy != nil {
		a.httpProxy.SetInsecureTLS(insecure)
	}
	if a.wsProxy != nil {
		a.wsProxy.SetInsecureTLS(insecure)
	}
}

// SetSNI sets custom TLS ServerName and Host header for signaling (useful when dialing Cloudflare Anycast IP directly).
func (a *Agent) SetSNI(sni string) {
	a.SNI = sni
}

// SetPingInterval sets custom interval for signaling WebSocket ping keepalive.
func (a *Agent) SetPingInterval(d time.Duration) {
	a.pingInterval = d
}

// SetReadTimeout sets custom read deadline timeout for signaling WebSocket.
func (a *Agent) SetReadTimeout(d time.Duration) {
	a.readTimeout = d
}

func (a *Agent) sendSignal(msg *signaling.Message) error {
	a.wsMu.Lock()
	defer a.wsMu.Unlock()
	if a.wsConn == nil {
		return nil
	}
	_ = a.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return a.wsConn.WriteJSON(msg)
}

func (a *Agent) sendPing() error {
	a.wsMu.Lock()
	defer a.wsMu.Unlock()
	if a.wsConn == nil {
		return nil
	}
	_ = a.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return a.wsConn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
}

var fingerprintRegex = regexp.MustCompile(`(?i)a=fingerprint:\S+\s+([A-Fa-f0-9:]{20,})`)

// ExtractSDPFingerprint extracts the DTLS certificate fingerprint from an SDP string.
func ExtractSDPFingerprint(sdp string) string {
	match := fingerprintRegex.FindStringSubmatch(sdp)
	if len(match) > 1 {
		return strings.ToUpper(strings.TrimSpace(match[1]))
	}
	return ""
}

// ComputeHMAC calculates an HMAC-SHA256 hex string with the given key and data.
func ComputeHMAC(key, data string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
}

// GenerateShareSignature calculates an HMAC-SHA256 signature for a temporary share link.
func GenerateShareSignature(nodeID string, exp int64, secret string) string {
	data := fmt.Sprintf("%s:%d", nodeID, exp)
	return ComputeHMAC(secret, data)
}

// GenerateShareLink formats a ready-to-share URL with HMAC signature.
func GenerateShareLink(baseURL, nodeID, secret string, duration time.Duration) string {
	if secret == "" {
		return fmt.Sprintf("%s#%s", strings.TrimRight(baseURL, "/"), nodeID)
	}
	exp := time.Now().Add(duration).Unix()
	sign := GenerateShareSignature(nodeID, exp, secret)
	return fmt.Sprintf("%s/#%s?exp=%d&sign=%s", strings.TrimRight(baseURL, "/"), nodeID, exp, sign)
}

// VerifyAuth validates whether a client connection request is authorized.
// It supports DTLS fingerprint replay defense (F-04), one-way derived auth keys (F-02),
// and backward-compatible temporary share link signatures.
func (a *Agent) VerifyAuth(nodeID, accessSecret, reqSecret string, exp int64, sign, sdpFingerprint string) bool {
	if accessSecret == "" {
		return true // No authentication configured on agent (public access)
	}

	// 1. Direct access secret token verification (legacy)
	if reqSecret != "" && subtle.ConstantTimeCompare([]byte(reqSecret), []byte(accessSecret)) == 1 {
		return true
	}

	if sign == "" || exp <= 0 {
		return false
	}

	now := time.Now().Unix()
	// Allow 60 seconds clock skew tolerance
	if now > exp+60 {
		log.Printf("[Agent] Signature expired: exp=%d, now=%d", exp, now)
		return false
	}

	// Derived authKey: HMAC-SHA256(accessSecret, "node-auth:" + nodeID)
	authKey := ComputeHMAC(accessSecret, "node-auth:"+nodeID)

	// Tier 1: DTLS certificate fingerprint binding with derived authKey
	if sdpFingerprint != "" {
		expectedSign := ComputeHMAC(authKey, fmt.Sprintf("%s:%d:%s", nodeID, exp, sdpFingerprint))
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(sign)), []byte(strings.ToLower(expectedSign))) == 1 {
			return true
		}
		// Tier 2: DTLS certificate fingerprint binding with direct accessSecret
		expectedSignDirect := ComputeHMAC(accessSecret, fmt.Sprintf("%s:%d:%s", nodeID, exp, sdpFingerprint))
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(sign)), []byte(strings.ToLower(expectedSignDirect))) == 1 {
			return true
		}
	}

	// Tier 3: Backward-compatible share link without fingerprint (nodeID:exp)
	if exp > 0 {
		expectedShareSign := GenerateShareSignature(nodeID, exp, accessSecret)
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(sign)), []byte(strings.ToLower(expectedShareSign))) == 1 {
			return true
		}
	}

	log.Printf("[Agent] Auth verification failed for node %s", nodeID)
	return false
}

func (a *Agent) schedulePendingTeardown(sessionID string, gen uint64, pc *webrtc.PeerConnection) {
	a.sessionMu.RLock()
	sess, exists := a.sessions[sessionID]
	a.sessionMu.RUnlock()
	if !exists {
		return
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed || sess.Generation != gen || sess.PC != pc {
		return
	}
	if sess.teardownTimer != nil {
		sess.teardownTimer.Stop()
	}
	sess.teardownTimer = time.AfterFunc(5*time.Second, func() {
		a.teardownSession(sessionID, gen, pc)
	})
	log.Printf("[Agent] [Session %s Gen %d] Connection degraded/disconnected, started 5s grace period for ICE Restart", sessionID, gen)
}

func (a *Agent) cancelPendingTeardown(sessionID string) {
	a.sessionMu.RLock()
	sess, exists := a.sessions[sessionID]
	a.sessionMu.RUnlock()
	if exists {
		sess.CancelTeardown()
		log.Printf("[Agent] [Session %s] Cancelled pending teardown (reconnected or ICE Restart in progress)", sessionID)
	}
}

func (a *Agent) teardownSession(sessionID string, gen uint64, pc *webrtc.PeerConnection) {
	a.sessionMu.Lock()
	sess, exists := a.sessions[sessionID]
	if !exists {
		a.sessionMu.Unlock()
		return
	}

	sess.mu.Lock()
	// Generation & PC Guard: if session generation has advanced or PC doesn't match, this teardown is stale!
	if sess.closed || sess.Generation != gen || (pc != nil && sess.PC != pc) {
		sess.mu.Unlock()
		a.sessionMu.Unlock()
		log.Printf("[Agent] [Session %s] Ignored stale teardown for gen=%d (current gen=%d)", sessionID, gen, sess.Generation)
		return
	}
	cookieID := sess.CookieID
	delete(a.sessions, sessionID)
	atomic.StoreInt64(&a.activePeers, int64(len(a.sessions)))
	sess.mu.Unlock()
	a.sessionMu.Unlock()

	sess.Destroy()
	if cookieID != "" {
		a.cookieManager.RemoveSession(cookieID)
	}
	log.Printf("[Agent] [Session %s Gen %d] Grace period expired, session destroyed", sessionID, gen)
}

// Close shuts down all active network connections and WebRTC peer connections.
func (a *Agent) Close() {
	a.wsMu.Lock()
	if a.wsConn != nil {
		_ = a.wsConn.Close()
		a.wsConn = nil
	}
	a.wsMu.Unlock()

	a.sessionMu.Lock()
	for id, sess := range a.sessions {
		sess.Destroy()
		delete(a.sessions, id)
	}
	atomic.StoreInt64(&a.activePeers, 0)
	a.sessionMu.Unlock()
}

func (a *Agent) getClientIP(sessionID string) string {
	a.sessionMu.RLock()
	defer a.sessionMu.RUnlock()
	if sess, exists := a.sessions[sessionID]; exists {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.ClientIP
	}
	return ""
}

func (a *Agent) getOrCreateSession(sessionID, cookieID, clientIP string) *AgentSession {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	sess, exists := a.sessions[sessionID]
	if !exists {
		// Cap pending sessions to MaxPendingSessions to mitigate DoS attacks
		if len(a.sessions) >= MaxPendingSessions {
			for id, s := range a.sessions {
				s.mu.Lock()
				isClosed := s.closed || (s.PC == nil && s.CandidateCount() == 0)
				s.mu.Unlock()
				if isClosed {
					delete(a.sessions, id)
					break
				}
			}
		}
		sess = newAgentSession(sessionID, cookieID, clientIP)
		a.sessions[sessionID] = sess
		atomic.StoreInt64(&a.activePeers, int64(len(a.sessions)))
		return sess
	}
	sess.mu.Lock()
	if clientIP != "" {
		sess.ClientIP = clientIP
	}
	if cookieID != "" {
		sess.CookieID = cookieID
	}
	sess.mu.Unlock()
	return sess
}

func (a *Agent) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		a.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		log.Printf("[Agent] Connecting to signaling server: %s", a.SignalURL)
		u, err := url.Parse(a.SignalURL)
		if err != nil {
			log.Fatalf("[Agent] Invalid signaling URL: %v", err)
		}

		// Cloudflare Workers Durable Objects edge sharding optimization (PERF-10):
		// Bind connection directly to the node's dedicated DO room via query parameter
		if u.Query().Get("node") == "" && a.NodeID != "" {
			q := u.Query()
			q.Set("node", a.NodeID)
			u.RawQuery = q.Encode()
		}

		dialer := *websocket.DefaultDialer
		dialer.HandshakeTimeout = 5 * time.Second
		tlsConfig := &tls.Config{}
		if a.InsecureTLS {
			tlsConfig.InsecureSkipVerify = true
		}
		if a.SNI != "" {
			tlsConfig.ServerName = a.SNI
		}
		dialer.TLSClientConfig = tlsConfig

		var reqHeader http.Header
		if a.SNI != "" {
			reqHeader = http.Header{"Host": []string{a.SNI}}
		}
		if a.ProxyURL != "" {
			if pu, err := url.Parse(a.ProxyURL); err == nil {
				dialer.Proxy = http.ProxyURL(pu)
				log.Printf("[Agent] Using proxy for signaling: %s", a.ProxyURL)
			}
		} else {
			dialer.Proxy = http.ProxyFromEnvironment
		}

		conn, _, err := dialer.DialContext(ctx, u.String(), reqHeader)
		if err != nil {
			log.Printf("[Agent] Signaling connection failed: %v, retrying in 2s...", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
				continue
			}
		}

		a.wsMu.Lock()
		a.wsConn = conn
		a.wsMu.Unlock()

		// Register NodeID
		requireVisitorAuth := (a.AccessSecret != "")
		regMsg := &signaling.Message{
			Type:               signaling.TypeRegister,
			NodeID:             a.NodeID,
			Role:               "agent",
			Secret:             a.SignalSecret,
			RequireVisitorAuth: &requireVisitorAuth,
		}
		if err := a.sendSignal(regMsg); err != nil {
			log.Printf("[Agent] Failed to register: %v", err)
			_ = conn.Close()
			continue
		}

		log.Printf("[Agent] Connected and registered as node_id=%s. Awaiting incoming peers...", a.NodeID)
		a.readLoop(ctx, conn)

		a.wsMu.Lock()
		a.wsConn = nil
		a.wsMu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (a *Agent) readLoop(ctx context.Context, conn *websocket.Conn) {
	defer conn.Close()

	// Security hardening: limit inbound signaling message size to 256KB (F-08)
	conn.SetReadLimit(256 * 1024)

	readCtx, readCancel := context.WithCancel(ctx)
	defer readCancel()

	pingInt := a.pingInterval
	if pingInt <= 0 {
		pingInt = 25 * time.Second
	}
	readTo := a.readTimeout
	if readTo <= 0 {
		readTo = 60 * time.Second
	}

	// 1. Synchronized Ping handler: reply Pong under wsMu to prevent concurrent write panic
	conn.SetPingHandler(func(appData string) error {
		a.wsMu.Lock()
		defer a.wsMu.Unlock()
		if a.wsConn == nil {
			return nil
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
		_ = conn.SetReadDeadline(time.Now().Add(readTo))
		return err
	})

	// 2. Pong handler: refresh read deadline when server responds to Agent Ping
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readTo))
		return nil
	})

	// 3. Proactive Agent Ping Pump: send Ping periodically to keep connection alive across proxies/firewalls
	go func() {
		ticker := time.NewTicker(pingInt)
		defer ticker.Stop()
		for {
			select {
			case <-readCtx.Done():
				return
			case <-ticker.C:
				if err := a.sendPing(); err != nil {
					return
				}
			}
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(readTo))
		var msg signaling.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}

		switch msg.Type {
		case signaling.TypeRegistered:
			log.Printf("[Agent] Server confirmed registration for node: %s", msg.NodeID)

		case signaling.TypeOffer:
			var sdpFingerprint string
			if msg.SDP != nil {
				sdpFingerprint = ExtractSDPFingerprint(msg.SDP.SDP)
			}
			log.Printf("[Agent] Received Offer from client session=%s (ice_restart=%v, exp=%d, client_ip=%s, fp=%s)", msg.SessionID, msg.IceRestart, msg.Exp, msg.ClientIP, sdpFingerprint)
			if !a.VerifyAuth(a.NodeID, a.AccessSecret, msg.Secret, msg.Exp, msg.Sign, sdpFingerprint) {
				log.Printf("[Agent] Rejected unauthorized Offer for session=%s (invalid access secret or expired/invalid signature)", msg.SessionID)
				_ = a.sendSignal(&signaling.Message{
					Type:      signaling.TypeError,
					NodeID:    a.NodeID,
					SessionID: msg.SessionID,
					Error:     "unauthorized: invalid access secret or expired/invalid share signature",
				})
				continue
			}
			cookieID := msg.SessionID
			if msg.ClientToken != "" {
				cookieID = msg.ClientToken
			}
			sess := a.getOrCreateSession(msg.SessionID, cookieID, msg.ClientIP)
			go func(s *AgentSession, sdp *webrtc.SessionDescription, clientToken string, iceRestart bool) {
				s.offerMu.Lock()
				defer s.offerMu.Unlock()
				a.handleOffer(s, sdp, clientToken, iceRestart)
			}(sess, msg.SDP, msg.ClientToken, msg.IceRestart)

		case signaling.TypeCandidate:
			a.handleRemoteCandidate(msg.SessionID, msg.Candidate)

		case signaling.TypeError:
			log.Printf("[Agent] Signaling error: %s", msg.Error)
		}
	}
}

func (a *Agent) handleOffer(sess *AgentSession, sdp *webrtc.SessionDescription, clientToken string, iceRestart bool) {
	if sess == nil || sdp == nil {
		return
	}

	sessionID := sess.ID
	a.cancelPendingTeardown(sessionID)

	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}

	existingPC := sess.PC
	// In-place ICE Restart on existing PeerConnection
	if iceRestart && existingPC != nil && existingPC.ConnectionState() != webrtc.PeerConnectionStateClosed {
		log.Printf("[Agent] [Session %s] Processing in-place ICE Restart on existing PeerConnection", sessionID)
		sess.mu.Unlock()

		if err := existingPC.SetRemoteDescription(*sdp); err != nil {
			log.Printf("[Agent] [Session %s] ICE Restart SetRemoteDescription error: %v, falling back to new PC", sessionID, err)
			sess.mu.Lock()
		} else {
			pending := sess.DrainCandidates()
			for _, cand := range pending {
				if err := existingPC.AddICECandidate(cand); err != nil {
					log.Printf("[Agent] [Session %s] Buffered ICE Restart AddICECandidate error: %v", sessionID, err)
				}
			}

			answer, err := existingPC.CreateAnswer(nil)
			if err != nil {
				log.Printf("[Agent] [Session %s] ICE Restart CreateAnswer error: %v", sessionID, err)
			} else if err := existingPC.SetLocalDescription(answer); err != nil {
				log.Printf("[Agent] [Session %s] ICE Restart SetLocalDescription error: %v", sessionID, err)
			} else {
				_ = a.sendSignal(&signaling.Message{
					Type:      signaling.TypeAnswer,
					NodeID:    a.NodeID,
					SessionID: sessionID,
					SDP:       &answer,
				})
				log.Printf("[Agent] [Session %s] Sent in-place ICE Restart Answer to client", sessionID)
				return
			}
			sess.mu.Lock()
		}
	}

	// Advance generation for new PeerConnection lifecycle
	sess.Generation++
	currentGen := sess.Generation

	// Close old PC and Mux if replacing session
	oldMux := sess.Mux
	oldPC := sess.PC
	sess.Mux = nil
	sess.PC = nil
	sess.mu.Unlock()

	if oldMux != nil {
		_ = oldMux.Close()
	}
	if oldPC != nil {
		_ = oldPC.Close()
	}

	pc, err := webrtc.NewPeerConnection(a.webrtcConfig)
	if err != nil {
		log.Printf("[Agent] Failed to create PeerConnection: %v", err)
		return
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		candidateInit := c.ToJSON()
		_ = a.sendSignal(&signaling.Message{
			Type:      signaling.TypeCandidate,
			NodeID:    a.NodeID,
			SessionID: sessionID,
			Candidate: &candidateInit,
		})
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("[Agent] [Session %s Gen %d] ICE State: %s", sessionID, currentGen, state.String())
		if state == webrtc.ICEConnectionStateConnected || state == webrtc.ICEConnectionStateCompleted {
			a.cancelPendingTeardown(sessionID)
		} else if state == webrtc.ICEConnectionStateDisconnected || state == webrtc.ICEConnectionStateFailed {
			a.schedulePendingTeardown(sessionID, currentGen, pc)
		} else if state == webrtc.ICEConnectionStateClosed {
			a.cancelPendingTeardown(sessionID)
			a.teardownSession(sessionID, currentGen, pc)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[Agent] [Session %s Gen %d] PeerConnection State: %s", sessionID, currentGen, state.String())
		if state == webrtc.PeerConnectionStateConnected {
			a.cancelPendingTeardown(sessionID)
		} else if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateFailed {
			a.schedulePendingTeardown(sessionID, currentGen, pc)
		} else if state == webrtc.PeerConnectionStateClosed {
			a.cancelPendingTeardown(sessionID)
			a.teardownSession(sessionID, currentGen, pc)
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		var dcID uint16
		if id := dc.ID(); id != nil {
			dcID = *id
		}
		log.Printf("[Agent] [Session %s Gen %d] OnDataChannel label=%s id=%d", sessionID, currentGen, dc.Label(), dcID)
		if dc.Label() != "p2pmux" {
			return
		}

		sess.mu.Lock()
		if sess.closed || sess.Generation != currentGen {
			sess.mu.Unlock()
			log.Printf("[Agent] [Session %s] Ignoring DataChannel for stale gen=%d (current gen=%d)", sessionID, currentGen, sess.Generation)
			return
		}
		cookieSession := sess.CookieID
		transport := &countedTransport{DataChannel: dc, txCounter: &a.txBytesTotal}
		m := mux.NewMux(transport, false)
		sess.Mux = m
		sess.mu.Unlock()

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			atomic.AddUint64(&a.rxBytesTotal, uint64(len(msg.Data)))
			if err := m.HandleIncomingMessage(msg.Data); err != nil {
				log.Printf("[Agent] Mux handle message error: %v", err)
			}
		})

		dc.OnClose(func() {
			log.Printf("[Agent] [Session %s Gen %d] DataChannel closed", sessionID, currentGen)
			_ = m.Close()
		})

		dc.OnOpen(func() {
			log.Printf("[Agent] [Session %s Gen %d] DataChannel p2pmux OPENED! (cookieSession: %s)", sessionID, currentGen, cookieSession)
		})

		// Accept incoming streams and route to Reverse Proxy
		go a.streamAcceptLoop(sessionID, cookieSession, m)
	})

	sess.mu.Lock()
	if sess.closed || sess.Generation != currentGen {
		sess.mu.Unlock()
		_ = pc.Close()
		return
	}
	sess.PC = pc
	sess.mu.Unlock()

	// Set Remote Description (Offer) before registering into session to avoid candidate race
	if err := pc.SetRemoteDescription(*sdp); err != nil {
		log.Printf("[Agent] SetRemoteDescription error: %v", err)
		_ = pc.Close()
		return
	}

	pending := sess.DrainCandidates()
	for _, cand := range pending {
		if err := pc.AddICECandidate(cand); err != nil {
			log.Printf("[Agent] Buffered AddICECandidate error: %v", err)
		}
	}

	// Create Answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("[Agent] CreateAnswer error: %v", err)
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("[Agent] SetLocalDescription error: %v", err)
		return
	}

	// Send Answer back
	_ = a.sendSignal(&signaling.Message{
		Type:      signaling.TypeAnswer,
		NodeID:    a.NodeID,
		SessionID: sessionID,
		SDP:       &answer,
	})
	log.Printf("[Agent] Sent Answer to client session=%s Gen=%d", sessionID, currentGen)
}

func (a *Agent) handleRemoteCandidate(sessionID string, candidate *webrtc.ICECandidateInit) {
	if candidate == nil {
		return
	}

	a.sessionMu.RLock()
	sess, exists := a.sessions[sessionID]
	a.sessionMu.RUnlock()

	if !exists {
		a.sessionMu.Lock()
		if len(a.sessions) >= MaxPendingSessions {
			a.sessionMu.Unlock()
			log.Printf("[Agent] Dropped candidate for session %s: pending sessions quota (%d) exceeded", sessionID, MaxPendingSessions)
			return
		}
		sess = newAgentSession(sessionID, sessionID, "")
		a.sessions[sessionID] = sess
		atomic.StoreInt64(&a.activePeers, int64(len(a.sessions)))
		a.sessionMu.Unlock()
	}

	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		return
	}
	pc := sess.PC
	if pc == nil || pc.RemoteDescription() == nil {
		sess.mu.Unlock()
		if !sess.AddCandidate(*candidate) {
			log.Printf("[Agent] [Session %s] Dropped candidate: candidate quota (%d) exceeded", sessionID, MaxCandidatesPerSession)
		}
		return
	}
	sess.mu.Unlock()

	if err := pc.AddICECandidate(*candidate); err != nil {
		log.Printf("[Agent] AddICECandidate error: %v", err)
	}
}

// tryAcquireStream attempts to atomically acquire a stream slot using CAS,
// eliminating the TOCTOU race window under concurrent stream arrival.
// It returns a sync.Once release closure and true on success, or nil and false if capacity is exceeded.
func (a *Agent) tryAcquireStream() (func(), bool) {
	limit := int64(a.maxConcurrentStreams)
	if limit <= 0 {
		limit = 512
	}

	for {
		curr := atomic.LoadInt64(&a.activeStreams)
		if curr >= limit {
			return nil, false
		}
		if atomic.CompareAndSwapInt64(&a.activeStreams, curr, curr+1) {
			var once sync.Once
			release := func() {
				once.Do(func() {
					atomic.AddInt64(&a.activeStreams, -1)
				})
			}
			return release, true
		}
	}
}

// streamAcceptLoop processes incoming multiplexed streams and proxies them to local services.
func (a *Agent) streamAcceptLoop(sessionID, cookieSession string, m *mux.Mux) {
	for {
		stream, err := m.AcceptStream(context.Background())
		if err != nil {
			return
		}

		releaseStream, ok := a.tryAcquireStream()
		if !ok {
			atomic.AddUint64(&a.streamRejections, 1)
			limit := a.maxConcurrentStreams
			if limit <= 0 {
				limit = 512
			}
			log.Printf("[Agent] [Session %s] Stream %d rejected: max concurrent streams limit (%d) reached", sessionID, stream.ID(), limit)
			go func(s *mux.Stream) {
				if s.InitialType() == protocol.TypeHeaders {
					respHeaders, _ := json.Marshal(map[string]interface{}{
						"s": 503,
						"m": "Service Unavailable",
						"h": map[string]string{
							"Content-Type": "text/plain; charset=utf-8",
							"Retry-After":  "2",
						},
					})
					_ = s.WriteHeaders(respHeaders)
					_, _ = s.Write([]byte("503 Service Unavailable: Agent concurrent streams capacity reached (backpressure limit)\n"))
					_ = s.Close()
				} else {
					_ = s.Reset()
				}
			}(stream)
			continue
		}

		go func(s *mux.Stream, release func()) {
			defer release()

			if s.InitialType() == protocol.TypeData {
				// Raw data stream (e.g. echo or direct raw pipe)
				defer s.Close()
				buf := new(bytes.Buffer)
				_, _ = io.Copy(buf, io.LimitReader(s, 64*1024))
				_, _ = s.Write(buf.Bytes())
				return
			}

			// Probe the stream headers to route between HTTP and WebSocket
			ctx, cancel := context.WithTimeout(s.Context(), 10*time.Second)
			headersPayload, err := s.ReadHeaders(ctx)
			cancel()

			if err != nil {
				log.Printf("[Agent] [Session %s] Stream %d failed to read probe headers: %v", sessionID, s.ID(), err)
				_ = s.Reset()
				return
			}

			var probe struct {
				Method     string            `json:"m"`
				MethodAlt  string            `json:"method"`
				Path       string            `json:"p"`
				PathAlt    string            `json:"path"`
				Headers    map[string]string `json:"h"`
				HeadersAlt map[string]string `json:"headers"`
				Protocols  []string          `json:"protocols"`
			}
			if err := json.Unmarshal(headersPayload, &probe); err != nil {
				log.Printf("[Agent] [Session %s] Stream %d failed to parse probe headers: %v", sessionID, s.ID(), err)
				_ = s.Reset()
				return
			}

			if probe.Method == "" && probe.MethodAlt != "" {
				probe.Method = probe.MethodAlt
			}
			if probe.Method == "" {
				probe.Method = "GET"
			}
			if probe.Path == "" && probe.PathAlt != "" {
				probe.Path = probe.PathAlt
			}
			if probe.Headers == nil && probe.HeadersAlt != nil {
				probe.Headers = probe.HeadersAlt
			}

			isWebSocket := false
			if len(probe.Protocols) > 0 {
				isWebSocket = true
			} else {
				for k, v := range probe.Headers {
					if strings.EqualFold(k, "upgrade") && strings.EqualFold(v, "websocket") {
						isWebSocket = true
						break
					}
				}
			}

			if a.fileProxy != nil {
				if isWebSocket {
					log.Printf("[Agent] [Session %s] WebSocket not supported in file server mode, resetting stream %d", sessionID, s.ID())
					_ = s.Reset()
					return
				}
				reqMeta := &proxy.RequestMetadata{
					Method:  probe.Method,
					Path:    probe.Path,
					Headers: probe.Headers,
				}
				a.fileProxy.HandleStreamWithMeta(cookieSession, s, reqMeta)
				return
			}

			clientIP := a.getClientIP(sessionID)
			if isWebSocket {
				log.Printf("[Agent] [Session %s] Routing Stream %d to WebSocket Proxy", sessionID, s.ID())
				wsMeta := &proxy.WSConnectMetadata{
					Path:      probe.Path,
					Protocols: probe.Protocols,
					Headers:   probe.Headers,
				}
				a.wsProxy.HandleStreamWithMeta(cookieSession, clientIP, s, wsMeta)
			} else {
				reqMeta := &proxy.RequestMetadata{
					Method:  probe.Method,
					Path:    probe.Path,
					Headers: probe.Headers,
				}
				a.httpProxy.HandleStreamWithMeta(cookieSession, clientIP, s, reqMeta)
			}
		}(stream, releaseStream)
	}
}
