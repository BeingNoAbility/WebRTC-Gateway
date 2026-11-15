package proxy

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
)

// SessionCookieManager maintains isolated CookieJar instances for each WebRTC peer session.
type SessionCookieManager struct {
	mu   sync.RWMutex
	jars map[string]http.CookieJar
}

// NewSessionCookieManager creates a new SessionCookieManager.
func NewSessionCookieManager() *SessionCookieManager {
	return &SessionCookieManager{
		jars: make(map[string]http.CookieJar),
	}
}

// GetJar retrieves or creates a CookieJar for the specified sessionID.
func (m *SessionCookieManager) GetJar(sessionID string) http.CookieJar {
	m.mu.RLock()
	jar, exists := m.jars[sessionID]
	m.mu.RUnlock()

	if exists {
		return jar
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double check
	if jar, exists = m.jars[sessionID]; exists {
		return jar
	}

	newJar, err := cookiejar.New(nil)
	if err != nil {
		// Fallback empty
		return nil
	}
	m.jars[sessionID] = newJar
	return newJar
}

// RemoveSession cleans up cookies when a session disconnects.
func (m *SessionCookieManager) RemoveSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jars, sessionID)
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
