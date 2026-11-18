---
layout: home

hero:
  name: "WebRTC Gateway"
  text: "Zero-Client P2P WebRTC Intranet Gateway"
  tagline: "Zero client software, zero plugins, zero root CAs. Access private intranet web services, NAS, and cloud apps directly in any modern browser."
  image:
    src: /logo.svg
    alt: WebRTC Gateway Logo
  actions:
    - theme: brand
      text: Get Started
      link: /en/guide/
    - theme: alt
      text: Architecture
      link: /en/guide/architecture
    - theme: alt
      text: GitHub Repo
      link: https://github.com/BeingNoAbility/WebRTC-Gateway

features:
  - icon: 🌐
    title: Zero-Client Experience
    details: Visitors do not need to install applications, privileged network drivers, or browser extensions. Open the portal URL in any browser to connect directly.
  - icon: ⚡
    title: P2P Direct Traversal
    details: Powered by WebRTC STUN/ICE NAT traversal. Traffic flows directly between visitor browsers and private servers, without public relay bandwidth bottlenecks.
  - icon: 🔒
    title: End-to-End Transport Encryption
    details: Enforces standard DTLS 1.2 / SRTP transport encryption (RFC 8831). Signaling servers only negotiate network topologies and cannot decrypt application data.
  - icon: 🧩
    title: Full Transparent Web Stack
    details: Features P2PMux binary multiplexing, HTTP/HTTPS, HTTP 206 Partial Content (media streaming), WebSocket full-duplex, and session-isolated CookieJars.
  - icon: 📂
    title: In-Memory File Distribution
    details: Built-in zero-port-listening file and directory server. Distribute files via memory-mapped streaming with stateless HMAC-SHA256 time-limited links.
  - icon: 🛡️
    title: Unprivileged Container Sandbox
    details: Container images run as unprivileged user 10001:10001 by default, fully supporting --user $(id -u):$(id -g) and read-only root filesystems.
---
