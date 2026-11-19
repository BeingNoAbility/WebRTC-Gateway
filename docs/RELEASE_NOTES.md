# WebRTC Gateway Release Notes

## [v2.3.1] - 2026-11-19

### 紧急热修复与功能完善 (Hotfix & Improvements)

- **信令服务端 CSWSH 跨域白名单多维匹配与端口解耦**：
  - 修复 `OriginChecker` 在处理子域名通配加端口（如 `https://*.example.com:8443`）或端口通配（如 `https://*.example.com:*`）时，因端口未解耦导致子域匹配误判阻断的问题。
  - 完善域名与端口解耦比对模型，原生支持子域通配符 `*.` 与端口约束的任意组合（支持精准端口、通配端口 `:*`、免端口匹配）。
  - 严格防御伪造攻击：禁止将裸 IP 地址（如 `192.168.1.1`）误匹配为后缀子域名通配符（如 `*.168.1.1`）。
- **服务端四级配置与环境变量一致性增强**：
  - 配置文件（`server.yaml` / `server.json`）新增 `allow_all_origins` 支持，环境变量新增支持 `ALLOW_ALL_ORIGINS=true`。
  - 纠正启动横幅对 Allowed Origins 严格模式与放行模式的展示优先级。
- **前端门户跨域诊断与同源引导优化**：
  - 在前端信令 WebSocket 发生跨域被拒时，提供精准的智能诊断指引与快速同源回退提示。

### 容器镜像拉取 (Docker Pull)

```bash
docker pull ghcr.io/beingnoability/webrtc-p2p-server:v2.3.1
docker pull ghcr.io/beingnoability/webrtc-p2p-agent:v2.3.1
```
