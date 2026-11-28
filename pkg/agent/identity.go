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
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"webrtc-gateway/pkg/signaling"
	"webrtc-gateway/pkg/state"
)

func decodeStrictBase64URL(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("invalid base64url characters")
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

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

func loadIdentityFromFile(cleanPath, origPath string) (*Identity, error) {
	if runtime.GOOS != "windows" {
		if fi, serr := os.Stat(cleanPath); serr == nil && fi.Mode().Perm()&0077 != 0 {
			log.Printf("[Agent] WARNING: identity key %q has overly broad permissions (%04o); recommend chmod 0600", origPath, fi.Mode().Perm())
		}
	}
	raw, err := os.ReadFile(cleanPath) // #nosec G304 -- operator-configured persistent Ed25519 identity key path
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("invalid PEM in identity key %q", origPath)
	}
	parsed, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
	if perr != nil {
		return nil, fmt.Errorf("parse PKCS8 key %q: %w", origPath, perr)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity key %q is not Ed25519", origPath)
	}
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{PublicKey: pub, PrivateKey: priv, KeyPath: origPath}, nil
}

// LoadOrCreateIdentity loads an Ed25519 PKCS#8 PEM private key from keyPath, or atomically
// creates and fsyncs a new one under a sibling file lock if the file does not exist.
func LoadOrCreateIdentity(keyPath string) (*Identity, error) {
	cleanPath := filepath.Clean(keyPath)
	if id, err := loadIdentityFromFile(cleanPath, keyPath); err == nil {
		return id, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	var result *Identity
	err := state.WithFileLock(cleanPath+".lock", func() error {
		// Re-check under file lock in case a concurrent process created the identity first.
		if id, lerr := loadIdentityFromFile(cleanPath, keyPath); lerr == nil {
			result = id
			return nil
		} else if !os.IsNotExist(lerr) {
			return lerr
		}

		id, gerr := GenerateEphemeralIdentity()
		if gerr != nil {
			return gerr
		}
		der, merr := x509.MarshalPKCS8PrivateKey(id.PrivateKey)
		if merr != nil {
			return merr
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if werr := state.WriteAtomic(cleanPath, pemBytes, 0600); werr != nil {
			return werr
		}
		id.KeyPath = keyPath
		result = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
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
	pkBytes, err := decodeStrictBase64URL(pkB64)
	if err != nil || len(pkBytes) != ed25519.PublicKeySize {
		return nil, errors.New("invalid Ed25519 public key")
	}
	sigBytes, err := decodeStrictBase64URL(sigB64)
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
	if len(p.JTI) > maxCapabilityJTILen {
		return "", errors.New("capability jti exceeds maximum length")
	}
	if p.MaxConns < 0 {
		return "", errors.New("capability max_conns cannot be negative")
	}
	if len(p.PathPrefix) > 512 {
		return "", errors.New("capability path_prefix exceeds 512 bytes")
	}
	if strings.ContainsAny(p.PathPrefix, "\x00\r\n\\") || strings.ContainsAny(p.JTI, "\x00\r\n\\") {
		return "", errors.New("capability contains forbidden control or backslash characters")
	}
	rawJSON, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	if len(rawJSON) > 4096 {
		return "", errors.New("capability payload exceeds 4096 byte limit")
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
	rawJSON, err := decodeStrictBase64URL(parts[0])
	if err != nil || len(rawJSON) == 0 || len(rawJSON) > 4096 {
		return nil, errors.New("invalid capability payload encoding")
	}
	sig, err := decodeStrictBase64URL(parts[1])
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
