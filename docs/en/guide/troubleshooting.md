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
  Starting with **v1.4.0**, the system incorporates a **Dual-Phase ICE Restart Pipeline**:
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
- **Root Cause**: Secret mismatch between control plane and data plane;
- **Checklist**:
  1. **Control Plane Cluster Secret**: Verify that `-signal-secret` configured on the signaling server matches `-signal-secret` passed to the Agent;
  2. **Data Plane Visitor Password**: If the Agent has `-access-secret` set, visitors must enter the password in the modal, or access via a signed link (`#nodeId?exp=xxx&sign=yyy`);
  3. **Deprecation Notice**: The legacy `-secret` flag alias has been completely removed. Ensure you use `-signal-secret` for signaling cluster authentication and `-access-secret` for visitor passwords.

---

### ⑥ Browser Console Error: WebSocket Connection Failed (403 Forbidden) or CSWSH Block
- **Root Cause**: Starting with **v2.3**, the gateway enforces strict Cross-Site WebSocket Hijacking (CSWSH) protection by default (`Strict Same-Origin Only`). When the browser Web portal frontend origin (`Origin` header, such as `https://portal.yourdomain.com`, `https://*.pages.dev`, or a local development server `http://localhost:5173`) does not match the signaling server origin, the cross-origin WebSocket handshake is rejected by the server with `HTTP 403 Forbidden`;
- **Diagnosis & Resolution**:
  1. **Configure Allowed Origins Whitelist (Recommended for Production)**:
     Add the `-allowed-origins` CLI flag or set `SIGNAL_ALLOWED_ORIGINS` on the signaling server (supports comma-separated values, wildcard subdomains `*.pages.dev`, and port wildcards `localhost:*`, with decoupled domain/port matching in v2.3.1):
     ```bash
     ./bin/signal_linux_amd64 -addr :443 \
       -allowed-origins "https://*.pages.dev,https://portal.yourdomain.com,http://localhost:*"
     ```
  2. **Temporarily Permit All Cross-Origin Handshakes (Dev/Test Environments Only)**:
     In local development or isolated test environments, pass `--allow-all-origins` (or set `SIGNAL_ALLOW_ALL_ORIGINS=true` / `ALLOW_ALL_ORIGINS=true`) to disable origin checks;
  3. **CLI Agent Verification**: Native CLI clients (Go CLI Agent, curl) do not send browser `Origin` headers and are never blocked by origin filters; they authenticate directly via `SIGNAL_SECRET` and HMAC signatures.

---

## 2. Frequently Asked Questions (FAQ)

### Q: What is the network throughput in direct P2P mode?
**A:** Once WebRTC punching establishes a direct P2P connection, data flows peer-to-peer over user-space UDP (DTLS 1.2 / SRTP, RFC 8831), bypassing all public cloud relay bandwidth bottlenecks. Actual throughput is bounded by physical link bandwidth, round-trip time (RTT), SCTP congestion control mechanics, and single-core DTLS crypto compute limits. In gigabit LAN and high-speed broadband environments, it fully utilizes available physical uplink. Official benchmark suites (1000 concurrent streams) demonstrate scheduling throughput reaching 2,307.9 streams/s with bounded linear memory overhead.

### Q: How does the v2.3 Native Dual-Channel Architecture (`p2pcontrol` + `p2pdata`) eliminate Head-of-Line (HoL) blocking during bulk transfers?
**A:** Under a single-channel design, transmitting multi-gigabyte files or handling high-volume chunked streams saturates the underlying SCTP buffer, triggering transport-level backpressure. Consequently, crucial control frames like `HEADERS` (initiating requests), `PING`/`PONG` (keepalive), and `RST_STREAM` (cancellation) are queued behind bulk data, creating severe Head-of-Line (HoL) latency spikes.
Starting in **v2.3**, the system establishes two decoupled WebRTC DataChannels during handshake:
- **`p2pcontrol`**: Dedicated exclusively to `HEADERS`, `PING`, `PONG`, `RST_STREAM`, and `WINDOW_UPDATE`, physically bypassing data backpressure queues to maintain sub-10ms latency even during link saturation;
- **`p2pdata`**: Exclusively carries `DATA` and `WS_DATA` payloads under strict governance (128MB global memory ceiling, 96MB/64MB dual-watermark backpressure);
- **Orphan Frame Reconciliation**: To handle out-of-order parallel SCTP stream arrivals (`DATA` arriving before `HEADERS`), the multiplexer provisions an orphan buffer pool (64KB per stream, 256KB global cap, 500ms TTL) that flushes frames in order upon `HEADERS` arrival or reclaims stalled streams via `RST_STREAM`, achieving zero data loss and lock-free concurrency.

### Q: Will downloading multi-gigabyte files in the Web portal cause browser Out-Of-Memory (OOM) or tab crashes?
**A:** Not at all. Legacy web applications often trigger downloads via simulated `<a download="file" href="blob:...">` tags, forcing the browser to buffer the entire binary payload in the JavaScript V8 heap and triggering fatal tab crashes (Chromium Bug 40410035).
The gateway's built-in **P2P File Vault** solves this with an adaptive dual-mode persistence architecture:
1. **Stream-to-Disk (showSaveFilePicker)**: In modern browsers (Chromium 86+), files exceeding 500MB automatically trigger the native File System Access API `showSaveFilePicker`, piping chunks directly into the local filesystem via a `WritableStream`. Data frames unpacked by the Service Worker are flushed directly to disk, keeping JavaScript heap memory usage at a constant $O(1)$;
2. **Streaming Fetch Interception**: For smaller files or browsers without the File System API, streaming `fetch(viewUrl)` mediated by the Service Worker provides real-time speed calculation, progress tracking, and `AbortController` cancellation cleanup, eliminating memory buildup.

### Q: How does mandatory DTLS certificate fingerprint binding (F-04) fundamentally improve signed link (`-sign-link`) security in v2.3+?
**A:** Conventional tunneling tools rely on static tokens or bearer query parameters that remain vulnerable to cross-device replay if copied, intercepted in transit, or leaked in server logs.
Starting in **v2.3**, the gateway establishes a strict **cryptographic generational boundary**:
- During the WebRTC handshake, the visitor browser generates an ephemeral DTLS certificate and derives its SHA-256 fingerprint (`sdpFingerprint`);
- The authorization mechanism binds this `sdpFingerprint` into the HMAC-SHA256 signature computation alongside timestamp expiration;
- Even if an adversary intercepts the full signed URL, handshake attempts from any distinct device or untrusted DTLS certificate fail signature verification, defeating cross-device replay attacks.

### Q: How is network traffic routed between LAN peers versus WAN peers? Does it inadvertently consume public server bandwidth?
**A:** WebRTC strictly implements IETF RFC 8445 Interactive Connectivity Establishment (ICE) for candidate harvesting and connectivity verification (STUN Binding Requests):
1. **Candidate Gathering**: Both peers simultaneously gather `host` (physical LAN network card IP), `srflx` (STUN-discovered server-reflexive public IP), and `relay` (TURN relay IP) candidates;
2. **Local Peer Direct Routing (Zero Cloud Bandwidth)**: When the visitor and Agent reside on the same Wi-Fi network or LAN switch, ICE connectivity checks prioritize the `host <-> host` pair. Data frames travel directly across local network adapters with sub-millisecond latency (< 1ms), consuming zero public cloud relay bandwidth. The signaling server merely brokers a few kilobytes of SDP handshake text;
3. **WAN P2P Traversal**: Across the public Internet, ICE negotiates direct `srflx <-> srflx` peer-to-peer punching whenever either side is not behind a symmetric NAT. Relay traversal via TURN is activated only when both sides are restricted by strict symmetric NAT topologies.

### Q: Why does Smart Pill avoid keystroke conflicts or accidental triggers in web terminals (e.g. Web SSH, Vim, Code-Server, NoVNC)?
**A:** Smart Pill 2.1 incorporates dedicated interaction hygiene engineering for high-density terminal environments:
1. **Zero Keyboard Interception**: The Smart Pill DOM component registers zero global `keydown`, `keyup`, or `keypress` listeners and never steals input focus. Keystrokes such as `Ctrl+C`, `Esc`, Vim combinations, and terminal shortcuts pass through 100% untouched to the underlying container;
2. **Euclidean Distance Gesture Disambiguation**: PointerCapture state mechanics differentiate taps from drags: displacements exceeding 5 pixels are treated as intentional drags, while motions $\le 5\text{px}$ trigger console modal activation, eliminating jitter false positives;
3. **Breathing Dot Collapse & Magnetic Edge Snapping**: The UI easily collapses into an unobtrusive 28px breathing dot with automatic screen edge snapping, injecting `pointer-events: none` into background iframes during drag motions to prevent event swallowing.

### Q: Does my intranet application need code modifications? How is real visitor IP preserved?
**A:** No backend code modifications are required.
- **Transparent Interception**: In the browser, the Service Worker intercepts standard HTTP/HTTPS requests and WebSockets transparently. On the Agent side, demultiplexed binary frames are forwarded to the local target application as standard HTTP/1.1 and RFC 6455 connections;
- **Authoritative IP Injection**: To satisfy enterprise WAFs, SafeLine, and security audit logs, the signaling server authoritatively extracts the visitor's public transport IP during handshake, and the Agent overwrites standard `X-Forwarded-For` and `X-Real-IP` request headers. Upstream services (Nginx, Go, Node.js, Python) perform single-IP rate limiting and geographical audit tracking exactly as if exposed directly to the public Internet.

### Q: Can I share large files directly without hosting a cloud drive?
**A:** Yes. Using the Agent's built-in single file mode (`-file`), any large file (e.g. 20GB ISO or archive) can be served directly without third-party web servers:
```bash
./agent -file ./large-game.zip -access-secret pass123 -sign-link 24h
```
The generated time-limited signed link allows recipient browsers to download directly over P2P streaming with HTTP 206 Range resumption, consuming zero relay bandwidth and expiring automatically.

### Q: Why do some intranet dashboards fail to load inside iframe tabs? How do I choose between Subdomain and Hash routing?
**A:** Many administrative dashboards include `Anti-Frame-Busting` scripts (such as checking `window.top !== window.self` to break out or abort rendering).
- The gateway's `patch.js` virtualizes window hierarchy references (`window.top`, `window.parent`) to neutralize nested sandbox restrictions transparently;
- **Recommended Routing Topology**:
  - **Subdomain Routing (Recommended for Production)**: Configure wildcard DNS (`*.p2p.yourdomain.com`) so distinct services map to distinct subdomains (e.g. `jellyfin.p2p.yourdomain.com` and `vaultwarden.p2p.yourdomain.com`). The browser allocates an independent physical Origin to each application, establishing strict isolation for LocalStorage, IndexedDB, and Cookie jars with zero credential pollution;
  - **Single-Domain Hash Mode**: Access via `https://p2p.yourdomain.com/#nodeId`, which requires no wildcard DNS or SSL certificates and uses Service Worker virtualization for sandboxed multiplexing.

### Q: Why do external media or third-party CDN assets fail to load when proxying rich-media websites?
**A:** Modern rich-media web applications frequently reference third-party CDNs and cross-origin static assets (e.g. external image hosts or video slice CDNs):
- To prevent misuse as an unrestricted public open proxy (Open Proxy Abuse), the gateway establishes a defensive boundary on `/--p2p-proxy/` requests, proxying only same-origin or explicitly whitelisted targets;
- **Resolution**: In the Agent CLI parameters or configuration file, declare an `-allowed-proxy-domains` wildcard whitelist (for example, allowing Bilibili media CDNs):
  ```bash
  ./agent -node-id mybili -target http://127.0.0.1:8080 \
          -allowed-proxy-domains "*.hdslb.com,*.bilivideo.com,*.akamaized.net"
  ```
  Authorized external media requests will proxy seamlessly without opening the gateway to unrestricted proxy abuse.
