# 浏览器兼容性矩阵 (Browser Compatibility Matrix)

WebRTC Gateway 基于标准 Web 规范（WebRTC DataChannel、Service Worker、Fetch、Streams 与 WebCrypto Ed25519）运行，无需浏览器插件或私有根证书。本页说明各浏览器的支持分级、验证依据与平台限制。

---

## 1. 浏览器支持分级

| 浏览器系列 | 内核引擎 | 支持等级 | 验证方式 | 备注说明 |
| :--- | :--- | :---: | :--- | :--- |
| **Google Chrome** | Chromium / Blink | **Tier 1** | CI 自动化 CDP E2E | Linux CI 持续运行基于内嵌 `web.DistFS` 的跨站 OOPIF 端到端测试（Chrome 137+ 使用原生 WebCrypto `Ed25519`，Chrome 87–136 自动无缝回退至内置零依赖纯 JS RFC 8032 Ed25519 验签） |
| **Microsoft Edge** | Chromium / Blink | **Tier 1** | 与 Chrome 共用 CDP 套件 | Edge 87+（137+ 原生 WebCrypto `Ed25519`，更早版本自动纯 JS 回退）；本地可通过 `EDGE_BIN` 或 `BROWSER_BIN` 指定二进制运行 |
| **Brave / Vivaldi / Opera** | Chromium / Blink | Tier 2 | 社区验证 | Chromium 87+ 内核（自动适配原生或纯 JS Ed25519 回退）；部分隐私扩展若禁用 WebRTC 本地候选，可能需配合 TURN 中继 |
| **Mozilla Firefox** | Gecko | Tier 2 | 社区验证 | Firefox 103+（129+ 原生 WebCrypto `Ed25519`，更早版本自动纯 JS 回退）；SW 对 `FetchEvent.request.body === undefined` 自动回退 `arrayBuffer()` 读取请求体 |
| **Apple Safari (macOS / iOS)** | WebKit | Tier 2 | 社区验证 | 需 Safari 17+（原生 WebCrypto `Ed25519` 与 Transferable `ReadableStream`）；受 iOS 后台冻结与 `getRemoteCertificates()` 差异约束，见下文 |
| **Android Chromium 浏览器** | Chromium / Blink | Tier 2 | 社区验证 | Chrome Android、Edge Mobile 等现代版本 |

> [!NOTE]
> **Tier 1** 表示每次 CI 均通过 `test/e2e/browser_suite.test.mjs` 自动化实测验证；**Tier 2** 表示基于标准 Web API 预期可用但不在 CI 中自动执行。

---

## 2. 跨引擎边界与休眠恢复机制

### 1. 后台冻结与 Service Worker `P2P_SW_EVICTED` 端口热恢复

- **系统表现**：移动端浏览器切入后台或锁屏后可能终止空闲的 Service Worker 进程，或触发 WebRTC 连接降级。
- **网关恢复流程**：
  1. 引导用 `/__p2p_gw__/bridge.html` 仅在挂载 `appFrame` 前短暂存在，端口移交完成即从 DOM 移除（零时间重叠）；
  2. 沙箱内发起新请求时，若 Service Worker 发现 `bridgePort` 缺失或在 **3000 ms** 内未收到 Portal 的 `REQ_ACK` 响应（经 **3000 ms** `BRIDGE_PING` 探活确认端口失效），立即按标签页淘汰失效端口（保留 `cookieMirror` 与 `clientIds`）并向所属 `appFrame` 广播 `P2P_SW_EVICTED`；
  3. 注入在 `appFrame` 内的 `patch.js` 收到 `P2P_SW_EVICTED` 后经 `window.parent.postMessage` 通知 Portal，由 Portal 重新短暂挂载 `bridge.html` 向 Service Worker 移交新的 `MessagePort`（不调用 `location.reload()` 或依赖 `sessionStorage` 刷新守卫，从结构上避免 Safari ITP / Firefox ETP 刷新死循环）；
  4. 若 WebRTC 传输断开，Portal 通过统一静默重连新建 `RTCPeerConnection` 并复用当前 Tab 内存派生的 SPAKE2+ 验证器 `(w0, w1)` 静默完成重认证，期间保持 `appFrame` 挂载不销毁且不重弹密码框。

### 2. 存储分区、Transferable 流与引擎限制

- **跨站 Cookie 与会话保持**：目标应用的 `HttpOnly` Cookie 完全由 Agent 内存中的 `ProxyCookieJar` 维护，不依赖浏览器第三方 Cookie，因此不受 Safari ITP 拦截；
- **纯内存 `CookieMirror`**：非 `HttpOnly` Cookie 不会写入真实浏览器 `document.cookie`（杜绝跨节点污染与残留），而是由 `patch.js` 在沙箱内存中维护镜像并经 P2PMux V8 行内请求/响应 TLV 双向同步；
- **原生 Transferable `ReadableStream` 与流式解压**：Service Worker 与 Portal 间直接通过可转移 `ReadableStream` 传递请求/响应体（在 Firefox `FetchEvent.request.body === undefined` 场景自动回退 `arrayBuffer()`），并使用浏览器原生 `DecompressionStream('gzip')` 解压 Agent 下发的可压缩响应，零第三方 JS 压缩库依赖；
- **WebKit / Safari 与 Dedicated Worker 已知边界**：
  - WebKit/Safari 若未暴露 `RTCDtlsTransport.getRemoteCertificates()`，浏览器侧依赖底层 DTLS 栈对已签名 SDP 指纹行的绑定校验（Agent 侧始终直接校验远端真实 DTLS 证书）；
  - `patch.js` 仅注入窗口文档上下文，Web 页面内的 `DedicatedWorker` 可经 Service Worker 正常发起 `fetch` / XHR 请求，但不具备 `window` 级 `WebSocket` 隧道补丁（`new WebSocket()` 需由主窗口或经主窗口中转）。

### 3. 安全上下文要求 (Secure Context)

- 按 W3C 规范，`navigator.serviceWorker` 与 `crypto.subtle` 仅在安全上下文中可用：
  - 本地开发：启动 `signal -dev` 后访问 `http://localhost:<port>` 与自动派生的 `http://sbx-*.localhost:<port>`（RFC 6761）；
  - 局域网与公网部署：**必须使用 HTTPS / WSS**（启动期显式配置 `-portal-origin` 与 `-sandbox-origin` 及正式 TLS 证书，或开发期使用 `-tls-auto`）。

---

## 3. 移动端使用建议

- 下载大文件或长时间播放视频时，建议保持浏览器处于前台，避免系统强制休眠标签页中断 SCTP 传输。
- 若浏览器升级后页面仍残留旧缓存，可在站点设置中清理对应沙箱源数据后重新打开 Portal 链接。
