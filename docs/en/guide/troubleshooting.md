# Troubleshooting & FAQ

---

## 1. Troubleshooting Handbook

### ① Browser Stuck on "Connecting to Agent" or ICE Failure
- **Root Cause**: Both peers are behind **Symmetric NATs (NAT 4)**, preventing direct STUN reflex candidate punching;
- **Diagnosis & Resolution**:
  1. Click the WebRTC HUD diagnostic badge in the bottom-right corner to review gathered ICE candidates;
  2. If both sides gather symmetric host/server-reflexive candidates without forming a valid pair, configure a TURN relay:
     ```bash
     ./agent -node-id myhome \
             -target http://127.0.0.1:8080 \
             -turn-url turn:relay.yourdomain.com:3478 \
             -turn-user your_username \
             -turn-pass your_password
     ```
  3. With TURN enabled, the gateway automatically falls back to `relay` candidates, and the HUD pill switches to `🛡️ TURN Relay`.

---

### ② Service Worker Fails to Register on LAN IP / Blank Page
- **Root Cause**: W3C Secure Context specification mandates that **Service Workers only register on `localhost` or over valid `https://` origins**. Accessing `http://192.168.1.100:8888` over plain HTTP blocks Service Worker execution;
- **One-Command Solution**:
  Start the signaling server with the `-tls-auto` flag:
  ```bash
  ./signal -addr :8443 -tls-auto
  ```
  The server dynamically generates an in-memory ECDSA SAN certificate covering local loopback and all detected physical LAN IP addresses. Open `https://<LAN-IP>:8443/#myhome`, accept the browser certificate warning, and the Service Worker activates immediately!

---

### ③ Connection Drops When Mobile Devices Switch Wi-Fi and 5G Cellular
- **Self-Healing Pipeline**:
  The system incorporates a **Dual-Phase ICE Restart Pipeline**:
  1. The momentary network drop terminates the signaling WebSocket, but the browser retains the existing `RTCPeerConnection` instance;
  2. The browser automatically reconnects to signaling and dispatches a new SDP Offer with `iceRestart: true`;
  3. The Agent applies a 5-second grace window, delaying teardown and seamlessly continuing data streams across the new IP route;
- **Manual Self-Healing**: If automatic roaming recovery fails under aggressive carrier firewall timeouts, click the **`🔄 Trigger ICE Restart`** button inside the HUD diagnostics card.

---

### ④ Docker Agent Cannot Reach Host Application at `http://127.0.0.1:8080`
- **Root Cause**: By default, Docker containers run inside an isolated bridge network namespace; `127.0.0.1` refers to the container itself rather than the host machine;
- **Recommended Solutions**:
  - **Linux Host**: Add `network_mode: "host"` (or `--net=host`) in `docker-compose.yml` or `docker run`;
  - **Windows / macOS Docker Desktop**: Set `-target` to `http://host.docker.internal:8080`.

---

### ⑤ Error: 401 Unauthorized or "Signaling Cluster Admission Rejected"
- **Root Cause**: Missing or mismatched secrets between control plane and data plane;
- **Checklist**:
  1. **Control Plane Cluster Secret**: Verify that `-signal-secret` configured on the signaling server matches `-signal-secret` passed to the Agent;
  2. **Data Plane Visitor Password**: If the Agent has `-access-secret` set, visitors must enter the password in the modal, or access via a signed link (`#nodeId?exp=xxx&sign=yyy`);
  3. **Configuration Notice**: Ensure you use `-signal-secret` for signaling cluster authentication and `-access-secret` for visitor passwords. Legacy `-secret` aliases are no longer supported.

---

### ⑥ Browser Console Error: WebSocket Connection Failed (403 Forbidden) or CSWSH Block
- **Root Cause**: The gateway enforces strict Cross-Site WebSocket Hijacking (CSWSH) protection by default (`Strict Same-Origin Only`). When the browser Web portal frontend origin (`Origin` header, such as `https://portal.yourdomain.com`, `https://*.pages.dev`, or a local development server `http://localhost:5173`) does not match the signaling server origin, the cross-origin WebSocket handshake is rejected by the server with `HTTP 403 Forbidden`;
- **Diagnosis & Resolution**:
  1. **Configure Allowed Origins Whitelist (Recommended for Production)**:
     Add the `-allowed-origins` CLI flag or set `SIGNAL_ALLOWED_ORIGINS` on the signaling server (supports comma-separated values, wildcard subdomains `*.pages.dev`, and port wildcards `localhost:*`, with decoupled domain/port matching):
     ```bash
     ./bin/signal_linux_amd64 -addr :443 \
       -allowed-origins "https://*.pages.dev,https://portal.yourdomain.com,http://localhost:*"
     ```
  2. **Temporarily Permit All Cross-Origin Handshakes (Dev/Test Environments Only)**:
     In local development or isolated test environments, pass `--allow-all-origins` (or set `SIGNAL_ALLOW_ALL_ORIGINS=true` / `ALLOW_ALL_ORIGINS=true`) to disable origin checks;
  3. **CLI Agent Verification**: Native CLI clients (Go CLI Agent, curl) do not send browser `Origin` headers and are never blocked by origin filters; they authenticate directly via `SIGNAL_SECRET` and HMAC signatures.

---

## 2. Frequently Asked Questions (FAQ)

### Q: Do visitors need to install any software or plugins on their phones or computers?
**A:** Not at all. Visitors only need a standard modern web browser (such as Chrome, Edge, Safari, or Firefox). Simply open the generated Web portal address or shared link to access intranet services directly. No client application, no VPN configuration, and no browser extensions are required on the visitor side, providing true zero-client access.

### Q: Do I need a public IP address or router port forwarding (DDNS) on the host machine?
**A:** Not at all. The system leverages the standard WebRTC peer-to-peer (P2P) protocol to automatically perform NAT traversal and establish direct communication channels between devices. Even if your home broadband operates behind multi-layer carrier-grade NAT (CGNAT) without a public IPv4 address, or if you cannot modify router settings in a rental apartment, devices can punch through and connect directly as long as a lightweight signaling service brokers initial handshake coordinates.

### Q: Is data transmission secure? Can cloud servers or third parties spy on my private data?
**A:** It provides high-grade security through layered cryptographic mechanisms:
1. **Mandatory Transport Layer Encryption**: All data transmitted over WebRTC is end-to-end encrypted by default using DTLS 1.2 and SRTP, preventing any third-party eavesdropping;
2. **Direct P2P Routing**: After the initial handshake, data flows directly between the two peer devices. The public signaling server neither inspects nor routes business traffic; even in rare cases where TURN relay fallback is active, the relay server merely forwards encrypted packets and cannot decrypt the payload;
3. **Strict Authentication & Anti-Replay**: The platform supports visitor password gates and temporary signed links bound to HMAC-SHA256 signatures and visitor device certificates, preventing unauthorized cross-device link misuse.

### Q: If I download large files or movies from my NAS, will it consume my cloud server bandwidth or data traffic?
**A:** In the vast majority of network environments, it consumes zero server bandwidth:
- Once a direct P2P connection is established, traffic travels entirely across the physical broadband connection between the two devices. Transmission speed is determined solely by your local upload and download capabilities, **consuming 0 bytes of relay server traffic**;
- Only when both peers are restricted by strict enterprise firewalls or symmetric NATs that prevent direct punching will traffic route through a TURN relay, in which case relay server bandwidth is used.

### Q: Does it work reliably on Apple devices (iPhone / iPad / Mac Safari) and Android phones?
**A:** Yes, it is fully compatible. As long as the device runs a modern browser meeting standard specifications (such as Safari on iOS 14.3+ or mainstream browsers on Android 8.0+), it opens seamlessly. Mobile devices support complete touch interactions, smooth audio/video streaming, context menus, and direct local file saving.

### Q: Will the connection disconnect when my phone switches between home Wi-Fi and 5G cellular on the go?
**A:** No, it will not disconnect. The gateway includes an automatic network self-healing mechanism (ICE Restart). When the phone detects an underlying network handover from Wi-Fi to cellular (or between different Wi-Fi access points), it automatically renegotiates the connection path within a few seconds without reloading the page or tearing down active video playback, allowing you to continue uninterrupted.

### Q: Can I connect to multiple computers or distinct services (such as NAS, Jellyfin, photo library) at home simultaneously?
**A:** Yes, management is flexible:
- **Multiple Independent Nodes**: You can run multiple Agents on different machines or the same host with unique node names (such as `nas`, `jellyfin`, or `photos`);
- **Subdomain Origin Isolation (Recommended)**: Using wildcard domain routing (such as `nas.p2p.yourdomain.com` and `jellyfin.p2p.yourdomain.com`), the browser assigns distinct physical origins to each service, keeping login cookies and local storage completely isolated without credential collisions;
- **Single-Domain Access**: You can also use the unified portal with node identifiers (such as `https://p2p.yourdomain.com/#nas`) for instant setup.

### Q: How do I safely share a large file from my home computer with a friend temporarily?
**A:** It is quick and straightforward, eliminating the need to pre-upload files to third-party cloud drives:
1. Start the Agent in single-file mode on your computer, specifying the desired validity window (e.g. 24 hours):
   ```bash
   ./agent -file ./family-video.mp4 -access-secret mypass -sign-link 24h
   ```
2. The terminal immediately outputs a time-limited signed URL;
3. Send the link to your friend. Opening it in any desktop or mobile browser downloads the file directly from your computer over P2P streaming at maximum available speed (with resume support). The link expires automatically after 24 hours without manual cleanup.

### Q: What is the floating pill on the bottom-right of the screen, and how can I move or hide it?
**A:** The Smart Pill is a lightweight companion widget showing link quality, latency, and reconnection controls. If it covers page content:
- **One-Click Collapse**: Tap the collapse button to shrink it into an unobtrusive 28px semi-transparent breathing dot;
- **Free Dragging & Magnetic Edge Snapping**: Drag it anywhere using touch or mouse; releasing it snaps it naturally to the nearest screen edge;
- **Zero Keyboard Interception**: The pill does not register global keyboard listeners. When typing in web terminals (such as Web SSH), code editors, or complex forms, all key shortcuts pass through directly to your application.

### Q: Why is the page stuck on "Connecting" or failing to connect? How should an everyday user troubleshoot?
**A:** If you experience connection issues, follow this simple checklist:
1. **Verify Host Status**: Ensure the target computer or NAS is powered on, awake (not in sleep mode), connected to the network, and the Agent process is active;
2. **Check Password & Expiration**: If the node is password-protected, verify the password entered in the prompt. If using a shared link, check whether the expiration window has passed;
3. **Verify Restrictive Network Environments**: If accessing from a locked-down corporate network, direct peer-to-peer punching may be blocked by strict firewalls. Configuring a free TURN relay server on the Agent or server resolves this;
4. **Trigger Manual Reconnect**: Click the floating Smart Pill in the bottom-right corner and tap "Reconnect" or "Trigger ICE Restart" to re-establish the connection.

### Q: Is there a graphical desktop client (GUI) available?
**A:** The core project focuses on lightweight headless command-line daemons (Agent CLI) and zero-client browser Web portals. For desktop interface needs, an experimental prototype is available at [WebRTC-Gateway-GUI](https://github.com/BeingNoAbility/WebRTC-Gateway-GUI). Note that this GUI is an independent, rapidly assembled prototype that is untested and outside the core management and support scope of this project.
