#!/usr/bin/env bash
# Local machine gate: mirrors .github/workflows/ci.yml (job "test-and-build").
# Single source of truth for the pre-PR command list referenced by AGENTS.md.
#
# Usage:
#   scripts/gate.sh          # fast gate (everything except Chrome CDP E2E)
#   scripts/gate.sh --full   # also runs unified Chrome CDP browser E2E suite (embedded DistFS)
set -euo pipefail

cd "$(dirname "$0")/.."

FULL=0
if [ "${1:-}" = "--full" ]; then
  FULL=1
fi

step() { printf '\n==> %s\n' "$*"; }

step "Release hygiene & version parity"
node scripts/verify-release-hygiene.mjs
node --test test/hygiene/release_hygiene.test.mjs

step "Web frontend typecheck & build"
if [ ! -d web/node_modules ]; then
  npm --prefix web ci
fi
npm --prefix web run build
if [ -n "$(git ls-files web/dist)" ]; then
  echo "ERROR: web/dist must not be tracked in Git (build-time embedding rule)"
  exit 1
fi

step "Web unit tests & Go Signaling Conformance"
npm --prefix web test
node --test test/conformance/signaling_conformance.test.mjs

step "Docs lint & build"
if [ ! -d docs/node_modules ]; then
  npm --prefix docs ci
fi
npm --prefix docs run docs:lint
npm --prefix docs run docs:build

step "Go formatting & static analysis"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "Files not gofmt formatted:"
  echo "$unformatted"
  exit 1
fi
go vet ./...
if command -v staticcheck >/dev/null 2>&1; then
  staticcheck ./...
elif [ -x "$(go env GOPATH)/bin/staticcheck" ]; then
  "$(go env GOPATH)/bin/staticcheck" ./...
fi
if command -v gosec >/dev/null 2>&1; then
  gosec ./...
elif [ -x "$(go env GOPATH)/bin/gosec" ]; then
  "$(go env GOPATH)/bin/gosec" ./...
fi

step "Go unit & integration tests (-race -short)"
go test -race -short ./...

step "Go fuzz smoke tests"
for pkg in ./pkg/protocol ./pkg/proxy; do
  fuzz_tests=$(go test -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true)
  for ft in $fuzz_tests; do
    go test -run='^$' -fuzz="^${ft}\$" -fuzztime=2s "$pkg"
  done
done

step "Benchmarks compile"
go test -c -o /dev/null ./test

step "Build Agent & Embedded Signal binaries (make build)"
make build

step "Verify embedded web.DistFS asset completeness"
embedded_files=$(go list -f '{{range .EmbedFiles}}{{println .}}{{end}}' -tags embedweb ./web)
if [ ! -f web/dist/__p2p_gw__/manifest.json ]; then
  echo "ERROR: web/dist/__p2p_gw__/manifest.json is missing"
  exit 1
fi
node -e '
  const fs = require("node:fs");
  const manifest = JSON.parse(fs.readFileSync("web/dist/__p2p_gw__/manifest.json", "utf8"));
  const embedded = new Set(process.argv[1].trim().split(/\r?\n/).filter(Boolean));
  const required = ["dist/index.html", "dist/favicon.svg", "dist/__p2p_gw__/bridge.html", "dist/__p2p_gw__/manifest.json"];
  for (const rel of Object.values(manifest)) {
    if (typeof rel === "string") required.push("dist/" + rel.replace(/^\/+/, ""));
  }
  const missing = required.filter((f) => !embedded.has(f));
  if (missing.length > 0) {
    console.error("ERROR: missing embedded assets in web.DistFS:", missing.join(", "));
    process.exit(1);
  }
' "$embedded_files"

if [ "$FULL" -eq 1 ]; then
  step "Unified Chrome CDP browser E2E suite (embedded web.DistFS)"
  SIGNAL_BIN="$PWD/bin/signal" AGENT_BIN="$PWD/bin/agent" node --test test/e2e/browser_suite.test.mjs
fi

printf '\nAll gate checks passed.\n'
