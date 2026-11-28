package proxy

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
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

	// 2. Semicolon / CTL value injection via ApplyClientMutations must be rejected.
	jar.ApplyClientMutations(httpsURL, []ClientCookieMutation{
		{Op: "set", Name: "evil", Value: "1; sid=attacker; __Host-x=1", Path: "/app/page"},
		{Op: "set", Name: "evil_del", Value: "bad\x7fvalue", Path: "/"},
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
	// HttpOnly `sid` is structurally immune to JS mutation/deletion/shadowing.
	// On a secure HTTPS origin, non-HttpOnly Secure `csrf_sec` CAN be updated by JS per RFC 6265bis
	// (eliminating CookieMirror split-brain, H-18), while on a non-secure origin it remains protected.
	jar.ApplyClientMutations(httpsURL, []ClientCookieMutation{
		{Op: "set", Name: "sid", Value: "pwned", Path: "/app"},
		{Op: "del", Name: "sid", Path: "/"},
	})
	afterFlood := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if !strings.Contains(afterFlood, "sid=real-admin") || !strings.Contains(afterFlood, "csrf_sec=real-csrf") {
		t.Fatalf("JS flood evicted or altered serverCookies: got %q", afterFlood)
	}
	if strings.Contains(afterFlood, "pwned") {
		t.Fatalf("JS overwrote HttpOnly server cookie: got %q", afterFlood)
	}
	// Verify non-secure origin still protects Secure non-HttpOnly `csrf_sec` ("Leave Secure Cookies Alone").
	nonSecureURL, _ := url.Parse("http://app.example.com/app/page")
	jar.ApplyClientMutations(nonSecureURL, []ClientCookieMutation{
		{Op: "set", Name: "csrf_sec", Value: "downgraded", Path: "/"},
		{Op: "del", Name: "csrf_sec", Path: "/"},
	})
	afterNonSec := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if !strings.Contains(afterNonSec, "csrf_sec=real-csrf") || strings.Contains(afterNonSec, "downgraded") {
		t.Fatalf("non-secure JS mutated Secure cookie: got %q", afterNonSec)
	}
	// Verify HTTPS JS CAN update non-HttpOnly Secure `csrf_sec` cleanly (no split-brain).
	jar.ApplyClientMutations(httpsURL, []ClientCookieMutation{
		{Op: "set", Name: "csrf_sec", Value: "updated-by-js", Path: "/"},
	})
	afterHTTPSJS := jar.BuildCookieHeaderWithContext(httpsURL, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if !strings.Contains(afterHTTPSJS, "csrf_sec=updated-by-js") {
		t.Fatalf("HTTPS JS update to non-HttpOnly Secure cookie failed (split-brain): got %q", afterHTTPSJS)
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

func TestCookieJarSecurityRegressions(t *testing.T) {
	// P1-20: Pre-login JS cookie on specific path /api MUST NOT shadow post-login HttpOnly/Secure cookie on /.
	jar := NewProxyCookieJar()
	apiURL, _ := url.Parse("https://app.example.com/api/profile")
	rootURL, _ := url.Parse("https://app.example.com/")

	jar.ApplyClientMutations(rootURL, []ClientCookieMutation{
		{Op: "set", Name: "sid", Value: "evil-prelogin", Path: "/api"},
		{Op: "set", Name: "csrf", Value: "evil-csrf", Path: "/api"},
	})
	deltas := jar.SaveResponseCookies(rootURL, []string{
		"sid=real-admin; Path=/; HttpOnly; Secure; SameSite=Strict",
		"csrf=real-csrf; Path=/; Secure",
	})
	hdr := jar.BuildCookieHeaderWithContext(apiURL, CookieRequestContext{Method: "POST", SameOriginClient: true})
	if strings.Contains(hdr, "evil-prelogin") || strings.Contains(hdr, "evil-csrf") {
		t.Fatalf("P1-20 regression: pre-login JS /api cookie shadowed protected server cookie: %q", hdr)
	}
	if !strings.Contains(hdr, "sid=real-admin") || !strings.Contains(hdr, "csrf=real-csrf") {
		t.Fatalf("expected protected server cookies present, got %q", hdr)
	}
	sawSidDel := false
	sawCsrfDel := false
	for _, d := range deltas {
		if d.Op == "del" && d.Name == "sid" && d.Path == "/api" {
			sawSidDel = true
		}
		if d.Op == "del" && d.Name == "csrf" && d.Path == "/api" {
			sawCsrfDel = true
		}
	}
	if !sawSidDel || !sawCsrfDel {
		t.Fatalf("expected del deltas scrubbing swept /api JS cookies, got %+v", deltas)
	}

	// P1-21a: Mode 4.1 loopback HTTP target (e.g. local Nginx setting X-Forwarded-Proto: https) MUST support Secure cookies.
	loopbackURL, _ := url.Parse("http://127.0.0.1:18083/login")
	jarLoop := NewProxyCookieJar()
	jarLoop.SaveResponseCookies(loopbackURL, []string{
		"sid=loopback-secure; Path=/; HttpOnly; Secure; SameSite=Lax",
		"__Host-token=host-tok; Path=/; Secure",
	})
	loopHdr := jarLoop.BuildCookieHeaderWithContext(loopbackURL, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if !strings.Contains(loopHdr, "sid=loopback-secure") || !strings.Contains(loopHdr, "__Host-token=host-tok") {
		t.Fatalf("P1-21 regression: loopback Mode 4.1 dropped Secure/__Host- cookies: %q", loopHdr)
	}
	// Non-loopback plaintext HTTP must STILL reject Secure cookies ("Leave Secure Cookies Alone").
	lanHTTPURL, _ := url.Parse("http://192.0.2.20:8080/login")
	jarLAN := NewProxyCookieJar()
	jarLAN.SaveResponseCookies(lanHTTPURL, []string{"sid=lan-sec; Path=/; HttpOnly; Secure"})
	if lanHdr := jarLAN.BuildCookieHeaderWithContext(lanHTTPURL, CookieRequestContext{Method: "GET", SameOriginClient: true}); lanHdr != "" {
		t.Fatalf("non-loopback plaintext HTTP must reject Secure cookie, got %q", lanHdr)
	}

	// P1-21b: JS MUST be able to update ordinary non-HttpOnly non-Secure server cookies (no split-brain).
	jarJS := NewProxyCookieJar()
	jarJS.SaveResponseCookies(rootURL, []string{"theme=dark; Path=/"})
	jarJS.ApplyClientMutations(rootURL, []ClientCookieMutation{
		{Op: "set", Name: "theme", Value: "light", Path: "/"},
	})
	jsHdr := jarJS.BuildCookieHeaderWithContext(rootURL, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if jsHdr != "theme=light" {
		t.Fatalf("P1-21 regression: JS update to non-HttpOnly non-Secure cookie failed (split-brain): %q", jsHdr)
	}

	// P2-21: Release() updates LRU order so recently released session is NOT evicted ahead of older idle session.
	mgr := NewSessionCookieManager(4) // owner=1, guest=3
	k1, tok1, _ := mgr.AcquireOrMint("guest", "")
	k2, tok2, _ := mgr.AcquireOrMint("guest", "")
	k3, tok3, _ := mgr.AcquireOrMint("guest", "")
	// Release k2 and k3 first, then release k1 LAST so k1 is most-recently-used.
	mgr.Release(k2)
	mgr.Release(k3)
	mgr.Release(k1)
	// Minting a 4th guest session must evict k2 (oldest released), NOT k1!
	k4, _, _ := mgr.AcquireOrMint("guest", "")
	defer mgr.Release(k4)
	r1, _, _ := mgr.AcquireOrMint("guest", tok1)
	if r1 != k1 {
		t.Fatalf("P2-21 regression: most-recently-released session %q was evicted (got %q; tok2=%q tok3=%q)", k1, r1, tok2, tok3)
	}
	mgr.Release(r1)
}

func TestRegression_P2_OversizedClientMutationPreservesExistingCookie(t *testing.T) {
	u, _ := url.Parse("http://app.local/")
	jar := NewProxyCookieJar()
	jar.SaveResponseCookies(u, []string{"session_state=valid; Path=/"})

	// 4090-byte value passes isValidClientCookieValue (<= 4096) but entry.byteSize() exceeds 4096
	// once name + domain + path are included. The rejected write MUST NOT wipe the existing cookie!
	jar.ApplyClientMutations(u, []ClientCookieMutation{
		{Op: "set", Name: "session_state", Value: strings.Repeat("x", 4090), Path: "/"},
	})
	hdr := jar.BuildCookieHeaderWithContext(u, CookieRequestContext{Method: "GET", SameOriginClient: true})
	if !strings.Contains(hdr, "session_state=valid") {
		t.Fatalf("P2-1 regression: rejected oversized mutation wiped existing valid cookie: %q", hdr)
	}
}

func TestRegression_P0_1_CookieJarExportConcurrentRaceFree(t *testing.T) {
	u, _ := url.Parse("https://app.example/dashboard")
	jar := NewProxyCookieJar()
	jar.SaveResponseCookies(u, []string{
		"a=1; Path=/",
		"b=2; Path=/",
		"c=3; Path=/",
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(3)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = jar.ExportNonHttpOnlySnapshot(u)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = jar.BuildCookieHeaderWithContext(u, CookieRequestContext{SameOriginClient: true, Method: "GET"})
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				jar.ApplyClientMutations(u, []ClientCookieMutation{
					{Op: "set", Name: "prefs", Value: "en,zh", Path: "/"},
				})
			}
		}
	}()
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestRegression_P1_4_BrowserAlignedCookieValuesAndAttributePreservation(t *testing.T) {
	u, _ := url.Parse("https://app.example/")
	jar := NewProxyCookieJar()

	// 1. Server sets a non-HttpOnly Secure; SameSite=None cookie (e.g. XSRF-TOKEN).
	jar.SaveResponseCookies(u, []string{"xsrf=v1; Path=/; Secure; SameSite=None"})

	// 2. JS updates xsrf and writes real-world comma/space/JSON cookies.
	jar.ApplyClientMutations(u, []ClientCookieMutation{
		{Op: "set", Name: "xsrf", Value: "v2", Path: "/"},
		{Op: "set", Name: "prefs", Value: "en,zh", Path: "/"},
		{Op: "set", Name: "ui", Value: `{"theme":"dark","tz":"UTC +8"}`, Path: "/"},
	})

	sameSiteHdr := jar.BuildCookieHeaderWithContext(u, CookieRequestContext{SameOriginClient: true, Method: "GET"})
	if !strings.Contains(sameSiteHdr, "prefs=en,zh") || !strings.Contains(sameSiteHdr, `ui={"theme":"dark","tz":"UTC +8"}`) {
		t.Fatalf("P1-4 regression: browser-valid comma/space/JSON cookies were dropped: %q", sameSiteHdr)
	}
	// Cross-site request MUST still carry xsrf=v2 (Secure; SameSite=None inherited, not downgraded to Lax!).
	crossSiteHdr := jar.BuildCookieHeaderWithContext(u, CookieRequestContext{SameOriginClient: false, Method: "GET"})
	if crossSiteHdr != "xsrf=v2" {
		t.Fatalf("P1-4 regression: JS update downgraded Secure; SameSite=None cookie: crossSite=%q", crossSiteHdr)
	}
}
