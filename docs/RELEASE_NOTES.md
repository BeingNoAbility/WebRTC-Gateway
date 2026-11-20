# WebRTC Gateway Release Notes

## [v2.4.0] - 2026-11-20

> **Notice for English Readers**: For comprehensive English release notes and full version history, please refer to [CHANGELOG.md](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/CHANGELOG.md).

### 核心更新与修复 (Core Updates & Fixes)

- **Zero-Dependency Bilingual Web Portal (零依赖纯净双语交互体系)**：
  - 构建轻量强类型 i18n 字典引擎 (`i18n.ts`, < 2.5KB)，支持简体中文与英文（en-US）双向自由切换；
  - 采用 DOM 原地热重绘机制，切换语言时 WebRTC DataChannel 与音视频/文件流传输 **100% 保持零中断**；
  - 彻底清理历史硬编码 Demo 标签（`librespeed`、`vaultwarden` 等），大幅收缩设置模态框体积，消除移动端误触排版溢出。
- **PaaS Multi-Tenant Subdomain Isolation & Security (PaaS 多租户根域名与子域名隔离)**：
  - 升级子域名智能提取逻辑，防御多级 PaaS 后缀（`*.pages.dev`、`*.workers.dev` 等）误穿透；
  - 增加全套国际化 1:1 双语键对齐与参数占位符自动化门禁（`test_i18n.cjs`）。

### 破坏性变更 (Breaking Changes)

- 无 (None). Strictly 100% backward-compatible.

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.4.0
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.4.0
```
