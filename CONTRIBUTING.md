# Contributing to WebRTC Gateway

Welcome! This guide covers local development setup, test workflows, governance structure, and contribution paths for human contributors. AI coding agents should start at [AGENTS.md](AGENTS.md).

> 中文速览：请先阅读 [核心原则 (CONSTITUTION)](docs/CONSTITUTION.md)、[审查清单 (REVIEW_GUIDELINES)](docs/REVIEW_GUIDELINES.md) 与 [设计边界与架构权衡 (DESIGN_TRADEOFFS)](docs/DESIGN_TRADEOFFS.md)。提交 PR 前在本地运行 `scripts/gate.sh`（涉及协议、信令或沙箱时运行 `scripts/gate.sh --full`）。

---

## 1. Prerequisites and Local Setup

| Toolchain | Version | Used For |
| :--- | :--- | :--- |
| **Go** | `1.26+` | `cmd/agent`, `cmd/signal`, `pkg/*`, Go unit/race/fuzz/E2E tests |
| **Node.js** | `24 LTS+` | Web TypeScript build (`esbuild`), native `node --test`, conformance, docs (`VitePress`) |
| **Chrome / Chromium / Edge** | Current stable | Optional locally; required for `scripts/gate.sh --full` CDP browser E2E |

Install dependencies and enable repository git hooks once after cloning:

```bash
npm --prefix web ci
npm --prefix docs ci
git config core.hooksPath scripts/git-hooks
```

---

## 2. Common Development Commands

All primary build and verification entry points are defined in [Makefile](Makefile) and [scripts/gate.sh](scripts/gate.sh):

```bash
# Build frontend (web/dist) and Go binaries (bin/agent, bin/signal with -tags embedweb)
make build

# Run fast local machine gate (mirrors CI: hygiene, web build/tests, conformance, docs, static checks, go test -race, build)
scripts/gate.sh

# Run full machine gate including headless Chrome CDP E2E against embedded bin/signal
scripts/gate.sh --full
```

Targeted workflows during active development:

```bash
# Inspect live Go test inventory or run a targeted package with race detector
go test -list . ./...
go test -race -v ./pkg/mux

# Run web TypeScript build and unit/vector tests
npm --prefix web run build && npm --prefix web test

# Run black-box Go signaling conformance suite
node --test test/conformance/signaling_conformance.test.mjs

# Run Chrome CDP browser E2E directly against built binaries
SIGNAL_BIN="$PWD/bin/signal" AGENT_BIN="$PWD/bin/agent" node --test test/e2e/browser_suite.test.mjs

# Lint and build bilingual VitePress docs
npm --prefix docs run docs:lint && npm --prefix docs run docs:build
```

---

## 3. Governance and Specification Map (Single Source of Truth)

Documentation is strictly layered so principles stay timeless and implementation specifics live in one authoritative place:

| Layer / Topic | Authoritative Document |
| :--- | :--- |
| **Core principles, change scope & Fail-Closed invariants** | [Constitution](docs/en/CONSTITUTION.md) ([中文](docs/CONSTITUTION.md)) |
| **First-principles PR / RFC review framework & sign-off matrix** | [Review Guidelines](docs/en/REVIEW_GUIDELINES.md) ([中文](docs/REVIEW_GUIDELINES.md)) |
| **Permanent non-goals, revisitable trade-offs & failure modes** | [Design Boundaries & Trade-offs](docs/en/DESIGN_TRADEOFFS.md) ([中文](docs/DESIGN_TRADEOFFS.md)) |
| **Current wire protocol, security & configuration baselines** | [Guide Overview](docs/en/guide/index.md) ([中文](docs/guide/index.md)) |
| **Testing pyramid, negative-testing rules & manual matrix** | [Testing Strategy & Checklist](docs/en/TESTING_CHECKLIST.md) ([中文](docs/TESTING_CHECKLIST.md)) |
| **AI-assisted adversarial review & accountability standards** | [AI Development Guide](docs/en/AI_DEVELOPMENT_GUIDE.md) ([中文](docs/AI_DEVELOPMENT_GUIDE.md)) |
| **Documentation style, layering & bilingual sync** | [Style Guide](docs/en/STYLE_GUIDE.md) ([中文](docs/STYLE_GUIDE.md)) |
| **Tagging & release runbook** | [Release Checklist](docs/en/RELEASE_CHECKLIST.md) ([中文](docs/RELEASE_CHECKLIST.md)) |

---

## 4. Contribution Paths, Branching, and Security Hygiene

- **Direct PR vs Issue / RFC**: Open a PR directly for focused bug fixes, docs improvements, or test hardening. Open an Issue first for new generic flags or behavioral changes. Major redesigns or proposals revisiting a documented architectural trade-off in `DESIGN_TRADEOFFS.md` MUST first land an approved RFC under `docs/rfcs/` (with an English mirror under `docs/en/rfcs/`).
- **Branches**: Active development happens on `edge`, integrates into `dev`, and merges into `main` for releases. Open pull requests against `edge` unless coordinating a hotfix.
- **Cross-runtime & config parity**: Wire protocol changes MUST update Go, TypeScript, and shared golden vectors together; CLI/YAML flag changes MUST update `configs/*.example.yaml` and configuration docs.
- **Mandatory deterministic negative tests**: Boundary, concurrency, resource, or security fixes MUST include deterministic negative tests against real modules that fail without the fix.
- **Bilingual docs parity**: Editing any `docs/<path>.md` requires updating `docs/en/<path>.md` (`README.md` ↔ `README_en.md`) in the same PR with matching structure (`docs:lint` enforces `E7` fatally).
- **Zero private data or build outputs**: Never commit real domains, private IPs, `.env`, private keys, `file://` links, or `web/dist/` (use `example.com`, `*.example`, `192.0.2.x`, `198.51.100.x`).
- **Security disclosures**: Do not open public issues for exploitable vulnerabilities; report privately via GitHub Security Advisories or contact the maintainer directly.
