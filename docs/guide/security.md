# 安全机制与权限控制 (Security & Authentication)

WebRTC Gateway 采用明确划分信任根的纵深防御架构，覆盖结构化 SDP 合成、Ed25519 身份验签、DTLS 通道内 RFC 9383 SPAKE2+ 认证、跨站双域沙箱与代理边界校验。

---

## 1. 结构化 SDP、传输层加密与 Ed25519 节点身份

WebRTC DataChannel 通过 DTLS 1.2 加密（SCTP over DTLS，RFC 8261 / RFC 8831），信令服务器不中继业务数据。为从根源消除 SDP 解析差异、分隔符注入及信令侧中间人攻击，采用结构化 `SessionDescriptor` + 持久化 **Ed25519 Agent 身份密钥** + 远端真实 DTLS 证书校验：

```mermaid
flowchart LR
    Agent["Agent (持久化 Ed25519 私钥)"] -->|"签名长度前缀结构化 Offer/Answer 转录"| Signal["信令服务器 (零数据中继 / 前端静态资产 TCB)"]
    Signal -->|"转发结构化 Answer + pk + sig"| Portal["浏览器 Portal"]
    Portal -->|"验签 + pk=/TOFU + 合成 16 行 SDP + 校验真实 DTLS 证书"| Decision{"全部校验通过?"}
    Decision -->|"通过"| DTLS["DTLS 控制通道执行 SPAKE2+ / 能力令牌认证"]
    Decision -->|"任一失败"| Block["红屏硬阻断并中止 PeerConnection"]
```

- **诚实信任边界（前端 TCB 与零数据中继）**：信令服务器不中继任何应用层明文或业务流量，且无权读取或离线暴破 DTLS 内的访客口令；但在纯浏览器零客户端模式下，Go `signal` 负责向浏览器交付 Portal、Service Worker 与 `patch.js` 静态资产，因此信令主机及反代处于**前端静态资产 TCB** 内。Portal CSP 严格锁定 `connect-src 'self'` 并禁止运行时 `?signal=` 覆写，所有前端资产于启动期按清单预渲染并带内容哈希与 SHA-256 `ETag`；
- **零原始对端 SDP 传递**：信令仅传输严格校验的 `SessionDescriptor`（`fp`、`ice_ufrag`、`ice_pwd`、`setup`、`mid`、`sctp_port`、`max_msg_size`、`sdp_ver`），双方本地确定性合成最小 16 行 RFC 8841 DataChannel-only SDP，从结构上消除多 `m=` 节与双指纹解析差异；
- **全模式 Pure Ed25519 验签与真实 DTLS 证书绑定**：无论是否启用 `-allow-public`，Agent 对长度前缀转录（`p2pgw-v7:answer-v2:`）做 Pure Ed25519 签名；Agent 侧始终校验 Pion `GetRemoteCertificate()`，浏览器侧在支持 `RTCDtlsTransport.getRemoteCertificates()` 的引擎（Chromium / Firefox）上直接校验远端真实证书指纹（在不支持该 API 的 WebKit/Safari 上依赖浏览器内置 DTLS 栈对已签名 SDP 指纹行的校验）；
- **公钥钉扎与 Fail-Closed SSH 风格 TOFU**：
  - 带 `#node?pk=<22字符SHA256摘要>` 的链接执行强钉扎校验且不可绕过；若节点已在 `IndexedDB` 记录不同 TOFU 指纹，URL `?pk=` 不得静默覆盖既有信任；
  - 裸 `#node` 访问在完成 Ed25519 验签与通道认证后才提交 TOFU 记录；`IndexedDB` I/O 失败严格 Fail-Closed；公钥变更在建联前即弹出红屏阻断。

---

## 2. 控制面与 DTLS 通道内 RFC 9383 SPAKE2+ 认证

### 1. 控制面：信令集群准入 (`SIGNAL_SECRET` / `-node-secrets-file`)

- Go 信令在 **HTTP WebSocket Upgrade 阶段**先校验 Origin 与合法 Upgrade 请求头（拒绝携带请求体的升级请求并强制静态读写超时），再执行 `Authorization: Bearer <secret>` 校验与速率限流；
- 全局 `SIGNAL_SECRET` 与逐节点隔离文件 `-node-secrets-file` 均强制最小 **16 字节**熵长度（命中逐节点密钥时禁止回退至全局密钥，且采用 `(IP, nodeID)` 分片锁定避免单节点误配连坐其他节点）；
- 非显式开启 `-allow-anonymous-agents` 时，未配置任何注册密钥直接 Fail-Closed 拒绝启动；
- 节点首次注册通过长度前缀 `p2pgw-v7:register-v2:` Ed25519 挑战绑定归属，支持 `-bound-keys-file` 经 `AtomicFileStore` 跨重启原子持久化；共享 `SIGNAL_SECRET` 下默认关闭 `-allow-force-rekey`（逐节点密钥下默认开启），轮换密钥需服务端允许且 Agent 持合法密钥传入 `-force-rekey`。

### 2. 数据面：DTLS `p2pctrl` 内 RFC 9383 SPAKE2+ 认证

- **零离线暴破预言机**：访客口令（`ACCESS_SECRET`，与 `-allow-public` 互斥，并存启动即拒）结合已校验的 `agent_pk` 与 `node_id` 经 1-Block `PBKDF2-HMAC-SHA256`（300,000 次迭代）+ `HKDF-SHA256`（`p2pgw-v7:spake2p-w0w1`）派生为 SPAKE2+ 验证子（`w0`, `L = w1·G`），并在 DTLS `p2pctrl` 内完成 **RFC 9383 SPAKE2+（P-256）双向零知识认证**（验证双方 `confirm_p` 与 `confirm_v`，严格要求完成远端证书指纹核验并下发挑战后方受理 `AUTH_RESPONSE`，拒绝降级与认证前任何数据通道业务帧）；
- **先输入口令后建联与单连接单次尝试**：Portal 在收到 `JoinedDTO` 后先展示口令弹窗，待用户输入完毕再建立 `RTCPeerConnection`，避免人类输入耗时触发 10 秒 `UnauthSessionTimeout`；每条连接严格仅允许 1 次认证尝试；认证完成后 Portal 立即关闭信令 WebSocket；
- **抗 IPv6 轮换、单 IP 串行化与全局 Fail-Closed 限流**：`VisitorAuthLimiter` 先对 IPv4-mapped IPv6 调用 `Unmap()` 再将真实 IPv6 归一至 `/64` 前缀，同时执行单 IP 并发握手上限（`4`）、单 IP 认证尝试串行化、活跃 5 分钟锁定永不驱逐的 LRU 表、表满 Fail-Closed 及全局失败速率上限（60 次/分钟），且对信令传入的 `ClientIP` 严格按 `netip.ParseAddr` 校验；
- **统一静默重连**：断连时按需重连信令、新建 `RTCPeerConnection` 并复用当前 Tab 内存派生的 `(w0, w1)` 静默完成完整 SPAKE2+ 重认证，不落盘明文口令且保活 `appFrame`。

---

## 3. Ed25519 能力分享令牌与持久化吊销

临时外部分享采用由 Agent Ed25519 私钥签名的结构化能力令牌（Capability Token）：

```json
{
  "v": 7,
  "jti": "unique-token-id",
  "node": "homeserver",
  "exp": 1795752000,
  "path_prefix": "/public",
  "readonly": true,
  "max_conns": 4
}
```

- **零密码再输入签发、跨进程锁合并与实时持续吊销**：Owner 可在 Portal 分享弹窗内经分片 `p2pctrl` 的 `CAP_REQ` 签发、列举并即时吊销能力令牌，也可用 CLI `./bin/agent -mint-link 24h -mint-readonly -mint-path /public` 离线签发；`-state-file` 经跨进程文件锁 + `AtomicFileStore` 读盘并集合并（`revoked` 只增不减）原子持久化，活跃 Agent 按文件变更轻量重载并立即使吊销与新签发记录生效；Portal 载入分享链接后立即使用 `history.replaceState` 清洗地址栏中的 `cap=` 参数；
- **后置规范化范围与独立会话隔离**：Agent 在 `DestinationPolicy.Resolve` 规范化路径（叠加 `TargetSubpath`）之后才校验 `path_prefix` 与 `readonly`（并剥离 `X-HTTP-Method-Override`），运行时同步检查令牌过期，并为每个能力访客分配独立 `guestPool` 随机 `ProxyCookieJar`（`cap:<jti>:<rand>`），杜绝同链访客共享 `HttpOnly` 会话；
- **安全声明说明**：分享链接本质为持有者令牌（Bearer Capability），请为外部分享设置短有效期、最小路径前缀与只读权限，并在不需要时即时吊销。

---

## 4. 两档沙箱隔离、W3C Fetch 抗 CSRF 门禁与统一 Cookie 保护

- **启动期显式源与两档沙箱隔离**：信令启动期要求显式配置 `-portal-origin` 与 `-sandbox-origin`（本地回环显式加 `-dev`，不再从运行时 `X-Forwarded-Proto` 猜测协议）：
  - **Tier 1 跨站通配符隔离（推荐）**：Portal 与 `-sandbox-origin https://*.sandbox.example.net` 位于不同 eTLD+1，按节点与能力盐派生独立 `sbx-<24hex>` 源，同时隔离 Portal、不同节点与不同能力访客的 Storage / Service Worker；
  - **Tier 2 受限隔离**：固定单域跨站（`-sandbox-origin https://app.example.net`，Portal 与 App 跨站但多节点与能力访客共享单一沙箱源）或单根域同站通配符（`-allow-same-site-sandbox` + `Origin-Agent-Cluster: ?1`）。两种 Tier 2 模式下 Portal 顶部均持续显示受限隔离警示条；
- **零时间重叠桥接、外置 `boot.js` 与 `appFrame` 沙箱**：引导用 `bridge.html` 仅在挂载 `appFrame` 之前短暂存活并附带 `sandbox="allow-scripts allow-same-origin"`，完成端口移交即从 DOM 移除（零时间重叠）；`/__p2p_gw__/sw.js` 要求 `Service-Worker: script` 或 `Sec-Fetch-Dest: serviceworker` 且带 `Cross-Origin-Resource-Policy: same-origin`；`bridge.js` 与 `patch.js` 拒绝 Service Worker 注册请求；HTML 响应注入外置 `<script src="/__p2p_gw__/boot.js">` + `patch.js`（零内联脚本），SW 通过指令级 CSP 重写保留上游策略并自动传递脚本 `nonce`；`appFrame` 强制设置 `sandbox` 属性阻断 `top.location` 跳转；
- **W3C Fetch 抗 CSRF 门禁与每标签页配额**：Portal CSP 锁定 `style-src 'self'`、`connect-src 'self'`、`form-action 'self'` 与 `frame-ancestors 'none'`，返回 `404` 拒绝 `/__p2p_gw__/sw.js` 与 `/__p2p_gw__/bridge.html`；SW 校验绑定 `(tabId, nextPath)` 且限 `GET`/`navigate`/`iframe` 的一次性 128 位 `enterNonce`，**无条件拦截全部 `destination === 'document'` 逃逸请求**（即使 `clientId` 已绑定），拦截全部未绑定子资源、跨站 POST 及代理模式（`-target`）下全部未绑定空 Referrer `iframe`（仅经 W3C `replacesClientId` 或已绑定根 `appFrame` 发出的单次短时 `P2P_EXPECT_NAV` 令牌放行合法刷新与前进后退），按单标签页独立隔离 `clientToTab` 配额并保护 `rootClientId` 不被子 iframe 泛洪驱逐，且对导航与 HTML 响应强制下发 `Content-Security-Policy: frame-ancestors 'self' <portalOrigin>`；
- **统一单表 RFC 6265bis `ProxyCookieJar` 与内存 `CookieMirror`**：浏览器侧覆写 `Document.prototype.cookie` 为纯内存镜像且绝不写入真实浏览器 Cookie；Agent 侧 `ProxyCookieJar` 以统一单表配合 `httpOnly` / `visible` 独立配额隔离受保护 Cookie——服务端下发 `HttpOnly`/`Secure` Cookie 时清扫同域同名更特异路径上的客户端 Cookie（阻断路径遮蔽），允许 JS 正常更新普通非受保护 Cookie，支持 W3C 潜在可信回环源的 `Secure`/`__Host-`/`__Secure-` 语义，严格遵守 `credentials: 'omit'`、Mozilla PSL、Leave Secure Cookies Alone、`SameSite` 与 400 天 `MaxAge` 上限，经 P2PMux V8 行内 TLV 双向同步（SW 仅在 `REQ_ACK` 后提交变更并彻底拒绝页面侧非信任 `SW_COOKIE_SYNC` 消息）。

---

## 5. 单目标代理边界、Fail-Closed mDNS、客户端 IP 透传与 `O_NOFOLLOW` 静态沙箱

- **严格单目标精确源与 WS 302 逃逸阻断**：每个 Agent 仅允许转发至管理员显式配置的单一 `-target` 精确 `(scheme, host, port)` 及可选子路径，拒绝 `/__p2p_proxy__/`、`//` 同源开放重定向、反斜杠与 `..;` 穿越；消费 P2PMux V8 `FetchCtx` 位域（未置位 `SameOriginClient` 一律 Fail-Closed 判跨站）；`-upstream-host` 仅覆写虚拟主机 `Host`/源语义及默认 HTTPS SNI，不改变实际拨号目标；WebSocket 拨号禁用 302 重定向；
- **Fail-Closed mDNS 候选清洗与可信代理从右向左剥离**：启用 `-ice-mdns=true`（默认）时，Agent 在出站 `OnICECandidate` 阶段强制丢弃一切非 `.local` 的 `typ host` 候选，并按 RFC 8828 §5.2 将 `srflx`/`relay` 中的 `raddr`/`rport` 置零，即使宿主禁用了 IPv6 或组播也绝不泄露内网真实 IP；Go 信令对 `r.URL.EscapedPath()` 做单次规范化解码防二次编码绕过，默认 `-trusted-proxies` 仅信任回环地址并从右向左剥离 `X-Forwarded-For`；
- **默认 Fail-Closed `O_NOFOLLOW` 的 Go `os.Root` 静态适配器**：`-dir` / `-file` 逐级调用 `os.Root.OpenRoot` 加 `O_RDONLY|O_NONBLOCK|O_NOFOLLOW` 打开，**默认拒绝全部符号链接**、点文件与 FIFO 阻塞（仅在显式启用 `-follow-symlinks` 时允许有限跳数解析，且目标真实路径必须严格位于根目录与当前能力 `path_prefix` 边界内），并默认对**全部**静态文件（含无扩展名文件）下发 `Content-Security-Policy: sandbox`。
