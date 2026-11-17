package proxy

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"webrtc-gateway/pkg/mux"
	"webrtc-gateway/pkg/protocol"
)

var chunkBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, protocol.RecommendedChunkSize)
		return &b
	},
}

// RequestMetadata represents serialized HTTP request metadata from the browser Service Worker.
type RequestMetadata struct {
	Method  string            `json:"m"` // HTTP Method (GET, POST, etc.)
	Path    string            `json:"p"` // Path + Query (e.g. /api/list?q=test)
	Headers map[string]string `json:"h"` // Request headers
}

// ResponseMetadata represents serialized HTTP response metadata returned to the browser.
type ResponseMetadata struct {
	Status  int               `json:"s"`           // HTTP Status Code (200, 404, etc.)
	Msg     string            `json:"m,omitempty"` // Status text
	Headers map[string]string `json:"h"`           // Response headers
	Cookies []string          `json:"c,omitempty"` // Client-syncable non-HttpOnly Set-Cookie list
}

// ExtractRootDomain extracts the effective root domain from a hostname.
// For example:
//
//	"www.example.com" -> "example.com"
//	"api.example.com" -> "example.com"
//	"sub.example.com:8443" -> "example.com"
//	"192.168.1.100:8080" -> "192.168.1.100"
//	"localhost:8080" -> "localhost"
func ExtractRootDomain(hostname string) string {
	host := strings.ToLower(strings.Split(hostname, ":")[0])
	if net.ParseIP(host) != nil || host == "localhost" {
		return host
	}
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	last := parts[len(parts)-1]
	secondLast := parts[len(parts)-2]
	if len(last) == 2 && (secondLast == "co" || secondLast == "com" || secondLast == "net" || secondLast == "org" || secondLast == "edu" || secondLast == "gov") {
		if len(parts) >= 3 {
			return strings.Join(parts[len(parts)-3:], ".")
		}
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// HTTPProxy bridges multiplexed P2P streams with local HTTP backend services.
type HTTPProxy struct {
	targetURL         *url.URL
	rootDomain        string
	allowedDomains    []string
	cookieManager     *SessionCookieManager
	insecureTLS       bool
	secureTransport   *http.Transport
	insecureTransport *http.Transport
	secureClient      *http.Client
	insecureClient    *http.Client
}

// NewHTTPProxy creates a new HTTPProxy targeting targetURLString.
func NewHTTPProxy(targetURLString string, cm *SessionCookieManager) (*HTTPProxy, error) {
	u, err := url.Parse(targetURLString)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	createTransport := func(insecure bool) *http.Transport {
		return &http.Transport{
			Proxy:               nil, // Cut ambient environment proxy leakage
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true, // Decompression handled explicitly
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: insecure},
			DialContext:         CreateSafeDialContext(u),
		}
	}

	secureTransport := createTransport(false)
	insecureTransport := createTransport(true)

	checkRedirect := func(req *http.Request, via []*http.Request) error {
		// Don't follow redirects automatically; let the browser receive 3xx
		return http.ErrUseLastResponse
	}

	return &HTTPProxy{
		targetURL:         u,
		rootDomain:        ExtractRootDomain(u.Hostname()),
		cookieManager:     cm,
		insecureTLS:       false,
		secureTransport:   secureTransport,
		insecureTransport: insecureTransport,
		secureClient: &http.Client{
			Transport:     secureTransport,
			CheckRedirect: checkRedirect,
			Timeout:       0,
		},
		insecureClient: &http.Client{
			Transport:     insecureTransport,
			CheckRedirect: checkRedirect,
			Timeout:       0,
		},
	}, nil
}

// SetInsecureTLS enables or disables skipping TLS certificate verification for the target host.
func (p *HTTPProxy) SetInsecureTLS(insecure bool) {
	p.insecureTLS = insecure
}

// SetProxy configures an outbound HTTP/HTTPS/SOCKS5 proxy for backend requests.
func (p *HTTPProxy) SetProxy(proxyURL string) {
	var proxyFunc func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		if pu, err := url.Parse(proxyURL); err == nil {
			proxyFunc = http.ProxyURL(pu)
		}
	}
	if p.secureTransport != nil {
		p.secureTransport.Proxy = proxyFunc
	}
	if p.insecureTransport != nil {
		p.insecureTransport.Proxy = proxyFunc
	}
}

// SetAllowedDomains configures explicit domain patterns (e.g. "*.hdslb.com") allowed for cross-origin proxying.
func (p *HTTPProxy) SetAllowedDomains(domains []string) {
	p.allowedDomains = nil
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d != "" {
			p.allowedDomains = append(p.allowedDomains, d)
		}
	}
}

func (p *HTTPProxy) isDomainAllowed(hostOnly string) bool {
	hostOnly = strings.ToLower(strings.TrimSpace(hostOnly))
	if hostOnly == "" {
		return false
	}
	targetHost := strings.ToLower(p.targetURL.Hostname())

	// 1. Same target host
	if strings.EqualFold(hostOnly, targetHost) {
		return true
	}

	// Subdomain matching ONLY permitted if targetHost is a legitimate domain name (NOT an IP address or localhost)
	isTargetIP := net.ParseIP(targetHost) != nil || strings.EqualFold(targetHost, "localhost")
	if !isTargetIP && p.rootDomain != "" && (hostOnly == p.rootDomain || strings.HasSuffix(hostOnly, "."+p.rootDomain)) {
		return true
	}

	// 2. Explicit allowed domain patterns
	for _, pattern := range p.allowedDomains {
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:] // ".hdslb.com"
			if strings.HasSuffix(hostOnly, suffix) || hostOnly == pattern[2:] {
				return true
			}
		} else if strings.EqualFold(hostOnly, pattern) {
			return true
		}
	}
	return false
}

func (p *HTTPProxy) resolveTargetURL(reqPath string, headers map[string]string) (*url.URL, string, error) {
	cleanPath := reqPath
	if !strings.HasPrefix(cleanPath, "/") {
		cleanPath = "/" + cleanPath
	}

	var destHost string
	var subPath string

	// Check if path is prefixed with /--p2p-proxy/<host>/...
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
			if IsRestrictedIP(parsedIP) && !IsTargetHost(hostOnly, "", p.targetURL) {
				return nil, "", fmt.Errorf("SSRF blocked: proxy access to restricted/private IP %s is forbidden", hostOnly)
			}
		}

		isAllowed := p.isDomainAllowed(hostOnly)

		if isAllowed {
			scheme := p.targetURL.Scheme
			if scheme == "" {
				scheme = "https"
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

	// Standard path targeting p.targetURL
	basePath := strings.TrimRight(p.targetURL.Path, "/")
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

	dest := *p.targetURL
	dest.Path = u.Path
	dest.RawPath = u.RawPath
	dest.RawQuery = u.RawQuery
	return &dest, p.targetURL.Host, nil
}

func (p *HTTPProxy) rewriteLocation(loc string, targetHost string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() {
		if strings.EqualFold(u.Host, p.targetURL.Host) {
			return u.RequestURI()
		}
		hostClean := strings.ToLower(u.Hostname())
		if p.isDomainAllowed(hostClean) {
			return "/--p2p-proxy/" + u.Host + u.RequestURI()
		}
		return loc
	}

	// Relative redirect (e.g. /login or /step2)
	// If the request was made to a proxy/sibling host, preserve destination on that host!
	if targetHost != "" && !strings.EqualFold(targetHost, p.targetURL.Host) {
		cleanLoc := loc
		if !strings.HasPrefix(cleanLoc, "/") {
			cleanLoc = "/" + cleanLoc
		}
		return "/--p2p-proxy/" + targetHost + cleanLoc
	}

	return loc
}

// HandleStream proxies an incoming P2PMux stream to the local target web application.
func (p *HTTPProxy) HandleStream(sessionID, clientIP string, stream *mux.Stream) {
	defer stream.Close()

	// 1. Read request headers
	ctx, cancel := context.WithTimeout(stream.Context(), 10*time.Second)
	headersPayload, err := stream.ReadHeaders(ctx)
	cancel()

	if err != nil {
		log.Printf("[Proxy] Stream %d failed to read headers: %v", stream.ID(), err)
		return
	}

	var reqMeta RequestMetadata
	if err := json.Unmarshal(headersPayload, &reqMeta); err != nil {
		log.Printf("[Proxy] Stream %d failed to parse request headers: %v", stream.ID(), err)
		p.sendErrorResponse(stream, http.StatusBadRequest, "Invalid request metadata")
		return
	}

	// 2. Build target URL
	destURL, targetHost, err := p.resolveTargetURL(reqMeta.Path, reqMeta.Headers)
	if err != nil {
		p.sendErrorResponse(stream, http.StatusBadRequest, "Invalid request path")
		return
	}

	// 3. Construct http.Request with exact Content-Length & Body semantics
	var bodyReader io.Reader
	method := strings.ToUpper(reqMeta.Method)
	hasBody := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"

	contentLength := int64(-1)
	if clStr, ok := reqMeta.Headers["content-length"]; ok && clStr != "" {
		if cl, err := strconv.ParseInt(strings.TrimSpace(clStr), 10, 64); err == nil && cl >= 0 {
			contentLength = cl
		}
	}

	if hasBody {
		if contentLength == 0 {
			bodyReader = http.NoBody
		} else if contentLength > 0 {
			bodyReader = io.NopCloser(io.LimitReader(stream, contentLength))
		} else {
			bodyReader = io.NopCloser(stream)
		}
	}

	req, err := http.NewRequestWithContext(stream.Context(), method, destURL.String(), bodyReader)
	if err != nil {
		p.sendErrorResponse(stream, http.StatusInternalServerError, "Failed to create request")
		return
	}

	if contentLength >= 0 {
		req.ContentLength = contentLength
	} else if hasBody {
		req.ContentLength = -1
	}

	// Copy headers
	for k, v := range reqMeta.Headers {
		req.Header.Set(k, v)
	}

	// Strip hop-by-hop headers to prevent HTTP/2 backend protocol violations
	req.Header.Del("Connection")
	req.Header.Del("Keep-Alive")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Transfer-Encoding")
	req.Header.Del("Upgrade")
	req.Header.Del("x-p2p-client-decompress")
	req.Header.Del("x-p2p-dest-host")

	// Standard reverse proxy headers
	req.Host = targetHost
	finalClientIP := strings.TrimSpace(clientIP)
	if finalClientIP == "" || finalClientIP == "unknown" {
		finalClientIP = "127.0.0.1"
	}
	req.Header.Set("X-Forwarded-For", finalClientIP)
	req.Header.Set("X-Real-IP", finalClientIP)
	req.Header.Set("X-Forwarded-Proto", destURL.Scheme)
	if origHost := reqMeta.Headers["host"]; origHost != "" {
		req.Header.Set("X-Forwarded-Host", origHost)
	}

	// Standardize Accept-Encoding to gzip for general text assets, but preserve raw binary streaming for Range/media requests
	if req.Header.Get("Range") != "" || strings.Contains(destURL.Path, ".m4s") || strings.Contains(destURL.Path, "/upgcxcode/") {
		req.Header.Del("Accept-Encoding")
	} else {
		req.Header.Set("Accept-Encoding", "gzip")
	}

	// Normalize Origin to target application to satisfy backend CORS/CSRF validation
	origOrigin := req.Header.Get("Origin")
	if origOrigin != "" && !strings.Contains(origOrigin, p.rootDomain) {
		req.Header.Set("Origin", p.targetURL.Scheme+"://"+p.targetURL.Host)
	}

	// Ensure Referer points to target origin if absent or from portal domain to satisfy backend CSRF checks
	origRef := req.Header.Get("Referer")
	if origRef == "" || (p.rootDomain != "" && !strings.Contains(origRef, p.rootDomain)) {
		refPath := "/"
		if origRef != "" {
			if u, err := url.Parse(origRef); err == nil && u.Path != "" && u.Path != "/" {
				cleanRefPath := u.Path
				if strings.HasPrefix(cleanRefPath, "/--p2p-proxy/") {
					rem := cleanRefPath[len("/--p2p-proxy/"):]
					if idx := strings.Index(rem, "/"); idx != -1 {
						cleanRefPath = rem[idx:]
					} else {
						cleanRefPath = "/"
					}
				}
				refPath = cleanRefPath
				if u.RawQuery != "" {
					refPath += "?" + u.RawQuery
				}
			}
		}
		req.Header.Set("Referer", p.targetURL.Scheme+"://"+p.targetURL.Host+refPath)
	}

	// Inject session cookies from CookieJar
	p.cookieManager.InjectCookies(sessionID, req)

	// If client-side JavaScript sent document.cookie via X-Client-Cookie header, merge them into request
	if clientCookieHeader := req.Header.Get("X-Client-Cookie"); clientCookieHeader != "" {
		req.Header.Del("X-Client-Cookie")
		rawCookies := strings.Split(clientCookieHeader, ";")
		for _, raw := range rawCookies {
			raw = strings.TrimSpace(raw)
			if parts := strings.SplitN(raw, "=", 2); len(parts) == 2 {
				name := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				// Security: Drop any cookie that starts with __p2p_ (virtualized cookie belonging to other nodes)
				if strings.HasPrefix(name, "__p2p_") {
					continue
				}
				if name != "" {
					req.AddCookie(&http.Cookie{
						Name:  name,
						Value: val,
					})
				}
			}
		}
	}

	// 4. Forward to local backend service (with strict TLS validation unless -insecure targets backend)
	client := p.secureClient
	if p.insecureTLS && IsTargetHost(destURL.Hostname(), destURL.Port(), p.targetURL) {
		client = p.insecureClient
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[Proxy] Stream %d request to %s failed: %v", stream.ID(), destURL, err)
		p.sendErrorResponse(stream, http.StatusBadGateway, fmt.Sprintf("Backend service error: %v", err))
		return
	}
	defer resp.Body.Close()

	// Check if client supports browser-side hardware streaming decompression (DecompressionStream)
	clientSupportsDecompress := strings.Contains(strings.ToLower(reqMeta.Headers["x-p2p-client-decompress"]), "gzip")

	// Handle gzip compression from backend:
	// Exclude HEAD requests and bodyless responses (204, 304, Content-Length == 0)
	// from Gzip decompression/passthrough to prevent browser DecompressionStream truncation errors.
	var respBody io.Reader = resp.Body
	isGzip := method != http.MethodHead &&
		resp.StatusCode != http.StatusNoContent &&
		resp.StatusCode != http.StatusNotModified &&
		resp.ContentLength != 0 &&
		strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip")
	gzipPassthrough := false

	if isGzip {
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")

		if clientSupportsDecompress {
			// Client supports hardware streaming decompression; pass through compressed raw gzip bytes
			// over P2P DataChannel to slash bandwidth by 60%~80% and accelerate static assets by 300%+
			gzipPassthrough = true
		} else {
			// Transparent fallback decompression for legacy clients or test suites
			gzReader, err := gzip.NewReader(resp.Body)
			if err == nil {
				defer gzReader.Close()
				respBody = gzReader
			}
		}
	}

	// 5. Store Set-Cookie headers in session CookieJar
	p.cookieManager.SaveCookies(sessionID, destURL, resp.Header)

	// 6. Build and send response headers
	respHeaders := make(map[string]string)
	if gzipPassthrough {
		respHeaders["x-p2p-content-encoding"] = "gzip"
	}
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "set-cookie" {
			continue // Set-Cookie handled separately in Cookies list
		}

		// Anti-iframe bypass & virtualization sandbox:
		// Strip CSP headers completely because the visitor browser runs inside a virtualization sandbox (/--p2p-proxy/ and Service Worker).
		// Retaining backend CSP causes policy violations against /patch.js, inline virtualization scripts (window.__P2P_*), and WebSocket tunnels.
		// The outer portal maintains its own defensive posture.
		if lk == "x-frame-options" || lk == "cross-origin-embedder-policy" || lk == "cross-origin-opener-policy" ||
			lk == "cross-origin-resource-policy" ||
			lk == "content-security-policy" || lk == "content-security-policy-report-only" {
			continue
		}

		// Rewrite redirects to remain inside the tunnel
		if lk == "location" {
			respHeaders[lk] = p.rewriteLocation(strings.Join(v, ", "), targetHost)
			continue
		}

		respHeaders[lk] = strings.Join(v, ", ")
	}

	// Expose target origin, root domain, and allowed proxy domains to Service Worker
	respHeaders["x-p2p-target-origin"] = p.targetURL.Scheme + "://" + p.targetURL.Host
	respHeaders["x-p2p-target-root-domain"] = p.rootDomain
	respHeaders["x-p2p-allowed-domains"] = strings.Join(p.allowedDomains, ",")
	respHeaders["referrer-policy"] = "no-referrer"

	// Extract non-HttpOnly cookies for document.cookie synchronization
	syncCookies := ExtractNonHttpOnlyCookies(resp.Header["Set-Cookie"])

	respMeta := ResponseMetadata{
		Status:  resp.StatusCode,
		Msg:     resp.Status,
		Headers: respHeaders,
		Cookies: syncCookies,
	}

	metaJSON, _ := json.Marshal(respMeta)
	if err := stream.WriteHeaders(metaJSON); err != nil {
		log.Printf("[Proxy] Stream %d failed to write response headers: %v", stream.ID(), err)
		return
	}

	// 7. Stream response body back to peer with backpressure
	bufPtr := chunkBufferPool.Get().(*[]byte)
	buf := *bufPtr
	defer chunkBufferPool.Put(bufPtr)

	for {
		n, readErr := respBody.Read(buf)
		if n > 0 {
			if _, writeErr := stream.Write(buf[:n]); writeErr != nil {
				// Client likely canceled or disconnected
				break
			}
		}
		if readErr != nil {
			if readErr != io.EOF && readErr != context.Canceled {
				log.Printf("[Proxy] Stream %d response body read error: %v", stream.ID(), readErr)
			}
			break
		}
	}
}

func (p *HTTPProxy) sendErrorResponse(stream *mux.Stream, statusCode int, message string) {
	respMeta := ResponseMetadata{
		Status: statusCode,
		Msg:    message,
		Headers: map[string]string{
			"content-type": "text/plain; charset=utf-8",
		},
	}
	metaJSON, _ := json.Marshal(respMeta)
	_ = stream.WriteHeaders(metaJSON)
	_, _ = stream.Write([]byte(message))
}
