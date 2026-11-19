# WebRTC Gateway Release Notes

## [v2.3.0] - 2026-11-19

### 核心功能突破与架构演进 (Highlights & Architecture)

- **原生双物理通道架构 (`p2pcontrol` + `p2pdata`)**：
  - 彻底解耦 WebRTC DataChannels：`p2pcontrol` 专运 `TypeHeaders`、`TypePing`、`TypePong`、`TypeRstStream`、`TypeWindowUpdate` 等轻量控制信令，`p2pdata` 专运 `TypeData` 与 `TypeWSData` 大吞吐载荷。
  - 控制信令完全绕过数据背压检测（`writeFrameControl`），在 10GB 饱和推流与混沌丢包下，Ping RTT 依然稳定在 < 10ms，从传输层（SCTP Multi-Streaming）彻底物理根除队头阻塞（Head-of-Line Blocking, HoL）。
  - 实现跨信道孤儿帧缓冲池（`pendingOrphans`，单流 64KB、全局 256KB、500ms TTL 收割），优雅解决不同 SCTP 流之间的到达竞态，零丢包、零内存泄漏。
- **千级并发流与混沌基准测试矩阵 (1000-Stream Benchmark Suite)**：
  - 交付全套基准压测套件（`test/concurrent_streams_benchmark_test.go` 与 `test/dual_channel_hol_benchmark_test.go`）。
  - 实测 1000 并发流达成 **100.0% 成功率 (1000/1000)**，吞吐峰值达 **2,307.9 streams/s**，P50 时延 403.84ms，瞬时内存峰值仅 42.21MB，`InboundLeak = 0`。
  - 正式发布《双通道抗队头阻塞与千级并发基准测试报告》（`docs/BENCHMARKS/DUAL_CHANNEL_HOL_BENCHMARK_REPORT.md`）。
- **DTLS 证书指纹联合防重放与债务彻底出清 (F-04)**：
  - 彻底清算历史陈旧兼容垫片（移除旧版明文密码直接比对、移除无指纹时效分享链接、移除 `legacyPongCh`、移除前端 `legacyFallbackAttempted` 全局房间重试）。
  - 强制要求 WebRTC 握手签名与访客物理 DTLS 证书指纹联合绑定，即使分享链接被中间人窃听，攻击者在不同物理设备上发起握手亦被 100% 拒绝。
- **信令服务 CSWSH 严苛强阻断**：
  - 信令服务端默认对空 Origin 实施 403 强阻断，仅在显式配置 `-allowed-origins` 白名单或 `--allow-all-origins`（开发模式）时方予放行。

### 破坏性变更 (Breaking Changes)

- **出清旧版明文口令握手与无指纹签名**：自 v2.3.0 起，所有客户端连接必须通过 DTLS 证书指纹联合签名鉴权，旧版未绑指纹的直接密码连接将被拒绝。
- **出清单通道 `p2pmux` 协议**：自 v2.3.0 起，网关双端必须使用 `p2pcontrol` 与 `p2pdata` 双通道建立隧道。

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.3.0
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.3.0
```

---

## [v2.2.0] - 2026-11-19

### 核心功能突破与架构演进 (Highlights & Architecture)

- **AgentSession 世代状态机与 CAS 流控**：废除散乱 Map 映射，抽象 `AgentSession` 聚合根与单调递增 `Generation` 计数器，彻底解决网络漂移重连下的旧定时器误杀活跃连接与 CookieJar 缺陷；引入 `tryAcquireStream()` CAS 原子预占循环，彻底杜绝高并发流准入穿透。
- **P2PMux 全局入站内存预算与物理流控**：设立 128MB 全局 Inbound 内存硬顶与 96MB/64MB 双水位背压，主动收缩底层 SCTP 接收通告窗口（`a_rwnd`）；流重置与异常关闭时未读残留缓冲原子销毁，杜绝全局记账泄漏。
- **信令安全准入与控制面防 DoS 治理**：引入 `-allowed-origins` / `SIGNAL_ALLOWED_ORIGINS` 严格校验浏览器跨域 WebSocket（防 CSWSH 劫持）并原生放行 CLI Agent；施加单会话 32 Candidate 转发限额、128 离线等待队列上限与 30 秒半开 SDP 握手主动超时回收。
- **P2PMux Wire 规范形式化与跨端一致性**：正式发布《P2PMux Wire Protocol 形式化规约》，沉淀 10 字节定长帧头与 32KB 阶梯内存对齐法则；固化 15 组跨语言黄金测试向量，Go 与 TypeScript 双端 100% 互通并实现 N-1 跨版本平滑兼容。
- **前端排障可观测性与交互防死锁加固**：离线引导弹窗支持右上角关闭、ESC 与背景点击关闭，退化为非阻塞 Alert 提示条；主页增设版本标识与脱敏环境诊断面板。

### 破坏性变更 (Breaking Changes)

- **无破坏性变更 (None)**。通过双端黄金测试向量与真实 N-1 跨版本套件验证（`test/cross_version_e2e_test.go`），完全向下兼容 v2.1.0 遗留通信协议、配置格式与信令握手。

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.2.0
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.2.0
```
