# Testing Strategy and Checklist

Before opening a PR or cutting a release, run the unified local machine gate [`scripts/gate.sh`](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/scripts/gate.sh) (which mirrors `.github/workflows/ci.yml`). This page defines the five-layer verification pyramid, deterministic negative-testing rules, and real-environment checks beyond CI. Live Go test inventories come directly from source via `go test -list . ./...` rather than a manually duplicated list.

---

## 1. Machine Gate and Targeted Commands

```bash
# Fast gate (hygiene, web build & unit tests, signaling conformance, doc lint/build, static analysis, Go -race, embedded build)
scripts/gate.sh

# Full gate (adds headless Chrome CDP E2E against binaries embedding web.DistFS)
scripts/gate.sh --full
```

Targeted suite runs and inventory lookup:

```bash
# Inspect live Go test inventory directly from source
go test -list . ./...

# Go race suite (with embedded web) and per-target fuzz smoke tests
go test -race -count=1 -tags embedweb ./...
for pkg in ./pkg/protocol ./pkg/proxy ./pkg/signaling; do
  for ft in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz'); do
    go test -run='^$' -fuzz="^${ft}\$" -fuzztime=2s "$pkg"
  done
done

# Real TS module tests, black-box signaling conformance, release hygiene, and Chrome CDP E2E
npm --prefix web run build && npm --prefix web test
node --test test/conformance/signaling_conformance.test.mjs
node --test test/hygiene/release_hygiene.test.mjs
SIGNAL_BIN="$PWD/bin/signal" AGENT_BIN="$PWD/bin/agent" node --test test/e2e/browser_suite.test.mjs
```

---

## 2. Five-Layer Verification Pyramid

Place new tests at the lowest layer that faithfully reproduces the boundary under test — never push unit invariants solely into slow E2E suites, and never rely on unit tests alone for real browser semantics:

| Layer | Location & Entry Point | Core Responsibility | When Required |
| :--- | :--- | :--- | :--- |
| **1. Unit & Concurrency Race** | `pkg/*/*_test.go`, `test/web/*.test.mjs` | State-machine invariants, boundary validation, lock-free cancellation, multi-tier budget reclamation, cookie/policy/auth/HTML tokenizer logic. | All logic, protocol, and state-machine changes (mandatory). |
| **2. Golden Vectors & Fuzzing** | `test/fixtures/`, `pkg/*/fuzz_test.go`, `test/web/html_injector_fuzz.test.mjs` | Byte-for-byte Go/TS wire & crypto parity; panic-free, differential-free resilience under random chunking and malformed inputs. | Codec, TLV, KDF, SDP, and streaming parser changes. |
| **3. Black-Box Conformance** | `test/conformance/` | Verify compiled signaling binaries for Fail-Closed startup, single-pass path canonicalization, Host/Origin/CSP isolation, flood protection, and restart persistence. | Changes to `cmd/signal`, `pkg/signaling`, or HTTP/WS route security. |
| **4. End-to-End & Real Browser CDP** | `test/e2e_test.go`, `test/e2e/browser_suite.test.mjs` | Exercise embedded `web.DistFS` binaries across real WebRTC/DTLS/auth, cross-site SW anti-CSRF, streaming WS, in-memory CookieMirror, live revocation, and TOFU blocking. | Protocol, transport, sandbox, Service Worker, Portal, or asset-embedding changes. |
| **5. Static Analysis, Docs & Supply Chain** | `scripts/gate.sh`, `govulncheck`, `npm audit` | Formatting, `go vet`, `staticcheck`, `gosec`, vulnerability scans, version parity, zero leaked secrets/artifacts, and bilingual doc integrity. | Every PR and release. |

---

## 3. Deterministic Negative Testing Discipline

Fixes touching security, validation boundaries, concurrency, or flow control MUST follow these rules:

- **Prove red before green**: Negative tests MUST model the real fault or attack path and demonstrate deterministic failure on unfixed code and consistent passes after the fix.
- **Exercise real production modules**: Frontend tests load real `web/src/` modules directly; backend tests exercise real packages — never duplicate simplified mirror functions or regex stubs inside tests.
- **Eliminate flaky timing**: Use channels, barriers, injected clocks/contexts, or bounded condition waits instead of unconstrained `time.Sleep`.
- **Use synthetic protocol vectors**: Reproduce issues with minimal RFC / W3C synthetic requests and responses rather than app-specific fixtures.
- **Assert cleanup and zero-leak invariants**: Close, early-response, and error-path tests MUST assert that memory budgets return to zero, reusable slots recover cleanly, and goroutines exit.

---

## 4. Real-Environment Verification Beyond CI

Headless Linux Chrome CI cannot capture every browser engine or real-world network topology. Before releases (alongside [Release Checklist](./RELEASE_CHECKLIST.md)) or when changing transport, Service Worker, or sandbox behavior, validate on real environments as applicable:

- **Safari / WebKit (macOS and iOS)**: Storage partitioning, background tab suspension and silent reconnect, SW wake-up, and `206 Range` media seeking.
- **Firefox (Desktop)**: DTLS/SCTP interoperability, Service Worker Fetch interception, and cross-site sandbox navigation.
- **Mobile handoffs and symmetric NAT**: Cellular ↔ Wi-Fi transitions and end-to-end encrypted traversal through a self-hosted TURN server.
- **Production reverse proxies and real TLS**: Tier 1 cross-site wildcard and Tier 2 restricted modes (fixed cross-site host / same-site wildcard) behind Nginx or Caddy, covering WebSocket upgrades, trusted client-IP propagation, and certificate hot-reload.
