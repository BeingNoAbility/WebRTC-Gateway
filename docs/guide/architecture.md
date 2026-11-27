# 系统架构与核心设计 (Architecture & Design)

WebRTC Gateway 由三大解耦组件构成：
1. **Browser Client（模块化浏览器运行时）**：包含独立 Portal（内容哈希 `/__p2p_gw__/portal.<hash>.js` 与 `portal.<hash>.css`）、隐藏非导航桥接页（`/__p2p_gw__/bridge.html`）、模块化 Service Worker（`/__p2p_gw__/sw.js`）、纯兼容层 DOM 与内存 `CookieMirror` 补丁（`/__p2p_gw__/patch.<hash>.js`）以及 TypeScript P2PMux V7；
2. **Signaling Server（单一权威 Go 信令）**：单二进制 `cmd/signal`（`-tags embedweb` 内嵌前端内容哈希资产清单），仅撮合结构化 `SessionDescriptor` Offer/Answer 与 ICE 候选，绝不中继或解码业务数据；
3. **Agent Daemon（内网守护进程）**：持久化 Ed25519 身份、RFC 9383 SPAKE2+ DTLS 通道内认证、单 PC 32-DC 池化 P2PMux V7 复用与 Pure Pipe HTTP/16 KiB 流式 WebSocket/`os.Root` 代理。

---

## 1. 端到端系统交互时序

```mermaid
sequenceDiagram
    autonumber
    actor Visitor as 访客浏览器 (Portal Origin)
    participant Signal as 信令服务 (Go Signal)
    participant Agent as 内网 Agent (Ed25519 Identity)
    participant Target as 本地服务 / os.Root 静态目录

    Agent->>Signal: Upgrade Bearer SIGNAL_SECRET + Ed25519 register-v2 注册节点
    Visitor->>Signal: 连接 /ws/client/{node_id}，获取 agent_pk 并先校验 pk=/TOFU
    Visitor->>Signal: 发送结构化 SessionDescriptor Offer + 32B Nonce
    Signal->>Agent: 转发 Offer（覆盖写入权威客户端 IP）
    Agent->>Signal: 回复结构化 Answer + Pure Ed25519 长度前缀签名
    Signal->>Visitor: 转发 Answer
    Note over Visitor,Agent: 双端确定性合成 16 行 SDP，并在 DTLS 建连后校验真实远端证书指纹

    Visitor<<->>Agent: 单 PeerConnection + p2pctrl + 32 条 HTTP 池 DC + 动态 p2pws DC
    Visitor<<->>Agent: p2pctrl 内完成 RFC 9383 SPAKE2+ 双向零知识认证或能力令牌校验
    Note over Visitor,Agent: 业务数据仅在端到端 DTLS 加密通道内传输

    Visitor->>Agent: 跨站沙箱 SW -> TunnelBridge -> P2PMux V7 TLV 请求
    Agent->>Target: 经 DestinationPolicy + ProxyCookieJar 转发 HTTP/WS
    Target-->>Agent: 响应头与流式 Body
    Agent-->>Visitor: 池通道回传（原生流式 Gzip 压缩 + 内存 CookieMirror 同步）
```

---

## 2. 单 PC DC 池化数据面与原生流式 Gzip

- **信道拓扑**：每个访客会话仅维持 1 条 `RTCPeerConnection`，通过 `{ negotiated: true, ordered: true }` 预协商 1 条控制通道 `p2pctrl`（`id=0`）与 32 条可复用 HTTP 池通道 `p2p_01..p2p_32`（`id=1..32`，每通道单活跃流 5 态 FSM + 单调 `uint32 Gen`），并为每个 WebSocket 动态创建独立有序 DCEP 通道 `"p2pws"`。详细线格式与校验规则见 [P2PMux V7 协议规范](./protocol-specification)。
- **天然无队头阻塞**：每个活跃 HTTP 流独占一条有序 SCTP 通道，由 SCTP 原生保证 `HEADERS` 必先于 `DATA` 到达；旧代迟到帧按 `f.Gen != active.Gen` O(1) 丢弃，从结构上避免跨流队头阻塞、无界流状态表与 2 MiB WebSocket 死锁。
- **原生流式 Gzip 压缩**：Service Worker 通过 V7 `FetchCtx` 声明客户端原生解压能力，Agent 使用 Go 标准库 `compress/gzip` 流式压缩并在每次写入后立即 `Flush`（SSE、`206 Partial Content`、`Cache-Control: no-transform` 与空体自动旁路并保留强 `ETag`），通过 `RespFlag.DECOMPRESS_GZIP` 通知浏览器调用原生 `DecompressionStream('gzip')`（首块 0B 窥探 + 膨胀比与输出上限保护，零第三方解压库依赖）。
- **统一静默重连**：ICE/PC/控制通道断开后，Portal 直接建立新 `RTCPeerConnection` 并复用当前 Tab 内存派生的 SPAKE2+ `(w0, w1)` 静默完成重认证，保持 `appFrame` 挂载不重载且不重弹密码框。

---

## 3. 跨站三档沙箱与模块化浏览器运行时

### 1. 三档隔离、隐藏桥接页 (`/__p2p_gw__/bridge.html`) 与一次性 `enterNonce`

- **三档沙箱隔离**：生产环境支持 Tier 1 双固定域（`-sandbox-host`）、Tier 2 单根域同站通配符（`-allow-same-site-sandbox` + `Origin-Agent-Cluster: ?1`）与 Tier 3 双域通配符（`-sandbox-domain`，本机私有盐 HMAC 派生 `sbx-<24hex>`；本地回环自动 `*.localhost`），能力分享访客按 Tab 分配 128 位随机 `sbx-<32hex>` 源；
- **零 Service Worker 门户、W3C Fetch 抗 CSRF 门禁与 `appFrame` 沙箱**：Portal 源拒绝服务 `/__p2p_gw__/sw.js` 与 `/__p2p_gw__/bridge.html`（返回 `404`），CSP 严格锁定 `style-src 'self'`、`connect-src 'self'`、`form-action 'self'` 与 `frame-ancestors 'none'`；沙箱源通过隐藏 `/__p2p_gw__/bridge.html?tab=<tabId>` 校验 `<meta name="p2p-portal-origins">` 并注册 `{ scope: '/' }` SW；挂载 `appFrame`（带 `sandbox` 属性禁用顶层跳转）前先向 SW 注册一次性 128 位 `enterNonce`，SW 对全部未绑定子资源与跨站导航实施严格 W3C Fetch 抗 CSRF 门禁并下发 `frame-ancestors 'self' <portalOrigin>`；
- **不可信租户防御**：Portal `TunnelBridge` 与 Go Agent 将整个沙箱源视为不可信租户——请求头在边界处剥离伪造转发头，下载补窗 `ackConsumed` 严格受限于已投递字节，WebSocket 强制 16 MiB 单消息上限，且 `HttpOnly` Cookie 仅保存在 Agent 内存的双池 RFC 6265bis `ProxyCookieJar` 中。

### 2. 流式 HTML5 头部注入与内存 `CookieMirror`

- **字节级 HTML5 分词器与 1024B `<meta charset>` 预扫描**（`web/src/sw/html_injector.ts`）：完整处理 HTML5 注释、未加引号属性、`<script>` 双重转义、`<template>`/`<svg>`/`<math>` 嵌套与自闭合、UTF-16 BOM 旁路与跨块 UTF-8 BOM；HTTP 头省略 `charset=` 时在 `<head>` 内最多前窥 1024 字节避免将 `<meta charset>` 挤出预扫描窗口，以单一 `<script src="${origin}${PATCH_ASSET_URL}" data-p2p-boot="..."></script>` 注入；
- **内存 `CookieMirror` 与 PWA 隧道保护**：`ws_patch.ts` 覆写 `Document.prototype.cookie` 为纯内存镜像且绝不写入真实 `document.cookie`，经 V7 行内请求/响应 TLV 与 Agent 双池 `ProxyCookieJar` 双向同步增删，并私有保存真实 `navigator.serviceWorker` 句柄对外屏蔽 `register`/`unregister`/`getRegistrations`，防止内网 PWA 误卸载隧道 SW；
- **安全端口探活与 SW 休眠恢复**：SW 仅对无体幂等请求（`GET`/`HEAD`/`OPTIONS`）在 1500 ms `REQ_ACK` 超时后取消旧请求并经 `BRIDGE_PING` 探测端口失效后再重建；SW 空闲重启后仅在单 Tab 且 `clients.get(clientId)` 确认为同源嵌套 `appFrame` 或 Worker 时自动唤醒 `bridge.html`。

### 3. 前端模块分层 (`web/src/`)

- `web/src/protocol/`：P2PMux V7 帧编解码（`frame.ts`）、二进制 TLV 编解码（`tlv.ts`）与 32-DC 池及动态 WS 复用器（`mux.ts`）；
- `web/src/shared/`：严格类型 IPC 契约（`ipc.ts`）、WebCrypto 与 HKDF 原语（`crypto.ts`）、RFC 9383 SPAKE2+（`spake2p.ts`）、Fail-Closed Ed25519 TOFU 存储（`identity.ts`）、结构化 SDP 抽取与合成（`sdp.ts`）、三档沙箱推导（`sandbox_origin.ts`）及严格 URL 重写（`url_rewrite.ts`）；
- `web/src/portal/`：配置与 RFC 7065 ICE 校验（`core/config.ts`，自动清洗 `cap=`）、信令客户端（`core/signaling_client.ts`）、单 PC 传输与静默重连（`core/peer_session.ts`）、3 态 SPAKE2+/能力认证状态机（`core/auth_client.ts`）、遥测采集（`core/stats_collector.ts`）、跨站桥接（`core/bridge.ts`）、外置样式（`portal.css`）以及轻量 UI（`ui/modals.ts`、`ui/smart_pill.ts`、`i18n.ts`、`app.ts`）；
- `web/src/bridge/` & `web/src/sw/` & `web/src/patch/`：极简隐藏桥接页、模块化 SW 抗 CSRF 路由/流控/原生 Gzip 解压/HTML5 注入/内存 CookieMirror，以及纯兼容层 `ws_patch.ts`。

---

## 4. 三档沙箱部署形态对比

| 维度 | Tier 1 跨站双固定域 | Tier 2 单根域同站通配符 | Tier 3 跨站双域通配符（推荐） |
| :--- | :--- | :--- | :--- |
| **域名与证书** | 两个不同 eTLD+1 固定主机，普通单域证书 | 单根域 + 通配符子域证书 | 两个不同 eTLD+1 域，沙箱侧泛域证书 |
| **关键参数** | `-portal-domain` + `-sandbox-host` | `-portal-domain` + `-sandbox-domain` + `-allow-same-site-sandbox` | `-portal-domain` + `-sandbox-domain` |
| **隔离特性** | 跨站站点级隔离 + SW 抗 CSRF 门禁 | 节点独立源 + `Origin-Agent-Cluster: ?1`（同站警示条） | 跨站站点级隔离 + 节点私有盐 HMAC 独立源 |
| **适用场景** | 无通配符证书能力的常规服务器 | 仅有单域名且仅穿透互信应用 | 多节点生产环境与企业内网 |
