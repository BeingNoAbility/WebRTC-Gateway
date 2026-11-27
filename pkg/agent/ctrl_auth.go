package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
)

type authResponseEnvelope struct {
	Method            string `json:"method"` // "spake2p", "cap", or "public"
	ShareP            string `json:"share_p,omitempty"`
	ConfirmP          string `json:"confirm_p,omitempty"`
	CapToken          string `json:"cap_token,omitempty"`
	CookieResumeToken string `json:"cookie_resume_token,omitempty"`
}

type capReqEnvelope struct {
	ReqID      string `json:"req_id"`
	Action     string `json:"action"` // "mint", "list", "revoke"
	TTLSec     int64  `json:"ttl_sec,omitempty"`
	PathPrefix string `json:"path_prefix,omitempty"`
	Readonly   bool   `json:"readonly,omitempty"`
	MaxConns   int    `json:"max_conns,omitempty"`
	JTI        string `json:"jti,omitempty"`
	Offset     int    `json:"offset,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

type settingsEnvelope struct {
	MaxFramePayload int `json:"max_frame_payload"`
}

// BackendMeta is delivered to Portal in AUTH_RESULT (never exposes internal IP to capability guests).
type BackendMeta struct {
	NodeID         string              `json:"nodeId"`
	BackendOrigin  string              `json:"backendOrigin"`
	IsStaticMode   bool                `json:"isStaticMode"`
	InitialCookies []proxy.CookieDelta `json:"initialCookies,omitempty"`
}

func (a *Agent) handleControlFrame(sess *VisitorSession, f *protocol.Frame) {
	switch f.Type {
	case protocol.TypeSettings:
		var env settingsEnvelope
		if json.Unmarshal(f.Payload, &env) == nil && env.MaxFramePayload >= protocol.MinChunkSize {
			sess.mu.Lock()
			m := sess.mux
			sess.mu.Unlock()
			if m != nil {
				m.SetMaxMessageSize(env.MaxFramePayload + protocol.HeaderSize)
			}
		}
	case protocol.TypeAuthResponse:
		a.handleDTLSAuthResponse(sess, f.Payload)
	case protocol.TypeCapReq:
		a.handleCapabilityRPC(sess, f.Payload)
	}
}

func (a *Agent) handleDTLSAuthResponse(sess *VisitorSession, raw []byte) {
	now := time.Now()
	sess.mu.Lock()
	// Single auth attempt per connection MUST be latched BEFORE json.Unmarshal so malformed JSON
	// cannot be replayed during the 150ms rejection window!
	if sess.state != StateHandshaking || sess.authAttempt || sess.mux == nil {
		sess.mu.Unlock()
		return
	}
	sess.authAttempt = true
	clientIP := sess.clientIP
	visitorFP := sess.visitorFP
	agentFP := sess.agentFP
	visitorNonce := sess.visitorNonce
	agentNonce := sess.agentNonce
	spakeSess := sess.spakeSess
	sess.spakeSess = nil // Single-use SPAKE2+ verifier session!
	sess.mu.Unlock()

	var req authResponseEnvelope
	if err := json.Unmarshal(raw, &req); err != nil {
		a.rejectDTLSAuth(sess, "INVALID_AUTH", "malformed auth response", 0)
		return
	}

	if allowed, retryAfter := a.authLimiter.Allow(clientIP, now); !allowed {
		a.metrics.AuthFailureTotal.Add(1)
		a.rejectDTLSAuth(sess, "RATE_LIMITED", "too many failed authentication attempts", int(retryAfter.Seconds())+1)
		return
	}

	var role string
	var capScope *proxy.CapabilityScope
	var capJTI string
	var capExp int64
	var confirmVB64 string
	var cookiePrefix string
	resetOwnerLimiter := false

	switch req.Method {
	case "public":
		if !a.cfg.AllowPublic {
			a.authLimiter.RecordFailure(clientIP, now)
			a.metrics.AuthFailureTotal.Add(1)
			a.rejectDTLSAuth(sess, "AUTH_REQUIRED", "public access is disabled on this node", 0)
			return
		}
		role = "public"
		cookiePrefix = "public"

	case "spake2p":
		if a.spakeVerifier == nil || spakeSess == nil {
			a.authLimiter.RecordFailure(clientIP, now)
			a.metrics.AuthFailureTotal.Add(1)
			a.rejectDTLSAuth(sess, "AUTH_UNAVAILABLE", "owner password authentication not configured", 0)
			return
		}
		shareP, err1 := base64.RawURLEncoding.DecodeString(req.ShareP)
		confirmP, err2 := base64.RawURLEncoding.DecodeString(req.ConfirmP)
		if err1 != nil || err2 != nil {
			a.authLimiter.RecordFailure(clientIP, now)
			a.metrics.AuthFailureTotal.Add(1)
			a.rejectDTLSAuth(sess, "AUTH_FAILED", "malformed SPAKE2+ parameters", 0)
			return
		}
		ctxBytes, ctxErr := BuildSPAKE2PContext(
			a.cfg.NodeID, visitorFP, agentFP, a.cfg.Identity.PublicKeyBase64(), visitorNonce, agentNonce,
		)
		if ctxErr != nil {
			a.authLimiter.RecordFailure(clientIP, now)
			a.metrics.AuthFailureTotal.Add(1)
			a.rejectDTLSAuth(sess, "AUTH_FAILED", "invalid SPAKE2+ context", 0)
			return
		}
		confirmV, _, err := spakeSess.Finish(ctxBytes, shareP, confirmP)
		if err != nil {
			a.authLimiter.RecordFailure(clientIP, now)
			a.metrics.AuthFailureTotal.Add(1)
			a.rejectDTLSAuth(sess, "AUTH_FAILED", "invalid access password", 0)
			return
		}
		confirmVB64 = base64.RawURLEncoding.EncodeToString(confirmV)
		role = "owner"
		cookiePrefix = "owner"
		resetOwnerLimiter = true

	case "cap":
		p, err := a.cfg.Identity.VerifyCapability(req.CapToken, a.cfg.NodeID, now)
		if err != nil {
			a.authLimiter.RecordFailure(clientIP, now)
			a.metrics.AuthFailureTotal.Add(1)
			a.rejectDTLSAuth(sess, "CAP_INVALID", err.Error(), 0)
			return
		}
		if !a.capStore.AcquireConn(p) {
			a.metrics.AuthFailureTotal.Add(1)
			a.rejectDTLSAuth(sess, "CAP_REVOKED_OR_FULL", "capability token revoked or connection limit reached", 0)
			return
		}
		role = "capability"
		capJTI = p.JTI
		capExp = p.Exp
		capScope = &proxy.CapabilityScope{
			PathPrefix: p.PathPrefix,
			Readonly:   p.Readonly,
		}
		cookiePrefix = "cap:" + p.JTI

	default:
		a.authLimiter.RecordFailure(clientIP, now)
		a.metrics.AuthFailureTotal.Add(1)
		a.rejectDTLSAuth(sess, "UNSUPPORTED_AUTH_METHOD", "unsupported auth method", 0)
		return
	}

	if resetOwnerLimiter {
		a.authLimiter.Reset(clientIP)
	}

	cookieKey, resumeTok, jar := a.cookieMgr.AcquireOrMint(cookiePrefix, req.CookieResumeToken)

	// STRICT LOCK ORDER: a.sessMu -> sess.mu.
	// Re-check !IsRevoked(capJTI) atomically right before committing StateAuthenticated
	// to close the TOCTOU window between AcquireConn and concurrent Revoke + disconnectCapabilitySessions!
	a.sessMu.Lock()
	sess.mu.Lock()
	if sess.state != StateHandshaking {
		sess.mu.Unlock()
		a.sessMu.Unlock()
		if capJTI != "" {
			a.capStore.ReleaseConn(capJTI)
		}
		a.cookieMgr.Release(cookieKey)
		return
	}
	if capJTI != "" && a.capStore.IsRevoked(capJTI) {
		sess.mu.Unlock()
		a.sessMu.Unlock()
		a.capStore.ReleaseConn(capJTI)
		a.cookieMgr.Release(cookieKey)
		a.metrics.AuthFailureTotal.Add(1)
		a.rejectDTLSAuth(sess, "CAP_REVOKED_OR_FULL", "capability token revoked", 0)
		return
	}
	if sess.unauthTimer != nil {
		sess.unauthTimer.Stop()
		sess.unauthTimer = nil
	}
	sess.state = StateAuthenticated
	sess.role = role
	sess.capScope = capScope
	sess.capJTI = capJTI
	sess.capExp = capExp
	sess.cookieKey = cookieKey
	sess.cookieJar = jar
	sess.authDone.Store(true)
	m := sess.mux
	sess.mu.Unlock()
	a.sessMu.Unlock()

	a.metrics.ActiveSessions.Add(1)
	a.metrics.AuthSuccessTotal.Add(1)

	pol := a.cfg.HTTPProxy.Policy()
	backendOrigin := pol.EffectiveOrigin()
	if role == "capability" {
		backendOrigin = "http://" + a.cfg.NodeID + ".internal"
	}

	resObj := map[string]any{
		"ok":                  true,
		"role":                role,
		"cookie_resume_token": resumeTok,
		"meta": BackendMeta{
			NodeID:         a.cfg.NodeID,
			BackendOrigin:  backendOrigin,
			IsStaticMode:   a.cfg.IsStaticMode,
			InitialCookies: jar.ExportNonHttpOnlySnapshot(pol.EffectiveBaseURL()),
		},
	}
	if confirmVB64 != "" {
		resObj["confirm_v"] = confirmVB64
	}
	resPayload, _ := json.Marshal(resObj)
	if err := m.SendControlFrame(protocol.TypeAuthResult, resPayload); err != nil {
		go a.closeSession(sess)
	}
}

func (a *Agent) rejectDTLSAuth(sess *VisitorSession, code, msg string, retryAfterSec int) {
	sess.mu.Lock()
	m := sess.mux
	sess.mu.Unlock()
	if m != nil {
		resPayload, _ := json.Marshal(map[string]any{
			"ok":              false,
			"code":            code,
			"error":           msg,
			"retry_after_sec": retryAfterSec,
		})
		_ = m.SendControlFrame(protocol.TypeAuthResult, resPayload)
	}
	time.AfterFunc(150*time.Millisecond, func() {
		a.closeSession(sess)
	})
}

func (a *Agent) handleCapabilityRPC(sess *VisitorSession, raw []byte) {
	sess.mu.Lock()
	isOwner := sess.state == StateAuthenticated && sess.role == "owner"
	m := sess.mux
	sess.mu.Unlock()
	if !isOwner || m == nil {
		return
	}

	var req capReqEnvelope
	if err := json.Unmarshal(raw, &req); err != nil {
		a.sendCapResp(sess, m, req.ReqID, false, "invalid cap request", nil)
		return
	}

	now := time.Now()
	switch req.Action {
	case "mint":
		ttl := time.Duration(req.TTLSec) * time.Second
		if ttl <= 0 {
			ttl = 2 * time.Hour
		}
		if ttl > 365*24*time.Hour {
			ttl = 365 * 24 * time.Hour
		}
		jti := "cap-" + randomTokenB64(12)
		payload := CapabilityPayload{
			V:          CapabilityVersion,
			JTI:        jti,
			Node:       a.cfg.NodeID,
			Exp:        now.Add(ttl).Unix(),
			PathPrefix: strings.TrimSpace(req.PathPrefix),
			Readonly:   req.Readonly,
			MaxConns:   req.MaxConns,
		}
		tok, err := a.cfg.Identity.MintCapability(payload)
		if err != nil {
			a.sendCapResp(sess, m, req.ReqID, false, err.Error(), nil)
			return
		}
		if err := a.capStore.Track(payload); err != nil {
			a.sendCapResp(sess, m, req.ReqID, false, "failed to persist capability state", nil)
			return
		}
		a.sendCapResp(sess, m, req.ReqID, true, "", map[string]any{
			"token":   tok,
			"pk_hash": a.cfg.Identity.PKHash(),
			"payload": payload,
		})

	case "list":
		offset := req.Offset
		if offset < 0 {
			offset = 0
		}
		limit := req.Limit
		if limit <= 0 {
			limit = DefaultCapListLimit
		}
		if limit > MaxCapListLimit {
			limit = MaxCapListLimit
		}
		items, total, hasMore := a.capStore.ListPage(now, offset, limit)
		a.sendCapResp(sess, m, req.ReqID, true, "", map[string]any{
			"items":    items,
			"total":    total,
			"offset":   offset,
			"limit":    limit,
			"has_more": hasMore,
		})

	case "revoke":
		if req.JTI == "" {
			a.sendCapResp(sess, m, req.ReqID, false, "missing jti", nil)
			return
		}
		persistErr := a.capStore.Revoke(req.JTI)
		a.cookieMgr.DeleteByPrefix("cap:" + req.JTI + ":")
		go a.disconnectCapabilitySessions(req.JTI)
		if persistErr != nil {
			a.sendCapResp(sess, m, req.ReqID, false, "revoked in memory but failed to persist state", map[string]any{"revoked": req.JTI})
			return
		}
		a.sendCapResp(sess, m, req.ReqID, true, "", map[string]any{"revoked": req.JTI})

	default:
		a.sendCapResp(sess, m, req.ReqID, false, fmt.Sprintf("unknown action %q", req.Action), nil)
	}
}

func (a *Agent) sendCapResp(sess *VisitorSession, m *mux.Mux, reqID string, ok bool, errMsg string, data map[string]any) {
	out := map[string]any{
		"req_id": reqID,
		"ok":     ok,
	}
	if errMsg != "" {
		out["error"] = errMsg
	}
	for k, v := range data {
		out[k] = v
	}
	raw, _ := json.Marshal(out)
	if err := m.SendControlFrame(protocol.TypeCapResp, raw); err != nil {
		go a.closeSession(sess)
	}
}
