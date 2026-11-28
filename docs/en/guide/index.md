# Guide Overview

Welcome to the **WebRTC Gateway** documentation. WebRTC Gateway enables stock browsers to reach intranet HTTP / WebSocket applications and read-only static files over end-to-end WebRTC DataChannels and a cross-site Service Worker sandbox without installing client software.

---

## Core Capabilities

- **Zero-Client Access**: Visitors open a URL in standard Chrome, Edge, Safari, or Firefox — no native apps or browser extensions required.
- **Zero Data Relay**: Signaling servers only broker SDP and ICE candidates; self-hosted TURN (forwarding DTLS ciphertext only) is an optional fallback for symmetric NATs.
- **Anti-MITM Identity and In-DTLS Auth**: Persistent Agent Ed25519 keys sign canonical SDP transcripts verified via `pk=` pinning or `IndexedDB` TOFU; passwords and revocable capability tokens are verified strictly inside DTLS.
- **Cross-Site Sandbox Transparent Proxy**: Transparently proxies HTTP/HTTPS, 206 Range media streaming, bidirectional WebSockets, and Agent-side `ProxyCookieJar` sessions.

---

## Guide Navigation

| Guide | Summary | Audience |
| :--- | :--- | :--- |
| **[Quickstart (5 Minutes)](./quickstart)** | Run locally, share a file or directory, and pick a production tier by your domains and certificates. | First-time users |
| **[Agent Use Cases](./agent-use-cases)** | Ready-to-use recipes for NAS/web apps, self-signed HTTPS consoles, subpath & local reverse-proxy virtual hosts (`-upstream-host`), static hosting, share links, TURN/proxy, and ops. | Intranet Users / Operators |
| **[Local Reverse-Proxy Recipes](./reverse-proxy-recipes)** | Virtual-host targeting, multi-origin fan-in, Nginx `sub_filter` / Caddy prefix stripping, forwarding/auth header injection, and trust-boundary guidelines. | Advanced Self-Hosters / Operators |
| **[Server Use Cases](./server-use-cases)** | Deployment recipes for local `-dev`, direct hot-reload TLS, Nginx/Caddy proxies, Tier 1 cross-site wildcard, Tier 2 restricted isolation, and hardened Docker. | DevOps / Self-Hosters |
| **[Architecture & Design](./architecture)** | Single-PC 12-lane unified mux data plane, 2-tier sandbox, hidden bridge, Transferable streams, streaming HTML5 tokenizer, and unified silent reconnect. | Architects / Contributors |
| **[P2PMux V8 Wire Protocol](./protocol-specification)** | 8-byte fixed header, binary TLVs (`FetchCtx`/`RespFlags`/inline cookies), chunked Ctrl, 12 ordered mux lanes for HTTP & WS, budgets, and golden vectors. | Protocol Developers |
| **[Security & Cryptography](./security)** | Honest frontend TCB boundary, structured SDP synthesis, Ed25519 node identity, TOFU red-screen block, in-DTLS SPAKE2+, W3C Fetch anti-CSRF gate, and split-pool cookies. | Security Auditors / Operators |
| **[Agent & Server Configuration](./agent-configuration)** | Complete reference for CLI flags, environment variables, `*_FILE` secret files, and strict YAML configs. | All Users |
| **[Production Deployment](./deployment)** | 2-tier sandbox DNS/TLS planning, explicit startup origins, reproducible binary builds, and hardened Docker Compose. | DevOps / Self-Hosters |
| **[Read-Only Static Adapter](./file-server)** | `-dir` and `-file` via Go `os.Root` with default `O_NOFOLLOW`, opt-in `-follow-symlinks`, and default `CSP: sandbox`. | Static Sharing Users |
| **[Browser Compatibility](./browser-compatibility)** | CI-tested Chrome / Edge matrix plus Safari / Firefox constraints and suspension recovery notes. | Frontend / Mobile Users |
| **[Troubleshooting & FAQ](./troubleshooting)** | Symmetric NAT and TURN, secure context and explicit origin configs, roaming recovery, TOFU key rotation, and common errors. | Operators / Troubleshooting |

---

## Quickstart

For a step-by-step tutorial and tier selection, see [Quickstart (5 Minutes)](./quickstart).

::: code-group

```bash [1. Build & Local Signal]
# Build frontend assets and compile Go binaries with embedded web assets
make build

# Start local Signal server (-dev enables loopback *.localhost sandbox; secret >= 16 bytes)
./bin/signal -addr 127.0.0.1:8888 -dev -signal-secret dev-cluster-secret-16b
```

```bash [2. Start Intranet Agent]
# Reverse-proxy a local HTTP app (ACCESS_SECRET required unless -allow-public is passed)
ACCESS_SECRET=change-me ./bin/agent \
  -id myhome \
  -signal ws://127.0.0.1:8888/ws \
  -signal-secret dev-cluster-secret-16b \
  -target http://127.0.0.1:8080

# Or mint an offline 24-hour read-only capability share link
./bin/agent -id myhome -mint-link 24h -mint-readonly -portal-url http://localhost:8888
```

```text [3. Open in Browser]
Visit:
http://localhost:8888/#myhome

Enter the access secret in the in-DTLS auth modal to reach your local backend inside the cross-site sandbox.
```

:::
