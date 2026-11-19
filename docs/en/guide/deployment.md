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

1. Install Wrangler CLI:
   ```bash
   npm install -g wrangler
   ```
2. Deploy edge signaling worker:
   ```bash
   cd edge
   wrangler deploy
   ```
3. Your signaling URL is immediately ready:
   `wss://your-subdomain.workers.dev/ws`

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
