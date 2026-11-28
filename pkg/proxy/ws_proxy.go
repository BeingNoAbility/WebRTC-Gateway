package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
)

type wsRawConnCapture struct {
	mu   sync.Mutex
	conn net.Conn
}

func (c *wsRawConnCapture) set(nc net.Conn) {
	c.mu.Lock()
	c.conn = nc
	c.mu.Unlock()
}

func (c *wsRawConnCapture) forceClose() {
	c.mu.Lock()
	nc := c.conn
	c.mu.Unlock()
	if nc != nil {
		_ = nc.SetDeadline(time.Now())
		_ = nc.Close()
	}
}

type wsDialCaptureKey struct{}

func withWSRawConnCapture(ctx context.Context, cap *wsRawConnCapture) context.Context {
	return context.WithValue(ctx, wsDialCaptureKey{}, cap)
}

const (
	MaxWSFramePayloadBytes = protocol.MaxWSMessageBytes // 16 MiB
	wsStreamChunkSize      = 16 * 1024                  // O(1) 16 KiB streaming chunk size
	wsDialTimeout          = 15 * time.Second
	wsBackendWriteTimeout  = 30 * time.Second
)

type WSProxyOptions struct {
	Policy             *DestinationPolicy
	InsecureSkipVerify bool
	SNI                string
	ProxyURL           *url.URL
}

// WSProxy bridges dedicated "p2pws" WebSocket DataChannel streams to the Agent's single
// configured WS backend using true O(1)-memory 16 KiB streaming WS_TEXT/WS_BINARY/WS_CONT/WS_CLOSE sub-frames.
type WSProxy struct {
	policy *DestinationPolicy
	client *http.Client
}

func NewWSProxy(opts WSProxyOptions) (*WSProxy, error) {
	if opts.Policy == nil {
		return nil, errors.New("proxy/ws: DestinationPolicy is required")
	}
	// NEVER follow HTTP redirects automatically during WebSocket dial!
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	targetTLS := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: opts.InsecureSkipVerify, // #nosec G402 -- Fail-Closed default false; explicitly opt-in via -insecure-target for self-signed intranet upstreams
	}
	if sni := opts.Policy.DefaultTLSServerName(opts.SNI); sni != "" {
		targetTLS.ServerName = sni
	}
	baseDial := newTargetDialContext()
	targetTr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			nc, err := baseDial(ctx, network, addr)
			if err == nil {
				if cap, ok := ctx.Value(wsDialCaptureKey{}).(*wsRawConnCapture); ok && cap != nil {
					cap.set(nc)
				}
			}
			return nc, err
		},
		TLSClientConfig:     targetTLS,
		TLSHandshakeTimeout: defaultTLSHandshakeTimeout,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
	}
	if opts.ProxyURL != nil {
		targetTr.Proxy = http.ProxyURL(opts.ProxyURL)
	}

	return &WSProxy{
		policy: opts.Policy,
		client: &http.Client{Transport: targetTr, CheckRedirect: noRedirect},
	}, nil
}

func (wp *WSProxy) HandleStream(
	stream *mux.Stream,
	reqHdr *protocol.RequestHeaders,
	clientIP string,
	jar *ProxyCookieJar,
	capScope *CapabilityScope,
) {
	// Step 1: Resolve destination first, then enforce capability scope against canonical path.
	dest, err := wp.policy.Resolve(reqHdr.Target, true)
	if err != nil {
		wp.writeError(stream, http.StatusForbidden)
		_ = stream.Close()
		return
	}
	if !capScope.AllowsDestination(protocol.StreamKindWS, reqHdr.Method, dest) {
		wp.writeError(stream, http.StatusForbidden)
		_ = stream.Close()
		return
	}

	credentialsOmit := (reqHdr.FetchCtx & protocol.FetchCtxCredentialsOmit) != 0
	if jar != nil && !credentialsOmit && len(reqHdr.CookieMutations) > 0 {
		jar.ApplyClientMutations(wp.policy.CookieURL(dest.URL), cookieEntriesToMutations(reqHdr.CookieMutations))
	}
	readonly := capScope != nil && capScope.Readonly
	upstreamHeaders := wp.policy.SanitizeRequestHeaders(
		reqHdr.Headers, reqHdr.FetchCtx, http.MethodGet, dest, clientIP, jar, true, readonly,
	)
	// Same-origin WS upgrades default to injecting Origin: EffectiveOrigin() when absent
	// (fixes backends like Code-Server / Jupyter / Grafana that require an Origin header on WS upgrades).
	if (reqHdr.FetchCtx&protocol.FetchCtxSameOriginClient) != 0 && upstreamHeaders.Get("Origin") == "" {
		upstreamHeaders.Set("Origin", wp.policy.EffectiveOrigin())
	}

	// Preserve ALL Sec-WebSocket-Protocol values across multiple header lines.
	var subprotocols []string
	for _, protoHdr := range upstreamHeaders.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(protoHdr, ",") {
			if t := strings.TrimSpace(p); t != "" {
				subprotocols = append(subprotocols, t)
			}
		}
	}
	upstreamHeaders.Del("Sec-WebSocket-Protocol")
	upstreamHeaders.Del("Sec-WebSocket-Extensions")

	rawCapture := &wsRawConnCapture{}
	dialCtx, dialCancel := context.WithTimeout(withWSRawConnCapture(stream.Context(), rawCapture), wsDialTimeout)
	conn, resp, err := websocket.Dial(dialCtx, dest.URL.String(), &websocket.DialOptions{
		HTTPClient:   wp.client,
		HTTPHeader:   upstreamHeaders,
		Host:         wp.policy.EffectiveHost(),
		Subprotocols: subprotocols,
	})
	dialCancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		status := uint16(http.StatusBadGateway)
		if resp != nil && resp.StatusCode >= 300 && resp.StatusCode <= 599 {
			status = uint16(resp.StatusCode)
		}
		wp.writeError(stream, status)
		_ = stream.Close()
		return
	}
	conn.SetReadLimit(MaxWSFramePayloadBytes)

	// Mark long-lived only after 101 Switching Protocols succeeds so failed upgrades never leak slots.
	stream.SetLongLived(true)

	var respFields []protocol.HeaderField
	var wsDeltas []CookieDelta
	if resp != nil && !credentialsOmit {
		if setCookies := resp.Header.Values("Set-Cookie"); len(setCookies) > 0 && jar != nil {
			wsDeltas = jar.SaveResponseCookies(wp.policy.CookieURL(dest.URL), setCookies)
		}
	}
	if sp := conn.Subprotocol(); sp != "" {
		respFields = append(respFields, protocol.HeaderField{Name: "sec-websocket-protocol", Value: sp})
	}
	respTLV, err := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{
		Status:       http.StatusSwitchingProtocols,
		Headers:      respFields,
		CookieDeltas: cookieDeltasToEntries(wsDeltas),
	})
	if err != nil || stream.WriteHeaders(respTLV, false) != nil {
		_ = conn.CloseNow()
		stream.Reset(protocol.ErrCodeInternalError)
		return
	}

	pumpCtx, pumpCancel := context.WithCancel(stream.Context())
	defer pumpCancel()

	var closeOnce sync.Once
	var wsCloseSentOnce sync.Once
	sendWSClose := func(code uint16, reason string) {
		wsCloseSentOnce.Do(func() {
			_ = stream.WriteWSClose(code, reason)
		})
	}
	var backendClosedClean atomic.Bool
	closeBackend := func(code websocket.StatusCode, reason string) bool {
		closeOnce.Do(func() {
			reason = sanitizeUTF8CloseReason(reason, 120)
			// Cancel pumpCtx so Goroutine A releases coder/websocket's internal readMu, then attempt
			// a strictly bounded 1s RFC 6455 Close handshake. If the backend stalls, force-close the
			// underlying net.Conn directly (bypassing coder/websocket's casClosing CloseNow stall, H-10b).
			done := make(chan error, 1)
			go func() {
				done <- conn.Close(code, reason)
			}()
			timer := time.NewTimer(1 * time.Second)
			select {
			case err := <-done:
				timer.Stop()
				if err == nil {
					backendClosedClean.Store(true)
				} else {
					rawCapture.forceClose()
					_ = conn.CloseNow()
				}
			case <-timer.C:
				pumpCancel()
				rawCapture.forceClose()
				_ = conn.CloseNow()
			}
			pumpCancel()
		})
		return backendClosedClean.Load()
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine A: Backend WS -> Dedicated "p2pws" DC (true O(1)-memory 16 KiB streaming with 1-chunk lookahead!).
	go func() {
		defer wg.Done()
		defer pumpCancel()
		bufA := make([]byte, wsStreamChunkSize)
		bufB := make([]byte, wsStreamChunkSize)

		handleBackendReadErr := func(err error) {
			if isWSMessageTooBigErr(err) {
				closeBackend(websocket.StatusMessageTooBig, "frame exceeds 16 MiB")
				sendWSClose(uint16(websocket.StatusMessageTooBig), "frame exceeds 16 MiB")
				return
			}
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) {
				sendWSClose(validWSCloseCode(int(closeErr.Code)), closeErr.Reason)
			}
		}

		for {
			msgType, reader, rerr := conn.Reader(pumpCtx)
			if rerr != nil {
				handleBackendReadErr(rerr)
				return
			}

			firstOp := protocol.TypeWSText
			if msgType != websocket.MessageText {
				firstOp = protocol.TypeWSBinary
			}

			curOp := firstOp
			curBuf, nextBuf := bufA, bufB
			totalBytes := 0

			nCur, errCur := readChunkNonEmptyOrEOF(reader, curBuf)
			if errCur != nil && errCur != io.EOF {
				handleBackendReadErr(errCur)
				return
			}
			totalBytes += nCur
			if totalBytes > MaxWSFramePayloadBytes {
				closeBackend(websocket.StatusMessageTooBig, "frame exceeds 16 MiB")
				sendWSClose(uint16(websocket.StatusMessageTooBig), "frame exceeds 16 MiB")
				return
			}
			// Empty 0-byte WS message (e.g. `ws.send("")`) -> single final frame.
			if nCur == 0 && errCur == io.EOF {
				if werr := stream.WriteWSChunk(pumpCtx, firstOp, true, nil); werr != nil {
					return
				}
				continue
			}

			for {
				if errCur == io.EOF {
					if werr := stream.WriteWSChunk(pumpCtx, curOp, true, curBuf[:nCur]); werr != nil {
						return
					}
					break
				}
				// Peek next chunk to determine exact FlagMsgFin without ever emitting 0B non-final WS_CONT.
				nNext, errNext := readChunkNonEmptyOrEOF(reader, nextBuf)
				if errNext != nil && errNext != io.EOF {
					handleBackendReadErr(errNext)
					return
				}
				if nNext == 0 && errNext == io.EOF {
					if werr := stream.WriteWSChunk(pumpCtx, curOp, true, curBuf[:nCur]); werr != nil {
						return
					}
					break
				}
				totalBytes += nNext
				if totalBytes > MaxWSFramePayloadBytes {
					closeBackend(websocket.StatusMessageTooBig, "frame exceeds 16 MiB")
					sendWSClose(uint16(websocket.StatusMessageTooBig), "frame exceeds 16 MiB")
					return
				}
				if werr := stream.WriteWSChunk(pumpCtx, curOp, false, curBuf[:nCur]); werr != nil {
					return
				}
				curBuf, nextBuf = nextBuf, curBuf
				nCur, errCur = nNext, errNext
			}
		}
	}()

	// Goroutine B: Dedicated "p2pws" DC -> Backend WS (commits writer.Close ONLY on FlagMsgFin!).
	// Enforces per-chunk progress deadlines instead of one fixed timeout across an entire 16 MiB message.
	go func() {
		defer wg.Done()
		defer pumpCancel()
		var curWriter io.WriteCloser
		var curCancel context.CancelFunc
		var curBytes int
		abortWriter := func() {
			if curCancel != nil {
				curCancel()
				curCancel = nil
			}
			// H-10a: NEVER call curWriter.Close() on abort — coder/websocket can acquire the write lock
			// on a cancelled context and flush a truncated FIN=1 frame to the backend! Force-close raw socket instead.
			if curWriter != nil {
				curWriter = nil
				rawCapture.forceClose()
			}
			curBytes = 0
		}
		defer abortWriter()

		withWriteDeadline := func(cancel context.CancelFunc, fn func() error) error {
			t := time.AfterFunc(wsBackendWriteTimeout, cancel)
			err := fn()
			t.Stop()
			return err
		}

		for {
			readCtx := pumpCtx
			var readCancel context.CancelFunc
			if curWriter != nil {
				readCtx, readCancel = context.WithTimeout(pumpCtx, wsBackendWriteTimeout)
			}
			chunk, rerr := stream.ReadWSChunk(readCtx)
			if readCancel != nil {
				readCancel()
			}
			if rerr != nil {
				return
			}
			if chunk.Op == protocol.TypeWSClose {
				if curWriter != nil {
					abortWriter()
				}
				code, reason, derr := protocol.DecodeWSClosePayload(chunk.Payload)
				if derr != nil {
					code = uint16(websocket.StatusNormalClosure)
					reason = ""
				}
				validCode := validWSCloseCode(int(code))
				if closeBackend(websocket.StatusCode(validCode), reason) {
					// Echo RFC 6455 peer Close ack back to browser so CloseEvent.wasClean === true immediately!
					sendWSClose(validCode, reason)
				}
				return
			}

			if chunk.Op == protocol.TypeWSText || chunk.Op == protocol.TypeWSBinary {
				if curWriter == nil {
					wsType := websocket.MessageBinary
					if chunk.Op == protocol.TypeWSText {
						wsType = websocket.MessageText
					}
					wctx, wcancel := context.WithCancel(pumpCtx)
					var w io.WriteCloser
					werr := withWriteDeadline(wcancel, func() error {
						var err error
						w, err = conn.Writer(wctx, wsType)
						return err
					})
					if werr != nil {
						wcancel()
						return
					}
					curWriter = w
					curCancel = wcancel
					curBytes = 0
				}
			} else {
				closeBackend(websocket.StatusUnsupportedData, "unknown ws opcode")
				sendWSClose(uint16(websocket.StatusUnsupportedData), "unknown ws opcode")
				return
			}

			curBytes += len(chunk.Payload)
			if curBytes > MaxWSFramePayloadBytes {
				abortWriter()
				closeBackend(websocket.StatusMessageTooBig, "client frame exceeds 16 MiB")
				sendWSClose(uint16(websocket.StatusMessageTooBig), "client frame exceeds 16 MiB")
				return
			}
			if len(chunk.Payload) > 0 {
				werr := withWriteDeadline(curCancel, func() error {
					_, err := curWriter.Write(chunk.Payload)
					return err
				})
				if werr != nil {
					abortWriter()
					return
				}
			}
			// ONLY call writer.Close() when chunk.Fin (FlagMsgFin) is explicitly set!
			if chunk.Fin {
				clErr := withWriteDeadline(curCancel, func() error {
					return curWriter.Close()
				})
				curCancel()
				curWriter = nil
				curCancel = nil
				curBytes = 0
				if clErr != nil {
					return
				}
			}
		}
	}()

	wg.Wait()
	closeBackend(websocket.StatusNormalClosure, "")
	_ = stream.Close()
}

func readChunkNonEmptyOrEOF(r io.Reader, buf []byte) (int, error) {
	for {
		n, err := r.Read(buf)
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func isWSMessageTooBigErr(err error) bool {
	if err == nil {
		return false
	}
	if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "read limited")
}

func (wp *WSProxy) writeError(stream *mux.Stream, status uint16) {
	respTLV, err := protocol.EncodeResponseHeaders(&protocol.ResponseHeaders{
		Status: status,
		Headers: []protocol.HeaderField{
			{Name: "cache-control", Value: "no-store"},
		},
	})
	if err == nil {
		_ = stream.WriteHeaders(respTLV, true)
	} else {
		stream.Reset(protocol.ErrCodeInternalError)
	}
}

// validWSCloseCode preserves ALL RFC 6455 §7.4.1 wire-valid status codes (1000..1003, 1007..1014,
// 3000..4999) while strictly blocking wire-forbidden pseudo-codes (1004, 1005, 1006, 1015).
func validWSCloseCode(code int) uint16 {
	if code < 1000 || code > 4999 {
		return uint16(websocket.StatusNormalClosure)
	}
	if code <= 1003 || (code >= 1007 && code <= 1014) || code >= 3000 {
		return uint16(code)
	}
	return uint16(websocket.StatusNormalClosure)
}

func sanitizeUTF8CloseReason(s string, maxBytes int) string {
	if !utf8.ValidString(s) {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
