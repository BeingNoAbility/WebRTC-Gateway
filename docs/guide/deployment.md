# 生产部署指南 (Deployment Guide)

WebRTC Gateway 提供三种生产级自建与托管方案，适配不同的网络环境、硬件设施与成本预算：

| 方案 | 运行载体 | 成本 | 架构复杂度 | 适用场景 |
| :--- | :--- | :--- | :--- | :--- |
| **方案 A：Cloudflare Workers 边缘部署** | Cloudflare 全球边缘网络 | **0 元** (免费额度充足) | 零基础设施维护 | 个人开发者、无独立公网 VPS、追求全球毫秒级访问 |
| **方案 B：自建 Linux / Windows 单二进制** | 云服务器 / 独立 VPS | 自备 VPS | 极简单文件运行 | 追求信令完全私有化、数据自主合规 |
| **方案 C：Docker & Docker Compose 编排** | Linux / NAS / 树莓派 | 容器化基础设施 | 一键脚本标准化管理 | 生产环境集群化部署、自动化 CI/CD 集成 |

---

## 方案 A：Cloudflare Workers 一体化边缘部署 (强烈推荐)

利用 Cloudflare Workers 的全球 300+ Anycast 边缘节点，搭配 Durable Objects 负责 WebSocket 信令撮合，同时托管静态门户前端。**无需公网云服务器，0 元月租，全球毫秒级接入**。

### 1. 前置准备
- 注册 Cloudflare 账号；
- 本地安装 Node.js (v18+) 与 npm。

### 2. 构建与部署
```bash
# 1. 编译前端资源
cd web
npm install
npm run build

# 2. 部署至 Cloudflare Workers
npx wrangler deploy
```

部署完成后，单个 Worker 即**同时承载前端与 WebSocket 信令**：
- **Web 门户前端**：`https://p2p.yourdomain.com/`
- **信令端点**：`wss://p2p.yourdomain.com/ws`

在 Cloudflare 控制台为该 Worker 绑定自定义域名（例如 `p2p.yourdomain.com` 及泛域名 `*.p2p.yourdomain.com`），即刻享受开箱即用的多级子域名智能路由。

---

## 方案 B：自建 Linux / Windows 一体化自包含服务器 (单一二进制)

如果您希望将信令与门户完全托管在自己的独立 VPS 或私网机房中，项目提供了**自包含一体化二进制**。

Go 二进制通过 `go:embed` 技术将编译好的 Web 门户（HTML、JS、Service Worker）直接内嵌进单一可执行文件，**零外部静态文件依赖，不依赖 Node.js，不依赖 Nginx**。

### 1. 启动方式

#### 方式 1：加载已有 SSL 域名证书（生产环境首选）
```bash
# Linux
./bin/signal_linux_amd64 -addr :443 -tls-cert /path/to/fullchain.pem -tls-key /path/to/privkey.pem

# Windows
.\bin\signal.exe -addr :443 -tls-cert C:\certs\fullchain.pem -tls-key C:\certs\privkey.pem
```

#### 方式 2：局域网 / 开发环境一键开启原生 HTTPS（无需证书文件，自动签发）
在启动时加上 `-tls-auto` 参数，服务端将在内存中自动生成覆盖 localhost 及局域网所有真实 IP 的 ECDSA SAN 证书：
```bash
./bin/signal_linux_amd64 -addr :8888 -tls-auto
```
*启动后直接在浏览器打开 `https://<局域网IP>:8888/#nodeId`，浏览器即可进入安全上下文（Secure Context），Service Worker 完整激活！*

#### 方式 3：纯 HTTP 运行（本地开发或前置自有反代）
```bash
./bin/signal_linux_amd64 -addr :8888
```

### 2. 前置 Nginx 反向代理配置示例
若服务端前置了 Nginx，请务必开启 WebSocket Upgrade 并正确传递 IP 头：
```nginx
server {
    listen 443 ssl http2;
    server_name p2p.yourdomain.com *.p2p.yourdomain.com;

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

## 方案 C：Docker & Docker Compose 一键编排容器部署

项目内置精简的多阶段构建 Dockerfile，基础镜像基于 Alpine 3.20，全面支持 x86_64 及 ARM64 跨平台架构：
- **服务端镜像 (`Dockerfile.server`)**：约 **16MB**
- **Agent 镜像 (`Dockerfile.agent`)**：纯 Go 静态编译守护进程，约 **20MB**

### 1. 拉取官方预构建多架构镜像
```bash
# 服务端 (含前端与信令)
docker pull ghcr.io/beingnoability/webrtc-p2p-server:latest

# Agent 客户端
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:latest
```

### 2. 标准 Docker Compose 一键启动
根目录下提供了开箱即用的 `docker-compose.yml`：
```bash
# 启动所有服务（后台守护运行）
docker compose up -d

# 查看实时运行日志与握手状态
docker compose logs -f

# 停止并移除容器
docker compose down
```

> [!TIP]
> **容器健康检查机制 (v1.6.2+)**：
> 服务端镜像内置了原生健康探测器（`CMD /app/server -health`），自动适配 HTTP/HTTPS 及自定义端口，支持自签名证书并即刻反馈健康状态（状态流转为 `healthy`），避免 Alpine Busybox wget 参数兼容问题。

### 3. 经典 NAS / 私有服务器纯 Agent 模式编排
在家庭 NAS、树莓派或内网服务器上，通常**只需运行 Agent 容器**连接公网信令：
```yaml
# docker-compose.agent-only.yml
services:
  agent:
    image: ghcr.io/beingnoability/webrtc-p2p-agent:latest
    container_name: webrtc-p2p-agent
    restart: unless-stopped
    # 强烈推荐 host 模式，以便直接穿透宿主机 127.0.0.1 上的各类服务
    network_mode: "host"
    environment:
      - NODE_ID=my-nas
      - SIGNAL_URL=wss://p2p.yourdomain.com/ws
      - TARGET_URL=http://127.0.0.1:8096  # 指向本地 Jellyfin / Alist / 私有云
      - SIGNAL_SECRET=your_cluster_secret # 可选集群准入密钥
      - ACCESS_SECRET=your_visitor_password # 可选访客访问口令
    volumes:
      - /etc/localtime:/etc/localtime:ro

### 4. 容器安全加固与非特权用户支持 (`--user` / Non-Root)

- **默认非特权运行 (Least Privilege by Default)**：
  - 镜像默认已剔除 root 特权，以独立非特权用户 `appuser` (UID: 10001, GID: 10001) 运行；
  - 严格遵循 CIS Docker Benchmark 与 Kubernetes Restricted Pod Security Standard，杜绝潜在的容器逃逸与提权风险。
- **全格式支持 `--user` 任意指定身份 (NAS / 宿主机权限对齐)**：
  - 在群晖 Synology、威联通 QNAP、Unraid 或 Linux 服务器上挂载本地目录/文件时，可直接传入 `--user $(id -u):$(id -g)`、`--user 1000:1000` 或 `--user nobody`，杜绝权限不足或文件属主冲突：
  ```bash
  # 以 NAS 宿主机 1000:1000 身份启动 Agent 并只读挂载媒体库
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
- **特权端口能力集绑定 (`CAP_NET_BIND_SERVICE`)**：
  - `server` 镜像在构建时通过 Linux File Capability 为 `/app/server` 授予 `cap_net_bind_service=+ep`，即便以非特权用户身份运行，也支持直接在容器内监听 `:443` 或 `:80` 等低位特权端口，无需赋予容器 root 权限。
- **纯内存只读文件系统兼容 (`--read-only`)**：
  - 服务端与 Agent 全链路采用内存流式无状态调度，不向磁盘写任何临时文件，原生支持 `docker run --read-only` 与 Kubernetes `readOnlyRootFilesystem: true` 金融级沙箱加固。
