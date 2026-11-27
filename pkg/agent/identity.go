package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"webrtc-gateway/pkg/signaling"
)

const (
	// PKHashChars is the 22-char base64url prefix of SHA-256(pk) (132 bits).
	PKHashChars = 22
	// CapabilityVersion is the current V7 Ed25519 capability token version.
	CapabilityVersion = 7
)

// Identity holds an Agent's persistent Ed25519 keypair used for signaling node ownership,
// anti-MITM structured SDP transcript signatures, and capability tokens.
type Identity struct {
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	KeyPath    string // non-empty for persistent identities (used to derive sibling state file)
}

// GenerateEphemeralIdentity creates a fresh in-memory Ed25519 keypair.
func GenerateEphemeralIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{PublicKey: pub, PrivateKey: priv}, nil
}

// LoadOrCreateIdentity loads an Ed25519 PKCS#8 PEM private key from keyPath, or creates
// and saves a new one with strict 0600 permissions if the file does not exist.
func LoadOrCreateIdentity(keyPath string) (*Identity, error) {
	raw, err := os.ReadFile(filepath.Clean(keyPath)) // #nosec G304 -- operator-configured persistent Ed25519 identity key path
	if err == nil {
		block, _ := pem.Decode(raw)
		if block == nil {
			return nil, fmt.Errorf("invalid PEM in identity key %q", keyPath)
		}
		parsed, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			return nil, fmt.Errorf("parse PKCS8 key %q: %w", keyPath, perr)
		}
		priv, ok := parsed.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("identity key %q is not Ed25519", keyPath)
		}
		pub := priv.Public().(ed25519.PublicKey)
		return &Identity{PublicKey: pub, PrivateKey: priv, KeyPath: keyPath}, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	id, err := GenerateEphemeralIdentity()
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(id.PrivateKey)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pemBytes, 0600); err != nil {
		return nil, err
	}
	id.KeyPath = keyPath
	return id, nil
}

// PublicKeyBase64 returns the raw 32-byte Ed25519 public key encoded as base64url (no padding).
func (id *Identity) PublicKeyBase64() string {
	return base64.RawURLEncoding.EncodeToString(id.PublicKey)
}

// PKHash returns the 22-char base64url SHA-256 fingerprint pinned in URLs (#node?pk=<hash>).
func (id *Identity) PKHash() string {
	return PKHashFromPublicKey(id.PublicKey)
}

func PKHashFromPublicKey(pk ed25519.PublicKey) string {
	sum := sha256.Sum256(pk)
	return base64.RawURLEncoding.EncodeToString(sum[:])[:PKHashChars]
}

// SignAnswerTranscript signs the length-prefixed structured Offer/Answer transcript with Pure Ed25519.
func (id *Identity) SignAnswerTranscript(nodeID, visitorNonce string, offer, answer *signaling.SessionDescriptor) (string, error) {
	transcript, err := EncodeAnswerTranscript(nodeID, visitorNonce, offer, answer)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(id.PrivateKey, transcript)
	return base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyAnswerTranscript verifies an Agent's Pure Ed25519 structured Offer/Answer transcript signature.
func VerifyAnswerTranscript(pkB64, nodeID, visitorNonce string, offer, answer *signaling.SessionDescriptor, sigB64 string) (ed25519.PublicKey, error) {
	pkBytes, err := base64.RawURLEncoding.DecodeString(pkB64)
	if err != nil || len(pkBytes) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return nil, errors.New("invalid Ed25519 signature")
	}
	transcript, err := EncodeAnswerTranscript(nodeID, visitorNonce, offer, answer)
	if err != nil {
		return nil, err
	}
	pk := ed25519.PublicKey(pkBytes)
	if !ed25519.Verify(pk, transcript, sigBytes) {
		return nil, errors.New("answer transcript signature invalid")
	}
	return pk, nil
}

// CapabilityPayload defines a revocable, path-scoped, optionally read-only share token.
type CapabilityPayload struct {
	V          int    `json:"v"`
	JTI        string `json:"jti"`
	Node       string `json:"node"`
	Exp        int64  `json:"exp"`
	PathPrefix string `json:"path_prefix,omitempty"`
	Readonly   bool   `json:"readonly,omitempty"`
	MaxConns   int    `json:"max_conns,omitempty"`
}

// MintCapability signs a CapabilityPayload with Agent's Pure Ed25519 private key.
func (id *Identity) MintCapability(p CapabilityPayload) (string, error) {
	if p.V == 0 {
		p.V = CapabilityVersion
	}
	if p.JTI == "" || p.Node == "" || p.Exp <= 0 {
		return "", errors.New("capability requires jti, node, and exp")
	}
	rawJSON, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	msg, err := signaling.AppendLenPrefixed([]byte("p2pgw-v7:cap-v2:"), string(rawJSON))
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(id.PrivateKey, msg)
	return base64.RawURLEncoding.EncodeToString(rawJSON) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyCapability verifies an Ed25519 capability token against this Agent's public key.
func (id *Identity) VerifyCapability(token, expectedNode string, now time.Time) (*CapabilityPayload, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("malformed capability token")
	}
	rawJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(rawJSON) == 0 || len(rawJSON) > 4096 {
		return nil, errors.New("invalid capability payload encoding")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("invalid capability signature encoding")
	}
	msg, err := signaling.AppendLenPrefixed([]byte("p2pgw-v7:cap-v2:"), string(rawJSON))
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(id.PublicKey, msg, sig) {
		return nil, errors.New("capability signature verification failed")
	}
	var p CapabilityPayload
	if err := json.Unmarshal(rawJSON, &p); err != nil {
		return nil, errors.New("invalid capability JSON")
	}
	if p.V != CapabilityVersion || p.JTI == "" || p.Node != expectedNode {
		return nil, errors.New("capability version or node mismatch")
	}
	if now.Unix() >= p.Exp {
		return nil, errors.New("capability token expired")
	}
	return &p, nil
}
