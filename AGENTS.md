# AGENTS.md

Entry point for AI coding agents working in this repository (human contributors: see [CONTRIBUTING.md](CONTRIBUTING.md)). Keep this file short and timeless; normative invariants and current implementation specs live in the linked documents.

**WebRTC Gateway** is a zero-client P2P intranet gateway: visitors use a standard browser to reach intranet HTTP / WebSocket services through end-to-end encrypted WebRTC DataChannels and a cross-site Service Worker sandbox, while the signaling server only brokers connections.

> 中文要点：先读 `docs/CONSTITUTION.md`、`docs/REVIEW_GUIDELINES.md` 与 `docs/DESIGN_TRADEOFFS.md`；提交前运行 `scripts/gate.sh`；严守零数据中继与诚实端到端边界、零客户端、应用中立与全部 Fail-Closed 不变量；不得提交私密数据或 `web/dist`；改动中文文档必须同步更新英文镜像。

## Repository map

| Path | Responsibility |
| :--- | :--- |
| `VERSION`, `Makefile` | Single source of truth for release version and build/test entry points |
| `cmd/agent`, `cmd/signal` | Intranet Agent and authoritative Go Signaling Server entry points and CLI wiring |
| `pkg/config`, `pkg/state` | Strict schema-validated config/secret loading and single-writer atomic state persistence |
| `pkg/protocol`, `pkg/mux` | Binary wire framing/TLV codec, ordered HTTP channel pool, streaming WebSocket channels, and bounded flow control |
| `pkg/agent`, `pkg/proxy`, `pkg/signaling` | Endpoint identity, in-DTLS mutual auth, structured SDP synthesis, single-target HTTP/WS/static proxy & cookie isolation, and signaling broker |
| `web/src` | Browser Portal, hidden sandbox bridge, Service Worker fetch/HTML/decompression pipeline, compatibility patch, and TS protocol/crypto modules (built into gitignored `web/dist`, embedded via `-tags embedweb`) |
| `test/` | Go E2E/benchmarks, `node --test` web & hygiene suites, black-box signaling conformance, and Chrome CDP browser E2E |
| `docs/` (+ `docs/en/`) | Bilingual VitePress documentation site (Chinese with strict English mirror) |
| `scripts/` | Local machine gate (`gate.sh`), release hygiene verifier, and git hooks |

## Document layering and precedence

`docs/CONSTITUTION.md` (core principles & Fail-Closed invariants) > `docs/REVIEW_GUIDELINES.md` (first-principles review framework) > `docs/DESIGN_TRADEOFFS.md` (permanent non-goals, revisitable architectural trade-offs & canonical failure modes). Current wire constants, crypto parameters, and exact defaults live in `docs/guide/`.

## Read before changing code

- [Constitution](docs/CONSTITUTION.md): positioning, three core principles, change scope, Fail-Closed security invariants
- [Review Guidelines](docs/REVIEW_GUIDELINES.md): six first-principles review dimensions and sign-off matrix
- [Design Boundaries & Trade-offs](docs/DESIGN_TRADEOFFS.md): permanent non-goals, revisitable current trade-offs (with RFC criteria), and canonical cross-layer failure modes
- [Current Implementation Specs](docs/guide/index.md): authoritative `protocol-specification.md`, `security.md`, `architecture.md`, and `agent-configuration.md`
- [Style Guide](docs/STYLE_GUIDE.md): required reading before any documentation change

## Machine gate

Run `scripts/gate.sh` before opening a PR (mirrors `.github/workflows/ci.yml`: release hygiene, web build & `node --test`, signaling conformance, bilingual doc lint & VitePress build, Go static checks & `go test -race -short`, fuzz smoke, and `make build`). Run `scripts/gate.sh --full` for protocol, signaling, or sandbox changes to include headless Chrome CDP E2E against embedded binaries. Enable git hooks via `git config core.hooksPath scripts/git-hooks`.

## Hard rules (MUST / MUST NOT)

1. **Zero data relay & honest E2E**: signaling MUST NOT relay, buffer, or inspect application data, and MUST NOT carry offline-crackable password verifiers or data-plane session handles. User-run TURN forwarding opaque encrypted ciphertext is the only allowed relay fallback.
2. **Zero client**: visitors MUST NOT need native clients, browser extensions, custom root CAs, or experimental flags; use standard Web Platform APIs only.
3. **Pure pipe & application-agnostic**: MUST NOT hardcode behavior for specific applications, paths, or proprietary headers, and MUST NOT bundle business application UIs or in-process app plugins. Fix gaps at the RFC / W3C layer or compose downstream via standard HTTP/WS boundaries.
4. **Fail-Closed security invariants**: MUST NOT relax any Fail-Closed invariant in `docs/CONSTITUTION.md` §3 (exact current defaults live in `docs/guide/agent-configuration.md` and `docs/guide/security.md`). Any downgrade or compatibility flag MUST default to off and document its boundary.
5. **Change scope**: routine fixes touch only directly related code; breaking architectural redesigns require an approved RFC in `docs/rfcs/` and coordinated cross-runtime migration.
6. **No private data or build outputs**: MUST NOT commit real domains, private IPs, node IDs, `.env`, private keys, `file://` links, or `web/dist/`. Use `example.com`, `*.example`, `192.0.2.x`, `198.51.100.x`.
7. **Deterministic negative tests**: fixes for boundary, concurrency, resource, or security issues MUST include deterministic negative tests against real modules that fail on unfixed code.
8. **Cross-runtime & conformance parity**: wire protocol changes MUST update Go, TypeScript, and golden vectors together; signaling changes MUST pass the black-box conformance suite.

## Documentation rules

- Chinese and English MUST stay in sync: editing `docs/X.md` requires the matching edit in `docs/en/X.md` (`README.md` ↔ `README_en.md`) with identical heading, code-block, and Mermaid counts (enforced fatally by `docs:lint`).
- Keep governance docs timeless and free of duplicated implementation trivia; put exact constants and flags only in their single source of truth under `docs/guide/`.
- Follow `docs/STYLE_GUIDE.md` and run `npm --prefix docs run docs:lint && npm --prefix docs run docs:build` after doc changes.
