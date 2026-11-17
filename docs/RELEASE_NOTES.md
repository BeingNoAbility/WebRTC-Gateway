# WebRTC Gateway Release Notes

## [v1.9.3] - 2026-11-17

### 🔒 协议与安全紧急加固 (Protocol & Security Emergency Hardening)

本版本为 **v1.9.3 协议与安全加固正式版本**。重点解决深水区隐蔽的幽灵流复活、信令会话越权、HTML 流式注入多字节截断乱码，并实现前后端代理白名单的契约对齐：

- **信令安全全面锁死 (Signaling Security Hardening)**：
  - **Worker 信令鉴权 (SIGNAL_SECRET)**：Cloudflare Worker 边缘节点补齐 `SIGNAL_SECRET` 校验，未授权的客户端连接直接断开并返回 4401 Unauthorized；
  - **IP 计数器清理幂等锁**：Worker 增加 `ipCleaned` 布尔防重锁，杜绝因异常连接双重递减导致的 IP 计数溢出为负数；
  - **严格节点归属校验 (Agent Attribution Check)**：Go 与 Cloudflare Worker 信令服务双端硬性校验 `session.Role == "agent"` 且 `client.NodeID == session.NodeID`，彻底杜绝跨节点仿冒信令包转发。
- **P2PMux 水位线防幽灵流 (P2PMux Ghost Stream Elimination)**：
  - **奇偶性严格校验与流墓碑机制**：客户端与穿透端双向校验 Stream ID 奇偶性；引入高水位线与已关闭流墓碑记录，彻底拦截延迟到达的孤儿数据包，杜绝旧流幽灵复活；
  - **流控溢出 RST 帧通知**：单流缓冲区超过 32MB 硬限制时，在关闭本地流前异步向对端发送 `TypeRstStream` 帧，防止对端挂死等待。
- **Service Worker 纯字节安全流式注入 (Pure Byte Stream Injection)**：
  - 彻底弃用 `TextDecoder` 字符串拼接注入逻辑，改为直接在 `Uint8Array` 字节数组上进行 ASCII 标签扫描 (`findAsciiTagEnd`)；
  - 完美避免跨 chunk 边界截断 UTF-8 多字节字符（如中文字符、Emoji 或复杂二进制流）导致的页面渲染乱码；后续数据块实现纯原生零拷贝透传。
- **前后端代理白名单契约对齐 (Proxy Whitelist Contract Alignment)**：
  - 后端响应头新增 `x-p2p-allowed-domains` 动态透传穿透端配置的白名单域名；
  - 前端 Service Worker 与客户端沙箱统一对齐白名单，彻底清理 `brandShort`、`isRelatedBrand`、`isMedia` 等不可靠的启发式猜测；
  - 非白名单的第三方外部资源自动透传至访客本地网络原生请求，杜绝误拦截导致的 400 Bad Request 报错。
- **沙箱虚拟化与 CSP 策略清晰化**：
  - 彻底清理后端无用死代码 `sanitizeCSP`，并在代码层明确注释全量剥离 CSP 响应头以保障前端沙箱与虚拟化补丁的正常工作。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.9.3` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.9.3` (`latest`)
