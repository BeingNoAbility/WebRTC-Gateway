# WebRTC Gateway Release Notes

## [v1.4.0] - 2026-11-16

### 📊 WebRTC HUD 诊断看板与实时可观测性 (WebRTC Diagnostics HUD & Observability)
- **浮动状态胶囊升级**：页面右上角状态胶囊实时呈现传输链路模式（`⚡ P2P 直连` 或 `🛡️ TURN 中继`）、毫秒级往返延迟 (RTT) 以及实时下行流速（`↓ XX KB/s`），点击胶囊即可呼出诊断面板。
- **WebRTC 实时诊断看板 (HUD Modal)**：前端基于 `pc.getStats()` 深度解析 WebRTC 传输层指标，实时呈现：
  - 本地与远端候选者对（Local/Remote IP、端口、协议 UDP/TCP、类型 host/srflx/relay）；
  - 动态往返时延 (RTT)、下行与上行实时速率 (KB/s)、累计传输流量 (MB) 与丢包率；
  - 提供 “🔄 测试 ICE Restart (网络漫游自愈)” 实时交互测试按钮，可视化验证无感重协商与会话韧性。
- **Prometheus `/metrics` 标准指标导出**：
  - **信令服务端 (`server`)**：原生暴露 `webrtc_gateway_registered_nodes`、`webrtc_gateway_active_clients`、`webrtc_gateway_signaling_messages_total` 与 `webrtc_gateway_ratelimit_rejected_total`，支持通过 `-metrics-addr` / `METRICS_ADDR` 配置独立监听端口。
  - **穿透客户端 (`agent`)**：**严格默认零监听**（Pure Outbound Daemon 守护模式，不开放任何入站端口，避免局域网暴露与 NAS 端口冲突）。显式配置 `-metrics-addr` / `METRICS_ADDR` 时安全暴露 `webrtc_gateway_active_peers`、`webrtc_gateway_active_streams`、`webrtc_gateway_max_streams`、`webrtc_gateway_traffic_bytes_total{direction="rx|tx"}` 及 `webrtc_gateway_stream_rejections_total`。

### 🛡️ 生产容量过载保护与信令防 DoS 治理 (Capacity Governance & Anti-DoS)
- **Agent 并发流容量治理**：引入 `-max-streams` / `MAX_STREAMS`（默认 512）并发流上限。在突发大并发或爬虫扫盘导致过载时，优雅返回带有 `Retry-After: 2` 的 HTTP 503 响应，保护宿主机与家庭 NAS 内存及文件描述符不被耗尽。
- **信令端 IP 并发连接与频次限制**：信令服务器内置 `IPRateLimiter`，默认单 IP 最大允许 64 个并发连接（`-max-ip-conns` / `MAX_IP_CONNS`），滑动窗口限制单连接 120 消息/10秒，防止恶意滥用与死循环洪水攻击。
- **暴力破解锁定防御**：单 IP 节点认证口令错误超过 10 次/分钟时自动触发临时封锁，拦截针对 `-secret` 的字典与彩虹表暴力破解。
- **Cloudflare Worker 边缘防滥用同步**：同步更新 `web/worker.js` 与 `edge/cloudflare-worker/worker.js`，Durable Object 支持单 IP 并发连接限制与认证失败封锁。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.4.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.4.0` (`latest`)
