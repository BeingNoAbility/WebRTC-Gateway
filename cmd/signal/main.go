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
var Version = "v5.0.0-alpha.3"

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
	PortalDomain         string `yaml:"portal_domain"`
	SandboxDomain        string `yaml:"sandbox_domain"`
	SandboxHost          string `yaml:"sandbox_host"`
	AllowSameSiteSandbox bool   `yaml:"allow_same_site_sandbox"`
	AllowAllOrigins      bool   `yaml:"allow_all_origins"`
	MaxIPConns           int    `yaml:"max_ip_conns"`
	MaxIPMessages        int    `yaml:"max_ip_msgs"`
	TrustedProxies       string `yaml:"trusted_proxies"`
	ClientIPHeader       string `yaml:"client_ip_header"`
	MetricsAddr          string `yaml:"metrics_addr"`
}

var valueFlags = map[string]bool{
	"config": true, "addr": true, "static": true, "tls-cert": true, "tls-key": true,
	"signal-secret": true, "node-secrets-file": true, "bound-keys-file": true,
	"portal-domain": true, "sandbox-domain": true, "sandbox-host": true,
	"max-ip-conns": true, "max-ip-msgs": true,
	"trusted-proxies": true, "client-ip-header": true, "metrics-addr": true, "health-url": true,
}

func main() {
	rawArgs := os.Args[1:]
	if config.HasBoolFlag(rawArgs, []string{"v", "version"}, valueFlags) {
		fmt.Printf("webrtc-gateway-signal %s\n", Version)
		os.Exit(0)
	}

	var fileCfg serverFileConfig
	if cfgPath := config.ResolveExplicitConfigPath(rawArgs, valueFlags); cfgPath != "" {
		if err := config.LoadYAMLFile(cfgPath, &fileCfg); err != nil {
			log.Fatalf("[Signal] Failed to load config: %v", err)
		}
	}

	defAllowForceRekey := false
	allowForceRekeyExplicit := false
	if fileCfg.AllowForceRekey != nil {
		defAllowForceRekey = *fileCfg.AllowForceRekey
		allowForceRekeyExplicit = true
	}

	fs := flag.NewFlagSet("signal", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "Explicit YAML/JSON config file path (also CONFIG_FILE; never auto-loaded from CWD)")
	addr := fs.String("addr", fileCfg.Addr, "Listen address (default 127.0.0.1:8888; non-loopback requires domains)")
	staticDir := fs.String("static", fileCfg.StaticDir, "Optional external static directory (overrides embedded web assets)")
	tlsAuto := fs.Bool("tls-auto", fileCfg.TLSAuto, "Generate self-signed loopback/domain TLS certificate")
	tlsCert := fs.String("tls-cert", fileCfg.TLSCert, "TLS certificate PEM path (hot-reloaded)")
	tlsKey := fs.String("tls-key", fileCfg.TLSKey, "TLS private key PEM path (hot-reloaded)")
	signalSecret := fs.String("signal-secret", "", "Shared secret for Agent HTTP Upgrade Bearer auth (prefer SIGNAL_SECRET_FILE or SIGNAL_SECRET)")
	nodeSecretsFile := fs.String("node-secrets-file", fileCfg.NodeSecretsFile, "Optional YAML/JSON map of node_id -> per-node registration secret")
	allowForceRekey := fs.Bool("allow-force-rekey", defAllowForceRekey, "Allow authenticated agents to rotate bound node keys via force_rekey (default false for shared secret)")
	allowAnon := fs.Bool("allow-anonymous-agents", fileCfg.AllowAnonymousAgents, "Explicitly permit anonymous Agent registration without SIGNAL_SECRET")
	boundKeysFile := fs.String("bound-keys-file", fileCfg.BoundKeysFile, "Optional JSON file path to persist Ed25519 node bindings across restarts")
	portalDomain := fs.String("portal-domain", fileCfg.PortalDomain, "Portal base domain (e.g. portal.example.com)")
	sandboxDomain := fs.String("sandbox-domain", fileCfg.SandboxDomain, "Wildcard sandbox domain for Tier 2/3 (e.g. sandbox.example.net)")
	sandboxHost := fs.String("sandbox-host", fileCfg.SandboxHost, "Fixed cross-site sandbox host for Tier 1 without wildcard TLS (e.g. app.example.net)")
	allowSameSite := fs.Bool("allow-same-site-sandbox", fileCfg.AllowSameSiteSandbox, "Allow Tier 2 same-site wildcard sandbox with Origin-Agent-Cluster isolation")
	allowAllOrigins := fs.Bool("allow-all-origins", fileCfg.AllowAllOrigins, "Allow all WebSocket Origins (dev/testing only)")
	maxIPConns := fs.Int("max-ip-conns", fileCfg.MaxIPConns, "Max concurrent WebSocket connections per IP (default 64)")
	maxIPMsgs := fs.Int("max-ip-msgs", fileCfg.MaxIPMessages, "Max signaling messages per IP per 10s (default 120)")
	trustedProxies := fs.String("trusted-proxies", fileCfg.TrustedProxies, "Comma-separated trusted proxy IPs/CIDRs (default loopback only)")
	clientIPHeader := fs.String("client-ip-header", fileCfg.ClientIPHeader, "Optional single-value client IP header from trusted proxies (e.g. CF-Connecting-IP, Fastly-Client-IP, CloudFront-Viewer-Address, X-Real-IP)")
	metricsAddr := fs.String("metrics-addr", fileCfg.MetricsAddr, "Optional Prometheus metrics listen address (prefer loopback)")
	healthFlag := fs.Bool("health", false, "Run health probe against /healthz and exit")
	healthURL := fs.String("health-url", "", "Optional custom URL for -health")
	_ = cfgFlag
	_ = fs.Parse(rawArgs)

	explicitFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
	if explicitFlags["allow-force-rekey"] {
		allowForceRekeyExplicit = true
	}

	applyEnvStr := func(flagName, envName string, dst *string) {
		if !explicitFlags[flagName] {
			if v, ok := os.LookupEnv(envName); ok && v != "" {
				*dst = v
			}
		}
	}
	applyEnvBool := func(flagName, envName string, dst *bool) bool {
		if !explicitFlags[flagName] {
			b, set, err := config.ParseEnvBool(envName)
			if err != nil {
				log.Fatalf("[Signal] %v", err)
			}
			if set {
				*dst = b
				return true
			}
		}
		return false
	}
	applyEnvInt := func(flagName, envName string, minVal, maxVal int, dst *int) {
		if !explicitFlags[flagName] {
			n, set, err := config.ParseEnvInt(envName, minVal, maxVal)
			if err != nil {
				log.Fatalf("[Signal] %v", err)
			}
			if set {
				*dst = n
			}
		}
	}

	applyEnvStr("addr", "ADDR", addr)
	applyEnvStr("static", "STATIC_DIR", staticDir)
	applyEnvBool("tls-auto", "TLS_AUTO", tlsAuto)
	applyEnvStr("tls-cert", "TLS_CERT", tlsCert)
	applyEnvStr("tls-key", "TLS_KEY", tlsKey)
	applyEnvStr("node-secrets-file", "NODE_SECRETS_FILE", nodeSecretsFile)
	if applyEnvBool("allow-force-rekey", "ALLOW_FORCE_REKEY", allowForceRekey) {
		allowForceRekeyExplicit = true
	}
	applyEnvBool("allow-anonymous-agents", "ALLOW_ANONYMOUS_AGENTS", allowAnon)
	applyEnvStr("bound-keys-file", "BOUND_KEYS_FILE", boundKeysFile)
	applyEnvStr("portal-domain", "PORTAL_DOMAIN", portalDomain)
	applyEnvStr("sandbox-domain", "SANDBOX_DOMAIN", sandboxDomain)
	applyEnvStr("sandbox-host", "SANDBOX_HOST", sandboxHost)
	applyEnvBool("allow-same-site-sandbox", "ALLOW_SAME_SITE_SANDBOX", allowSameSite)
	applyEnvBool("allow-all-origins", "SIGNAL_ALLOW_ALL_ORIGINS", allowAllOrigins)
	applyEnvInt("max-ip-conns", "MAX_IP_CONNS", 1, 10000, maxIPConns)
	applyEnvInt("max-ip-msgs", "MAX_IP_MSGS", 1, 100000, maxIPMsgs)
	applyEnvStr("trusted-proxies", "TRUSTED_PROXIES", trustedProxies)
	applyEnvStr("client-ip-header", "CLIENT_IP_HEADER", clientIPHeader)
	applyEnvStr("metrics-addr", "METRICS_ADDR", metricsAddr)

	if *addr == "" {
		*addr = "127.0.0.1:8888"
	}
	if *maxIPConns == 0 {
		*maxIPConns = signaling.DefaultMaxIPConns
	} else if *maxIPConns < 1 || *maxIPConns > 10000 {
		log.Fatalf("[Signal] max-ip-conns out of range (%d): must be 1..10000", *maxIPConns)
	}
	if *maxIPMsgs == 0 {
		*maxIPMsgs = signaling.DefaultMaxIPMessages
	} else if *maxIPMsgs < 1 || *maxIPMsgs > 100000 {
		log.Fatalf("[Signal] max-ip-msgs out of range (%d): must be 1..100000", *maxIPMsgs)
	}

	if *healthFlag {
		tlsOn := *tlsAuto || (*tlsCert != "" && *tlsKey != "")
		if err := signaling.RunHealthCheck(*addr, tlsOn, *healthURL); err != nil {
			fmt.Fprintf(os.Stderr, "health check failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	resolvedSecret, fromCLI, err := config.ResolveSecret(
		*signalSecret, explicitFlags["signal-secret"],
		"SIGNAL_SECRET", "SIGNAL_SECRET_FILE", fileCfg.SignalSecret,
	)
	if err != nil {
		log.Fatalf("[Signal] %v", err)
	}
	if fromCLI {
		log.Printf("[Signal] WARNING: -signal-secret passed via CLI args (visible in process list); prefer SIGNAL_SECRET_FILE or SIGNAL_SECRET env")
	}

	var nodeSecrets map[string]string
	if *nodeSecretsFile != "" {
		if err := config.LoadYAMLFile(*nodeSecretsFile, &nodeSecrets); err != nil {
			log.Fatalf("[Signal] Failed to load node-secrets-file: %v", err)
		}
		if len(nodeSecrets) == 0 {
			log.Fatalf("[Signal] node-secrets-file %q is empty (Fail-Closed)", *nodeSecretsFile)
		}
	}

	var allowForceRekeyPtr *bool
	if allowForceRekeyExplicit {
		v := *allowForceRekey
		allowForceRekeyPtr = &v
	}

	srv, err := signaling.NewSignalServer(signaling.ServerOptions{
		Addr:                 *addr,
		StaticDir:            *staticDir,
		StaticFS:             web.DistFS,
		TLSAuto:              *tlsAuto,
		TLSCert:              *tlsCert,
		TLSKey:               *tlsKey,
		SignalSecret:         resolvedSecret,
		NodeSecrets:          nodeSecrets,
		AllowForceRekey:      allowForceRekeyPtr,
		AllowAnonymousAgents: *allowAnon,
		BoundKeysFile:        *boundKeysFile,
		PortalDomain:         *portalDomain,
		SandboxDomain:        *sandboxDomain,
		SandboxHost:          *sandboxHost,
		AllowSameSiteSandbox: *allowSameSite,
		AllowAllOrigins:      *allowAllOrigins,
		MaxIPConns:           *maxIPConns,
		MaxIPMessages:        *maxIPMsgs,
		TrustedProxies:       *trustedProxies,
		ClientIPHeader:       *clientIPHeader,
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
	if *metricsAddr != "" {
		if !signaling.IsLoopbackBindAddr(*metricsAddr) {
			log.Printf("[Signal] WARNING: metrics-addr %q binds non-loopback; restrict access via firewall", *metricsAddr)
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
			Addr:              *metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    64 * 1024,
		}
		go func() {
			log.Printf("[Signal] Metrics listening on %s", *metricsAddr)
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
		log.Printf("[Signal] Received %v, draining WebSockets and shutting down...", sig)
	case serveErr := <-errCh:
		if serveErr != nil && serveErr != http.ErrServerClosed {
			log.Fatalf("[Signal] Server error: %v", serveErr)
		}
	}

	srv.Shutdown()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutCtx)
	}
}
