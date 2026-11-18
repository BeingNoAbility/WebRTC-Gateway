---
layout: home

hero:
  name: "WebRTC Gateway"
  text: "浏览器零客户端的 P2P 内网穿透网关"
  tagline: "无需安装任何客户端、插件或证书，直接在现代浏览器中直连内网 Web 服务、NAS 与私有云。"
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
    details: 访客无需下载 App、无特权安装、无需配置根证书或插件，任何现代浏览器（Chrome / Edge / Firefox / Safari）开箱即连。
  - icon: ⚡
    title: 端到端 P2P 直连
    details: 基于 WebRTC STUN/ICE 打洞实现点对点物理直连，无云中继带宽限制，中心服务器仅撮合信令，零中继流量成本。
  - icon: 🔒
    title: 原生端到端传输加密
    details: 数据流采用标准 DTLS 1.2 / SRTP 传输层加密，信令服务仅撮合网络拓扑，中继节点亦无法解密任何用户业务通信。
  - icon: 🧩
    title: 完整 Web 协议栈透明透传
    details: 内置 P2PMux 多路复用，无缝支持 HTTP/HTTPS、HTTP 206 媒体流分片点播、WebSocket 全双工、以及独立隔离 CookieJar。
  - icon: 📂
    title: 内存级轻量文件分发
    details: 原生内置无需第三方 Web 容器的文件与目录穿透模式，支持大文件流式传输与 HMAC-SHA256 时效安全分享。
  - icon: 🛡️
    title: 非特权容器与沙箱加固
    details: 镜像原生以独立非特权用户 (10001:10001) 运行，全格式适配 --user 与只读文件系统，杜绝提权与安全逃逸风险。
---
