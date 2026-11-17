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
      text: Get Started 🚀
      link: /en/guide/
    - theme: alt
      text: Architecture 📐
      link: /en/guide/architecture
    - theme: alt
      text: GitHub Repo ⭐️
      link: https://github.com/BeingNoAbility/WebRTC-Gateway

features:
  - icon: 🌐
    title: 100% Zero-Client Experience
    details: Visitors do not need to install applications, privileged network drivers, or browser extensions. Open the portal URL in any browser to connect immediately.
  - icon: ⚡
    title: True P2P Direct Line Speed
    details: Powered by WebRTC STUN/ICE NAT traversal. Traffic flows directly between visitor browsers and private servers, consuming zero public relay bandwidth.
  - icon: 🔒
    title: End-to-End Native Encryption
    details: Enforces DTLS-SRTP transport-layer physical encryption. Signaling servers only negotiate network topologies and cannot decrypt application data.
  - icon: 🧩
    title: Full Transparent Web Stack
    details: Features P2PMux binary multiplexing, HTTP/HTTPS, HTTP 206 Partial Content (media streaming), WebSocket full-duplex, and session-isolated CookieJars.
  - icon: 📂
    title: In-Memory Fast File Distribution
    details: Built-in zero-port-listening file and directory server. Distribute ISO/media files in 1 second with stateless HMAC-SHA256 time-limited links.
  - icon: 🛡️
    title: Enterprise Sandbox & Non-Root
    details: Container images run as unprivileged user 10001:10001 by default, fully supporting --user $(id -u):$(id -g) and read-only root filesystems.
---
