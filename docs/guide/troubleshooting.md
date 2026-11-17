# 故障排查与常见问题 (Troubleshooting & FAQ)

---

## 1. 常见故障排查手册

### ① 页面一直卡在“正在连接 Agent (Connecting)”或最终报 ICE 连接失败
- **故障根因**：双方网络存在最严格的**对称型 NAT (Symmetric NAT / NAT 4)**，导致纯 STUN 反射候选无法完成打洞；
- **排查与解决**：
  1. 打开右下角 WebRTC HUD 悬浮球，查看 ICE Candidates 候选列表；
  2. 若两端都是对称型 NAT，必须配置 TURN 中继服务器以实现平滑降级：
     ```bash
     ./agent -node-id myhome \
             -target http://127.0.0.1:8080 \
             -turn-url turn:relay.yourdomain.com:3478 \
             -turn-user your_username \
             -turn-pass your_password
     ```
  3. 配置 TURN 后，系统将自动使用 `relay` 候选在中继模式下完成直连，HUD 状态药丸将切换为 `🛡️ TURN Relay`。

---

### ② 局域网纯 IP 访问时 Service Worker 注册失败或页面白屏
- **故障根因**：现代浏览器安全规范（W3C Secure Context）强制要求：**Service Worker 仅允许在 `localhost` 或具备有效 SSL/TLS 证书的 `https://` 域下激活**。在局域网直接用纯 HTTP 打开 `http://192.168.1.100:8888` 会被浏览器直接拦截 Service Worker；
- **一键解法**：
  在启动一体化信令服务端时增加 `-tls-auto` 参数：
  ```bash
  ./signal -addr :8443 -tls-auto
  ```
  服务端将在内存中动态签发覆盖本机所有局域网真实物理网卡 IP 的 ECDSA SAN 证书。在浏览器直接打开 `https://<局域网IP>:8443/#myhome` 接受证书提示，即可完整激活 Service Worker！

---

### ③ 移动设备（手机/笔记本）在 Wi-Fi 与 5G 移动蜂窝网络间切换时连接断开
- **自动自愈机制**：
  从 **v1.4.0** 起，系统原生内建了**双阶段 WebRTC ICE Restart 韧性流水线**：
  1. 手机切网瞬间底层 TCP/WebSocket 套接字断开，浏览器自动闪连信令并保留原有 PeerConnection；
  2. 触发带有 `iceRestart: true` 的新一轮 Offer 协商；
  3. Agent 具备 5 秒断网宽限保护期，取消提前析构，在新网络建立后无缝继续传输数据；
- **排查提示**：若漫游自愈偶尔失效，点击右下角 HUD 诊断看板中的 **`🔄 立即触发 ICE Restart`** 按钮可一键手工自愈。

---

### ④ Docker 容器内的 Agent 无法访问宿主机的 `http://127.0.0.1:8080`
- **故障根因**：Docker 容器默认处于独立的 Bridge 网络命名空间中，容器内的 `127.0.0.1` 指向容器自身而非宿主机；
- **推荐解法**：
  - **Linux 宿主机**：在 `docker-compose.yml` 或 `docker run` 中添加 `network_mode: "host"`（或 `--net=host`），Agent 直接共享宿主机网络栈，访问 `127.0.0.1` 畅通无阻；
  - **Windows / macOS Docker Desktop**：将 `-target` 设置为 `http://host.docker.internal:8080`。

---

### ⑤ 提示 401 Unauthorized 或“信令集群准入拒绝”
- **故障根因**：新旧版本密钥混淆或配置不匹配；
- **排查清单**：
  1. **检查控制面集群密钥**：确认信令服务端的 `-signal-secret` 与 Agent 启动时的 `-signal-secret` 完全一致；
  2. **检查数据面访客口令**：若 Agent 配置了 `-access-secret`，访客必须在弹窗中输入该口令，或使用对应的 HMAC-SHA256 签名分享链接（`#nodeId?exp=xxx&sign=yyy`）；
  3. **升级提示**：旧版 `-secret` 与 `SECRET` 已彻底废弃并完全移除，请勿在 Agent 或 Signal 服务端中继续传递 `-secret`，必须使用明确语义的 `-signal-secret`（集群注册）与 `-access-secret`（访客口令）。

---

## 2. 常见问题 (FAQ)

### Q: 这个网关能跑满我的千兆宽带吗？
**A:** 可以！只要打洞成功（HUD 显示 `⚡ P2P Direct`），数据包通过 WebRTC UDP 直连直通，传输速率完全取决于双方物理宽带的上行和下行峰值，中间没有任何限速节点。

### Q: 穿透后我的 NAS / 本地 Web 服务需要修改代码吗？
**A:** 完全不需要！网关在浏览器端基于 Service Worker 透明拦截所有请求，在 Agent 端将二进制流解包并按标准 HTTP/WebSocket 反向代理发送给本地服务，目标服务看到的就像来自 `127.0.0.1` 的原生请求。

### Q: 我可以分享给朋友单个大文件（如 20GB 的 ISO），而不用搭建网盘吗？
**A:** 完全可以！只需一条命令：
```bash
./agent -file ./large-game.zip -access-secret pass123 -sign-link 24h
```
将生成的链接发给朋友，对方浏览器打开即刻通过 P2P 满速下载，零中转流量，到期自动失效。

### Q: 为什么某些页面在 iframe 嵌套中无法打开？
**A:** 许多后台系统带有 `Anti-Frame-Busting` 脚本。网关的 `patch.js` 已经内置了沙箱虚拟化拦截。若遇到极为特殊的系统，可在控制台设置弹窗中尝试将虚拟路由参数微调或开启沙箱增强。
