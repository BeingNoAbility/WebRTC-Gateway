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
