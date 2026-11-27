# Production Deployment and 3-Tier Sandbox

WebRTC Gateway runs on a single authoritative Go signaling server (`cmd/signal`) that serves both content-hashed frontend assets and WebSocket signaling, with three explicit sandbox isolation tiers. Configure domains, TLS certificates, persistent state, and hardened containers following the layout below. For ready-to-use templates by topology and workload, see [Server Use Cases](./server-use-cases) and [Agent Use Cases](./agent-use-cases).

---

## 1. 3-Tier Sandbox DNS and TLS Planning

Choose the isolation tier matching your domain and TLS certificate setup (loopback development automatically uses `*.localhost`):

| Tier | Flags | Required DNS Records | TLS Certificate | Isolation Properties |
| :--- | :--- | :--- | :--- | :--- |
| **Tier 1: Dual Fixed Cross-Site** | `-portal-domain portal.example.com` + `-sandbox-host app.example.net` | Two standard `A`/`AAAA`/`CNAME` records | One multi-SAN HTTP-01 cert (direct `-tls-cert`) or two separate single-host certs behind Nginx/Caddy (zero wildcard cert needed) | Full cross-site isolation (distinct eTLD+1); apps share the fixed sandbox origin |
| **Tier 2: Single-Root Same-Site Wildcard** | `-portal-domain portal.example.com` + `-sandbox-domain sbx.example.com` + `-allow-same-site-sandbox` | `portal.example.com` + `*.sbx.example.com` | Wildcard cert `*.sbx.example.com` | Per-node `sbx-<24hex>` origins + `Origin-Agent-Cluster: ?1` process isolation (same-site warning banner) |
| **Tier 3: Dual-Root Cross-Site Wildcard (Recommended)** | `-portal-domain portal.example.com` + `-sandbox-domain sandbox.example.net` | `portal.example.com` + `*.sandbox.example.net` | `portal.example.com` + wildcard `*.sandbox.example.net` | Full cross-site isolation + per-node device-salted HMAC `sbx-<24hex>` origins |

> [!NOTE]
> When wildcard DNS-01 certificates are unavailable, prefer **Tier 1 Dual Fixed Cross-Site** for full cross-site isolation with ordinary HTTP-01 certs. Multi-node production environments should use **Tier 3 Dual-Root Cross-Site Wildcard**.

---

## 2. Reproducible Binary Build and Reverse Proxy Setup

Run `make build` to compile content-hashed frontend bundles and embed the explicit manifest into the standalone `bin/signal` binary via `-tags embedweb`:

```bash
make build

# Tier 3: Cross-site dual-root wildcard deployment (recommended)
SIGNAL_SECRET_FILE=/etc/webrtc-gateway/signal.secret \
./bin/signal \
  -addr 0.0.0.0:8888 \
  -portal-domain portal.example.com \
  -sandbox-domain sandbox.example.net \
  -bound-keys-file /var/lib/webrtc-gateway/bound-keys.json \
  -trusted-proxies 127.0.0.0/8,::1/128,192.0.2.0/24
```

When terminating TLS behind Nginx or Caddy, forward the original `Host`, `X-Forwarded-For`, `X-Forwarded-Proto`, and WebSocket `Upgrade` headers, and list the proxy CIDR in `-trusted-proxies` so visitors do not share one rate-limit bucket. For multi-node deployments, configure `-node-secrets-file` to isolate per-node admission secrets so a leaked secret on one node cannot take over another node.

---

## 3. Hardened Container Deployment (`docker-compose.yml`)

The root `docker-compose.yml` binds signaling to `127.0.0.1:8888:8888` by default (for TLS termination via a host reverse proxy), enforces `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges:true`, and resource limits, and persists signaling node key bindings in `signal_state` and Agent Ed25519 keys plus capability revocations in `agent_identity` (when a host reverse proxy forwards traffic over Docker bridge, add that bridge CIDR such as `172.17.0.0/16` to `TRUSTED_PROXIES`):

```bash
cat <<'EOF' > .env
PORTAL_DOMAIN=portal.example.com
SANDBOX_DOMAIN=sandbox.example.net
SIGNAL_SECRET=replace-with-random-cluster-secret
ACCESS_SECRET=replace-with-random-visitor-secret
EOF

docker compose up -d --build
```

---

## 4. No Wildcard Cert: Tier 1 Dual Fixed Cross-Site Example

When only standard HTTP-01 certificates are available, configure two fixed hostnames on distinct eTLD+1 sites (`portal.example.com` and `app.example.net`) via `-sandbox-host` to obtain cross-site isolation and W3C Fetch anti-CSRF gating. Direct TLS termination on `cmd/signal` via `-tls-cert` loads a single PEM certificate (use one multi-SAN cert covering both hostnames); to serve two separate single-host certificates by SNI, terminate TLS at Nginx or Caddy and reverse-proxy to `127.0.0.1:8888`:

```bash
SIGNAL_SECRET_FILE=/etc/webrtc-gateway/signal.secret \
./bin/signal \
  -addr 0.0.0.0:443 \
  -portal-domain portal.example.com \
  -sandbox-host app.example.net \
  -tls-cert /etc/webrtc-gateway/certs/fullchain.pem \
  -tls-key /etc/webrtc-gateway/certs/privkey.pem \
  -bound-keys-file /var/lib/webrtc-gateway/bound-keys.json
```
