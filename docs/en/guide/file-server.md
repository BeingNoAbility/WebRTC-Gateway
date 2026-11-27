# Read-Only Static Directory and File Adapter (`-dir` / `-file`)

`-dir` and `-file` plug a minimal Go `os.Root` read-only `http.Handler` (`pkg/proxy/static_fs.go`) behind `HTTPProxy` via an in-process `RoundTripper`, sharing the exact same P2PMux V7 framing, flow control, compression, and capability-scope enforcement as HTTP reverse proxying.

---

## 1. Quickstart

`-target`, `-dir`, and `-file` are mutually exclusive; specify exactly one at startup:

```bash
# Serve a local directory read-only (auto-serves index.html or bounded directory listing)
ACCESS_SECRET=change-me ./bin/agent \
  -id my-files \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret \
  -dir /srv/share

# Share a single local file mounted at /
ACCESS_SECRET=change-me ./bin/agent \
  -id release-pkg \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret \
  -file /srv/share/archive.tar.gz
```

---

## 2. Security Boundaries and Fail-Closed Defaults

- **Kernel-Level Root Confinement & Stepwise `os.Root.OpenRoot`**: All filesystem access traverses directories stepwise via Go 1.24+ `os.Root` handles, blocking `../` traversal, intermediate-directory symlink swap TOCTOU, and cross-root symlink escapes;
- **Non-Blocking No-Follow Open (`O_RDONLY | O_NONBLOCK | O_NOFOLLOW`)**: Eliminates `Stat`-to-`Open` FIFO hangs and final-component symlink swap races, rejecting non-regular files with `403 Forbidden`;
- **Dotfile and Multi-Hop Dotfile-Symlink Protection (`-hide-dotfiles`, default `true`)**: Blocks dot-prefixed segments and walks every component (including multi-hop symlinks) to reject in-root symlinks pointing to dotfiles (such as `readme -> .env`);
- **Default Sandbox CSP for All Served Files (`-file-inline-active`, default `false`)**: Serves **all** static files (including extensionless files) with `Content-Security-Policy: sandbox; default-src 'none'; ...` (and `sandbox allow-downloads` on generated directory listings), which the Service Worker preserves verbatim without injecting scripts; enable `-file-inline-active` only for trusted static web apps;
- **Canonical Directory Handling**: Issues trailing-slash `301` redirects before checking `index.html`, reads directory entries in bounded batches (`ReadDir(512)`), escapes `%`/`#`/`?` via `url.URL.EscapedPath()`, and prefixes listing links with `./` to block `:` scheme confusion;
- **Read-Only and Capability Scoping**: Accepts `GET`/`HEAD` only and composes cleanly with `-mint-link` and `-mint-path /subdir` for time-limited subdirectory sharing.
