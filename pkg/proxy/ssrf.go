package proxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// IsRestrictedIP checks whether an IP address belongs to private (RFC 1918),
// loopback, link-local, multicast, carrier-grade NAT, or cloud metadata ranges.
func IsRestrictedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}

	// Check IPv4 specific ranges
	if ip4 := ip.To4(); ip4 != nil {
		// Carrier-Grade NAT (RFC 6598: 100.64.0.0/10)
		if ip4[0] == 100 && (ip4[1]&0xC0) == 64 {
			return true
		}
		// Cloud metadata 169.254.169.254 (AWS, GCP, Azure, OpenStack)
		if ip4[0] == 169 && ip4[1] == 254 && ip4[2] == 169 && ip4[3] == 254 {
			return true
		}
		// AWS ECS task metadata 169.254.170.2
		if ip4[0] == 169 && ip4[1] == 254 && ip4[2] == 170 && ip4[3] == 2 {
			return true
		}
		// Alibaba Cloud metadata 100.100.100.200
		if ip4[0] == 100 && ip4[1] == 100 && ip4[2] == 100 && ip4[3] == 200 {
			return true
		}
	} else {
		// IPv6 cloud metadata fd00:ec2::254
		if ip.String() == "fd00:ec2::254" {
			return true
		}
	}

	return false
}

// IsTargetHost checks whether the given host and port match the configured target backend URL.
func IsTargetHost(host, port string, target *url.URL) bool {
	if target == nil {
		return false
	}
	targetHost := target.Hostname()
	targetPort := target.Port()
	if targetPort == "" {
		if strings.EqualFold(target.Scheme, "https") || strings.EqualFold(target.Scheme, "wss") {
			targetPort = "443"
		} else {
			targetPort = "80"
		}
	}
	if port == "" {
		if strings.EqualFold(target.Scheme, "https") || strings.EqualFold(target.Scheme, "wss") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.EqualFold(host, targetHost) && port == targetPort
}

// CreateSafeDialContext creates a net.DialContext function that blocks SSRF attempts
// against private/restricted networks while preventing DNS Rebinding (TOCTOU) attacks.
func CreateSafeDialContext(target *url.URL) func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		// Direct access to target backend itself is always permitted
		if IsTargetHost(host, port, target) {
			return dialer.DialContext(ctx, network, addr)
		}

		// If host is an IP literal
		if ip := net.ParseIP(host); ip != nil {
			if IsRestrictedIP(ip) {
				return nil, fmt.Errorf("SSRF blocked: direct dial to restricted/private IP %s is forbidden", host)
			}
			return dialer.DialContext(ctx, network, addr)
		}

		// If host is a domain, resolve IPs using system resolver
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("SSRF check: DNS lookup failed for %s: %w", host, err)
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("SSRF check: no IP addresses found for %s", host)
		}

		for _, ip := range ips {
			if IsRestrictedIP(ip) {
				return nil, fmt.Errorf("SSRF blocked: %s resolves to restricted/private IP %s", host, ip)
			}
		}

		// TOCTOU mitigation: dial the first verified safe IP directly
		safeAddr := net.JoinHostPort(ips[0].String(), port)
		return dialer.DialContext(ctx, network, safeAddr)
	}
}
