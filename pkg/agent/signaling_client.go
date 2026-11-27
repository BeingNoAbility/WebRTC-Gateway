package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"webrtc-gateway/pkg/signaling"
)

const (
	agentSignalPingInterval = 25 * time.Second
	agentSignalPingTimeout  = 10 * time.Second
	agentSignalReadTimeout  = 60 * time.Second
)

func jitterDuration(base time.Duration) time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return base
	}
	// Uniform jitter in [0.75, 1.25] (+/-25%) to avoid thundering-herd reconnect storms.
	frac := float64(binary.LittleEndian.Uint64(b[:])%10000) / 10000.0
	factor := 0.75 + 0.50*frac
	return time.Duration(float64(base) * factor)
}

// BuildAgentSignalURL converts a base signal URL (e.g. wss://portal.example.com/ws or wss://portal.example.com)
// into the canonical endpoint `/ws/agent/<node_id>`.
func BuildAgentSignalURL(rawBase, nodeID string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawBase))
	if err != nil {
		return "", "", err
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", "", fmt.Errorf("unsupported signal URL scheme %q", u.Scheme)
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", "", errors.New("missing host in signal URL")
	}
	u.Path = "/ws/agent/" + nodeID
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), u.Redacted(), nil
}

// RunSignalingLoop maintains the Agent's persistent WebSocket connection to the signaling server
// with context-aware shutdown, ping keepalive, and jittered exponential backoff that resets after healthy uptime.
func (a *Agent) RunSignalingLoop(ctx context.Context) error {
	dialURL, redactedURL, err := BuildAgentSignalURL(a.cfg.SignalURL, a.cfg.NodeID)
	if err != nil {
		return err
	}

	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			a.Close()
			return ctx.Err()
		}

		registeredAt, connErr := a.runOneSignalingConn(ctx, dialURL, redactedURL)
		a.sigRegistered.Store(false)
		if ctx.Err() != nil {
			a.Close()
			return ctx.Err()
		}
		// Reset backoff ONLY if registration succeeded and stayed healthy for >= 15 seconds.
		if !registeredAt.IsZero() && time.Since(registeredAt) >= 15*time.Second {
			backoff = 1 * time.Second
		}
		wait := jitterDuration(backoff)
		if connErr != nil {
			log.Printf("[Agent] Signaling disconnected (%v), retrying in %v...", connErr, wait.Round(time.Millisecond))
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			a.Close()
			return ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (a *Agent) runOneSignalingConn(ctx context.Context, dialURL, redactedURL string) (registeredAt time.Time, runErr error) {
	hdr := http.Header{}
	if a.cfg.SignalSecret != "" {
		hdr.Set("Authorization", "Bearer "+a.cfg.SignalSecret)
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, dialURL, &websocket.DialOptions{
		HTTPHeader: hdr,
	})
	dialCancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("dial %s: %w", redactedURL, err)
	}
	conn.SetReadLimit(signaling.MaxWSMessageBytes)
	defer func() {
		if ctx.Err() != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "agent stopping")
		} else {
			_ = conn.CloseNow()
		}
	}()

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()
	sendCh := make(chan []byte, 256)

	var lastAlive atomic.Int64
	lastAlive.Store(time.Now().UnixNano())

	// Writer & RFC 6455 Ping keepalive goroutine.
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Agent] Recovered panic in signaling writer: %v", rec)
				connCancel()
			}
		}()
		pingTicker := time.NewTicker(agentSignalPingInterval)
		defer pingTicker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case raw := <-sendCh:
				wctx, wcancel := context.WithTimeout(connCtx, agentSignalPingTimeout)
				werr := conn.Write(wctx, websocket.MessageText, raw)
				wcancel()
				if werr != nil {
					connCancel()
					return
				}
			case <-pingTicker.C:
				pctx, pcancel := context.WithTimeout(connCtx, agentSignalPingTimeout)
				perr := conn.Ping(pctx)
				pcancel()
				if perr != nil {
					connCancel()
					return
				}
				lastAlive.Store(time.Now().UnixNano())
			}
		}
	}()

	// 60s liveness watchdog: catches silently wedged or half-open signaling sockets even if sendCh starves pings.
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Agent] Recovered panic in signaling watchdog: %v", rec)
				connCancel()
			}
		}()
		watchTicker := time.NewTicker(agentSignalPingTimeout)
		defer watchTicker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-watchTicker.C:
				if time.Since(time.Unix(0, lastAlive.Load())) > agentSignalReadTimeout {
					log.Printf("[Agent] Signaling connection read/keepalive timeout (%v)", agentSignalReadTimeout)
					connCancel()
					return
				}
			}
		}
	}()

	enqueue := func(v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		select {
		case <-connCtx.Done():
			return errors.New("agent: signaling connection closed")
		case sendCh <- raw:
			return nil
		default:
			return errors.New("agent: signaling outbound queue full")
		}
	}

	// Step 1: Read challenge & complete Pure Ed25519 ownership handshake.
	hctx, hcancel := context.WithTimeout(connCtx, 10*time.Second)
	_, chRaw, err := conn.Read(hctx)
	hcancel()
	if err != nil {
		return time.Time{}, fmt.Errorf("read challenge: %w", err)
	}
	lastAlive.Store(time.Now().UnixNano())
	var ch signaling.ChallengeDTO
	if err := json.Unmarshal(chRaw, &ch); err != nil || ch.Type != signaling.TypeChallenge || ch.Nonce == "" {
		return time.Time{}, errors.New("invalid signaling challenge")
	}

	sigBytes := ed25519.Sign(a.cfg.Identity.PrivateKey, signaling.RegisterSignPayload(a.cfg.NodeID, ch.Nonce))
	regMsg := map[string]any{
		"type":                 signaling.TypeRegister,
		"pk":                   a.cfg.Identity.PublicKeyBase64(),
		"sig":                  base64.RawURLEncoding.EncodeToString(sigBytes),
		"require_visitor_auth": a.RequiresVisitorAuth(),
		"force_rekey":          a.cfg.ForceRekey,
	}
	if err := enqueue(regMsg); err != nil {
		return time.Time{}, err
	}

	ackCtx, ackCancel := context.WithTimeout(connCtx, 10*time.Second)
	_, ackRaw, err := conn.Read(ackCtx)
	ackCancel()
	if err != nil {
		return time.Time{}, fmt.Errorf("read register ack: %w", err)
	}
	lastAlive.Store(time.Now().UnixNano())
	var ack signaling.InboundEnvelope
	if err := json.Unmarshal(ackRaw, &ack); err != nil || ack.Type != signaling.TypeRegistered {
		return time.Time{}, fmt.Errorf("registration rejected: %s (%s)", ack.Code, ack.Error)
	}

	a.sigMu.Lock()
	a.sigSend = enqueue
	a.sigMu.Unlock()
	a.sigRegistered.Store(true)
	registeredAt = time.Now()
	defer func() {
		a.sigRegistered.Store(false)
		a.sigMu.Lock()
		a.sigSend = a.cfg.SendSignal
		a.sigMu.Unlock()
	}()

	log.Printf("[Agent] Registered node=%s pk=%s at %s", a.cfg.NodeID, a.cfg.Identity.PKHash(), redactedURL)

	// Step 2: Steady-state read loop.
	for {
		_, raw, err := conn.Read(connCtx)
		if err != nil {
			return registeredAt, err
		}
		lastAlive.Store(time.Now().UnixNano())
		var env signaling.InboundEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		switch env.Type {
		case signaling.TypePong:
		case signaling.TypeOffer:
			var offer signaling.OfferRelayDTO
			if json.Unmarshal(raw, &offer) == nil {
				// Synchronously stage handshaking session before spawning heavy WebRTC setup so
				// immediately following TypeCandidate frames never race and get dropped.
				if offerSDP, sess, ok := a.prepareOffer(offer); ok {
					go func(o signaling.OfferRelayDTO, sdp string, s *VisitorSession) {
						defer func() {
							if rec := recover(); rec != nil {
								log.Printf("[Agent] Recovered panic in completeOffer (session=%s): %v", o.SessionID, rec)
								a.closeSession(s)
							}
						}()
						a.completeOffer(o, sdp, s)
					}(offer, offerSDP, sess)
				}
			}
		case signaling.TypeCandidate:
			var cand signaling.CandidateRelayDTO
			if json.Unmarshal(raw, &cand) == nil {
				a.HandleCandidate(cand)
			}
		case signaling.TypeError:
			log.Printf("[Agent] Signal error (%s): %s", env.Code, env.Error)
		}
	}
}
