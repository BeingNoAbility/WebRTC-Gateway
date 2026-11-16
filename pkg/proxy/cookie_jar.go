package proxy

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

type sessionEntry struct {
	jar        http.CookieJar
	lastActive time.Time
}

// SessionCookieManager maintains isolated CookieJar instances for each WebRTC peer session.
type SessionCookieManager struct {
	mu          sync.RWMutex
	entries     map[string]*sessionEntry
	idleTTL     time.Duration
	stopJanitor chan struct{}
}

// DefaultSessionCookieTTL is the idle timeout before an inactive client session's cookies are purged.
const DefaultSessionCookieTTL = 2 * time.Hour

// NewSessionCookieManager creates a new SessionCookieManager with default 2-hour idle TTL.
func NewSessionCookieManager() *SessionCookieManager {
	return NewSessionCookieManagerWithTTL(DefaultSessionCookieTTL)
}

// NewSessionCookieManagerWithTTL creates a new SessionCookieManager with a custom idle expiration TTL.
func NewSessionCookieManagerWithTTL(idleTTL time.Duration) *SessionCookieManager {
	m := &SessionCookieManager{
		entries:     make(map[string]*sessionEntry),
		idleTTL:     idleTTL,
		stopJanitor: make(chan struct{}),
	}
	go m.janitorLoop()
	return m
}

func (m *SessionCookieManager) janitorLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopJanitor:
			return
		case <-ticker.C:
			m.reapExpiredSessions()
		}
	}
}

func (m *SessionCookieManager) reapExpiredSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for id, entry := range m.entries {
		if now.Sub(entry.lastActive) > m.idleTTL {
			delete(m.entries, id)
		}
	}
}

// Close stops the background expiration janitor.
func (m *SessionCookieManager) Close() {
	select {
	case <-m.stopJanitor:
	default:
		close(m.stopJanitor)
	}
}

// GetJar retrieves or creates a CookieJar for the specified sessionID or client token.
func (m *SessionCookieManager) GetJar(sessionID string) http.CookieJar {
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, exists := m.entries[sessionID]; exists {
		entry.lastActive = time.Now()
		return entry.jar
	}

	newJar, err := cookiejar.New(nil)
	if err != nil {
		return nil
	}
	m.entries[sessionID] = &sessionEntry{
		jar:        newJar,
		lastActive: time.Now(),
	}
	return newJar
}

// RemoveSession cleans up cookies when a session disconnects.
// Persistent client sessions (prefixed with "cl_") are retained across F5 reloads
// and are only cleaned up after the idle TTL expires.
func (m *SessionCookieManager) RemoveSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if strings.HasPrefix(sessionID, "cl_") {
		// Retain across page refreshes; update lastActive
		if entry, exists := m.entries[sessionID]; exists {
			entry.lastActive = time.Now()
		}
		return
	}

	delete(m.entries, sessionID)
}

// ExtractNonHttpOnlyCookies extracts Set-Cookie lines that do not have HttpOnly flag.
// These cookies can be safely synchronized to the browser's document.cookie.
func ExtractNonHttpOnlyCookies(setCookieHeaders []string) []string {
	var result []string
	for _, cookieStr := range setCookieHeaders {
		lower := strings.ToLower(cookieStr)
		if !strings.Contains(lower, "httponly") {
			result = append(result, cookieStr)
		}
	}
	return result
}

// InjectCookies attaches stored cookies from the session jar to the outgoing request.
func (m *SessionCookieManager) InjectCookies(sessionID string, req *http.Request) {
	jar := m.GetJar(sessionID)
	if jar == nil || req.URL == nil {
		return
	}

	cookies := jar.Cookies(req.URL)
	if len(cookies) == 0 {
		return
	}

	var cookiePairs []string
	// Existing Cookie header from client
	existing := req.Header.Get("Cookie")
	if existing != "" {
		cookiePairs = append(cookiePairs, existing)
	}

	for _, c := range cookies {
		cookiePairs = append(cookiePairs, c.String())
	}

	req.Header.Set("Cookie", strings.Join(cookiePairs, "; "))
}

// SaveCookies parses Set-Cookie headers from the response and stores them in the session jar.
func (m *SessionCookieManager) SaveCookies(sessionID string, targetURL *url.URL, respHeaders http.Header) {
	jar := m.GetJar(sessionID)
	if jar == nil || targetURL == nil {
		return
	}

	// Parse cookies using net/http helper
	var cookies []*http.Cookie
	for _, v := range respHeaders["Set-Cookie"] {
		header := http.Header{"Set-Cookie": {v}}
		resp := http.Response{Header: header}
		cookies = append(cookies, resp.Cookies()...)
	}

	if len(cookies) > 0 {
		jar.SetCookies(targetURL, cookies)
	}
}
