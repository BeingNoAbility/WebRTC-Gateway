# WebRTC Gateway Release Notes

## [v1.7.1] - 2026-11-16

### 🎬 特殊文件名编解码修复、媒体流原生播放与 P2P File Vault 文件舱 (Special Filenames, Media Streaming & P2P File Vault)

- **`-dir` 目录模式特殊文件名 404 彻底修复 (Special Characters & Percent-Decoding)**：
  - **根本原因解决**：修复了浏览器发起包含 RFC 3986 百分号编码（如中文字符、方括号 `[ANi]`、空格 `%20` 等动漫番剧文件名）的请求时，Agent 服务端直接将编码路径传入底层 `os.Stat` 导致 404 Not Found 的历史缺陷；
  - **安全防御前置**：在执行 `path.Clean` 之前严格先执行 `url.PathUnescape`，并在解码后剔除空字节（Null-byte `%00`）注入，从根本上兼顾了 RFC 3986 还原与 `%2e%2e` 路径穿越防御；
  - **严格边界校验**：结合 `filepath.Rel` 与 `filepath.EvalSymlinks` 彻底防范目录逃逸与软链接逃逸。

- **`-file` 视频播放失败修复与多媒体流式体验升级 (Media Streaming Fix & Native Player)**：
  - **Service Worker 根路径精准放行**：将原 `url.pathname === '/' && event.request.destination !== 'iframe'` 粗暴规则重构为基于标准 W3C 规范的 `isTopLevelDocNav`，仅允许顶层 HTML 文档导航穿透到信令服务端加载门户外壳，100% 拦截并转发 `<video src="/">`、`<audio>` 及带有 `Range` 请求头的多媒体流；
  - **全平台内置 MIME 映射表**：在 `pkg/proxy/file_proxy.go` 中内置涵盖视频（MP4, WebM, MKV, MOV, TS, AVI）、音频（MP3, WAV, FLAC, AAC, M4A, OGG）、文档（PDF, TXT, MD, JSON）及压缩镜像的完整 MIME 字典，彻底解决 Alpine Linux、极简 Docker 镜像及 Windows 缺失 `/etc/mime.types` 时默认退化为 `application/octet-stream` 导致 HTML5 播放器拒播的痛点；
  - **原生 DOM 媒体播放器**：当检测到目标为视频/音频流时，脱离 iframe 沙箱限制，在 DOM 顶层动态挂载原生 HTML5 `<video controls autoplay playsinline>`，全面解锁 GPU 硬件加速、画中画（PiP）、全屏与原生快捷键交互。

- **Edge 桌面端与安卓端 PDF 拦截解决：全新打造 P2P File Vault 文件舱**：
  - **规避 W3C 沙箱插件冲突**：W3C 规范中 `sandbox` 属性强制禁用浏览器扩展插件（包括 Edge/Chromium 内置的 PDFium 插件），导致 iframe 渲染 PDF 时被 Edge 安全机制强行拦截（“此页面已被 Microsoft Edge 阻止” / 安卓端“已在 Microsoft Edge 中禁用 PDF 阅读器”）；
  - **P2P File Vault 极简高质感文件舱**：
    - 精致深色系卡片展示真实文件名、文件体积及 `⚡ P2P Direct Tunnel` 零中转直连状态；
    - **[ ⚡ 极速 P2P 下载 ]**：一键触发浏览器原生下载，跑满局域网/公网直连带宽；
    - **[ 📑 在新标签页打开原生预览 ]**：在独立的无沙箱标签页激活 Edge/Chrome 原生 PDF 阅读器，完整享受缩略图、目录、缩放、搜索与批注能力；
    - 针对移动端（iOS / Android）提供针对性使用指引。

- **严密的多层测试与真实浏览器矩阵验证**：
  - **单元测试**：`pkg/proxy/file_proxy_test.go` 新增 `TestFileProxy_SpecialFilenameAndURLDecoding`、`TestFileProxy_PathTraversalDefense_PercentEncoded`、`TestFileProxy_SingleFile_MediaAndPDF_MimeTypes`；
  - **端到端测试**：`test/file_server_e2e_test.go` 新增 `TestE2E_FileServer_SpecialCharactersAndMediaMIME`，完整跑通 DataChannel 隧道下的特殊字符视频点播与 PDF 传输；
  - **真实浏览器矩阵**：基于 CDP 真实无头 Edge 自动化运行 22 个场景（12 个复杂应用场景 + F5 会话自愈 + 全球 Top 10 真实网站镜像）全部 100% PASS。

---

### 📦 资产下载与快速验证
- 预编译二进制跨平台支持：Linux (amd64 / arm64)、Windows (amd64 / arm64)、macOS Darwin (amd64 / Apple Silicon arm64)。
- 多架构 Docker 镜像已同步发布至 GitHub Container Registry (`ghcr.io`)：
  - 服务端：`ghcr.io/beingnoability/webrtc-p2p-server:v1.7.1` (`latest`)
  - 穿透端：`ghcr.io/beingnoability/webrtc-p2p-agent:v1.7.1` (`latest`)
