package agent

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentV7_IdentityPersistenceAndCapabilityRevocationPersistence(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "nas.ed25519.key")
	id1, err := LoadOrCreateIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadOrCreateIdentity(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if id1.PKHash() != id2.PKHash() || len(id1.PKHash()) != PKHashChars {
		t.Fatalf("persisted identity mismatch: %q vs %q", id1.PKHash(), id2.PKHash())
	}

	p := CapabilityPayload{
		V: CapabilityVersion, JTI: "cap-1", Node: "nas", Exp: time.Now().Add(time.Hour).Unix(),
		PathPrefix: "/docs", Readonly: true, MaxConns: 2,
	}
	tok, err := id1.MintCapability(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := id2.VerifyCapability(tok, "nas", time.Now())
	if err != nil || got.JTI != "cap-1" || !got.Readonly || got.PathPrefix != "/docs" {
		t.Fatalf("capability verification failed: %+v err=%v", got, err)
	}
	if _, err := id2.VerifyCapability(tok, "other-node", time.Now()); err == nil {
		t.Fatal("capability accepted for wrong node")
	}
	if _, err := id2.VerifyCapability(tok, "nas", time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired capability accepted")
	}

	// Revoked capability must remain revoked across Agent restart.
	stateFile := filepath.Join(dir, "nas.state.json")
	cs1, err := NewCapabilityStoreWithFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs1.Track(p); err != nil {
		t.Fatal(err)
	}
	if err := cs1.Revoke("cap-1"); err != nil {
		t.Fatal(err)
	}

	cs2, err := NewCapabilityStoreWithFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if cs2.AcquireConn(&p) {
		t.Fatal("revoked capability token resurrected after restart")
	}
}

func TestCapabilityListPagination(t *testing.T) {
	cs := NewCapabilityStore()
	now := time.Now().Unix()
	for i := 0; i < 65; i++ {
		if err := cs.Track(CapabilityPayload{
			V: CapabilityVersion, JTI: fmt.Sprintf("cap-%03d", i), Node: "nas",
			Exp: now + 3600 + int64(i), PathPrefix: "/",
		}); err != nil {
			t.Fatal(err)
		}
	}
	nowTime := time.Now()
	page1, total, hasMore1 := cs.ListPage(nowTime, 0, 0)
	if total != 65 || len(page1) != DefaultCapListLimit || !hasMore1 {
		t.Fatalf("default page mismatch: len=%d total=%d hasMore=%v", len(page1), total, hasMore1)
	}
	page2, total2, hasMore2 := cs.ListPage(nowTime, 50, 20)
	if total2 != 65 || len(page2) != 15 || hasMore2 {
		t.Fatalf("page2 mismatch: len=%d total=%d hasMore=%v", len(page2), total2, hasMore2)
	}
	pageEmpty, _, hasMore3 := cs.ListPage(nowTime, 999, 10)
	if len(pageEmpty) != 0 || hasMore3 {
		t.Fatalf("out-of-range page must be empty, got %d hasMore=%v", len(pageEmpty), hasMore3)
	}
}

func TestAuthLimiterFailClosedWhenSaturated(t *testing.T) {
	lim := NewVisitorAuthLimiter()
	now := time.Now()

	// Fill every slot with an active unexpired lockout.
	for i := 0; i < visitorAuthMaxKeys; i++ {
		ip := fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff)
		for f := 0; f < visitorAuthMaxFailures; f++ {
			lim.RecordFailure(ip, now)
		}
	}

	// Unseen IP must fail closed with positive retryAfter while saturated.
	allowed, retryAfter := lim.Allow("198.51.100.200", now)
	if allowed {
		t.Fatal("VisitorAuthLimiter failed open when saturated with active lockouts")
	}
	if retryAfter <= 0 {
		t.Fatalf("expected positive retryAfter when saturated, got %v", retryAfter)
	}
	if !lim.RecordFailure("198.51.100.201", now) {
		t.Fatal("RecordFailure must return true when saturated")
	}

	// Once the lockout window expires, new IPs must recover cleanly.
	afterWindow := now.Add(visitorAuthWindow + time.Second)
	allowed, _ = lim.Allow("198.51.100.202", afterWindow)
	if !allowed {
		t.Fatal("VisitorAuthLimiter stayed stuck locked after lockout window expired")
	}
}

func TestAgentV7_SPAKE2P_MutualAuthAndNegativeAttacks(t *testing.T) {
	const testAgentPK = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	verifierRec, err := DeriveSPAKE2PVerifier("hunter2-strong-pass", "nas", testAgentPK)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := NewSPAKE2PVerifierSession(verifierRec)
	if err != nil {
		t.Fatal(err)
	}

	ctxBytes, err := BuildSPAKE2PContext("nas", strings.Repeat("aa", 32), strings.Repeat("bb", 32), testAgentPK, "vnonce", "anonce")
	if err != nil {
		t.Fatal(err)
	}
	proverCreds, err := DeriveSPAKE2PProverCreds("hunter2-strong-pass", "nas", testAgentPK)
	if err != nil {
		t.Fatal(err)
	}
	shareP, confirmP, wantConfirmV, proverKey, err := SPAKE2PProverCompute(proverCreds, ctxBytes, vs.ShareV())
	if err != nil {
		t.Fatalf("SPAKE2PProverCompute: %v", err)
	}
	gotConfirmV, verifierKey, err := vs.Finish(ctxBytes, shareP, confirmP)
	if err != nil {
		t.Fatalf("SPAKE2P Verifier Finish failed: %v", err)
	}
	if !bytes.Equal(gotConfirmV, wantConfirmV) || !bytes.Equal(proverKey, verifierKey) {
		t.Fatal("SPAKE2+ mutual confirmation or shared key mismatch")
	}

	// Negative 1: Wrong password fails.
	vs2, _ := NewSPAKE2PVerifierSession(verifierRec)
	wrongCreds, err := DeriveSPAKE2PProverCreds("wrong-password", "nas", testAgentPK)
	if err != nil {
		t.Fatal(err)
	}
	sp2, cp2, _, _, err := SPAKE2PProverCompute(wrongCreds, ctxBytes, vs2.ShareV())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := vs2.Finish(ctxBytes, sp2, cp2); err == nil {
		t.Fatal("SPAKE2+ accepted wrong password")
	}

	// Negative 2: Mismatched DTLS context fails (anti-MITM channel binding).
	vs3, _ := NewSPAKE2PVerifierSession(verifierRec)
	sp3, cp3, _, _, _ := SPAKE2PProverCompute(proverCreds, ctxBytes, vs3.ShareV())
	otherCtx, err := BuildSPAKE2PContext("nas", strings.Repeat("cc", 32), strings.Repeat("bb", 32), testAgentPK, "vnonce", "anonce")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := vs3.Finish(otherCtx, sp3, cp3); err == nil {
		t.Fatal("SPAKE2+ accepted mismatched DTLS fingerprint context")
	}

	// Negative 3: Off-curve / bogus shareP rejected.
	if _, _, err := vs3.Finish(ctxBytes, make([]byte, 65), cp3); err == nil {
		t.Fatal("SPAKE2+ accepted off-curve shareP")
	}
}

func TestRFC9383AppendixCVectorAndV7KDF(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "protocol_v7_golden_vectors.json"))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var doc struct {
		RFC9383 struct {
			Context      string `json:"context"`
			IDProver     string `json:"id_prover"`
			IDVerifier   string `json:"id_verifier"`
			W0Hex        string `json:"w0_hex"`
			W1Hex        string `json:"w1_hex"`
			LHex         string `json:"L_hex"`
			XHex         string `json:"x_hex"`
			SharePHex    string `json:"share_p_hex"`
			YHex         string `json:"y_hex"`
			ShareVHex    string `json:"share_v_hex"`
			ConfirmPHex  string `json:"confirm_p_hex"`
			ConfirmVHex  string `json:"confirm_v_hex"`
			SharedKeyHex string `json:"shared_key_hex"`
		} `json:"rfc9383_appendix_c"`
		V7KDF struct {
			Password   string `json:"password"`
			NodeID     string `json:"node_id"`
			AgentPKB64 string `json:"agent_pk_b64"`
			W0Hex      string `json:"w0_hex"`
			W1Hex      string `json:"w1_hex"`
		} `json:"v7_spake2p_kdf"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	mustHex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	var w0, w1, x, y [32]byte
	copy(w0[:], mustHex(doc.RFC9383.W0Hex))
	copy(w1[:], mustHex(doc.RFC9383.W1Hex))
	copy(x[:], mustHex(doc.RFC9383.XHex))
	copy(y[:], mustHex(doc.RFC9383.YHex))
	L := mustHex(doc.RFC9383.LHex)

	vs, err := NewSPAKE2PVerifierSessionWithScalar(
		&SPAKE2PVerifierRecord{W0: w0, L: L},
		y,
		[]byte(doc.RFC9383.IDProver),
		[]byte(doc.RFC9383.IDVerifier),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(vs.ShareV(), mustHex(doc.RFC9383.ShareVHex)) {
		t.Fatalf("RFC 9383 shareV mismatch:\ngot:  %x\nwant: %s", vs.ShareV(), doc.RFC9383.ShareVHex)
	}

	shareP, confirmP, wantConfirmV, proverKey, err := SPAKE2PProverComputeWithScalar(
		SPAKE2PProverCreds{W0: w0, W1: w1},
		x,
		[]byte(doc.RFC9383.Context),
		[]byte(doc.RFC9383.IDProver),
		[]byte(doc.RFC9383.IDVerifier),
		vs.ShareV(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(shareP, mustHex(doc.RFC9383.SharePHex)) {
		t.Fatalf("RFC 9383 shareP mismatch: %x", shareP)
	}
	if !bytes.Equal(confirmP, mustHex(doc.RFC9383.ConfirmPHex)) {
		t.Fatalf("RFC 9383 confirmP mismatch:\ngot:  %x\nwant: %s", confirmP, doc.RFC9383.ConfirmPHex)
	}
	if !bytes.Equal(wantConfirmV, mustHex(doc.RFC9383.ConfirmVHex)) {
		t.Fatalf("RFC 9383 confirmV mismatch:\ngot:  %x\nwant: %s", wantConfirmV, doc.RFC9383.ConfirmVHex)
	}
	if !bytes.Equal(proverKey, mustHex(doc.RFC9383.SharedKeyHex)) {
		t.Fatalf("RFC 9383 sharedKey mismatch:\ngot:  %x\nwant: %s", proverKey, doc.RFC9383.SharedKeyHex)
	}

	gotConfirmV, verifierKey, err := vs.Finish([]byte(doc.RFC9383.Context), shareP, confirmP)
	if err != nil {
		t.Fatalf("RFC 9383 verifier Finish failed: %v", err)
	}
	if !bytes.Equal(gotConfirmV, mustHex(doc.RFC9383.ConfirmVHex)) || !bytes.Equal(verifierKey, mustHex(doc.RFC9383.SharedKeyHex)) {
		t.Fatal("RFC 9383 verifier Finish key mismatch")
	}

	// Verify V7 SPAKE2+ KDF vector parity (shared with TypeScript).
	creds, err := DeriveSPAKE2PProverCreds(doc.V7KDF.Password, doc.V7KDF.NodeID, doc.V7KDF.AgentPKB64)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(creds.W0[:]) != doc.V7KDF.W0Hex || hex.EncodeToString(creds.W1[:]) != doc.V7KDF.W1Hex {
		t.Fatalf("V7 KDF mismatch: w0=%x w1=%x", creds.W0, creds.W1)
	}
}
