# WebRTC Gateway Release Notes

## [v2.3.2] - 2026-11-19

### 核心更新与修复 (Core Updates & Fixes)

- **根域名提取与多租户隔离安全加固 (Mozilla PSL & eTLD+1)**：
  - 引入 `golang.org/x/net/publicsuffix` 静态 Trie 树实现权威 eTLD+1 识别，彻底解决高校/学术双层顶级域（如 `oxford.ac.uk`、`tokyo.ac.jp`）被误截断为国家顶级域的安全边界隐患。
  - 实现公共多租户托管平台（如 `*.github.io`、`*.pages.dev`）的租户根域物理隔离，杜绝利用网关代理通道横向穿透同平台其他攻击者站点。
  - 建立裸公共后缀防泛化熔断保护，当目标为 `github.io` 或 `co.uk` 时禁止通配放行全体同级租户。
  - 采用 `net.SplitHostPort` 规范重构主机端口解析，完美支持全格式 IPv6（`[::1]:8080`、`::1`）带端口安全代理。
- **专项对抗性测试集与常见问题解答完善**：
  - 新增 PSL 边界隔离、CSWSH 端口解耦与重连退避专项回归测试矩阵。
  - 重新梳理中英文 FAQ 故障排除指南，覆盖常见生产部署与跨域排查场景。

### 破坏性变更 (Breaking Changes)

- 无 (None)。严格保持 100% 向后兼容。

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.3.2
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.3.2
```
