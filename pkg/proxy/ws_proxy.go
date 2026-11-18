package proxy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"webrtc-gateway/pkg/mux"
)

const (
	// maxWSMessageSize limits incoming WebSocket frame payload to 32MB to prevent memory exhaustion
	maxWSMessageSize = 32 * 1024 * 1024
)

// WSConnectMetadata represents the initial handshake for a WebSocket proxy stream.
type WSConnectMetadata struct {
	Path      string            `json:"p"`                   // Target WebSocket path & query
	Protocols []string          `json:"protocols,omitempty"` // Requested subprotocols
	Headers   map[string]string `json:"headers,omitempty"`   // Extra headers
	H         map[string]string `json:"h,omitempty"`         // Compact extra headers shorthand
}

// WSProxy handles bidirectional relay between WebRTC streams and local WebSocket services.
type WSProxy struct {
	targetBase     *url.URL
	rootDomain     string
	allowedDomains []string
	cookieManager  *SessionCookieManager
	insecureTLS    bool
	secureDialer   *websocket.Dialer
	insecureDialer *websocket.Dialer
}

// NewWSProxy creates a new WSProxy targeting the given base URL.
func NewWSProxy(targetBaseURL string, cm *SessionCookieManager) (*WSProxy, error) {
	u, err := url.Parse(targetBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid target base URL: %w", err)
	}

	wsScheme := "ws"
	if u.Scheme == "https" || u.Scheme == "wss" {
		wsScheme = "wss"
	}
	u.Scheme = wsScheme

	createDialer := func(insecure bool) *websocket.Dialer {
		return &websocket.Dialer{
			Proxy:            nil, // Cut ambient environment proxy leakage
			HandshakeTimeout: 10 * time.Second,
			TLSClientConfig:  &tls.Config{InsecureSkipVerify: insecure},
			NetDialContext:   CreateSafeDialContext(u),
		}
	}

	return &WSProxy{
		targetBase:     u,
		rootDomain:     ExtractRootDomain(u.Hostname()),
		cookieManager:  cm,
		insecureTLS:    false,
		secureDialer:   createDialer(false),
		insecureDialer: createDialer(true),
	}, nil
}

// SetInsecureTLS enables or disables skipping TLS certificate verification for the target host.
func (wp *WSProxy) SetInsecureTLS(insecure bool) {
	wp.insecureTLS = insecure
}

// SetProxy configures an outbound HTTP/HTTPS/SOCKS5 proxy for WebSocket backend requests.
func (wp *WSProxy) SetProxy(proxyURL string) {
	var proxyFunc func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		if pu, err := url.Parse(proxyURL); err == nil {
			proxyFunc = http.ProxyURL(pu)
		}
	}
	if wp.secureDialer != nil {
		wp.secureDialer.Proxy = proxyFunc
	}
	if wp.insecureDialer != nil {
		wp.insecureDialer.Proxy = proxyFunc
	}
}

// SetAllowedDomains configures explicit domain patterns (e.g. "*.hdslb.com") allowed for cross-origin proxying.
func (wp *WSProxy) SetAllowedDomains(domains []string) {
	wp.allowedDomains = nil
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			wp.allowedDomains = append(wp.allowedDomains, d)
		}
	}
}

func (wp *WSProxy) isDomainAllowed(hostOnly string) bool {
	hostOnly = strings.ToLower(strings.TrimSpace(hostOnly))
	if hostOnly == "" {
		return false
	}
	targetHost := strings.ToLower(wp.targetBase.Hostname())

	if strings.EqualFold(hostOnly, targetHost) {
		return true
	}

	isTargetIP := net.ParseIP(targetHost) != nil || strings.EqualFold(targetHost, "localhost")
	if !isTargetIP && wp.rootDomain != "" && (hostOnly == wp.rootDomain || strings.HasSuffix(hostOnly, "."+wp.rootDomain)) {
		return true
	}

	for _, pattern := range wp.allowedDomains {
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:]
			if strings.HasSuffix(hostOnly, suffix) || hostOnly == pattern[2:] {
				return true
			}
		} else if strings.EqualFold(hostOnly, pattern) {
			return true
		}
	}
	return false
}

func (wp *WSProxy) resolveTargetWSURL(reqPath string, headers map[string]string) (*url.URL, string, error) {
	cleanPath := reqPath
	// RFC 3986 specifies that a relative URI starting with "//" begins an Authority (Host).
	// Standard web servers and proxies (e.g. Nginx merge_slashes on;) normalize leading consecutive slashes
	// into a single slash to prevent parser differential from swallowing the first path segment.
	for strings.HasPrefix(cleanPath, "//") {
		cleanPath = cleanPath[1:]
	}
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}

	var destHost string
	var subPath string

	const proxyPrefix = "/--p2p-proxy/"
	if strings.HasPrefix(cleanPath, proxyPrefix) {
		remainder := cleanPath[len(proxyPrefix):]
		slashIdx := strings.Index(remainder, "/")
		if slashIdx != -1 {
			destHost = remainder[:slashIdx]
			subPath = remainder[slashIdx:]
		} else {
			destHost = remainder
			subPath = "/"
		}
	} else if dh, ok := headers["x-p2p-dest-host"]; ok && dh != "" {
		destHost = dh
		subPath = cleanPath
	}

	if destHost != "" {
		// Strict input validation: prohibit @, \, /, and whitespace to prevent SSRF authority confusion
		if strings.ContainsAny(destHost, "@\\/ \t\r\n") {
			return nil, "", fmt.Errorf("SSRF blocked: invalid characters in target host %q", destHost)
		}

		parsedURL, err := url.Parse("http://" + destHost)
		if err != nil || parsedURL.User != nil || parsedURL.Hostname() == "" {
			return nil, "", fmt.Errorf("SSRF blocked: malformed authority in target host %q", destHost)
		}

		hostOnly := strings.ToLower(parsedURL.Hostname())

		// SSRF Defense: If destHost is a raw IP, reject if it is a private/restricted IP (unless it is the target backend itself)
		if parsedIP := net.ParseIP(hostOnly); parsedIP != nil {
			if IsRestrictedIP(parsedIP) && !IsTargetHost(hostOnly, "", wp.targetBase) {
				return nil, "", fmt.Errorf("SSRF blocked: proxy access to restricted/private IP %s is forbidden", hostOnly)
			}
		}

		isAllowed := wp.isDomainAllowed(hostOnly)

		if isAllowed {
			scheme := wp.targetBase.Scheme
			if scheme == "" {
				scheme = "wss"
			}
			fullURL := scheme + "://" + destHost + subPath
			u, err := url.Parse(fullURL)
			if err != nil {
				return nil, "", err
			}
			return u, destHost, nil
		}
		return nil, "", fmt.Errorf("cross-origin proxy access to %s is forbidden (not in allowed-proxy-domains whitelist)", destHost)
	}

	basePath := strings.TrimRight(wp.targetBase.Path, "/")
	var finalPath string
	if basePath != "" {
		if cleanPath == basePath || strings.HasPrefix(cleanPath, basePath+"/") {
			finalPath = cleanPath
		} else {
			finalPath = basePath + cleanPath
		}
	} else {
		finalPath = cleanPath
	}

	u, err := url.Parse(finalPath)
	if err != nil {
		return nil, "", err
	}

	dest := *wp.targetBase
	dest.Path = u.Path
	dest.RawPath = u.RawPath
	dest.RawQuery = u.RawQuery
	return &dest, wp.targetBase.Host, nil
}

// HandleStream handles a WebSocket proxy stream.
func (wp *WSProxy) HandleStream(sessionID, clientIP string, stream *mux.Stream) {
	ctx, cancel := context.WithTimeout(stream.Context(), 10*time.Second)
	headersPayload, err := stream.ReadHeaders(ctx)
	cancel()

	if err != nil {
		log.Printf("[WSProxy] Stream %d failed to read WS handshake: %v", stream.ID(), err)
		_ = stream.Close()
		return
	}

	var meta WSConnectMetadata
	if err := json.Unmarshal(headersPayload, &meta); err != nil {
		log.Printf("[WSProxy] Stream %d invalid WS metadata: %v", stream.ID(), err)
		_ = stream.Close()
		return
	}

	wp.HandleStreamWithMeta(sessionID, clientIP, stream, &meta)
}

// HandleStreamWithMeta handles a WebSocket proxy stream directly with pre-parsed metadata.
func (wp *WSProxy) HandleStreamWithMeta(sessionID, clientIP string, stream *mux.Stream, meta *WSConnectMetadata) {
	defer stream.Close()

	if meta == nil {
		log.Printf("[WSProxy] Stream %d missing WS metadata", stream.ID())
		return
	}
	if meta.Headers == nil && meta.H != nil {
		meta.Headers = meta.H
	}

	// 2. Build local WS URL
	targetWSURL, targetHost, err := wp.resolveTargetWSURL(meta.Path, meta.Headers)
	if err != nil {
		log.Printf("[WSProxy] Stream %d invalid target WS URL: %v", stream.ID(), err)
		return
	}

	// 3. Prepare dial headers
	dialHeaders := make(http.Header)
	for k, v := range meta.Headers {
		lk := strings.ToLower(k)
		if lk == "upgrade" || lk == "connection" || strings.HasPrefix(lk, "sec-websocket-") {
			continue
		}
		dialHeaders.Set(k, v)
	}
	dialHeaders.Set("Host", targetHost)
	finalClientIP := strings.TrimSpace(clientIP)
	if finalClientIP == "" || finalClientIP == "unknown" {
		finalClientIP = "127.0.0.1"
	}
	dialHeaders.Set("X-Forwarded-For", finalClientIP)
	dialHeaders.Set("X-Real-IP", finalClientIP)
	wsProto := "ws"
	if wp.targetBase.Scheme == "https" || wp.targetBase.Scheme == "wss" {
		wsProto = "wss"
	}
	dialHeaders.Set("X-Forwarded-Proto", wsProto)
	dialHeaders.Set("Referer", wp.targetBase.Scheme+"://"+wp.targetBase.Host+"/")

	// Prepare dialer with subprotocols if specified (strict TLS unless -insecure targets backend)
	baseDialer := wp.secureDialer
	if wp.insecureTLS && IsTargetHost(targetWSURL.Hostname(), targetWSURL.Port(), wp.targetBase) {
		baseDialer = wp.insecureDialer
	}
	dialer := *baseDialer
	if len(meta.Protocols) > 0 {
		dialer.Subprotocols = meta.Protocols
	}

	// Inject cookies
	dummyReq := &http.Request{URL: targetWSURL, Header: dialHeaders}
	wp.cookieManager.InjectCookies(sessionID, dummyReq)

	// 4. Dial local WebSocket service
	conn, resp, err := dialer.DialContext(stream.Context(), targetWSURL.String(), dialHeaders)
	if err != nil {
		log.Printf("[WSProxy] Stream %d dial local WS failed: %v", stream.ID(), err)
		errResp := ResponseMetadata{
			Status: http.StatusBadGateway,
			Msg:    fmt.Sprintf("Failed to connect to local WebSocket: %v", err),
		}
		raw, _ := json.Marshal(errResp)
		_ = stream.WriteHeaders(raw)
		return
	}
	defer conn.Close()

	if resp != nil {
		wp.cookieManager.SaveCookies(sessionID, targetWSURL, resp.Header)
	}

	// 5. Send 101 Switching Protocols confirmation back to browser
	confirmMeta := ResponseMetadata{
		Status: http.StatusSwitchingProtocols,
		Msg:    "Switching Protocols",
		Headers: map[string]string{
			"upgrade":    "websocket",
			"connection": "Upgrade",
		},
	}
	confirmRaw, _ := json.Marshal(confirmMeta)
	if err := stream.WriteHeaders(confirmRaw); err != nil {
		return
	}

	log.Printf("[WSProxy] Stream %d <-> Local WS [%s] bidirectional tunnel established", stream.ID(), targetWSURL)

	errCh := make(chan error, 2)

	// Goroutine A: Local WS -> WebRTC Stream
	go func() {
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				errCh <- err
				return
			}

			// Format packet: [Opcode (1 byte)] + [Length (4 bytes)] + [Payload]
			// Opcode: 1 = Text, 2 = Binary
			packet := make([]byte, 5+len(data))
			if msgType == websocket.TextMessage {
				packet[0] = 0x01
			} else {
				packet[0] = 0x02
			}
			binary.BigEndian.PutUint32(packet[1:5], uint32(len(data)))
			copy(packet[5:], data)

			if _, err := stream.Write(packet); err != nil {
				errCh <- err
				return
			}
		}
	}()

	// Goroutine B: WebRTC Stream -> Local WS
	go func() {
		hdrBuf := make([]byte, 5)
		for {
			// Read 5-byte packet header: [Opcode (1 byte)] + [Length (4 bytes)]
			if _, err := io.ReadFull(stream, hdrBuf); err != nil {
				errCh <- err
				return
			}

			opcode := hdrBuf[0]
			length := binary.BigEndian.Uint32(hdrBuf[1:5])

			if length > maxWSMessageSize {
				errCh <- fmt.Errorf("websocket frame payload exceeds maximum size limit (%d > %d)", length, maxWSMessageSize)
				return
			}

			payload := make([]byte, length)
			if length > 0 {
				if _, err := io.ReadFull(stream, payload); err != nil {
					errCh <- err
					return
				}
			}

			var wsType int
			if opcode == 0x01 {
				wsType = websocket.TextMessage
			} else {
				wsType = websocket.BinaryMessage
			}

			if err := conn.WriteMessage(wsType, payload); err != nil {
				errCh <- err
				return
			}
		}
	}()

	select {
	case <-stream.Context().Done():
	case <-errCh:
	}

	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	log.Printf("[WSProxy] Stream %d closed", stream.ID())
}
