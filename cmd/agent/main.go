package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"webrtc-gateway/pkg/agent"
	"webrtc-gateway/pkg/config"
	"webrtc-gateway/pkg/proxy"
	"webrtc-gateway/pkg/signaling"
)

// Version is injected at build time via `-ldflags="-X main.Version=vX.Y.Z"`.
var Version = "v5.0.0-alpha.3"

type agentFileConfig struct {
	NodeID              string `yaml:"node_id"`
	SignalURL           string `yaml:"signal_url"`
	SignalSecret        string `yaml:"signal_secret"`
	AllowInsecureSignal bool   `yaml:"allow_insecure_signal"`
	ForceRekey          bool   `yaml:"force_rekey"`
	AccessSecret        string `yaml:"access_secret"`
	AllowPublic         bool   `yaml:"allow_public"`
	IdentityKey         string `yaml:"identity_key"`
	StateFile           string `yaml:"state_file"`
	EphemeralIdentity   bool   `yaml:"ephemeral_identity"`
	TargetURL           string `yaml:"target_url"`
	DirPath             string `yaml:"dir_path"`
	FilePath            string `yaml:"file_path"`
	HideDotfiles        *bool  `yaml:"hide_dotfiles"`
	FileInlineActive    bool   `yaml:"file_inline_active"`
	UpstreamHost        string `yaml:"upstream_host"`
	ProxyURL            string `yaml:"proxy_url"`
	SNI                 string `yaml:"sni"`
	InsecureTarget      bool   `yaml:"insecure_target"`
	ICEMDNS             *bool  `yaml:"ice_mdns"`
	STUNServers         string `yaml:"stun_servers"`
	TURNURL             string `yaml:"turn_url"`
	TURNUser            string `yaml:"turn_user"`
	TURNPass            string `yaml:"turn_pass"`
	MaxStreams          int    `yaml:"max_streams"`
	MetricsAddr         string `yaml:"metrics_addr"`
	PortalURL           string `yaml:"portal_url"`
}

var valueFlags = map[string]bool{
	"config": true, "id": true, "signal": true, "signal-secret": true,
	"access-secret": true, "identity-key": true, "state-file": true, "target": true,
	"dir": true, "file": true, "upstream-host": true,
	"proxy": true, "sni": true, "stun-servers": true,
	"turn-url": true, "turn-user": true, "turn-pass": true, "max-streams": true,
	"metrics-addr": true, "portal-url": true, "mint-link": true,
	"mint-path": true, "mint-max-conns": true, "health-url": true,
}

func main() {
	rawArgs := os.Args[1:]
	if config.HasBoolFlag(rawArgs, []string{"v", "version"}, valueFlags) {
		fmt.Printf("webrtc-gateway-agent %s\n", Version)
		os.Exit(0)
	}

	var fileCfg agentFileConfig
	if cfgPath := config.ResolveExplicitConfigPath(rawArgs, valueFlags); cfgPath != "" {
		if err := config.LoadYAMLFile(cfgPath, &fileCfg); err != nil {
			log.Fatalf("[Agent] Failed to load config: %v", err)
		}
	}

	defHideDot := true
	if fileCfg.HideDotfiles != nil {
		defHideDot = *fileCfg.HideDotfiles
	}
	defICEMDNS := true
	if fileCfg.ICEMDNS != nil {
		defICEMDNS = *fileCfg.ICEMDNS
	}

	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "Explicit YAML/JSON config path (also CONFIG_FILE; never auto-loaded from CWD)")
	nodeID := fs.String("id", fileCfg.NodeID, "Node ID (lowercase DNS-safe, default homeserver)")
	signalURL := fs.String("signal", fileCfg.SignalURL, "Signaling server base URL (e.g. wss://portal.example.com/ws)")
	signalSecret := fs.String("signal-secret", "", "Shared signaling secret (prefer SIGNAL_SECRET_FILE or SIGNAL_SECRET)")
	allowInsecureSignal := fs.Bool("allow-insecure-signal", fileCfg.AllowInsecureSignal, "Allow sending SIGNAL_SECRET over plaintext non-loopback ws:// (dev only)")
	forceRekey := fs.Bool("force-rekey", fileCfg.ForceRekey, "Force update bound Ed25519 key on signaling server (requires SIGNAL_SECRET)")
	accessSecret := fs.String("access-secret", "", "Visitor access password (prefer ACCESS_SECRET_FILE or ACCESS_SECRET)")
	allowPublic := fs.Bool("allow-public", fileCfg.AllowPublic, "Explicitly permit visitor connections without ACCESS_SECRET")
	identityKey := fs.String("identity-key", fileCfg.IdentityKey, "Path to persistent Ed25519 private key PEM")
	stateFile := fs.String("state-file", fileCfg.StateFile, "Optional path for persistent capability revocation state JSON")
	ephemeralID := fs.Bool("ephemeral-identity", fileCfg.EphemeralIdentity, "Use ephemeral in-memory Ed25519 identity (breaks pinned links across restarts)")
	targetURL := fs.String("target", fileCfg.TargetURL, "Backend HTTP/HTTPS target URL (e.g. http://127.0.0.1:8080)")
	dirPath := fs.String("dir", fileCfg.DirPath, "Serve a local directory read-only via os.Root adapter")
	filePath := fs.String("file", fileCfg.FilePath, "Serve a single local file read-only via os.Root adapter")
	hideDotfiles := fs.Bool("hide-dotfiles", defHideDot, "Hide dotfiles and symlinks pointing to dotfiles in -dir/-file mode")
	fileInlineActive := fs.Bool("file-inline-active", fileCfg.FileInlineActive, "Allow inline script execution on static files (disables CSP: sandbox)")
	upstreamHost := fs.String("upstream-host", fileCfg.UpstreamHost, "Optional Host header override (RFC 9110 host[:port]) for virtual-host -target backends")
	proxyURLRaw := fs.String("proxy", fileCfg.ProxyURL, "Optional outbound HTTP/SOCKS proxy URL for -target")
	sni := fs.String("sni", fileCfg.SNI, "Optional TLS SNI override for HTTPS -target")
	insecureTarget := fs.Bool("insecure-target", fileCfg.InsecureTarget, "Skip TLS verification for self-signed HTTPS -target ONLY")
	iceMDNS := fs.Bool("ice-mdns", defICEMDNS, "Obfuscate local LAN/Docker/VPN host ICE candidates via mDNS (default true)")
	stunServers := fs.String("stun-servers", fileCfg.STUNServers, "Comma-separated STUN server URLs")
	turnURL := fs.String("turn-url", fileCfg.TURNURL, "Optional TURN server URL")
	turnUser := fs.String("turn-user", fileCfg.TURNUser, "Optional TURN username")
	turnPass := fs.String("turn-pass", "", "Optional TURN password (prefer TURN_PASS_FILE or TURN_PASS)")
	maxStreams := fs.Int("max-streams", fileCfg.MaxStreams, "Max concurrent global streams (default 512)")
	metricsAddr := fs.String("metrics-addr", fileCfg.MetricsAddr, "Optional Prometheus /metrics + /healthz listen address (prefer loopback)")
	portalURL := fs.String("portal-url", fileCfg.PortalURL, "Portal base URL for printing pinned/share links")

	mintLink := fs.String("mint-link", "", "Mint a scoped Ed25519 capability share link with duration (e.g. 2h, 7d) and exit")
	mintPath := fs.String("mint-path", "", "Optional path prefix scope for -mint-link (e.g. /public)")
	mintReadonly := fs.Bool("mint-readonly", false, "Restrict -mint-link to GET/HEAD/OPTIONS only (blocks WS)")
	mintMaxConns := fs.Int("mint-max-conns", 0, "Optional max concurrent connections for -mint-link")

	healthFlag := fs.Bool("health", false, "Probe local Agent /healthz endpoint and exit")
	healthURL := fs.String("health-url", "", "Optional custom URL for -health")
	_ = cfgFlag
	_ = fs.Parse(rawArgs)

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	applyEnvStr := func(flagName, envName string, dst *string) {
		if !explicit[flagName] {
			if v, ok := os.LookupEnv(envName); ok && v != "" {
				*dst = v
			}
		}
	}
	applyEnvBool := func(flagName, envName string, dst *bool) {
		if !explicit[flagName] {
			b, set, err := config.ParseEnvBool(envName)
			if err != nil {
				log.Fatalf("[Agent] %v", err)
			}
			if set {
				*dst = b
			}
		}
	}
	applyEnvInt := func(flagName, envName string, minVal, maxVal int, dst *int) {
		if !explicit[flagName] {
			n, set, err := config.ParseEnvInt(envName, minVal, maxVal)
			if err != nil {
				log.Fatalf("[Agent] %v", err)
			}
			if set {
				*dst = n
			}
		}
	}

	applyEnvStr("id", "NODE_ID", nodeID)
	applyEnvStr("signal", "SIGNAL_URL", signalURL)
	applyEnvBool("allow-insecure-signal", "ALLOW_INSECURE_SIGNAL", allowInsecureSignal)
	applyEnvBool("force-rekey", "FORCE_REKEY", forceRekey)
	applyEnvBool("allow-public", "ALLOW_PUBLIC", allowPublic)
	applyEnvStr("identity-key", "IDENTITY_KEY", identityKey)
	applyEnvStr("state-file", "STATE_FILE", stateFile)
	applyEnvBool("ephemeral-identity", "EPHEMERAL_IDENTITY", ephemeralID)
	applyEnvStr("target", "TARGET_URL", targetURL)
	applyEnvStr("dir", "DIR_PATH", dirPath)
	applyEnvStr("file", "FILE_PATH", filePath)
	applyEnvBool("hide-dotfiles", "P2P_HIDE_DOTFILES", hideDotfiles)
	applyEnvBool("file-inline-active", "FILE_INLINE_ACTIVE", fileInlineActive)
	applyEnvStr("upstream-host", "UPSTREAM_HOST", upstreamHost)
	applyEnvStr("proxy", "PROXY_URL", proxyURLRaw)
	applyEnvStr("sni", "SNI", sni)
	applyEnvBool("insecure-target", "INSECURE_TARGET", insecureTarget)
	applyEnvBool("ice-mdns", "ICE_MDNS", iceMDNS)
	applyEnvStr("stun-servers", "STUN_SERVERS", stunServers)
	applyEnvStr("turn-url", "TURN_URL", turnURL)
	applyEnvStr("turn-user", "TURN_USER", turnUser)
	applyEnvInt("max-streams", "MAX_STREAMS", 1, 16384, maxStreams)
	applyEnvStr("metrics-addr", "METRICS_ADDR", metricsAddr)
	applyEnvStr("portal-url", "PORTAL_URL", portalURL)

	if *healthFlag {
		probeURL := strings.TrimSpace(*healthURL)
		if probeURL == "" && strings.TrimSpace(*metricsAddr) != "" {
			rawAddr := strings.TrimSpace(*metricsAddr)
			host, port, splitErr := net.SplitHostPort(rawAddr)
			if splitErr != nil {
				fmt.Fprintf(os.Stderr, "agent health check: invalid -metrics-addr %q: %v\n", rawAddr, splitErr)
				os.Exit(1)
			}
			switch host {
			case "", "0.0.0.0":
				host = "127.0.0.1"
			case "::":
				host = "::1"
			}
			probeURL = "http://" + net.JoinHostPort(host, port) + "/healthz"
		}
		if probeURL == "" {
			fmt.Fprintln(os.Stderr, "agent health check requires -metrics-addr / METRICS_ADDR or -health-url")
			os.Exit(1)
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(probeURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent health check failed: %v\n", err)
			os.Exit(1)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "agent health check failed: HTTP %d\n", resp.StatusCode)
			os.Exit(1)
		}
		os.Exit(0)
	}

	if *nodeID == "" {
		*nodeID = "homeserver"
	}
	normNodeID, ok := signaling.NormalizeAndValidateNodeID(*nodeID)
	if !ok {
		log.Fatalf("[Agent] Invalid node ID %q (must be lowercase alphanumeric/hyphen/dot, never sbx-*)", *nodeID)
	}
	*nodeID = normNodeID

	if *signalURL == "" {
		*signalURL = "ws://127.0.0.1:8888/ws"
	}
	if *maxStreams == 0 {
		*maxStreams = agent.DefaultMaxGlobalStreams
	} else if *maxStreams < 1 || *maxStreams > 16384 {
		log.Fatalf("[Agent] max-streams out of range (%d): must be 1..16384", *maxStreams)
	}

	var identity *agent.Identity
	var idErr error
	if *ephemeralID {
		identity, idErr = agent.GenerateEphemeralIdentity()
		log.Printf("[Agent] WARNING: running with ephemeral Ed25519 identity; pinned URLs will break on restart")
	} else {
		keyPath := *identityKey
		if keyPath == "" {
			cfgDir, err := os.UserConfigDir()
			if err != nil {
				log.Fatalf("[Agent] Cannot resolve user config dir for identity key (pass -identity-key or -ephemeral-identity): %v", err)
			}
			keyPath = filepath.Join(cfgDir, "webrtc-gateway", normNodeID+".ed25519.key")
		}
		identity, idErr = agent.LoadOrCreateIdentity(keyPath)
	}
	if idErr != nil {
		log.Fatalf("[Agent] Identity key error: %v", idErr)
	}

	if *mintLink != "" {
		dur, err := parseFlexDuration(*mintLink)
		if err != nil {
			log.Fatalf("[Agent] Invalid -mint-link duration %q: %v", *mintLink, err)
		}
		var jtiBuf [12]byte
		if _, err := rand.Read(jtiBuf[:]); err != nil {
			log.Fatalf("[Agent] CSPRNG failure generating capability JTI: %v", err)
		}
		jti := "cap-" + base64.RawURLEncoding.EncodeToString(jtiBuf[:])
		payload := agent.CapabilityPayload{
			V:          agent.CapabilityVersion,
			JTI:        jti,
			Node:       normNodeID,
			Exp:        time.Now().Add(dur).Unix(),
			PathPrefix: strings.TrimSpace(*mintPath),
			Readonly:   *mintReadonly,
			MaxConns:   *mintMaxConns,
		}
		tok, err := identity.MintCapability(payload)
		if err != nil {
			log.Fatalf("[Agent] Mint capability failed: %v", err)
		}
		statePath := strings.TrimSpace(*stateFile)
		if statePath == "" && identity.KeyPath != "" {
			statePath = identity.KeyPath + ".state.json"
		}
		if statePath != "" {
			capStore, serr := agent.NewCapabilityStoreWithFile(statePath)
			if serr != nil {
				log.Fatalf("[Agent] Load capability state %q failed: %v", statePath, serr)
			}
			if terr := capStore.Track(payload); terr != nil {
				log.Fatalf("[Agent] Persist minted capability to %q failed: %v", statePath, terr)
			}
		}
		base := strings.TrimRight(*portalURL, "/")
		if base == "" {
			base = "https://portal.example.com"
		}
		fmt.Printf("%s/#%s?pk=%s&cap=%s\n", base, normNodeID, identity.PKHash(), tok)
		os.Exit(0)
	}

	resolvedSignalSecret, sigFromCLI, err := config.ResolveSecret(
		*signalSecret, explicit["signal-secret"],
		"SIGNAL_SECRET", "SIGNAL_SECRET_FILE", fileCfg.SignalSecret,
	)
	if err != nil {
		log.Fatalf("[Agent] %v", err)
	}
	resolvedAccessSecret, accFromCLI, err := config.ResolveSecret(
		*accessSecret, explicit["access-secret"],
		"ACCESS_SECRET", "ACCESS_SECRET_FILE", fileCfg.AccessSecret,
	)
	if err != nil {
		log.Fatalf("[Agent] %v", err)
	}
	resolvedTurnPass, turnFromCLI, err := config.ResolveSecret(
		*turnPass, explicit["turn-pass"],
		"TURN_PASS", "TURN_PASS_FILE", fileCfg.TURNPass,
	)
	if err != nil {
		log.Fatalf("[Agent] %v", err)
	}
	if sigFromCLI || accFromCLI || turnFromCLI {
		log.Printf("[Agent] WARNING: secret passed via CLI flag (visible in process list); prefer *_FILE or environment variables")
	}

	var parsedProxyURL *url.URL
	if strings.TrimSpace(*proxyURLRaw) != "" {
		u, perr := url.Parse(strings.TrimSpace(*proxyURLRaw))
		if perr != nil || u.Scheme == "" || u.Host == "" {
			log.Fatalf("[Agent] Invalid -proxy URL (Fail-Closed)")
		}
		parsedProxyURL = u
	}

	modeCount := 0
	if *targetURL != "" {
		modeCount++
	}
	if *dirPath != "" {
		modeCount++
	}
	if *filePath != "" {
		modeCount++
	}
	if modeCount != 1 {
		log.Fatalf("[Agent] Specify exactly ONE of -target, -dir, or -file")
	}

	var httpProxy *proxy.HTTPProxy
	var wsProxy *proxy.WSProxy
	var staticHandler *proxy.StaticFSHandler
	isStaticMode := false

	if *targetURL != "" {
		pol, err := proxy.NewDestinationPolicy(*targetURL, *upstreamHost)
		if err != nil {
			if errors.Is(err, proxy.ErrInvalidUpstreamHost) {
				log.Fatalf("[Agent] Invalid -upstream-host (must be valid RFC 9110 host[:port] without scheme, path, or userinfo)")
			}
			log.Fatalf("[Agent] Invalid -target URL (check scheme and host)")
		}
		httpProxy, err = proxy.NewHTTPProxy(proxy.HTTPProxyOptions{
			Policy:             pol,
			InsecureSkipVerify: *insecureTarget,
			SNI:                *sni,
			ProxyURL:           parsedProxyURL,
		})
		if err != nil {
			log.Fatalf("[Agent] HTTPProxy init failed: %v", err)
		}
		wsProxy, err = proxy.NewWSProxy(proxy.WSProxyOptions{
			Policy:             pol,
			InsecureSkipVerify: *insecureTarget,
			SNI:                *sni,
			ProxyURL:           parsedProxyURL,
		})
		if err != nil {
			log.Fatalf("[Agent] WSProxy init failed: %v", err)
		}
	} else {
		if strings.TrimSpace(*upstreamHost) != "" {
			log.Fatalf("[Agent] -upstream-host is only valid with -target")
		}
		isStaticMode = true
		staticHandler, err = proxy.NewStaticFSHandler(proxy.StaticFSOptions{
			RootDir:      *dirPath,
			SingleFile:   *filePath,
			HideDotfiles: *hideDotfiles,
			InlineActive: *fileInlineActive,
		})
		if err != nil {
			log.Fatalf("[Agent] Static os.Root init failed: %v", err)
		}
		defer staticHandler.Close()

		pol, err := proxy.NewDestinationPolicy("http://static.internal", "")
		if err != nil {
			log.Fatalf("[Agent] Static policy init failed: %v", err)
		}
		httpProxy, err = proxy.NewHTTPProxy(proxy.HTTPProxyOptions{
			Policy:          pol,
			CustomTransport: proxy.NewInProcessTransport(staticHandler),
		})
		if err != nil {
			log.Fatalf("[Agent] Static HTTPProxy init failed: %v", err)
		}
	}

	ag, err := agent.New(agent.Config{
		NodeID:              normNodeID,
		SignalURL:           *signalURL,
		SignalSecret:        resolvedSignalSecret,
		AllowInsecureSignal: *allowInsecureSignal,
		ForceRekey:          *forceRekey,
		AccessSecret:        resolvedAccessSecret,
		AllowPublic:         *allowPublic,
		Identity:            identity,
		StateFile:           *stateFile,
		HTTPProxy:           httpProxy,
		WSProxy:             wsProxy,
		IsStaticMode:        isStaticMode,
		ICEMDNS:             *iceMDNS,
		STUNServers:         config.SplitNonEmptyCSV(*stunServers),
		TURNURL:             *turnURL,
		TURNUser:            *turnUser,
		TURNPass:            resolvedTurnPass,
		MaxGlobalStream:     *maxStreams,
	})
	if err != nil {
		log.Fatalf("[Agent] Startup rejected: %v", err)
	}
	// Clear plaintext secret references from startup config structs after SPAKE2+ verifier derivation.
	fileCfg.AccessSecret = ""
	fileCfg.SignalSecret = ""
	fileCfg.TURNPass = ""
	resolvedAccessSecret = ""
	*accessSecret = ""

	var msrv *http.Server
	if *metricsAddr != "" {
		if !signaling.IsLoopbackBindAddr(*metricsAddr) {
			log.Printf("[Agent] WARNING: metrics-addr %q binds non-loopback; restrict access via firewall", *metricsAddr)
		}
		mmux := http.NewServeMux()
		mmux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			_, _ = fmt.Fprint(w, ag.Metrics().ExportPrometheus(ag.GlobalMemUsed()))
		})
		mmux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if !ag.IsSignalingRegistered() {
				w.WriteHeader(http.StatusServiceUnavailable)
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprintf(w, `{"status":"connecting","version":%q,"node":%q}`, Version, normNodeID)
				}
				return
			}
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q,"node":%q}`, Version, normNodeID)
			}
		})
		msrv = &http.Server{
			Addr:              *metricsAddr,
			Handler:           mmux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    64 * 1024,
		}
		go func() {
			log.Printf("[Agent] Metrics/Health listening on %s", *metricsAddr)
			if err := msrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("[Agent] Metrics error: %v", err)
			}
		}()
	}

	targetDisplay := *dirPath
	if *filePath != "" {
		targetDisplay = *filePath
	} else if *targetURL != "" {
		if tu, perr := url.Parse(*targetURL); perr == nil {
			targetDisplay = tu.Redacted()
		}
	}
	log.Printf("[Agent %s] Node=%s | PK=%s | Target=%s | mDNS=%v | Public=%v",
		Version, normNodeID, identity.PKHash(), targetDisplay, *iceMDNS, *allowPublic)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	loopErr := ag.RunSignalingLoop(ctx)
	if msrv != nil {
		sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = msrv.Shutdown(sctx)
		scancel()
	}
	if loopErr != nil && ctx.Err() == nil {
		log.Fatalf("[Agent] Fatal signaling error: %v", loopErr)
	}
	log.Printf("[Agent] Clean shutdown complete.")
}

func parseFlexDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	const maxDays = 365
	const maxDur = maxDays * 24 * time.Hour
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || days <= 0 || days > maxDays {
			return 0, fmt.Errorf("days must be between 1 and %d", maxDays)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > maxDur {
		return 0, fmt.Errorf("duration must be >0 and <=365d")
	}
	return d, nil
}
