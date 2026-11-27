package proxy

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyV7_CookieJarRFC6265bisAndMutations(t *testing.T) {
	jar := NewProxyCookieJar()
	httpsURL, _ := url.Parse("https://app.example.com/dashboard")
	httpURL, _ := url.Parse("http://app.example.com/dashboard")

	// Target sets HttpOnly Secure + non-HttpOnly + SameSite=Strict cookies.
	deltas := jar.SaveResponseCookies(httpsURL, []string{
		"sid=admin-secret; Path=/; HttpOnly; Secure; SameSite=Strict",
		"theme=dark; Path=/",
	})
	if len(deltas) != 1 || deltas[0].Name != "theme" || deltas[0].Value != "dark" || deltas[0].Op != "set" {
		t.Fatalf("unexpected non-HttpOnly deltas: %+v", deltas)
	}

	// RFC 6265: Cookie with unrelated Domain attribute must be rejected and emit zero deltas.
	badDomDeltas := jar.SaveResponseCookies(httpsURL, []string{"evil=1; Domain=other.example; Path=/"})
	if len(badDomDeltas) != 0 {
		t.Fatalf("mismatched Domain cookie emitted deltas: %+v", badDomDeltas)
	}

	// Plain HTTP must not overwrite or delete existing Secure cookie ("Leave Secure Cookies Alone").
	jar.SaveResponseCookies(httpURL, []string{"sid=downgraded; Path=/; Max-Age=-1"})

	// Cross-site subrequest (SameOriginClient=false, Fail-Closed) must not receive SameSite=Strict cookie.
	csHdr := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{
		Method: "POST", SameOriginClient: false,
	})
	if strings.Contains(csHdr, "sid=") {
		t.Fatalf("SameSite=Strict cookie leaked on cross-site request: %q", csHdr)
	}

	// Same-origin request receives both sid and theme.
	soHdr := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{
		Method: "GET", SameOriginClient: true,
	})
	if !strings.Contains(soHdr, "sid=admin-secret") || !strings.Contains(soHdr, "theme=dark") {
		t.Fatalf("missing expected cookies on same-origin: %q", soHdr)
	}

	// JS mutation attempting to overwrite HttpOnly or __Host- fails, while deleting non-HttpOnly `theme` succeeds.
	jar.ApplyClientMutations(httpsURL, []ClientCookieMutation{
		{Op: "set", Name: "sid", Value: "forged", Path: "/"},
		{Op: "set", Name: "__Host-evil", Value: "1", Path: "/"},
		{Op: "del", Name: "theme", Path: "/"},
	})
	afterDel := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{
		Method: "GET", SameOriginClient: true,
	})
	if !strings.Contains(afterDel, "sid=admin-secret") {
		t.Fatalf("HttpOnly cookie tampered: %q", afterDel)
	}
	if strings.Contains(afterDel, "__Host-evil") {
		t.Fatalf("JS forged __Host- cookie: %q", afterDel)
	}
	if strings.Contains(afterDel, "theme=") {
		t.Fatalf("JS-deleted cookie remained as zombie: %q", afterDel)
	}

	// SessionCookieManager.AcquireOrMint rejects client-chosen token fixation.
	mgr := NewSessionCookieManager(4)
	k1, tok1, j1 := mgr.AcquireOrMint("owner", "attacker-chosen-token")
	if tok1 == "attacker-chosen-token" || j1 == nil || k1 == "" {
		t.Fatalf("accepted client-chosen CookieResumeToken %q", tok1)
	}
	k2, tok2, j2 := mgr.AcquireOrMint("owner", tok1)
	if k2 != k1 || tok2 != tok1 || j2 != j1 {
		t.Fatalf("expected valid server-issued resume token to resume same jar")
	}
}

func TestCookieJarSplitPoolInjectionAndEvictionBlocked(t *testing.T) {
	jar := NewProxyCookieJar()
	httpsURL, _ := url.Parse("https://app.example.com/app/page")
	ipURL, _ := url.Parse("https://192.0.2.10/app/page")

	// 1. Upstream sets HttpOnly Secure session + Secure non-HttpOnly CSRF + plain non-HttpOnly pref.
	deltas := jar.SaveResponseCookies(httpsURL, []string{
		"sid=real-admin; Path=/; HttpOnly; Secure; SameSite=Strict",
		"csrf_sec=real-csrf; Path=/; Secure",
		"pref=v1; Path=/",
	})
	if len(deltas) != 2 {
		t.Fatalf("expected 2 non-HttpOnly deltas, got %+v", deltas)
	}

	// 2. Semicolon / space / CTL value injection via ApplyClientMutations must be rejected.
	jar.ApplyClientMutations(httpsURL, []ClientCookieMutation{
		{Op: "set", Name: "evil", Value: "1; sid=attacker; __Host-x=1", Path: "/app/page"},
		{Op: "set", Name: "evil_sp", Value: "bad value", Path: "/"},
		{Op: "set", Name: "evil_ctl", Value: "bad\r\nval", Path: "/"},
		{Op: "set", Name: "evil_path", Value: "ok", Path: "/;sid=hack"},
	})
	hdr := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if strings.Contains(hdr, "evil") || strings.Contains(hdr, "attacker") || strings.Contains(hdr, "__Host-x") {
		t.Fatalf("semicolon/CTL injection passed ApplyClientMutations: %q", hdr)
	}

	// 3. Flooding clientCookies (100 x 3.5 KiB) must never evict serverCookies.
	bigVal := strings.Repeat("A", 3500)
	for batch := 0; batch < 4; batch++ {
		var muts []ClientCookieMutation
		for i := 0; i < 25; i++ {
			muts = append(muts, ClientCookieMutation{
				Op: "set", Name: fmt.Sprintf("spam_%d_%d", batch, i), Value: bigVal, Path: "/",
			})
		}
		jar.ApplyClientMutations(httpsURL, muts)
	}
	jar.ApplyClientMutations(httpsURL, []ClientCookieMutation{
		{Op: "set", Name: "sid", Value: "pwned", Path: "/app"},
		{Op: "del", Name: "sid", Path: "/"},
		{Op: "set", Name: "csrf_sec", Value: "downgraded", Path: "/"},
		{Op: "del", Name: "csrf_sec", Path: "/"},
	})
	afterFlood := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if !strings.Contains(afterFlood, "sid=real-admin") || !strings.Contains(afterFlood, "csrf_sec=real-csrf") {
		t.Fatalf("JS flood evicted or altered serverCookies: got %q", afterFlood)
	}
	if strings.Contains(afterFlood, "pwned") || strings.Contains(afterFlood, "downgraded") {
		t.Fatalf("JS overwrote/downgraded server cookie: got %q", afterFlood)
	}

	// 4. __Host- with explicit Domain attribute must be rejected even on IP host.
	hostDomDeltas := jar.SaveResponseCookies(ipURL, []string{
		"__Host-bad=1; Domain=192.0.2.10; Path=/; Secure",
	})
	if len(hostDomDeltas) != 0 {
		t.Fatalf("__Host- with explicit Domain= was accepted on IP host: %+v", hostDomDeltas)
	}
	if ipHdr := jar.BuildCookieHeaderWithContext(ipURL, CookieRequestContext{Method: "GET", SameOriginClient: true}); strings.Contains(ipHdr, "__Host-bad") {
		t.Fatalf("__Host- with explicit Domain= stored in jar: %q", ipHdr)
	}

	// 5. Upgrading non-HttpOnly `pref` to HttpOnly must emit a "del" delta to scrub CookieMirror.
	upgradeDeltas := jar.SaveResponseCookies(httpsURL, []string{"pref=now-secret; Path=/; HttpOnly"})
	if len(upgradeDeltas) != 1 || upgradeDeltas[0].Op != "del" || upgradeDeltas[0].Name != "pref" {
		t.Fatalf("upgrading non-HttpOnly -> HttpOnly must emit del delta, got: %+v", upgradeDeltas)
	}

	// 6. Failed write (oversized > 8 KiB) must never emit a "set" delta.
	oversizedDeltas := jar.SaveResponseCookies(httpsURL, []string{"huge=" + strings.Repeat("x", 9000) + "; Path=/"})
	if len(oversizedDeltas) != 0 {
		t.Fatalf("failed oversized cookie write emitted delta: %+v", oversizedDeltas)
	}

	// 7. SessionCookieManager split pools: guest flood never evicts owner, and 2h idle TTL enforced.
	mgr := NewSessionCookieManager(16) // owner=4, guest=12
	ownerKey, ownerTok, ownerJar := mgr.AcquireOrMint("owner", "")
	ownerJar.SaveResponseCookies(httpsURL, []string{"owner_sid=keep; Path=/; HttpOnly"})
	mgr.Release(ownerKey)

	for i := 0; i < 50; i++ {
		gk, _, _ := mgr.AcquireOrMint(fmt.Sprintf("cap:jti-%d", i), "")
		mgr.Release(gk)
	}
	resumedKey, resumedTok, resumedJar := mgr.AcquireOrMint("owner", ownerTok)
	if resumedKey != ownerKey || resumedTok != ownerTok || resumedJar != ownerJar {
		t.Fatal("guest session flood evicted owner cookie jar")
	}
	mgr.Release(resumedKey)

	expiredKey, expiredTok, expiredJar := mgr.acquireOrMintAt("owner", ownerTok, time.Now().Add(CookieSessionIdleExpiry+time.Minute))
	if expiredKey == ownerKey || expiredTok == ownerTok || expiredJar == ownerJar {
		t.Fatal("SessionCookieManager resurrected idle-expired cookie jar after 2h TTL")
	}
}
