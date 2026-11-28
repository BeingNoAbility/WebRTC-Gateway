package signaling

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

// GenerateSelfSignedCert generates an in-memory ECDSA P-256 certificate strictly scoped to
// loopback + explicitly supplied DNS SANs (never leaking LAN/Docker/VPN interface IPs).
func GenerateSelfSignedCert(extraDNSNames ...string) (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate private key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	dnsNames := []string{"localhost", "*.localhost"}
	seen := map[string]bool{"localhost": true, "*.localhost": true}
	for _, name := range extraDNSNames {
		if name != "" && !seen[name] {
			seen[name] = true
			dnsNames = append(dnsNames, name)
		}
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"WebRTC Gateway"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create certificate: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal ec private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})
	return tls.X509KeyPair(certPEM, keyPEM)
}

// ReloadingCertProvider hot-reloads custom TLS cert/key files when inode, mtime, or size changes,
// serializing reload probes to prevent concurrent handshake stampedes.
type ReloadingCertProvider struct {
	certPath string
	keyPath  string

	reloadMu  sync.Mutex
	mu        sync.RWMutex
	cert      *tls.Certificate
	certInfo  os.FileInfo
	keyInfo   os.FileInfo
	lastCheck time.Time
}

func NewReloadingCertProvider(certPath, keyPath string) (*ReloadingCertProvider, error) {
	p := &ReloadingCertProvider{certPath: certPath, keyPath: keyPath}
	if err := p.reload(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *ReloadingCertProvider) reload() error {
	cs, err := os.Stat(p.certPath)
	if err != nil {
		return err
	}
	ks, err := os.Stat(p.keyPath)
	if err != nil {
		return err
	}
	loaded, err := tls.LoadX509KeyPair(p.certPath, p.keyPath)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.cert = &loaded
	p.certInfo = cs
	p.keyInfo = ks
	p.lastCheck = time.Now()
	p.mu.Unlock()
	return nil
}

func certFileChanged(prev, cur os.FileInfo) bool {
	if prev == nil || cur == nil {
		return true
	}
	return !os.SameFile(prev, cur) || !cur.ModTime().Equal(prev.ModTime()) || cur.Size() != prev.Size()
}

func (p *ReloadingCertProvider) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.RLock()
	cur := p.cert
	stale := time.Since(p.lastCheck) > 30*time.Second
	p.mu.RUnlock()

	if !stale {
		return cur, nil
	}

	p.reloadMu.Lock()
	defer p.reloadMu.Unlock()

	p.mu.RLock()
	cur = p.cert
	stale = time.Since(p.lastCheck) > 30*time.Second
	ci, ki := p.certInfo, p.keyInfo
	p.mu.RUnlock()
	if !stale {
		return cur, nil
	}

	cs, cerr := os.Stat(p.certPath)
	ks, kerr := os.Stat(p.keyPath)
	if cerr != nil || kerr != nil {
		log.Printf("[Signal] TLS cert stat failed (keeping active cert): certErr=%v keyErr=%v", cerr, kerr)
		p.mu.Lock()
		p.lastCheck = time.Now()
		p.mu.Unlock()
		return cur, nil
	}
	if certFileChanged(ci, cs) || certFileChanged(ki, ks) {
		if err := p.reload(); err != nil {
			log.Printf("[Signal] TLS cert hot-reload failed (keeping active cert): %v", err)
			p.mu.Lock()
			p.lastCheck = time.Now()
			p.mu.Unlock()
			return cur, nil
		}
		p.mu.RLock()
		cur = p.cert
		p.mu.RUnlock()
		return cur, nil
	}

	p.mu.Lock()
	p.lastCheck = time.Now()
	p.mu.Unlock()
	return cur, nil
}
