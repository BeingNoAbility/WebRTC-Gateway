# Agent 与信令服务端配置参考

WebRTC Gateway 采用统一配置加载器（`pkg/config`）：
- **优先级**：命令行参数 > 环境变量 / `*_FILE` 密钥文件 > 显式配置文件；
- **绝不自动加载当前目录配置文件**：仅当显式传入 `-config <path>` 或设置 `CONFIG_FILE` 时读取 YAML/JSON，并启用 `KnownFields(true)` 拒绝拼写错误或未知字段；
- **密钥文件优先**：支持 `SIGNAL_SECRET_FILE`、`ACCESS_SECRET_FILE`、`TURN_PASS_FILE`（适合 Docker Secrets / Kubernetes Secrets / systemd `LoadCredential`）；命令行传入密钥会打印进程列表可见性警告。

完整示例见 [`configs/agent.example.yaml`](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/configs/agent.example.yaml) 与 [`configs/server.example.yaml`](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/configs/server.example.yaml)，按场景组合的完整命令与部署模板见 [Agent 典型用例](./agent-use-cases) 与 [服务端部署用例](./server-use-cases)。

---

## 1. Agent 参数与环境变量 (`cmd/agent`)

| CLI 参数 | 环境变量 / 密钥文件 | YAML 字段 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- | :--- |
| `-config` | `CONFIG_FILE` | — | 空 | 显式 YAML/JSON 配置文件路径 |
| `-id` | `NODE_ID` | `node_id` | `homeserver` | 节点 ID（小写字母、数字、点、连字符，禁止 `sbx-` 前缀） |
| `-signal` | `SIGNAL_URL` | `signal_url` | `ws://127.0.0.1:8888/ws` | 信令服务基地址（携带密钥连非回环地址时默认要求 `wss://`） |
| `-signal-secret` | `SIGNAL_SECRET` / `SIGNAL_SECRET_FILE` | `signal_secret` | 空 | 信令集群或节点专属准入密钥（HTTP Upgrade Bearer） |
| `-allow-insecure-signal` | `ALLOW_INSECURE_SIGNAL` | `allow_insecure_signal` | `false` | 显式允许经非回环明文 `ws://` 发送 `SIGNAL_SECRET`（仅限隔离测试） |
| `-force-rekey` | `FORCE_REKEY` | `force_rekey` | `false` | 强制刷新信令端绑定的 Ed25519 公钥（需持有准入密钥且信令端允许） |
| `-access-secret` | `ACCESS_SECRET` / `ACCESS_SECRET_FILE` | `access_secret` | 空 | 访客 DTLS 通道内 RFC 9383 SPAKE2+ 口令（未设且无 `-allow-public` 时拒绝启动；与 `-allow-public` 互斥） |
| `-allow-public` | `ALLOW_PUBLIC` | `allow_public` | `false` | 显式允许无口令公开访问（仍强制 Ed25519 与 DTLS 证书验签；禁止与 `-access-secret` 同时配置） |
| `-identity-key` | `IDENTITY_KEY` | `identity_key` | 用户配置目录 | 持久化 Ed25519 私钥 PEM 路径（自动以 `0600` 原子创建） |
| `-state-file` | `STATE_FILE` | `state_file` | `<identity-key>.state.json`（临时身份为空） | 持久化能力令牌与吊销状态 JSON 路径（跨进程锁 + `0600` 原子并集合并，重启不丢失） |
| `-ephemeral-identity` | `EPHEMERAL_IDENTITY` | `ephemeral_identity` | `false` | 仅在内存生成临时身份密钥（重启后钉扎链接失效；不可与 `-identity-key` 或 `-mint-link` 同用） |
| `-target` | `TARGET_URL` | `target_url` | 空 | 本地 HTTP/HTTPS 反代目标（与 `-dir`/`-file` 三选一） |
| `-dir` | `DIR_PATH` | `dir_path` | 空 | 只读静态目录（`os.Root` 适配器） |
| `-file` | `FILE_PATH` | `file_path` | 空 | 只读单文件分享（`os.Root` 适配器） |
| `-hide-dotfiles` | `P2P_HIDE_DOTFILES` | `hide_dotfiles` | `true` | 隐藏点文件与指向点文件的根内软链 |
| `-file-inline-active` | `FILE_INLINE_ACTIVE` | `file_inline_active` | `false` | 允许静态文件内联执行脚本（关闭默认全文件 `CSP: sandbox`） |
| `-upstream-host` | `UPSTREAM_HOST` | `upstream_host` | 空 | 覆写上游虚拟主机 `Host` 头（RFC 9110 `host[:port]`，仅 `-target` 有效，同步作用于 `Origin`/`Location`/CookieJar） |
| `-proxy` | `PROXY_URL` | `proxy_url` | 空 | 出站 HTTP/HTTPS/SOCKS5 代理（无效 URL 启动即拒） |
| `-sni` | `SNI` | `sni` | 空 | HTTPS 目标服务的 TLS SNI 覆盖 |
| `-insecure-target` | `INSECURE_TARGET` | `insecure_target` | `false` | 跳过本地自签 HTTPS 目标证书校验 |
| `-ice-mdns` | `ICE_MDNS` | `ice_mdns` | `true` | 通过 mDNS 与 Fail-Closed 出站候选清洗隐藏内网 LAN/Docker/VPN 宿主 ICE IP |
| `-stun-servers` | `STUN_SERVERS` | `stun_servers` | `stun:stun.cloudflare.com:3478,stun:stun.l.google.com:19302` | 自定义 STUN 列表（逗号分隔，仅限 `stun:`/`stuns:`；设为 `none`/`off` 禁用） |
| `-turn-url` / `-turn-user` / `-turn-pass` | `TURN_URL` / `TURN_USER` / `TURN_PASS(_FILE)` | `turn_*` | 空 | 用户自建 TURN 中继配置 |
| `-max-streams` | `MAX_STREAMS` | `max_streams` | `512` | 全局并发 P2PMux V7 流上限 |
| `-metrics-addr` | `METRICS_ADDR` | `metrics_addr` | 空 | Prometheus `/metrics` 与 `/healthz` 监听地址 |
| `-portal-url` | `PORTAL_URL` | `portal_url` | 空 | 门户基地址（用于输出钉扎与分享链接） |
| `-mint-link` | — | — | 空 | 离线签发 Ed25519 能力分享链接（如 `2h`、`7d`）并退出 |
| `-mint-path` / `-mint-readonly` / `-mint-max-conns` | — | — | 空 / `false` / `0` | 能力令牌路径前缀、只读与并发连接限制 |
| `-health` / `-health-url` | — | — | `false` / 空 | 执行容器健康检查并退出 |

---

## 2. 信令服务端参数与环境变量 (`cmd/signal`)

| CLI 参数 | 环境变量 / 密钥文件 | YAML 字段 | 默认值 | 说明 |
| :--- | :--- | :--- | :--- | :--- |
| `-config` | `CONFIG_FILE` | — | 空 | 显式 YAML/JSON 配置文件路径 |
| `-addr` | `ADDR` | `addr` | `127.0.0.1:8888` | 监听地址（非回环地址必须配域名，否则拒绝启动） |
| `-portal-domain` | `PORTAL_DOMAIN` | `portal_domain` | 空 | Portal 基准域名（如 `portal.example.com`） |
| `-sandbox-domain` | `SANDBOX_DOMAIN` | `sandbox_domain` | 空 | 通配符沙箱域（Tier 3 跨站或 Tier 2 同站，如 `sandbox.example.net`，与 `-sandbox-host` 互斥） |
| `-sandbox-host` | `SANDBOX_HOST` | `sandbox_host` | 空 | Tier 1 跨站固定沙箱主机名（如 `app.example.net`，无需通配符证书，与 `-sandbox-domain` 互斥） |
| `-allow-same-site-sandbox` | `ALLOW_SAME_SITE_SANDBOX` | `allow_same_site_sandbox` | `false` | 显式允许 Tier 2 单根域同站通配符沙箱（自动附加 `Origin-Agent-Cluster: ?1`） |
| `-signal-secret` | `SIGNAL_SECRET` / `SIGNAL_SECRET_FILE` | `signal_secret` | 空 | Agent 集群默认注册密钥（未设且无 `-node-secrets-file` / `-allow-anonymous-agents` 时拒绝启动） |
| `-node-secrets-file` | `NODE_SECRETS_FILE` | `node_secrets_file` | 空 | 节点级专属密钥映射文件（YAML/JSON `node_id: secret`，覆盖全局密钥且不回退） |
| `-allow-force-rekey` | `ALLOW_FORCE_REKEY` | `allow_force_rekey` | 自动（共享密钥 `false`，逐节点密钥 `true`） | 是否允许已鉴权 Agent 通过 `force_rekey` 轮换已绑定的 Ed25519 公钥 |
| `-allow-anonymous-agents` | `ALLOW_ANONYMOUS_AGENTS` | `allow_anonymous_agents` | `false` | 显式允许匿名 Agent 注册 |
| `-bound-keys-file` | `BOUND_KEYS_FILE` | `bound_keys_file` | 空 | 持久化各 `node_id` Ed25519 公钥绑定表的 JSON 文件（`AtomicFileStore` 原子写入，重启防抢注） |
| `-allow-all-origins` | `SIGNAL_ALLOW_ALL_ORIGINS` | `allow_all_origins` | `false` | 放行任意 WebSocket Origin（仅限开发测试） |
| `-tls-cert` / `-tls-key` | `TLS_CERT` / `TLS_KEY` | `tls_cert` / `tls_key` | 空 | 正式 TLS 证书与私钥（支持磁盘变更热重载） |
| `-tls-auto` | `TLS_AUTO` | `tls_auto` | `false` | 生成仅含回环与配置域名的自签证书（不泄露局域网 IP；与 `-tls-cert`/`-tls-key` 互斥） |
| `-trusted-proxies` | `TRUSTED_PROXIES` | `trusted_proxies` | `127.0.0.0/8,::1/128` | 受信反代 IP/CIDR（从右向左剥离 XFF，不隐式信任 `X-Real-IP`） |
| `-client-ip-header` | `CLIENT_IP_HEADER` | `client_ip_header` | 空 | 可选单值客户端 IP 头（仅允许单个头名，兼容 `IP` 与 `IP:port`，如 `CF-Connecting-IP`、`Fastly-Client-IP`、`CloudFront-Viewer-Address`、`X-Real-IP`） |
| `-max-ip-conns` / `-max-ip-msgs` | `MAX_IP_CONNS` / `MAX_IP_MSGS` | `max_ip_conns` / `max_ip_msgs` | `64` / `120` | 每 IP（IPv6 按 `/64` 聚合）并发 WS 上限与每 10 秒消息上限 |
| `-static` | `STATIC_DIR` | `static_dir` | 空 | 外部前端静态目录（未设时使用编译期内嵌 `web.DistFS`） |
| `-metrics-addr` | `METRICS_ADDR` | `metrics_addr` | 空 | Prometheus `/metrics` 监听地址 |
| `-health` / `-health-url` | — | — | `false` / 空 | 探测 `/healthz` 健康状态并退出 |
