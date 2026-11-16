# WebRTC Gateway Release Notes

## [v1.8.0] - 2026-11-17

### 🛡️ 权威真实客户端 IP 穿透传递至后端 / WAF & 工业级防伪装中枢 (Authoritative Client IP Propagation to Backend & WAF with Anti-Spoofing)

- **企业级后端与 WAF 安全能力解锁 (Enterprise WAF & Security Readiness)**：
  - **核心痛点解决**：彻底告别内网穿透将所有访客来源硬编码为 `127.0.0.1` 导致的“一人被封，全员连坐”、单 IP 限流误杀 429、日志 GeoIP 溯源彻底失真等刚性生产缺陷；
  - **L7 工业级标准事实解**：穿透 Agent 转发 HTTP 与 WebSocket 握手流量时，权威提取访客公网真实 IP 并精准注入 `X-Forwarded-For`（最左侧源地址）与 `X-Real-IP` 请求头；
  - **全面兼容各大 WAF 与反向代理**：完美配合雷池 SafeLine、Cloudflare WAF、ModSecurity、Nginx `real_ip` 模块、Traefik、Caddy 实现精准的单 IP 黑白名单封禁、单 IP 频率限制防刷、日志审计与地理位置追溯。

- **三道安全防线严惩请求头伪造 (3-Tier Anti-Spoofing Architecture)**：
  1. **信令端物理/受信权威锚点**：信令服务器在 WebSocket 握手时权威提取访客物理套接字 IP（或受信反代链最前地址），并在转发 WebRTC Offer 时**强制覆写 `msg.ClientIP = session.RemoteIP`**，恶意客户端在信令 JSON 中自填伪造的任何 `client_ip` 被物理抹杀；
  2. **双信令形态 100% 对齐**：Cloudflare Workers 边缘信令端（Durable Object）同步从 `cf-connecting-ip` 提取并注入 `msg.client_ip`，无论自建信令还是边缘信令，功能无缝平替；
  3. **L7 代理强制覆写 (.Set)**：Agent 在转发数据通道中的 HTTP/WebSocket 请求时，在解析客户端自报请求头之后，强制调用 `req.Header.Set("X-Forwarded-For", clientIP)` 与 `req.Header.Set("X-Real-IP", clientIP)`，攻击者在 WebRTC DataChannel 内自报伪造的内网白名单头会被 100% 抹杀覆盖。

- **移动端漫游原地热重连 IP 实时更新 (In-place ICE Restart Roaming IP Update)**：
  - 当移动端访客在 Wi-Fi 与 5G/蜂窝网络之间漫游切换时，WebRTC 通过原地 ICE Restart（In-place ICE Restart）保持 DataChannel 连接与媒体流不中断；
  - Agent 在重连协商成功时即刻同步刷新会话绑定的公网出口 IP，后续流经隧道的新请求立即自动按漫游后的新 IP 计入 WAF 审计与防护规则。

- **空值安全回退与防内存泄漏 (Graceful Fallback & Anti-Memory-Leak)**：
  - **优雅兜底**：若对端对接旧版第三方信令服务器导致 `ClientIP` 为空，自动兜底为 `127.0.0.1`，严禁向后端输出空头，保障与严格模式 Web 框架 100% 向后兼容；
  - **内存零泄漏**：会话 IP 映射表受并发互斥锁 `connMu` 保护，在会话 Teardown 销毁及 Agent 关闭时立即删除注销，杜绝常驻内存积累。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.8.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.8.0` (`latest`)

---

## [v1.7.4] - 2026-11-16

### 🎬 全能极速文件舱 (P2P File Hub)、音视频在线播放与极速下载双通道 & 即时终止下载 (Action Sheet with Play Online, P2P Download & Cancel Control)

- **全能极速文件舱 (P2P File Hub) 升级 (Unified Action Sheet Architecture)**：
  - **核心痛点解决**：修复了在之前的版本中，音视频在移动端无法下载、桌面端原生播放器下载会绕过 Service Worker 打向信令服务器报 404 的问题。将文件操作舱全面泛化支持视频（MP4/MKV/WebM/AVI/MOV）、音频（MP3/WAV/FLAC/AAC/M4A）及各类常规大文件；
  - **双通道清晰对等**：
    - **`▶ 立即在线播放 (Play Online)`**：专为音视频打造，点击后通过 `view=inline` 意图标记直接唤起 iframe 浏览器原生媒体引擎起播，享受 GPU 硬件加速、全屏、倍速与画中画，且**100% 杜绝操作舱死循环反复弹窗**；
    - **`⚡ 极速 P2P 下载 (P2P Download)`**：直接唤起 v1.7.3 打造的工业级双模流式存盘引擎，实时呈现动态传输进度与流速 HUD，并直存至本地设备；
  - **精准隔离第三方法定 Web 应用 (Third-Party Web App Isolation)**：
    - 仅对 `-file`（单文件模式）和 `-dir`（目录服务模式）激活文件操作舱；
    - 常规 `-target` 模式（穿透 Jellyfin、Nextcloud、Gitea 等第三方 Web 系统）保持完全透明透传，绝对不发生误判拦截。

- **显式“终止下载 (Cancel)”即时反悔与带宽止损能力**：
  - 在下载进行时，动态展开高辨识度 `[ ⏹ 终止下载 (Cancel) ]` 按钮；
  - 用户点击后瞬时触发前端 `AbortController.abort()`，底层 Service Worker 与多路复用隧道协同向远端 Agent 发送 `stream.reset()`；
  - 即刻掐断 P2P 传输推流，0 浪费网络带宽与系统内存，并在 1.5 秒内自适应平滑恢复初始就绪状态。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.7.4` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.7.4` (`latest`)
