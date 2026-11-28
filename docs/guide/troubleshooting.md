# 故障排查与常见问题 (Troubleshooting & FAQ)

部署配置见 [生产部署指南](./deployment)，参数全集见 [Agent 与服务端配置](./agent-configuration)，密码学与隔离模型见 [安全机制](./security)。

---

## 1. 常见故障排查

### 1. 页面长时间停留在“正在连接”或 ICE 连接失败

- **原因**：访客与内网 Agent 双方均位于对称型 NAT（Symmetric NAT）之后，STUN 无法完成直接打洞。
- **处理**：配置用户自建的 TURN 服务器（TURN 仅转发端到端 DTLS 密文）：

```bash
ACCESS_SECRET=change-me TURN_PASS=your-turn-pass ./bin/agent \
  -id myhome \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret \
  -target http://127.0.0.1:8080 \
  -turn-url turn:turn.example.com:3478?transport=udp \
  -turn-user turn_user
```

在浏览器右下角 SmartPill 面板中打开“网络设置”，也可填写 RFC 7065 格式的 TURN URI（注意格式为 `turn:host:port`，不要写成 `turn://host:port`）。

### 2. 服务端或 Agent 启动即退出（Fail-Closed 拒绝启动）

| 报错关键词 | 原因 | 解决方法 |
| :--- | :--- | :--- |
| `SIGNAL_SECRET is required` / `at least 16 bytes` | 信令端未配置准入密钥或长度不足 16 字节 | 设置不少于 16 字节的 `SIGNAL_SECRET` / `SIGNAL_SECRET_FILE`，或仅限开发期显式加 `-allow-anonymous-agents` |
| `both -portal-origin and -sandbox-origin are required` | 未配置显式源且未开启本地 `-dev` | 生产环境配置 `-portal-origin` + `-sandbox-origin`（单根域同站需加 `-allow-same-site-sandbox`）；本地回环开发显式传入 `-dev` |
| `ACCESS_SECRET is required` | Agent 未配置访客密钥 | 设置 `ACCESS_SECRET` / `ACCESS_SECRET_FILE`，或确需公开时显式加 `-allow-public` |
| `Specify exactly ONE of -target, -dir, or -file` | 未指定或同时指定了多个工作模式 | 启动时明确传入 `-target`、`-dir`、`-file` 三者之一 |
| `unknown field ...` | YAML 配置文件存在拼写错误或非法字段 | 对照 `configs/*.example.yaml` 修正字段名（配置加载启用了 `KnownFields(true)`） |

### 3. 浏览器弹出红屏阻断：“Agent 身份公钥发生变化”

- **原因**：远端 Agent 使用的 Ed25519 公钥与链接中钉扎的 `?pk=` 指纹或浏览器 `IndexedDB` 此前记录的 TOFU 指纹不一致。
- **处理**：
  - 若你**未**重装或轮换 Agent 密钥，**立即中止访问**，排查信令服务器或网络链路是否遭劫持；
  - 若你在宿主机主动更换了 `-identity-key`（或使用了 `-ephemeral-identity` 重启了 Agent）：对裸 `#node` 访问可点击红屏弹窗中的重置信任按钮；对带 `?pk=` 的钉扎分享链接须由 Agent 重新签发新链接（钉扎链接绝不允许界面绕过）；
  - 若信令端返回公钥冲突拒绝 Agent 注册，在确认合法轮换后为 Agent 传入 `-force-rekey`（注意：共享 `SIGNAL_SECRET` 下服务端 `-allow-force-rekey` 默认 `false`，须使用逐节点 `-node-secrets-file` 或在服务端显式开启 `-allow-force-rekey=true`）。

### 4. 浏览器 WebSocket 握手报 `403 Forbidden` 或 `421 Misdirected Request`

- **`421 Misdirected Request`**：请求的 `Host` 头不在 `PORTAL_ORIGIN` / `SANDBOX_ORIGIN`（或 `-dev` 模式下的 `localhost` / `*.localhost`）白名单内。确认反向代理透传了原始 `Host` 头，且未通过裸 IP 或未授权域名直连。
- **`403 Forbidden`（反代后方）**：浏览器发送的 `Origin` 头与服务端启动期配置的 `-portal-origin`（含协议与端口）不完全一致。核对反代对外协议/端口是否与 `-portal-origin` 一致；若访客全部触发同一 IP 限流，再在 `-trusted-proxies` 中添加反代真实 CIDR。
- **`403 Forbidden`（直接访问沙箱域名）**：直接打开或外站探测沙箱域名会触发 SW W3C Fetch 抗 CSRF 门禁；请始终通过 Portal URL（如 `https://portal.example.com/#node`）接入。

### 5. Docker 容器内 Agent 无法访问宿主机 `127.0.0.1:8080`

- **Linux 宿主机**：使用 `network_mode: "host"` 直接访问宿主机回环服务；
- **macOS / Windows Docker Desktop**：改用 bridge 网络，添加 `extra_hosts: ["host.docker.internal:host-gateway"]`，并将 `TARGET_URL` 指向 `http://host.docker.internal:8080`。

### 6. 反代本地自签名 HTTPS 服务（如 Proxmox VE、NAS 面板）报 `502`

- Agent 默认校验目标服务的 TLS 证书链；对使用自签证书的内网目标显式传入 `-insecure-target`（环境变量 `INSECURE_TARGET=true`）。

### 7. 应用依赖虚拟主机 `Host` 头或多域名资源聚合

- 每个 Agent 严格限定为单一 `-target` 精确源（不提供外域跨源代理）；
- 若目标位于本机 Nginx / Caddy / Ingress 虚拟主机后方，按需配置 `-upstream-host`（配合 HTTPS `-sni`）覆写上游 `Host` 头与对应 `Origin`/`Location`/Cookie 域；
- 若应用强依赖多子域或分离式 API 聚合、响应体链接改写，在 `-target` 下游部署本机通用反向代理（Nginx / Caddy）统一收口到单一本地入口后再接入 Agent，完整配方见 [本地反代协作与典型适配模式](./reverse-proxy-recipes.md)。

---

## 2. 常见问题 (FAQ)

### Q: 访客设备需要安装客户端或插件吗？
不需要。使用标准现代浏览器（Chrome、Edge、Safari、Firefox）直接打开 Portal 链接或分享链接即可连接。

### Q: 传输大文件会消耗公网信令服务器带宽吗？
不会。信令服务器只交换少量 JSON 握手消息；建立直连后全部数据在访客浏览器与内网 Agent 之间点对点传输。仅当双方均处于对称 NAT 且配置了自建 TURN 时，才消耗用户自建 TURN 的流量。

### Q: 如何安全地临时分享某个子目录或只读页面给外部访客？
在已认证的 Portal 面板中打开“分享链接”弹窗（或使用命令行 `./bin/agent -mint-link 2h -mint-path /public -mint-readonly`），生成带 `?pk=...&cap=...` 的 Ed25519 能力链接。访客无需密码即可在限定路径与只读权限内访问，Owner 可随时在面板中一键吊销该令牌。

### Q: 手机在 Wi-Fi 与蜂窝网络之间切换会丢失当前页面吗？
Portal 的统一静默重连会在底层连接断开后自动新建 `RTCPeerConnection`，复用当前 Tab 内存派生的 SPAKE2+ 验证器完成重认证，整个过程保持 `appFrame` 挂载不刷新且不重弹密码框。
