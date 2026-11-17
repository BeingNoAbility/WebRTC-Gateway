package signaling

import (
	"encoding/json"
	"github.com/pion/webrtc/v4"
)

const (
	TypeRegister   = "register"
	TypeRegistered = "registered"
	TypeJoin       = "join"
	TypeOffer      = "offer"
	TypeAnswer     = "answer"
	TypeCandidate  = "candidate"
	TypeError      = "error"
	TypePeerStatus = "peer_status"
)

// Message is the standard JSON envelope used across the signaling channel.
type Message struct {
	Type               string                     `json:"type"`
	NodeID             string                     `json:"node_id,omitempty"`
	SessionID          string                     `json:"session_id,omitempty"`
	ClientIP           string                     `json:"client_ip,omitempty"`
	ClientToken        string                     `json:"client_token,omitempty"`
	Role               string                     `json:"role,omitempty"` // "agent" or "client"
	Secret             string                     `json:"secret,omitempty"`
	RequiresSecret     bool                       `json:"requires_secret,omitempty"`
	RequireVisitorAuth *bool                      `json:"require_visitor_auth,omitempty"`
	IceRestart         bool                       `json:"ice_restart,omitempty"`
	Exp                int64                      `json:"exp,omitempty"`  // Share link expiration timestamp (unix seconds)
	Sign               string                     `json:"sign,omitempty"` // HMAC-SHA256 signature for temporary share
	SDP                *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate          *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
	Error              string                     `json:"error,omitempty"`
	Status             string                     `json:"status,omitempty"` // "online", "offline"
}

// Encode serializes Message into JSON bytes.
func (m *Message) Encode() ([]byte, error) {
	return json.Marshal(m)
}

// DecodeMessage parses JSON bytes into a Message.
func DecodeMessage(data []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
