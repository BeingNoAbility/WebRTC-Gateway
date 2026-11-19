package agent

import (
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"webrtc-gateway/pkg/mux"
)

const (
	// MaxCandidatesPerSession limits early ICE candidates buffered per session to prevent memory exhaustion
	MaxCandidatesPerSession = 32
	// MaxPendingSessions limits the total number of unestablished/pending sessions stored
	MaxPendingSessions = 128
)

// AgentSession aggregates all state and manages connection generation for a visitor session.
type AgentSession struct {
	mu sync.Mutex

	ID         string
	Generation uint64
	CookieID   string
	ClientIP   string

	PC  *webrtc.PeerConnection
	Mux *mux.Mux

	pendingCandidates []webrtc.ICECandidateInit
	teardownTimer     *time.Timer
	closed            bool

	// offerMu serializes Offer and ICE Restart handling strictly for this session,
	// preventing concurrent PC creation races without blocking other sessions.
	offerMu sync.Mutex
}

func newAgentSession(id, cookieID, clientIP string) *AgentSession {
	return &AgentSession{
		ID:                id,
		Generation:        1,
		CookieID:          cookieID,
		ClientIP:          clientIP,
		pendingCandidates: make([]webrtc.ICECandidateInit, 0, 8),
	}
}

// AddCandidate buffers an early candidate, capping at MaxCandidatesPerSession.
func (s *AgentSession) AddCandidate(c webrtc.ICECandidateInit) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if len(s.pendingCandidates) >= MaxCandidatesPerSession {
		return false
	}
	s.pendingCandidates = append(s.pendingCandidates, c)
	return true
}

// DrainCandidates atomically returns and clears all buffered candidates.
func (s *AgentSession) DrainCandidates() []webrtc.ICECandidateInit {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingCandidates) == 0 {
		return nil
	}
	cands := s.pendingCandidates
	s.pendingCandidates = nil
	return cands
}

// CandidateCount returns the number of currently buffered candidates.
func (s *AgentSession) CandidateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingCandidates)
}

// CancelTeardown cancels any active grace teardown timer.
func (s *AgentSession) CancelTeardown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.teardownTimer != nil {
		s.teardownTimer.Stop()
		s.teardownTimer = nil
	}
}

// Destroy cleans up all resources associated with this session.
func (s *AgentSession) Destroy() {
	s.mu.Lock()
	s.closed = true
	if s.teardownTimer != nil {
		s.teardownTimer.Stop()
		s.teardownTimer = nil
	}
	pc := s.PC
	m := s.Mux
	s.PC = nil
	s.Mux = nil
	s.pendingCandidates = nil
	s.mu.Unlock()

	if m != nil {
		_ = m.Close()
	}
	if pc != nil {
		_ = pc.Close()
	}
}
