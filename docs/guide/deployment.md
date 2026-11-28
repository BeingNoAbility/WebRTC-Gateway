# 生产部署与两档沙箱规划 (Production Deployment)

WebRTC Gateway 以单一权威 Go 信令服务（`cmd/signal`）于启动期预渲染内容哈希前端资产并统一交付信令服务，提供两档明确的沙箱隔离模式（均在启动期显式配置 `-portal-origin` 与 `-sandbox-origin`，本地回环使用 `-dev`）。非回环生产部署需按以下规范准备域名、证书、持久化状态与容器配置。按具体拓扑与业务需求快速选用模板见 [服务端典型用例](./server-use-cases) 与 [Agent 典型用例](./agent-use-cases)。

---

## 1. 两档沙箱 DNS 与 TLS 证书规划

生产环境按域名与证书条件选择对应隔离档位（本机回环显式加 `-dev` 启用 `*.localhost`）：

| 档位 | 配置参数 | 必需 DNS 记录 | TLS 证书要求 | 隔离特性 |
| :--- | :--- | :--- | :--- | :--- |
| **Tier 1：跨站双域通配符（推荐）** | `-portal-origin https://portal.example.com` + `-sandbox-origin https://*.sandbox.example.net` | `portal.example.com` + `*.sandbox.example.net` | `portal.example.com` + 泛域 `*.sandbox.example.net` | 跨站站点级隔离 + 按节点本机私有盐 HMAC 派生 `sbx-<24hex>` 源（无警示条） |
| **Tier 2 受限：固定单域跨站** | `-portal-origin https://portal.example.com` + `-sandbox-origin https://app.example.net` | 两条普通 `A`/`AAAA`/`CNAME` | 单张含双域名 SAN 的 HTTP-01 证书（直连 `-tls-cert`），或由前置 Nginx/Caddy 分别托管两张单域证书 | Portal 与应用跨站，但多节点与能力访客共享单一沙箱源（Portal 持续显示 Tier 2 警示条） |
| **Tier 2 受限：单根域同站通配符** | `-portal-origin https://portal.example.com` + `-sandbox-origin https://*.sbx.example.com` + `-allow-same-site-sandbox` | `portal.example.com` + `*.sbx.example.com` | 泛域证书 `*.sbx.example.com` | 按节点独立 `sbx-<24hex>` 源 + `Origin-Agent-Cluster: ?1`，与 Portal 同站（Portal 持续显示 Tier 2 警示条） |

> [!NOTE]
> 多节点生产环境与外部能力分享推荐使用 **Tier 1 跨站双域通配符**；暂无泛域名证书能力且仅服务单节点或互信应用时，可选用 **Tier 2 固定单域跨站**（无需 DNS-01 通配符证书）。

---

## 2. 二进制构建与反向代理部署

运行 `make build` 编译内容哈希前端资产并以 `-tags embedweb` 按显式清单嵌入 `bin/signal` 单二进制：

```bash
make build

# Tier 1：跨站双域通配符部署（推荐）
SIGNAL_SECRET_FILE=/etc/webrtc-gateway/signal.secret \
./bin/signal \
  -addr 0.0.0.0:8888 \
  -portal-origin https://portal.example.com \
  -sandbox-origin 'https://*.sandbox.example.net' \
  -bound-keys-file /var/lib/webrtc-gateway/bound-keys.json \
  -trusted-proxies 127.0.0.0/8,::1/128,192.0.2.0/24
```

若前置 Nginx / Caddy 终止 TLS，务必透传原始 `Host`、`X-Forwarded-For` 并开启 WebSocket 升级，同时在 `-trusted-proxies` 中声明反代真实网段，避免所有访客共享同一 IP 限流配额（外部访问协议与端口由启动期 `-portal-origin` / `-sandbox-origin` 权威声明，不再依赖 `X-Forwarded-Proto` 运行时猜测）。多节点集群建议配置 `-node-secrets-file` 为每个 `node_id` 分配独立准入密钥（每条不少于 16 字节），使单节点凭据泄露无法接管其他节点。

---

## 3. 容器化加固部署 (`docker-compose.yml`)

仓库根目录提供的 `docker-compose.yml` 默认仅将信令明文端口绑定在 `127.0.0.1:8888:8888`（供宿主机反代终止 TLS），默认启用 `read_only: true`、`cap_drop: [ALL]`、`no-new-privileges:true` 与资源上限，并将信令节点公钥绑定表持久化至 `signal_state` 卷、将 Agent 的 Ed25519 私钥与能力吊销状态持久化至 `agent_identity` 卷（宿主机反代经 Docker 网桥访问容器时，须将对应网桥 CIDR 如 `172.17.0.0/16` 加入 `TRUSTED_PROXIES`）：

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

## 4. 无通配符证书场景：Tier 2 固定单域跨站部署示例

若仅有普通 HTTP-01 证书能力且仅部署单节点或互信自用应用，可准备两个不同根域的固定主机名（如 `portal.example.com` 与 `app.example.net`），通过显式固定 `-sandbox-origin` 启动（Portal 将持续提示多节点共享单源的 Tier 2 边界）。直接通过 `cmd/signal` 的 `-tls-cert` 终止 TLS 时须提供同时覆盖两个域名的单张多 SAN 证书；若希望两个域名各自使用独立单域证书，请在前置 Nginx 或 Caddy 上按 SNI 终止 TLS 并反代至本机 `127.0.0.1:8888`：

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
