package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"webrtc-gateway/pkg/signaling"
	"webrtc-gateway/web"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow cross-origin signaling from Cloudflare Pages / Portal
	},
}

type Session struct {
	ID     string
	Role   string
	NodeID string
	Conn   *websocket.Conn
	mu     sync.Mutex
}

func (s *Session) Send(msg *signaling.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Conn == nil {
		return nil
	}
	return s.Conn.WriteJSON(msg)
}

type SignalBroker struct {
	mu           sync.RWMutex
	serverSecret string              // Global server secret (if set via -secret / SECRET)
	agents       map[string]*Session // node_id -> Session
	nodeSecrets  map[string]string   // node_id -> registered secret (anti-hijack)
	clients      map[string]*Session // session_id -> Session
}

func NewSignalBroker(serverSecret string) *SignalBroker {
	return &SignalBroker{
		serverSecret: serverSecret,
		agents:       make(map[string]*Session),
		nodeSecrets:  make(map[string]string),
		clients:      make(map[string]*Session),
	}
}

func (b *SignalBroker) RegisterAgent(nodeID, secret string, s *Session) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// 1. Verify global server secret if server was started with -secret
	if b.serverSecret != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(b.serverSecret)) != 1 {
		return fmt.Errorf("unauthorized: server secret mismatch")
	}

	// 2. Node ownership protection (anti-hijack)
	if boundSecret, exists := b.nodeSecrets[nodeID]; exists && boundSecret != "" {
		if subtle.ConstantTimeCompare([]byte(secret), []byte(boundSecret)) != 1 {
			return fmt.Errorf("unauthorized: node_id %q is protected by a different secret", nodeID)
		}
	}

	if old, exists := b.agents[nodeID]; exists {
		_ = old.Send(&signaling.Message{Type: signaling.TypeError, Error: "replaced by new agent connection"})
		if old.Conn != nil {
			_ = old.Conn.Close()
		}
	}
	b.agents[nodeID] = s
	if secret != "" {
		b.nodeSecrets[nodeID] = secret
	}
	log.Printf("[Broker] Agent registered: node_id=%s (auth: %v)", nodeID, secret != "")
	return nil
}

func (b *SignalBroker) HasSecret(nodeID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	sec, exists := b.nodeSecrets[nodeID]
	return exists && sec != ""
}

func (b *SignalBroker) RemoveAgent(nodeID string, s *Session) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if curr, exists := b.agents[nodeID]; exists && curr == s {
		delete(b.agents, nodeID)
		log.Printf("[Broker] Agent disconnected: node_id=%s", nodeID)
	}
}

func (b *SignalBroker) RegisterClient(s *Session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[s.ID] = s
	log.Printf("[Broker] Client joined: session_id=%s", s.ID)
}

func (b *SignalBroker) RemoveClient(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, sessionID)
	log.Printf("[Broker] Client disconnected: session_id=%s", sessionID)
}

func (b *SignalBroker) GetAgent(nodeID string) *Session {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.agents[nodeID]
}

func (b *SignalBroker) GetClient(sessionID string) *Session {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.clients[sessionID]
}

func (b *SignalBroker) NotifyWaitingClients(nodeID string) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	hasSecret := b.nodeSecrets[nodeID] != ""
	for _, client := range b.clients {
		if client.NodeID == nodeID {
			_ = client.Send(&signaling.Message{
				Type:           signaling.TypePeerStatus,
				NodeID:         nodeID,
				SessionID:      client.ID,
				Status:         "online",
				RequiresSecret: hasSecret,
			})
		}
	}
}

// generateSelfSignedCert generates an in-memory ECDSA TLS certificate valid for localhost and local interface IPs.
func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Zero-Client P2P WebRTC Gateway"},
			CommonName:   "webrtc-gateway-selfsigned",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
	}

	// Auto-discover local interface IPs to add to certificate SAN
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if addrs, err := iface.Addrs(); err == nil {
				for _, addr := range addrs {
					if ipNet, ok := addr.(*net.IPNet); ok {
						if ip := ipNet.IP.To4(); ip != nil && !ip.IsLoopback() {
							template.IPAddresses = append(template.IPAddresses, ip)
						}
					}
				}
			}
		}
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

func main() {
	defaultAddr := ":8888"
	if env := os.Getenv("ADDR"); env != "" {
		defaultAddr = env
	} else if env := os.Getenv("PORT"); env != "" {
		defaultAddr = ":" + env
	}
	defaultStatic := os.Getenv("STATIC_DIR")
	if defaultStatic == "" {
		defaultStatic = "./web/dist"
	}
	defaultTLSCert := os.Getenv("TLS_CERT")
	defaultTLSKey := os.Getenv("TLS_KEY")
	defaultTLSAuto := os.Getenv("TLS_AUTO") == "true" || os.Getenv("TLS_AUTO") == "1"
	defaultSecret := os.Getenv("SECRET")

	addr := flag.String("addr", defaultAddr, "Signaling HTTP/WS listening address (env: ADDR or PORT)")
	staticDir := flag.String("static", defaultStatic, "Path to web portal static assets (env: STATIC_DIR)")
	tlsCert := flag.String("tls-cert", defaultTLSCert, "Path to TLS certificate PEM file (e.g. cert.pem or fullchain.pem, env: TLS_CERT)")
	tlsKey := flag.String("tls-key", defaultTLSKey, "Path to TLS private key PEM file (e.g. key.pem or privkey.pem, env: TLS_KEY)")
	tlsAuto := flag.Bool("tls-auto", defaultTLSAuto, "Automatically generate in-memory self-signed TLS cert for HTTPS (env: TLS_AUTO)")
	serverSecret := flag.String("secret", defaultSecret, "Optional server-wide pre-shared secret for agent registration (env: SECRET)")
	flag.Parse()

	broker := NewSignalBroker(*serverSecret)

	mux := http.NewServeMux()

	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("[WS] Upgrade failed: %v", err)
			return
		}

		sessionID := uuid.New().String()
		session := &Session{
			ID:   sessionID,
			Conn: conn,
		}

		defer func() {
			_ = conn.Close()
			if session.Role == "agent" && session.NodeID != "" {
				broker.RemoveAgent(session.NodeID, session)
			} else {
				broker.RemoveClient(session.ID)
			}
		}()

		// Keepalive
		conn.SetPongHandler(func(string) error {
			_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			return nil
		})

		for {
			_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
			var msg signaling.Message
			if err := conn.ReadJSON(&msg); err != nil {
				break
			}

			switch msg.Type {
			case signaling.TypeRegister:
				// Agent registers NodeID
				if msg.NodeID == "" {
					_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: "node_id is required"})
					return
				}
				if err := broker.RegisterAgent(msg.NodeID, msg.Secret, session); err != nil {
					log.Printf("[Broker] Registration rejected for node_id=%s: %v", msg.NodeID, err)
					_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: err.Error()})
					return
				}
				session.Role = "agent"
				session.NodeID = msg.NodeID
				_ = session.Send(&signaling.Message{Type: signaling.TypeRegistered, NodeID: msg.NodeID})

				// Broadcast to any clients currently waiting for this NodeID
				broker.NotifyWaitingClients(msg.NodeID)

			case signaling.TypeJoin:
				// Visitor browser joins with NodeID
				session.Role = "client"
				session.NodeID = msg.NodeID
				broker.RegisterClient(session)

				agent := broker.GetAgent(msg.NodeID)
				hasSecret := broker.HasSecret(msg.NodeID)
				if agent == nil {
					_ = session.Send(&signaling.Message{
						Type:           signaling.TypePeerStatus,
						NodeID:         msg.NodeID,
						Status:         "offline",
						RequiresSecret: hasSecret,
						Error:          "agent is currently offline",
					})
				} else {
					_ = session.Send(&signaling.Message{
						Type:           signaling.TypePeerStatus,
						NodeID:         msg.NodeID,
						SessionID:      session.ID,
						Status:         "online",
						RequiresSecret: hasSecret,
					})
				}

			case signaling.TypeOffer:
				// Client sends Offer to Agent
				msg.SessionID = session.ID
				agent := broker.GetAgent(msg.NodeID)
				if agent == nil {
					_ = session.Send(&signaling.Message{Type: signaling.TypeError, Error: "target agent offline"})
				} else {
					_ = agent.Send(&msg)
				}

			case signaling.TypeAnswer:
				// Agent returns Answer to specific Client
				client := broker.GetClient(msg.SessionID)
				if client != nil {
					_ = client.Send(&msg)
				}

			case signaling.TypeCandidate:
				// Relay ICE candidates
				if session.Role == "agent" {
					client := broker.GetClient(msg.SessionID)
					if client != nil {
						_ = client.Send(&msg)
					}
				} else {
					msg.SessionID = session.ID
					agent := broker.GetAgent(msg.NodeID)
					if agent != nil {
						_ = agent.Send(&msg)
					}
				}

			case signaling.TypeError:
				// Relay error messages (e.g. auth rejection from agent to client)
				if session.Role == "agent" && msg.SessionID != "" {
					client := broker.GetClient(msg.SessionID)
					if client != nil {
						_ = client.Send(&msg)
					}
				}
			}
		}
	})

	// Determine static asset source:
	// 1. If explicit or default -static dir exists on disk, serve from disk (allows hot-reloading in dev)
	// 2. Otherwise, serve embedded static files from web.DistFS
	var staticSource string
	var staticHandler http.Handler
	if *staticDir != "" {
		if abs, err := filepath.Abs(*staticDir); err == nil {
			if stat, err := os.Stat(abs); err == nil && stat.IsDir() {
				staticSource = "disk: " + abs
				staticHandler = http.FileServer(http.Dir(abs))
			}
		}
	}
	if staticHandler == nil {
		if sub, err := fs.Sub(web.DistFS, "dist"); err == nil {
			staticSource = "embedded binary FS (web/dist)"
			staticHandler = http.FileServer(http.FS(sub))
		}
	}

	if staticHandler != nil {
		mux.Handle("/", staticHandler)
	}

	server := &http.Server{
		Addr:    *addr,
		Handler: mux,
	}

	var isTLS bool
	if (*tlsCert != "" && *tlsKey == "") || (*tlsCert == "" && *tlsKey != "") {
		log.Fatalf("[Server] Both -tls-cert and -tls-key must be provided together.")
	}

	if *tlsCert != "" && *tlsKey != "" {
		isTLS = true
		server.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
	} else if *tlsAuto {
		cert, err := generateSelfSignedCert()
		if err != nil {
			log.Fatalf("[Server] Failed to generate self-signed certificate: %v", err)
		}
		isTLS = true
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	scheme := "http"
	wsScheme := "ws"
	if isTLS {
		scheme = "https"
		wsScheme = "wss"
	}

	log.Printf("==================================================")
	log.Printf("  Zero-Client WebRTC P2P Integrated Server")
	log.Printf("  Listening: %s", *addr)
	log.Printf("  Web Portal: %s://localhost%s/", scheme, *addr)
	log.Printf("  Signaling Endpoint: %s://localhost%s/ws", wsScheme, *addr)
	log.Printf("  Static Assets: %s", staticSource)
	if isTLS {
		if *tlsCert != "" {
			log.Printf("  TLS / HTTPS: Enabled (Custom cert: %s)", *tlsCert)
		} else {
			log.Printf("  TLS / HTTPS: Enabled (Auto-generated self-signed SAN cert)")
		}
	} else {
		log.Printf("  TLS / HTTPS: Disabled (Plain HTTP)")
	}
	log.Printf("==================================================")

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[Server] Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	var err error
	if isTLS {
		if *tlsCert != "" && *tlsKey != "" {
			err = server.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = server.ListenAndServeTLS("", "")
		}
	} else {
		err = server.ListenAndServe()
	}

	if err != nil && err != http.ErrServerClosed {
		log.Fatalf("[Server] Listen error: %v", err)
	}
}
