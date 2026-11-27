# 发布验收清单 (Release Checklist)

每次打 Tag 发布前按顺序完成下列检查，单一版本源为仓库根目录的 `VERSION` 文件。已发布标签不可覆写；预发布迭代按 `-alpha.N` / `-beta.N` / `-rc.N` 顺次递增。

---

## 1. 版本号同步清单

升级版本时同步更新以下文件（均在 `edge` 分支提交 `release: vX.Y.Z`）：

- `VERSION`（单一事实源，不含前导 `v`）
- `web/package.json`、`web/package-lock.json`、`docs/package.json`、`docs/package-lock.json`
- `cmd/agent/main.go`、`cmd/signal/main.go` 中的默认 `var Version = "vX.Y.Z"`
- `docker-compose.yml`、`README.md`、`README_en.md`、`docs/guide/server-use-cases.md` 与 `docs/en/guide/server-use-cases.md` 中的镜像与版本标签
- `CHANGELOG.md`（在顶部追加 `## [vX.Y.Z] - YYYY-MM-DD`，保留历史条目）与 `docs/RELEASE_NOTES.md`（仅保留当前单一版本与 `docker pull` 命令）

```bash
# 确认 VERSION、包清单、日志与目标 tag 完全一致，且零追踪 web/dist 或私密凭据
node scripts/verify-release-hygiene.mjs --check-tag "v$(cat VERSION)"
node --test test/hygiene/release_hygiene.test.mjs
```

---

## 2. 全量门禁与安全扫描

```bash
# 运行完整本地门禁（含内嵌 web.DistFS 的真实 Chrome CDP E2E）
scripts/gate.sh --full

# 验证生产前端依赖与 Go 模块零已知漏洞
npm --prefix web audit --omit=dev
govulncheck ./...
```

---

## 3. 分支合并、打标与发布后核验

```bash
# 1. 将已通过门禁的 edge 合并至 dev 与 main，在 main 创建注释标签
git checkout dev && git merge --ff-only edge
git checkout main && git merge --no-ff dev -m "Merge branch 'dev' for v$(cat VERSION)"
git tag -a "v$(cat VERSION)" -m "Release v$(cat VERSION)"

# 2. 推送分支与标签触发 GitHub Release 与多架构容器构建，随后切回 edge
git push origin edge dev main
git push origin "v$(cat VERSION)"
git checkout edge
```

- 确认 GitHub Release 二进制校验和（`SHA256SUMS.txt`）与 tag 版本一致；
- 确认多架构容器镜像（`linux/amd64`、`linux/arm64`）附带 SBOM 与 Provenance 证明；
- 预发布 tag（含 `-alpha` / `-beta` / `-rc`）仅发布至对应预发布标签与通道标签（如 `:alpha`），不覆盖 `:latest`。
