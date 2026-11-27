# Architecture and Core Design

WebRTC Gateway consists of three decoupled components:
1. **Browser Client (Modular Web Runtime)**: Independent Portal (content-hashed `/__p2p_gw__/portal.<hash>.js` and `portal.<hash>.css`), hidden non-navigable bridge (`/__p2p_gw__/bridge.html`), modular Service Worker (`/__p2p_gw__/sw.js`), pure-compatibility DOM and in-memory `CookieMirror` patch (`/__p2p_gw__/patch.<hash>.js`), and TypeScript P2PMux V7;
2. **Signaling Server (Single Authoritative Go Signal)**: Standalone Go binary (`cmd/signal`, embedding the content-hashed asset manifest under `-tags embedweb`) that brokers structured `SessionDescriptor` Offer/Answer payloads and ICE candidates without ever relaying or decoding application traffic;
3. **Agent Daemon (Intranet Service)**: Persistent Ed25519 identity, RFC 9383 SPAKE2+ in-DTLS authentication, single-PC 32-DC pool P2PMux V7 multiplexing, and Pure Pipe HTTP / 16 KiB streaming WebSocket / `os.Root` proxying.

---

## 1. End-to-End Interaction Sequence

```mermaid
sequenceDiagram
    autonumber
    actor Visitor as Visitor Browser (Portal Origin)
    participant Signal as Signaling Server (Go Signal)
    participant Agent as Intranet Agent (Ed25519 Identity)
    participant Target as Local Service / os.Root Directory

    Agent->>Signal: Upgrade Bearer SIGNAL_SECRET + Ed25519 register-v2
    Visitor->>Signal: Connect /ws/client/{node_id}, receive agent_pk & check pk=/TOFU
    Visitor->>Signal: Send structured SessionDescriptor Offer + 32B Nonce
    Signal->>Agent: Forward Offer (inject authoritative client IP)
    Agent->>Signal: Reply structured Answer + Pure Ed25519 length-prefixed signature
    Signal->>Visitor: Forward Answer
    Note over Visitor,Agent: Both peers synthesize deterministic 16-line SDP & verify live DTLS cert

    Visitor<<->>Agent: Single PeerConnection + p2pctrl + 32 HTTP Pool DCs + Dynamic p2pws DCs
    Visitor<<->>Agent: In-DTLS p2pctrl RFC 9383 SPAKE2+ mutual auth or capability auth
    Note over Visitor,Agent: Application traffic flows solely inside end-to-end DTLS

    Visitor->>Agent: Cross-site SW -> TunnelBridge -> P2PMux V7 TLV request
    Agent->>Target: Reverse proxy HTTP/WS via DestinationPolicy + ProxyCookieJar
    Target-->>Agent: Response headers and streaming body
    Agent-->>Visitor: Pool DC delivery (native streaming Gzip + in-memory CookieMirror sync)
```

---

## 2. Single-PC DC Pool Data Plane and Native Streaming Gzip

- **Channel Topology**: Each visitor session runs 1 `RTCPeerConnection` with 1 pre-negotiated control channel `p2pctrl` (`id=0`), 32 pre-negotiated reusable HTTP pool channels `p2p_01..p2p_32` (`id=1..32`, strict 5-state single-slot FSM with monotonic `uint32 Gen`), and dynamic dedicated ordered DCEP channels `"p2pws"` per WebSocket. See [P2PMux V7 Wire Protocol Specification](./protocol-specification) for wire rules.
- **Zero Head-of-Line Blocking**: Every active HTTP stream owns an ordered SCTP channel exclusively so SCTP guarantees `HEADERS` before `DATA` natively; stale frames from prior generations drop in O(1) via `f.Gen != active.Gen`, avoiding cross-stream HoL blocking, unbounded stream state tables, and 2 MiB WebSocket deadlocks.
- **Native Streaming Gzip Compression**: Service Worker signals native gzip support via V7 `FetchCtx`; Agent streams Go stdlib `compress/gzip` with immediate per-write `Flush` (bypassing SSE, `206 Partial Content`, `Cache-Control: no-transform`, and empty bodies while preserving strong `ETag`s) and sets `RespFlag.DECOMPRESS_GZIP`, while SW uses native `DecompressionStream('gzip')` with 0-byte EOF peeking and expansion-ratio/output ceilings (zero third-party JS decompression dependencies).
- **Unified Silent Reconnect**: On ICE/PC/control channel disconnect, Portal creates a fresh `RTCPeerConnection` and silently re-authenticates via SPAKE2+ using the tab's in-memory `(w0, w1)` verifier while keeping `appFrame` mounted without re-prompting for a password.

---

## 3. 3-Tier Cross-Site Sandbox and Modular Browser Runtime

### 1. 3-Tier Isolation, Hidden Bridge (`/__p2p_gw__/bridge.html`), and One-Time `enterNonce`

- **3-Tier Sandbox Isolation**: Production supports Tier 1 dual fixed domains (`-sandbox-host`), Tier 2 single-root same-site wildcard (`-allow-same-site-sandbox` + `Origin-Agent-Cluster: ?1`), and Tier 3 dual-root wildcard (`-sandbox-domain`, device-salted HMAC `sbx-<24hex>`; local dev auto-uses `*.localhost`), while capability visitors receive ephemeral 128-bit `sbx-<32hex>` origins per tab;
- **Zero-SW Portal, W3C Fetch Anti-CSRF Gate, and `appFrame` Sandbox**: Portal returns `404` on `/__p2p_gw__/sw.js` and `/__p2p_gw__/bridge.html` with CSP locked to `style-src 'self'`, `connect-src 'self'`, `form-action 'self'`, and `frame-ancestors 'none'`; hidden `/__p2p_gw__/bridge.html?tab=<tabId>` validates `<meta name="p2p-portal-origins">` and registers `{ scope: '/' }` SW; before mounting `appFrame` (which enforces `sandbox` blocking top navigation), Portal registers a one-time 128-bit `enterNonce` with SW, and SW enforces the W3C Fetch anti-CSRF gate plus `frame-ancestors 'self' <portalOrigin>`;
- **Untrusted Tenant Boundary**: Portal `TunnelBridge` and Go Agent treat the sandbox origin as an untrusted tenant, stripping spoofed forwarding headers, clamping HTTP download `ackConsumed` strictly to delivered bytes, enforcing a 16 MiB WebSocket message cap, and isolating `HttpOnly` cookies inside the Agent's split-pool RFC 6265bis `ProxyCookieJar`.

### 2. Streaming HTML5 Head Injector and In-Memory `CookieMirror`

- **Byte-Level HTML5 Tokenizer and 1024B `<meta charset>` Prescan** (`web/src/sw/html_injector.ts`): Handles HTML5 comments, unquoted attributes, `<script>` double-escaped states, nested/self-closing `<template>`/`<svg>`/`<math>`, UTF-16 BOM passthrough, and split UTF-8 BOMs; when HTTP omits `charset=`, peeks up to 1024 bytes inside `<head>` to keep `<meta charset>` ahead of the single injected `<script src="${origin}${PATCH_ASSET_URL}" data-p2p-boot="..."></script>`;
- **In-Memory `CookieMirror` and PWA Shield**: `ws_patch.ts` backs `Document.prototype.cookie` purely in memory (never writing real `document.cookie`), syncing bidirectionally via V7 inline request/response TLVs with the Agent's split-pool `ProxyCookieJar`, and holds a private `navigator.serviceWorker` reference while hiding `/__p2p_gw__/sw.js` from app PWA `register`/`unregister`/`getRegistrations` calls;
- **Safe Port Liveness Probing and SW Idle Recovery**: SW retries ONLY strictly idempotent bodyless requests (`GET`/`HEAD`/`OPTIONS`) after a 1500 ms `REQ_ACK` timeout, cancelling the old request and probing `BRIDGE_PING` first; after SW idle termination, cold-start recovery wakes `bridge.html` solely for an unambiguous single-tab nested `appFrame` or worker verified via `clients.get(clientId)`.

### 3. Frontend Module Structure (`web/src/`)

- `web/src/protocol/`: P2PMux V7 frame codec (`frame.ts`), binary TLV codec (`tlv.ts`), and 32-DC pool + dynamic WS multiplexer (`mux.ts`);
- `web/src/shared/`: Strict IPC schemas (`ipc.ts`), WebCrypto and HKDF helpers (`crypto.ts`), RFC 9383 SPAKE2+ (`spake2p.ts`), Fail-Closed Ed25519 TOFU store (`identity.ts`), structured SDP extractor/synthesizer (`sdp.ts`), 3-tier sandbox derivation (`sandbox_origin.ts`), and strict URL rewriting (`url_rewrite.ts`);
- `web/src/portal/`: Config and RFC 7065 ICE validation (`core/config.ts`, automatic `cap=` scrubbing), signaling client (`core/signaling_client.ts`), single-PC transport and unified silent reconnect (`core/peer_session.ts`), 3-state SPAKE2+/capability auth FSM (`core/auth_client.ts`), stats (`core/stats_collector.ts`), cross-site bridge (`core/bridge.ts`), external stylesheet (`portal.css`), and lightweight UI (`ui/modals.ts`, `ui/smart_pill.ts`, `i18n.ts`, `app.ts`);
- `web/src/bridge/`, `web/src/sw/`, and `web/src/patch/`: Minimal hidden bridge page, modular SW anti-CSRF router/stream/native gzip/HTML5 injector/in-memory CookieMirror, and pure-compatibility `ws_patch.ts`.

---

## 4. 3-Tier Sandbox Deployment Modes Comparison

| Dimension | Tier 1: Dual Fixed Cross-Site | Tier 2: Single-Root Same-Site Wildcard | Tier 3: Dual-Root Cross-Site Wildcard (Recommended) |
| :--- | :--- | :--- | :--- |
| **Domains & TLS** | Two fixed hosts on distinct eTLD+1 sites, standard single-host certs | Single root + wildcard subdomain cert | Two distinct eTLD+1 roots, wildcard sandbox cert |
| **Key Flags** | `-portal-domain` + `-sandbox-host` | `-portal-domain` + `-sandbox-domain` + `-allow-same-site-sandbox` | `-portal-domain` + `-sandbox-domain` |
| **Isolation Properties** | Cross-site isolation + SW anti-CSRF gate | Per-node origins + `Origin-Agent-Cluster: ?1` (same-site banner) | Cross-site isolation + device-salted per-node HMAC origins |
| **Best Fit** | Hosts without wildcard cert automation | Single-domain trusted-app personal setups | Multi-node production and enterprise intranets |
