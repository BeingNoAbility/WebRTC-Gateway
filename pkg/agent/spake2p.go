package agent

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"

	"filippo.io/nistec"
	"webrtc-gateway/pkg/signaling"
)

const (
	// V7 single-block (32-byte) PBKDF2-HMAC-SHA256 work factor (300,000 iterations),
	// followed by HKDF-SHA256 expansion to 80 bytes for (w0, w1).
	SPAKE2PPBKDF2Iters = 300000
	spake2pPointLen    = 65 // Uncompressed SEC1 P-256 point (0x04 || X || Y)
	spake2pScalarLen   = 32

	// RFC 9383 Section 4 P-256 compressed generator points M and N.
	spake2pMCompressedHex = "02886e2f97ace46e55ba9dd7242579f2993b64e16ef3dcab95afd497333d8fa12f"
	spake2pNCompressedHex = "03d8bbd6c639c62937b04d997f38c3770719c629d7014d49a24b4f98baa1292b49"

	spake2pProverID   = "webrtc-gateway-visitor"
	spake2pVerifierID = "webrtc-gateway-agent"
)

var (
	p256Order, _ = new(big.Int).SetString("ffffffff00000000ffffffffffffffffbce6faada7179e84f3b9cac2fc632551", 16)
	spake2pM     = mustDecodeP256PointHex(spake2pMCompressedHex)
	spake2pN     = mustDecodeP256PointHex(spake2pNCompressedHex)
	spake2pMB    = spake2pM.Bytes()
	spake2pNB    = spake2pN.Bytes()
)

func mustDecodeP256PointHex(h string) *nistec.P256Point {
	raw, err := hex.DecodeString(h)
	if err != nil {
		panic(err)
	}
	p, err := nistec.NewP256Point().SetBytes(raw)
	if err != nil {
		panic(err)
	}
	return p
}

// SPAKE2PVerifierRecord holds the stretched offline verifier derived once at Agent startup.
type SPAKE2PVerifierRecord struct {
	W0 [spake2pScalarLen]byte
	L  []byte // 65-byte uncompressed SEC1 point w1 * G
}

// SPAKE2PProverCreds holds stretched (w0, w1) derived by the visitor from ACCESS_SECRET.
type SPAKE2PProverCreds struct {
	W0 [spake2pScalarLen]byte
	W1 [spake2pScalarLen]byte
}

func deriveSPAKE2PSalt(nodeID, agentPKB64 string) ([]byte, error) {
	if nodeID == "" || agentPKB64 == "" {
		return nil, errors.New("spake2p: nodeID and agentPKB64 are required for salt derivation")
	}
	in, err := signaling.AppendLenPrefixed([]byte("p2pgw-v7:spake2p-salt:"), nodeID, agentPKB64)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(in)
	return sum[:], nil
}

func reduceModN(be []byte) [spake2pScalarLen]byte {
	n := new(big.Int).SetBytes(be)
	n.Mod(n, p256Order)
	var out [spake2pScalarLen]byte
	n.FillBytes(out[:])
	return out
}

// DeriveSPAKE2PProverCreds stretches password with 1-block PBKDF2-HMAC-SHA256 (300k iters, 32B)
// salted by SHA256(LenPrefixed("p2pgw-v7:spake2p-salt:", nodeID, agentPKB64)),
// then expands via HKDF-SHA256("p2pgw-v7:spake2p-w0w1") into (w0, w1) mod n.
func DeriveSPAKE2PProverCreds(password, nodeID, agentPKB64 string) (SPAKE2PProverCreds, error) {
	salt, err := deriveSPAKE2PSalt(nodeID, agentPKB64)
	if err != nil {
		return SPAKE2PProverCreds{}, err
	}
	seed, err := pbkdf2.Key(sha256.New, password, salt, SPAKE2PPBKDF2Iters, 32)
	if err != nil {
		return SPAKE2PProverCreds{}, err
	}
	okm, err := hkdf.Key(sha256.New, seed, nil, "p2pgw-v7:spake2p-w0w1", 80)
	if err != nil {
		return SPAKE2PProverCreds{}, err
	}
	return SPAKE2PProverCreds{
		W0: reduceModN(okm[:40]),
		W1: reduceModN(okm[40:80]),
	}, nil
}

// DeriveSPAKE2PVerifier computes the Agent's startup verifier record (w0, L = w1*G).
func DeriveSPAKE2PVerifier(password, nodeID, agentPKB64 string) (*SPAKE2PVerifierRecord, error) {
	creds, err := DeriveSPAKE2PProverCreds(password, nodeID, agentPKB64)
	if err != nil {
		return nil, err
	}
	lPt, err := nistec.NewP256Point().ScalarBaseMult(creds.W1[:])
	if err != nil {
		return nil, err
	}
	return &SPAKE2PVerifierRecord{
		W0: creds.W0,
		L:  lPt.Bytes(),
	}, nil
}

// BuildSPAKE2PContext constructs the V7 length-prefixed channel-bound context for RFC 9383 TT.
func BuildSPAKE2PContext(nodeID, visitorFP, agentFP, agentPKB64, visitorNonce, agentNonce string) ([]byte, error) {
	return signaling.AppendLenPrefixed(
		[]byte("p2pgw-v7:spake2p-ctx:"),
		nodeID,
		visitorFP,
		agentFP,
		agentPKB64,
		visitorNonce,
		agentNonce,
	)
}

func randomP256Scalar() ([spake2pScalarLen]byte, error) {
	var buf [40]byte
	for {
		if _, err := rand.Read(buf[:]); err != nil {
			return [spake2pScalarLen]byte{}, err
		}
		s := reduceModN(buf[:])
		if new(big.Int).SetBytes(s[:]).Sign() != 0 {
			return s, nil
		}
	}
}

func decodeUncompressedP256(b []byte) (*nistec.P256Point, error) {
	if len(b) != spake2pPointLen || b[0] != 0x04 {
		return nil, errors.New("spake2p: expected 65-byte uncompressed SEC1 P-256 point")
	}
	p, err := nistec.NewP256Point().SetBytes(b)
	if err != nil {
		return nil, errors.New("spake2p: invalid P-256 point")
	}
	return p, nil
}

func appendLE64Prefixed(dst, data []byte) []byte {
	var lb [8]byte
	binary.LittleEndian.PutUint64(lb[:], uint64(len(data)))
	dst = append(dst, lb[:]...)
	return append(dst, data...)
}

// SPAKE2PComputeKeysRFC9383 computes the RFC 9383 Section 3.4 transcript and confirmation/shared keys
// for explicit idProver/idVerifier (used by production code and RFC 9383 Appendix C conformance vectors).
func SPAKE2PComputeKeysRFC9383(context, idProver, idVerifier, shareP, shareV, zBytes, vBytes, w0 []byte) (confirmP, confirmV, sharedKey []byte, err error) {
	if len(zBytes) != spake2pPointLen || len(vBytes) != spake2pPointLen {
		return nil, nil, nil, errors.New("spake2p: degenerate shared point")
	}
	var tt []byte
	tt = appendLE64Prefixed(tt, context)
	tt = appendLE64Prefixed(tt, idProver)
	tt = appendLE64Prefixed(tt, idVerifier)
	tt = appendLE64Prefixed(tt, spake2pMB)
	tt = appendLE64Prefixed(tt, spake2pNB)
	tt = appendLE64Prefixed(tt, shareP)
	tt = appendLE64Prefixed(tt, shareV)
	tt = appendLE64Prefixed(tt, zBytes)
	tt = appendLE64Prefixed(tt, vBytes)
	tt = appendLE64Prefixed(tt, w0)

	kMain := sha256.Sum256(tt)

	// RFC 9383 Section 3.4 key schedule (HKDF-SHA256, empty salt).
	confKeys, err := hkdf.Key(sha256.New, kMain[:], nil, "ConfirmationKeys", 64)
	if err != nil {
		return nil, nil, nil, err
	}
	kcP := confKeys[:32]
	kcV := confKeys[32:64]

	sharedKey, err = hkdf.Key(sha256.New, kMain[:], nil, "SharedKey", 32)
	if err != nil {
		return nil, nil, nil, err
	}

	macP := hmac.New(sha256.New, kcP)
	macP.Write(shareV)
	confirmP = macP.Sum(nil)

	macV := hmac.New(sha256.New, kcV)
	macV.Write(shareP)
	confirmV = macV.Sum(nil)
	return confirmP, confirmV, sharedKey, nil
}

// SPAKE2PVerifierSession represents one Agent-side SPAKE2+ handshake state.
type SPAKE2PVerifierSession struct {
	rec        *SPAKE2PVerifierRecord
	y          [spake2pScalarLen]byte
	shareV     []byte
	idProver   []byte
	idVerifier []byte
}

// NewSPAKE2PVerifierSession generates ephemeral y and shareV = y*G + w0*N.
func NewSPAKE2PVerifierSession(rec *SPAKE2PVerifierRecord) (*SPAKE2PVerifierSession, error) {
	y, err := randomP256Scalar()
	if err != nil {
		return nil, err
	}
	return NewSPAKE2PVerifierSessionWithScalar(rec, y, []byte(spake2pProverID), []byte(spake2pVerifierID))
}

// NewSPAKE2PVerifierSessionWithScalar builds a deterministic verifier session (for RFC 9383 Appendix C vectors).
func NewSPAKE2PVerifierSessionWithScalar(rec *SPAKE2PVerifierRecord, y [spake2pScalarLen]byte, idProver, idVerifier []byte) (*SPAKE2PVerifierSession, error) {
	yG, err := nistec.NewP256Point().ScalarBaseMult(y[:])
	if err != nil {
		return nil, err
	}
	w0N, err := nistec.NewP256Point().ScalarMult(spake2pN, rec.W0[:])
	if err != nil {
		return nil, err
	}
	shareV := nistec.NewP256Point().Add(yG, w0N).Bytes()
	if len(shareV) != spake2pPointLen {
		return nil, errors.New("spake2p: degenerate shareV")
	}
	return &SPAKE2PVerifierSession{
		rec:        rec,
		y:          y,
		shareV:     shareV,
		idProver:   idProver,
		idVerifier: idVerifier,
	}, nil
}

func (s *SPAKE2PVerifierSession) ShareV() []byte {
	return append([]byte(nil), s.shareV...)
}

// Finish verifies the Prover's (shareP, confirmP) and returns (confirmV, sharedKey) on success.
func (s *SPAKE2PVerifierSession) Finish(context, shareP, gotConfirmP []byte) (confirmV, sharedKey []byte, err error) {
	pPt, err := decodeUncompressedP256(shareP)
	if err != nil {
		return nil, nil, err
	}
	lPt, err := decodeUncompressedP256(s.rec.L)
	if err != nil {
		return nil, nil, err
	}
	w0M, err := nistec.NewP256Point().ScalarMult(spake2pM, s.rec.W0[:])
	if err != nil {
		return nil, nil, err
	}
	negW0M := nistec.NewP256Point().Negate(w0M)
	diff := nistec.NewP256Point().Add(pPt, negW0M)
	zPt, err := nistec.NewP256Point().ScalarMult(diff, s.y[:])
	if err != nil {
		return nil, nil, err
	}
	vPt, err := nistec.NewP256Point().ScalarMult(lPt, s.y[:])
	if err != nil {
		return nil, nil, err
	}

	wantConfirmP, confirmV, sharedKey, err := SPAKE2PComputeKeysRFC9383(
		context, s.idProver, s.idVerifier, shareP, s.shareV, zPt.Bytes(), vPt.Bytes(), s.rec.W0[:],
	)
	if err != nil {
		return nil, nil, err
	}
	if subtle.ConstantTimeCompare(wantConfirmP, gotConfirmP) != 1 {
		return nil, nil, errors.New("spake2p: prover confirmation mismatch")
	}
	return confirmV, sharedKey, nil
}

// SPAKE2PProverCompute executes the Prover side in one shot given Verifier's shareV.
func SPAKE2PProverCompute(creds SPAKE2PProverCreds, context, shareV []byte) (shareP, confirmP, expectedConfirmV, sharedKey []byte, err error) {
	x, err := randomP256Scalar()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return SPAKE2PProverComputeWithScalar(creds, x, context, []byte(spake2pProverID), []byte(spake2pVerifierID), shareV)
}

// SPAKE2PProverComputeWithScalar executes the Prover side deterministically with scalar x (for RFC 9383 Appendix C vectors).
func SPAKE2PProverComputeWithScalar(creds SPAKE2PProverCreds, x [spake2pScalarLen]byte, context, idProver, idVerifier, shareV []byte) (shareP, confirmP, expectedConfirmV, sharedKey []byte, err error) {
	vPt, err := decodeUncompressedP256(shareV)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	xG, err := nistec.NewP256Point().ScalarBaseMult(x[:])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	w0M, err := nistec.NewP256Point().ScalarMult(spake2pM, creds.W0[:])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	shareP = nistec.NewP256Point().Add(xG, w0M).Bytes()
	if len(shareP) != spake2pPointLen {
		return nil, nil, nil, nil, errors.New("spake2p: degenerate shareP")
	}

	w0N, err := nistec.NewP256Point().ScalarMult(spake2pN, creds.W0[:])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	negW0N := nistec.NewP256Point().Negate(w0N)
	diff := nistec.NewP256Point().Add(vPt, negW0N)
	zPt, err := nistec.NewP256Point().ScalarMult(diff, x[:])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	vOut, err := nistec.NewP256Point().ScalarMult(diff, creds.W1[:])
	if err != nil {
		return nil, nil, nil, nil, err
	}

	confirmP, expectedConfirmV, sharedKey, err = SPAKE2PComputeKeysRFC9383(
		context, idProver, idVerifier, shareP, shareV, zPt.Bytes(), vOut.Bytes(), creds.W0[:],
	)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return shareP, confirmP, expectedConfirmV, sharedKey, nil
}
