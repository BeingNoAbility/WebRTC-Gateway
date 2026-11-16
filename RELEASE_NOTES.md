# WebRTC Gateway Release Notes

## [v1.6.2] - 2026-11-16

### 🛡️ 官方高科技品牌矢量 Logo 正式发布与 Docker 原生健康检查修复 (Native Healthcheck & Brand Identity)

- **Docker 容器 Unhealthy 缺陷彻底根治 (Docker Health Check Hardening)**：
  - **根因分析**：原 `Dockerfile.server` 依赖 Alpine BusyBox 的 `wget --no-check-certificate` 参数，在开启 HTTPS/自签名证书或修改监听端口时，因 BusyBox `wget` 不支持该参数或探测端口不匹配导致命令失败并返回退出码 1，从而使容器被 Docker 判定为 `unhealthy`。
  - **零外部依赖 Go 原生健康探测器 (`-health`)**：
    - 为服务端引入 `-health` CLI 探针命令（兼备 `-health-url`），仅耗时 2ms、内存开销 < 5MB；
    - 自动智能适配 HTTP/HTTPS、自签名证书 (`InsecureSkipVerify: true`)、动态端口 (`ADDR` / `PORT`) 及 YAML/JSON 配置文件；
    - 成功返回退出码 0，失败返回退出码 1 并向 `stderr` 打印精准诊断日志；
    - 全面兼容 Docker `HEALTHCHECK`、Docker Compose 与 Kubernetes Liveness/Readiness 探针。
  - **信令端主端口原生挂载 `/healthz` 与 `/health`**：
    - 主 HTTP 服务监听端口开箱提供标准 `/healthz` 和 `/health` 端点（返回 HTTP 200 `{"status":"ok"}`）；
    - 保留指标端点 `/metrics` 及其专用端口探针。
  - **Docker 构建与编排文件同步升级**：
    - `Dockerfile.server`：更新为 `HEALTHCHECK CMD /app/server -health || exit 1`，并在生产镜像预装 `curl` 诊断工具；
    - `docker-compose.yml`：为 `server` 服务声明标准 `healthcheck` 块，容器启动 5 秒内平滑流转为 `(healthy)` 状态。

- **官方品牌视觉系统 (Brand Logo & Visual Identity)**：
  - **品牌徽章 (`assets/logo.svg`)**：
    - 512×512 纯矢量高科技网络盾牌构图，融合赛博蓝 (`#38bdf8`)、星系紫 (`#c084fc`) 与微光金核；
    - 象征双端 P2P 节点（左侧现代浏览器节点，右侧家庭 NAS/服务器节点）、中央穿透网关能量环与毫秒级端到端穿透闪电；
    - 军工级 DTLS-SRTP 加密防护盾与去中心化 Discovery 传播波纹。
  - **品牌横幅 (`assets/logo-banner.svg`)**：
    - 880×220 宽屏 GitHub 官方展示横幅，搭载徽章、中英双语字体与三大核心特性药丸徽章（`ZERO-CLIENT`、`P2P DIRECT`、`DTLS ENCRYPTED`）。
  - **门户与全端集成**：
    - Web 控制台界面 (`web/src/index.html`)、`favicon.svg`、`README.md`、`README_zh.md`、`README_en.md` 及 GitHub Wiki 全量接入官方 Logo 与 Banner。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.6.2` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.6.2` (`latest`)
