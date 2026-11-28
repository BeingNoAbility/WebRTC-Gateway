package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type goldenCookieEntry struct {
	Op        uint8  `json:"op"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Path      string `json:"path"`
	ExpiresMs int64  `json:"expires_ms"`
}

type goldenFile struct {
	Version     int `json:"version"`
	ValidFrames []struct {
		Name       string `json:"name"`
		StreamID   uint32 `json:"stream_id"`
		Type       uint8  `json:"type"`
		Flags      uint8  `json:"flags"`
		PayloadHex string `json:"payload_hex"`
		WireHex    string `json:"wire_hex"`
	} `json:"valid_frames"`
	InvalidFrames []struct {
		Name    string `json:"name"`
		WireHex string `json:"wire_hex"`
	} `json:"invalid_frames"`
	TLVRequests []struct {
		Name     string `json:"name"`
		Kind     uint8  `json:"kind"`
		FetchCtx uint8  `json:"fetch_ctx"`
		Method   string `json:"method"`
		Target   string `json:"target"`
		Headers  []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
		CookieMutations []goldenCookieEntry `json:"cookie_mutations"`
		Hex             string              `json:"hex"`
	} `json:"tlv_requests"`
	TLVResponses []struct {
		Name      string `json:"name"`
		Status    uint16 `json:"status"`
		RespFlags uint8  `json:"resp_flags"`
		Headers   []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
		CookieDeltas []goldenCookieEntry `json:"cookie_deltas"`
		Hex          string              `json:"hex"`
	} `json:"tlv_responses"`
	InvalidTLVRequests []struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
	} `json:"invalid_tlv_requests"`
	InvalidTLVResponses []struct {
		Name string `json:"name"`
		Hex  string `json:"hex"`
	} `json:"invalid_tlv_responses"`
}

func TestV8GoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "protocol_v8_golden_vectors.json"))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("unmarshal golden vectors: %v", err)
	}
	if g.Version != ProtocolVersion {
		t.Fatalf("version mismatch: got %d want %d", g.Version, ProtocolVersion)
	}

	for _, tc := range g.ValidFrames {
		t.Run("valid/"+tc.Name, func(t *testing.T) {
			payload, err := hex.DecodeString(tc.PayloadHex)
			if err != nil {
				t.Fatal(err)
			}
			wantWire, err := hex.DecodeString(tc.WireHex)
			if err != nil {
				t.Fatal(err)
			}
			f := &Frame{StreamID: tc.StreamID, Type: tc.Type, Flags: tc.Flags, Payload: payload}
			gotWire, err := f.Encode()
			if err != nil {
				t.Fatalf("Encode failed: %v", err)
			}
			if !bytes.Equal(gotWire, wantWire) {
				t.Fatalf("wire mismatch:\ngot:  %x\nwant: %x", gotWire, wantWire)
			}
			dec, err := Decode(wantWire)
			if err != nil {
				t.Fatalf("Decode failed: %v", err)
			}
			if dec.StreamID != tc.StreamID || dec.Type != tc.Type || dec.Flags != tc.Flags || !bytes.Equal(dec.Payload, payload) {
				t.Fatalf("decoded mismatch: %+v", dec)
			}
		})
	}

	for _, tc := range g.InvalidFrames {
		t.Run("invalid/"+tc.Name, func(t *testing.T) {
			rawWire, err := hex.DecodeString(tc.WireHex)
			if err != nil {
				t.Fatal(err)
			}
			if f, err := Decode(rawWire); err == nil {
				t.Fatalf("expected invalid frame %s to fail, got %+v", tc.Name, f)
			}
		})
	}

	for _, tc := range g.TLVRequests {
		t.Run("tlv_req/"+tc.Name, func(t *testing.T) {
			req := &RequestHeaders{Kind: tc.Kind, FetchCtx: tc.FetchCtx, Method: tc.Method, Target: tc.Target}
			for _, h := range tc.Headers {
				req.Headers = append(req.Headers, HeaderField{Name: h.Name, Value: h.Value})
			}
			for _, c := range tc.CookieMutations {
				req.CookieMutations = append(req.CookieMutations, CookieEntry(c))
			}
			enc, err := EncodeRequestHeaders(req)
			if err != nil {
				t.Fatalf("EncodeRequestHeaders failed: %v", err)
			}
			if gotHex := hex.EncodeToString(enc); gotHex != tc.Hex {
				t.Fatalf("TLV req hex mismatch:\ngot:  %s\nwant: %s", gotHex, tc.Hex)
			}
			dec, err := DecodeRequestHeaders(enc)
			if err != nil {
				t.Fatalf("DecodeRequestHeaders failed: %v", err)
			}
			if dec.Kind != req.Kind || dec.FetchCtx != req.FetchCtx || dec.Method != req.Method || dec.Target != req.Target ||
				len(dec.Headers) != len(req.Headers) || len(dec.CookieMutations) != len(req.CookieMutations) {
				t.Fatalf("decoded req mismatch: %+v", dec)
			}
		})
	}

	for _, tc := range g.TLVResponses {
		t.Run("tlv_resp/"+tc.Name, func(t *testing.T) {
			resp := &ResponseHeaders{Status: tc.Status, RespFlags: tc.RespFlags}
			for _, h := range tc.Headers {
				resp.Headers = append(resp.Headers, HeaderField{Name: h.Name, Value: h.Value})
			}
			for _, c := range tc.CookieDeltas {
				resp.CookieDeltas = append(resp.CookieDeltas, CookieEntry(c))
			}
			enc, err := EncodeResponseHeaders(resp)
			if err != nil {
				t.Fatalf("EncodeResponseHeaders failed: %v", err)
			}
			if gotHex := hex.EncodeToString(enc); gotHex != tc.Hex {
				t.Fatalf("TLV resp hex mismatch:\ngot:  %s\nwant: %s", gotHex, tc.Hex)
			}
			dec, err := DecodeResponseHeaders(enc)
			if err != nil {
				t.Fatalf("DecodeResponseHeaders failed: %v", err)
			}
			if dec.Status != resp.Status || dec.RespFlags != resp.RespFlags ||
				len(dec.Headers) != len(resp.Headers) || len(dec.CookieDeltas) != len(resp.CookieDeltas) {
				t.Fatalf("decoded resp mismatch: %+v", dec)
			}
		})
	}

	for _, tc := range g.InvalidTLVRequests {
		t.Run("invalid_tlv_req/"+tc.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(tc.Hex)
			if err != nil {
				t.Fatal(err)
			}
			if req, err := DecodeRequestHeaders(raw); err == nil {
				t.Fatalf("expected invalid TLV request %s to fail, got %+v", tc.Name, req)
			}
		})
	}

	for _, tc := range g.InvalidTLVResponses {
		t.Run("invalid_tlv_resp/"+tc.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(tc.Hex)
			if err != nil {
				t.Fatal(err)
			}
			if resp, err := DecodeResponseHeaders(raw); err == nil {
				t.Fatalf("expected invalid TLV response %s to fail, got %+v", tc.Name, resp)
			}
		})
	}
}

func TestFrameBoundariesAndV8Validation(t *testing.T) {
	// Exact 65535-byte DATA payload must succeed; 65536 must fail.
	maxData := make([]byte, MaxPayloadSize)
	f := &Frame{StreamID: 1, Type: TypeData, Payload: maxData}
	wire, err := f.Encode()
	if err != nil {
		t.Fatalf("exact MaxPayloadSize failed: %v", err)
	}
	if dec, err := Decode(wire); err != nil || len(dec.Payload) != MaxPayloadSize {
		t.Fatalf("decode exact MaxPayloadSize failed: %v", err)
	}
	fOver := &Frame{StreamID: 1, Type: TypeData, Payload: make([]byte, MaxPayloadSize+1)}
	if _, err := fOver.Encode(); err == nil {
		t.Fatal("expected MaxPayloadSize+1 to fail")
	}

	// H-6a: EncodeRequestHeaders with 65535B Target and 0 headers MUST fail 64 KiB cap!
	if _, err := EncodeRequestHeaders(&RequestHeaders{
		Kind:   StreamKindHTTP,
		Method: "GET",
		Target: "/" + strings.Repeat("a", 65534),
	}); err == nil {
		t.Fatal("H-6a regression: EncodeRequestHeaders allowed >64 KiB block with 0 headers")
	}

	// H-6c: ExpiresMs > MaxSafeExpiresMs (2^53 - 1) MUST fail encode.
	if _, err := EncodeRequestHeaders(&RequestHeaders{
		Kind:   StreamKindHTTP,
		Method: "GET",
		Target: "/",
		CookieMutations: []CookieEntry{
			{Op: CookieOpSet, Name: "a", Value: "1", Path: "/", ExpiresMs: MaxSafeExpiresMs + 1},
		},
	}); err == nil {
		t.Fatal("H-6c regression: expected ExpiresMs > 2^53-1 to fail encode")
	}

	// H-6d: EncodeWSClosePayload sanitizes invalid UTF-8 so DecodeWSClosePayload always accepts it.
	badUTF8Close := EncodeWSClosePayload(1000, "bad\xff\xfeutf8")
	if c, _, derr := DecodeWSClosePayload(badUTF8Close); derr != nil || c != 1000 {
		t.Fatalf("H-6d regression: EncodeWSClosePayload produced invalid UTF-8 payload: err=%v", derr)
	}

	// Pool channel IDs & labels (1..12 -> p2p_01..p2p_12).
	if PoolChannelID(0) != 1 || PoolChannelLabel(0) != "p2p_01" {
		t.Fatalf("unexpected lane 0: id=%d label=%s", PoolChannelID(0), PoolChannelLabel(0))
	}
	if PoolChannelID(11) != 12 || PoolChannelLabel(11) != "p2p_12" {
		t.Fatalf("unexpected lane 11: id=%d label=%s", PoolChannelID(11), PoolChannelLabel(11))
	}

	// Target validation: must start with '/', no space, no backslash, no CTL.
	for _, badTarget := range []string{"", "no-slash", "/bad path HTTP/1.1", "/bad\\path", "/bad\x01path"} {
		if _, err := EncodeRequestHeaders(&RequestHeaders{
			Kind: StreamKindHTTP, Method: "GET", Target: badTarget,
		}); err == nil {
			t.Fatalf("expected bad target %q to fail", badTarget)
		}
	}

	// Unicode Kelvin sign U+212A ('K') / Latin long s U+017F ('ſ') MUST be rejected BEFORE ASCII folding!
	for _, badHeader := range []string{"x-\u212aey", "x-\u017fest", "bad name", "bad:name"} {
		if _, err := EncodeRequestHeaders(&RequestHeaders{
			Kind:    StreamKindHTTP,
			Method:  "GET",
			Target:  "/",
			Headers: []HeaderField{{Name: badHeader, Value: "ok"}},
		}); err == nil {
			t.Fatalf("expected non-tchar header name %q to fail", badHeader)
		}
	}
	if _, err := EncodeRequestHeaders(&RequestHeaders{
		Kind: StreamKindHTTP, Method: "G\u212aT", Target: "/",
	}); err == nil {
		t.Fatal("expected Unicode Kelvin sign in method to fail")
	}

	// CTL characters in header values MUST be rejected; HTAB (0x09) and valid UTF-8 MUST pass.
	for _, badVal := range []string{"bad\r\nEvil: 1", "bad\x00val", "bad\x01val", "bad\x0bval", "bad\x7fval"} {
		if _, err := EncodeRequestHeaders(&RequestHeaders{
			Kind:    StreamKindHTTP,
			Method:  "GET",
			Target:  "/",
			Headers: []HeaderField{{Name: "x-test", Value: badVal}},
		}); err == nil {
			t.Fatalf("expected CTL header value %q to fail", badVal)
		}
	}
	if _, err := EncodeRequestHeaders(&RequestHeaders{
		Kind:    StreamKindHTTP,
		Method:  "GET",
		Target:  "/",
		Headers: []HeaderField{{Name: "x-test", Value: "ok\t中文"}},
	}); err != nil {
		t.Fatalf("expected HTAB + UTF-8 header value to pass, got %v", err)
	}
	if got := SanitizeHeaderValue("a\x00\x01\x0b\x7fb\tc"); got != "ab\tc" {
		t.Fatalf("SanitizeHeaderValue mismatch: got %q", got)
	}

	// 64 KiB header block limit MUST be enforced on encode.
	if _, err := EncodeResponseHeaders(&ResponseHeaders{
		Status:  200,
		Headers: []HeaderField{{Name: "x-big", Value: strings.Repeat("a", MaxTotalHeadersSize)}},
	}); err == nil {
		t.Fatal("expected > 64 KiB response header block to fail")
	}

	// Reserved FetchCtx / RespFlags bits MUST be rejected.
	if _, err := EncodeRequestHeaders(&RequestHeaders{
		Kind: StreamKindHTTP, FetchCtx: 0x80, Method: "GET", Target: "/",
	}); err == nil {
		t.Fatal("expected reserved FetchCtx bit to fail")
	}
	if _, err := EncodeResponseHeaders(&ResponseHeaders{
		Status: 200, RespFlags: 0x80,
	}); err == nil {
		t.Fatal("expected reserved RespFlags bit to fail")
	}

	// Negative cookie ExpiresMs MUST be rejected on encode, and >MaxInt64 on decode.
	if _, err := EncodeRequestHeaders(&RequestHeaders{
		Kind:   StreamKindHTTP,
		Method: "GET",
		Target: "/",
		CookieMutations: []CookieEntry{
			{Op: CookieOpSet, Name: "a", Value: "1", Path: "/", ExpiresMs: -1},
		},
	}); err == nil {
		t.Fatal("expected negative ExpiresMs to fail encode")
	}
	validCook, err := EncodeRequestHeaders(&RequestHeaders{
		Kind:   StreamKindHTTP,
		Method: "GET",
		Target: "/",
		CookieMutations: []CookieEntry{
			{Op: CookieOpSet, Name: "a", Value: "1", Path: "/", ExpiresMs: 12345},
		},
	})
	if err != nil {
		t.Fatalf("valid cookie encode failed: %v", err)
	}
	// Set MSB of trailing 8-byte ExpiresMs to force uint64 > MaxInt64.
	validCook[len(validCook)-8] = 0x80
	if _, err := DecodeRequestHeaders(validCook); err == nil {
		t.Fatal("expected >MaxInt64 ExpiresMs to fail decode")
	}

	// WS close payload truncation preserves valid UTF-8 and <= 125 bytes.
	longReason := string(bytes.Repeat([]byte("测"), 60)) // 180 bytes
	cl := EncodeWSClosePayload(1000, longReason)
	if len(cl) > MaxWSClosePayloadBytes {
		t.Fatalf("WS close payload exceeded 125B: %d", len(cl))
	}
	code, reason, err := DecodeWSClosePayload(cl)
	if err != nil || code != 1000 || len(reason) > 123 {
		t.Fatalf("unexpected decoded WS close: code=%d len=%d err=%v", code, len(reason), err)
	}
}
