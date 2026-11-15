package signaling

import (
	"reflect"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestSignalingMessage_EncodeDecodeAllTypes(t *testing.T) {
	sdp := &webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  "v=0\r\no=- 12345 2 IN IP4 127.0.0.1\r\n",
	}
	candStr := "candidate:1 1 UDP 2130706431 192.168.1.50 54321 typ host"
	cand := &webrtc.ICECandidateInit{
		Candidate: candStr,
	}

	testMessages := []*Message{
		{
			Type:   TypeRegister,
			NodeID: "my-home-nas",
			Role:   "agent",
			Secret: "vault-secret-123",
		},
		{
			Type:   TypeRegistered,
			NodeID: "my-home-nas",
		},
		{
			Type:   TypeJoin,
			NodeID: "my-home-nas",
			Secret: "vault-secret-123",
		},
		{
			Type:           TypePeerStatus,
			NodeID:         "my-home-nas",
			SessionID:      "sess-999",
			Status:         "online",
			RequiresSecret: true,
		},
		{
			Type:      TypeOffer,
			NodeID:    "my-home-nas",
			SessionID: "sess-999",
			Secret:    "vault-secret-123",
			SDP:       sdp,
		},
		{
			Type:      TypeAnswer,
			NodeID:    "my-home-nas",
			SessionID: "sess-999",
			SDP:       sdp,
		},
		{
			Type:      TypeCandidate,
			NodeID:    "my-home-nas",
			SessionID: "sess-999",
			Candidate: cand,
		},
		{
			Type:      TypeError,
			NodeID:    "my-home-nas",
			SessionID: "sess-999",
			Error:     "unauthorized: invalid secret token",
		},
	}

	for _, msg := range testMessages {
		encoded, err := msg.Encode()
		if err != nil {
			t.Fatalf("Encode failed for type %q: %v", msg.Type, err)
		}

		decoded, err := DecodeMessage(encoded)
		if err != nil {
			t.Fatalf("Decode failed for type %q: %v", msg.Type, err)
		}

		if decoded.Type != msg.Type {
			t.Errorf("Type mismatch: expected %q, got %q", msg.Type, decoded.Type)
		}
		if decoded.NodeID != msg.NodeID {
			t.Errorf("NodeID mismatch: expected %q, got %q", msg.NodeID, decoded.NodeID)
		}
		if decoded.SessionID != msg.SessionID {
			t.Errorf("SessionID mismatch: expected %q, got %q", msg.SessionID, decoded.SessionID)
		}
		if decoded.Secret != msg.Secret {
			t.Errorf("Secret mismatch: expected %q, got %q", msg.Secret, decoded.Secret)
		}
		if decoded.RequiresSecret != msg.RequiresSecret {
			t.Errorf("RequiresSecret mismatch: expected %v, got %v", msg.RequiresSecret, decoded.RequiresSecret)
		}
		if decoded.Status != msg.Status {
			t.Errorf("Status mismatch: expected %q, got %q", msg.Status, decoded.Status)
		}
		if decoded.Error != msg.Error {
			t.Errorf("Error mismatch: expected %q, got %q", msg.Error, decoded.Error)
		}
		if msg.SDP != nil {
			if decoded.SDP == nil || decoded.SDP.SDP != msg.SDP.SDP {
				t.Errorf("SDP payload mismatch")
			}
		}
		if msg.Candidate != nil {
			if decoded.Candidate == nil || !reflect.DeepEqual(decoded.Candidate, msg.Candidate) {
				t.Errorf("Candidate payload mismatch")
			}
		}
	}
}

func TestSignalingMessage_MalformedJSON(t *testing.T) {
	malformed := []byte(`{"type": "register", "node_id": `)
	_, err := DecodeMessage(malformed)
	if err == nil {
		t.Errorf("Expected JSON parse error for malformed payload, got nil")
	}
}
