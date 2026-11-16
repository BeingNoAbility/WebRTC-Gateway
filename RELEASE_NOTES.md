# WebRTC Gateway Release Notes

## [v1.4.1] - 2026-11-16

### 🛡️ 信令端受信反向代理信任与防伪造机制 (Signaling Server Trusted Proxies & Anti-Spoofing)
- **请求头防伪造闭环**：彻底解决公网直连场景下客户端伪造 `X-Forwarded-For`、`CF-Connecting-IP` 或 `X-Real-IP` 绕过连接限制与口令防爆破锁定的安全隐患。
- **开箱即用零误杀设计**：
  - 默认内置信任本地回环（`127.0.0.0/8`, `::1/128`）、RFC 1918 私网网段（`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`）、运营商 CGNAT（`100.64.0.0/10`，兼容 Tailscale/Docker 覆盖网络）及 IPv6 ULA；
  - 确保在 Docker 容器网桥、Kubernetes Ingress、Nginx 及 Caddy 等前置反代部署下，真实外网访客 IP 能无感透传，避免将所有访客误判为网桥内网 IP 造成并发限制误杀。
- **公网裸连套接字硬绑定**：
  - 当信令端直接暴露在公网、直连 TCP 来源为未受信任的公网 IP 时，一律强制忽略所有代理请求头，直接绑定底层 TCP 套接字的真实 IP，杜绝 IP 变换洪水攻击。
- **Cloudflare 优先识别**：
  - 来源属于受信反代时，优先提取 Cloudflare 边缘节点强行重写的 `CF-Connecting-IP`，随后按序解析 `X-Forwarded-For` 首个 IP 与 `X-Real-IP`。
- **纳秒级高效匹配**：
  - 基于 Go 标准库 `net/netip` 在服务启动阶段将 CIDR 列表预编译为 `[]netip.Prefix`，运行时无任何堆内存分配与高频正则开销。
  - 支持 `-trusted-proxies` 命令行参数与 `TRUSTED_PROXIES` 环境变量配置自定义 CIDR / IP 列表，传入 `none` 可全局禁用代理头信任。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.4.1` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.4.1` (`latest`)
