package signaling

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

const (
	sessionSendQueueSize = 256
	wsWriteTimeout       = 10 * time.Second
	offerWindowDuration  = 60 * time.Second
)

var (
	ErrSessionClosed = errors.New("signaling: session closed")
	ErrQueueFull     = errors.New("signaling: outbound queue full")
)

type SessionRole string

const (
	RoleAgent  SessionRole = "agent"
	RoleClient SessionRole = "client"
)

// Session represents one WebSocket connection with immutable Role, NodeID, SessionID, and ClientIP.
type Session struct {
	ID       string
	Role     SessionRole
	NodeID   string
	ClientIP string

	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	outCh       chan []byte
	closeOnce   sync.Once
	errSendOnce sync.Once
	closed      atomic.Bool

	stateMu         sync.Mutex
	joinedAt        time.Time
	lastActivity    time.Time
	hasSentOffer    bool
	offerPendingAt  time.Time
	offerTimestamps []time.Time
	candidateCount  int
	requireVisAuth  bool
	agentPubKeyB64  string
}

func NewSession(parentCtx context.Context, conn *websocket.Conn, role SessionRole, nodeID, sessionID, clientIP string) *Session {
	ctx, cancel := context.WithCancel(parentCtx)
	now := time.Now()
	s := &Session{
		ID:           sessionID,
		Role:         role,
		NodeID:       nodeID,
		ClientIP:     clientIP,
		conn:         conn,
		ctx:          ctx,
		cancel:       cancel,
		outCh:        make(chan []byte, sessionSendQueueSize),
		joinedAt:     now,
		lastActivity: now,
	}
	go s.writeLoop()
	return s
}

func (s *Session) Context() context.Context { return s.ctx }

func (s *Session) writeLoop() {
	pingTicker := time.NewTicker(25 * time.Second)
	defer pingTicker.Stop()
	defer s.Close(websocket.StatusNormalClosure, "")

	for {
		select {
		case <-s.ctx.Done():
			return
		case raw := <-s.outCh:
			wctx, cancel := context.WithTimeout(s.ctx, wsWriteTimeout)
			err := s.conn.Write(wctx, websocket.MessageText, raw)
			cancel()
			if err != nil {
				return
			}
		case <-pingTicker.C:
			pctx, cancel := context.WithTimeout(s.ctx, wsWriteTimeout)
			err := s.conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// TrySend non-blockingly serializes and enqueues v.
// Returning ErrQueueFull NEVER closes an Agent session.
func (s *Session) TrySend(v any) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case <-s.ctx.Done():
		return ErrSessionClosed
	case s.outCh <- raw:
		return nil
	default:
		return ErrQueueFull
	}
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	n := maxBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Close closes the WebSocket with a clean status code and stops the writer loop.
func (s *Session) Close(status websocket.StatusCode, reason string) {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.cancel != nil {
			s.cancel()
		}
		if s.conn != nil {
			reason = truncateUTF8Bytes(reason, 120)
			_ = s.conn.Close(status, reason)
		}
	})
}

// CloseNow immediately aborts the underlying WebSocket connection without waiting on peer close handshake.
func (s *Session) CloseNow() {
	s.closed.Store(true)
	if s.cancel != nil {
		s.cancel()
	}
	if s.conn != nil {
		_ = s.conn.CloseNow()
	}
}

// SendErrorAndClose best-effort transmits an ErrorDTO before closing the connection
// synchronously (use SendErrorAndCloseAsync from shared loops like reaperLoop).
func (s *Session) SendErrorAndClose(code, msg string, wsStatus websocket.StatusCode) {
	s.errSendOnce.Do(func() {
		if !s.closed.Load() && s.conn != nil {
			if raw, err := json.Marshal(ErrorDTO{
				Type:   TypeError,
				NodeID: s.NodeID,
				Code:   code,
				Error:  msg,
			}); err == nil {
				parentCtx := s.ctx
				if parentCtx == nil {
					parentCtx = context.Background()
				}
				wctx, cancel := context.WithTimeout(parentCtx, 1500*time.Millisecond)
				_ = s.conn.Write(wctx, websocket.MessageText, raw)
				cancel()
			}
		}
		s.Close(wsStatus, code)
	})
}

// SendErrorAndCloseAsync performs SendErrorAndClose without blocking callers such as
// RegisterAgent or reaperLoop (prevents 64 slow-read sockets from stalling the broker reaper).
func (s *Session) SendErrorAndCloseAsync(code, msg string, wsStatus websocket.StatusCode) {
	go s.SendErrorAndClose(code, msg, wsStatus)
}

func (s *Session) MarkRegistered(pk []byte, requireVisitorAuth bool) {
	s.stateMu.Lock()
	s.requireVisAuth = requireVisitorAuth
	s.agentPubKeyB64 = base64.RawURLEncoding.EncodeToString(pk)
	s.stateMu.Unlock()
}

func (s *Session) RequireVisitorAuth() bool {
	s.stateMu.Lock()
	v := s.requireVisAuth
	s.stateMu.Unlock()
	return v
}

func (s *Session) AgentPKBase64() string {
	s.stateMu.Lock()
	v := s.agentPubKeyB64
	s.stateMu.Unlock()
	return v
}

// RecordOffer checks the per-session offer rate cap (6/60s), marks hasSentOffer=true,
// and starts the half-open SDP timer WITHOUT resetting candidateCount.
func (s *Session) RecordOffer(now time.Time) (allowed bool, prevHasSentOffer bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	cutoff := now.Add(-offerWindowDuration)
	kept := s.offerTimestamps[:0]
	for _, ts := range s.offerTimestamps {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= MaxOffersPerWindow {
		s.offerTimestamps = kept
		return false, s.hasSentOffer
	}
	prev := s.hasSentOffer
	s.offerTimestamps = append(kept, now)
	s.hasSentOffer = true
	s.lastActivity = now
	if s.offerPendingAt.IsZero() {
		s.offerPendingAt = now
	}
	return true, prev
}

// RollbackOffer restores hasSentOffer when an offer could not be forwarded (PEER_OFFLINE / AGENT_BUSY),
// preventing attackers from gaining post-offer immunity via rejected offers.
func (s *Session) RollbackOffer(prevHasSentOffer bool) {
	s.stateMu.Lock()
	s.hasSentOffer = prevHasSentOffer
	s.offerPendingAt = time.Time{}
	s.stateMu.Unlock()
}

func (s *Session) TouchActivity(now time.Time) {
	s.stateMu.Lock()
	s.lastActivity = now
	s.stateMu.Unlock()
}

func (s *Session) LastActivity() time.Time {
	s.stateMu.Lock()
	t := s.lastActivity
	s.stateMu.Unlock()
	return t
}

func (s *Session) ClearOfferPending() {
	s.stateMu.Lock()
	s.offerPendingAt = time.Time{}
	s.stateMu.Unlock()
}

func (s *Session) OfferPendingSince() time.Time {
	s.stateMu.Lock()
	t := s.offerPendingAt
	s.stateMu.Unlock()
	return t
}

func (s *Session) JoinedAt() time.Time {
	s.stateMu.Lock()
	t := s.joinedAt
	s.stateMu.Unlock()
	return t
}

func (s *Session) HasSentOffer() bool {
	s.stateMu.Lock()
	v := s.hasSentOffer
	s.stateMu.Unlock()
	return v
}

// AllowCandidate enforces post-Offer-only and strict non-resetting candidate quota.
func (s *Session) AllowCandidate() (allowed bool, preOffer bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.hasSentOffer {
		return false, true
	}
	if s.candidateCount >= MaxCandidatesPerClient {
		return false, false
	}
	s.candidateCount++
	return true, false
}
