# WebRTC Gateway Release Notes

## [v2.0.4] - 2026-11-18

### ⚡ 高并发性能优化、零拷贝流式穿透与信令非阻塞分发 (High-Concurrency Performance Optimization & Protocol Robustness)

本版本为 **v2.0.4 高并发性能调优与协议健壮性升级版本 (Performance Optimization & Protocol Robustness Release)**。在恪守三大安全红线（不自建 DNS 缓存、不削弱 `EvalSymlinks` 目录沙箱、不硬编码单一 UDP Mux 端口）与既有通信协议不变量的前提下，系统性完成了热路径内存对齐、端到端零拷贝、信令异步广播与边缘分片优化：

- **Go/V8 32KB 阶梯对齐与帧编码池化 (PERF-01)**：
  - 将分块常量 `RecommendedChunkSize` 精确设为 `32*1024 - HeaderSize`（32758 字节，载荷 32758 + 帧头 10 = 32768 字节），完美契合 Go 内存分配器的 32KB Small Object Class；
  - 在 `pkg/mux/mux.go` 中引入 `frameEncodePool = sync.Pool`，实现串行发送热路径下的零堆分配编码与即时回收。
- **浏览器 Transferable ArrayBuffer 零拷贝直传 (PERF-02)**：
  - 在 `web/src/portal/app.ts` 的流式穿透中，直接利用原生 `ArrayBuffer` 作为 Transferable 移交 `postMessage`，消除对每个数据块执行二次 `.slice()` 深度克隆的内存反模式。
- **WebSocket $O(N)$ 线性拼包累积队列 (PERF-03)**：
  - 将原 `new Uint8Array(wsBuffer.byteLength + chunk.byteLength)` 二次方内存重新分配反模式重构为基于 `consumeBytes` 与 `peekHeader` 的线性分片累加器，大吞吐下有效消除 V8 堆抖动与 GC 停顿。
- **传输连接池扩容、Cookie 短路与切片即时 GC (PERF-05, PERF-07, PERF-11)**：
  - 在 HTTP 代理中启用 `ForceAttemptHTTP2: true`，连接池扩容至 `MaxIdleConns: 512, MaxIdleConnsPerHost: 128`；
  - `CookieJar.SaveCookies` 增设空 `Set-Cookie` 短路检查，95%+ 请求直接免除读锁竞争；
  - `Stream.Read` 在消费后显式置空 `s.inbound[0] = nil`，并在清空时收缩切片，彻底释放历史 chunk 的底层数组引用。
- **信令广播无锁化与独立 SessionID 守护 (PERF-09 & Trap 1)**：
  - `NotifyWaitingClients` 仅在 `RLock` 下快照目标客户端并立即释放锁，随后通过异步 goroutine 逐个分发，彻底杜绝单客户端慢写对整个信令总线的队头阻塞，且为每个客户端独立保留唯一 `SessionID: client.ID`。
- **握手协议解复用零冗余 (PERF-06 & Trap 2)**：
  - 严格保持线上 Wire 帧为 `TypeHeaders`，规避 `TypeWSConnect` 死锁；在 Go 端通过 `HandleStreamWithMeta` 实现单次反序列化后直接分流，彻底移除 `PushInternalHeaders` 以及代理层的二次读取与二次解析。
- **Cloudflare DO 边缘房间分片与平滑降级 (PERF-10 & Trap 4)**：
  - Agent 连接信令时自动注入 `?node=${nodeID}` 实现 Durable Object 房间物理隔离；Portal 端优先连接分片房间，若对端离线且未回退过，`legacyFallbackAttempted` 触发一次无缝断开并自动回退至全局旧版信令 URL，确保老版本 Agent 互通无阻。
- **浏览器沙箱性能与 SSR 流式注入防御 (PERF-13, PERF-14 & Blind Spot 3)**：
  - Portal 监听 `visibilitychange` 事件，后台标签页自动休眠 HUD 统计；弹窗折叠时将轮询放宽至 3000ms 且仅更新胶囊 DOM；
  - Service Worker 对 `sw.clients.get()` 增加 5 秒节流窗口，杜绝海量静态并发下的 IPC 风暴；
  - SSR 初始流式 chunk 小于 2048 字节且未匹配到 `<head>` 时引入小缓冲队列等待后续 chunk，彻底规避微小首包触发的双 `<head>` 畸形标签；
  - XHR 补丁中改用 `new Blob([body]).size` 计算上传大小，避免 `TextEncoder` 产生多余的堆内存字符串克隆。
- **探针结构体宽容度防御纵深补强**：
  - 在 `pkg/agent/agent.go` 与 `pkg/proxy/http_proxy.go` 中为 `Method` 增设 `method` 别名，并在缺失时优雅回退为标准 `GET`，全面兼容第三方自定义客户端与自动化测试 SDK。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v2.0.4` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.4` (`latest`)
