# WebRTC Gateway Release Notes

## [v2.0.3] - 2026-11-18

### 🛡️ 全面安全加固与深水区协议重构 (Comprehensive Security Hardening & Protocol Restructuring)

本版本为 **v2.0.3 安全加固与深水区协议重构版本 (Security Hardening & Protocol Restructuring Release)**。针对第三方安全审计与深度推演识别出的潜在隐患，系统性落实了 10 项核心安全修复与协议健壮性升级，实现零回归、零信任全方位闭环：

- **SSRF 深度防御与边界阻断 (F-01)**：
  - **混淆字符拦截**：在 `http_proxy.go` 与 `ws_proxy.go` 中严格阻断包含 `@`、`\`、`/` 或空白字符的非法目标主机名；
  - **后缀匹配收敛**：移除当目标为 IP 或 localhost 时的模糊子域名后缀通配，杜绝子域名混淆绕过；
  - **受限网段全覆盖**：补齐 `0.0.0.0/8`、广播、多播、IPv4-mapped (`::ffff:`)、NAT64 (`64:ff9b::/96`) 与 6to4 (`2002::/16`) 限制网段；
  - **环境代理剥离**：消除无意继承宿主机 HTTP 代理环境变量导致的内部网络探测风险（`Proxy: nil`）。
- **凭据生命周期与 SDP 指纹防重放签名 (F-02, F-04)**：
  - **单向派生密钥**：Portal 端通过 Web Crypto 计算 `authKey = HMAC(secret, "node-auth:" + peerId)`，派生后立即清空明文密钥输入与内存，禁止持久化明文凭据；
  - **SDP DTLS 指纹绑定**：信令 Offer 彻底移除明文 `secret`，提取本地 SDP DTLS 证书指纹 `a=fingerprint:sha-256 ...`，使用派生密钥计算 `sign = HMAC(authKey, peerId:exp:sdpFingerprint)` 载入 Offer，Agent 端常数时间比对，彻底杜绝中间人重放与会话劫持；
  - **时间戳严格校验**：强制签名必须包含有效正整数过期时间戳（`exp <= 0` 严格阻断），封死永久免签绕过漏洞；
  - **活跃节点防抢注**：Go 信令服务器与 Cloudflare Worker 同步实施防劫持保护，禁止外部匿名连接对无保护的活跃节点半途注入 Secret 夺权，并限制 `nodeSecrets` 上限与淘汰机制。
- **Cloudflare Worker TDZ 修复与防暴破限频 (F-03, F-08)**：
  - **消除 TDZ 崩溃**：调整 `requireVisitorAuth` 变量声明顺序，彻底修复 Agent 注册时的 `ReferenceError` 崩溃；
  - **语法防线自动化**：将 `test_worker_syntax.cjs` 接入 CI 测试套件，构建前执行 V8 静态语法与作用域前置断言；
  - **防暴破限频与防时序攻击**：Worker 针对单 IP 认证失败实现速率限制（>= 10 次返回 429 阻断），并引入 `crypto.subtle.timingSafeEqual` 实现常数时间比较。
- **TLS 校验严格化与安全分流 (F-05)**：
  - **消除硬编码免验**：移除全局硬编码 `InsecureSkipVerify: true`，默认启用严格 CA 根证书链校验；
  - **特异性安全通道**：仅在显式传入 `-insecure` 且完全匹配目标主机时特异性启用免验传输器；外部子域与 `--p2p-proxy` 始终强制严格校验。
- **FileProxy 301 死循环与软链逃逸根除 (F-06)**：
  - **物理路径锚定**：使用 `filepath.EvalSymlinks` 锚定真实物理根路径；
  - **死循环根治**：改用 `http.ServeContent` 与自研目录渲染，杜绝 `/subdir/` 301 重定向递归，并对 `index.html` 强制沙箱复验阻断软链逃逸。
- **Mux 优雅关闭、死锁消除与流 ID 回绕保护 (F-07, F-10)**：
  - **背压条件变量死锁消除**：`writeFrame` 在等待背压条件变量前检查 `m.closed` 与 `s.Context().Done()`，消除死锁；
  - **全生命周期平滑释放**：新会话替换、优雅关闭显式调用 `mux.Close()`；探针流缓冲上限从 16MB 收敛至 64KB；
  - **32 位流 ID 回绕保护**：Go 与 TypeScript Mux 同步支持到达 `0xFFFF0000` 后的安全回绕、墓碑清理与对端回绕识别重置。
- **Cookie 双向命名空间虚拟化打通 (F-09)**：
  - **双向虚拟化**：前端 `ws_patch.ts` 对 `document.cookie` 的 Getter/Setter 自动加减 `__p2p_${nodeId}_` 前缀；
  - **作用域写入**：Portal 端新增 `writeScopedCookie`，确保后端下发的非 HttpOnly Cookie（如 CSRF Token）透明注入对应前缀，与内层应用无缝衔接；
  - **会话持久化隔离**：修复切换节点时 `clientToken` 跨节点漂移与派生密钥漂移；后端 `CookieJar` 施加 1024 槽位限制与 LRU 淘汰。
- **HTML DOCTYPE 注入定位修复 (F-10)**：
  - 脚本注入精确定位至 `<!DOCTYPE ...>` 标签末尾 `>` 后，彻底规避浏览器 Quirks Mode 怪异模式渲染。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v2.0.3` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.3` (`latest`)
