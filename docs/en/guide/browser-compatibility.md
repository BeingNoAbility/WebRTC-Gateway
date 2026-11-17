# Browser Compatibility & Limitation Matrix

This document defines tier support, tested versions, and technical limitations across desktop and mobile browsers.

---

## Browser Support Tiers

| Tier | Browsers | Status | Verification |
| :---: | :--- | :---: | :--- |
| **Tier 1 (Full Support)** | Chrome (Desktop/Android) 90+<br/>Microsoft Edge 90+<br/>Firefox 100+ | ✅ **Optimal** | Full Service Worker stream interception, RTCDataChannel, full-duplex WebSockets, media streaming, file downloads. |
| **Tier 2 (Supported with Caveats)** | Apple Safari (macOS / iOS) 15.4+<br/>WebKit WebViews | ⚠️ **Supported** | WebRTC and Service Worker supported. iOS Safari aggressively suspends background JavaScript execution when tabs are switched or backgrounded. Handled via in-place ICE Restart. |
| **Tier 3 (Unsupported)** | Internet Explorer 11<br/>Legacy Edge (EdgeHTML) | ❌ **Unsupported** | Lacks modern WebRTC, ES2020 modules, and Service Worker streaming APIs. |

---

## Core Capabilities Matrix

| Feature | Chrome / Edge | Firefox | Safari (macOS) | iOS Safari |
| :--- | :---: | :---: | :---: | :---: |
| **RTCDataChannel (SCTP)** | ✅ Full | ✅ Full | ✅ Full | ✅ Full |
| **Service Worker Stream Proxy** | ✅ Full | ✅ Full | ✅ Full | ✅ Full |
| **HTTP 206 Partial Content (Seeking)** | ✅ Full | ✅ Full | ✅ Full | ✅ Full |
| **WebSocket Multiplexing** | ✅ Full | ✅ Full | ✅ Full | ✅ Full |
| **W3C Upload Progress Virtualization** | ✅ Full | ✅ Full | ✅ Full | ✅ Full |
| **Background Tab Hibernation Recovery** | ⚡ Instant | ⚡ Instant | 🔄 ICE Restart | 🔄 ICE Restart |
