package signaling

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func (s *SignalServer) verifyAgentUpgradeSecret(nodeID, authHdr string) (required bool, verified bool) {
	nodeSecret, hasNodeSecret := s.nodeSecrets[nodeID]
	clusterSecret := s.opts.SignalSecret
	if !hasNodeSecret && clusterSecret == "" {
		if len(s.nodeSecrets) > 0 {
			return true, false
		}
		return false, false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHdr, prefix) {
		return true, false
	}
	got := []byte(authHdr[len(prefix):])
	if hasNodeSecret {
		return true, subtle.ConstantTimeCompare(got, []byte(nodeSecret)) == 1
	}
	return true, subtle.ConstantTimeCompare(got, []byte(clusterSecret)) == 1
}

func (s *SignalServer) handleAgentWS(w http.ResponseWriter, r *http.Request, nodeID, clientIP string) {
	// HTTP Upgrade stage secret verification (rejects before spawning WS goroutines).
	// When per-node secrets are active, scope failure tracking by (clientIP, nodeID) ONLY for
	// configured nodeIDs so unknown/fake nodeIDs bucket under pure clientIP and cannot amplify
	// lockout slots, while authenticating with one node still cannot reset another node's lockout.
	secretVerified := false
	needsAuth := s.opts.SignalSecret != "" || len(s.nodeSecrets) > 0
	if needsAuth {
		authScope := ""
		if len(s.nodeSecrets) > 0 {
			if _, exists := s.nodeSecrets[nodeID]; exists {
				authScope = nodeID
			}
		}
		unlock := s.Limiter.LockAuthScope(clientIP, authScope)
		if s.Limiter.IsAuthLockedScoped(clientIP, authScope) {
			unlock()
			http.Error(w, "Too Many Requests: IP temporarily locked", http.StatusTooManyRequests)
			return
		}
		_, ok := s.verifyAgentUpgradeSecret(nodeID, r.Header.Get("Authorization"))
		if !ok {
			s.Limiter.RecordFailedAuthScoped(clientIP, authScope)
			unlock()
			http.Error(w, "Unauthorized: invalid or missing Bearer secret", http.StatusUnauthorized)
			return
		}
		s.Limiter.ResetFailedAuthScoped(clientIP, authScope)
		unlock()
		secretVerified = true
	}

	// Rate-limit Agent upgrades in an isolated bucket AFTER secret verification so unauthenticated
	// client floods or invalid-secret probes can never starve legitimate Agents.
	if !s.Limiter.AllowAgentUpgrade(clientIP) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	if !s.Limiter.AcquireAgentConn(clientIP) {
		http.Error(w, "Too Many Requests: Agent connection limit reached", http.StatusTooManyRequests)
		return
	}

	clearStaticDeadlines(w)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // Origin & Host already strictly verified in handleWSRoute.
	})
	if err != nil {
		applyStaticDeadlines(w, r)
		s.Limiter.ReleaseAgentConn(clientIP)
		return
	}
	conn.SetReadLimit(MaxWSMessageBytes)

	s.wsWG.Add(1)
	defer s.wsWG.Done()

	sess := NewSession(s.rootCtx, conn, RoleAgent, nodeID, newSessionID(), clientIP)
	defer func() {
		s.Broker.CleanupSession(sess)
		sess.Close(websocket.StatusNormalClosure, "")
		s.Limiter.ReleaseAgentConn(clientIP)
	}()

	// Step 1: Send Ed25519 ownership challenge and enforce 10s handshake timeout.
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		sess.Close(websocket.StatusInternalError, "rng failure")
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	if err := sess.TrySend(ChallengeDTO{Type: TypeChallenge, NodeID: nodeID, Nonce: nonce}); err != nil {
		return
	}

	hctx, hcancel := context.WithTimeout(sess.Context(), agentHandshakeTimeout)
	_, rawReg, err := conn.Read(hctx)
	hcancel()
	if err != nil {
		sess.SendErrorAndClose("HANDSHAKE_TIMEOUT", "agent registration timed out", websocket.StatusCode(4008))
		return
	}

	var regEnv InboundEnvelope
	if err := json.Unmarshal(rawReg, &regEnv); err != nil || regEnv.Type != TypeRegister {
		sess.SendErrorAndClose("INVALID_REGISTER", "expected register message", websocket.StatusPolicyViolation)
		return
	}
	pk, err := VerifyAgentRegistrationSignature(nodeID, nonce, regEnv.PK, regEnv.Sig)
	if err != nil {
		sess.SendErrorAndClose("INVALID_SIGNATURE", err.Error(), websocket.StatusCode(4003))
		return
	}
	allowRekey := secretVerified && s.allowForceRekey && regEnv.ForceRekey
	// RegisterAgent atomically enqueues RegisteredDTO inside b.mu before exposing the agent or
	// notifying waiting clients so OfferRelayDTO can never overtake RegisteredDTO.
	if err := s.Broker.RegisterAgent(sess, pk, regEnv.RequireVisitorAuth, allowRekey); err != nil {
		code := "REGISTER_FAILED"
		if errors.Is(err, ErrNodeOwnershipConflict) {
			code = "NODE_OWNERSHIP_CONFLICT"
		} else if errors.Is(err, ErrBoundKeyCapacityFull) {
			code = "REGISTRY_FULL"
		}
		sess.SendErrorAndClose(code, err.Error(), websocket.StatusCode(4003))
		return
	}

	// Authenticated Agent gets its own high-throughput per-session message limiter (decoupled from visitor IP buckets).
	agentMsgLim := NewSessionMessageLimiter(DefaultMaxAgentSessionMsgs)

	// Step 2: Steady-state Agent signaling loop.
	for {
		_, raw, err := conn.Read(sess.Context())
		if err != nil {
			return
		}
		if sess.IsEvicted() || s.Broker.GetAgent(nodeID) != sess {
			return
		}
		if !agentMsgLim.Allow(time.Now()) {
			_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, Code: "RATE_LIMITED", Error: "rate limit exceeded"})
			continue
		}
		s.messagesTotal.Add(1)

		var env InboundEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			sess.SendErrorAndClose("INVALID_JSON", "malformed JSON frame", websocket.StatusPolicyViolation)
			return
		}
		switch env.Type {
		case TypePing:
			_ = sess.TrySend(PongDTO{Type: TypePong})
		case TypeAnswer:
			if env.SessionID == "" || env.Desc == nil || env.Desc.Validate(false) != nil || env.PK == "" || env.Sig == "" || len(env.Sig) > maxAnswerSigLen {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: env.SessionID, Code: "INVALID_ANSWER", Error: "invalid answer descriptor or signature fields"})
				continue
			}
			if env.PK != sess.AgentPKBase64() {
				_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, SessionID: env.SessionID, Code: "PK_MISMATCH", Error: "answer pk does not match registered agent identity"})
				continue
			}
			client := s.Broker.GetClientForNode(nodeID, env.SessionID)
			if client == nil {
				continue
			}
			client.ClearOfferPending()
			_ = client.TrySend(AnswerRelayDTO{
				Type:      TypeAnswer,
				NodeID:    nodeID,
				SessionID: client.ID,
				Desc:      env.Desc,
				PK:        env.PK,
				Sig:       env.Sig,
			})
		case TypeCandidate:
			if env.SessionID == "" || len(env.Candidate) > MaxCandidateBytes || len(env.SDPMid) > maxSDPMidLen || !isValidSDPMLineIndex(env.SDPMLineIndex) {
				continue
			}
			client := s.Broker.GetClientForNode(nodeID, env.SessionID)
			if client == nil {
				continue
			}
			_ = client.TrySend(CandidateRelayDTO{
				Type:          TypeCandidate,
				NodeID:        nodeID,
				SessionID:     client.ID,
				Candidate:     env.Candidate,
				SDPMid:        env.SDPMid,
				SDPMLineIndex: env.SDPMLineIndex,
			})
		case TypeError:
			if env.SessionID == "" {
				continue
			}
			if len(env.Code) > maxErrorCodeLen {
				env.Code = truncateUTF8Bytes(env.Code, maxErrorCodeLen)
			}
			if len(env.Error) > maxErrorMsgLen {
				env.Error = truncateUTF8Bytes(env.Error, maxErrorMsgLen)
			}
			client := s.Broker.GetClientForNode(nodeID, env.SessionID)
			if client == nil {
				continue
			}
			client.ClearOfferPending()
			_ = client.TrySend(ErrorDTO{
				Type:      TypeError,
				NodeID:    nodeID,
				SessionID: client.ID,
				Code:      env.Code,
				Error:     env.Error,
			})
		default:
			_ = sess.TrySend(ErrorDTO{Type: TypeError, NodeID: nodeID, Code: "UNSUPPORTED_TYPE", Error: "unsupported agent message type"})
		}
	}
}
