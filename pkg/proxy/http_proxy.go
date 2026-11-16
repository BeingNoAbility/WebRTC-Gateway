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
	targetURL     *url.URL
	rootDomain    string
	cookieManager *SessionCookieManager
	httpClient    *http.Client
	transport     *http.Transport
}

// NewHTTPProxy creates a new HTTPProxy targeting targetURLString.
func NewHTTPProxy(targetURLString string, cm *SessionCookieManager) (*HTTPProxy, error) {
	u, err := url.Parse(targetURLString)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // Decompression handled explicitly
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DialContext:         CreateSafeDialContext(u),
	}

	return &HTTPProxy{
		targetURL:     u,
		rootDomain:    ExtractRootDomain(u.Hostname()),
		cookieManager: cm,
		transport:     transport,
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Don't follow redirects automatically; let the browser receive 3xx
				return http.ErrUseLastResponse
			},
			Timeout: 0, // No blanket timeout, support long streaming downloads
		},
	}, nil
}

// SetProxy configures an outbound HTTP/HTTPS/SOCKS5 proxy for backend requests.
func (p *HTTPProxy) SetProxy(proxyURL string) {
	if p.transport == nil {
		return
	}
	if proxyURL != "" {
		if pu, err := url.Parse(proxyURL); err == nil {
			p.transport.Proxy = http.ProxyURL(pu)
			return
		}
	}
	p.transport.Proxy = http.ProxyFromEnvironment
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
		hostOnly := strings.ToLower(strings.Split(destHost, ":")[0])

		// SSRF Defense: If destHost is a raw IP, reject if it is a private/restricted IP (unless it is the target backend itself)
		if parsedIP := net.ParseIP(hostOnly); parsedIP != nil {
			if IsRestrictedIP(parsedIP) && !IsTargetHost(hostOnly, "", p.targetURL) {
				return nil, "", fmt.Errorf("SSRF blocked: proxy access to restricted/private IP %s is forbidden", hostOnly)
			}
		}

		brand := strings.Split(p.rootDomain, ".")[0]
		brandShort := brand
		if len(brandShort) > 4 {
			brandShort = brandShort[:4]
		}

		lowerPath := strings.ToLower(subPath)
		isMedia := strings.Contains(lowerPath, ".m4s") || strings.Contains(lowerPath, ".mp4") ||
			strings.Contains(lowerPath, ".ts") || strings.Contains(lowerPath, ".m3u8") ||
			strings.Contains(lowerPath, ".webm") || strings.Contains(lowerPath, "/upgcxcode/")

		isAllowed := hostOnly == p.rootDomain ||
			strings.HasSuffix(hostOnly, "."+p.rootDomain) ||
			strings.EqualFold(hostOnly, p.targetURL.Hostname()) ||
			(brandShort != "" && strings.Contains(hostOnly, brandShort)) ||
			isMedia

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
		if strings.HasPrefix(cleanPath, proxyPrefix) {
			return nil, "", fmt.Errorf("cross-origin proxying to %s is restricted", destHost)
		}
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
		if hostClean == p.rootDomain || strings.HasSuffix(hostClean, "."+p.rootDomain) {
			return "/--p2p-proxy/" + u.Host + u.RequestURI()
		}
		brand := strings.Split(p.rootDomain, ".")[0]
		brandShort := brand
		if len(brandShort) > 4 {
			brandShort = brandShort[:4]
		}
		if brandShort != "" && strings.Contains(hostClean, brandShort) {
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

func sanitizeCSP(csp string) string {
	parts := strings.Split(csp, ";")
	var kept []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) > 0 {
			dirName := strings.ToLower(fields[0])
			if dirName == "frame-ancestors" {
				// Strip frame-ancestors to permit iframe embedding
				continue
			}
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "; ")
}

// HandleStream proxies an incoming P2PMux stream to the local target web application.
func (p *HTTPProxy) HandleStream(sessionID string, stream *mux.Stream) {
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

	// Standard reverse proxy headers
	req.Host = targetHost
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
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
				if name != "" {
					req.AddCookie(&http.Cookie{
						Name:  name,
						Value: val,
					})
				}
			}
		}
	}

	// 4. Forward to local backend service
	resp, err := p.httpClient.Do(req)
	if err != nil {
		log.Printf("[Proxy] Stream %d request to %s failed: %v", stream.ID(), destURL, err)
		p.sendErrorResponse(stream, http.StatusBadGateway, fmt.Sprintf("Backend service error: %v", err))
		return
	}
	defer resp.Body.Close()

	// Decompress gzip transparently if backend compressed the response
	var respBody io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gzReader, err := gzip.NewReader(resp.Body)
		if err == nil {
			defer gzReader.Close()
			respBody = gzReader
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("Content-Length")
		}
	}

	// 5. Store Set-Cookie headers in session CookieJar
	p.cookieManager.SaveCookies(sessionID, destURL, resp.Header)

	// 6. Build and send response headers
	respHeaders := make(map[string]string)
	for k, v := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "set-cookie" {
			continue // Set-Cookie handled separately in Cookies list
		}

		// Anti-iframe bypass: strip headers that forbid framing or break sandbox virtualization
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

	// Expose target origin and root domain to Service Worker so it knows which cross-origin requests belong to this app
	respHeaders["x-p2p-target-origin"] = p.targetURL.Scheme + "://" + p.targetURL.Host
	respHeaders["x-p2p-target-root-domain"] = p.rootDomain
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
