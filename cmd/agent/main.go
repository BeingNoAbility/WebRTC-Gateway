package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"webrtc-gateway/pkg/agent"
)

func main() {
	defaultNodeID := "homeserver"
	if env := os.Getenv("NODE_ID"); env != "" {
		defaultNodeID = env
	}
	defaultSignal := "ws://127.0.0.1:8888/ws"
	if env := os.Getenv("SIGNAL_URL"); env != "" {
		defaultSignal = env
	}
	defaultTarget := "http://127.0.0.1:8080"
	if env := os.Getenv("TARGET_URL"); env != "" {
		defaultTarget = env
	}
	defaultSecret := os.Getenv("SECRET")
	defaultProxy := os.Getenv("PROXY_URL")
	defaultSNI := os.Getenv("SNI")
	defaultInsecure := os.Getenv("INSECURE") == "true" || os.Getenv("INSECURE") == "1"
	defaultTurnURL := os.Getenv("TURN_URL")
	defaultTurnUser := os.Getenv("TURN_USER")
	defaultTurnPass := os.Getenv("TURN_PASS")
	defaultStunServers := os.Getenv("STUN_SERVERS")
	defaultMetricsAddr := os.Getenv("METRICS_ADDR")
	defaultMaxStreams := 512
	if env := os.Getenv("MAX_STREAMS"); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v > 0 {
			defaultMaxStreams = v
		}
	}

	nodeID := flag.String("node-id", defaultNodeID, "Unique node ID for this agent (env: NODE_ID)")
	signalURL := flag.String("signal", defaultSignal, "Signaling server WebSocket URL (env: SIGNAL_URL)")
	secret := flag.String("secret", defaultSecret, "Optional secret token for authentication (env: SECRET)")
	target := flag.String("target", defaultTarget, "Local destination Web application URL (env: TARGET_URL)")
	proxyURL := flag.String("proxy", defaultProxy, "Optional HTTP/HTTPS/SOCKS5 proxy for signaling (env: PROXY_URL)")
	sni := flag.String("sni", defaultSNI, "Optional TLS SNI and Host header for signaling (useful when dialing Cloudflare IP directly, env: SNI)")
	insecure := flag.Bool("insecure", defaultInsecure, "Skip TLS verification for self-signed signaling servers (env: INSECURE)")
	turnURL := flag.String("turn-url", defaultTurnURL, "Optional TURN relay server URL (e.g. turn:relay.example.com:3478, env: TURN_URL)")
	turnUser := flag.String("turn-user", defaultTurnUser, "Optional TURN relay username (env: TURN_USER)")
	turnPass := flag.String("turn-pass", defaultTurnPass, "Optional TURN relay password / credential (env: TURN_PASS)")
	stunServers := flag.String("stun-servers", defaultStunServers, "Optional comma-separated custom STUN servers (env: STUN_SERVERS)")
	maxStreams := flag.Int("max-streams", defaultMaxStreams, "Maximum concurrent multiplexed streams before capacity backpressure rejection (env: MAX_STREAMS)")
	metricsAddr := flag.String("metrics-addr", defaultMetricsAddr, "Optional address to serve Prometheus metrics (e.g. :9090, env: METRICS_ADDR, default disabled)")
	shareDuration := flag.Duration("sign-link", 0, "Generate a temporary signed share link with the given expiration duration (e.g. 1h, 24h, 7d) and exit")
	portalURL := flag.String("portal-url", "https://your-portal-domain.com", "Base URL of the Web Portal for -sign-link generation")
	flag.Parse()

	// If -sign-link was requested, generate the temporary link and exit
	if *shareDuration > 0 {
		if *secret == "" {
			log.Fatalf("[Error] -secret is required to generate signed share links.")
		}
		exp := time.Now().Add(*shareDuration).Unix()
		sign := agent.GenerateShareSignature(*nodeID, exp, *secret)
		base := strings.TrimRight(*portalURL, "/")
		link := fmt.Sprintf("%s/#%s?exp=%d&sign=%s", base, *nodeID, exp, sign)
		log.Printf("==================================================")
		log.Printf("  Temporary Stateless Signed Share Link")
		log.Printf("  Node ID:    %s", *nodeID)
		log.Printf("  Expires:    %s (in %v)", time.Unix(exp, 0).Format("2006-01-02 15:04:05 MST"), *shareDuration)
		log.Printf("  Link:       %s", link)
		log.Printf("==================================================")
		fmt.Println(link)
		os.Exit(0)
	}

	log.Printf("==================================================")
	log.Printf("  Zero-Client P2P WebRTC Gateway Agent (Go Daemon)")
	log.Printf("  Node ID: %s | Target: %s", *nodeID, *target)
	if *sni != "" {
		log.Printf("  TLS SNI / Host: %s", *sni)
	}
	if *proxyURL != "" {
		log.Printf("  Proxy: %s", *proxyURL)
	}
	if *insecure {
		log.Printf("  TLS Verify: Disabled (-insecure)")
	}
	log.Printf("==================================================")

	ag := agent.New(*nodeID, *signalURL, *secret, *target)
	if *sni != "" {
		ag.SetSNI(*sni)
	}
	if *proxyURL != "" {
		ag.SetProxy(*proxyURL)
	}
	if *insecure {
		ag.SetInsecureTLS(true)
	}
	if *stunServers != "" {
		ag.SetSTUN(strings.Split(*stunServers, ","))
	}
	if *turnURL != "" {
		ag.SetTURN(*turnURL, *turnUser, *turnPass)
	}
	ag.SetMaxConcurrentStreams(*maxStreams)

	if *metricsAddr != "" {
		go func() {
			metricsMux := http.NewServeMux()
			metricsMux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
				_, _ = w.Write([]byte(ag.ExportPrometheusMetrics()))
			})
			metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("OK\n"))
			})
			log.Printf("[Agent] Prometheus metrics listening on http://%s/metrics (healthcheck: /healthz)", *metricsAddr)
			if err := http.ListenAndServe(*metricsAddr, metricsMux); err != nil && err != http.ErrServerClosed {
				log.Printf("[Agent] Metrics server error: %v", err)
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[Agent] Shutting down...")
		cancel()
		ag.Close()
	}()

	if err := ag.Start(ctx); err != nil && err != context.Canceled {
		log.Fatalf("[Agent] Fatal error: %v", err)
	}
}
