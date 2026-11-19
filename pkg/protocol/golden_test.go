package protocol

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type GoldenVectorFile struct {
	Description          string              `json:"description"`
	Version              string              `json:"version"`
	HeaderSize           int                 `json:"header_size"`
	RecommendedChunkSize int                 `json:"recommended_chunk_size"`
	MaxPayloadSize       int                 `json:"max_payload_size"`
	ValidVectors         []GoldenValidVector `json:"valid_vectors"`
	ErrorVectors         []GoldenErrorVector `json:"error_vectors"`
}

type GoldenValidVector struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	StreamID    uint32 `json:"stream_id"`
	Type        byte   `json:"type"`
	TypeName    string `json:"type_name"`
	Flags       byte   `json:"flags"`
	FlagsName   string `json:"flags_name"`
	Length      uint32 `json:"length"`
	PayloadHex  string `json:"payload_hex"`
	WireHex     string `json:"wire_hex"`
}

type GoldenErrorVector struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	WireHex       string `json:"wire_hex"`
	ExpectedError string `json:"expected_error"`
}

func loadGoldenVectors(t *testing.T) *GoldenVectorFile {
	// Search in common relative directories
	candidates := []string{
		"../../test/fixtures/protocol_golden_vectors.json",
		"test/fixtures/protocol_golden_vectors.json",
		"../test/fixtures/protocol_golden_vectors.json",
	}

	var data []byte
	var foundPath string
	for _, p := range candidates {
		if content, err := os.ReadFile(p); err == nil {
			data = content
			foundPath = p
			break
		}
	}

	if data == nil {
		// Try resolving via executable or working directory
		cwd, _ := os.Getwd()
		absCandidate := filepath.Join(cwd, "..", "..", "test", "fixtures", "protocol_golden_vectors.json")
		if content, err := os.ReadFile(absCandidate); err == nil {
			data = content
			foundPath = absCandidate
		}
	}

	if data == nil {
		t.Fatalf("Could not locate test/fixtures/protocol_golden_vectors.json")
	}

	var gvf GoldenVectorFile
	if err := json.Unmarshal(data, &gvf); err != nil {
		t.Fatalf("Failed to parse golden vector JSON from %s: %v", foundPath, err)
	}

	return &gvf
}

func TestP2PMux_GoldenVectors_Valid(t *testing.T) {
	gvf := loadGoldenVectors(t)

	// 1. Verify protocol baseline constants
	if gvf.HeaderSize != HeaderSize {
		t.Errorf("HeaderSize mismatch: golden JSON has %d, Go has %d", gvf.HeaderSize, HeaderSize)
	}
	if gvf.RecommendedChunkSize != RecommendedChunkSize {
		t.Errorf("RecommendedChunkSize mismatch: golden JSON has %d, Go has %d", gvf.RecommendedChunkSize, RecommendedChunkSize)
	}
	if gvf.MaxPayloadSize != MaxPayloadSize {
		t.Errorf("MaxPayloadSize mismatch: golden JSON has %d, Go has %d", gvf.MaxPayloadSize, MaxPayloadSize)
	}

	// 2. Iterate and verify all valid test vectors
	for _, v := range gvf.ValidVectors {
		t.Run(v.Name, func(t *testing.T) {
			wireBytes, err := hex.DecodeString(v.WireHex)
			if err != nil {
				t.Fatalf("Invalid hex in WireHex %q: %v", v.WireHex, err)
			}

			expectedPayload, err := hex.DecodeString(v.PayloadHex)
			if err != nil {
				t.Fatalf("Invalid hex in PayloadHex %q: %v", v.PayloadHex, err)
			}

			// A. Decode wire bytes
			frame, err := DecodeFrameBytes(wireBytes)
			if err != nil {
				t.Fatalf("DecodeFrameBytes failed: %v", err)
			}

			if frame.StreamID != v.StreamID {
				t.Errorf("StreamID mismatch: got %d, expected %d", frame.StreamID, v.StreamID)
			}
			if frame.Type != v.Type {
				t.Errorf("Type mismatch: got %d, expected %d", frame.Type, v.Type)
			}
			if frame.Flags != v.Flags {
				t.Errorf("Flags mismatch: got %d, expected %d", frame.Flags, v.Flags)
			}
			if frame.Length != v.Length {
				t.Errorf("Length mismatch: got %d, expected %d", frame.Length, v.Length)
			}
			if string(frame.Payload) != string(expectedPayload) {
				t.Errorf("Payload mismatch: got %x, expected %x", frame.Payload, expectedPayload)
			}

			// B. Re-encode and verify byte-for-byte wire match
			encoded := frame.Encode()
			encodedHex := hex.EncodeToString(encoded)
			if !strings.EqualFold(encodedHex, v.WireHex) {
				t.Errorf("Encode() wire mismatch:\nGot:      %s\nExpected: %s", encodedHex, v.WireHex)
			}

			// C. Test EncodeHeader into destination buffer
			hdrBuf := make([]byte, HeaderSize)
			if err := frame.EncodeHeader(hdrBuf); err != nil {
				t.Fatalf("EncodeHeader failed: %v", err)
			}
			if hex.EncodeToString(hdrBuf) != v.WireHex[:HeaderSize*2] {
				t.Errorf("EncodeHeader mismatch:\nGot:      %s\nExpected: %s", hex.EncodeToString(hdrBuf), v.WireHex[:HeaderSize*2])
			}
		})
	}
}

func TestP2PMux_GoldenVectors_Errors(t *testing.T) {
	gvf := loadGoldenVectors(t)

	for _, v := range gvf.ErrorVectors {
		t.Run(v.Name, func(t *testing.T) {
			wireBytes, err := hex.DecodeString(v.WireHex)
			if err != nil {
				t.Fatalf("Invalid hex in WireHex %q: %v", v.WireHex, err)
			}

			_, err = DecodeFrameBytes(wireBytes)
			if err == nil {
				t.Fatalf("Expected error for vector %q, but decoding succeeded", v.Name)
			}

			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(v.ExpectedError)) {
				t.Errorf("Error message mismatch for %q:\nGot:      %v\nExpected substring: %q", v.Name, err, v.ExpectedError)
			}
		})
	}
}

func TestP2PMux_RecommendedChunk32KB_Boundary(t *testing.T) {
	// Verify that RecommendedChunkSize (32758) + HeaderSize (10) = 32768 (exact 32KB allocation class)
	if RecommendedChunkSize+HeaderSize != 32*1024 {
		t.Fatalf("RecommendedChunkSize + HeaderSize (%d + %d) != 32768", RecommendedChunkSize, HeaderSize)
	}

	payload := make([]byte, RecommendedChunkSize)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	frame := NewFrame(101, TypeData, FlagNone, payload)
	wire := frame.Encode()

	if len(wire) != 32*1024 {
		t.Fatalf("Expected wire packet size to equal exactly 32768 bytes, got %d", len(wire))
	}

	decoded, err := DecodeFrameBytes(wire)
	if err != nil {
		t.Fatalf("Failed to decode 32KB frame: %v", err)
	}

	if decoded.Length != uint32(RecommendedChunkSize) {
		t.Fatalf("Decoded length mismatch: got %d, expected %d", decoded.Length, RecommendedChunkSize)
	}
	if len(decoded.Payload) != RecommendedChunkSize {
		t.Fatalf("Decoded payload length mismatch: got %d", len(decoded.Payload))
	}
}
