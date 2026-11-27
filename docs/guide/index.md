# 文档概览 (Guide Overview)

欢迎阅读 **WebRTC Gateway** 技术文档。本项目通过标准浏览器内的 WebRTC DataChannel 与跨站 Service Worker 沙箱，为内网 HTTP / WebSocket 服务与只读静态资源提供免装客户端的点对点访问能力。

---

## 核心特性

- **零客户端接入**：访客使用标准 Chrome、Edge、Safari 或 Firefox 打开链接即可直连，无需原生客户端或浏览器扩展。
- **零数据中继**：信令服务器仅转发 SDP 与 ICE 候选；对称 NAT 无法直连时可选用户自建 TURN（仅转发端到端 DTLS 密文）。
- **抗中间人身份与通道认证**：Agent 持久化 Ed25519 密钥对规范化 SDP 转录签名，浏览器通过 `pk=` 钉扎或 `IndexedDB` TOFU 验证；访问口令与可吊销能力令牌仅在 DTLS 控制信道内校验。
- **跨站双域沙箱透明代理**：透明支持 HTTP/HTTPS、206 Range 媒体拖拽、WebSocket 双向通信与 Agent 内存 `ProxyCookieJar`。

---

## 专题指南导航

| 专题模块 | 核心内容概述 | 推荐阅读对象 |
| :--- | :--- | :--- |
| **[Agent 典型用例](./agent-use-cases)** | NAS/自托管应用、自签 HTTPS 后台、子路径与本机反代虚拟主机（`-upstream-host`）、静态共享、能力外链、TURN/代理与运维模板。 | 内网应用使用者 / 运维 |
| **[本地反代协作模式](./reverse-proxy-recipes)** | 直连虚拟主机、前后端多源收口、Nginx `sub_filter` / Caddy 前缀剥离、协议头与上游鉴权头注入及拆分红线。 | 进阶内网部署 / 运维 |
| **[服务端部署用例](./server-use-cases)** | 本机回环、单二进制热重载 TLS、Nginx/Caddy 反代、Tier 1 无泛域双固定域、Tier 2 单根域同站与 Docker 加固部署。 | 自建运维 / 边缘部署用户 |
| **[系统架构与核心设计](./architecture)** | 单 PC 32-DC 池化数据面、三档跨站沙箱与隐藏桥接、流式 HTML5 分词器、统一静默重连与模块分层。 | 架构师 / 核心开发者 |
| **[P2PMux V7 协议规范](./protocol-specification)** | 8 字节定长帧头、V7 二进制 TLV（`FetchCtx`/`RespFlags`/行内 Cookie）、控制/32 HTTP 池/动态 WS 通道与跨语言黄金向量。 | 协议实现与审查人员 |
| **[安全机制与密码学边界](./security)** | 结构化 SDP 合成、Ed25519 节点身份、TOFU 红屏阻断、DTLS SPAKE2+ 认证、W3C Fetch 抗 CSRF 门禁与双池 Cookie。 | 安全审计 / 运维管理员 |
| **[Agent 与服务端配置](./agent-configuration)** | 全部 CLI 参数、环境变量、`*_FILE` 密钥文件与严格校验 YAML 配置参考。 | 全体使用者 |
| **[生产部署指南](./deployment)** | 三档沙箱 DNS/TLS 规划（含无通配符证书 Tier 1 方案）、单二进制构建与加固 Docker Compose。 | 运维 / 自建用户 |
| **[只读文件与目录适配器](./file-server)** | `-dir` / `-file` 基于 Go `os.Root` 的只读静态适配、点文件软链防护与 `CSP: sandbox`。 | 静态共享使用者 |
| **[浏览器兼容性矩阵](./browser-compatibility)** | Chrome / Edge CI 实测支持与 Safari / WebKit 休眠恢复与限制说明。 | 前端与移动端用户 |
| **[故障排查与 FAQ](./troubleshooting)** | 对称 NAT 与 TURN、安全上下文与双域配置、漫游重连、TOFU 密钥变更与常见启动报错处理。 | 故障排查人员 |

---

## 快速上手 (Quickstart)

::: code-group

```bash [1. 构建与本地信令]
# 构建前端资产并编译内嵌 Web 资源的 Go 二进制
make build

# 启动本地信令服务端（回环地址自动启用 *.localhost 跨站沙箱）
./bin/signal -addr 127.0.0.1:8888 -signal-secret dev-cluster-secret
```

```bash [2. 启动内网 Agent]
# 反向代理内网 HTTP 服务（默认要求 ACCESS_SECRET；公开访问需显式加 -allow-public）
ACCESS_SECRET=change-me ./bin/agent \
  -id myhome \
  -signal ws://127.0.0.1:8888/ws \
  -signal-secret dev-cluster-secret \
  -target http://127.0.0.1:8080

# 或离线签发 24 小时只读能力分享链接
./bin/agent -id myhome -mint-link 24h -mint-readonly -portal-url http://localhost:8888
```

```text [3. 浏览器访问]
打开：
http://localhost:8888/#myhome

按提示在 DTLS 通道内输入访问口令，即可在跨站沙箱中访问本地目标服务。
```

:::
