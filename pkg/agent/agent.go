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

	webrtcConfig     webrtc.Configuration
	activeConns      map[string]*webrtc.PeerConnection
	clientIPs        map[string]string
	connMu           sync.Mutex
	pendingTeardowns map[string]*time.Timer
	pendingMu        sync.Mutex

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
		activeConns:          make(map[string]*webrtc.PeerConnection),
		clientIPs:            make(map[string]string),
		pendingTeardowns:     make(map[string]*time.Timer),
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

// GenerateShareSignature calculates an HMAC-SHA256 signature for a temporary share link.
func GenerateShareSignature(nodeID string, exp int64, secret string) string {
	data := fmt.Sprintf("%s:%d", nodeID, exp)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(data))
	return hex.EncodeToString(mac.Sum(nil))
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
// It supports both direct access secret tokens and time-limited HMAC-SHA256 signatures.
func (a *Agent) VerifyAuth(nodeID, accessSecret, reqSecret string, exp int64, sign string) bool {
	if accessSecret == "" {
		return true // No authentication configured on agent (public access)
	}

	// 1. Direct access secret token verification
	if reqSecret != "" && subtle.ConstantTimeCompare([]byte(reqSecret), []byte(accessSecret)) == 1 {
		return true
	}

	// 2. Stateless HMAC-SHA256 signed temporary link verification
	if sign != "" && exp > 0 {
		now := time.Now().Unix()
		// Allow 60 seconds clock skew tolerance
		if now > exp+60 {
			log.Printf("[Agent] Share signature expired: exp=%d, now=%d", exp, now)
			return false
		}
		expectedSign := GenerateShareSignature(nodeID, exp, accessSecret)
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(sign)), []byte(strings.ToLower(expectedSign))) == 1 {
			return true
		}
		log.Printf("[Agent] Share signature mismatch for node %s", nodeID)
		return false
	}

	return false
}

func (a *Agent) schedulePendingTeardown(sessionID, cookieSession string, pc *webrtc.PeerConnection) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()

	if timer, exists := a.pendingTeardowns[sessionID]; exists {
		timer.Stop()
	}

	a.pendingTeardowns[sessionID] = time.AfterFunc(5*time.Second, func() {
		a.pendingMu.Lock()
		delete(a.pendingTeardowns, sessionID)
		a.pendingMu.Unlock()

		log.Printf("[Agent] [Session %s] Grace period expired (5s), tearing down connection", sessionID)
		a.teardownSession(sessionID, cookieSession, pc)
	})
	log.Printf("[Agent] [Session %s] Connection degraded/disconnected, started 5s grace period for ICE Restart", sessionID)
}

func (a *Agent) cancelPendingTeardown(sessionID string) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	if timer, exists := a.pendingTeardowns[sessionID]; exists {
		timer.Stop()
		delete(a.pendingTeardowns, sessionID)
		log.Printf("[Agent] [Session %s] Cancelled pending teardown (reconnected or ICE Restart in progress)", sessionID)
	}
}

func (a *Agent) teardownSession(sessionID, cookieSession string, pc *webrtc.PeerConnection) {
	a.connMu.Lock()
	currPC, exists := a.activeConns[sessionID]
	if !exists || currPC == pc || pc == nil {
		delete(a.activeConns, sessionID)
		delete(a.clientIPs, sessionID)
	}
	atomic.StoreInt64(&a.activePeers, int64(len(a.activeConns)))
	a.connMu.Unlock()
	a.cookieManager.RemoveSession(cookieSession)
	if pc != nil {
		_ = pc.Close()
	}
}

// Close shuts down all active network connections and WebRTC peer connections.
func (a *Agent) Close() {
	a.wsMu.Lock()
	if a.wsConn != nil {
		_ = a.wsConn.Close()
		a.wsConn = nil
	}
	a.wsMu.Unlock()

	a.pendingMu.Lock()
	for id, timer := range a.pendingTeardowns {
		timer.Stop()
		delete(a.pendingTeardowns, id)
	}
	a.pendingMu.Unlock()

	a.connMu.Lock()
	for id, pc := range a.activeConns {
		_ = pc.Close()
		delete(a.activeConns, id)
	}
	a.clientIPs = make(map[string]string)
	atomic.StoreInt64(&a.activePeers, 0)
	a.connMu.Unlock()
}

func (a *Agent) getClientIP(sessionID string) string {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.clientIPs[sessionID]
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
			log.Printf("[Agent] Received Offer from client session=%s (ice_restart=%v, exp=%d, client_ip=%s)", msg.SessionID, msg.IceRestart, msg.Exp, msg.ClientIP)
			if !a.VerifyAuth(a.NodeID, a.AccessSecret, msg.Secret, msg.Exp, msg.Sign) {
				log.Printf("[Agent] Rejected unauthorized Offer for session=%s (invalid access secret or expired/invalid signature)", msg.SessionID)
				_ = a.sendSignal(&signaling.Message{
					Type:      signaling.TypeError,
					NodeID:    a.NodeID,
					SessionID: msg.SessionID,
					Error:     "unauthorized: invalid access secret or expired/invalid share signature",
				})
				continue
			}
			go a.handleOffer(msg.SessionID, msg.ClientIP, msg.SDP, msg.ClientToken, msg.IceRestart)

		case signaling.TypeCandidate:
			a.handleRemoteCandidate(msg.SessionID, msg.Candidate)

		case signaling.TypeError:
			log.Printf("[Agent] Signaling error: %s", msg.Error)
		}
	}
}

func (a *Agent) handleOffer(sessionID, clientIP string, sdp *webrtc.SessionDescription, clientToken string, iceRestart bool) {
	cookieSession := sessionID
	if clientToken != "" {
		cookieSession = clientToken
	}

	a.cancelPendingTeardown(sessionID)

	a.connMu.Lock()
	existingPC, exists := a.activeConns[sessionID]
	a.connMu.Unlock()

	// In-place ICE Restart on existing PeerConnection
	if iceRestart && exists && existingPC != nil && existingPC.ConnectionState() != webrtc.PeerConnectionStateClosed {
		log.Printf("[Agent] [Session %s] Processing in-place ICE Restart on existing PeerConnection", sessionID)
		if err := existingPC.SetRemoteDescription(*sdp); err != nil {
			log.Printf("[Agent] [Session %s] ICE Restart SetRemoteDescription error: %v, falling back to new PC", sessionID, err)
		} else {
			answer, err := existingPC.CreateAnswer(nil)
			if err != nil {
				log.Printf("[Agent] [Session %s] ICE Restart CreateAnswer error: %v", sessionID, err)
			} else if err := existingPC.SetLocalDescription(answer); err != nil {
				log.Printf("[Agent] [Session %s] ICE Restart SetLocalDescription error: %v", sessionID, err)
			} else {
				if clientIP != "" {
					a.connMu.Lock()
					a.clientIPs[sessionID] = clientIP
					a.connMu.Unlock()
				}
				_ = a.sendSignal(&signaling.Message{
					Type:      signaling.TypeAnswer,
					NodeID:    a.NodeID,
					SessionID: sessionID,
					SDP:       &answer,
				})
				log.Printf("[Agent] [Session %s] Sent in-place ICE Restart Answer to client", sessionID)
				return
			}
		}
	}

	// Close old PC if replacing session
	if exists && existingPC != nil {
		_ = existingPC.Close()
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
		log.Printf("[Agent] [Session %s] ICE State: %s", sessionID, state.String())
		if state == webrtc.ICEConnectionStateConnected || state == webrtc.ICEConnectionStateCompleted {
			a.cancelPendingTeardown(sessionID)
		} else if state == webrtc.ICEConnectionStateDisconnected || state == webrtc.ICEConnectionStateFailed {
			a.schedulePendingTeardown(sessionID, cookieSession, pc)
		} else if state == webrtc.ICEConnectionStateClosed {
			a.cancelPendingTeardown(sessionID)
			a.teardownSession(sessionID, cookieSession, pc)
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[Agent] [Session %s] PeerConnection State: %s", sessionID, state.String())
		if state == webrtc.PeerConnectionStateConnected {
			a.cancelPendingTeardown(sessionID)
		} else if state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateFailed {
			a.schedulePendingTeardown(sessionID, cookieSession, pc)
		} else if state == webrtc.PeerConnectionStateClosed {
			a.cancelPendingTeardown(sessionID)
			a.teardownSession(sessionID, cookieSession, pc)
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		var dcID uint16
		if id := dc.ID(); id != nil {
			dcID = *id
		}
		log.Printf("[Agent] [Session %s] OnDataChannel label=%s id=%d", sessionID, dc.Label(), dcID)
		if dc.Label() != "p2pmux" {
			return
		}

		transport := &countedTransport{DataChannel: dc, txCounter: &a.txBytesTotal}
		m := mux.NewMux(transport, false)

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			atomic.AddUint64(&a.rxBytesTotal, uint64(len(msg.Data)))
			if err := m.HandleIncomingMessage(msg.Data); err != nil {
				log.Printf("[Agent] Mux handle message error: %v", err)
			}
		})

		dc.OnClose(func() {
			log.Printf("[Agent] [Session %s] DataChannel closed", sessionID)
			_ = m.Close()
		})

		dc.OnOpen(func() {
			log.Printf("[Agent] [Session %s] DataChannel p2pmux OPENED! (cookieSession: %s)", sessionID, cookieSession)
		})

		// Accept incoming streams and route to Reverse Proxy
		go a.streamAcceptLoop(sessionID, cookieSession, m)
	})

	// Set Remote Description (Offer) before registering into activeConns to avoid ICE candidate races
	if err := pc.SetRemoteDescription(*sdp); err != nil {
		log.Printf("[Agent] SetRemoteDescription error: %v", err)
		_ = pc.Close()
		return
	}

	a.connMu.Lock()
	a.activeConns[sessionID] = pc
	if clientIP != "" {
		a.clientIPs[sessionID] = clientIP
	}
	atomic.StoreInt64(&a.activePeers, int64(len(a.activeConns)))
	a.connMu.Unlock()

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
	log.Printf("[Agent] Sent Answer to client session=%s", sessionID)
}

func (a *Agent) handleRemoteCandidate(sessionID string, candidate *webrtc.ICECandidateInit) {
	if candidate == nil {
		return
	}

	a.connMu.Lock()
	pc, exists := a.activeConns[sessionID]
	a.connMu.Unlock()

	if !exists || pc == nil {
		// Buffered retry for trickle ICE candidates arriving before handshake completes
		go func() {
			for i := 0; i < 10; i++ {
				time.Sleep(50 * time.Millisecond)
				a.connMu.Lock()
				p, ex := a.activeConns[sessionID]
				a.connMu.Unlock()
				if ex && p != nil && p.RemoteDescription() != nil {
					_ = p.AddICECandidate(*candidate)
					return
				}
			}
		}()
		return
	}

	if pc.RemoteDescription() != nil {
		if err := pc.AddICECandidate(*candidate); err != nil {
			log.Printf("[Agent] AddICECandidate error: %v", err)
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

		limit := a.maxConcurrentStreams
		if limit <= 0 {
			limit = 512
		}

		if atomic.LoadInt64(&a.activeStreams) >= int64(limit) {
			atomic.AddUint64(&a.streamRejections, 1)
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

		atomic.AddInt64(&a.activeStreams, 1)
		go func(s *mux.Stream) {
			defer atomic.AddInt64(&a.activeStreams, -1)

			if s.InitialType() == protocol.TypeData {
				// Raw data stream (e.g. echo or direct raw pipe)
				defer s.Close()
				buf := new(bytes.Buffer)
				_, _ = io.Copy(buf, io.LimitReader(s, 16*1024*1024))
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
				Headers   map[string]string `json:"h"`
				Protocols []string          `json:"protocols"`
			}
			_ = json.Unmarshal(headersPayload, &probe)

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

			// Restore headers for the proxy handler
			s.PushInternalHeaders(headersPayload)

			if a.fileProxy != nil {
				if isWebSocket {
					log.Printf("[Agent] [Session %s] WebSocket not supported in file server mode, resetting stream %d", sessionID, s.ID())
					_ = s.Reset()
					return
				}
				a.fileProxy.HandleStream(cookieSession, s)
				return
			}

			clientIP := a.getClientIP(sessionID)
			if isWebSocket {
				log.Printf("[Agent] [Session %s] Routing Stream %d to WebSocket Proxy", sessionID, s.ID())
				a.wsProxy.HandleStream(cookieSession, clientIP, s)
			} else {
				a.httpProxy.HandleStream(cookieSession, clientIP, s)
			}
		}(stream)
	}
}
