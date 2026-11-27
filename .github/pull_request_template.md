## PR 概述 (Overview)
<!-- 简要说明此 PR 要解决的问题、动机与受影响的边界 / Briefly describe the problem, motivation, and affected boundaries -->

### PR 类型 (Type of Change)
- [ ] Bug 修复 (Bug Fix)
- [ ] 新特性 / 通用能力 (Feature)
- [ ] 安全 / 协议加固 (Security / Protocol Hardening)
- [ ] 性能优化 (Performance)
- [ ] 文档 (Documentation)
- [ ] 构建 / CI (Build / CI)
- [ ] 架构演进 / 大版本重构 (Architectural Redesign)：已在 `docs/rfcs/` 提交并获批 RFC，链接：<!-- RFC link -->

---

## 第一性审查自检 (First-Principles Self-Check)
> 涉及核心传输、并发状态机、密码学、代理边界或浏览器沙箱的改动，请对照 [核心原则 (CONSTITUTION)](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/docs/CONSTITUTION.md)、[审查清单 (REVIEW_GUIDELINES)](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/docs/REVIEW_GUIDELINES.md) 与 [设计边界与架构权衡 (DESIGN_TRADEOFFS)](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/docs/DESIGN_TRADEOFFS.md) 核对：

- [ ] **核心原则与应用中立**：严守零数据中继与诚实端到端边界、零客户端、透明管道定位；不含针对特定应用的硬编码，改动尺度符合日常修复或获批 RFC。
- [ ] **信任边界与 Fail-Closed 不变量**：未将不可信上下文中的脚本补丁当作安全边界，未放宽 [CONSTITUTION 第三节](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/docs/CONSTITUTION.md) 的任何安全不变量；新增兼容或降级开关默认关闭并已文档化边界。
- [ ] **资源边界与并发生命周期**：对不可信对端可控的内存、CPU、FD、队列与状态基数均有明确上限与清理路径；无跨阻塞 I/O 持锁，异常断连与停机可在有界时间内收敛；严禁对非幂等请求盲目重试。
- [ ] **规范一致、配置与双语文档同步**：正确处理 HTTP/WS/浏览器合成执行边界（空载荷、编码、流式、跨引擎）；涉及协议、配置、错误语义或文档的改动已同步更新双端运行时、`configs/*.example.yaml` 与中英文镜像。
- [ ] **隐私与制品洁净**：示例与测试仅使用保留域名与文档 IP；无凭据、`file://` 本地链接或 `web/dist` 构建产物进入提交。

---

## AI 辅助开发声明 (AI-Assisted Changes)
<!-- 若本 PR 涉及核心逻辑的 AI 辅助编写，请勾选并遵守 AI_DEVELOPMENT_GUIDE.md；纯人工或简单文档修改可跳过 -->
- [ ] **纯人工编写** 或 **低风险文档/小改动**（跳过下方子项）
- [ ] **包含 AI 辅助编写的核心逻辑**：已遵守 [AI 辅助开发指南](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/docs/AI_DEVELOPMENT_GUIDE.md)——完成独立上下文对抗审查、为成立风险补充驱动真实模块的确定性负向测试，且提交者已逐行审阅全部 diff 并承担最终责任。

---

## 验证与门禁 (Verification)
- [ ] 本地已通过 [`scripts/gate.sh`](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/scripts/gate.sh)
- [ ] 涉及协议 / 信令 / 沙箱 / Service Worker 的改动，已通过 `scripts/gate.sh --full`（含无头 Chrome CDP E2E）
- [ ] 边界 / 并发 / 资源 / 安全改动已附确定性负向测试，并验证其在未修复代码上失败、修复后通过：<!-- 简述测试名称或验证结果 -->
