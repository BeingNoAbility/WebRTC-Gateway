# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

Pre-release versions (tags containing `-`, such as `v5.0.0-alpha.3`) are published as GitHub pre-releases and do not move the `:latest` container image tag.

## [Unreleased]

## [v5.0.0-alpha.3] - 2026-11-27

> **Pre-release (WebRTC Gateway V7 Architecture).** Moves `:v5.0.0-alpha.3`, `:5.0.0-alpha.3`, and `:alpha`; `:latest` is not moved.

### Changed
- **P2PMux V7 Data Plane & Streaming WebSocket**: Single `RTCPeerConnection` with `1 Ctrl (id=0)` + `32` pre-negotiated reusable ordered HTTP pool channels (`id=1..32`, 5-state single-slot FSM `IDLE -> RECEIVING_HEADERS -> OPEN -> HALF_CLOSED -> CLOSED`, monotonic `uint32 Gen`, no same-slot preemption), event-driven `OnBufferedAmountLow` backpressure, `len(payload) + 64` overhead-aware memory accounting, native binary TLV fields (`FetchCtx`, `RespFlags`, inline cookie mutations/deltas), and 16 KiB fixed-buffer streaming `WSProxy` preserving valid RFC 6455 close codes.
- **Symmetric 3-State Auth, RFC 9383 SPAKE2+ & Silent Reconnect**: Structured `SessionDescriptor` signaling synthesized into deterministic 16-line SDPs; persistent Agent Ed25519 transcript signing (`p2pgw-v7:answer-v2:` / `p2pgw-v7:register-v2:`) bound to the live DTLS certificate fingerprint; symmetric `WAIT_CHALLENGE -> WAIT_RESULT -> AUTHENTICATED` handshake gating all non-control DataChannel frames; single-block 300,000-round PBKDF2-HMAC-SHA256 + HKDF-SHA256 SPAKE2+ (P-256) mutual verification; silent background `RTCPeerConnection` rebuild on disconnect with `cookie_resume_token` continuity keeping `appFrame` mounted; and live-revocable Ed25519 capability tokens.
- **Three-Tier Cross-Site Sandbox & W3C Fetch Anti-CSRF Gate**: Single-binary `cmd/signal` embedding content-hashed web assets verified against `manifest.json`; Tier 1 dual fixed domains (`-portal-domain` + `-sandbox-host`), Tier 2 single-domain wildcard (`-allow-same-site-sandbox` + `Origin-Agent-Cluster: ?1`), and Tier 3 dual-domain wildcard (`-portal-domain` + `-sandbox-domain`, per-browser salted `sbx-<24hex>` subdomains, automatic `*.localhost` locally); HTML `sandbox` attribute on `appFrame`; Service Worker W3C Fetch anti-CSRF gate with one-time `enterNonce` and `frame-ancestors`; `<meta charset>` 1024-byte prescan-safe HTML5 head injection; and PWA tunnel-SW self-unregistration shield.
- **Split-Pool `ProxyCookieJar`, Pure Pipe Proxy & Stepwise `os.Root`**: Authoritative `serverCookies` and RFC 6265 `cookie-octet`-validated `clientCookies` pools with single-path causal TLV sync; exact single-target `DestinationPolicy` with optional `-upstream-host` override, WS 302 redirect blocking, and post-resolve capability checks; stepwise `os.Root.OpenRoot` static handler blocking cross-root symlinks and dotfiles while applying `CSP: sandbox` to all served files by default.
- **Atomic Persistence, Hardened Containers & Verification Gate**: Shared `pkg/state` single-writer atomic file store (`0600` temp+fsync+rename) for `CapabilityStore` and `SignalBroker`; fail-closed rate limiters on table saturation; `-allow-force-rekey=false` default under shared cluster secrets; hardened `Dockerfile.server` / `Dockerfile.agent` / `docker-compose.yml` preserving Fail-Closed defaults and YAML `-config` precedence; unified Go/TS/CDP test suites, `gosec`, Go/Node fuzzing, and bidirectional bilingual documentation linting.
