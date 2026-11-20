# WebRTC Gateway Release Notes

## [v2.3.2] - 2026-11-19

> **Notice for English Readers**: For comprehensive English release notes and full version history, please refer to [CHANGELOG.md](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/CHANGELOG.md).

### 核心更新与修复 (Core Updates & Fixes)

- **Mozilla PSL & eTLD+1 Root Domain Isolation (根域名提取与多租户隔离安全加固)**：
  - 引入 `golang.org/x/net/publicsuffix` 实现权威 eTLD+1 识别，根除高校/学术双层顶级域（如 `oxford.ac.uk`）截断隐患；
  - 隔离 `*.github.io` / `*.pages.dev` 等公共多租户平台，杜绝利用网关反代通道跨租户横向穿透；
  - 建立裸公共后缀防泛化熔断保护（禁止针对 `github.io` 全体放行）；重构 `net.SplitHostPort` 完美支持 IPv6。
- **Targeted Test Matrix & Troubleshooting FAQ (专项对抗测试集与排障问答完善)**：
  - 新增 PSL 边界隔离、CSWSH 端口解耦与重连退避专项回归测试；
  - 重新梳理面向普通使用者的中英文 FAQ 故障排除指南与架构说明。

### 破坏性变更 (Breaking Changes)

- 无 (None). Strictly 100% backward-compatible.

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.3.2
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.3.2
```
