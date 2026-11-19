# Agent & Server Configuration Manual

WebRTC Gateway follows modern cloud-native Twelve-Factor App principles, supporting **Command-Line Flags (CLI)**, **Environment Variables (ENV)**, and **YAML / JSON Configuration Files**.

### Configuration Precedence
```text
Command-Line Flags (CLI)
  ➔ Environment Variables (ENV)
    ➔ Configuration File (YAML / JSON)
      ➔ Built-in Default Values
```

---

## 1. Agent Modes of Operation

The Agent client daemon supports three mutually exclusive operational modes:

| Mode | Parameters | Description | Typical Use Cases |
| :--- | :--- | :--- | :--- |
| **Mode A: Reverse Proxy** | `-target` / `TARGET_URL` | Reverse-proxy HTTP and WebSocket requests to an existing local or LAN service. | Jellyfin, NAS web dashboards, Vaultwarden, HomeAssistant. |
| **Mode B: Directory & Static Site** | `-dir` / `DIR_PATH` | Host a local folder as a read-only static site or directory tree with index.html fallback. | Documentation sites, frontend build artifacts (`dist`), public download directories. |
| **Mode C: Single File Sharing** | `-file` / `FILE_PATH` | Share a single local file with HTTP 206 Partial Content (Range seeking) support. | Private Wormhole-style distribution for ISOs, media files, and system logs. |

> [!NOTE]
> **Desktop Graphical User Interface (GUI) Note**:
> The core Agent is designed as a lightweight, headless command-line daemon. If you need a desktop graphical interface, refer to the experimental prototype [WebRTC-Gateway-GUI](https://github.com/BeingNoAbility/WebRTC-Gateway-GUI). Note that this GUI is a rapidly assembled prototype that is untested and outside the core management and support scope of this project.

---

## 2. Agent CLI Flags & Environment Variables

| CLI Flag | Environment Variable | Default | Description |
| :--- | :--- | :---: | :--- |
| `-node-id` | `NODE_ID` | `homeserver` | Unique peer identifier for this agent in the signaling cluster. |
| `-signal` | `SIGNAL_URL` | `ws://127.0.0.1:8888/ws` | Public signaling server WebSocket endpoint (`ws://` or `wss://`). |
| `-target` | `TARGET_URL` | `http://127.0.0.1:8080` | Local target service URL to reverse-proxy (mutually exclusive with `-dir`, `-file`). |
| `-dir` | `DIR_PATH` | `""` | Local directory path to serve statically (mutually exclusive with `-target`, `-file`). |
| `-file` | `FILE_PATH` | `""` | Single local file path to distribute (mutually exclusive with `-target`, `-dir`). |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | Path to YAML/JSON config file (auto-detects `./agent.yaml`, `./agent.yml`, `./agent.json`). |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | Control-plane cluster registration secret (prevents node hijacking; never exposed to visitors). |
| `-access-secret` | `ACCESS_SECRET` | `""` | Data-plane visitor password (empty = public node; set = requires password or HMAC link). |
| `-proxy` | `PROXY_URL` | `""` | Outbound proxy for signaling (`http://`, `https://`, `socks5://`; does not taint local target). |
| `-allowed-proxy-domains` | `ALLOWED_PROXY_DOMAINS` | `""` | Comma-separated domain wildcard whitelist for `/--p2p-proxy/` (e.g. `*.hdslb.com,*.bilivideo.com`). |
| `-sni` | `SNI` | `""` | TLS SNI and Host header override for bypassing DNS pollution on Anycast IPs. |
| `-insecure` | `INSECURE` | `false` | Skip TLS certificate verification when connecting to self-signed signaling endpoints. |
| `-turn-url` | `TURN_URL` | `""` | Optional TURN relay server address (e.g. `turn:relay.domain.com:3478`) for Symmetric NAT fallback. |
| `-turn-user` | `TURN_USER` | `""` | Optional TURN relay username. |
| `-turn-pass` | `TURN_PASS` | `""` | Optional TURN relay password. |
| `-stun-servers` | `STUN_SERVERS` | `""` | Comma-separated list of custom STUN servers (or array in YAML config). |
| `-max-streams` | `MAX_STREAMS` | `512` | Maximum concurrent multiplexed streams; returns 503 when exceeded to protect host memory. |
| `-metrics-addr` | `METRICS_ADDR` | `""` | Optional Prometheus metrics exporter address (e.g. `:9090`; disabled by default for outbound security). |
| `-sign-link` | - | `0` | Generates a time-limited share link (e.g. `1h`, `24h`) based on `-access-secret` and exits. |
| `-portal-url` | - | `https://your-portal-domain.com` | Base portal URL used for `-sign-link` output. |
| `-v`, `-version` | - | `false` | Display Agent version and exit. |
| `-h`, `-help` | - | `false` | Display command-line usage help and exit. |

---

## 3. Signal Server CLI Flags & Environment Variables

| CLI Flag | Environment Variable | Default | Description |
| :--- | :--- | :---: | :--- |
| `-addr` | `ADDR` / `PORT` | `:8888` | Address and port to bind HTTP/HTTPS and WebSocket listener on. |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | Path to YAML/JSON config file (auto-detects `./server.yaml`, `./server.yml`, `./server.json`). |
| `-allowed-origins` | `SIGNAL_ALLOWED_ORIGINS` / `ALLOWED_ORIGINS` | `""` (strict: same-origin only) | Whitelist of allowed browser Origins for WebSocket handshakes (comma-separated, wildcard subdomains `*.pages.dev`, port wildcards `localhost:*`). |
| `-allow-all-origins` | `SIGNAL_ALLOW_ALL_ORIGINS` / `ALLOW_ALL_ORIGINS` | `false` | Permit all cross-origin WebSocket signaling requests without whitelist (dev/test only). |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | Cluster signaling authorization secret (-secret is fully deprecated and blocked). |
| `-tls-cert` | `TLS_CERT` | `""` | Path to TLS certificate PEM file (e.g. fullchain.pem for native HTTPS). |
| `-tls-key` | `TLS_KEY` | `""` | Path to TLS private key PEM file (e.g. privkey.pem for native HTTPS). |
| `-tls-auto` | `TLS_AUTO` | `false` | Auto-generate in-memory ECDSA SAN certificate covering all LAN IPs for local Secure Context. |
| `-static` | `STATIC_DIR` | `./web/dist` | Static asset directory (defaults to embedded DistFS binary assets). |
| `-max-ip-conns` | `MAX_IP_CONNS` | `64` | Maximum concurrent WebSocket connections per IP address (DoS mitigation). |
| `-trusted-proxies` | `TRUSTED_PROXIES` | `""` (private CIDRs) | Trusted proxy CIDRs (defaults to loopback and RFC1918; set `none` to disable proxy headers). |
| `-metrics-addr` | `METRICS_ADDR` | `""` | Optional dedicated Prometheus metrics address (e.g. `:9090`; defaults to main `/metrics`). |
| `-health` | - | `false` | Run local healthcheck probe (exits 0 on healthy, 1 on failure; for Docker/K8s HEALTHCHECK). |
| `-health-url` | - | `""` | Custom healthcheck probe target URL (defaults to `-addr`/`ADDR` `/healthz`). |
| `-v`, `-version` | - | `false` | Display Server version and exit. |
| `-h`, `-help` | - | `false` | Display command-line usage help and exit. |

---

## 4. Configuration File Templates

### ① Agent Configuration (`agent.yaml`)
```yaml
node_id: "nas-home"
signal: "wss://p2p.yourdomain.com/ws"

# Select one operational mode:
target: "http://127.0.0.1:8096"
# dir: "/data/www"
# file: "/data/isos/Ubuntu-24.04.iso"

# Decoupled security credentials:
signal_secret: "cluster_secret_abc123" # Control plane: Cluster registration secret
access_secret: "visitor_pass_456"      # Data plane: Visitor access password

# Advanced network tuning:
stun_servers:
  - "stun:stun.cloudflare.com:3478"
  - "stun:stun.l.google.com:19302"

turn_url: "turn:turn.yourdomain.com:3478"
turn_user: "turnuser"
turn_pass: "turnpassword"

max_streams: 1024
metrics_addr: ":9090"
```

### ② Server Configuration (`server.yaml`)
```yaml
addr: ":443"
allowed_origins: "https://*.pages.dev,http://localhost:*"
signal_secret: "cluster_secret_abc123"

# TLS Configuration
tls_cert: "/etc/ssl/certs/fullchain.pem"
tls_key: "/etc/ssl/private/privkey.pem"
# tls_auto: false

# Production capacity and security policy
max_ip_conns: 128
trusted_proxies: "127.0.0.1/32,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
```
