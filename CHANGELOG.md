# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

Pre-release versions (tags containing `-`, such as `v5.0.0-alpha.3`) are published as GitHub pre-releases and do not move the `:latest` container image tag.

## [Unreleased]

## [v5.0.0-alpha.4] - 2026-11-28

> **Pre-release (P2PMux V8 audit hardening).** Moves `:v5.0.0-alpha.4`, `:5.0.0-alpha.4`, and `:alpha`; `:latest` is not moved.

### Changed
- **P2PMux V8 Unified Data Plane**: Replaces the V7 `1 Ctrl` + `32` HTTP pool layout with 1 fragmented control channel (`id=0` `p2pctrl`) + 12 ordered multiplexed channels (`id=1..12` `p2p_01..p2p_12`) carrying HTTP and streaming WebSocket under monotonic `StreamID`s (zero dynamic DCEP channels); 8-byte fixed frame header, payload-byte `CREDIT` flow control, native Transferable `ReadableStream` IPC, and streaming gzip via `compress/gzip` / `DecompressionStream('gzip')`. Per-stream memory budgets (`memCost` + chunk cap), immutable HTTP/WS stream kind after `HEADERS`, a 10 s deadline for incomplete `HEADERS`, `MaxWSMessageChunks` (16384), and TS `PoolStream` 2 MiB / 32 MiB inbound and `WSStream` 16 MiB outbound caps are enforced.
- **Startup-Explicit Origins**: `cmd/signal` requires explicit `-portal-origin` + `-sandbox-origin` (loopback requires explicit `-dev`), with Tier 1 cross-site wildcard and Tier 2 restricted isolation (fixed single-domain cross-site or `-allow-same-site-sandbox`); the Portal shows a Tier 2 warning banner.
- **Service Worker Navigation & Static Gate**: Single-use `P2P_EXPECT_NAV` / `P2P_NAV_SETTLED` fallback for Firefox and Safari reload/Back/Forward; recovery from SW eviction; `-dir` / `-file` static content is served under a script-free `sandbox allow-same-origin` CSP with no SW bypass path; the legacy `data-p2p-boot` fallback is removed.
- **Unified Cookie Mirror**: `CookieMirrorStore` and Go `ProxyCookieJar` share `Secure` / `__Host-` / `__Secure-` prefix rules; TLV cookie op byte packs `Secure` as bit 7 (`0x80`) and `FetchCtx` bits are compacted (bit 0 `SameOriginClient`, bit 1 `ClientDecompressGzip`, bit 2 `CredentialsOmit`); `HEADERS` carrying both `FlagMore` and `FlagFin` is rejected. Go/TS golden vectors updated.
- **Signaling & Agent Hardening**: Per-node lockout scoped to configured node IDs; global saturation kill-switch disabled for machine secrets of at least 128 bits; IPv6 single-probe sprays no longer evict unseen visitors from the rate-limit table; TLS certificate hot-reload verifies inode identity (`os.SameFile`); `HEALTH_URL` and agent exit-code-2 healthcheck fallback; RFC 6455 WebSocket subprotocol validation; `CAP_RESP` revoke schema (`jti` + boolean `revoked`).

### Added
- `-revoke-cap` Agent CLI flag to revoke a capability token by JTI in the persistent state file and exit.
- Zero-dependency pure-JS RFC 8032 Ed25519 verification fallback for browsers without WebCrypto Ed25519 support, plus a clear `NotSupportedError` diagnostic.
- mDNS zero-candidate diagnostic warning and a warning when embedded web assets are missing.

### Fixed
- `Mux.Close` vs `HandleLaneMessage` permanent stream/budget leak (`ErrStreamLimitReached`, bounded `Close()` backpressure wait); WebSocket `WS_CLOSE` mid-message truncation; silent HTTP 200 truncation on mid-stream upstream errors in the decompression pipeline; `ProxyCookieJar.ExportNonHttpOnlySnapshot` data race; `CapabilityStore.Track` now persists atomically before memory commit; the 512 MiB decompression hard cap is removed.

## [v5.0.0-alpha.3] - 2026-11-27

> **Pre-release (WebRTC Gateway V7 Architecture).** Moves `:v5.0.0-alpha.3`, `:5.0.0-alpha.3`, and `:alpha`; `:latest` is not moved.

### Changed
- **P2PMux V7 Data Plane & Streaming WebSocket**: Single `RTCPeerConnection` with `1 Ctrl (id=0)` + `32` pre-negotiated reusable ordered HTTP pool channels (`id=1..32`, 5-state single-slot FSM `IDLE -> RECEIVING_HEADERS -> OPEN -> HALF_CLOSED -> CLOSED`, monotonic `uint32 Gen`, no same-slot preemption), event-driven `OnBufferedAmountLow` backpressure, `len(payload) + 64` overhead-aware memory accounting, native binary TLV fields (`FetchCtx`, `RespFlags`, inline cookie mutations/deltas), and 16 KiB fixed-buffer streaming `WSProxy` preserving valid RFC 6455 close codes.
- **Symmetric 3-State Auth, RFC 9383 SPAKE2+ & Silent Reconnect**: Structured `SessionDescriptor` signaling synthesized into deterministic 16-line SDPs; persistent Agent Ed25519 transcript signing (`p2pgw-v7:answer-v2:` / `p2pgw-v7:register-v2:`) bound to the live DTLS certificate fingerprint; symmetric `WAIT_CHALLENGE -> WAIT_RESULT -> AUTHENTICATED` handshake gating all non-control DataChannel frames; single-block 300,000-round PBKDF2-HMAC-SHA256 + HKDF-SHA256 SPAKE2+ (P-256) mutual verification; silent background `RTCPeerConnection` rebuild on disconnect with `cookie_resume_token` continuity keeping `appFrame` mounted; and live-revocable Ed25519 capability tokens.
- **Three-Tier Cross-Site Sandbox & W3C Fetch Anti-CSRF Gate**: Single-binary `cmd/signal` embedding content-hashed web assets verified against `manifest.json`; Tier 1 dual fixed domains (`-portal-domain` + `-sandbox-host`), Tier 2 single-domain wildcard (`-allow-same-site-sandbox` + `Origin-Agent-Cluster: ?1`), and Tier 3 dual-domain wildcard (`-portal-domain` + `-sandbox-domain`, per-browser salted `sbx-<24hex>` subdomains, automatic `*.localhost` locally); HTML `sandbox` attribute on `appFrame`; Service Worker W3C Fetch anti-CSRF gate with one-time `enterNonce` and `frame-ancestors`; `<meta charset>` 1024-byte prescan-safe HTML5 head injection; and PWA tunnel-SW self-unregistration shield.
- **Split-Pool `ProxyCookieJar`, Pure Pipe Proxy & Stepwise `os.Root`**: Authoritative `serverCookies` and RFC 6265 `cookie-octet`-validated `clientCookies` pools with single-path causal TLV sync; exact single-target `DestinationPolicy` with optional `-upstream-host` override, WS 302 redirect blocking, and post-resolve capability checks; stepwise `os.Root.OpenRoot` static handler blocking cross-root symlinks and dotfiles while applying `CSP: sandbox` to all served files by default.
- **Atomic Persistence, Hardened Containers & Verification Gate**: Shared `pkg/state` single-writer atomic file store (`0600` temp+fsync+rename) for `CapabilityStore` and `SignalBroker`; fail-closed rate limiters on table saturation; `-allow-force-rekey=false` default under shared cluster secrets; hardened `Dockerfile.server` / `Dockerfile.agent` / `docker-compose.yml` preserving Fail-Closed defaults and YAML `-config` precedence; unified Go/TS/CDP test suites, `gosec`, Go/Node fuzzing, and bidirectional bilingual documentation linting.
