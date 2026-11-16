# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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
