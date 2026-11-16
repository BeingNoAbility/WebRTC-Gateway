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

		// Multicast & Unspecified
		{"0.0.0.0", true},
		{"224.0.0.1", true},

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
