# 浏览器兼容性矩阵 (Browser Compatibility Matrix)

WebRTC Gateway 基于标准 Web 规范（WebRTC DataChannel、Service Worker、Fetch、Streams 与 WebCrypto Ed25519）运行，无需浏览器插件或私有根证书。本页说明各浏览器的支持分级、验证依据与平台限制。

---

## 1. 浏览器支持分级

| 浏览器系列 | 内核引擎 | 支持等级 | 验证方式 | 备注说明 |
| :--- | :--- | :---: | :--- | :--- |
| **Google Chrome** | Chromium / Blink | **Tier 1** | CI 自动化 CDP E2E | Linux CI 持续运行基于内嵌 `web.DistFS` 的跨站 OOPIF 端到端测试 |
| **Microsoft Edge** | Chromium / Blink | **Tier 1** | 与 Chrome 共用 CDP 套件 | 本地可通过 `EDGE_BIN` 或 `BROWSER_BIN` 指定二进制运行 |
| **Brave / Vivaldi / Opera** | Chromium / Blink | Tier 2 | 社区验证 | 部分隐私扩展若禁用 WebRTC 本地候选，可能需配合 TURN 中继 |
| **Mozilla Firefox** | Gecko | Tier 2 | 社区验证 | 需支持 WebCrypto `Ed25519`、Service Worker 与 `ReadableStream` |
| **Apple Safari (macOS / iOS)** | WebKit | Tier 2 | 社区验证 | 需支持 WebCrypto `Ed25519`（Safari 17+）；受 iOS 后台冻结约束，见下文 |
| **Android Chromium 浏览器** | Chromium / Blink | Tier 2 | 社区验证 | Chrome Android、Edge Mobile 等现代版本 |

> [!NOTE]
> **Tier 1** 表示每次 CI 均通过 `test/e2e/browser_suite.test.mjs` 自动化实测验证；**Tier 2** 表示基于标准 Web API 预期可用但不在 CI 中自动执行。

---

## 2. WebKit (Safari / iOS) 与休眠恢复机制

### 1. 后台冻结与 Service Worker 端口热恢复

- **系统表现**：移动端浏览器切入后台或锁屏后可能终止空闲的 Service Worker 进程，或触发 WebRTC 连接降级。
- **网关恢复流程**：
  1. 沙箱内发起新请求时，Service Worker 若发现 `bridgePort` 缺失或在 **1500 ms** 内未收到 Portal 的 `REQ_ACK` 响应（对无体幂等请求并经 `BRIDGE_PING` 探活确认端口失效），立即淘汰失效端口（保留 `cookieMirror` 与 `clientIds`）并向隐藏桥接页 `/__p2p_gw__/bridge.html` 发送 `SW_NEED_PORT`；
  2. 若 Service Worker 空闲终止后冷重启，仅在单 Tab 且 `clients.get(clientId)` 确认为同源嵌套 `appFrame` 或 Worker 时自动唤醒隐藏桥接页重挂活端口；
  3. 隐藏桥接页不调用 `location.reload()` 或 `sessionStorage` 刷新守卫，从结构上避免 Safari ITP / Firefox ETP 刷新死循环；
  4. 若 WebRTC 传输断开，Portal 通过统一静默重连新建 `RTCPeerConnection` 并复用当前 Tab 内存派生的 SPAKE2+ 验证器 `(w0, w1)` 静默完成重认证，期间保持 `appFrame` 挂载不销毁且不重弹密码框。

### 2. 存储分区与智能防跟踪 (ITP)

- **跨站 Cookie 与会话保持**：目标应用的 `HttpOnly` Cookie 完全由 Agent 内存中的双池 `ProxyCookieJar` 维护，不依赖浏览器第三方 Cookie，因此不受 Safari ITP 拦截；
- **纯内存 `CookieMirror`**：非 `HttpOnly` Cookie 不会写入真实浏览器 `document.cookie`（杜绝跨节点污染与残留），而是由 `ws_patch.ts` 在沙箱内存中维护镜像并经 P2PMux V7 行内请求/响应 TLV 双向同步。
- **原生流式解压**：浏览器侧直接使用标准 `DecompressionStream('gzip')` 解压 Agent 下发的可压缩响应，零第三方 JS 压缩库依赖。

### 3. 安全上下文要求 (Secure Context)

- 按 W3C 规范，`navigator.serviceWorker` 与 `crypto.subtle` 仅在安全上下文中可用：
  - 本地开发：`http://localhost:<port>` 与自动派生的 `http://sbx-*.localhost:<port>`（RFC 6761）；
  - 局域网与公网部署：**必须使用 HTTPS / WSS**（配置双域正式 TLS 证书，或开发期使用 `-tls-auto`）。

---

## 3. 移动端使用建议

- 下载大文件或长时间播放视频时，建议保持浏览器处于前台，避免系统强制休眠标签页中断 SCTP 传输。
- 若浏览器升级后页面仍残留旧缓存，可在站点设置中清理对应沙箱源数据后重新打开 Portal 链接。
