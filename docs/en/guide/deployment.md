# Production Deployment and 2-Tier Sandbox

> Audience: operators publishing the signaling and portal service to the internet, to choose a deployment tier and plan domains, certificates, and reverse proxies.

WebRTC Gateway runs on a single authoritative Go signaling server (`cmd/signal`) that pre-renders embedded content-hashed frontend assets at startup and serves WebSocket signaling, offering two explicit sandbox isolation tiers configured via explicit startup origins (`-portal-origin` and `-sandbox-origin`, or `-dev` on loopback). Configure domains, TLS certificates, persistent state, and hardened containers following the layout below. For ready-to-use templates by topology and workload, see [Server Use Cases](./server-use-cases) and [Agent Use Cases](./agent-use-cases).

---

## 1. 2-Tier Sandbox DNS and TLS Planning

Choose the isolation tier matching your domain and TLS certificate setup (loopback development explicitly passes `-dev` for `*.localhost`):

| Tier | Flags | Required DNS Records | TLS Certificate | Isolation Properties |
| :--- | :--- | :--- | :--- | :--- |
| **Tier 1: Dual-Root Cross-Site Wildcard (Recommended)** | `-portal-origin https://portal.example.com` + `-sandbox-origin https://*.sandbox.example.net` | `portal.example.com` + `*.sandbox.example.net` | `portal.example.com` + wildcard `*.sandbox.example.net` | Full cross-site isolation + per-node device-salted HMAC `sbx-<24hex>` origins (no warning banner) |
| **Tier 2 Restricted: Fixed Cross-Site Host** | `-portal-origin https://portal.example.com` + `-sandbox-origin https://app.example.net` | Two standard `A`/`AAAA`/`CNAME` records | One multi-SAN HTTP-01 cert (direct `-tls-cert`) or two separate single-host certs behind Nginx/Caddy | Cross-site from Portal, but multiple nodes and capability guests share one sandbox origin (persistent Tier 2 warning banner) |
| **Tier 2 Restricted: Single-Root Same-Site Wildcard** | `-portal-origin https://portal.example.com` + `-sandbox-origin https://*.sbx.example.com` + `-allow-same-site-sandbox` | `portal.example.com` + `*.sbx.example.com` | Wildcard cert `*.sbx.example.com` | Per-node `sbx-<24hex>` origins + `Origin-Agent-Cluster: ?1`, same-site with Portal (persistent Tier 2 warning banner) |

> [!NOTE]
> Multi-node production deployments and external capability sharing should use **Tier 1 Dual-Root Cross-Site Wildcard**. When wildcard DNS-01 certificates are unavailable and you run single-node or mutually trusted apps, **Tier 2 Fixed Cross-Site Host** provides cross-site Portal isolation with ordinary HTTP-01 certs.

---

## 2. Reproducible Binary Build and Reverse Proxy Setup

Run `make build` to compile content-hashed frontend bundles and embed the explicit manifest into the standalone `bin/signal` binary via `-tags embedweb`:

```bash
make build

# Tier 1: Cross-site dual-root wildcard deployment (recommended)
SIGNAL_SECRET_FILE=/etc/webrtc-gateway/signal.secret \
./bin/signal \
  -addr 0.0.0.0:8888 \
  -portal-origin https://portal.example.com \
  -sandbox-origin 'https://*.sandbox.example.net' \
  -bound-keys-file /var/lib/webrtc-gateway/bound-keys.json \
  -trusted-proxies 127.0.0.0/8,::1/128,192.0.2.0/24
```

When terminating TLS behind Nginx or Caddy, forward the original `Host`, `X-Forwarded-For`, and WebSocket `Upgrade` headers, and list the proxy CIDR in `-trusted-proxies` so visitors do not share one rate-limit bucket (external scheme and port are declared authoritatively at startup via `-portal-origin` / `-sandbox-origin`, with zero runtime `X-Forwarded-Proto` guessing). For multi-node deployments, configure `-node-secrets-file` to isolate per-node admission secrets (minimum 16 bytes each) so a leaked secret on one node cannot take over another node.

---

## 3. Hardened Container Deployment (`docker-compose.yml`)

The root `docker-compose.yml` binds signaling to `127.0.0.1:8888:8888` by default (for TLS termination via a host reverse proxy), enforces `read_only: true`, `cap_drop: [ALL]`, `no-new-privileges:true`, and resource limits, and persists signaling node key bindings in `signal_state` and Agent Ed25519 keys plus capability revocations in `agent_identity` (when a host reverse proxy forwards traffic over Docker bridge, add that bridge CIDR such as `172.17.0.0/16` to `TRUSTED_PROXIES`):

```bash
cat <<'EOF' > .env
PORTAL_ORIGIN=https://portal.example.com
SANDBOX_ORIGIN=https://*.sandbox.example.net
SIGNAL_URL=wss://portal.example.com/ws
SIGNAL_SECRET=replace-with-random-cluster-secret
ACCESS_SECRET=replace-with-random-visitor-secret
EOF

docker compose up -d --build
```

---

## 4. No Wildcard Cert: Tier 2 Fixed Cross-Site Host Example

When only standard HTTP-01 certificates are available for a single-node or mutually trusted setup, configure two fixed hostnames on distinct eTLD+1 sites (`portal.example.com` and `app.example.net`) via explicit `-sandbox-origin` (Portal displays the Tier 2 warning banner noting shared sandbox origin across nodes). Direct TLS termination on `cmd/signal` via `-tls-cert` loads a single PEM certificate (use one multi-SAN cert covering both hostnames); to serve two separate single-host certificates by SNI, terminate TLS at Nginx or Caddy and reverse-proxy to `127.0.0.1:8888`:

```bash
SIGNAL_SECRET_FILE=/etc/webrtc-gateway/signal.secret \
./bin/signal \
  -addr 0.0.0.0:443 \
  -portal-origin https://portal.example.com \
  -sandbox-origin https://app.example.net \
  -tls-cert /etc/webrtc-gateway/certs/fullchain.pem \
  -tls-key /etc/webrtc-gateway/certs/privkey.pem \
  -bound-keys-file /var/lib/webrtc-gateway/bound-keys.json
```
