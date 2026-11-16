# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [v1.5.0] - 2026-11-16

### Added
- **Unified YAML / JSON Configuration File Support (`-config`)**:
  - Both `agent` and `server` binaries support structured configuration files via `gopkg.in/yaml.v3`. Supports YAML with comments and standard JSON syntax.
  - **Four-Tier Precedence Hierarchy**: `Defaults < Configuration File < Environment Variables < CLI Flags`.
  - **Zero-Config Auto-Detection**: Automatically searches for `./agent.yaml`, `./agent.yml`, `./agent.json` (or `server.yaml`, `server.yml`, `server.json`) in current and binary directories; continues silently if none are present.
  - **Fail-Fast Explicit Mode**: If `-config` or `CONFIG_FILE` is explicitly provided but the file is missing or contains syntax errors, startup terminates immediately with informative error messages.
  - **Flexible Type Ingestion**: Enhanced fields such as `stun_servers` to accept both YAML lists (`["stun:..."]`) and comma-separated strings (`"stun:...,stun:..."`).
  - Added template examples: `configs/agent.example.yaml` and `configs/server.example.yaml`.

### Changed & Security
- **Secret Architecture Decoupling (Control Plane vs Data Plane)**:
  - **Signaling Cluster Secret (`-signal-secret` / `SIGNAL_SECRET`)**: Dedicated to Agent-to-Server registration and node anti-hijacking. Never exposed to visitor browsers or frontend WebSockets. (Server accepts `-secret` / `SECRET` as an alias for backward compatibility).
  - **Visitor Access Password (`-access-secret` / `ACCESS_SECRET`)**: Dedicated to visitor browser client authorization.
  - **True Public Node Mode**: When `-access-secret` is omitted, the node operates in public access mode. Visitor browsers connect directly via WebRTC DataChannel without entering any password, even if the signaling server enforces a cluster master secret.
  - **Fail-Fast Deprecation on Agent**: The ambiguous legacy `-secret` flag and `SECRET` environment variable are completely deprecated on the Agent daemon, terminating startup with explicit upgrade instructions to eliminate security misconfiguration risks.
  - **Temporary Link Signing Alignment**: Agent `-sign-link` generates HMAC-SHA256 signatures derived from `-access-secret`, and alerts immediately if invoked on a public node without an access secret configured.
  - **Edge Worker Signaling Room Alignment**: Updated Cloudflare Worker signaling room implementation to support `require_visitor_auth` decoupling.

---

## [v1.4.1] - 2026-11-16

### Security
- **Signaling Server Trusted Proxies & Anti-Spoofing (`-trusted-proxies` / `TRUSTED_PROXIES`)**:
  - Implemented strict proxy trust verification against client IP header spoofing (`X-Forwarded-For`, `CF-Connecting-IP`, `X-Real-IP`).
  - Pre-compiled zero-allocation CIDR matching using Go `netip.Prefix`.
  - **Zero-Config Compatibility**: Defaults to trusting loopback (`127.0.0.0/8`, `::1/128`), RFC 1918 private networks (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`), CGNAT (`100.64.0.0/10`), link-local, and IPv6 ULA, ensuring out-of-the-box compatibility with Docker bridges, Kubernetes Ingress, and Nginx/Caddy reverse proxies.
  - **Public Socket Fallback**: Direct untrusted connections bypassing proxies strictly ignore proxy headers and bind directly to the underlying TCP socket address, preventing attackers from spoofing IPs to bypass rate limits and auth brute-force locks.
  - **Priority Ingestion**: Evaluates `CF-Connecting-IP` before `X-Forwarded-For` and `X-Real-IP` when incoming requests originate from trusted proxies.

---

## [v1.4.0] - 2026-11-16

### Added
- **WebRTC Diagnostics HUD & Observability**: Real-time stats engine (`pc.getStats`) embedded into the web portal with an interactive HUD modal (`p2pHudModal`) displaying connection mode (`⚡ Direct P2P` vs `🛡️ Relay TURN`), selected candidate pair (local & remote IP/port/protocol/candidateType), live latency (RTT), downlink/uplink bitrates and throughput (MB), packet loss, and on-demand "🔄 测试 ICE Restart (网络漫游自愈)" action.
- **Enhanced Connection Pill**: Live floating pill in top-right displaying real-time connection status badge (`⚡ P2P` or `🛡️ TURN`), RTT latency in milliseconds, and live download speed (`↓ XX KB/s`), clicking opens the diagnostic HUD.
- **Prometheus Metrics Exporter**:
  - **Signaling Server**: Native `/metrics` endpoint exposing `webrtc_gateway_registered_nodes`, `webrtc_gateway_active_clients`, `webrtc_gateway_signaling_messages_total`, and `webrtc_gateway_ratelimit_rejected_total`. Configurable via `-metrics-addr` / `METRICS_ADDR`.
  - **Agent Daemon (Strict Opt-in)**: Exposes `webrtc_gateway_active_peers`, `webrtc_gateway_active_streams`, `webrtc_gateway_max_streams`, `webrtc_gateway_traffic_bytes_total{direction="rx|tx"}`, and `webrtc_gateway_stream_rejections_total`. Default disabled (Pure Outbound Daemon with zero open listening ports); activated only when `-metrics-addr` / `METRICS_ADDR` is explicitly set.
- **Agent Capacity Governance & Backpressure**: Implemented concurrent stream semaphore limit (`-max-streams` / `MAX_STREAMS`, default 512). Reaching capacity gracefully returns standard HTTP 503 Service Unavailable with `Retry-After: 2` header, protecting host/NAS resources under high load.
- **Signaling Server Anti-DoS Rate Limiting**: Built-in `IPRateLimiter` enforcing max concurrent WebSockets per client IP (`-max-ip-conns` / `MAX_IP_CONNS`, default 64), sliding window message rate limits (120 msg / 10s), and authentication failure brute-force lockout (10 fails / min).
- **Edge Worker Anti-Abuse**: Cloudflare Worker SignalingRoom Durable Object updated with per-IP concurrent connection tracking and authentication failure lockout.

---

## [v1.3.0] - 2026-11-16

### Added
- **WebRTC Standard ICE Restart (Mobile Roaming Self-Healing)**: Implemented two-stage self-healing pipeline for network handoffs (e.g. Wi-Fi to 5G cellular, AP roaming). Seamlessly reconnects broken signaling sockets and renegotiates in-place on existing RTCPeerConnections with `iceRestart: true`.
- **Agent Teardown Grace Window**: Agent introduces a 5-second grace window upon connection disruption instead of immediately destructing connections. If an ICE Restart offer arrives within 5 seconds, DataChannels, Mux multiplexers, active streams, and session cookies are preserved 100% without user page reloads.
- **Stateless HMAC-SHA256 Signed Share Links**: Pure mathematical zero-database authorization scheme (`#nodeId?exp=timestamp&sign=hex_hmac`). Allows temporary access to protected nodes without exposing permanent master secrets. Naturally expires after expiration timestamp.
- **1-Click Share Link Generator in Web UI**: Interactive generator in settings modal supporting 1h, 6h, 24h, 7d, 30d durations using browser `crypto.subtle` HMAC-SHA256.
- **CLI Share Link Generator**: Added `-sign-link <duration>` and `-portal-url <url>` flags to `agent` binary to easily generate temporary shareable links from the command line.
- **Hash Parameter Retention**: Enhanced route synchronization engine to preserve temporary share signatures (`exp` and `sign`) across iframe sub-route navigation.

---

## [v1.2.0] - 2026-11-16

### Added
- **TURN Relay Support**: Introduced `--turn-url`, `--turn-user`, `--turn-pass` CLI flags and environment variables (`TURN_URL`, `TURN_USER`, `TURN_PASS`) to bypass strict symmetric NATs.
- **Domestic STUN Nodes**: Integrated high-availability STUN candidates (QQ, MiWiFi, Bilibili, Cloudflare) and added custom STUN configuration support (`--stun-servers`, `STUN_SERVERS`).
- **Session Persistence**: Decoupled WebRTC ICE transport lifecycle from business cookie sessions using long-lived client tokens and a 2-hour idle sliding window in `CookieJar`, preserving HttpOnly logins across F5 page reloads.
- **Bidirectional Route Synchronization**: Synchronized internal SPA routing to outer URL Hash (`#nodeId?path=/subpath`) with open-redirect and XSS filtering, restoring exact sub-routes upon refresh.
- **Durable Object Node Secret Protection**: Added anti-hijack secret verification in Cloudflare Worker (`web/worker.js` and `edge/cloudflare-worker/worker.js`).
- **Stream Idle Reaper**: Automatic background sweeping of inactive non-WebSocket streams to prevent resource leaks.

### Security
- **SSRF Hardening**: Implemented strict IP filtering at the TCP `DialContext` layer, blocking RFC 1918 private networks, loopback (127.0.0.1), link-local/cloud metadata (169.254.169.254, 100.100.100.200), and CGNAT.
- **DNS Rebinding (TOCTOU) Mitigation**: Direct dialing to verified safe resolved IP addresses to prevent time-of-check to time-of-use DNS rebinding attacks.
- **URL Query Secret Deprecation**: Deprecated `?secret=` query string; sensitive access tokens are strictly confined to URL Hash or user prompt.

### Performance
- **Mux Backpressure Decoupling**: Separated `writeMu` send lock and backpressure waiting. Control frames (Ping, Pong, Headers, RST, Stream 0) bypass backpressure queues, eliminating Head-of-Line (HoL) blocking during heavy media and file streaming.

---

## [v1.1.0] - 2026-11-15

### Added
- **Multi-Domain & Media Streaming**: Full proxy support for complex multi-CDN streaming media sites (Bilibili, video slices, dynamic audio).
- **Subdomain Routing**: Support for native browser Origin isolation via subdomain routing (`<node>.p2p.domain.com`).
- **Real Browser Testing Matrix**: 22/22 end-to-end scenarios covering HTTP semantics, WebSocket jumbo frames, upload/download speed, and Top 10 real websites.

---

## [v1.0.0] - 2026-11-14

### Initial Release
- Zero-client WebRTC P2P Reverse Proxy Gateway.
- Single static Go binary embedding Web portal assets via `go:embed`.
- Cloudflare Worker and standalone Go signaling server support.
- Stream multiplexing over WebRTC DataChannel (P2PMux).
