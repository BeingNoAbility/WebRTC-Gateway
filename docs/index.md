---
layout: home

hero:
  name: "WebRTC Gateway"
  text: "浏览器零客户端的 P2P 内网穿透网关"
  tagline: "无需安装任何客户端、插件或证书，直接在浏览器中秒级直连内网 Web 服务、NAS 与私有云。"
  image:
    src: /logo.svg
    alt: WebRTC Gateway Logo
  actions:
    - theme: brand
      text: 快速开始 🚀
      link: /guide/
    - theme: alt
      text: 架构原理 📐
      link: /guide/architecture
    - theme: alt
      text: GitHub 仓库 ⭐️
      link: https://github.com/BeingNoAbility/WebRTC-Gateway

features:
  - icon: 🌐
    title: 浏览器 100% 零依赖
    details: 访客无需下载 App、无特权安装、无需配置根证书或插件，任何现代浏览器（Chrome / Edge / Firefox / Safari）开箱即连。
  - icon: ⚡
    title: 真实端到端 P2P 直连
    details: 基于 WebRTC STUN/ICE 打洞实现点对点物理直连，突破内网穿透带宽限制，中心服务器仅撮合信令，零中继流量成本。
  - icon: 🔒
    title: 原生端到端强加密
    details: 数据流采用军工级 DTLS-SRTP 物理加密，信令服务仅撮合网络拓扑，中继节点亦无法解密任何用户业务通信。
  - icon: 🧩
    title: 完整 Web 协议栈透明透传
    details: 内置 P2PMux 多路复用，无缝支持 HTTP/HTTPS、HTTP 206 媒体流拖拽播放、WebSocket 全双工、以及独立隔离 CookieJar。
  - icon: 📂
    title: 内存级极速文件分发
    details: 原生内置无需第三方 Web 容器的文件与目录穿透模式，单文件 1 秒安全分发，支持大文件拖拽秒传与时效分享。
  - icon: 🛡️
    title: 工业级沙箱与容器加固
    details: 镜像原生以独立非特权用户 (10001:10001) 运行，全格式适配 --user 与只读文件系统，杜绝提权与安全逃逸风险。
---
