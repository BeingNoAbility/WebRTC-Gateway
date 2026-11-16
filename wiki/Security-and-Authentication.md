# 安全机制与权限控制 (Security & Authentication)

WebRTC Gateway 遵循纵深防御（Defense-in-Depth）的安全设计理念，在传输层、控制面、数据面与应用沙箱中构筑了多层安全屏障。

---

## 1. 传输层原生端到端强加密 (DTLS-SRTP)

所有跨公网流经 WebRTC DataChannel 的流量，均强制经过 **DTLS 1.2 / 1.3** 握手与 **AES-128-GCM / AES-256-GCM** 端到端加密：
- **信令服务零知识 (Zero-Knowledge)**：信令服务器（无论是自建还是 Cloudflare Workers）仅在握手阶段路由 SDP 元数据，在数据传输阶段完全无法窥探、解密或篡改任何应用层 HTTP 请求与内容；
- **防中间人攻击 (MitM)**：客户端与 Agent 在 DTLS 握手中交换证书指纹（Fingerprint），确保通信对端真实性。

---

## 2. 控制面与数据面密钥解耦体系 (Dual-Secret Decoupling)

从 **v1.5.0** 起，系统彻底重构并解耦了旧版模糊的 `-secret`，划分为**控制面**与**数据面**两套相互独立的密钥系统：

```text
               [ 控制面: SIGNAL_SECRET ]
 Agent -------------------------------------> 信令服务器
       (仅用于 Agent 准入认证与防冒领注册)

               [ 数据面: ACCESS_SECRET ]
 访客 -------------------------------------> Agent
       (仅用于访客浏览器进入内网服务的口令校验)
```

### ① 信令集群准入密钥 (Control Plane: `-signal-secret` / `SIGNAL_SECRET`)
- **使用场景**：信令服务端与内网 Agent 共享；
- **作用域**：仅用于 Agent 注册时的准入控制与节点防冒领（`Node Hijack Protection`）；
- **安全保障**：任何外部恶意节点尝试抢注已有的 `node_id` 或接入集群时，若未提供匹配的 `signal-secret`，信令端直接拒绝注册；
- **完全保密**：**信令端绝不将此密钥告知访客浏览器**，外部访客亦无需知晓此密钥。

### ② 访客访问口令门禁 (Data Plane: `-access-secret` / `ACCESS_SECRET`)
- **公开免密模式（默认）**：
  - 当 `-access-secret` 留空时，该 Agent 为**真·公开免密节点**；
  - 访客在浏览器打开时直接打洞直连，零弹窗、零密码要求。即使信令服务器开启了严格的集群密钥，访客访问公开节点也畅通无阻；
- **受保护私有模式**：
  - 当配置了 `-access-secret` 时，访客进入 Web 门户将弹出暗黑风密码输入弹窗（或通过 URL Hash/Query 传递）；
  - Agent 在收到 WebRTC 握手 Offer 时进行口令比对，不匹配直接阻断连接，绝不泄露内网服务。

---

## 3. 无状态 HMAC-SHA256 时效安全分享链接

无需任何数据库、无需 Redis 缓存、无需维护撤销黑名单，纯数学方式实现限时安全分享：

```text
签名算法：
signature = HMAC-SHA256(nodeId + ":" + expTimestamp, access_secret)
```

### ① CLI 极速生成
```bash
./agent -node-id mynas \
        -target http://127.0.0.1:8096 \
        -access-secret myPassword \
        -sign-link 24h
```

### ② Web 控制台一键生成
在浏览器打开已连接的私有节点，点击右上角设置图标，在“安全时效分享”卡片中选择有效期（1小时、6小时、24小时、7天、30天），点击即可生成并复制专属安全直达链接：
👉 `https://p2p.yourdomain.com/#mynas?exp=1731852000&sign=3f89e...`

### ③ 核心优势
- 访客点击签名链接后直接进入服务，**无需输入密码**；
- 访客**无法反推出主口令**；
- 超过过期时间戳后，Agent 本地以毫秒级时间比对直接判定失效，彻底封死过期访问。

---

## 4. 生产容量治理与防 DoS 限流策略

### ① Agent 端并发流信号量防护 (`-max-streams`)
- Agent 维护一个带容量上限的逻辑流信号量（默认 512）；
- 当内网突发海量请求或遭受慢速爬虫抓取时，超出阈值的请求由 Agent 直接在协议层快速回复 `503 Service Unavailable`，防止内网目标应用（如轻量 NAS）内存溢出 (OOM) 崩溃。

### ② 服务端单 IP 并发连接限制 (`-max-ip-conns`)
- 服务端跟踪每个对端 IP 的活跃 WebSocket 握手连接数（默认 64）；
- 超出限制的新握手请求被立即拒绝，有效遏制恶意脚本耗尽信令服务端文件描述符与套接字资源。

### ③ 智能受信代理过滤 (`-trusted-proxies`)
- 默认自动信任标准私网 CIDR（`127.0.0.1/32`, `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` 等回环与 Docker 网桥）；
- 当服务端直接裸跑在公网且请求来自非受信 IP 时，强制忽略伪造的 `X-Forwarded-For` 请求头，直接取底层套接字的真实 `RemoteAddr`，彻底封死 IP 伪造漏洞。

---

## 5. 设计哲学与安全边界声明 (Design Philosophy & Security Boundaries)

为了守住产品的核心护城河与轻量极致体验，本项目坚定恪守以下两条设计纪律：

### ① 无状态网络门禁定位 (Stateless Transport Gatekeeper)
- `-access-secret` 与 HMAC 时效签名本质上是**网络传输层的 P2P 隧道准入门禁（L4/L7 Gatekeeper）**；
- 它的使命是：以极速纯数学验证（HMAC-SHA256）、零中心化数据库依赖、零 Redis 内存表阻断非授权访客建立 WebRTC 物理隧道；
- **更细粒度的业务安全职责划分**：细粒度用户账户体系、多租户 RBAC 权限管理、OAuth2/OIDC 单点登录以及单会话即时撤销，属于内网各 Web 应用（如 Nextcloud、Jellyfin、Vaultwarden、Gitea）或上层专职反代（如 Authelia / Keycloak）的职责。网关坚决不做业务层的大一统身份认证中心。

### ② 原生拓扑免疫 SSRF (Native SSRF Immunity by Design)
- Agent 守护进程采用严格的**静态单向服务绑定**（`-target http://127.0.0.1:8080`、`-dir` 或 `-file`）；
- 访客端建立 WebRTC 隧道后，其所有 HTTP 请求完全被静态转发至该单向绑定的目标地址，Agent 绝不提供“由访客指定任意目标 IP 或外网 URL 代发”的能力；
- 因此，本项目在拓扑与架构层面**原生免疫 SSRF（服务端请求伪造）**，无需也绝不引入脆弱复杂的内部 IP/CIDR 黑白名单过滤器。
