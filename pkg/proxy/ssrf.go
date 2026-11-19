package proxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

var (
	nat64Net = func() *net.IPNet {
		_, n, _ := net.ParseCIDR("64:ff9b::/96")
		return n
	}()
	sixToFourNet = func() *net.IPNet {
		_, n, _ := net.ParseCIDR("2002::/16")
		return n
	}()
)

// IsRestrictedIP checks whether an IP address belongs to private (RFC 1918),
// loopback, link-local, multicast, carrier-grade NAT, or cloud metadata ranges.
func IsRestrictedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || ip.Equal(net.IPv4bcast) {
		return true
	}

	// Check IPv4 specific ranges
	if ip4 := ip.To4(); ip4 != nil {
		// Entire 0.0.0.0/8 network (e.g. 0.0.0.1 which resolves to loopback on Linux)
		if ip4[0] == 0 {
			return true
		}
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
		// IPv6 NAT64 translation prefix (64:ff9b::/96)
		if nat64Net != nil && nat64Net.Contains(ip) {
			return true
		}
		// IPv6 6to4 translation prefix (2002::/16)
		if sixToFourNet != nil && sixToFourNet.Contains(ip) {
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

// ValidateTrustedOrigin checks whether rawOrigin strictly belongs to rootDomain or targetURL,
// enforcing strict (Scheme, Host, Port) tuple verification to avoid cross-port and substring attacks.
func ValidateTrustedOrigin(rawOrigin, rootDomain string, targetURL *url.URL) bool {
	if rawOrigin == "" {
		return false
	}
	u, err := url.Parse(rawOrigin)
	if err != nil || u.Hostname() == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}

	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}

	// 1. Strict tuple comparison with targetURL (Scheme, Host, Port)
	if targetURL != nil && strings.EqualFold(host, strings.ToLower(targetURL.Hostname())) {
		targetPort := targetURL.Port()
		if targetPort == "" {
			if strings.EqualFold(targetURL.Scheme, "https") {
				targetPort = "443"
			} else {
				targetPort = "80"
			}
		}
		if port == targetPort {
			return true
		}
	}

	// 2. Suffix / Subdomain matching with rootDomain
	if rootDomain != "" {
		root := strings.ToLower(rootDomain)
		if host == root || strings.HasSuffix(host, "."+root) {
			return true
		}
	}
	return false
}

// IsPermittedProxyPort validates whether an explicit port is allowed for cross-origin proxying
// to prevent SSRF port scanning against non-standard internal services (e.g. SSH 22, Redis 6379, MySQL 3306).
func IsPermittedProxyPort(port string, targetURL *url.URL) bool {
	if port == "" {
		return true // Default scheme ports (80 / 443)
	}
	if targetURL != nil && port == targetURL.Port() {
		return true
	}
	switch port {
	case "80", "443", "8080", "8443":
		return true
	default:
		return false
	}
}
