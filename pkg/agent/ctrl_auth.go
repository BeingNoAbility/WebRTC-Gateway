package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/proxy"
)

const (
	maxCapTTLSec      int64 = 365 * 24 * 3600
	maxCapRPCInFlight int32 = 4
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
	NodeID              string              `json:"nodeId"`
	BackendOrigin       string              `json:"backendOrigin"`
	CookieSecureContext bool                `json:"cookieSecureContext"`
	InitialCookies      []proxy.CookieDelta `json:"initialCookies,omitempty"`
}

func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b, nil
}

// marshalBoundedAuthResult serializes AUTH_RESULT without HTML escaping and trims the lowest-priority
// tail of initialCookies via binary search if the JSON payload would exceed MaxControlJSONSize (128 KiB),
// ensuring valid large cookie jars can never brick session reconnects.
func marshalBoundedAuthResult(role, resumeTok, confirmVB64 string, meta BackendMeta) ([]byte, error) {
	build := func(cookies []proxy.CookieDelta) ([]byte, error) {
		m := meta
		m.InitialCookies = cookies
		resObj := map[string]any{
			"ok":                  true,
			"role":                role,
			"cookie_resume_token": resumeTok,
			"meta":                m,
		}
		if confirmVB64 != "" {
			resObj["confirm_v"] = confirmVB64
		}
		return marshalNoHTMLEscape(resObj)
	}

	raw, err := build(meta.InitialCookies)
	if err != nil || len(raw) <= protocol.MaxControlJSONSize || len(meta.InitialCookies) == 0 {
		return raw, err
	}

	// Binary-search the maximum prefix of deterministically sorted cookies that fits within MaxControlJSONSize.
	lo, hi := 0, len(meta.InitialCookies)-1
	var best []byte
	for lo <= hi {
		mid := (lo + hi) / 2
		cand, cerr := build(meta.InitialCookies[:mid])
		if cerr != nil {
			return nil, cerr
		}
		if len(cand) <= protocol.MaxControlJSONSize {
			best = cand
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best != nil {
		return best, nil
	}
	return build(nil)
}

func normalizeCapPathPrefix(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
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
		if !sess.authDispatched.CompareAndSwap(false, true) {
			return
		}
		payload := append([]byte(nil), f.Payload...)
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[Agent] Recovered panic in auth handler: %v", rec)
					a.closeSession(sess)
				}
			}()
			a.handleDTLSAuthResponse(sess, payload)
		}()
	case protocol.TypeCapReq:
		if sess.capInFlight.Add(1) > maxCapRPCInFlight {
			sess.capInFlight.Add(-1)
			return
		}
		payload := append([]byte(nil), f.Payload...)
		go func() {
			defer sess.capInFlight.Add(-1)
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[Agent] Recovered panic in cap RPC handler: %v", rec)
				}
			}()
			a.handleCapabilityRPC(sess, payload)
		}()
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

	// Reload capability store from disk OUTSIDE authLimiter.LockKey(clientIP) so disk I/O
	// never holds the per-IP auth mutex.
	if req.Method == "cap" {
		a.capStore.MaybeReload()
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
			shareP, err1 := decodeStrictBase64URL(req.ShareP)
			confirmP, err2 := decodeStrictBase64URL(req.ConfirmP)
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
		a.cookieMgr.DeleteByPrefix(cookieKey)
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
	if m != nil {
		m.SetAuthenticated(true)
	}
	sess.mu.Unlock()
	a.sessMu.Unlock()

	a.metrics.ActiveSessions.Add(1)
	a.metrics.AuthSuccessTotal.Add(1)

	pol := a.cfg.HTTPProxy.Policy()
	backendOrigin := pol.EffectiveOrigin()
	if role != "owner" {
		backendOrigin = "http://" + a.cfg.NodeID + ".internal"
	}

	meta := BackendMeta{
		NodeID:              a.cfg.NodeID,
		BackendOrigin:       backendOrigin,
		CookieSecureContext: proxy.IsSecureCookieOrigin(pol.EffectiveBaseURL()),
		InitialCookies:      jar.ExportNonHttpOnlySnapshot(pol.EffectiveBaseURL()),
	}
	resPayload, err := marshalBoundedAuthResult(role, resumeTok, confirmVB64, meta)
	if err != nil || m.SendControlFrame(protocol.TypeAuthResult, resPayload) != nil {
		go a.closeSession(sess)
	}
}

func (a *Agent) rejectDTLSAuth(sess *VisitorSession, code, msg string, retryAfterSec int) {
	sess.mu.Lock()
	if sess.unauthTimer != nil {
		sess.unauthTimer.Stop()
		sess.unauthTimer = nil
	}
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
		ttlSec := req.TTLSec
		if ttlSec <= 0 {
			ttlSec = 2 * 3600
		}
		if ttlSec > maxCapTTLSec {
			ttlSec = maxCapTTLSec
		}
		ttl := time.Duration(ttlSec) * time.Second
		jti := "cap-" + randomTokenB64(12)
		payload := CapabilityPayload{
			V:          CapabilityVersion,
			JTI:        jti,
			Node:       a.cfg.NodeID,
			Exp:        now.Add(ttl).Unix(),
			PathPrefix: normalizeCapPathPrefix(req.PathPrefix),
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
		jti := strings.TrimSpace(req.JTI)
		if jti == "" || len(jti) > maxCapabilityJTILen {
			a.sendCapResp(sess, m, req.ReqID, false, "invalid jti", nil)
			return
		}
		revokeErr := a.capStore.Revoke(jti)
		if revokeErr != nil && !errors.Is(revokeErr, ErrCapPersistFailed) {
			a.sendCapResp(sess, m, req.ReqID, false, revokeErr.Error(), nil)
			return
		}
		a.cookieMgr.DeleteByPrefix("cap:" + jti + ":")
		go a.disconnectCapabilitySessions(jti)
		if revokeErr != nil {
			a.sendCapResp(sess, m, req.ReqID, false, "revoked in memory but failed to persist state", map[string]any{"jti": jti, "revoked": true})
			return
		}
		a.sendCapResp(sess, m, req.ReqID, true, "", map[string]any{"jti": jti, "revoked": true})

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
