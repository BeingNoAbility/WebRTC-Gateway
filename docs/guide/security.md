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
- **受保护私有模式 (v2.3+ DTLS 证书指纹联合绑定)**：
  - 当配置了 `-access-secret` 时，访客进入 Web 门户将弹出暗黑风密码输入弹窗；
  - **单向派生鉴权密钥 (F-02)**：前端通过 `HMAC-SHA256(access_secret, "node-auth:" + nodeId)` 生成单向派生密钥 `authKey`，绝不在网络中明文传输主口令；
  - **DTLS 证书指纹联合防重放 (F-04)**：客户端生成 WebRTC Offer 时提取本端物理 DTLS 证书指纹，计算 `HMAC-SHA256(authKey, nodeId + ":" + exp + ":" + sdpFingerprint)`。自 v2.3 起彻底出清旧版明文口令握手，即便握手报文在公网遭窃听，攻击者因不同终端物理生成的 DTLS 证书指纹不同而无法跨机重放，连接直接被 100% 拒绝。

---

## 3. 无状态 HMAC-SHA256 时效安全分享链接

无需任何数据库、无需 Redis 缓存、无需维护撤销黑名单，纯数学方式实现限时安全分享：

```text
基础签名算法：
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

### ④ DTLS 证书指纹联合防窃听重放 (v2.3+)
- 在 v2.3 及以上版本中，访客端打开安全分享链接发起 WebRTC 建联时，前端提取本端动态生成的 DTLS 证书指纹，在 `sign` 的基础上进一步计算联合签名 `HMAC-SHA256(sign, nodeId + ":" + exp + ":" + sdpFingerprint)`；
- 彻底防范“攻击者在中间信道窃听到未过期的合法分享链接 URL 后，在第三方设备上冒领建立 P2P 隧道”的重放威胁；
- 网关强制出清历史技术债务，对未结合指纹签名的旧协议握手实施物理级 100% 阻断。

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
- 当服务端直接裸跑在公网且请求来自非受信 IP 时，强制忽略伪造的 `X-Forwarded-For` 请求头，直接取底层套接字的真实 `RemoteAddr`，防范 IP 伪造风险。

---

## 5. 权威真实客户端 IP 穿透与 WAF 协同 (Authoritative Client IP & WAF Integration)

自 **v1.8.0** 起，网关全面支持生产级真实客户端 IP 透传与防伪体系，支持权威客户端 IP 透传 (X-Forwarded-For)，解决穿透场景下目标后端将所有流量误识别为 `127.0.0.1` 的问题：

### ① 权威源头注入与防伪 (Anti-Spoofing Anchor)
- **信令端物理覆盖**：客户端在发起 WebRTC 握手时，信令服务器权威提取物理套接字 IP（或 Cloudflare / 受信反代源 IP），在转发 Offer 时**强行覆盖 `msg.ClientIP = session.RemoteIP`**，恶意客户端在 JSON 信令中自报的任何伪造 IP 被直接抹除；
- **边缘信令对齐**：Cloudflare Workers 边缘信令通过 Durable Object 同步提取 `cf-connecting-ip` 注入 `msg.client_ip`，双信令形态无缝平替；
- **Agent 代理层强制覆写**：Agent 在反向代理 HTTP 与 WebSocket 请求时，在解析完客户端请求头之后，执行 `req.Header.Set("X-Forwarded-For", clientIP)` 与 `req.Header.Set("X-Real-IP", clientIP)`，覆盖客户端在 DataChannel 中夹带的伪造头。

### ② WAF 与安全组件协同
- **精准单 IP 封禁与黑白名单**：雷池 SafeLine、Nginx WAF、ModSecurity 可直接拦截恶意公网 IP，不会导致 `127.0.0.1` 封禁全员误杀；
- **单 IP 频率限制 (Rate Limiting)**：单 IP 刷接口触发 429 仅针对该黑客 IP，正常访客完全不受影响；
- **日志审计与 GeoIP 地理溯源**：Nginx / WAF 访问日志中完整保留真实访客公网 IP 与地理位置证据链。

### ③ 移动端漫游原地热重连 (In-place ICE Restart Roaming Update)
- 访客在手机 Wi-Fi 与 5G 移动蜂窝网络之间漫游切换时，原地 ICE Restart 保持 DataChannel 连接不中断，Agent 同步动态更新绑定的公网出口 IP，后续流经隧道的新请求立即自动按新 IP 执行 WAF 审计与防护规则。

---

## 6. 通用防御加固与收敛治理 (v1.8.1 Universal Security Hardening)

网关自 **v1.8.1** 起，建立通用防御闭环：

### ① 门户控制台 DOM XSS 防御
- **正向字符集门禁**：对所有节点 ID（URL Hash、Query、子域名、Storage）实行强制白名单（`/^[a-zA-Z0-9._-]{1,64}$/`），非规范字符直接拒绝并降级兜底；
- **原生安全 DOM 赋值**：设置弹窗废除模板字符串拼接，改由标准 DOM API 属性设值（`input.value = ...`）与 HTML 实体转义。

### ② 全链路最小特权口令切断 (PoLP)
- 物理拔除 Service Worker 对代理 HTML 页面的 `window.__P2P_SECRET__` 注入，目标站点内部任何脚本均无法嗅探口令；
- 移除 URL Query 中 `?secret=` 的拼接暴露；移除信令 `join` 握手阶段及 iframe 广播的冗余口令字段。

### ③ `/--p2p-proxy/` 外域代发安全收敛
- 彻底废除基于后缀模糊匹配（`.mp4` 参数绕过）及关键字匹配的启发式规则；
- 默认仅允许同源及根域名子域代理；新增 `-allowed-proxy-domains` 通配符白名单配置，实现通用且安全的外链多媒体与图床直连；
- `-proxy` 严格解耦于信令外出通道，不污染本地反向代理 Transport。

### ④ 信令滑动窗口防暴破与敏感端点收敛
- 在 WebSocket 信令握手阶段实行 `IsLocked` 滑动窗口熔断阻断，封禁期间硬性断开连接；
- 彻底移除 Cloudflare Worker 未鉴权的 `list_nodes` 节点枚举端点。

---

## 7. 控制面与数据面纵深防御治理 (v2.2 Resource Governance & Defenses)

自 **v2.2.0** 起，网关对控制面信令与数据面传输实施更严格的配额治理与防 DoS 加固：

### ① 跨站 WebSocket 劫持防御与分流准入 (CSWSH & Origin Whitelisting)
- **浏览器环境强制校验**：针对携带 `Origin` 请求头的浏览器客户端（Web Portal / 访客），信令服务端通过 `-allowed-origins` / `SIGNAL_ALLOWED_ORIGINS` 配置严格白名单。支持通配符子域（如 `https://*.pages.dev`）与端口通配（如 `http://localhost:*`），非法来源立即返回 `HTTP 403 Forbidden`；
- **同源自动信任**：默认信任当前信令服务主机同源（`Origin` 主机与 `Host` 一致），避免合法部署产生误报；
- **Native / CLI 链路独立放行**：Go CLI Agent、curl 或原生非浏览器客户端不携带 `Origin` 请求头，系统智能识别并放行 WebSocket Upgrade，随后在信令消息层通过 `SIGNAL_SECRET` / HMAC 防重放机制进行强鉴权，绝不因跨域策略阻断命令行守护进程；
- **向下兼容安全阀**：未显式配置白名单时，保持放行并输出安全提示，防止存量无配置环境升级后服务中断。

### ② 控制面会话配额与频控防 DoS (Control Plane Quotas & Janitor)
- **单会话 Candidate 转发限额**：单 Session ICE Candidate 转发上限限制为 **32 条**，与 Agent 端 `MaxCandidatesPerSession = 32` 形成端到端双重拦截闭环，超出限制立即丢弃并记录限频日志；
- **离线等待队列容量硬顶**：当目标 Agent 处于离线状态时，单节点等待队列上限限制为 **128 个**，超出上限直接拒绝新访客排队，防止恶意堆积半开会话耗尽信令服务端内存；
- **半开 SDP 握手主动超时回收**：访客下发 Offer 后若 30 秒内未收到对应 Answer，后台 Janitor Loop 自动主动关闭连接并回收会话，彻底消除僵尸半开握手。

### ③ Mux 全局 Inbound 内存预算与背压流控 (Global Inbound Budget & Backpressure)
- **128MB 全局内存配额**：在多路复用层引入全局 Inbound 内存上限（默认 128MB），防止慢速消费者通过并发多流恶意压爆 Agent 内存（消除 $512 \times 32\text{MB} = 16\text{GB}$ 理论风险）；
- **双水位背压与 SCTP 滑动窗口联动**：积压达到 96MB 高水位时，数据帧消费挂起进入背压，暂停从底层套接字读取，自然促使物理传输层 SCTP 接收通告窗口（a_rwnd）收缩；回落至 64MB 低水位时广播唤醒；
- **超时精准熔断与零泄漏记账**：背压等待超过 3 秒仍未缓解的慢速流被单点发送 `RST_STREAM` 熔断，绝不影响同连接其他流；流被重置或关闭时未读残留缓冲原子扣减并严格归零，彻底杜绝内存计数器幽灵虚高。

### ④ 生产安全模式加固与 TTY 友好型告警 (Security Warning Banner)
- **未配置密钥醒目告警**：当 Signal 或 Agent 在未配置 `SIGNAL_SECRET` 与 `ACCESS_SECRET` 的情况下启动时，控制台输出高对比度安全警告，提示当前运行于“公开/匿名模式”及潜在暴露风险；
- **智能 TTY 探测**：自动识别字符终端与重定向环境，终端环境启用高对比度 ANSI 黄色高亮，非终端（日志重定向、Docker、Kubernetes）自动剥离 ANSI 转义字符，保障运维日志检索无乱码。

---

## 8. 设计哲学与安全边界声明 (Design Philosophy & Security Boundaries)

为了守住产品的核心护城河与轻量极致体验，本项目坚定恪守以下两条设计纪律：

### ① 无状态网络门禁定位 (Stateless Transport Gatekeeper)
- `-access-secret` 与 HMAC 时效签名本质上是**网络传输层的 P2P 隧道准入门禁（L4/L7 Gatekeeper）**；
- 它的使命是：以极速纯数学验证（HMAC-SHA256）、零中心化数据库依赖、零 Redis 内存表阻断非授权访客建立 WebRTC 物理隧道；
- **更细粒度的业务安全职责划分**：细粒度用户账户体系、多租户 RBAC 权限管理、OAuth2/OIDC 单点登录以及单会话即时撤销，属于内网各 Web 应用（如 Nextcloud、Jellyfin、Vaultwarden、Gitea）或上层专职反代（如 Authelia / Keycloak）的职责。网关坚决不做业务层的大一统身份认证中心。

### ② 原生拓扑免疫 SSRF (Native SSRF Immunity by Design)
- Agent 守护进程采用严格的**静态单向服务绑定**（`-target http://127.0.0.1:8080`、`-dir` 或 `-file`）；
- 访客端建立 WebRTC 隧道后，其所有 HTTP 请求完全被静态转发至该单向绑定的目标地址，Agent 绝不提供“由访客指定任意目标 IP 或外网 URL 代发”的能力；
- 因此，本项目在拓扑与架构层面**原生免疫 SSRF（服务端请求伪造）**，无需也绝不引入脆弱复杂的内部 IP/CIDR 黑白名单过滤器。
