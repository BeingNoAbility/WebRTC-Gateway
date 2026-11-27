package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"webrtc-gateway/pkg/protocol"
)

const (
	// Forbidden cross-origin proxy prefix (single-target Agent rejects all cross-origin tunneling).
	forbiddenCrossOriginProxyPrefix = "/__p2p_proxy__/"
)

var (
	ErrInvalidTargetURL    = errors.New("proxy: target URL must use http:// or https:// with a non-empty host")
	ErrInvalidUpstreamHost = errors.New("proxy: invalid upstream host authority")
	ErrUnsafeDestination   = errors.New("proxy: unsafe or unauthorized destination")
)

// CapabilityScope restricts a share-link visitor session to a path prefix and optional read-only methods.
type CapabilityScope struct {
	PathPrefix string
	Readonly   bool
}

// AllowsDestination validates a request AFTER policy.Resolve() against the canonical decoded
// destination path, composing cleanly with TargetSubpath (eliminates percent-encoding, query,
// and TargetSubpath+PathPrefix mismatch bypasses).
func (c *CapabilityScope) AllowsDestination(kind uint8, method string, dest *ResolvedDestination) bool {
	if c == nil {
		return true
	}
	if dest == nil || dest.URL == nil {
		return false
	}
	if c.Readonly {
		if kind == protocol.StreamKindWS {
			return false
		}
		switch strings.ToUpper(method) {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			return false
		}
	}
	if c.PathPrefix != "" && c.PathPrefix != "/" {
		cleanPrefix := path.Clean("/" + c.PathPrefix)
		allowedPrefix := cleanPrefix
		if dest.TargetSubpath != "" &&
			cleanPrefix != dest.TargetSubpath &&
			!strings.HasPrefix(cleanPrefix, dest.TargetSubpath+"/") {
			allowedPrefix = path.Clean(dest.TargetSubpath + cleanPrefix)
		}
		cleanReq := cleanPathPreserveSlash(dest.URL.Path)
		if cleanReq != allowedPrefix && !strings.HasPrefix(cleanReq, allowedPrefix+"/") {
			return false
		}
	}
	return true
}

// DestinationPolicy is the single authoritative routing & header-sanitization policy
// shared identically by HTTPProxy and WSProxy.
// Strict 1 Agent = 1 Target Origin (zero external/multi-domain proxying).
type DestinationPolicy struct {
	TargetBase     *url.URL
	TargetScheme   string // "http" or "https"
	TargetHost     string // lowercase hostname without port
	TargetPort     string // canonical port ("80", "443", or explicit)
	TargetHostPort string // canonical host[:port]
	TargetSubpath  string // "" or "/subpath" (cleaned, no trailing slash)

	// Optional -upstream-host override for virtual-host reverse proxies (Nginx/Caddy/Ingress).
	UpstreamHost     string // "" or canonical host[:port]
	UpstreamHostName string // "" or lowercase hostname without port
	UpstreamPort     string // "" or canonical port
}

// NewDestinationPolicy validates targetRaw and an optional upstreamHostRaw override.
func NewDestinationPolicy(targetRaw string, upstreamHostRaw string) (*DestinationPolicy, error) {
	u, err := url.Parse(strings.TrimSpace(targetRaw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidTargetURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" {
		return nil, ErrInvalidTargetURL
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = defaultSchemePort(scheme)
	}
	sub := ""
	if u.Path != "" && u.Path != "/" {
		sub = strings.TrimRight(path.Clean("/"+u.Path), "/")
		if sub == "/" {
			sub = ""
		}
	}

	upCanon, upHost, upPort, err := NormalizeUpstreamHost(scheme, upstreamHostRaw)
	if err != nil {
		return nil, err
	}

	return &DestinationPolicy{
		TargetBase:       u,
		TargetScheme:     scheme,
		TargetHost:       host,
		TargetPort:       port,
		TargetHostPort:   canonicalHostPort(scheme, host, port),
		TargetSubpath:    sub,
		UpstreamHost:     upCanon,
		UpstreamHostName: upHost,
		UpstreamPort:     upPort,
	}, nil
}

// NormalizeUpstreamHost validates an optional RFC 9110 host[:port] Host header override (Fail-Closed).
func NormalizeUpstreamHost(scheme, raw string) (canonHostPort, hostOnly, port string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", "", nil
	}
	if strings.ContainsAny(s, "?#@\\/ \t\r\n") || strings.Contains(s, "://") {
		return "", "", "", fmt.Errorf("%w: illegal character in %q", ErrInvalidUpstreamHost, raw)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return "", "", "", fmt.Errorf("%w: control char in %q", ErrInvalidUpstreamHost, raw)
		}
	}
	u, perr := url.Parse(scheme + "://" + s)
	if perr != nil || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", "", fmt.Errorf("%w: malformed authority %q", ErrInvalidUpstreamHost, raw)
	}
	hostOnly = strings.ToLower(u.Hostname())
	port = u.Port()
	if port != "" {
		pNum, aerr := strconv.Atoi(port)
		if aerr != nil || pNum < 1 || pNum > 65535 {
			return "", "", "", fmt.Errorf("%w: invalid port in %q", ErrInvalidUpstreamHost, raw)
		}
	} else {
		port = defaultSchemePort(scheme)
	}
	canonHostPort = canonicalHostPort(scheme, hostOnly, port)
	return canonHostPort, hostOnly, port, nil
}

// EffectiveHost returns UpstreamHost if configured, otherwise TargetHostPort.
func (p *DestinationPolicy) EffectiveHost() string {
	if p.UpstreamHost != "" {
		return p.UpstreamHost
	}
	return p.TargetHostPort
}

// TargetOrigin returns the canonical scheme://host[:port] of -target.
func (p *DestinationPolicy) TargetOrigin() string {
	return p.TargetScheme + "://" + p.TargetHostPort
}

// EffectiveOrigin returns scheme://EffectiveHost() used for Origin/Referer/BackendMeta/Location matching.
func (p *DestinationPolicy) EffectiveOrigin() string {
	return p.TargetScheme + "://" + p.EffectiveHost()
}

// DefaultTLSServerName returns explicit SNI if set, or defaults to UpstreamHostName for HTTPS targets
// (so `-target https://192.0.2.10 -upstream-host app.example.com` automatically sends SNI app.example.com).
func (p *DestinationPolicy) DefaultTLSServerName(explicitSNI string) string {
	if explicitSNI != "" {
		return explicitSNI
	}
	if p.TargetScheme == "https" && p.UpstreamHostName != "" && net.ParseIP(p.UpstreamHostName) == nil {
		return p.UpstreamHostName
	}
	return ""
}

// EffectiveBaseURL returns a URL sharing TargetBase's scheme/path with Host set to EffectiveHost(),
// ensuring ProxyCookieJar domain checks match virtual-host cookies when -upstream-host is set.
func (p *DestinationPolicy) EffectiveBaseURL() *url.URL {
	cp := *p.TargetBase
	cp.Host = p.EffectiveHost()
	return &cp
}

// CookieURL maps a resolved target URL to the virtual-host authority for ProxyCookieJar operations.
func (p *DestinationPolicy) CookieURL(destURL *url.URL) *url.URL {
	if destURL == nil || p.UpstreamHost == "" {
		return destURL
	}
	cp := *destURL
	cp.Host = p.UpstreamHost
	return &cp
}

func (p *DestinationPolicy) IsExactTargetOrigin(scheme, host, port string) bool {
	s := normalizeOriginScheme(scheme)
	if port == "" {
		port = defaultSchemePort(s)
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	return s == p.TargetScheme && h == p.TargetHost && port == p.TargetPort
}

func (p *DestinationPolicy) isEffectiveOrigin(scheme, host, port string) bool {
	if p.UpstreamHost == "" {
		return false
	}
	s := normalizeOriginScheme(scheme)
	if port == "" {
		port = defaultSchemePort(s)
	}
	h := strings.ToLower(strings.Trim(host, "[]"))
	return s == p.TargetScheme && h == p.UpstreamHostName && port == p.UpstreamPort
}

func normalizeOriginScheme(scheme string) string {
	s := strings.ToLower(scheme)
	switch s {
	case "ws":
		return "http"
	case "wss":
		return "https"
	default:
		return s
	}
}

// ResolvedDestination is the canonical validated upstream URL for HTTP or WS proxying.
type ResolvedDestination struct {
	URL           *url.URL
	TargetSubpath string
}

// Resolve translates an incoming request target strictly to the single configured -target origin
// (confined to TargetSubpath when configured). Cross-origin proxy prefixes are rejected Fail-Closed.
func (p *DestinationPolicy) Resolve(rawTarget string, isWS bool) (*ResolvedDestination, error) {
	if rawTarget == "" || !strings.HasPrefix(rawTarget, "/") {
		return nil, fmt.Errorf("%w: target must start with /", ErrUnsafeDestination)
	}
	if strings.ContainsRune(rawTarget, '\\') {
		return nil, fmt.Errorf("%w: backslash forbidden in target", ErrUnsafeDestination)
	}
	if strings.HasPrefix(rawTarget, forbiddenCrossOriginProxyPrefix) {
		return nil, fmt.Errorf("%w: cross-origin proxying is not supported (configure a local reverse proxy behind -target)", ErrUnsafeDestination)
	}

	cleanReq, rawP, rawQuery, err := parseAndCleanPath(rawTarget)
	if err != nil {
		return nil, err
	}

	// Subpath confinement: ensure `-target http://host/subpath` can NEVER be escaped via `/../admin`.
	finalPath := cleanReq
	finalRawPath := rawP
	if p.TargetSubpath != "" {
		if cleanReq != p.TargetSubpath && !strings.HasPrefix(cleanReq, p.TargetSubpath+"/") {
			if cleanReq == "/" {
				finalPath = p.TargetSubpath + "/"
			} else {
				finalPath = p.TargetSubpath + cleanReq
			}
			if rawP != "" {
				finalRawPath = p.TargetSubpath + rawP
			}
		}
	}

	u := &url.URL{
		Scheme:   wsOrHTTPScheme(p.TargetScheme, isWS),
		Host:     p.TargetHostPort,
		Path:     finalPath,
		RawPath:  finalRawPath,
		RawQuery: rawQuery,
	}
	return &ResolvedDestination{URL: u, TargetSubpath: p.TargetSubpath}, nil
}

func parseAndCleanPath(rawPathAndQuery string) (cleanPath, rawPath, rawQuery string, err error) {
	reqPath, rawQuery := splitPathQuery(rawPathAndQuery)
	if strings.ContainsRune(reqPath, '\\') || strings.Contains(strings.ToLower(reqPath), "%5c") {
		return "", "", "", fmt.Errorf("%w: backslash forbidden in path", ErrUnsafeDestination)
	}
	decodedPath, uerr := url.PathUnescape(reqPath)
	if uerr != nil {
		return "", "", "", fmt.Errorf("%w: invalid percent-encoding in path", ErrUnsafeDestination)
	}
	if strings.ContainsRune(decodedPath, '\\') || strings.ContainsRune(decodedPath, '\x00') {
		return "", "", "", fmt.Errorf("%w: forbidden character in decoded path", ErrUnsafeDestination)
	}
	// Reject path parameter traversal (`..;` / `.;` used to bypass reverse proxies on Tomcat/Spring).
	for _, seg := range strings.Split(decodedPath, "/") {
		if strings.HasPrefix(seg, "..;") || strings.HasPrefix(seg, ".;") {
			return "", "", "", fmt.Errorf("%w: path parameter traversal forbidden", ErrUnsafeDestination)
		}
	}
	cleanP := cleanPathPreserveSlash(decodedPath)

	// Preserve %2F in RawPath when there is no dot-segment traversal (GitLab / S3 API compatibility).
	lowerReq := strings.ToLower(reqPath)
	if strings.Contains(lowerReq, "%2f") && !strings.Contains(decodedPath, "/../") && !strings.HasSuffix(decodedPath, "/..") && !strings.Contains(lowerReq, "%2e") {
		rawPath = reqPath
	}
	return cleanP, rawPath, rawQuery, nil
}

// RewriteLocationHeader rewrites same-target (or same-upstream-host) Location headers to safe
// single-slash relative paths while preserving #fragment. Collapses leading `//` to `/` so
// `http://target//evil.example` can NEVER become a protocol-relative open redirect!
func (p *DestinationPolicy) RewriteLocationHeader(loc string, currentDest *ResolvedDestination) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return loc
	}
	baseURL := p.CookieURL(currentDest.URL)
	if baseURL.Scheme == "ws" {
		cp := *baseURL
		cp.Scheme = "http"
		baseURL = &cp
	} else if baseURL.Scheme == "wss" {
		cp := *baseURL
		cp.Scheme = "https"
		baseURL = &cp
	}

	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	resolved := baseURL.ResolveReference(u)
	scheme := strings.ToLower(resolved.Scheme)
	if scheme != "http" && scheme != "https" {
		return loc
	}
	host := strings.ToLower(resolved.Hostname())
	port := resolved.Port()
	if port == "" {
		port = defaultSchemePort(scheme)
	}

	if !(p.IsExactTargetOrigin(scheme, host, port) || p.isEffectiveOrigin(scheme, host, port)) {
		return loc
	}

	suffix := resolved.EscapedPath()
	// Normalize leading multiple slashes (`//evil.example` -> `/evil.example`) to block protocol-relative redirects.
	suffix = "/" + strings.TrimLeft(suffix, "/")
	if resolved.RawQuery != "" {
		suffix += "?" + resolved.RawQuery
	}
	if resolved.Fragment != "" {
		suffix += "#" + resolved.Fragment
	}
	return suffix
}

func newTargetDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return dialer.DialContext
}

func defaultSchemePort(scheme string) string {
	if scheme == "https" || scheme == "wss" {
		return "443"
	}
	return "80"
}

func canonicalHostPort(scheme, host, port string) string {
	if port == "" || port == defaultSchemePort(scheme) {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}

func wsOrHTTPScheme(httpScheme string, isWS bool) string {
	if !isWS {
		return httpScheme
	}
	if httpScheme == "https" {
		return "wss"
	}
	return "ws"
}

func splitPathQuery(raw string) (p, q string) {
	if idx := strings.IndexByte(raw, '?'); idx >= 0 {
		return raw[:idx], raw[idx+1:]
	}
	return raw, ""
}

func cleanPathPreserveSlash(rawPath string) string {
	if rawPath == "" {
		return "/"
	}
	trailing := strings.HasSuffix(rawPath, "/")
	cleaned := path.Clean("/" + rawPath)
	if trailing && cleaned != "/" && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
}

// ---- Unified Request & Response Header Sanitization (HTTP + WS Parity) ----

var forbiddenRequestHeaders = map[string]bool{
	"connection":              true,
	"keep-alive":              true,
	"proxy-authenticate":      true,
	"proxy-authorization":     true,
	"te":                      true,
	"trailer":                 true,
	"transfer-encoding":       true,
	"upgrade":                 true,
	"host":                    true,
	"cookie":                  true,
	"x-client-cookie":         true,
	"x-p2p-cookie-mutations":  true,
	"x-p2p-cookie-seq":        true,
	"x-p2p-client-decompress": true,
	"x-forwarded-for":         true,
	"x-forwarded-host":        true,
	"x-forwarded-proto":       true,
	"x-forwarded-port":        true,
	"x-real-ip":               true,
	"cf-connecting-ip":        true,
	"true-client-ip":          true,
	"x-original-url":          true,
	"x-rewrite-url":           true,
	"forwarded":               true,
	"x-p2p-dest-host":         true,
	"x-p2p-dest-scheme":       true,
}

var methodOverrideHeaders = map[string]bool{
	"x-http-method-override": true,
	"x-method-override":      true,
	"x-http-method":          true,
}

var forbiddenResponseHeaders = map[string]bool{
	"connection":                true,
	"keep-alive":                true,
	"proxy-authenticate":        true,
	"proxy-authorization":       true,
	"te":                        true,
	"trailer":                   true,
	"transfer-encoding":         true,
	"upgrade":                   true,
	"strict-transport-security": true,
	"clear-site-data":           true,
	"service-worker-allowed":    true,
	"x-p2p-content-encoding":    true,
	"x-p2p-cookie-delta":        true,
}

// SanitizeRequestHeaders builds clean upstream HTTP/WS headers from V7 TLV fields using
// the authoritative V7 `fetchCtx` bitfield (Fail-Closed: unset `FetchCtxSameOriginClient`
// is ALWAYS treated as cross-site; `Origin: null` and external Origin/Referer are NEVER
// rewritten to EffectiveOrigin; `FetchCtxCredentialsOmit` suppresses Cookie injection;
// `readonly` strips method-override headers).
func (p *DestinationPolicy) SanitizeRequestHeaders(
	fields []protocol.HeaderField,
	fetchCtx uint8,
	method string,
	dest *ResolvedDestination,
	clientIP string,
	jar *ProxyCookieJar,
	allowWSUpgradeHeaders bool,
	readonly bool,
) http.Header {
	connDrop := map[string]bool{}
	for _, f := range fields {
		if strings.EqualFold(f.Name, "connection") {
			for _, tok := range strings.Split(f.Value, ",") {
				if t := strings.ToLower(strings.TrimSpace(tok)); t != "" {
					connDrop[t] = true
				}
			}
		}
	}

	cookieURL := p.CookieURL(dest.URL)
	sameOriginClient := (fetchCtx & protocol.FetchCtxSameOriginClient) != 0
	isTopLevelNav := (fetchCtx & protocol.FetchCtxTopLevelNav) != 0
	credentialsOmit := (fetchCtx & protocol.FetchCtxCredentialsOmit) != 0

	out := make(http.Header)
	for _, f := range fields {
		k := strings.ToLower(f.Name)
		if allowWSUpgradeHeaders && (k == "sec-websocket-protocol" || k == "sec-websocket-extensions") {
			out.Add(f.Name, f.Value)
			continue
		}
		if forbiddenRequestHeaders[k] || connDrop[k] {
			continue
		}
		if readonly && methodOverrideHeaders[k] {
			continue
		}
		if k == "origin" {
			trimmed := strings.TrimSpace(f.Value)
			// NEVER rewrite `Origin: null` or cross-site external Origin to EffectiveOrigin()!
			if strings.EqualFold(trimmed, "null") {
				out.Set("Origin", "null")
			} else if sameOriginClient {
				out.Set("Origin", p.EffectiveOrigin())
			} else if trimmed != "" {
				out.Set("Origin", trimmed)
			}
			continue
		}
		if k == "referer" {
			// Rewrite Referer ONLY for verified same-origin clients (never spoof cross-site Referer!).
			if sameOriginClient {
				if rewritten := p.sanitizeReferer(f.Value); rewritten != "" {
					out.Set("Referer", rewritten)
				}
			}
			continue
		}
		out.Add(f.Name, f.Value)
	}

	if clientIP != "" {
		out.Set("X-Forwarded-For", clientIP)
		out.Set("X-Real-IP", clientIP)
	}
	proto := dest.URL.Scheme
	if proto == "ws" {
		proto = "http"
	} else if proto == "wss" {
		proto = "https"
	}
	out.Set("X-Forwarded-Proto", proto)

	if jar != nil && !credentialsOmit {
		ctx := CookieRequestContext{
			Method:           method,
			SameOriginClient: sameOriginClient,
			IsTopLevelNav:    isTopLevelNav,
		}
		if cookieVal := jar.BuildCookieHeaderWithContext(cookieURL, ctx); cookieVal != "" {
			out.Set("Cookie", cookieVal)
		}
	}

	return out
}

func (p *DestinationPolicy) sanitizeReferer(rawRef string) string {
	u, err := url.Parse(rawRef)
	if err != nil {
		return ""
	}
	refPath := u.EscapedPath()
	if refPath == "" {
		refPath = "/"
	}
	out := p.EffectiveOrigin() + refPath
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// StripResponseHopByHop returns the lowercase set of response headers to strip,
// including any headers named inside the response's Connection header.
func StripResponseHopByHop(respHdr http.Header) map[string]bool {
	drop := make(map[string]bool, len(forbiddenResponseHeaders)+4)
	for k := range forbiddenResponseHeaders {
		drop[k] = true
	}
	for _, connVal := range respHdr.Values("Connection") {
		for _, tok := range strings.Split(connVal, ",") {
			if t := strings.ToLower(strings.TrimSpace(tok)); t != "" {
				drop[t] = true
			}
		}
	}
	return drop
}

func cookieEntriesToMutations(entries []protocol.CookieEntry) []ClientCookieMutation {
	if len(entries) == 0 {
		return nil
	}
	out := make([]ClientCookieMutation, 0, len(entries))
	for _, e := range entries {
		op := "set"
		if e.Op == protocol.CookieOpDel {
			op = "del"
		}
		out = append(out, ClientCookieMutation{
			Op:      op,
			Name:    e.Name,
			Value:   e.Value,
			Path:    e.Path,
			Expires: e.ExpiresMs,
		})
	}
	return out
}

func cookieDeltasToEntries(deltas []CookieDelta) []protocol.CookieEntry {
	if len(deltas) == 0 {
		return nil
	}
	maxN := len(deltas)
	if maxN > protocol.MaxResponseCookieDeltas {
		maxN = protocol.MaxResponseCookieDeltas
	}
	out := make([]protocol.CookieEntry, 0, maxN)
	for i := 0; i < maxN; i++ {
		d := deltas[i]
		op := protocol.CookieOpSet
		if d.Op == "del" {
			op = protocol.CookieOpDel
		}
		name := protocol.SanitizeHeaderValue(d.Name)
		if len(name) == 0 || len(name) > protocol.MaxCookieNameLen {
			continue
		}
		out = append(out, protocol.CookieEntry{
			Op:        op,
			Name:      name,
			Value:     protocol.SanitizeHeaderValue(d.Value),
			Path:      protocol.SanitizeHeaderValue(d.Path),
			ExpiresMs: d.Expires,
		})
	}
	return out
}
