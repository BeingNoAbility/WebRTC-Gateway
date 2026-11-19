# 文档概览 (Guide Overview)

欢迎查阅 **Zero-Client P2P WebRTC Intranet Penetration Gateway**（浏览器零客户端依赖的 P2P WebRTC 端到端内网穿透网关）官方技术文档库与知识中心！

---

## 核心理念与愿景

这是一个采用免装客户端浏览器端 WebRTC 穿透架构的开源项目：

- **浏览器端零客户端依赖**：访客不需要下载客户端、安装应用、配置特权根证书、或安装浏览器插件；
- **端到端 P2P 直连**：基于 WebRTC STUN/ICE 打洞，流量直接在访客浏览器与内网服务器之间传输，不消耗任何中心服务器的中继带宽；
- **原生端到端传输加密**：传输层强制采用标准 DTLS 1.2 / SRTP 传输层加密 (RFC 8831)，公网信令服务仅负责网络拓扑撮合，无法解密任何业务内容；
- **完整 Web 协议栈透明透传**：HTTP/HTTPS、HTTP 206 Partial Content (音视频流点播)、WebSocket 双向通信、Cookie 会话隔离、W3C 上传进度虚拟化开箱即用。

---

## 专题指南导航 (Guide Navigation)

| 专题模块 | 核心内容概述 | 推荐阅读对象 |
| :--- | :--- | :--- |
| **[架构与设计原理](./architecture)** | 深入了解 P2PMux 二进制分帧协议、浏览器端 Service Worker 透明拦截、DOM/XHR 虚拟化猴子补丁与独立 CookieJar 机制。 | 架构师 / 核心开发者 |
| **[P2PMux 协议形式化规约](./protocol-specification)** | 10 字节定长帧头规范、奇偶流分配律、状态机模型、32KB 内存阶梯对齐、SCTP 接收窗口物理背压与跨端黄金测试向量。 | 协议开发者 / 核心实现人员 |
| **[生产部署指南](./deployment)** | 详尽指导 Cloudflare Workers 边缘部署、一体化自包含 Go 二进制单文件运行、Docker / Docker Compose 容器编排。 | 运维工程师 / 自建用户 |
| **[Agent 配置指南](./agent-configuration)** | Agent 与信令服务全量 CLI 命令行参数、环境变量、YAML/JSON 配置文件结构与优先级机制。 | 全体使用者 / NAS 管理员 |
| **[P2P 文件与目录服务](./file-server)** | 探索基于内存映射的文件分发、轻量安全分享与静态目录在线托管特性。 | 开发者 / 交付协作人员 |
| **[安全与权限控制](./security)** | 控制面与数据面密钥解耦 (`-signal-secret` vs `-access-secret`)、HMAC-SHA256 无状态时效安全分享、路径穿越沙箱防御。 | 安全合规 / 团队管理员 |
| **[浏览器兼容性矩阵](./browser-compatibility)** | Chrome、Edge、Firefox (Tier 1) 及 iOS Safari / WebKit (Tier 2) 跨端支持细则与后台进程休眠自愈机制。 | 前端工程师 / 移动端用户 |
| **[故障排查与 FAQ](./troubleshooting)** | 对称型 NAT 穿透诊断、TURN 中继配置、自签 HTTPS 与 Service Worker 安全上下文、移动漫游 ICE Restart 自愈。 | 故障排查 / 遇到问题人员 |

---

## 30 秒上手体验

::: code-group

```bash [公网信令服务 (Signal)]
# 单一可执行二进制直接启动（内置嵌入式 Web 控制台）
./signal -addr :8888
```

```bash [内网穿透客户端 (Agent)]
# 反代内网现有 Web 应用程序 (模式 A)
./agent -node-id myhome -signal ws://your-server-ip:8888/ws -target http://127.0.0.1:8080

# 或免配置极速加密分发单个大文件 (模式 C)
./agent -node-id share -signal ws://your-server-ip:8888/ws -file ./Ubuntu.iso -access-secret pass123 -sign-link 24h
```

```text [访客浏览器访问 (Browser)]
在任意现代浏览器中输入：
👉 http://your-server-ip:8888/#myhome

即刻建立真正的端到端 P2P 直连通道！
```

:::

---

## 周边生态与实验工具 (Ecosystem & Experimental Tools)

- **[WebRTC-Gateway-GUI](https://github.com/BeingNoAbility/WebRTC-Gateway-GUI)**：实验性桌面图形界面客户端原型。需要注意，该 GUI 为快速搭建的原型工程，尚未经过完整测试验证，不在本项目的核心维护与支持范围内。
