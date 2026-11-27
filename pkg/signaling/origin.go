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

var originHostRegex = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

type SandboxMode string

const (
	SandboxModeLocal    SandboxMode = "local"
	SandboxModeFixed    SandboxMode = "fixed"
	SandboxModeWildcard SandboxMode = "wildcard"
)

// DomainPolicy enforces 3-tier host classification, exact Portal origin isolation,
// strict Host allowlisting (421), Origin validation (CSWSH prevention),
// and single-pass canonical URL path normalization.
type DomainPolicy struct {
	Mode                 SandboxMode
	PortalDomain         string // lowercase host[:port], e.g. "portal.example.com"
	PortalHost           string // lowercase host without port
	PortalPort           string // optional explicit port (digits 1..65535)
	SandboxDomain        string // wildcard base domain (Tier 2/3) or exact fixed host[:port] (Tier 1)
	SandboxHost          string // lowercase host without port
	SandboxPort          string // optional explicit port
	AllowSameSiteSandbox bool   // Tier 2 explicit opt-in (-allow-same-site-sandbox)
	LocalDevMode         bool   // automatic RFC 6761 *.localhost on loopback
	AllowAllOrigins      bool
	TrustedProxies       []netip.Prefix
}

type HostClass int

const (
	HostRejected HostClass = iota
	HostPortal
	HostSandbox
)

var (
	ErrInvalidDomainConfig = errors.New("signaling: invalid portal/sandbox domain configuration")
	ErrSameSiteRequiresOpt = errors.New("signaling: sandbox_domain shares eTLD+1 with portal_domain; use a distinct registrable domain or set -allow-same-site-sandbox")
	ErrPublicRequiresDom   = errors.New("signaling: non-loopback bind requires -portal-domain with -sandbox-host (Tier 1) or -sandbox-domain (Tier 2/3)")
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

// ValidateDomainInput validates and normalizes a domain string ("host" or "host:port").
func ValidateDomainInput(raw string) (full, host, port string, err error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", "", "", nil
	}
	if strings.ContainsAny(s, "/\\?*#@ \t\r\n;,'\"") {
		return "", "", "", fmt.Errorf("%w: illegal character in %q", ErrInvalidDomainConfig, raw)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return "", "", "", fmt.Errorf("%w: control char in %q", ErrInvalidDomainConfig, raw)
		}
	}
	h := s
	if strings.Contains(s, ":") {
		var splitErr error
		h, port, splitErr = net.SplitHostPort(s)
		if splitErr != nil || !isValidPortString(port) {
			return "", "", "", fmt.Errorf("%w: invalid host:port %q", ErrInvalidDomainConfig, raw)
		}
	}
	if !originHostRegex.MatchString(h) || strings.Contains(h, "..") {
		return "", "", "", fmt.Errorf("%w: malformed hostname %q", ErrInvalidDomainConfig, raw)
	}
	if _, ipErr := netip.ParseAddr(h); ipErr == nil {
		return "", "", "", fmt.Errorf("%w: IP literal %q cannot be used as portal/sandbox domain", ErrInvalidDomainConfig, raw)
	}
	if HasSandboxLabel(h) {
		return "", "", "", fmt.Errorf("%w: reserved sbx- label in %q", ErrInvalidDomainConfig, raw)
	}
	if h != "localhost" && !strings.HasSuffix(h, ".localhost") {
		etld, eErr := publicsuffix.EffectiveTLDPlusOne(h)
		if eErr != nil || etld == "" {
			return "", "", "", fmt.Errorf("%w: %q is a public suffix or single-label host", ErrInvalidDomainConfig, raw)
		}
	}
	if port != "" {
		return h + ":" + port, h, port, nil
	}
	return h, h, "", nil
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

// NewDomainPolicy constructs and strictly validates the V7 3-tier domain isolation policy:
//   - LocalDev: loopback bind with no domains (localhost / 127.0.0.1 / [::1] + *.localhost)
//   - Tier 1 (Fixed): -portal-domain + -sandbox-host (distinct eTLD+1, no wildcard cert needed)
//   - Tier 2 (Same-Site Wildcard): -portal-domain + -sandbox-domain + -allow-same-site-sandbox (adds Origin-Agent-Cluster: ?1)
//   - Tier 3 (Cross-Site Wildcard): -portal-domain + -sandbox-domain (distinct eTLD+1)
func NewDomainPolicy(
	listenAddr string,
	portalDomainRaw string,
	sandboxDomainRaw string,
	sandboxHostRaw string,
	allowSameSiteSandbox bool,
	allowAllOrigins bool,
	trustedProxies []netip.Prefix,
) (*DomainPolicy, error) {
	pFull, pHost, pPort, err := ValidateDomainInput(portalDomainRaw)
	if err != nil {
		return nil, err
	}
	sDomFull, sDomHost, sDomPort, err := ValidateDomainInput(sandboxDomainRaw)
	if err != nil {
		return nil, err
	}
	sFixFull, sFixHost, sFixPort, err := ValidateDomainInput(sandboxHostRaw)
	if err != nil {
		return nil, err
	}

	if sDomHost != "" && sFixHost != "" {
		return nil, fmt.Errorf("%w: -sandbox-domain and -sandbox-host are mutually exclusive", ErrInvalidDomainConfig)
	}

	dp := &DomainPolicy{
		PortalDomain:         pFull,
		PortalHost:           pHost,
		PortalPort:           pPort,
		AllowSameSiteSandbox: allowSameSiteSandbox,
		AllowAllOrigins:      allowAllOrigins,
		TrustedProxies:       trustedProxies,
	}

	// LocalDev mode (loopback only, no configured domains).
	if pHost == "" && sDomHost == "" && sFixHost == "" {
		if !IsLoopbackBindAddr(listenAddr) {
			return nil, ErrPublicRequiresDom
		}
		dp.Mode = SandboxModeLocal
		dp.LocalDevMode = true
		return dp, nil
	}

	if pHost == "" {
		return nil, fmt.Errorf("%w: -sandbox-domain / -sandbox-host requires -portal-domain", ErrInvalidDomainConfig)
	}

	// Tier 1: Dual Fixed Domains (no wildcard DNS/TLS required; strictly distinct eTLD+1).
	if sFixHost != "" {
		if sFixHost == pHost {
			return nil, fmt.Errorf("%w: -sandbox-host must differ from -portal-domain", ErrInvalidDomainConfig)
		}
		if effectiveSite(pHost) == effectiveSite(sFixHost) {
			return nil, fmt.Errorf("%w: Tier 1 -sandbox-host must use a distinct eTLD+1 from -portal-domain", ErrInvalidDomainConfig)
		}
		dp.Mode = SandboxModeFixed
		dp.SandboxDomain = sFixFull
		dp.SandboxHost = sFixHost
		dp.SandboxPort = sFixPort
		return dp, nil
	}

	// Tier 2 / Tier 3: Wildcard Sandbox Domain.
	if sDomHost == "" {
		if !allowSameSiteSandbox {
			return nil, fmt.Errorf("%w: cross-site mode requires -sandbox-domain or -sandbox-host (or pass -allow-same-site-sandbox)", ErrInvalidDomainConfig)
		}
		dp.Mode = SandboxModeWildcard
		dp.SandboxDomain = pFull
		dp.SandboxHost = pHost
		dp.SandboxPort = pPort
		return dp, nil
	}

	pSite := effectiveSite(pHost)
	sSite := effectiveSite(sDomHost)
	if pSite == sSite && !allowSameSiteSandbox {
		return nil, ErrSameSiteRequiresOpt
	}
	dp.Mode = SandboxModeWildcard
	dp.SandboxDomain = sDomFull
	dp.SandboxHost = sDomHost
	dp.SandboxPort = sDomPort
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

// splitHostPortLower parses rawHost into lowercase host + strictly numeric port (1..65535).
// Rejects trailing colons, non-numeric ports, and any CSP/header metacharacters.
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

func formatOriginHostPort(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
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

func portMatchesConfigured(gotPort, configuredPort string) bool {
	if configuredPort != "" {
		return gotPort == configuredPort
	}
	// When no explicit port is configured in production, accept only implicit or standard HTTP(S) ports.
	return gotPort == "" || gotPort == "443" || gotPort == "80"
}

// ClassifyHost determines whether r.Host is the EXACT authorized Portal host, an authorized
// Sandbox host (Tier 1 fixed host or Tier 2/3/Local sbx-* single-label subdomain), or rejected (421).
func (dp *DomainPolicy) ClassifyHost(rawHost string) HostClass {
	host, port, ok := splitHostPortLower(rawHost)
	if !ok {
		return HostRejected
	}

	if dp.LocalDevMode {
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return HostPortal
		}
		if lbl, sub := isSingleLabelSubdomain(host, "localhost"); sub && strings.HasPrefix(lbl, "sbx-") {
			return HostSandbox
		}
		return HostRejected
	}

	if host == dp.PortalHost && portMatchesConfigured(port, dp.PortalPort) {
		return HostPortal
	}
	if dp.Mode == SandboxModeFixed {
		if host == dp.SandboxHost && portMatchesConfigured(port, dp.SandboxPort) {
			return HostSandbox
		}
		return HostRejected
	}
	if lbl, sub := isSingleLabelSubdomain(host, dp.SandboxHost); sub && portMatchesConfigured(port, dp.SandboxPort) {
		if strings.HasPrefix(lbl, "sbx-") {
			return HostSandbox
		}
	}
	return HostRejected
}

// IsHTTPS strictly determines whether the client connection used TLS.
// Reads RIGHTMOST X-Forwarded-Proto ONLY when remote peer is a verified trusted proxy.
func (dp *DomainPolicy) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = strings.TrimSpace(r.RemoteAddr)
	}
	if !IsTrustedProxy(remoteHost, dp.TrustedProxies) {
		return false
	}
	vals := r.Header.Values("X-Forwarded-Proto")
	if len(vals) == 0 {
		return false
	}
	for i := len(vals) - 1; i >= 0; i-- {
		parts := strings.Split(vals[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			v := strings.TrimSpace(parts[j])
			if v != "" {
				return strings.EqualFold(v, "https")
			}
		}
	}
	return false
}

// CheckWSOrigin strictly validates browser Origin headers for /ws/* endpoints.
func (dp *DomainPolicy) CheckWSOrigin(r *http.Request, requireBrowserOrigin bool) bool {
	if dp.ClassifyHost(r.Host) != HostPortal {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return !requireBrowserOrigin
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
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

	reqHTTPS := dp.IsHTTPS(r)
	if reqHTTPS && u.Scheme != "https" {
		return false
	}

	rHost, rPort, ok := splitHostPortLower(r.Host)
	if !ok {
		return false
	}

	if dp.LocalDevMode {
		if effectivePort(u.Scheme, oPort) == effectivePort(schemeFromBool(reqHTTPS), rPort) {
			if oHost == "localhost" || oHost == "127.0.0.1" || oHost == "::1" {
				return true
			}
		}
		return false
	}

	expectedPort := dp.PortalPort
	if expectedPort == "" {
		expectedPort = defaultPort(schemeFromBool(reqHTTPS))
	}
	return oHost == dp.PortalHost && oHost == rHost && effectivePort(u.Scheme, oPort) == expectedPort
}

// SandboxFrameSrc returns the exact CSP frame-src directive value for Portal pages.
func (dp *DomainPolicy) SandboxFrameSrc(r *http.Request) string {
	scheme := schemeFromBool(dp.IsHTTPS(r))
	if dp.LocalDevMode {
		_, rPort, ok := splitHostPortLower(r.Host)
		if ok && rPort != "" {
			return fmt.Sprintf("%s://*.localhost:%s", scheme, rPort)
		}
		return fmt.Sprintf("%s://*.localhost", scheme)
	}
	if dp.Mode == SandboxModeFixed {
		return fmt.Sprintf("%s://%s", scheme, formatOriginHostPort(dp.SandboxHost, dp.SandboxPort))
	}
	if dp.SandboxPort != "" {
		return fmt.Sprintf("%s://*.%s:%s", scheme, dp.SandboxHost, dp.SandboxPort)
	}
	return fmt.Sprintf("%s://*.%s", scheme, dp.SandboxHost)
}

// AllowedPortalOriginsList returns the exact list of authorized Portal origins allowed to
// embed /__p2p_gw__/bridge.html and send P2P_PORTAL_INIT.
// CRITICAL: NEVER includes *.localhost or *.portalHost!
func (dp *DomainPolicy) AllowedPortalOriginsList(r *http.Request) []string {
	scheme := schemeFromBool(dp.IsHTTPS(r))
	if dp.LocalDevMode {
		_, rPort, ok := splitHostPortLower(r.Host)
		if !ok {
			rPort = ""
		}
		return []string{
			fmt.Sprintf("%s://%s", scheme, formatOriginHostPort("localhost", rPort)),
			fmt.Sprintf("%s://%s", scheme, formatOriginHostPort("127.0.0.1", rPort)),
			fmt.Sprintf("%s://%s", scheme, formatOriginHostPort("::1", rPort)),
		}
	}
	return []string{
		fmt.Sprintf("%s://%s", scheme, formatOriginHostPort(dp.PortalHost, dp.PortalPort)),
	}
}

// PortalFrameAncestors returns the exact CSP frame-ancestors directive for /__p2p_gw__/bridge.html.
func (dp *DomainPolicy) PortalFrameAncestors(r *http.Request) string {
	return strings.Join(dp.AllowedPortalOriginsList(r), " ")
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

func schemeFromBool(https bool) string {
	if https {
		return "https"
	}
	return "http"
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
