package agent

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"math"
	"net/netip"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/signaling"
)

func normalizeClientIP(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return ""
	}
	return addr.Unmap().String()
}

// sanitizeOutboundICECandidate enforces Fail-Closed mDNS privacy on outbound ICE candidates
// when mdnsEnabled == true:
//   - Drops ALL non-`.local` `typ host` candidates (even if Pion fell back to disabled mDNS on
//     IPv4-only / Docker hosts).
//   - Scrubs `raddr <ip> rport <port>` on `srflx`/`prflx`/`relay` candidates to `0.0.0.0`/`::` and `0`
//     per RFC 8828 §5.2 so TURN/STUN candidates never leak real internal LAN/Docker/VPN IPs.
func sanitizeOutboundICECandidate(cand string, mdnsEnabled bool) (string, bool) {
	if !mdnsEnabled || cand == "" {
		return cand, true
	}
	fields := strings.Fields(cand)
	if len(fields) < 8 || fields[6] != "typ" {
		return "", false
	}
	candType := strings.ToLower(fields[7])
	if candType == "host" {
		if !strings.HasSuffix(strings.ToLower(fields[4]), ".local") {
			return "", false
		}
		return cand, true
	}
	modified := false
	for i := 8; i+1 < len(fields); i += 2 {
		switch fields[i] {
		case "raddr":
			if strings.Contains(fields[i+1], ":") {
				fields[i+1] = "::"
			} else {
				fields[i+1] = "0.0.0.0"
			}
			modified = true
		case "rport":
			fields[i+1] = "0"
			modified = true
		}
	}
	if !modified {
		return cand, true
	}
	return strings.Join(fields, " "), true
}

// HandleOffer handles a visitor WebRTC Offer. Raw SDP NEVER crosses signaling;
// SDP is deterministically synthesized from msg.Desc.
func (a *Agent) HandleOffer(msg signaling.OfferRelayDTO) {
	offerSDP, sess, ok := a.prepareOffer(msg)
	if !ok {
		return
	}
	a.completeOffer(msg, offerSDP, sess)
}

// prepareOffer validates an incoming Offer, acquires an offer concurrency slot, and registers
// the handshaking session with offerInFlight=true so concurrent ICE candidates are staged.
func (a *Agent) prepareOffer(msg signaling.OfferRelayDTO) (string, *VisitorSession, bool) {
	if a.rootCtx.Err() != nil {
		return "", nil, false
	}
	// Bounded Offer concurrency protects against CPU exhaustion bursts.
	select {
	case a.offerSem <- struct{}{}:
	default:
		a.sendSignalError(msg.SessionID, "AGENT_BUSY", "too many concurrent offers")
		return "", nil, false
	}

	releaseSem := true
	defer func() {
		if releaseSem {
			<-a.offerSem
		}
	}()

	if msg.SessionID == "" || msg.Desc == nil || msg.Desc.Validate(true) != nil || len(msg.Nonce) < 16 || len(msg.Nonce) > 128 {
		a.sendSignalError(msg.SessionID, "INVALID_OFFER", "invalid offer descriptor or nonce")
		return "", nil, false
	}
	clientIP := normalizeClientIP(msg.ClientIP)
	if allowed, _ := a.authLimiter.Allow(clientIP, time.Now()); !allowed {
		a.sendSignalError(msg.SessionID, "RATE_LIMITED", "too many failed authentication attempts")
		return "", nil, false
	}

	offerSDP, err := SynthesizeSDP(msg.Desc, true)
	if err != nil {
		a.sendSignalError(msg.SessionID, "INVALID_OFFER", err.Error())
		return "", nil, false
	}

	sess, err := a.createHandshakingSession(msg.SessionID, clientIP, msg.Desc.FP, msg.Nonce)
	if err != nil {
		a.sendSignalError(msg.SessionID, "AGENT_BUSY", err.Error())
		return "", nil, false
	}

	releaseSem = false
	return offerSDP, sess, true
}

func (a *Agent) completeOffer(msg signaling.OfferRelayDTO, offerSDP string, sess *VisitorSession) {
	defer func() { <-a.offerSem }()

	pc, err := a.webrtcAPI.NewPeerConnection(a.webrtcConfig)
	if err != nil {
		a.closeSession(sess)
		a.sendSignalError(msg.SessionID, "INTERNAL_ERROR", "failed to create peer connection")
		return
	}

	m := mux.New(mux.Config{
		PoolSize:            a.cfg.PoolSize,
		MaxGlobalStreams:    a.cfg.MaxGlobalStream,
		GlobalBudget:        a.globalMem,
		GlobalActiveStreams: &a.activeStreams,
		OnControlFrame: func(f *protocol.Frame) {
			a.handleControlFrame(sess, f)
		},
		OnGoAway: func(code protocol.ErrorCode) {
			log.Printf("[Agent] Received GOAWAY(%s) from session %s", code, msg.SessionID)
		},
		OnProtocolError: func(perr error) {
			log.Printf("[Agent] Protocol error on session %s (%v); closing", msg.SessionID, perr)
			go a.closeSession(sess)
		},
	})
	m.SetMaxMessageSize(msg.Desc.MaxMsgSize)

	// Pre-negotiate 1 Control DC (id=0) + N Multiplexed Ordered Data Lanes (id=1..N, default 12).
	ordered := true
	negotiated := true
	ctrlID := protocol.ControlChannelID
	ctrlDC, err := pc.CreateDataChannel(protocol.ControlChannelLabel, &webrtc.DataChannelInit{
		Ordered:    &ordered,
		Negotiated: &negotiated,
		ID:         &ctrlID,
	})
	if err != nil {
		_ = pc.Close()
		m.Close()
		a.closeSession(sess)
		return
	}
	m.AttachControlDC(ctrlDC)

	for i := 0; i < a.cfg.PoolSize; i++ {
		laneIdx := i
		chID := protocol.PoolChannelID(laneIdx)
		dc, dcErr := pc.CreateDataChannel(protocol.PoolChannelLabel(laneIdx), &webrtc.DataChannelInit{
			Ordered:    &ordered,
			Negotiated: &negotiated,
			ID:         &chID,
		})
		if dcErr != nil {
			_ = pc.Close()
			m.Close()
			a.closeSession(sess)
			return
		}
		m.AttachLaneDC(laneIdx, dc)
		dc.OnMessage(func(dm webrtc.DataChannelMessage) {
			// Hard gate: drop and terminate immediately on any pre-auth frame over data lanes.
			if !sess.authDone.Load() {
				go a.closeSession(sess)
				return
			}
			m.HandleLaneMessage(laneIdx, dm.Data)
		})
		dc.OnClose(func() {
			m.HandleLaneClose(laneIdx)
		})
	}

	// P2PMux V8: All HTTP and WebSocket streams multiplex over pre-negotiated lanes; reject unexpected DCEP DCs Fail-Closed.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		_ = dc.Close()
		if !sess.authDone.Load() {
			go a.closeSession(sess)
		}
	})

	sess.mu.Lock()
	if sess.state == StateClosed {
		sess.mu.Unlock()
		_ = pc.Close()
		m.Close()
		return
	}
	sess.pc = pc
	sess.mux = m
	sess.mu.Unlock()

	ctrlDC.OnMessage(func(dm webrtc.DataChannelMessage) {
		m.HandleControlMessage(dm.Data)
	})
	ctrlDC.OnOpen(func() {
		a.onControlChannelOpen(sess, pc, m)
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		scrubbedCand, allow := sanitizeOutboundICECandidate(init.Candidate, a.cfg.ICEMDNS)
		if !allow {
			return
		}
		var idxPtr *int
		if init.SDPMLineIndex != nil {
			v := int(*init.SDPMLineIndex)
			idxPtr = &v
		}
		var mid string
		if init.SDPMid != nil {
			mid = *init.SDPMid
		}
		sess.mu.Lock()
		sigID := sess.signalSessionID
		closed := sess.state == StateClosed
		sess.mu.Unlock()
		if closed {
			return
		}
		_ = a.sendSignal(signaling.CandidateRelayDTO{
			Type:          signaling.TypeCandidate,
			NodeID:        a.cfg.NodeID,
			SessionID:     sigID,
			Candidate:     scrubbedCand,
			SDPMid:        mid,
			SDPMLineIndex: idxPtr,
		})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			sess.mu.Lock()
			if sess.discTimer != nil {
				sess.discTimer.Stop()
				sess.discTimer = nil
			}
			sess.mu.Unlock()
		case webrtc.PeerConnectionStateDisconnected:
			sess.mu.Lock()
			if sess.state != StateClosed && sess.discTimer == nil {
				sess.discTimer = time.AfterFunc(DisconnectGraceTimeout, func() {
					a.closeSession(sess)
				})
			}
			sess.mu.Unlock()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			a.closeSession(sess)
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		a.closeSession(sess)
		a.sendSignalError(msg.SessionID, "INVALID_OFFER", "SetRemoteDescription failed")
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		a.closeSession(sess)
		a.sendSignalError(msg.SessionID, "INTERNAL_ERROR", "CreateAnswer failed")
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		a.closeSession(sess)
		a.sendSignalError(msg.SessionID, "INTERNAL_ERROR", "SetLocalDescription failed")
		return
	}

	ansDesc, err := ExtractSessionDescriptor(answer.SDP, msg.Desc.SDPVer)
	if err != nil {
		a.closeSession(sess)
		a.sendSignalError(msg.SessionID, "INTERNAL_ERROR", "invalid local answer descriptor")
		return
	}
	ansDesc.Mid = msg.Desc.Mid

	sess.mu.Lock()
	sess.agentFP = ansDesc.FP
	sess.mu.Unlock()

	sigB64, err := a.cfg.Identity.SignAnswerTranscript(a.cfg.NodeID, msg.Nonce, msg.Desc, ansDesc)
	if err != nil {
		a.closeSession(sess)
		a.sendSignalError(msg.SessionID, "INTERNAL_ERROR", "sign answer failed")
		return
	}

	if err := a.sendSignal(signaling.AnswerRelayDTO{
		Type:      signaling.TypeAnswer,
		NodeID:    a.cfg.NodeID,
		SessionID: msg.SessionID,
		Desc:      ansDesc,
		PK:        a.cfg.Identity.PublicKeyBase64(),
		Sig:       sigB64,
	}); err != nil {
		a.closeSession(sess)
		return
	}

	a.completeOfferAndDrainCandidates(sess)
	go a.serveMuxStreams(sess, m)
}

func (a *Agent) onControlChannelOpen(sess *VisitorSession, pc *webrtc.PeerConnection, m *mux.Mux) {
	// Fail-Closed DTLS verification: SCTP and DTLS transports must be present and verified.
	sctpTr := pc.SCTP()
	if sctpTr == nil {
		log.Printf("[Agent] Rejecting session %s: missing SCTP transport", sess.signalSessionID)
		a.closeSession(sess)
		return
	}
	if caps := sctpTr.GetCapabilities(); caps.MaxMessageSize > 0 {
		m.SetMaxMessageSize(int(caps.MaxMessageSize))
	}
	dtlsTr := sctpTr.Transport()
	if dtlsTr == nil {
		log.Printf("[Agent] Rejecting session %s: missing DTLS transport", sess.signalSessionID)
		a.closeSession(sess)
		return
	}
	certDER := dtlsTr.GetRemoteCertificate()
	sess.mu.Lock()
	expectedFP := sess.visitorFP
	sess.mu.Unlock()
	if err := VerifyRemoteCertFingerprint(certDER, expectedFP); err != nil {
		log.Printf("[Agent] Rejecting session %s: %v", sess.signalSessionID, err)
		a.closeSession(sess)
		return
	}

	settingsPayload, _ := json.Marshal(map[string]any{
		"max_frame_payload": protocol.DefaultChunkSize,
		"pool_size":         a.cfg.PoolSize,
		"max_concurrent_ws": protocol.DefaultMaxWSConns,
	})
	if err := m.SendControlFrame(protocol.TypeSettings, settingsPayload); err != nil {
		a.closeSession(sess)
		return
	}

	agentNonce := randomTokenB64(32)
	var shareVB64 string
	var spakeSess *SPAKE2PVerifierSession
	if a.spakeVerifier != nil {
		var err error
		spakeSess, err = NewSPAKE2PVerifierSession(a.spakeVerifier)
		if err != nil {
			a.closeSession(sess)
			return
		}
		shareVB64 = base64.RawURLEncoding.EncodeToString(spakeSess.ShareV())
	}

	sess.mu.Lock()
	if sess.state != StateHandshaking {
		sess.mu.Unlock()
		return
	}
	sess.agentNonce = agentNonce
	sess.spakeSess = spakeSess
	sess.challengeReady = true
	sess.mu.Unlock()

	authMethods := []string{"cap"}
	if a.spakeVerifier != nil {
		authMethods = []string{"spake2p", "cap"}
	} else if a.cfg.AllowPublic {
		authMethods = []string{"public", "cap"}
	}
	chal := map[string]any{
		"agent_nonce":  agentNonce,
		"auth_methods": authMethods,
	}
	if shareVB64 != "" {
		chal["share_v"] = shareVB64
	}
	chalPayload, _ := json.Marshal(chal)
	if err := m.SendControlFrame(protocol.TypeAuthChallenge, chalPayload); err != nil {
		a.closeSession(sess)
		return
	}
}

const maxSDPMidBytes = 64

// HandleCandidate validates SDPMLineIndex/SDPMid bounds and enqueues incoming visitor ICE candidates
// onto a bounded per-session asynchronous queue so signaling reads never stall.
func (a *Agent) HandleCandidate(msg signaling.CandidateRelayDTO) {
	if msg.SessionID == "" || len(msg.Candidate) > signaling.MaxCandidateBytes || len(msg.SDPMid) > maxSDPMidBytes {
		return
	}
	init := webrtc.ICECandidateInit{Candidate: msg.Candidate}
	if msg.SDPMid != "" {
		m := msg.SDPMid
		init.SDPMid = &m
	}
	if msg.SDPMLineIndex != nil {
		idx := *msg.SDPMLineIndex
		if idx < 0 || idx > math.MaxUint16 {
			return
		}
		v := uint16(idx)
		init.SDPMLineIndex = &v
	}

	a.sessMu.Lock()
	sess := a.sessions[msg.SessionID]
	a.sessMu.Unlock()
	if sess == nil {
		return
	}

	sess.mu.Lock()
	if sess.state == StateClosed {
		sess.mu.Unlock()
		return
	}
	if sess.offerInFlight {
		candSize := int64(len(msg.Candidate) + len(msg.SDPMid) + 16)
		if len(sess.stagedCands) < MaxStagedCandCountSession &&
			sess.stagedBytes+candSize <= MaxStagedCandBytesSession {
			if a.stagedGlobal.Add(candSize) <= MaxStagedCandBytesGlobal {
				sess.stagedCands = append(sess.stagedCands, init)
				sess.stagedBytes += candSize
			} else {
				a.stagedGlobal.Add(-candSize)
			}
		}
		sess.mu.Unlock()
		return
	}
	if sess.pc == nil {
		sess.mu.Unlock()
		return
	}
	ch := sess.candCh
	stop := sess.candStop
	sess.mu.Unlock()

	if ch == nil {
		return
	}
	select {
	case <-stop:
	case ch <- init:
	default:
		// Bounded candidate queue full: drop excess candidate non-blockingly.
	}
}

func (a *Agent) completeOfferAndDrainCandidates(sess *VisitorSession) {
	sess.mu.Lock()
	sess.offerInFlight = false
	queued := sess.stagedCands
	released := sess.stagedBytes
	sess.stagedCands = nil
	sess.stagedBytes = 0
	pc := sess.pc
	ch := sess.candCh
	stop := sess.candStop
	closed := sess.state == StateClosed
	sess.mu.Unlock()

	if released > 0 {
		a.stagedGlobal.Add(-released)
	}
	if closed || pc == nil || ch == nil {
		return
	}

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Agent] Recovered panic in candidate worker: %v", rec)
			}
		}()
		for _, cand := range queued {
			select {
			case <-stop:
				return
			case <-a.rootCtx.Done():
				return
			default:
			}
			_ = pc.AddICECandidate(cand)
		}
		for {
			select {
			case <-stop:
				return
			case <-a.rootCtx.Done():
				return
			case cand := <-ch:
				_ = pc.AddICECandidate(cand)
			}
		}
	}()
}

func (a *Agent) createHandshakingSession(signalSessionID, clientIP, visitorFP, visitorNonce string) (*VisitorSession, error) {
	var displaced *VisitorSession
	ipKey := signaling.RateLimitKey(clientIP)

	a.sessMu.Lock()
	if a.rootCtx.Err() != nil {
		a.sessMu.Unlock()
		return nil, errAgentClosed
	}
	if len(a.sessions) >= MaxConcurrentSessions {
		a.sessMu.Unlock()
		return nil, errTooManySessions
	}
	handshaking := 0
	perIPHandshaking := 0
	for _, s := range a.sessions {
		if s.signalSessionID == signalSessionID {
			continue
		}
		s.mu.Lock()
		if s.state == StateHandshaking {
			handshaking++
			if signaling.RateLimitKey(s.clientIP) == ipKey {
				perIPHandshaking++
			}
		}
		s.mu.Unlock()
	}
	if handshaking >= MaxHandshakingSessions || perIPHandshaking >= MaxHandshakingPerIP {
		a.sessMu.Unlock()
		return nil, errTooManyHandshakes
	}
	if old, exists := a.sessions[signalSessionID]; exists {
		displaced = old
	}

	sess := &VisitorSession{
		signalSessionID: signalSessionID,
		clientIP:        clientIP,
		state:           StateHandshaking,
		visitorFP:       visitorFP,
		visitorNonce:    visitorNonce,
		offerInFlight:   true,
		candCh:          make(chan webrtc.ICECandidateInit, MaxStagedCandCountSession),
		candStop:        make(chan struct{}),
	}
	sess.unauthTimer = time.AfterFunc(UnauthSessionTimeout, func() {
		sess.mu.Lock()
		stillUnauth := sess.state == StateHandshaking
		sess.mu.Unlock()
		if stillUnauth {
			// Do NOT record unauthenticated signaling-only handshake timeouts into
			// VisitorAuthLimiter.globalRing (prevents global lockout DoS from spoofed Offers).
			// Per-IP handshake flooding is already bounded by MaxHandshakingPerIP & MaxHandshakingSessions.
			a.closeSession(sess)
		}
	})
	a.sessions[signalSessionID] = sess
	a.sessMu.Unlock()

	if displaced != nil {
		a.closeSession(displaced)
	}
	return sess, nil
}

var (
	errAgentClosed       = errorString("agent is shutting down")
	errTooManySessions   = errorString("too many concurrent visitor sessions")
	errTooManyHandshakes = errorString("too many unauthenticated handshaking sessions")
)

type errorString string

func (e errorString) Error() string { return string(e) }
