package proxy

import (
	"net/url"
	"strings"
	"testing"
)

func FuzzApplyClientMutations(f *testing.F) {
	f.Add("set", "theme", "dark", "/")
	f.Add("set", "a", "1; sid=attacker", "/app")
	f.Add("set", "sid", "forged", "/")
	f.Add("set", "__Host-x", "1", "/")
	f.Add("del", "sid", "", "/")
	f.Add("set", "bad\r\nname", "ok", "/;evil")

	targetURL, _ := url.Parse("https://app.example.com/app/page")
	f.Fuzz(func(t *testing.T, op, name, val, p string) {
		jar := NewProxyCookieJar()
		jar.SaveResponseCookies(targetURL, []string{
			"sid=protected-admin; Path=/; HttpOnly; Secure; SameSite=Strict",
		})
		jar.ApplyClientMutations(targetURL, []ClientCookieMutation{
			{Op: op, Name: name, Value: val, Path: p},
		})
		hdr := jar.BuildCookieHeaderWithContext(targetURL, CookieRequestContext{
			Method:           "GET",
			SameOriginClient: true,
		})
		// Invariants that MUST hold for ALL adversarial inputs:
		// 1. Protected HttpOnly/Secure server cookie `sid=protected-admin` NEVER evicted/overwritten!
		if !strings.Contains(hdr, "sid=protected-admin") {
			t.Fatalf("protected server cookie lost or tampered: %q", hdr)
		}
		// 2. Output Cookie header NEVER contains CR/LF/NUL!
		if strings.ContainsAny(hdr, "\r\n\x00") {
			t.Fatalf("CTL leaked into Cookie header: %q", hdr)
		}
		// 3. Parse every Cookie pair strictly: zero semicolon smuggling, exact single `sid=protected-admin`!
		sidCount := 0
		for _, pair := range strings.Split(hdr, "; ") {
			k, v, ok := strings.Cut(pair, "=")
			if !ok {
				t.Fatalf("malformed cookie pair in %q: %q", hdr, pair)
			}
			if strings.ContainsAny(k, "; \t\r\n") || strings.ContainsAny(v, ";\r\n") {
				t.Fatalf("smuggled delimiter in cookie pair %q of %q", pair, hdr)
			}
			if k == "sid" {
				sidCount++
				if v != "protected-admin" {
					t.Fatalf("sid cookie tampered: %q in %q", pair, hdr)
				}
			}
		}
		if sidCount != 1 {
			t.Fatalf("expected exactly 1 sid cookie, got %d in %q", sidCount, hdr)
		}
	})
}

func FuzzResolveSafeRootPath(f *testing.F) {
	f.Add("/")
	f.Add("/docs/readme.txt")
	f.Add("/../.env")
	f.Add("/%2e%2e/%2e%2e/etc/passwd")
	f.Add("/a\\b\\c")
	f.Add("/..;/admin")
	f.Add("/sub/.hidden/file")

	pol, err := NewDestinationPolicy("http://127.0.0.1:8080/sub", "")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, rawTarget string) {
		dest, err := pol.Resolve(rawTarget, false)
		if err != nil {
			return
		}
		// Invariant: resolved URL MUST NEVER escape TargetSubpath or contain backslashes/NUL!
		if dest.URL.Path != "/sub" && !strings.HasPrefix(dest.URL.Path, "/sub/") {
			t.Fatalf("TargetSubpath escape: raw=%q -> %q", rawTarget, dest.URL.Path)
		}
		if strings.ContainsAny(dest.URL.Path, "\\\x00") {
			t.Fatalf("forbidden char in resolved path: %q", dest.URL.Path)
		}
	})
}
