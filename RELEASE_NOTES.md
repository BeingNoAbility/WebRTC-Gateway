# WebRTC Gateway Release Notes

## [v1.3.0] - 2026-11-16

### 🔄 WebRTC 标准 ICE Restart 移动漫游自愈 (Mobile Roaming Self-Healing)
- **两阶段漫游韧性流水线**：解决手机、笔记本在移动过程中从 Wi-Fi 切换为 5G 蜂窝网络、AP 漫游或电梯弱网恢复时底层 IP 突变导致的断连。
- **自动信令闪连与原地重协商**：底层套接字断开时快速重建信令 WebSocket，并在现有 `RTCPeerConnection` 上以 `iceRestart: true` 发起重协商，无需销毁重建连接。
- **Agent 5 秒平滑宽限期 (Grace Window)**：在网络状态恶化或中断时启动 5 秒自愈窗口，若收到 ICE Restart Offer 则取消析构并原地生成 Answer，应用层 DataChannel、P2PMux 多路复用器与内网会话 100% 保持，正在播放的视频或后台操作完全无需刷新、不中断。

### 🔐 HMAC-SHA256 无状态时效安全分享 (Stateless Time-Limited Sharing)
- **数学级无状态验签**：纯数学自包含签名方案（`#nodeId?exp=timestamp&sign=hex_hmac`），无需中心化数据库、无需 Redis、无需服务器记录撤销表。
- **防止主口令泄露**：向同事或朋友分享内网服务时，无需透露主口令 `-secret`；签名携带过期时间戳，Agent 本地以微秒级时间复杂度比对验签，到期后数学上天然失效。
- **Web 控制台 1 键生成**：在前端设置弹窗中提供 1 小时、6 小时、24 小时、7 天、30 天等多种时效选项，基于现代浏览器原生 `crypto.subtle` 计算 HMAC-SHA256，生成即时分享链接并支持一键复制。
- **CLI 极速生成**：Agent CLI 新增 `-sign-link <duration>`（如 `24h`、`7d`）与 `-portal-url <url>` 参数，命令行直接输出可分享链接。
- **路由同步签名保留**：优化 iframe 内部 SPA 路由同步引擎，在子页面跳转同步至外层 Hash 时完整保留 `exp` 与 `sign` 参数，防止页面跳转导致签名丢失。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.3.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.3.0` (`latest`)

---

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
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.2.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.2.0` (`latest`)
