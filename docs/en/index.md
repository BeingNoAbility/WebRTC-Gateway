---
layout: home

hero:
  name: "WebRTC Gateway"
  text: "Zero-Client Browser P2P Intranet Gateway"
  tagline: "Reach intranet web apps, NAS consoles, and private clouds directly from a stock browser over end-to-end DTLS without installing clients, extensions, or root certificates."
  image:
    src: /logo.svg
    alt: WebRTC Gateway Logo
  actions:
    - theme: brand
      text: Quickstart
      link: /en/guide/
    - theme: alt
      text: Architecture
      link: /en/guide/architecture
    - theme: alt
      text: GitHub
      link: https://github.com/BeingNoAbility/WebRTC-Gateway

features:
  - icon: 🌐
    title: Zero-Client Browser Access
    details: Visitors need no native app, browser extension, or custom root CA — open a Portal URL in a standard browser. Chrome and Edge are verified continuously in CI.
  - icon: ⚡
    title: P2PMux V7 DataChannel Pool
    details: One PeerConnection pre-negotiates 1 control channel, 32 reusable HTTP pool channels, and dynamic dedicated WebSocket channels over an 8-byte header and V7 binary TLV codec.
  - icon: 🔒
    title: Ed25519 Identity and DTLS SPAKE2+
    details: Structured descriptors synthesize deterministic SDPs signed by persistent Agent Ed25519 keys with live DTLS cert verification and RFC 9383 SPAKE2+ mutual auth inside DTLS.
  - icon: 🧩
    title: Cross-Site Sandbox and CookieMirror
    details: Portal and proxied apps run on separate sites (automatic *.localhost locally), syncing non-HttpOnly cookies via an in-memory CookieMirror alongside Agent HttpOnly ProxyCookieJar.
  - icon: 📂
    title: Read-Only os.Root Static Adapter
    details: Serve local directories or single files via -dir / -file backed by Go os.Root with default CSP sandbox on all static files, multi-hop symlink blocking, and revocable Ed25519 share tokens.
  - icon: 🛡️
    title: Least-Privilege Go Signaling Server
    details: Containers run non-root (UID 10001) on read-only root filesystems; the single authoritative Go signal binary embeds web assets with persistent node key bindings and per-node secrets.
---
