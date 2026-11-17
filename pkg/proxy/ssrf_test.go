package proxy

import (
	"context"
	"net"
	"net/url"
	"testing"
)

func TestIsRestrictedIP(t *testing.T) {
	tests := []struct {
		ip       string
		expected bool
	}{
		// Private IPv4 (RFC 1918)
		{"10.0.0.1", true},
		{"10.254.0.1", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"192.168.1.1", true},
		{"192.168.100.50", true},

		// Loopback
		{"127.0.0.1", true},
		{"127.0.1.1", true},
		{"::1", true},

		// Cloud metadata & Link-local
		{"169.254.169.254", true},
		{"169.254.170.2", true},
		{"169.254.1.1", true},
		{"100.100.100.200", true}, // Alibaba Cloud metadata
		{"100.64.0.1", true},      // CGNAT
		{"fd00:ec2::254", true},   // IPv6 metadata

		// Multicast, Broadcast, 0.0.0.0/8 & Unspecified
		{"0.0.0.0", true},
		{"0.0.0.1", true},
		{"0.255.255.255", true},
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"255.255.255.255", true},

		// IPv6 translation prefixes
		{"64:ff9b::1.1.1.1", true},
		{"2002:c0a8:0101::1", true},

		// Public IPs (should be allowed)
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		{"114.114.114.114", false},
		{"140.82.121.4", false}, // GitHub
		{"2606:4700:4700::1111", false},
	}

	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if ip == nil {
			t.Fatalf("Failed to parse test IP %s", tt.ip)
		}
		got := IsRestrictedIP(ip)
		if got != tt.expected {
			t.Errorf("IsRestrictedIP(%s) = %v; want %v", tt.ip, got, tt.expected)
		}
	}
}

func TestIsTargetHost(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:8080/app")
	if !IsTargetHost("127.0.0.1", "8080", target) {
		t.Errorf("Expected 127.0.0.1:8080 to match target")
	}
	if IsTargetHost("127.0.0.1", "8081", target) {
		t.Errorf("Expected 127.0.0.1:8081 not to match target")
	}
	if IsTargetHost("192.168.1.1", "8080", target) {
		t.Errorf("Expected 192.168.1.1:8080 not to match target")
	}

	targetDef, _ := url.Parse("https://mybackend.local")
	if !IsTargetHost("mybackend.local", "443", targetDef) {
		t.Errorf("Expected mybackend.local:443 to match default https target")
	}
}

func TestSafeDialContextBlocksPrivate(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:9099")
	dialFn := CreateSafeDialContext(target)

	// Dialing arbitrary private IP must fail with SSRF blocked
	ctx := context.Background()
	_, err := dialFn(ctx, "tcp", "10.0.0.1:80")
	if err == nil {
		t.Errorf("Expected dial to 10.0.0.1:80 to be blocked, but succeeded")
	} else if !IsSSRFBlockedError(err) {
		t.Errorf("Expected SSRF blocked error, got: %v", err)
	}

	_, err = dialFn(ctx, "tcp", "169.254.169.254:80")
	if err == nil {
		t.Errorf("Expected dial to metadata 169.254.169.254 to be blocked, but succeeded")
	}
}

func IsSSRFBlockedError(err error) bool {
	return err != nil && (len(err.Error()) > 12 && (err.Error()[:12] == "SSRF blocked" || err.Error()[:10] == "SSRF check"))
}

func TestResolveTargetURL_SSRF_AuthorityConfusionAndSubdomainEscape(t *testing.T) {
	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy("http://127.0.0.1:8080", cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
	}

	// 1. Prohibit @ authority confusion attack: a.127.0.0.1:80@169.254.169.254:80
	_, _, err = hp.resolveTargetURL("/--p2p-proxy/a.127.0.0.1:80@169.254.169.254:80/latest/meta-data/", nil)
	if err == nil {
		t.Errorf("CRITICAL: Expected SSRF block for @ authority confusion attack, but got nil")
	}

	// 2. Prohibit IP subdomain matching: a.127.0.0.1 should NOT match target 127.0.0.1
	_, _, err = hp.resolveTargetURL("/--p2p-proxy/a.127.0.0.1/test", nil)
	if err == nil {
		t.Errorf("CRITICAL: Subdomain of raw IP 127.0.0.1 should not be allowed, but got nil")
	}

	// 3. Prohibit 0.0.0.1 (0.0.0.0/8 loopback alias on Linux)
	_, _, err = hp.resolveTargetURL("/--p2p-proxy/0.0.0.1:8080/secret", nil)
	if err == nil {
		t.Errorf("CRITICAL: 0.0.0.1 should be blocked as restricted IP, but got nil")
	}

	// 4. Prohibit cloud metadata
	_, _, err = hp.resolveTargetURL("/--p2p-proxy/169.254.169.254:80/meta-data", nil)
	if err == nil {
		t.Errorf("CRITICAL: 169.254.169.254 should be blocked, but got nil")
	}

	// 5. Prohibit illegal characters (backslashes, whitespace)
	_, _, err = hp.resolveTargetURL("/--p2p-proxy/evil.com\\127.0.0.1/test", nil)
	if err == nil {
		t.Errorf("CRITICAL: Backslash in destHost should be blocked, but got nil")
	}
	_, _, err = hp.resolveTargetURL("/--p2p-proxy/evil.com 127.0.0.1/test", nil)
	if err == nil {
		t.Errorf("CRITICAL: Whitespace in destHost should be blocked, but got nil")
	}

	// 6. Prohibit same for WSProxy
	wp, err := NewWSProxy("ws://127.0.0.1:8080/ws", cm)
	if err != nil {
		t.Fatalf("NewWSProxy failed: %v", err)
	}
	_, _, err = wp.resolveTargetWSURL("/--p2p-proxy/a.127.0.0.1:80@169.254.169.254:80/ws", nil)
	if err == nil {
		t.Errorf("CRITICAL: WSProxy expected SSRF block for @ attack, got nil")
	}
}

func TestHTTPProxy_InsecureTLS_TargetOnly(t *testing.T) {
	cm := NewSessionCookieManager()
	hp, err := NewHTTPProxy("https://127.0.0.1:8443", cm)
	if err != nil {
		t.Fatalf("NewHTTPProxy failed: %v", err)
	}

	// Default: InsecureSkipVerify must be false on secureTransport
	if hp.secureTransport.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("secureTransport InsecureSkipVerify must default to false")
	}

	// Enable -insecure on agent
	hp.SetInsecureTLS(true)
	if !hp.insecureTLS {
		t.Errorf("insecureTLS should be true after SetInsecureTLS(true)")
	}

	// Ensure secureTransport remains InsecureSkipVerify: false (for external proxy domains)
	if hp.secureTransport.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("secureTransport InsecureSkipVerify must NEVER be true (external domains must strictly verify CA)")
	}
	// While insecureTransport is available with InsecureSkipVerify: true strictly for TargetHost
	if !hp.insecureTransport.TLSClientConfig.InsecureSkipVerify {
		t.Errorf("insecureTransport InsecureSkipVerify should be true")
	}
}
