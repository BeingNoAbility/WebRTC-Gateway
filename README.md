# Zero-Client P2P WebRTC Intranet Penetration Gateway
### 浏览器零客户端依赖的 P2P WebRTC 端到端内网穿透网关

<p align="center">
  <a href="README.md"><b>简体中文</b></a> · <a href="README_en.md"><b>English</b></a> · <a href="https://github.com/BeingNoAbility/WebRTC-Gateway/wiki"><b>📖 GitHub Wiki</b></a>
</p>

[![Go Version](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![TypeScript](https://img.shields.io/badge/TypeScript-5.0%2B-3178C6?logo=typescript)](https://www.typescriptlang.org/)
[![WebRTC](https://img.shields.io/badge/WebRTC-Pion%20v4-FF6B6B?logo=webrtc)](https://github.com/pion/webrtc)
[![Docker](https://img.shields.io/badge/Docker-Multi--Arch-2496ED?logo=docker)](https://github.com/BeingNoAbility/WebRTC-Gateway/pkgs/container/webrtc-p2p-server)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![CI](https://github.com/BeingNoAbility/WebRTC-Gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/BeingNoAbility/WebRTC-Gateway/actions/workflows/ci.yml)

---

## 💡 什么是 Zero-Client P2P WebRTC Gateway？

这是一个颠覆传统内网穿透思路的开源解决方案：**访客浏览器端“零客户端依赖、零浏览器插件、零特权根证书、零网络配置”**。

在传统穿透方案中（如 FRP、NPS、Ngrok、Cloudflare Tunnel 等），所有内网流量均需经由公网服务器中转，不仅受限于中继 VPS 昂贵且贫瘠的下行带宽，还存在单点流量成本与隐私顾虑；而 Tailscale、WireGuard、ZeroTier 则强制访客终端必须预装客户端软件或系统级 VPN 驱动。

本项目基于 **WebRTC RTCDataChannel (SCTP) + 纯浏览器端 Service Worker 透明拦截与 DOM 虚拟化沙箱**：
1. **访客端**：只需在任意主流现代浏览器（Chrome、Edge、Safari、Firefox、iOS/Android 移动端）中打开一个门户链接或子域名；
2. **穿透层**：浏览器自动通过轻量信令撮合与家庭内网/私有服务器中的 **Agent 守护进程** 完成 WebRTC NAT 打洞（STUN/ICE），建立**点对点端到端加密直连通道（DTLS-SRTP）**；
3. **应用体验**：所有 HTTP 请求、API 调用、Cookie 凭证、音视频媒体流、WebSocket 实时通信完全在浏览器与内网服务器之间**直连高速传输**，不消耗任何中继服务器流量，目标内网 Web 服务**无需做任何代码修改**！

---

## 📑 目录

- [🌟 核心特性](#-核心特性)
- [📖 使用者指南 (User Guide)](#-使用者指南-user-guide)
  - [1. 30 秒快速上手](#1-30-秒快速上手)
  - [2. 经典内网应用实践 (NAS / Jellyfin / Vaultwarden 等)](#2-经典内网应用实践)
  - [3. 多节点与子域名智能路由 (Subdomain & Routing)](#3-多节点与子域名智能路由)
  - [4. Web 控制台与节点设置器](#4-web-控制台与节点设置器)
  - [5. Agent 命令行参数与环境变量全量说明](#5-agent-命令行参数与环境变量全量说明)
  - [6. 一体化服务端 (Signal Server) 命令行参数全量说明](#6-一体化服务端-signal-server-命令行参数全量说明)
  - [7. 安全性与访问控制](#7-安全性与访问控制)
- [🛠️ 生产部署指南 (Self-Hosting & Operations)](#️-生产部署指南-self-hosting--operations)
  - [方案 A：Cloudflare Workers 一体化边缘部署 (零服务器成本)](#方案-acloudflare-workers-一体化边缘部署-零服务器成本)
  - [方案 B：自建 Linux / Windows 一体化自包含服务器 (单一二进制)](#方案-b自建-linux--windows-一体化自包含服务器-单一二进制)
  - [方案 C：Docker & Docker Compose 一键编排容器部署](#方案-cdocker--docker-compose-一键编排容器部署)
- [💻 开发者指南 (Developer Guide)](#-开发者指南-developer-guide)
  - [1. 系统架构与端到端数据流向](#1-系统架构与端到端数据流向)
  - [2. P2PMux v1 二进制流式多路复用协议规范](#2-p2pmux-v1-二进制流式多路复用协议规范)
  - [3. 浏览器端 DOM 虚拟化与 Service Worker 核心机制](#3-浏览器端-dom-虚拟化与-service-worker-核心机制)
  - [4. 项目代码仓库结构全景](#4-项目代码仓库结构全景)
  - [5. 本地源码构建与自动化测试套件](#5-本地源码构建与自动化测试套件)
- [📄 开源协议 (License)](#-开源协议-license)

---

## 🌟 核心特性

- 🚀 **真正的零客户端 (Zero-Client)**：访客只要有一台带有现代浏览器的电脑或手机，点开 URL 即可直达内网应用。
- ⚡ **P2P 点对点极速直连**：基于标准 WebRTC ICE/STUN 打洞，家庭千兆宽带能跑多快就跑多快（实测内网/局域网 RTT 1~3ms，跨运营商直连跑满宽带上行），完全脱离云中继带宽限制。
- 🔄 **WebRTC 标准 ICE Restart（移动漫游自愈）**：在手机或笔记本网络突变（Wi-Fi 切换 5G 蜂窝网络、AP 漫游、弱网恢复）时，自动触发信令保活与 in-place ICE Restart 双阶段重协商，无需刷新浏览器页面，正在播放的视频与底层长连接不中断。
- 🔗 **HMAC-SHA256 无状态时效安全分享**：纯数学自包含签名，零中心化数据库依赖。支持在 Web 控制台或 CLI 中一键生成限时免密分享链接（`#nodeId?exp=xxx&sign=yyy`），到期自动失效，免向访客透露主口令。
- 📊 **前端 WebRTC HUD 实时诊断看板**：悬浮药丸实时显示 `⚡ P2P Direct` 直连或 `🛡️ TURN Relay` 中继状态，一键展开高科技诊断 HUD，透视往返延迟 (RTT)、丢包率、瞬时上下行流速与双方候选 IP，支持一键现场触发 ICE Restart 测试。
- 🛡️ **生产容量治理与防 DoS 限流**：Agent 端支持 `-max-streams` 并发流信号量容量防守（默认 512），超出时平滑回复 503 保护 NAS 内存；信令服务端支持单 IP 并发连接限制 (`-max-ip-conns`)、请求速率频控与防暴力破解锁定。
- 📈 **Prometheus 监控开箱支持**：服务端开箱提供 `/metrics`；Agent 严格遵循纯出站安全原则，支持显式 opt-in `-metrics-addr` 导出活跃连接、流数、吞吐字节与拒绝量。
- 🔒 **端到端原生加密**：传输层采用 WebRTC 强制的 DTLS-SRTP 军工级非对称协商与强加密，信令服务仅撮合网络拓扑，中继节点不可见、无法解密任何业务数据。
- 🌐 **多节点与子域名智能感知**：支持多台机器/多个服务并发在线，通过 `<nodeId>.p2p.domain.com`、`#nodeId` 或前端可视化设置弹窗秒级切换。
- 🧰 **全类型 Web 协议透明透传**：
  - **HTTP/HTTPS**：支持全方法、流式响应（Streaming）、文件下载。
  - **HTTP 206 Partial Content**：完整支持音视频流媒体 Range 分片请求，进度条拖动即时响应。
  - **WebSocket 双向全双工**：自带跨 SCTP 帧的流式分片重组引擎，超大消息零丢失。
  - **Cookie 状态持久化**：Agent 端维护独立的会话 CookieJar，隔离不同客户端并发登录态；前端同步维护非 HttpOnly Cookie。
  - **W3C 上传进度虚拟化**：突破浏览器 Service Worker 无法感知 XHR 上传进度的缺陷，网盘、相册、测速上传进度条平滑流式展现。
  - **防盗链破壁与沙箱安全**：自动净化 Referrer-Policy，解决跨域图床 403 问题；深度防御 Anti-Frame-Busting。
  - **新标签页 P2P 隧道保持**：页面内新开窗口（`<a target="_blank">` 或 `window.open`）自动继承 P2P 隧道。

---

## 📖 使用者指南 (User Guide)

### 1. 30 秒快速上手

使用本网关只需要两个步骤：**在内网启动 Agent，在浏览器打开访问页面**。

#### 第一步：在内网目标主机启动 Agent

选择已编译好的二进制（位于 `bin/` 目录下）或直接使用 Go 运行：

**Windows 环境：**
```powershell
.\bin\agent.exe -node-id myhome -signal wss://signal.yourdomain.com/ws -target http://127.0.0.1:8080
# 本地测试（使用内置一体化信令服务端）：
# .\bin\agent.exe -node-id myhome -signal ws://127.0.0.1:8888/ws -target http://127.0.0.1:8080
```

**Linux / NAS / 树莓派环境：**
```bash
chmod +x ./bin/agent_linux_amd64
./bin/agent_linux_amd64 -node-id myhome -signal wss://signal.yourdomain.com/ws -target http://127.0.0.1:8080
# 本地测试：
# ./bin/agent_linux_amd64 -node-id myhome -signal ws://127.0.0.1:8888/ws -target http://127.0.0.1:8080
```

*（注：`-target` 指向您想要暴露给外部访问的本地 Web 服务地址，例如内网 NAS 面板、Alist、Jellyfin 等）*

#### 第二步：在任意远程浏览器中打开门户

在手机或远程电脑的浏览器中输入：
👉 **`https://p2p.yourdomain.com/#myhome`**（本地开发测试则直接在浏览器打开 **`http://localhost:8888/#myhome`**）

浏览器将自动连接信令，与您的 Agent 完成 NAT 打洞并在数秒内呈现内网服务，右下角悬浮药丸实时显示直连往返延迟（RTT）。

---

### 2. 经典内网应用实践

| 应用场景 | 典型本地服务地址 | Agent 启动命令示例 | 浏览器端体验 |
| :--- | :--- | :--- | :--- |
| **家庭媒体中心 (Jellyfin / Emby / Plex)** | `http://192.168.1.100:8096` | `./agent -node-id media -target http://192.168.1.100:8096` | 支持音视频在线解码播放，进度条随意拖动快进，走 P2P 原生流式分片。 |
| **密码管理器 (Vaultwarden / Bitwarden)** | `http://127.0.0.1:8080` | `./agent -node-id bitwarden -target http://127.0.0.1:8080` | 完整支持 WebCrypto API 与 Passkey，Session Cookie 隔离，安全加密访问。 |
| **私人网盘 (Alist / OpenList / FileBrowser)** | `http://127.0.0.1:5244` | `./agent -node-id pan -target http://127.0.0.1:5244` | 支持多文件并发上传下载、预览文档，W3C 上传进度条平滑展示。 |
| **云相册 (Immich / Photoprism)** | `http://127.0.0.1:2283` | `./agent -node-id photo -target http://127.0.0.1:2283` | 相册极速缩略图瀑布流渲染，SvelteKit 前端无缝兼容。 |
| **容器控制台 (Portainer)** | `https://127.0.0.1:9443` | `./agent -node-id docker -target https://127.0.0.1:9443` | 自动破除 Anti-Frame-Busting 限制，WebSocket 容器终端与日志实时交互。 |
| **本地开发与 Web 调试** | `http://localhost:3000` | `./agent -node-id dev -target http://localhost:3000` | 免配置临时暴露 Vite / Next.js / Spring Boot 开发服务给客户或同事预览。 |
| **P2P 单文件秒传 (私有 Wormhole / AirDrop)** | 本地单个大文件 (ISO/ZIP/MP4) | `./agent -file ./Ubuntu.iso -access-secret pass123 -sign-link 24h` | 访客浏览器打开签名链接即刻触发满速 P2P 直连下载，不限大小，零中心流量，时效自毁。 |
| **静态网站与本地目录在线托管** | 本地静态目录路径 | `./agent -dir /path/to/dist` | 零额外 Web 服务器，自动索引 `index.html` 或渲染现代化极简只读文件浏览列表。 |

---

### 3. 多节点与子域名智能路由

系统内置了多层级、自适应的节点标识解析引擎，您可以非常灵活地为不同的应用分配访问入口：

```
配置解析优先级：
URL 查询参数 (?peer=nas) 
  ➔ URL 短 Hash (#nas) 
    ➔ 子域名前缀 (nas.p2p.domain.com 或 nas.domain.com) 
      ➔ localStorage 本地记忆 
        ➔ 内置默认节点
```

1. **子域名无感直达（推荐）**：
   - 绑定泛解析域名后，访客可直接访问 `https://nas.p2p.yourdomain.com/`，系统自动从域名提取 `nas` 节点，无需带任何多余参数。
   - 二级子域名同样支持：`https://jellyfin.yourdomain.com/` ➔ 自动识别 `jellyfin` 节点。
2. **URL 短 Hash 格式**：
   - `https://p2p.yourdomain.com/#vaultwarden` ➔ 自动连入 `vaultwarden` 节点。
3. **URL 参数格式**：
   - `https://p2p.yourdomain.com/?peer=photo` ➔ 自动连入 `photo` 节点。

---

### 4. Web 控制台与节点设置器

页面右下角常驻**状态胶囊（Status Badge）**：
- **实时网络指示**：显示连接状态（Connected / Connecting）以及端到端真实 RTT（如 `⚡ RTT: 2.1ms`）。
- **设置控制弹窗**：点击右上角或状态胶囊的 `⚙` 图标即可呼出设置弹窗：
  - **手动修改节点 ID**：即时切换连接至其他在线 Agent；
  - **自定义信令地址**：支持快速指定自定义 WebSocket 信令服务器；
  - **快捷芯片**：一键切换预设节点与信令服务；
  - **热重连与持久化**：点击保存后，前端毫秒级销毁旧连接并完成新打洞，设置自动记忆在浏览器 `localStorage` 中。

---

### 5. Agent 命令行参数与环境变量全量说明

Agent 既支持标准命令行 Flag，也完全支持环境变量，方便在 Linux systemd、Docker、Kubernetes 中运行：

| 命令行参数 (CLI) | 环境变量 (ENV) | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-node-id` | `NODE_ID` | `homeserver` | 该 Agent 在信令网络中的唯一节点标识（字符串） |
| `-signal` | `SIGNAL_URL` | `ws://127.0.0.1:8888/ws` | 信令服务器的 WebSocket 地址（支持 ws:// 或 wss://） |
| `-target` | `TARGET_URL` | `http://127.0.0.1:8080` (缺省) | 本地或局域网反代的目标 Web 服务地址（模式 A，与 `-dir`、`-file` 互斥） |
| `-dir` | `DIR_PATH` | `""` | 托管为静态网站或目录浏览的本地文件夹路径（模式 B，与 `-target`、`-file` 互斥） |
| `-file` | `FILE_PATH` | `""` | 直接作为单文件分发服务的本地文件路径（模式 C，与 `-target`、`-dir` 互斥，P2P 极速闪传） |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | 可选的 YAML / JSON 配置文件路径（默认自动探测 `./agent.yaml`、`./agent.yml`、`./agent.json`） |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | 信令集群准入密钥（控制面，必须与信令端配置一致，防节点冒领，绝不透露给访客） |
| `-access-secret` | `ACCESS_SECRET` | `""` | 访客访问口令（数据面，留空表示公开免密直连；配置后访客需输入口令或持有 HMAC 签名链接） |
| `-proxy` | `PROXY_URL` | `""` | 可选的前置代理（支持 HTTP/HTTPS/SOCKS5，用于连接受限信令） |
| `-sni` | `SNI` | `""` | 可选的 TLS SNI 与 Host 标头覆盖（用于绕过 DNS 污染直连 Cloudflare Anycast IP） |
| `-insecure` | `INSECURE` | `false` | 连接自签名 TLS 信令服务器时跳过证书链校验 |
| `-turn-url` | `TURN_URL` | `""` | 可选的 TURN 中继服务器地址（如 `turn:relay.domain.com:3478`，用于对称型 NAT 降级中继） |
| `-turn-user` | `TURN_USER` | `""` | 可选的 TURN 中继认证用户名 |
| `-turn-pass` | `TURN_PASS` | `""` | 可选的 TURN 中继认证密码 |
| `-stun-servers` | `STUN_SERVERS` | `""` | 可选的自定义 STUN 服务器列表（支持逗号分隔字符串或配置文件列表） |
| `-max-streams` | `MAX_STREAMS` | `512` | 最大并发逻辑流上限，超出时平滑回复 503 保护宿主机内存 |
| `-metrics-addr` | `METRICS_ADDR` | `""` | 可选的 Prometheus 监控导出地址（如 `:9090`，默认不开放任何端口，保持纯出站安全） |
| `-sign-link` | - | `0` | 基于 `-access-secret` 生成指定有效期的无状态安全分享链接（如 `1h`、`24h`、`7d`）并退出 |
| `-portal-url` | - | `https://your-portal-domain.com` | 为 `-sign-link` 指定 Web 门户的基准访问地址 |

> [!NOTE]
> **关于 `-secret` / `SECRET` 的升级说明 (v1.5.0+)**：
> 旧版模糊命名的 `-secret` 与 `SECRET` 现已彻底解耦并废弃。若在 Agent 启动时传入，程序将直接退出并提示改用 `-signal-secret`（集群注册）或 `-access-secret`（访客口令）。

### 6. 一体化服务端 (Signal Server) 命令行参数全量说明

| 命令行参数 (CLI) | 环境变量 (ENV) | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-addr` | `ADDR` / `PORT` | `:8888` | 服务端 HTTP/HTTPS 与 WebSocket 监听端口及地址 |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | 可选的 YAML / JSON 配置文件路径（默认自动探测 `./server.yaml`、`./server.yml`、`./server.json`） |
| `-signal-secret` | `SIGNAL_SECRET` / `SECRET` | `""` | 可选的服务端全局集群准入密钥，仅允许匹配口令的 Agent 注册（兼容别名 `-secret`） |
| `-tls-cert` | `TLS_CERT` | `""` | 加载的 TLS 证书 PEM 文件路径（如 fullchain.pem，开启原生 HTTPS） |
| `-tls-key` | `TLS_KEY` | `""` | 加载的 TLS 私钥 PEM 文件路径（如 privkey.pem，开启原生 HTTPS） |
| `-tls-auto` | `TLS_AUTO` | `false` | 一键内存自动签发包含当前局域网所有真实 IP 的 ECDSA SAN 证书 |
| `-static` | `STATIC_DIR` | `./web/dist` | 静态网页资产目录（默认优先使用二进制内置嵌入的 DistFS） |
| `-max-ip-conns` | `MAX_IP_CONNS` | `64` | 单个 IP 允许的最大并发 WebSocket 握手连接数（防 DoS 耗尽套接字） |
| `-trusted-proxies` | `TRUSTED_PROXIES` | `""` (默认私网) | 受信代理 CIDR 列表（默认信任回环及 RFC1918 私网；填 `none` 禁用代理头信任） |
| `-metrics-addr` | `METRICS_ADDR` | `""` | 可选的专用 Prometheus 监控导出地址（如 `:9090`，默认直接复用主服务 `/metrics`） |

---

### 7. 安全性与访问控制

- **端到端完全加密**：所有流经 WebRTC DataChannel 的流量均经过 DTLS 1.2 / 1.3 协商与 AES-GCM 强加密。即便公网信令服务端被恶意监听，攻击者也绝对无法解密传输中的任何数据。
- **控制面与数据面双重密钥解耦体系 (Dual-Secret Decoupling, v1.5.0+)**：
  1. **信令集群准入密钥 (Control Plane: `-signal-secret` / `SIGNAL_SECRET`)**：
     - 信令端与各 Agent 共享，仅用于 Agent 注册时的集群准入与节点防冒领 (`Node Hijack Protection`)；
     - 任何其他非法 Agent 尝试抢注已注册的 `node_id` 或接入集群时，若未提供匹配的密钥将被信令服务器直接拒绝；
     - **绝不暴露给访客浏览器**，访客亦无需知晓此密钥。
  2. **访客访问口令门禁 (Data Plane: `-access-secret` / `ACCESS_SECRET`)**：
     - **公开免密模式（默认）**：若留空，该节点即为公开站点，访客浏览器直接打洞进入，不弹出密码框，即使信令端启用了集群密钥也不影响访客；
     - **独立口令模式**：若配置，访客在浏览器打开时 Web 门户将自动感知并弹出**高保真暗黑密码输入弹窗**（或通过 URL 参数 `#nodeId?secret=xxx` 传递）；Agent 收到 WebRTC 握手 Offer 时进行毫秒级口令比对，不匹配直接阻断握手，绝不泄露内网服务。
- **无状态时效分享链接 (Stateless HMAC-SHA256 Sharing)**：
  - 无需数据库、无需 Redis、无需服务器记录撤销表；
  - 纯数学签名验证：`sign = HMAC-SHA256(nodeId + ":" + exp, access_secret)`；
  - 访客在浏览器打开 `#nodeId?exp=xxx&sign=yyy` 时，Agent 本地以毫秒级验证时效与签名，到期后数学上天然失效；
  - Web 控制台支持一键选择 1 小时、6 小时、24 小时、7 天、30 天生成专属分享链接并一键复制；CLI 亦支持 `./agent -access-secret xxx -sign-link 24h` 极速生成。
- **纯内存零监听 P2P 文件与目录服务器 (v1.6.0+)**：
  - **纯出站零端口哲学**：直接在内存中将 WebRTC DataChannel 流适配为标准 HTTP 响应，不在宿主机开启任何本地 TCP 监听端口，避免端口暴露与冲突；
  - **单文件秒传 (私有版 Wormhole / 跨公网 AirDrop)**：`./agent -file /path/to/archive.zip -access-secret pass -sign-link 24h`，访客在浏览器打开即可直接触发原生浏览器高速 P2P 下载，不限文件大小、不走中心服务器中转流量；
  - **静态网站与目录托管**：`./agent -dir /path/to/dist`，自动索引 `index.html` 或呈现只读文件树列表；
  - **原生 HTTP 206 断点续传与媒体拖拽**：原生支持 `Range` 请求头，音视频/图片/PDF 自动采用 `inline` 供浏览器即时播放与拖拽，压缩包/镜像自动采用 `attachment` 触发下载；
  - **严格沙箱安全防护**：基于 `filepath.Rel` 与 `filepath.EvalSymlinks` 严密防御 `../` 路径穿越与符号链接逃逸攻击。
- **连接韧性与移动漫游自愈 (WebRTC ICE Restart)**：
  - 当手机或笔记本在 Wi-Fi 和蜂窝移动网络之间漫游切换时，系统启动两阶段自愈流水线；
  - 自动快速重建断开的信令套接字，并在原 PeerConnection 上发起带有 `iceRestart: true` 的重协商；
  - Agent 提供 5 秒断网宽限期（Grace Window），免除销毁连接，应用层 DataChannel、长连接与内网页面会话 100% 保持。
- **独立会话隔离**：每个浏览器会话拥有独立的虚拟连接通道与服务端 CookieJar，防止不同访客之间的登录态串号。

---

## 🛠️ 生产部署指南 (Self-Hosting & Operations)

本系统采用极度解耦的现代化工程架构，支持三种自建与生产部署方式：

---

### 方案 A：Cloudflare Workers 一体化边缘部署 (零服务器成本)

**最推荐的生产部署架构**。利用 Cloudflare Workers 的全球 300+ Anycast 边缘网络，搭配 Durable Objects 负责 WebSocket 信令撮合，同时托管静态门户前端。**无需购买任何公网云主机，0 元月租，全球毫秒级接入**。

#### 1. 前置准备
- 拥有一个 Cloudflare 账号；
- 本机安装 Node.js 与 npm。

#### 2. 一键发布部署
```bash
# 1. 编译前端资源
cd web
npm install
npm run build

# 2. 部署至 Cloudflare Workers
npx wrangler deploy
```

部署完成后，单个 Worker 即**同时承载前端与 WebSocket 信令**：
- **Web 门户前端**：`https://p2p.yourdomain.com/`
- **信令端点**：`wss://p2p.yourdomain.com/ws`

在 Cloudflare 控制台为该 Worker 绑定自定义域名（例如 `p2p.yourdomain.com` 及泛域名 `*.p2p.yourdomain.com`），即刻享受开箱即用的多级子域名智能路由。

---

### 方案 B：自建 Linux / Windows 一体化自包含服务器 (单一二进制)

如果您希望将信令与门户完全托管在自己的独立 VPS、云主机或私网机房中，项目提供了**自包含一体化二进制**。

Go 二进制通过 `go:embed` 技术将编译好的 Web 门户（HTML、JS、Service Worker）直接内嵌进单一可执行文件，**零外部文件依赖，不依赖 Node.js，不依赖 Nginx**：

#### 1. 启动一体化服务端（原生支持 HTTPS / TLS 直载，告别反代套娃）
服务端内置原生 TLS 引擎与自签名 SAN 证书生成器，**无需额外配置 Nginx、Caddy 或任何反向代理，即可直接提供原生 HTTPS 与 WSS 服务**：

**方式 1：加载已有 SSL 域名证书（生产环境首选）**
```bash
# Linux
./bin/signal_linux_amd64 -addr :443 -tls-cert /path/to/fullchain.pem -tls-key /path/to/privkey.pem

# Windows
.\bin\signal.exe -addr :443 -tls-cert C:\path\to\fullchain.pem -tls-key C:\path\to\privkey.pem
```

**方式 2：局域网 / 开发环境一键开启原生 HTTPS（无需证书文件，自动签发）**
```bash
# 启动时加上 -tls-auto 参数，服务端将在内存中自动生成覆盖 localhost 及局域网所有真实 IP 的 ECDSA SAN 证书：
./bin/signal_linux_amd64 -addr :8443 -tls-auto
```
*启动后直接在浏览器打开 `https://<局域网IP>:8443/#nodeId`，浏览器即可进入安全上下文（Secure Context），Service Worker 完整激活！*

**方式 3：纯 HTTP 运行（本地开发）**
```bash
./bin/signal_linux_amd64 -addr :8888
```

#### 2. 内网启动 Agent 连接自建信令
- 若服务端配置了权威 CA 证书（如 Let's Encrypt）：
  ```bash
  ./bin/agent_linux_amd64 -node-id mynode -signal wss://yourdomain.com/ws -target http://127.0.0.1:8080
  ```
- 若服务端使用 `-tls-auto` 自签名证书：
  ```bash
  # 加上 -insecure 参数跳过证书链校验：
  ./bin/agent_linux_amd64 -node-id mynode -signal wss://<服务器IP>:8443/ws -insecure -target http://127.0.0.1:8080
  ```

---

### 方案 C：Docker & Docker Compose 一键编排容器部署

项目内置精简的多阶段构建 Dockerfile，基础镜像基于 Alpine 3.20，全面支持 x86_64 及 ARM64 跨平台架构，打包后的生产镜像极为轻量：
- **服务端镜像 (`Dockerfile.server`)**：包含前端编译产物与信令服务，仅约 **16MB**
- **Agent 镜像 (`Dockerfile.agent`)**：纯 Go 静态编译守护进程，仅约 **20MB**

> [!TIP]
> 项目已配置 GitHub Container Registry (GHCR) 官方镜像流水线，支持直接拉取多架构（amd64 / arm64）镜像，免除本地构建步骤：
> ```bash
> # 拉取服务端镜像 (含前端与信令)
> docker pull ghcr.io/beingnoability/webrtc-p2p-server:latest
> # 或指定最新版本号：
> docker pull ghcr.io/beingnoability/webrtc-p2p-server:v1.3.0
>
> # 拉取内网穿透 Agent 客户端
> docker pull ghcr.io/beingnoability/webrtc-p2p-agent:latest
> # 或指定最新版本号：
> docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v1.3.0
> ```

根目录下提供了**高度模块化且内置完备注释的 [docker-compose.yml](docker-compose.yml)**，开箱即用。

#### 1. 快速启动 (默认配置)
```bash
# 启动所有服务（后台运行）
docker compose up -d

# 查看实时运行日志与握手状态
docker compose logs -f

# 停止并移除容器
docker compose down
```
启动后：
- 浏览器打开：`http://localhost:8888/#homeserver` 即可直连测试目标服务；
- 服务端健康检查：容器内置自动健康监测（Healthcheck），状态一目了然。

---

#### 2. 经典 Docker 编排场景示例

##### 场景一：家庭 NAS / 私服纯 Agent 运行（连公网信令）
若您的信令与前端已部署在公网（如 Cloudflare Workers 或独立 VPS），在家庭 NAS、树莓派或内网服务器上**只需运行 Agent 容器**：

```yaml
# docker-compose.agent-only.yml
services:
  agent:
    image: ghcr.io/beingnoability/webrtc-p2p-agent:latest
    build:
      context: .
      dockerfile: Dockerfile.agent
    container_name: webrtc-p2p-agent
    restart: unless-stopped
    # 强烈推荐 host 模式，以便直接穿透宿主机 127.0.0.1 上的各类服务
    network_mode: "host"
    environment:
      - NODE_ID=my-nas
      - SIGNAL_URL=wss://p2p.yourdomain.com/ws
      - TARGET_URL=http://127.0.0.1:8096  # 指向本地 Jellyfin / Alist / 私有云
      # - TURN_URL=turn:relay.domain.com:3478  # 可选：对称型 NAT 下启用 TURN 中继
      # - TURN_USER=turn_user
      # - TURN_PASS=turn_pass
    volumes:
      - /etc/localtime:/etc/localtime:ro
```

##### 场景二：局域网纯 IP 原生自签 HTTPS 部署 (解决 Service Worker 白屏)
在私网局域网中直接通过 IP（如 `192.168.1.100`）访问时，现代浏览器会强制拦截纯 HTTP 下的 Service Worker。启用 `TLS_AUTO=true` 即可一键生成覆盖所有局域网真实 IP 的 ECDSA SAN 证书，彻底根除白屏：

```yaml
services:
  server:
    image: ghcr.io/beingnoability/webrtc-p2p-server:latest
    build:
      context: .
      dockerfile: Dockerfile.server
    container_name: webrtc-p2p-server
    restart: unless-stopped
    ports:
      - "8443:8443"
    environment:
      - ADDR=:8443
      - TLS_AUTO=true # 开启纯内存动态 SAN 证书生成
    volumes:
      - /etc/localtime:/etc/localtime:ro

  agent:
    image: ghcr.io/beingnoability/webrtc-p2p-agent:latest
    build:
      context: .
      dockerfile: Dockerfile.agent
    container_name: webrtc-p2p-agent
    restart: unless-stopped
    network_mode: "host"
    environment:
      - NODE_ID=nas
      - SIGNAL_URL=wss://127.0.0.1:8443/ws
      - INSECURE=true # 信任服务端的自签 TLS 证书
      - TARGET_URL=http://127.0.0.1:5244
    depends_on:
      - server
```
*启动后，使用浏览器直接访问 `https://<局域网IP>:8443/#nas` 即可瞬间秒开！*

##### 场景三：挂载正式域名 SSL 证书运行
```yaml
services:
  server:
    image: ghcr.io/beingnoability/webrtc-p2p-server:latest
    ports:
      - "443:443"
    environment:
      - ADDR=:443
      - TLS_CERT=/etc/certs/fullchain.pem
      - TLS_KEY=/etc/certs/privkey.pem
    volumes:
      - ./certs:/etc/certs:ro
      - /etc/localtime:/etc/localtime:ro
```

##### 场景四：单机多服务/多节点并行穿透编排
在同一台机器上，可以通过 Docker Compose 编排多个不同 `NODE_ID` 的 Agent 分别穿透不同的服务，外部按需直连：
```yaml
services:
  server:
    # ... 一体化信令服务端 ...

  agent-media:
    image: ghcr.io/beingnoability/webrtc-p2p-agent:latest
    network_mode: "host"
    environment:
      - NODE_ID=media
      - SIGNAL_URL=ws://127.0.0.1:8888/ws
      - TARGET_URL=http://127.0.0.1:8096 # 影音 Jellyfin

  agent-pan:
    image: ghcr.io/beingnoability/webrtc-p2p-agent:latest
    network_mode: "host"
    environment:
      - NODE_ID=pan
      - SIGNAL_URL=ws://127.0.0.1:8888/ws
      - TARGET_URL=http://127.0.0.1:5244 # 网盘 Alist

  agent-pass:
    image: ghcr.io/beingnoability/webrtc-p2p-agent:latest
    network_mode: "host"
    environment:
      - NODE_ID=pass
      - SIGNAL_URL=ws://127.0.0.1:8888/ws
      - TARGET_URL=http://127.0.0.1:8080 # 密码库 Vaultwarden
```

---

#### 3. 极速生成 HMAC-SHA256 时效安全分享链接 (Docker 运行)

无需在宿主机安装 Go 环境，通过 Docker Compose 或独立 Agent 容器即可极速生成数学级安全时效分享链接：

```bash
# 方式 A：通过 Docker Compose 极速生成 (例如 24 小时有效)
docker compose run --rm agent -sign-link 24h -portal-url https://p2p.yourdomain.com

# 方式 B：通过独立 docker run 容器生成
docker run --rm -e ACCESS_SECRET=your_visitor_password \
  ghcr.io/beingnoability/webrtc-p2p-agent:latest \
  -node-id homeserver -sign-link 24h -portal-url https://p2p.yourdomain.com
```

---

#### 4. 环境变量全量速查表

##### 服务端环境变量 (`webrtc-p2p-server`)
| 环境变量 | 默认值 | 对应命令行参数 | 说明 |
| :--- | :--- | :--- | :--- |
| `ADDR` / `PORT` | `:8888` | `-addr` | HTTP/WS 或 HTTPS/WSS 监听地址（例如 `:8888`、`:443`） |
| `CONFIG_FILE` / `CONFIG` | 空 | `-config` | YAML / JSON 配置文件路径 |
| `SIGNAL_SECRET` / `SECRET` | 空 | `-signal-secret` | 可选的服务端全局准入预共享密钥，仅允许匹配口令的 Agent 注册 |
| `TLS_AUTO` | `false` | `-tls-auto` | 是否在内存中自动签发覆盖本机所有局域网真实 IP 的 ECDSA SAN 证书 |
| `TLS_CERT` | 空 | `-tls-cert` | 容器内的 SSL/TLS 证书文件 PEM 路径（需挂载外部证书） |
| `TLS_KEY` | 空 | `-tls-key` | 容器内的 SSL/TLS 私钥文件 PEM 路径 |
| `STATIC_DIR` | 空 (内置) | `-static` | 自定义前端静态资源文件目录路径（若挂载外部 `dist`） |
| `MAX_IP_CONNS` | `64` | `-max-ip-conns` | 单 IP 最大并发连接数限制（防 DoS 耗尽套接字） |
| `TRUSTED_PROXIES` | 空 (默认私网) | `-trusted-proxies` | 受信反向代理 CIDR 列表（填 `none` 可禁用代理头信任） |
| `METRICS_ADDR` | 空 | `-metrics-addr` | 可选的专用 Prometheus 监控导出地址（默认直接通过主端口 `/metrics` 导出） |

##### Agent 客户端环境变量 (`webrtc-p2p-agent`)
| 环境变量 | 默认值 | 对应命令行参数 | 说明 |
| :--- | :--- | :--- | :--- |
| `NODE_ID` | `homeserver` | `-node-id` | 当前节点的唯一标识符（外部通过 `#nodeId` 访问） |
| `SIGNAL_URL` | `ws://127.0.0.1:8888/ws` | `-signal` | 信令服务器 WebSocket 端点（支持 `ws://` 与 `wss://`） |
| `TARGET_URL` | `http://127.0.0.1:8080` (缺省) | `-target` | 需要反代的目标 Web 服务地址（模式 A，与 `DIR_PATH`/`FILE_PATH` 互斥） |
| `DIR_PATH` | 空 | `-dir` | 托管为静态网站或目录浏览的本地文件夹路径（模式 B） |
| `FILE_PATH` | 空 | `-file` | 直接作为单文件分发服务的本地文件路径（模式 C，极速 P2P 闪传） |
| `CONFIG_FILE` / `CONFIG` | 空 | `-config` | YAML / JSON 配置文件路径 |
| `SIGNAL_SECRET` | 空 | `-signal-secret` | 信令集群准入密钥（控制面，必须与信令端匹配，防节点冒领） |
| `ACCESS_SECRET` | 空 | `-access-secret` | 访客访问口令（数据面，留空免密直连，配置后访客需输入密码） |
| `INSECURE` | `false` | `-insecure` | 是否跳过自签 TLS 信令服务器的证书安全验证 |
| `PROXY_URL` | 空 | `-proxy` | 可选的出站代理（支持 `socks5://`、`http://`） |
| `SNI` | 空 | `-sni` | 可选的 TLS SNI 与 Host 标头覆盖（用于绕过 DNS 污染直连 Anycast IP） |
| `TURN_URL` | 空 | `-turn-url` | 可选的 TURN 中继地址（突破对称型 NAT 限制） |
| `TURN_USER` | 空 | `-turn-user` | 可选的 TURN 认证用户名 |
| `TURN_PASS` | 空 | `-turn-pass` | 可选的 TURN 认证密码 |
| `STUN_SERVERS` | 空 | `-stun-servers` | 可选的自定义 STUN 服务器列表（逗号分隔） |
| `MAX_STREAMS` | `512` | `-max-streams` | 最大并发逻辑流上限（超出时平滑返回 503 保护 NAS 内存） |
| `METRICS_ADDR` | 空 | `-metrics-addr` | 可选的 Prometheus 监控导出地址（默认关闭，保持纯出站安全） |

---

#### 5. 单独构建与运行 Docker 容器命令速查

**构建本地镜像（可选）：**
```bash
# 构建一体化服务端镜像
docker build -t ghcr.io/beingnoability/webrtc-p2p-server:latest -f Dockerfile.server .

# 构建穿透 Agent 镜像
docker build -t ghcr.io/beingnoability/webrtc-p2p-agent:latest -f Dockerfile.agent .
```

**运行服务端容器：**
```bash
# 1. 默认 HTTP 运行 (直接拉取官方镜像)
docker run -d --name webrtc-server --restart unless-stopped \
  -p 8888:8888 \
  ghcr.io/beingnoability/webrtc-p2p-server:latest

# 2. 原生自签 HTTPS 运行 (自动生成覆盖所有内网 IP 的 SAN 证书)
docker run -d --name webrtc-server-tls --restart unless-stopped \
  -p 8443:8443 \
  -e ADDR=:8443 \
  -e TLS_AUTO=true \
  ghcr.io/beingnoability/webrtc-p2p-server:latest
```

**运行 Agent 容器：**
```bash
# 宿主机 host 模式（推荐 Linux）
docker run -d --name webrtc-agent --restart unless-stopped \
  --net=host \
  -e NODE_ID=my-nas \
  -e SIGNAL_URL=wss://p2p.yourdomain.com/ws \
  -e TARGET_URL=http://127.0.0.1:8096 \
  ghcr.io/beingnoability/webrtc-p2p-agent:latest
```

---

## 💻 开发者指南 (Developer Guide)

本节专为希望深入理解技术原理、二次开发、定制功能或参与开源贡献的工程师提供。

---

### 1. 系统架构与端到端数据流向

整个系统分为三大核心组件：
1. **Browser Client (前端运行时)**：包含 App-Shell 门户、Service Worker 流量拦截器、DOM 虚拟化补丁 (`patch.js`) 与 TypeScript 版 P2PMux 多路复用器。
2. **Signaling Server (信令撮合中心)**：仅在握手阶段传递 SDP Offer/Answer 及 ICE Candidates，一旦 P2P 建立即退出数据路径。
3. **Agent Daemon (内网守护进程)**：Go 编写的高性能守护进程，管理 WebRTC PeerConnection，负责 P2PMux 二进制流解包，并向本地服务发起 HTTP/WS 代理。

#### 系统时序交互图：

```
[访客浏览器]                       [公网信令服务]                   [内网 Agent]           [本地 Web 服务]
     |                                  |                              |                  |
     | 1. 连接信令 (WebSocket)          |                              |                  |
     |--------------------------------->|                              |                  |
     |                                  | <--- 2. 节点注册 (Register) -|                  |
     |                                  |                              |                  |
     | 3. 发送 SDP Offer (指定 Node ID) |                              |                  |
     |--------------------------------->|--- 4. 路由转发 Offer ------->|                  |
     |                                  |<-- 5. 回复 SDP Answer -------|                  |
     |<-- 6. 路由转发 Answer -----------|                              |                  |
     |                                  |                              |                  |
     |<============== 7. WebRTC ICE NAT 打洞 (STUN 直连) =============>|                  |
     |                                                                 |                  |
     |===== 8. 建立点对点端到端加密 DataChannel (SCTP: "p2pmux") =====>|                  |
     |                                                                 |                  |
     | [Service Worker 拦截 fetch]                                     |                  |
     | 9. 封装为 P2PMux Stream Frame (HTTP 请求头与 Body)              |                  |
     |---------------------------------------------------------------->|                  |
     |                                                                 | 10. 反向代理请求 |
     |                                                                 |----------------->|
     |                                                                 |<-----------------|
     |                                                                 | 11. 获取响应     |
     | 12. P2PMux Stream Frame (HTTP 状态码、头、分块数据流)           |                  |
     |<----------------------------------------------------------------|                  |
     | 13. ReadableStream 流式注入浏览器页面渲染                       |                  |
```

---

### 2. P2PMux v1 二进制流式多路复用协议规范

为了在单个 WebRTC `RTCDataChannel` 上并发承载成百上千个独立的 HTTP 请求、大文件下载与 WebSocket 连接，且避免单个大文件的慢传输阻塞关键 API（消除队头阻塞 Head-of-Line Blocking），网关设计了轻量级二进制多路复用协议 **P2PMux v1**。

#### 协议帧结构 (10 字节固定头部)：

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|                          Stream ID (32 bits)                  |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|   Type (8)    |   Flags (8)   |       Payload Length (32)     |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|       Payload Length (cont.)  |   Payload Data (0 ~ 32KB) ... |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+                               |
```

- **Stream ID (uint32, 4 字节)**：流的唯一标识符。浏览器发起奇数流 ID（1, 3, 5...），Agent 发起偶数流 ID（2, 4, 6...），杜绝冲突。
- **Type (uint8, 1 字节)**：
  - `0x01 (FrameHeaders)`：流的首包元数据（包含 HTTP 路径、方法、Query、Headers 等 JSON 序列化数据）。
  - `0x02 (FrameData)`：流的实际载荷数据块（分片传输）。
  - `0x03 (FrameRst)`：流重置/异常终止。
  - `0x04 (FramePing)` / `0x05 (FramePong)`：用于端到端延迟测定与保活心跳。
- **Flags (uint8, 1 字节)**：
  - `0x01 (FlagFIN)`：标记该流的数据发送完毕（半关闭/完全关闭信号）。
- **Payload Length (uint32, 4 字节)**：载荷字节长度（大端序）。单帧最大载荷严格限制在 **32KB (32768 字节)**，以完美适配 WebRTC 底层 SCTP MTU，杜绝由于超大分包导致的底层缓冲区溢出与丢包。

---

### 3. 浏览器端 DOM 虚拟化与 Service Worker 核心机制

本项目最具技术突破性的成果之一，是在不需要用户修改任何目标应用源码的前提下，仅凭浏览器原生开放能力实现了完整的 Web 协议栈虚拟化：

#### ① Service Worker 与 Dedicated MessagePort 通道
- Portal 页面在注册 Service Worker 时，通过专用 `MessagePort` 建立双向直通通信管道（`PORTAL_ATTACH`）。
- Service Worker 拦截页面及其内嵌 iframe 的同源 `fetch` 事件，将请求通过 MessagePort 发送至页面中的 P2PMux 调度器，转换为 WebRTC 二进制帧并发送给 Agent。
- Agent 返回的响应以流式方式（`ReadableStream`）逐块返还给 Service Worker，使得超大文件与音视频流能够一边传输一边由浏览器解码呈现。

#### ② W3C Fetch 规范严格对齐 (204/304 Null-Body)
- Chromium 与 WebKit 内核在根据规范处理响应时，严禁 HTTP 204 No Content、205 Reset Content、304 Not Modified 携带 Body，否则会触发 `TypeError: Response with null-body status cannot have body`。
- 网关在 Service Worker 中进行了规范级状态校验，遇到上述状态码强制使用 `new Response(null, { status })`，保证了各类 SPA 应用（如 SvelteKit、Next.js）缓存协商的 100% 稳定性。

#### ③ W3C XMLHttpRequest Upload 上传进度虚拟化
- 浏览器原生 Service Worker 存在已知底层缺陷（Chromium Bug 1214207）：由 Service Worker 代理的 XHR 上传请求无法自动触发 `xhr.upload.onprogress` 事件，导致网盘、测速等应用的上行进度条卡死在 0%。
- 网关在 `patch.js` 中拦截了 `XMLHttpRequest.prototype.send`，根据物理传输速率自适应虚拟化调度 W3C 标准 `ProgressEvent` 事件流，使得 LibreSpeed 测速、网盘大文件上传进度条能够平滑、真实地前进。

#### ④ WebSocket 跨 SCTP 帧流式重组 (Defragmentation)
- 原生 WebSocket 消息可能长达数百 KB，远超 WebRTC SCTP 每次安全发送的 16KB~32KB 限制。
- 网关在前端与 Agent 端分别设计了流式拼包缓冲区：在发送端将 WebSocket 报文打上 5 字节定长头（1 字节 Opcode + 4 字节 PayloadLen），在接收端自动维护环形缓冲，并在数据完整收齐后才触发前端 WebSocket 的 `onmessage`，确保超大 WebSocket 报文零乱序、零损坏。

#### ⑤ 独立的会话 CookieJar 与 document.cookie 双向同步
- Agent 内部为每个远程 Session 实例化独立的 `http.CookieJar`。目标应用返回的敏感 `Set-Cookie`（包括 `HttpOnly`）由 Agent 自主维护并在后续请求中自动带上。
- Agent 将非 HttpOnly 的 Cookie 列表通过响应元数据下发给前端，由 `patch.js` 自动同步至浏览器本地的 `document.cookie`，兼顾了安全性与前端 JavaScript 读取 Cookie 的兼容性。

#### ⑥ 激进防框架劫持 (Anti-Frame-Busting)
- 许多企业级管理系统（如 Portainer、Grafana、各类路由器后台）内置了类似于 `if (window.top !== window.self) window.top.location = window.self.location;` 的框架劫持检测脚本。
- 网关在渲染 iframe 时移除了 `allow-top-navigation` 权限，并在页面注入虚拟化代理，拦截对 `window.top`、`window.parent`、`window.frameElement` 的检测，使其安全无感地在 Portal 内嵌沙箱中运行。

#### ⑦ 第三方 CDN 防盗链破壁与新标签页/弹窗 Node ID 深度持久化
- **防盗链破壁**：在响应头和 HTML 中动态注入 `<meta name="referrer" content="no-referrer">`，并在 Agent 请求上游 CDN 时伪装 Origin 与 Referer，化解各类视频、图片图床返回 403 Forbidden 的问题。
- **新标签页/弹窗隧道保持**：
  - 针对 Bilibili、Google、Naver 等点击新标签页打开子域链接（`<a target="_blank">` 或 `window.open`）的场景，Service Worker 维护全局 `activeNodeId` 并向 HTML 响应实时注入 `window.__P2P_NODE_ID__`；
  - `patch.js` 自动将新页面重写为虚拟化路径 `?peer=${nodeId}#${nodeId}`，彻底解决新窗口脱离上下文误将子域名作为 Node ID 导致 `Target agent offline` 的问题。

---

### 4. 项目代码仓库结构全景

```
webrtc-gateway/
├── bin/                            # 预编译跨平台可执行二进制
│   ├── agent.exe                   # Windows x64 Agent 守护进程
│   ├── agent_linux_amd64           # Linux x64 Agent 守护进程
│   ├── signal.exe                  # Windows x64 一体化自包含服务端
│   ├── signal_linux_amd64          # Linux x64 一体化自包含服务端
│   └── mock_service.exe            # 辅助测试 Mock 服务
├── cmd/                            # Go 应用程序入口
│   ├── agent/                      # Agent CLI 入口 (main.go)
│   ├── signal/                     # 一体化服务端 CLI 入口 (内嵌 web/dist)
│   └── mock_service/               # 具备流式下载/Cookie认证/WS的测试服务
├── pkg/                            # 核心业务与协议库 (跨平台可复用)
│   ├── agent/                      # WebRTC 客户端打洞与连接生命周期管理
│   ├── mux/                        # P2PMux 多路复用流调度器与背压控制
│   ├── protocol/                   # 二进制帧编解码规范与序列化
│   ├── proxy/                      # HTTP/WS 反向代理调度与 CookieJar 隔离
│   └── signaling/                  # 信令协商消息结构定义
├── web/                            # 前端单页系统 (纯原生 TypeScript，零庞大框架)
│   ├── src/
│   │   ├── portal/                 # 引导页控制台、设置弹窗、WebRTC 连接管理
│   │   ├── sw/                     # Service Worker 流量拦截与流式管道
│   │   ├── patch/                  # DOM/XHR/WebSocket/Cookie 虚拟化猴子补丁
│   │   ├── protocol/               # TypeScript 版 P2PMux 二进制编解码实现
│   │   └── index.html              # 现代化极简前端模板
│   ├── dist/                       # 前端编译生成目录 (自动嵌入 Go 服务端)
│   ├── worker.js                   # Cloudflare 一体化 Worker (Durable Object + 静态托管)
│   ├── esbuild.config.mjs          # 极速打包脚本 (毫秒级输出)
│   ├── package.json
│   └── tsconfig.json
├── edge/                           # 边缘信令配置
│   ├── cloudflare-worker/          # 独立信令 Worker 脚本
│   └── wrangler.toml               # Wrangler 部署配置
├── test/                           # 自动化测试套件
│   ├── advanced_e2e_scenarios_test.go # HTTP 206 Partial Content / Range / Gzip / 标头反射透传
│   ├── secret_auth_test.go         # SECRET 鉴权校验、防冒领与未授权拦截 (401 实时断开)
│   ├── tls_server_test.go          # 原生自签 ECDSA SAN 证书与安全上下文自动配置
│   ├── large_file_e2e_test.go      # 100MB 大文件流式传输与背压测试
│   ├── mvp_e2e_test.go             # WebRTC 撮合与并发流测试
│   ├── phase3_tunnel_test.go       # HTTP/WS 全功能穿透回归测试
│   ├── test_subdomain_logic.cjs    # 多级子域名解析算法单测
│   └── node_id_preservation_test.cjs # 弹窗新窗口 Node ID 保持与 DOM 虚拟化集成测试
├── docker-compose.yml              # 一键编排全套服务配置
├── Dockerfile.agent                # Agent 极简多阶段构建文件 (~20MB)
├── Dockerfile.server               # 一体化服务端多阶段构建文件 (~16MB)
├── go.mod
├── go.sum
└── README.md
```

---

### 5. 本地源码构建与自动化测试套件

#### ① 编译前端资源
前端采用纯原生 TypeScript 编写，打包工具选用极速的 `esbuild`，构建仅需几毫秒：
```bash
cd web
npm install
npm run build
cd ..
```

#### ② 编译 Go 二进制
内置的前端静态文件位于 `web/dist`，通过 `//go:embed` 直接编译进 `signal`：
```bash
# 编译当前平台二进制
go build -ldflags="-s -w" -o bin/agent.exe ./cmd/agent
go build -ldflags="-s -w" -o bin/signal.exe ./cmd/signal

# 交叉编译 Linux x64 二进制
# PowerShell:
$env:GOOS="linux"; $env:GOARCH="amd64"
go build -ldflags="-s -w" -o bin/agent_linux_amd64 ./cmd/agent
go build -ldflags="-s -w" -o bin/signal_linux_amd64 ./cmd/signal
Remove-Item Env:GOOS, Env:GOARCH

# Linux / macOS Bash:
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/agent_linux_amd64 ./cmd/agent
GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o bin/signal_linux_amd64 ./cmd/signal
```

#### ③ 运行全量单元与集成测试套件
项目自带严密的纯自动化测试集，无需外部浏览器依赖即可在任意操作系统上极速运行：
```bash
# 1. 运行底层协议、多路复用与端到端穿透全量 Go 测试
go test -v ./...

# 2. 运行前端路由、子域名解析与新窗口 Node ID 保持测试套件
cd web && npm test
# 或直接在项目根目录下通过 Node 运行：
node test/test_subdomain_logic.cjs
node test/node_id_preservation_test.cjs

# 3. 运行基于真实 Chromium 内核 (Edge / Chrome CDP) 的全量浏览器测试矩阵
node test/real_browser_matrix_runner.cjs

# 4. 运行 HTTP / WebSocket 核心业务特性全项核验套件
node test/verify_features.cjs
```

**测试套件涵盖的核心验证点：**
- `RealBrowserMatrixTests`：基于真实无头 Chromium (CDP 协议) 自动化运行 22/22 项全量矩阵（12 项复杂 Web 应用场景、页面刷新与会话韧性保持、Top 10 真实高流量网站秒级渲染）；
- `HTTP & WebSocket Feature Verification`：覆盖 GET/POST/PUT/PATCH/DELETE、Query String、Request Body、Cookie 隔离与双向穿透、Authorization 标头、301/302/307 重定向、HTTP 206 Range 断点续传、5MB 流式传输、204/304 Null-Body 规范、WebSocket 20 突发与 128KB 巨型帧分片重组；
- `TestFrameEncodeDecode`：二进制 10 字节帧头与大端序序列化边界验证；
- `TestMuxPingPongAndConcurrentStreams`：并发流隔离读写与流控背压；
- `TestPhase2E2EWebRTCP2PMux`：端到端 WebRTC 信令撮合与 P2P SCTP 通道打洞；
- `TestPhase3FullHTTPAndWSTunnel`：
  - 标准 HTTP GET / POST 代理验证；
  - 动态 CookieJar 认证持久化校验；
  - 1MB / 100MB 分块流式数据传输无损检验；
  - WebSocket 101 Switching Protocols 双向全双工通道中继；
- `TestPhase4Large100MBStreamingAndBackpressure`：大文件流式传输与 SHA-256 逐字节完整性校验；
- `TestAdvancedE2EScenarios`：HTTP 206 Partial Content / Range 分块点播、Gzip 解压缩与自定义标头双向镜像反射；
- `TestSecretAuthScenarios`：SECRET 预共享密钥鉴权校验、非法访问毫秒级 401 实时断开阻断与信令防冒领保护；
- `TestTLSServerAuto`：纯内存动态 ECDSA 证书自动签发与局域网物理网卡 SAN 适配；
- `NodeIdPreservationTests`：DOM 模拟环境下 30/30 项节点识别优先级、弹窗新标签页上下文继承与沙箱虚拟化验证。

---

## 📄 开源协议 (License)

本项目基于 [MIT License](LICENSE) 开源协议发布，欢迎自由商用、个人使用、学术研究与二次开发。如果本项目对您有所帮助，欢迎点亮右上角的 ⭐️ **Star** 给予支持！
