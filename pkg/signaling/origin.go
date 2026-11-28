package signaling

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"
)

var (
	originHostRegex = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	sbxLabelRegex   = regexp.MustCompile(`^sbx-[a-z0-9]{1,59}$`)
)

type SandboxMode string

const (
	SandboxModeLocal    SandboxMode = "local"
	SandboxModeFixed    SandboxMode = "fixed"
	SandboxModeWildcard SandboxMode = "wildcard"
)

// DomainPolicy enforces explicit startup-origin classification (Tier 1 cross-site wildcard,
// Tier 2 restricted fixed-host / same-site wildcard, or loopback LocalDev), strict Host
// allowlisting (421), Origin validation (CSWSH prevention), and single-pass path normalization.
// No runtime X-Forwarded-Proto guessing is ever performed.
type DomainPolicy struct {
	Mode              SandboxMode
	Scheme            string   // "http" or "https" (fixed at startup)
	PortalOrigin      string   // Canonical origin, e.g. "https://portal.example.com" (empty in LocalDev)
	PortalDomain      string   // Canonical host[:port] without default port
	PortalHost        string   // Lowercase host without port
	PortalPort        string   // Non-default port only, e.g. "8443" (empty if default for Scheme)
	SandboxOrigin     string   // Canonical origin, e.g. "https://*.sandbox.example.net" or "https://app.example.net"
	SandboxDomain     string   // Canonical host[:port] without "*." and without default port
	SandboxHost       string   // Lowercase host without port or "*."
	SandboxPort       string   // Non-default port only (empty if default for Scheme)
	RestrictedSandbox bool     // True for Tier 2 (fixed single sandbox host OR same-site wildcard)
	IsSameSite        bool     // True ONLY when active sandbox shares eTLD+1 with portal (emits Origin-Agent-Cluster)
	LocalDevMode      bool     // RFC 6761 *.localhost on loopback
	LocalPort         string   // Explicit listen port in LocalDev (empty only when listenAddr uses :0)
	LocalPortalHosts  []string // Allowed loopback hosts strictly bound to listen address family
	AllowAllOrigins   bool
}

type HostClass int

const (
	HostRejected HostClass = iota
	HostPortal
	HostSandbox
)

var (
	ErrInvalidOriginConfig   = errors.New("signaling: invalid portal/sandbox origin configuration")
	ErrSameSiteRequiresOpt   = errors.New("signaling: sandbox origin shares eTLD+1 with portal origin; use a distinct registrable domain or set -allow-same-site-sandbox")
	ErrPublicRequiresOrigins = errors.New("signaling: non-loopback bind requires explicit -portal-origin and -sandbox-origin")
)

func isValidPortString(p string) bool {
	if p == "" || len(p) > 5 {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func effectivePort(scheme, port string) string {
	if port == "" {
		return defaultPort(scheme)
	}
	return port
}

func formatOriginHostPort(scheme, host, port string) string {
	if port != "" && port != defaultPort(scheme) {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// HasSandboxLabel reports whether ANY DNS label in host starts with "sbx-".
func HasSandboxLabel(host string) bool {
	for _, lbl := range strings.Split(strings.ToLower(host), ".") {
		if strings.HasPrefix(lbl, "sbx-") {
			return true
		}
	}
	return false
}

// IsLoopbackBindAddr checks if an HTTP listen address binds exclusively to loopback.
func IsLoopbackBindAddr(addr string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return false
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

type parsedOrigin struct {
	canonical string
	scheme    string
	domain    string // host[:nonDefaultPort]
	host      string
	port      string // non-default port only
	wildcard  bool
}

func parseConfiguredOrigin(raw string, allowWildcard bool) (parsedOrigin, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return parsedOrigin{}, fmt.Errorf("%w: empty origin", ErrInvalidOriginConfig)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return parsedOrigin{}, fmt.Errorf("%w: control char in %q", ErrInvalidOriginConfig, raw)
		}
	}
	lower := strings.ToLower(s)
	var scheme, rest string
	switch {
	case strings.HasPrefix(lower, "https://"):
		scheme = "https"
		rest = lower[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		scheme = "http"
		rest = lower[len("http://"):]
	default:
		return parsedOrigin{}, fmt.Errorf("%w: origin %q must start with https:// or http://", ErrInvalidOriginConfig, raw)
	}

	wildcard := false
	if strings.HasPrefix(rest, "*.") {
		if !allowWildcard {
			return parsedOrigin{}, fmt.Errorf("%w: wildcard not permitted in portal origin %q", ErrInvalidOriginConfig, raw)
		}
		wildcard = true
		rest = rest[2:]
	}

	if rest == "" || strings.ContainsAny(rest, "/\\?*#@ \t\r\n;,'\"") {
		return parsedOrigin{}, fmt.Errorf("%w: illegal character or path in origin %q", ErrInvalidOriginConfig, raw)
	}

	host := rest
	port := ""
	if strings.Contains(rest, ":") {
		h, p, err := net.SplitHostPort(rest)
		if err != nil || !isValidPortString(p) {
			return parsedOrigin{}, fmt.Errorf("%w: invalid host:port in origin %q", ErrInvalidOriginConfig, raw)
		}
		host, port = h, p
	}
	if !originHostRegex.MatchString(host) || strings.Contains(host, "..") {
		return parsedOrigin{}, fmt.Errorf("%w: malformed hostname %q in origin %q", ErrInvalidOriginConfig, host, raw)
	}
	if _, ipErr := netip.ParseAddr(host); ipErr == nil {
		return parsedOrigin{}, fmt.Errorf("%w: IP literal %q cannot be used as portal/sandbox origin", ErrInvalidOriginConfig, raw)
	}
	if HasSandboxLabel(host) {
		return parsedOrigin{}, fmt.Errorf("%w: reserved sbx- label in %q", ErrInvalidOriginConfig, raw)
	}
	if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		etld, eErr := publicsuffix.EffectiveTLDPlusOne(host)
		if eErr != nil || etld == "" {
			return parsedOrigin{}, fmt.Errorf("%w: %q is a public suffix or single-label host", ErrInvalidOriginConfig, raw)
		}
	}

	if port == defaultPort(scheme) {
		port = ""
	}
	domain := formatOriginHostPort(scheme, host, port)
	canon := scheme + "://" + domain
	if wildcard {
		canon = scheme + "://*." + domain
	}
	return parsedOrigin{
		canonical: canon,
		scheme:    scheme,
		domain:    domain,
		host:      host,
		port:      port,
		wildcard:  wildcard,
	}, nil
}

func resolveLocalDevHostsAndPort(listenAddr string) ([]string, string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddr))
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid listen address %q", ErrInvalidOriginConfig, listenAddr)
	}
	if port == "0" {
		port = ""
	} else if !isValidPortString(port) {
		return nil, "", fmt.Errorf("%w: invalid listen port %q", ErrInvalidOriginConfig, listenAddr)
	}
	if strings.EqualFold(host, "localhost") {
		return []string{"localhost"}, port, nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return nil, "", ErrPublicRequiresOrigins
	}
	ip = ip.WithZone("").Unmap()
	if ip.Is4() {
		// Strictly bind IPv4 loopback only — never cross-trust [::1] on IPv4-only listeners.
		return []string{"localhost", ip.String()}, port, nil
	}
	// Strictly bind IPv6 loopback only — never cross-trust 127.0.0.1 on IPv6-only listeners.
	return []string{"localhost", ip.String()}, port, nil
}

// NewDomainPolicy constructs and strictly validates explicit startup origin isolation:
//   - LocalDev (-dev or loopback bind with no origins): *.localhost sandbox + address-family-scoped loopback portal.
//   - Tier 1 (Full Isolation): -portal-origin https://gw.example.com + -sandbox-origin https://*.sandbox.example.net (distinct eTLD+1).
//   - Tier 2 (Restricted Isolation):
//     a) Fixed cross-site sandbox: -portal-origin https://gw.example.com + -sandbox-origin https://app.example.net (distinct eTLD+1, RestrictedSandbox=true).
//     b) Same-site wildcard sandbox: -portal-origin https://gw.example.com + -sandbox-origin https://*.sbx.example.com + -allow-same-site-sandbox (RestrictedSandbox=true, Origin-Agent-Cluster).
func NewDomainPolicy(
	listenAddr string,
	portalOriginRaw string,
	sandboxOriginRaw string,
	devMode bool,
	tlsEnabled bool,
	allowSameSiteSandbox bool,
	allowAllOrigins bool,
) (*DomainPolicy, error) {
	pRaw := strings.TrimSpace(portalOriginRaw)
	sRaw := strings.TrimSpace(sandboxOriginRaw)

	if devMode {
		if pRaw != "" || sRaw != "" {
			return nil, fmt.Errorf("%w: -dev cannot be combined with -portal-origin or -sandbox-origin", ErrInvalidOriginConfig)
		}
		if !IsLoopbackBindAddr(listenAddr) {
			return nil, fmt.Errorf("%w: -dev requires a loopback -addr (e.g. 127.0.0.1:8888)", ErrInvalidOriginConfig)
		}
	}

	if devMode || (pRaw == "" && sRaw == "") {
		if !IsLoopbackBindAddr(listenAddr) {
			return nil, ErrPublicRequiresOrigins
		}
		localHosts, localPort, err := resolveLocalDevHostsAndPort(listenAddr)
		if err != nil {
			return nil, err
		}
		scheme := "http"
		if tlsEnabled {
			scheme = "https"
		}
		if localPort == defaultPort(scheme) {
			localPort = ""
		}
		return &DomainPolicy{
			Mode:             SandboxModeLocal,
			Scheme:           scheme,
			LocalDevMode:     true,
			LocalPort:        localPort,
			LocalPortalHosts: localHosts,
			AllowAllOrigins:  allowAllOrigins,
		}, nil
	}

	if pRaw == "" || sRaw == "" {
		return nil, fmt.Errorf("%w: both -portal-origin and -sandbox-origin must be configured together", ErrInvalidOriginConfig)
	}

	p, err := parseConfiguredOrigin(pRaw, false)
	if err != nil {
		return nil, err
	}
	s, err := parseConfiguredOrigin(sRaw, true)
	if err != nil {
		return nil, err
	}
	if p.scheme != s.scheme {
		return nil, fmt.Errorf("%w: -portal-origin (%s) and -sandbox-origin (%s) must use the same scheme", ErrInvalidOriginConfig, p.scheme, s.scheme)
	}

	pSite := effectiveSite(p.host)
	sSite := effectiveSite(s.host)

	dp := &DomainPolicy{
		Scheme:          p.scheme,
		PortalOrigin:    p.canonical,
		PortalDomain:    p.domain,
		PortalHost:      p.host,
		PortalPort:      p.port,
		SandboxOrigin:   s.canonical,
		SandboxDomain:   s.domain,
		SandboxHost:     s.host,
		SandboxPort:     s.port,
		AllowAllOrigins: allowAllOrigins,
	}

	if !s.wildcard {
		// Tier 2 Restricted Fixed Single-Host Sandbox: cross-site against Portal is still mandatory.
		if s.host == p.host || pSite == sSite {
			return nil, fmt.Errorf("%w: fixed -sandbox-origin must use a distinct eTLD+1 from -portal-origin", ErrInvalidOriginConfig)
		}
		dp.Mode = SandboxModeFixed
		dp.RestrictedSandbox = true
		dp.IsSameSite = false
		return dp, nil
	}

	// Wildcard Sandbox Origin: Tier 1 (cross-site) or Tier 2 (same-site with explicit opt-in).
	dp.Mode = SandboxModeWildcard
	if pSite == sSite {
		if !allowSameSiteSandbox {
			return nil, ErrSameSiteRequiresOpt
		}
		dp.IsSameSite = true
		dp.RestrictedSandbox = true
	}
	return dp, nil
}

func effectiveSite(host string) string {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "localhost"
	}
	etld, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return host
	}
	return etld
}

func splitHostPortLower(rawHost string) (host, port string, ok bool) {
	s := strings.ToLower(strings.TrimSpace(rawHost))
	if s == "" || strings.ContainsAny(s, " \t\r\n;,'\"/\\") {
		return "", "", false
	}
	if strings.HasPrefix(s, "[") {
		if strings.Contains(s, "]:") {
			h, p, err := net.SplitHostPort(s)
			if err != nil || !isValidPortString(p) {
				return "", "", false
			}
			if _, err := netip.ParseAddr(h); err != nil {
				return "", "", false
			}
			return h, p, true
		}
		if !strings.HasSuffix(s, "]") {
			return "", "", false
		}
		inner := s[1 : len(s)-1]
		if _, err := netip.ParseAddr(inner); err != nil {
			return "", "", false
		}
		return inner, "", true
	}
	if strings.Count(s, ":") == 1 {
		h, p, err := net.SplitHostPort(s)
		if err != nil || h == "" || !isValidPortString(p) {
			return "", "", false
		}
		return h, p, true
	}
	if strings.Contains(s, ":") {
		return "", "", false
	}
	return s, "", true
}

func isSingleLabelSubdomain(host, base string) (label string, ok bool) {
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	lbl := strings.TrimSuffix(host, suffix)
	if lbl == "" || strings.Contains(lbl, ".") {
		return "", false
	}
	return lbl, true
}

func portMatchesConfigured(gotPort, configuredPort, scheme string) bool {
	if configuredPort != "" {
		return gotPort == configuredPort
	}
	return gotPort == "" || gotPort == defaultPort(scheme)
}

func (dp *DomainPolicy) isAllowedLocalHost(host string) bool {
	for _, h := range dp.LocalPortalHosts {
		if host == h {
			return true
		}
	}
	return false
}

// IsHTTPS reports whether the configured external origin uses HTTPS.
func (dp *DomainPolicy) IsHTTPS() bool {
	return dp.Scheme == "https"
}

// ClassifyHost determines whether r.Host is the EXACT authorized Portal host, an authorized
// Sandbox host (fixed host or strict sbx-[a-z0-9]+ single-label subdomain), or rejected (421).
func (dp *DomainPolicy) ClassifyHost(rawHost string) HostClass {
	host, port, ok := splitHostPortLower(rawHost)
	if !ok {
		return HostRejected
	}

	if dp.LocalDevMode {
		if dp.LocalPort != "" && !portMatchesConfigured(port, dp.LocalPort, dp.Scheme) {
			return HostRejected
		}
		if dp.isAllowedLocalHost(host) {
			return HostPortal
		}
		if lbl, sub := isSingleLabelSubdomain(host, "localhost"); sub && sbxLabelRegex.MatchString(lbl) {
			return HostSandbox
		}
		return HostRejected
	}

	if host == dp.PortalHost && portMatchesConfigured(port, dp.PortalPort, dp.Scheme) {
		return HostPortal
	}
	if dp.Mode == SandboxModeFixed {
		if host == dp.SandboxHost && portMatchesConfigured(port, dp.SandboxPort, dp.Scheme) {
			return HostSandbox
		}
		return HostRejected
	}
	if lbl, sub := isSingleLabelSubdomain(host, dp.SandboxHost); sub && portMatchesConfigured(port, dp.SandboxPort, dp.Scheme) {
		if sbxLabelRegex.MatchString(lbl) {
			return HostSandbox
		}
	}
	return HostRejected
}

// CheckWSOrigin strictly validates browser Origin headers for /ws/* endpoints.
// Absent Origin is allowed (non-browser CLI/Agent); present empty or multiple Origin headers fail closed.
func (dp *DomainPolicy) CheckWSOrigin(r *http.Request) bool {
	if dp.ClassifyHost(r.Host) != HostPortal {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) > 1 {
		return false
	}
	origin := strings.TrimSpace(origins[0])
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	oHost := strings.ToLower(u.Hostname())
	oPort := u.Port()
	if oPort != "" && !isValidPortString(oPort) {
		return false
	}

	// Hard invariant: ANY host containing an sbx-* label or matching SandboxHost is forbidden from /ws/*.
	if HasSandboxLabel(oHost) {
		return false
	}
	if dp.Mode == SandboxModeFixed && oHost == dp.SandboxHost {
		return false
	}
	if dp.AllowAllOrigins {
		return true
	}

	if u.Scheme != dp.Scheme {
		return false
	}

	if dp.LocalDevMode {
		rHost, rPort, ok := splitHostPortLower(r.Host)
		if !ok {
			return false
		}
		// Require exact loopback host & port match (no 127.0.0.1 <-> localhost cross-origin WS).
		return oHost == rHost && effectivePort(u.Scheme, oPort) == effectivePort(dp.Scheme, rPort)
	}

	canonOrigin := u.Scheme + "://" + formatOriginHostPort(u.Scheme, oHost, oPort)
	return canonOrigin == dp.PortalOrigin
}

// SandboxFrameSrc returns the exact CSP frame-src directive value for Portal pages.
// portOverride is used only in ephemeral LocalDev (:0) mode.
func (dp *DomainPolicy) SandboxFrameSrc(portOverride string) string {
	if !dp.LocalDevMode {
		return dp.SandboxOrigin
	}
	port := dp.LocalPort
	if port == "" {
		port = portOverride
	}
	if port != "" && port != defaultPort(dp.Scheme) {
		return fmt.Sprintf("%s://*.localhost:%s", dp.Scheme, port)
	}
	return fmt.Sprintf("%s://*.localhost", dp.Scheme)
}

// AllowedPortalOriginsList returns the exact list of authorized Portal origins allowed to
// embed /__p2p_gw__/bridge.html and send P2P_PORTAL_INIT. NEVER includes wildcards.
func (dp *DomainPolicy) AllowedPortalOriginsList(portOverride string) []string {
	if !dp.LocalDevMode {
		return []string{dp.PortalOrigin}
	}
	port := dp.LocalPort
	if port == "" {
		port = portOverride
	}
	out := make([]string, 0, len(dp.LocalPortalHosts))
	for _, h := range dp.LocalPortalHosts {
		out = append(out, fmt.Sprintf("%s://%s", dp.Scheme, formatOriginHostPort(dp.Scheme, h, port)))
	}
	return out
}

// PortalFrameAncestors returns the exact CSP frame-ancestors directive for /__p2p_gw__/bridge.html.
func (dp *DomainPolicy) PortalFrameAncestors(portOverride string) string {
	return strings.Join(dp.AllowedPortalOriginsList(portOverride), " ")
}

// CanonicalizeHTTPPath performs a SINGLE-PASS decode and normalization on an already-escaped
// HTTP request path (`r.URL.EscapedPath()`), rejecting double-encoding bypasses (`%25`),
// encoded slash/backslash/NUL, and malformed percent escapes.
func CanonicalizeHTTPPath(escapedPath string) (string, error) {
	if escapedPath == "" {
		return "/", nil
	}
	if strings.ContainsRune(escapedPath, '\\') || strings.ContainsRune(escapedPath, '\x00') {
		return "", errors.New("forbidden backslash or NUL in path")
	}
	lowerEsc := strings.ToLower(escapedPath)
	if strings.Contains(lowerEsc, "%2f") || strings.Contains(lowerEsc, "%5c") || strings.Contains(lowerEsc, "%00") {
		return "", errors.New("forbidden encoded path separator or NUL")
	}
	decoded, err := url.PathUnescape(escapedPath)
	if err != nil {
		return "", fmt.Errorf("invalid percent-encoding: %w", err)
	}
	if strings.ContainsRune(decoded, '\\') || strings.ContainsRune(decoded, '\x00') {
		return "", errors.New("forbidden decoded backslash or NUL in path")
	}
	if !strings.HasPrefix(decoded, "/") {
		decoded = "/" + decoded
	}
	return path.Clean(decoded), nil
}
