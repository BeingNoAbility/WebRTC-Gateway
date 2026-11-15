package agent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

type Agent struct {
	NodeID      string
	SignalURL   string
	Secret      string
	Target      string
	ProxyURL    string
	SNI         string
	InsecureTLS bool

	wsConn *websocket.Conn
	wsMu   sync.Mutex

	cookieManager *proxy.SessionCookieManager
	httpProxy     *proxy.HTTPProxy
	wsProxy       *proxy.WSProxy

	webrtcConfig webrtc.Configuration
	activeConns  map[string]*webrtc.PeerConnection
	connMu       sync.Mutex
}

func New(nodeID, signalURL, secret, target string) *Agent {
	cm := proxy.NewSessionCookieManager()
	hp, err := proxy.NewHTTPProxy(target, cm)
	if err != nil {
		log.Fatalf("[Agent] Failed to init HTTPProxy: %v", err)
	}

	wp, err := proxy.NewWSProxy(target, cm)
	if err != nil {
		log.Fatalf("[Agent] Failed to init WSProxy: %v", err)
	}

	return &Agent{
		NodeID:        nodeID,
		SignalURL:     signalURL,
		Secret:        secret,
		Target:        target,
		cookieManager: cm,
		httpProxy:     hp,
		wsProxy:       wp,
		webrtcConfig: webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{URLs: []string{"stun:stun.l.google.com:19302"}},
				{URLs: []string{"stun:stun1.l.google.com:19302"}},
				{URLs: []string{"stun:stun.cloudflare.com:3478"}},
			},
		},
		activeConns: make(map[string]*webrtc.PeerConnection),
	}
}

// SetProxy configures an explicit proxy (http://, https://, or socks5://) for signaling and backend proxies.
func (a *Agent) SetProxy(proxyURL string) {
	a.ProxyURL = proxyURL
	if a.httpProxy != nil {
		a.httpProxy.SetProxy(proxyURL)
	}
	if a.wsProxy != nil {
		a.wsProxy.SetProxy(proxyURL)
	}
}

// SetInsecureTLS enables or disables skipping TLS certificate verification (useful for self-signed certificates).
func (a *Agent) SetInsecureTLS(insecure bool) {
	a.InsecureTLS = insecure
}

// SetSNI sets custom TLS ServerName and Host header for signaling (useful when dialing Cloudflare Anycast IP directly).
func (a *Agent) SetSNI(sni string) {
	a.SNI = sni
}

func (a *Agent) sendSignal(msg *signaling.Message) error {
	a.wsMu.Lock()
	defer a.wsMu.Unlock()
	if a.wsConn == nil {
		return nil
	}
	return a.wsConn.WriteJSON(msg)
}

// Close shuts down all active network connections and WebRTC peer connections.
func (a *Agent) Close() {
	a.wsMu.Lock()
	if a.wsConn != nil {
		_ = a.wsConn.Close()
		a.wsConn = nil
	}
	a.wsMu.Unlock()

	a.connMu.Lock()
	for id, pc := range a.activeConns {
		_ = pc.Close()
		delete(a.activeConns, id)
	}
	a.connMu.Unlock()
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
		regMsg := &signaling.Message{
			Type:   signaling.TypeRegister,
			NodeID: a.NodeID,
			Role:   "agent",
			Secret: a.Secret,
		}
		if err := a.sendSignal(regMsg); err != nil {
			log.Printf("[Agent] Failed to register: %v", err)
			_ = conn.Close()
			continue
		}

		log.Printf("[Agent] Connected and registered as node_id=%s. Awaiting incoming peers...", a.NodeID)
		a.readLoop(ctx, conn)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (a *Agent) readLoop(ctx context.Context, conn *websocket.Conn) {
	defer conn.Close()

	for {
		var msg signaling.Message
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}

		switch msg.Type {
		case signaling.TypeRegistered:
			log.Printf("[Agent] Server confirmed registration for node: %s", msg.NodeID)

		case signaling.TypeOffer:
			log.Printf("[Agent] Received Offer from client session=%s", msg.SessionID)
			if a.Secret != "" && subtle.ConstantTimeCompare([]byte(msg.Secret), []byte(a.Secret)) != 1 {
				log.Printf("[Agent] Rejected unauthorized Offer for session=%s (secret mismatch or missing)", msg.SessionID)
				_ = a.sendSignal(&signaling.Message{
					Type:      signaling.TypeError,
					NodeID:    a.NodeID,
					SessionID: msg.SessionID,
					Error:     "unauthorized: invalid or missing secret token",
				})
				continue
			}
			go a.handleOffer(msg.SessionID, msg.SDP)

		case signaling.TypeCandidate:
			a.handleRemoteCandidate(msg.SessionID, msg.Candidate)

		case signaling.TypeError:
			log.Printf("[Agent] Signaling error: %s", msg.Error)
		}
	}
}

func (a *Agent) handleOffer(sessionID string, sdp *webrtc.SessionDescription) {
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
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateClosed {
			a.connMu.Lock()
			delete(a.activeConns, sessionID)
			a.connMu.Unlock()
			a.cookieManager.RemoveSession(sessionID)
			_ = pc.Close()
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[Agent] [Session %s] PeerConnection State: %s", sessionID, state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			a.connMu.Lock()
			delete(a.activeConns, sessionID)
			a.connMu.Unlock()
			a.cookieManager.RemoveSession(sessionID)
			_ = pc.Close()
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

		dc.OnOpen(func() {
			log.Printf("[Agent] [Session %s] DataChannel p2pmux OPENED!", sessionID)
			m := mux.NewMux(dc, false)

			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				if err := m.HandleIncomingMessage(msg.Data); err != nil {
					log.Printf("[Agent] Mux handle message error: %v", err)
				}
			})

			dc.OnClose(func() {
				log.Printf("[Agent] [Session %s] DataChannel closed", sessionID)
				_ = m.Close()
			})

			// Accept incoming streams and route to Reverse Proxy
			go a.streamAcceptLoop(sessionID, m)
		})
	})

	// Set Remote Description (Offer) before registering into activeConns to avoid ICE candidate races
	if err := pc.SetRemoteDescription(*sdp); err != nil {
		log.Printf("[Agent] SetRemoteDescription error: %v", err)
		_ = pc.Close()
		return
	}

	a.connMu.Lock()
	a.activeConns[sessionID] = pc
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
func (a *Agent) streamAcceptLoop(sessionID string, m *mux.Mux) {
	for {
		stream, err := m.AcceptStream(context.Background())
		if err != nil {
			return
		}

		go func(s *mux.Stream) {
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

			if isWebSocket {
				log.Printf("[Agent] [Session %s] Routing Stream %d to WebSocket Proxy", sessionID, s.ID())
				a.wsProxy.HandleStream(sessionID, s)
			} else {
				a.httpProxy.HandleStream(sessionID, s)
			}
		}(stream)
	}
}
