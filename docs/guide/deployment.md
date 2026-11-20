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

### 2. 构建与本地部署
```bash
# 1. 编译前端资源
cd web
npm install
npm run build

# 2. 部署至生产环境或测试环境
npm run deploy:prod      # 部署正式生产版 (p2p)
npm run deploy:testing   # 部署独立测试版 (testing.p2p)
```

部署完成后，单个 Worker 即**同时承载前端与 WebSocket 信令**：
- **Web 门户前端**：`https://p2p.yourdomain.com/`（或 `https://p2p.<subdomain>.workers.dev/`）
- **信令端点**：`wss://p2p.yourdomain.com/ws`

---

### 3. GitHub Actions 自动跟随发布教程 (CI/CD 自动化)

项目已内置完整的 GitHub Actions 自动化部署流水线：
- **生产环境自动发布**（`.github/workflows/release.yml`）：每当为仓库打上语义化版本 Tag（如 `v2.3.3`）并推送到 GitHub 时，GitHub Actions 会自动编译多平台客户端，并自动将最新代码部署到 Cloudflare 生产环境 Worker（`p2p`）。
- **测试环境持续部署**（`.github/workflows/deploy-testing.yml`）：每当 `dev` 分支发生代码更新或在 GitHub 页面手动触发时，自动部署到测试环境 Worker（`testing.p2p`）。

#### 自动化配置步骤（任何人复刻或自建均可开箱即用）：
1. **生成 Cloudflare 专用 API Token**：
   - 访问 [Cloudflare API 令牌管理面板](https://dash.cloudflare.com/profile/api-tokens)；
   - 点击 **创建令牌** -> 选择 **编辑 Cloudflare Workers** 模板；
   - 账户资源选择您的目标账户，点击保存并复制该 Token。
2. **在 GitHub 仓库添加加密 Secrets**：
   - 打开您的 GitHub 仓库，进入 `Settings` -> `Secrets and variables` -> `Actions`；
   - 点击 **New repository secret**，添加以下两项：
     - `CLOUDFLARE_API_TOKEN`：刚创建的 Cloudflare API 令牌；
     - `CLOUDFLARE_ACCOUNT_ID`：您的 Cloudflare 账户 ID（可在 Workers 仪表盘右侧栏获取）。
3. **完成配置**：
   配置完成后，后续所有版本发布（Release）均由 GitHub 全自动接管部署，零人工介入。

---

### 4. 专属私有域名的本地无感发布 (隐私安全模式)

若您不希望将 Cloudflare 凭据托管在 GitHub 上，或者希望将项目开源的同时为自己绑定专属私有自定义域名：

1. **本地专属域名隔离**：
   在 `web/` 目录下创建 `wrangler.local.toml`（此文件已被全局 `.gitignore` 强制忽略，永远不会被 Git 追踪或意外提交）：
   ```toml
   name = "p2p"
   main = "../edge/cloudflare-worker/worker.js"
   compatibility_date = "2024-04-03"
   assets = { directory = "./dist", binding = "ASSETS" }
   workers_dev = true

   routes = [
     { pattern = "p2p.yourdomain.com", custom_domain = true }
   ]

   [durable_objects]
   bindings = [
     { name = "ROOM", class_name = "SignalingRoom" }
   ]

   [[migrations]]
   tag = "v1"
   new_sqlite_classes = ["SignalingRoom"]

   [env.testing]
   name = "testing-p2p"
   workers_dev = true
   routes = [
     { pattern = "testing.p2p.yourdomain.com", custom_domain = true }
   ]

   [env.testing.durable_objects]
   bindings = [
     { name = "ROOM", class_name = "SignalingRoom" }
   ]

   [[env.testing.migrations]]
   tag = "v1"
   new_sqlite_classes = ["SignalingRoom"]
   ```
2. **智能部署脚本与本地 Git Hook 自动化**：
   - 项目内置的 `web/scripts/deploy.mjs` 会优先智能检测 `wrangler.local.toml`，存在时自动加载私有域名配置；在不存在的 CI 或他人环境则优雅回退至通用 `wrangler.toml`。
   - 本地已配置 `.git/hooks/pre-push` 钩子：每当您在本地执行 `git push origin main dev --tags` 推送正式版本标签时，Git 钩子会在本地自动调用当前已认证的 Wrangler 执行生产发布，实现**代码零泄漏、发版全自动**。

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

### 3. 跨源 WebSocket 访问控制配置 (CSWSH & Allowed Origins)
当 Web 门户前端与信令服务部署在不同域名或不同端口时（例如前端托管于 Cloudflare Pages、独立静态站或本地前端开发端口，而信令服务端运行于独立 VPS）：
- 信令服务端默认开启严格同源防御，非同源跨域握手将返回 `HTTP 403 Forbidden`；
- 通过 `-allowed-origins` 参数或 `SIGNAL_ALLOWED_ORIGINS` 环境变量配置受信来源白名单，支持子域名与端口通配符（支持域名与端口解耦匹配）：
```bash
# 启动时放行指定前端跨域来源
./bin/signal_linux_amd64 -addr :443 \
  -tls-cert /path/to/fullchain.pem \
  -tls-key /path/to/privkey.pem \
  -allowed-origins "https://*.pages.dev,https://portal.yourdomain.com,http://localhost:*"
```
- 本地开发测试环境下，可传入 `--allow-all-origins`（或 `SIGNAL_ALLOW_ALL_ORIGINS=true`）临时放行所有跨域信令请求。

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
> **容器健康检查机制**：
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
