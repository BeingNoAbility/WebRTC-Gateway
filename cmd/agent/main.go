package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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

// Version represents the current release version of WebRTC Gateway Agent.
var Version = "v2.0.1"

func printAgentUsage(out io.Writer) {
	fmt.Fprintf(out, "WebRTC Gateway Agent (%s)\n", Version)
	fmt.Fprintf(out, "Zero-Client WebRTC P2P Intranet Penetration & Reverse Proxy Agent\n\n")
	fmt.Fprintf(out, "Usage:\n")
	fmt.Fprintf(out, "  agent [mode] [options]\n\n")
	fmt.Fprintf(out, "Operational Modes (Mutually Exclusive, Default: -target http://127.0.0.1:8080):\n")
	fmt.Fprintf(out, "  -target string\n")
	fmt.Fprintf(out, "        Local destination Web application URL to reverse proxy (HTTP/WebSocket)\n")
	fmt.Fprintf(out, "        (env: TARGET_URL, default: http://127.0.0.1:8080)\n")
	fmt.Fprintf(out, "  -dir string\n")
	fmt.Fprintf(out, "        Directory path to serve as static files with automatic directory index\n")
	fmt.Fprintf(out, "        (env: DIR_PATH)\n")
	fmt.Fprintf(out, "  -file string\n")
	fmt.Fprintf(out, "        Single file path to serve directly for P2P download or stream\n")
	fmt.Fprintf(out, "        (env: FILE_PATH)\n\n")
	fmt.Fprintf(out, "Core & Signaling Options:\n")
	fmt.Fprintf(out, "  -node-id string\n")
	fmt.Fprintf(out, "        Unique node ID for this agent across the cluster\n")
	fmt.Fprintf(out, "        Allowed characters: a-z, A-Z, 0-9, dot (.), underscore (_), hyphen (-)\n")
	fmt.Fprintf(out, "        (env: NODE_ID, default: homeserver)\n")
	fmt.Fprintf(out, "  -signal string\n")
	fmt.Fprintf(out, "        Signaling server WebSocket URL (env: SIGNAL_URL, default: ws://127.0.0.1:8888/ws)\n")
	fmt.Fprintf(out, "  -config string\n")
	fmt.Fprintf(out, "        Path to YAML or JSON configuration file (env: CONFIG_FILE)\n")
	fmt.Fprintf(out, "  -proxy string\n")
	fmt.Fprintf(out, "        Optional HTTP/HTTPS/SOCKS5 proxy for signaling (e.g. socks5://127.0.0.1:1080, env: PROXY_URL)\n")
	fmt.Fprintf(out, "  -sni string\n")
	fmt.Fprintf(out, "        Optional TLS SNI server name and HTTP Host header for signaling (env: SNI)\n")
	fmt.Fprintf(out, "  -insecure\n")
	fmt.Fprintf(out, "        Skip TLS certificate verification for self-signed signaling servers (env: INSECURE)\n\n")
	fmt.Fprintf(out, "Security & Decoupled Authentication Options:\n")
	fmt.Fprintf(out, "  -signal-secret string\n")
	fmt.Fprintf(out, "        Cluster secret token for signaling server registration & anti-hijacking (env: SIGNAL_SECRET)\n")
	fmt.Fprintf(out, "  -access-secret string\n")
	fmt.Fprintf(out, "        Visitor password token for P2P proxy access; empty for public access (env: ACCESS_SECRET)\n")
	fmt.Fprintf(out, "  -allowed-proxy-domains string\n")
	fmt.Fprintf(out, "        Comma-separated allowed domain suffixes for cross-origin proxying\n")
	fmt.Fprintf(out, "        Supports wildcards (e.g. *.hdslb.com,*.bilivideo.com,*.qpic.cn, env: ALLOWED_PROXY_DOMAINS)\n\n")
	fmt.Fprintf(out, "Share Link Generation Options:\n")
	fmt.Fprintf(out, "  -sign-link duration\n")
	fmt.Fprintf(out, "        Generate a temporary HMAC-SHA256 signed share link with expiration (e.g. 1h, 24h, 7d) and exit\n")
	fmt.Fprintf(out, "  -portal-url string\n")
	fmt.Fprintf(out, "        Base URL of the Web Portal for -sign-link generation\n")
	fmt.Fprintf(out, "        (env: PORTAL_URL, default: https://your-portal-domain.com)\n\n")
	fmt.Fprintf(out, "Network & ICE Traversal Options:\n")
	fmt.Fprintf(out, "  -stun-servers string\n")
	fmt.Fprintf(out, "        Comma-separated custom STUN servers (e.g. stun:stun.l.google.com:19302, env: STUN_SERVERS)\n")
	fmt.Fprintf(out, "  -turn-url string\n")
	fmt.Fprintf(out, "        Optional TURN relay server URL (e.g. turn:relay.example.com:3478, env: TURN_URL)\n")
	fmt.Fprintf(out, "  -turn-user string\n")
	fmt.Fprintf(out, "        Optional TURN relay username (env: TURN_USER)\n")
	fmt.Fprintf(out, "  -turn-pass string\n")
	fmt.Fprintf(out, "        Optional TURN relay password / credential (env: TURN_PASS)\n\n")
	fmt.Fprintf(out, "Performance & Monitoring Options:\n")
	fmt.Fprintf(out, "  -max-streams int\n")
	fmt.Fprintf(out, "        Maximum concurrent multiplexed streams before capacity backpressure (env: MAX_STREAMS, default: 512)\n")
	fmt.Fprintf(out, "  -metrics-addr string\n")
	fmt.Fprintf(out, "        Optional address to serve Prometheus metrics (e.g. :9090, env: METRICS_ADDR, default: disabled)\n\n")
	fmt.Fprintf(out, "Help & Version Options:\n")
	fmt.Fprintf(out, "  -h, -help\n")
	fmt.Fprintf(out, "        Show this help message and exit\n")
	fmt.Fprintf(out, "  -v, -version\n")
	fmt.Fprintf(out, "        Show version information and exit\n\n")
	fmt.Fprintf(out, "Configuration Precedence:\n")
	fmt.Fprintf(out, "  1. CLI Flags (Highest Priority)\n")
	fmt.Fprintf(out, "  2. Environment Variables\n")
	fmt.Fprintf(out, "  3. Configuration File (agent.yaml / agent.yml / agent.json)\n")
	fmt.Fprintf(out, "  4. Built-in Defaults (Lowest Priority)\n\n")
	fmt.Fprintf(out, "Examples:\n")
	fmt.Fprintf(out, "  # Reverse proxy a local Web application\n")
	fmt.Fprintf(out, "  agent -node-id myapp -target http://127.0.0.1:3000 -signal wss://signal.example.com/ws\n\n")
	fmt.Fprintf(out, "  # Share a local directory protected by visitor password\n")
	fmt.Fprintf(out, "  agent -node-id vault -dir /var/www/data -access-secret MyPassword123\n\n")
	fmt.Fprintf(out, "  # Serve a single file directly over P2P\n")
	fmt.Fprintf(out, "  agent -node-id iso-share -file /data/ubuntu-24.04.iso\n\n")
	fmt.Fprintf(out, "  # Generate a 24-hour temporary signed share link\n")
	fmt.Fprintf(out, "  agent -node-id vault -access-secret MyPassword123 -sign-link 24h -portal-url https://portal.example.com\n\n")
	fmt.Fprintf(out, "  # Cluster anti-hijacking + visitor password protection\n")
	fmt.Fprintf(out, "  agent -node-id secure-nas -signal-secret ClusterToken123 -access-secret VisitorPass456\n")
}

func main() {
	flag.Usage = func() {
		printAgentUsage(flag.CommandLine.Output())
	}

	// 0. Early check for help / version and fail-fast deprecation check
	for _, arg := range os.Args[1:] {
		if arg == "help" {
			printAgentUsage(os.Stdout)
			os.Exit(0)
		}
		if arg == "version" {
			fmt.Printf("WebRTC Gateway Agent %s\n", Version)
			os.Exit(0)
		}
		if arg == "-secret" || strings.HasPrefix(arg, "-secret=") || arg == "--secret" || strings.HasPrefix(arg, "--secret=") {
			fmt.Fprintln(os.Stderr, "Error: flag -secret has been deprecated and removed.")
			fmt.Fprintln(os.Stderr, "  - Use -signal-secret (SIGNAL_SECRET) for cluster node registration and anti-hijacking.")
			fmt.Fprintln(os.Stderr, "  - Use -access-secret (ACCESS_SECRET) for visitor password protection.")
			os.Exit(1)
		}
	}
	if os.Getenv("SECRET") != "" {
		fmt.Fprintln(os.Stderr, "Error: environment variable SECRET has been deprecated and removed.")
		fmt.Fprintln(os.Stderr, "  - Use SIGNAL_SECRET for cluster node registration and anti-hijacking.")
		fmt.Fprintln(os.Stderr, "  - Use ACCESS_SECRET for visitor password protection.")
		os.Exit(1)
	}

	// 1. Defaults (Tier 1: Defaults)
	cfgNodeID := "homeserver"
	cfgSignalURL := "ws://127.0.0.1:8888/ws"
	cfgSignalSecret := ""
	cfgAccessSecret := ""
	cfgTarget := ""
	cfgDir := ""
	cfgFile := ""
	cfgProxy := ""
	cfgSNI := ""
	cfgInsecure := false
	cfgTurnURL := ""
	cfgTurnUser := ""
	cfgTurnPass := ""
	cfgStunServers := ""
	cfgMaxStreams := 512
	cfgMetricsAddr := ""
	cfgPortalURL := "https://your-portal-domain.com"
	cfgAllowedProxyDomains := ""

	// 2. Load Config File (Tier 2: Config File)
	configPath, explicitConfig := FindConfigFile(os.Args[1:], []string{"agent.yaml", "agent.yml", "agent.json"})
	if configPath != "" {
		fileCfg, err := LoadAgentConfigFile(configPath)
		if err != nil {
			if explicitConfig {
				log.Fatalf("[Error] Failed to load configuration file %q: %v", configPath, err)
			} else {
				log.Fatalf("[Error] Failed to parse auto-detected configuration file %q: %v", configPath, err)
			}
		}
		log.Printf("[Config] Loaded configuration from %s", configPath)
		if fileCfg.NodeID != "" {
			cfgNodeID = fileCfg.NodeID
		}
		if fileCfg.SignalURL != "" {
			cfgSignalURL = fileCfg.SignalURL
		}
		if fileCfg.SignalSecret != "" {
			cfgSignalSecret = fileCfg.SignalSecret
		}
		if fileCfg.AccessSecret != "" {
			cfgAccessSecret = fileCfg.AccessSecret
		}
		if fileCfg.TargetURL != "" {
			cfgTarget = fileCfg.TargetURL
		}
		if fileCfg.Dir != "" {
			cfgDir = fileCfg.Dir
		}
		if fileCfg.File != "" {
			cfgFile = fileCfg.File
		}
		if fileCfg.ProxyURL != "" {
			cfgProxy = fileCfg.ProxyURL
		}
		if fileCfg.SNI != "" {
			cfgSNI = fileCfg.SNI
		}
		if fileCfg.Insecure != nil {
			cfgInsecure = *fileCfg.Insecure
		}
		if fileCfg.TurnURL != "" {
			cfgTurnURL = fileCfg.TurnURL
		}
		if fileCfg.TurnUser != "" {
			cfgTurnUser = fileCfg.TurnUser
		}
		if fileCfg.TurnPass != "" {
			cfgTurnPass = fileCfg.TurnPass
		}
		if len(fileCfg.StunServers) > 0 {
			cfgStunServers = fileCfg.StunServers.String()
		}
		if fileCfg.MaxStreams > 0 {
			cfgMaxStreams = fileCfg.MaxStreams
		}
		if fileCfg.MetricsAddr != "" {
			cfgMetricsAddr = fileCfg.MetricsAddr
		}
		if fileCfg.PortalURL != "" {
			cfgPortalURL = fileCfg.PortalURL
		}
		if len(fileCfg.AllowedProxyDomains) > 0 {
			cfgAllowedProxyDomains = fileCfg.AllowedProxyDomains.String()
		}
	}

	// 3. Override with Environment Variables (Tier 3: ENV)
	if env := os.Getenv("NODE_ID"); env != "" {
		cfgNodeID = env
	}
	if env := os.Getenv("SIGNAL_URL"); env != "" {
		cfgSignalURL = env
	}
	if env := os.Getenv("SIGNAL_SECRET"); env != "" {
		cfgSignalSecret = env
	}
	if env := os.Getenv("ACCESS_SECRET"); env != "" {
		cfgAccessSecret = env
	}
	if env := os.Getenv("TARGET_URL"); env != "" {
		cfgTarget = env
	}
	if env := os.Getenv("DIR_PATH"); env != "" {
		cfgDir = env
	}
	if env := os.Getenv("FILE_PATH"); env != "" {
		cfgFile = env
	}
	if env := os.Getenv("PROXY_URL"); env != "" {
		cfgProxy = env
	}
	if env := os.Getenv("SNI"); env != "" {
		cfgSNI = env
	}
	if env := os.Getenv("INSECURE"); env == "true" || env == "1" {
		cfgInsecure = true
	}
	if env := os.Getenv("TURN_URL"); env != "" {
		cfgTurnURL = env
	}
	if env := os.Getenv("TURN_USER"); env != "" {
		cfgTurnUser = env
	}
	if env := os.Getenv("TURN_PASS"); env != "" {
		cfgTurnPass = env
	}
	if env := os.Getenv("STUN_SERVERS"); env != "" {
		cfgStunServers = env
	}
	if env := os.Getenv("MAX_STREAMS"); env != "" {
		if v, err := strconv.Atoi(env); err == nil && v > 0 {
			cfgMaxStreams = v
		}
	}
	if env := os.Getenv("METRICS_ADDR"); env != "" {
		cfgMetricsAddr = env
	}
	if env := os.Getenv("PORTAL_URL"); env != "" {
		cfgPortalURL = env
	}
	if env := os.Getenv("ALLOWED_PROXY_DOMAINS"); env != "" {
		cfgAllowedProxyDomains = env
	}

	// 4. CLI Flags (Tier 4: CLI Flags - Highest Priority)
	nodeID := flag.String("node-id", cfgNodeID, "Unique node ID for this agent across cluster (env: NODE_ID)")
	signalURL := flag.String("signal", cfgSignalURL, "Signaling server WebSocket URL (env: SIGNAL_URL)")
	signalSecret := flag.String("signal-secret", cfgSignalSecret, "Cluster secret token for signaling server registration & anti-hijacking (env: SIGNAL_SECRET)")
	accessSecret := flag.String("access-secret", cfgAccessSecret, "Visitor password token for proxy access; empty for public access (env: ACCESS_SECRET)")
	target := flag.String("target", cfgTarget, "Local destination Web application URL to reverse proxy (HTTP/WebSocket) (env: TARGET_URL)")
	dirPath := flag.String("dir", cfgDir, "Directory path to serve as static files / directory index (env: DIR_PATH)")
	filePath := flag.String("file", cfgFile, "Single file path to serve directly for P2P download or stream (env: FILE_PATH)")
	configFlag := flag.String("config", "", "Path to configuration file (.yaml, .yml, or .json, env: CONFIG_FILE)")
	proxyURL := flag.String("proxy", cfgProxy, "Optional HTTP/HTTPS/SOCKS5 proxy for signaling (env: PROXY_URL)")
	sni := flag.String("sni", cfgSNI, "Optional TLS SNI and Host header for signaling (env: SNI)")
	insecure := flag.Bool("insecure", cfgInsecure, "Skip TLS verification for self-signed signaling servers (env: INSECURE)")
	turnURL := flag.String("turn-url", cfgTurnURL, "Optional TURN relay server URL (e.g. turn:relay.example.com:3478, env: TURN_URL)")
	turnUser := flag.String("turn-user", cfgTurnUser, "Optional TURN relay username (env: TURN_USER)")
	turnPass := flag.String("turn-pass", cfgTurnPass, "Optional TURN relay password / credential (env: TURN_PASS)")
	stunServers := flag.String("stun-servers", cfgStunServers, "Optional comma-separated custom STUN servers (env: STUN_SERVERS)")
	maxStreams := flag.Int("max-streams", cfgMaxStreams, "Maximum concurrent multiplexed streams before capacity backpressure rejection (env: MAX_STREAMS)")
	metricsAddr := flag.String("metrics-addr", cfgMetricsAddr, "Optional address to serve Prometheus metrics (e.g. :9090, env: METRICS_ADDR, default disabled)")
	shareDuration := flag.Duration("sign-link", 0, "Generate a temporary signed share link with the given expiration duration (e.g. 1h, 24h, 7d) and exit")
	portalURL := flag.String("portal-url", cfgPortalURL, "Base URL of the Web Portal for -sign-link generation (env: PORTAL_URL)")
	allowedProxyDomains := flag.String("allowed-proxy-domains", cfgAllowedProxyDomains, "Comma-separated allowed domain suffixes for cross-origin proxying with wildcard support (e.g. *.hdslb.com,*.bilivideo.com, env: ALLOWED_PROXY_DOMAINS)")
	versionFlag := flag.Bool("version", false, "Display version information and exit")
	vFlag := flag.Bool("v", false, "Display version information and exit (shorthand)")
	_ = configFlag

	flag.Parse()

	if *versionFlag || *vFlag {
		fmt.Printf("WebRTC Gateway Agent %s\n", Version)
		os.Exit(0)
	}

	// Mutual Exclusivity Check between -target, -dir, and -file
	modesCount := 0
	if *target != "" {
		modesCount++
	}
	if *dirPath != "" {
		modesCount++
	}
	if *filePath != "" {
		modesCount++
	}

	if modesCount > 1 {
		log.Fatalf("FATAL ERROR: Conflicting modes: cannot specify more than one of -target, -dir, and -file.")
	}
	if modesCount == 0 {
		*target = "http://127.0.0.1:8080"
	}

	// If -sign-link was requested, generate the temporary link and exit
	if *shareDuration > 0 {
		if *accessSecret == "" {
			fmt.Fprintln(os.Stderr, "Notice: Node is configured for public access (-access-secret is empty).")
			fmt.Fprintln(os.Stderr, "Visitors can access directly without a signed link:")
			fmt.Printf("%s/#%s\n", strings.TrimRight(*portalURL, "/"), *nodeID)
			os.Exit(0)
		}
		exp := time.Now().Add(*shareDuration).Unix()
		sign := agent.GenerateShareSignature(*nodeID, exp, *accessSecret)
		base := strings.TrimRight(*portalURL, "/")
		link := fmt.Sprintf("%s/#%s?exp=%d&sign=%s", base, *nodeID, exp, sign)
		log.Printf("==================================================")
		log.Printf("  Temporary Stateless Signed Share Link")
		log.Printf("  Node ID:    %s", *nodeID)
		if *filePath != "" {
			log.Printf("  Mode:       Single File (%s)", *filePath)
		} else if *dirPath != "" {
			log.Printf("  Mode:       Directory Server (%s)", *dirPath)
		} else {
			log.Printf("  Mode:       Reverse Proxy (%s)", *target)
		}
		log.Printf("  Expires:    %s (in %v)", time.Unix(exp, 0).Format("2006-01-02 15:04:05 MST"), *shareDuration)
		log.Printf("  Link:       %s", link)
		log.Printf("==================================================")
		fmt.Println(link)
		os.Exit(0)
	}

	ag := agent.New(*nodeID, *signalURL, *signalSecret, *accessSecret, *target)
	if *dirPath != "" {
		if err := ag.SetDirectory(*dirPath); err != nil {
			log.Fatalf("FATAL ERROR: Failed to initialize directory server: %v", err)
		}
	} else if *filePath != "" {
		if err := ag.SetSingleFile(*filePath); err != nil {
			log.Fatalf("FATAL ERROR: Failed to initialize single file server: %v", err)
		}
	}

	log.Printf("==================================================")
	log.Printf("  Zero-Client P2P WebRTC Gateway Agent (Go Daemon)")
	if *filePath != "" {
		log.Printf("  Node ID: %s | Mode: Single File (%s, %d bytes)", *nodeID, ag.FileProxy().FileName(), ag.FileProxy().FileSize())
	} else if *dirPath != "" {
		log.Printf("  Node ID: %s | Mode: Directory Server (%s)", *nodeID, ag.FileProxy().TargetPath())
	} else {
		log.Printf("  Node ID: %s | Mode: Reverse Proxy (Target: %s)", *nodeID, *target)
	}
	log.Printf("  Cluster Auth: %v | Visitor Auth: %v", *signalSecret != "", *accessSecret != "")
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
	if *sni != "" {
		ag.SetSNI(*sni)
	}
	if *proxyURL != "" {
		ag.SetProxy(*proxyURL)
	}
	if *allowedProxyDomains != "" {
		var domains []string
		for _, d := range strings.Split(*allowedProxyDomains, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				domains = append(domains, d)
			}
		}
		ag.SetAllowedProxyDomains(domains)
		log.Printf("  Allowed Proxy Domains: %v", domains)
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
