# Release Checklist

Complete every step below in order before tagging a release. The root `VERSION` file is the single source of truth. Never force-overwrite a published tag; increment pre-releases sequentially (`-alpha.N`, `-beta.N`, `-rc.N`).

---

## 1. Version Bump File List

Update all of the following files together in a dedicated `release: vX.Y.Z` commit on `edge`:

- `VERSION` (single source of truth, without leading `v`)
- `web/package.json`, `web/package-lock.json`, `docs/package.json`, `docs/package-lock.json`
- Default `var Version = "vX.Y.Z"` in `cmd/agent/main.go` and `cmd/signal/main.go`
- Version and image references in `docker-compose.yml`, `README.md`, `README_en.md`, `docs/guide/server-use-cases.md`, and `docs/en/guide/server-use-cases.md`
- `CHANGELOG.md` (add `## [vX.Y.Z] - YYYY-MM-DD` above prior entries) and `docs/RELEASE_NOTES.md` (single current release section plus `docker pull` commands)

```bash
# Verify VERSION, manifests, changelog, and requested tag align with zero tracked web/dist or secrets
node scripts/verify-release-hygiene.mjs --check-tag "v$(cat VERSION)"
node --test test/hygiene/release_hygiene.test.mjs
```

---

## 2. Full Machine Gate and Security Audit

```bash
# Run the complete local gate including real Chrome CDP E2E against embedded web.DistFS
scripts/gate.sh --full

# Verify zero known vulnerabilities in production web dependencies and Go modules
npm --prefix web audit --omit=dev
govulncheck ./...
```

---

## 3. Branch Merge, Tagging, and Post-Release Verification

```bash
# 1. Merge verified edge commits into dev and main, then tag main
git checkout dev && git merge --ff-only edge
git checkout main && git merge --no-ff dev -m "Merge branch 'dev' for v$(cat VERSION)"
git tag -a "v$(cat VERSION)" -m "Release v$(cat VERSION)"

# 2. Push branches and release tag to trigger GitHub Release and Docker workflows, then return to edge
git push origin edge dev main
git push origin "v$(cat VERSION)"
git checkout edge
```

- Confirm GitHub Release binaries and `SHA256SUMS.txt` match the release tag;
- Confirm multi-arch container images (`linux/amd64`, `linux/arm64`) include SBOM and provenance attestations;
- Confirm pre-release tags (`-alpha` / `-beta` / `-rc`) publish only to pre-release and channel tags (such as `:alpha`) without updating `:latest`.
