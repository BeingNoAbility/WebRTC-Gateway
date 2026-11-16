# WebRTC Gateway Release Notes

## [v1.9.0] - 2026-11-17

### 🚀 架构韧性与工程规范演进 (Architecture Resilience & Engineering Standardization)

本版本为 **v1.9.0 架构韧性与工程规范演进版本**。聚焦于多标签页并发体验、脚本注入边界严密性、前端编译门禁与协议标准对齐：

- **Service Worker 多标签页会话隔离 (Task 1 / P1-2)**：
  - 彻底解耦 Service Worker 全局单例通信信道与单例目标源，建立多租户/多会话状态机 `sessions` 与 `iframeToPortalMap`；
  - 引入基于 `FetchEvent` 导航生命周期的父子 Client ID 级联映射机制（利用 `event.clientId` 与 `event.resultingClientId`），精准追踪外层 Portal 标签页与内嵌 iframe 的血缘关系；
  - 完美支持在同一网关域名下并发打开多个标签页分别访问不同的穿透节点，彻底杜绝节点 ID 串台、MessageChannel 信道踩踏与代理目标源污染。
- **Worker 脚本精准过滤与 UTF-8 流解码防护 (Task 2 / P1-4)**：
  - 彻底废除 `url.pathname.includes('/worker')` 模糊匹配，根除后端业务 API 路由（如 `/api/worker/tasks`、`/worker/status.json`）被误注入 `importScripts` 导致 JSON 损坏的隐患；
  - 严格限定仅对 `request.destination === 'worker' || request.destination === 'sharedworker' || /(?:\.worker|_worker)\.js$/i` 且 `Content-Type` 为 `javascript` 的真实脚本注入补丁；
  - HTML 流式虚拟化注入器引入 `TextDecoder('utf-8', { stream: true })` 与安全刷新逻辑，彻底消除跨 chunk 多字节中文字符与 Emoji 边界截断乱码。
- **前端工程 TypeScript 严格类型门禁 (Task 3 / P2-1)**：
  - 清理 `PortalApp` 中遗留的未声明悬挂属性 `mediaContainer` 悬空引用；
  - 订正 `PatchedWebSocket.readyState` 联合字面量类型与 `P2PMux.sendRaw` 缓冲区类型，全面通过 `tsc --noEmit` 严格检查；
  - `package.json` 正式确立 `"typecheck": "tsc --noEmit"`，将类型编译门禁深度集成至 `npm run build` 前置流程，杜绝未定型代码流入构建产物。
- **规范校准与协议对齐 (Task 4 / P2-4 & ws_proxy)**：
  - 校正 `configs/server.example.yaml` 中 `tls_auto` 的释义，明确其为本地/局域网 Secure Context 专用的内存自签 ECDSA 证书；
  - 订正 `pkg/proxy/ws_proxy.go` 中的 `X-Forwarded-Proto` 头注入逻辑，根据后端实际协议（`http`/`ws` 注入 `ws`，`https`/`wss` 注入 `wss`）动态对齐，与 `http_proxy.go` 保持严格规范一致。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.9.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.9.0` (`latest`)
