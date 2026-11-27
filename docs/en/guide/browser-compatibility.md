# Browser Compatibility Matrix

WebRTC Gateway runs strictly on standard Web APIs (WebRTC DataChannel, Service Worker, Fetch, Streams, and WebCrypto Ed25519) with no browser extensions or custom root CAs. This page summarizes support tiers, verification coverage, and platform limits.

---

## 1. Support Tiers

| Browser Family | Engine | Tier | Verification | Notes |
| :--- | :--- | :---: | :--- | :--- |
| **Google Chrome** | Chromium / Blink | **Tier 1** | Automated CI CDP E2E | Linux CI runs cross-site OOPIF E2E against the embedded `web.DistFS` binary |
| **Microsoft Edge** | Chromium / Blink | **Tier 1** | Shared CDP E2E suite | Select locally via `EDGE_BIN` or `BROWSER_BIN` |
| **Brave / Vivaldi / Opera** | Chromium / Blink | Tier 2 | Community verified | Aggressive WebRTC privacy blockers may require TURN relay |
| **Mozilla Firefox** | Gecko | Tier 2 | Community verified | Requires WebCrypto `Ed25519`, Service Worker, and `ReadableStream` |
| **Apple Safari (macOS / iOS)** | WebKit | Tier 2 | Community verified | Requires WebCrypto `Ed25519` (Safari 17+); subject to iOS background suspension below |
| **Android Chromium Browsers** | Chromium / Blink | Tier 2 | Community verified | Modern Chrome Android and Edge Mobile |

> [!NOTE]
> **Tier 1** browsers are verified on every CI run via `test/e2e/browser_suite.test.mjs`; **Tier 2** browsers rely on standard Web APIs without automated CI coverage.

---

## 2. WebKit (Safari / iOS) and Sleep Recovery

### 1. Background Suspension and Service Worker Port Recovery

- **Platform behavior**: Mobile browsers may terminate idle Service Workers or suspend sockets when backgrounded.
- **Gateway recovery path**:
  1. When a sandbox request finds no attached `bridgePort` or receives no `REQ_ACK` from Portal within **1500 ms** (for idempotent bodyless requests after confirming port death via `BRIDGE_PING`), SW evicts the stale port (preserving `cookieMirror` and `clientIds`) and notifies `/__p2p_gw__/bridge.html` via `SW_NEED_PORT`;
  2. If the Service Worker wakes cold after idle termination, recovery automatically wakes the hidden bridge ONLY when an unambiguous single-tab nested `appFrame` or worker client is confirmed via `clients.get(clientId)`;
  3. The hidden bridge omits `location.reload()` and `sessionStorage` reload guards, eliminating Safari ITP and Firefox ETP reload loops;
  4. If WebRTC disconnects, Portal runs unified silent reconnect over a fresh `RTCPeerConnection` reusing the tab's in-memory `(w0, w1)` SPAKE2+ verifier, keeping `appFrame` mounted without re-prompting for a password.

### 2. Storage Partitioning and ITP

- **Session cookies**: Backend `HttpOnly` cookies live exclusively in the Agent's split-pool `ProxyCookieJar` and do not depend on browser third-party cookies, remaining unaffected by Safari ITP;
- **In-memory `CookieMirror`**: Non-`HttpOnly` cookies are never written to real browser `document.cookie` (preventing cross-node leakage and stale cookies); `ws_patch.ts` maintains an in-memory mirror synchronized bidirectionally via P2PMux V7 inline request/response TLVs.
- **Native streaming decompression**: The Service Worker uses standard `DecompressionStream('gzip')` to decompress compressible responses from the Agent with zero third-party JS compression dependencies.

### 3. Secure Context Requirement

- Per W3C specifications, `navigator.serviceWorker` and `crypto.subtle` require a Secure Context:
  - Local development: `http://localhost:<port>` and automatically derived `http://sbx-*.localhost:<port>` (RFC 6761);
  - LAN and public production: **HTTPS / WSS is mandatory** (dual-domain TLS certificates, or `-tls-auto` for local testing).

---

## 3. Mobile Usage Tips

- Keep the browser tab in the foreground during large file downloads or long media playback to prevent OS-level background throttling from stalling SCTP streams.
- If a cached app behaves unexpectedly after an upgrade, clear site data for the sandbox origin and reload the Portal link.
