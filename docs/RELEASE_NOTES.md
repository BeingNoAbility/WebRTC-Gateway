# WebRTC Gateway Release Notes

## [v2.0.8] - 2026-11-18

### 核心修复与工程规范建立 (Core Fixes & Engineering Style Guide)

本版本为 **v2.0.8 协议健壮性修复与工程规范版本 (Protocol Robustness & Style Guide Release)**。重点修复了群晖 Tailscale 等老旧 CGI 接口在 Service Worker 层因误判 HTML 导致 API JSON 解析崩溃的底层缺陷，并正式发布了官方技术文档与文案工程规范体系及自动化 CI 质检门禁：

- **Service Worker 子资源 HTML 判定收敛与 JSON 安全直传 (Navigation Guard & Safe JSON Streaming)**：
  - 针对群晖 Tailscale (`/webman/3rdparty/Tailscale/index.cgi/`) 等老旧 CGI 或特定反代后端将 API/Fetch 请求错误返回 `Content-Type: text/html` 的场景；
  - 在 [web/src/sw/sw.ts](file:///web/src/sw/sw.ts) 中引入 `isNavDoc` 导航守卫，严格将 HTML 补丁注入判定范围限定在顶级文档与 iframe 上下文（`request.mode === 'navigate' || request.destination === 'document' || request.destination === 'iframe'`）；
  - 彻底剔除 `injectScriptIntoHtmlStream` 在缺失 HTML 骨架标签（`<head>`, `<html>`, `<!doctype>`）时的盲目前置拼接兜底逻辑，保证非 HTML 响应 Body 保持 100% 原始字节无损流式直传；
  - 彻底根除控制台抛出的语法解析异常：`SyntaxError: Unexpected token '<', "<meta name"... is not valid JSON`，确保所有内网嵌装第三方套件与 API 接口平稳加载；
  - 在 [test/node_id_preservation_test.cjs](file:///test/node_id_preservation_test.cjs) 中新增全套 14 项自动化防护断言。
- **发布官方《文档与文案工程规范》与 CI 自动化质检门禁**：
  - 在 [docs/STYLE_GUIDE.md](file:///docs/STYLE_GUIDE.md) 与 [docs/en/STYLE_GUIDE.md](file:///docs/en/STYLE_GUIDE.md) 中正式确立“中性克制、简洁明了、技术除魅、标题纯净、排版严谨、平视对话”的现代极客工程文档基准；
  - 推出自动化文档质检脚本 `docs/scripts/lint-docs.cjs`（通过 `npm --prefix docs run docs:lint` 执行），并将其纳入 GitHub Actions CI 机器门禁流水线；
  - 全量完成项目 30+ 份中英文技术文档、TOC 目录锚点与侧边栏的规范化对齐与除魅。

---

### 破坏性变更提示 (Breaking Changes)
- **无破坏性变更 (None)**。本版本完全向下兼容既有的通信协议、二进制帧格式、信令握手与各平台配置文件。所有已部署的 Agent 与 Signal 节点均可无缝升级。

---

### 资产下载与快速验证 (Downloads & Verification)
- **预编译二进制跨平台支持**：
  - Linux (amd64 / arm64)
  - Windows (amd64 / arm64)
  - macOS Darwin (amd64 / Apple Silicon arm64)
  - 可前往 GitHub Releases 页面下载对应的 `.tar.gz` / `.zip` 压缩包及 SHA256 校验文件 `checksums.txt`。

- **官方 Docker 镜像拉取与启动**：
  多架构容器镜像已同步发布至 GitHub Container Registry (`ghcr.io`)，支持一键拉取：
  ```bash
  # 拉取服务端镜像
  docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.0.8
  docker pull ghcr.io/beingnoability/webrtc-p2p-server:latest

  # 拉取穿透端镜像
  docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.8
  docker pull ghcr.io/beingnoability/webrtc-p2p-agent:latest

  # 快速启动示例
  docker run -d --name p2p-server -p 8888:8888 ghcr.io/beingnoability/webrtc-p2p-server:v2.0.8
  docker run -d --name p2p-agent --network host ghcr.io/beingnoability/webrtc-p2p-agent:v2.0.8 -node-id nas -signal ws://127.0.0.1:8888/ws -target http://127.0.0.1:8080
  ```
