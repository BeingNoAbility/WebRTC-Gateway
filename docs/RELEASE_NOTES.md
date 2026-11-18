# WebRTC Gateway Release Notes

## [v2.0.8] - 2026-11-18

### 核心更新与修复 (Highlights & Fixes)

- **Service Worker 导航守卫与非 HTML 直传**：在 `web/src/sw/sw.ts` 引入 `isNavDoc` 守卫，将 HTML 补丁注入严格限制在顶级导航与 iframe 上下文；移除非 HTML 响应在缺失骨架标签时的盲目前置拼接兜底，解决群晖 Tailscale 等老旧 CGI 接口反代时导致的 API JSON 语法解析报错（`SyntaxError: Unexpected token '<'`）。
- **工程文档规范体系与 CI 自动化门禁**：确立中性克制、简洁明了的文档工程基准（`docs/STYLE_GUIDE.md`）；新增自动化文档质检脚本并在 CI 流水线中强制阻断标题 Emoji 与营销词汇；解耦单体 README 为高信噪比门面。

### 破坏性变更 (Breaking Changes)

- **无破坏性变更 (None)**。完全向下兼容既有通信协议、配置格式与信令握手。

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.0.8
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.8
```
