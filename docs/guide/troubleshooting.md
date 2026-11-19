# 故障排查与常见问题 (Troubleshooting & FAQ)

---

## 1. 常见故障排查手册

### ① 页面一直卡在“正在连接 Agent (Connecting)”或最终报 ICE 连接失败
- **故障根因**：双方网络存在最严格的**对称型 NAT (Symmetric NAT / NAT 4)**，导致纯 STUN 反射候选无法完成打洞；
- **排查与解决**：
  1. 打开右下角 WebRTC HUD 悬浮球，查看 ICE Candidates 候选列表；
  2. 若两端都是对称型 NAT，必须配置 TURN 中继服务器以实现平滑降级：
     ```bash
     ./agent -node-id myhome \
             -target http://127.0.0.1:8080 \
             -turn-url turn:relay.yourdomain.com:3478 \
             -turn-user your_username \
             -turn-pass your_password
     ```
  3. 配置 TURN 后，系统将自动使用 `relay` 候选在中继模式下完成直连，HUD 状态药丸将切换为 `🛡️ TURN Relay`。

---

### ② 局域网纯 IP 访问时 Service Worker 注册失败或页面白屏
- **故障根因**：现代浏览器安全规范（W3C Secure Context）强制要求：**Service Worker 仅允许在 `localhost` 或具备有效 SSL/TLS 证书的 `https://` 域下激活**。在局域网直接用纯 HTTP 打开 `http://192.168.1.100:8888` 会被浏览器直接拦截 Service Worker；
- **一键解法**：
  在启动一体化信令服务端时增加 `-tls-auto` 参数：
  ```bash
  ./signal -addr :8888 -tls-auto
  ```
  服务端将在内存中动态签发覆盖本机所有局域网真实物理网卡 IP 的 ECDSA SAN 证书。在浏览器直接打开 `https://<局域网IP>:8888/#myhome` 接受证书提示，即可完整激活 Service Worker！

---

### ③ 移动设备（手机/笔记本）在 Wi-Fi 与 5G 移动蜂窝网络间切换时连接断开
- **自动自愈机制**：
  从 **v1.4.0** 起，系统原生内建了**双阶段 WebRTC ICE Restart 韧性流水线**：
  1. 手机切网瞬间底层 TCP/WebSocket 套接字断开，浏览器自动闪连信令并保留原有 PeerConnection；
  2. 触发带有 `iceRestart: true` 的新一轮 Offer 协商；
  3. Agent 具备 5 秒断网宽限保护期，取消提前析构，在新网络建立后无缝继续传输数据；
- **排查提示**：若漫游自愈偶尔失效，点击右下角 HUD 诊断看板中的 **`🔄 立即触发 ICE Restart`** 按钮可一键手工自愈。

---

### ④ Docker 容器内的 Agent 无法访问宿主机的 `http://127.0.0.1:8080`
- **故障根因**：Docker 容器默认处于独立的 Bridge 网络命名空间中，容器内的 `127.0.0.1` 指向容器自身而非宿主机；
- **推荐解法**：
  - **Linux 宿主机**：在 `docker-compose.yml` 或 `docker run` 中添加 `network_mode: "host"`（或 `--net=host`），Agent 直接共享宿主机网络栈，访问 `127.0.0.1` 畅通无阻；
  - **Windows / macOS Docker Desktop**：将 `-target` 设置为 `http://host.docker.internal:8080`。

---

### ⑤ 提示 401 Unauthorized 或“信令集群准入拒绝”
- **故障根因**：新旧版本密钥混淆或配置不匹配；
- **排查清单**：
  1. **检查控制面集群密钥**：确认信令服务端的 `-signal-secret` 与 Agent 启动时的 `-signal-secret` 完全一致；
  2. **检查数据面访客口令**：若 Agent 配置了 `-access-secret`，访客必须在弹窗中输入该口令，或使用对应的 HMAC-SHA256 签名分享链接（`#nodeId?exp=xxx&sign=yyy`）；
  3. **升级提示**：旧版 `-secret` 与 `SECRET` 已彻底废弃并完全移除，请勿在 Agent 或 Signal 服务端中继续传递 `-secret`，必须使用明确语义的 `-signal-secret`（集群注册）与 `-access-secret`（访客口令）。

---

### ⑥ 浏览器控制台报错：WebSocket 连接失败 (403 Forbidden) 或 CSWSH 跨域阻断
- **故障根因**：网关自 **v2.3** 起全面强化跨站 WebSocket 劫持防御（CSWSH），信令服务端默认启用严格同源检查（Strict Same-Origin Only）。当浏览器 Web 门户的前端地址（`Origin` 标头，如 `https://portal.yourdomain.com`、`https://*.pages.dev` 或本地开发端口 `http://localhost:5173`）与信令服务端的对外域名或端口不一致时，跨域 WebSocket 握手将被服务端直接拦截并返回 `HTTP 403 Forbidden`；
- **排查与解决**：
  1. **配置允许的 Origin 白名单（生产环境推荐）**：
     在启动信令服务端时添加 `-allowed-origins` 参数，或配置环境变量 `SIGNAL_ALLOWED_ORIGINS`（支持逗号分隔、子域通配符 `*.pages.dev` 与端口通配符 `localhost:*`，v2.3.1 引入域名与端口解耦匹配）：
     ```bash
     ./bin/signal_linux_amd64 -addr :443 \
       -allowed-origins "https://*.pages.dev,https://portal.yourdomain.com,http://localhost:*"
     ```
  2. **临时放行所有跨域握手（仅限测试/开发环境）**：
     在本地调试或独立内网环境中，可传入 `--allow-all-origins` 参数（或环境变量 `SIGNAL_ALLOW_ALL_ORIGINS=true` / `ALLOW_ALL_ORIGINS=true`）关闭白名单校验；
  3. **命令行 Agent 检查**：Go CLI Agent 与 curl 等原生命令行客户端不发送浏览器 `Origin` 请求头，不受此策略阻断，直接基于 `SIGNAL_SECRET` 与 HMAC 签名鉴权。

---

## 2. 常见问题 (FAQ)

### Q: P2P 直连模式下的网络吞吐性能如何？
**A:** 当 WebRTC 打洞成功建立 P2P 直连后，数据流直接经由用户态 UDP (DTLS 1.2 / SRTP，RFC 8831) 点对点传输，不存在任何公网云中继服务器的带宽瓶颈。实际吞吐上限受限于两端物理链路带宽、网络往返时延（RTT）、SCTP 拥塞控制机制以及宿主机单核加解密计算能力。在千兆局域网与优质宽带环境下，可充分利用物理上行带宽。官方基准测试（1000 并发流套件）实测吞吐调度速率达 2,307.9 streams/s，内存保持线性受控。

### Q: v2.3 原生双通道架构（`p2pcontrol` + `p2pdata`）是如何消除传输大文件时的控制帧队头阻塞（HoL Blocking）的？
**A:** 在单通道架构下，当用户下载数十 GB 大文件或并发海量数据切片时，底层 SCTP 缓冲区被大量 `DATA` 帧塞满并触发背压，导致关键的 `HEADERS`（新请求发起）、`PING`/`PONG`（心跳保活）以及 `RST_STREAM`（取消流）控制帧必须排队等待，引发严重的队头阻塞。
从 **v2.3** 起，系统在握手阶段原生初始化两条解耦的 WebRTC DataChannel：
- **`p2pcontrol`**：独占承载 `HEADERS`、`PING`、`PONG`、`RST_STREAM`、`WINDOW_UPDATE`，物理绕开数据背压队列，即便数据通道饱和也能在 10ms 内极速到达；
- **`p2pdata`**：独立传输 `DATA` 与 `WS_DATA`，严格遵循 128MB 全局内存配额与 96MB/64MB 双水位背压机制；
- **孤儿帧对齐（Orphan Frame Reconciliation）**：应对并行 SCTP 流可能的乱序递送（`DATA` 帧先于 `HEADERS` 到达），多路复用器内置单流 64KB、全局 256KB、TTL 500ms 的孤儿暂存池，待 `HEADERS` 确认后按序重放或在超时后由 Janitor 回收，实现零数据丢失与零阻塞并发。

### Q: 在 Web 门户直接下载数 GB 或几十 GB 大文件时，浏览器会不会出现内存溢出（OOM / 标签页崩溃）？
**A:** 完全不会。早期 Web 应用常使用虚拟 `<a download="file" href="blob:...">` 触发下载，这会导致浏览器将数十 GB 的全量二进制文件全部缓存进 JavaScript V8 堆内存中，触发 Chromium Bug 40410035 致命崩溃。
网关内置的 **P2P File Vault** 引入了自适应双模式落盘机制：
1. **流式直接落盘（Stream-to-Disk）**：在 Chromium 86+ 等现代浏览器中，针对大于 500MB 的文件自动唤起原生 `showSaveFilePicker` API，建立直接指向用户本地磁盘的文件写入流（WritableStream），数据帧经由 Service Worker 解包后边收边刷盘，前端内存占用稳定在常量级（O(1) 内存占用）；
2. **流式 Fetch 拦截**：在不支持文件系统 API 的旧版浏览器或普通文件下载场景中，通过 `fetch(viewUrl)` 搭配 Service Worker 流式转发，提供实时速度与进度计算，并配备 `AbortController` 取消防护，杜绝内存堆积。

### Q: v2.3+ 强制 DTLS 证书指纹绑定（F-04）后，时效安全分享链接（`-sign-link`）在安全性上有何本质提升？
**A:** 传统穿透工具的分享链接通常只包含静态 Token 或弱签名，一旦链接在即时通讯软件、公共日志或中间网络中泄露，任何未经授权的第三方设备均可在有效期内重复使用该链接发起连接。
自 **v2.3** 起，网关建立了基于现代密码学的**代际安全边界**：
- 访客浏览器在发起 WebRTC 协商时，底层会生成专属的瞬时 DTLS 证书，并提取 SHA-256 指纹（`sdpFingerprint`）；
- 握手鉴权不仅校验时间戳是否过期，还将访客的 `sdpFingerprint` 深度绑定至 HMAC-SHA256 签名计算中；
- 即便攻击者中途截获完整的分享 URL，只要其使用的物理设备与握手证书不同，指纹校验便会立即失败，彻底防御跨设备中间人重放攻击。

### Q: 局域网同网段与跨公网环境下，流量是如何选路的？会不会无意中消耗公网服务器流量？
**A:** WebRTC 严格遵循 IETF RFC 8445 ICE（交互式连通性建立）规范进行候选收集与连通性检查（STUN Binding Requests）：
1. **候选收集**：两端同时生成 `host`（本机真实物理网卡内网 IP）、`srflx`（STUN 反射公网出口 IP）以及 `relay`（TURN 中继服务器 IP）；
2. **同网段直连（零公网消耗）**：当访客与 Agent 处于同一 Wi-Fi 或局域网交换机下时，两端通过 ICE 连通性测试会以最高优先级选定 `host <-> host` 候选对。此时数据流纯粹在局域网网卡之间点对点传输，延迟通常 < 1ms，完全不占用公网带宽，公网信令服务端仅在握手阶段交换数 KB 的 SDP 文本；
3. **跨公网穿透**：当两端跨越公网时，只要至少一方不是对称型 NAT，ICE 会优先协商建立 `srflx <-> srflx` 点对点直连；仅在双端均为严格对称型 NAT 时，才自动降级使用 TURN 中继。

### Q: 为什么通过网关使用网页终端（如 Web SSH、Vim、Code-Server、NoVNC）时，Smart Pill 悬浮药丸不会产生按键冲突或误触？
**A:** Smart Pill 2.1 针对高密度交互场景执行了专门的人机交互防干扰工程：
1. **零键盘拦截（Zero Keyboard Interception）**：悬浮药丸组件在 DOM 层坚决不注册任何全局 `keydown`、`keyup` 或 `keypress` 事件监听器，也不抢占焦点。终端中输入 `Ctrl+C`、`Esc`、Vim 快捷键或组合操作时，按键事件 100% 原始透传到底层终端；
2. **防误触欧氏距离判断**：点击与拖拽采用 PointerCapture 状态机判定，移动位移超过 5 像素才判定为拖拽意图，小于等于 5 像素判定为点击唤起控制台，杜绝手抖误触发；
3. **折叠与边缘磁吸**：支持一键收起为 28px 呼吸微点，并具备屏幕边缘自适应吸附能力；在拖拽过程中自动对底层 iframe 注入 `pointer-events: none`，防止鼠标穿透吞噬事件。

### Q: 穿透后我的 NAS / 本地 Web 服务需要修改代码吗？真实访客 IP 如何获取？
**A:** 完全不需要修改任何后端业务代码。
- **透明代理**：网关在浏览器端基于 Service Worker 拦截所有标准 HTTP 请求与 WebSocket，在 Agent 端解包后按标准 HTTP/1.1 与 RFC 6455 格式投递给本地服务；
- **权威真实 IP 透传**：为了满足企业级 WAF、雷池 SafeLine 与访问日志审计需求，信令服务端在握手时权威提取访客物理出口 IP，Agent 在反代转发时权威覆写 `X-Forwarded-For` 与 `X-Real-IP` 请求头。后端服务（如 Nginx、Node.js、Go、Python）可像部署在真实公网一样直接按单 IP 执行安全审计与频控限制。

### Q: 是否支持直接分享大文件而无需额外搭建网盘？
**A:** 支持。通过 Agent 内置的单文件分发模式（`-file`），无需搭建第三方 Web 容器即可将单个大文件（如 20GB ISO 或压缩包）提供给对端：
```bash
./agent -file ./large-game.zip -access-secret pass123 -sign-link 24h
```
生成的带时效签名分享链接支持浏览器直接触发 P2P 流式下载（支持 HTTP 206 断点续传），零云中继流量消耗，过期自动失效。

### Q: 为什么某些页面在 iframe 嵌套中无法打开？多服务多节点如何选择隔离模式？
**A:** 许多后台管理系统内建了 `Anti-Frame-Busting` 脚本（例如判断 `window.top !== window.self` 时强行跳出或拒绝渲染）。
- 网关的 `patch.js` 内置了沙箱虚拟化代理，针对常见的防嵌套脚本进行透明拦截与上下文伪装；
- **推荐部署隔离形态**：
  - **子域名模式（生产推荐）**：配置泛域名解析（如 `*.p2p.yourdomain.com`），不同服务映射为不同子域（如 `jellyfin.p2p.yourdomain.com` 与 `vaultwarden.p2p.yourdomain.com`）。此时浏览器为每个服务分配独立的物理 Origin，拥有隔离的 LocalStorage、IndexedDB 与 Cookie 空间，多服务之间彻底杜绝凭证污染；
  - **单域名 Hash 模式**：通过 `https://p2p.yourdomain.com/#nodeId` 访问，无需泛域名证书与复杂 DNS 配置，由网关在前端基于 Service Worker 实现虚拟沙箱隔离。

### Q: 为什么自建反向代理某些包含第三方 CDN 或外部多媒体的站点时，部分图床或视频切片加载失败？
**A:** 在反向代理现代化富媒体网站时，网页常引用非同源的第三方 CDN 静态资源（如各类公共图床、第三方音视频切片）：
- 为了防止被恶意利用作为公网无限制开放代理（Open Proxy Abuse），网关对 `/--p2p-proxy/` 外域转发通道设置了默认安全边界，只代理同源或经过授权的请求；
- **解决方案**：在 Agent 启动命令或配置文件中，显式声明 `-allowed-proxy-domains` 通配符白名单（例如允许 Bilibili 常用媒体 CDN）：
  ```bash
  ./agent -node-id mybili -target http://127.0.0.1:8080 \
          -allowed-proxy-domains "*.hdslb.com,*.bilivideo.com,*.akamaized.net"
  ```
  合法外域媒体请求即可顺畅直连加载，同时保障代理网关不沦为开放代理跳板。
