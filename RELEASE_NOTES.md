# WebRTC Gateway Release Notes

## [v1.7.0] - 2026-11-16

### 🚀 协议文档严格对齐、双端报文校验加固、混合压测基准与跨端生态矩阵 (Protocol Spec, Benchmarking & Browser Matrix)

- **P2PMux 协议定义规范 100% 对齐与勘误 (Protocol Specification Alignment)**：
  - 全面消除 `README.md`、`README_zh.md`、`README_en.md` 及 GitHub Wiki 中遗留的历史笔误；
  - 严格统一双端报文定义：`TypeData = 0x01`、`TypeHeaders = 0x02`、`TypeRstStream = 0x03`、`TypeWindowUpdate = 0x04`、`TypePing = 0x05`、`TypePong = 0x06`、原生 WebSocket 隧道帧 `0x10/0x11/0x12`；
  - 明确标注规范物理尺寸：单帧最大载荷上限为 **64KB (MaxPayloadSize = 65536)**，推荐流式传输分块为 **32KB (RecommendedChunkSize = 32768)**。

- **双端严格报文报长防御加固 (Strict Message Length Validation)**：
  - **Go 核心库 (`pkg/protocol/frame.go`)**：
    - 声明导出的哨兵错误 `ErrInvalidFrameLength`；
    - 在 `DecodeFrameBytes` 中将截断与越界判断改为完全匹配：`len(data) != HeaderSize + int(frame.Length)`，彻底杜绝尾部脏数据吞没与越界隐患；
  - **TypeScript 前端运行时 (`web/src/protocol/frame.ts`)**：
    - 同步执行 `data.byteLength !== HeaderSize + length` 强一致校验，保证双端解包防御对齐；
  - **新增单元测试 (`pkg/protocol/frame_test.go`)**：
    - 针对截断数据包与注入额外垃圾字节场景补充严密测试。

- **大文件与高频 API 混合并发基准测试 (Mixed Workload Benchmark)**：
  - 新增 `test/mixed_workload_benchmark_test.go`：大文件 10MB 跑满 WebRTC DataChannel 底层背压队列，同时高频并发发起 50 次轻量 REST API 请求；
  - **实测性能数据**：
    - 大文件传输吞吐达 **46.21 MB/s**；
    - 50 次并发 API 请求 100% 成功（0 失败）；
    - API 延迟指标：**Min 0.58 ms / P50 4.14 ms / P95 7.11 ms / P99 8.65 ms**；
    - 以坚实的客观测试数据证明：P2PMux 基于 32KB 分片调度与控制帧优先传输，在保持轻量无状态的同时完美兼顾了公平性，无需盲目照搬 HTTP/2 复杂的逐流滑动窗口（Per-Stream Flow Control）。

- **WebRTC ICE Restart 移动弱网自愈与会话连续性自动化测试**：
  - 新增 `test/ice_restart_resilience_test.go`：
    - 自动化仿真手机移动漫游（Wi-Fi 切换 5G 蜂窝网络）触发的原生 in-place `iceRestart: true` 协商重连；
    - 验证网络自愈后，Agent 端 CookieJar 与 visitorID 100% 持续保持，已有会话与数据通道无缝续传。

- **跨浏览器生态兼容性矩阵与 WebKit 移动端自愈梳理**：
  - 发布专属官方指南 [docs/BROWSER_COMPATIBILITY.md](docs/BROWSER_COMPATIBILITY.md) 与 Wiki 同步页面；
  - 详细定义 Tier 1 (Chrome/Edge/Brave/Firefox) 与 Tier 2 (Safari/iOS WebKit) 支持基线；
  - 详细阐明 iOS Safari 后台休眠强杀 Service Worker 下的 `PORTAL_ATTACH` 管道自愈与重新绑定机制。

- **架构纪律与产品护城河边界声明 (Architecture Boundaries)**：
  - **无状态网络门禁定位**：明确网关口令与 HMAC 签名属于网络传输层门禁，细粒度 RBAC 与多用户权限体系由上游业务自理，拒绝臃肿功能膨胀；
  - **原生拓扑免疫 SSRF**：明确 Agent 纯单向绑定后端服务，绝不提供任意网络代发能力，从数学和拓扑上彻底杜绝 SSRF 攻击面。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.7.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.7.0` (`latest`)
