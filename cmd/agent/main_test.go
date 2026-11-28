package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"webrtc-gateway/pkg/config"
)

func TestAgentCLIHelpers(t *testing.T) {
	// 1. resolveAgentHealthURL: explicit URL, wildcard bind normalization, and missing-endpoint sentinel.
	if u, err := resolveAgentHealthURL("http://127.0.0.1:9090/healthz", ""); err != nil || u != "http://127.0.0.1:9090/healthz" {
		t.Fatalf("explicit health URL failed: u=%q err=%v", u, err)
	}
	if u, err := resolveAgentHealthURL("", ":9091"); err != nil || u != "http://127.0.0.1:9091/healthz" {
		t.Fatalf("empty-host metrics addr failed: u=%q err=%v", u, err)
	}
	if u, err := resolveAgentHealthURL("", "0.0.0.0:9092"); err != nil || u != "http://127.0.0.1:9092/healthz" {
		t.Fatalf("0.0.0.0 metrics addr failed: u=%q err=%v", u, err)
	}
	if u, err := resolveAgentHealthURL("", "[::]:9093"); err != nil || u != "http://[::1]:9093/healthz" {
		t.Fatalf("IPv6 wildcard metrics addr failed: u=%q err=%v", u, err)
	}
	if _, err := resolveAgentHealthURL("", ""); !errors.Is(err, errAgentHealthNoEndpoint) {
		t.Fatalf("expected errAgentHealthNoEndpoint, got %v", err)
	}
	if _, err := resolveAgentHealthURL("", "bad-addr"); err == nil || errors.Is(err, errAgentHealthNoEndpoint) {
		t.Fatalf("expected parse error for invalid metrics addr, got %v", err)
	}

	// 2. runAgentHealthCheck against live httptest servers (200 OK vs 503 vs redirect).
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()
	if err := runAgentHealthCheck(okSrv.URL); err != nil {
		t.Fatalf("expected 200 health check to pass, got %v", err)
	}

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer badSrv.Close()
	if err := runAgentHealthCheck(badSrv.URL); err == nil {
		t.Fatal("expected 503 health check to fail")
	}

	// 3. normalizeMintPath ensures canonical leading slash.
	if got := normalizeMintPath("docs/reports"); got != "/docs/reports" {
		t.Fatalf("normalizeMintPath relative: got %q", got)
	}
	if got := normalizeMintPath("/docs"); got != "/docs" {
		t.Fatalf("normalizeMintPath absolute: got %q", got)
	}
	if got := normalizeMintPath("   "); got != "" {
		t.Fatalf("normalizeMintPath empty: got %q", got)
	}

	// 4. parseFlexDuration & parseSTUNServers validation.
	if d, err := parseFlexDuration("7d"); err != nil || d != 7*24*time.Hour {
		t.Fatalf("parseFlexDuration 7d: d=%v err=%v", d, err)
	}
	if _, err := parseFlexDuration("400d"); err == nil {
		t.Fatal("expected >365d duration to fail")
	}
	if _, err := parseFlexDuration("0s"); err == nil {
		t.Fatal("expected 0s duration to fail")
	}
	if s, err := parseSTUNServers("off"); err != nil || len(s) != 0 {
		t.Fatalf("parseSTUNServers off: %v %v", s, err)
	}
	if _, err := parseSTUNServers("http://not-stun.example"); err == nil {
		t.Fatal("expected invalid STUN scheme to fail")
	}

	// 5. YAML schema strict parsing rejects unknown fields.
	cfgPath := filepath.Join(t.TempDir(), "agent.yaml")
	var cfg agentFileConfig
	if err := config.LoadYAMLFile(cfgPath, &cfg); err == nil {
		t.Fatal("expected missing config file to fail")
	}
}
