package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

var (
	builtSignalBin string
	builtAgentBin  string
	binTempDir     string
)

// TestMain compiles cmd/signal (with -tags embedweb) and cmd/agent ONCE and reuses them across all CLI tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "v7-e2e-bins-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	binTempDir = dir
	builtSignalBin = filepath.Join(dir, "signal")
	builtAgentBin = filepath.Join(dir, "agent")

	repoRoot := ".."
	if _, statErr := os.Stat(filepath.Join(repoRoot, "web", "dist", "index.html")); statErr != nil {
		if out, berr := exec.Command("npm", "--prefix", filepath.Join(repoRoot, "web"), "run", "build").CombinedOutput(); berr != nil {
			fmt.Fprintf(os.Stderr, "web build failed: %v\n%s\n", berr, string(out))
			os.RemoveAll(dir)
			os.Exit(1)
		}
	}

	cmdS := exec.Command("go", "build", "-tags", "embedweb", "-o", builtSignalBin, "./cmd/signal")
	cmdS.Dir = repoRoot
	if out, berr := cmdS.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "build signal failed: %v\n%s\n", berr, string(out))
		os.RemoveAll(dir)
		os.Exit(1)
	}
	cmdA := exec.Command("go", "build", "-o", builtAgentBin, "./cmd/agent")
	cmdA.Dir = repoRoot
	if out, berr := cmdA.CombinedOutput(); berr != nil {
		fmt.Fprintf(os.Stderr, "build agent failed: %v\n%s\n", berr, string(out))
		os.RemoveAll(dir)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// TestExampleConfigs_StrictKnownFields validates configs/*.example.yaml directly against the
// REAL compiled agent and signal binaries (zero hand-copied mirror structs!).
func TestExampleConfigs_StrictKnownFields(t *testing.T) {
	// 1. Validate agent.example.yaml via real compiled agent binary (`-config` parses with KnownFields(true);
	//    it must NOT fail with "Failed to load config", and instead proceeds to Fail-Closed missing ACCESS_SECRET).
	agCmd := exec.Command(builtAgentBin, "-config", filepath.Join("..", "configs", "agent.example.yaml"), "-ephemeral-identity")
	agCmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	agOut, _ := agCmd.CombinedOutput()
	if strings.Contains(string(agOut), "Failed to load config") {
		t.Fatalf("configs/agent.example.yaml failed real binary KnownFields(true) parse: %s", string(agOut))
	}
	if !strings.Contains(string(agOut), "ACCESS_SECRET") {
		t.Fatalf("expected agent.example.yaml to pass YAML parse and reach Fail-Closed ACCESS_SECRET check, got: %s", string(agOut))
	}

	// 2. Validate server.example.yaml via real compiled signal binary.
	srvCmd := exec.Command(builtSignalBin, "-config", filepath.Join("..", "configs", "server.example.yaml"))
	srvCmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	srvOut, _ := srvCmd.CombinedOutput()
	if strings.Contains(string(srvOut), "Failed to load config") {
		t.Fatalf("configs/server.example.yaml failed real binary KnownFields(true) parse: %s", string(srvOut))
	}
	if !strings.Contains(string(srvOut), "SIGNAL_SECRET") {
		t.Fatalf("expected server.example.yaml to pass YAML parse and reach Fail-Closed SIGNAL_SECRET check, got: %s", string(srvOut))
	}
}

func TestCLI_Binaries_VersionMintAndFailClosed(t *testing.T) {
	outS, err := exec.Command(builtSignalBin, "-version").CombinedOutput()
	if err != nil || !strings.Contains(string(outS), "webrtc-gateway-signal") {
		t.Fatalf("signal -version failed: err=%v out=%s", err, string(outS))
	}
	outA, err := exec.Command(builtAgentBin, "-version").CombinedOutput()
	if err != nil || !strings.Contains(string(outA), "webrtc-gateway-agent") {
		t.Fatalf("agent -version failed: err=%v out=%s", err, string(outA))
	}

	// Fail-Closed: Signal on non-loopback without domains MUST exit non-zero.
	failSig := exec.Command(builtSignalBin, "-addr", "0.0.0.0:0", "-allow-anonymous-agents")
	if out, err := failSig.CombinedOutput(); err == nil {
		t.Fatalf("expected non-loopback signal without domains to Fail-Closed, got: %s", string(out))
	}

	// Fail-Closed: Agent without ACCESS_SECRET and without -allow-public MUST exit non-zero.
	keyPath := filepath.Join(t.TempDir(), "cli.ed25519.key")
	failAg := exec.Command(builtAgentBin, "-id", "clitest", "-target", "http://127.0.0.1:8080", "-identity-key", keyPath)
	failAg.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	if out, err := failAg.CombinedOutput(); err == nil {
		t.Fatalf("expected agent without ACCESS_SECRET to Fail-Closed, got: %s", string(out))
	}

	// Fail-Closed: Agent with invalid -upstream-host MUST exit non-zero.
	badHostAg := exec.Command(builtAgentBin, "-id", "clitest", "-target", "http://127.0.0.1:8080", "-upstream-host", "http://bad.example", "-allow-public", "-identity-key", keyPath)
	badHostAg.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	if out, err := badHostAg.CombinedOutput(); err == nil || !strings.Contains(string(out), "Invalid -upstream-host") {
		t.Fatalf("expected invalid -upstream-host to Fail-Closed, err=%v out=%s", err, string(out))
	}

	// Fail-Closed: Agent with -upstream-host in -dir static mode MUST exit non-zero.
	badDirHostAg := exec.Command(builtAgentBin, "-id", "clitest", "-dir", t.TempDir(), "-upstream-host", "app.example", "-allow-public", "-identity-key", keyPath)
	badDirHostAg.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	if out, err := badDirHostAg.CombinedOutput(); err == nil || !strings.Contains(string(out), "-upstream-host is only valid with -target") {
		t.Fatalf("expected -upstream-host with -dir to Fail-Closed, err=%v out=%s", err, string(out))
	}

	// Offline -mint-link prints pinned + capability share link cleanly to stdout.
	mintCmd := exec.Command(
		builtAgentBin,
		"-id", "clitest",
		"-identity-key", keyPath,
		"-mint-link", "2h",
		"-mint-path", "/public",
		"-mint-readonly",
		"-portal-url", "https://portal.example.com",
	)
	mintOut, err := mintCmd.Output()
	if err != nil {
		t.Fatalf("agent -mint-link failed: %v", err)
	}
	link := strings.TrimSpace(string(mintOut))
	if !strings.HasPrefix(link, "https://portal.example.com/#clitest?pk=") || !strings.Contains(link, "&cap=") {
		t.Fatalf("unexpected minted share link: %q", link)
	}
}

type e2eAuthResult struct {
	OK                bool   `json:"ok"`
	ConfirmV          string `json:"confirm_v"`
	Role              string `json:"role"`
	CookieResumeToken string `json:"cookie_resume_token"`
}

func dialLiveVisitor(
	t *testing.T,
	ctx context.Context,
	sigHTTPURL string,
	nodeID string,
	accessSecret string,
	extraHeaders http.Header,
) (*webrtc.PeerConnection, *mux.Mux, *e2eAuthResult, string, func()) {
	t.Helper()

	wsURL := strings.Replace(sigHTTPURL, "http://", "ws://", 1) + "/ws/client/" + nodeID
	hdr := http.Header{"Origin": {sigHTTPURL}}
	for k, vals := range extraHeaders {
		hdr[k] = vals
	}
	wsConn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("visitor ws dial: %v", err)
	}

	var sigSessionID string
	for {
		_, raw, rerr := wsConn.Read(ctx)
		if rerr != nil {
			t.Fatalf("read joined: %v", rerr)
		}
		var env map[string]any
		_ = json.Unmarshal(raw, &env)
		if env["type"] == "joined" {
			sigSessionID, _ = env["session_id"].(string)
			if online, _ := env["online"].(bool); online {
				break
			}
		} else if env["type"] == "peer_status" {
			if online, _ := env["online"].(bool); online {
				break
			}
		}
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("visitor NewPeerConnection: %v", err)
	}
	neg, ord := true, true
	ctrlID := protocol.ControlChannelID
	ctrlDC, err := pc.CreateDataChannel(protocol.ControlChannelLabel, &webrtc.DataChannelInit{
		Negotiated: &neg, ID: &ctrlID, Ordered: &ord,
	})
	if err != nil {
		t.Fatalf("create ctrlDC: %v", err)
	}
	poolDCs := make([]*webrtc.DataChannel, protocol.DefaultPoolSize)
	for i := 0; i < protocol.DefaultPoolSize; i++ {
		pid := protocol.PoolChannelID(i)
		dc, lerr := pc.CreateDataChannel(protocol.PoolChannelLabel(i), &webrtc.DataChannelInit{
			Negotiated: &neg, ID: &pid, Ordered: &ord,
		})
		if lerr != nil {
			t.Fatalf("create poolDC %d: %v", i, lerr)
		}
		poolDCs[i] = dc
	}

	type authChalDTO struct {
		AgentNonce string `json:"agent_nonce"`
		ShareV     string `json:"share_v"`
	}
	authChalCh := make(chan authChalDTO, 1)
	authResCh := make(chan e2eAuthResult, 1)
	vMux := mux.New(mux.Config{
		PoolSize: protocol.DefaultPoolSize,
		OnControlFrame: func(f *protocol.Frame) {
			switch f.Type {
			case protocol.TypeAuthChallenge:
				var c authChalDTO
				if json.Unmarshal(f.Payload, &c) == nil {
					select {
					case authChalCh <- c:
					default:
					}
				}
			case protocol.TypeAuthResult:
				var r e2eAuthResult
				if json.Unmarshal(f.Payload, &r) == nil {
					select {
					case authResCh <- r:
					default:
					}
				}
			}
		},
	})
	vMux.AttachControlDC(ctrlDC)
	ctrlDC.OnMessage(func(msg webrtc.DataChannelMessage) { vMux.HandleControlMessage(msg.Data) })
	for i, pdc := range poolDCs {
		idx := i
		dc := pdc
		dc.OnOpen(func() { vMux.AttachPoolDC(idx, dc) })
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { vMux.HandlePoolMessage(idx, msg.Data) })
	}

	var wsWriteMu sync.Mutex
	sendWS := func(v any) {
		raw, _ := json.Marshal(v)
		wsWriteMu.Lock()
		defer wsWriteMu.Unlock()
		_ = wsConn.Write(ctx, websocket.MessageText, raw)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		var mIdx *int
		if j.SDPMLineIndex != nil {
			v := int(*j.SDPMLineIndex)
			mIdx = &v
		}
		mid := ""
		if j.SDPMid != nil {
			mid = *j.SDPMid
		}
		sendWS(signaling.CandidateRelayDTO{
			Type: signaling.TypeCandidate, Candidate: j.Candidate, SDPMid: mid, SDPMLineIndex: mIdx,
		})
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("SetLocalDescription: %v", err)
	}
	offerDesc, err := agent.ExtractSessionDescriptor(offer.SDP, 2)
	if err != nil {
		t.Fatalf("ExtractSessionDescriptor(offer): %v", err)
	}
	visitorNonce := base64.RawURLEncoding.EncodeToString([]byte("visitor-e2e-nonce-32-bytes-12345"))
	sendWS(signaling.OfferRelayDTO{
		Type: signaling.TypeOffer, Desc: offerDesc, Nonce: visitorNonce,
	})

	ansDone := make(chan signaling.AnswerRelayDTO, 1)
	var candMu sync.Mutex
	var pendingCands []webrtc.ICECandidateInit
	remoteApplied := false
	go func() {
		for {
			_, raw, rerr := wsConn.Read(ctx)
			if rerr != nil {
				return
			}
			var env struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &env)
			switch env.Type {
			case signaling.TypeAnswer:
				var a signaling.AnswerRelayDTO
				if json.Unmarshal(raw, &a) == nil {
					select {
					case ansDone <- a:
					default:
					}
				}
			case signaling.TypeCandidate:
				var c signaling.CandidateRelayDTO
				if json.Unmarshal(raw, &c) == nil && c.Candidate != "" {
					var mIdx *uint16
					if c.SDPMLineIndex != nil {
						v := uint16(*c.SDPMLineIndex)
						mIdx = &v
					}
					ci := webrtc.ICECandidateInit{
						Candidate: c.Candidate, SDPMid: &c.SDPMid, SDPMLineIndex: mIdx,
					}
					candMu.Lock()
					applied := remoteApplied
					if !applied {
						pendingCands = append(pendingCands, ci)
					}
					candMu.Unlock()
					if applied {
						_ = pc.AddICECandidate(ci)
					}
				}
			}
		}
	}()

	var ans signaling.AnswerRelayDTO
	select {
	case ans = <-ansDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for Answer")
	}

	if _, err := agent.VerifyAnswerTranscript(ans.PK, nodeID, visitorNonce, offerDesc, ans.Desc, ans.Sig); err != nil {
		t.Fatalf("Ed25519 structured Answer verification failed: %v", err)
	}
	ansSDP, err := agent.SynthesizeSDP(ans.Desc, false)
	if err != nil {
		t.Fatalf("SynthesizeSDP(answer): %v", err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ansSDP}); err != nil {
		t.Fatalf("SetRemoteDescription: %v", err)
	}
	candMu.Lock()
	remoteApplied = true
	queued := pendingCands
	pendingCands = nil
	candMu.Unlock()
	for _, ci := range queued {
		_ = pc.AddICECandidate(ci)
	}

	var chal authChalDTO
	select {
	case chal = <-authChalCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for DTLS AUTH_CHALLENGE")
	}

	shareV, _ := base64.RawURLEncoding.DecodeString(chal.ShareV)
	creds, err := agent.DeriveSPAKE2PProverCreds(accessSecret, nodeID, ans.PK)
	if err != nil {
		t.Fatalf("DeriveSPAKE2PProverCreds: %v", err)
	}
	spCtx, err := agent.BuildSPAKE2PContext(nodeID, offerDesc.FP, ans.Desc.FP, ans.PK, visitorNonce, chal.AgentNonce)
	if err != nil {
		t.Fatalf("BuildSPAKE2PContext: %v", err)
	}
	shareP, confirmP, wantCV, _, err := agent.SPAKE2PProverCompute(creds, spCtx, shareV)
	if err != nil {
		t.Fatalf("SPAKE2PProverCompute: %v", err)
	}
	respRaw, _ := json.Marshal(map[string]any{
		"method":    "spake2p",
		"share_p":   base64.RawURLEncoding.EncodeToString(shareP),
		"confirm_p": base64.RawURLEncoding.EncodeToString(confirmP),
	})
	if err := vMux.SendControlFrame(protocol.TypeAuthResponse, respRaw); err != nil {
		t.Fatalf("send AUTH_RESPONSE: %v", err)
	}

	var authRes e2eAuthResult
	select {
	case authRes = <-authResCh:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for DTLS AUTH_RESULT")
	}
	if !authRes.OK {
		t.Fatalf("DTLS SPAKE2+ auth failed: %+v", authRes)
	}
	gotCV, _ := base64.RawURLEncoding.DecodeString(authRes.ConfirmV)
	if !bytes.Equal(gotCV, wantCV) {
		t.Fatal("SPAKE2+ verifier confirm_v mismatch")
	}

	cleanup := func() {
		vMux.Close()
		_ = pc.Close()
		_ = wsConn.Close(websocket.StatusNormalClosure, "")
	}
	return pc, vMux, &authRes, sigSessionID, cleanup
}

func TestFullV7Tunnel_HTTP_Cookie_WS_ClientIP_E2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var ipMu sync.Mutex
	var seenClientIP string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			ipMu.Lock()
			seenClientIP = r.Header.Get("X-Forwarded-For")
			ipMu.Unlock()
			http.SetCookie(w, &http.Cookie{
				Name: "session_id", Value: "secret-httponly-999", Path: "/", HttpOnly: true,
			})
			http.SetCookie(w, &http.Cookie{
				Name: "ui_theme", Value: "dark", Path: "/",
			})
			_, _ = w.Write([]byte("logged-in"))
		case "/whoami":
			c, err := r.Cookie("session_id")
			if err != nil {
				http.Error(w, "no session cookie", http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, "ok:%s:%s", c.Value, r.Header.Get("X-Forwarded-For"))
		case "/blob":
			payload := bytes.Repeat([]byte("INTEGRITY-BLOCK-"), 2048)
			sum := sha256.Sum256(payload)
			w.Header().Set("X-SHA256", fmt.Sprintf("%x", sum))
			_, _ = w.Write(payload)
		case "/ws":
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			conn.SetReadLimit(4 * 1024 * 1024)
			typ, data, rerr := conn.Read(r.Context())
			if rerr != nil {
				return
			}
			_ = conn.Write(r.Context(), typ, append([]byte("echo:"), data...))
			_ = conn.Close(websocket.StatusNormalClosure, "done")
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	sigSrv, err := signaling.NewSignalServer(signaling.ServerOptions{
		Addr:           "127.0.0.1:0",
		SignalSecret:   "cluster-secret-e2e",
		TrustedProxies: "127.0.0.0/8,::1/128",
	})
	if err != nil {
		t.Fatalf("NewSignalServer: %v", err)
	}
	defer sigSrv.Shutdown()
	sigHTTP := httptest.NewServer(sigSrv.Handler())
	defer sigHTTP.Close()

	id, _ := agent.GenerateEphemeralIdentity()
	pol, _ := proxy.NewDestinationPolicy(backend.URL, "")
	hp, _ := proxy.NewHTTPProxy(proxy.HTTPProxyOptions{Policy: pol})
	wp, _ := proxy.NewWSProxy(proxy.WSProxyOptions{Policy: pol})
	ag, err := agent.New(agent.Config{
		NodeID:       "e2enode",
		SignalURL:    strings.Replace(sigHTTP.URL, "http://", "ws://", 1) + "/ws",
		SignalSecret: "cluster-secret-e2e",
		AccessSecret: "owner-pass-e2e",
		Identity:     id,
		HTTPProxy:    hp,
		WSProxy:      wp,
		ICEMDNS:      false,
	})
	if err != nil {
		t.Fatal(err)
	}
	agCtx, agCancel := context.WithCancel(ctx)
	defer agCancel()
	go func() { _ = ag.RunSignalingLoop(agCtx) }()

	deadline := time.Now().Add(5 * time.Second)
	for sigSrv.Broker.GetAgent("e2enode") == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sigSrv.Broker.GetAgent("e2enode") == nil {
		t.Fatal("agent failed to register with live SignalServer")
	}

	pc, vMux, _, _, cleanup := dialLiveVisitor(
		t, ctx, sigHTTP.URL, "e2enode", "owner-pass-e2e",
		http.Header{"X-Forwarded-For": {"198.51.100.77"}},
	)
	defer cleanup()

	doHTTP := func(target string) (*protocol.ResponseHeaders, []byte) {
		reqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
			Kind: protocol.StreamKindHTTP, FetchCtx: protocol.FetchCtxSameOriginClient, Method: "GET", Target: target,
		})
		st, err := vMux.OpenHTTPStream(ctx, reqTLV, true)
		if err != nil {
			t.Fatalf("OpenHTTPStream(%s): %v", target, err)
		}
		defer st.Close()
		hdrRaw, err := st.ReadResponseHeaders(ctx)
		if err != nil {
			t.Fatalf("ReadResponseHeaders(%s): %v", target, err)
		}
		rh, err := protocol.DecodeResponseHeaders(hdrRaw)
		if err != nil {
			t.Fatalf("DecodeResponseHeaders(%s): %v", target, err)
		}
		body, err := io.ReadAll(st)
		if err != nil {
			t.Fatalf("ReadAll(%s): %v", target, err)
		}
		return rh, body
	}

	// Step A: /login sets HttpOnly session_id (never leaks raw Set-Cookie!) + non-HttpOnly ui_theme delta!
	loginHdr, _ := doHTTP("/login")
	if loginHdr.Status != 200 {
		t.Fatalf("/login status=%d", loginHdr.Status)
	}
	ipMu.Lock()
	gotSeenIP := seenClientIP
	ipMu.Unlock()
	if gotSeenIP != "198.51.100.77" {
		t.Fatalf("trusted X-Forwarded-For not propagated: got %q", gotSeenIP)
	}
	for _, h := range loginHdr.Headers {
		if h.Name == "set-cookie" {
			t.Fatalf("raw Set-Cookie must never be forwarded to sandbox: %q", h.Value)
		}
	}

	// Step B: /whoami receives HttpOnly session_id injected by Agent's ProxyCookieJar!
	whoHdr, whoBody := doHTTP("/whoami")
	if whoHdr.Status != 200 || string(whoBody) != "ok:secret-httponly-999:198.51.100.77" {
		t.Fatalf("/whoami got status=%d body=%q", whoHdr.Status, string(whoBody))
	}

	// Step C: SHA-256 integrity over multi-frame binary stream.
	blobHdr, blobBody := doHTTP("/blob")
	var wantHash string
	for _, h := range blobHdr.Headers {
		if h.Name == "x-sha256" {
			wantHash = h.Value
		}
	}
	gotHash := fmt.Sprintf("%x", sha256.Sum256(blobBody))
	if gotHash != wantHash {
		t.Fatalf("SHA-256 mismatch: got=%s want=%s", gotHash, wantHash)
	}

	// Step D: Real dynamic DCEP "p2pws" DataChannel WebSocket echo!
	ord := true
	wsDC, err := pc.CreateDataChannel(protocol.WSChannelLabel, &webrtc.DataChannelInit{Ordered: &ord})
	if err != nil {
		t.Fatalf("CreateDataChannel(p2pws): %v", err)
	}
	wsOpen := make(chan struct{})
	wsDC.OnOpen(func() { close(wsOpen) })
	select {
	case <-wsOpen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for dynamic p2pws DC open")
	}

	wsReqTLV, _ := protocol.EncodeRequestHeaders(&protocol.RequestHeaders{
		Kind: protocol.StreamKindWS, Method: "GET", Target: "/ws",
	})
	wsSt, onMsg, onClose, err := vMux.OpenWSStream(ctx, wsDC, wsReqTLV)
	if err != nil {
		t.Fatalf("OpenWSStream: %v", err)
	}
	defer onClose()
	wsDC.OnMessage(func(m webrtc.DataChannelMessage) { onMsg(m.Data) })
	wsDC.OnClose(onClose)

	wsRespRaw, err := wsSt.ReadResponseHeaders(ctx)
	if err != nil {
		t.Fatalf("WS ReadResponseHeaders: %v", err)
	}
	wsResp, err := protocol.DecodeResponseHeaders(wsRespRaw)
	if err != nil || wsResp.Status != 101 {
		t.Fatalf("WS handshake expected 101, got err=%v resp=%+v", err, wsResp)
	}
	if err := wsSt.WriteWSMessage(false, []byte("ping-ws")); err != nil {
		t.Fatalf("WS WriteWSMessage: %v", err)
	}
	var echoBuf bytes.Buffer
	for {
		ch, rerr := wsSt.ReadWSChunk(ctx)
		if rerr != nil {
			t.Fatalf("WS ReadWSChunk: %v", rerr)
		}
		echoBuf.Write(ch.Payload)
		if ch.Fin {
			break
		}
	}
	if echoBuf.String() != "echo:ping-ws" {
		t.Fatalf("WS echo mismatch: got %q", echoBuf.String())
	}
}
