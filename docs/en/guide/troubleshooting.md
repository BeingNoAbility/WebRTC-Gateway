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

## 2. Frequently Asked Questions (FAQ)

### Q: Can this gateway saturate gigabit internet connections?
**A:** Yes! Once direct punching succeeds (HUD displays `⚡ P2P Direct`), UDP traffic flows directly between peers via WebRTC. Transfer speeds are limited only by the sender's upstream and receiver's downstream physical bandwidth, with zero relay bottleneck.

### Q: Does my intranet application need code modifications?
**A:** None at all! The browser intercept layer uses a Service Worker to capture HTTP/WebSocket requests transparently. The Agent unpacks binary streams and reverse-proxies them locally as standard requests. Your backend service observes standard HTTP requests as if they originated locally from `127.0.0.1`.

### Q: Can I share large files (e.g. 20GB disk images) without cloud drives?
**A:** Absolutely. A single command suffices:
```bash
./agent -file ./large-game.zip -access-secret pass123 -sign-link 24h
```
Share the generated URL with your recipient; their browser downloads the file directly over P2P at full wire speed.

### Q: Why do some intranet dashboards fail to load inside iframe tabs?
**A:** Many administration consoles include `Anti-Frame-Busting` scripts. The gateway's `patch.js` virtualizes `window.top` and `window.parent` references. For stubborn applications, open the dashboard in a new browser tab; the Service Worker preserves node routing via `?peer=${nodeId}#${nodeId}`.
