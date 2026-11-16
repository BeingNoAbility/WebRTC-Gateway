# WebRTC Gateway Release Notes

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
