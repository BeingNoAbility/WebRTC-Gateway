/**
 * Tab-Scoped Service Worker Session Registry
 * - Pins `portalOrigin` strictly Fail-Closed to server-injected `self.__P2P_GW_PORTAL_ORIGINS__`.
 * - Binds fetch authorization strictly to authenticated clientId / one-time enterNonce tokens.
 * - Actively garbage-collects closed tabs (`removeSession` / `pruneDeadSessions`) so reloading
 *   Portal never leaks zombie sessions or bricks single-tab static `-dir` navigation.
 * - Zero reliance on a permanently resident `bridge.html` iframe (ephemeral bootstrap model).
 */

import type { BackendMeta } from '../shared/ipc';
import { isBackendMeta, isRecord } from '../shared/ipc';
import { CookieMirror } from './cookie_sync';

export interface TabSession {
  tabId: string;
  portalOrigin: string;
  meta: BackendMeta;
  port: MessagePort | null;
  cookieMirror: CookieMirror;
  clientIds: Set<string>;
  wsByClient: Map<string, Set<string>>;
  attachedAt: number;
}

interface EnterNonceEntry {
  tabId: string;
  expiresAt: number;
}

const ENTER_NONCE_TTL_MS = 30_000;
const MAX_ENTER_NONCES = 64;
const CLIENT_PRUNE_INTERVAL_MS = 30_000;
const ORPHAN_SESSION_GRACE_MS = 10_000;

export function readServerInjectedPortalOrigins(): Set<string> {
  const out = new Set<string>();
  const raw = (globalThis as { __P2P_GW_PORTAL_ORIGINS__?: unknown }).__P2P_GW_PORTAL_ORIGINS__;
  if (!Array.isArray(raw)) return out;
  for (const item of raw) {
    if (typeof item !== 'string') continue;
    try {
      const u = new URL(item);
      if (u.protocol === 'http:' || u.protocol === 'https:') {
        out.add(u.origin);
      }
    } catch {
      // ignore invalid origin
    }
  }
  return out;
}

/**
 * Validates and canonicalizes `/__p2p_gw__/enter` redirect `next` target to a strictly
 * same-origin, non-gateway, percent-encoded ASCII relative URL (`pathname + search + hash`).
 * Eliminates `\t` open-redirect smuggling and non-ASCII `Headers` TypeError crashes.
 */
export function sanitizeEnterNextPath(rawNext: string, reqOrigin: string): string {
  if (!rawNext || typeof rawNext !== 'string') return '/';
  // Reject any ASCII control character (\t, \r, \n, \0, 0x00..0x1f, 0x7f) BEFORE URL parsing!
  for (let i = 0; i < rawNext.length; i++) {
    const c = rawNext.charCodeAt(i);
    if (c < 0x20 || c === 0x7f) {
      return '/';
    }
  }
  if (!rawNext.startsWith('/') || rawNext.startsWith('//') || rawNext.startsWith('/\\')) {
    return '/';
  }
  try {
    const parsed = new URL(rawNext, reqOrigin);
    if (parsed.origin !== reqOrigin) {
      return '/';
    }
    if (parsed.pathname.startsWith('/__p2p_gw__/')) {
      return '/';
    }
    // WHATWG URL guarantees pathname & search are percent-encoded ASCII; encode hash non-ASCII safely.
    let safeHash = parsed.hash;
    if (safeHash) {
      let asciiHash = '#';
      for (const ch of safeHash.slice(1)) {
        const cp = ch.codePointAt(0)!;
        if (cp >= 0x21 && cp <= 0x7e) {
          asciiHash += ch;
        } else {
          asciiHash += encodeURIComponent(ch);
        }
      }
      safeHash = asciiHash;
    }
    return `${parsed.pathname}${parsed.search}${safeHash}`;
  } catch {
    return '/';
  }
}

export class SessionRegistry {
  private sessions = new Map<string, TabSession>();
  private clientToTab = new Map<string, string>();
  private enterNonces = new Map<string, EnterNonceEntry>();
  private pingWaiters = new Map<
    string,
    { resolve: (alive: boolean) => void; timer: ReturnType<typeof setTimeout> }
  >();
  private pingSeq = 0;
  private lastClientPruneAt = 0;
  private pruneInFlight: Promise<void> | null = null;
  private readonly allowedPortalOrigins: Set<string>;

  constructor(allowedPortalOrigins?: Set<string>) {
    this.allowedPortalOrigins = allowedPortalOrigins ?? readServerInjectedPortalOrigins();
  }

  /**
   * Strictly Fail-Closed portalOrigin check: empty allowlist ALWAYS rejects, and `origin`
   * must match `new URL(origin).origin` exactly (blocks CSP `frame-ancestors` injection).
   */
  isAllowedPortalOrigin(origin: string): boolean {
    if (!origin || this.allowedPortalOrigins.size === 0) return false;
    try {
      const u = new URL(origin);
      if (u.origin !== origin) return false;
      if (u.protocol !== 'http:' && u.protocol !== 'https:') return false;
    } catch {
      return false;
    }
    return this.allowedPortalOrigins.has(origin);
  }

  attachPort(tabId: string, portalOrigin: string, meta: BackendMeta, port: MessagePort): boolean {
    if (!this.isAllowedPortalOrigin(portalOrigin)) {
      try {
        port.close();
      } catch {
        // ignore
      }
      return false;
    }

    const existing = this.sessions.get(tabId);
    // Reject mismatched portalOrigin on an existing tabId!
    if (existing && existing.portalOrigin !== portalOrigin) {
      try {
        port.close();
      } catch {
        // ignore
      }
      return false;
    }

    const cookieMirror = existing?.cookieMirror ?? new CookieMirror();
    if (meta.initialCookies && meta.initialCookies.length > 0) {
      cookieMirror.applyDeltas(meta.initialCookies);
    }
    const clientIds = existing?.clientIds ?? new Set<string>();
    const wsByClient = existing?.wsByClient ?? new Map<string, Set<string>>();

    if (existing && existing.port && existing.port !== port) {
      try {
        existing.port.onmessage = null;
        existing.port.close();
      } catch {
        // ignore
      }
    }

    const session: TabSession = {
      tabId,
      portalOrigin,
      meta,
      port,
      cookieMirror,
      clientIds,
      wsByClient,
      attachedAt: Date.now(),
    };

    port.onmessage = (ev: MessageEvent) => {
      const data = ev.data;
      if (!isRecord(data) || typeof data.type !== 'string') return;
      if (data.type === 'BRIDGE_META_UPDATE' && isBackendMeta(data.meta)) {
        session.meta = data.meta;
        if (data.meta.initialCookies && data.meta.initialCookies.length > 0) {
          session.cookieMirror.applyDeltas(data.meta.initialCookies);
        }
      } else if (data.type === 'BRIDGE_PONG' && typeof data.pingId === 'string') {
        const pw = this.pingWaiters.get(data.pingId);
        if (pw) {
          this.pingWaiters.delete(data.pingId);
          clearTimeout(pw.timer);
          pw.resolve(true);
        }
      } else if (data.type === 'PORTAL_PING' && typeof data.pingId === 'string') {
        try {
          port.postMessage({ type: 'PORTAL_PONG', pingId: data.pingId });
        } catch {
          // ignore
        }
      } else if (data.type === 'BRIDGE_TAB_CLOSE') {
        this.removeSession(tabId);
      } else if (data.type === 'BRIDGE_REGISTER_ENTER_NONCE' && typeof data.nonce === 'string') {
        const nonce = data.nonce.trim();
        if (nonce.length >= 16 && nonce.length <= 128) {
          this.registerEnterNonce(nonce, tabId);
          try {
            port.postMessage({ type: 'BRIDGE_ENTER_NONCE_ACK', nonce });
          } catch {
            // ignore
          }
        }
      }
    };
    port.start?.();

    this.sessions.set(tabId, session);
    return true;
  }

  registerEnterNonce(nonce: string, tabId: string, now = Date.now()): void {
    for (const [k, v] of this.enterNonces) {
      if (v.expiresAt <= now) {
        this.enterNonces.delete(k);
      }
    }
    if (this.enterNonces.size >= MAX_ENTER_NONCES) {
      const oldest = this.enterNonces.keys().next().value;
      if (oldest) this.enterNonces.delete(oldest);
    }
    this.enterNonces.set(nonce, { tabId, expiresAt: now + ENTER_NONCE_TTL_MS });
  }

  /**
   * Handles `/__p2p_gw__/enter?tab=<tabId>&nonce=<nonce>&next=<path>`:
   * Validates & consumes one-time `enterNonce`, binds `resultingClientId -> tabId`,
   * and returns a strictly sanitized ASCII relative 302 redirect.
   */
  handleEnterNavigation(
    reqUrl: URL,
    resultingClientId: string,
    clientId: string,
    now = Date.now()
  ): Response {
    const tabId = (reqUrl.searchParams.get('tab') || '').trim();
    const nonce = (reqUrl.searchParams.get('nonce') || '').trim();

    if (!tabId || !nonce) {
      return new Response('Forbidden: missing enter token', {
        status: 403,
        headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
      });
    }
    const entry = this.enterNonces.get(nonce);
    // Always consume nonce on lookup (single-use!).
    this.enterNonces.delete(nonce);
    if (!entry || entry.tabId !== tabId || entry.expiresAt <= now || !this.sessions.has(tabId)) {
      return new Response('Forbidden: invalid or expired enter token', {
        status: 403,
        headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
      });
    }

    const rawNext = reqUrl.searchParams.get('next') || '/';
    const safeNext = sanitizeEnterNextPath(rawNext, reqUrl.origin);

    if (resultingClientId) this.bindClient(resultingClientId, tabId);
    if (clientId) this.bindClient(clientId, tabId);

    return new Response(null, {
      status: 302,
      headers: {
        location: safeNext,
        'cache-control': 'no-store',
      },
    });
  }

  bindClient(clientId: string, tabId: string): void {
    if (!clientId || !tabId) return;
    const prevTab = this.clientToTab.get(clientId);
    if (prevTab && prevTab !== tabId) {
      this.sessions.get(prevTab)?.clientIds.delete(clientId);
    }
    this.clientToTab.set(clientId, tabId);
    this.sessions.get(tabId)?.clientIds.add(clientId);
    if (this.clientToTab.size > 512) {
      const firstKey = this.clientToTab.keys().next().value;
      if (firstKey) {
        const oldTab = this.clientToTab.get(firstKey);
        this.clientToTab.delete(firstKey);
        if (oldTab) this.sessions.get(oldTab)?.clientIds.delete(firstKey);
      }
    }
  }

  isBoundClient(clientId: string): boolean {
    if (!clientId) return false;
    const mapped = this.clientToTab.get(clientId);
    return Boolean(mapped && this.sessions.has(mapped));
  }

  /**
   * Resolves a session ONLY via explicit `clientId` / `resultingClientId` binding
   * (zero `x-p2p-tab-id` trust, zero blind `lastTabId` fallback!).
   */
  getBoundSession(clientId: string, resultingClientId: string): TabSession | null {
    if (clientId) {
      const mapped = this.clientToTab.get(clientId);
      if (mapped) {
        const s = this.sessions.get(mapped);
        if (s) {
          if (resultingClientId) this.bindClient(resultingClientId, mapped);
          return s;
        }
      }
    }
    if (resultingClientId) {
      const mapped = this.clientToTab.get(resultingClientId);
      if (mapped) {
        const s = this.sessions.get(mapped);
        if (s) return s;
      }
    }
    return null;
  }

  getSessionByTabId(tabId: string): TabSession | null {
    return this.sessions.get(tabId) ?? null;
  }

  getActiveSessionCount(): number {
    return this.sessions.size;
  }

  /**
   * Returns the single active TabSession ONLY when `sessions.size === 1`.
   * On multi-tab fixed domains (`sessions.size !== 1`), returns `null` (never guesses ownership!).
   */
  getSingleActiveSession(): TabSession | null {
    if (this.sessions.size !== 1) return null;
    return this.sessions.values().next().value ?? null;
  }

  removeSession(tabId: string): void {
    const sess = this.sessions.get(tabId);
    if (!sess) return;
    this.sessions.delete(tabId);
    if (sess.port) {
      const old = sess.port;
      sess.port = null;
      try {
        old.onmessage = null;
        old.close();
      } catch {
        // ignore
      }
    }
    for (const cid of sess.clientIds) {
      if (this.clientToTab.get(cid) === tabId) {
        this.clientToTab.delete(cid);
      }
    }
    sess.clientIds.clear();
    sess.wsByClient.clear();
    for (const [nonce, entry] of this.enterNonces) {
      if (entry.tabId === tabId) {
        this.enterNonces.delete(nonce);
      }
    }
  }

  registerClientWebSocket(session: TabSession, clientId: string, wsId: string): void {
    if (!clientId || !wsId) return;
    let set = session.wsByClient.get(clientId);
    if (!set) {
      set = new Set<string>();
      session.wsByClient.set(clientId, set);
    }
    set.add(wsId);
  }

  async pruneDeadClientWebSockets(swScope: ServiceWorkerGlobalScope): Promise<void> {
    for (const sess of this.sessions.values()) {
      for (const [cid, wsIds] of Array.from(sess.wsByClient.entries())) {
        const client = await swScope.clients.get(cid).catch(() => undefined);
        if (!client) {
          sess.wsByClient.delete(cid);
          sess.clientIds.delete(cid);
          this.clientToTab.delete(cid);
          for (const wsId of wsIds) {
            try {
              sess.port?.postMessage({ type: 'TUNNEL_WS_ABORT', wsId });
            } catch {
              // ignore
            }
          }
        }
      }
    }
  }

  /**
   * Probes existing sessions (optionally excluding a freshly attached tabId) and deletes any
   * whose Portal bridgePort is dead and no longer responds to `BRIDGE_PING`.
   * Single-flight deduplicated so concurrent callers share one active sweep.
   */
  pruneDeadSessions(
    swScope: ServiceWorkerGlobalScope,
    excludeTabId = ''
  ): Promise<void> {
    if (this.pruneInFlight) {
      return this.pruneInFlight;
    }
    const run = (async () => {
      await this.pruneDeadClientWebSockets(swScope);
      for (const [tabId, sess] of Array.from(this.sessions.entries())) {
        if (tabId === excludeTabId) continue;
        for (const cid of Array.from(sess.clientIds)) {
          const client = await swScope.clients.get(cid).catch(() => undefined);
          if (!client) {
            sess.clientIds.delete(cid);
            if (this.clientToTab.get(cid) === tabId) {
              this.clientToTab.delete(cid);
            }
          }
        }
        if (!sess.port) {
          this.removeSession(tabId);
          continue;
        }
        const alive = await this.pingBridgePort(sess, 500);
        if (!alive && this.sessions.get(tabId) === sess) {
          this.removeSession(tabId);
        }
      }
    })();
    this.pruneInFlight = run.finally(() => {
      if (this.pruneInFlight === run) {
        this.pruneInFlight = null;
      }
    });
    return this.pruneInFlight;
  }

  async maybePruneClosedClients(swScope: ServiceWorkerGlobalScope): Promise<void> {
    const now = Date.now();
    if (now - this.lastClientPruneAt < CLIENT_PRUNE_INTERVAL_MS) return;
    this.lastClientPruneAt = now;
    await this.pruneDeadClientWebSockets(swScope);
    for (const [cid, tabId] of Array.from(this.clientToTab.entries())) {
      const client = await swScope.clients.get(cid).catch(() => undefined);
      if (!client) {
        this.clientToTab.delete(cid);
        this.sessions.get(tabId)?.clientIds.delete(cid);
      }
    }
    for (const [tabId, sess] of Array.from(this.sessions.entries())) {
      if (sess.clientIds.size > 0) continue;
      if (!sess.port) {
        this.removeSession(tabId);
        continue;
      }
      if (now - sess.attachedAt >= ORPHAN_SESSION_GRACE_MS) {
        const alive = await this.pingBridgePort(sess, 500);
        if (!alive && this.sessions.get(tabId) === sess) {
          this.removeSession(tabId);
        }
      }
    }
  }

  pingBridgePort(session: TabSession, timeoutMs = 1_500): Promise<boolean> {
    if (!session.port) return Promise.resolve(false);
    const pingId = `p_${++this.pingSeq}`;
    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => {
        this.pingWaiters.delete(pingId);
        resolve(false);
      }, timeoutMs);
      this.pingWaiters.set(pingId, { resolve, timer });
      try {
        session.port!.postMessage({ type: 'BRIDGE_PING', pingId });
      } catch {
        clearTimeout(timer);
        this.pingWaiters.delete(pingId);
        resolve(false);
      }
    });
  }

  /**
   * Detaches a dead MessagePort and removes the session when no live port can serve it.
   */
  evictDeadPort(tabId: string, deadPort: MessagePort | null): void {
    const cur = this.sessions.get(tabId);
    if (!cur) return;
    if (deadPort === null || cur.port === deadPort) {
      this.removeSession(tabId);
    }
  }

  /**
   * Checks whether `clientId` belongs to a live same-origin nested `appFrame` or worker
   * (never `/__p2p_gw__/*`, never top-level, never cross-site). Used ONLY to decide whether
   * to emit a `P2P_SW_EVICTED` recovery hint when the SW wakes up with `sessions.size === 0`.
   */
  async isLiveNestedAppClient(
    swScope: ServiceWorkerGlobalScope,
    clientId: string
  ): Promise<Client | null> {
    if (!clientId || !swScope?.clients || typeof swScope.clients.get !== 'function') {
      return null;
    }
    let caller: Client | undefined;
    try {
      caller = await swScope.clients.get(clientId);
    } catch {
      return null;
    }
    if (!caller || typeof caller.url !== 'string') return null;
    try {
      const cu = new URL(caller.url);
      if (cu.origin !== swScope.location.origin) return null;
      if (cu.pathname.startsWith('/__p2p_gw__/')) return null;
    } catch {
      return null;
    }
    if (caller.type === 'window') {
      return (caller as WindowClient).frameType === 'nested' ? caller : null;
    }
    if (caller.type === 'worker' || caller.type === 'sharedworker') {
      return caller;
    }
    return null;
  }
}
