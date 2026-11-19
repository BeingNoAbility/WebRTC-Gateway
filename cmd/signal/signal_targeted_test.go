package main

import (
	"net/http"
	"net/url"
	"testing"
)

// 1. Adversarial Origin headers targeting CSWSH defenses (commits a308dc8, bbf93e8)
func TestSignal_OriginChecker_AdversarialOrigins(t *testing.T) {
	// Strict checker with whitelist "https://portal.example.com:8443"
	ocStrict := NewOriginChecker("https://portal.example.com:8443", false)

	// Permissive checker with allowAll=true
	ocPermissive := NewOriginChecker("", true)

	tests := []struct {
		name        string
		checker     *OriginChecker
		origin      string
		host        string
		expected    bool
		description string
	}{
		// A. Sandboxed iframe attacks
		{
			name:        "Sandboxed iframe 'null' origin is blocked",
			checker:     ocStrict,
			origin:      "null",
			host:        "portal.example.com:8443",
			expected:    false,
			description: "Browsers emit Origin: null for sandboxed iframes; must be blocked",
		},
		{
			name:        "Data URI origin is blocked",
			checker:     ocStrict,
			origin:      "data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
			host:        "portal.example.com:8443",
			expected:    false,
			description: "Data URI pseudo-origins must be blocked",
		},
		{
			name:        "Malformed URI origin is blocked",
			checker:     ocStrict,
			origin:      "://malformed-url",
			host:        "portal.example.com:8443",
			expected:    false,
			description: "Malformed origin string must fail safely",
		},

		// B. Native / CLI clients without Origin header
		{
			name:        "CLI agent without Origin header passes through",
			checker:     ocStrict,
			origin:      "",
			host:        "portal.example.com:8443",
			expected:    true,
			description: "Go CLI agent, curl, and backend bots do not send Origin header; pass to auth",
		},

		// C. Same-origin validation (Host == Origin host:port)
		{
			name:        "Same-origin HTTP request is allowed",
			checker:     ocStrict,
			origin:      "http://192.168.1.50:8080",
			host:        "192.168.1.50:8080",
			expected:    true,
			description: "Host matching Origin is trusted same-origin",
		},
		{
			name:        "Same-origin IPv6 request is allowed",
			checker:     ocStrict,
			origin:      "http://[::1]:8080",
			host:        "[::1]:8080",
			expected:    true,
			description: "IPv6 Host matching Origin is trusted same-origin",
		},

		// D. Cross-origin requests against whitelist
		{
			name:        "Whitelisted cross-origin request is allowed",
			checker:     ocStrict,
			origin:      "https://portal.example.com:8443",
			host:        "signal.example.com:8443",
			expected:    true,
			description: "Origin explicitly matches allowed whitelist pattern",
		},
		{
			name:        "Unauthorized cross-origin request is blocked",
			checker:     ocStrict,
			origin:      "https://attacker.com",
			host:        "signal.example.com:8443",
			expected:    false,
			description: "Unlisted origin blocked under strict CSWSH mode",
		},

		// E. Permissive mode (allowAll=true)
		{
			name:        "Permissive mode allows cross-origin with warning",
			checker:     ocPermissive,
			origin:      "https://attacker.com",
			host:        "signal.example.com:8443",
			expected:    true,
			description: "allowAll=true allows cross-origin requests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", "http://"+tt.host+"/ws", nil)
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			allowed := tt.checker.CheckOrigin(req)
			if allowed != tt.expected {
				t.Errorf("%s: CheckOrigin = %v, expected %v (%s)", tt.name, allowed, tt.expected, tt.description)
			}
		})
	}
}

// 2. Comprehensive decoupled host/port pattern matching (commit a308dc8)
func TestSignal_DecoupledHostPort_EdgeCases(t *testing.T) {
	testCases := []struct {
		name     string
		pattern  string
		origin   string
		expected bool
	}{
		// Port wildcard :*
		{
			name:     "Pattern with port wildcard matches arbitrary port",
			pattern:  "https://api.example.com:*",
			origin:   "https://api.example.com:9000",
			expected: true,
		},
		{
			name:     "Pattern with port wildcard matches standard https (443)",
			pattern:  "https://api.example.com:*",
			origin:   "https://api.example.com:443",
			expected: true,
		},
		{
			name:     "Subdomain wildcard with port wildcard",
			pattern:  "https://*.example.com:*",
			origin:   "https://sub.corp.example.com:3000",
			expected: true,
		},

		// Port normalization (default ports 80/443)
		{
			name:     "Pattern with port 443 matches origin with implicit 443",
			pattern:  "https://example.com:443",
			origin:   "https://example.com",
			expected: true,
		},
		{
			name:     "Pattern with port 80 matches origin with implicit 80",
			pattern:  "http://example.com:80",
			origin:   "http://example.com",
			expected: true,
		},
		{
			name:     "Pattern with explicit non-standard port rejects implicit 80",
			pattern:  "http://example.com:8080",
			origin:   "http://example.com",
			expected: false,
		},

		// IPv6 origins
		{
			name:     "IPv6 exact match with port",
			pattern:  "http://[::1]:8080",
			origin:   "http://[::1]:8080",
			expected: true,
		},
		{
			name:     "IPv6 with port wildcard :*",
			pattern:  "http://[::1]:*",
			origin:   "http://[::1]:5000",
			expected: true,
		},
		{
			name:     "IPv6 address mismatch rejected",
			pattern:  "http://[::1]:8080",
			origin:   "http://[::2]:8080",
			expected: false,
		},
		{
			name:     "IPv6 port mismatch rejected",
			pattern:  "http://[::1]:8080",
			origin:   "http://[::1]:9090",
			expected: false,
		},

		// Anti-spoofing: IP addresses must NOT match subdomain wildcards
		{
			name:     "IP address rejecting *.168.1.1 pattern",
			pattern:  "http://*.168.1.1:8080",
			origin:   "http://192.168.1.1:8080",
			expected: false,
		},
		{
			name:     "IP address rejecting *.0.0.1 pattern",
			pattern:  "http://*.0.0.1",
			origin:   "http://127.0.0.1",
			expected: false,
		},

		// Domain boundary isolation
		{
			name:     "Subdomain wildcard rejects similar domain prefix attack",
			pattern:  "https://*.example.com",
			origin:   "https://attacker-example.com",
			expected: false,
		},
		{
			name:     "Subdomain wildcard rejects similar domain suffix attack",
			pattern:  "https://*.example.com",
			origin:   "https://example.com.attacker.com",
			expected: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.origin)
			if err != nil {
				t.Fatalf("Failed to parse origin %q: %v", tc.origin, err)
			}
			actual := matchOriginPattern(tc.pattern, tc.origin, u)
			if actual != tc.expected {
				t.Errorf("matchOriginPattern(%q, %q) = %v, expected %v", tc.pattern, tc.origin, actual, tc.expected)
			}
		})
	}
}
