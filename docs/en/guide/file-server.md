# Read-Only Static Directory and File Adapter (`-dir` / `-file`)

`-dir` and `-file` plug a minimal Go `os.Root` read-only `http.Handler` (`pkg/proxy/static_fs.go`) behind `HTTPProxy` via an in-process `RoundTripper`, sharing the exact same P2PMux V8 framing, flow control, compression, and capability-scope enforcement as HTTP reverse proxying.

---

## 1. Quickstart

`-target`, `-dir`, and `-file` are mutually exclusive; specify exactly one at startup:

```bash
# Serve a local directory read-only (auto-serves index.html or bounded directory listing)
ACCESS_SECRET=change-me ./bin/agent \
  -id my-files \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret-16bytes \
  -dir /srv/share

# Share a single local file mounted at /
ACCESS_SECRET=change-me ./bin/agent \
  -id release-pkg \
  -signal wss://portal.example.com/ws \
  -signal-secret cluster-secret-16bytes \
  -file /srv/share/archive.tar.gz
```

---

## 2. Security Boundaries and Fail-Closed Defaults

- **Kernel-Level Root Confinement & Stepwise `os.Root.OpenRoot`**: All filesystem access traverses directories stepwise via Go 1.24+ `os.Root` handles, blocking `../` traversal, intermediate-directory symlink swap TOCTOU, and cross-root symlink escapes;
- **Default Fail-Closed `O_NOFOLLOW` Rejecting All Symlinks (`-follow-symlinks`, default `false`)**: Directory and file opens default to `O_RDONLY | O_NONBLOCK | O_NOFOLLOW`, eliminating both FIFO hangs and symlink traversal (non-regular files return `403 Forbidden`, symlinks return `404 Not Found`); opt-in `-follow-symlinks` bounds symlink hops and strictly confines canonical targets within both the root directory and the active capability's `path_prefix`;
- **Dotfile Protection (`-hide-dotfiles`, default `true`)**: Blocks any dot-prefixed path segment (and when `-follow-symlinks` is enabled, also rejects single- or multi-hop symlinks resolving to dotfiles);
- **Default Sandbox CSP for All Served Files (`-file-inline-active`, default `false`)**: Serves **all** static files (including extensionless files) and generated directory listings with `Content-Security-Policy: sandbox allow-same-origin allow-downloads; default-src 'none'; script-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; ...` (strictly blocking scripts, forms, popups, and top-level escapes while preserving the sandbox origin so Service Worker `clientId` binding stays unified with `-target` mode without any bypass exception), which the Service Worker preserves verbatim without injecting scripts; enable `-file-inline-active` only for trusted static web apps;
- **Canonical Directory Handling**: Issues trailing-slash `301` redirects before checking `index.html`, reads directory entries in bounded batches (`ReadDir(512)`), escapes `%`/`#`/`?` via `url.URL.EscapedPath()`, and prefixes listing links with `./` to block `:` scheme confusion;
- **Read-Only and Capability Scoping**: Accepts `GET`/`HEAD` only and composes cleanly with `-mint-link` and `-mint-path /subdir` for time-limited subdirectory sharing.
