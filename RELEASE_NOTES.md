# WebRTC Gateway Release Notes

## [v1.7.3] - 2026-11-16

### 🚀 治愈 Edge 下载“没有文件”与 txt 误判、现代双模 P2P 流式存盘与实时传输速率显示 (Zero-Network P2P Stream Download, File System Access & Real-Time Progress)

- **彻底治愈 Chromium / Edge `<a download>` 绕过 Service Worker 的内核缺陷 (Chromium Bug 40410035 Resolved)**：
  - **根本原因解决**：修复了在 File Vault（文件金库）中点击下载时，由于合成点击 `<a href="..." download="...">` 触发了 Chromium 内核对带有 `download` 属性的网络 URL 强行绕过 Service Worker 的限制，导致请求直接穿透打到信令服务器返回 `HTTP 404 Not Found (text/plain)`，进而引发 Edge 报告“无法下载 - 没有文件”并将文件类型误判为 `.txt` 的缺陷；
  - **100% 经由 JS Fetch 隧道**：下载触发全面迁移为 `fetch(viewUrl, { signal })` 管道，根据 W3C 规范保证百分之百被 Service Worker 捕获并由 WebRTC DataChannel 向 Agent 索取数据流，完全切断向信令服务器的网络逃逸；
  - **本地 `blob:` 零网络直存**：常规文件（<= 500MB）接收完毕后生成 `blob:` URL 并触发保存，Chromium 内核对 `blob:` 协议完全不发起外部网络请求，直接将本地内存数据流写入磁盘，100% 严格保证原始文件名与 `.pdf` 等扩展名。

- **工业级“双模直写体系”：兼顾多端通用与超大文件防 OOM (Dual-Mode Direct-to-Disk Architecture)**：
  - **常规与中大型文件（<= 500MB，涵盖所有课本、文档、源码、常规音视频与压缩包）**：
    - 采用 `ReadableStream` 收集分块数据，自动组装为带精准 MIME 类型的 `Blob` 进行毫秒级本地存盘，全平台（iOS/Android/Safari/Edge/Chrome/Firefox）无缝平滑自适应；
  - **超大文件（> 500MB，长篇原盘视频、系统镜像）**：
    - 在桌面端 Edge / Chrome 环境下优先探测并调用现代 **File System Access API (`window.showSaveFilePicker()`)**；
    - 唤起系统原生另存为对话框，获取本地底层文件可写流 `FileSystemWritableFileStream`；
    - 边从 WebRTC 收包边落盘，**内存占用恒定保持在数百 KB 以内**，即使传输数十 GB 的超大型镜像也绝不发生浏览器内存溢出（OOM / 页面崩溃）。

- **实时极速下载进度条与传输流速显示 (Real-time P2P Progress & Speed HUD)**：
  - 点击下载后，下载按钮自动置灰并展示实时下载状态与速率（例如：`📥 正在接收: 45% (18.2 MB/s)`）；
  - 在按钮下方提供高精度平滑动态绿色进度条与已传输字节 / 总字节实时比对；
  - **防呆保护与带宽保护**：
    - 下载期间防止用户重复频繁点击产生重复打洞；
    - 深度绑定 `AbortController`，当用户点击“⬅ 返回文件目录”或关闭弹窗时，立即中断传输并自动通知远端 Agent `stream.reset()`，即时节省带宽。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.7.3` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.7.3` (`latest`)
