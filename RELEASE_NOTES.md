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
