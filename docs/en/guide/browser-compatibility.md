# Browser Compatibility Matrix

WebRTC Gateway runs strictly on standard Web APIs (WebRTC DataChannel, Service Worker, Fetch, Streams, and WebCrypto Ed25519) with no browser extensions or custom root CAs. This page summarizes support tiers, verification coverage, and platform limits.

---

## 1. Support Tiers

| Browser Family | Engine | Tier | Verification | Notes |
| :--- | :--- | :---: | :--- | :--- |
| **Google Chrome** | Chromium / Blink | **Tier 1** | Automated CI CDP E2E | Linux CI runs cross-site OOPIF E2E against the embedded `web.DistFS` binary (Chrome 137+ uses native WebCrypto `Ed25519`; Chrome 87–136 automatically fall back to built-in zero-dependency pure-JS RFC 8032 Ed25519 verification) |
| **Microsoft Edge** | Chromium / Blink | **Tier 1** | Shared CDP E2E suite | Edge 87+ (native WebCrypto `Ed25519` on 137+, automatic pure-JS fallback on earlier builds); select locally via `EDGE_BIN` or `BROWSER_BIN` |
| **Brave / Vivaldi / Opera** | Chromium / Blink | Tier 2 | Community verified | Chromium 87+ base (native WebCrypto or automatic pure-JS Ed25519 fallback); aggressive WebRTC privacy blockers may require TURN relay |
| **Mozilla Firefox** | Gecko | Tier 2 | Community verified | Firefox 103+ (native WebCrypto `Ed25519` on 129+, automatic pure-JS fallback on earlier builds); SW falls back to `arrayBuffer()` when `FetchEvent.request.body === undefined` |
| **Apple Safari (macOS / iOS)** | WebKit | Tier 2 | Community verified | Requires Safari 17+ (native WebCrypto `Ed25519` and Transferable `ReadableStream`); subject to iOS suspension and `getRemoteCertificates()` limits below |
| **Android Chromium Browsers** | Chromium / Blink | Tier 2 | Community verified | Modern Chrome Android and Edge Mobile |

> [!NOTE]
> **Tier 1** browsers are verified on every CI run via `test/e2e/browser_suite.test.mjs`; **Tier 2** browsers rely on standard Web APIs without automated CI coverage.

---

## 2. Cross-Engine Boundaries and Sleep Recovery

### 1. Background Suspension and `P2P_SW_EVICTED` Port Recovery

- **Platform behavior**: Mobile browsers may terminate idle Service Workers or suspend sockets when backgrounded.
- **Gateway recovery path**:
  1. Bootstrap `/__p2p_gw__/bridge.html` exists strictly before `appFrame` mounts and is removed immediately upon `P2P_BRIDGE_READY` (zero temporal overlap);
  2. When a sandbox request finds no attached `bridgePort` or receives no `REQ_ACK` from Portal within **3000 ms** (confirmed dead via a **3000 ms** `BRIDGE_PING`), SW evicts that tab's stale port (preserving `cookieMirror` and `clientIds`) and broadcasts `P2P_SW_EVICTED` to the tab's nested `appFrame` clients;
  3. `patch.js` inside `appFrame` relays `P2P_SW_EVICTED` to `window.parent` (Portal), and Portal spawns an ephemeral `bridge.html` to hand off a fresh `MessagePort` (never invoking `location.reload()` or `sessionStorage` reload guards, avoiding Safari ITP / Firefox ETP reload loops);
  4. If WebRTC disconnects, Portal runs unified silent reconnect over a fresh `RTCPeerConnection` reusing the tab's in-memory `(w0, w1)` SPAKE2+ verifier, keeping `appFrame` mounted without re-prompting for a password.

### 2. Storage Partitioning, Transferable Streams, and Engine Limits

- **Session cookies**: Backend `HttpOnly` cookies live exclusively in the Agent's `ProxyCookieJar` and do not depend on browser third-party cookies, remaining unaffected by Safari ITP;
- **In-memory `CookieMirror`**: Non-`HttpOnly` cookies are never written to real browser `document.cookie` (preventing cross-node leakage and stale cookies); `patch.js` maintains an in-memory mirror synchronized bidirectionally via P2PMux V8 inline request/response TLVs;
- **Native Transferable `ReadableStream` and decompression**: Service Worker and Portal transfer request/response bodies natively as Transferable `ReadableStream`s (automatically falling back to `arrayBuffer()` when Firefox exposes `FetchEvent.request.body === undefined`), and decompress compressible responses via native `DecompressionStream('gzip')` with zero third-party JS dependencies;
- **WebKit / Safari and Dedicated Worker boundaries**:
  - On WebKit/Safari builds that do not expose `RTCDtlsTransport.getRemoteCertificates()`, the browser relies on the underlying DTLS stack matching the signed SDP fingerprint line (Agent always verifies the live remote DTLS certificate directly);
  - `patch.js` injects into window document contexts only: a page's `DedicatedWorker` can issue `fetch` / XHR through the Service Worker normally, but does not receive the window-level `WebSocket` tunnel patch (`new WebSocket()` must originate from or be proxied through the window context).

### 3. Secure Context Requirement

- Per W3C specifications, `navigator.serviceWorker` and `crypto.subtle` require a Secure Context:
  - Local development: start `signal -dev` and visit `http://localhost:<port>` with automatically derived `http://sbx-*.localhost:<port>` (RFC 6761);
  - LAN and public production: **HTTPS / WSS is mandatory** (explicit startup `-portal-origin` and `-sandbox-origin` with TLS certificates, or `-tls-auto` for local testing).

---

## 3. Mobile Usage Tips

- Keep the browser tab in the foreground during large file downloads or long media playback to prevent OS-level background throttling from stalling SCTP streams.
- If a cached app behaves unexpectedly after an upgrade, clear site data for the sandbox origin and reload the Portal link.
