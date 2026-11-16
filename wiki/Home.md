# Welcome to the WebRTC Gateway Wiki

欢迎查阅 **Zero-Client P2P WebRTC Intranet Penetration Gateway**（浏览器零客户端依赖的 P2P WebRTC 端到端内网穿透网关）官方技术文档库与知识中心！

---

## 🌟 核心理念与愿景

这是一个彻底颠覆传统内网穿透范式的开源项目：
- **浏览器端 100% 零依赖**：访客不需要下载客户端、安装应用、配置特权根证书、或安装浏览器插件；
- **真正的 P2P 极速直连**：基于 WebRTC STUN/ICE 打洞，流量直接在访客浏览器与内网服务器之间传输，不消耗任何中心服务器的中继带宽；
- **端到端原生强加密**：传输层强制采用军工级 DTLS-SRTP 加密，公网信令服务仅负责网络拓扑撮合，无法解密任何业务内容；
- **完整 Web 协议栈透明透传**：HTTP/HTTPS、HTTP 206 Partial Content (音视频流点播)、WebSocket 双向通信、Cookie 会话隔离、W3C 上传进度虚拟化开箱即用。

---

## 📚 文档速览 (Documentation Index)

| 专题模块 | 核心内容概述 | 推荐阅读对象 |
| :--- | :--- | :--- |
| **[📐 架构与设计 (Architecture & Design)](Architecture-and-Design)** | 深入了解 P2PMux v1 二进制分帧协议、浏览器端 Service Worker 透明拦截、DOM/XHR 虚拟化猴子补丁与独立 CookieJar 机制。 | 架构师 / 核心开发者 |
| **[🚀 生产部署指南 (Deployment Guide)](Deployment-Guide)** | 详尽指导 Cloudflare Workers 边缘部署 (0元成本)、一体化自包含 Go 二进制单文件运行、Docker / Docker Compose 容器编排。 | 运维工程师 / 自建用户 |
| **[⚙️ Agent 配置指南 (Agent Configuration)](Agent-Configuration)** | Agent 与信令服务全量 CLI 命令行参数、环境变量、YAML/JSON 配置文件结构与优先级机制。 | 全体使用者 / NAS 玩家 |
| **[📂 P2P 文件与目录服务 (File & Dir Server)](File-and-Directory-Server)** | 探索 v1.6.0+ 杀手级特性：纯内存零监听端口的文件分发、私有版 Wormhole / AirDrop 单文件秒传与静态目录在线托管。 | 开发者 / 交付协作人员 |
| **[🔒 安全与权限控制 (Security & Auth)](Security-and-Authentication)** | 控制面与数据面密钥解耦 (`-signal-secret` vs `-access-secret`)、HMAC-SHA256 无状态时效安全分享、路径穿越沙箱防御。 | 安全合规 / 团队管理员 |
| **[❓ 故障排查与 FAQ (Troubleshooting & FAQ)](Troubleshooting-and-FAQ)** | 对称型 NAT 穿透诊断、TURN 中继配置、自签 HTTPS 与 Service Worker 安全上下文、移动漫游 ICE Restart 自愈。 | 故障排查 / 遇到问题人员 |

---

## 🚀 30 秒极速体验

### 1. 服务端运行 (公网 VPS 或 本地测试)
```bash
# 单一二进制启动（内置嵌入式前端）
./signal -addr :8888
```

### 2. 客户端 Agent 运行 (内网目标机器)
```bash
# 反代内网现有 Web 服务 (模式 A)
./agent -node-id myhome -signal ws://your-server-ip:8888/ws -target http://127.0.0.1:8080

# 或免配置极速分发单个大文件 (模式 C)
./agent -node-id share -signal ws://your-server-ip:8888/ws -file ./Ubuntu.iso -access-secret pass123 -sign-link 24h
```

### 3. 浏览器访问
在远程电脑或移动手机浏览器打开：
👉 **`http://your-server-ip:8888/#myhome`**

即刻享受端到端 P2P 直连！
