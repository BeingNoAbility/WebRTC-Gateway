# P2P 零监听端口文件与目录服务 (File & Directory Server)

从 **v1.6.0** 开始，WebRTC Gateway 引入了原生**零监听端口 (Zero-Listener)** 的 P2P 文件分发与静态目录托管能力。

这一特性的核心价值在于：**将 WebRTC 的“免客户端”优势发挥到极致，实现跨公网私有版 AirDrop / 零中转 Wormhole**。发送方无需安装任何复杂的网盘软件或开放公网端口，接收方在任意设备打开现代浏览器即可跑满带宽直连下载。

---

## 1. 架构理念与设计边界

### ① 纯出站零端口哲学 (Zero Host Listening Ports)
传统文件服务器（如 Nginx、Caddy、python `http.server`、FileBrowser）必须在宿主机上绑定并监听一个本地 TCP 端口（如 `:8000`）。
WebRTC Gateway 创新性地设计了 **`streamResponseWriter` 内存流适配器**：
- 直接在 Go 进程内部将 WebRTC P2PMux 二进制流桥接至 `http.ResponseWriter`；
- **宿主机不开启任何额外的 TCP 监听端口**，杜绝了端口冲突、特权绑定限制以及防火墙入站暴露风险；
- 保持严格的**纯出站（Outbound-Only）**安全模型。

### ② 四条防臃肿红线 (Design Boundaries)
1. **纯只读 (Read-Only)**：仅支持 `GET`、`HEAD`、`OPTIONS`，严禁 `POST`、`PUT`、`DELETE`、上传、改名等复杂写操作；
2. **轻交互**：单文件直接推流触发下载/播放，目录使用轻量级现代化只读 HTML 呈现，零第三方前端依赖；
3. **零外部存储依赖**：不引入 SQLite、MySQL 或 Redis，利用文件系统本身，即关即走；
4. **强安全沙箱**：严密防御 `../` 路径穿越与符号链接逃逸攻击。

---

## 2. 核心模式与使用指南

### 模式一：单文件极速秒传 (私有版 Wormhole / AirDrop)
将本地任意单个文件（不限大小，大至数十 GB 的虚拟机镜像或超清视频）作为服务目标：

```bash
# 启动单文件 Agent，并一键生成 24 小时有效的免密分享链接：
./agent -node-id share-iso \
        -file /data/isos/Ubuntu-24.04-desktop-amd64.iso \
        -access-secret mySecret123 \
        -sign-link 24h
```

**命令行输出示例：**
```text
================================================================================
Generated Stateless Signed Share Link:
https://p2p.yourdomain.com/#share-iso?exp=1731852000&sign=a3f89e...
Expiration: 2026-11-17 18:00:00 CST (Valid for: 24h0m0s)
================================================================================
```

**接收方体验：**
- 接收方只需在手机、平板或电脑浏览器打开该链接；
- 浏览器在数秒内与发送方完成 WebRTC 直连打洞，立即触发满速直连下载；
- 流量完全点对点传输，不消耗任何中心服务器流量；
- 24 小时后，签名在数学上天然失效。

---

### 模式二：静态网站与目录在线浏览
将本地文件夹托管为 Web 站点或文件列表：

```bash
./agent -node-id docs -dir /data/websites/vitepress-dist
```

- **自动索引**：若目录下包含 `index.html`，自动作为首页渲染，完美托管 VitePress、Vue/React 打包产物、MkDocs 等纯静态网站；
- **自适应目录浏览**：若无 `index.html`，自动呈现高保真响应式暗黑风文件树，支持按文件名排序、显示文件大小和修改时间。

---

## 3. 关键特性与工程实现

### ① 智能 Content-Disposition 策略
根据 MIME 类型智能研判文件展示方式：
- **内联展示 (`inline`)**：图片（`.png`, `.jpg`, `.webp`, `.svg`）、视频（`.mp4`, `.webm`）、音频（`.mp3`, `.wav`）、文档（`.pdf`, `.txt`, `.json`）；
  - 访客在浏览器中打开链接可直接在线播放、预览与拖拽进度条；
- **强制下载 (`attachment; filename="..."`)**：压缩包（`.zip`, `.tar.gz`, `.7z`）、系统镜像（`.iso`, `.img`）、执行文件（`.exe`, `.bin`）；
  - 自动触发浏览器原生保存文件下载流程。

### ② 原生 HTTP 206 Partial Content (断点续传与媒体快进)
- 完整实现标准 `Range: bytes=start-end` 请求头解析；
- 返回 `206 Partial Content` 与精准的 `Content-Range: bytes 0-1048575/104857600` 标头；
- 视频播放时随意拖动进度条即可瞬时跳播，支持多线程分块下载与断点续传。

---

## 4. 严密的安全沙箱与特殊文件名解析 (v1.7.1 加固)

针对目录托管模式，系统内置了严密的安全与 URL 解码防御流程：

```text
访客请求路径 ➔ RFC 3986 安全百分号解码 ➔ Null-byte 空字节拦截 ➔ filepath.Clean 净化
              ➔ filepath.Rel 词法边界校验 (杜绝 ../ 穿越)
                ➔ filepath.EvalSymlinks 物理软链求值 (杜绝符号链接逃逸)
```

1. **特殊文件名与亚洲字符兼容**：
   - 浏览器对非 ASCII 字符（如中文、日文番剧名、方括号 `[ANi]`、空格等）按规范进行百分号编码；
   - 在路径净化前先执行 `url.PathUnescape`，确保正确匹配磁盘物理文件，彻底避免 404 Not Found；
   - 解码后严密校验 `\0` 空字节注入并直接阻断（`400 Bad Request`）。
2. **词法穿越防御**：通过 `filepath.Rel(rootDir, targetPath)`，若相对路径以 `..` 开头或为绝对路径，立即阻断并响应 `403 Forbidden`；
3. **符号链接逃逸防御**：通过 `filepath.EvalSymlinks` 解析真实物理路径，若软链指向了受管目录之外的系统敏感文件（如 `/etc/passwd`、`C:\Windows`），直接阻断并响应 `403 Forbidden`；
4. **隐藏文件屏蔽**：以 `.` 开头的隐藏文件或隐藏目录拒绝直接读取。

---

## 5. P2P File Vault 文件舱与原生媒体流 (v1.7.1 新增)

### ① P2P File Vault 文件舱 (解决 Edge/Android PDF 沙箱拦截)
W3C HTML5 规范规定：带有 `sandbox` 属性的 iframe 强制禁用浏览器扩展插件（包括 Edge/Chromium 内置的 PDFium 插件），导致直接内嵌 PDF 时浏览器提示“此页面已被 Microsoft Edge 阻止”。
在 v1.7.1 中，门户检测到文档/二进制资源时自动挂载 **P2P File Vault 文件舱**：
- **极简深色文件卡片**：展示文件名、格式化体积与 `⚡ P2P Direct Tunnel` 零中转直连状态；
- **[ ⚡ 极速 P2P 下载 ]**：通过 WebRTC DataChannel 流式下载，跑满对等端带宽；
- **[ 📑 在新标签页打开原生预览 ]**：在独立的顶层无沙箱标签页激活 Edge/Chrome 原生 PDFium 阅读器，提供完整的缩略图、目录、缩放、搜索和批注功能。

### ② 全功能原生 DOM 媒体播放器
针对视频（MP4, WebM, MKV）与音频（MP3, FLAC, WAV）文件：
- 脱离 iframe 沙箱限制，在 DOM 顶层动态挂载原生 HTML5 播放器；
- **GPU 硬件加速**：解锁硬件解码性能，彻底消除卡顿；
- **原生全屏与画中画 (PiP)**：支持键盘快捷键（空格播放/暂停、方向键快进/后退/调音量、F 键全屏）；
- **目录模式一键返回**：目录点播视频时提供顶栏 `[ ⬅ 返回目录 ]` 浮动按钮，随时返回文件树。
