package signaling

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func (s *SignalServer) handleClientWS(w http.ResponseWriter, r *http.Request, nodeID, clientIP string) {
	if !s.Limiter.AcquireConn(clientIP) {
		http.Error(w, "Too Many Requests: IP connection limit reached", http.StatusTooManyRequests)
		return
	}

	clearStaticDeadlines(w)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin & Host already strictly verified in handleWSRoute.
	})
	if err != nil {
		applyStaticDeadlines(w, r)
		s.Limiter.ReleaseConn(clientIP)
		return
	}
	conn.SetReadLimit(MaxWSMessageBytes)

	s.wsWG.Add(1)
	defer s.wsWG.Done()

	sess := NewSession(s.rootCtx, conn, RoleClient, nodeID, newSessionID(), clientIP)
	defer func() {
		s.Broker.CleanupSession(sess)
		sess.Close(websocket.StatusNormalClosure, "")
		s.Limiter.ReleaseConn(clientIP)
	}()

	// JoinClient atomically enqueues JoinedDTO inside b.mu before any PeerStatusDTO can race ahead.
	if _, _, _, err := s.Broker.JoinClient(sess); err != nil {
		code := "WAITING_QUEUE_FULL"
		if errors.Is(err, ErrNodeClientLimitFull) {
			code = "NODE_CLIENT_LIMIT"
		} else if errors.Is(err, ErrBrokerClosed) {
			code = "SERVER_SHUTTING_DOWN"
		}
		sess.SendErrorAndClose(code, err.Error(), websocket.StatusCode(4029))
		return
	}

	for {
		_, raw, err := conn.Read(sess.Context())
		if err != nil {
			return
		}
		if sess.IsEvicted() {
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

		now := time.Now()
		switch env.Type {
		case TypePing:
			_ = sess.TrySend(PongDTO{Type: TypePong})
		case TypeOffer:
			if env.Desc == nil || env.Desc.Validate(true) != nil || len(env.Nonce) < 16 || len(env.Nonce) > 128 {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "INVALID_OFFER", Error: "invalid offer descriptor or nonce"})
				continue
			}
			allowed, prevHasOffer, prevOfferPend := sess.RecordOffer(now)
			if !allowed {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "OFFER_RATE_LIMITED", Error: "too many offers"})
				continue
			}
			agent := s.Broker.GetAgent(nodeID)
			if agent == nil {
				sess.RollbackOffer(prevHasOffer, prevOfferPend)
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
				sess.RollbackOffer(prevHasOffer, prevOfferPend)
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "AGENT_BUSY", Error: "agent signaling queue busy"})
			}
		case TypeCandidate:
			if len(env.Candidate) > MaxCandidateBytes || len(env.SDPMid) > maxSDPMidLen || !isValidSDPMLineIndex(env.SDPMLineIndex) {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: sess.ID, Code: "CANDIDATE_TOO_LARGE", Error: "candidate or sdp_mid exceeds limit"})
				continue
			}
			allowed, preOffer := sess.AllowCandidate(now)
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
			sess.TouchActivity(now)
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
