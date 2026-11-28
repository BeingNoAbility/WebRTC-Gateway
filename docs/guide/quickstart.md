# 五分钟上手 (Quickstart Guide)

本文面向第一次使用 WebRTC Gateway 的用户：先在本机跑通，再分享文件，最后按条件选择生产部署档位。每一步的完整参数见 [Agent 与信令服务端配置参考](./agent-configuration)。

---

## 1. 选择适合你的方式

| 你的目标 | 推荐方式 | 需要域名与证书 | 阅读章节 |
| :--- | :--- | :--- | :--- |
| 先体验、本机开发调试 | 本机 `-dev` 回环模式 | 否 | 第 2 节 |
| 把一个文件或目录临时分享给他人 | `-file` / `-dir` 静态适配器 | 取决于信令服务部署位置 | 第 3 节 |
| 把内网 Web 服务提供给外部访客 | 生产部署：Tier 1 或 Tier 2 | 是 | 第 4 节 |

---

## 2. 本机体验

需要已安装 Go 与 Node.js（版本要求见 [CONTRIBUTING](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/CONTRIBUTING.md)）。以下命令分别在三个终端中运行：

```bash
# 终端 1：构建并启动信令服务器（-dev 仅用于本机回环）
make build
./bin/signal -addr 127.0.0.1:8888 -dev -signal-secret dev-cluster-secret-16b

# 终端 2：启动一个被代理的本地服务，例如 8080 端口上的任意 HTTP 服务
python3 -m http.server 8080 --bind 127.0.0.1

# 终端 3：启动 Agent，并设置访问口令
ACCESS_SECRET=change-me ./bin/agent \
  -id homeserver \
  -signal ws://127.0.0.1:8888/ws \
  -signal-secret dev-cluster-secret-16b \
  -target http://127.0.0.1:8080
```

在浏览器打开 `http://localhost:8888/#homeserver`，输入 `change-me` 即可访问本地 `8080` 服务。

---

## 3. 分享一个文件或目录

保持第 2 节的信令服务器运行，将 `-target` 换成 `-dir` 或 `-file`（三者互斥）：

```bash
ACCESS_SECRET=change-me ./bin/agent \
  -id my-files \
  -signal ws://127.0.0.1:8888/ws \
  -signal-secret dev-cluster-secret-16b \
  -dir /srv/share
```

静态适配器默认只读，拒绝软链接与点文件，并对所有文件下发沙箱 CSP。如需让他人无需主口令即可访问，可用 `-mint-link` 离线签发限时、限路径、只读的分享链接，详见 [Agent 典型用例](./agent-use-cases) 与 [只读文件与目录适配器](./file-server)。

---

## 4. 提供给外部访客：选择部署档位

生产环境必须在启动期显式配置 `-portal-origin` 与 `-sandbox-origin`。按你拥有的域名与证书选择：

| 你的条件 | 推荐档位 | 说明 |
| :--- | :--- | :--- |
| 有两个不同主域名，且能申请泛域名证书 | **Tier 1 跨站通配符** | 跨站隔离边界最完整，适合多节点与对外分享 |
| 只有两个普通域名，无法申请泛域名证书 | **Tier 2 固定单域跨站** | 单节点或互信应用；门户会显示警示条 |
| 只有一个主域名 | **Tier 2 同站通配符** | 需显式 `-allow-same-site-sandbox`；门户会显示警示条 |

DNS、证书与反向代理的完整步骤见 [生产部署指南](./deployment)，Docker 加固部署见 [服务端部署用例](./server-use-cases)。部署后遇到问题请查阅 [故障排查与常见问题](./troubleshooting)。

---

## 5. 下一步

- 了解安全模型：[安全机制与密码学边界](./security)。
- 确认访客浏览器是否受支持：[浏览器兼容性矩阵](./browser-compatibility)。
- 了解整体设计：[系统架构与核心设计](./architecture)。
