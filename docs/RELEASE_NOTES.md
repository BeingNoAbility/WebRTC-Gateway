# WebRTC Gateway Release Notes

## [v2.0.1] - 2026-11-17

### 🛡️ 极端边界防御性加固与协议规范闭环 (Defensive Hardening & Specification Compliance)

本版本为 **v2.0.1 官方维护与防御性加固补丁版本 (Maintenance & Defensive Hardening Release)**。在 v2.0.0 重大里程碑架构（流式硬件解压、ES Module 动态适配、Hibernation 边缘休眠与内存泄露根治）的基础上，针对第三方审计反馈的 3 处深水区极端边界隐患完成最小侵入式闭环，并彻底移除弃用配置别名：

- **Cloudflare DO 边缘休眠状态断裂彻底封堵 (Hibernation State Recovery & Idempotency)**：
  - **持久化上下文存储**：在 Agent 发起 `register` 注册时，将节点的共享密钥（`secret`）、访客强制鉴权标识（`requireVisitorAuth`）与 IP 清理标记（`ipCleaned: false`）写入 WebSocket 会话附件 `ws.serializeAttachment(att)`；
  - **全入口多点懒恢复**：在 Durable Object 的 `fetch()` 与 `handleWSClose()` 入口统一前置调用 `ensureState()`，不仅恢复活跃 Agent 与 IP 连接数，更完整重建 `nodeSecrets` 与 `nodeVisitorAuth` 内存映射，彻底杜绝长时间休眠唤醒后因内存状态丢失而绕过访客鉴权或防劫持校验的隐患；
  - **连接扣减幂等保护**：通过 `ipCleaned` 原子性判断，杜绝因异常反复触发 `handleWSClose` 导致客户端 IP 连接计数器下溢的问题。
- **Gzip 硬件流式解压规避 HEAD 与空体响应陷阱 (HEAD & Bodyless Response Bypass)**：
  - **协议规避规范陷阱**：W3C 规范要求空响应体（如 `HEAD` 方法、`204 No Content`、`304 Not Modified` 及 `Content-Length: 0`）不得包含实体内容，若送入 `DecompressionStream('gzip')` 将抛出 `TypeError: Compressed input was truncated`；
  - **双端协同绕过**：Go 代理端在检测到上述无实体响应时自动抑制 `x-p2p-content-encoding: gzip` 标记并剥离内部 `x-p2p-client-decompress` 与 `x-p2p-dest-host`；Service Worker 客户端在构造合成响应前检测 `HEAD` 请求直接回传 `null` body 并旁路解压管线，确保遵循 W3C Fetch 规范。
- **前端 `mux.ts` 流 ID 奇偶校验消除幽灵流 (Stream ID Parity Enforcement)**：
  - **契约对齐**：对齐 Go 端 `pkg/mux/mux.go` 的规范设计，在前端 `mux.ts` 创建新入站流前强制执行流 ID 奇偶性验证（浏览器客户端仅接收偶数流 ID `frame.streamId % 2 === 0`）；
  - **彻底丢弃残卷**：对客户端自身因超时或切换页面主动取消后仍在管道中回荡的迟到奇数流数据帧予以安全丢弃，彻底消除游离流与僵尸会话的内存堆积。
- **信令服务端参数全面收敛 (Signaling CLI Flag Hardening)**：
  - 彻底移除 `cmd/signal/main.go` 中历史兼容别名 `-secret`，强制要求显式指定 `-signal-secret`；
  - 传入遗留参数时实行即时阻断（Fail-Fast）并打印清晰的整改建议，彻底规避运维人员因误配导致的未鉴权暴露风险。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v2.0.1` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.1` (`latest`)
