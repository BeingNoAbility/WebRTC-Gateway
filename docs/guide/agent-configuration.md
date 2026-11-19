# Agent 与服务端配置全景 (Agent & Server Configuration)

WebRTC Gateway 遵循现代云原生十二要素应用原则（Twelve-Factor App），支持 **命令行参数 (CLI Flags)**、**环境变量 (ENV)** 与 **YAML / JSON 配置文件** 三种配置方式。

### 参数生效优先级 (Configuration Precedence)
```text
命令行参数 (CLI Flags)
  ➔ 环境变量 (Environment Variables)
    ➔ 配置文件 (YAML / JSON)
      ➔ 默认内置缺省值
```

---

## 1. Agent 运行模式 (Modes of Operation)

Agent 支持三种核心运行模式，三者在业务上严格三选一（互斥校验）：

| 模式 | 参数 | 说明 | 典型场景 |
| :--- | :--- | :--- | :--- |
| **模式 A：反向代理模式** | `-target` / `TARGET_URL` | 将请求反向代理到本地或局域网现有的 HTTP/WS 服务。 | 穿透 Jellyfin、NAS 面板、Vaultwarden 等。 |
| **模式 B：目录与静态站模式** | `-dir` / `DIR_PATH` | 直接将本地文件夹托管为只读网站，自动索引 `index.html` 或展现文件树。 | 托管文档站、前端打包产物 dist、公共下载目录。 |
| **模式 C：单文件轻量分发** | `-file` / `FILE_PATH` | 将本地单个文件作为分发目标，支持 Range 206 断点续传，不限大小。 | 私有版 Wormhole / 跨公网文件分发，分享 ISO、视频、日志。 |

---

## 2. Agent 命令行参数与环境变量全量说明

| 命令行参数 (CLI) | 环境变量 (ENV) | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-node-id` | `NODE_ID` | `homeserver` | 该 Agent 在信令网络中的唯一节点标识（字符串） |
| `-signal` | `SIGNAL_URL` | `ws://127.0.0.1:8888/ws` | 信令服务器的 WebSocket 地址（支持 `ws://` 或 `wss://`） |
| `-target` | `TARGET_URL` | `http://127.0.0.1:8080` (缺省) | 反向代理的目标服务地址（与 `-dir`、`-file` 互斥） |
| `-dir` | `DIR_PATH` | `""` | 托管为静态网站或目录浏览的本地文件夹路径（与 `-target`、`-file` 互斥） |
| `-file` | `FILE_PATH` | `""` | 单文件分发服务的本地文件路径（与 `-target`、`-dir` 互斥） |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | 可选的 YAML / JSON 配置文件路径（默认自动探测 `./agent.yaml`、`./agent.yml`、`./agent.json`） |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | 信令集群准入密钥（控制面，必须与信令端一致，防冒领，绝不透露给访客） |
| `-access-secret` | `ACCESS_SECRET` | `""` | 访客访问口令（数据面，留空免密直连；配置后访客需输入口令或持有签名链接） |
| `-proxy` | `PROXY_URL` | `""` | 可选的出站代理（支持 `http://`、`https://`、`socks5://`，仅用于信令外出，不污染本地反代） |
| `-allowed-proxy-domains` | `ALLOWED_PROXY_DOMAINS` | `""` | 可选的 `/--p2p-proxy/` 外域转发通配符白名单（如 `*.hdslb.com,*.bilivideo.com`，防开放代理逃逸） |
| `-sni` | `SNI` | `""` | 可选的 TLS SNI 与 Host 标头覆盖（用于绕过 DNS 污染直连 Anycast IP） |
| `-insecure` | `INSECURE` | `false` | 连接自签名 TLS 信令服务器时跳过证书链校验 |
| `-turn-url` | `TURN_URL` | `""` | 可选的 TURN 中继地址（如 `turn:relay.domain.com:3478`，用于对称型 NAT 降级中继） |
| `-turn-user` | `TURN_USER` | `""` | 可选的 TURN 认证用户名 |
| `-turn-pass` | `TURN_PASS` | `""` | 可选的 TURN 认证密码 |
| `-stun-servers` | `STUN_SERVERS` | `""` | 可选的自定义 STUN 服务器列表（逗号分隔或配置文件列表） |
| `-max-streams` | `MAX_STREAMS` | `512` | 最大并发逻辑流上限，超出时平滑回复 503 保护宿主机内存 |
| `-metrics-addr` | `METRICS_ADDR` | `""` | 可选的 Prometheus 监控导出地址（如 `:9090`，默认关闭保持纯出站安全） |
| `-sign-link` | - | `0` | 基于 `-access-secret` 生成指定有效期的时效安全分享链接（如 `1h`、`24h`）并退出 |
| `-portal-url` | - | `https://your-portal-domain.com` | 为 `-sign-link` 指定 Web 门户的基准访问地址 |
| `-v`, `-version` | - | `false` | 显示 Agent 版本信息并退出 |
| `-h`, `-help` | - | `false` | 显示完整命令行帮助文档与运行模式示例并退出 |

---

## 3. 一体化服务端 (Signal Server) 命令行参数全量说明

| 命令行参数 (CLI) | 环境变量 (ENV) | 默认值 | 说明 |
| :--- | :--- | :--- | :--- |
| `-addr` | `ADDR` / `PORT` | `:8888` | 服务端 HTTP/HTTPS 与 WebSocket 监听端口及地址 |
| `-config` | `CONFIG_FILE` / `CONFIG` | `""` | 可选的 YAML / JSON 配置文件路径（默认自动探测 `./server.yaml`、`./server.yml`、`./server.json`） |
| `-allowed-origins` | `SIGNAL_ALLOWED_ORIGINS` / `ALLOWED_ORIGINS` | `""` (严格模式：同源允许/跨域阻断) | 允许通过 WebSocket 握手的浏览器 Origin 白名单（支持逗号分隔、子域通配 `*.pages.dev` 及端口通配 `localhost:*`） |
| `-allow-all-origins` | `SIGNAL_ALLOW_ALL_ORIGINS` / `ALLOW_ALL_ORIGINS` | `false` | 是否放行所有跨域 WebSocket 信令连接（测试/开发模式，生产环境慎用） |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | 可选的服务端全局集群准入密钥，仅允许匹配口令的 Agent 注册（-secret 已废弃并阻断） |
| `-tls-cert` | `TLS_CERT` | `""` | 加载的 TLS 证书 PEM 文件路径（如 fullchain.pem，开启原生 HTTPS） |
| `-tls-key` | `TLS_KEY` | `""` | 加载的 TLS 私钥 PEM 文件路径（如 privkey.pem，开启原生 HTTPS） |
| `-tls-auto` | `TLS_AUTO` | `false` | 一键内存自动签发包含当前局域网所有真实 IP 的 ECDSA SAN 证书 |
| `-static` | `STATIC_DIR` | `./web/dist` | 静态网页资产目录（默认优先使用二进制内置嵌入的 DistFS） |
| `-max-ip-conns` | `MAX_IP_CONNS` | `64` | 单个 IP 允许的最大并发 WebSocket 握手连接数（防 DoS 耗尽套接字） |
| `-trusted-proxies` | `TRUSTED_PROXIES` | `""` (默认私网) | 受信代理 CIDR 列表（默认信任回环及 RFC1918 私网；填 `none` 禁用代理头信任） |
| `-metrics-addr` | `METRICS_ADDR` | `""` | 可选的专用 Prometheus 监控导出地址（如 `:9090`，默认直接复用主服务 `/metrics`） |
| `-health` | - | `false` | 执行本地健康检查探测（自动适配 HTTP/HTTPS/端口），成功退出码 0，失败退出码 1（专用于 Docker/K8s HEALTHCHECK） |
| `-health-url` | - | `""` | 自定义健康检查探测 URL（默认探测 `-addr` / `ADDR` 的 `/healthz` 端点） |
| `-v`, `-version` | - | `false` | 显示 Signal 服务端版本信息并退出 |
| `-h`, `-help` | - | `false` | 显示完整命令行帮助文档与参数示例并退出 |

---

## 4. 配置文件模版示例

### ① Agent 配置文件 (`agent.yaml`)
```yaml
node_id: "nas-home"
signal: "wss://p2p.yourdomain.com/ws"

# 运行模式三选一：
target: "http://127.0.0.1:8096"
# dir: "/data/www"
# file: "/data/isos/Ubuntu-24.04.iso"

# 双重安全密钥解耦：
signal_secret: "cluster_secret_abc123" # 控制面：信令集群注册密钥
access_secret: "visitor_pass_456"      # 数据面：访客访问口令

# 进阶网络优化：
stun_servers:
  - "stun:stun.cloudflare.com:3478"
  - "stun:stun.l.google.com:19302"

turn_url: "turn:turn.yourdomain.com:3478"
turn_user: "turnuser"
turn_pass: "turnpassword"

max_streams: 1024
metrics_addr: ":9090"
```

### ② 服务端配置文件 (`server.yaml`)
```yaml
addr: ":443"
allowed_origins: "https://*.pages.dev,http://localhost:*"
signal_secret: "cluster_secret_abc123"

# TLS 配置
tls_cert: "/etc/ssl/certs/fullchain.pem"
tls_key: "/etc/ssl/private/privkey.pem"
# tls_auto: false

# 生产容量与安全策略
max_ip_conns: 128
trusted_proxies: "127.0.0.1/32,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
```
