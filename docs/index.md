---
layout: home

hero:
  name: "WebRTC Gateway"
  text: "浏览器零客户端的 P2P 内网穿透网关"
  tagline: "无需安装客户端、插件或根证书，在标准浏览器中经端到端 DTLS 隧道直连内网 Web 服务、NAS 与私有云。"
  image:
    src: /logo.svg
    alt: WebRTC Gateway Logo
  actions:
    - theme: brand
      text: 快速开始
      link: /guide/
    - theme: alt
      text: 架构原理
      link: /guide/architecture
    - theme: alt
      text: GitHub 仓库
      link: https://github.com/BeingNoAbility/WebRTC-Gateway

features:
  - icon: 🌐
    title: 浏览器零客户端依赖
    details: 访客无需下载 App、安装插件或信任私有根证书，使用标准浏览器打开门户链接即可直连。Chrome / Edge 经 CI 自动化验证，其他浏览器见兼容性矩阵。
  - icon: ⚡
    title: P2PMux V8 统一多路复用数据面
    details: 单一 PeerConnection 预协商 1 条分片控制通道与 12 条有序多路复用通道，按单调 StreamID 统一承载 HTTP 与流式 WebSocket，8 字节帧头与纯载荷字节 CREDIT 流控。
  - icon: 🔒
    title: Ed25519 身份与 DTLS SPAKE2+ 认证
    details: 信令采用结构化描述符确定性合成 SDP，Agent 持久化 Ed25519 密钥验签并校验远端真实 DTLS 证书，DTLS 通道内完成 RFC 9383 SPAKE2+ 双向零知识认证。
  - icon: 🧩
    title: 跨站双域沙箱与内存 CookieMirror
    details: Portal 与应用沙箱运行在启动期显式配置的跨站源（本地 -dev 使用 *.localhost），通过原生 Transferable 流与内存 CookieMirror 隔离非 HttpOnly Cookie，支持 HTTP/HTTPS、206 Range 与 WebSocket。
  - icon: 📂
    title: 只读 O_NOFOLLOW 静态适配器
    details: 支持通过 -dir / -file 经 Go os.Root 只读托管静态目录或单文件，默认 O_NOFOLLOW 拒绝全部软链与点文件并下发 CSP sandbox，支持可吊销 Ed25519 能力链接。
  - icon: 🛡️
    title: 最小权限与单一权威 Go 信令
    details: 容器默认以 UID 10001 非特权用户与只读根文件系统运行；Go 信令单二进制内置前端资产，支持节点绑定持久化与逐节点密钥隔离。
---
