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
    title: P2PMux V8 Unified Multiplexed Data Plane
    details: One PeerConnection pre-negotiates 1 chunked control channel and 12 ordered multiplexed lanes carrying both HTTP and streaming WebSocket by monotonic StreamID over an 8-byte header and payload-byte CREDIT flow control.
  - icon: 🔒
    title: Ed25519 Identity and DTLS SPAKE2+
    details: Structured descriptors synthesize deterministic SDPs signed by persistent Agent Ed25519 keys with live DTLS cert verification and RFC 9383 SPAKE2+ mutual auth inside DTLS.
  - icon: 🧩
    title: Cross-Site Sandbox and CookieMirror
    details: Portal and proxied apps run on explicit startup origins (local -dev uses *.localhost), bridging streams via native Transferable ReadableStream alongside CookieMirror and Agent HttpOnly ProxyCookieJar.
  - icon: 📂
    title: Read-Only O_NOFOLLOW Static Adapter
    details: Serve local directories or single files via -dir / -file backed by Go os.Root with default Fail-Closed O_NOFOLLOW rejecting symlinks and dotfiles, CSP sandbox on all files, and revocable Ed25519 share tokens.
  - icon: 🛡️
    title: Least-Privilege Go Signaling Server
    details: Containers run non-root (UID 10001) on read-only root filesystems; the single authoritative Go signal binary embeds web assets with persistent node key bindings and per-node secrets.
---
