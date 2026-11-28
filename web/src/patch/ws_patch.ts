/**
 * Compatibility Patch Entrypoint (/__p2p_gw__/assets/patch.<hash>.js) — Pure Compat Layer, ZERO Security Duties!
 * Reads per-session boot config from SW-served `/__p2p_gw__/boot.js` (`globalThis.__P2P_GW_BOOT__`),
 * captures & stubs `navigator.serviceWorker`, and wires modular cookie/fetch/XHR/WebSocket/navigation patches.
 */

import { base64UrlDecode } from '../shared/crypto';
import { isCookieDelta, isRecord } from '../shared/ipc';
import { installCookiePatch, parseDocumentCookieAssignment, type CookieParseContext } from './cookie_patch';
import {
  buildPatchedRequestInit,
  installFetchAndXhrPatch,
  installWorkerFetchPatch,
  isSameCanonicalRequestUrl,
} from './fetch_xhr_patch';
import { installNavigationPatch } from './nav_patch';
import type { PrivateSWHandle, RuntimePatchConfig } from './types';
import { installVirtualWebSocket } from './ws_client_patch';

export {
  buildPatchedRequestInit,
  isSameCanonicalRequestUrl,
  parseDocumentCookieAssignment,
  type CookieParseContext,
};

const textDecoder = new TextDecoder();

function captureAndStubServiceWorker(): PrivateSWHandle | null {
  if (typeof navigator === 'undefined' || !('serviceWorker' in navigator)) {
    return null;
  }
  const realSw = navigator.serviceWorker;
  if (!realSw) return null;

  const proto = Object.getPrototypeOf(realSw) as ServiceWorkerContainer | null;
  const ctrlGetter = proto ? Object.getOwnPropertyDescriptor(proto, 'controller')?.get : undefined;
  const readyGetter = proto ? Object.getOwnPropertyDescriptor(proto, 'ready')?.get : undefined;
  const realAddListener = realSw.addEventListener.bind(realSw);

  const handle: PrivateSWHandle = {
    getController: () => (ctrlGetter ? (ctrlGetter.call(realSw) as ServiceWorker | null) : realSw.controller),
    getReady: () => (readyGetter ? (readyGetter.call(realSw) as Promise<ServiceWorkerRegistration>) : realSw.ready),
    addMessageListener: (fn) => realAddListener('message', fn as EventListener),
  };

  const neverReady = new Promise<ServiceWorkerRegistration>(() => {});
  const dummyTarget = new EventTarget();
  const swStub = {
    controller: null,
    ready: neverReady,
    oncontrollerchange: null,
    onmessage: null,
    onmessageerror: null,
    register: () =>
      Promise.reject(
        new DOMException('ServiceWorker registration is managed by WebRTC Gateway', 'SecurityError')
      ),
    getRegistration: () => Promise.resolve(undefined),
    getRegistrations: () => Promise.resolve([]),
    startMessages: () => {},
    addEventListener: dummyTarget.addEventListener.bind(dummyTarget),
    removeEventListener: dummyTarget.removeEventListener.bind(dummyTarget),
    dispatchEvent: dummyTarget.dispatchEvent.bind(dummyTarget),
  };

  try {
    Object.defineProperty(navigator, 'serviceWorker', {
      configurable: true,
      enumerable: true,
      get: () => swStub,
    });
  } catch {
    try {
      Object.defineProperty(realSw, 'register', { value: swStub.register, configurable: true });
      Object.defineProperty(realSw, 'getRegistrations', { value: swStub.getRegistrations, configurable: true });
      Object.defineProperty(realSw, 'getRegistration', { value: swStub.getRegistration, configurable: true });
      Object.defineProperty(realSw, 'ready', { get: () => neverReady, configurable: true });
    } catch {
      // ignore
    }
  }

  return handle;
}

const g = globalThis as unknown as Record<string, unknown>;

function parseRawBootObject(raw: unknown): RuntimePatchConfig | null {
  if (!isRecord(raw)) return null;
  const rawInit = Array.isArray(raw.initCookies) ? raw.initCookies.filter(isCookieDelta) : [];
  return {
    nodeId: typeof raw.nodeId === 'string' ? raw.nodeId : '',
    portalOrigin: typeof raw.portalOrigin === 'string' ? raw.portalOrigin : '',
    backendOrigin: typeof raw.backendOrigin === 'string' ? raw.backendOrigin : '',
    cookieSecureContext:
      typeof raw.cookieSecureContext === 'boolean' ? raw.cookieSecureContext : undefined,
    pinnedPk: typeof raw.pinnedPk === 'string' ? raw.pinnedPk : undefined,
    initCookies: rawInit,
  };
}

function loadRuntimeConfig(isWorker: boolean): RuntimePatchConfig | null {
  if (isWorker) {
    const wCfg = g.__P2P_GW_WORKER_CFG__;
    if (!isRecord(wCfg)) return null;
    return {
      nodeId: '',
      portalOrigin: '',
      backendOrigin: typeof wCfg.backendOrigin === 'string' ? wCfg.backendOrigin : '',
      initCookies: [],
    };
  }

  // Primary path: SW-served `/__p2p_gw__/boot.js` sets `globalThis.__P2P_GW_BOOT__` (immediately deleted!).
  if (g.__P2P_GW_BOOT__ !== undefined) {
    const raw = g.__P2P_GW_BOOT__;
    try {
      delete g.__P2P_GW_BOOT__;
    } catch {
      g.__P2P_GW_BOOT__ = undefined;
    }
    return parseRawBootObject(raw);
  }

  // Fallback for legacy inline attribute in standalone tests.
  if (typeof document === 'undefined') return null;
  const scriptEl =
    (document.currentScript as HTMLScriptElement | null) ||
    (document.querySelector('script[data-p2p-boot]') as HTMLScriptElement | null);
  const bootB64 = scriptEl?.getAttribute('data-p2p-boot');
  if (!bootB64) return null;
  scriptEl?.removeAttribute('data-p2p-boot');
  try {
    return parseRawBootObject(JSON.parse(textDecoder.decode(base64UrlDecode(bootB64))));
  } catch {
    return null;
  }
}

if (typeof window !== 'undefined' || typeof self !== 'undefined') {
  if (!g.__P2P_GW_PATCH_INSTALLED__) {
    const isWorker = typeof document === 'undefined';
    const cfg = loadRuntimeConfig(isWorker);
    if (cfg) {
      g.__P2P_GW_PATCH_INSTALLED__ = true;
      if (isWorker) {
        installWorkerFetchPatch(cfg);
      } else {
        const swHandle = captureAndStubServiceWorker();
        const notifyPortalSwEvicted = () => {
          // Always reach Portal (window.top) even when triggered inside a nested child iframe.
          const targetWin = window.top && window.top !== window ? window.top : window.parent;
          if (targetWin && targetWin !== window) {
            try {
              targetWin.postMessage({ type: 'P2P_SW_EVICTED' }, cfg.portalOrigin || '*');
            } catch {
              // ignore
            }
          }
        };
        const cookieState = installCookiePatch(cfg, swHandle, notifyPortalSwEvicted);
        installFetchAndXhrPatch(cfg, cookieState, notifyPortalSwEvicted);
        installVirtualWebSocket(
          cfg,
          swHandle,
          () => cookieState.getUnsyncedMutationsPayload(),
          (maxSeq) => cookieState.pruneUnsyncedUpTo(maxSeq),
          (deltas) => cookieState.applyDeltas(deltas)
        );
        installNavigationPatch(cfg, swHandle);
      }
    }
  }
}
