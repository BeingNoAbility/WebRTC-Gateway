# WebRTC Gateway Release Notes

## [v2.4.1] - 2026-11-20

> **Notice for English Readers**: For comprehensive English release notes and full version history, please refer to [CHANGELOG.md](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/CHANGELOG.md).

### 核心更新与修复 (Core Updates & Fixes)

- **Universal Upload Content-Length Restoration & Nginx 413 Mitigation (通用上传长度穿透与 413 规避)**：
  - 在前端 XHR 与 Fetch 拦截层自动计算注入 `x-p2p-body-length` 标头，攻克 W3C Fetch 规范剥离 `Content-Length` 的缺陷；
  - Go 代理层自动提取重构真实 `req.ContentLength` 并还原标头，停用 `Transfer-Encoding: chunked`，彻底解决 LibreSpeed、Nextcloud、Alist 等大文件上传触发 Nginx 默认 1M 限额熔断问题；
  - 在 `port.onmessage` 中补齐 `REQ_BODY_ERROR` 状态机重置处理，杜绝上传流异常时的信道与协程挂死。
- **Worker Injection Regex & UI Consistency (Worker 压缩脚本注入增强与界面语言一致性收敛)**：
  - 放宽 Service Worker 脚本嗅探正则，消除 `$` 结尾限制，全面适配 `speedtest_worker.min.js` 等压缩型 Worker；
  - 修正调试视图 Header 语言按钮指示模型，统一显示当前生效语言（`🌐 简中` / `🌐 EN`），与设置弹窗内部高亮状态 100% 逻辑自洽。

### 破坏性变更 (Breaking Changes)

- 无 (None). Strictly 100% backward-compatible.

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.4.1
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.4.1
```
