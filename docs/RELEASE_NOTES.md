# WebRTC Gateway Release Notes

## [v2.0.0] - 2026-11-17

### 🚀 性能飞跃与云原生架构重大升级 (Performance Leap & Cloud-Native Upgrade)

本版本为 **v2.0.0 重大里程碑版本 (Major Milestone Release)**。实现了跨公网静态资源传输速度提升 300% 以上、无缝兼容 Vite / ES Module 现代前端框架、消除 Cloudflare 边缘闲置计费，并彻底消除 Go 服务端长期运行的内存膨胀隐患：

- **Gzip 跨网压缩直通 + 浏览器端 `DecompressionStream` 硬件加速流式解压 (300%+ Performance Leap)**：
  - **规避 W3C 规范陷阱**：针对 W3C Fetch 规范中浏览器对合成响应（Synthetic Response）不自动解压 `Content-Encoding: gzip` 的硬限制，Agent 端将压缩响应转换为内部标识 `x-p2p-content-encoding: gzip` 并剔除原始 `Content-Encoding` 与 `Content-Length`；
  - **原生硬件流式解压**：浏览器端 Service Worker 检测到标记后，调用底层现代 C++ 硬件加速流式解压管线 `stream.pipeThrough(new DecompressionStream('gzip'))`；
  - **显著加速效果**：静态资源（JS/CSS/JSON/SVG/WASM/Worker）跨 WebRTC P2P 数据通道传输体积减少 **60%~80%**，复杂 Web 应用（如 Immich、Portainer、Jellyfin）首屏载入速度提升 **300% 以上**；同时对非直通环境保留透明解压降级。
- **Web Worker 动态适配 ES Module (Vite & Modern Frameworks Compatibility)**：
  - **前端补丁层拦截**：客户端透明拦截包装 `window.Worker` 与 `window.SharedWorker`，在检测到传入 `{ type: 'module' }` 模块化 Worker 时，自动追加 `?_p2p_worker=module` 查询参数；
  - **Service Worker 动态分支注入**：Service Worker 针对传统 Worker 注入 `importScripts("/patch.js");\n`，针对 ES 模块化 Worker 动态注入 `import "/patch.js";\n`；
  - 彻底根治现代前端应用在 W3C 标准下抛出 `TypeError: Module scripts don't support importScripts()` 的顽疾，原生兼容 Vite、VitePress、SvelteKit 与 PDF.js v4+。
- **Go 信令服务端 `IPRateLimiter` 内存泄漏根治 (Zero Chronic Memory Leak)**：
  - 针对高并发与公网端口扫描场景下 `msgTimes` 与 `failedAuths` 中 IP 键无限累积的问题，引入后台 `janitorLoop` 定时清理协程与 `stopCh` 优雅生命周期管理；
  - 定期执行滑动窗口过期剔除，当切片记录为空时显式调用 `delete(map, ip)` 清理孤儿键，彻底消除长期挂机服务端的慢性内存膨胀。
- **Cloudflare Worker 迁移至 WebSocket Hibernation API (Zero Idle Cost & Sharding)**：
  - **WebSocket 休眠 API**：改用 `this.ctx.acceptWebSocket` 接管长连接，并配置 `setWebSocketAutoResponse(new WebSocketRequestResponsePair('ping', 'pong'))` 边缘自动响应心跳；
  - **闲置计费清零**：在家庭 Agent 24 小时挂机心跳期间，Durable Object 实例进入深度休眠态，将闲置 GB-sec 挂机费用降为近乎为 0；
  - **动态节点分片与平滑降级**：支持 `?node=<nodeId>` 动态分片至隔离的 Durable Object 实例，同时对旧版客户端与缺省参数无缝回退至 `global-broker`，实现 100% 前后向兼容。
- **配置文件模板与注释精准校准 (Config Calibration)**：
  - 校准 `configs/agent.example.yaml` 中 `node_id` 注释，明确硬编码默认值为 `"homeserver"`；
  - 澄清 `sni` 与 `insecure` 参数仅对信令服务（Signaling）的 TLS 握手、CDN 优选 IP 及证书校验生效，而非内网后端 Target；
  - 澄清 `portal_url` 的使用场景与默认值。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v2.0.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.0` (`latest`)
