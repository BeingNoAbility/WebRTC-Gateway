# Agent & Server Configuration Manual

This document provides a comprehensive reference for all command-line flags and environment variables for both the Agent client daemon and the Signal server.

---

## Agent CLI Flags & Environment Variables

| CLI Flag | Environment Variable | Default | Description |
| :--- | :--- | :---: | :--- |
| `-node-id` | `NODE_ID` | `""` | **(Required)** Unique peer identifier for this agent. |
| `-signal` | `SIGNAL_URL` | `ws://127.0.0.1:8888/ws` | Public signaling server WebSocket URL. |
| `-target` | `TARGET_URL` | `http://127.0.0.1:8080` | Local target service to proxy (Mode A). |
| `-file` | `FILE_PATH` | `""` | Single file path to share via in-memory server (Mode C). |
| `-dir` | `DIR_PATH` | `""` | Directory path to share statically (Mode B). |
| `-access-secret` | `ACCESS_SECRET` | `""` | Data-plane visitor authentication password. |
| `-signal-secret` | `SIGNAL_SECRET` | `""` | Control-plane signaling cluster authorization secret. |
| `-sign-link` | - | `0` | Generates a time-limited share link (e.g. `1h`, `24h`) and exits. |
| `-portal-url` | - | `https://your-portal.com` | Base portal URL for `-sign-link` output. |
| `-max-streams` | `MAX_STREAMS` | `512` | Maximum concurrent multiplexed streams. |
| `-metrics-addr` | `METRICS_ADDR` | `""` | Optional Prometheus exporter address (e.g. `:9090`). |
| `-v`, `-version` | - | `false` | Display Agent version and exit. |
| `-h`, `-help` | - | `false` | Display command-line usage help and exit. |

---

## Signal Server CLI Flags & Environment Variables

| CLI Flag | Environment Variable | Default | Description |
| :--- | :--- | :---: | :--- |
| `-addr` | `ADDR` | `:8888` | Address and port to bind the server on. |
| `-secret` | `SECRET` | `""` | Cluster signaling authorization secret. |
| `-tls-cert` | `TLS_CERT` | `""` | Path to TLS certificate PEM file. |
| `-tls-key` | `TLS_KEY` | `""` | Path to TLS private key PEM file. |
| `-tls-auto` | `TLS_AUTO` | `false` | Auto-generate self-signed ECDSA SAN certificates for LAN IPs. |
| `-max-ip-conns` | `MAX_IP_CONNS` | `64` | Maximum concurrent connections per IP address. |
| `-health` | - | `false` | Run local healthcheck probe (exits 0 on success, 1 on failure). |
| `-v`, `-version` | - | `false` | Display Server version and exit. |
| `-h`, `-help` | - | `false` | Display usage help and exit. |
