package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

type chanDC struct {
	ch chan []byte
}

func (d *chanDC) Send(b []byte) error {
	cp := append([]byte(nil), b...)
	d.ch <- cp
	return nil
}
func (d *chanDC) BufferedAmount() uint64 { return 0 }
func (d *chanDC) Close() error           { return nil }

func TestAgentV7_Negative_SDPParserDifferentialAndDelimiterInjectionBlocked(t *testing.T) {
	// Conflicting session-level vs media-level fingerprints must be rejected.
	dualFPSdp := "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n" +
		"a=fingerprint:sha-256 " + FormatColonFingerprint(strings.Repeat("11", 32)) + "\r\n" +
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\nc=IN IP4 0.0.0.0\r\n" +
		"a=ice-ufrag:ufrag1\r\na=ice-pwd:password1234567890123456\r\n" +
		"a=fingerprint:sha-256 " + FormatColonFingerprint(strings.Repeat("22", 32)) + "\r\n" +
		"a=setup:active\r\na=mid:0\r\na=sctp-port:5000\r\n"
	if _, err := ExtractSessionDescriptor(dualFPSdp, 2); err == nil {
		t.Fatal("conflicting session/media fingerprints accepted")
	}

	// Multi-m= BUNDLE SDP must be rejected.
	multiMSdp := "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\na=group:BUNDLE 1\r\n" +
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:0\r\n" +
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:1\r\n"
	if _, err := ExtractSessionDescriptor(multiMSdp, 2); err == nil {
		t.Fatal("multi-section BUNDLE SDP accepted")
	}

	// NUL / pipe characters in SessionDescriptor rejected strictly.
	badDesc := &signaling.SessionDescriptor{
		FP: strings.Repeat("aa", 32), IceUfrag: "ufrag\x00|evil", IcePwd: "password1234567890123456",
		Setup: "actpass", Mid: "0", SCTPPort: 5000, MaxMsgSize: 262144, SDPVer: 2,
	}
	if err := badDesc.Validate(true); err == nil {
		t.Fatal("NUL/pipe injection in descriptor accepted")
	}
}

func TestMultiAlgoSDPFingerprintAndShortExpectedFP(t *testing.T) {
	sha256FP := strings.Repeat("ab", 32)
	sha512FP := strings.Repeat("cd", 64)
	sdp := "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n" +
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\nc=IN IP4 0.0.0.0\r\n" +
		"a=ice-ufrag:ufrag1234\r\na=ice-pwd:password1234567890123456\r\n" +
		"a=fingerprint:sha-512 " + FormatColonFingerprint(sha512FP) + "\r\n" +
		"a=fingerprint:sha-256 " + FormatColonFingerprint(sha256FP) + "\r\n" +
		"a=setup:actpass\r\na=mid:0\r\na=sctp-port:5000\r\n"
	desc, err := ExtractSessionDescriptor(sdp, 2)
	if err != nil {
		t.Fatalf("multi-algo SDP with sha-512 + sha-256 should succeed, got: %v", err)
	}
	if desc.FP != sha256FP {
		t.Fatalf("extracted FP mismatch: got %q want %q", desc.FP, sha256FP)
	}

	// VerifyRemoteCertFingerprint with short expectedFPHex must return error without panic.
	if err := VerifyRemoteCertFingerprint(nil, "abcd"); err == nil {
		t.Fatal("expected error for nil PC / short expectedFPHex")
	}
}

func TestSignalURLWhitespacePlaintextRejected(t *testing.T) {
	id, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := proxy.NewDestinationPolicy("http://127.0.0.1:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	hp, err := proxy.NewHTTPProxy(proxy.HTTPProxyOptions{Policy: pol})
	if err != nil {
		t.Fatal(err)
	}

	// Leading space before non-loopback ws:// must not bypass plaintext SIGNAL_SECRET check.
	if _, err := New(Config{
		NodeID:       "nas",
		SignalURL:    "  ws://portal.example.com/ws",
		SignalSecret: "secret-token",
		AccessSecret: "visitor-pass",
		Identity:     id,
		HTTPProxy:    hp,
	}); err == nil {
		t.Fatal("leading whitespace bypassed non-loopback plaintext ws:// SIGNAL_SECRET rejection")
	}

	// Whitespace-only ACCESS_SECRET or SIGNAL_SECRET must fail closed.
	if _, err := New(Config{
		NodeID:       "nas",
		SignalURL:    "wss://portal.example.com/ws",
		SignalSecret: "   ",
		AccessSecret: "visitor-pass",
		Identity:     id,
		HTTPProxy:    hp,
	}); err == nil {
		t.Fatal("whitespace-only SIGNAL_SECRET must be rejected")
	}
	if _, err := New(Config{
		NodeID:       "nas",
		SignalURL:    "wss://portal.example.com/ws",
		SignalSecret: "secret",
		AccessSecret: "\t  ",
		Identity:     id,
		HTTPProxy:    hp,
	}); err == nil {
		t.Fatal("whitespace-only ACCESS_SECRET must be rejected")
	}

	// Missing host in SignalURL must fail closed.
	if _, err := New(Config{
		NodeID:       "nas",
		SignalURL:    "wss://",
		SignalSecret: "secret",
		AccessSecret: "visitor-pass",
		Identity:     id,
		HTTPProxy:    hp,
	}); err == nil {
		t.Fatal("missing host in SignalURL must be rejected")
	}
}

func TestHandleCandidateRejectsOutOfRangeMLineIndex(t *testing.T) {
	pol, _ := proxy.NewDestinationPolicy("http://127.0.0.1:18080", "")
	hp, _ := proxy.NewHTTPProxy(proxy.HTTPProxyOptions{Policy: pol})
	id, _ := GenerateEphemeralIdentity()
	ag, err := New(Config{
		NodeID:       "nas",
		AccessSecret: "secret-cand-range",
		Identity:     id,
		HTTPProxy:    hp,
		ICEMDNS:      false,
		STUNServers:  []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	sess := &VisitorSession{
		signalSessionID: "cand-range-1",
		state:           StateHandshaking,
		offerInFlight:   true,
		pc:              &webrtc.PeerConnection{},
	}
	ag.sessMu.Lock()
	ag.sessions[sess.signalSessionID] = sess
	ag.sessMu.Unlock()
	defer func() {
		ag.sessMu.Lock()
		delete(ag.sessions, sess.signalSessionID)
		ag.sessMu.Unlock()
	}()

	for _, badIdx := range []int{-1, -99999, 65536, 1 << 20} {
		idx := badIdx
		ag.HandleCandidate(signaling.CandidateRelayDTO{
			Type:          signaling.TypeCandidate,
			NodeID:        "nas",
			SessionID:     sess.signalSessionID,
			Candidate:     "candidate:1 1 udp 2122260223 192.0.2.1 50000 typ host",
			SDPMid:        "0",
			SDPMLineIndex: &idx,
		})
	}
	if len(sess.stagedCands) != 0 {
		t.Fatalf("expected out-of-range SDPMLineIndex candidates to be dropped, staged=%d", len(sess.stagedCands))
	}
}

func TestAgentV7_Negative_NoRateLimitBypassAndZeroABBADeadlock(t *testing.T) {
	// Zero limiter leak check.
	lim := NewVisitorAuthLimiter()
	now := time.Now()
	for i := 0; i < 10000; i++ {
		lim.RecordFailure("198.51.100.1", now)
		lim.Reset("198.51.100.1")
	}
	if lim.Len() != 0 || lim.ListLen() != 0 {
		t.Fatalf("VisitorAuthLimiter leaked memory: entries=%d list=%d", lim.Len(), lim.ListLen())
	}

	id, _ := GenerateEphemeralIdentity()
	pol, _ := proxy.NewDestinationPolicy("http://127.0.0.1:8080", "")
	hp, _ := proxy.NewHTTPProxy(proxy.HTTPProxyOptions{Policy: pol})
	var mu sync.Mutex
	var lastCodes []string
	ag, err := New(Config{
		NodeID:       "nas",
		AccessSecret: "strong-secret-v7",
		Identity:     id,
		HTTPProxy:    hp,
		SendSignal: func(v any) error {
			if e, ok := v.(signaling.ErrorDTO); ok {
				mu.Lock()
				lastCodes = append(lastCodes, e.Code)
				mu.Unlock()
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	// Locked-out IP must be rejected before creating a PeerConnection.
	for f := 0; f < visitorAuthMaxFailures; f++ {
		ag.authLimiter.RecordFailure("198.51.100.77", time.Now())
	}
	validDesc := &signaling.SessionDescriptor{
		FP:         strings.Repeat("ab", 32),
		IceUfrag:   "ufrag1234",
		IcePwd:     "password1234567890123456",
		Setup:      "actpass",
		Mid:        "0",
		SCTPPort:   5000,
		MaxMsgSize: 262144,
		SDPVer:     3,
	}
	for i := 0; i < 5; i++ {
		ag.HandleOffer(signaling.OfferRelayDTO{
			Type:      signaling.TypeOffer,
			NodeID:    "nas",
			SessionID: "sess-attack",
			ClientIP:  "198.51.100.77",
			Desc:      validDesc,
			Nonce:     "0123456789abcdef0123",
		})
	}
	mu.Lock()
	if len(lastCodes) != 5 || lastCodes[0] != "RATE_LIMITED" || lastCodes[4] != "RATE_LIMITED" {
		t.Fatalf("locked-out client bypassed VisitorAuthLimiter: codes=%v", lastCodes)
	}
	mu.Unlock()

	// Concurrent HandleCandidate + disconnectCapabilitySessions must never AB-BA deadlock.
	fakeSess := &VisitorSession{
		signalSessionID: "cand-race",
		state:           StateHandshaking,
		offerInFlight:   true,
		pc:              &webrtc.PeerConnection{},
	}
	ag.sessMu.Lock()
	ag.sessions["cand-race"] = fakeSess
	ag.sessMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			ag.HandleCandidate(signaling.CandidateRelayDTO{
				SessionID: "cand-race",
				Candidate: "candidate:1 1 udp 1 127.0.0.1 9 typ host",
			})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			ag.disconnectCapabilitySessions("other-jti")
		}
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AB-BA deadlock between HandleCandidate and disconnectCapabilitySessions")
	}
	ag.sessMu.Lock()
	delete(ag.sessions, "cand-race")
	ag.sessMu.Unlock()

	// Plaintext ws:// with SIGNAL_SECRET to non-loopback host must fail closed.
	if _, err := New(Config{
		NodeID:       "nas",
		SignalURL:    "ws://signal.example.com/ws",
		SignalSecret: "secret-leak",
		AllowPublic:  true,
		Identity:     id,
		HTTPProxy:    hp,
	}); err == nil {
		t.Fatal("expected plaintext ws:// with SIGNAL_SECRET to non-loopback host to fail closed")
	}
}

func TestCapabilityRevocationTOCTOUAndContinuousEnforcement(t *testing.T) {
	id, _ := GenerateEphemeralIdentity()
	pol, _ := proxy.NewDestinationPolicy("http://127.0.0.1:18080", "")
	hp, _ := proxy.NewHTTPProxy(proxy.HTTPProxyOptions{Policy: pol})
	ag, err := New(Config{
		NodeID:       "nas",
		AccessSecret: "owner-pass",
		Identity:     id,
		HTTPProxy:    hp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	capPayload := CapabilityPayload{
		V: CapabilityVersion, JTI: "jti-live-revoke", Node: "nas",
		Exp: time.Now().Add(time.Hour).Unix(), PathPrefix: "/", Readonly: true, MaxConns: 2,
	}
	if err := ag.capStore.Track(capPayload); err != nil {
		t.Fatal(err)
	}

	// Authenticated capability session: once capStore.Revoke is called, dispatchStream must reject immediately.
	var gotReset protocol.ErrorCode
	resetCh := make(chan protocol.ErrorCode, 1)
	clientMux := mux.New(mux.Config{PoolSize: 1})
	serverMux := mux.New(mux.Config{PoolSize: 1})
	defer clientMux.Close()
	defer serverMux.Close()

	c2s := make(chan []byte, 32)
	s2c := make(chan []byte, 32)
	go func() {
		for b := range c2s {
			serverMux.HandleLaneMessage(0, b)
		}
	}()
	go func() {
		for b := range s2c {
			if f, derr := protocol.Decode(b); derr == nil && f.Type == protocol.TypeReset {
				if code, cerr := protocol.DecodeResetPayload(f.Payload); cerr == nil {
					select {
					case resetCh <- code:
					default:
					}
				}
			}
			clientMux.HandleLaneMessage(0, b)
		}
	}()
	clientMux.AttachLaneDC(0, &chanDC{ch: c2s})
	serverMux.AttachLaneDC(0, &chanDC{ch: s2c})

	sess := &VisitorSession{
		signalSessionID: "cap-sess-1",
		state:           StateAuthenticated,
		role:            "capability",
		capJTI:          capPayload.JTI,
		capExp:          capPayload.Exp,
		mux:             serverMux,
	}
	sess.authDone.Store(true)

	if err := ag.capStore.Revoke(capPayload.JTI); err != nil {
		t.Fatal(err)
	}

	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/check",
	})
	_, err = clientMux.OpenStream(reqTLV, false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := serverMux.AcceptStream(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ag.dispatchStream(sess, st)

	select {
	case gotReset = <-resetCh:
	case <-time.After(time.Second):
		t.Fatal("revoked capability stream did not receive RESET")
	}
	if gotReset != protocol.ErrCodeAuthRequired {
		t.Fatalf("expected ErrCodeAuthRequired on revoked capability stream, got %v", gotReset)
	}
}

type pionTestSession struct {
	ag           *Agent
	clientPC     *webrtc.PeerConnection
	ctrlDC       *webrtc.DataChannel
	poolDCs      []*webrtc.DataChannel
	offerDesc    *signaling.SessionDescriptor
	answer       signaling.AnswerRelayDTO
	visitorNonce string
	cleanup      func()
}

func startPionTestHandshake(t *testing.T, backendURL, accessSecret, sessionID string, poolSize int) *pionTestSession {
	t.Helper()
	pol, err := proxy.NewDestinationPolicy(backendURL, "")
	if err != nil {
		t.Fatal(err)
	}
	hp, err := proxy.NewHTTPProxy(proxy.HTTPProxyOptions{Policy: pol})
	if err != nil {
		t.Fatal(err)
	}
	id, err := GenerateEphemeralIdentity()
	if err != nil {
		t.Fatal(err)
	}

	answerCh := make(chan signaling.AnswerRelayDTO, 1)
	candCh := make(chan signaling.CandidateRelayDTO, 64)

	ag, err := New(Config{
		NodeID:       "nas",
		AccessSecret: accessSecret,
		Identity:     id,
		HTTPProxy:    hp,
		ICEMDNS:      false,
		STUNServers:  []string{},
		PoolSize:     poolSize,
		SendSignal: func(v any) error {
			switch m := v.(type) {
			case signaling.AnswerRelayDTO:
				answerCh <- m
			case signaling.CandidateRelayDTO:
				select {
				case candCh <- m:
				default:
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		ag.Close()
		t.Fatal(err)
	}

	neg, ord := true, true
	cid := protocol.ControlChannelID
	ctrlDC, err := clientPC.CreateDataChannel(protocol.ControlChannelLabel, &webrtc.DataChannelInit{
		Negotiated: &neg, ID: &cid, Ordered: &ord,
	})
	if err != nil {
		_ = clientPC.Close()
		ag.Close()
		t.Fatal(err)
	}
	poolDCs := make([]*webrtc.DataChannel, poolSize)
	for i := 0; i < poolSize; i++ {
		pid := protocol.PoolChannelID(i)
		dc, err := clientPC.CreateDataChannel(protocol.PoolChannelLabel(i), &webrtc.DataChannelInit{
			Negotiated: &neg, ID: &pid, Ordered: &ord,
		})
		if err != nil {
			_ = clientPC.Close()
			ag.Close()
			t.Fatal(err)
		}
		poolDCs[i] = dc
	}

	clientPC.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		var idx *int
		if j.SDPMLineIndex != nil {
			v := int(*j.SDPMLineIndex)
			idx = &v
		}
		mid := ""
		if j.SDPMid != nil {
			mid = *j.SDPMid
		}
		ag.HandleCandidate(signaling.CandidateRelayDTO{
			Type:          signaling.TypeCandidate,
			NodeID:        "nas",
			SessionID:     sessionID,
			Candidate:     j.Candidate,
			SDPMid:        mid,
			SDPMLineIndex: idx,
		})
	})

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		_ = clientPC.Close()
		ag.Close()
		t.Fatal(err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		_ = clientPC.Close()
		ag.Close()
		t.Fatal(err)
	}
	offerDesc, err := ExtractSessionDescriptor(offer.SDP, 2)
	if err != nil {
		_ = clientPC.Close()
		ag.Close()
		t.Fatal(err)
	}

	visitorNonce := randomTokenB64(32)
	go ag.HandleOffer(signaling.OfferRelayDTO{
		Type:      signaling.TypeOffer,
		NodeID:    "nas",
		SessionID: sessionID,
		ClientIP:  "198.51.100.50",
		Desc:      offerDesc,
		Nonce:     visitorNonce,
	})

	var ans signaling.AnswerRelayDTO
	select {
	case ans = <-answerCh:
	case <-time.After(5 * time.Second):
		_ = clientPC.Close()
		ag.Close()
		t.Fatal("timed out waiting for Answer")
	}

	ansSDP, err := SynthesizeSDP(ans.Desc, false)
	if err != nil {
		_ = clientPC.Close()
		ag.Close()
		t.Fatal(err)
	}
	if err := clientPC.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ansSDP}); err != nil {
		_ = clientPC.Close()
		ag.Close()
		t.Fatal(err)
	}
	stopCand := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopCand:
				return
			case c := <-candCh:
				init := webrtc.ICECandidateInit{Candidate: c.Candidate}
				if c.SDPMid != "" {
					m := c.SDPMid
					init.SDPMid = &m
				}
				if c.SDPMLineIndex != nil {
					u := uint16(*c.SDPMLineIndex)
					init.SDPMLineIndex = &u
				}
				_ = clientPC.AddICECandidate(init)
			}
		}
	}()

	return &pionTestSession{
		ag:           ag,
		clientPC:     clientPC,
		ctrlDC:       ctrlDC,
		poolDCs:      poolDCs,
		offerDesc:    offerDesc,
		answer:       ans,
		visitorNonce: visitorNonce,
		cleanup: func() {
			close(stopCand)
			_ = clientPC.Close()
			ag.Close()
		},
	}
}

func TestPreAuthPoolDCBlocked(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(200)
	}))
	defer backend.Close()

	h := startPionTestHandshake(t, backend.URL, "secret-pass-preauth", "unauth-1", 2)
	defer h.cleanup()

	pool0 := h.poolDCs[0]
	poolOpen := make(chan struct{})
	poolClosed := make(chan struct{})
	if pool0.ReadyState() == webrtc.DataChannelStateOpen {
		close(poolOpen)
	} else {
		pool0.OnOpen(func() {
			select {
			case <-poolOpen:
			default:
				close(poolOpen)
			}
		})
	}
	pool0.OnClose(func() {
		select {
		case <-poolClosed:
		default:
			close(poolClosed)
		}
	})

	select {
	case <-poolOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pool DC open")
	}

	// Send an HTTP HEADERS frame on Pool DC before completing SPAKE2+ auth.
	reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindHTTP, Method: "GET", Target: "/secret",
	})
	wire, _ := (&protocol.Frame{StreamID: 1, Type: protocol.TypeHeaders, Flags: protocol.FlagFin, Payload: reqTLV}).Encode()
	_ = pool0.Send(wire)

	// Agent must terminate the unauthenticated session and never hit backend.
	select {
	case <-poolClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("pre-auth Pool DC traffic did not terminate unauthenticated session")
	}
	if backendHits.Load() != 0 {
		t.Fatalf("pre-auth Pool DC traffic reached backend %d times", backendHits.Load())
	}
}

func TestAgentV7_EndToEnd_RealPionSPAKE2P_StructuredSDP_DTLSBind_AndHTTPProxy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "admin_sid", Value: "secret-cookie", Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "hello-v7:"+r.URL.Path+":"+r.Header.Get("Cookie"))
	}))
	defer backend.Close()

	const poolSize = 4
	h := startPionTestHandshake(t, backend.URL, "owner-pass-2026", "client-1", poolSize)
	defer h.cleanup()

	// Verify Pure Ed25519 structured transcript signature.
	pk, err := VerifyAnswerTranscript(h.answer.PK, "nas", h.visitorNonce, h.offerDesc, h.answer.Desc, h.answer.Sig)
	if err != nil {
		t.Fatalf("Ed25519 structured answer transcript verification failed: %v", err)
	}
	if PKHashFromPublicKey(pk) != h.ag.Identity().PKHash() {
		t.Fatal("pinned PKHash mismatch")
	}

	type authResDTO struct {
		OK                bool   `json:"ok"`
		ConfirmV          string `json:"confirm_v"`
		Role              string `json:"role"`
		CookieResumeToken string `json:"cookie_resume_token"`
	}
	type capRespDTO struct {
		OK     bool   `json:"ok"`
		Token  string `json:"token"`
		PKHash string `json:"pk_hash"`
	}
	authResultCh := make(chan authResDTO, 1)
	capRespCh := make(chan capRespDTO, 1)
	goAwayCh := make(chan protocol.ErrorCode, 1)
	var expectedConfirmV []byte

	var clientMux *mux.Mux
	clientMux = mux.New(mux.Config{
		PoolSize: poolSize,
		OnGoAway: func(code protocol.ErrorCode) {
			select {
			case goAwayCh <- code:
			default:
			}
		},
		OnControlFrame: func(frame *protocol.Frame) {
			switch frame.Type {
			case protocol.TypeAuthChallenge:
				var chal struct {
					AgentNonce string `json:"agent_nonce"`
					ShareV     string `json:"share_v"`
				}
				_ = json.Unmarshal(frame.Payload, &chal)
				shareV, _ := base64.RawURLEncoding.DecodeString(chal.ShareV)
				creds, err := DeriveSPAKE2PProverCreds("owner-pass-2026", "nas", h.answer.PK)
				if err != nil {
					t.Errorf("DeriveSPAKE2PProverCreds failed: %v", err)
					return
				}
				ctxBytes, err := BuildSPAKE2PContext("nas", h.offerDesc.FP, h.answer.Desc.FP, h.answer.PK, h.visitorNonce, chal.AgentNonce)
				if err != nil {
					t.Errorf("BuildSPAKE2PContext failed: %v", err)
					return
				}
				shareP, confirmP, wantCV, _, err := SPAKE2PProverCompute(creds, ctxBytes, shareV)
				if err != nil {
					t.Errorf("SPAKE2PProverCompute failed: %v", err)
					return
				}
				expectedConfirmV = wantCV
				respRaw, _ := json.Marshal(authResponseEnvelope{
					Method:   "spake2p",
					ShareP:   base64.RawURLEncoding.EncodeToString(shareP),
					ConfirmP: base64.RawURLEncoding.EncodeToString(confirmP),
				})
				_ = clientMux.SendControlFrame(protocol.TypeAuthResponse, respRaw)
			case protocol.TypeAuthResult:
				var res authResDTO
				_ = json.Unmarshal(frame.Payload, &res)
				authResultCh <- res
			case protocol.TypeCapResp:
				var cr capRespDTO
				_ = json.Unmarshal(frame.Payload, &cr)
				capRespCh <- cr
			}
		},
	})
	defer clientMux.Close()

	clientMux.AttachControlDC(h.ctrlDC)
	h.ctrlDC.OnMessage(func(msg webrtc.DataChannelMessage) { clientMux.HandleControlMessage(msg.Data) })
	for i, pdc := range h.poolDCs {
		idx := i
		dc := pdc
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			clientMux.AttachLaneDC(idx, dc)
		} else {
			dc.OnOpen(func() {
				clientMux.AttachLaneDC(idx, dc)
			})
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { clientMux.HandleLaneMessage(idx, msg.Data) })
	}

	var authRes authResDTO
	select {
	case authRes = <-authResultCh:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for in-DTLS SPAKE2+ AUTH_RESULT")
	}
	if !authRes.OK || authRes.Role != "owner" || authRes.CookieResumeToken == "" {
		t.Fatalf("unexpected AUTH_RESULT: %+v", authRes)
	}
	gotCV, err := base64.RawURLEncoding.DecodeString(authRes.ConfirmV)
	if err != nil || !bytes.Equal(gotCV, expectedConfirmV) {
		t.Fatalf("SPAKE2+ verifier mutual confirmation mismatch")
	}

	// Request 1: sets HttpOnly cookie in Agent ProxyCookieJar.
	req1, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{Kind: protocol.StreamKindHTTP, FetchCtx: protocol.FetchCtxSameOriginClient, Method: "GET", Target: "/first"})
	st1, err := clientMux.OpenStream(req1, false)
	if err != nil {
		t.Fatal(err)
	}
	rh1, err := st1.ReadResponseHeaders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resp1, _ := protocol.DecodeResponseHeaders(rh1)
	body1, _ := io.ReadAll(st1)
	_ = st1.Close()
	if resp1.Status != 200 || !strings.HasPrefix(string(body1), "hello-v7:/first:") {
		t.Fatalf("unexpected req1: status=%d body=%q", resp1.Status, string(body1))
	}

	// Request 2: verifies HttpOnly admin_sid cookie is automatically sent by Agent's ProxyCookieJar.
	req2, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{Kind: protocol.StreamKindHTTP, FetchCtx: protocol.FetchCtxSameOriginClient, Method: "GET", Target: "/second"})
	st2, err := clientMux.OpenStream(req2, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st2.ReadResponseHeaders(context.Background())
	body2, _ := io.ReadAll(st2)
	_ = st2.Close()
	if !strings.Contains(string(body2), "admin_sid=secret-cookie") {
		t.Fatalf("HttpOnly cookie missing on 2nd request: %q", string(body2))
	}

	// Owner mints a capability token over DTLS p2pctrl.
	mintReq, _ := json.Marshal(capReqEnvelope{
		ReqID: "m1", Action: "mint", TTLSec: 3600, PathPrefix: "/shared", Readonly: true,
	})
	_ = clientMux.SendControlFrame(protocol.TypeCapReq, mintReq)
	select {
	case cr := <-capRespCh:
		if !cr.OK || cr.Token == "" || cr.PKHash != h.ag.Identity().PKHash() {
			t.Fatalf("DTLS CAP_REQ mint failed: %+v", cr)
		}
		if _, err := h.ag.Identity().VerifyCapability(cr.Token, "nas", time.Now()); err != nil {
			t.Fatalf("minted capability failed verify: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for CAP_RESP")
	}

	// Graceful Agent shutdown MUST emit GOAWAY(GOING_AWAY) before closing DataChannels.
	h.ag.Close()
	select {
	case code := <-goAwayCh:
		if code != protocol.ErrCodeGoingAway {
			t.Fatalf("expected GOAWAY(GOING_AWAY), got %v", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for GOAWAY on graceful Agent.Close()")
	}
}

func TestRegression_P1_AuthResultBoundedWithinMaxControlJSONSize(t *testing.T) {
	// Populate a valid ProxyCookieJar with 35 RFC 6265 cookies containing `<` (which Go's default
	// encoding/json would expand 6x into `\u003c`, exploding past MaxControlJSONSize = 128 KiB).
	jar := proxy.NewProxyCookieJar()
	targetURL := mustParseTestURL("https://app.example.com/")
	val := strings.Repeat("<", 1500)
	for i := 0; i < 40; i++ {
		jar.SaveResponseCookies(targetURL, []string{
			"c" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "=" + val + "; Path=/",
		})
	}
	// Also append huge client cookies to push raw JSON past 128 KiB even without HTML escaping.
	var bigDeltas []proxy.CookieDelta
	bigDeltas = append(bigDeltas, jar.ExportNonHttpOnlySnapshot(targetURL)...)
	for i := 0; i < 80; i++ {
		bigDeltas = append(bigDeltas, proxy.CookieDelta{
			Op:    "set",
			Name:  "extra_" + strings.Repeat("k", 20) + string(rune('0'+i%10)),
			Value: strings.Repeat("v", 2000),
			Path:  "/",
		})
	}

	raw, err := marshalBoundedAuthResult("owner", "resume-tok", "", BackendMeta{
		NodeID:              "demo",
		BackendOrigin:       "https://app.example.com",
		IsStaticMode:        false,
		CookieSecureContext: proxy.IsSecureCookieOrigin(targetURL),
		InitialCookies:      bigDeltas,
	})
	if err != nil {
		t.Fatalf("marshalBoundedAuthResult failed: %v", err)
	}
	if len(raw) > protocol.MaxControlJSONSize {
		t.Fatalf("P1-6 regression: AUTH_RESULT exceeded MaxControlJSONSize: got %d > %d", len(raw), protocol.MaxControlJSONSize)
	}
	if bytes.Contains(raw, []byte(`\u003c`)) {
		t.Fatal("expected SetEscapeHTML(false) to avoid 6x JSON escaping bloat")
	}

	var decoded struct {
		OK   bool        `json:"ok"`
		Meta BackendMeta `json:"meta"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal AUTH_RESULT: %v", err)
	}
	if !decoded.OK || !decoded.Meta.CookieSecureContext || len(decoded.Meta.InitialCookies) == 0 {
		t.Fatalf("unexpected decoded AUTH_RESULT: ok=%v secure=%v cookies=%d", decoded.OK, decoded.Meta.CookieSecureContext, len(decoded.Meta.InitialCookies))
	}

	if got := normalizeCapPathPrefix("api/v1"); got != "/api/v1" {
		t.Fatalf("expected leading slash on cap path prefix, got %q", got)
	}
	if got := normalizeCapPathPrefix("  "); got != "" {
		t.Fatalf("expected empty cap path prefix, got %q", got)
	}
}

func mustParseTestURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return u
}
