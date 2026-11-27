# 生产部署与三档沙箱规划 (Production Deployment)

WebRTC Gateway 以单一权威 Go 信令服务（`cmd/signal`）统一交付内容哈希前端资产与信令服务，并提供三档明确的沙箱隔离模式。非回环生产部署需按以下规范准备域名、证书、持久化状态与容器配置。按具体拓扑与业务需求快速选用模板见 [服务端典型用例](./server-use-cases) 与 [Agent 典型用例](./agent-use-cases)。

---

## 1. 三档沙箱 DNS 与 TLS 证书规划

生产环境按域名与证书条件选择对应隔离档位（本机回环自动启用 `*.localhost`）：

| 档位 | 配置参数 | 必需 DNS 记录 | TLS 证书要求 | 隔离特性 |
| :--- | :--- | :--- | :--- | :--- |
| **Tier 1：跨站双固定域** | `-portal-domain portal.example.com` + `-sandbox-host app.example.net` | 两条普通 `A`/`AAAA`/`CNAME` | 两张普通 ACME HTTP-01 单域证书（零泛域证书） | 跨站站点级隔离（不同 eTLD+1）；所有应用共享固定沙箱源 |
| **Tier 2：单根域同站通配符** | `-portal-domain portal.example.com` + `-sandbox-domain sbx.example.com` + `-allow-same-site-sandbox` | `portal.example.com` + `*.sbx.example.com` | 泛域证书 `*.sbx.example.com` | 按节点独立 `sbx-<24hex>` 源 + `Origin-Agent-Cluster: ?1` 进程隔离（同站警示条） |
| **Tier 3：跨站双域通配符（推荐）** | `-portal-domain portal.example.com` + `-sandbox-domain sandbox.example.net` | `portal.example.com` + `*.sandbox.example.net` | `portal.example.com` + 泛域 `*.sandbox.example.net` | 跨站站点级隔离 + 按节点本机私有盐 HMAC 派生 `sbx-<24hex>` 源 |

> [!NOTE]
> 当节点较少且暂无泛域名证书自动化能力时，优先选用 **Tier 1 跨站双固定域**（无需 DNS-01 通配符证书即可获得完整的跨站双域隔离）；多节点生产环境推荐使用 **Tier 3 跨站双域通配符**。

---

## 2. 二进制构建与反向代理部署

运行 `make build` 编译内容哈希前端资产并以 `-tags embedweb` 按显式清单嵌入 `bin/signal` 单二进制：

```bash
make build

# Tier 3：跨站双域通配符部署（推荐）
SIGNAL_SECRET_FILE=/etc/webrtc-gateway/signal.secret \
./bin/signal \
  -addr 0.0.0.0:8888 \
  -portal-domain portal.example.com \
  -sandbox-domain sandbox.example.net \
  -bound-keys-file /var/lib/webrtc-gateway/bound-keys.json \
  -trusted-proxies 127.0.0.0/8,::1/128,192.0.2.0/24
```

若前置 Nginx / Caddy 终止 TLS，务必透传原始 `Host`、`X-Forwarded-For`、`X-Forwarded-Proto` 并开启 WebSocket 升级，同时在 `-trusted-proxies` 中声明反代真实网段，避免所有访客共享同一 IP 限流配额。多节点集群建议配置 `-node-secrets-file` 为每个 `node_id` 分配独立准入密钥，使单节点凭据泄露无法接管其他节点。

---

## 3. 容器化加固部署 (`docker-compose.yml`)

仓库根目录提供的 `docker-compose.yml` 默认启用 `read_only: true`、`cap_drop: [ALL]`、`no-new-privileges:true` 与资源上限，并将信令节点公钥绑定表持久化至 `signal_state` 卷、将 Agent 的 Ed25519 私钥与能力吊销状态持久化至 `agent_identity` 卷：

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

## 4. 无通配符证书场景：Tier 1 双固定域部署示例

若仅有普通 HTTP-01 单域证书，只需准备两个不同根域的固定主机名（如 `portal.example.com` 与 `app.example.net`），通过 `-sandbox-host` 启动即可享受跨站双域隔离与严格 W3C Fetch 抗 CSRF 门禁：

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
