<p align="center">
  <img src="assets/logo-banner.svg" alt="Zero-Client WebRTC P2P Gateway Banner" width="820">
</p>

# Zero-Client P2P WebRTC Intranet Penetration Gateway

### Browser Zero-Client End-to-End Encrypted P2P WebRTC Gateway

<p align="center">
  <a href="README.md"><b>简体中文</b></a> · <a href="README_en.md"><b>English</b></a> · <a href="https://beingnoability.github.io/WebRTC-Gateway/en/"><b>Documentation Site</b></a>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev/)
[![TypeScript](https://img.shields.io/badge/TypeScript-7.x-3178C6?logo=typescript)](https://www.typescriptlang.org/)
[![WebRTC](https://img.shields.io/badge/WebRTC-Pion%20v4-FF6B6B?logo=webrtc)](https://github.com/pion/webrtc)
[![Docker](https://img.shields.io/badge/Docker-Multi--Arch-2496ED?logo=docker)](https://github.com/BeingNoAbility/WebRTC-Gateway/pkgs/container/webrtc-p2p-server)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![CI](https://img.shields.io/badge/CI-GitHub%20Actions-2088FF?logo=githubactions)](https://github.com/BeingNoAbility/WebRTC-Gateway/actions/workflows/ci.yml)

> [!IMPORTANT]
> **Release Status**: Current pre-release is **v5.0.0-alpha.3** ([V7 Architecture RFC](docs/en/rfcs/v7.0-architecture.md)). Full version history is recorded in [CHANGELOG](CHANGELOG.md).

---

## Why WebRTC Gateway?

| Approach | Representative Tools | Key Constraints | WebRTC Gateway Approach |
| :--- | :--- | :--- | :--- |
| **Public Port Relay** | FRP / NPS / Ngrok / Cloudflare Tunnel | All traffic transits a central server with relay bandwidth cost and privacy exposure | **Direct P2P Tunnel**: End-to-end DTLS over WebRTC STUN/ICE; signaling only brokers SDP and ICE candidates and never relays payload data; user-run TURN is an optional fallback forwarding DTLS ciphertext only |
| **Mesh VPN** | Tailscale / WireGuard / ZeroTier | Visitors must install native apps or kernel TUN/TAP drivers | **Zero-Client Browser Access**: Visitors open a URL in stock Chrome, Edge, Safari, or Firefox |

---

## Core Capabilities

- **Unified Multiplexed Data Plane (P2PMux V8)**: Uses one `RTCPeerConnection` with 1 chunked control channel (`id=0` `p2pctrl`) and 12 pre-negotiated ordered multiplexed lanes (`id=1..12` `p2p_01..p2p_12`) carrying both HTTP and streaming WebSocket streams by monotonic `StreamID` without dynamic DCEP channels, over an 8-byte fixed header, pure payload-byte `CREDIT` flow control, and compact binary TLV encoding (`FetchCtx`, `RespFlags`, and inline cookie TLVs).
- **Anti-MITM Cryptographic Boundary**: Signaling exchanges strictly validated structured `SessionDescriptor` payloads and both peers deterministically synthesize minimal 16-line DataChannel-only SDPs. Each Agent persists an **Ed25519 identity keypair** signing length-prefixed transcripts (including `-allow-public`), verified against the live negotiated DTLS certificate fingerprint (on engines supporting `getRemoteCertificates`), `#node?pk=` pinning, and Fail-Closed `IndexedDB` TOFU. Visitor password auth runs **RFC 9383 SPAKE2+ (P-256, 300,000-round PBKDF2 + HKDF)** mutual zero-knowledge verification inside DTLS with mandatory downgrade protection; Owners can mint live-revocable, path- and read-only-scoped **Ed25519 capability tokens** backed by atomic persistence.
- **2-Tier Sandbox Isolation, W3C Fetch Anti-CSRF Gate & Transferable Stream Bridge**: Signaling requires explicit startup origins (`-portal-origin` + `-sandbox-origin`, or `-dev` on loopback), offering **Tier 1 cross-site wildcard isolation** (distinct eTLD+1 deriving `sbx-<24hex>` origins) and **Tier 2 restricted isolation** (fixed single cross-site host or `-allow-same-site-sandbox` same-site wildcard, both displaying a persistent Tier 2 warning banner). Portal CSP locks `style-src 'self'` and `connect-src 'self'` with startup pre-rendered content-hashed assets and ETags; SW bridges streams via native Transferable `ReadableStream`, serves external `/__p2p_gw__/boot.js` while preserving upstream CSP directives, unconditionally blocks `destination=document` escapes, enforces per-tab `clientToTab` quotas, and pairs `CookieMirror` with `ProxyCookieJar` to isolate `HttpOnly` cookies.
- **Pure Pipe Proxy and Fail-Closed `O_NOFOLLOW` Static Adapter**: HTTP and WebSocket share a strict single-target `DestinationPolicy` (exact target origin only, optional `-upstream-host` virtual-host header override, WS 302 redirect blocking, and post-resolve capability enforcement), unified `ProxyCookieJar` (`httpOnly` / `visible` quotas), 16 KiB streaming WebSocket proxying, and native streaming `gzip` (`DecompressionStream('gzip')`, zero third-party decompression dependencies); `-dir` and `-file` use stepwise `os.Root` read-only handlers with default Fail-Closed `O_NOFOLLOW` rejecting symlinks and dotfiles (optional `-follow-symlinks` strictly confined to root and capability prefix) and `CSP: sandbox` on all static files.
- **Single Authoritative Go Signal & Build-Time Asset Embedding**: Content-hashed frontend bundles are embedded at build time via an explicit manifest under `-tags embedweb` (`make build`) into the single `signal` binary, supporting atomic Ed25519 node bindings (`-bound-keys-file`) and per-node registration secrets (`-node-secrets-file`, minimum 16 bytes).

---

## Quickstart

### Option 1: Local Source Run (`localhost` / `*.localhost` Secure Context)

```bash
# 1. Build web frontend assets and Go binaries with embedded web assets
make build

# 2. Start Signal server (-dev enables loopback *.localhost cross-site sandbox; secret >= 16 bytes)
./bin/signal -addr 127.0.0.1:8888 -dev -signal-secret dev-cluster-secret-16b

# 3. Start intranet Agent (ACCESS_SECRET required unless -allow-public is passed)
ACCESS_SECRET=change-me ./bin/agent \
  -id homeserver \
  -signal ws://127.0.0.1:8888/ws \
  -signal-secret dev-cluster-secret-16b \
  -target http://127.0.0.1:8080
```

Open `http://localhost:8888/#homeserver` in your browser and enter the access secret inside the DTLS auth prompt to reach local port `8080`.

### Option 2: Production Dual-Domain Deployment with Docker Compose

Production deployments fail closed unless explicit cross-site origins (recommended Tier 1 wildcard `PORTAL_ORIGIN` + `SANDBOX_ORIGIN=https://*.sandbox.example.net`, or Tier 2 restricted fixed host `https://app.example.net`) and a `SIGNAL_SECRET` of at least 16 bytes (or explicit `-allow-anonymous-agents`) are configured:

```bash
cat <<'EOF' > .env
PORTAL_ORIGIN=https://portal.example.com
SANDBOX_ORIGIN=https://*.sandbox.example.net
SIGNAL_URL=wss://portal.example.com/ws
SIGNAL_SECRET=replace-with-strong-cluster-secret
ACCESS_SECRET=replace-with-strong-visitor-secret
EOF

docker compose up -d --build
```

Mint an offline scoped read-only capability link:

```bash
./bin/agent -id homeserver -identity-key /path/to/homeserver.ed25519.key \
  -mint-link 24h -mint-path /public -mint-readonly \
  -portal-url https://portal.example.com
```

---

## End-to-End Sequence

```mermaid
sequenceDiagram
    autonumber
    actor Visitor as Visitor Browser (Portal)
    participant Signal as Signaling Server (Go Signal)
    participant Agent as Intranet Agent Daemon
    participant Target as Local Target Service

    Agent->>Signal: Upgrade Bearer SIGNAL_SECRET + Ed25519 public key registration
    Visitor->>Signal: Connect /ws/client/{node} and send structured Offer + 32B Nonce
    Signal->>Agent: Forward Offer (inject trusted client IP)
    Agent->>Signal: Return structured Answer + Pure Ed25519 transcript signature
    Signal->>Visitor: Forward Answer
    Note over Visitor,Agent: Browser verifies Ed25519 signature, pk=/TOFU, and live DTLS cert

    Visitor<<->>Agent: Single PC + Chunked p2pctrl + 12 Ordered Mux Lanes (HTTP & WS)
    Visitor<<->>Agent: In-DTLS RFC 9383 SPAKE2+ mutual auth or capability verification
    Note over Visitor,Agent: All HTTP/WS payload traffic stays strictly inside end-to-end DTLS

    Visitor->>Agent: Cross-site SW -> Transferable Stream Bridge -> P2PMux V8 TLV request
    Agent->>Target: Pure Pipe single-target HTTP/WS proxy (ProxyCookieJar + boundary checks)
    Target-->>Agent: Response headers and streaming body
    Agent-->>Visitor: Multiplexed lane delivery (native streaming Gzip + in-memory CookieMirror sync)
```

---

## Documentation Map

- [Agent Use Cases & Recipes](docs/en/guide/agent-use-cases.md)
- [Local Reverse-Proxy & Adapter Recipes](docs/en/guide/reverse-proxy-recipes.md)
- [Server Use Cases & Deployment Recipes](docs/en/guide/server-use-cases.md)
- [Architecture and Core Design](docs/en/guide/architecture.md)
- [P2PMux V8 Wire Protocol Specification](docs/en/guide/protocol-specification.md)
- [Security and Cryptographic Boundaries](docs/en/guide/security.md)
- [Agent and Signal Server Configuration](docs/en/guide/agent-configuration.md)
- [Production Deployment and 2-Tier Sandbox](docs/en/guide/deployment.md)
- [Read-Only Static File and Directory Adapter](docs/en/guide/file-server.md)
- [V7 Architecture Decision Record (RFC)](docs/en/rfcs/v7.0-architecture.md)
- [Contributor Guide (`CONTRIBUTING.md`)](CONTRIBUTING.md)
- [Testing Strategy & Machine Gate (`scripts/gate.sh`)](docs/en/TESTING_CHECKLIST.md)

---

## License

Released under the [MIT License](LICENSE).
