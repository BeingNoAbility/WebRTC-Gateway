# WebRTC Gateway Release Notes

## [v1.9.2] - 2026-11-17

### 🛡️ 架构与工程全景升级：容器非特权安全加固、VitePress 独立文档站与工业级发版质检协议

本版本为 **v1.9.2 正式发布版本**。在保持 WebRTC 极速直连与零客户端沙箱穿透的基础上，完成了容器安全基线加固、命令行体验规范化、官方文档站现代化跨越与发版质量门禁体系的建立：

- **容器安全加固与非特权用户支持 (Non-Root & Arbitrary `--user` Compatibility)**：
  - **默认非特权运行 (Least Privilege by Default)**：官方多架构 Docker 镜像默认以独立非特权用户 `appuser` (`UID: 10001, GID: 10001`) 运行，严格对齐 CIS Docker Benchmark 与 Kubernetes Restricted Pod Security Standard；
  - **NAS / 宿主机权限对齐**：支持通过 `--user $(id -u):$(id -g)`、`--user 1000:1000` 或 `--user nobody` 任意指定身份启动，彻底杜绝群晖 Synology、威联通 QNAP、Unraid 挂载卷权限不足或属主冲突；
  - **特权端口能力集绑定 (`CAP_NET_BIND_SERVICE`)**：`server` 镜像通过 Linux File Capability 预授权 `cap_net_bind_service=+ep`，即使以非特权身份运行，亦支持在容器内直接监听 `:443` 或 `:80` 等低位特权端口；
  - **只读文件系统沙箱兼容**：原生支持 `docker run --read-only` 与 Kubernetes `readOnlyRootFilesystem: true`。
- **结构化 CLI 帮助文档与版本探测 (-v / -h)**：
  - `agent` 与 `signal` 原生支持 `-v` / `-version` 显示版本信息并以退出码 `0` 退出；
  - 原生支持 `-h` / `-help` 显示完整排版规整的命令行运行模式、参数表格、环境变量与实战命令示例。
- **VitePress 官方独立文档站与中英双语国际化 (VitePress Docs & i18n)**：
  - 正式将原割裂的 GitHub Wiki 迁移升级为基于 VitePress 驱动的官方独立文档站：👉 **https://beingnoability.github.io/WebRTC-Gateway/**；
  - 原生支持中英文双语平滑切换（`/en/`）、本地 MiniSearch 毫秒级分词离线全文检索、Mermaid 架构交互流程图与 Code Groups 代码组；
  - GitHub Wiki 全局设置只读归档告示并直链重定向至官方文档站对应章节。
- **工业化发版门禁清单 (`RELEASE_CHECKLIST.md`) 与 CI/CD 优化**：
  - 制定覆盖版本号对齐、全量静态类型、自动化测试矩阵、容器构建预审、单版本发布说明、Git 附注标签与发布后验证的七大阶段质量审计清单；
  - 优化 GitHub Actions 触发路径，消除重复 Docker 构建，全量集成 SHA256 校验和自动生成。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.9.2` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.9.2` (`latest`)
