package protocol

import (
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"unicode/utf8"
)

const (
	StreamKindHTTP uint8 = 1
	StreamKindWS   uint8 = 2

	MaxHeaderCount            = 256
	MaxMethodLen              = 16
	MaxRequestCookieMutations = 32
	MaxResponseCookieDeltas   = 64
	MaxCookieNameLen          = 255
	// MaxSafeExpiresMs matches JavaScript Number.MAX_SAFE_INTEGER (2^53 - 1) for Go<->TS wire parity (H-6).
	MaxSafeExpiresMs int64 = (1 << 53) - 1
)

// P2PMux V8 FetchCtx bitfield (RequestHeaders).
// Bit 1 (0x02, legacy TopLevelNav) is removed and reserved because top-level sandbox
// document navigation is unconditionally blocked at the Service Worker gate.
const (
	FetchCtxSameOriginClient     uint8 = 1 << 0
	FetchCtxClientDecompressGzip uint8 = 1 << 2
	FetchCtxCredentialsOmit      uint8 = 1 << 3
	FetchCtxValidMask            uint8 = FetchCtxSameOriginClient | FetchCtxClientDecompressGzip | FetchCtxCredentialsOmit
)

// V7 RespFlags bitfield (ResponseHeaders).
const (
	RespFlagDecompressGzip uint8 = 1 << 0
	RespFlagsValidMask     uint8 = RespFlagDecompressGzip
)

// V7 binary TLV cookie operation codes.
const (
	CookieOpSet uint8 = 1
	CookieOpDel uint8 = 2
)

var (
	ErrTLVTruncated      = errors.New("protocol/tlv: truncated header block")
	ErrTLVTrailingBytes  = errors.New("protocol/tlv: unexpected trailing bytes")
	ErrTLVInvalidKind    = errors.New("protocol/tlv: invalid stream kind")
	ErrTLVInvalidFlags   = errors.New("protocol/tlv: invalid reserved flags")
	ErrTLVInvalidMethod  = errors.New("protocol/tlv: invalid HTTP method")
	ErrTLVInvalidTarget  = errors.New("protocol/tlv: invalid request target")
	ErrTLVInvalidStatus  = errors.New("protocol/tlv: status code must be in [100, 599]")
	ErrTLVTooManyHeaders = errors.New("protocol/tlv: header count exceeds limit")
	ErrTLVInvalidHeader  = errors.New("protocol/tlv: invalid header name or value")
	ErrTLVTooManyCookies = errors.New("protocol/tlv: cookie record count exceeds limit")
	ErrTLVInvalidCookie  = errors.New("protocol/tlv: invalid cookie record")
	ErrTLVTooLarge       = errors.New("protocol/tlv: encoded header block exceeds 64 KiB")
)

// HeaderField is an ordered header key-value entry preserving multi-value headers natively.
type HeaderField struct {
	Name  string
	Value string
}

// CookieEntry is a compact binary TLV cookie mutation (request) or delta (response).
type CookieEntry struct {
	Op        uint8  // CookieOpSet (1) or CookieOpDel (2)
	Name      string // 1..255 bytes
	Value     string // 0..65535 bytes
	Path      string // 0..65535 bytes
	ExpiresMs int64  // unix ms (0 = session cookie)
}

// RequestHeaders is the V7 compact binary TLV representation of a stream open request.
type RequestHeaders struct {
	Kind            uint8         // StreamKindHTTP or StreamKindWS
	FetchCtx        uint8         // FetchCtx* bitfield
	Method          string        // Uppercase RFC 9110 tchar token (e.g. GET, POST)
	Target          string        // Request path+query starting with '/'
	Headers         []HeaderField // Lowercase RFC 9110 tchar header names
	CookieMutations []CookieEntry // Up to 32 causal JS non-HttpOnly cookie mutations
}

// ResponseHeaders is the V7 compact binary TLV representation of a stream response.
type ResponseHeaders struct {
	Status       uint16        // HTTP status code 100..599
	RespFlags    uint8         // RespFlag* bitfield
	Headers      []HeaderField // Lowercase RFC 9110 tchar header names
	CookieDeltas []CookieEntry // Up to 64 non-HttpOnly Set-Cookie deltas
}

// RFC 9110 §5.6.2 tchar lookup table.
var tcharTable [256]bool

func init() {
	for c := '0'; c <= '9'; c++ {
		tcharTable[c] = true
	}
	for c := 'A'; c <= 'Z'; c++ {
		tcharTable[c] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		tcharTable[c] = true
	}
	for _, c := range []byte("!#$%&'*+-.^_`|~") {
		tcharTable[c] = true
	}
}

// isValidRFC9110Token validates pure ASCII RFC 9110 tchar BEFORE any case folding
// (prevents Unicode Kelvin sign U+212A or long s U+017F folding into ASCII!).
func isValidRFC9110Token(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !tcharTable[s[i]] {
			return false
		}
	}
	return true
}

func asciiToLower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func asciiToUpper(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// isValidHeaderValue allows strictly HTAB (0x09), visible ASCII/SP (0x20..0x7E),
// and valid UTF-8 sequences (>= 0x80); rejects all ASCII CTLs (0x00..0x08, 0x0A..0x1F, 0x7F).
func isValidHeaderValue(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 && c != 0x09 {
			return false
		}
		if c == 0x7F {
			return false
		}
	}
	return true
}

// SanitizeHeaderValue converts arbitrary raw upstream header bytes into a valid UTF-8,
// CTL-free string (retaining HTAB 0x09, 0x20..0x7E, and valid UTF-8 >= 0x80) so legacy
// backends never cause TLV errors or browser `new Response()` TypeErrors.
func SanitizeHeaderValue(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); {
		c := raw[i]
		if c < 0x80 {
			if c == 0x09 || (c >= 0x20 && c <= 0x7E) {
				b.WriteByte(c)
			}
			i++
			continue
		}
		r, sz := utf8.DecodeRuneInString(raw[i:])
		if r == utf8.RuneError && sz == 1 {
			// Preserve raw Latin-1 / non-UTF-8 byte (>= 0x80) as its Unicode code point.
			b.WriteRune(rune(c))
			i++
			continue
		}
		b.WriteRune(r)
		i += sz
	}
	return b.String()
}

func isValidTarget(s string) bool {
	if len(s) == 0 || len(s) > 0xFFFF || s[0] != '/' || !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		// Reject control chars, ASCII space (0x20), DEL (0x7F), and backslash ('\').
		if c <= 0x20 || c == 0x7F || c == '\\' {
			return false
		}
	}
	return true
}

func validateCookieEntry(e CookieEntry) error {
	if e.Op != CookieOpSet && e.Op != CookieOpDel {
		return ErrTLVInvalidCookie
	}
	if len(e.Name) == 0 || len(e.Name) > MaxCookieNameLen || !isValidHeaderValue(e.Name) {
		return ErrTLVInvalidCookie
	}
	if len(e.Value) > 0xFFFF || !isValidHeaderValue(e.Value) {
		return ErrTLVInvalidCookie
	}
	if len(e.Path) > 0xFFFF || !isValidHeaderValue(e.Path) {
		return ErrTLVInvalidCookie
	}
	if e.ExpiresMs < 0 || e.ExpiresMs > MaxSafeExpiresMs {
		return ErrTLVInvalidCookie
	}
	return nil
}

func boundedUint8(n int) uint8 {
	if n <= 0 {
		return 0
	}
	if n > math.MaxUint8 {
		return math.MaxUint8
	}
	return uint8(n)
}

func boundedUint16(n int) uint16 {
	if n <= 0 {
		return 0
	}
	if n > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(n)
}

// EncodeRequestHeaders encodes V7 RequestHeaders into compact binary TLV:
// [u8 kind][u8 fetchCtx][u8 methodLen][method][u16 targetLen][target]
// [u16 headerCount]([u16 kLen][k][u16 vLen][v])*
// [u16 cookieMutCount]([u8 op][u8 nameLen][name][u16 valLen][val][u16 pathLen][path][i64 expiresMs])*
func EncodeRequestHeaders(req *RequestHeaders) ([]byte, error) {
	if req == nil || (req.Kind != StreamKindHTTP && req.Kind != StreamKindWS) {
		return nil, ErrTLVInvalidKind
	}
	if req.FetchCtx & ^FetchCtxValidMask != 0 {
		return nil, ErrTLVInvalidFlags
	}
	// Validate strict ASCII RFC 9110 tchar BEFORE uppercasing.
	if len(req.Method) == 0 || len(req.Method) > MaxMethodLen || !isValidRFC9110Token(req.Method) {
		return nil, ErrTLVInvalidMethod
	}
	method := asciiToUpper(req.Method)
	if !isValidTarget(req.Target) {
		return nil, ErrTLVInvalidTarget
	}
	if len(req.Headers) > MaxHeaderCount {
		return nil, ErrTLVTooManyHeaders
	}
	if len(req.CookieMutations) > MaxRequestCookieMutations {
		return nil, ErrTLVTooManyCookies
	}

	total := 1 + 1 + 1 + len(method) + 2 + len(req.Target) + 2 + 2
	if total > MaxTotalHeadersSize {
		return nil, ErrTLVTooLarge
	}
	normNames := make([]string, len(req.Headers))
	for i, h := range req.Headers {
		if len(h.Name) == 0 || len(h.Name) > 0xFFFF || !isValidRFC9110Token(h.Name) || len(h.Value) > 0xFFFF || !isValidHeaderValue(h.Value) {
			return nil, ErrTLVInvalidHeader
		}
		name := asciiToLower(h.Name)
		normNames[i] = name
		total += 4 + len(name) + len(h.Value)
		if total > MaxTotalHeadersSize {
			return nil, ErrTLVTooLarge
		}
	}
	for _, c := range req.CookieMutations {
		if err := validateCookieEntry(c); err != nil {
			return nil, err
		}
		total += 1 + 1 + len(c.Name) + 2 + len(c.Value) + 2 + len(c.Path) + 8
		if total > MaxTotalHeadersSize {
			return nil, ErrTLVTooLarge
		}
	}

	buf := make([]byte, total)
	pos := 0
	buf[pos] = req.Kind
	pos++
	buf[pos] = req.FetchCtx
	pos++
	buf[pos] = boundedUint8(len(method))
	pos++
	copy(buf[pos:], method)
	pos += len(method)
	binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(req.Target)))
	pos += 2
	copy(buf[pos:], req.Target)
	pos += len(req.Target)

	binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(req.Headers)))
	pos += 2
	for i, h := range req.Headers {
		k := normNames[i]
		binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(k)))
		pos += 2
		copy(buf[pos:], k)
		pos += len(k)
		binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(h.Value)))
		pos += 2
		copy(buf[pos:], h.Value)
		pos += len(h.Value)
	}

	pos = encodeCookieEntries(buf, pos, req.CookieMutations)
	return buf[:pos], nil
}

// DecodeRequestHeaders strictly decodes V7 binary TLV request headers.
func DecodeRequestHeaders(raw []byte) (*RequestHeaders, error) {
	if len(raw) > MaxTotalHeadersSize {
		return nil, ErrTLVTooLarge
	}
	pos := 0
	if len(raw) < 3 {
		return nil, ErrTLVTruncated
	}
	kind := raw[pos]
	pos++
	if kind != StreamKindHTTP && kind != StreamKindWS {
		return nil, ErrTLVInvalidKind
	}
	fetchCtx := raw[pos]
	pos++
	if fetchCtx & ^FetchCtxValidMask != 0 {
		return nil, ErrTLVInvalidFlags
	}
	mLen := int(raw[pos])
	pos++
	if mLen == 0 || mLen > MaxMethodLen || pos+mLen+6 > len(raw) {
		return nil, ErrTLVInvalidMethod
	}
	methodRaw := string(raw[pos : pos+mLen])
	pos += mLen
	if !isValidRFC9110Token(methodRaw) {
		return nil, ErrTLVInvalidMethod
	}
	method := asciiToUpper(methodRaw)

	tLen := int(binary.BigEndian.Uint16(raw[pos:]))
	pos += 2
	if tLen == 0 || pos+tLen+4 > len(raw) {
		return nil, ErrTLVTruncated
	}
	target := string(raw[pos : pos+tLen])
	pos += tLen
	if !isValidTarget(target) {
		return nil, ErrTLVInvalidTarget
	}

	headers, nextPos, err := decodeHeaderFields(raw, pos)
	if err != nil {
		return nil, err
	}
	cookies, finalPos, err := decodeCookieEntries(raw, nextPos, MaxRequestCookieMutations)
	if err != nil {
		return nil, err
	}
	if finalPos != len(raw) {
		return nil, ErrTLVTrailingBytes
	}
	return &RequestHeaders{
		Kind:            kind,
		FetchCtx:        fetchCtx,
		Method:          method,
		Target:          target,
		Headers:         headers,
		CookieMutations: cookies,
	}, nil
}

// EncodeResponseHeaders encodes V7 ResponseHeaders into compact binary TLV:
// [u16 status][u8 respFlags][u16 headerCount]([u16 kLen][k][u16 vLen][v])*
// [u16 cookieDeltaCount]([u8 op][u8 nameLen][name][u16 valLen][val][u16 pathLen][path][i64 expiresMs])*
func EncodeResponseHeaders(resp *ResponseHeaders) ([]byte, error) {
	if resp == nil || resp.Status < 100 || resp.Status > 599 {
		return nil, ErrTLVInvalidStatus
	}
	if resp.RespFlags & ^RespFlagsValidMask != 0 {
		return nil, ErrTLVInvalidFlags
	}
	if len(resp.Headers) > MaxHeaderCount {
		return nil, ErrTLVTooManyHeaders
	}
	if len(resp.CookieDeltas) > MaxResponseCookieDeltas {
		return nil, ErrTLVTooManyCookies
	}

	total := 2 + 1 + 2 + 2
	normNames := make([]string, len(resp.Headers))
	for i, h := range resp.Headers {
		if len(h.Name) == 0 || len(h.Name) > 0xFFFF || !isValidRFC9110Token(h.Name) || len(h.Value) > 0xFFFF || !isValidHeaderValue(h.Value) {
			return nil, ErrTLVInvalidHeader
		}
		name := asciiToLower(h.Name)
		normNames[i] = name
		total += 4 + len(name) + len(h.Value)
		if total > MaxTotalHeadersSize {
			return nil, ErrTLVTooLarge
		}
	}
	for _, c := range resp.CookieDeltas {
		if err := validateCookieEntry(c); err != nil {
			return nil, err
		}
		total += 1 + 1 + len(c.Name) + 2 + len(c.Value) + 2 + len(c.Path) + 8
		if total > MaxTotalHeadersSize {
			return nil, ErrTLVTooLarge
		}
	}

	buf := make([]byte, total)
	pos := 0
	binary.BigEndian.PutUint16(buf[pos:], resp.Status)
	pos += 2
	buf[pos] = resp.RespFlags
	pos++
	binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(resp.Headers)))
	pos += 2
	for i, h := range resp.Headers {
		k := normNames[i]
		binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(k)))
		pos += 2
		copy(buf[pos:], k)
		pos += len(k)
		binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(h.Value)))
		pos += 2
		copy(buf[pos:], h.Value)
		pos += len(h.Value)
	}

	pos = encodeCookieEntries(buf, pos, resp.CookieDeltas)
	return buf[:pos], nil
}

// DecodeResponseHeaders strictly decodes V7 binary TLV response headers.
func DecodeResponseHeaders(raw []byte) (*ResponseHeaders, error) {
	if len(raw) > MaxTotalHeadersSize {
		return nil, ErrTLVTooLarge
	}
	if len(raw) < 7 {
		return nil, ErrTLVTruncated
	}
	status := binary.BigEndian.Uint16(raw[0:2])
	if status < 100 || status > 599 {
		return nil, ErrTLVInvalidStatus
	}
	respFlags := raw[2]
	if respFlags & ^RespFlagsValidMask != 0 {
		return nil, ErrTLVInvalidFlags
	}
	headers, nextPos, err := decodeHeaderFields(raw, 3)
	if err != nil {
		return nil, err
	}
	deltas, finalPos, err := decodeCookieEntries(raw, nextPos, MaxResponseCookieDeltas)
	if err != nil {
		return nil, err
	}
	if finalPos != len(raw) {
		return nil, ErrTLVTrailingBytes
	}
	return &ResponseHeaders{
		Status:       status,
		RespFlags:    respFlags,
		Headers:      headers,
		CookieDeltas: deltas,
	}, nil
}

func decodeHeaderFields(raw []byte, pos int) ([]HeaderField, int, error) {
	if pos+2 > len(raw) {
		return nil, pos, ErrTLVTruncated
	}
	count := int(binary.BigEndian.Uint16(raw[pos:]))
	pos += 2
	if count > MaxHeaderCount {
		return nil, pos, ErrTLVTooManyHeaders
	}
	fields := make([]HeaderField, count)
	for i := 0; i < count; i++ {
		if pos+2 > len(raw) {
			return nil, pos, ErrTLVTruncated
		}
		kLen := int(binary.BigEndian.Uint16(raw[pos:]))
		pos += 2
		if kLen == 0 || pos+kLen+2 > len(raw) {
			return nil, pos, ErrTLVTruncated
		}
		kRaw := string(raw[pos : pos+kLen])
		pos += kLen
		if !isValidRFC9110Token(kRaw) {
			return nil, pos, ErrTLVInvalidHeader
		}
		k := asciiToLower(kRaw)

		vLen := int(binary.BigEndian.Uint16(raw[pos:]))
		pos += 2
		if pos+vLen > len(raw) {
			return nil, pos, ErrTLVTruncated
		}
		v := string(raw[pos : pos+vLen])
		pos += vLen
		if !isValidHeaderValue(v) {
			return nil, pos, ErrTLVInvalidHeader
		}
		fields[i] = HeaderField{Name: k, Value: v}
	}
	return fields, pos, nil
}

func encodeCookieEntries(buf []byte, pos int, entries []CookieEntry) int {
	binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(entries)))
	pos += 2
	for _, c := range entries {
		buf[pos] = c.Op
		pos++
		buf[pos] = boundedUint8(len(c.Name))
		pos++
		copy(buf[pos:], c.Name)
		pos += len(c.Name)
		binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(c.Value)))
		pos += 2
		copy(buf[pos:], c.Value)
		pos += len(c.Value)
		binary.BigEndian.PutUint16(buf[pos:], boundedUint16(len(c.Path)))
		pos += 2
		copy(buf[pos:], c.Path)
		pos += len(c.Path)
		var exp uint64
		if c.ExpiresMs > 0 {
			exp = uint64(c.ExpiresMs)
		}
		binary.BigEndian.PutUint64(buf[pos:], exp)
		pos += 8
	}
	return pos
}

func decodeCookieEntries(raw []byte, pos, maxCount int) ([]CookieEntry, int, error) {
	if pos+2 > len(raw) {
		return nil, pos, ErrTLVTruncated
	}
	count := int(binary.BigEndian.Uint16(raw[pos:]))
	pos += 2
	if count > maxCount {
		return nil, pos, ErrTLVTooManyCookies
	}
	if count == 0 {
		return nil, pos, nil
	}
	out := make([]CookieEntry, count)
	for i := 0; i < count; i++ {
		if pos+2 > len(raw) {
			return nil, pos, ErrTLVTruncated
		}
		op := raw[pos]
		pos++
		nLen := int(raw[pos])
		pos++
		if nLen == 0 || pos+nLen+2 > len(raw) {
			return nil, pos, ErrTLVTruncated
		}
		name := string(raw[pos : pos+nLen])
		pos += nLen

		vLen := int(binary.BigEndian.Uint16(raw[pos:]))
		pos += 2
		if pos+vLen+2 > len(raw) {
			return nil, pos, ErrTLVTruncated
		}
		val := string(raw[pos : pos+vLen])
		pos += vLen

		pLen := int(binary.BigEndian.Uint16(raw[pos:]))
		pos += 2
		if pos+pLen+8 > len(raw) {
			return nil, pos, ErrTLVTruncated
		}
		p := string(raw[pos : pos+pLen])
		pos += pLen

		rawExp := binary.BigEndian.Uint64(raw[pos:])
		pos += 8
		if rawExp > uint64(MaxSafeExpiresMs) {
			return nil, pos, ErrTLVInvalidCookie
		}
		exp := int64(rawExp)

		entry := CookieEntry{
			Op:        op,
			Name:      name,
			Value:     val,
			Path:      p,
			ExpiresMs: exp,
		}
		if err := validateCookieEntry(entry); err != nil {
			return nil, pos, err
		}
		out[i] = entry
	}
	return out, pos, nil
}
