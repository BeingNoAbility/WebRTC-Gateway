<p align="center">
  <img src="assets/logo-banner.svg" alt="Zero-Client WebRTC P2P Gateway Banner" width="820">
</p>

# Zero-Client P2P WebRTC Intranet Penetration Gateway
### End-to-End P2P WebRTC Reverse Proxy Gateway with Zero Client Dependencies in Visitor Browsers

<p align="center">
  <a href="README.md"><b>简体中文</b></a> · <a href="README_en.md"><b>English</b></a> · <a href="https://beingnoability.github.io/WebRTC-Gateway/"><b>📖 Official Docs</b></a>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![TypeScript](https://img.shields.io/badge/TypeScript-5.0%2B-3178C6?logo=typescript)](https://www.typescriptlang.org/)
[![WebRTC](https://img.shields.io/badge/WebRTC-Pion%20v4-FF6B6B?logo=webrtc)](https://github.com/pion/webrtc)
[![Docker](https://img.shields.io/badge/Docker-Multi--Arch-2496ED?logo=docker)](https://github.com/BeingNoAbility/WebRTC-Gateway/pkgs/container/webrtc-p2p-server)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![CI](https://github.com/BeingNoAbility/WebRTC-Gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/BeingNoAbility/WebRTC-Gateway/actions/workflows/ci.yml)

---

## 💡 What is Zero-Client P2P WebRTC Gateway?

This project introduces a revolutionary intranet penetration and remote access architecture: **"Zero client software, zero browser extensions, zero privileged root CA certificates, and zero network configuration on the visitor's side."**

In traditional penetration solutions (such as FRP, NPS, Ngrok, or Cloudflare Tunnel), all private intranet traffic must be relayed through public relay servers. This architecture is constrained by expensive and narrow bandwidth quotas on relay VPS instances, creates single-point bandwidth bottlenecks, and raises significant privacy concerns. On the other hand, VPN solutions like Tailscale, WireGuard, or ZeroTier mandate that visitors install dedicated desktop/mobile client applications or system-level TUN/TAP virtual network drivers.

This project is built upon **WebRTC RTCDataChannel (SCTP) + In-Browser Service Worker Transparent Interception & DOM Virtualization Sandbox**:
1. **Visitor Side**: Simply navigate to a portal link or custom subdomain in any modern browser (Chrome, Edge, Safari, Firefox, iOS/Android mobile browsers);
2. **Penetration Layer**: The browser automatically negotiates via lightweight signaling with the **Agent daemon** running inside your home intranet / private server, executes WebRTC NAT traversal (STUN/ICE), and establishes an **end-to-end encrypted direct peer-to-peer data channel (DTLS-SRTP)**;
3. **Application Experience**: All HTTP requests, API calls, cookies, media streaming, and WebSocket full-duplex communications are transmitted **directly between the browser and the intranet server**, consuming zero relay server traffic. Your backend web applications require **zero code modifications**!

---

## 📑 Table of Contents

- [🌟 Key Features](#-key-features)
- [📖 User Guide](#-user-guide)
  - [1. 30-Second Quickstart](#1-30-second-quickstart)
  - [2. Classic Intranet Application Practices](#2-classic-intranet-application-practices)
  - [3. Multi-Node & Subdomain Intelligent Routing](#3-multi-node--subdomain-intelligent-routing)
  - [4. Web Console & Node Settings Modal](#4-web-console--node-settings-modal)
  - [5. Agent Command-Line Flags & Environment Variables](#5-agent-command-line-flags--environment-variables)
  - [6. Integrated Signal Server CLI & Environment Variables](#6-integrated-signal-server-cli--environment-variables)
  - [7. Security & Access Control](#7-security--access-control)
- [🛠️ Production Deployment Guide (Self-Hosting & Operations)](#️-production-deployment-guide-self-hosting--operations)
  - [Option A: Cloudflare Workers Unified Edge Deployment (Zero Server Cost)](#option-a-cloudflare-workers-unified-edge-deployment-zero-server-cost)
  - [Option B: Self-Hosted Linux / Windows Standalone Server (Single Binary)](#option-b-self-hosted-linux--windows-standalone-server-single-binary)
  - [Option C: Docker & Docker Compose Containerized Deployment](#option-c-docker--docker-compose-containerized-deployment)
- [💻 Developer Guide](#-developer-guide)
  - [1. System Architecture & End-to-End Data Flow](#1-system-architecture--end-to-end-data-flow)
  - [2. P2PMux v1 Binary Streaming Multiplexing Protocol Specification](#2-p2pmux-v1-binary-streaming-multiplexing-protocol-specification)
  - [3. In-Browser DOM Virtualization & Service Worker Core Mechanisms](#3-in-browser-dom-virtualization--service-worker-core-mechanisms)
  - [4. Project Repository Structure Overview](#4-project-repository-structure-overview)
  - [5. Local Source Build & Automated Test Suite](#5-local-source-build--automated-test-suite)
- [📄 License](#-license)

---

## 🌟 Key Features

- 🚀 **True Zero-Client Experience**: Visitors only need a modern browser on a desktop or phone. Open the URL to access private intranet services immediately.
- ⚡ **P2P Direct Line Speed**: Powered by WebRTC ICE/STUN NAT traversal. Enjoy full upstream speeds of home gigabit broadband (verified LAN RTT of 1~3ms; cross-ISP P2P utilizes full broadband uplink without third-party throttling).
- 🔄 **WebRTC Standard ICE Restart (Mobile Roaming Self-Healing)**: When devices switch networks (e.g. Wi-Fi to 5G cellular, AP roaming, or elevator signal restoration), the client automatically reconnects the signaling socket and negotiates an in-place ICE Restart. Streaming media, WebSockets, and page sessions continue without reloading.
- 🔗 **Stateless HMAC-SHA256 Time-Limited Share Links**: Pure mathematical zero-database authorization. Generate temporary shareable links (`#nodeId?exp=xxx&sign=yyy`) with 1-click in the Web UI or CLI. Mathematically expires when elapsed without leaking master credentials.
- 📊 **WebRTC HUD Real-time Diagnostic Dashboard**: Floating status pill dynamically displays `⚡ P2P Direct` vs `🛡️ TURN Relay` connection status. 1-click opens an interactive diagnostics HUD displaying real-time RTT, downlink/uplink transfer rates, packet loss, and candidate IPs with on-demand ICE Restart testing.
- 🛡️ **Production Capacity Governance & Anti-DoS**: Agent features a semaphore-guarded capacity limit (`-max-streams`, default 512) that gracefully returns 503 to protect NAS memory; signaling server enforces per-IP concurrent connection quotas (`-max-ip-conns`), message rate limits, and brute-force lockouts.
- 🛡️ **Authoritative Client IP & Enterprise WAF Integration**: The Agent reverse proxy injects the authoritative client IP into standard `X-Forwarded-For` and `X-Real-IP` headers. Upstream WAFs (SafeLine, ModSecurity, Cloudflare WAF, Nginx `real_ip`) can accurately perform per-IP rate limiting, blacklisting, and GeoIP auditing without mistaking all traffic for `127.0.0.1`. Features strict anti-spoofing overwrite and mobile roaming in-place ICE Restart IP updates.
- 📈 **Prometheus Metrics Out-of-the-Box**: Signaling server serves `/metrics`; Agent strictly adheres to the pure outbound daemon principle and supports optional opt-in `-metrics-addr` to export peer counts, active streams, traffic bytes, and backpressure rejections.
- 🔒 **End-to-End Native Encryption**: Transport layer enforces DTLS-SRTP asymmetric negotiation and strong cryptography. The signaling broker only routes metadata; relay nodes cannot decrypt or inspect application traffic.
- 🌐 **Multi-Node & Subdomain Awareness**: Run multiple machines and services simultaneously. Route seamlessly via `<nodeId>.p2p.domain.com`, `#nodeId`, or interactive settings modal.
- 🧰 **Transparent Web Protocol Stack Virtualization**:
  - **HTTP/HTTPS**: Full method support (GET, POST, PUT, DELETE, PATCH, OPTIONS, HEAD), chunked streaming responses, and file downloads.
  - **HTTP 206 Partial Content**: Complete support for video/audio Range streaming; instant seeking and fast forwarding without waiting for full downloads.
  - **WebSocket Full-Duplex**: Built-in fragmentation and defragmentation across SCTP frames, supporting arbitrarily large payloads without frame drops.
  - **Cookie Session Persistence**: Agent isolates independent session CookieJars per visitor; non-HttpOnly cookies synchronize bidirectionally with the browser DOM.
  - **W3C Upload Progress Virtualization**: Overcomes Chromium's limitation where Service Worker fetch proxies fail to fire `xhr.upload.onprogress`. Upload progress bars advance smoothly in cloud drives and speed tests.
  - **Hotlink Protection Bypass & Sandbox Security**: Automatically strips restrictive Referrer policies; provides deep defenses against Anti-Frame-Busting techniques.
  - **New Tab / Window Node ID Persistence**: Service Worker injects `window.__P2P_NODE_ID__` into HTML streams, and `patch.js` rewrites popups to `?peer=${nodeId}#${nodeId}`, preventing new windows from falling back to subdomains and causing offline errors.

---

## 📖 User Guide

### 1. 30-Second Quickstart

Using this gateway requires only two steps: **launch the Agent in your intranet, then open the URL in your browser**.

#### Step 1: Launch Agent on the Intranet Target Machine

Download pre-compiled binaries from [Releases](https://github.com/BeingNoAbility/WebRTC-Gateway/releases) or build from source:

**Windows:**
```powershell
.\bin\agent.exe -node-id myhome -signal wss://signal.yourdomain.com/ws -target http://127.0.0.1:8080
# Local development test:
# .\bin\agent.exe -node-id myhome -signal ws://127.0.0.1:8888/ws -target http://127.0.0.1:8080
```

**Linux / NAS / Raspberry Pi:**
```bash
chmod +x ./bin/agent_linux_amd64
./bin/agent_linux_amd64 -node-id myhome -signal wss://signal.yourdomain.com/ws -target http://127.0.0.1:8080
# Local development test:
# ./bin/agent_linux_amd64 -node-id myhome -signal ws://127.0.0.1:8888/ws -target http://127.0.0.1:8080
```

*(Note: `-target` points to the local web application you want to expose, such as an internal NAS dashboard, Alist, Jellyfin, etc.)*

#### Step 2: Open the Portal in Any Remote Browser

In your smartphone or remote computer's browser, open:
👉 **`https://p2p.yourdomain.com/#myhome`** *(or `http://localhost:8888/#myhome` for local testing)*

The browser automatically exchanges signaling with the Agent, punches through NAT via STUN/ICE in seconds, and renders the service. A floating badge in the bottom-right corner displays real-time RTT latency.

---

### 2. Classic Intranet Application Practices

| Application Scenario | Typical Target Address | Agent Startup Command | Browser Experience |
| :--- | :--- | :--- | :--- |
| **Home Media (Jellyfin / Emby / Plex)** | `http://192.168.1.100:8096` | `./agent -node-id media -target http://192.168.1.100:8096` | High-definition media streaming, fast seeking, full Range 206 chunking. |
| **Password Manager (Vaultwarden)** | `http://127.0.0.1:8080` | `./agent -node-id bitwarden -target http://127.0.0.1:8080` | Full WebCrypto API, Passkey support, Cookie isolation. |
| **Private Cloud Storage (Alist / FileBrowser)** | `http://127.0.0.1:5244` | `./agent -node-id pan -target http://127.0.0.1:5244` | Multi-file concurrent uploads/downloads, W3C progress bar virtualization. |
| **Cloud Photos (Immich / PhotoPrism)** | `http://127.0.0.1:2283` | `./agent -node-id photo -target http://127.0.0.1:2283` | Instant thumbnail waterfall rendering, seamless SvelteKit frontend compatibility. |
| **Container Dashboard (Portainer)** | `https://127.0.0.1:9443` | `./agent -node-id docker -target https://127.0.0.1:9443` | Neutralizes Anti-Frame-Busting restrictions, real-time WebSocket terminal interaction. |
| **Local Development & Debugging** | `http://localhost:3000` | `./agent -node-id dev -target http://localhost:3000` | Zero-configuration sharing of local Vite / Next.js / Spring Boot apps for clients or peers. |
| **P2P Single-File Transfer (Private Wormhole / AirDrop)** | Local file (ISO / ZIP / MP4) | `./agent -file ./Ubuntu.iso -access-secret pass123 -sign-link 24h` | Direct line-speed P2P download in visitor's browser, zero relay traffic, auto-expires. |
| **Static Web Hosting & Directory Browsing** | Local folder / static dist | `./agent -dir /path/to/dist` | Zero-configuration static hosting, serves `index.html` or responsive read-only directory tree. |

---

### 3. Multi-Node & Subdomain Intelligent Routing

The portal features an adaptive routing resolution pipeline:

```
URL Query Parameter (?peer=nas)
  ➔ URL Short Hash (#nas)
    ➔ Subdomain Prefix (nas.p2p.domain.com or nas.domain.com)
      ➔ LocalStorage Stored Node
        ➔ Built-in Default Node
```

1. **Subdomain Direct Routing (Recommended)**:
   - With wildcard DNS configured, visitors can visit `https://nas.p2p.yourdomain.com/`. The portal extracts `nas` as the Node ID automatically.
   - Secondary subdomains are also supported: `https://jellyfin.yourdomain.com/` ➔ routes to node `jellyfin`.
2. **URL Short Hash Format**:
   - `https://p2p.yourdomain.com/#vaultwarden` ➔ connects to node `vaultwarden`.
3. **URL Query Parameter Format**:
   - `https://p2p.yourdomain.com/?peer=photo` ➔ connects to node `photo`.

---

### 4. Web Console & Node Settings Modal

A permanent **Status Pill Badge** is rendered in the bottom-right corner:
- **Network Status**: Displays live connection status (Connected / Connecting) and end-to-end RTT latency (e.g. `⚡ RTT: 2.1ms`).
- **Settings Modal**: Click the `⚙` icon to open the configuration dialog:
  - Switch Node ID on the fly;
  - Configure custom WebSocket signaling server URLs;
  - Quick-switch node chips;
  - Hot reconnection with settings saved in `localStorage`.

---

### 5. Agent Command-Line Flags & Environment Variables

The Agent supports both command-line flags and environment variables:

| CLI Flag | Environment Variable (ENV) | Default Value | Description |
| :--- | :--- | :--- | :--- |
| `-node-id` | `NODE_ID` | `homeserver` | Unique node identifier in the signaling network |
| `-signal` | `SIGNAL_URL` | `ws://127.0.0.1:8888/ws` | Signaling server WebSocket URL (`ws://` or `wss://`) |
| `-target` | `TARGET_URL` | `http://127.0.0.1:8080` (default) | Destination Web app URL to reverse proxy (Mode A, mutually exclusive with `-dir`/`-file`) |
| `-dir` | `DIR_PATH` | `""` | Local directory path to serve as static website / file index (Mode B, mutually exclusive) |
| `-file` | `FILE_PATH` | `""` | Local file path for direct zero-relay P2P file sharing (Mode C, mutually exclusive) |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | Optional YAML / JSON configuration file path (auto-detects `./agent.yaml`, `./agent.yml`, `./agent.json`) |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | Signaling cluster admission secret (Control Plane, shared with server, never exposed to visitors) |
| `-access-secret` | `ACCESS_SECRET` | `""` | Visitor access password (Data Plane, leave empty for public access without password prompt) |
| `-proxy` | `PROXY_URL` | `""` | Optional outbound proxy (`http://`, `https://`, `socks5://`) |
| `-allowed-proxy-domains` | `ALLOWED_PROXY_DOMAINS` | `""` | Optional outbound proxy domain wildcard whitelist for `/--p2p-proxy/` (e.g. `*.hdslb.com,*.bilivideo.com`) |
| `-sni` | `SNI` | `""` | Optional TLS SNI & Host header override (for direct Anycast IP dialing) |
| `-insecure` | `INSECURE` | `false` | Skip TLS certificate verification for self-signed signaling servers |
| `-turn-url` | `TURN_URL` | `""` | Optional TURN relay server URL (`turn:relay.domain.com:3478`, bypasses symmetric NAT) |
| `-turn-user` | `TURN_USER` | `""` | Optional TURN relay authentication username |
| `-turn-pass` | `TURN_PASS` | `""` | Optional TURN relay authentication credential / password |
| `-stun-servers` | `STUN_SERVERS` | `""` | Optional custom STUN server list (comma-separated string or YAML list) |
| `-max-streams` | `MAX_STREAMS` | `512` | Maximum concurrent multiplexed streams (returns 503 upon limit to protect memory) |
| `-metrics-addr` | `METRICS_ADDR` | `""` | Optional Prometheus `/metrics` exporter address (e.g. `:9090`, default disabled) |
| `-sign-link` | - | `0` | Generate a temporary stateless share link with expiration (e.g. `1h`, `24h`, `7d`) using `-access-secret` |
| `-portal-url` | - | `https://your-portal-domain.com` | Base URL of Web Portal used for `-sign-link` generation |

> [!NOTE]
> **Complete Deprecation of `-secret` / `SECRET` (v1.5.0+)**:
> The ambiguous `-secret` and `SECRET` flags are fully deprecated and removed with zero compatibility aliases. Passing `-secret` to either Agent or Signal server will fail fast and terminate with instructions to use `-signal-secret` (cluster admission) or `-access-secret` (visitor password).

---

### 6. Integrated Signal Server CLI & Environment Variables

| CLI Flag | Environment Variable (ENV) | Default Value | Description |
| :--- | :--- | :--- | :--- |
| `-addr` | `ADDR` / `PORT` | `:8888` | Listening port and address for HTTP/HTTPS & WebSocket |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | Optional YAML / JSON configuration file path (auto-detects `./server.yaml`, `./server.yml`, `./server.json`) |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | Optional global cluster admission secret token (-secret is fully deprecated and removed) |
| `-tls-cert` | `TLS_CERT` | `""` | Path to TLS certificate PEM file |
| `-tls-key` | `TLS_KEY` | `""` | Path to TLS private key PEM file |
| `-tls-auto` | `TLS_AUTO` | `false` | Automatically generates in-memory ECDSA SAN certificates for LAN IPs |
| `-static` | `STATIC_DIR` | `./web/dist` | Custom static web directory (defaults to embedded DistFS) |
| `-max-ip-conns` | `MAX_IP_CONNS` | `64` | Maximum concurrent WebSocket connections per IP address |
| `-trusted-proxies` | `TRUSTED_PROXIES` | `""` (private CIDRs) | Comma-separated trusted proxy CIDRs/IPs (set `none` to disable proxy headers) |
| `-metrics-addr` | `METRICS_ADDR` | `""` | Optional dedicated Prometheus `/metrics` address (default served on `/metrics`) |
| `-health` | - | `false` | Run local health check probe against signal server (auto-detects HTTP/HTTPS/port), exits 0 on success, 1 on failure (for Docker/K8s HEALTHCHECK) |
| `-health-url` | - | `""` | Custom URL for health check probe (default probes `-addr` / `ADDR` on `/healthz`) |

---

### 7. Security & Access Control

- **End-to-End Native Cryptography**: All data traveling through the WebRTC DataChannel is encrypted via DTLS 1.2/1.3 and AES-GCM. Even if signaling traffic is monitored, payload decryption is cryptographically impossible.
- **Control Plane & Data Plane Dual-Secret Decoupling (v1.5.0+)**:
  1. **Signaling Cluster Secret (`-signal-secret` / `SIGNAL_SECRET`)**:
     - Shared between the signaling server and trusted Agents for admission control and node anti-hijacking.
     - **Never exposed to visitors or client browsers**.
  2. **Visitor Access Password (`-access-secret` / `ACCESS_SECRET`)**:
     - **Public Access (Default)**: When omitted/empty, the node is fully public. Visitors connect directly via P2P without password prompts, even if the signaling server has cluster secrets enabled.
     - **Protected Access**: When configured, the portal prompts a dark-mode password modal (or reads `#nodeId?secret=xxx`), verified by the Agent during WebRTC Offer handshake.
- **Stateless HMAC-SHA256 Time-Limited Sharing**:
  - Zero database, zero Redis, zero server-side revocation tables required;
  - Pure mathematical validation: `sign = HMAC-SHA256(nodeId + ":" + exp, access_secret)`;
  - When a visitor opens `#nodeId?exp=xxx&sign=yyy`, the Agent validates freshness and HMAC locally in constant time;
  - Web console provides 1-click generation for 1h, 6h, 24h, 7d, 30d durations; CLI supports `./agent -access-secret xxx -sign-link 24h`.
- **Pure In-Memory Zero-Port P2P File & Directory Server (v1.6.0+)**:
  - **Pure Outbound Architecture**: Directly adapts WebRTC DataChannel streams to standard HTTP responses in memory without binding any listening TCP sockets on the host.
  - **Single File Direct Share (Private AirDrop / Wormhole)**: `./agent -file /path/to/archive.zip -access-secret pass -sign-link 24h` gives recipients direct high-speed P2P browser downloads with zero intermediate cloud relays or size limits.
  - **Static Web & Directory Hosting**: `./agent -dir /path/to/dist` serves static SPA/docs (`index.html`) or generates clean file listings.
  - **Native HTTP 206 Partial Content**: Full `Range` request support for seeking video/audio and multi-stream download acceleration. Previewable media uses `inline` while archives use `attachment`.
  - **Strict Sandbox & Path Traversal Defense**: Lexical containment via `filepath.Rel` and symlink escape verification via `filepath.EvalSymlinks`.
- **Connection Resilience & Mobile Roaming Self-Healing (WebRTC ICE Restart)**:
  - When mobile devices switch between Wi-Fi and 5G cellular or roaming APs, a two-stage pipeline triggers automatically;
  - Re-establishes broken signaling sockets and renegotiates in-place on the existing `RTCPeerConnection` with `iceRestart: true`;
  - Agent introduces a 5-second grace window, preserving the DataChannel and Mux state without reloading the inner app.
- **Session Isolation**: Each visitor browser session is allocated isolated channel IDs and CookieJars.
- **Design Philosophy & Security Boundaries Declaration**:
  1. **Stateless Transport Gatekeeper**: `-access-secret` and HMAC-SHA256 signatures are transport-level NAT traversal gatekeepers. They are strictly mathematical, zero-state, with zero external database or Redis dependencies. Granular user directories, RBAC permissions, and single-session revocation belong inside the upstream application (e.g. Nextcloud, Jellyfin, Vaultwarden);
  2. **Native SSRF Immunity by Design**: The Agent daemon statically binds only to its configured target (`-target`, `-dir`, or `-file`). It never proxies arbitrary outbound requests or performs internal port scanning, making it natively immune to SSRF attacks without bloated IP/CIDR filtering rules.

---

## 🛠️ Production Deployment Guide (Self-Hosting & Operations)

### Option A: Cloudflare Workers Unified Edge Deployment (Zero Server Cost)

**Recommended production architecture**. Leverages Cloudflare Workers' 300+ global Anycast edge nodes, paired with Durable Objects for signaling and static asset delivery. **Zero hosting cost, high availability, sub-millisecond edge routing**.

```bash
# 1. Build frontend assets
cd web
npm install
npm run build

# 2. Deploy to Cloudflare Workers
npx wrangler deploy
```

Once deployed, the Worker handles both static UI and WebSocket signaling:
- **Web Portal**: `https://p2p.yourdomain.com/`
- **Signaling Endpoint**: `wss://p2p.yourdomain.com/ws`

---

### Option B: Self-Hosted Linux / Windows Standalone Server (Single Binary)

The Go binary embeds the compiled Web portal assets via `go:embed`. **Zero external file dependencies, no Node.js runtime, no Nginx reverse proxy required**:

#### 1. Launch Integrated Server
```bash
# Production with valid SSL certificate:
./bin/signal_linux_amd64 -addr :443 -tls-cert /path/to/fullchain.pem -tls-key /path/to/privkey.pem

# LAN / Development with Auto-TLS (automatically generates SAN certificate for all physical IPs):
./bin/signal_linux_amd64 -addr :8443 -tls-auto
```

#### 2. Launch Intranet Agent
```bash
./bin/agent_linux_amd64 -node-id mynode -signal wss://yourdomain.com/ws -target http://127.0.0.1:8080

# When connecting to -tls-auto self-signed server:
./bin/agent_linux_amd64 -node-id mynode -signal wss://<LAN_IP>:8443/ws -insecure -target http://127.0.0.1:8080
```

---

### Option C: Docker & Docker Compose Containerized Deployment

Official pre-built multi-architecture (`linux/amd64`, `linux/arm64`) images are published to GitHub Container Registry (GHCR):

> [!TIP]
> Pull pre-built multi-arch images directly without local compilation:
> ```bash
> # Pull server image (Portal + Signaling broker)
> docker pull ghcr.io/beingnoability/webrtc-p2p-server:latest
> # Or specific version:
> docker pull ghcr.io/beingnoability/webrtc-p2p-server:v1.3.0
>
> # Pull agent image (Intranet reverse proxy client)
> docker pull ghcr.io/beingnoability/webrtc-p2p-agent:latest
> # Or specific version:
> docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v1.3.0
> ```

#### 1. Quick Launch with Docker Compose

A production-ready [docker-compose.yml](docker-compose.yml) is included:

```bash
# Start all services
docker compose up -d

# View live logs
docker compose logs -f

# Stop services
docker compose down
```

#### 2. Standalone Container Execution Examples

**Run Server:**
```bash
docker run -d --name webrtc-server --restart unless-stopped \
  -p 8888:8888 \
  ghcr.io/beingnoability/webrtc-p2p-server:latest
```

**Run Agent (Host network mode recommended for Linux):**
```bash
docker run -d --name webrtc-agent --restart unless-stopped \
  --net=host \
  -e NODE_ID=my-nas \
  -e SIGNAL_URL=wss://p2p.yourdomain.com/ws \
  -e TARGET_URL=http://127.0.0.1:8096 \
  ghcr.io/beingnoability/webrtc-p2p-agent:latest
```

#### 3. Generate Stateless HMAC-SHA256 Share Links with Docker

Generate mathematical temporary share links on the fly without installing Go:

```bash
# Via Docker Compose (e.g. valid for 24 hours):
docker compose run --rm agent -sign-link 24h -portal-url https://p2p.yourdomain.com

# Via standalone docker run:
docker run --rm -e ACCESS_SECRET=your_visitor_password \
  ghcr.io/beingnoability/webrtc-p2p-agent:latest \
  -node-id homeserver -sign-link 24h -portal-url https://p2p.yourdomain.com
```

---

## 💻 Developer Guide

### 1. System Architecture & End-to-End Data Flow

```
[Visitor Browser]                  [Public Signaling Server]             [Intranet Agent]        [Local Target App]
      |                                        |                                |                        |
      | 1. Connect WebSocket                   |                                |                        |
      |--------------------------------------->|                                |                        |
      |                                        |<-- 2. Register (Node ID) ------|                        |
      |                                        |                                |                        |
      | 3. Send SDP Offer (Target Node ID)     |                                |                        |
      |--------------------------------------->|--- 4. Relay Offer ------------>|                        |
      |                                        |<-- 5. Reply SDP Answer --------|                        |
      |<-- 6. Relay Answer --------------------|                                |                        |
      |                                        |                                |                        |
      |<================ 7. WebRTC ICE NAT Traversal (STUN / Host) =============>|                        |
      |                                                                         |                        |
      |======= 8. Establish End-to-End Encrypted DataChannel ("p2pmux") =======>|                        |
      |                                                                         |                        |
      | [Service Worker intercepts fetch()]                                     |                        |
      | 9. Encode P2PMux Stream Frame (HTTP Headers & Body)                     |                        |
      |------------------------------------------------------------------------>|                        |
      |                                                                         | 10. Reverse proxy req  |
      |                                                                         |----------------------->|
      |                                                                         |<-----------------------|
      |                                                                         | 11. Read response      |
      | 12. P2PMux Stream Frame (Status, Headers, Chunked Body Stream)          |                        |
      |<------------------------------------------------------------------------|                        |
      | 13. Stream into ReadableStream to render webpage                        |                        |
```

---

### 2. P2PMux v1 Binary Streaming Multiplexing Protocol Specification

To multiplex hundreds of concurrent HTTP requests, WebSocket tunnels, and large file streams over a single WebRTC `RTCDataChannel` while avoiding Head-of-Line Blocking, the gateway utilizes the **P2PMux v1** protocol.

#### Frame Structure (10-byte fixed header):

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          Stream ID (32 bits)                  |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|   Type (8)    |   Flags (8)   |       Payload Length (32)     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|       Payload Length (cont.)  |   Payload Data (0 ~ 32KB) ... |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+                               |
```

- **Stream ID (uint32, 4 bytes)**: Unique stream identifier. Browser initiates odd IDs (1, 3, 5...), Agent initiates even IDs (2, 4, 6...).
- **Type (uint8, 1 byte)**:
  - `0x01 (TypeData)`: Stream payload data chunks (streaming transfer).
  - `0x02 (TypeHeaders)`: Stream metadata (HTTP method, URL, headers serialized in JSON).
  - `0x03 (TypeRstStream)`: Stream reset or abnormal termination with error code.
  - `0x04 (TypeWindowUpdate)`: Flow control window update (reserved for high-concurrency backpressure).
  - `0x05 (TypePing)` / `0x06 (TypePong)`: Precise RTT latency measurement and keep-alive heartbeats.
  - `0x10 (TypeWSConnect)` / `0x11 (TypeWSData)` / `0x12 (TypeWSClose)`: Native WebSocket tunneling handshake, bidirectional data, and teardown frames.
- **Flags (uint8, 1 byte)**:
  - `0x00 (FlagNone)`: Standard data frame.
  - `0x01 (FlagFin)`: Indicates end of data stream (half-close / full-close signal).
  - `0x02 (FlagAck)`: Control frame acknowledgement.
- **Payload Length (uint32, 4 bytes)**: Big-endian payload byte length. Maximum payload is capped at **64KB (MaxPayloadSize = 65536 bytes)**, with a recommended chunk size of **32KB (RecommendedChunkSize = 32768 bytes)** for optimal SCTP transmission safety and low buffer bloat.

---

### 3. In-Browser DOM Virtualization & Service Worker Core Mechanisms

1. **Service Worker Dedicated MessagePort**: Bypasses origin boundary limits via bidirectional `MessagePort` channels (`PORTAL_ATTACH`).
2. **W3C Fetch Strict Null-Body Alignment**: Enforces `new Response(null, { status })` for HTTP 204, 205, and 304 responses, preventing Chromium/WebKit type errors.
3. **W3C XHR Upload Progress Virtualization**: Intercepts `XMLHttpRequest.prototype.send` to dispatch simulated `ProgressEvent` sequences based on physical transfer speed.
4. **WebSocket Defragmentation**: Employs circular buffers and 5-byte headers (Opcode + Length) to reassemble fragmented frames up to hundreds of kilobytes across SCTP boundaries.
5. **Session-Isolated CookieJar & DOM Synchronization**: Server isolates independent CookieJars; non-HttpOnly cookies are mirrored into `document.cookie`.
6. **Anti-Frame-Busting Defense**: Strips `allow-top-navigation` on iframes and proxies `window.top` / `window.parent` references.
7. **New Tab / Window Node ID Deep Persistence**: Service Worker injects `window.__P2P_NODE_ID__` into HTML streams, and `patch.js` rewrites dynamic links to `?peer=${nodeId}#${nodeId}`, preventing new window clicks from dropping the node context.

---

### 4. Project Repository Structure Overview

```
webrtc-gateway/
├── bin/                            # Pre-compiled multi-platform binaries
│   ├── agent.exe                   # Windows x64 Agent daemon
│   ├── agent_linux_amd64           # Linux x64 Agent daemon
│   ├── signal.exe                  # Windows x64 Integrated server
│   ├── signal_linux_amd64          # Linux x64 Integrated server
│   └── mock_service.exe            # Local mock backend for testing
├── cmd/                            # Go application entrypoints
│   ├── agent/main.go               # Agent CLI
│   ├── signal/main.go              # Integrated server CLI (embeds web/dist)
│   └── mock_service/main.go        # Test mock backend with chunking/WS
├── pkg/                            # Core protocol libraries
│   ├── agent/                      # WebRTC peer connection manager
│   ├── mux/                        # P2PMux stream scheduler and backpressure
│   ├── protocol/                   # Binary frame encode/decode
│   ├── proxy/                      # HTTP/WebSocket reverse proxy & CookieJar
│   └── signaling/                  # Signaling message models
├── web/                            # Frontend single-page app (native TypeScript)
│   ├── src/
│   │   ├── portal/                 # Portal UI, settings modal, WebRTC manager
│   │   ├── sw/                     # Service Worker stream pipeline
│   │   ├── patch/                  # DOM/XHR/WebSocket monkey patches
│   │   ├── protocol/               # TypeScript P2PMux implementation
│   │   └── index.html              # HTML5 template
│   ├── dist/                       # Compiled frontend assets
│   ├── esbuild.config.mjs          # Fast build script
│   ├── package.json
│   ├── tsconfig.json
│   └── wrangler.toml               # Integrated Cloudflare Worker deployment
├── edge/                           # Edge signaling configuration
│   ├── cloudflare-worker/          # Unified signaling worker (Durable Object + static assets)
│   │   └── worker.js
│   └── wrangler.toml               # Standalone edge deployment config
├── configs/                        # Configuration templates
│   ├── agent.example.yaml          # Agent configuration reference
│   └── server.example.yaml         # Server configuration reference
├── docs/                           # VitePress documentation site & specifications
│   ├── .vitepress/                 # VitePress config (nav, sidebar, search, Mermaid)
│   ├── guide/                      # Guides (architecture, deployment, configs, security, FAQ)
│   ├── BROWSER_COMPATIBILITY.md    # Browser compatibility & limitation matrix
│   ├── RELEASE_CHECKLIST.md        # Release quality gate audit protocol
│   ├── RELEASE_NOTES.md            # Latest single-version release notes (GitHub Release)
│   └── package.json                # Isolated docs dependencies & build scripts
├── test/                           # Automated test suites
│   ├── advanced_e2e_scenarios_test.go # HTTP 206 Partial Content / Range / Gzip / Headers
│   ├── secret_auth_test.go         # SECRET authentication & 401 instant rejection
│   ├── tls_server_test.go          # Auto-TLS SAN certificate generation
│   ├── large_file_e2e_test.go      # 100MB streaming & backpressure
│   ├── mvp_e2e_test.go             # WebRTC signaling & concurrent streams
│   ├── phase3_tunnel_test.go       # HTTP/WS full regression
│   ├── test_subdomain_logic.cjs    # Subdomain resolution unit tests
│   └── node_id_preservation_test.cjs # New window Node ID persistence & DOM tests
├── .github/workflows/              # GitHub Actions CI/CD workflows
│   ├── ci.yml                      # CI build & automated test matrix
│   ├── release.yml                 # Multi-arch GitHub Release packager
│   ├── docker.yml                  # Multi-arch Docker image GHCR publisher
│   └── deploy-pages.yml            # Automated GitHub Pages docs deployment
├── docker-compose.yml              # Production orchestration template
├── Dockerfile.agent                # Minimal multi-stage agent image (~20MB)
├── Dockerfile.server               # Minimal multi-stage server image (~16MB)
├── go.mod
├── go.sum
├── CHANGELOG.md                    # Cumulative changelog (Keep a Changelog)
├── LICENSE                         # License (MIT)
├── README.md                       # Documentation (Simplified Chinese)
└── README_en.md                    # Documentation (English)
```

---

### 5. Local Source Build & Automated Test Suite

#### ① Build Frontend Assets
```bash
cd web
npm install
npm run build
cd ..
```

#### ② Compile Go Binaries
```bash
# Build native binaries
go build -ldflags="-s -w" -o bin/agent.exe ./cmd/agent
go build -ldflags="-s -w" -o bin/signal.exe ./cmd/signal

# Cross-compile for Linux x64
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/agent_linux_amd64 ./cmd/agent
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/signal_linux_amd64 ./cmd/signal
```

#### ③ Run Full Test Suite
```bash
# 1. Run all Go protocol, mux, and integration tests
go test -v ./...

# 2. Run frontend routing and Node ID preservation test suites
cd web && npm test
# Or directly from root:
node test/test_subdomain_logic.cjs
node test/node_id_preservation_test.cjs

# 3. Run full real browser test matrix (Headless Chromium / Edge CDP)
node test/real_browser_matrix_runner.cjs

# 4. Run HTTP & WebSocket feature verification suite
node test/verify_features.cjs
```

**Key Verification Highlights:**
- `RealBrowserMatrixTests`: Automated execution of 22/22 test matrix using real Chromium CDP (12 complex WebApp scenarios, page reload & session resilience, and Top 10 real-world websites);
- `HTTP & WebSocket Feature Verification`: Comprehensive verification of GET/POST/PUT/PATCH/DELETE, Query String, Request Body, Cookie isolation, Authorization headers, 301/302/307 Redirects, HTTP 206 Range streaming, 5MB chunked download, 204/304 Null-Body compliance, and WebSocket bursts & 128KB jumbo frame defragmentation.

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).
Contributions, issues, and feature requests are welcome! If this project helped you, please give it a ⭐️ **Star**!
