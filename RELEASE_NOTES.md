# WebRTC Gateway Release Notes

## [v1.8.1] - 2026-11-17

### 🔒 紧急安全加固与通用防御架构收敛 (Emergency Security Hardening & Universal Defense Architecture)

本版本为 **P0 级紧急安全补丁与通用架构收敛版本**。经独立代码审计研判，系统摒弃特例打补丁思维，从架构契约、安全边界与状态机层面进行了彻底的泛化治理：

- **Portal 控制台 DOM XSS 彻底根除 (P0-2)**：
  - 建立强正向字符集门禁（`/^[a-zA-Z0-9._-]{1,64}$/`），对 6 大入口来源进行格式清洗，非规范字符直接拒绝并降级兜底；
  - 弹窗与调试视图彻底废除模板字符串裸拼，全面改用标准 DOM API 属性设值（`input.value = ...`）与通用 HTML 实体转义。
- **全链路切断 Secret 口令泄露通道 (P0-3)**：
  - 回归最小特权架构契约（PoLP），物理拔除 Service Worker 对代理 HTML 页面的 `window.__P2P_SECRET__` 注入；
  - 移除 URL Query 中 `?secret=` 的拼接泄漏；移除信令 `join` 报文及 iframe 间无序广播的口令字段。
- **`/--p2p-proxy/` 跨站代发全面收敛至显式白名单 (P0-4)**：
  - 彻底废除基于后缀模糊匹配（`.mp4` 参数绕过）及关键字匹配的启发式规则；
  - 默认仅允许同源及根域名子域代理，新增 `-allowed-proxy-domains` 通配符白名单配置，实现通用且安全的外链多媒体与图床直连；
  - 解耦信令代理与后端反代，`-proxy` 严格限定于信令外出通道。
- **信令端前置熔断暴破封禁与敏感端点收敛 (P0-5)**：
  - 在信令服务握手阶段实行 `IsLocked` 滑动窗口硬性阻断，封禁期间瞬时切断握手；
  - 彻底移除 Cloudflare Worker 未鉴权的 `list_nodes` 节点枚举端点。
- **边缘信令代码权威源归一 (P0-1)**：
  - 物理删除 `web/worker.js`，统一以 `edge/cloudflare-worker/worker.js` 为单一权威源，杜绝版本分叉隐患。
- **Agent 并发活跃连接竞态消除 (P1-1)**：
  - 将 `activePeers` 的原子状态同步全面纳入 `connMu.Lock()` 保护临界区，消除极端网络震荡下的 Data Race。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.8.1` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.8.1` (`latest`)
