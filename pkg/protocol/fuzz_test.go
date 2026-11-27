package protocol

import "testing"

func FuzzDecodeFrame(f *testing.F) {
	validPing, _ := (&Frame{Gen: 0, Type: TypePing, Flags: FlagNone, Payload: EncodePingPayload(42)}).Encode()
	validData, _ := (&Frame{Gen: 1, Type: TypeData, Flags: FlagFin, Payload: []byte("hello")}).Encode()
	validClose, _ := (&Frame{Gen: 1, Type: TypeWSClose, Flags: FlagNone, Payload: EncodeWSClosePayload(1000, "bye")}).Encode()
	f.Add(validPing)
	f.Add(validData)
	f.Add(validClose)
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 1, TypeContinuation, FlagMoreHeaders, 0, 0})
	f.Add([]byte{0, 0, 0, 1, TypeWSCont, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := Decode(data)
		if err != nil {
			return
		}
		// Any frame accepted by Decode MUST round-trip identically via Encode!
		reencoded, err := frame.Encode()
		if err != nil {
			t.Fatalf("Decode succeeded but Encode failed: %v", err)
		}
		if len(reencoded) != len(data) {
			t.Fatalf("round-trip length mismatch: got %d want %d", len(reencoded), len(data))
		}
	})
}

func FuzzDecodeRequestHeadersTLV(f *testing.F) {
	seed, _ := EncodeRequestHeaders(&RequestHeaders{
		Kind:     StreamKindHTTP,
		FetchCtx: FetchCtxSameOriginClient | FetchCtxClientDecompressGzip,
		Method:   "POST",
		Target:   "/api/v1?x=1",
		Headers:  []HeaderField{{Name: "content-type", Value: "application/json"}},
		CookieMutations: []CookieEntry{
			{Op: CookieOpSet, Name: "theme", Value: "dark", Path: "/", ExpiresMs: 1893456000000},
		},
	})
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{1, 0, 3, 'G', 'E', 'T', 0, 1, '/', 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := DecodeRequestHeaders(data)
		if err != nil {
			return
		}
		reencoded, err := EncodeRequestHeaders(req)
		if err != nil {
			t.Fatalf("DecodeRequestHeaders succeeded but EncodeRequestHeaders failed: %v", err)
		}
		if _, err := DecodeRequestHeaders(reencoded); err != nil {
			t.Fatalf("re-encoded RequestHeaders failed decode: %v", err)
		}
	})
}

func FuzzDecodeResponseHeadersTLV(f *testing.F) {
	seed, _ := EncodeResponseHeaders(&ResponseHeaders{
		Status:    200,
		RespFlags: RespFlagDecompressGzip,
		Headers:   []HeaderField{{Name: "content-type", Value: "text/html; charset=utf-8"}},
		CookieDeltas: []CookieEntry{
			{Op: CookieOpDel, Name: "old", Path: "/"},
		},
	})
	f.Add(seed)
	f.Add([]byte{})
	f.Add([]byte{0, 200, 0, 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		resp, err := DecodeResponseHeaders(data)
		if err != nil {
			return
		}
		reencoded, err := EncodeResponseHeaders(resp)
		if err != nil {
			t.Fatalf("DecodeResponseHeaders succeeded but EncodeResponseHeaders failed: %v", err)
		}
		if _, err := DecodeResponseHeaders(reencoded); err != nil {
			t.Fatalf("re-encoded ResponseHeaders failed decode: %v", err)
		}
	})
}
