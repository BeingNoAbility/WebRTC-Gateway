package agent

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"webrtc-gateway/pkg/protocol"
	"webrtc-gateway/pkg/signaling"
)

const defaultSynthesizedSessionID = "4611731400430051336"

// ExtractSessionDescriptor strictly extracts DataChannel-only RFC 8841 parameters from a
// locally generated Pion SDP. Supports multi-algorithm fingerprint lines (ignoring non-sha-256)
// while rejecting multi-section SDPs, conflicting sha-256 fingerprints, or invalid attributes.
func ExtractSessionDescriptor(rawSDP string, sdpVer uint64) (*signaling.SessionDescriptor, error) {
	if strings.TrimSpace(rawSDP) == "" {
		return nil, errors.New("empty SDP")
	}
	var sd sdp.SessionDescription
	if err := sd.UnmarshalString(rawSDP); err != nil {
		return nil, fmt.Errorf("parse SDP: %w", err)
	}
	if len(sd.MediaDescriptions) != 1 {
		return nil, fmt.Errorf("expected exactly 1 media description, got %d", len(sd.MediaDescriptions))
	}
	md := sd.MediaDescriptions[0]
	if md.MediaName.Media != "application" {
		return nil, fmt.Errorf("expected application media section, got %q", md.MediaName.Media)
	}

	var sessFP, mediaFP string
	var ufrag, pwd, setup, mid string
	sctpPort := 5000
	maxMsgSize := 262144

	scanAttrs := func(attrs []sdp.Attribute, isMedia bool) error {
		for _, a := range attrs {
			switch a.Key {
			case "fingerprint":
				fpHex, matched, err := parseSHA256FingerprintHex(a.Value)
				if err != nil {
					return err
				}
				if !matched {
					continue // Ignore non-sha-256 algorithms (e.g. sha-512) when sha-256 is also present.
				}
				if isMedia {
					if mediaFP != "" && mediaFP != fpHex {
						return errors.New("multiple conflicting media fingerprints in SDP")
					}
					mediaFP = fpHex
				} else {
					if sessFP != "" && sessFP != fpHex {
						return errors.New("multiple conflicting session fingerprints in SDP")
					}
					sessFP = fpHex
				}
			case "ice-ufrag":
				ufrag = strings.TrimSpace(a.Value)
			case "ice-pwd":
				pwd = strings.TrimSpace(a.Value)
			case "setup":
				setup = strings.ToLower(strings.TrimSpace(a.Value))
			case "mid":
				if isMedia {
					mid = strings.TrimSpace(a.Value)
				}
			case "sctp-port":
				v, err := strconv.Atoi(strings.TrimSpace(a.Value))
				if err != nil {
					return fmt.Errorf("invalid sctp-port %q", a.Value)
				}
				sctpPort = v
			case "max-message-size":
				v, err := strconv.Atoi(strings.TrimSpace(a.Value))
				if err != nil {
					return fmt.Errorf("invalid max-message-size %q", a.Value)
				}
				maxMsgSize = v
			}
		}
		return nil
	}

	if err := scanAttrs(sd.Attributes, false); err != nil {
		return nil, err
	}
	if err := scanAttrs(md.Attributes, true); err != nil {
		return nil, err
	}

	if sessFP != "" && mediaFP != "" && sessFP != mediaFP {
		return nil, errors.New("conflicting session-level and media-level DTLS fingerprints")
	}
	fp := mediaFP
	if fp == "" {
		fp = sessFP
	}
	if mid == "" {
		mid = "0"
	}
	if sdpVer < 2 {
		sdpVer = 2
	}
	// RFC 8841 §6.6: max-message-size:0 denotes unlimited -> clamp to our safe 256 KiB cap.
	// Positive values below MinNegotiatedMaxMsgSize (4096) fail closed.
	if maxMsgSize == 0 {
		maxMsgSize = 262144
	} else if maxMsgSize < protocol.MinNegotiatedMaxMsgSize {
		return nil, fmt.Errorf("max-message-size %d below minimum %d", maxMsgSize, protocol.MinNegotiatedMaxMsgSize)
	}
	if maxMsgSize > 1073741824 {
		maxMsgSize = 1073741824
	}

	desc := &signaling.SessionDescriptor{
		FP:         fp,
		IceUfrag:   ufrag,
		IcePwd:     pwd,
		Setup:      setup,
		Mid:        mid,
		SCTPPort:   sctpPort,
		MaxMsgSize: maxMsgSize,
		SDPVer:     sdpVer,
	}
	isOffer := setup == "actpass"
	if err := desc.Validate(isOffer); err != nil {
		return nil, err
	}
	return desc, nil
}

// parseSHA256FingerprintHex parses an RFC 8122 fingerprint line.
// Returns (hex, true, nil) for valid sha-256, ("", false, nil) for other algorithms (e.g. sha-512),
// or error for malformed lines or invalid sha-256 hex.
func parseSHA256FingerprintHex(raw string) (string, bool, error) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(raw)))
	if len(fields) != 2 {
		return "", false, fmt.Errorf("malformed DTLS fingerprint line %q", raw)
	}
	if fields[0] != "sha-256" {
		return "", false, nil
	}
	clean := strings.ReplaceAll(fields[1], ":", "")
	if len(clean) != 64 {
		return "", true, errors.New("invalid SHA-256 fingerprint length")
	}
	if _, err := hex.DecodeString(clean); err != nil {
		return "", true, errors.New("invalid SHA-256 fingerprint hex")
	}
	return clean, true, nil
}

// FormatColonFingerprint converts a 64-char hex SHA-256 fingerprint into RFC 8122 colon format.
func FormatColonFingerprint(fpHex string) string {
	upper := strings.ToUpper(fpHex)
	var b strings.Builder
	b.Grow(95)
	for i := 0; i+2 <= len(upper); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(upper[i : i+2])
	}
	return b.String()
}

// SynthesizeSDP deterministically constructs a minimal, RFC 8841 compliant DataChannel-only
// SDP from a validated SessionDescriptor. Raw peer SDP is NEVER fed to Pion or Chrome.
func SynthesizeSDP(desc *signaling.SessionDescriptor, isOffer bool) (string, error) {
	if err := desc.Validate(isOffer); err != nil {
		return "", err
	}
	lines := []string{
		"v=0",
		fmt.Sprintf("o=- %s %d IN IP4 127.0.0.1", defaultSynthesizedSessionID, desc.SDPVer),
		"s=-",
		"t=0 0",
		"a=group:BUNDLE " + desc.Mid,
		"a=msid-semantic: WMS",
		"m=application 9 UDP/DTLS/SCTP webrtc-datachannel",
		"c=IN IP4 0.0.0.0",
		"a=ice-ufrag:" + desc.IceUfrag,
		"a=ice-pwd:" + desc.IcePwd,
		"a=ice-options:trickle",
		"a=fingerprint:sha-256 " + FormatColonFingerprint(desc.FP),
		"a=setup:" + desc.Setup,
		"a=mid:" + desc.Mid,
		fmt.Sprintf("a=sctp-port:%d", desc.SCTPPort),
		fmt.Sprintf("a=max-message-size:%d", desc.MaxMsgSize),
	}
	return strings.Join(lines, "\r\n") + "\r\n", nil
}

// EncodeAnswerTranscript returns the canonical V7 length-prefixed Pure Ed25519 transcript
// binding nodeID, visitorNonce, and every validated field of both Offer and Answer descriptors.
func EncodeAnswerTranscript(nodeID, visitorNonce string, offer, answer *signaling.SessionDescriptor) ([]byte, error) {
	if nodeID == "" || visitorNonce == "" {
		return nil, errors.New("empty nodeID or visitorNonce")
	}
	if err := offer.Validate(true); err != nil {
		return nil, fmt.Errorf("invalid offer descriptor: %w", err)
	}
	if err := answer.Validate(false); err != nil {
		return nil, fmt.Errorf("invalid answer descriptor: %w", err)
	}
	return signaling.AppendLenPrefixed(
		[]byte("p2pgw-v7:answer-v2:"),
		nodeID,
		visitorNonce,
		offer.FP,
		offer.IceUfrag,
		offer.IcePwd,
		offer.Setup,
		offer.Mid,
		strconv.Itoa(offer.SCTPPort),
		strconv.Itoa(offer.MaxMsgSize),
		strconv.FormatUint(offer.SDPVer, 10),
		answer.FP,
		answer.IceUfrag,
		answer.IcePwd,
		answer.Setup,
		answer.Mid,
		strconv.Itoa(answer.SCTPPort),
		strconv.Itoa(answer.MaxMsgSize),
		strconv.FormatUint(answer.SDPVer, 10),
	)
}

func safePrefix16(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:16]
}

// VerifyRemoteCertFingerprint verifies that the actual negotiated DTLS remote certificate DER
// hashes to the exact expected 64-char hex SHA-256 fingerprint from the signed descriptor.
func VerifyRemoteCertFingerprint(certDER []byte, expectedFPHex string) error {
	if len(certDER) == 0 {
		return errors.New("missing negotiated DTLS remote certificate")
	}
	normExpected := strings.ToLower(strings.TrimSpace(expectedFPHex))
	if len(normExpected) != 64 {
		return fmt.Errorf("invalid expected DTLS fingerprint length (%d)", len(normExpected))
	}
	sum := sha256.Sum256(certDER)
	gotHex := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(gotHex), []byte(normExpected)) != 1 {
		return fmt.Errorf("DTLS remote certificate fingerprint mismatch: got %s, want %s", safePrefix16(gotHex), safePrefix16(normExpected))
	}
	return nil
}
