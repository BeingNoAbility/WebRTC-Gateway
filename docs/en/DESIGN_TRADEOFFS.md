# Design Boundaries, Trade-offs, and Failure Modes

> **Document Layering and Precedence**: [Constitution](./CONSTITUTION.md) > [Review Guidelines](./REVIEW_GUIDELINES.md) > this document.
>
> To prevent current engineering choices from hardening into dogma while still stopping already-disproven ideas from returning, this document separates three distinct categories: (1) **permanent non-goals**, (2) **current architectural trade-offs with explicit RFC revisit criteria**, and (3) **canonical cross-layer failure modes**. Exact current protocol constants and default flags live in [Wire Protocol](./guide/protocol-specification.md) and [Security](./guide/security.md).

---

## 1. Permanent Non-Goals (Identity and Security Physics)

The following four directions violate the project's core identity or fundamental browser/cryptographic security physics. They are not trade-offs and SHOULD be rejected in review:

### 1. Centralized Data Relay and Signaling-Side Offline Password Oracles

- **Why rejected**: Buffering, decoding, or forwarding application traffic on the signaling server breaks Zero Data Relay; carrying password hashes or data-plane session handles in signaling gives any signaling observer or log leak an unlimited offline brute-force oracle.
- **Boundary**: Signaling brokers connection setup only; endpoint verification and visitor authentication run strictly inside end-to-end encrypted transport; symmetric-NAT fallback permits only user-operated TURN relaying opaque end-to-end ciphertext.

### 2. In-Page JS Shims as a Security Boundary or Real-Cookie Single-Origin Defaults

- **Why rejected**: Native browser primitives (`window.top`, `Document.prototype.cookie`) cannot be reliably spoofed inside an untrusted page; defaulting to single-origin or same-site sharing invites Cookie Tossing and zombie credentials. In-page JS patching is a compatibility shim only, never a security boundary.
- **Boundary**: Non-loopback deployments rely on browser-enforced cross-site isolation by default; server `HttpOnly` cookies stay isolated in the Agent while client-accessible cookies sync via an in-memory mirror without touching real browser `document.cookie`.

### 3. Bundled Application UIs, App-Specific Hardcoding, and In-Process Compatibility Plugins

- **Why rejected**: Bundling file managers or terminals into the portal/agent, branching on specific application paths or headers, or exposing in-process plugins / sandbox JS hooks violates Pure Pipe neutrality, exposes private keys and session jars to extensions, locks down refactoring, and hides spec bugs that belong in core.
- **Boundary**: Follow the [Three-Step Compatibility Triage](./CONSTITUTION.md#principle-3-pure-pipe-and-application-agnostic) — fix standard gaps in core, offer default-off generic proxy knobs, and handle proprietary app quirks downstream at the standard HTTP/WS boundary (local Nginx / Caddy or standalone sidecar).

### 4. Generic Cross-Origin Open Proxying, Regex Host Rewriting, and Runtime Endpoint Overrides

- **Why rejected**: Built-in multi-domain cross-origin proxying turns a narrow tunnel into a generic SSRF surface; regex/prefix hostname guessing creates origin confusion; runtime URL overrides for signaling endpoints or weakened portal CSP (`'unsafe-inline'`) open phishing and XSS vectors.
- **Boundary**: Enforce strict 1 Agent = 1 exact target origin using standard `URL` exact-origin comparison; maintain strict portal CSP with zero runtime external endpoint overrides.

---

## 2. Current Architectural Trade-offs and Revisit Criteria

The following five decisions reflect **engineering bets under today's browser APIs, complexity budget, and operational constraints** — not immutable laws. Each records why we chose it today and what condition changes justify revisiting it via an RFC:

### 1. Single-Connection Ordered Channel Pool vs Multi-PC Trunking or Cross-Channel Splitting

- **Current choice**: Combine one control channel, a fixed pre-negotiated ordered HTTP channel pool, and dynamic dedicated WebSocket channels over a single `RTCPeerConnection`, relying on native SCTP channel ordering to avoid cross-stream head-of-line blocking and orphan reassembly.
- **Why not multi-PC or split control/data today**: Distinct SCTP channels lack causal order; splitting headers from data or managing parallel PCs introduces orphan queues, complex teardown choreography, ordering races, and memory amplification.
- **Revisit via RFC when**: Standardized browser P2P transports with native lightweight multiplexed streams (such as P2P WebTransport / QUIC streams) achieve broad cross-browser support, or reproducible workloads demonstrate single-connection saturation that cannot be resolved within one connection.

### 2. Fresh-Connection Silent Reconnect vs In-Place ICE Restart

- **Current choice**: After network handoffs or disconnects, the portal silently opens a fresh connection and re-authenticates using in-memory derived credentials while keeping the application iframe mounted.
- **Why not in-place ICE Restart today**: Earlier custom cryptographic roaming ratchets and multi-state restart machines introduced high complexity and replay/desync surfaces, whereas fresh-connection reconnect resets state cleanly with low latency for interactive HTTP workloads.
- **Revisit via RFC when**: A minimal, standards-based RFC 8839 ICE Restart design — free of bespoke key-ratchet state machines — can reliably preserve long-lived WebSockets and in-flight uploads across cellular/Wi-Fi handoffs on supported browsers and Pion WebRTC.

### 3. Single Authoritative Signaling Binary vs Split Static Hosting or Dual-Stack Signaling

- **Current choice**: Deliver content-hashed frontend assets from one authoritative Go `signal` binary via an explicit manifest, and maintain a single authoritative Go signaling implementation.
- **Why not split hosting or dual hand-written stacks today**: Manual cross-origin CDN/CORS/CSP/SW splits are error-prone and do not meaningfully shrink the TCB without cryptographic verification; maintaining two hand-written signaling servers in different languages leads to continuous security drift across rate limiting, path normalization, and concurrency guards.
- **Revisit via RFC when**: Either (a) cryptographically verifiable frontend delivery can genuinely remove the broker from the frontend TCB without fragile operator setup, or (b) secondary signaling targets can be compiled or generated from a single source of truth passing the full black-box conformance suite.

### 4. Zero-Dependency Native Primitives vs Third-Party JS / WASM Codecs and Parsers

- **Current choice**: Prefer browser-native streaming compression (`DecompressionStream`) and standard libraries, paired with a zero-dependency byte-level streaming HTML tokenizer.
- **Why not third-party JS/WASM libraries today**: Pure-JS decompressors and heavy WASM parsers increase Service Worker bundle size, cold-start latency, OOM exposure on hostile compressed streams, and supply-chain surface.
- **Revisit via RFC when**: Standard Web Platform APIs provide superior built-in capabilities (such as native streaming HTML rewriters or additional native codecs), or a strictly bounded, negligible-overhead module closes a verified spec gap that native JS cannot cleanly cover.

### 5. External Reverse-Proxy Composition vs Built-In Multi-Host Routing and Body Rewriting

- **Current choice**: Keep the gateway focused on transparent single-origin transport, delegating multi-origin fan-in, complex path/body rewriting, and proprietary auth shims to a local Nginx / Caddy or standalone HTTP/WS sidecar behind the target.
- **Why not embed a full routing engine today**: Standard HTTP/WS provides process isolation and zero plugin API lock-in, leveraging mature reverse-proxy ecosystems without exposing Agent secrets or session jars to extension code.
- **Revisit via RFC when**: A capability proves demonstrably vendor-neutral, RFC-standard, and broadly needed across web applications — entering Step 2 of the Constitution's Three-Step Triage as a default-off generic flag, never an app-specific plugin.

---

## 3. Canonical Cross-Layer Failure Modes

The six failure classes below recur across browser-based P2P proxies. Use these general mechanisms to reason about new changes rather than memorizing isolated historical patches:

### 1. Parser Differentials and Double Canonicalization

- **Mechanism**: Engines disagree on free-text protocols (raw SDP), percent-encoding, IPv4-mapped IPv6 addresses, redundant slashes, and symlinks; decoding or re-resolving paths twice between validation and execution bypasses security checks.
- **Defense principle**: Replace free-text regex parsing with minimal structured schemas; canonicalize paths, origins, and forwarded headers in a single pass; unmap IPv4-mapped addresses before IPv6 prefix aggregation; enforce filesystem confinement through kernel handles (such as stepwise `os.Root` and single non-following opens) to eliminate TOCTOU.

### 2. Hidden Metadata and Underlying Capacity Amplification

- **Mechanism**: Counting only payload bytes fails against floods of zero/tiny frames, sub-slices pinning large receive buffers (heap amplification), high-ratio decompression bombs, or buffering full WebSocket messages in memory.
- **Defense principle**: Copy retained inbound data into exact-sized allocations, include per-item structural overhead in multi-tier budgets, stream payloads in O(1) fixed buffers with hard decompression ratio/size caps, and release budgets and slots unconditionally on every close, reset, or early-response exit path.

### 3. Locks Across Blocking I/O and Async Generation Races

- **Mechanism**: Holding mutexes while awaiting network or channel I/O causes permanent deadlocks on half-closed disconnects; during handoffs and reconnects, stale async callbacks, timers, or delayed frames can corrupt active sessions.
- **Defense principle**: Enforce a strict global lock hierarchy with lock-free cancellation/reset paths that wake blocked I/O; guard sessions and reusable slots with monotonic generation tokens to drop stale callbacks and late frames; bind every background goroutine and resource so silent peer loss and shutdown converge in bounded time.

### 4. Browser Synthetic Execution Impedance Mismatch

- **Mechanism**: Service Worker synthetic responses lose implicit browser handling — no automatic decompression, decoders fail on empty-body responses, Fetch `Headers` enforce strict ByteString encoding, text-chunk HTML rewriting corrupts split UTF-8 or pushes `<meta charset>` past the prescan window, inserting before `<!DOCTYPE>` triggers Quirks Mode, and retrying non-idempotent requests duplicates side effects.
- **Defense principle**: Handle empty payloads, explicit encoding negotiation, RFC 5987 header sanitization, and byte-level HTML5 streaming tokenization strictly per W3C / IETF specs; restrict automatic retries to bodyless idempotent requests; bypass transforms on SSE, `206 Range`, and strong `ETag`s.

### 5. Origin/Site Privilege Confusion and Untrusted-Tenant Pollution

- **Mechanism**: Serving untrusted static documents under interactive-app CSP policies enables Stored XSS; sharing one cookie store between untrusted page scripts and protected server sessions enables semicolon injection, shadowing, and eviction; relaxing Origin or Fetch checks locally or cross-site enables CSRF and Cross-Site WebSocket Hijacking (CSWSH).
- **Defense principle**: Always treat the application sandbox origin as an untrusted tenant — serve sandbox CSP on all static files by default, isolate server and client cookies per RFC 6265bis, and enforce anti-CSRF / CSWSH gates via native W3C Fetch properties and exact Origin allowlists.

### 6. Fail-Open Saturation and Non-Atomic Persistence Loss

- **Mechanism**: Failing open when rate-limiter tables fill — or evicting entries currently in active lockout — lets attackers wash out brute-force lockouts by flooding new keys; concurrent non-atomic file writes lose key bindings or token revocations across crashes and restarts.
- **Defense principle**: Fail closed when limiters saturate without evicting active lockouts; persist security state through single-writer atomic storage (temp file + `fsync` + atomic rename) and enforce revocations continuously across active connections and streams.
