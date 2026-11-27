# 测试策略与验证清单 (Testing Strategy & Checklist)

提交 PR 或发版前，应通过统一本地门禁脚本 [`scripts/gate.sh`](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/scripts/gate.sh)（与 `.github/workflows/ci.yml` 对齐）。本文定义仓库的五层测试金字塔、负向测试纪律以及 CI 无法完全替代的真机与生产环境验证面；具体测试函数清单直接以代码为准（可通过 `go test -list . ./...` 检索），不在文档中手工抄录以免漂移。

---

## 一、本地门禁与定向命令

```bash
# 快速门禁（发版卫生、Web 构建与单测、信令黑盒一致性、文档 lint/构建、静态分析与 Go -race、内嵌构建）
scripts/gate.sh

# 完整门禁（额外运行基于内嵌 web.DistFS 的无头 Chrome CDP 端到端套件）
scripts/gate.sh --full
```

定向运行与检索单套件：

```bash
# 查看当前全部 Go 测试用例名称（单一事实源）
go test -list . ./...

# Go 全量并发竞态测试与模糊冒烟测试
go test -race -count=1 ./...
go test -run=^$ -fuzz=. -fuzztime=5s ./pkg/protocol ./pkg/proxy

# 前端真实 TS 模块测试、信令黑盒一致性、发版卫生与无头 Chrome CDP 端到端
npm --prefix web run build && npm --prefix web test
node --test test/conformance/signaling_conformance.test.mjs
node --test test/hygiene/release_hygiene.test.mjs
SIGNAL_BIN="$PWD/bin/signal" AGENT_BIN="$PWD/bin/agent" node --test test/e2e/browser_suite.test.mjs
```

---

## 二、五层测试金字塔与落点指引

新增或修复能力时，按改动触及的边界选择对应验证层级（不把低层边界推给慢速 E2E，也不以纯单测替代真实浏览器语义验证）：

| 层级 | 位置与入口 | 核心验证职责 | 适用改动场景 |
| :--- | :--- | :--- | :--- |
| **1. 单元与并发竞态** | `pkg/*/*_test.go`、`test/web/*.test.mjs` | 状态机不变量、边界输入拦截、锁层级与无锁取消、多层内存预算归还、Cookie / 策略 / 认证 / HTML 分词逻辑。 | 全部业务逻辑、协议与状态机改动（必选）。 |
| **2. 黄金向量与模糊测试** | `test/fixtures/`、`pkg/*/fuzz_test.go`、`test/web/html_injector_fuzz.test.mjs` | Go 与 TypeScript 线协议与密码原语逐字节一致、随机分片与畸形输入下零 panic 与零解析差分。 | 编解码器、TLV、KDF、SDP 与流式解析器改动。 |
| **3. 黑盒协议一致性** | `test/conformance/` | 针对编译产物验证信令启动 Fail-Closed、单次路径规范化、Host/Origin/CSP 隔离、限流抗洪水与跨重启持久化。 | `cmd/signal`、`pkg/signaling` 与 HTTP/WS 路由安全策略改动。 |
| **4. 端到端与实机浏览器 CDP** | `test/e2e_test.go`、`test/e2e/browser_suite.test.mjs` | 针对内嵌 `web.DistFS` 二进制运行真实 WebRTC/DTLS/认证链路、跨站 SW 抗 CSRF、流式 WS、内存 CookieMirror、能力实时吊销与 TOFU 阻断。 | 协议、传输、沙箱、Service Worker、Portal 与内嵌构建改动。 |
| **5. 静态分析、文档与供应链** | `scripts/gate.sh`、`govulncheck`、`npm audit` | 格式、`go vet`、`staticcheck`、`gosec`、已知漏洞扫描、版本一致、零私密数据/构建产物追踪与双语文档结构校验。 | 全部 PR 与发版前验证。 |

---

## 三、确定性负向测试纪律

涉及安全、边界校验、并发或资源流控的修复，必须满足以下测试纪律：

- **先红后绿证据**：负向测试必须针对真实故障或攻击路径编写，并在提交说明中验证其“未修复代码上稳定失败、修复后稳定通过”。
- **驱动真实生产模块**：前端测试通过构建加载器直接执行 `web/src/` 真实模块，后端测试直接调用生产包实现；严禁在测试文件内复制简化版镜像函数或正则占位。
- **零脆弱定时同步**：使用通道、屏障、可注入时钟/上下文或有界条件轮询替代无约束 `time.Sleep`，避免在慢速 CI 机器上偶发抖动。
- **合成协议向量**：构造最小 RFC / W3C 合成请求与响应复现问题，严禁以特定第三方应用页面或私有路径作为测试夹具。
- **验证清理与归零不变量**：连接关闭、提前响应或错误退场测试必须显式断言内存预算归零、复用槽位可再次分配且无协程泄漏。

---

## 四、自动化门禁之外的真机与环境验证

无头 Linux Chrome CI 无法覆盖全部真实浏览器引擎与公网拓扑。涉及网络传输、Service Worker、沙箱隔离或发版前（配合 [发版验收清单](./RELEASE_CHECKLIST.md)），应当按需补充以下真机手工核验：

- **Safari / WebKit（macOS 与 iOS）**：验证存储分区、后台标签页暂停后的静默重连、SW 休眠唤醒与媒体 206 拖拽。
- **Firefox（桌面端）**：验证 DTLS/SCTP 互操作、Service Worker Fetch 拦截与跨站沙箱导航。
- **真实弱网与对称 NAT**：验证移动网络与 Wi-Fi 切换后的恢复表现，以及经用户自建 TURN 中继加密转发的连通性。
- **生产反代与真实 TLS 证书**：在 Nginx / Caddy 前置反代下验证 Tier 1 固定双域、Tier 2 同站通配符与 Tier 3 双域通配符的 WebSocket 升级、客户端 IP 透传与证书热重载。
