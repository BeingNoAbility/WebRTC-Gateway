# WebRTC Gateway Release Notes

## [v1.7.2] - 2026-11-16

### 🎧 彻底消除音视频重音回声、回归浏览器原生播放器 & 严密截断文件舱后台流 (Eliminate Audio Echo, Native Player & File Vault Stream Truncation)

- **彻底消除音视频“双重声音 / 重音”缺陷 (100% Elimination of Double Audio Echo)**：
  - **根本原因解决**：修复了在 `-dir` 与 `-file` 模式下打开音视频时，前端一方面在 DOM 顶层动态创建新播放器发起第二路流，另一方面被隐藏的 iframe 仍在持续接收第一路流并起播，导致“前台一个声音、后台一个声音”以及 P2P 带宽被瞬间翻倍消耗的双重缺陷；
  - **全面回归浏览器原生能力 (Plan A)**：基于 `v1.7.1` 已治愈的底层 Service Worker 根路由精准放行（`isTopLevelDocNav`）与 Go 端内置标准 MIME 字典（`video/mp4` 及 `Accept-Ranges: bytes`），彻底移除多余的前端 DOM 播放器逻辑，让音视频自然在 `appViewport` iframe 中播放；
  - **享受顶级原生播放体验**：由浏览器（Chrome/Edge/Safari/Firefox/移动端）原生媒体引擎渲染，完美支持 GPU 硬件加速、倍速播放（0.5x~2.0x）、音量记忆、画中画（PiP）、全屏以及空格/方向键快捷键；
  - **目录模式历史栈自然自愈**：在 `-dir` 模式点播视频后，点击浏览器“后退”按钮或手机侧滑返回，浏览器的 History API 会自然无缝回到文件目录树。

- **弹出 P2P File Vault 文件舱时彻底截断后台 iframe 推流 (Stream Truncation & Bandwidth Conservation)**：
  - 当检测到 PDF 文档（`application/pdf`）或非内联二进制下载（`isAttachment`）并激活文件舱时：
    - 主动向 iframe 的 MessagePort 发送 `HTTP 204 No Content` 终止响应；
    - 立即调用 `stream.reset()` 告知 Agent 停止推送后续数据块；
  - **双重收益**：
    1. 根据 W3C 规范，导航请求收到 204 时浏览器强制终止跳转并保留在当前页面，彻底杜绝 Edge 沙箱报出“此页面已被阻止”错误；
    2. 彻底掐断后台静默推流，节省 100% 的无谓 P2P 带宽损耗，并避免与文件舱中的下载按钮产生交互冲突。

- **完备的 Feature Policy 权限穿透**：
  - 为 `appViewport` 显式补全 `allow="fullscreen; ... picture-in-picture"` 及 `allowfullscreen="true"` 跨代兼容属性，确保原生播放器在点击全屏或画中画时不受父级容器约束，100% 铺满整块物理屏幕。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.7.2` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.7.2` (`latest`)
