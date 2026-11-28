package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webrtc-gateway/pkg/protocol"
)

func TestSecurityRegressions_H7_Percent2FBypassBlocked(t *testing.T) {
	// H-7a: TargetSubpath confinement MUST NOT be bypassed by `/app%2Fsecret`.
	polSub, err := NewDestinationPolicy("http://192.0.2.10:8080/app", "")
	if err != nil {
		t.Fatal(err)
	}
	dest, err := polSub.Resolve("/app%2Fsecret", false)
	if err != nil {
		t.Fatal(err)
	}
	if wire := dest.URL.EscapedPath(); !strings.HasPrefix(wire, "/app/") {
		t.Fatalf("H-7a regression: /app%%2Fsecret escaped TargetSubpath=/app on wire: %q", wire)
	}

	// H-7b: CapabilityScope.PathPrefix MUST NOT be bypassed by `/public%2Fsecret`.
	polRoot, err := NewDestinationPolicy("http://192.0.2.10:8080", "")
	if err != nil {
		t.Fatal(err)
	}
	destCap, err := polRoot.Resolve("/public%2Fsecret", false)
	if err != nil {
		t.Fatal(err)
	}
	scope := &CapabilityScope{PathPrefix: "/public", Readonly: true}
	if scope.AllowsDestination(protocol.StreamKindHTTP, http.MethodGet, destCap) {
		t.Fatalf("H-7b regression: /public%%2Fsecret bypassed CapabilityScope PathPrefix=/public (wire=%q)", destCap.URL.EscapedPath())
	}

	// Legitimate %2F strictly INSIDE `/public/` MUST still be allowed (GitLab / S3 compatibility).
	destLegit, err := polRoot.Resolve("/public/group%2Frepo", false)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.AllowsDestination(protocol.StreamKindHTTP, http.MethodGet, destLegit) {
		t.Fatalf("legitimate /public/group%%2Frepo should be allowed under /public")
	}
}

func TestSecurityRegressions_H8_StaticSymlinkScopeBlocked(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmpDir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "secrets", "payroll.csv"), []byte("alice,999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "docs", "readme.txt"), []byte("public-doc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../secrets/payroll.csv", filepath.Join(tmpDir, "docs", "ref")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("readme.txt", filepath.Join(tmpDir, "docs", "in_scope_link")); err != nil {
		t.Fatal(err)
	}

	// 1. Default mode (FollowSymlinks=false): ALL symlinks rejected.
	hDefault, err := NewStaticFSHandler(StaticFSOptions{RootDir: tmpDir, HideDotfiles: true})
	if err != nil {
		t.Fatal(err)
	}
	defer hDefault.Close()

	req := httptest.NewRequest(http.MethodGet, "http://static.internal/docs/ref", nil)
	req = req.WithContext(WithStaticPathPrefix(req.Context(), "/docs"))
	rr := httptest.NewRecorder()
	hDefault.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("H-8 regression (default): symlink escaped PathPrefix /docs: %d %q", rr.Code, rr.Body.String())
	}

	// 2. Opt-in FollowSymlinks=true: escaping /docs -> ../secrets/payroll.csv MUST STILL BE BLOCKED,
	//    while in-scope docs/in_scope_link -> readme.txt is permitted!
	hFollow, err := NewStaticFSHandler(StaticFSOptions{RootDir: tmpDir, HideDotfiles: true, FollowSymlinks: true})
	if err != nil {
		t.Fatal(err)
	}
	defer hFollow.Close()

	rrEsc := httptest.NewRecorder()
	hFollow.ServeHTTP(rrEsc, req)
	if rrEsc.Code == http.StatusOK {
		t.Fatalf("H-8 regression (FollowSymlinks=true): symlink escaped PathPrefix /docs: %d %q", rrEsc.Code, rrEsc.Body.String())
	}

	reqOk := httptest.NewRequest(http.MethodGet, "http://static.internal/docs/in_scope_link", nil)
	reqOk = reqOk.WithContext(WithStaticPathPrefix(reqOk.Context(), "/docs"))
	rrOk := httptest.NewRecorder()
	hFollow.ServeHTTP(rrOk, reqOk)
	if rrOk.Code != http.StatusOK || strings.TrimSpace(rrOk.Body.String()) != "public-doc" {
		t.Fatalf("in-scope symlink under FollowSymlinks=true failed: %d %q", rrOk.Code, rrOk.Body.String())
	}

	// M-17: 301 redirect and 404 responses MUST include nosniff and CSP: sandbox.
	rr301 := httptest.NewRecorder()
	hDefault.ServeHTTP(rr301, httptest.NewRequest(http.MethodGet, "http://static.internal/docs", nil))
	if rr301.Code != http.StatusMovedPermanently || rr301.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rr301.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("M-17 regression: 301 missing security headers: code=%d hdr=%v", rr301.Code, rr301.Header())
	}
}

func TestSecurityRegressions_H9_M12_M16_PolicyHardening(t *testing.T) {
	pol, err := NewDestinationPolicy("https://app.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	dest, err := pol.Resolve("/", false)
	if err != nil {
		t.Fatal(err)
	}

	// H-9a: HTAB in protocol-relative Location MUST be stripped and collapsed to single '/'.
	if got := pol.RewriteLocationHeader("/\t/evil.example", dest); got != "/evil.example" {
		t.Fatalf("H-9a regression: HTAB bypassed protocol-relative collapse: got %q", got)
	}
	// H-9b: HTAB in same-origin Location MUST be stripped and rewritten (no internal origin leak).
	if got := pol.RewriteLocationHeader("https://app.example.com/next?q=a\tb#frag%201", dest); got != "/next?q=ab#frag%201" {
		t.Fatalf("H-9b regression: HTAB or fragment escaping broken: got %q", got)
	}

	// M-12: Scheme-less `@evil.example/steal` Referer MUST NOT produce `https://app.example.com@evil.example/steal`.
	hdr := pol.SanitizeRequestHeaders(
		[]protocol.HeaderField{
			{Name: "referer", Value: "@evil.example/steal"},
			{Name: "x-forwarded-prefix", Value: "/evil"},
			{Name: "x-original-host", Value: "evil.example"},
			{Name: "x-client-ip", Value: "1.2.3.4"},
		},
		protocol.FetchCtxSameOriginClient, http.MethodGet, dest, "198.51.100.1", nil, false, false,
	)
	if ref := hdr.Get("Referer"); ref != "" {
		t.Fatalf("M-12 regression: scheme-less Referer spoofed authority: %q", ref)
	}
	for _, h := range []string{"X-Forwarded-Prefix", "X-Original-Host", "X-Client-Ip"} {
		if hdr.Get(h) != "" {
			t.Fatalf("M-16 regression: forbidden proxy header %s was not stripped", h)
		}
	}

	// M-16: NewDestinationPolicy rejects userinfo, query, fragment, and invalid port.
	for _, bad := range []string{
		"http://user:pass@127.0.0.1:8080",
		"http://127.0.0.1:8080/app?x=1",
		"http://127.0.0.1:8080/app#frag",
		"http://127.0.0.1:99999",
	} {
		if _, err := NewDestinationPolicy(bad, ""); err == nil {
			t.Fatalf("M-16 regression: expected NewDestinationPolicy(%q) to fail closed", bad)
		}
	}
	// Post-clean /__p2p_proxy__/ rejection.
	if _, err := pol.Resolve("/./__p2p_proxy__/evil", false); err == nil {
		t.Fatalf("M-16 regression: /./__p2p_proxy__/ bypassed prefix check")
	}
}

func TestSecurityRegressions_H11_M13_M14_CookieJar(t *testing.T) {
	jar := NewProxyCookieJar()
	u, _ := url.Parse("https://app.example.com/sub/page")

	// H-11b: Quoted server cookie preserves quotes across round-trip.
	deltas := jar.SaveResponseCookies(u, []string{`prefs="a, b"; Path=/; Secure`})
	if len(deltas) != 1 || deltas[0].Value != `"a, b"` {
		t.Fatalf("H-11b regression: quoted cookie delta lost quotes: %+v", deltas)
	}
	if got := jar.BuildCookieHeaderWithContext(u, CookieRequestContext{Method: "GET", SameOriginClient: true}); got != `prefs="a, b"` {
		t.Fatalf("H-11b regression: quoted cookie corrupted on Cookie header: %q", got)
	}

	// H-11a: Serialized Cookie header capped at MaxSerializedCookieHeaderBytes and client bucket at 4 KiB.
	jar.SaveResponseCookies(u, []string{"sid=root-auth; Path=/; HttpOnly; Secure"})
	for i := 0; i < 16; i++ {
		jar.ApplyClientMutations(u, []ClientCookieMutation{{
			Op:    "set",
			Name:  "big_" + strings.Repeat("k", 4) + string(rune('a'+i)),
			Value: strings.Repeat("V", 1000),
			Path:  "/sub/page",
		}})
	}
	hdr := jar.BuildCookieHeaderWithContext(u, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if len(hdr) > MaxSerializedCookieHeaderBytes {
		t.Fatalf("H-11a regression: Cookie header exceeded budget: %d > %d", len(hdr), MaxSerializedCookieHeaderBytes)
	}
	if !strings.Contains(hdr, "sid=root-auth") {
		t.Fatalf("H-11a regression: JS deep-path cookies crowded out HttpOnly sid: %q", hdr)
	}
	if len(hdr) > MaxSerializedClientCookieBytes+512 {
		t.Fatalf("H-11a regression: JS cookies exceeded 4 KiB client serialization cap: len=%d", len(hdr))
	}

	// M-13: Parent-domain non-HttpOnly cookie superseded cleanly by JS update without duplicate stale value.
	jarParent := NewProxyCookieJar()
	jarParent.SaveResponseCookies(u, []string{"theme=dark; Domain=example.com; Path=/"})
	jarParent.ApplyClientMutations(u, []ClientCookieMutation{{Op: "set", Name: "theme", Value: "light", Path: "/"}})
	if got := jarParent.BuildCookieHeaderWithContext(u, CookieRequestContext{Method: "GET", SameOriginClient: true}); got != "theme=light" {
		t.Fatalf("M-13 regression: parent-domain server cookie shadowed JS update: %q", got)
	}

	// M-14: localhost treated as PSL (sub.localhost cannot mint Domain=localhost).
	subLocal, _ := url.Parse("http://sub.localhost:8080/")
	if _, _, ok := validateCookieDomain(subLocal.Hostname(), "localhost"); ok {
		t.Fatalf("M-14 regression: sub.localhost allowed Domain=localhost supercookie")
	}

	// M-14: SessionCookieManager enforces hard cap when all entries have refCount > 0.
	mgr := NewSessionCookieManager(4) // owner=1, guest=3
	k1, _, _ := mgr.AcquireOrMint("owner", "")
	k2, tok2, j2 := mgr.AcquireOrMint("owner", "")
	if k1 == "" {
		t.Fatal("first owner acquire should be tracked")
	}
	if k2 != "" || tok2 != "" || j2 == nil {
		t.Fatalf("M-14 regression: exceeded ownerPool maxSize=1 while pinned: k2=%q tok2=%q", k2, tok2)
	}
	mgr.Release(k1)
}
