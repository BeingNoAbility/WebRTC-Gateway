package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

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

	nodeID := flag.String("node-id", defaultNodeID, "Unique node ID for this agent (env: NODE_ID)")
	signalURL := flag.String("signal", defaultSignal, "Signaling server WebSocket URL (env: SIGNAL_URL)")
	secret := flag.String("secret", defaultSecret, "Optional secret token for authentication (env: SECRET)")
	target := flag.String("target", defaultTarget, "Local destination Web application URL (env: TARGET_URL)")
	proxyURL := flag.String("proxy", defaultProxy, "Optional HTTP/HTTPS/SOCKS5 proxy for signaling (env: PROXY_URL)")
	sni := flag.String("sni", defaultSNI, "Optional TLS SNI and Host header for signaling (useful when dialing Cloudflare IP directly, env: SNI)")
	insecure := flag.Bool("insecure", defaultInsecure, "Skip TLS verification for self-signed signaling servers (env: INSECURE)")
	flag.Parse()

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
