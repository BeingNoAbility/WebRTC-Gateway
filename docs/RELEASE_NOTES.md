# WebRTC Gateway Release Notes

## [v2.0.7] - 2026-11-18

### 🛡️ Service Worker 流式控制器中断防御与客户端韧性加固 (Service Worker Stream Controller Resilience & Client Robustness)

本版本为 **v2.0.7 稳定性与客户端韧性加固版本 (Client Robustness & Stream Resilience Release)**。重点修复了在高并发反向代理、富前端资源密集加载以及第三方浏览器扩展（如 Dark Reader、拦截器）主动中止网络请求时暴露的 Service Worker 运行时流控制边界异常，并巩固了悬浮控制器的事件隔离机制：

- **Service Worker 可读流控制器安全入队防御 (Stream Controller Cancellation Guard)**：
  - 针对客户端页面导航、DOM 销毁或浏览器扩展取消请求导致 `ReadableStream` 被提前 `cancel()` / `close()` 的场景；
  - 在 [web/src/sw/sw.ts](file:///web/src/sw/sw.ts) 中引入 `isStreamClosed` 状态锁，并将 `controller.enqueue(chunk)` 严格封装在防御性 `try...catch` 块中；
  - 彻底根除控制台抛出的未捕获异常：`TypeError: Failed to execute 'enqueue' on 'ReadableStreamDefaultController': Cannot enqueue a chunk into a closed readable stream`，确保在流被中止时优雅丢弃在途分块，不破坏 Service Worker 主线程的事件循环与长连接状态。
- **悬浮指示器（Smart Pill）事件隔离与反指针捕获机制稳固**：
  - 全面巩固了 `v2.0.6` 中对于 W3C `setPointerCapture` 事件劫持的修复成果；
  - 确保悬浮指示器的折叠小圆点按钮（`#pillCollapseBtn`）与连接设置按钮（`#pillGear`）具备独立的事件阻断通道（`stopPropagation`），彻底杜绝误触发 Diagnostic HUD 诊断面板的现象，实现 100% 灵敏的双模切换与参数配置。
- **发布核验规范与单一版本发布说明 (Single-Version Release Notes) 严格对齐**：
  - 订正发布文档体系，严格遵守规范将历史版本更新轨迹收敛沉淀至 [CHANGELOG.md](file:///CHANGELOG.md)，[docs/RELEASE_NOTES.md](file:///docs/RELEASE_NOTES.md) 严格聚焦于本次最新版本的单一说明，满足多平台发版门禁。

---

### ⚠️ 破坏性变更提示 (Breaking Changes)
- **无破坏性变更 (None)**。本版本完全兼容既有的 WebRTC 通信协议、信令握手格式、配置文件及数据通道多路复用模型。所有已部署的 Agent 与 Signal 节点均可无缝平滑升级。

---

### 📦 资产下载与快速验证 (Downloads & Verification)
- **预编译二进制跨平台支持**：
  - Linux (amd64 / arm64)
  - Windows (amd64 / arm64)
  - macOS Darwin (amd64 / Apple Silicon arm64)
  - 可前往 GitHub Releases 页面下载对应的 `.tar.gz` / `.zip` 压缩包及 SHA256 校验文件 `checksums.txt`。

- **官方 Docker 镜像拉取与启动**：
  多架构容器镜像已同步发布至 GitHub Container Registry (`ghcr.io`)，支持一键拉取：
  ```bash
  # 拉取服务端镜像
  docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.0.7
  docker pull ghcr.io/beingnoability/webrtc-p2p-server:latest

  # 拉取穿透端镜像
  docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.7
  docker pull ghcr.io/beingnoability/webrtc-p2p-agent:latest

  # 快速启动示例
  docker run -d --name p2p-server -p 8888:8888 ghcr.io/beingnoability/webrtc-p2p-server:v2.0.7
  docker run -d --name p2p-agent --network host ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.7 -node-id nas -signal ws://127.0.0.1:8888/ws -target http://127.0.0.1:8080
  ```
