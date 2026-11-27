package signaling

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTempSelfSignedCert(t *testing.T, dir, cn string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestGenerateSelfSignedCert_NoPrivateLANIPLeak(t *testing.T) {
	cert, err := GenerateSelfSignedCert("portal.example.com", "*.sandbox.example.net")
	if err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	// Must contain ONLY loopback IPs (127.0.0.1, ::1) — NEVER leak host LAN/Docker/VPN IPs!
	for _, ip := range leaf.IPAddresses {
		if !ip.IsLoopback() {
			t.Fatalf("privacy leak: non-loopback IP %s found in self-signed SAN", ip)
		}
	}
	hasPortal := false
	for _, dns := range leaf.DNSNames {
		if dns == "portal.example.com" {
			hasPortal = true
		}
	}
	if !hasPortal {
		t.Fatalf("expected portal.example.com in DNSNames, got %v", leaf.DNSNames)
	}
}

func TestReloadingCertProvider_HotReload(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeTempSelfSignedCert(t, dir, "first.example.com")

	prov, err := NewReloadingCertProvider(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewReloadingCertProvider: %v", err)
	}
	c1, err := prov.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf1, _ := x509.ParseCertificate(c1.Certificate[0])
	if leaf1.Subject.CommonName != "first.example.com" {
		t.Fatalf("got CN %q, want first.example.com", leaf1.Subject.CommonName)
	}

	// Overwrite cert with newer mtime & force reload check window.
	writeTempSelfSignedCert(t, dir, "rotated.example.com")
	future := time.Now().Add(5 * time.Second)
	_ = os.Chtimes(certPath, future, future)
	_ = os.Chtimes(keyPath, future, future)

	prov.mu.Lock()
	prov.lastCheck = time.Time{}
	prov.mu.Unlock()

	c2, err := prov.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf2, _ := x509.ParseCertificate(c2.Certificate[0])
	if leaf2.Subject.CommonName != "rotated.example.com" {
		t.Fatalf("expected hot-reloaded CN rotated.example.com, got %q", leaf2.Subject.CommonName)
	}
}
