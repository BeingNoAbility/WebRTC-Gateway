# Troubleshooting and FAQ

> Audience: users and operators looking up causes by symptom when connections fail, authentication is rejected, or pages misbehave.

See [Production Deployment](./deployment) for DNS/proxy setup, [Agent & Server Configuration](./agent-configuration) for all flags, and [Security & Cryptography](./security) for trust boundaries.

---

## 1. Common Troubleshooting

### 1. Stuck on "Connecting" or ICE Connection Failed

- **Cause**: Both visitor and intranet Agent sit behind Symmetric NATs where STUN direct hole-punching cannot succeed.
- **Fix**: Configure a self-hosted TURN server (TURN forwards end-to-end DTLS ciphertext only):

```bash
ACCESS_SECRET=change-me TURN_PASS=your-turn-pass ./bin/agent \
  -id myhome \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret \
  -target http://127.0.0.1:8080 \
  -turn-url turn:turn.example.com:3478?transport=udp \
  -turn-user turn_user
```

Visitors can also enter an RFC 7065 TURN URI in the SmartPill Settings modal (use `turn:host:port`, never `turn://host:port`).

### 2. Signal or Agent Exits Immediately at Startup (Fail-Closed)

| Error Message | Cause | Fix |
| :--- | :--- | :--- |
| `SIGNAL_SECRET is required` / `at least 16 bytes` | Missing or too-short cluster admission secret on Signal | Set a `SIGNAL_SECRET` / `SIGNAL_SECRET_FILE` of at least 16 bytes, or pass `-allow-anonymous-agents` for local dev |
| `both -portal-origin and -sandbox-origin are required` | Signal started without explicit origins or `-dev` | Configure `-portal-origin` + `-sandbox-origin` (add `-allow-same-site-sandbox` for Tier 2 single-root setups), or pass `-dev` for loopback dev |
| `ACCESS_SECRET is required` | Missing visitor secret on Agent | Set `ACCESS_SECRET` / `ACCESS_SECRET_FILE`, or pass `-allow-public` explicitly |
| `Specify exactly ONE of -target, -dir, or -file` | Zero or multiple backend modes set | Specify exactly one of `-target`, `-dir`, or `-file` |
| `unknown field ...` | Typo or invalid YAML key in config file | Check keys against `configs/*.example.yaml` (`KnownFields(true)` is enforced) |

### 3. Red-Screen Blocker: "Agent Identity Public Key Changed"

- **Cause**: The remote Agent's Ed25519 public key differs from the pinned `?pk=` hash or previously recorded `IndexedDB` TOFU fingerprint.
- **Fix**:
  - If you did **not** rotate the Agent key, **stop immediately** and investigate signaling or network tampering;
  - If you intentionally rotated `-identity-key` (or restarted with `-ephemeral-identity`): for bare `#node` URLs, use the explicit TOFU reset button in the modal; for pinned `?pk=` share links, mint a fresh link from the Agent (pinned links never offer a UI bypass);
  - If Signal rejects the rotated Agent key, pass `-force-rekey` on the Agent (note: under a shared `SIGNAL_SECRET`, `-allow-force-rekey` defaults to `false`; use per-node `-node-secrets-file` or explicitly enable `-allow-force-rekey=true` on Signal).

### 4. Browser WebSocket Handshake Returns `403 Forbidden` or `421 Misdirected Request`

- **`421 Misdirected Request`**: The `Host` header does not match `PORTAL_ORIGIN`, `SANDBOX_ORIGIN`, or loopback `localhost` / `*.localhost` under `-dev`. Ensure your reverse proxy preserves the original `Host` header.
- **`403 Forbidden` (behind a reverse proxy)**: The browser's `Origin` header does not exactly match the configured startup `-portal-origin` (scheme, host, and port). Verify external scheme/port matches `-portal-origin`, and add your reverse proxy CIDR to `-trusted-proxies` for accurate per-IP rate limiting.
- **`403 Forbidden` (direct sandbox URL visit)**: Direct visits or cross-site probes to the sandbox origin are rejected by the SW W3C Fetch anti-CSRF gate; always enter through the Portal URL (e.g. `https://portal.example.com/#node`).

### 5. Agent Inside Docker Cannot Reach Host `127.0.0.1:8080`

- **Linux hosts**: Use `network_mode: "host"` to reach loopback services directly;
- **macOS / Windows Docker Desktop**: Use bridge networking with `extra_hosts: ["host.docker.internal:host-gateway"]` and point `TARGET_URL` to `http://host.docker.internal:8080`.

### 6. `502 Bad Gateway` Proxying Local Self-Signed HTTPS Backends

- The Agent verifies upstream TLS certificates by default; pass `-insecure-target` (`INSECURE_TARGET=true`) for trusted self-signed intranet panels such as Proxmox VE or NAS UIs.

### 7. Applications Requiring Virtual-Host `Host` Headers or Multi-Origin Aggregation

- Each Agent proxies strictly to one exact `-target` origin (zero cross-origin proxying);
- When targeting a local Nginx, Caddy, or Ingress virtual host, pass `-upstream-host` (paired with `-sni` for HTTPS) to override upstream `Host`, `Origin`, `Location`, and `ProxyCookieJar` matching;
- If an application requires multi-subdomain/API aggregation or response-body URL rewriting, place a local reverse proxy (Nginx or Caddy) behind `-target` to consolidate those origins into one single local endpoint; see [Local Reverse-Proxy & Adapter Recipes](./reverse-proxy-recipes.md).

---

## 2. Frequently Asked Questions (FAQ)

### Q: Do visitors need to install any software or browser extension?
No. Visitors open the Portal URL or share link in any standard modern browser (Chrome, Edge, Safari, Firefox).

### Q: Does transferring large files consume signaling server bandwidth?
No. Signaling only exchanges small JSON handshake messages; payload data travels directly peer-to-peer between browser and Agent. Bandwidth transits a relay only if both peers sit behind symmetric NATs and you configure a self-hosted TURN server.

### Q: How can I share a read-only subpath temporarily without giving out my main password?
Use the Share modal inside an authenticated Owner session (or `./bin/agent -mint-link 2h -mint-path /public -mint-readonly`) to mint a `?pk=...&cap=...` Ed25519 capability link. Owners can revoke active tokens immediately from the Share modal.

### Q: Will switching between Wi-Fi and cellular lose my current SPA route?
Portal's unified silent reconnect automatically builds a fresh `RTCPeerConnection` and silently re-authenticates with the tab's in-memory SPAKE2+ verifier, keeping `appFrame` mounted without reloading or re-prompting for a password.
