package proxy

import (
	"container/list"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	// Server-authored Set-Cookie pool quotas (isolated from JS client writes).
	MaxServerCookiesPerJar    = 256
	MaxServerCookiesPerDomain = 64
	MaxServerCookieBytes      = 64 * 1024

	// Browser JS document.cookie pool quotas (can NEVER evict or shadow serverCookies).
	MaxClientCookiesPerJar    = 128
	MaxClientCookiesPerDomain = 32
	MaxClientCookieBytes      = 16 * 1024
	MaxClientMutationsPerCall = 32
	MaxCookiePathBytes        = 1024
	MaxCookieNameBytes        = 255

	MaxCookieMaxAgeSeconds  = 400 * 24 * 3600 // RFC 6265bis 400-day cap (prevents int64 overflow!)
	MaxCookieSessions       = 1024
	OwnerCookieSessionCap   = 256
	GuestCookieSessionCap   = 768
	CookieSessionIdleExpiry = 2 * time.Hour
)

// CookieDelta describes one validated non-HttpOnly target cookie update sent to the browser's
// in-memory cookie mirror.
type CookieDelta struct {
	Op      string `json:"op"` // "set" or "del"
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"`
	Path    string `json:"path"`
	Expires int64  `json:"expires,omitempty"` // unix ms (0 = session cookie)
}

// ClientCookieMutation describes a JS `document.cookie = ...` write or deletion sent from browser.
type ClientCookieMutation struct {
	Op      string `json:"op"` // "set" or "del"
	Name    string `json:"name"`
	Value   string `json:"value,omitempty"`
	Path    string `json:"path,omitempty"`
	Expires int64  `json:"expires,omitempty"` // unix ms (0 = session cookie)
}

// CookieRequestContext carries V7 FetchCtx-derived SameSite/navigation context.
// Zero value is Fail-Closed (`SameOriginClient == false` => treated as cross-site).
type CookieRequestContext struct {
	Method           string
	SameOriginClient bool
	IsTopLevelNav    bool
}

type storedCookie struct {
	name         string
	value        string
	domain       string
	hostOnly     bool
	path         string
	secure       bool
	httpOnly     bool
	sameSite     http.SameSite
	expires      time.Time
	hasExpiry    bool
	createdAt    time.Time
	lastAccessed time.Time
}

func (c *storedCookie) byteSize() int {
	return len(c.name) + len(c.value) + len(c.domain) + len(c.path) + 32
}

type cookieKey struct {
	domain string
	path   string
	name   string
}

type cookiePool struct {
	cookies    map[cookieKey]*storedCookie
	totalBytes int
	maxJar     int
	maxDomain  int
	maxBytes   int
}

func newCookiePool(maxJar, maxDomain, maxBytes int) *cookiePool {
	return &cookiePool{
		cookies:   make(map[cookieKey]*storedCookie),
		maxJar:    maxJar,
		maxDomain: maxDomain,
		maxBytes:  maxBytes,
	}
}

func (p *cookiePool) deleteKey(k cookieKey) {
	if existing, ok := p.cookies[k]; ok {
		p.totalBytes -= existing.byteSize()
		delete(p.cookies, k)
	}
}

func (p *cookiePool) pruneExpired(now time.Time) {
	for k, c := range p.cookies {
		if c.hasExpiry && !c.expires.After(now) {
			p.totalBytes -= c.byteSize()
			delete(p.cookies, k)
		}
	}
}

func (p *cookiePool) enforceDomainCap(dom string) {
	var domainKeys []cookieKey
	for k, c := range p.cookies {
		if c.domain == dom {
			domainKeys = append(domainKeys, k)
		}
	}
	if len(domainKeys) < p.maxDomain {
		return
	}
	sort.Slice(domainKeys, func(a, b int) bool {
		return p.cookies[domainKeys[a]].lastAccessed.Before(p.cookies[domainKeys[b]].lastAccessed)
	})
	for len(domainKeys) >= p.maxDomain {
		victim := domainKeys[0]
		domainKeys = domainKeys[1:]
		p.deleteKey(victim)
	}
}

func (p *cookiePool) evictForSpace(needBytes int, protect cookieKey) {
	type cand struct {
		key cookieKey
		ts  time.Time
	}
	var cands []cand
	for k, c := range p.cookies {
		if k == protect {
			continue
		}
		cands = append(cands, cand{key: k, ts: c.lastAccessed})
	}
	sort.Slice(cands, func(a, b int) bool {
		return cands[a].ts.Before(cands[b].ts)
	})
	for _, c := range cands {
		if len(p.cookies) < p.maxJar && p.totalBytes+needBytes <= p.maxBytes {
			return
		}
		p.deleteKey(c.key)
	}
}

// upsert inserts or updates entry inside this pool, returning true only if actually stored.
func (p *cookiePool) upsert(key cookieKey, entry *storedCookie) bool {
	if existing, exists := p.cookies[key]; exists {
		deltaBytes := entry.byteSize() - existing.byteSize()
		if p.totalBytes+deltaBytes > p.maxBytes {
			p.evictForSpace(deltaBytes, key)
		}
		if p.totalBytes+deltaBytes <= p.maxBytes {
			entry.createdAt = existing.createdAt
			p.totalBytes += deltaBytes
			p.cookies[key] = entry
			return true
		}
		return false
	}
	p.enforceDomainCap(entry.domain)
	if len(p.cookies) >= p.maxJar || p.totalBytes+entry.byteSize() > p.maxBytes {
		p.evictForSpace(entry.byteSize(), cookieKey{})
	}
	if len(p.cookies) < p.maxJar && p.totalBytes+entry.byteSize() <= p.maxBytes {
		p.cookies[key] = entry
		p.totalBytes += entry.byteSize()
		return true
	}
	return false
}

// ProxyCookieJar is the sole authoritative RFC 6265bis cookie store on the Go Agent.
// V7 splits storage into two physically isolated pools:
//   - serverCookies (64/domain, 256/jar, 64 KiB): written exclusively by upstream Set-Cookie.
//   - clientCookies (32/domain, 128/jar, 16 KiB): written exclusively by JS document.cookie.
//
// JS writes can NEVER evict, shadow, inject into, or downgrade server-authored cookies.
type ProxyCookieJar struct {
	mu            sync.RWMutex
	serverCookies *cookiePool
	clientCookies *cookiePool
}

func NewProxyCookieJar() *ProxyCookieJar {
	return &ProxyCookieJar{
		serverCookies: newCookiePool(MaxServerCookiesPerJar, MaxServerCookiesPerDomain, MaxServerCookieBytes),
		clientCookies: newCookiePool(MaxClientCookiesPerJar, MaxClientCookiesPerDomain, MaxClientCookieBytes),
	}
}

// isRFC9110Token checks strict RFC 9110 §5.6.2 tchar for cookie names.
func isValidCookieName(s string) bool {
	if len(s) == 0 || len(s) > MaxCookieNameBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		default:
			return false
		}
	}
	return true
}

// isValidCookieOctet validates RFC 6265 §4.1.1 cookie-octet bytes (forbids ';', ',', '\', '"', CTL, whitespace).
func isValidCookieOctet(c byte) bool {
	return c == 0x21 ||
		(c >= 0x23 && c <= 0x2B) ||
		(c >= 0x2D && c <= 0x3A) ||
		(c >= 0x3C && c <= 0x5B) ||
		(c >= 0x5D && c <= 0x7E)
}

// isValidClientCookieValue enforces strict RFC 6265 §4.1.1 cookie-value grammar
// (either empty, *cookie-octet, or DQUOTE *cookie-octet DQUOTE), preventing delimiter injection.
func isValidClientCookieValue(v string) bool {
	if len(v) > 4096 {
		return false
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	for i := 0; i < len(v); i++ {
		if !isValidCookieOctet(v[i]) {
			return false
		}
	}
	return true
}

func isValidClientCookiePath(p string) bool {
	if len(p) == 0 || len(p) > MaxCookiePathBytes || p[0] != '/' {
		return false
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x20 || c == 0x7f || c == ';' {
			return false
		}
	}
	return true
}

// SaveResponseCookies validates and stores Set-Cookie headers from reqURL into serverCookies
// and returns validated non-HttpOnly CookieDeltas for the browser's in-memory mirror.
func (j *ProxyCookieJar) SaveResponseCookies(reqURL *url.URL, rawSetCookies []string) []CookieDelta {
	if reqURL == nil || len(rawSetCookies) == 0 {
		return nil
	}
	reqHost := strings.ToLower(reqURL.Hostname())
	reqScheme := strings.ToLower(reqURL.Scheme)
	isSecureOrigin := reqScheme == "https" || reqScheme == "wss"
	now := time.Now()

	dummyResp := &http.Response{Header: http.Header{"Set-Cookie": rawSetCookies}}
	parsed := dummyResp.Cookies()
	if len(parsed) == 0 {
		return nil
	}

	var deltas []CookieDelta
	j.mu.Lock()
	defer j.mu.Unlock()

	j.serverCookies.pruneExpired(now)
	j.clientCookies.pruneExpired(now)

	for _, c := range parsed {
		if !isValidCookieName(c.Name) {
			continue
		}
		rawDomainAttr := strings.TrimSpace(c.Domain)
		dom, hostOnly, ok := validateCookieDomain(reqHost, rawDomainAttr)
		if !ok {
			continue
		}
		explicitPath := c.Path
		cPath := explicitPath
		if cPath == "" || !strings.HasPrefix(cPath, "/") {
			cPath = defaultCookiePath(reqURL.Path)
		}

		// RFC 6265bis: case-insensitive __Secure- and __Host- prefix enforcement.
		lowerName := strings.ToLower(c.Name)
		if strings.HasPrefix(lowerName, "__secure-") && (!c.Secure || !isSecureOrigin) {
			continue
		}
		if strings.HasPrefix(lowerName, "__host-") {
			// Must have Secure, https origin, NO Domain attribute (`c.Domain == ""`), AND explicit Path=/.
			if !c.Secure || !isSecureOrigin || rawDomainAttr != "" || !hostOnly || explicitPath != "/" {
				continue
			}
		}

		// RFC 6265bis: SameSite=None requires Secure attribute.
		if c.SameSite == http.SameSiteNoneMode && !c.Secure {
			continue
		}

		var expires time.Time
		hasExpiry := false
		if c.MaxAge < 0 {
			expires = now.Add(-time.Second)
			hasExpiry = true
		} else if c.MaxAge > 0 {
			ma := c.MaxAge
			if ma > MaxCookieMaxAgeSeconds {
				ma = MaxCookieMaxAgeSeconds
			}
			expires = now.Add(time.Duration(ma) * time.Second)
			hasExpiry = true
		} else if !c.Expires.IsZero() {
			maxExp := now.Add(time.Duration(MaxCookieMaxAgeSeconds) * time.Second)
			expires = c.Expires
			if expires.After(maxExp) {
				expires = maxExp
			}
			hasExpiry = true
		}

		key := cookieKey{domain: dom, path: cPath, name: c.Name}

		// RFC 6265bis §5.7 "Leave Secure Cookies Alone": non-HTTPS origin MUST NOT overwrite or delete
		// an existing Secure cookie.
		if !isSecureOrigin {
			if existing, exists := j.serverCookies.cookies[key]; exists && existing.secure {
				continue
			}
			if c.Secure {
				continue
			}
		}

		prevSrv := j.serverCookies.cookies[key]
		prevCli := j.clientCookies.cookies[key]
		wasVisibleToJS := (prevSrv != nil && !prevSrv.httpOnly) || prevCli != nil

		isDelete := hasExpiry && !expires.After(now)
		if isDelete {
			j.serverCookies.deleteKey(key)
			j.clientCookies.deleteKey(key)
			// Emit "del" if not HttpOnly OR if a formerly JS-visible cookie was just expired/deleted.
			if !c.HttpOnly || wasVisibleToJS {
				deltas = append(deltas, CookieDelta{Op: "del", Name: c.Name, Path: cPath})
			}
			continue
		}

		entry := &storedCookie{
			name:         c.Name,
			value:        c.Value,
			domain:       dom,
			hostOnly:     hostOnly,
			path:         cPath,
			secure:       c.Secure,
			httpOnly:     c.HttpOnly,
			sameSite:     c.SameSite,
			expires:      expires,
			hasExpiry:    hasExpiry,
			createdAt:    now,
			lastAccessed: now,
		}
		if entry.byteSize() > 8192 {
			continue
		}

		written := j.serverCookies.upsert(key, entry)
		if !written {
			// NEVER emit a "set" delta if writing to the jar failed!
			continue
		}
		// Server cookie takes precedence over any same-key client cookie.
		j.clientCookies.deleteKey(key)

		if !c.HttpOnly {
			var expMs int64
			if hasExpiry {
				expMs = expires.UnixMilli()
			}
			deltas = append(deltas, CookieDelta{
				Op:      "set",
				Name:    c.Name,
				Value:   c.Value,
				Path:    cPath,
				Expires: expMs,
			})
		} else if wasVisibleToJS {
			// Cookie was upgraded from non-HttpOnly -> HttpOnly: scrub it from browser CookieMirror!
			deltas = append(deltas, CookieDelta{
				Op:   "del",
				Name: c.Name,
				Path: cPath,
			})
		}
	}

	return deltas
}

// ApplyClientMutations applies explicit browser JS `document.cookie` writes and deletions
// into the isolated `clientCookies` pool. Enforces:
//   - <= 32 mutations per call
//   - Strict RFC 9110 tchar cookie name & RFC 6265 §4.1.1 cookie-octet value & safe path
//   - Forbids `__Host-` / `__Secure-` prefixes
//   - Forbids colliding with any matching entry in `serverCookies` (`HttpOnly` or `Secure` are strictly
//     immutable; non-HttpOnly non-Secure server cookies cannot be shadowed/injected via `set`,
//     and `del` only removes exact-path non-HttpOnly non-Secure cookies).
func (j *ProxyCookieJar) ApplyClientMutations(targetURL *url.URL, muts []ClientCookieMutation) {
	if targetURL == nil || len(muts) == 0 {
		return
	}
	if len(muts) > MaxClientMutationsPerCall {
		muts = muts[:MaxClientMutationsPerCall]
	}
	reqHost := strings.ToLower(targetURL.Hostname())
	reqScheme := strings.ToLower(targetURL.Scheme)
	isSecureOrigin := reqScheme == "https" || reqScheme == "wss"
	now := time.Now()

	j.mu.Lock()
	defer j.mu.Unlock()
	j.serverCookies.pruneExpired(now)
	j.clientCookies.pruneExpired(now)

	for _, m := range muts {
		name := strings.TrimSpace(m.Name)
		if !isValidCookieName(name) {
			continue
		}
		lowerName := strings.ToLower(name)
		if strings.HasPrefix(lowerName, "__host-") || strings.HasPrefix(lowerName, "__secure-") {
			continue
		}
		cPath := m.Path
		if cPath == "" {
			cPath = "/"
		}
		if !isValidClientCookiePath(cPath) {
			continue
		}

		// Inspect serverCookies for matching name on reqHost.
		hasProtectedServerCookie := false
		hasAnyServerCookie := false
		for _, existing := range j.serverCookies.cookies {
			if existing.name == name && cookieDomainMatches(reqHost, existing.domain, existing.hostOnly) {
				hasAnyServerCookie = true
				if existing.httpOnly || existing.secure {
					hasProtectedServerCookie = true
					break
				}
			}
		}

		if m.Op == "del" {
			if hasProtectedServerCookie {
				continue
			}
			// Delete matching entries from clientCookies.
			for k, existing := range j.clientCookies.cookies {
				if existing.name == name && existing.path == cPath && cookieDomainMatches(reqHost, existing.domain, existing.hostOnly) {
					if existing.secure && !isSecureOrigin {
						continue
					}
					j.clientCookies.deleteKey(k)
				}
			}
			// Also remove matching non-HttpOnly non-Secure server cookie at exact path so JS deletion works (no zombie).
			for k, existing := range j.serverCookies.cookies {
				if existing.name == name && existing.path == cPath && cookieDomainMatches(reqHost, existing.domain, existing.hostOnly) {
					if existing.httpOnly || existing.secure {
						continue
					}
					j.serverCookies.deleteKey(k)
				}
			}
			continue
		}

		if m.Op == "set" {
			// Strict RFC 6265 §4.1.1 cookie-octet check on Value (prevents ';' delimiter injection).
			if !isValidClientCookieValue(m.Value) {
				continue
			}
			// Never allow JS to overwrite, shadow, or cookie-toss any name already owned by serverCookies!
			if hasAnyServerCookie {
				continue
			}

			key := cookieKey{domain: reqHost, path: cPath, name: name}
			var expires time.Time
			hasExpiry := false
			if m.Expires > 0 {
				expires = time.UnixMilli(m.Expires)
				maxExp := now.Add(time.Duration(MaxCookieMaxAgeSeconds) * time.Second)
				if expires.After(maxExp) {
					expires = maxExp
				}
				hasExpiry = true
				if !expires.After(now) {
					j.clientCookies.deleteKey(key)
					continue
				}
			}

			entry := &storedCookie{
				name:         name,
				value:        m.Value,
				domain:       reqHost,
				hostOnly:     true,
				path:         cPath,
				secure:       false,
				httpOnly:     false,
				sameSite:     http.SameSiteLaxMode,
				expires:      expires,
				hasExpiry:    hasExpiry,
				createdAt:    now,
				lastAccessed: now,
			}
			if entry.byteSize() > 4096 {
				continue
			}
			j.clientCookies.upsert(key, entry)
		}
	}
}

// ExportNonHttpOnlySnapshot returns all active non-HttpOnly cookies matching targetURL for initial page sync.
func (j *ProxyCookieJar) ExportNonHttpOnlySnapshot(targetURL *url.URL) []CookieDelta {
	if targetURL == nil {
		return nil
	}
	reqHost := strings.ToLower(targetURL.Hostname())
	isSecure := targetURL.Scheme == "https" || targetURL.Scheme == "wss"
	now := time.Now()

	j.mu.RLock()
	defer j.mu.RUnlock()
	var out []CookieDelta
	collect := func(pool *cookiePool) {
		for _, c := range pool.cookies {
			if c.httpOnly {
				continue
			}
			if c.hasExpiry && !c.expires.After(now) {
				continue
			}
			if c.secure && !isSecure {
				continue
			}
			if !cookieDomainMatches(reqHost, c.domain, c.hostOnly) {
				continue
			}
			var expMs int64
			if c.hasExpiry {
				expMs = c.expires.UnixMilli()
			}
			out = append(out, CookieDelta{
				Op:      "set",
				Name:    c.name,
				Value:   c.value,
				Path:    c.path,
				Expires: expMs,
			})
		}
	}
	collect(j.serverCookies)
	collect(j.clientCookies)
	return out
}

// BuildCookieHeaderWithContext builds the upstream Cookie header for reqURL, enforcing
// RFC 6265bis SameSite (Strict/Lax/None), Secure, Domain, and Path rules.
// Server cookies are always prioritized ahead of client cookies of identical path length.
func (j *ProxyCookieJar) BuildCookieHeaderWithContext(reqURL *url.URL, ctx CookieRequestContext) string {
	if reqURL == nil {
		return ""
	}
	reqHost := strings.ToLower(reqURL.Hostname())
	reqPath := reqURL.Path
	if reqPath == "" {
		reqPath = "/"
	}
	isSecure := reqURL.Scheme == "https" || reqURL.Scheme == "wss"
	isCrossSite := !ctx.SameOriginClient
	isSafeMethod := isSafeHTTPMethod(ctx.Method)
	now := time.Now()

	type matchedEntry struct {
		c        *storedCookie
		isServer bool
	}

	j.mu.Lock()
	var matched []matchedEntry
	collect := func(pool *cookiePool, isServer bool) {
		for _, c := range pool.cookies {
			if c.hasExpiry && !c.expires.After(now) {
				continue
			}
			if c.secure && !isSecure {
				continue
			}
			if !cookieDomainMatches(reqHost, c.domain, c.hostOnly) {
				continue
			}
			if !cookiePathMatches(reqPath, c.path) {
				continue
			}
			if isCrossSite {
				switch c.sameSite {
				case http.SameSiteStrictMode:
					continue
				case http.SameSiteLaxMode, http.SameSiteDefaultMode:
					if !(ctx.IsTopLevelNav && isSafeMethod) {
						continue
					}
				case http.SameSiteNoneMode:
					if !c.secure || !isSecure {
						continue
					}
				}
			}
			c.lastAccessed = now
			matched = append(matched, matchedEntry{c: c, isServer: isServer})
		}
	}
	collect(j.serverCookies, true)
	collect(j.clientCookies, false)
	j.mu.Unlock()

	sort.Slice(matched, func(i, k int) bool {
		if len(matched[i].c.path) != len(matched[k].c.path) {
			return len(matched[i].c.path) > len(matched[k].c.path)
		}
		if matched[i].isServer != matched[k].isServer {
			return matched[i].isServer
		}
		return matched[i].c.createdAt.Before(matched[k].c.createdAt)
	})

	if len(matched) == 0 {
		return ""
	}
	var b strings.Builder
	for i, item := range matched {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(item.c.name)
		b.WriteByte('=')
		b.WriteString(item.c.value)
	}
	return b.String()
}

func isSafeHTTPMethod(m string) bool {
	switch strings.ToUpper(m) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func validateCookieDomain(reqHost, rawDomain string) (dom string, hostOnly bool, ok bool) {
	d := strings.ToLower(strings.TrimSpace(rawDomain))
	if d == "" {
		return reqHost, true, true
	}
	d = strings.TrimPrefix(d, ".")
	if d == "" {
		return "", false, false
	}
	if _, ipErr := netip.ParseAddr(reqHost); ipErr == nil {
		return reqHost, true, d == reqHost
	}
	// Check Public Suffix List BEFORE allowing d == reqHost (prevents pages.dev / co.uk supercookies!).
	if d != "localhost" && !strings.HasSuffix(d, ".localhost") {
		etld, err := publicsuffix.EffectiveTLDPlusOne(d)
		if err != nil || etld == "" {
			if d == reqHost {
				return reqHost, true, true
			}
			return "", false, false
		}
	}
	if d == reqHost || strings.HasSuffix(reqHost, "."+d) {
		return d, false, true
	}
	return "", false, false
}

func cookieDomainMatches(reqHost, dom string, hostOnly bool) bool {
	if hostOnly {
		return reqHost == dom
	}
	return reqHost == dom || strings.HasSuffix(reqHost, "."+dom)
}

func cookiePathMatches(reqPath, cPath string) bool {
	if reqPath == cPath {
		return true
	}
	if strings.HasPrefix(reqPath, cPath) {
		return strings.HasSuffix(cPath, "/") || reqPath[len(cPath)] == '/'
	}
	return false
}

func defaultCookiePath(reqPath string) string {
	if reqPath == "" || !strings.HasPrefix(reqPath, "/") {
		return "/"
	}
	idx := strings.LastIndexByte(reqPath, '/')
	if idx <= 0 {
		return "/"
	}
	return reqPath[:idx]
}

// ---- Split-Pool RefCount-Aware SessionCookieManager (Owner 256 / Guest 768) ----

type cookieSessionEntry struct {
	key        string
	jar        *ProxyCookieJar
	refCount   int
	lastActive time.Time
	elem       *list.Element
}

type sessionPool struct {
	entries map[string]*cookieSessionEntry
	lru     *list.List
	maxSize int
}

func newSessionPool(maxSize int) *sessionPool {
	return &sessionPool{
		entries: make(map[string]*cookieSessionEntry),
		lru:     list.New(),
		maxSize: maxSize,
	}
}

func (sp *sessionPool) evictIdle(now time.Time) {
	for el := sp.lru.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*cookieSessionEntry)
		if e.refCount == 0 && (now.Sub(e.lastActive) > CookieSessionIdleExpiry || len(sp.entries) >= sp.maxSize) {
			sp.lru.Remove(el)
			delete(sp.entries, e.key)
		}
		el = prev
	}
}

// SessionCookieManager manages per-principal cookie jars across two isolated pools:
//   - ownerPool (256 reserved slots): public/capability floods can NEVER evict owner sessions.
//   - guestPool (768 slots): capability and public visitor sessions.
//
// Enforces CookieSessionIdleExpiry (2h) on AcquireOrMint and server-minted resume tokens.
type SessionCookieManager struct {
	mu        sync.Mutex
	ownerPool *sessionPool
	guestPool *sessionPool
}

func NewSessionCookieManager(maxSessions int) *SessionCookieManager {
	ownerCap := OwnerCookieSessionCap
	guestCap := GuestCookieSessionCap
	if maxSessions > 0 && maxSessions < MaxCookieSessions {
		ownerCap = maxSessions / 4
		if ownerCap < 1 {
			ownerCap = 1
		}
		guestCap = maxSessions - ownerCap
		if guestCap < 1 {
			guestCap = 1
		}
	}
	return &SessionCookieManager{
		ownerPool: newSessionPool(ownerCap),
		guestPool: newSessionPool(guestCap),
	}
}

func (m *SessionCookieManager) poolForPrefix(prefix string) *sessionPool {
	if prefix == "owner" || strings.HasPrefix(prefix, "owner:") {
		return m.ownerPool
	}
	return m.guestPool
}

func newServerCookieToken() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// AcquireOrMint either resumes an existing, non-idle-expired server-issued key (prefix + ":" + existingToken)
// OR mints a fresh unguessable server-side key.
func (m *SessionCookieManager) AcquireOrMint(prefix, requestedToken string) (key string, token string, jar *ProxyCookieJar) {
	return m.acquireOrMintAt(prefix, requestedToken, time.Now())
}

func (m *SessionCookieManager) acquireOrMintAt(prefix, requestedToken string, now time.Time) (key string, token string, jar *ProxyCookieJar) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sp := m.poolForPrefix(prefix)
	if requestedToken != "" {
		candKey := prefix + ":" + requestedToken
		if e, ok := sp.entries[candKey]; ok {
			if e.refCount == 0 && now.Sub(e.lastActive) > CookieSessionIdleExpiry {
				// Enforce 2h idle TTL on resume attempts!
				sp.lru.Remove(e.elem)
				delete(sp.entries, candKey)
			} else {
				e.refCount++
				e.lastActive = now
				sp.lru.MoveToFront(e.elem)
				return candKey, requestedToken, e.jar
			}
		}
	}

	sp.evictIdle(now)
	token = newServerCookieToken()
	key = prefix + ":" + token
	e := &cookieSessionEntry{
		key:        key,
		jar:        NewProxyCookieJar(),
		refCount:   1,
		lastActive: now,
	}
	e.elem = sp.lru.PushFront(e)
	sp.entries[key] = e
	return key, token, e.jar
}

func (m *SessionCookieManager) Release(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sp := m.poolForPrefix(key)
	if e, ok := sp.entries[key]; ok {
		if e.refCount > 0 {
			e.refCount--
		}
		e.lastActive = time.Now()
	}
}

// DeleteByPrefix deletes all jars matching prefix (e.g. "cap:<jti>:" on capability revocation).
func (m *SessionCookieManager) DeleteByPrefix(prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sp := range []*sessionPool{m.ownerPool, m.guestPool} {
		for k, e := range sp.entries {
			if strings.HasPrefix(k, prefix) {
				sp.lru.Remove(e.elem)
				delete(sp.entries, k)
			}
		}
	}
}
