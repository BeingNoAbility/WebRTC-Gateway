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
var Version = "dev"

const defaultSTUNServers = "stun:stun.cloudflare.com:3478,stun:stun.l.google.com:19302"

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
	FollowSymlinks      bool   `yaml:"follow_symlinks"`
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

func main() {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	showVerShort := fs.Bool("v", false, "Print version and exit")
	showVerLong := fs.Bool("version", false, "Print version and exit")
	cfgFlag := fs.String("config", "", "Explicit YAML/JSON config path (also CONFIG_FILE; never auto-loaded from CWD)")
	nodeID := fs.String("id", "", "Node ID (lowercase DNS-safe, default homeserver)")
	signalURL := fs.String("signal", "", "Signaling server base URL (e.g. wss://portal.example.com/ws)")
	signalSecret := fs.String("signal-secret", "", "Shared signaling secret (prefer SIGNAL_SECRET_FILE or SIGNAL_SECRET)")
	allowInsecureSignal := fs.Bool("allow-insecure-signal", false, "Allow sending SIGNAL_SECRET over plaintext non-loopback ws:// (dev only)")
	forceRekey := fs.Bool("force-rekey", false, "Force update bound Ed25519 key on signaling server (requires SIGNAL_SECRET)")
	accessSecret := fs.String("access-secret", "", "Visitor access password (prefer ACCESS_SECRET_FILE or ACCESS_SECRET)")
	allowPublic := fs.Bool("allow-public", false, "Explicitly permit visitor connections without ACCESS_SECRET")
	identityKey := fs.String("identity-key", "", "Path to persistent Ed25519 private key PEM")
	stateFile := fs.String("state-file", "", "Optional path for persistent capability revocation state JSON")
	ephemeralID := fs.Bool("ephemeral-identity", false, "Use ephemeral in-memory Ed25519 identity (breaks pinned links across restarts)")
	targetURL := fs.String("target", "", "Backend HTTP/HTTPS target URL (e.g. http://127.0.0.1:8080)")
	dirPath := fs.String("dir", "", "Serve a local directory read-only via os.Root adapter")
	filePath := fs.String("file", "", "Serve a single local file read-only via os.Root adapter")
	hideDotfiles := fs.Bool("hide-dotfiles", true, "Hide dotfiles and symlinks pointing to dotfiles in -dir/-file mode")
	fileInlineActive := fs.Bool("file-inline-active", false, "Allow inline script execution on static files (disables CSP: sandbox)")
	followSymlinks := fs.Bool("follow-symlinks", false, "Allow following in-root symlinks in -dir/-file mode (default false: Fail-Closed O_NOFOLLOW)")
	upstreamHost := fs.String("upstream-host", "", "Optional Host header override (RFC 9110 host[:port]) for virtual-host -target backends")
	proxyURLRaw := fs.String("proxy", "", "Optional outbound HTTP/SOCKS proxy URL for -target (use standard HTTPS_PROXY/HTTP_PROXY env vars for signaling egress)")
	sni := fs.String("sni", "", "Optional TLS SNI override for HTTPS -target")
	insecureTarget := fs.Bool("insecure-target", false, "Skip TLS verification for self-signed HTTPS -target ONLY")
	iceMDNS := fs.Bool("ice-mdns", true, "Obfuscate local LAN/Docker/VPN host ICE candidates via mDNS (default true)")
	stunServers := fs.String("stun-servers", "", "Comma-separated STUN server URLs (default built-in STUN; set 'none' to disable)")
	turnURL := fs.String("turn-url", "", "Optional TURN server URL (turn: or turns:)")
	turnUser := fs.String("turn-user", "", "Optional TURN username")
	turnPass := fs.String("turn-pass", "", "Optional TURN password (prefer TURN_PASS_FILE or TURN_PASS)")
	maxStreams := fs.Int("max-streams", 0, "Max concurrent global streams (default 512)")
	metricsAddr := fs.String("metrics-addr", "", "Optional Prometheus /metrics + /healthz listen address (prefer loopback)")
	portalURL := fs.String("portal-url", "", "Portal base URL for printing pinned/share links")

	mintLink := fs.String("mint-link", "", "Mint a scoped Ed25519 capability share link with duration (e.g. 2h, 7d) and exit")
	mintPath := fs.String("mint-path", "", "Optional path prefix scope for -mint-link (e.g. /public)")
	mintReadonly := fs.Bool("mint-readonly", false, "Restrict -mint-link to GET/HEAD/OPTIONS only (blocks WS)")
	mintMaxConns := fs.Int("mint-max-conns", 0, "Optional max concurrent connections for -mint-link")
	revokeCap := fs.String("revoke-cap", "", "Revoke a capability token by JTI in the persistent state file and exit")

	healthFlag := fs.Bool("health", false, "Probe local Agent /healthz endpoint and exit")
	healthURL := fs.String("health-url", "", "Optional custom URL for -health")
	_ = fs.Parse(os.Args[1:])

	if *showVerShort || *showVerLong {
		fmt.Printf("webrtc-gateway-agent %s\n", Version)
		os.Exit(0)
	}
	if fs.NArg() > 0 {
		log.Fatalf("[Agent] Unexpected positional arguments: %v", fs.Args())
	}

	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	var fileCfg agentFileConfig
	if cfgPath := config.ResolveExplicitConfigPath(*cfgFlag); cfgPath != "" {
		if err := config.LoadYAMLFile(cfgPath, &fileCfg); err != nil {
			log.Fatalf("[Agent] Failed to load config: %v", err)
		}
	}

	resolveStr := func(flagName, envName, fileVal, defaultVal string, ptr *string) {
		*ptr = config.ResolveString(*ptr, explicit[flagName], envName, fileVal, defaultVal)
	}
	resolveBool := func(flagName, envName string, cfgPtr *bool, cfgBool, defaultVal bool, ptr *bool) {
		v, _, err := config.ResolveBool(*ptr, explicit[flagName], envName, cfgPtr, cfgBool, defaultVal)
		if err != nil {
			log.Fatalf("[Agent] %v", err)
		}
		*ptr = v
	}
	resolveInt := func(flagName, envName string, fileVal, defaultVal, minVal, maxVal int, ptr *int) {
		v, err := config.ResolveInt(*ptr, explicit[flagName], envName, fileVal, defaultVal, minVal, maxVal)
		if err != nil {
			log.Fatalf("[Agent] %v", err)
		}
		*ptr = v
	}

	resolveStr("id", "NODE_ID", fileCfg.NodeID, "homeserver", nodeID)
	resolveStr("signal", "SIGNAL_URL", fileCfg.SignalURL, "ws://127.0.0.1:8888/ws", signalURL)
	resolveBool("allow-insecure-signal", "ALLOW_INSECURE_SIGNAL", nil, fileCfg.AllowInsecureSignal, false, allowInsecureSignal)
	resolveBool("force-rekey", "FORCE_REKEY", nil, fileCfg.ForceRekey, false, forceRekey)
	resolveBool("allow-public", "ALLOW_PUBLIC", nil, fileCfg.AllowPublic, false, allowPublic)
	resolveStr("identity-key", "IDENTITY_KEY", fileCfg.IdentityKey, "", identityKey)
	resolveStr("state-file", "STATE_FILE", fileCfg.StateFile, "", stateFile)
	resolveBool("ephemeral-identity", "EPHEMERAL_IDENTITY", nil, fileCfg.EphemeralIdentity, false, ephemeralID)
	resolveStr("target", "TARGET_URL", fileCfg.TargetURL, "", targetURL)
	resolveStr("dir", "DIR_PATH", fileCfg.DirPath, "", dirPath)
	resolveStr("file", "FILE_PATH", fileCfg.FilePath, "", filePath)
	resolveBool("hide-dotfiles", "P2P_HIDE_DOTFILES", fileCfg.HideDotfiles, false, true, hideDotfiles)
	resolveBool("file-inline-active", "FILE_INLINE_ACTIVE", nil, fileCfg.FileInlineActive, false, fileInlineActive)
	resolveBool("follow-symlinks", "FOLLOW_SYMLINKS", nil, fileCfg.FollowSymlinks, false, followSymlinks)
	resolveStr("upstream-host", "UPSTREAM_HOST", fileCfg.UpstreamHost, "", upstreamHost)
	resolveStr("proxy", "PROXY_URL", fileCfg.ProxyURL, "", proxyURLRaw)
	resolveStr("sni", "SNI", fileCfg.SNI, "", sni)
	resolveBool("insecure-target", "INSECURE_TARGET", nil, fileCfg.InsecureTarget, false, insecureTarget)
	resolveBool("ice-mdns", "ICE_MDNS", fileCfg.ICEMDNS, false, true, iceMDNS)
	resolveStr("stun-servers", "STUN_SERVERS", fileCfg.STUNServers, defaultSTUNServers, stunServers)
	resolveStr("turn-url", "TURN_URL", fileCfg.TURNURL, "", turnURL)
	resolveStr("turn-user", "TURN_USER", fileCfg.TURNUser, "", turnUser)
	resolveInt("max-streams", "MAX_STREAMS", fileCfg.MaxStreams, agent.DefaultMaxGlobalStreams, 1, 16384, maxStreams)
	resolveStr("metrics-addr", "METRICS_ADDR", fileCfg.MetricsAddr, "", metricsAddr)
	resolveStr("portal-url", "PORTAL_URL", fileCfg.PortalURL, "", portalURL)

	if *healthFlag {
		probeURL, err := resolveAgentHealthURL(
			config.ResolveString(*healthURL, explicit["health-url"], "HEALTH_URL", "", ""),
			*metricsAddr,
		)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agent health check: %v\n", err)
			if errors.Is(err, errAgentHealthNoEndpoint) {
				os.Exit(2)
			}
			os.Exit(1)
		}
		if err := runAgentHealthCheck(probeURL); err != nil {
			fmt.Fprintf(os.Stderr, "agent health check failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	normNodeID, ok := signaling.NormalizeAndValidateNodeID(*nodeID)
	if !ok {
		log.Fatalf("[Agent] Invalid node ID %q (must be lowercase alphanumeric/hyphen/dot, never sbx-*)", *nodeID)
	}
	*nodeID = normNodeID

	if *ephemeralID && strings.TrimSpace(*identityKey) != "" {
		log.Fatalf("[Agent] -ephemeral-identity and -identity-key are mutually exclusive (Fail-Closed)")
	}
	if *ephemeralID && *mintLink != "" {
		log.Fatalf("[Agent] -mint-link cannot be used with -ephemeral-identity (Fail-Closed)")
	}
	if *mintMaxConns < 0 {
		log.Fatalf("[Agent] -mint-max-conns cannot be negative (%d)", *mintMaxConns)
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
			PathPrefix: normalizeMintPath(*mintPath),
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

	if strings.TrimSpace(*revokeCap) != "" {
		if *ephemeralID {
			log.Fatalf("[Agent] -revoke-cap requires persistent identity/state (cannot combine with -ephemeral-identity)")
		}
		statePath := strings.TrimSpace(*stateFile)
		if statePath == "" && identity.KeyPath != "" {
			statePath = identity.KeyPath + ".state.json"
		}
		if statePath == "" {
			log.Fatalf("[Agent] -revoke-cap requires a persistent -state-file or -identity-key")
		}
		capStore, serr := agent.NewCapabilityStoreWithFile(statePath)
		if serr != nil {
			log.Fatalf("[Agent] Load capability state %q failed: %v", statePath, serr)
		}
		jti := strings.TrimSpace(*revokeCap)
		if rerr := capStore.Revoke(jti); rerr != nil {
			log.Fatalf("[Agent] Revoke capability %q failed: %v", jti, rerr)
		}
		fmt.Printf("revoked %s\n", jti)
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

	parsedSTUN, err := parseSTUNServers(*stunServers)
	if err != nil {
		log.Fatalf("[Agent] Invalid -stun-servers: %v", err)
	}
	trimmedTurnURL := strings.TrimSpace(*turnURL)
	if trimmedTurnURL != "" {
		lt := strings.ToLower(trimmedTurnURL)
		if !strings.HasPrefix(lt, "turn:") && !strings.HasPrefix(lt, "turns:") {
			log.Fatalf("[Agent] Invalid -turn-url %q: must start with turn: or turns:", trimmedTurnURL)
		}
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
		if parsedProxyURL != nil || strings.TrimSpace(*sni) != "" || *insecureTarget {
			log.Fatalf("[Agent] -proxy, -sni, and -insecure-target are only valid with -target")
		}
		staticHandler, err = proxy.NewStaticFSHandler(proxy.StaticFSOptions{
			RootDir:        *dirPath,
			SingleFile:     *filePath,
			HideDotfiles:   *hideDotfiles,
			InlineActive:   *fileInlineActive,
			FollowSymlinks: *followSymlinks,
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
		ICEMDNS:             *iceMDNS,
		STUNServers:         parsedSTUN,
		TURNURL:             trimmedTurnURL,
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

func parseSTUNServers(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, "none") || strings.EqualFold(trimmed, "off") {
		return nil, nil
	}
	parts := config.SplitNonEmptyCSV(trimmed)
	for _, s := range parts {
		ls := strings.ToLower(s)
		if !strings.HasPrefix(ls, "stun:") && !strings.HasPrefix(ls, "stuns:") {
			return nil, fmt.Errorf("entry %q must use stun: or stuns: scheme", s)
		}
	}
	return parts, nil
}

var errAgentHealthNoEndpoint = errors.New("requires -metrics-addr / METRICS_ADDR or -health-url / HEALTH_URL")

func resolveAgentHealthURL(healthURL, metricsAddr string) (string, error) {
	probeURL := strings.TrimSpace(healthURL)
	if probeURL != "" {
		return probeURL, nil
	}
	rawAddr := strings.TrimSpace(metricsAddr)
	if rawAddr == "" {
		return "", errAgentHealthNoEndpoint
	}
	host, port, err := net.SplitHostPort(rawAddr)
	if err != nil {
		return "", fmt.Errorf("invalid -metrics-addr %q: %w", rawAddr, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func runAgentHealthCheck(probeURL string) error {
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(probeURL)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func normalizeMintPath(raw string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
