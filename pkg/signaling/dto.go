package signaling

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
)

const (
	MaxWSMessageBytes      = 64 * 1024 // 64 KiB hard WebSocket read limit (no raw SDP!)
	MaxCandidateBytes      = 2 * 1024  // 2 KiB max ICE candidate string
	MaxCandidatesPerClient = 128       // Never reset by subsequent offers
	MaxOffersPerWindow     = 6         // Per-session offer rate cap
	MaxWaitingPerNode      = 128       // Offline waiting clients per node
	MaxClientsPerNode      = 256       // Online concurrent clients per node
	MinNegotiatedMaxMsgSz  = 4096      // V7 floor so 64 KiB headers always fit <= 16 CONTINUATION frames
)

var (
	// Lowercase DNS-safe node IDs (no uppercase drift, no `_`, never `sbx-*`).
	nodeIDRegex   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,61}[a-z0-9])?$`)
	fpHexRegex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	iceUfragRegex = regexp.MustCompile(`^[A-Za-z0-9+/]{4,256}$`)
	icePwdRegex   = regexp.MustCompile(`^[A-Za-z0-9+/]{22,256}$`)
	midRegex      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)

	ErrTranscriptFieldTooLong = errors.New("signaling: length-prefixed transcript field exceeds 65535 bytes")
)

// NormalizeAndValidateNodeID canonicalizes a node ID to lowercase and validates syntax.
func NormalizeAndValidateNodeID(raw string) (string, bool) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if !nodeIDRegex.MatchString(id) {
		return "", false
	}
	if strings.HasPrefix(id, "sbx-") || strings.Contains(id, "..") {
		return "", false
	}
	return id, true
}

// SessionDescriptor replaces raw SDP relay with strictly validated DataChannel-only
// RFC 8841 transport parameters. Peers synthesize minimal SDP locally from this struct,
// eliminating all SDP parser differential and delimiter injection attacks by construction.
type SessionDescriptor struct {
	FP         string `json:"fp"`           // 64 lowercase hex chars (SHA-256 DTLS cert fingerprint)
	IceUfrag   string `json:"ice_ufrag"`    // RFC 5245 ice-ufrag
	IcePwd     string `json:"ice_pwd"`      // RFC 5245 ice-pwd
	Setup      string `json:"setup"`        // "actpass" (offer) or "active"/"passive" (answer)
	Mid        string `json:"mid"`          // single m=application media ID (e.g. "0")
	SCTPPort   int    `json:"sctp_port"`    // 1..65535 (typically 5000)
	MaxMsgSize int    `json:"max_msg_size"` // 4096..1073741824
	SDPVer     uint64 `json:"sdp_ver"`      // monotonic RFC 4566 o= session version (>= 2)
}

// Validate strictly validates every field of a SessionDescriptor.
func (d *SessionDescriptor) Validate(isOffer bool) error {
	if d == nil {
		return errors.New("missing session descriptor")
	}
	if !fpHexRegex.MatchString(d.FP) {
		return errors.New("invalid DTLS SHA-256 fingerprint")
	}
	if !iceUfragRegex.MatchString(d.IceUfrag) {
		return errors.New("invalid ice_ufrag")
	}
	if !icePwdRegex.MatchString(d.IcePwd) {
		return errors.New("invalid ice_pwd")
	}
	if isOffer {
		if d.Setup != "actpass" && d.Setup != "active" && d.Setup != "passive" {
			return fmt.Errorf("invalid offer setup %q", d.Setup)
		}
	} else {
		if d.Setup != "active" && d.Setup != "passive" {
			return fmt.Errorf("invalid answer setup %q", d.Setup)
		}
	}
	if !midRegex.MatchString(d.Mid) {
		return errors.New("invalid media mid")
	}
	if d.SCTPPort < 1 || d.SCTPPort > 65535 {
		return errors.New("invalid sctp_port")
	}
	if d.MaxMsgSize < MinNegotiatedMaxMsgSz || d.MaxMsgSize > 1073741824 {
		return errors.New("invalid max_msg_size")
	}
	if d.SDPVer < 2 || d.SDPVer > (1<<50) {
		return errors.New("invalid sdp_ver")
	}
	return nil
}

// AppendLenPrefixed appends length-prefixed (uint16 BE + bytes) fields to dst.
// Returns ErrTranscriptFieldTooLong if any field exceeds 65535 bytes (Fail-Closed against uint16 truncation).
func AppendLenPrefixed(dst []byte, fields ...string) ([]byte, error) {
	for _, f := range fields {
		n := len(f)
		if n < 0 || n > math.MaxUint16 {
			return nil, ErrTranscriptFieldTooLong
		}
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(n))
		dst = append(dst, lb[:]...)
		dst = append(dst, f...)
	}
	return dst, nil
}

// RegisterSignPayload returns the canonical V7 Pure-Ed25519 length-prefixed registration transcript.
func RegisterSignPayload(nodeID, nonce string) []byte {
	out, err := AppendLenPrefixed([]byte("p2pgw-v7:register-v2:"), nodeID, nonce)
	if err != nil {
		return nil
	}
	return out
}

// VerifyAgentRegistrationSignature verifies the Agent's Pure Ed25519 proof-of-possession over (nodeID, nonce).
func VerifyAgentRegistrationSignature(nodeID, nonce, pkB64, sigB64 string) (ed25519.PublicKey, error) {
	payload, err := AppendLenPrefixed([]byte("p2pgw-v7:register-v2:"), nodeID, nonce)
	if err != nil {
		return nil, err
	}
	pkBytes, err := base64.RawURLEncoding.DecodeString(pkB64)
	if err != nil || len(pkBytes) != ed25519.PublicKeySize {
		return nil, errors.New("invalid ed25519 public key")
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return nil, errors.New("invalid ed25519 signature")
	}
	pk := ed25519.PublicKey(pkBytes)
	if !ed25519.Verify(pk, payload, sigBytes) {
		return nil, errors.New("ed25519 signature verification failed")
	}
	return pk, nil
}

// Message types (strict wire vocabulary).
const (
	TypeChallenge  = "challenge"
	TypeRegister   = "register"
	TypeRegistered = "registered"
	TypeJoined     = "joined"
	TypePeerStatus = "peer_status"
	TypeOffer      = "offer"
	TypeAnswer     = "answer"
	TypeCandidate  = "candidate"
	TypeError      = "error"
	TypePing       = "ping"
	TypePong       = "pong"
)

// InboundEnvelope parses any inbound signaling JSON message cleanly without leaking extra fields.
type InboundEnvelope struct {
	Type string `json:"type"`

	// Agent register fields
	PK                 string `json:"pk,omitempty"`
	Sig                string `json:"sig,omitempty"`
	RequireVisitorAuth bool   `json:"require_visitor_auth,omitempty"`
	ForceRekey         bool   `json:"force_rekey,omitempty"`

	// Offer / Answer fields (structured SessionDescriptor — zero raw SDP!)
	SessionID string             `json:"session_id,omitempty"`
	Desc      *SessionDescriptor `json:"desc,omitempty"`
	Nonce     string             `json:"nonce,omitempty"`

	// Candidate fields
	Candidate     string `json:"candidate,omitempty"`
	SDPMid        string `json:"sdp_mid,omitempty"`
	SDPMLineIndex *int   `json:"sdp_mline_index,omitempty"`

	// Error fields
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// Strict allowlisted outbound DTOs (server always populates authoritative routing fields).

type ChallengeDTO struct {
	Type   string `json:"type"`
	NodeID string `json:"node_id"`
	Nonce  string `json:"nonce"`
}

type RegisteredDTO struct {
	Type   string `json:"type"`
	NodeID string `json:"node_id"`
}

type JoinedDTO struct {
	Type               string `json:"type"`
	NodeID             string `json:"node_id"`
	SessionID          string `json:"session_id"`
	Online             bool   `json:"online"`
	RequireVisitorAuth bool   `json:"require_visitor_auth"`
	AgentPK            string `json:"agent_pk,omitempty"`
}

type PeerStatusDTO struct {
	Type               string `json:"type"`
	NodeID             string `json:"node_id"`
	Online             bool   `json:"online"`
	RequireVisitorAuth bool   `json:"require_visitor_auth"`
	AgentPK            string `json:"agent_pk,omitempty"`
}

type OfferRelayDTO struct {
	Type      string             `json:"type"`
	NodeID    string             `json:"node_id"`
	SessionID string             `json:"session_id"`
	ClientIP  string             `json:"client_ip"`
	Desc      *SessionDescriptor `json:"desc"`
	Nonce     string             `json:"nonce"`
}

type AnswerRelayDTO struct {
	Type      string             `json:"type"`
	NodeID    string             `json:"node_id"`
	SessionID string             `json:"session_id"`
	Desc      *SessionDescriptor `json:"desc"`
	PK        string             `json:"pk"`
	Sig       string             `json:"sig"`
}

type CandidateRelayDTO struct {
	Type          string `json:"type"`
	NodeID        string `json:"node_id"`
	SessionID     string `json:"session_id"`
	Candidate     string `json:"candidate"`
	SDPMid        string `json:"sdp_mid,omitempty"`
	SDPMLineIndex *int   `json:"sdp_mline_index,omitempty"`
}

type ErrorDTO struct {
	Type      string `json:"type"`
	NodeID    string `json:"node_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Code      string `json:"code"`
	Error     string `json:"error"`
}

type PongDTO struct {
	Type string `json:"type"`
}
