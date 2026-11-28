package signaling

import (
	"container/list"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultMaxIPConns          = 64
	DefaultMaxIPMessages       = 120  // per 10 seconds (unauthenticated/visitor client IP)
	DefaultMaxAgentSessionMsgs = 2000 // per 10 seconds (per authenticated Agent session)
	DefaultMaxIPUpgrades       = 120  // per 60 seconds
	DefaultMaxGlobalClientConn = 4096
	DefaultMaxGlobalAgentConn  = 1024
	maxTrackedIPs              = 16384
	maxAuthFailedIPs           = 8192
	authLockoutThreshold       = 10
	authLockoutDuration        = 5 * time.Minute
	authStripeCount            = 64
)

// AuthScopeKey combines normalized IP (/32 IPv4, /64 IPv6) with an optional scope (such as nodeID
// in per-node secret mode) so successful auth on one node cannot reset failures for another node.
func AuthScopeKey(rawIP, scope string) string {
	k := RateLimitKey(rawIP)
	if scope == "" {
		return k
	}
	return k + "\x00" + scope
}

// RateLimitKey normalizes an IP address for rate-limiting and brute-force lockouts:
// IPv4 addresses are tracked per /32; IPv6 addresses are normalized to their /64 prefix
// so attackers with an IPv6 /64 allocation cannot evade rate limits or flush lockouts.
func RateLimitKey(rawIP string) string {
	s := strings.TrimSpace(rawIP)
	addr, err := netip.ParseAddr(s)
	if err != nil {
		if s == "" {
			return "_unknown_"
		}
		return s
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is4() {
		return addr.String()
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}

type tokenBucket struct {
	key        string
	tokens     float64
	lastRefill time.Time
	elem       *list.Element
}

// IPRateLimiter provides O(1) bounded-LRU per-IP (/32 IPv4, /64 IPv6) rate limiting
// with strictly isolated Agent vs Client connection pools and an independent O(1)
// AuthLockoutTable (separate mutex — zero contention with live message forwarding).
type IPRateLimiter struct {
	mu                   sync.Mutex
	maxIPConns           int
	maxGlobalClientConns int
	maxGlobalAgentConns  int
	maxMsgsPer10s        float64
	maxUpgrPer60s        float64

	clientGlobalConns int
	clientConns       map[string]int
	agentGlobalConns  int
	agentConns        map[string]int

	msgBuckets       map[string]*tokenBucket
	msgLRU           *list.List
	upgrBuckets      map[string]*tokenBucket
	upgrLRU          *list.List
	agentUpgrBuckets map[string]*tokenBucket
	agentUpgrLRU     *list.List

	authLockout *AuthLockoutTable
	authStripes [authStripeCount]sync.Mutex
	rejections  atomic.Uint64
}

func NewIPRateLimiter(maxIPConns, maxMsgsPer10s int) *IPRateLimiter {
	if maxIPConns <= 0 {
		maxIPConns = DefaultMaxIPConns
	}
	if maxMsgsPer10s <= 0 {
		maxMsgsPer10s = DefaultMaxIPMessages
	}
	return &IPRateLimiter{
		maxIPConns:           maxIPConns,
		maxGlobalClientConns: DefaultMaxGlobalClientConn,
		maxGlobalAgentConns:  DefaultMaxGlobalAgentConn,
		maxMsgsPer10s:        float64(maxMsgsPer10s),
		maxUpgrPer60s:        float64(DefaultMaxIPUpgrades),
		clientConns:          make(map[string]int),
		agentConns:           make(map[string]int),
		msgBuckets:           make(map[string]*tokenBucket),
		msgLRU:               list.New(),
		upgrBuckets:          make(map[string]*tokenBucket),
		upgrLRU:              list.New(),
		agentUpgrBuckets:     make(map[string]*tokenBucket),
		agentUpgrLRU:         list.New(),
		authLockout: NewAuthLockoutTable(AuthLockoutConfig{
			MaxFailures:     authLockoutThreshold,
			LockoutDuration: authLockoutDuration,
			MaxPendingKeys:  maxAuthFailedIPs,
			MaxLockedKeys:   maxAuthFailedIPs,
			// Machine secrets (>= 128 bits) are immune to online brute-force under per-IP
			// lockout; disable global kill-switches so distributed floods cannot DoS all Agents.
			GlobalMaxFailures:      0,
			AllowEvictLockedOnFull: true,
		}),
	}
}

func (l *IPRateLimiter) Rejections() uint64 { return l.rejections.Load() }

// AllowUpgrade rate-limits visitor client WebSocket upgrades per IP (isolated from Agent upgrades).
func (l *IPRateLimiter) AllowUpgrade(ip string) bool {
	return l.allowBucket(RateLimitKey(ip), l.upgrBuckets, l.upgrLRU, l.maxUpgrPer60s, l.maxUpgrPer60s/60.0)
}

// AllowAgentUpgrade rate-limits Agent WebSocket upgrades in a dedicated pool so unauthenticated
// visitor floods (same NAT or distributed) can never starve legitimate Agents.
func (l *IPRateLimiter) AllowAgentUpgrade(ip string) bool {
	return l.allowBucket(RateLimitKey(ip), l.agentUpgrBuckets, l.agentUpgrLRU, l.maxUpgrPer60s, l.maxUpgrPer60s/60.0)
}

func (l *IPRateLimiter) AllowMessage(ip string) bool {
	return l.allowBucket(RateLimitKey(ip), l.msgBuckets, l.msgLRU, l.maxMsgsPer10s, l.maxMsgsPer10s/10.0)
}

func (l *IPRateLimiter) allowBucket(
	key string,
	m map[string]*tokenBucket,
	lru *list.List,
	burst float64,
	ratePerSec float64,
) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := m[key]
	if !ok {
		for len(m) >= maxTrackedIPs {
			back := lru.Back()
			if back == nil {
				break
			}
			victim := back.Value.(*tokenBucket)
			elapsed := now.Sub(victim.lastRefill).Seconds()
			refilled := victim.tokens
			if elapsed > 0 {
				refilled += elapsed * ratePerSec
			}
			// Fail-Closed on saturation: never evict an actively rate-limited bucket to hand
			// fresh full bursts to a distributed IP flood. Only evict fully-refilled idle keys.
			if refilled < burst {
				l.rejections.Add(1)
				return false
			}
			lru.Remove(back)
			delete(m, victim.key)
		}
		b = &tokenBucket{key: key, tokens: burst, lastRefill: now}
		b.elem = lru.PushFront(b)
		m[key] = b
	} else {
		lru.MoveToFront(b.elem)
		elapsed := now.Sub(b.lastRefill).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * ratePerSec
			if b.tokens > burst {
				b.tokens = burst
			}
			b.lastRefill = now
		}
	}
	if b.tokens < 1.0 {
		l.rejections.Add(1)
		return false
	}
	b.tokens -= 1.0
	return true
}

// AcquireConn reserves a visitor client WebSocket slot (isolated from Agent quota).
func (l *IPRateLimiter) AcquireConn(ip string) bool {
	key := RateLimitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clientGlobalConns >= l.maxGlobalClientConns || l.clientConns[key] >= l.maxIPConns {
		l.rejections.Add(1)
		return false
	}
	l.clientGlobalConns++
	l.clientConns[key]++
	return true
}

// ReleaseConn releases a visitor client WebSocket slot.
func (l *IPRateLimiter) ReleaseConn(ip string) {
	key := RateLimitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.clientGlobalConns > 0 {
		l.clientGlobalConns--
	}
	if n := l.clientConns[key]; n <= 1 {
		delete(l.clientConns, key)
	} else {
		l.clientConns[key] = n - 1
	}
}

// AcquireAgentConn reserves an authenticated Agent WebSocket slot (unauthenticated client floods
// can never starve authenticated Agents).
func (l *IPRateLimiter) AcquireAgentConn(ip string) bool {
	key := RateLimitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.agentGlobalConns >= l.maxGlobalAgentConns || l.agentConns[key] >= l.maxIPConns {
		l.rejections.Add(1)
		return false
	}
	l.agentGlobalConns++
	l.agentConns[key]++
	return true
}

// ReleaseAgentConn releases an Agent WebSocket slot.
func (l *IPRateLimiter) ReleaseAgentConn(ip string) {
	key := RateLimitKey(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.agentGlobalConns > 0 {
		l.agentGlobalConns--
	}
	if n := l.agentConns[key]; n <= 1 {
		delete(l.agentConns, key)
	} else {
		l.agentConns[key] = n - 1
	}
}

func (l *IPRateLimiter) LockAuthScope(ip, scope string) func() {
	key := AuthScopeKey(ip, scope)
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	stripe := &l.authStripes[h%authStripeCount]
	stripe.Lock()
	return stripe.Unlock
}

func (l *IPRateLimiter) IsAuthLockedScoped(ip, scope string) bool {
	allowed, _ := l.authLockout.Allow(AuthScopeKey(ip, scope), time.Now())
	if !allowed {
		l.rejections.Add(1)
		return true
	}
	return false
}

func (l *IPRateLimiter) RecordFailedAuthScoped(ip, scope string) bool {
	locked := l.authLockout.RecordFailure(AuthScopeKey(ip, scope), time.Now())
	if locked {
		l.rejections.Add(1)
	}
	return locked
}

func (l *IPRateLimiter) ResetFailedAuthScoped(ip, scope string) {
	l.authLockout.Reset(AuthScopeKey(ip, scope))
}

func (l *IPRateLimiter) IsAuthLocked(ip string) bool {
	return l.IsAuthLockedScoped(ip, "")
}

func (l *IPRateLimiter) RecordFailedAuth(ip string) bool {
	return l.RecordFailedAuthScoped(ip, "")
}

func (l *IPRateLimiter) ResetFailedAuth(ip string) {
	l.ResetFailedAuthScoped(ip, "")
}

// SessionMessageLimiter is an independent per-session token bucket for authenticated Agents
// so busy Agents serving up to 256 concurrent visitors are never throttled by per-IP visitor buckets.
type SessionMessageLimiter struct {
	tokens     float64
	burst      float64
	ratePerSec float64
	lastRefill time.Time
}

func NewSessionMessageLimiter(msgsPer10s int) *SessionMessageLimiter {
	if msgsPer10s <= 0 {
		msgsPer10s = DefaultMaxAgentSessionMsgs
	}
	b := float64(msgsPer10s)
	return &SessionMessageLimiter{
		tokens:     b,
		burst:      b,
		ratePerSec: b / 10.0,
		lastRefill: time.Now(),
	}
}

func (s *SessionMessageLimiter) Allow(now time.Time) bool {
	if elapsed := now.Sub(s.lastRefill).Seconds(); elapsed > 0 {
		s.tokens += elapsed * s.ratePerSec
		if s.tokens > s.burst {
			s.tokens = s.burst
		}
		s.lastRefill = now
	}
	if s.tokens < 1.0 {
		return false
	}
	s.tokens -= 1.0
	return true
}

// ParseTrustedProxies parses a comma-separated list of IPs/CIDRs (unmapping IPv4-mapped IPv6).
// Defaults to loopback-only (127.0.0.0/8, ::1/128) when empty. Fails closed on malformed entries
// or IPv4-mapped IPv6 prefixes shorter than /96 (which would otherwise widen to 0.0.0.0/0).
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []netip.Prefix{
			netip.MustParsePrefix("127.0.0.0/8"),
			netip.MustParsePrefix("::1/128"),
		}, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(trimmed, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.Contains(p, "/") {
			pref, err := netip.ParsePrefix(p)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", p, err)
			}
			addr := pref.Addr().WithZone("")
			if addr.Is4In6() {
				if pref.Bits() < 96 {
					return nil, fmt.Errorf("invalid trusted proxy IPv4-mapped IPv6 CIDR %q: prefix length must be >= 96", p)
				}
				pref = netip.PrefixFrom(addr.Unmap(), pref.Bits()-96)
			}
			out = append(out, pref.Masked())
			continue
		}
		addr, err := netip.ParseAddr(p)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy IP %q: %w", p, err)
		}
		addr = addr.WithZone("").Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("trusted proxies list %q produced no valid entries", raw)
	}
	return out, nil
}

// IsTrustedProxy checks whether rawIP belongs to trusted (handling IPv6 zones & IPv4-mapped IPv6).
func IsTrustedProxy(rawIP string, trusted []netip.Prefix) bool {
	addr, err := netip.ParseAddr(strings.TrimSpace(rawIP))
	if err != nil {
		return false
	}
	addr = addr.WithZone("").Unmap()
	for _, pref := range trusted {
		if pref.Contains(addr) {
			return true
		}
	}
	return false
}

var forbiddenClientIPHeaders = map[string]bool{
	"X-Forwarded-For":     true,
	"Forwarded":           true,
	"Host":                true,
	"Origin":              true,
	"Cookie":              true,
	"Authorization":       true,
	"Proxy-Authorization": true,
	"Connection":          true,
	"Upgrade":             true,
	"Content-Length":      true,
	"Transfer-Encoding":   true,
}

func isValidHeaderTokenByte(b byte) bool {
	if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

// NormalizeClientIPHeader validates and canonicalizes an optional single-value client IP header name
// (e.g. CF-Connecting-IP, Fastly-Client-IP, True-Client-IP, CloudFront-Viewer-Address, X-Real-IP).
// Rejects comma-separated multi-header lists, invalid RFC 9110 header tokens, and multi-hop headers
// such as X-Forwarded-For / Forwarded (Fail-Closed).
func NormalizeClientIPHeader(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if len(s) > 64 {
		return "", fmt.Errorf("client-ip-header %q exceeds 64 characters", s)
	}
	for i := 0; i < len(s); i++ {
		if !isValidHeaderTokenByte(s[i]) {
			return "", fmt.Errorf("client-ip-header %q contains invalid header token character (configure a single HTTP header name only)", s)
		}
	}
	canon := http.CanonicalHeaderKey(s)
	if forbiddenClientIPHeaders[canon] {
		return "", fmt.Errorf("client-ip-header %q is not allowed (X-Forwarded-For right-to-left peeling is already built in)", canon)
	}
	return canon, nil
}

// parseSingleClientIP parses a single authoritative client IP value, supporting both plain IPs
// (e.g. CF-Connecting-IP, Fastly-Client-IP, X-Real-IP) and IP:port pairs (e.g. CloudFront-Viewer-Address).
func parseSingleClientIP(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if addr, err := netip.ParseAddr(s); err == nil {
		return addr.WithZone("").Unmap().String(), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().WithZone("").Unmap().String(), true
	}
	return "", false
}

// ExtractClientIP resolves the authoritative client IP with strict trusted-proxy checks:
//  1. If the immediate peer does not match trusted CIDRs, always return remoteHost.
//  2. If clientIPHeader is configured and present, require exactly ONE valid single IP (or IP:port);
//     duplicate header lines or malformed values fail closed to remoteHost without falling through.
//  3. Otherwise perform strict right-to-left X-Forwarded-For peeling (no implicit X-Real-IP fallback).
func ExtractClientIP(r *http.Request, trusted []netip.Prefix, clientIPHeader string) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = strings.TrimSpace(r.RemoteAddr)
	}
	if addr, perr := netip.ParseAddr(remoteHost); perr == nil {
		remoteHost = addr.WithZone("").Unmap().String()
	}

	if !IsTrustedProxy(remoteHost, trusted) {
		return remoteHost
	}

	if hdr := strings.TrimSpace(clientIPHeader); hdr != "" {
		vals := r.Header.Values(hdr)
		if len(vals) != 1 {
			return remoteHost
		}
		if ip, ok := parseSingleClientIP(vals[0]); ok {
			return ip
		}
		return remoteHost
	}

	xffVals := r.Header.Values("X-Forwarded-For")
	if len(xffVals) > 0 {
		var hops []string
		for _, v := range xffVals {
			for _, part := range strings.Split(v, ",") {
				hops = append(hops, strings.TrimSpace(part))
			}
		}
		for i := len(hops) - 1; i >= 0; i-- {
			hop := hops[i]
			if hop == "" {
				return remoteHost
			}
			addr, perr := netip.ParseAddr(hop)
			if perr != nil {
				return remoteHost
			}
			clean := addr.WithZone("").Unmap().String()
			if !IsTrustedProxy(clean, trusted) {
				return clean
			}
		}
		return remoteHost
	}

	return remoteHost
}
