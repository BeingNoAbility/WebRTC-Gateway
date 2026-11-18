## Pull Request 概述 (PR Overview)
<!-- 请简要描述此 PR 的改动背景、目的及解决的问题 -->
<!-- Please briefly describe the purpose, context, and issue resolved by this PR -->

### PR 类型 (Type of Change)
- [ ] 🐛 Bug 修复 (Bug Fix)
- [ ] ✨ 新特性/功能演进 (Feature)
- [ ] 🛡️ 安全/协议加固 (Security / Protocol Hardening)
- [ ] ⚡ 性能优化 (Performance Optimization)
- [ ] 📝 文档更新 (Documentation)
- [ ] 🔧 构建/CI 流水线调整 (CI / Build System)

---

## 架构审查与范式自检 (Systemic Invariants Check)
> 涉及协议层、并发状态机、沙箱虚拟化或边界处理的改动，必须对照 [docs/REVIEW_GUIDELINES.md](file:///docs/REVIEW_GUIDELINES.md) 进行对抗性思维推演：

- [ ] **是否引入了新架构范式（Paradigm Shift）？**
  *(例如：引入 WebTransport/QUIC、新型 Serverless 边缘环境、多线程 WASM、非标准浏览器 Web API)*
  - [ ] 是：已在本次 PR 中为 `docs/REVIEW_GUIDELINES.md`（及英文版）增补对应的新不变量与防御准则
  - [ ] 否：常规演进，未引入新架构范式

- [ ] **是否攻克了未知盲区的深水区 Bug？**
  *(例如：W3C 规范不对称性截断、罕见网络状态机悬挂、跨内核沙箱配额异常、多字节字符切片乱码)*
  - [ ] 是：已在 `docs/REVIEW_GUIDELINES.md` 的【经典案例库 (Case Studies)】中追加 Post-Mortem 战报复盘
  - [ ] 否：常规修复，无新增范式案例

- [ ] **深水区 5 大根本不变量推演 (Systemic Invariants Verification)**
  - [ ] **2.1 规范不对称性**：已排查空实体（`HEAD`、`204`、`304`、`0-byte`）、提前 Abort 及流式截断异常
  - [ ] **2.2 并发安全与协程泄漏**：锁获取顺序全局一致，派生的 Goroutine 均受 Context/关闭信号约束
  - [ ] **2.3 背压与缓冲区耗尽**：已监听低水位线（`bufferedAmount`），内存会话表受容量限制与 Janitor 清理约束
  - [ ] **2.4 拓扑漂移与瞬态恢复**：已验证网络切换（ICE Restart）自愈，Serverless 唤醒具备完整上下文幂等恢复
  - [ ] **2.5 跨引擎沙箱与协议透明**：兼容 iOS WebKit/Safari 配额，对 SSE、206 Range、WebSocket 保持透明

---

## 自动化机器防线预检 (Machine Gate Self-Check)
在提交 PR 之前，请确认本地已通过以下机器门禁：
- [ ] `npm --prefix web run typecheck` 通过（TypeScript 0 errors）
- [ ] `npm --prefix web run build` 成功，且 `git diff --exit-code web/dist` 干净无差异
- [ ] `go vet ./...` & `go fmt ./...` 检查通过
- [ ] `npm --prefix web test` (65 项沙箱与子域名测试) 全部通过
- [ ] `go test -v ./...` 全量单元与端到端穿透测试全部通过
- [ ] `npm --prefix docs run docs:build` 双语文档静态 SSR 编译通过（零死链警告）
