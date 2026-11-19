# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Reliability & Concurrency (Sprint 1)
- **AgentSession Aggregate Root & Generation Lifecycle (G, E, F 项)**:
  - Replaced disparate session maps in `pkg/agent/agent.go` with unified `AgentSession` aggregate root in `pkg/agent/session.go`.
  - Added monotonic `Generation` counter and PC binding to prevent stale 5-second teardown timers or asynchronous Pion PC closure callbacks from closing new connections' Mux instances or evicting active CookieJars.
  - Added session-scoped `offerMu` serializing concurrent Offers and ICE Restarts without cross-session head-of-line blocking.
  - Implemented bounded early candidate queues (32 per session) and pending sessions quota (128 max) to prevent memory exhaustion DoS.
- **Atomic CAS Stream Limit Admission (A 项)**:
  - Replaced TOCTOU `atomic.LoadInt64 >= limit` check in `pkg/agent/agent.go` with atomic CAS loop `tryAcquireStream()` and RAII `sync.Once` release handle, guaranteeing active streams strictly adhere to concurrency limits under heavy load.
- **Ping/Pong Sequence ID Multiplexing (D 项)**:
  - Added 8-byte big-endian sequence ID payload to Mux Ping/Pong frames in `pkg/mux/mux.go` with sequence tracking, eliminating RTT measurement crosstalk and stale pong false positives while preserving 100% backward compatibility with Web Portal clients.
- **Stream.Close Error Propagation (C 项)**:
  - Ensured `Stream.Close()` in `pkg/mux/stream.go` returns underlying `writeFrame` errors instead of masking write failures.

### Security & Defense (Sprint 1)
- **TargetPolicy Strict Origin & Proxy Port Whitelist (J, H 项)**:
  - Replaced substring matching in `pkg/proxy/ssrf.go` with strict URL origin parsing `ValidateTrustedOrigin()` against target hostnames and subdomains, preventing origin spoofing and subdomain escape.
  - Enforced strict proxy port whitelisting `IsPermittedProxyPort()` across HTTP and WebSocket proxies to prevent outbound SSRF scans on internal network infrastructure.

## [v2.1.0] - 2026-11-19

### Security & Sandboxing
- **PostMessage Cross-Origin Hijack Protection & Wildcard Deprecation**:
  - Enforced strict `event.origin` and `event.source` validation on postMessage handlers (`TUNNEL_WS_CONNECT`, `TUNNEL_COOKIE_UPDATE`, and `P2P_ROUTE_SYNC`) in `web/src/portal/app.ts`, preventing malicious third-party origins from taking over intranet WebSocket tunnels via `window.open` or cross-origin iframes.
  - Replaced wildcard `'*'` target origin with exact window origin in `web/src/patch/ws_patch.ts`.

### Fixed & Stream Resilience
- **GET/HEAD Empty-Body Stream Teardown & Leak Prevention**:
  - Fixed an architectural stream leak where GET/HEAD requests never emitted `REQ_BODY_END`, causing the Portal client to omit the FIN flag and leaving server streams permanently open in `mux.streams` until the 5-minute timeout.
- **Backpressure Deadlock & Stream Removal Auto-Wakeup**:
  - Resolved a critical deadlock in `pkg/mux/mux.go` where `m.bpCond.Wait()` hung indefinitely when a stream was reset or closed from `m.streams` (`s == nil`), adding broadcast signaling and fail-fast stream removal checks.
- **Trickle ICE Candidate Buffering Queue**:
  - Replaced `time.Sleep` polling in candidate handling with an orderly `pendingCandidates` buffer in `pkg/agent/agent.go` and `cmd/agent/main.go`, guaranteeing no candidate is dropped when `RemoteDescription` is not yet applied.
- **Protocol Proxy Path Decoupling**:
  - Removed hardcoded third-party Bilibili `/upgcxcode/` path magic value from `pkg/proxy/http_proxy.go`, converging on standard media extension classification.

### Added & Verification Testbed
- **Live Cloudflare Worker SQLite Durable Object E2E Test Suite**:
  - Implemented `test/cf_worker_e2e_test.go` with live public edge testing, verifying WebSocket hibernation, peer status synchronization, and room sharding without local broker dependencies.
- **Real Linux Bare-Metal Chaos Testbed Verification**:
  - Implemented `test/real_linux_testbed_e2e_test.go` validating kernel-level network loss and jitter (`tc qdisc netem 30% loss 150ms delay 20ms`), zero race conditions under native Linux CGO `-race`, and strict certificate validation.
- **Testing Checklist 2.0 & Real Browser Matrix Runner**:
  - Upgraded real-world testing checklist to 2.0 (`docs/TESTING_CHECKLIST.md`) and added automated CDP browser matrix runner (`test/real_browser_matrix_runner.cjs`).
  - Signed off and archived official release test report `docs/RELEASE_REPORTS/v2.1.0_TEST_REPORT.md` (100% pass rate).

### Docs & Engineering Governance
- **Versioning Policy & Deprecation Lifecycle**:
  - Established formal Semantic Versioning Policy and two-stage Parameter Deprecation Lifecycle in `docs/RELEASE_CHECKLIST.md` and `docs/en/RELEASE_CHECKLIST.md`.

## [v2.0.8] - 2026-11-18

### Fixed & Protocol Robustness
- **Service Worker Subresource HTML Detection Guard & Safe JSON Streaming**:
  - Restricted `isHtml` classification in `web/src/sw/sw.ts` strictly to navigation document requests (`request.mode === 'navigate' || request.destination === 'document' || request.destination === 'iframe'`).
  - Removed blind byte-0 prepending of `patchBytes` in `injectScriptIntoHtmlStream` when HTML structural tags (`<head>`, `<html>`, `<!doctype>`) are absent, returning raw response payloads completely untouched.
  - Resolves Synology Tailscale and legacy CGI/API failures where subresource JSON payloads with `Content-Type: text/html` were corrupted with injected `<meta>` tags causing `SyntaxError: Unexpected token '<', "<meta name"... is not valid JSON`.
  - Added 14 automated verification assertions in `test/node_id_preservation_test.cjs`.

### Docs & Engineering Standards
- **Official Documentation Style Guide & Automated Hygiene Linter**:
  - Published official bilingual *Documentation & Copywriting Engineering Style Guide* (`docs/STYLE_GUIDE.md` & `docs/en/STYLE_GUIDE.md`) establishing core tenets: Neutral Restraint, Conciseness & BLUF (Bottom Line Up Front), Technical Demystification, Header Emoji Hygiene, Typographic Precision ("Pangu Spacing"), and the Single-Version Release Notes Invariant.
  - Implemented automated documentation hygiene linter (`docs/scripts/lint-docs.cjs` via `npm --prefix docs run docs:lint`) integrated into the GitHub Actions CI pipeline.
  - Systematically audited and refactored full-repo technical documentation, TOC permalink anchors, and de-hyped technical vocabulary across 30+ Markdown files.

## [v2.0.7] - 2026-11-18

### Fixed & Resilience
- **Service Worker Stream Controller Cancellation Defense (`ReadableStreamDefaultController.enqueue`)**:
  - Added an atomic `isStreamClosed` state lock and enclosed `controller.enqueue(chunk)` within a defensive `try...catch` block in `web/src/sw/sw.ts`.
  - Completely eliminates unhandled `TypeError: Failed to execute 'enqueue' on 'ReadableStreamDefaultController': Cannot enqueue a chunk into a closed readable stream` in high-concurrency environments or when client fetch requests / browser extensions (such as Dark Reader or aggressive tab switchers) cancel in-flight HTTP streams before pending chunks arrive across the `MessagePort` IPC channel.
  - Safely logs dropped in-flight chunks and returns early to ensure stream teardown never destabilizes the Service Worker thread.

## [v2.0.6] - 2026-11-18

### Fixed & UX
- **HUD Smart Pill Button Event Isolation & Pointer Capture Conflict Resolution**:
  - Fixed a critical event hijacking issue where `setPointerCapture` on the parent floating pill caused the browser to redirect all click targets to `#p2pPill`, rendering the collapse arrow (`#pillCollapseBtn`) and settings gear (`#pillGear`) inoperative and mistakenly opening the Diagnostic HUD modal instead.
  - Added dedicated `pointerdown` and `click` listeners with `e.stopPropagation()` on `#pillCollapseBtn`, `#pillGear`, and `#pillCollapsedContent`.
  - Added target guard in `pill.onpointerdown` (`target.closest('#pillCollapseBtn') || target.closest('#pillGear')`) to bypass drag initiation on interactive buttons.
  - Expanded automated test `test/test_smart_pill.cjs` to enforce button click handler presence and anti-capture isolation.

## [v2.0.5] - 2026-11-18

### Fixed & Protocol Robustness
- **RFC 3986 Authority Trap Resolution & Double-Slash Path Normalization**:
  - Normalized leading duplicate slashes (e.g. `//synoscgi.sock/...` -> `/synoscgi.sock/...`) across `pkg/proxy/http_proxy.go`, `pkg/proxy/ws_proxy.go`, `web/src/sw/sw.ts`, and `web/src/portal/app.ts`.
  - Fixed an RFC 3986 parser differential where Go's standard library `url.Parse` mistook relative URLs starting with `//` as Authority (Host), truncating request paths and causing 404 errors on Synology DSM, Socket.IO long-polling, and real-time resource monitor graphs.
  - Added unit test `TestResolveTargetURL_LeadingDoubleSlashesAndSynologySocketIO` asserting full path preservation and correct target host routing for HTTP and WebSocket proxies.
- **Chromium Service Worker Language Preference Passthrough (`Accept-Language`)**:
  - Implemented automatic language fallback in `web/src/sw/sw.ts` referencing `navigator.languages` to circumvent Chromium Issue 40728185 (Service Worker omitting `Accept-Language` from fetch headers).
  - Eliminates forced English fallbacks on intranet management consoles (Synology DSM, Proxmox, Nextcloud, TrueNAS).

### Changed & UX Refinement
- **Smart Pill 2.1 KISS Architecture & Zero Keystroke Interception**:
  - Streamlined the WebRTC diagnostic HUD into a pure dual-mode interface: [Expanded Pill with RTT/Speed/Settings/Collapse Arrow] ⇋ [28px Magnetic Breathing Dot].
  - Completely removed global `keydown` event listeners (dropped double `Esc` shortcut and ghost mode), guaranteeing 100% untouched keystroke passthrough for Vim, Neovim, Emacs, and Web SSH terminals (ttyd, Portainer).
  - Implemented `setPointerCapture` and dynamic `pointer-events: none` on `appFrame` during drag, eliminating event swallowing by child iframes.
  - Added automated regression test `test/test_smart_pill.cjs` validating DOM lifecycle, Euclidean distance drag disambiguation, boundary clamping, and coordinate sanitization.

### Security & Privacy
- **Documentation & Test Bed Privacy Cleansing**:
  - Fully scrubbed all private IP references (`192.168.1.100` -> `192.168.1.100`, `192.168.1.100` -> `192.168.1.100`) and private port mappings (`8443` -> default `:8888`) across `README.md`, `README_en.md`, `docs/`, and test runners.

## [v2.0.4] - 2026-11-18

### Performance & Scalability
- **32KB Allocator Class Alignment & Zero-Alloc Frame Encoding (PERF-01)**:
  - Adjusted `RecommendedChunkSize` to `32*1024 - HeaderSize` (32758 bytes), aligning total frame size (32768) perfectly with Go runtime's 32KB Small Object Class.
  - Implemented `frameEncodePool = sync.Pool` in `pkg/mux/mux.go`'s `writeFrame` for zero-allocation frame serialization on the sequential send path.
- **Browser Transferable ArrayBuffer Streaming (PERF-02)**:
  - Transferred native `ArrayBuffer` directly into `postMessage` when unshared (`byteOffset === 0 && byteLength === buffer.byteLength`), eliminating redundant `.slice()` deep clones.
- **WebSocket Linear $O(N)$ Defragmentation Queue (PERF-03)**:
  - Replaced quadratic array reallocation with a preallocated accumulator using `consumeBytes` and `peekHeader`, eliminating V8 heap bloat and GC pauses under high-throughput WebSocket streams.
- **HTTP/2 Transport Pool & Resource Lifecycle Optimization (PERF-05, PERF-07, PERF-11)**:
  - Enabled `ForceAttemptHTTP2: true` and expanded connection pool to `MaxIdleConns: 512, MaxIdleConnsPerHost: 128` in `pkg/proxy/http_proxy.go`.
  - Added short-circuit check in `CookieJar.SaveCookies` on empty `Set-Cookie`, bypassing read locks for >95% of asset requests.
  - Cleared consumed chunk references (`s.inbound[0] = nil`) in `Stream.Read` to allow immediate garbage collection.
- **Signaling Broker Asynchronous Broadcast & Session Isolation (PERF-09 & Trap 1)**:
  - Refactored `NotifyWaitingClients` to release read lock immediately after capturing target client snapshots, dispatching peer notifications asynchronously via goroutine with unique `SessionID: client.ID` preserved per client.
- **Zero-Redundant Stream Demultiplexing & Dispatch (PERF-06 & Trap 2)**:
  - Retained `TypeHeaders` on wire to preserve protocol invariants; introduced `HandleStreamWithMeta` across HTTP, WebSocket, and File proxies, eliminating `PushInternalHeaders` and secondary unmarshaling in agent stream loops.
- **Cloudflare DO Edge Room Sharding & Seamless Fallback (PERF-10 & Trap 4)**:
  - Appended `?node=${nodeID}` to agent signaling URL to isolate Durable Object instances; implemented `legacyFallbackAttempted` in Web Portal to fallback smoothly to global room if peer is not found.
- **Browser Sandbox & SSR Streaming Injection Hardening (PERF-13, PERF-14 & Blind Spot 3)**:
  - Added `visibilitychange` listener to pause `hudStatsTimer` when tab is in background; throttled HUD metrics polling to 3000ms and updated only floating badge DOM when modal is closed.
  - Throttled Service Worker `sw.clients.get()` with a 5-second verification window to eliminate IPC storms on asset-dense pages.
  - Buffered initial SSR chunks `< 2048` bytes before evaluating `<head>` injection, preventing duplicate `<head>` tag generation on tiny first chunks.
  - Calculated XHR upload length with `new Blob([body]).size` to avoid `TextEncoder` string duplication in JS heap.

### Changed & Robustness
- **Request Probe Tolerance & Default Method Fallback**:
  - Added `MethodAlt` (`method`) support alongside `m` in `pkg/agent/agent.go` and `pkg/proxy/http_proxy.go`, with graceful fallback to `GET` when method is omitted, improving third-party client interoperability.

## [v2.0.3] - 2026-11-18

### Security & Hardening
- **SSRF Defense-in-Depth & Boundary Hardening (F-01)**:
  - Blocked obfuscation and control characters (`@`, `\`, `/`, whitespace) in target hosts.
  - Eliminated fuzzy subdomain wildcarding when the destination is an IP address or localhost.
  - Expanded restricted IP ranges with `0.0.0.0/8`, multicast, broadcast, IPv4-mapped (`::ffff:`), NAT64 (`64:ff9b::/96`), and 6to4 (`2002::/16`).
  - Stripped ambient environment proxies (`Proxy: nil`) to prevent accidental internal network reconnaissance.
- **SDP DTLS Certificate Fingerprint Binding & Credential Derivation (F-02, F-04)**:
  - Replaced plaintext secret transmission in signaling offers with HMAC-SHA256 signature bound to the client's local SDP DTLS certificate fingerprint (`sign = HMAC(authKey, peerId:exp:sdpFingerprint)`).
  - Implemented Web Crypto single-direction key derivation (`authKey = HMAC(secret, "node-auth:" + peerId)`) on the portal, purging plaintext secrets from DOM and memory immediately upon entry.
  - Strictly enforced positive expiration timestamps (`exp <= 0` rejected), closing timestamp bypass vulnerabilities.
  - Hardened signaling brokers (Go and Cloudflare Worker) against unauthorized secret injection on active unauthenticated nodes.
- **Cloudflare Worker TDZ Crash Fix & Anti-Brute-Force Rate Limiting (F-03, F-08)**:
  - Fixed variable declaration order for `requireVisitorAuth` to resolve Temporal Dead Zone `ReferenceError` during agent registration.
  - Added automated V8 syntax and TDZ guardrail (`test_worker_syntax.cjs`) to CI test pipelines.
  - Added IP-based authentication failure rate limiting (429 Too Many Requests after 10 failures) and constant-time secret comparison (`crypto.subtle.timingSafeEqual`).
- **Strict TLS Certificate Verification by Default (F-05)**:
  - Eliminated hardcoded `InsecureSkipVerify: true`, defaulting to strict system CA root verification.
  - Restricted insecure TLS skipping strictly to target host endpoints when `-insecure` is explicitly passed; external `--p2p-proxy` traffic strictly verifies CA certificates.
- **FileProxy 301 Infinite Loop & Symlink Escape Resolution (F-06)**:
  - Anchored physical root path via `filepath.EvalSymlinks`.
  - Replaced `http.ServeFile` with `http.ServeContent` and custom directory rendering to eliminate `/subdir/` redirect recursion and enforce sandbox escape verification on `index.html`.
- **Mux Backpressure Deadlock Fix, Stream ID Wraparound & Lifecycle Release (F-07, F-10)**:
  - Fixed condition variable deadlock in `writeFrame` by checking `m.closed` and stream context cancellation before `m.bpCond.Wait()`.
  - Explicitly invoked `mux.Close()` on session teardown and graceful shutdown; tightened unknown probe header buffer limit from 16MB to 64KB.
  - Handled 32-bit stream ID wraparound beyond `0xFFFF0000` in both Go and TypeScript mux engines with tombstone resets.
- **Bidirectional Cookie Virtualization & Namespace Isolation (F-09)**:
  - Implemented virtualized `document.cookie` getter/setter in `ws_patch.ts` with transparent `__p2p_${nodeId}_` prefixing.
  - Added `writeScopedCookie` in portal to ensure non-HttpOnly backend cookies (CSRF tokens) are properly namespaced.
  - Fixed node switching dead code to ensure `clientToken` and derived keys are cleanly isolated per node.
- **Quirks Mode Prevention in HTML Stream Injection (F-10)**:
  - Positioned script injection strictly after `<!DOCTYPE ...>` when `<head>` and `<html>` are absent, preserving W3C standard rendering mode.

## [v2.0.2] - 2026-11-17

### Added & Enhanced
- **Observability & Startup Banner Enhancement**:
  - `agent` and `signal` startup banners now explicitly output the release version (`v2.0.2`) on the first log line, resolving container and systemd triage ambiguities.
  - Agent banner displays active runtime topology: signaling endpoint, cluster vs visitor authentication status, concurrent stream limits, STUN/TURN configurations, proxy URL, and TLS SNI.
  - Allowed Proxy Domains consolidated cleanly inside the banner boundary; explicit security warning printed when `-insecure` is active.
  - Signal server banner resolves host/port bindings cleanly, eliminating legacy `localhost0.0.0.0:8888` concatenation issues, and specifies static asset source (`Embedded (v2.0.2)` vs `Local Directory`).
  - Strict adherence to CWE-532: all secret tokens and passwords are masked into status indicators (`Enabled (Protected)` / `Disabled (Public)`).
- **Engineering Quality & Shift-Left Defenses**:
  - Implemented automated CI machine assertion `git diff --exit-code dist` in `.github/workflows/ci.yml` to prevent uncommitted frontend asset drift.
  - Introduced GitHub PR template (`.github/pull_request_template.md`) with a Systemic Invariants self-check card for atomic review guideline and code updates.
  - Added gate 5.6 to Release Checklist for retrospective post-mortem and review guideline auditing.
- **100% Bilingual Documentation Suite**:
  - Created comprehensive English guides for `file-server.md`, `security.md`, `troubleshooting.md`, and `RELEASE_CHECKLIST.md`.
  - Introduced dialectical review guidelines (`REVIEW_GUIDELINES.md` and `en/REVIEW_GUIDELINES.md`) featuring the 5 Golden Principles, machine vs human review division, 5 foundational systemic invariants, and post-mortem case studies.
  - Synchronized VitePress bilingual navigation and MiniSearch localized indexes.

## [v2.0.1] - 2026-11-17

### Fixed & Hardened
- **Cloudflare DO Hibernation State Recovery**:
  - Persisted node secrets, visitor authentication flags (`requireVisitorAuth`), and IP connection cleanup markers (`ipCleaned`) in WebSocket attachments during agent registration.
  - Restored in-memory `nodeSecrets`, `nodeVisitorAuth`, and `ipConns` state maps upon Durable Object hibernation wake-up in both `fetch()` and `handleWSClose()`, guaranteeing visitor authorization and anti-hijacking defenses persist indefinitely across idle intervals.
- **Hardware Gzip Streaming Decompression Bypass for HEAD & Empty Responses**:
  - Excluded `HEAD` requests, `204 No Content`, `304 Not Modified`, and responses with `Content-Length: 0` from Gzip compression passthrough in both backend HTTP proxy and browser Service Worker.
  - Stripped internal proxy control headers (`x-p2p-client-decompress`, `x-p2p-dest-host`) prior to upstream dispatch.
  - Eliminated browser `DecompressionStream('gzip')` throwing `TypeError: Compressed input was truncated` and Service Worker `new Response()` null-body specification violations on `HEAD` requests.
- **Frontend Mux Stream Parity Validation**:
  - Added stream ID parity checks (`frame.streamId % 2 === 0` for client) in `web/src/protocol/mux.ts` before creating inbound multiplexed streams, perfectly aligned with Go backend `pkg/mux/mux.go`.
  - Safely drops stray chunks from client-cancelled requests to eliminate ghost stream leaks.
- **Signaling Server Flag Hardening**:
  - Completely removed the legacy `-secret` alias flag in `cmd/signal/main.go` and enforced fail-fast exit with clear remediation instructions, requiring explicit `-signal-secret` to eliminate accidental unauthenticated deployments.

## [v2.0.0] - 2026-11-17

### Performance & Modern Architecture
- **Gzip Passthrough & Hardware DecompressionStream (300%+ Performance Leap)**:
  - Agent passes through upstream Gzip-compressed responses for static assets (`.js`, `.css`, `.json`, `.svg`, `.wasm`, workers) without inflations across P2P tunnels.
  - Replaces upstream `Content-Encoding` with internal `x-p2p-content-encoding: gzip` and strips `Content-Length`, completely sidestepping W3C synthetic response decoding traps.
  - Service Worker utilizes browser-native hardware streaming decompression `stream.pipeThrough(new DecompressionStream('gzip'))`, slashing transfer sizes by 60%~80% and speeding up page loads by 300%+. Transparent fallback decompression preserved for non-direct environments.
- **Web Worker Dynamic ES Module Adaptation**:
  - Virtualized `window.Worker` and `window.SharedWorker` in client patch, appending `?_p2p_worker=module` when `{ type: 'module' }` is detected.
  - Service Worker conditionally branches script injection: classic workers receive `importScripts("/patch.js");\n`, while module workers receive `import "/patch.js";\n`.
  - Solves W3C `TypeError: Module scripts don't support importScripts()` across modern frontends including Vite, VitePress, SvelteKit, and PDF.js v4+.
- **Zero Chronic Memory Leak in Signaling Server (`IPRateLimiter`)**:
  - Implemented background `janitorLoop` goroutine with graceful `stopCh` lifecycle management in `cmd/signal/main.go`.
  - Automatically sweeps `msgTimes` and `failedAuths` maps, pruning expired timestamps and deleting orphan IP keys from memory.
- **Cloudflare Worker WebSocket Hibernation API & Dynamic Sharding**:
  - Migrated Cloudflare Worker signaling room from `server.accept()` to `this.ctx.acceptWebSocket(server)` Hibernation API.
  - Integrated `setWebSocketAutoResponse(new WebSocketRequestResponsePair('ping', 'pong'))` to handle heartbeats at Cloudflare edge, enabling deep DO hibernation and slashing 24x7 idle standby billing to near zero.
  - Implemented dynamic node sharding routing `?node=<nodeId>` to isolated DO instances with smooth fallback to `global-broker`.
- **Configuration Templates & Documentation Alignment**:
  - Calibrated `configs/agent.example.yaml`: corrected `node_id` default annotation to `"homeserver"`.
  - Clarified that `sni` and `insecure` options apply exclusively to outbound signaling connections, not intranet backend targets.
  - Clarified `portal_url` configuration and default value for signed share links.

---

## [v1.9.3] - 2026-11-17

### Security & Hardening
- **Signaling Security & Attribution Hardening**:
  - Cloudflare Worker edge node now verifies `SIGNAL_SECRET` authentication for client registrations.
  - Implemented `ipCleaned` boolean idempotency lock in Worker to prevent duplicate IP decrement under abnormal connection terminations.
  - Enforced strict node attribution checks (`session.Role == "agent" && client.NodeID == session.NodeID`) in both Go signaling server and Cloudflare Worker to block cross-node spoofing attacks.
- **P2PMux Ghost Stream Elimination**:
  - Implemented strict stream ID parity checks and monotonic high-water mark with closed stream tombstones, dropping orphan frames and preventing ghost stream resurrection.
  - Added asynchronous `TypeRstStream` frame dispatch before terminating local stream on 32MB buffer overflow.
- **Service Worker Pure Byte Streaming Injection**:
  - Replaced string-based `TextDecoder` HTML stream injection with direct `Uint8Array` ASCII tag scanning (`findAsciiTagEnd`), eliminating multi-byte UTF-8 chunk-slicing corruption across streaming boundaries.
  - Subsequent data chunks pass through without memory conversions.
- **Proxy Domain Whitelist Contract Alignment**:
  - Added `x-p2p-allowed-domains` response header in backend HTTP proxy, exposing agent whitelist dynamically to the visitor browser.
  - Removed brittle heuristics (`brandShort`, `isRelatedBrand`, `isMedia`) from Service Worker and client patch, aligning domain interception with `session.allowedDomains` / `window.__P2P_ALLOWED_DOMAINS__`.
  - Non-whitelisted 3rd-party domains safely route through native visitor network without triggering 400 Bad Request errors.
- **Sandbox Virtualization & Dead Code Cleanup**:
  - Removed dead `sanitizeCSP` helper from `pkg/proxy/http_proxy.go`.
  - Documented explicit rationale for full CSP stripping at the proxy boundary to protect Service Worker virtualization and WebSocket tunnels.

---

## [v1.9.2] - 2026-11-17

### Added
- **Container Non-Root Security Hardening & Arbitrary `--user` Support**:
  - Official multi-architecture Docker images run as unprivileged user `appuser` (`UID: 10001, GID: 10001`) by default, conforming to CIS Docker Benchmarks and Kubernetes Restricted Pod Security Standard.
  - Native runtime compatibility with arbitrary `--user $(id -u):$(id -g)` and `--user 1000:1000`, solving NAS (Synology, QNAP, Unraid) volume permission mismatches.
  - Bound `cap_net_bind_service=+ep` capability to `/app/server` binary via Linux File Capabilities, allowing unprivileged containers to listen on low privileged ports (e.g. `:443`, `:80`).
  - Full compatibility with read-only root filesystems (`--read-only` / `readOnlyRootFilesystem: true`).
- **Structured CLI Usage Help & Version Options**:
  - Implemented `-v` / `-version` flags for `agent` and `signal` binaries to output formatted version information with exit code `0`.
  - Implemented `-h` / `-help` flags for structured CLI usage, environment variables, operational modes, and security sharing examples.
- **VitePress Standalone Documentation Site & i18n Bilingual Support**:
  - Upgraded legacy GitHub Wiki to modern VitePress documentation site deployed to GitHub Pages (`https://beingnoability.github.io/WebRTC-Gateway/`).
  - Implemented path-based i18n routing (`zh-CN` default and `/en/` English), localized MiniSearch offline full-text indexing, and interactive Mermaid architecture diagrams.
  - Archived GitHub Wiki with prominent redirect banners and direct links to live documentation chapters.
- **Industrial Quality Gate Protocol & Release Checklist**:
  - Established 7-stage official release checklist (`docs/RELEASE_CHECKLIST.md`) enforcing strict typechecks, test matrices, Docker smoke tests, and git cleanliness.
  - Streamlined GitHub Actions workflows to eliminate redundant Docker builds and automate SHA256 checksum generation.

---

## [v1.9.1] - 2026-11-17

### Fixed
- **Service Worker Active & Lazy GC against Zombie Sessions (F5 Fast-Refresh)**:
  - Eliminated 30-second `504 Gateway Timeout` and white screen caused by dispatching requests to dead `MessagePort`s of destroyed pages after short-interval refreshes.
  - Implemented Active GC in `PORTAL_ATTACH` sweeping `sw.clients.matchAll({ type: 'window' })` to purge orphaned sessions on page reloads.
  - Implemented Lazy GC in `handleTunneledRequest` via `sw.clients.get(session.clientId)`, instantly pruning stale sessions and recovering the living portal window client.
  - Removed self-poisoning timestamp mutation (`best.lastActive = Date.now()`) in fallback session selection.
- **Web Worker Context Tracking (LibreSpeed & Worker-Heavy Apps)**:
  - Extended child context mapping in `sw.ts` to `destination === 'worker' || destination === 'sharedworker'`.
  - Enables Web Worker requests (e.g. LibreSpeed's `speedtest_worker.js`, Monaco Editor, PDF.js) to resolve accurately to the parent portal session without falling into unmapped fallbacks.

---

## [v1.9.0] - 2026-11-17

### Added
- **Service Worker Multi-Tab Session Isolation (P1-2)**:
  - Eliminated SW global singleton state (`portalPort`, `activeNodeId`, `currentBackendOrigin`), replacing it with client-isolated `PortalSession` maps.
  - Implemented parent-child client mapping cascading via `FetchEvent.resultingClientId` on navigation requests, enabling isolated parallel browsing of different nodes in multiple tabs without crosstalk or context confusion.
- **Strict TypeScript Quality Gate (P2-1)**:
  - Integrated `tsc --noEmit` as an automated typecheck gate into `npm run build` (`package.json`).
  - Added dedicated multi-tab session mapping and worker injection automated test suites to `test/node_id_preservation_test.cjs`.

### Fixed
- **Web Worker Virtualization False Positives (P1-4)**:
  - Removed dangerous `url.pathname.includes('/worker')` heuristic that caused JSON corruption on API routes like `/api/worker/tasks`.
  - Restricted worker virtualization injection strictly to `destination === 'worker' || destination === 'sharedworker' || /(?:\.worker|_worker)\.js$/i` combined with `contentType.includes('javascript')`.
- **UTF-8 Multi-Byte Character Chunk Boundary Splitting (P1-4)**:
  - Adopted `TextDecoder('utf-8', { stream: true })` with trailing flush logic in `injectScriptIntoHtmlStream`, preventing Unicode replacement character (`\uFFFD`) corruption on chunk boundaries.
- **Frontend Strict TypeScript Type Errors (P2-1)**:
  - Pruned dead property reference `this.mediaContainer` in `app.ts`.
  - Fixed `PatchedWebSocket.readyState` union literal typing (`0 | 1 | 2 | 3`) in `ws_patch.ts`.
  - Aligned `ArrayBufferView<ArrayBuffer>` buffer casting in `P2PMux.sendRaw` and `BlobPart[]` casting in `triggerBlobDownload`.
- **WebSocket Reverse Proxy Protocol Header Alignment (P2-4)**:
  - Dynamically set `X-Forwarded-Proto` (`"ws"` vs `"wss"`) in `ws_proxy.go` based on `targetBase.Scheme`, aligning with `http_proxy.go`.
- **Configuration Documentation Calibration (P2-4)**:
  - Corrected `tls_auto` comment in `configs/server.example.yaml` to specify in-memory self-signed ECDSA certificate for local/LAN Secure Context.

---

## [v1.8.1] - 2026-11-17

### Security
- **Portal DOM XSS Elimination**:
  - Enforced strict regex whitelist validation (`/^[a-zA-Z0-9._-]{1,64}$/`) across all node ID inputs (URL Hash, Query, Hostnames, Subdomains, Local/SessionStorage). Malformed node IDs safely fall back to default.
  - Eliminated template string interpolation in settings and diagnostic modals, adopting DOM API property setting (`input.value = ...`) and comprehensive HTML entity escaping.
- **Secret Leakage Chain Severed (PoLP Minimal Privilege)**:
  - Completely removed `window.__P2P_SECRET__` injection into proxied HTML pages via Service Worker.
  - Removed `?secret=` query parameter appending in virtualized URL resolutions (`ws_patch.ts`).
  - Removed secret transmission in WebSocket `join` signaling messages and child iframe `postMessage`.
- **Outbound Media & Proxy Relay Whitelist Defense (`/--p2p-proxy/`)**:
  - Deprecated and removed fuzzy heuristic `isMedia` and `brandShort` substring matching in reverse HTTP and WebSocket proxies.
  - Enforced default strict target host / root domain matching with an explicit `-allowed-proxy-domains` wildcard pattern whitelist flag and YAML config.
  - Isolated signaling proxy from backend reverse proxy Transport.
- **Signaling Server Anti-Brute-Force & Node Enumeration Hardening**:
  - Implemented preemptive `IsLocked` sliding window circuit breaker on WebSocket signaling handshake, immediately rejecting blocked attacker IPs and clearing lockout upon successful auth.
  - Removed unauthenticated `list_nodes` endpoint from Cloudflare Worker edge signaling.
- **Unified Worker Single Source of Truth**:
  - Removed redundant `web/worker.js` and consolidated `web/wrangler.toml` entrypoint to `edge/cloudflare-worker/worker.js`, ensuring authoritative client IP stamping across all edge deployments.

### Fixed
- **Agent Active Peer Concurrency Race Condition**:
  - Encapsulated `len(a.activeConns)` and atomic updates to `activePeers` inside `connMu.Lock()` across session handling, in-place replacement, and teardown lifecycles.

---

## [v1.8.0] - 2026-11-17

### Added
- **Authoritative Client IP Propagation to Backend / WAF (Anti-Spoofing)**:
  - Agent reverse proxies (HTTP and WebSocket) inject real client IP into `X-Forwarded-For` and `X-Real-IP` request headers, enabling upstream WAFs (SafeLine, Cloudflare WAF, ModSecurity, Nginx WAF) and backend logs to accurately recognize individual visitor public IPs for rate limiting, blacklisting, and GeoIP audit.
  - Multi-tier anti-spoofing defense: signaling server forcefully stamps `msg.ClientIP = session.RemoteIP` on Offer, Cloudflare Workers parity with `cf-connecting-ip`, and proxy `.Set()` completely overwrites any client-supplied spoofed headers.
  - In-place ICE Restart mobile roaming IP update: dynamically refreshes session client IP when visitors roam between Wi-Fi and cellular without dropping connection.
  - Graceful fallback: legacy signaling servers safely default to `127.0.0.1`.
  - Memory leak protection: strict session IP lifecycle eviction upon teardown and agent termination.

---

## [v1.7.4] - 2026-11-16

### Added
- **P2P File Hub Action Sheet**:
  - Unified action sheet with Play Online, P2P Download and Cancel control.
  - Cancel control with immediate stream reset to save network bandwidth.

---

## [v1.7.3] - 2026-11-16

### Fixed
- **Zero-Network Fetch Download**:
  - Dual-mode disk writing and real-time progress HUD for file downloads.

---

## [v1.7.2] - 2026-11-16

### Fixed
- **Elimination of Audio Echo & Double Playback in Media Streaming**:
  - Removed duplicate foreground DOM video/audio player (`showNativeMediaPlayer`), completely curing the concurrency flaw where a foreground player and a hidden iframe player simultaneously streamed and played the same video.
  - Returned media rendering entirely to the browser's native engine inside `appViewport` iframe, preserving line-rate P2P streaming with zero duplicate streams.
  - Enabled smooth natural history navigation via browser Back button / mobile swipe in directory browsing mode.
- **P2P File Vault Background Stream Truncation & 204 Interception**:
  - When activating `P2P File Vault` for PDFs and binary downloads, immediately replies with `HTTP 204 No Content` to the iframe navigation and calls `stream.reset()`.
  - Cancels iframe navigation per W3C spec, preventing Edge sandbox plugin blocking errors and saving 100% of background P2P bandwidth.
- **Enhanced Feature Policy on App Shell Viewport**:
  - Explicitly configured `allowfullscreen="true"` alongside `allow="fullscreen; ... picture-in-picture"` on `appViewport` for universal cross-browser fullscreen capability.

---

## [v1.7.1] - 2026-11-16

### Fixed
- **`-dir` Special Filename 404 Resolution (RFC 3986 Percent-Decoding)**:
  - Fixed 404 Not Found error when accessing files with Asian characters, spaces, and brackets (e.g. `[ANi] FX 戰士久留美 - 01 ...mp4`).
  - Placed `url.PathUnescape` strictly before `path.Clean` to restore legitimate characters while defending against `%2e%2e` traversal bypasses and null-byte injection.
- **`-file` Media Streaming Fix & Service Worker Precise Interception**:
  - Refined root path bypass condition in Service Worker (`isTopLevelDocNav`) to strictly allow top-level document HTML navigations while 100% intercepting and tunneling `<video src="/">` and Range requests.
  - Added comprehensive built-in MIME dictionary in `pkg/proxy/file_proxy.go` covering MP4, WebM, MKV, MP3, PDF, etc., ensuring proper `video/mp4` headers across minimal environments.
- **Edge & Mobile PDF Plugin Sandbox Blocking Resolution (P2P File Vault)**:
  - Eliminated Edge's "此页面已被 Microsoft Edge 阻止" and mobile Edge disabled plugin errors caused by W3C sandboxed iframe restrictions on PDFium.
  - Introduced **P2P File Vault** UI featuring 1-click line-rate P2P streaming download and open in native un-sandboxed tab preview.
  - Mounted native HTML5 `<video controls autoplay playsinline>` in portal DOM, unlocking full GPU acceleration, PiP, and fullscreen.

---

## [v1.7.0] - 2026-11-16

### Added
- **Mixed Workload Benchmarking (`test/mixed_workload_benchmark_test.go`)**:
  - Validated P2PMux 32KB chunk scheduling under concurrent 10MB file streaming (46.21 MB/s) and 50 burst REST API calls (Min 0.58ms, P50 4.14ms, P95 7.11ms).
- **ICE Restart Mobile Roaming Resilience Test (`test/ice_restart_resilience_test.go`)**:
  - Automated in-place `iceRestart: true` simulation with full session preservation and CookieJar retention.
- **Browser Compatibility Matrix (`docs/guide/browser-compatibility.md`)**:
  - Documented Tier 1 and Tier 2 browser support and WebKit/iOS Safari hibernation re-attachment.

### Fixed
- **P2PMux Frame Specification Alignment**:
  - Corrected frame type documentation across README, Chinese README, English README, and Wiki (`DATA=0x01`, `HEADERS=0x02`, `WINDOW_UPDATE=0x04`, `PING=0x05`, `PONG=0x06`).
- **Strict Frame Length Validation**:
  - Hardened `DecodeFrameBytes` in Go (`pkg/protocol/frame.go`) and TypeScript (`web/src/protocol/frame.ts`) to enforce exact match `len(data) == HeaderSize + int(frame.Length)`.

---

## [v1.6.2] - 2026-11-16

### Added
- **Native Healthcheck Probe (`-health`) & `/healthz` HTTP Endpoint**:
  - Go-native healthcheck subcommand in `server` and `agent` eliminating Alpine/Docker curl/wget dependency.
  - Resolved Docker Compose `unhealthy` container status.
- **Official Vector Logos**:
  - Vector square icon (`assets/logo.svg`) and full banner (`assets/logo-banner.svg`).

---

## [v1.6.1] - 2026-11-16

### Fixed
- **WebSocket Keepalive Ping Pump & Oscillation Elimination (Dual-Sided Heartbeat)**:
  - **Root Cause Resolution**: Eliminated the "61-second oscillation" bug where the signaling server enforced a 60-second `ReadDeadline` expecting Pongs, but neither the server nor the agent sent periodic Ping frames during idle periods, causing repetitive socket timeout disconnections every 61 seconds.
  - **Server-Side Proactive Ping Pump (`cmd/signal/main.go`)**: Implemented a 25-second Ping ticker sending RFC 6455 `PingMessage` control frames with 5-second write deadlines, synchronized strictly under `session.mu` mutex to prevent concurrent write panics. Automatically keeps visitor browsers connected without client-side code changes.
  - **Agent-Side Proactive Ping Pump & Synchronized Handlers (`pkg/agent/agent.go`)**:
    - Added a proactive 25-second Ping pump protected by `a.wsMu` mutex, ensuring long-lived connections survive across idle timeouts on enterprise firewalls, Nginx reverse proxies, and Cloudflare Workers (100s idle timeout);
    - Implemented thread-safe `SetPingHandler` (responding with Pong under `a.wsMu`) and `SetPongHandler` extending `ReadDeadline` to +60s;
    - Safely cleared `a.wsConn = nil` on disconnect under mutex to prevent stale socket writes.
  - **Comprehensive Automated Test Coverage**:
    - Added `TestSignalBroker_WebSocketKeepalivePingPong` in `cmd/signal/broker_test.go`;
    - Added `TestAgent_KeepalivePingPump` in `pkg/agent/agent_test.go`;
    - Added `TestE2E_WebSocketDualKeepalive` in `test/keepalive_e2e_test.go`.

---

## [v1.6.0] - 2026-11-16

### Added
- **P2P Zero-Listener Single File & Directory Server (`-file` / `-dir`)**:
  - **Single File Direct Sharing (`-file` / `FILE_PATH` / `file`)**: Unlocks private, zero-relay, cross-WAN AirDrop / Wormhole direct transfer. Visitors download directly in their browser at line-rate P2P speed without cloud relays or file size limits.
  - **Pure In-Memory Architecture**: Custom `streamResponseWriter` adapts WebRTC DataChannel streams (`mux.Stream`) to Go's standard `http.ResponseWriter` in memory, strictly preserving the Agent's zero-listening-port "Pure Outbound" security philosophy.
  - **Native HTTP 206 Range Streaming**: Full support for `Range` and `Content-Range` headers via `net/http.ServeFile`, enabling smooth seeking for video/audio streaming and multi-part downloads.
  - **Smart Content-Disposition**:
    - Previewable media (MP4, WebM, MP3, PNG, JPG, PDF, TXT, etc.) defaults to `inline` for in-browser playback;
    - Binary archives (ZIP, ISO, TAR, EXE, BIN, etc.) defaults to `attachment` to immediately trigger native browser download prompts.
  - **Static Web & Directory Hosting (`-dir` / `DIR_PATH` / `dir`)**: Host static SPAs/documentation trees (`index.html`) or render clean read-only directory file listings.
  - **Strict Sandbox & Path Traversal Hardening**: Enforces lexical containment via `filepath.Rel` and symlink escape verification via `filepath.EvalSymlinks`, strictly blocking any `../` path traversal or symlink escape attempts.
  - **Three-Way Mutual Exclusivity**: Enforces strict exclusivity between `-target`, `-dir`, and `-file` with Fail-Fast validation, defaulting safely to `http://127.0.0.1:8080` if none are specified.
  - **Seamless Stateless Sharing**: Fully integrated with HMAC-SHA256 `-sign-link` temporary links for secure, expiring P2P file downloads.

### Fixed
- **WebSocket Thread-Safety in E2E Test Suites**:
  - Wrapped `clientWS.WriteJSON` calls in `test/secret_auth_test.go`, `test/mvp_e2e_test.go`, `test/large_file_e2e_test.go`, and `test/advanced_e2e_scenarios_test.go` with `sync.Mutex`, eliminating sporadic concurrent write panics caused by asynchronous ICE candidate emission during heavy test concurrency.

---

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
