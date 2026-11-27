/**
 * V7 In-Memory Non-HttpOnly Cookie Mirror (Eliminates Browser Cookie Tossing & Zombie Deletes)
 * - NEVER reads or writes real browser `document.cookie` on the sandbox origin.
 * - Maintains per-tab in-memory non-HttpOnly target cookies synchronized from Go Agent's
 *   authoritative `ProxyCookieJar` via V7 binary TLV `RespCookieDelta` and `AUTH_RESULT.meta.initialCookies`.
 * - Tracks JS `document.cookie = ...` mutations (both set and delete) with monotonic per-client
 *   sequence numbers to guarantee zero ordering races between `SW_COOKIE_MUTATE` and `fetch()`.
 */

import { base64UrlDecode } from '../shared/crypto';
import {
  isClientCookieMutation,
  isCookieDelta,
  type ClientCookieMutation,
  type CookieDelta,
} from '../shared/ipc';

const COOKIE_SYNC_ACK_TIMEOUT_MS = 50;
const MAX_MIRROR_COOKIES = 256;
const textDecoder = new TextDecoder();

export interface MirrorCookieEntry {
  name: string;
  value: string;
  path: string;
  expires: number; // unix ms, 0 = session cookie
}

function normalizeCookiePath(rawPath?: string): string {
  if (!rawPath || !rawPath.startsWith('/')) return '/';
  return rawPath;
}

function cookieKey(path: string, name: string): string {
  return `${path}\0${name}`;
}

export function cookiePathMatches(reqPath: string, cookiePath: string): boolean {
  const p = reqPath || '/';
  if (p === cookiePath) return true;
  if (!p.startsWith(cookiePath)) return false;
  if (cookiePath.endsWith('/')) return true;
  return p.charCodeAt(cookiePath.length) === 0x2f; // '/'
}

export class CookieMirror {
  private readonly entries = new Map<string, MirrorCookieEntry>();
  private readonly pendingMutations = new Map<string, ClientCookieMutation>();
  private readonly clientLastSeq = new Map<string, number>();

  applyDeltas(deltas: CookieDelta[], now = Date.now()): void {
    for (const d of deltas) {
      if (!isCookieDelta(d)) continue;
      const path = normalizeCookiePath(d.path);
      const key = cookieKey(path, d.name);
      if (d.op === 'del' || (d.expires && d.expires > 0 && d.expires <= now)) {
        this.entries.delete(key);
        continue;
      }
      if (!this.entries.has(key) && this.entries.size >= MAX_MIRROR_COOKIES) {
        const oldestKey = this.entries.keys().next().value;
        if (oldestKey) this.entries.delete(oldestKey);
      }
      this.entries.set(key, {
        name: d.name,
        value: d.value ?? '',
        path,
        expires: d.expires && d.expires > 0 ? d.expires : 0,
      });
    }
  }

  /**
   * Applies client JS mutations (`document.cookie = ...`) to the in-memory mirror,
   * queues them for upstream delivery, and returns normalized CookieDeltas for sibling tabs.
   */
  applyClientMutations(
    mutations: ClientCookieMutation[],
    opts: { clientId?: string; seq?: number; queueForUpstream?: boolean; now?: number } = {}
  ): CookieDelta[] {
    const now = opts.now ?? Date.now();
    if (opts.clientId && typeof opts.seq === 'number' && opts.seq > 0) {
      const prev = this.clientLastSeq.get(opts.clientId) ?? 0;
      if (opts.seq <= prev) {
        return [];
      }
      this.clientLastSeq.set(opts.clientId, opts.seq);
    }

    const deltas: CookieDelta[] = [];
    for (const m of mutations) {
      if (!isClientCookieMutation(m)) continue;
      const path = normalizeCookiePath(m.path);
      const key = cookieKey(path, m.name);
      const isDelete = m.op === 'del' || (m.expires !== undefined && m.expires > 0 && m.expires <= now);

      if (isDelete) {
        this.entries.delete(key);
        const delDelta: CookieDelta = { op: 'del', name: m.name, path };
        deltas.push(delDelta);
        if (opts.queueForUpstream !== false) {
          this.pendingMutations.set(key, { op: 'del', name: m.name, path });
        }
      } else {
        if (!this.entries.has(key) && this.entries.size >= MAX_MIRROR_COOKIES) {
          const oldestKey = this.entries.keys().next().value;
          if (oldestKey) this.entries.delete(oldestKey);
        }
        const exp = m.expires && m.expires > 0 ? m.expires : 0;
        const val = m.value ?? '';
        this.entries.set(key, { name: m.name, value: val, path, expires: exp });
        deltas.push({ op: 'set', name: m.name, value: val, path, expires: exp || undefined });
        if (opts.queueForUpstream !== false) {
          this.pendingMutations.set(key, { op: 'set', name: m.name, value: val, path, expires: exp || undefined });
        }
      }
    }
    return deltas;
  }

  drainPendingMutations(): ClientCookieMutation[] {
    if (this.pendingMutations.size === 0) return [];
    const list = Array.from(this.pendingMutations.values());
    this.pendingMutations.clear();
    return list;
  }

  snapshot(now = Date.now()): CookieDelta[] {
    const out: CookieDelta[] = [];
    for (const [k, e] of this.entries) {
      if (e.expires > 0 && e.expires <= now) {
        this.entries.delete(k);
        continue;
      }
      out.push({
        op: 'set',
        name: e.name,
        value: e.value,
        path: e.path,
        expires: e.expires || undefined,
      });
    }
    return out;
  }

  serializeForPath(reqPath: string, now = Date.now()): string {
    const matched: MirrorCookieEntry[] = [];
    for (const [k, e] of this.entries) {
      if (e.expires > 0 && e.expires <= now) {
        this.entries.delete(k);
        continue;
      }
      if (cookiePathMatches(reqPath, e.path)) {
        matched.push(e);
      }
    }
    matched.sort((a, b) => b.path.length - a.path.length);
    return matched.map((c) => `${c.name}=${c.value}`).join('; ');
  }
}

export function decodeClientMutationsHeader(rawB64: string): ClientCookieMutation[] {
  if (!rawB64) return [];
  try {
    const raw = base64UrlDecode(rawB64.trim());
    if (raw.byteLength > 32768) return [];
    const parsed = JSON.parse(textDecoder.decode(raw)) as unknown;
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(isClientCookieMutation);
  } catch {
    return [];
  }
}

/**
 * Delivers non-HttpOnly CookieDelta updates to the requesting Window client (with 50ms ACK wait)
 * and broadcasts to sibling app Window clients belonging to the same tab session.
 */
export async function syncCookieDeltasToClients(
  swScope: ServiceWorkerGlobalScope,
  primaryClientId: string,
  siblingClientIds: Iterable<string>,
  deltas: CookieDelta[]
): Promise<void> {
  if (deltas.length === 0) return;

  const targetClient = primaryClientId
    ? await swScope.clients.get(primaryClientId).catch(() => undefined)
    : undefined;
  if (targetClient && targetClient.type === 'window') {
    await postDeltasWithAck(targetClient, deltas);
  }

  for (const cid of siblingClientIds) {
    if (!cid || (targetClient && cid === targetClient.id)) continue;
    const c = await swScope.clients.get(cid).catch(() => undefined);
    if (!c || c.url.includes('/__p2p_gw__/bridge.html')) continue;
    try {
      c.postMessage({ type: 'P2P_COOKIE_MIRROR_UPDATE', deltas });
    } catch {
      // ignore
    }
  }
}

function postDeltasWithAck(client: Client, deltas: CookieDelta[]): Promise<void> {
  return new Promise<void>((resolve) => {
    const ch = new MessageChannel();
    let settled = false;
    const finish = () => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      ch.port1.onmessage = null;
      try {
        ch.port1.close();
      } catch {
        // ignore
      }
      resolve();
    };
    const timer = setTimeout(finish, COOKIE_SYNC_ACK_TIMEOUT_MS);
    ch.port1.onmessage = () => finish();
    try {
      client.postMessage({ type: 'P2P_COOKIE_MIRROR_UPDATE', deltas }, [ch.port2]);
    } catch {
      try {
        ch.port2.close();
      } catch {
        // ignore
      }
      finish();
    }
  });
}
