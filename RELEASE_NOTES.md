# WebRTC Gateway Release Notes

## [v1.9.1] - 2026-11-17

### ⚡ 稳定性热修复：Service Worker 活体检测 GC 与 Web Worker 链路追踪 (Service Worker Active GC & Worker Tracking)

本版本为 **v1.9.1 紧急稳定性热修复版本**。彻底解决在多标签页隔离架构下，短时间快速刷新网页（F5）导致 LibreSpeed 等应用 504 超时白屏的问题：

- **Service Worker 活体检测与垃圾回收（Active & Lazy GC 双保险）**：
  - 彻底根除因页面刷新导致 Service Worker 向已销毁窗口的死 `MessagePort` 盲发请求并触发 30 秒 504 超时白屏的问题；
  - **主动全量 GC**：在 `PORTAL_ATTACH` 握手阶段扫描 `sw.clients.matchAll({ type: 'window' })`，一次性扫荡清除上一次页面残留的僵尸 Session 与失效映射；
  - **被动实时 GC**：在 `handleTunneledRequest` 分发前通过 `sw.clients.get(session.clientId)` 进行快速活体探活，发现死窗口立即物理注销并自动重定向至当前的真实活体 Portal 客户端；
  - 彻底废除回退查找阶段的 `lastActive = Date.now()` 自污染死锁，杜绝死会话常驻。
- **Web Worker 链路精准追踪（完美支持 LibreSpeed 测速页及 Worker 生态）**：
  - 将 Service Worker 父子映射作用域拓展至 `destination === 'worker'` 与 `'sharedworker'`；
  - 使 LibreSpeed (`speedtest_worker.js`)、Monaco Editor、PDF.js 等独立 Web Worker 发起的数据接口与测速请求，能够无缝继承其宿主 Portal 标签页的会话信道，彻底杜绝 Worker 探活请求脱靶超时。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.9.1` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.9.1` (`latest`)
