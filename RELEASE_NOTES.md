# WebRTC Gateway Release Notes

## [v1.6.1] - 2026-11-16

### 🛠️ 信令通道双向心跳保活与 61 秒断开振荡彻底根治 (WebSocket Keepalive Ping Pump)

- **根因彻底消除 (Root Cause Elimination)**：
  - 彻底根治了信令服务在空闲无流量时“每隔 61 秒固定断开重连一次”的振荡 Bug；
  - 补齐了服务端与 Agent 双向缺失的 RFC 6455 `PingMessage` 定时泵，使得 60 秒的底层套接字 `ReadDeadline` 得以在心跳周期内持续刷新，达成信令通道 7x24 小时零断流。
- **服务端主动心跳泵 (`cmd/signal/main.go`)**：
  - 为接入的 Session 启动 25 秒轻量 Ping Ticker，严格在 `session.mu` 互斥保护下发送 `websocket.PingMessage` 控制帧（附带 5 秒 WriteDeadline）；
  - 自动覆盖现代浏览器客户端：浏览器内核（Chromium/WebKit/Gecko）底层对标准 WebSocket Ping 协议帧原生支持，收到 Ping 会自动在网络层回发 Pong，前端零代码改动即可永久保活；
  - 完善连接断开时的原子资源清理（`ctx.Cancel()` 即时停止心跳协程，并置 `session.Conn = nil`）。
- **Agent 端主动防御与并发安全加固 (`pkg/agent/agent.go`)**：
  - Agent 启动 25 秒主动 Ping 泵，即使处于强制掐断空闲 TCP 流的云防火墙、Nginx 反代或 Cloudflare Workers（100 秒空闲超时）边缘环境下，信令长连接依然坚如磐石；
  - 挂载并发安全的 `SetPingHandler`（在 `a.wsMu` 保护下回复 Pong 并顺延 60 秒超时），彻底杜绝与业务数据并发写导致的底层 Panic；
  - 提供 `SetPingInterval` 与 `SetReadTimeout` 接口，支持快速调整或极速自动化测试。
- **全栈自动化测试套件与保活核验**：
  - 新增 `TestSignalBroker_WebSocketKeepalivePingPong`（`cmd/signal/broker_test.go`）；
  - 新增 `TestAgent_KeepalivePingPump`（`pkg/agent/agent_test.go`）；
  - 新增 `TestE2E_WebSocketDualKeepalive`（`test/keepalive_e2e_test.go`）；
  - 全量 Go 单元/集成测试与 Chromium CDP 浏览器测试矩阵 100% 通过。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.6.1` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.6.1` (`latest`)
