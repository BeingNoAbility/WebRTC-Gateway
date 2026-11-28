/**
 * In-Memory Non-HttpOnly Cookie Mirror (Service Worker Side)
 * - NEVER reads or writes real browser `document.cookie` on the sandbox origin.
 * - Shares core storage/expiry/path-matching with `ClientCookieMirror` via `CookieMirrorStore`.
 * - Tracks JS `document.cookie = ...` mutations with monotonic per-client sequence numbers
 *   and non-destructive peek/commit watermarks so failed tunnel sends never lose mutations.
 */

import {
  CookieMirrorStore,
  cookiePathMatches,
  type MirrorCookieEntry,
} from '../shared/cookie_mirror';
import { base64UrlDecode } from '../shared/crypto';
import {
  isClientCookieMutation,
  type ClientCookieMutation,
  type CookieDelta,
} from '../shared/ipc';

export { cookiePathMatches, type MirrorCookieEntry };

const COOKIE_SYNC_ACK_TIMEOUT_MS = 50;
const textDecoder = new TextDecoder();

export class CookieMirror extends CookieMirrorStore {
  private readonly pendingMutations = new Map<string, { mut: ClientCookieMutation; ver: number }>();
  private mutVerSeq = 0;
  private readonly clientLastSeq = new Map<string, number>();

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
      const res = this.setOrDelete(m.op, m.name, m.value, m.path, m.expires, now);
      if (res.deleted) {
        const delDelta: CookieDelta = { op: 'del', name: m.name, path: res.path };
        deltas.push(delDelta);
        if (opts.queueForUpstream !== false) {
          this.pendingMutations.set(res.key, {
            mut: { op: 'del', name: m.name, path: res.path },
            ver: ++this.mutVerSeq,
          });
        }
      } else {
        deltas.push({
          op: 'set',
          name: m.name,
          value: res.val,
          path: res.path,
          expires: res.exp || undefined,
        });
        if (opts.queueForUpstream !== false) {
          this.pendingMutations.set(res.key, {
            mut: {
              op: 'set',
              name: m.name,
              value: res.val,
              path: res.path,
              expires: res.exp || undefined,
            },
            ver: ++this.mutVerSeq,
          });
        }
      }
    }
    return deltas;
  }

  /**
   * Non-destructively peeks queued client mutations along with a watermark version so
   * failed tunnel sends never lose pending mutations.
   */
  peekPendingMutations(): { mutations: ClientCookieMutation[]; watermark: number } {
    if (this.pendingMutations.size === 0) return { mutations: [], watermark: this.mutVerSeq };
    const mutations: ClientCookieMutation[] = [];
    for (const entry of this.pendingMutations.values()) {
      mutations.push(entry.mut);
    }
    return { mutations, watermark: this.mutVerSeq };
  }

  commitPendingMutations(watermark: number): void {
    for (const [k, entry] of this.pendingMutations) {
      if (entry.ver <= watermark) {
        this.pendingMutations.delete(k);
      }
    }
  }

  drainPendingMutations(): ClientCookieMutation[] {
    const { mutations, watermark } = this.peekPendingMutations();
    this.commitPendingMutations(watermark);
    return mutations;
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

/**
 * Unified single-path broadcast for JS-originated cookie mutations: propagates validated
 * CookieDeltas to sibling clients in the same tab AND to Portal's `liveCookieSnapshot`
 * (used by both `SW_COOKIE_MUTATE` and inline `X-P2P-Cookie-Mutations` fetch ACKs so the
 * two tracks can never diverge or drop Portal snapshot updates).
 */
export async function broadcastClientCookieDeltas(
  swScope: ServiceWorkerGlobalScope,
  session: { clientIds: Set<string>; port: MessagePort | null },
  originClientId: string,
  deltas: CookieDelta[]
): Promise<void> {
  if (deltas.length === 0) return;
  const others = Array.from(session.clientIds).filter((id) => id !== originClientId);
  await syncCookieDeltasToClients(swScope, '', others, deltas);
  try {
    session.port?.postMessage({ type: 'BRIDGE_CLIENT_COOKIE_DELTAS', deltas });
  } catch {
    // ignore
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
