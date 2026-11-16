package proxy

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestSessionCookieManagerClientTokenRetention(t *testing.T) {
	cm := NewSessionCookieManagerWithTTL(50 * time.Millisecond)
	defer cm.Close()

	u, _ := url.Parse("http://example.com")
	clientToken := "cl_test_user_session"

	// 1. Set cookie for clientToken
	jar := cm.GetJar(clientToken)
	jar.SetCookies(u, []*http.Cookie{
		{Name: "session", Value: "secret_123"},
	})

	// 2. Simulate WebRTC disconnect (RemoveSession)
	cm.RemoveSession(clientToken)

	// 3. Since it is a client token ("cl_..."), it should NOT be purged immediately
	reconnectedJar := cm.GetJar(clientToken)
	cookies := reconnectedJar.Cookies(u)
	if len(cookies) == 0 || cookies[0].Value != "secret_123" {
		t.Fatalf("Expected cookie to be preserved for clientToken, got: %v", cookies)
	}

	// 4. Test normal ephemeral session removal
	ephemeralID := "sess_random_123"
	eJar := cm.GetJar(ephemeralID)
	eJar.SetCookies(u, []*http.Cookie{{Name: "temp", Value: "456"}})
	cm.RemoveSession(ephemeralID)

	// Normal session should be gone
	newEJar := cm.GetJar(ephemeralID)
	if len(newEJar.Cookies(u)) != 0 {
		t.Errorf("Expected ephemeral session to be deleted upon RemoveSession")
	}

	// 5. Test expiration after idle TTL
	time.Sleep(70 * time.Millisecond)
	cm.reapExpiredSessions()
	expiredJar := cm.GetJar(clientToken)
	if len(expiredJar.Cookies(u)) != 0 {
		t.Errorf("Expected clientToken to be expired after TTL, but still has cookies")
	}
}
