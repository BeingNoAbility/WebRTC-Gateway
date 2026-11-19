# Documentation Overview & Quickstart

Welcome to the **Zero-Client P2P WebRTC Intranet Penetration Gateway** documentation and technical knowledge center!

---

## Core Philosophy & Vision

This project introduces a zero-client browser-based WebRTC penetration architecture:

- **Zero-Client in Visitor Browsers**: Visitors do not need to install software, install browser extensions, configure root CA certificates, or configure virtual network drivers.
- **Direct P2P Traversal**: Powered by WebRTC ICE/STUN NAT traversal. Traffic is routed directly between visitor browsers and private servers without consuming relay bandwidth.
- **End-to-End Transport Encryption**: Transport layer enforces standard DTLS 1.2 / SRTP transport encryption (RFC 8831). The public signaling server only negotiates topologies and cannot inspect user data.
- **Full Transparent Web Protocol Stack**: HTTP/HTTPS, HTTP 206 Partial Content (video streaming seeking), WebSocket full-duplex, CookieJar isolation, and W3C upload progress virtualization out of the box.

---

## Topics Navigation

| Topic | Overview | Audience |
| :--- | :--- | :--- |
| **[Architecture & Design](./architecture)** | P2PMux v1 binary framing protocol, Service Worker interception, DOM virtualization monkey patches, and isolated CookieJars. | Architects & Developers |
| **[P2PMux Wire Protocol Specification](./protocol-specification)** | 10-byte fixed header, stream parity invariants, state machines, 32KB runtime memory alignment, and SCTP backpressure. | Protocol Implementers & Systems Engineers |
| **[Production Deployment](./deployment)** | Cloudflare Workers edge deployment, single self-contained Go binaries, and Docker / Docker Compose orchestration. | DevOps & Self-Hosters |
| **[Agent Configuration](./agent-configuration)** | Complete CLI flags, environment variables, YAML configuration templates, and priority resolution. | All Users & NAS Admins |
| **[P2P File & Directory Server](./file-server)** | Zero-port in-memory file sharing, private lightweight file delivery, and static directory hosting. | All Users & Collaborators |
| **[Security & Access Control](./security)** | Dual-secret decoupling, HMAC-SHA256 expiring links, and authoritative client IP propagation. | Security & System Admins |
| **[Browser Compatibility](./browser-compatibility)** | Tier 1 (Chrome, Edge, Firefox) & Tier 2 (Safari, WebKit) support details, background hibernation, and mobile recovery. | Frontend Engineers |
| **[Troubleshooting & FAQ](./troubleshooting)** | Symmetric NAT diagnosis, TURN relay setup, self-signed HTTPS certificates, and roaming ICE Restart. | Troubleshooting & Support |

---

## 30-Second Quickstart

::: code-group

```bash [Public Signal Server (VPS)]
# Launch standalone binary with embedded web assets
./signal -addr :8888
```

```bash [Intranet Client (Agent)]
# Reverse proxy internal web application (Mode A)
./agent -node-id myhome -signal ws://your-server-ip:8888/ws -target http://127.0.0.1:8080

# Or distribute an ISO/file in 1 second (Mode C)
./agent -node-id share -signal ws://your-server-ip:8888/ws -file ./Ubuntu.iso -access-secret pass123 -sign-link 24h
```

```text [Visitor Browser]
Open in any modern desktop or mobile browser:
👉 http://your-server-ip:8888/#myhome

Enjoy peer-to-peer end-to-end direct connection immediately!
```

:::
