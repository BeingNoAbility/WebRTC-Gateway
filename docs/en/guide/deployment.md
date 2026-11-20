# Production Deployment Guide

This guide covers deployment options for the public signaling server and the intranet agent daemon.

---

## Deployment Architecture Options

| Method | Target Platform | Server Cost | Best For |
| :--- | :--- | :--- | :--- |
| **Option A: Cloudflare Workers** | Cloudflare Global Edge | **\$0 (Free Tier)** | Zero VPS maintenance, worldwide edge signaling |
| **Option B: Standalone Binary** | Linux VPS / Windows Server | Self-Hosted | Full self-containment, custom enterprise networks |
| **Option C: Docker Compose** | Linux / NAS / Kubernetes | Containerized | NAS users (Synology/QNAP/Unraid), automated deployments |

---

## Option A: Cloudflare Workers Edge Deployment (Zero Server Cost)

The project includes an edge signaling implementation built on Cloudflare Workers and Durable Objects:

### 2. Build & Local Deployment
```bash
# 1. Build web portal assets
cd web
npm install
npm run build

# 2. Deploy to Cloudflare Workers (Production or Testing)
npm run deploy:prod      # Deploy official production Worker (p2p)
npm run deploy:testing   # Deploy isolated testing Worker (testing.p2p)
```

Once deployed, the unified Worker handles both frontend static assets and WebSocket signaling:
- **Web Portal**: `https://p2p.yourdomain.com/` (or `https://p2p.<subdomain>.workers.dev/`)
- **Signaling Endpoint**: `wss://p2p.yourdomain.com/ws`

---

### 3. GitHub Actions Automated Release Deployment (CI/CD)

The repository provides automated CI/CD workflows out of the box:
- **Production Release Deployment** (`.github/workflows/release.yml`): Whenever a semantic version tag (e.g. `v2.3.3`) is pushed to GitHub, GitHub Actions builds client binaries and automatically deploys the updated Edge Worker to Cloudflare production (`p2p`).
- **Testing Continuous Deployment** (`.github/workflows/deploy-testing.yml`): Whenever changes land on `dev` or via manual trigger, updates deploy directly to `testing.p2p`.

#### Configuration Steps:
1. **Create Cloudflare API Token**:
   - Navigate to [Cloudflare API Tokens](https://dash.cloudflare.com/profile/api-tokens);
   - Select **Create Token** -> use **Edit Cloudflare Workers** template;
   - Copy the generated token.
2. **Add Encrypted Secrets to GitHub Repository**:
   - Go to your repository `Settings` -> `Secrets and variables` -> `Actions`;
   - Add two repository secrets:
     - `CLOUDFLARE_API_TOKEN`: Your Cloudflare API Token;
     - `CLOUDFLARE_ACCOUNT_ID`: Your Cloudflare Account ID.
3. **Automated Operation**:
   Subsequent releases will deploy seamlessly without manual intervention.

---

### 4. Zero-Leakage Private Custom Domain Deployment (Local Hook Mode)

If you prefer keeping credentials off GitHub or maintaining a custom domain privately:
1. Create a `wrangler.local.toml` file under `web/` (which is globally gitignored):
   ```toml
   routes = [
     { pattern = "p2p.yourdomain.com", custom_domain = true }
   ]
   ```
2. The local deployment wrapper (`web/scripts/deploy.mjs`) automatically prioritizes `wrangler.local.toml` locally while gracefully falling back to public `wrangler.toml` in CI.
3. The included `.git/hooks/pre-push` hook automatically triggers production deployment with your local domain whenever pushing a release tag, achieving 100% automated release with zero secret leakage.

---

## Option B: Self-Hosted Standalone Server (Single Binary)

The `signal` binary embeds all compiled frontend assets and features zero runtime dependencies.

### Running with Native TLS

```bash
# Auto-generates local self-signed ECDSA certificates for secure contexts
./signal -addr :443 -tls-auto
```

### Behind Nginx / Caddy Reverse Proxy

```bash
# Listen on internal port :8888
./signal -addr :8888 -signal-secret MasterSecretKey
```

**Nginx Configuration:**

```nginx
server {
    listen 443 ssl http2;
    server_name p2p.yourdomain.com;

    ssl_certificate /path/to/fullchain.pem;
    ssl_certificate_key /path/to/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8888;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### 3. Cross-Origin WebSocket Access Control (CSWSH & Allowed Origins)
When the Web portal frontend and the signaling server are hosted on distinct domains or ports (for example, frontend on Cloudflare Pages, S3/CDN, or local development ports while signaling runs on a standalone VPS):
- The signaling server enforces strict same-origin protection by default in v2.3+, rejecting non-whitelisted cross-origin WebSocket handshakes with `HTTP 403 Forbidden`;
- Whitelist trusted origins using the `-allowed-origins` CLI flag or `SIGNAL_ALLOWED_ORIGINS` environment variable, which supports decoupled subdomain and port wildcards in v2.3.1:
```bash
# Whitelist trusted frontend origins
./bin/signal_linux_amd64 -addr :443 \
  -tls-cert /path/to/fullchain.pem \
  -tls-key /path/to/privkey.pem \
  -allowed-origins "https://*.pages.dev,https://portal.yourdomain.com,http://localhost:*"
```
- In local development or isolated testing environments, pass `--allow-all-origins` (or `SIGNAL_ALLOW_ALL_ORIGINS=true`) to permit cross-origin signaling without a whitelist.

---

## Option C: Docker & Docker Compose Containerized Deployment

Official multi-architecture Docker images are published to GitHub Container Registry (`ghcr.io`):
- Server: `ghcr.io/beingnoability/webrtc-p2p-server:latest`
- Agent: `ghcr.io/beingnoability/webrtc-p2p-agent:latest`

### Docker Run (Least Privilege Non-Root)

All images run as independent unprivileged user `appuser` (`UID: 10001, GID: 10001`) by default:

```bash
# Run Server
docker run -d --name webrtc-server --restart unless-stopped \
  -p 8888:8888 \
  -e SIGNAL_SECRET=YourMasterSecretKey \
  ghcr.io/beingnoability/webrtc-p2p-server:latest

# Run Agent
docker run -d --name webrtc-agent --restart unless-stopped \
  --net=host \
  --user 1000:1000 \
  -e NODE_ID=nas-vault \
  -e SIGNAL_URL=wss://p2p.yourdomain.com/ws \
  -e TARGET_URL=http://127.0.0.1:8080 \
  -e SIGNAL_SECRET=YourMasterSecretKey \
  -e ACCESS_SECRET=YourPassword123 \
  ghcr.io/beingnoability/webrtc-p2p-agent:latest
```

### Docker Compose Template

```yaml
version: '3.8'

services:
  signal:
    image: ghcr.io/beingnoability/webrtc-p2p-server:latest
    container_name: webrtc-signal
    restart: unless-stopped
    ports:
      - "8888:8888"
    environment:
      - ADDR=:8888
      - SIGNAL_SECRET=YourMasterSecretKey
    healthcheck:
      test: ["CMD", "/app/server", "-health"]
      interval: 15s
      timeout: 5s
      retries: 3

  agent:
    image: ghcr.io/beingnoability/webrtc-p2p-agent:latest
    container_name: webrtc-agent
    restart: unless-stopped
    network_mode: "host"
    user: "1000:1000"
    environment:
      - NODE_ID=nas-vault
      - SIGNAL_URL=ws://127.0.0.1:8888/ws
      - TARGET_URL=http://127.0.0.1:8080
      - SIGNAL_SECRET=YourMasterSecretKey
      - ACCESS_SECRET=YourPassword123
```

### 4. Container Hardening & Non-Root Execution (`--user` / Least Privilege)

- **Least Privilege by Default**:
  - Official container images run under non-root user `appuser` (UID: 10001, GID: 10001) by default;
  - Conforms strictly to CIS Docker Benchmarks and Kubernetes Restricted Pod Security Standards, mitigating container escape risks.
- **Arbitrary UID/GID `--user` Support (Host Permission Alignment)**:
  - On Synology DSM, QNAP, Unraid, or custom Linux hosts, specify `--user $(id -u):$(id -g)` or `--user 1000:1000` to avoid file permission conflicts:
  ```bash
  docker run -d --name webrtc-agent --restart unless-stopped \
    --user 1000:1000 \
    --net=host \
    -v /volume1/media:/media:ro \
    -e NODE_ID=nas-vault \
    -e SIGNAL_URL=wss://p2p.yourdomain.com/ws \
    -e DIR_PATH=/media \
    -e ACCESS_SECRET=YourPassword123 \
    ghcr.io/beingnoability/webrtc-p2p-agent:latest
  ```
- **Privileged Port Capabilities (`CAP_NET_BIND_SERVICE`)**:
  - The `server` image is provisioned with `setcap 'cap_net_bind_service=+ep'`, allowing unprivileged users to bind directly to privileged low ports (`:443` or `:80`) without root access.
- **Read-Only Root Filesystem Compatibility (`--read-only`)**:
  - Both Signal server and Agent maintain purely stateless in-memory streaming pipelines, natively supporting `docker run --read-only` and Kubernetes `readOnlyRootFilesystem: true`.
