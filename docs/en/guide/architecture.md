# System Architecture & Technical Specifications

This document outlines the architecture, end-to-end data flow, and protocol specifications of the Zero-Client WebRTC P2P Gateway.

---

## 1. System Architecture & End-to-End Data Flow

```mermaid
sequenceDiagram
    autonumber
    actor Visitor as Visitor Browser (Chrome/Edge/Safari/Firefox)
    participant Portal as Web Console (HTML/DOM)
    participant SW as Service Worker (Virtualization Sandbox)
    participant Signal as Signaling Server / CF Worker (Public Broker)
    participant Agent as Intranet Agent Daemon (Private Server)
    participant Service as Private Web Application (127.0.0.1:8080)

    Note over Visitor,Agent: Phase 1: Signaling Handshake & NAT Traversal
    Visitor->>Portal: 1. Navigate to https://p2p.domain.com/#myhome
    Portal->>Signal: 2. Connect WebSocket & register peer session
    Agent->>Signal: 3. Long-lived WebSocket connection listening for nodes
    Signal-->>Agent: 4. Forward Visitor SDP Offer & ICE Candidates
    Agent-->>Signal: 5. Return Agent SDP Answer & Local Candidates
    Signal-->>Portal: 6. Relay SDP Answer to Browser

    Note over Portal,Agent: Phase 2: WebRTC P2P Direct Connection Established
    Portal->>Agent: 7. WebRTC STUN/ICE Punching -> Direct RTCDataChannel (DTLS-SRTP)
    Portal->>SW: 8. Attach Dedicated MessagePort to Service Worker (PORTAL_ATTACH)

    Note over Visitor,Service: Phase 3: Transparent Traffic Interception & Tunneling
    Visitor->>SW: 9. Fetch /api/data or load iframe /
    SW->>Portal: 10. Forward HTTP request over MessagePort
    Portal->>Agent: 11. Frame into P2PMux binary stream (HEADERS + DATA)
    Agent->>Service: 12. Unframe & reverse-proxy to local target (HTTP/WS)
    Service-->>Agent: 13. Return chunked HTTP response
    Agent-->>Portal: 14. Stream DATA frames back over WebRTC DataChannel
    Portal-->>SW: 15. Push chunked streams via ReadableStream
    SW-->>Visitor: 16. Browser renders response seamlessly (Zero relay traffic!)
```

---

## 2. P2PMux v1 Binary Multiplexing Protocol Specification

All communications across the WebRTC `RTCDataChannel` utilize the high-performance **P2PMux v1** binary framing protocol. It features minimal overhead, stream multiplexing, and window-based backpressure.

### Frame Layout (9-Byte Header)

```text
+----------------+----------------+----------------+--------------------------+
|  StreamID (4B) | Type (1B, 0-6) |  Length (4B)   |   Payload (0..N Bytes)   |
|  Big-Endian    |  Uint8 Enum    |  Big-Endian    |   Binary Slice           |
+----------------+----------------+----------------+--------------------------+
```

### Frame Types

| Type Code | Constant | Direction | Description |
| :---: | :--- | :--- | :--- |
| `0x01` | `DATA` | Bidirectional | Carries raw binary payload chunks. |
| `0x02` | `HEADERS` | Bidirectional | Initial HTTP request/response metadata serialized as JSON. |
| `0x03` | `RST_STREAM` | Bidirectional | Terminates or aborts an active stream immediately. |
| `0x04` | `WINDOW_UPDATE` | Bidirectional | Credit-based flow control / backpressure acknowledgment. |
| `0x05` | `PING` | Bidirectional | Heartbeat keep-alive probe. |
| `0x06` | `PONG` | Bidirectional | Heartbeat keep-alive response. |

---

## 3. In-Browser DOM Virtualization Sandbox

1. **Service Worker MessagePort Tunneling**: Bypasses origin boundary limits via bidirectional `MessagePort` channels.
2. **W3C Fetch Strict Null-Body Alignment**: Enforces `new Response(null, { status })` for HTTP 204, 205, and 304 responses, preventing Chromium/WebKit type errors.
3. **W3C XHR Upload Progress Virtualization**: Intercepts `XMLHttpRequest.prototype.send` to dispatch simulated `ProgressEvent` sequences based on physical transfer speed.
4. **WebSocket Defragmentation**: Employs circular buffers and 5-byte headers (Opcode + Length) to reassemble fragmented frames across SCTP boundaries.
5. **Session-Isolated CookieJar & DOM Synchronization**: Server isolates independent CookieJars; non-HttpOnly cookies are mirrored into `document.cookie`.
6. **Anti-Frame-Busting Defense**: Strips `allow-top-navigation` on iframes and proxies `window.top` / `window.parent` references.
7. **New Tab / Window Node ID Persistence**: Service Worker injects `window.__P2P_NODE_ID__` into HTML streams, and `patch.js` rewrites dynamic links to `?peer=${nodeId}#${nodeId}`, preventing new window clicks from dropping the node context.
