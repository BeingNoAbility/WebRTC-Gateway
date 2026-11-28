# 静态目录与单文件只读适配器 (`-dir` / `-file`)

`-dir` 与 `-file` 通过基于 Go `os.Root` 的极简只读 `http.Handler`（`pkg/proxy/static_fs.go`）经进程内 `RoundTripper` 直接挂载在 `HTTPProxy` 之后，与常规 HTTP 反代共享同一套 P2PMux V8 协议、流控、压缩与能力令牌权限校验。

---

## 1. 快速启动

`-target`、`-dir`、`-file` 三个模式参数互斥，启动时必须且只能指定其一：

```bash
# 托管静态目录（只读目录列表或自动服务 index.html）
ACCESS_SECRET=change-me ./bin/agent \
  -id my-files \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret-16bytes \
  -dir /srv/share

# 分享单个文件（直接挂载于根路径 /）
ACCESS_SECRET=change-me ./bin/agent \
  -id release-pkg \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret-16bytes \
  -file /srv/share/archive.tar.gz
```

---

## 2. 安全边界与默认 Fail-Closed 策略

- **内核级根禁锢与逐级 `os.Root.OpenRoot` 遍历**：全部文件访问均通过 Go 1.24+ `os.Root` 句柄逐级打开目录，从内核 `openat2` / 句柄层根除 `../`、中间目录软链替换 TOCTOU 与符号链接跨根逃逸；
- **默认 Fail-Closed `O_NOFOLLOW` 拒绝全部符号链接（`-follow-symlinks`，默认 `false`）**：打开文件与目录默认携带 `O_RDONLY | O_NONBLOCK | O_NOFOLLOW`，同时消除 FIFO 命名管道阻塞与一切符号链接跟随（非常规文件立即返回 `403 Forbidden`，符号链接返回 `404 Not Found`）；仅在显式开启 `-follow-symlinks` 时允许有限跳数解析，且解析后的规范路径必须同时位于根目录与当前能力令牌的 `path_prefix` 边界之内；
- **隐藏点文件（`-hide-dotfiles`，默认 `true`）**：拒绝任意以 `.` 开头的路径段（在开启 `-follow-symlinks` 时同样拒绝指向点文件的单跳或多跳软链）；
- **全文件默认沙箱 CSP（`-file-inline-active`，默认 `false`）**：默认对**全部**静态文件（含无扩展名文件）下发 `Content-Security-Policy: sandbox; default-src 'none'; ...`，对生成的目录列表下发 `Content-Security-Policy: sandbox allow-downloads; ...`，且 Service Worker 保留该安全头并跳过脚本注入，防范同源存储型 XSS；仅在托管可信静态站点且需要执行脚本时显式开启 `-file-inline-active`；
- **目录规范与防注入**：先对缺少尾斜杠的目录发送 `301` 跳转再检查 `index.html`；目录列表采用有界分批 `ReadDir(512)` 防止超大目录 OOM；所有文件名经 `url.URL.EscapedPath()` 正确编码 `%`、`#`、`?`，并使用 `./` 相对前缀阻止含 `:` 的文件名触发伪协议注入；
- **只读与能力令牌组合**：静态适配器仅允许 `GET` / `HEAD` 请求，可与 `-mint-link` / `-mint-path /subdir` 组合生成限定子目录的短期分享链接。
