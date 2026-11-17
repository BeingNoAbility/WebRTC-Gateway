# WebRTC Gateway Release Notes

## [v2.0.2] - 2026-11-17

### 🌟 可观测性与拓扑快照全面增强，双语文档站与系统不变量审查门禁闭环 (Observability, Bilingual Docs Suite & Systemic Invariants Gates)

本版本为 **v2.0.2 维护与可观测性增强版本 (Maintenance & Observability Release)**。重点解决了容器化与守护进程运维中“版本归因难”与“多级配置覆盖排错黑盒”的问题，并在 CI/PR 全生命周期中固化了系统级深水区审查防线，同时实现了官方文档站 100% 完整中英双语对齐：

- **服务端与穿透端启动 Banner 全息增强 (Observability & Topology Snapshot)**：
  - **版本即时归因**：`agent` 与 `signal` 启动日志第一行显式打印标准版本号（`v2.0.2`），使 Docker 容器与 systemd 日志排错一目了然；
  - **运行态拓扑快照**：Agent 完整打印信令连接地址（`Signaling`）、STUN/TURN 状态、并发流上限、代理与 SNI，将代理白名单域名统一收纳入闭合框线内；
  - **修复端口拼接瑕疵**：重构 Signal 监听地址解析逻辑，彻底消除 `-addr 0.0.0.0:8888` 导致 Web Portal 拼出 `localhost0.0.0.0:8888` 的历史遗留 Bug；
  - **静态资源形态透传**：明确指示静态前端资源来源为 `Embedded (v2.0.2)` 还是 `Local Directory (/custom/dist)`，便于前端联调即时确认；
  - **严格恪守 CWE-532 规范**：对所有密钥（`signal-secret`、`access-secret`、`turn-pass`）做脱敏处理，统一输出 `Enabled (Protected) / Disabled (Public)`，绝不在日志或终端输出敏感凭据明文。
- **工程防线左移与原子 PR 机制 (Shift-Left Engineering Gates)**：
  - **CI 机器防线硬核断言**：在 `.github/workflows/ci.yml` 的前端构建步骤后追加 `git diff --exit-code dist`，杜绝修改 TS 源码却忘记提交重新打包产物的疏漏；
  - **PR 范式自检卡**：建立 [`.github/pull_request_template.md`](file:///c:/Users/Star/Documents/webrtc-gateway/.github/pull_request_template.md)，在 PR 创建时刻唤起 5 大系统级根本不变量（规范不对称、并发时序、背压限额、拓扑漂移、跨引擎沙箱）推演，实现代码与审查准则原子合入；
  - **发版门禁守门兜底**：在发版清单中追加 5.6 条目，作为封版打标前的最终复盘审查防线。
- **100% 中英双语官方文档站全量对齐 (Full Bilingual Documentation Suite)**：
  - 深度补全 [P2P 文件与目录服务](file:///c:/Users/Star/Documents/webrtc-gateway/docs/en/guide/file-server.md)、[安全与权限控制](file:///c:/Users/Star/Documents/webrtc-gateway/docs/en/guide/security.md)、[故障排查与 FAQ](file:///c:/Users/Star/Documents/webrtc-gateway/docs/en/guide/troubleshooting.md) 以及 [发版审计清单](file:///c:/Users/Star/Documents/webrtc-gateway/docs/en/RELEASE_CHECKLIST.md) 的全量英文版；
  - 引入 [《辩证审查原则与核对清单》](file:///c:/Users/Star/Documents/webrtc-gateway/docs/REVIEW_GUIDELINES.md) 与英文版，同步升级 VitePress 双语镜像侧边栏与独立分词全文检索。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v2.0.2` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.2` (`latest`)
