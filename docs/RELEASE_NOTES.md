# WebRTC Gateway Release Notes

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
