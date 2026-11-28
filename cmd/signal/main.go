package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"webrtc-gateway/pkg/config"
	"webrtc-gateway/pkg/signaling"
	"webrtc-gateway/web"
)

// Version is injected at build time via `-ldflags="-X main.Version=vX.Y.Z"`.
var Version = "dev"

type serverFileConfig struct {
	Addr                 string `yaml:"addr"`
	StaticDir            string `yaml:"static_dir"`
	TLSAuto              bool   `yaml:"tls_auto"`
	TLSCert              string `yaml:"tls_cert"`
	TLSKey               string `yaml:"tls_key"`
	SignalSecret         string `yaml:"signal_secret"`
	NodeSecretsFile      string `yaml:"node_secrets_file"`
	AllowForceRekey      *bool  `yaml:"allow_force_rekey"`
	AllowAnonymousAgents bool   `yaml:"allow_anonymous_agents"`
	BoundKeysFile        string `yaml:"bound_keys_file"`
	PortalOrigin         string `yaml:"portal_origin"`
	SandboxOrigin        string `yaml:"sandbox_origin"`
	DevMode              bool   `yaml:"dev"`
	AllowSameSiteSandbox bool   `yaml:"allow_same_site_sandbox"`
	AllowAllOrigins      bool   `yaml:"allow_all_origins"`
	MaxIPConns           int    `yaml:"max_ip_conns"`
	MaxIPMessages        int    `yaml:"max_ip_msgs"`
	TrustedProxies       string `yaml:"trusted_proxies"`
	ClientIPHeader       string `yaml:"client_ip_header"`
	MetricsAddr          string `yaml:"metrics_addr"`
}

func main() {
	fs := flag.NewFlagSet("signal", flag.ExitOnError)
	vShort := fs.Bool("v", false, "Print version and exit")
	vLong := fs.Bool("version", false, "Print version and exit")
	cfgFlag := fs.String("config", "", "Explicit YAML/JSON config file path (also CONFIG_FILE; never auto-loaded from CWD)")
	addrFlag := fs.String("addr", "", "Listen address (default 127.0.0.1:8888; non-loopback requires explicit origins)")
	staticFlag := fs.String("static", "", "Optional external static directory (overrides embedded web assets)")
	tlsAutoFlag := fs.Bool("tls-auto", false, "Generate self-signed loopback/domain TLS certificate")
	tlsCertFlag := fs.String("tls-cert", "", "TLS certificate PEM path (hot-reloaded)")
	tlsKeyFlag := fs.String("tls-key", "", "TLS private key PEM path (hot-reloaded)")
	signalSecretFlag := fs.String("signal-secret", "", "Shared secret (>=16 bytes) for Agent HTTP Upgrade Bearer auth (prefer SIGNAL_SECRET_FILE or SIGNAL_SECRET)")
	nodeSecretsFlag := fs.String("node-secrets-file", "", "Optional YAML/JSON map of node_id -> per-node registration secret (>=16 bytes)")
	allowForceRekeyFlag := fs.Bool("allow-force-rekey", false, "Allow authenticated agents to rotate bound node keys via force_rekey (default false for shared secret)")
	allowAnonFlag := fs.Bool("allow-anonymous-agents", false, "Explicitly permit anonymous Agent registration without SIGNAL_SECRET")
	boundKeysFlag := fs.String("bound-keys-file", "", "Optional JSON file path to persist Ed25519 node bindings across restarts")
	portalOriginFlag := fs.String("portal-origin", "", "Explicit Portal origin (e.g. https://portal.example.com)")
	sandboxOriginFlag := fs.String("sandbox-origin", "", "Explicit Sandbox origin: wildcard https://*.sandbox.example.net (Tier 1) or fixed https://app.example.net (Tier 2 restricted)")
	devFlag := fs.Bool("dev", false, "Explicit local loopback dev mode (*.localhost; requires loopback -addr)")
	allowSameSiteFlag := fs.Bool("allow-same-site-sandbox", false, "Allow Tier 2 same-site wildcard sandbox with Origin-Agent-Cluster isolation")
	allowAllOriginsFlag := fs.Bool("allow-all-origins", false, "Allow all WebSocket Origins (dev/testing only)")
	maxIPConnsFlag := fs.Int("max-ip-conns", 0, "Max concurrent WebSocket connections per IP (default 64)")
	maxIPMsgsFlag := fs.Int("max-ip-msgs", 0, "Max signaling messages per IP per 10s (default 120)")
	trustedProxiesFlag := fs.String("trusted-proxies", "", "Comma-separated trusted proxy IPs/CIDRs (default loopback only)")
	clientIPHeaderFlag := fs.String("client-ip-header", "", "Optional single-value client IP header from trusted proxies (e.g. CF-Connecting-IP, Fastly-Client-IP, CloudFront-Viewer-Address, X-Real-IP)")
	metricsAddrFlag := fs.String("metrics-addr", "", "Optional Prometheus metrics listen address (prefer loopback)")
	healthFlag := fs.Bool("health", false, "Run health probe against /healthz and exit")
	healthURLFlag := fs.String("health-url", "", "Optional custom URL for -health")
	_ = fs.Parse(os.Args[1:])

	if fs.NArg() > 0 {
		log.Fatalf("[Signal] Unexpected positional arguments: %v (did you forget a leading '-' on a flag?)", fs.Args())
	}
	if *vShort || *vLong {
		fmt.Printf("webrtc-gateway-signal %s\n", Version)
		os.Exit(0)
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	var fileCfg serverFileConfig
	if cfgPath := config.ResolveExplicitConfigPath(*cfgFlag); cfgPath != "" {
		if err := config.LoadYAMLFile(cfgPath, &fileCfg); err != nil {
			log.Fatalf("[Signal] Failed to load config: %v", err)
		}
	}

	mustBool := func(val, cliSet bool, envName string, cfgPtr *bool, cfgBool, def bool) (bool, bool) {
		b, set, err := config.ResolveBool(val, cliSet, envName, cfgPtr, cfgBool, def)
		if err != nil {
			log.Fatalf("[Signal] %v", err)
		}
		return b, set
	}
	mustInt := func(val int, cliSet bool, envName string, cfgVal, def, minVal, maxVal int) int {
		n, err := config.ResolveInt(val, cliSet, envName, cfgVal, def, minVal, maxVal)
		if err != nil {
			log.Fatalf("[Signal] %v", err)
		}
		return n
	}

	addr := config.ResolveString(*addrFlag, explicit["addr"], "ADDR", fileCfg.Addr, "127.0.0.1:8888")
	staticDir := config.ResolveString(*staticFlag, explicit["static"], "STATIC_DIR", fileCfg.StaticDir, "")
	tlsAuto, _ := mustBool(*tlsAutoFlag, explicit["tls-auto"], "TLS_AUTO", nil, fileCfg.TLSAuto, false)
	tlsCert := config.ResolveString(*tlsCertFlag, explicit["tls-cert"], "TLS_CERT", fileCfg.TLSCert, "")
	tlsKey := config.ResolveString(*tlsKeyFlag, explicit["tls-key"], "TLS_KEY", fileCfg.TLSKey, "")
	nodeSecretsFile := config.ResolveString(*nodeSecretsFlag, explicit["node-secrets-file"], "NODE_SECRETS_FILE", fileCfg.NodeSecretsFile, "")
	allowForceRekey, allowForceRekeySet := mustBool(*allowForceRekeyFlag, explicit["allow-force-rekey"], "ALLOW_FORCE_REKEY", fileCfg.AllowForceRekey, false, false)
	allowAnon, _ := mustBool(*allowAnonFlag, explicit["allow-anonymous-agents"], "ALLOW_ANONYMOUS_AGENTS", nil, fileCfg.AllowAnonymousAgents, false)
	boundKeysFile := config.ResolveString(*boundKeysFlag, explicit["bound-keys-file"], "BOUND_KEYS_FILE", fileCfg.BoundKeysFile, "")
	portalOrigin := config.ResolveString(*portalOriginFlag, explicit["portal-origin"], "PORTAL_ORIGIN", fileCfg.PortalOrigin, "")
	sandboxOrigin := config.ResolveString(*sandboxOriginFlag, explicit["sandbox-origin"], "SANDBOX_ORIGIN", fileCfg.SandboxOrigin, "")
	devMode, _ := mustBool(*devFlag, explicit["dev"], "DEV_MODE", nil, fileCfg.DevMode, false)
	allowSameSite, _ := mustBool(*allowSameSiteFlag, explicit["allow-same-site-sandbox"], "ALLOW_SAME_SITE_SANDBOX", nil, fileCfg.AllowSameSiteSandbox, false)
	allowAllOrigins, _ := mustBool(*allowAllOriginsFlag, explicit["allow-all-origins"], "SIGNAL_ALLOW_ALL_ORIGINS", nil, fileCfg.AllowAllOrigins, false)
	maxIPConns := mustInt(*maxIPConnsFlag, explicit["max-ip-conns"], "MAX_IP_CONNS", fileCfg.MaxIPConns, signaling.DefaultMaxIPConns, 1, 10000)
	maxIPMsgs := mustInt(*maxIPMsgsFlag, explicit["max-ip-msgs"], "MAX_IP_MSGS", fileCfg.MaxIPMessages, signaling.DefaultMaxIPMessages, 1, 100000)
	trustedProxies := config.ResolveString(*trustedProxiesFlag, explicit["trusted-proxies"], "TRUSTED_PROXIES", fileCfg.TrustedProxies, "")
	clientIPHeader := config.ResolveString(*clientIPHeaderFlag, explicit["client-ip-header"], "CLIENT_IP_HEADER", fileCfg.ClientIPHeader, "")
	metricsAddr := config.ResolveString(*metricsAddrFlag, explicit["metrics-addr"], "METRICS_ADDR", fileCfg.MetricsAddr, "")

	if *healthFlag {
		tlsOn := tlsAuto || (tlsCert != "" && tlsKey != "")
		resolvedHealthURL := config.ResolveString(*healthURLFlag, explicit["health-url"], "HEALTH_URL", "", "")
		if err := signaling.RunHealthCheck(addr, tlsOn, resolvedHealthURL); err != nil {
			fmt.Fprintf(os.Stderr, "health check failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	resolvedSecret, fromCLI, err := config.ResolveSecret(
		*signalSecretFlag, explicit["signal-secret"],
		"SIGNAL_SECRET", "SIGNAL_SECRET_FILE", fileCfg.SignalSecret,
	)
	if err != nil {
		log.Fatalf("[Signal] %v", err)
	}
	if fromCLI {
		log.Printf("[Signal] WARNING: -signal-secret passed via CLI args (visible in process list); prefer SIGNAL_SECRET_FILE or SIGNAL_SECRET env")
	}

	var nodeSecrets map[string]string
	if nodeSecretsFile != "" {
		if err := config.LoadYAMLFile(nodeSecretsFile, &nodeSecrets); err != nil {
			log.Fatalf("[Signal] Failed to load node-secrets-file: %v", err)
		}
		if len(nodeSecrets) == 0 {
			log.Fatalf("[Signal] node-secrets-file %q is empty (Fail-Closed)", nodeSecretsFile)
		}
	}

	var allowForceRekeyPtr *bool
	if allowForceRekeySet {
		v := allowForceRekey
		allowForceRekeyPtr = &v
	}

	srv, err := signaling.NewSignalServer(signaling.ServerOptions{
		Addr:                 addr,
		StaticDir:            staticDir,
		StaticFS:             web.DistFS,
		TLSAuto:              tlsAuto,
		TLSCert:              tlsCert,
		TLSKey:               tlsKey,
		SignalSecret:         resolvedSecret,
		NodeSecrets:          nodeSecrets,
		AllowForceRekey:      allowForceRekeyPtr,
		AllowAnonymousAgents: allowAnon,
		BoundKeysFile:        boundKeysFile,
		PortalOrigin:         portalOrigin,
		SandboxOrigin:        sandboxOrigin,
		DevMode:              devMode,
		AllowSameSiteSandbox: allowSameSite,
		AllowAllOrigins:      allowAllOrigins,
		MaxIPConns:           maxIPConns,
		MaxIPMessages:        maxIPMsgs,
		TrustedProxies:       trustedProxies,
		ClientIPHeader:       clientIPHeader,
		Version:              Version,
	})
	if err != nil {
		log.Fatalf("[Signal] Startup rejected: %v", err)
	}

	httpSrv, err := srv.NewHTTPServer()
	if err != nil {
		log.Fatalf("[Signal] TLS init failed: %v", err)
	}

	var metricsSrv *http.Server
	if metricsAddr != "" {
		if !signaling.IsLoopbackBindAddr(metricsAddr) {
			log.Printf("[Signal] WARNING: metrics-addr %q binds non-loopback; restrict access via firewall", metricsAddr)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if r.Method == http.MethodGet {
				_, _ = fmt.Fprint(w, srv.ExportMetrics())
			}
		})
		metricsSrv = &http.Server{
			Addr:              metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    64 * 1024,
		}
		go func() {
			log.Printf("[Signal] Metrics listening on %s", metricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[Signal] Metrics server error: %v", err)
			}
		}()
	}

	srv.PrintStartupSummary()

	errCh := make(chan error, 1)
	go func() {
		if httpSrv.TLSConfig != nil {
			errCh <- httpSrv.ListenAndServeTLS("", "")
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Printf("[Signal] Received %v, draining listeners and WebSockets...", sig)
	case serveErr := <-errCh:
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Fatalf("[Signal] Server error: %v", serveErr)
		}
	}

	// Stop accepting new HTTP/WS connections BEFORE draining active WebSockets so no new
	// upgrades slip in during shutdown.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	srv.Shutdown()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutCtx)
	}
}
