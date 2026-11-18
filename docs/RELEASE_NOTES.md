# WebRTC Gateway Release Notes

## [v2.1.0] - 2026-11-19

### 核心更新与修复 (Highlights & Fixes)

- **协议流式韧性与并发防死锁**：修复 GET/HEAD 无 Body 流未发 FIN 导致的映射表内存泄漏；根治 `m.bpCond.Wait()` 在流被远端 Reset 移出映射表后的自旋假死；引入 Trickle ICE 候选挂起队列，彻底消除 RemoteDescription 未就绪时的丢包竞态。
- **跨域沙箱与 IPC 通道安全加固**：在 `app.ts` 与 `ws_patch.ts` 中补齐 `event.origin` 与 `event.source` 严格校验，消除通过 `window.open` 或跨域 iframe 劫持内网 WebSocket 控制流的高危破防点，并废弃通配符 `'*'` postMessage。
- **Cloudflare Worker SQLite Durable Object 实战套件**：引入原生 WebSocket 休眠与在线探测验证（`test/cf_worker_e2e_test.go`），实现零主机依赖的生产级 Serverless 边缘信令与房间分片。
- **Linux 混沌靶场与工程发布准入体系**：完成 Debian 13 原生内核高损耗高延迟（`tc qdisc 30% loss`）实网穿透签核；发布测试核查清单 2.0 与版本废弃生命周期门禁。

### 破坏性变更 (Breaking Changes)

- **无破坏性变更 (None)**。完全向下兼容既有通信协议、配置格式与信令握手。

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.1.0
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.1.0
```
