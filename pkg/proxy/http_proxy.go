package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
)

const (
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultExpectContinueTimeout = 1 * time.Second
	defaultResponseHeaderTimeout = 120 * time.Second
)

// HTTPProxyOptions configures the single-target Pure Pipe HTTPProxy.
type HTTPProxyOptions struct {
	Policy             *DestinationPolicy
	InsecureSkipVerify bool
	SNI                string
	ProxyURL           *url.URL
	CustomTransport    http.RoundTripper // Optional in-process RoundTripper (used by os.Root static adapter)
}

// HTTPProxy reverse-proxies V7 HTTP streams strictly to the Agent's single configured target.
type HTTPProxy struct {
	policy *DestinationPolicy
	client *http.Client
}

func NewHTTPProxy(opts HTTPProxyOptions) (*HTTPProxy, error) {
	if opts.Policy == nil {
		return nil, errors.New("proxy: DestinationPolicy is required")
	}

	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	var targetTr http.RoundTripper = opts.CustomTransport
	if targetTr == nil {
		targetTLS := &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: opts.InsecureSkipVerify, // #nosec G402 -- Fail-Closed default false; explicitly opt-in via -insecure-target for self-signed intranet upstreams
		}
		if sni := opts.Policy.DefaultTLSServerName(opts.SNI); sni != "" {
			targetTLS.ServerName = sni
		}
		t := &http.Transport{
			DialContext:           newTargetDialContext(),
			TLSClientConfig:       targetTLS,
			TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
			ExpectContinueTimeout: defaultExpectContinueTimeout,
			ResponseHeaderTimeout: defaultResponseHeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   32,
			DisableCompression:    true,
		}
		if opts.ProxyURL != nil {
			t.Proxy = http.ProxyURL(opts.ProxyURL)
		}
		targetTr = t
	}

	return &HTTPProxy{
		policy: opts.Policy,
		client: &http.Client{Transport: targetTr, CheckRedirect: noRedirect},
	}, nil
}

func (p *HTTPProxy) Policy() *DestinationPolicy { return p.policy }

// HandleStream processes a single HTTP stream whose RequestHeaders have already been decoded.
func (p *HTTPProxy) HandleStream(
	stream *mux.Stream,
	reqHdr *protocol.RequestHeaders,
	clientIP string,
	jar *ProxyCookieJar,
	capScope *CapabilityScope,
) {
	// Always Close() on return to flush FIN and unconditionally purge unread inbound bytes.
	defer func() {
		_ = stream.Close()
	}()

	method := strings.ToUpper(reqHdr.Method)
	if method == http.MethodTrace || method == http.MethodConnect {
		p.writeSimpleError(stream, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Step 1: Resolve & canonicalize destination before capability validation.
	dest, err := p.policy.Resolve(reqHdr.Target, false)
	if err != nil {
		p.writeSimpleError(stream, http.StatusForbidden, "destination not allowed")
		return
	}

	// Step 2: Enforce capability token scope against canonical decoded destination!
	if !capScope.AllowsDestination(protocol.StreamKindHTTP, method, dest) {
		p.writeSimpleError(stream, http.StatusForbidden, "forbidden by share capability scope")
		return
	}

	clientAcceptsGzip := (reqHdr.FetchCtx & protocol.FetchCtxClientDecompressGzip) != 0
	clientAcceptsSSE := false
	hasRangeHeader := false
	var declaredContentLen int64 = -1
	sawContentLen := false
	for _, f := range reqHdr.Headers {
		k := strings.ToLower(f.Name)
		switch k {
		case "accept":
			if strings.Contains(strings.ToLower(f.Value), "text/event-stream") {
				clientAcceptsSSE = true
			}
		case "range":
			if strings.TrimSpace(f.Value) != "" {
				hasRangeHeader = true
			}
		case "content-length":
			n, perr := parseStrictContentLength(f.Value)
			if perr != nil {
				p.writeSimpleError(stream, http.StatusBadRequest, "invalid Content-Length header")
				return
			}
			if sawContentLen && n != declaredContentLen {
				// Reject conflicting duplicate Content-Length (RFC 9112 §6.3 request smuggling defense).
				p.writeSimpleError(stream, http.StatusBadRequest, "conflicting Content-Length headers")
				return
			}
			sawContentLen = true
			declaredContentLen = n
		}
	}

	credentialsOmit := (reqHdr.FetchCtx & protocol.FetchCtxCredentialsOmit) != 0
	if jar != nil && !credentialsOmit && len(reqHdr.CookieMutations) > 0 {
		jar.ApplyClientMutations(p.policy.CookieURL(dest.URL), cookieEntriesToMutations(reqHdr.CookieMutations))
	}
	readonly := capScope != nil && capScope.Readonly
	upstreamHeaders := p.policy.SanitizeRequestHeaders(
		reqHdr.Headers, reqHdr.FetchCtx, method, dest, clientIP, jar, false, readonly,
	)
	if clientAcceptsGzip && !clientAcceptsSSE && !hasRangeHeader && method != http.MethodHead && upstreamHeaders.Get("Accept-Encoding") == "" {
		upstreamHeaders.Set("Accept-Encoding", "gzip")
	}

	// Step 3: Gate request body strictly on stream.HasRequestBody() (!headersHadFin) so a fast
	// response never races with a short request body; wrap with io.NopCloser so Transport closing
	// req.Body never closes the P2P response stream.
	var bodyReader io.Reader
	if method == http.MethodGet || method == http.MethodHead {
		// Drop body AND strip Content-Length on GET/HEAD so upstream never waits for an unsent body.
		upstreamHeaders.Del("Content-Length")
	} else if declaredContentLen == 0 {
		// Explicit Content-Length: 0 -> pass nil body cleanly.
		bodyReader = nil
	} else if stream.HasRequestBody() {
		bodyReader = io.NopCloser(stream)
	}

	httpReq, err := http.NewRequestWithContext(stream.Context(), method, dest.URL.String(), bodyReader)
	if err != nil {
		p.writeSimpleError(stream, http.StatusBadRequest, "invalid request")
		return
	}
	httpReq.Header = upstreamHeaders
	httpReq.Host = p.policy.EffectiveHost()
	if method != http.MethodGet && method != http.MethodHead && sawContentLen {
		httpReq.ContentLength = declaredContentLen
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		if stream.Context().Err() != nil {
			return
		}
		p.writeSimpleError(stream, http.StatusBadGateway, "upstream request failed")
		return
	}
	stopCancelClose := context.AfterFunc(stream.Context(), func() {
		_ = resp.Body.Close()
	})
	defer func() {
		stopCancelClose()
		_ = resp.Body.Close()
	}()

	// Mark long-lived streaming responses (SSE, NDJSON, MJPEG/multipart) so idle reaper never kills them.
	respCT := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if isLongLivedStreamMIME(respCT) {
		stream.SetLongLived(true)
	}

	// Save Set-Cookie in authoritative ProxyCookieJar; emit non-HttpOnly deltas for browser CookieMirror.
	// Per Fetch standard, credentials: 'omit' must not store response cookies.
	var cookieDeltas []CookieDelta
	if !credentialsOmit {
		if setCookies := resp.Header.Values("Set-Cookie"); len(setCookies) > 0 && jar != nil {
			cookieDeltas = jar.SaveResponseCookies(p.policy.CookieURL(dest.URL), setCookies)
		}
	}

	ceTokens := parseContentEncodingTokens(resp.Header.Values("Content-Encoding"))
	isNullBody := method == http.MethodHead ||
		resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusResetContent ||
		resp.StatusCode == http.StatusNotModified ||
		(resp.StatusCode >= 100 && resp.StatusCode < 200)
	isPartial := resp.StatusCode == http.StatusPartialContent || resp.Header.Get("Content-Range") != ""
	noTransform := hasNoTransformCacheControl(resp.Header.Values("Cache-Control"))

	// Native browser DecompressionStream('gzip') pipeline (zero double-transcoding, zero strong ETag loss!):
	// 1) Passthrough ONLY if Content-Encoding is a single exact "gzip" token and body is non-empty!
	// 2) Compress ONLY if uncompressed, non-empty, non-partial, no `Cache-Control: no-transform`, compressible MIME!
	passthroughGzip := !isNullBody && !isPartial && clientAcceptsGzip &&
		resp.ContentLength != 0 && len(ceTokens) == 1 && ceTokens[0] == "gzip"
	compressGzip := !isNullBody && !isPartial && !noTransform && clientAcceptsGzip &&
		resp.ContentLength != 0 && len(ceTokens) == 0 &&
		ShouldCompressContentType(resp.Header.Get("Content-Type"), resp.ContentLength)
	swDecompressGzip := passthroughGzip || compressGzip

	dropSet := StripResponseHopByHop(resp.Header)
	var outFields []protocol.HeaderField
	for k, vals := range resp.Header {
		lk := strings.ToLower(k)
		if dropSet[lk] || lk == "set-cookie" {
			continue
		}
		if swDecompressGzip && (lk == "content-encoding" || lk == "content-length") {
			continue
		}
		for _, v := range vals {
			if lk == "location" {
				v = p.policy.RewriteLocationHeader(v, dest)
			}
			// Preserve strong ETag! Gateway gzip is transparent hop-by-hop transfer compression;
			// SW decompresses back to exact original representation bytes before returning to the app.
			outFields = append(outFields, protocol.HeaderField{
				Name:  lk,
				Value: protocol.SanitizeHeaderValue(v),
			})
		}
	}
	var respFlags uint8
	if swDecompressGzip {
		respFlags |= protocol.RespFlagDecompressGzip
	}
	statusCode := uint16(http.StatusBadGateway)
	if resp.StatusCode >= 100 && resp.StatusCode <= 599 {
		statusCode = uint16(resp.StatusCode)
	}
	respTLV, err := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{
		Status:       statusCode,
		RespFlags:    respFlags,
		Headers:      outFields,
		CookieDeltas: cookieDeltasToEntries(cookieDeltas),
	})
	if err != nil {
		stream.Reset(protocol.ErrCodeFrameSizeError)
		return
	}

	if isNullBody {
		_ = stream.WriteHeaders(respTLV, true)
		return
	}
	if err := stream.WriteHeaders(respTLV, false); err != nil {
		stream.Reset(protocol.ErrCodeInternalError)
		return
	}

	var dst io.Writer = stream
	var gzW *StreamingGzipWriter
	if compressGzip {
		gzW = NewStreamingGzipWriter(stream)
		dst = gzW
	}

	copyBuf := make([]byte, 32*1024)
	_, copyErr := io.CopyBuffer(dst, resp.Body, copyBuf)
	if copyErr != nil {
		if gzW != nil {
			gzW.Abort()
		}
		stream.Reset(protocol.ErrCodeInternalError)
		return
	}
	if gzW != nil {
		if err := gzW.Close(); err != nil {
			stream.Reset(protocol.ErrCodeInternalError)
			return
		}
	}
}

func (p *HTTPProxy) writeSimpleError(stream *mux.Stream, status uint16, msg string) {
	body := []byte(msg + "\n")
	respTLV, err := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{
		Status: status,
		Headers: []protocol.HeaderField{
			{Name: "content-type", Value: "text/plain; charset=utf-8"},
			{Name: "content-length", Value: fmt.Sprintf("%d", len(body))},
			{Name: "cache-control", Value: "no-store"},
		},
	})
	if err != nil {
		stream.Reset(protocol.ErrCodeInternalError)
		return
	}
	if err := stream.WriteHeaders(respTLV, false); err != nil {
		return
	}
	_, _ = stream.Write(body)
}

func parseStrictContentLength(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, errors.New("empty Content-Length")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("non-digit Content-Length")
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("overflow Content-Length")
	}
	return n, nil
}

func parseContentEncodingTokens(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, tok := range strings.Split(v, ",") {
			if t := strings.ToLower(strings.TrimSpace(tok)); t != "" && t != "identity" {
				out = append(out, t)
			}
		}
	}
	return out
}

func hasNoTransformCacheControl(vals []string) bool {
	for _, v := range vals {
		for _, dir := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(dir), "no-transform") {
				return true
			}
		}
	}
	return false
}

func isLongLivedStreamMIME(ct string) bool {
	switch ct {
	case "text/event-stream", "application/x-ndjson", "multipart/x-mixed-replace":
		return true
	default:
		return false
	}
}
