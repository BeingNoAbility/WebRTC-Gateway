package proxy

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
)

const (
	// Server-authored Set-Cookie quotas (isolated from JS client writes).
	MaxServerCookiesPerJar = 256
	MaxServerCookieBytes   = 64 * 1024

	// Browser JS document.cookie quotas (can NEVER evict or shadow HttpOnly/server cookies).
	MaxClientCookiesPerJar    = 128
	MaxClientCookieBytes      = 16 * 1024
	MaxClientMutationsPerCall = 32
	MaxCookiePathBytes        = 1024
	MaxCookieNameBytes        = 255

	// Bounded serialized Cookie: header budgets (prevents JS-triggered 431 DoS against 8K/16K upstreams).
	MaxSerializedCookieHeaderBytes = 16 * 1024
	MaxSerializedClientCookieBytes = 4 * 1024

	MaxCookieMaxAgeSeconds  = 400 * 24 * 3600 // RFC 6265bis 400-day cap
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

// CookieRequestContext carries FetchCtx-derived SameSite context.
// Zero value is Fail-Closed (`SameOriginClient == false` => treated as cross-site).
type CookieRequestContext struct {
	Method           string
	SameOriginClient bool
}

type storedCookie struct {
	name         string
	value        string
	quoted       bool
	domain       string
	hostOnly     bool
	path         string
	secure       bool
	httpOnly     bool
	sameSite     http.SameSite
	fromServer   bool
	expires      time.Time
	hasExpiry    bool
	createdAt    time.Time
	lastAccessed time.Time
}

func (c *storedCookie) byteSize() int {
	return len(c.name) + len(c.value) + len(c.domain) + len(c.path) + 32
}

func (c *storedCookie) wireValue() string {
	if c.quoted {
		return `"` + c.value + `"`
	}
	return c.value
}

type cookieKey struct {
	domain string
	path   string
	name   string
}

// ProxyCookieJar is the authoritative RFC 6265bis cookie store on the Go Agent,
// structurally split along the security boundary:
//   - httpOnly: server-only cookies (invisible to JS/CookieMirror, immune to JS mutation,
//     path-shadowing, or eviction by construction).
//   - visible: non-HttpOnly cookies (shared coherent truth between server Set-Cookie and
//     browser JS document.cookie; JS can never evict server-authored entries and on non-secure
//     origins cannot overwrite Secure entries per RFC 6265bis §5.7).
type ProxyCookieJar struct {
	mu          sync.RWMutex
	httpOnly    map[cookieKey]*storedCookie
	visible     map[cookieKey]*storedCookie
	serverCount int
	serverBytes int
	clientCount int
	clientBytes int
}

func NewProxyCookieJar() *ProxyCookieJar {
	return &ProxyCookieJar{
		httpOnly: make(map[cookieKey]*storedCookie),
		visible:  make(map[cookieKey]*storedCookie),
	}
}

func (j *ProxyCookieJar) tableFor(httpOnly bool) map[cookieKey]*storedCookie {
	if httpOnly {
		return j.httpOnly
	}
	return j.visible
}

func (j *ProxyCookieJar) adjustBucket(fromServer bool, deltaCount, deltaBytes int) {
	if fromServer {
		j.serverCount += deltaCount
		j.serverBytes += deltaBytes
	} else {
		j.clientCount += deltaCount
		j.clientBytes += deltaBytes
	}
}

func (j *ProxyCookieJar) bucketLimits(fromServer bool) (maxCount, maxBytes int) {
	if fromServer {
		return MaxServerCookiesPerJar, MaxServerCookieBytes
	}
	return MaxClientCookiesPerJar, MaxClientCookieBytes
}

func (j *ProxyCookieJar) bucketStats(fromServer bool) (count, totalBytes int) {
	if fromServer {
		return j.serverCount, j.serverBytes
	}
	return j.clientCount, j.clientBytes
}

func (j *ProxyCookieJar) deleteEntryLocked(tbl map[cookieKey]*storedCookie, k cookieKey) {
	if existing, ok := tbl[k]; ok {
		j.adjustBucket(existing.fromServer, -1, -existing.byteSize())
		delete(tbl, k)
	}
}

func (j *ProxyCookieJar) pruneExpiredLocked(now time.Time) {
	for _, tbl := range []map[cookieKey]*storedCookie{j.httpOnly, j.visible} {
		for k, c := range tbl {
			if c.hasExpiry && !c.expires.After(now) {
				j.adjustBucket(c.fromServer, -1, -c.byteSize())
				delete(tbl, k)
			}
		}
	}
}

// evictOldestLocked performs an allocation-free linear scan (<= 256 entries, < 1µs) to free space
// strictly within the caller's privilege bucket (client writes can NEVER evict server cookies).
func (j *ProxyCookieJar) evictOldestLocked(fromServer bool, needCount, needBytes int, protect cookieKey) {
	maxCount, maxBytes := j.bucketLimits(fromServer)
	for {
		cnt, bytes := j.bucketStats(fromServer)
		if cnt+needCount <= maxCount && bytes+needBytes <= maxBytes {
			return
		}
		var victimTbl map[cookieKey]*storedCookie
		var victimKey cookieKey
		var victimTS time.Time
		found := false
		for _, tbl := range []map[cookieKey]*storedCookie{j.visible, j.httpOnly} {
			for k, c := range tbl {
				if c.fromServer != fromServer || k == protect {
					continue
				}
				if !found || c.lastAccessed.Before(victimTS) {
					found = true
					victimTbl = tbl
					victimKey = k
					victimTS = c.lastAccessed
				}
			}
		}
		if !found {
			return
		}
		j.deleteEntryLocked(victimTbl, victimKey)
	}
}

func (j *ProxyCookieJar) upsertLocked(key cookieKey, entry *storedCookie) bool {
	tbl := j.tableFor(entry.httpOnly)
	maxCount, maxBytes := j.bucketLimits(entry.fromServer)

	if existing, exists := tbl[key]; exists && existing.fromServer == entry.fromServer {
		deltaBytes := entry.byteSize() - existing.byteSize()
		_, bytes := j.bucketStats(entry.fromServer)
		if bytes+deltaBytes > maxBytes {
			j.evictOldestLocked(entry.fromServer, 0, deltaBytes, key)
		}
		_, bytes = j.bucketStats(entry.fromServer)
		if bytes+deltaBytes <= maxBytes {
			entry.createdAt = existing.createdAt
			j.adjustBucket(entry.fromServer, 0, deltaBytes)
			tbl[key] = entry
			return true
		}
		return false
	}

	cnt, bytes := j.bucketStats(entry.fromServer)
	if cnt+1 > maxCount || bytes+entry.byteSize() > maxBytes {
		j.evictOldestLocked(entry.fromServer, 1, entry.byteSize(), key)
	}
	cnt, bytes = j.bucketStats(entry.fromServer)
	if cnt+1 <= maxCount && bytes+entry.byteSize() <= maxBytes {
		if existing, exists := tbl[key]; exists {
			j.adjustBucket(existing.fromServer, -1, -existing.byteSize())
		}
		tbl[key] = entry
		j.adjustBucket(entry.fromServer, 1, entry.byteSize())
		return true
	}
	return false
}

// isLoopbackHost returns true for W3C Secure Contexts potentially trustworthy loopback hosts.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(h); err == nil && ip.IsLoopback() {
		return true
	}
	return false
}

// IsSecureCookieOrigin implements RFC 6265bis / W3C Secure Contexts trustworthy origin matching.
func IsSecureCookieOrigin(u *url.URL) bool {
	if u == nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "https" || scheme == "wss" {
		return true
	}
	return isLoopbackHost(u.Hostname())
}

func isSecureCookieOrigin(u *url.URL) bool {
	return IsSecureCookieOrigin(u)
}

func cookieDomainsOverlap(domA string, hostOnlyA bool, domB string, hostOnlyB bool) bool {
	return cookieDomainMatches(domA, domB, hostOnlyB) || cookieDomainMatches(domB, domA, hostOnlyA)
}

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

// isValidClientCookieValue enforces browser-aligned document.cookie hygiene (matching TS
// parseDocumentCookieAssignment and Go TLV header validation): allows printable ASCII, spaces,
// commas, JSON punctuation, and valid UTF-8 while strictly rejecting CTLs (< 0x20, 0x7F) and ';'.
func isValidClientCookieValue(v string) bool {
	if len(v) > 4096 || !utf8.ValidString(v) {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x20 || c == 0x7f || c == ';' {
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

// SaveResponseCookies validates and stores Set-Cookie headers from reqURL and returns validated
// non-HttpOnly CookieDeltas for the browser's in-memory mirror.
// When an HttpOnly cookie is set/deleted, any pre-existing visible cookies with the same name
// on overlapping domains (any path) are swept to eliminate pre-login path-shadowing.
func (j *ProxyCookieJar) SaveResponseCookies(reqURL *url.URL, rawSetCookies []string) []CookieDelta {
	if reqURL == nil || len(rawSetCookies) == 0 {
		return nil
	}
	reqHost := strings.ToLower(reqURL.Hostname())
	isSecureOrigin := isSecureCookieOrigin(reqURL)
	now := time.Now()

	dummyResp := &http.Response{Header: http.Header{"Set-Cookie": rawSetCookies}}
	parsed := dummyResp.Cookies()
	if len(parsed) == 0 {
		return nil
	}

	var deltas []CookieDelta
	j.mu.Lock()
	defer j.mu.Unlock()

	j.pruneExpiredLocked(now)

	sweepVisibleCookies := func(name, dom string, hostOnly bool, clientOnly bool) {
		for k, existing := range j.visible {
			if clientOnly && existing.fromServer {
				continue
			}
			if existing.name == name && cookieDomainsOverlap(existing.domain, existing.hostOnly, dom, hostOnly) {
				j.deleteEntryLocked(j.visible, k)
				deltas = append(deltas, CookieDelta{Op: "del", Name: existing.name, Path: existing.path})
			}
		}
	}

	emitVisibleDeltaAfterDelete := func(name, cPath string) {
		// If a surviving sibling exists on the same (name, path) for reqHost, emit "set" for it instead of "del".
		for _, surv := range j.visible {
			if surv.name == name && surv.path == cPath && cookieDomainMatches(reqHost, surv.domain, surv.hostOnly) {
				var expMs int64
				if surv.hasExpiry {
					expMs = surv.expires.UnixMilli()
				}
				deltas = append(deltas, CookieDelta{
					Op:      "set",
					Name:    surv.name,
					Value:   surv.wireValue(),
					Path:    surv.path,
					Expires: expMs,
				})
				return
			}
		}
		deltas = append(deltas, CookieDelta{Op: "del", Name: name, Path: cPath})
	}

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

		lowerName := strings.ToLower(c.Name)
		if strings.HasPrefix(lowerName, "__secure-") && (!c.Secure || !isSecureOrigin) {
			continue
		}
		if strings.HasPrefix(lowerName, "__host-") {
			if !c.Secure || !isSecureOrigin || rawDomainAttr != "" || !hostOnly || explicitPath != "/" {
				continue
			}
		}
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

		// RFC 6265bis §5.7 "Leave Secure Cookies Alone": non-trustworthy origin MUST NOT overwrite
		// or delete an existing Secure cookie, nor mint a Secure cookie.
		if !isSecureOrigin {
			if existing, exists := j.httpOnly[key]; exists && existing.secure {
				continue
			}
			if existing, exists := j.visible[key]; exists && existing.secure {
				continue
			}
			if c.Secure {
				continue
			}
		}

		_, wasVisible := j.visible[key]
		isDelete := hasExpiry && !expires.After(now)
		if isDelete {
			j.deleteEntryLocked(j.httpOnly, key)
			j.deleteEntryLocked(j.visible, key)
			if !c.HttpOnly || wasVisible {
				emitVisibleDeltaAfterDelete(c.Name, cPath)
			}
			if c.HttpOnly {
				sweepVisibleCookies(c.Name, dom, hostOnly, false)
			} else if c.Secure {
				sweepVisibleCookies(c.Name, dom, hostOnly, true)
			}
			continue
		}

		entry := &storedCookie{
			name:         c.Name,
			value:        c.Value,
			quoted:       c.Quoted,
			domain:       dom,
			hostOnly:     hostOnly,
			path:         cPath,
			secure:       c.Secure,
			httpOnly:     c.HttpOnly,
			sameSite:     c.SameSite,
			fromServer:   true,
			expires:      expires,
			hasExpiry:    hasExpiry,
			createdAt:    now,
			lastAccessed: now,
		}
		if entry.byteSize() > 8192 {
			continue
		}

		if !j.upsertLocked(key, entry) {
			continue
		}

		if c.HttpOnly {
			// Upgrading non-HttpOnly -> HttpOnly or setting HttpOnly: remove any visible entry & scrub mirror!
			if wasVisible {
				j.deleteEntryLocked(j.visible, key)
				deltas = append(deltas, CookieDelta{Op: "del", Name: c.Name, Path: cPath})
			}
			sweepVisibleCookies(c.Name, dom, hostOnly, false)
		} else {
			// Downgrading HttpOnly -> non-HttpOnly on exact key removes old httpOnly record.
			j.deleteEntryLocked(j.httpOnly, key)
			if c.Secure {
				// Sweep any lower-privilege pre-login JS cookies shadowing this Secure server cookie on other paths.
				for k, existing := range j.visible {
					if k == key || existing.fromServer {
						continue
					}
					if existing.name == c.Name && cookieDomainsOverlap(existing.domain, existing.hostOnly, dom, hostOnly) {
						j.deleteEntryLocked(j.visible, k)
						deltas = append(deltas, CookieDelta{Op: "del", Name: existing.name, Path: existing.path})
					}
				}
			}
			var expMs int64
			if hasExpiry {
				expMs = expires.UnixMilli()
			}
			deltas = append(deltas, CookieDelta{
				Op:      "set",
				Name:    c.Name,
				Value:   entry.wireValue(),
				Path:    cPath,
				Expires: expMs,
			})
		}
	}

	return deltas
}

// ApplyClientMutations applies browser JS `document.cookie` writes and deletions billed to the
// isolated client quota.
//   - HttpOnly table is structurally untouchable (never mutated, shadowed, or evicted by JS).
//   - On non-secure origins, existing Secure visible cookies are protected (RFC 6265bis §5.7).
//   - On secure origins (HTTPS/loopback), JS may legitimately update/delete non-HttpOnly Secure
//     cookies (e.g. csrftoken / XSRF-TOKEN), eliminating CookieMirror split-brain (H-18 / M-13).
func (j *ProxyCookieJar) ApplyClientMutations(targetURL *url.URL, muts []ClientCookieMutation) {
	if targetURL == nil || len(muts) == 0 {
		return
	}
	if len(muts) > MaxClientMutationsPerCall {
		muts = muts[:MaxClientMutationsPerCall]
	}
	reqHost := strings.ToLower(targetURL.Hostname())
	isSecureOrigin := isSecureCookieOrigin(targetURL)
	now := time.Now()

	j.mu.Lock()
	defer j.mu.Unlock()
	j.pruneExpiredLocked(now)

	deleteVisibleMatching := func(name, cPath string) {
		for k, existing := range j.visible {
			if existing.name == name && existing.path == cPath && cookieDomainMatches(reqHost, existing.domain, existing.hostOnly) {
				if existing.secure && !isSecureOrigin {
					continue
				}
				j.deleteEntryLocked(j.visible, k)
			}
		}
	}

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
			cPath = defaultCookiePath(targetURL.Path)
		}
		if !isValidClientCookiePath(cPath) {
			continue
		}

		// Structural HttpOnly boundary: JS can NEVER shadow or mutate any HttpOnly cookie on any path.
		blockedByHttpOnly := false
		for _, existing := range j.httpOnly {
			if existing.name == name && cookieDomainMatches(reqHost, existing.domain, existing.hostOnly) {
				blockedByHttpOnly = true
				break
			}
		}
		if blockedByHttpOnly {
			continue
		}

		if m.Op == "del" {
			deleteVisibleMatching(name, cPath)
			continue
		}

		if m.Op == "set" {
			if !isValidClientCookieValue(m.Value) {
				continue
			}

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
					deleteVisibleMatching(name, cPath)
					continue
				}
			}

			// RFC 6265bis §5.7: non-secure origin cannot overwrite an existing Secure cookie.
			// On a secure origin, inherit existing Secure / SameSite attributes when JS updates an
			// existing visible cookie on (name, path) so e.g. Secure; SameSite=None XSRF tokens are
			// never silently downgraded to Secure=false; SameSite=Lax.
			inheritedSecure := false
			inheritedSameSite := http.SameSiteLaxMode
			for _, existing := range j.visible {
				if existing.name == name && existing.path == cPath &&
					cookieDomainMatches(reqHost, existing.domain, existing.hostOnly) {
					if existing.secure {
						if !isSecureOrigin {
							inheritedSecure = true // marker to reject below
							break
						}
						inheritedSecure = true
					}
					if existing.sameSite != http.SameSiteDefaultMode {
						inheritedSameSite = existing.sameSite
					}
				}
			}
			if !isSecureOrigin && inheritedSecure {
				continue
			}

			val := m.Value
			quoted := false
			if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
				val = val[1 : len(val)-1]
				quoted = true
			}

			key := cookieKey{domain: reqHost, path: cPath, name: name}
			entry := &storedCookie{
				name:         name,
				value:        val,
				quoted:       quoted,
				domain:       reqHost,
				hostOnly:     true,
				path:         cPath,
				secure:       inheritedSecure,
				httpOnly:     false,
				sameSite:     inheritedSameSite,
				fromServer:   false,
				expires:      expires,
				hasExpiry:    hasExpiry,
				createdAt:    now,
				lastAccessed: now,
			}
			// Validate full RFC byte budget BEFORE deleting any existing visible cookie on (name, path)
			// so a rejected oversized write never destroys an existing valid cookie.
			if entry.byteSize() > 4096 {
				continue
			}

			// Remove matching non-HttpOnly parent-domain cookie on exact (name, path) so JS update
			// cleanly supersedes it without duplicate/stale shadowing (M-13).
			deleteVisibleMatching(name, cPath)
			j.upsertLocked(key, entry)
		}
	}
}

// ExportNonHttpOnlySnapshot returns a deterministically sorted snapshot of all active non-HttpOnly
// cookies matching targetURL for initial page sync (server cookies first, then newest accessed).
func (j *ProxyCookieJar) ExportNonHttpOnlySnapshot(targetURL *url.URL) []CookieDelta {
	if targetURL == nil {
		return nil
	}
	reqHost := strings.ToLower(targetURL.Hostname())
	isSecure := isSecureCookieOrigin(targetURL)
	now := time.Now()

	type snapshotItem struct {
		fromServer   bool
		lastAccessed time.Time
		name         string
		wireVal      string
		path         string
		expiresMs    int64
	}

	j.mu.RLock()
	matched := make([]snapshotItem, 0, len(j.visible))
	for _, c := range j.visible {
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
		matched = append(matched, snapshotItem{
			fromServer:   c.fromServer,
			lastAccessed: c.lastAccessed,
			name:         c.name,
			wireVal:      c.wireValue(),
			path:         c.path,
			expiresMs:    expMs,
		})
	}
	j.mu.RUnlock()

	sort.Slice(matched, func(i, k int) bool {
		if matched[i].fromServer != matched[k].fromServer {
			return matched[i].fromServer
		}
		if !matched[i].lastAccessed.Equal(matched[k].lastAccessed) {
			return matched[i].lastAccessed.After(matched[k].lastAccessed)
		}
		if matched[i].name != matched[k].name {
			return matched[i].name < matched[k].name
		}
		return matched[i].path < matched[k].path
	})

	out := make([]CookieDelta, 0, len(matched))
	for _, c := range matched {
		out = append(out, CookieDelta{
			Op:      "set",
			Name:    c.name,
			Value:   c.wireVal,
			Path:    c.path,
			Expires: c.expiresMs,
		})
	}
	return out
}

// BuildCookieHeaderWithContext builds a bounded upstream Cookie header for reqURL enforcing
// RFC 6265bis SameSite, Secure, Domain, and Path rules.
// Enforces MaxSerializedCookieHeaderBytes (16 KiB) with priority reserved for HttpOnly & server
// cookies ahead of JS-written entries (capped at 4 KiB) to prevent JS-triggered 431 DoS (H-11).
func (j *ProxyCookieJar) BuildCookieHeaderWithContext(reqURL *url.URL, ctx CookieRequestContext) string {
	if reqURL == nil {
		return ""
	}
	reqHost := strings.ToLower(reqURL.Hostname())
	reqPath := reqURL.Path
	if reqPath == "" {
		reqPath = "/"
	}
	isSecure := isSecureCookieOrigin(reqURL)
	isCrossSite := !ctx.SameOriginClient
	now := time.Now()

	matches := func(c *storedCookie) bool {
		if c.hasExpiry && !c.expires.After(now) {
			return false
		}
		if c.secure && !isSecure {
			return false
		}
		if !cookieDomainMatches(reqHost, c.domain, c.hostOnly) {
			return false
		}
		if !cookiePathMatches(reqPath, c.path) {
			return false
		}
		if isCrossSite {
			switch c.sameSite {
			case http.SameSiteStrictMode, http.SameSiteLaxMode, http.SameSiteDefaultMode:
				return false
			case http.SameSiteNoneMode:
				if !c.secure || !isSecure {
					return false
				}
			}
		}
		return true
	}

	j.mu.Lock()
	var serverMatched []*storedCookie
	var clientMatched []*storedCookie
	for _, c := range j.httpOnly {
		if matches(c) {
			c.lastAccessed = now
			serverMatched = append(serverMatched, c)
		}
	}
	for _, c := range j.visible {
		if matches(c) {
			c.lastAccessed = now
			if c.fromServer {
				serverMatched = append(serverMatched, c)
			} else {
				clientMatched = append(clientMatched, c)
			}
		}
	}
	j.mu.Unlock()

	if len(serverMatched) == 0 && len(clientMatched) == 0 {
		return ""
	}

	// RFC 6265 §5.4 ordering within each tier; select within bounded byte budgets so JS cookies
	// can never crowd out HttpOnly/server cookies or trigger HTTP 431.
	sortRFC := func(list []*storedCookie) {
		sort.Slice(list, func(i, k int) bool {
			if len(list[i].path) != len(list[k].path) {
				return len(list[i].path) > len(list[k].path)
			}
			if list[i].httpOnly != list[k].httpOnly {
				return list[i].httpOnly
			}
			if list[i].fromServer != list[k].fromServer {
				return list[i].fromServer
			}
			return list[i].createdAt.Before(list[k].createdAt)
		})
	}
	sortRFC(serverMatched)
	sortRFC(clientMatched)

	selected := make([]*storedCookie, 0, len(serverMatched)+len(clientMatched))
	totalBytes := 0
	for _, c := range serverMatched {
		pairLen := len(c.name) + 1 + len(c.wireValue())
		sep := 0
		if len(selected) > 0 {
			sep = 2
		}
		if totalBytes+sep+pairLen > MaxSerializedCookieHeaderBytes {
			continue
		}
		selected = append(selected, c)
		totalBytes += sep + pairLen
	}

	clientBytes := 0
	for _, c := range clientMatched {
		pairLen := len(c.name) + 1 + len(c.wireValue())
		sep := 0
		if len(selected) > 0 {
			sep = 2
		}
		if clientBytes+pairLen > MaxSerializedClientCookieBytes || totalBytes+sep+pairLen > MaxSerializedCookieHeaderBytes {
			continue
		}
		selected = append(selected, c)
		clientBytes += pairLen
		totalBytes += sep + pairLen
	}

	// Final RFC 6265 path-length sort across admitted cookies (HttpOnly still ahead on equal path length).
	sortRFC(selected)

	var b strings.Builder
	b.Grow(totalBytes)
	for i, c := range selected {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.name)
		b.WriteByte('=')
		b.WriteString(c.wireValue())
	}
	return b.String()
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
	// RFC 6265bis: treat localhost as a public suffix (sub.localhost cannot set Domain=localhost).
	if d == "localhost" {
		if reqHost == "localhost" {
			return reqHost, true, true
		}
		return "", false, false
	}
	if !strings.HasSuffix(d, ".localhost") {
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

// ---- Split-Pool Bounded SessionCookieManager (Owner 256 / Guest 768) ----

type cookieSessionEntry struct {
	key        string
	jar        *ProxyCookieJar
	refCount   int
	lastActive time.Time
	seq        uint64
}

type sessionPool struct {
	entries map[string]*cookieSessionEntry
	maxSize int
	seq     uint64
}

func newSessionPool(maxSize int) *sessionPool {
	return &sessionPool{
		entries: make(map[string]*cookieSessionEntry),
		maxSize: maxSize,
	}
}

func (sp *sessionPool) touch(e *cookieSessionEntry, now time.Time) {
	sp.seq++
	e.lastActive = now
	e.seq = sp.seq
}

func (sp *sessionPool) evictIdle(now time.Time) {
	for k, e := range sp.entries {
		if e.refCount == 0 && now.Sub(e.lastActive) > CookieSessionIdleExpiry {
			delete(sp.entries, k)
		}
	}
	for len(sp.entries) >= sp.maxSize {
		var oldestKey string
		var oldestSeq uint64
		found := false
		for k, e := range sp.entries {
			if e.refCount != 0 {
				continue
			}
			if !found || e.seq < oldestSeq {
				found = true
				oldestKey = k
				oldestSeq = e.seq
			}
		}
		if !found {
			return
		}
		delete(sp.entries, oldestKey)
	}
}

// SessionCookieManager manages per-principal cookie jars across two isolated pools:
//   - ownerPool (256 reserved slots): public/capability floods can NEVER evict owner sessions.
//   - guestPool (768 slots): capability and public visitor sessions.
//
// Enforces CookieSessionIdleExpiry (2h) and strict maxSize hard cap (if all slots are actively
// pinned with refCount > 0, returns an untracked ephemeral jar rather than breaching maxSize).
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
				delete(sp.entries, candKey)
			} else {
				e.refCount++
				sp.touch(e, now)
				return candKey, requestedToken, e.jar
			}
		}
	}

	sp.evictIdle(now)
	token = newServerCookieToken()
	key = prefix + ":" + token
	jar = NewProxyCookieJar()
	if len(sp.entries) >= sp.maxSize {
		// Hard cap enforced: all slots are actively held; return an ephemeral untracked jar.
		return "", "", jar
	}
	e := &cookieSessionEntry{
		key:      key,
		jar:      jar,
		refCount: 1,
	}
	sp.touch(e, now)
	sp.entries[key] = e
	return key, token, jar
}

func (m *SessionCookieManager) Release(key string) {
	if key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sp := m.poolForPrefix(key)
	if e, ok := sp.entries[key]; ok {
		if e.refCount > 0 {
			e.refCount--
		}
		sp.touch(e, time.Now())
	}
}

// DeleteByPrefix deletes all jars matching prefix (e.g. "cap:<jti>:" on capability revocation).
func (m *SessionCookieManager) DeleteByPrefix(prefix string) {
	if prefix == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sp := range []*sessionPool{m.ownerPool, m.guestPool} {
		for k := range sp.entries {
			if strings.HasPrefix(k, prefix) {
				delete(sp.entries, k)
			}
		}
	}
}
