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
	// Also Fail-Closed if AUTH_RESPONSE raced ahead of DTLS cert FP verification (`!challengeReady`).
	if sess.state != StateHandshaking || sess.authAttempt || sess.mux == nil {
		sess.mu.Unlock()
		return
	}
	if !sess.challengeReady {
		sess.authAttempt = true
		sess.mu.Unlock()
		go a.closeSession(sess)
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

	type authOutcome struct {
		role         string
		capScope     *proxy.CapabilityScope
		capJTI       string
		capExp       int64
		confirmVB64  string
		cookiePrefix string
		rejectCode   string
		rejectMsg    string
		retryAfter   int
	}

	// Serialize auth attempts per IP bucket to eliminate concurrent multi-session brute-force windows.
	outcome := func() authOutcome {
		unlockIP := a.authLimiter.LockKey(clientIP)
		defer unlockIP()

		if allowed, retryAfter := a.authLimiter.Allow(clientIP, now); !allowed {
			a.metrics.AuthFailureTotal.Add(1)
			return authOutcome{
				rejectCode: "RATE_LIMITED",
				rejectMsg:  "too many failed authentication attempts",
				retryAfter: int(retryAfter.Seconds()) + 1,
			}
		}

		var out authOutcome
		resetOwnerLimiter := false

		switch req.Method {
		case "public":
			if !a.cfg.AllowPublic || a.spakeVerifier != nil {
				a.authLimiter.RecordFailure(clientIP, now)
				a.metrics.AuthFailureTotal.Add(1)
				return authOutcome{rejectCode: "AUTH_REQUIRED", rejectMsg: "public access is disabled on this node"}
			}
			out.role = "public"
			out.cookiePrefix = "public"

		case "spake2p":
			if a.spakeVerifier == nil || spakeSess == nil {
				a.authLimiter.RecordFailure(clientIP, now)
				a.metrics.AuthFailureTotal.Add(1)
				return authOutcome{rejectCode: "AUTH_UNAVAILABLE", rejectMsg: "owner password authentication not configured"}
			}
			shareP, err1 := base64.RawURLEncoding.DecodeString(req.ShareP)
			confirmP, err2 := base64.RawURLEncoding.DecodeString(req.ConfirmP)
			if err1 != nil || err2 != nil {
				a.authLimiter.RecordFailure(clientIP, now)
				a.metrics.AuthFailureTotal.Add(1)
				return authOutcome{rejectCode: "AUTH_FAILED", rejectMsg: "malformed SPAKE2+ parameters"}
			}
			ctxBytes, ctxErr := BuildSPAKE2PContext(
				a.cfg.NodeID, visitorFP, agentFP, a.cfg.Identity.PublicKeyBase64(), visitorNonce, agentNonce,
			)
			if ctxErr != nil {
				a.authLimiter.RecordFailure(clientIP, now)
				a.metrics.AuthFailureTotal.Add(1)
				return authOutcome{rejectCode: "AUTH_FAILED", rejectMsg: "invalid SPAKE2+ context"}
			}
			confirmV, _, err := spakeSess.Finish(ctxBytes, shareP, confirmP)
			if err != nil {
				a.authLimiter.RecordFailure(clientIP, now)
				a.metrics.AuthFailureTotal.Add(1)
				return authOutcome{rejectCode: "AUTH_FAILED", rejectMsg: "invalid access password"}
			}
			out.confirmVB64 = base64.RawURLEncoding.EncodeToString(confirmV)
			out.role = "owner"
			out.cookiePrefix = "owner"
			resetOwnerLimiter = true

		case "cap":
			p, err := a.cfg.Identity.VerifyCapability(req.CapToken, a.cfg.NodeID, now)
			if err != nil {
				a.authLimiter.RecordFailure(clientIP, now)
				a.metrics.AuthFailureTotal.Add(1)
				return authOutcome{rejectCode: "CAP_INVALID", rejectMsg: err.Error()}
			}
			if !a.capStore.AcquireConn(p) {
				a.metrics.AuthFailureTotal.Add(1)
				return authOutcome{rejectCode: "CAP_REVOKED_OR_FULL", rejectMsg: "capability token revoked or connection limit reached"}
			}
			out.role = "capability"
			out.capJTI = p.JTI
			out.capExp = p.Exp
			out.capScope = &proxy.CapabilityScope{
				PathPrefix: p.PathPrefix,
				Readonly:   p.Readonly,
			}
			out.cookiePrefix = "cap:" + p.JTI

		default:
			a.authLimiter.RecordFailure(clientIP, now)
			a.metrics.AuthFailureTotal.Add(1)
			return authOutcome{rejectCode: "UNSUPPORTED_AUTH_METHOD", rejectMsg: "unsupported auth method"}
		}

		if resetOwnerLimiter {
			a.authLimiter.Reset(clientIP)
		}
		return out
	}()

	if outcome.rejectCode != "" {
		a.rejectDTLSAuth(sess, outcome.rejectCode, outcome.rejectMsg, outcome.retryAfter)
		return
	}

	role := outcome.role
	capScope := outcome.capScope
	capJTI := outcome.capJTI
	capExp := outcome.capExp
	confirmVB64 := outcome.confirmVB64
	cookiePrefix := outcome.cookiePrefix

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
	if role != "owner" {
		backendOrigin = "http://" + a.cfg.NodeID + ".internal"
	}

	allCookies := jar.ExportNonHttpOnlySnapshot(pol.EffectiveBaseURL())
	resPayload := marshalBoundedAuthResult(m.MaxControlPayloadSize(), role, resumeTok, confirmVB64, BackendMeta{
		NodeID:        a.cfg.NodeID,
		BackendOrigin: backendOrigin,
		IsStaticMode:  a.cfg.IsStaticMode,
	}, allCookies)
	if err := m.SendControlFrame(protocol.TypeAuthResult, resPayload); err != nil {
		go a.closeSession(sess)
	}
}

// marshalBoundedAuthResult serializes AUTH_RESULT while ensuring InitialCookies never cause the
// control JSON frame to exceed the negotiated max control payload (preventing reconnect crash loops).
func marshalBoundedAuthResult(maxPayload int, role, resumeTok, confirmVB64 string, meta BackendMeta, allCookies []proxy.CookieDelta) []byte {
	build := func(cookies []proxy.CookieDelta) []byte {
		m := meta
		m.InitialCookies = cookies
		obj := map[string]any{
			"ok":                  true,
			"role":                role,
			"cookie_resume_token": resumeTok,
			"meta":                m,
		}
		if confirmVB64 != "" {
			obj["confirm_v"] = confirmVB64
		}
		raw, _ := json.Marshal(obj)
		return raw
	}
	if maxPayload <= 0 || maxPayload > protocol.MaxControlJSONSize {
		maxPayload = protocol.MaxControlJSONSize
	}
	if len(allCookies) == 0 {
		return build(nil)
	}
	if full := build(allCookies); len(full) <= maxPayload {
		return full
	}
	// Binary search largest prefix of InitialCookies that strictly fits inside maxPayload.
	lo, hi := 0, len(allCookies)
	best := build(nil)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		cand := build(allCookies[:mid])
		if len(cand) <= maxPayload {
			best = cand
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return best
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
		if req.MaxConns < 0 {
			a.sendCapResp(sess, m, req.ReqID, false, "max_conns cannot be negative", nil)
			return
		}
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
		packed, packedHasMore := fitCapListPage(m.MaxControlPayloadSize(), req.ReqID, items, total, offset, limit, hasMore)
		a.sendCapResp(sess, m, req.ReqID, true, "", map[string]any{
			"items":    packed,
			"total":    total,
			"offset":   offset,
			"limit":    limit,
			"has_more": packedHasMore,
		})

	case "revoke":
		jti := strings.TrimSpace(req.JTI)
		if jti == "" || len(jti) > maxCapabilityJTILen {
			a.sendCapResp(sess, m, req.ReqID, false, "invalid jti", nil)
			return
		}
		persistErr := a.capStore.Revoke(jti)
		a.cookieMgr.DeleteByPrefix("cap:" + jti + ":")
		go a.disconnectCapabilitySessions(jti)
		if persistErr != nil {
			a.sendCapResp(sess, m, req.ReqID, false, "revoked in memory but failed to persist state", map[string]any{"revoked": jti})
			return
		}
		a.sendCapResp(sess, m, req.ReqID, true, "", map[string]any{"revoked": jti})

	default:
		a.sendCapResp(sess, m, req.ReqID, false, fmt.Sprintf("unknown action %q", req.Action), nil)
	}
}

// fitCapListPage dynamically packs CAP_RESP list items to strictly fit within maxPayload,
// marking has_more=true whenever any page items are trimmed for small negotiated chunk sizes.
func fitCapListPage(maxPayload int, reqID string, items []CapabilityRecord, total, offset, limit int, hasMore bool) ([]CapabilityRecord, bool) {
	if maxPayload <= 0 || maxPayload > protocol.MaxControlJSONSize {
		maxPayload = protocol.MaxControlJSONSize
	}
	marshalLen := func(subset []CapabilityRecord, hm bool) int {
		raw, _ := json.Marshal(map[string]any{
			"req_id":   reqID,
			"ok":       true,
			"items":    subset,
			"total":    total,
			"offset":   offset,
			"limit":    limit,
			"has_more": hm,
		})
		return len(raw)
	}
	if len(items) == 0 || marshalLen(items, hasMore) <= maxPayload {
		return items, hasMore
	}
	lo, hi := 0, len(items)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		hm := hasMore || mid < len(items)
		if marshalLen(items[:mid], hm) <= maxPayload {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return []CapabilityRecord{}, true
	}
	return items[:lo], hasMore || lo < len(items)
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
