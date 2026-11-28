# Agent and Signal Server Configuration Reference

WebRTC Gateway uses a unified configuration loader (`pkg/config`):
- **Precedence**: CLI flags > Environment variables / `*_FILE` secret files > Explicit config file;
- **No CWD Auto-Loading**: Config files are loaded only when `-config <path>` or `CONFIG_FILE` is explicitly specified, with strict `KnownFields(true)` validation rejecting typos or unknown keys;
- **Secret Files First**: Supports `SIGNAL_SECRET_FILE`, `ACCESS_SECRET_FILE`, and `TURN_PASS_FILE` (compatible with Docker Secrets, Kubernetes Secrets, and systemd `LoadCredential`); passing secrets via CLI flags emits a process-list visibility warning.

See complete templates in [`configs/agent.example.yaml`](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/configs/agent.example.yaml) and [`configs/server.example.yaml`](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/configs/server.example.yaml), and scenario-oriented recipes in [Agent Use Cases](./agent-use-cases) and [Server Use Cases](./server-use-cases).

---

## 1. Agent Flags and Environment Variables (`cmd/agent`)

| CLI Flag | Env / Secret File | YAML Key | Default | Description |
| :--- | :--- | :--- | :--- | :--- |
| `-config` | `CONFIG_FILE` | — | empty | Explicit YAML/JSON config path |
| `-id` | `NODE_ID` | `node_id` | `homeserver` | Lowercase DNS-safe node ID (`sbx-` prefix forbidden) |
| `-signal` | `SIGNAL_URL` | `signal_url` | `ws://127.0.0.1:8888/ws` | Signaling server base URL (`wss://` required on non-loopback when carrying secret) |
| `-signal-secret` | `SIGNAL_SECRET` / `SIGNAL_SECRET_FILE` | `signal_secret` | empty | Cluster or per-node admission secret (HTTP Upgrade Bearer) |
| `-allow-insecure-signal` | `ALLOW_INSECURE_SIGNAL` | `allow_insecure_signal` | `false` | Explicitly allow sending `SIGNAL_SECRET` over non-loopback `ws://` (dev only) |
| `-force-rekey` | `FORCE_REKEY` | `force_rekey` | `false` | Force-rotate bound Ed25519 key on signaling server (requires valid secret and server policy) |
| `-access-secret` | `ACCESS_SECRET` / `ACCESS_SECRET_FILE` | `access_secret` | empty | Visitor in-DTLS RFC 9383 SPAKE2+ password (required unless `-allow-public`; mutually exclusive with `-allow-public`) |
| `-allow-public` | `ALLOW_PUBLIC` | `allow_public` | `false` | Explicitly allow passwordless visitors (Ed25519 and DTLS cert verification still mandatory; cannot combine with `-access-secret`) |
| `-identity-key` | `IDENTITY_KEY` | `identity_key` | user config dir | Persistent Ed25519 private key PEM path (atomically created with `0600`) |
| `-state-file` | `STATE_FILE` | `state_file` | `<identity-key>.state.json` (empty if ephemeral) | Persistent capability and revocation state JSON path (cross-process locked + `0600` atomic union merge across restarts) |
| `-ephemeral-identity` | `EPHEMERAL_IDENTITY` | `ephemeral_identity` | `false` | In-memory ephemeral identity key (breaks pinned links across restarts; cannot combine with `-identity-key` or `-mint-link`) |
| `-target` | `TARGET_URL` | `target_url` | empty | Local HTTP/HTTPS target URL (mutually exclusive with `-dir`/`-file`) |
| `-dir` | `DIR_PATH` | `dir_path` | empty | Read-only static directory (`os.Root` adapter) |
| `-file` | `FILE_PATH` | `file_path` | empty | Read-only single file (`os.Root` adapter) |
| `-hide-dotfiles` | `P2P_HIDE_DOTFILES` | `hide_dotfiles` | `true` | Hide dotfiles and dot-prefixed directories |
| `-follow-symlinks` | `FOLLOW_SYMLINKS` | `follow_symlinks` | `false` | Opt-in bounded symlink traversal strictly confined to root and active capability `path_prefix` (default Fail-Closed `O_NOFOLLOW` rejects all symlinks) |
| `-file-inline-active` | `FILE_INLINE_ACTIVE` | `file_inline_active` | `false` | Permit inline scripts in served static files (disables default all-file `CSP: sandbox`) |
| `-upstream-host` | `UPSTREAM_HOST` | `upstream_host` | empty | Override upstream virtual-host `Host` header (RFC 9110 `host[:port]`, valid with `-target` only; also applies to `Origin`, `Location`, and CookieJar) |
| `-proxy` | `PROXY_URL` | `proxy_url` | empty | Outbound HTTP/HTTPS/SOCKS5 proxy (fails closed on invalid URL) |
| `-sni` | `SNI` | `sni` | empty | TLS SNI override for HTTPS target |
| `-insecure-target` | `INSECURE_TARGET` | `insecure_target` | `false` | Skip TLS verification for self-signed local HTTPS target |
| `-ice-mdns` | `ICE_MDNS` | `ice_mdns` | `true` | Obfuscate LAN/Docker/VPN host ICE candidates via mDNS and Fail-Closed outbound scrubbing |
| `-stun-servers` | `STUN_SERVERS` | `stun_servers` | `stun:stun.cloudflare.com:3478,stun:stun.l.google.com:19302` | Custom comma-separated `stun:`/`stuns:` URIs (set `none`/`off` to disable) |
| `-turn-url` / `-turn-user` / `-turn-pass` | `TURN_URL` / `TURN_USER` / `TURN_PASS(_FILE)` | `turn_*` | empty | User-run TURN relay configuration |
| `-max-streams` | `MAX_STREAMS` | `max_streams` | `512` | Global concurrent P2PMux V8 stream ceiling |
| `-metrics-addr` | `METRICS_ADDR` | `metrics_addr` | empty | Prometheus `/metrics` and `/healthz` listen address |
| `-portal-url` | `PORTAL_URL` | `portal_url` | empty | Portal base URL for printing pinned and share links |
| `-mint-link` | — | — | empty | Mint an offline Ed25519 capability share link (e.g. `2h`, `7d`) and exit |
| `-mint-path` / `-mint-readonly` / `-mint-max-conns` | — | — | empty / `false` / `0` | Path prefix, read-only, and concurrency limits for `-mint-link` |
| `-revoke-cap` | — | — | empty | Revoke a capability token by JTI in the persistent state file and exit (cross-process lock safe) |
| `-health` / `-health-url` | — | — | `false` / empty | Run container health probe and exit |

---

## 2. Signal Server Flags and Environment Variables (`cmd/signal`)

| CLI Flag | Env / Secret File | YAML Key | Default | Description |
| :--- | :--- | :--- | :--- | :--- |
| `-config` | `CONFIG_FILE` | — | empty | Explicit YAML/JSON config path |
| `-addr` | `ADDR` | `addr` | `127.0.0.1:8888` | Listen address (non-loopback requires explicit origins; cannot combine with `-dev`) |
| `-portal-origin` | `PORTAL_ORIGIN` | `portal_origin` | empty | Explicit startup Portal origin including scheme and optional port (e.g. `https://portal.example.com`) |
| `-sandbox-origin` | `SANDBOX_ORIGIN` | `sandbox_origin` | empty | Explicit startup Sandbox origin: Tier 1 cross-site wildcard (`https://*.sandbox.example.net`) or Tier 2 restricted fixed host (`https://app.example.net`) |
| `-dev` | `DEV_MODE` | `dev` | `false` | Explicitly enable local loopback dev mode (`localhost` + `*.localhost`; loopback `-addr` only, address-family scoped) |
| `-allow-same-site-sandbox` | `ALLOW_SAME_SITE_SANDBOX` | `allow_same_site_sandbox` | `false` | Explicitly allow Tier 2 single-root same-site wildcard sandbox (adds `Origin-Agent-Cluster: ?1` and Tier 2 warning banner) |
| `-signal-secret` | `SIGNAL_SECRET` / `SIGNAL_SECRET_FILE` | `signal_secret` | empty | Default Agent registration secret (minimum 16 bytes; required unless `-node-secrets-file` or `-allow-anonymous-agents`) |
| `-node-secrets-file` | `NODE_SECRETS_FILE` | `node_secrets_file` | empty | Per-node secret map file (YAML/JSON `node_id: secret`, minimum 16 bytes each; overrides global secret with no fallback) |
| `-allow-force-rekey` | `ALLOW_FORCE_REKEY` | `allow_force_rekey` | auto (`false` under shared secret, `true` under per-node secrets) | Allow authenticated Agents to rotate bound Ed25519 keys via `force_rekey` |
| `-allow-anonymous-agents` | `ALLOW_ANONYMOUS_AGENTS` | `allow_anonymous_agents` | `false` | Explicitly allow anonymous Agent registration |
| `-bound-keys-file` | `BOUND_KEYS_FILE` | `bound_keys_file` | empty | JSON file persisting per-node Ed25519 public key bindings via `AtomicFileStore` across restarts |
| `-allow-all-origins` | `SIGNAL_ALLOW_ALL_ORIGINS` | `allow_all_origins` | `false` | Permit all WebSocket Origins (dev/testing only) |
| `-tls-cert` / `-tls-key` | `TLS_CERT` / `TLS_KEY` | `tls_cert` / `tls_key` | empty | TLS cert/key PEM paths (hot-reloaded on update) |
| `-tls-auto` | `TLS_AUTO` | `tls_auto` | `false` | Self-signed cert limited to loopback and configured domains (no LAN IP leak; mutually exclusive with `-tls-cert`/`-tls-key`) |
| `-trusted-proxies` | `TRUSTED_PROXIES` | `trusted_proxies` | `127.0.0.0/8,::1/128` | Trusted reverse proxy CIDRs for right-to-left XFF peeling (zero implicit `X-Real-IP`) |
| `-client-ip-header` | `CLIENT_IP_HEADER` | `client_ip_header` | empty | Optional single-value client IP header (accepts `IP` or `IP:port`, e.g. `CF-Connecting-IP`, `Fastly-Client-IP`, `CloudFront-Viewer-Address`, `X-Real-IP`) |
| `-max-ip-conns` / `-max-ip-msgs` | `MAX_IP_CONNS` / `MAX_IP_MSGS` | `max_ip_conns` / `max_ip_msgs` | `64` / `120` | Per-IP (IPv6 aggregated by `/64`) concurrent WS limit and 10 s message budget |
| `-static` | `STATIC_DIR` | `static_dir` | empty | External web directory overriding build-time embedded `web.DistFS` |
| `-metrics-addr` | `METRICS_ADDR` | `metrics_addr` | empty | Prometheus `/metrics` listen address |
| `-health` / `-health-url` | — | — | `false` / empty | Probe `/healthz` and exit |
