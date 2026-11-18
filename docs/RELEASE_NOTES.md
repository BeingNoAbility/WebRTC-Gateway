# WebRTC Gateway Release Notes

## [v2.0.6] - 2026-11-18

### ⚡ 悬浮指示器按钮事件隔离与指针捕获冲突热修复 (HUD Smart Pill Anti-Capture Event Isolation Hotfix)

本版本为 **v2.0.6 交互冲突热修复补丁 (Hotfix Release)**，紧急修复了 `v2.0.5` 中因 W3C `setPointerCapture` 事件劫持导致的悬浮胶囊交互失效缺陷：

- **指针捕获事件劫持根治**：
  - 在移动与拖拽加固机制中，原先在 `pointerdown` 阶段对父级容器 `#p2pPill` 施加了 `setPointerCapture`，导致现代浏览器将后续所有子元素点击事件的目标（`e.target`）强行改写为父容器自身；
  - 导致折叠按钮（`#pillCollapseBtn`）与连接设置按钮（`#pillGear`）的点击命中判定失败，误穿透至底层打开 Diagnostic HUD 诊断面板，无法将胶囊折叠为 28px 小圆点；
  - 现已为折叠按钮、齿轮按钮与折叠圆点容器注入独立的 `pointerdown` 与 `click` 阻断监听器（`stopPropagation`），并在父级拖拽前置判断中增加了子按钮排他门禁，彻底恢复了**【一键收起为小圆点】**与**【打开连接设置】**的即时响应；
  - 同步扩充 [test/test_smart_pill.cjs](file:///test/test_smart_pill.cjs) 自动化回归门禁，确保未来迭代零回归。

## [v2.0.5] - 2026-11-18

### 🛡️ RFC 3986 路径归一化、浏览器语言透传、Smart Pill 2.1 交互轻量化与隐私深度脱敏 (Synology DSM Socket.IO Fix, SW Accept-Language Passthrough, Smart Pill 2.1 & Privacy Scrubbing)

本版本为 **v2.0.5 协议健壮性、全屏交互轻量化与隐私合规发布版本 (Protocol Robustness & UX Refinement Release)**。针对复杂现代内网 Web 系统（如群晖 Synology DSM、Proxmox VE、TrueNAS、Nextcloud 等）在反代穿透中暴露出的深水区协议边界、全屏 Web 应用的按键穿透诉求，进行了底层根治与纵深加固：

- **RFC 3986 双斜杠路径消除与 Authority 陷阱根除 (群晖 DSM Socket.IO 404 彻底修复)**：
  - 群晖 DSM 前端发起的 `//synoscgi.sock/socket.io/?...` 会因前导双斜杠触发 RFC 3986 的 Authority 分支，导致 Go 标准库 `url.Parse` 误将 `synoscgi.sock` 解析为主机名并剥离，向后端 NAS 发送残损的 `/socket.io/...` 导致 404 死锁与资源监控波形图永久假死；
  - 在 `pkg/proxy/http_proxy.go`、`pkg/proxy/ws_proxy.go`、`web/src/sw/sw.ts` 与 `web/src/portal/app.ts` 中全面实现前导连续斜杠消除归一化（等效于 Nginx `merge_slashes on;`），彻底打通 Socket.IO 长轮询与 Unix Domain Socket 反代信道；
  - 补齐了高覆盖率的反向证伪单元测试 `TestResolveTargetURL_LeadingDoubleSlashesAndSynologySocketIO`。
- **Chromium Service Worker 语言偏好透传 (`Accept-Language`)**：
  - 针对 Chromium Issue 40728185（Service Worker 出于防指纹隐私策略在 `FetchEvent.request.headers` 中主动剔除 `Accept-Language`）导致的内网管理界面强制回退英文（`en-US`）问题；
  - 在 `web/src/sw/sw.ts` 中根据 `navigator.languages` 自动回填真实浏览器语言偏好（如 `zh-CN,zh;q=0.9,en;q=0.8`），完美恢复群晖 DSM、Proxmox VE 等系统的原生中文界面。
- **Smart Pill 2.1 极简双模架构与零按键劫持 (KISS 原则与深度穿透)**：
  - 彻底移除全局 `keydown` 监听器与激进的 `Double Esc` 快捷键，消除与 Vim、Neovim、Emacs、Web SSH 终端（ttyd、Portainer）、VSCode Web 及视频全屏播放器的按键冲突，实现键盘事件 100% 原生穿透；
  - 悬浮指示器收敛为纯粹的双模极简交互：**【完整指示药丸 (含延迟/流速/设置/折叠箭头)】 ⇋ 【28px 磁吸呼吸小圆点】**；
  - 采用 `setPointerCapture` 与拖拽期间动态为 `appFrame` 注入 `pointer-events: none`，彻底解决跨域 Iframe 吞噬鼠标指针导致的拖拽粘滞；新增 `test/test_smart_pill.cjs` 自动化门禁守护。
- **文档与测试套件隐私信息全量清洗脱敏**：
  - 全量排查全库文档与测试配置，彻底消除了私有测试 IP（`192.168.1.100`、`192.168.1.100`）与私有映射端口（`8443`），全部标准化收敛为项目官方缺省标准端口 **`:8888`** 与标准测试占位 IP `192.168.1.100`。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v2.0.5` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.5` (`latest`)

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
