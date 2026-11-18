# Browser Compatibility Matrix & Architectural Guide

WebRTC Gateway achieves "Zero-Client, Zero-Plugin, Zero-Root-CA" end-to-end P2P tunneling through **WebRTC RTCDataChannel + Service Worker Transparent Interception + DOM/XHR Virtualization Patching**.

This document defines compatibility tiers, tested engine versions, and browser sandbox constraints for systems architects, DevOps, and developers.

---

## Browser Support Tiers

| Browser Family | Engine Kernel | Support Tier | Recommended Version | Service Worker Proxy | WebRTC P2P (SCTP) | Media & Partial Content (HTTP 206) | Notes |
| :--- | :--- | :---: | :---: | :---: | :---: | :---: | :--- |
| **Google Chrome** | Chromium / Blink | **Tier 1** | 90+ | ✅ Supported | ✅ Supported | ✅ Supported | Official baseline testbed; passes 22/22 real-world rendering test suite. |
| **Microsoft Edge** | Chromium / Blink | **Tier 1** | 90+ | ✅ Supported | ✅ Supported | ✅ Supported | Official baseline testbed; native high-performance runtime across Windows/macOS. |
| **Brave / Vivaldi / Opera** | Chromium / Blink | **Tier 1** | Latest | ✅ Supported | ✅ Supported | ✅ Supported | Ensure third-party WebRTC privacy extensions do not strip local ICE candidates. |
| **Mozilla Firefox** | Gecko | **Tier 1** | 95+ | ✅ Supported | ✅ Supported | ✅ Supported | Native support for W3C ReadableStream and Service Worker MessagePort pipelines. |
| **Dual-Core Browsers** | Blink (Speed Mode) | **Tier 1** | Latest | ✅ Supported | ✅ Supported | ✅ Supported | Must operate in Blink speed mode; legacy IE compatibility mode is unsupported. |
| **Apple Safari (macOS)** | WebKit | **Tier 2** | 15.4+ | ✅ Supported | ✅ Supported | ✅ Supported | Full support for Service Worker stream interception and WebRTC SCTP DataChannels. |
| **Apple Safari (iOS / iPadOS)** | WebKit | **Tier 2** | iOS 15.4+ | ⚠️ Supported | ✅ Supported | ✅ Supported | Governed by iOS memory eviction and background suspension; see self-healing below. |
| **Android Mobile Browsers** | Chromium / WebView | **Tier 1** | Android 8+ | ✅ Supported | ✅ Supported | ✅ Supported | Chrome Android, Kiwi, and Edge Mobile behave identically to desktop browsers. |

> **Tier Definitions**:
> - **Tier 1 (Native Production Grade)**: Covered by full CI automated matrices; continuous background operation and high-throughput transfers are 100% reliable.
> - **Tier 2 (Fully Compatible with System Constraints)**: Core capabilities are fully functional but adhere to OS-enforced background suspension policies (such as iOS WebKit).

---

## WebKit (Safari / iOS) Characteristics & Self-Healing Architecture

On iOS WebKit and macOS Safari, battery optimization and Intelligent Tracking Prevention (ITP) impose specific platform constraints handled by native gateway mitigations:

### 1. iOS Safari Background Suspension & Worker Termination
- **Platform Behavior**: When switching tabs, locking the screen, or moving Safari to the background on iOS, the WebKit kernel pauses and terminates the Service Worker process within seconds to preserve battery.
- **Gateway Self-Healing Pipeline**:
  1. Upon returning to the foreground, `portal.js` automatically detects window focus and triggers the `PORTAL_ATTACH` handshake pipeline;
  2. If the Service Worker was terminated, the handshake re-attaches a fresh `MessagePort` channel and synchronizes active `SessionID` and `NodeID`;
  3. Combined with in-place WebRTC ICE Restart and a 5-second grace window, the user experiences instant recovery without manual page reloads.

### 2. Apple Intelligent Tracking Prevention (ITP) & Storage Partitioning
- **Platform Behavior**: Safari strictly isolates third-party cookies across origins.
- **Gateway Mitigation**:
  - The gateway relies on **Same-Origin Transparency**: all proxied intranet assets are served within the Web Portal origin scope via Service Worker virtualization without relying on cross-site third-party cookies;
  - Each visitor session maintains an isolated in-memory CookieJar on the Agent daemon, completely bypassing ITP restrictions.

### 3. Secure Context Requirements (HTTPS / WSS)
- **W3C Requirement**: `navigator.serviceWorker` is only exposed in **Secure Contexts**:
  - Localhost development: `http://localhost:...` or `http://127.0.0.1:...`;
  - LAN & Public internet: **Must use HTTPS / WSS**.
- **Out-of-the-Box Solution**:
  - The signal server provides the `-tls-auto` flag to dynamically generate in-memory ECDSA SAN certificates covering all physical LAN IP addresses, eliminating blank screens on mobile devices over local Wi-Fi.

---

## Best Practices & Recommendations

1. **Install as PWA for Mobile Devices**:
   - On iOS Safari, tap "Share -> Add to Home Screen" to run the gateway as an independent standalone Web App with a more resilient background runtime.
2. **Foreground Operation During Large Transfers**:
   - During multi-gigabyte file transfers or sustained 4K media streaming, keep the browser tab in the foreground or maintain screen wakefulness for maximum throughput.
