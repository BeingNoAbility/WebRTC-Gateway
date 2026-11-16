# WebRTC Gateway Release Notes

## [v1.5.0] - 2026-11-16

### 🔑 密钥职责解耦体系 (Secret Decoupling: Control Plane vs Data Plane)
- **信令集群准入密钥 (`-signal-secret` / `SIGNAL_SECRET`)**：
  - 仅用于 Agent ↔ 信令服务器的节点注册准入与防冒领 (`Node Hijack Protection`)；
  - 核心防护：信令端收到 Offer 时绝不再将此密钥透露给访客浏览器，彻底避免因分享内网站点导致集群注册凭据泄露。
  - 兼容性：信令服务端保留 `-secret` 作为别名以平滑过渡。
- **访客访问口令门禁 (`-access-secret` / `ACCESS_SECRET`)**：
  - 仅用于访客浏览器 ↔ Agent 内网服务的访问权限；
  - **公开免密模式（默认）**：若未配置 `-access-secret`，访客浏览器直接打洞进入，不再弹出密码输入框。即使信令集群配置了严格的 `-signal-secret`，亦可顺畅发布开源演示站或公开博客！
  - **受保护模式**：配置后访客在浏览器打开时 Web 门户将自动感知并弹出高保真暗黑密码输入弹窗（或通过 URL 参数 `#nodeId?secret=xxx` 传递）；
  - **时效签名免密**：与 `-sign-link` 无缝绑定，生成纯数学 HMAC-SHA256 签名临时安全分享链接。
- **旧版 `-secret` 快速失败 (Fail-Fast)**：
  - Agent 彻底废弃模糊的 `-secret` 与 `SECRET` 环境变量，若启动时传入将直接报错退出并给出清晰的迁移提示，从根源杜绝安全配置歧义。

### ⚙️ 统一 YAML / JSON 配置文件支持 (`-config`)
- **双格式支持 (选项 B 架构)**：
  - 基于工业级标准 `gopkg.in/yaml.v3`，原生支持带注释的 YAML 语法，并 100% 兼容标准 JSON；
  - 提供开箱即用模板：`configs/agent.example.yaml` 与 `configs/server.example.yaml`。
- **四级优先级分层引擎**：
  - 优先级严格遵循：`默认内置值 < 配置文件 < 环境变量 < 命令行参数`。
- **零配置候选自动探测**：
  - 缺省未指定 `-config` 时，自动在当前目录及二进制所在目录探测 `./agent.yaml`、`./agent.yml`、`./agent.json`（服务端探测 `./server.yaml` 等），文件不存在时静默跳过，体验零心智负担。
- **显式指定快速失败**：
  - 显式通过 `-config <path>` 或 `CONFIG_FILE` 指定时，若文件不存在或语法损坏立即报错退出，绝不盲目回退默认配置。
- **友好类型解析**：
  - 支持类似 `stun_servers` 既可配置为 YAML 列表（`["stun:..."]`），也可写为逗号分隔字符串（`"stun:...,stun:..."`）。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.5.0` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.5.0` (`latest`)
