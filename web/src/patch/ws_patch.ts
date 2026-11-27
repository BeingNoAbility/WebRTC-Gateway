/**
 * V7 Compatibility Patch (/__p2p_gw__/patch.js) — Pure Compat Layer, ZERO Security Duties!
 * - Saves real `navigator.serviceWorker` in a private closure and stubs `navigator.serviceWorker`
 *   (`register`/`getRegistrations`/`getRegistration`/`ready`/`controller`) so PWAs never self-destruct `/__p2p_gw__/sw.js`.
 * - Patches `Document.prototype.cookie` with strict RFC 6265 validation (`Domain`/`HttpOnly`/`__Host-`/`__Secure-`/`Max-Age`).
 * - Fixes `VirtualWebSocket`: separate `closeQueued` flag (flushes `ws.send(m); ws.close()` 100%),
 *   IDL `on*` via `addEventListener` (`ev.target === ws` + exception isolation), canonical `this.url`,
 *   `wasClean: true` on any peer `WS_CLOSE` (code !== 1006), active `close()` awaits peer close ack (2s fallback),
 *   copies `SharedArrayBuffer`, and applies `WS_OPEN.cookieDeltas` into `ClientCookieMirror`.
 * - Fixes `fetch(Request, init)` header replacement & `POST -> GET/HEAD` body stripping.
 * - Rewrites `target="_top"` / `target="_parent"` on links & forms to `_self` for sandboxed iframe compat.
 */

import { base64UrlDecode, base64UrlEncode } from '../shared/crypto';
import {
  isCookieDelta,
  isRecord,
  type ClientCookieMutation,
  type CookieDelta,
  type WsPortFromPortalMsg,
} from '../shared/ipc';
import {
  resolvePortalPopupUrl,
  rewriteBackendUrl,
  rewriteWebSocketTarget,
  type UrlRewriteContext,
} from '../shared/url_rewrite';

interface RuntimePatchConfig {
  nodeId: string;
  portalOrigin: string;
  backendOrigin: string;
  pinnedPk?: string;
  initCookies: CookieDelta[];
}

interface MirrorCookie {
  name: string;
  value: string;
  path: string;
  expires: number; // unix ms, 0 = session
}

interface PrivateSWHandle {
  getController: () => ServiceWorker | null;
  getReady: () => Promise<ServiceWorkerRegistration>;
  addMessageListener: (fn: (ev: MessageEvent) => void) => void;
}

const textEncoder = new TextEncoder();
const textDecoder = new TextDecoder();

function defaultCookiePath(pathname: string): string {
  if (!pathname || !pathname.startsWith('/')) return '/';
  if (pathname === '/') return '/';
  const lastSlash = pathname.lastIndexOf('/');
  if (lastSlash <= 0) return '/';
  return pathname.slice(0, lastSlash);
}

function cookiePathMatches(reqPath: string, cookiePath: string): boolean {
  const p = reqPath || '/';
  if (p === cookiePath) return true;
  if (!p.startsWith(cookiePath)) return false;
  if (cookiePath.endsWith('/')) return true;
  return p.charCodeAt(cookiePath.length) === 0x2f;
}

export interface CookieParseContext {
  pathname?: string;
  now?: number;
  isSecureContext?: boolean;
  allowedHosts?: string[];
}

/**
 * Parses a JS `document.cookie = "..."` assignment into a structured `ClientCookieMutation`,
 * enforcing RFC 6265 validation (`Domain`, `HttpOnly`, `__Host-`, `__Secure-`, strict `Max-Age`,
 * and CTL/semicolon hygiene). Exported for unit testing.
 */
export function parseDocumentCookieAssignment(
  raw: string,
  pathnameOrCtx: string | CookieParseContext = '/'
): ClientCookieMutation | null {
  const ctx: CookieParseContext =
    typeof pathnameOrCtx === 'string'
      ? { pathname: pathnameOrCtx }
      : pathnameOrCtx;
  const currentPathname = ctx.pathname ?? '/';
  const now = ctx.now ?? Date.now();
  const isSecure = ctx.isSecureContext ?? true;
  const allowedHosts = (ctx.allowedHosts || []).map((h) => h.toLowerCase()).filter(Boolean);

  const parts = String(raw).split(';');
  if (parts.length === 0) return null;
  const first = parts[0].trim();
  const eq = first.indexOf('=');
  if (eq <= 0) return null;

  const name = first.slice(0, eq).trim();
  const value = first.slice(eq + 1).trim();
  if (!name || /[\s\r\n\0;=]/.test(name)) return null;
  // Reject CTL / NUL / ';' inside cookie value.
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c < 0x20 || c === 0x7f || c === 0x3b /* ';' */) return null;
  }

  let path = defaultCookiePath(currentPathname);
  let explicitPath = '';
  let hasDomainAttr = false;
  let hasSecureAttr = false;
  let expires = 0;
  let hasMaxAge = false;
  let isDelete = false;

  for (let i = 1; i < parts.length; i++) {
    const attr = parts[i].trim();
    if (!attr) continue;
    const aEq = attr.indexOf('=');
    const aName = (aEq === -1 ? attr : attr.slice(0, aEq)).trim().toLowerCase();
    const aVal = aEq === -1 ? '' : attr.slice(aEq + 1).trim();

    if (aName === 'httponly') {
      // RFC 6265 §5.2: script API (`document.cookie`) CANNOT set HttpOnly cookies!
      return null;
    } else if (aName === 'secure') {
      hasSecureAttr = true;
    } else if (aName === 'domain') {
      hasDomainAttr = true;
      const dom = aVal.replace(/^\.+/, '').toLowerCase();
      if (!dom) return null;
      if (allowedHosts.length > 0) {
        const matches = allowedHosts.some((h) => h === dom || h.endsWith(`.${dom}`));
        if (!matches) return null;
      }
    } else if (aName === 'path' && aVal.startsWith('/')) {
      explicitPath = aVal;
      path = aVal;
    } else if (aName === 'max-age') {
      // Strict RFC 6265 §5.2.2 integer grammar (`/^-?\d+$/`)
      if (/^-?\d+$/.test(aVal)) {
        const sec = Number.parseInt(aVal, 10);
        if (Number.isFinite(sec)) {
          hasMaxAge = true;
          if (sec <= 0) {
            isDelete = true;
            expires = 1;
          } else {
            const capped = Math.min(sec, 400 * 24 * 3600);
            expires = now + capped * 1000;
            isDelete = false;
          }
        }
      }
    } else if (aName === 'expires' && !hasMaxAge) {
      const parsedMs = Date.parse(aVal);
      if (Number.isFinite(parsedMs)) {
        if (parsedMs <= now) {
          isDelete = true;
          expires = 1;
        } else {
          expires = parsedMs;
          isDelete = false;
        }
      }
    }
  }

  const lowerName = name.toLowerCase();
  if (lowerName.startsWith('__secure-') && (!hasSecureAttr || !isSecure)) {
    return null;
  }
  if (lowerName.startsWith('__host-')) {
    if (!hasSecureAttr || !isSecure || hasDomainAttr || explicitPath !== '/') {
      return null;
    }
  }
  if (hasSecureAttr && !isSecure) {
    return null;
  }

  if (isDelete) {
    return { op: 'del', name, path };
  }
  return {
    op: 'set',
    name,
    value,
    path,
    expires: expires > 0 ? expires : undefined,
  };
}

class ClientCookieMirror {
  private readonly entries = new Map<string, MirrorCookie>();

  applyDeltas(deltas: CookieDelta[], now = Date.now()): void {
    for (const d of deltas) {
      if (!isCookieDelta(d)) continue;
      const p = d.path && d.path.startsWith('/') ? d.path : '/';
      const key = `${p}\0${d.name}`;
      if (d.op === 'del' || (d.expires && d.expires > 0 && d.expires <= now)) {
        this.entries.delete(key);
        continue;
      }
      this.entries.set(key, {
        name: d.name,
        value: d.value ?? '',
        path: p,
        expires: d.expires && d.expires > 0 ? d.expires : 0,
      });
    }
  }

  applyMutation(mut: ClientCookieMutation, now = Date.now()): void {
    const p = mut.path && mut.path.startsWith('/') ? mut.path : '/';
    const key = `${p}\0${mut.name}`;
    if (mut.op === 'del' || (mut.expires && mut.expires > 0 && mut.expires <= now)) {
      this.entries.delete(key);
      return;
    }
    this.entries.set(key, {
      name: mut.name,
      value: mut.value ?? '',
      path: p,
      expires: mut.expires && mut.expires > 0 ? mut.expires : 0,
    });
  }

  serialize(pathname: string, now = Date.now()): string {
    const matched: MirrorCookie[] = [];
    for (const [k, c] of this.entries) {
      if (c.expires > 0 && c.expires <= now) {
        this.entries.delete(k);
        continue;
      }
      if (cookiePathMatches(pathname, c.path)) {
        matched.push(c);
      }
    }
    matched.sort((a, b) => b.path.length - a.path.length);
    return matched.map((c) => `${c.name}=${c.value}`).join('; ');
  }
}

/**
 * Captures the real `navigator.serviceWorker` container in a private closure and replaces
 * the public `navigator.serviceWorker` property with a benign sandbox stub so legitimate
 * PWAs calling `register()` or `getRegistrations().forEach(r => r.unregister())` NEVER
 * destroy `/__p2p_gw__/sw.js`!
 */
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

  // Replace public `navigator.serviceWorker` with a harmless stub hiding `/__p2p_gw__/sw.js`.
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
    // Fallback: shadow individual methods on the container if navigator property is non-configurable.
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

if (typeof window !== 'undefined' || typeof self !== 'undefined') {
  if (!g.__P2P_GW_PATCH_INSTALLED__) {
    const isWorker = typeof document === 'undefined';
    const cfg = loadRuntimeConfig(isWorker);
    if (cfg) {
      g.__P2P_GW_PATCH_INSTALLED__ = true;
      if (isWorker) {
        installWorkerPatches(cfg);
      } else {
        installWindowPatches(cfg);
      }
    }
  }
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

  if (typeof document === 'undefined') return null;
  const scriptEl =
    (document.currentScript as HTMLScriptElement | null) ||
    (document.querySelector('script[data-p2p-boot]') as HTMLScriptElement | null);
  const bootB64 = scriptEl?.getAttribute('data-p2p-boot');
  if (!bootB64) return null;
  scriptEl?.removeAttribute('data-p2p-boot');

  try {
    const parsed = JSON.parse(textDecoder.decode(base64UrlDecode(bootB64))) as unknown;
    if (!isRecord(parsed)) return null;
    const rawInit = Array.isArray(parsed.initCookies) ? parsed.initCookies.filter(isCookieDelta) : [];
    return {
      nodeId: typeof parsed.nodeId === 'string' ? parsed.nodeId : '',
      portalOrigin: typeof parsed.portalOrigin === 'string' ? parsed.portalOrigin : '',
      backendOrigin: typeof parsed.backendOrigin === 'string' ? parsed.backendOrigin : '',
      pinnedPk: typeof parsed.pinnedPk === 'string' ? parsed.pinnedPk : undefined,
      initCookies: rawInit,
    };
  } catch {
    return null;
  }
}

function makeRewriteCtx(cfg: RuntimePatchConfig): UrlRewriteContext {
  const baseHref = typeof document !== 'undefined' ? document.baseURI : location.href;
  return {
    currentOrigin: location.origin,
    portalOrigin: cfg.portalOrigin,
    nodeId: cfg.nodeId,
    targetOrigin: cfg.backendOrigin,
    pinnedPk: cfg.pinnedPk,
    baseHref,
  };
}

function buildPatchedRequestInit(
  input: Request,
  init: RequestInit | undefined,
  extraHeaders?: Record<string, string>
): RequestInit & { duplex?: 'half' } {
  const method = (init?.method ?? input.method).toUpperCase();
  // Fetch spec: `init.headers` REPLACES `input.headers` (never merges onto old headers!).
  const headers = init?.headers !== undefined ? new Headers(init.headers) : new Headers(input.headers);
  if (extraHeaders) {
    for (const [k, v] of Object.entries(extraHeaders)) {
      headers.set(k, v);
    }
  }
  const isBodylessMethod = method === 'GET' || method === 'HEAD';
  const body = isBodylessMethod
    ? undefined
    : init?.body !== undefined
      ? init.body
      : input.body;
  const reqInit: RequestInit & { duplex?: 'half' } = {
    method,
    headers,
    body,
    mode: init?.mode ?? (input.mode === 'navigate' ? 'same-origin' : input.mode),
    credentials: init?.credentials ?? input.credentials,
    cache: init?.cache ?? input.cache,
    redirect: init?.redirect ?? input.redirect,
    referrer: init?.referrer ?? input.referrer,
    referrerPolicy: init?.referrerPolicy ?? input.referrerPolicy,
    integrity: init?.integrity ?? input.integrity,
    keepalive: init?.keepalive ?? input.keepalive,
    signal: init?.signal ?? input.signal,
  };
  // Set `duplex: 'half'` ONLY when body is an actual ReadableStream (never force streaming mode
  // onto string/Blob/ArrayBuffer/FormData bodies — preserves Content-Length & 307/308 redirects!).
  if (typeof ReadableStream !== 'undefined' && reqInit.body instanceof ReadableStream) {
    reqInit.duplex = 'half';
  }
  return reqInit;
}

function installWorkerPatches(cfg: RuntimePatchConfig): void {
  const origFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = function (input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const ctx = makeRewriteCtx(cfg);
    if (typeof input === 'string' || input instanceof URL) {
      const nextUrl = rewriteBackendUrl(String(input), ctx);
      return origFetch(nextUrl, init);
    }
    const nextUrl = rewriteBackendUrl(input.url, ctx);
    if (nextUrl === input.url) {
      return origFetch(input, init);
    }
    return origFetch(new Request(nextUrl, buildPatchedRequestInit(input, init)));
  };
}

function installWindowPatches(cfg: RuntimePatchConfig): void {
  // 0. Capture private ServiceWorker handle and shield `/__p2p_gw__/sw.js` from PWA unregister/register!
  const swHandle = captureAndStubServiceWorker();

  // 1. Install pure in-memory CookieMirror on `Document.prototype.cookie` (fallback to `document`).
  const mirror = new ClientCookieMirror();
  mirror.applyDeltas(cfg.initCookies);

  const allowedCookieHosts: string[] = [location.hostname];
  if (cfg.backendOrigin) {
    try {
      allowedCookieHosts.push(new URL(cfg.backendOrigin).hostname);
    } catch {
      // ignore
    }
  }

  let mutSeq = 0;
  const unsyncedMuts: Array<{ seq: number; ts: number; mut: ClientCookieMutation }> = [];

  const pruneUnsyncedUpTo = (maxSeq: number) => {
    if (maxSeq <= 0 || unsyncedMuts.length === 0) return;
    while (unsyncedMuts.length > 0 && unsyncedMuts[0].seq <= maxSeq) {
      unsyncedMuts.shift();
    }
  };

  const notifyPortalSwEvicted = () => {
    if (window.parent && window.parent !== window) {
      try {
        window.parent.postMessage({ type: 'P2P_SW_EVICTED' }, cfg.portalOrigin || '*');
      } catch {
        // ignore
      }
    }
  };

  const postCookieMutationToSW = (seq: number, mut: ClientCookieMutation) => {
    if (!swHandle) return;
    const ctrl = swHandle.getController();
    const send = (target: ServiceWorker) => {
      const ch = new MessageChannel();
      let settled = false;
      const onAck = () => {
        if (settled) return;
        settled = true;
        const idx = unsyncedMuts.findIndex((x) => x.seq === seq);
        if (idx >= 0) unsyncedMuts.splice(idx, 1);
        cleanup();
      };
      const cleanup = () => {
        ch.port1.onmessage = null;
        try {
          ch.port1.close();
        } catch {
          // ignore
        }
      };
      ch.port1.onmessage = onAck;
      setTimeout(cleanup, 5_000);
      try {
        target.postMessage({ type: 'SW_COOKIE_MUTATE', seq, mutations: [mut] }, [ch.port2]);
      } catch {
        cleanup();
      }
    };
    if (ctrl) {
      send(ctrl);
    } else {
      swHandle
        .getReady()
        .then((reg) => {
          if (reg.active) send(reg.active);
        })
        .catch(() => {});
    }
  };

  const postCookieDeltasToSW = (deltas: CookieDelta[]) => {
    if (!swHandle || deltas.length === 0) return;
    const ctrl = swHandle.getController();
    if (ctrl) {
      try {
        ctrl.postMessage({ type: 'SW_COOKIE_SYNC', deltas });
      } catch {
        // ignore
      }
    }
  };

  const cookieDescriptor: PropertyDescriptor = {
    configurable: true,
    enumerable: true,
    get() {
      return mirror.serialize(location.pathname);
    },
    set(val: string) {
      const mut = parseDocumentCookieAssignment(String(val), {
        pathname: location.pathname,
        isSecureContext: location.protocol === 'https:',
        allowedHosts: allowedCookieHosts,
      });
      if (!mut) return;
      mirror.applyMutation(mut);
      const seq = ++mutSeq;
      unsyncedMuts.push({ seq, ts: Date.now(), mut });
      if (unsyncedMuts.length > 128) unsyncedMuts.shift();
      postCookieMutationToSW(seq, mut);
    },
  };

  let patchedProto = false;
  if (typeof Document !== 'undefined' && Document.prototype) {
    try {
      Object.defineProperty(Document.prototype, 'cookie', cookieDescriptor);
      patchedProto = true;
    } catch {
      patchedProto = false;
    }
  }
  if (!patchedProto) {
    Object.defineProperty(document, 'cookie', cookieDescriptor);
  }

  if (swHandle) {
    swHandle.addMessageListener((ev: MessageEvent) => {
      const data = ev.data;
      if (!isRecord(data) || typeof data.type !== 'string') return;
      if (data.type === 'P2P_SW_EVICTED') {
        notifyPortalSwEvicted();
        return;
      }
      if (data.type !== 'P2P_COOKIE_MIRROR_UPDATE' || !Array.isArray(data.deltas)) {
        return;
      }
      mirror.applyDeltas(data.deltas.filter(isCookieDelta));
      const ackPort = ev.ports?.[0];
      if (ackPort) {
        try {
          ackPort.postMessage({ type: 'COOKIE_SYNC_ACK' });
          ackPort.close();
        } catch {
          // ignore
        }
      }
    });
  }

  const getUnsyncedMutationsPayload = (): { b64: string; maxSeq: number; list: ClientCookieMutation[] } => {
    const cutoff = Date.now() - 30_000;
    while (unsyncedMuts.length > 0 && unsyncedMuts[0].ts < cutoff) {
      unsyncedMuts.shift();
    }
    if (unsyncedMuts.length === 0) return { b64: '', maxSeq: 0, list: [] };
    const list = unsyncedMuts.map((x) => x.mut);
    const maxSeq = unsyncedMuts[unsyncedMuts.length - 1].seq;
    const b64 = base64UrlEncode(textEncoder.encode(JSON.stringify(list)));
    return { b64, maxSeq, list };
  };

  // 2. Patch fetch() with Fetch-spec-compliant `Request + init` override, inline cookie mutations,
  //    and SW eviction recovery notification.
  const origFetch = window.fetch.bind(window);
  window.fetch = function (input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const ctx = makeRewriteCtx(cfg);
    const isReq = input instanceof Request;
    const rawUrl = isReq ? input.url : String(input);
    const rewrittenUrl = rewriteBackendUrl(rawUrl, ctx);
    const isSameOrigin = (() => {
      try {
        return new URL(rewrittenUrl, document.baseURI).origin === location.origin;
      } catch {
        return false;
      }
    })();

    const credentials = init?.credentials ?? (isReq ? input.credentials : 'same-origin');
    const pending =
      isSameOrigin && credentials !== 'omit'
        ? getUnsyncedMutationsPayload()
        : { b64: '', maxSeq: 0, list: [] };
    const attachMuts = Boolean(pending.b64);

    let promise: Promise<Response>;
    if (!isReq) {
      if (!attachMuts) {
        promise = origFetch(rewrittenUrl, init);
      } else {
        const headers = new Headers(init?.headers);
        headers.set('x-p2p-cookie-mutations', pending.b64);
        headers.set('x-p2p-cookie-seq', String(pending.maxSeq));
        promise = origFetch(rewrittenUrl, { ...init, headers });
      }
    } else if (rewrittenUrl === input.url && !attachMuts) {
      promise = origFetch(input, init);
    } else {
      const extra = attachMuts
        ? {
            'x-p2p-cookie-mutations': pending.b64,
            'x-p2p-cookie-seq': String(pending.maxSeq),
          }
        : undefined;
      promise = origFetch(new Request(rewrittenUrl, buildPatchedRequestInit(input, init, extra)));
    }

    if (!isSameOrigin) return promise;
    return promise.then((resp) => {
      if (resp.headers.get('x-p2p-sw-evicted') === '1') {
        notifyPortalSwEvicted();
      } else if (attachMuts && resp.status !== 502 && resp.status !== 503) {
        pruneUnsyncedUpTo(pending.maxSeq);
      }
      return resp;
    });
  };

  // 3. Patch XMLHttpRequest (URL rewrite + ASCII base64url inline cookie mutations).
  const xhrProto = XMLHttpRequest.prototype;
  const origOpen = xhrProto.open;
  const origSend = xhrProto.send;
  const xhrMeta = new WeakMap<XMLHttpRequest, { sameOrigin: boolean }>();

  xhrProto.open = function (
    this: XMLHttpRequest,
    method: string,
    url: string | URL,
    async?: boolean,
    username?: string | null,
    password?: string | null
  ): void {
    const rewritten = rewriteBackendUrl(String(url), makeRewriteCtx(cfg));
    let sameOrigin = false;
    try {
      sameOrigin = new URL(rewritten, document.baseURI).origin === location.origin;
    } catch {
      sameOrigin = false;
    }
    xhrMeta.set(this, { sameOrigin });
    return origOpen.call(this, method, rewritten, async ?? true, username ?? null, password ?? null);
  };

  xhrProto.send = function (
    this: XMLHttpRequest,
    body?: Document | XMLHttpRequestBodyInit | null
  ): void {
    const meta = xhrMeta.get(this);
    if (meta?.sameOrigin) {
      const pending = getUnsyncedMutationsPayload();
      if (pending.b64) {
        try {
          this.setRequestHeader('X-P2P-Cookie-Mutations', pending.b64);
          this.setRequestHeader('X-P2P-Cookie-Seq', String(pending.maxSeq));
          this.addEventListener(
            'load',
            () => {
              if (this.getResponseHeader('x-p2p-sw-evicted') === '1') {
                notifyPortalSwEvicted();
              } else if (this.status !== 502 && this.status !== 503) {
                pruneUnsyncedUpTo(pending.maxSeq);
              }
            },
            { once: true }
          );
        } catch {
          // ignore
        }
      }
    }
    return origSend.call(this, body);
  };

  // 4. Patch virtual WebSocket routed via private Service Worker handle.
  installVirtualWebSocket(
    cfg,
    swHandle,
    () => {
      const p = getUnsyncedMutationsPayload();
      return p;
    },
    (maxSeq) => pruneUnsyncedUpTo(maxSeq),
    (deltas) => {
      mirror.applyDeltas(deltas);
      postCookieDeltasToSW(deltas);
    }
  );

  // 5. Rewrite target="_blank", target="_top"/"_parent" -> "_self", window.open, and <form action>.
  const origWindowOpen = window.open.bind(window);
  window.open = function (url?: string | URL, target?: string, features?: string): Window | null {
    if (url) {
      const ctx = makeRewriteCtx(cfg);
      const normTarget = target === '_top' || target === '_parent' ? '_self' : target;
      // Only wrap true new-window/tab opens (`undefined`, `""`, or `"_blank"`) into Portal URLs;
      // preserve `_self` and named child iframes (e.g. `window.open(url, 'previewFrame')`) as backend URLs!
      if (!normTarget || normTarget === '_blank') {
        const popup = resolvePortalPopupUrl(String(url), ctx);
        if (popup) return origWindowOpen(popup, normTarget || '_blank', features);
      } else {
        return origWindowOpen(rewriteBackendUrl(String(url), ctx), normTarget, features);
      }
    }
    return origWindowOpen(url, target, features);
  };

  // Use BUBBLE phase (capture = false) so SPA routers calling `ev.preventDefault()` run first,
  // and never hijack `<a download>` links into Portal popups!
  document.addEventListener(
    'click',
    (ev: MouseEvent) => {
      if (ev.defaultPrevented || ev.button !== 0) return;
      const anchor = (ev.target as Element | null)?.closest?.('a[href]') as HTMLAnchorElement | null;
      if (!anchor) return;
      const rawHref = anchor.getAttribute('href') || '';
      if (!rawHref || rawHref.startsWith('#')) return;

      const ctx = makeRewriteCtx(cfg);
      const tgt = (anchor.getAttribute('target') || '').toLowerCase();
      if (tgt === '_top' || tgt === '_parent') {
        anchor.setAttribute('target', '_self');
      }
      if (!anchor.hasAttribute('download') && (tgt === '_blank' || ev.ctrlKey || ev.metaKey)) {
        const popup = resolvePortalPopupUrl(rawHref, ctx);
        if (popup) {
          ev.preventDefault();
          origWindowOpen(popup, '_blank', 'noopener');
        }
        return;
      }
      const rewritten = rewriteBackendUrl(rawHref, ctx);
      if (rewritten !== rawHref) {
        anchor.setAttribute('href', rewritten);
      }
    },
    false
  );

  document.addEventListener(
    'submit',
    (ev: SubmitEvent) => {
      const form = ev.target as HTMLFormElement | null;
      if (!form || form.tagName !== 'FORM') return;
      const tgt = (form.getAttribute('target') || '').toLowerCase();
      if (tgt === '_top' || tgt === '_parent') {
        form.setAttribute('target', '_self');
      }
      const rawAction = form.getAttribute('action');
      if (!rawAction) return;
      const rewritten = rewriteBackendUrl(rawAction, makeRewriteCtx(cfg));
      if (rewritten !== rawAction) {
        form.setAttribute('action', rewritten);
      }
    },
    true
  );

  // 6. Sync top-level app route & document title to Portal (pure UX, never trusted for security).
  if (window.parent && window.parent !== window && cfg.portalOrigin) {
    const postToPortal = (msg: Record<string, unknown>) => {
      try {
        window.parent.postMessage(msg, cfg.portalOrigin);
      } catch {
        // ignore
      }
    };
    const syncRoute = () => {
      postToPortal({
        type: 'P2P_ROUTE_SYNC',
        path: `${location.pathname}${location.search}${location.hash}`,
      });
    };
    const syncTitle = () => {
      if (document.title) postToPortal({ type: 'P2P_TITLE_SYNC', title: document.title });
    };

    const origPush = history.pushState.bind(history);
    const origReplace = history.replaceState.bind(history);
    history.pushState = function (...args: Parameters<History['pushState']>) {
      const res = origPush(...args);
      syncRoute();
      return res;
    };
    history.replaceState = function (...args: Parameters<History['replaceState']>) {
      const res = origReplace(...args);
      syncRoute();
      return res;
    };
    window.addEventListener('popstate', syncRoute);
    window.addEventListener('hashchange', syncRoute);
    syncRoute();
    syncTitle();
    window.addEventListener('load', syncTitle, { once: true });
  }
}

function truncateUtf8Bytes(str: string, maxBytes: number): string {
  const bytes = textEncoder.encode(str);
  if (bytes.byteLength <= maxBytes) return str;
  let end = maxBytes;
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) {
    end--;
  }
  return textDecoder.decode(bytes.subarray(0, end));
}

function canonicalizeWebSocketUrl(rawUrl: string): string {
  try {
    const u = new URL(rawUrl, typeof document !== 'undefined' ? document.baseURI : location.href);
    if (u.protocol === 'http:') u.protocol = 'ws:';
    else if (u.protocol === 'https:') u.protocol = 'wss:';
    return u.href;
  } catch {
    return rawUrl;
  }
}

function installVirtualWebSocket(
  cfg: RuntimePatchConfig,
  swHandle: PrivateSWHandle | null,
  getPendingMutations: () => { list: ClientCookieMutation[]; maxSeq: number },
  onMutationsSent: (maxSeq: number) => void,
  onCookieDeltas: (deltas: CookieDelta[]) => void
): void {
  const NativeWS = window.WebSocket;
  if (!swHandle) return;

  const openSockets = new Set<VirtualWebSocket>();
  window.addEventListener('pagehide', () => {
    for (const sock of Array.from(openSockets)) {
      sock.closeOnUnload();
    }
  });

  type HandlerMap = {
    open: ((this: WebSocket, ev: Event) => unknown) | null;
    message: ((this: WebSocket, ev: MessageEvent) => unknown) | null;
    error: ((this: WebSocket, ev: Event) => unknown) | null;
    close: ((this: WebSocket, ev: CloseEvent) => unknown) | null;
  };

  class VirtualWebSocket extends EventTarget {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSING = 2;
    static readonly CLOSED = 3;
    readonly CONNECTING = 0;
    readonly OPEN = 1;
    readonly CLOSING = 2;
    readonly CLOSED = 3;

    readonly url: string = '';
    readyState: number = VirtualWebSocket.CONNECTING;
    bufferedAmount = 0;
    extensions = '';
    protocol = '';
    binaryType: BinaryType = 'blob';

    private port: MessagePort | null = null;
    private sendQueue: Promise<void> = Promise.resolve();
    private closeQueued = false;
    private closeFallbackTimer: ReturnType<typeof setTimeout> | null = null;

    // IDL event handlers implemented via addEventListener so `ev.target === ws` & errors isolate cleanly!
    private readonly idlHandlers: HandlerMap = { open: null, message: null, error: null, close: null };
    private readonly idlWrappers: Record<keyof HandlerMap, EventListener | null> = {
      open: null,
      message: null,
      error: null,
      close: null,
    };

    private setIdlHandler<K extends keyof HandlerMap>(type: K, fn: HandlerMap[K]): void {
      const prev = this.idlWrappers[type];
      if (prev) {
        this.removeEventListener(type, prev);
        this.idlWrappers[type] = null;
      }
      this.idlHandlers[type] = typeof fn === 'function' ? fn : null;
      if (this.idlHandlers[type]) {
        const bound: EventListener = (ev: Event) => {
          (this.idlHandlers[type] as ((this: WebSocket, e: Event) => unknown)).call(
            this as unknown as WebSocket,
            ev
          );
        };
        this.idlWrappers[type] = bound;
        this.addEventListener(type, bound);
      }
    }

    get onopen(): HandlerMap['open'] {
      return this.idlHandlers.open;
    }
    set onopen(fn: HandlerMap['open']) {
      this.setIdlHandler('open', fn);
    }
    get onmessage(): HandlerMap['message'] {
      return this.idlHandlers.message;
    }
    set onmessage(fn: HandlerMap['message']) {
      this.setIdlHandler('message', fn);
    }
    get onerror(): HandlerMap['error'] {
      return this.idlHandlers.error;
    }
    set onerror(fn: HandlerMap['error']) {
      this.setIdlHandler('error', fn);
    }
    get onclose(): HandlerMap['close'] {
      return this.idlHandlers.close;
    }
    set onclose(fn: HandlerMap['close']) {
      this.setIdlHandler('close', fn);
    }

    constructor(rawUrl: string | URL, protocols?: string | string[]) {
      super();
      const urlStr = String(rawUrl);
      const target = rewriteWebSocketTarget(urlStr, makeRewriteCtx(cfg));
      if (!target) {
        return new NativeWS(rawUrl, protocols) as unknown as VirtualWebSocket;
      }

      this.url = canonicalizeWebSocketUrl(urlStr);
      const protoList = Array.isArray(protocols)
        ? protocols.map(String)
        : protocols
          ? [String(protocols)]
          : [];

      const ch = new MessageChannel();
      this.port = ch.port1;
      this.port.onmessage = (ev: MessageEvent) => this.handlePortMessage(ev.data);
      this.port.start?.();
      openSockets.add(this);

      const pendingMuts = getPendingMutations();
      const openPayload = {
        type: 'SW_WS_OPEN',
        target,
        protocols: protoList,
        mutations: pendingMuts.list,
      };

      const dispatchToSW = (sw: ServiceWorker | null) => {
        if (!sw) {
          ch.port2.close();
          queueMicrotask(() => this.dispatchErrorAndClose('Service Worker controller unavailable'));
          return;
        }
        try {
          sw.postMessage(openPayload, [ch.port2]);
          if (pendingMuts.maxSeq > 0) {
            onMutationsSent(pendingMuts.maxSeq);
          }
        } catch {
          ch.port2.close();
          queueMicrotask(() => this.dispatchErrorAndClose('Failed to post SW_WS_OPEN'));
        }
      };

      const ctrl = swHandle!.getController();
      if (ctrl) {
        dispatchToSW(ctrl);
      } else {
        swHandle!
          .getReady()
          .then((reg) => dispatchToSW(reg.active))
          .catch(() => dispatchToSW(null));
      }
    }

    send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
      if (this.readyState === VirtualWebSocket.CONNECTING) {
        throw new DOMException(
          "Failed to execute 'send' on 'WebSocket': Still in CONNECTING state.",
          'InvalidStateError'
        );
      }
      if (this.closeQueued || this.readyState !== VirtualWebSocket.OPEN || !this.port) return;

      let snapshotKind: 'text' | 'blob' | 'ab';
      let textVal = '';
      let blobVal: Blob | null = null;
      let abVal: ArrayBuffer | null = null;
      let byteLen = 0;

      if (typeof data === 'string') {
        snapshotKind = 'text';
        textVal = data;
        byteLen = textEncoder.encode(data).byteLength;
      } else if (data instanceof Blob) {
        snapshotKind = 'blob';
        blobVal = data;
        byteLen = data.size;
      } else if (ArrayBuffer.isView(data)) {
        snapshotKind = 'ab';
        const copy = new Uint8Array(data.byteLength);
        copy.set(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
        abVal = copy.buffer;
        byteLen = copy.byteLength;
      } else if (data instanceof ArrayBuffer) {
        snapshotKind = 'ab';
        abVal = data.slice(0);
        byteLen = abVal.byteLength;
      } else if (typeof SharedArrayBuffer !== 'undefined' && data instanceof SharedArrayBuffer) {
        // Copy SharedArrayBuffer into a transferable ArrayBuffer safely!
        snapshotKind = 'ab';
        const copy = new Uint8Array(data.byteLength);
        copy.set(new Uint8Array(data));
        abVal = copy.buffer;
        byteLen = copy.byteLength;
      } else {
        snapshotKind = 'text';
        textVal = String(data);
        byteLen = textEncoder.encode(textVal).byteLength;
      }

      this.bufferedAmount += byteLen;

      // Flush even if `close()` transitions `readyState` to CLOSING right after `send()`!
      this.sendQueue = this.sendQueue
        .then(async () => {
          if (this.readyState === VirtualWebSocket.CLOSED || !this.port) {
            this.bufferedAmount = Math.max(0, this.bufferedAmount - byteLen);
            return;
          }
          if (snapshotKind === 'text') {
            this.port.postMessage({ type: 'WS_SEND', opcode: 1, data: textVal });
          } else if (snapshotKind === 'blob' && blobVal) {
            const buf = await blobVal.arrayBuffer();
            if (this.readyState === VirtualWebSocket.CLOSED || !this.port) {
              this.bufferedAmount = Math.max(0, this.bufferedAmount - byteLen);
              return;
            }
            this.port.postMessage({ type: 'WS_SEND', opcode: 2, data: buf }, [buf]);
          } else if (abVal) {
            this.port.postMessage({ type: 'WS_SEND', opcode: 2, data: abVal }, [abVal]);
          }
        })
        .catch(() => {
          this.bufferedAmount = Math.max(0, this.bufferedAmount - byteLen);
        });
    }

    close(code?: number, reason = ''): void {
      if (code !== undefined) {
        if (!Number.isInteger(code) || (code !== 1000 && (code < 3000 || code > 4999))) {
          throw new DOMException(
            `Failed to execute 'close' on 'WebSocket': The code must be either 1000, or between 3000 and 4999. ${code} is neither.`,
            'InvalidAccessError'
          );
        }
      }
      const safeCode = code ?? 1000;
      const reasonStr = String(reason);
      if (textEncoder.encode(reasonStr).byteLength > 123) {
        throw new DOMException(
          "Failed to execute 'close' on 'WebSocket': The message must not be greater than 123 bytes.",
          'SyntaxError'
        );
      }

      if (this.closeQueued || this.readyState === VirtualWebSocket.CLOSING || this.readyState === VirtualWebSocket.CLOSED) {
        return;
      }
      this.closeQueued = true;
      this.readyState = VirtualWebSocket.CLOSING;
      const safeReason = truncateUtf8Bytes(reasonStr, 123);

      // Flush every queued send BEFORE sending WS_CLOSE, then await peer WS_CLOSE ack (2s fallback).
      this.sendQueue = this.sendQueue
        .then(() => {
          if (this.readyState === VirtualWebSocket.CLOSED) return;
          try {
            this.port?.postMessage({ type: 'WS_CLOSE', code: safeCode, reason: safeReason });
          } catch {
            this.finalizeClose(safeCode, safeReason, false);
            return;
          }
          if (!this.closeFallbackTimer) {
            this.closeFallbackTimer = setTimeout(() => {
              this.finalizeClose(safeCode, safeReason, false);
            }, 2_000);
          }
        })
        .catch(() => {
          this.finalizeClose(safeCode, safeReason, false);
        });
    }

    closeOnUnload(): void {
      if (this.readyState === VirtualWebSocket.CLOSED) return;
      this.closeQueued = true;
      try {
        this.port?.postMessage({ type: 'WS_CLOSE', code: 1001, reason: 'page unloading' });
      } catch {
        // ignore
      }
      this.finalizeClose(1001, 'page unloading', false);
    }

    private dispatchErrorAndClose(msg: string): void {
      this.dispatchEvent(new Event('error'));
      this.finalizeClose(1006, msg, false);
    }

    private handlePortMessage(raw: unknown): void {
      if (!isRecord(raw) || typeof raw.type !== 'string') return;
      const msg = raw as WsPortFromPortalMsg;

      switch (msg.type) {
        case 'WS_OPEN': {
          if (this.readyState !== VirtualWebSocket.CONNECTING) return;
          this.readyState = VirtualWebSocket.OPEN;
          this.protocol = msg.protocol || '';
          if (Array.isArray(msg.cookieDeltas) && msg.cookieDeltas.length > 0) {
            onCookieDeltas(msg.cookieDeltas.filter(isCookieDelta));
          }
          this.dispatchEvent(new Event('open'));
          break;
        }
        case 'WS_ACK': {
          this.bufferedAmount = Math.max(0, this.bufferedAmount - msg.bytes);
          break;
        }
        case 'WS_MSG': {
          if (this.readyState !== VirtualWebSocket.OPEN && this.readyState !== VirtualWebSocket.CLOSING) return;
          let payload: string | ArrayBuffer | Blob;
          if (msg.opcode === 1) {
            payload = typeof msg.data === 'string' ? msg.data : textDecoder.decode(msg.data);
          } else {
            const ab = msg.data instanceof ArrayBuffer ? msg.data : new ArrayBuffer(0);
            payload = this.binaryType === 'arraybuffer' ? ab : new Blob([ab]);
          }
          this.dispatchEvent(new MessageEvent('message', { data: payload, origin: location.origin }));
          break;
        }
        case 'WS_ERROR': {
          this.dispatchErrorAndClose(msg.message || 'WebSocket error');
          break;
        }
        case 'WS_CLOSE': {
          const code = typeof msg.code === 'number' ? msg.code : 1000;
          const reason = typeof msg.reason === 'string' ? msg.reason : '';
          // Any explicit peer WS_CLOSE frame (non-1006) is a clean RFC 6455 close!
          this.finalizeClose(code, reason, code !== 1006);
          break;
        }
      }
    }

    private finalizeClose(code: number, reason: string, wasClean: boolean): void {
      if (this.readyState === VirtualWebSocket.CLOSED) return;
      this.readyState = VirtualWebSocket.CLOSED;
      if (this.closeFallbackTimer) {
        clearTimeout(this.closeFallbackTimer);
        this.closeFallbackTimer = null;
      }
      openSockets.delete(this);
      if (this.port) {
        this.port.onmessage = null;
        try {
          this.port.close();
        } catch {
          // ignore
        }
        this.port = null;
      }
      this.dispatchEvent(new CloseEvent('close', { code, reason, wasClean }));
    }
  }

  (window as unknown as { WebSocket: typeof WebSocket }).WebSocket =
    VirtualWebSocket as unknown as typeof WebSocket;
}
