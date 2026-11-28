# WebRTC Gateway Release Notes

## [v5.0.0-alpha.4] - 2026-11-28

> 预发布版本（pre-release）：WebRTC Gateway P2PMux V8 安全审计加固预发布版本，不建议用于生产环境。预发布标签不移动 `:latest`（可通过 `:v5.0.0-alpha.4` 或 `:alpha` 拉取）。
>
> **English readers**: see [CHANGELOG.md](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/CHANGELOG.md) for the full English release notes.

### 核心架构特性

- **P2PMux V8 统一多路复用数据面与流式 WebSocket**：单一 `RTCPeerConnection` 预协商 `1` 条分片控制通道（`id=0` `p2pctrl`）+ `12` 条有序多路复用通道（`id=1..12` `p2p_01..p2p_12`），按单调 `StreamID` 统一承载 HTTP 与流式 WebSocket（零动态 DCEP 通道）；采用 8 字节定长帧头、纯载荷字节 `CREDIT` 流控、原生 Transferable `ReadableStream` IPC 与二进制 TLV 编码（`FetchCtx`、`RespFlags`、双向 Cookie 变更），流式压缩采用标准库 `compress/gzip` 与浏览器原生 `DecompressionStream('gzip')`。
- **对称 3 态认证、RFC 9383 SPAKE2+ 与静默重连**：信令仅传输结构化 `SessionDescriptor` 并由双端确定性合成 16 行 SDP；Agent 持久化 Ed25519 密钥对长度前缀转录签名并在建连后校验远端真实 DTLS 证书指纹；DTLS 控制通道内执行对称 3 态认证与 RFC 9383 SPAKE2+（P-256、300,000 轮 PBKDF2 + HKDF）双向零知识口令认证，并在认证前原子拒收非控制通道帧；断网后 Portal 后台静默重建 PC 并凭 `cookie_resume_token` 续接 Cookie 状态；支持可实时吊销、限路径、限只读且原子落盘的 Ed25519 能力令牌。
- **两档沙箱隔离、启动期显式源与 W3C Fetch 抗 CSRF 门禁**：单一 Go `cmd/signal` 二进制于启动期预渲染内嵌内容哈希资产与 ETag，要求显式配置 `-portal-origin` + `-sandbox-origin`（本地回环显式 `-dev`），提供 Tier 1 跨站通配符隔离与 Tier 2 受限隔离（固定单域跨站或 `-allow-same-site-sandbox` 同站通配符，门户展示 Tier 2 警示条）；SW 外置 `/__p2p_gw__/boot.js` 并按指令级重写保留上游 CSP、无条件阻断 `destination=document` 逃逸与按标签页隔离 `clientToTab` 配额，配合 `CookieMirror` / `ProxyCookieJar` 隔离 `HttpOnly` Cookie。
- **纯管道代理、`O_NOFOLLOW` 静态适配器与严苛门禁**：HTTP/WS 统一复用单目标 `DestinationPolicy`（可选 `-upstream-host` 覆写、WS 302 跳转拦截与 Resolve 后置能力校验）；`-file` / `-dir` 通过 Go `os.Root` 只读适配器默认强制 `O_NOFOLLOW` 拒绝全部软链与点文件（可选 `-follow-symlinks` 严守根与能力前缀边界）并默认下发 `CSP: sandbox`；支持 `pkg/state` 原子落盘、饱和 Fail-Closed 限流、最小 16 字节机器密钥、Go `-race`/`gosec`/Fuzz、黑盒一致性测试及真实 Chrome CDP E2E 校验。

### 容器镜像拉取

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v5.0.0-alpha.4
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v5.0.0-alpha.4
# 或拉取 alpha 通道最新构建（不会影响 :latest）
docker pull ghcr.io/beingnoability/webrtc-p2p-server:alpha
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:alpha
```
