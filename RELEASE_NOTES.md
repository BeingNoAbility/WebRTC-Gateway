# WebRTC Gateway Release Notes

## [v1.2.0] - 2026-11-16

### 🛡️ 安全性加固 (Security Hardening)
- **底层 SSRF 拦截与 IP 绝对封锁**：在 TCP 拨号底层（`DialContext` / `NetDialContext`）实施目标 IP 解析校验，严格禁止访问 RFC 1918 私网（`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`）、回环（`127.0.0.0/8`）、链路本地及云元数据（`169.254.169.254`, `100.100.100.200` 等）与运营商 CGNAT，仅允许直连预设的后端目标服务。
- **防止 DNS 重绑定（TOCTOU）**：DNS 解析后直接拨号已验证的安全 IP，彻底杜绝基于 TTL=0 的 DNS Rebinding 竞争穿透漏洞。
- **Cloudflare Worker 节点密钥防冒领**：在 Durable Object（`SignalingRoom`）中补齐 `nodeSecrets` 密钥注册与校验机制，杜绝公网节点被恶意顶替劫持。
- **废弃 URL Query 明文口令**：废弃 `?secret=` 查询参数传递敏感 Token，改为仅从 URL Hash（`#node?secret=xxx`）读取，并自动清理地址栏历史记录，防止日志与 Referer 泄露。

### ⚡ 传输层性能与并发解耦 (High Performance Mux)
- **锁粒度与背压解耦**：将 Mux 物理发送锁（`writeMu`）与数据帧背压等待锁（`bpMu/bpCond`）彻底解耦。
- **消除队头阻塞 (No Head-of-Line Blocking)**：控制帧（`StreamID == 0`, `Ping`, `Pong`, `RstStream`, `Headers`）作为高优先级报文完全绕过背压排队，秒级插队直发；大文件传输（打满 256KB 缓冲区）不再阻塞 API、心跳与交互帧。

### 🌐 网络可用性与 NAT 突破 (Networking & NAT Traversal)
- **支持 TURN 中继（突破对称型 NAT）**：新增 `-turn-url`, `-turn-user`, `-turn-pass` 参数与环境变量支持，并在前端设置弹窗中提供自定义配置。打洞成功走 0 流量 P2P 直连，打洞失败无缝自动降级走加密 TURN 中继，保障恶劣网络环境下的 100% 连通。
- **国内高可用 STUN 备选**：默认内置腾讯（`stun.qq.com:18302`）、小米（`stun.miwifi.com:3478`）、B站（`stun.bilibili.com:3478`）、Cloudflare 与 Google STUN 备选，显著优化国内网络 Candidate 收集延迟。

### 🔄 会话持久化与路由联动 (Session Persistence & UX)
- **页面刷新保持 HttpOnly 登录态**：引入浏览器端长期客户端标识并绑定 `CookieJar`，配合 2 小时滑动过期窗口。用户刷新页面（F5）或短暂断网重连后，各类管理后台与复杂应用的登录态与会话 100% 保持，不再被强制登出。
- **iframe 路由与外层 URL Hash 实时双向同步**：通过消息总线拦截 SPA 内部路由跳转并同步至外层 Hash（`#node?path=/subpath`），严格防御开放重定向与 XSS；页面刷新时精准恢复当前子页面。
- **Streams 空闲超时清理**：后台自动扫描并清理空闲超过 5 分钟且无交互的非长连接 Stream，杜绝内存微泄漏。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/<owner>/webrtc-p2p-server:v1.2.0` (`latest`)
  - 穿透端：`ghcr.io/<owner>/webrtc-p2p-agent:v1.2.0` (`latest`)
