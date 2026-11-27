/**
 * V7 Ephemeral Sandbox Bootstrap Bridge (/__p2p_gw__/bridge.html)
 * - Mounted ONLY for the brief SW-registration + MessagePort handoff, then IMMEDIATELY removed
 *   from Portal's DOM BEFORE `appFrame` is created (zero temporal overlap with untrusted app JS!).
 * - Strictly validates `ev.origin` against Go Signal-injected `<meta name="p2p-portal-origins">`
 *   allowlist (Fail-Closed when meta tag is missing or empty!).
 * - Prunes any non-`/__p2p_gw__/sw.js` Service Worker registrations before attaching.
 */

import { isBackendMeta, isRecord } from '../shared/ipc';

const SW_URL = '/__p2p_gw__/sw.js';

export function readAllowedPortalOrigins(doc: Document = document): Set<string> {
  const allow = new Set<string>();
  const meta = doc.querySelector('meta[name="p2p-portal-origins"]');
  const raw = meta?.getAttribute('content');
  if (!raw) return allow;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (!Array.isArray(parsed)) return allow;
    for (const item of parsed) {
      if (typeof item !== 'string') continue;
      try {
        const u = new URL(item);
        if (u.protocol === 'http:' || u.protocol === 'https:') {
          allow.add(u.origin);
        }
      } catch {
        // ignore invalid origin
      }
    }
  } catch {
    // fail-closed on malformed metadata
  }
  return allow;
}

export function isAllowedPortalOrigin(origin: string, allowlist: Set<string>): boolean {
  if (!origin || allowlist.size === 0) return false;
  return allowlist.has(origin);
}

let swRegPromise: Promise<ServiceWorkerRegistration> | null = null;

async function pruneStrayServiceWorkers(): Promise<void> {
  if (!('serviceWorker' in navigator) || typeof navigator.serviceWorker.getRegistrations !== 'function') {
    return;
  }
  try {
    const regs = await navigator.serviceWorker.getRegistrations();
    for (const reg of regs) {
      const scriptUrl = reg.active?.scriptURL || reg.waiting?.scriptURL || reg.installing?.scriptURL || '';
      if (!scriptUrl) continue;
      try {
        const u = new URL(scriptUrl);
        if (u.origin === location.origin && u.pathname !== SW_URL) {
          await reg.unregister().catch(() => {});
        }
      } catch {
        // ignore
      }
    }
  } catch {
    // ignore
  }
}

function initBridgeRuntime(): void {
  if (typeof window === 'undefined' || typeof document === 'undefined') return;

  const allowedPortalOrigins = readAllowedPortalOrigins(document);
  const urlTabId = new URLSearchParams(location.search).get('tab') || '';

  async function ensureServiceWorkerReady(): Promise<ServiceWorker> {
    if (!('serviceWorker' in navigator)) {
      throw new Error('ServiceWorker API unavailable in this browser context');
    }
    if (!swRegPromise) {
      swRegPromise = (async () => {
        await pruneStrayServiceWorkers();
        return navigator.serviceWorker.register(SW_URL, { scope: '/' });
      })().catch((err) => {
        swRegPromise = null;
        throw err;
      });
    }
    const reg = await swRegPromise;

    if (reg.active && reg.active.state === 'activated') {
      return reg.active;
    }

    const readyReg = await navigator.serviceWorker.ready;
    const worker = readyReg.active || reg.active || navigator.serviceWorker.controller;
    if (!worker) {
      throw new Error('ServiceWorker failed to activate');
    }
    return worker;
  }

  window.addEventListener('message', async (ev: MessageEvent) => {
    if (!ev.isTrusted || ev.source !== window.parent) return;
    if (!isAllowedPortalOrigin(ev.origin, allowedPortalOrigins)) {
      ev.ports?.[0]?.close();
      return;
    }
    const data = ev.data;
    if (!isRecord(data) || data.type !== 'P2P_PORTAL_INIT') return;
    if (typeof data.tabId !== 'string' || !data.tabId || !isBackendMeta(data.meta)) {
      ev.ports?.[0]?.close();
      return;
    }
    if (urlTabId && data.tabId !== urlTabId) {
      ev.ports?.[0]?.close();
      return;
    }
    const bridgePort = ev.ports?.[0];
    if (!bridgePort) return;

    try {
      const sw = await ensureServiceWorkerReady();
      sw.postMessage(
        {
          type: 'SW_ATTACH_PORT',
          tabId: data.tabId,
          portalOrigin: ev.origin,
          meta: data.meta,
        },
        [bridgePort]
      );
      window.parent.postMessage({ type: 'P2P_BRIDGE_READY', tabId: data.tabId }, ev.origin);
    } catch (err) {
      bridgePort.close();
      console.error('[P2P Bridge] Failed to attach Service Worker port:', err);
    }
  });

  if ('serviceWorker' in navigator) {
    // Proactively kick off SW registration & stray SW pruning on ephemeral bridge load.
    ensureServiceWorkerReady().catch(() => {});
  }
}

initBridgeRuntime();
