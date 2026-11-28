import { base64UrlEncode } from '../shared/crypto';
import {
  isCookieDelta,
  isRecord,
  type ClientCookieMutation,
  type CookieDelta,
} from '../shared/ipc';
import type { PrivateSWHandle, RuntimePatchConfig } from './types';

interface MirrorCookie {
  name: string;
  value: string;
  path: string;
  expires: number; // unix ms, 0 = session
}

const textEncoder = new TextEncoder();

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
 * and CTL/semicolon hygiene).
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
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c < 0x20 || c === 0x7f || c === 0x3b) return null;
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

export class ClientCookieMirror {
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

export interface CookieBridgeState {
  getUnsyncedMutationsPayload: () => { b64: string; maxSeq: number; list: ClientCookieMutation[] };
  pruneUnsyncedUpTo: (maxSeq: number) => void;
  applyDeltas: (deltas: CookieDelta[]) => void;
}

export function installCookiePatch(
  cfg: RuntimePatchConfig,
  swHandle: PrivateSWHandle | null,
  notifyPortalSwEvicted: () => void
): CookieBridgeState {
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

  const postCookieMutationToSW = (seq: number, mut: ClientCookieMutation) => {
    if (!swHandle) return;
    const ctrl = swHandle.getController();
    const send = (target: ServiceWorker) => {
      const ch = new MessageChannel();
      let settled = false;
      const cleanup = () => {
        ch.port1.onmessage = null;
        try {
          ch.port1.close();
        } catch {
          // ignore
        }
      };
      ch.port1.onmessage = () => {
        if (settled) return;
        settled = true;
        const idx = unsyncedMuts.findIndex((x) => x.seq === seq);
        if (idx >= 0) unsyncedMuts.splice(idx, 1);
        cleanup();
      };
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

  return {
    getUnsyncedMutationsPayload,
    pruneUnsyncedUpTo,
    applyDeltas: (deltas) => mirror.applyDeltas(deltas),
  };
}
