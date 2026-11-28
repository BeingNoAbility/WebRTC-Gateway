# Quickstart Guide (5 Minutes)

This page is for first-time users: run it locally first, then share a file, and finally pick a production deployment tier. Full parameters are in the [Agent and Signaling Configuration Reference](./agent-configuration).

---

## 1. Choose Your Path

| Your goal | Recommended path | Domain and certificate needed | Section |
| :--- | :--- | :--- | :--- |
| Try it out or debug locally | Local `-dev` loopback mode | No | Section 2 |
| Share a file or directory temporarily | `-file` / `-dir` static adapter | Depends on where signaling is deployed | Section 3 |
| Expose an intranet web service to external visitors | Production: Tier 1 or Tier 2 | Yes | Section 4 |

---

## 2. Try It Locally

Go and Node.js must be installed (see [CONTRIBUTING](https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/CONTRIBUTING.md) for versions). Run the following in three terminals:

```bash
# Terminal 1: build and start the signaling server (-dev is for local loopback only)
make build
./bin/signal -addr 127.0.0.1:8888 -dev -signal-secret dev-cluster-secret-16b

# Terminal 2: start a local service to proxy, e.g. any HTTP service on port 8080
python3 -m http.server 8080 --bind 127.0.0.1

# Terminal 3: start the Agent with an access password
ACCESS_SECRET=change-me ./bin/agent \
  -id homeserver \
  -signal ws://127.0.0.1:8888/ws \
  -signal-secret dev-cluster-secret-16b \
  -target http://127.0.0.1:8080
```

Open `http://localhost:8888/#homeserver` in a browser and enter `change-me` to reach the local `8080` service.

---

## 3. Share a File or Directory

Keep the signaling server from Section 2 running and replace `-target` with `-dir` or `-file` (the three are mutually exclusive):

```bash
ACCESS_SECRET=change-me ./bin/agent \
  -id my-files \
  -signal ws://127.0.0.1:8888/ws \
  -signal-secret dev-cluster-secret-16b \
  -dir /srv/share
```

The static adapter is read-only by default, rejects symlinks and dotfiles, and serves every file under a sandbox CSP. To let others in without the master password, use `-mint-link` to mint an offline, time-limited, path-scoped, read-only share link; see [Agent Use Cases](./agent-use-cases) and [Read-Only Static Adapter](./file-server).

---

## 4. Serve External Visitors: Choose a Deployment Tier

Production requires explicit `-portal-origin` and `-sandbox-origin` at startup. Choose by the domains and certificates you have:

| Your situation | Recommended tier | Notes |
| :--- | :--- | :--- |
| Two different registrable domains and wildcard certificates are available | **Tier 1 cross-site wildcard** | Strongest isolation; suited for multiple nodes and external sharing |
| Two ordinary domains, no wildcard certificate | **Tier 2 fixed single-domain cross-site** | Single node or mutually trusted apps; the portal shows a warning banner |
| Only one registrable domain | **Tier 2 same-site wildcard** | Requires explicit `-allow-same-site-sandbox`; the portal shows a warning banner |

See the [Production Deployment Guide](./deployment) for DNS, certificate, and reverse-proxy steps, and [Server Use Cases](./server-use-cases) for hardened Docker deployment. If something goes wrong, check [Troubleshooting and FAQ](./troubleshooting).

---

## 5. Next Steps

- Learn the security model: [Security and Cryptography](./security).
- Check that visitors' browsers are supported: [Browser Compatibility](./browser-compatibility).
- Understand the overall design: [Architecture and Design](./architecture).
