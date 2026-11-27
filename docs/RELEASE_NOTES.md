# WebRTC Gateway Release Notes

## [v5.0.0-alpha.3] - 2026-11-27

> 预发布版本（pre-release）：WebRTC Gateway V7 架构预发布版本，不建议用于生产环境。预发布标签不移动 `:latest`（可通过 `:v5.0.0-alpha.3` 或 `:alpha` 拉取）。
>
> **English readers**: see [CHANGELOG.md](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/CHANGELOG.md) for the full English release notes.

### 核心架构特性

- **P2PMux V7 数据面与流式 WebSocket**：单一 `RTCPeerConnection` 预协商 `1 Ctrl (id=0)` + `32` 条可复用有序 HTTP 池 DataChannel（5 态单槽 FSM、单调 `uint32 Gen`、事件驱动 `OnBufferedAmountLow` 背压与开销感知内存预算），动态 DCEP `"p2pws"` 通道以 16 KiB 固定缓冲流式转发 WebSocket 帧并保留合法 RFC 6455 关闭码；元数据（`FetchCtx`、`RespFlags`、双向 Cookie 变更）原生编入 V7 二进制 TLV，流式压缩采用标准库 `compress/gzip` 与浏览器原生 `DecompressionStream('gzip')`。
- **对称 3 态认证、RFC 9383 SPAKE2+ 与静默重连**：信令仅传输结构化 `SessionDescriptor` 并由双端确定性合成 16 行 SDP；Agent 持久化 Ed25519 密钥对长度前缀转录签名并在建连后校验真实 DTLS 证书指纹；DTLS 控制通道内执行对称 3 态认证与 RFC 9383 SPAKE2+（P-256、300,000 轮 PBKDF2 + HKDF）双向零知识口令认证，并在认证前原子拒收非控制通道帧；断网后 Portal 后台静默重建 PC 并凭 `cookie_resume_token` 续接 Cookie 状态，保持应用不刷新；支持可实时吊销、限路径、限只读且原子落盘的 Ed25519 能力令牌。
- **三档跨站沙箱隔离与 W3C Fetch 抗 CSRF 门禁**：单一 Go `cmd/signal` 二进制按构建清单内嵌内容哈希前端资产，支持 Tier 1 双固定域（`-sandbox-host`，无需通配符证书）、Tier 2 单根域通配符（`-allow-same-site-sandbox` + `Origin-Agent-Cluster: ?1`）与 Tier 3 双根域通配符（`-sandbox-domain`，本机私有盐派生子域，本地自动 `*.localhost`）；门户 CSP 锁定 `style-src 'self'` 与 `connect-src 'self'`，SW 强制一次性 `enterNonce` 与 W3C Fetch 抗 CSRF 门禁并下发 `frame-ancestors`，`appFrame` 启用 HTML `sandbox` 属性；前端纯内存 `CookieMirror` 配合双池 RFC 6265bis `ProxyCookieJar` 并屏蔽 PWA 误卸载隧道 SW。
- **纯管道代理、逐级 `os.Root` 静态适配器与严苛门禁**：HTTP/WS 统一复用单目标 `DestinationPolicy`（可选 `-upstream-host` 覆写、WS 302 跳转拦截与 Resolve 后置能力校验）；`-file` / `-dir` 通过 Go `os.Root` 逐级 `OpenRoot` 只读适配器拦截跨根软链与点文件并默认对全部静态文件下发 `CSP: sandbox`；支持 `pkg/state` 原子落盘、饱和 Fail-Closed 限流、共享密钥默认禁止 `-allow-force-rekey`、Go `-race`/`gosec`/Fuzz、黑盒一致性测试及真实 Chrome CDP E2E 校验。

### 容器镜像拉取

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v5.0.0-alpha.3
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v5.0.0-alpha.3
# 或拉取 alpha 通道最新构建（不会影响 :latest）
docker pull ghcr.io/beingnoability/webrtc-p2p-server:alpha
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:alpha
```
