# WebRTC Gateway Release Notes

## [v1.6.0] - 2026-11-16

### 🚀 纯内存零监听 P2P 文件与目录服务器 (Zero-Listener P2P File & Directory Server)
- **单文件秒传 (私有版 Wormhole / 跨公网 AirDrop)**：
  - 新增 `-file <path>` 命令行参数、`FILE_PATH` 环境变量与配置项 `file`；
  - 发送方只需一条命令：`./agent -file ./Ubuntu-24.04.iso -access-secret pass -sign-link 24h`；
  - 接收方在任意设备浏览器点开链接即刻跑满带宽直连下载，无需安装任何客户端软件、不限文件大小、不走中心云端中转流量、24 小时后数学上自然失效！
- **纯出站零端口架构 (Pure Outbound In-Memory Adapter)**：
  - 编写轻量 `streamResponseWriter` 内存适配器，直接将 WebRTC DataChannel 的 `mux.Stream` 接入 Go 标准库 `net/http.ServeFile`，在宿主机上不开启任何本地 TCP 监听端口，恪守 Agent 纯出站安全哲学。
- **智能 Content-Disposition 响应策略**：
  - 音视频/图片/PDF/文本等媒体格式默认下发 `inline; filename="..."`，支持浏览器原生在线播放与预览；
  - 压缩包、系统镜像及二进制可执行文件（.zip, .iso, .tar, .exe, .bin 等）默认下发 `attachment; filename="..."`，直接唤起浏览器原生下载弹窗。
- **原生 HTTP 206 断点续传与媒体拖拽**：
  - 完备支持 `Range` 与 `Content-Range` 报文，支持浏览器视频播放器随意拖拽进度条 Seek，支持多线程分段加速下载。
- **静态网站与目录托管 (`-dir` / `DIR_PATH`)**：
  - 新增 `-dir <path>` 模式，自动呈现 `index.html` 静态页面（适合前端 SPA/文档产物临时托管）或生成轻量只读文件列表。
- **严格沙箱安全与路径穿越防逃逸**：
  - 基于 `filepath.Rel` 与 `filepath.EvalSymlinks` 彻底阻断 `../` 路径穿越与符号链接逃逸攻击，严防越权读取宿主机敏感文件。
- **工作模式严格三选一互斥**：
  - 在 CLI 与配置文件解析层严格执行 `-target`（反向代理）、`-dir`（目录托管）、`-file`（单文件直传）互斥校验，缺省时安全回退至 `http://127.0.0.1:8080`，保证 100% 向后兼容。

### 🛠️ 稳定性与单测加固 (E2E Concurrency Hardening)
- **信令 WebSocket 并发写保护**：
  - 在全量端到端测试套件中为 `clientWS.WriteJSON` 补齐 `sync.Mutex` 保护，彻底消除了由于 Pion 内部独立 goroutine 异步发射 ICE Candidate 导致 Gorilla WebSocket 偶发并发写 panic 的时序竞争。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.6.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.6.0` (`latest`)
