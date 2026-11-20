# System Architecture & Technical Specifications

WebRTC Gateway adopts a decoupled, cloud-native edge architecture comprising three core components:
1. **Browser Client (In-Browser Sandbox Runtime)**: Includes the Web Portal App-Shell, Service Worker traffic interceptor, DOM virtualization monkey-patches (`patch.js`), and the TypeScript P2PMux binary multiplexer;
2. **Signaling Server (Signaling & Session Broker)**: Stateless or lightweight in-memory WebSocket broker (supporting Cloudflare Workers or standalone Go daemons). Negotiates SDP Offer/Answer and ICE Candidates during handshakes and exits the data path once the P2P tunnel is established;
3. **Agent Daemon (Intranet Daemon)**: Long-running Go daemon managing WebRTC PeerConnections, unframing and multiplexing P2PMux binary streams, and reverse-proxying HTTP/WS to local services or streaming local files directly in memory.

---

## 1. System Architecture & End-to-End Data Flow

```mermaid
sequenceDiagram
    autonumber
    actor Visitor as Visitor Browser (Chrome/Edge/Safari/Firefox)
    participant Signal as Public Signal Server / CF Worker
    participant Agent as Intranet Agent Daemon (Private Server)
    participant Target as Local Target Service (127.0.0.1:8080)

    Agent->>Signal: Long-lived WebSocket connection & Register Node ID
    Visitor->>Signal: Open Web Console & request target node connection
    Visitor->>Signal: Send WebRTC SDP Offer (with target Node ID)
    Signal->>Agent: Forward SDP Offer
    Agent->>Signal: Return SDP Answer & ICE Candidates
    Signal->>Visitor: Relay SDP Answer to Browser
    Note over Visitor,Agent: Phase 1 complete: Signaling server exits data transmission path

    Visitor<<->>Agent: WebRTC STUN/ICE NAT traversal & punching
    Visitor<<->>Agent: Establish end-to-end encrypted dual DataChannels (SCTP: "p2pcontrol" + "p2pdata")
    Note over Visitor,Agent: Phase 2 complete: Direct P2P tunnel established (DTLS 1.2 / SRTP)

    Note over Visitor: Service Worker intercepts page HTTP/WS requests
    Visitor->>Agent: Demuxed routing: Control metadata via p2pcontrol, bulk body via p2pdata
    Agent->>Target: Reverse-proxy HTTP/WS request (injects authoritative client IP)
    Target-->>Agent: Local service response (HTTP status, headers, chunked body)
    Agent-->>Visitor: Split framed response (Headers & streaming data concurrent over wire)
    Note over Visitor: Service Worker streams ReadableStream to DOM renderer
```

---

## 2. Native Dual-Channel Architecture & P2PMux Wire Specification

To eliminate Head-of-Line (HoL) blocking between large bulk file streaming and critical interactive API control traffic when handling hundreds of concurrent streams, the gateway introduces the **Native Dual-Channel Architecture** and lightweight binary framing protocol **P2PMux**.

For formal byte-level mathematical specifications and high-concurrency benchmarks, consult:
- [P2PMux Wire Protocol Specification](./protocol-specification)
- [Dual-Channel Anti-HoL & 1000-Stream Benchmark Report](../BENCHMARKS/DUAL_CHANNEL_HOL_BENCHMARK_REPORT)

### ① Dual-Channel Demuxing & Anti-Head-of-Line (HoL) Mitigation
- **`p2pcontrol` (Dedicated Control Channel)**: Exclusively carries `TypeHeaders` (HTTP metadata and URL routing), `TypePing` / `TypePong` (millisecond RTT latency probes and keepalive), `TypeRstStream` (per-stream resets), and `TypeWindowUpdate`. This channel **completely bypasses bulk data backpressure delays**, maintaining single-digit millisecond responsiveness even when data channels are saturated with 10GB transfers or experiencing packet loss;
- **`p2pdata` (High-Throughput Data Channel)**: Exclusively carries `TypeData` (HTTP bodies and file streams) and `TypeWSData` (native WebSocket frames). Governed by the 128MB global inbound memory budget and 96MB/64MB dual-watermark backpressure, closely linked with physical SCTP receive window (`a_rwnd`) throttling;
- **Orphan Frame Reconciliation & 500ms Reaper Fuse**:
  - Across independent parallel SCTP transport streams, network scheduling may cause `TypeData` chunks to arrive slightly before the corresponding `TypeHeaders` frame;
  - The Mux maintains an internal orphan buffer with **64KB per stream, 256KB global cap** limits;
  - Early-arriving `TypeData` frames are safely buffered and flushed immediately in order when `TypeHeaders` arrives on `p2pcontrol`. If no header arrives within 500ms, the background Reaper cleans the orphaned buffers and emits `RST_STREAM`, balancing out-of-order tolerance with anti-DoS memory boundaries.

### ② Binary Frame Layout (10-Byte Fixed Header)

```text
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          Stream ID (32 bits)                  |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|   Type (8)    |   Flags (8)   |       Payload Length (32)     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|       Payload Length (cont.)  |   Payload Data (0 ~ 32KB) ... |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+                               |
```

- **Stream ID (uint32, 4 bytes)**: Unique stream identifier. Client browsers allocate odd stream IDs (1, 3, 5...), while Agents allocate even stream IDs (2, 4, 6...) to prevent collisions;
- **Type (uint8, 1 byte)**: Frame semantic type:
  - `0x01 (TypeData)`: Stream payload chunk;
  - `0x02 (TypeHeaders)`: Stream initial metadata (JSON-serialized HTTP method, path, query, headers);
  - `0x03 (TypeRstStream)`: Stream reset or abnormal abort with optional error code;
  - `0x04 (TypeWindowUpdate)`: Flow control window increment credit;
  - `0x05 (TypePing)` / `0x06 (TypePong)`: RTT latency measurement and keepalive heartbeat;
  - `0x10 (TypeWSConnect)` / `0x11 (TypeWSData)` / `0x12 (TypeWSClose)`: Native WebSocket handshake, bi-directional data, and clean close frames.
- **Flags (uint8, 1 byte)**: Bitmask control flags:
  - `0x00 (FlagNone)`: Standard data frame;
  - `0x01 (FlagFin)`: Marks completion of stream transmission (half-close signal);
  - `0x02 (FlagAck)`: Control frame acknowledgment.
- **Payload Length (uint32, 4 bytes)**: Payload byte count (Big-Endian). Hard ceiling is **64KB (`MaxPayloadSize = 65536` bytes)**; recommended streaming chunk size is **32KB (`RecommendedChunkSize = 32758` bytes)**. Combining a 32758-byte payload with a 10-byte header yields exactly **32768 bytes (32KB)**, aligning with Go runtime `mcache` allocation size-classes and OS buffer efficiencies, while physical transmission relies on WebRTC SCTP stack fragmentation over the network path MTU (~1500 bytes).

---

## 3. In-Browser DOM Virtualization & Service Worker Sandbox

### ① Service Worker & Dedicated MessagePort Channel
- The Portal registers the Service Worker and establishes a dedicated bidirectional pipeline via `MessagePort` (`PORTAL_ATTACH`);
- The Service Worker intercepts same-origin `fetch` events from the page and embedded iframes, forwarding requests over `MessagePort` to the in-page P2PMux dispatcher;
- Responses return as chunked `ReadableStream` pipelines, enabling large media and binary streams to decode and render progressively.

### ② W3C Fetch Strict Null-Body Alignment (204/304)
- Chromium and WebKit strictly prohibit response bodies on HTTP 204 No Content, 205 Reset Content, and 304 Not Modified status codes, throwing `TypeError: Response with null-body status cannot have body` if violated;
- The Service Worker validates status codes and forces `new Response(null, { status })`, guaranteeing stability for SPAs (Next.js, SvelteKit).

### ③ W3C XMLHttpRequest Upload Progress Virtualization
- Standard Service Workers contain a known limitation (Chromium Bug 1214207): proxied XHR uploads do not dispatch `xhr.upload.onprogress` events, causing upload progress bars to freeze at 0%;
- `patch.js` intercepts `XMLHttpRequest.prototype.send` and schedules virtualized W3C-compliant `ProgressEvent` sequences based on physical transfer speed, restoring smooth progress bars for LibreSpeed and file vaults.

### ④ WebSocket Defragmentation across SCTP Boundaries
- Native WebSocket messages can reach hundreds of kilobytes, exceeding WebRTC SCTP safe transmission limits (16KB~32KB);
- The gateway incorporates streaming defragmentation buffers on both ends: the sender prepends a 5-byte header (1-byte Opcode + 4-byte Length), and the receiver reassembles frames in a ring buffer before emitting `onmessage`.

### ⑤ Session-Isolated CookieJar & document.cookie Synchronization
- The Agent maintains an independent `http.CookieJar` per remote session, autonomously managing sensitive `Set-Cookie` (including `HttpOnly`) across upstream requests;
- Non-HttpOnly cookies are forwarded to the browser via response metadata, where `patch.js` synchronizes them into `document.cookie` for JavaScript compatibility.

### ⑥ Anti-Frame-Busting Sandbox
- Enterprise management dashboards (Portainer, Grafana) often include anti-clickjacking scripts (`if (window.top !== window.self) window.top.location = ...`);
- The gateway renders iframes without `allow-top-navigation` and injects virtualized proxies for `window.top`, `window.parent`, and `window.frameElement`, allowing seamless execution within the Portal sandbox.

### ⑦ Anti-Hotlinking Defense & Tab/Window Node ID Persistence
- **Anti-Hotlinking**: Injects `<meta name="referrer" content="no-referrer">` into HTML responses and rewrites Origin/Referer headers upstream, eliminating 403 Forbidden errors from third-party CDNs;
- **Window/Tab Persistence**: When opening links in new tabs (`<a target="_blank">` or `window.open`), the Service Worker maintains `activeNodeId` and injects `window.__P2P_NODE_ID__` into HTML streams. `patch.js` rewrites dynamic links to `?peer=${nodeId}#${nodeId}`, preventing new tabs from dropping node context.

### ⑧ Authoritative Client IP Propagation & WAF Synergy
- **Authoritative Signaling Anchor**: When relaying SDP Offers, the signaling server extracts the socket remote IP (or Cloudflare `cf-connecting-ip`) and sets `msg.ClientIP = session.RemoteIP`, discarding forged client headers;
- **Agent Reverse-Proxy Override**: The Agent forcibly overrides `X-Forwarded-For` and `X-Real-IP` with the authoritative IP before proxying HTTP/WS requests;
- **WAF Synergy**: Enables SafeLine, Nginx WAF, and ModSecurity to perform per-IP rate limiting, individual IP blocking, and GeoIP logging, while supporting in-place ICE Restart IP updates on mobile network roaming.

### ⑨ Zero-Dependency Frontend Bilingual i18n & In-Place DOM Repaint
- **Ultra-Lightweight Type-Safe i18n Engine**: The Web Portal embeds a bespoke zero-dependency i18n dictionary engine (`web/src/portal/i18n.ts`, < 2.5KB overhead) supporting Simplified Chinese (`zh-CN`) and English (`en-US`);
- **In-Place DOM Repainting with Zero DataChannel Disruption**: Language switching performs localized text node updates on elements with `data-i18n` attributes without reloading the page, guaranteeing that underlying physical WebRTC PeerConnections, dual DataChannels, active media streams, and file transfers remain **100% uninterrupted**;
- **Input & State Preservation**: Dynamically preserves typed node IDs, access secrets, and floating HUD snap coordinates during live language toggling.

---

## 4. Signaling Architecture & Topology Trade-offs

WebRTC Gateway provides dual-track signaling deployment topologies to accommodate diverse operational scales:

| Dimension | Standalone Go Broker (`cmd/signal`) | Cloud-Native Edge Distributed (`edge/cloudflare-worker/`) |
| :--- | :--- | :--- |
| **Runtime** | Single static Go executable binary | Cloudflare Workers + SQLite Durable Objects (DO) |
| **Routing** | Single public IP / VPS direct routing | 300+ city worldwide Anycast edge routing |
| **State Consistency** | In-memory sync.Map with RWMutex | Edge hash-partitioned per NodeID with singleton DO consistency |
| **Connection Model** | Long-running Go goroutine per connection | WebSocket Hibernation API (zero CPU billing while idle) |
| **Target Use Case** | Local LAN, private NAS, homelab, and self-hosted instances | Global high-availability, zero-maintenance serverless production |

---

## 5. Target Web Application Architecture Compatibility Matrix

Because WebRTC Gateway operates on a "Zero-Client / Browser Sandbox Virtualization" model, target web applications perform differently based on their architectural patterns:

| Tier | Category | Examples | Compatibility & Architectural Notes |
| :---: | :--- | :--- | :--- |
| **Tier 1 (Native Support)** | • Standard SPAs / MPAs<br/>• Admin dashboards<br/>• Media streaming<br/>• Monitoring & analytics | Portainer, Grafana, Jellyfin, LibreSpeed, Vaultwarden, OpenList, NAS Web UIs, static sites | • Standard HTTP/1.1 and WebSocket APIs;<br/>• Supports 204/304 caching and HTTP 206 range requests;<br/>• CookieJar and anti-frame-busting active automatically. |
| **Tier 2 (Conditional Support)** | • Dynamic multi-domain redirects<br/>• External 3rd-party OAuth login | Apps relying on external Google / GitHub OAuth | • Requires `--p2p-proxy` or subdomain virtualization to preserve intra-session tunneling;<br/>• External OAuth callbacks must preserve query/hash context. |
| **Tier 3 (Known Limitations)** | • Applications registering **their own Service Worker**<br/>• Apps with strict `COOP: same-origin` | Heavy offline PWAs, cloud browser IDEs with nested SW interception | • Browser same-origin security restricts each origin to a single active controlling Service Worker; target SW conflicts with gateway SW;<br/>• Strict `Cross-Origin-Opener-Policy` may sever cross-window messaging. Dedicated subdomain or native agent recommended. |
