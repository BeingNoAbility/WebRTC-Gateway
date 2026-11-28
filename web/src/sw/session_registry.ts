/**
 * Tab-Scoped Service Worker Session Registry
 * - Pins `portalOrigin` strictly Fail-Closed to server-injected `self.__P2P_GW_PORTAL_ORIGINS__`.
 * - Per-tab `clientToTab` quotas (64/tab, 512 global) that NEVER evict any session's root `appFrame`.
 * - One-time 128-bit `enterNonce` bound to exact `(tabId, nextPath)` + `GET` + `navigate` + `iframe`.
 * - Supports W3C `replacesClientId` so Back/Forward/Reload inherits bound session cleanly without
 *   referrer heuristics.
 * - Background-tab-safe 3000ms `BRIDGE_PING` timeout.
 */

import type { BackendMeta, CookieDelta } from '../shared/ipc';
import { isBackendMeta, isCookieDelta, isRecord } from '../shared/ipc';
import { CookieMirror } from './cookie_sync';

export interface TabSession {
  tabId: string;
  portalOrigin: string;
  meta: BackendMeta;
  port: MessagePort | null;
  cookieMirror: CookieMirror;
  rootClientId: string;
  expectRootNavUntil: number;
  clientIds: Set<string>;
  wsByClient: Map<string, Set<string>>;
  attachedAt: number;
}

interface EnterNonceEntry {
  tabId: string;
  nextPath: string;
  expiresAt: number;
}

export interface EnterNavCheck {
  method?: string;
  mode?: string;
  destination?: string;
  now?: number;
}

const ENTER_NONCE_TTL_MS = 30_000;
const EXPECT_ROOT_NAV_TTL_MS = 3_000;
const MAX_ENTER_NONCES = 64;
const CLIENT_PRUNE_INTERVAL_MS = 30_000;
const ORPHAN_SESSION_GRACE_MS = 10_000;
export const BRIDGE_PING_TIMEOUT_MS = 3_000;
export const MAX_CLIENTS_PER_TAB = 64;
export const MAX_TOTAL_CLIENTS = 512;

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
  private engineSupportsReplacesClientId = false;
  private readonly allowedPortalOrigins: Set<string>;
  private readonly onBridgeCookieDeltas?: (session: TabSession, deltas: CookieDelta[]) => void;

  constructor(
    allowedPortalOrigins?: Set<string>,
    opts?: { onBridgeCookieDeltas?: (session: TabSession, deltas: CookieDelta[]) => void }
  ) {
    this.allowedPortalOrigins = allowedPortalOrigins ?? readServerInjectedPortalOrigins();
    this.onBridgeCookieDeltas = opts?.onBridgeCookieDeltas;
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
    const rootClientId = existing?.rootClientId ?? '';

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
      rootClientId,
      expectRootNavUntil: existing?.expectRootNavUntil ?? 0,
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
      } else if (data.type === 'BRIDGE_COOKIE_DELTAS' && Array.isArray(data.deltas)) {
        const valid = data.deltas.filter(isCookieDelta);
        if (valid.length > 0) {
          session.cookieMirror.applyDeltas(valid);
          this.onBridgeCookieDeltas?.(session, valid);
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
        const nextPath = typeof data.nextPath === 'string' ? data.nextPath : '/';
        if (nonce.length >= 16 && nonce.length <= 128) {
          this.registerEnterNonce(nonce, tabId, nextPath);
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

  registerEnterNonce(nonce: string, tabId: string, nextPath = '/', now = Date.now()): void {
    for (const [k, v] of this.enterNonces) {
      if (v.expiresAt <= now) {
        this.enterNonces.delete(k);
      }
    }
    if (this.enterNonces.size >= MAX_ENTER_NONCES) {
      const oldest = this.enterNonces.keys().next().value;
      if (oldest) this.enterNonces.delete(oldest);
    }
    const canonNext = sanitizeEnterNextPath(nextPath, 'https://sandbox.invalid');
    this.enterNonces.set(nonce, { tabId, nextPath: canonNext, expiresAt: now + ENTER_NONCE_TTL_MS });
  }

  /**
   * Handles `/__p2p_gw__/enter?tab=<tabId>&nonce=<nonce>&next=<path>`:
   * Enforces GET + mode==='navigate' + destination==='iframe', validates & consumes the
   * one-time `enterNonce` bound to exact `(tabId, nextPath)`, marks `resultingClientId` as the
   * protected root client of `tabId`, and returns a strictly sanitized ASCII relative 302.
   */
  handleEnterNavigation(
    reqUrl: URL,
    resultingClientId: string,
    clientId: string,
    check: EnterNavCheck = {}
  ): Response {
    const now = check.now ?? Date.now();
    if (check.method && check.method.toUpperCase() !== 'GET') {
      return new Response('Forbidden: enter requires GET', {
        status: 403,
        headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
      });
    }
    if (check.mode && check.mode !== 'navigate') {
      return new Response('Forbidden: enter requires navigation request', {
        status: 403,
        headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
      });
    }
    if (check.destination && check.destination !== 'iframe') {
      return new Response('Forbidden: enter requires iframe navigation', {
        status: 403,
        headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
      });
    }

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
    if (entry.nextPath && safeNext !== entry.nextPath) {
      return new Response('Forbidden: enter target mismatch', {
        status: 403,
        headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
      });
    }

    if (resultingClientId) this.bindClient(resultingClientId, tabId, true);
    if (clientId && clientId !== resultingClientId) this.bindClient(clientId, tabId, false);

    return new Response(null, {
      status: 302,
      headers: {
        location: safeNext,
        'cache-control': 'no-store',
      },
    });
  }

  /**
   * Binds `clientId -> tabId` with per-tab quotas (`MAX_CLIENTS_PER_TAB = 64`) and global
   * caps (`MAX_TOTAL_CLIENTS = 512`), NEVER evicting any active session's `rootClientId` or
   * allowing one tab to evict another tab's clients under shared sandbox origins.
   */
  bindClient(clientId: string, tabId: string, isRoot = false): void {
    if (!clientId || !tabId) return;
    const sess = this.sessions.get(tabId);
    if (!sess) return;

    const prevTab = this.clientToTab.get(clientId);
    if (prevTab && prevTab !== tabId) {
      const prevSess = this.sessions.get(prevTab);
      if (prevSess) {
        prevSess.clientIds.delete(clientId);
        if (prevSess.rootClientId === clientId) {
          prevSess.rootClientId = '';
        }
      }
    }

    if (isRoot) {
      sess.rootClientId = clientId;
    }

    // Enforce per-tab quota strictly within THIS tab (never touches sibling tabs!).
    if (!sess.clientIds.has(clientId)) {
      while (sess.clientIds.size >= MAX_CLIENTS_PER_TAB) {
        if (!this.evictOldestNonRootInSession(sess)) break;
      }
    }

    // Refresh insertion order.
    this.clientToTab.delete(clientId);
    this.clientToTab.set(clientId, tabId);
    sess.clientIds.delete(clientId);
    sess.clientIds.add(clientId);

    // Enforce global ceiling by evicting non-root clients from the largest tab only.
    while (this.clientToTab.size > MAX_TOTAL_CLIENTS) {
      let largest: TabSession | null = null;
      let largestNonRoot = 0;
      for (const candidate of this.sessions.values()) {
        const nonRoot = candidate.clientIds.size - (candidate.rootClientId && candidate.clientIds.has(candidate.rootClientId) ? 1 : 0);
        if (nonRoot > largestNonRoot) {
          largestNonRoot = nonRoot;
          largest = candidate;
        }
      }
      if (!largest || !this.evictOldestNonRootInSession(largest)) {
        break;
      }
    }
  }

  private evictOldestNonRootInSession(sess: TabSession): boolean {
    for (const cid of sess.clientIds) {
      if (cid === sess.rootClientId) continue;
      sess.clientIds.delete(cid);
      if (this.clientToTab.get(cid) === sess.tabId) {
        this.clientToTab.delete(cid);
      }
      return true;
    }
    return false;
  }

  isBoundClient(clientId: string): boolean {
    if (!clientId) return false;
    const mapped = this.clientToTab.get(clientId);
    return Boolean(mapped && this.sessions.has(mapped));
  }

  /**
   * Records a short-lived (3s), single-use root-frame navigation expectation from an already-bound
   * `appFrame` client (`beforeunload`). Ignored once the browser engine is confirmed to support
   * W3C `FetchEvent.replacesClientId` (Chromium).
   */
  markExpectedRootNav(clientId: string, now = Date.now()): boolean {
    if (this.engineSupportsReplacesClientId || !clientId) return false;
    const mapped = this.clientToTab.get(clientId);
    if (!mapped) return false;
    const s = this.sessions.get(mapped);
    if (!s) return false;
    if (s.rootClientId && s.rootClientId !== clientId) return false;
    s.expectRootNavUntil = now + EXPECT_ROOT_NAV_TTL_MS;
    return true;
  }

  /**
   * Immediately disarms any pending root-nav expectation when `appFrame` settles (`pageshow`,
   * including BFCache restore) and restores `rootClientId` if a bound top-level frame returned
   * from BFCache.
   */
  clearExpectedRootNav(clientId: string): void {
    if (!clientId) return;
    const mapped = this.clientToTab.get(clientId);
    if (!mapped) return;
    const s = this.sessions.get(mapped);
    if (!s) return;
    s.expectRootNavUntil = 0;
    s.rootClientId = clientId;
  }

  /**
   * Consumes a pending single-use root navigation expectation ONLY on engines lacking W3C
   * `replacesClientId` and ONLY when unambiguously matched by exactly one active session.
   */
  consumeExpectedRootNav(now = Date.now()): TabSession | null {
    if (this.engineSupportsReplacesClientId) return null;
    let matched: TabSession | null = null;
    for (const s of this.sessions.values()) {
      if (s.expectRootNavUntil > now) {
        if (matched) return null; // Ambiguous across multiple tabs -> Fail-Closed.
        matched = s;
      }
    }
    if (!matched) return null;
    matched.expectRootNavUntil = 0;
    return matched;
  }

  /**
   * Resolves a session ONLY via explicit `clientId`, pre-bound `resultingClientId`
   * (from `/__p2p_gw__/enter`), or W3C `replacesClientId` (iframe Back/Forward/Reload).
   */
  getBoundSession(
    clientId: string,
    resultingClientId: string,
    replacesClientId = '',
    isIframeNavigate = false,
    now = Date.now()
  ): TabSession | null {
    if (replacesClientId !== '') {
      this.engineSupportsReplacesClientId = true;
    }
    if (clientId) {
      const mapped = this.clientToTab.get(clientId);
      if (mapped) {
        const s = this.sessions.get(mapped);
        if (s) {
          const hadPendingRootNav = s.expectRootNavUntil > now;
          if (isIframeNavigate || replacesClientId !== '') {
            s.expectRootNavUntil = 0;
          }
          if (resultingClientId) {
            let inheritsRoot = false;
            if (replacesClientId !== '') {
              inheritsRoot = replacesClientId === s.rootClientId;
            } else if (
              isIframeNavigate &&
              !this.engineSupportsReplacesClientId &&
              hadPendingRootNav &&
              (clientId === s.rootClientId || !s.rootClientId)
            ) {
              inheritsRoot = true;
            }
            this.bindClient(resultingClientId, mapped, inheritsRoot);
          }
          return s;
        }
      }
    }
    if (resultingClientId) {
      const mapped = this.clientToTab.get(resultingClientId);
      if (mapped) {
        const s = this.sessions.get(mapped);
        if (s) {
          if (isIframeNavigate) s.expectRootNavUntil = 0;
          return s;
        }
      }
    }
    if (replacesClientId) {
      const mapped = this.clientToTab.get(replacesClientId);
      if (mapped) {
        const s = this.sessions.get(mapped);
        if (s) {
          s.expectRootNavUntil = 0;
          if (resultingClientId) {
            this.bindClient(resultingClientId, mapped, replacesClientId === s.rootClientId);
          }
          return s;
        }
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

  getSingleActiveSession(): TabSession | null {
    if (this.sessions.size !== 1) return null;
    return this.sessions.values().next().value ?? null;
  }

  /**
   * Returns a read-only static (`-dir`/`-file`) session for opaque-origin iframe links where
   * `CSP: sandbox` omits `clientId` and `replacesClientId`. Permits multiple Portal tabs viewing
   * the SAME static `nodeId` (choosing the most recently attached tab), while failing closed
   * (`null`) if any active session is non-static or targets a different `nodeId`.
   */
  getStaticFallbackSession(): TabSession | null {
    if (this.sessions.size === 0) return null;
    let chosen: TabSession | null = null;
    let sharedNodeId = '';
    for (const sess of this.sessions.values()) {
      if (!sess.meta.isStaticMode) return null;
      if (!sharedNodeId) {
        sharedNodeId = sess.meta.nodeId;
      } else if (sess.meta.nodeId !== sharedNodeId) {
        return null;
      }
      if (!chosen || sess.attachedAt >= chosen.attachedAt) {
        chosen = sess;
      }
    }
    return chosen;
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
    sess.rootClientId = '';
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
          if (sess.rootClientId === cid) sess.rootClientId = '';
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

  pruneDeadSessions(
    swScope: ServiceWorkerGlobalScope,
    excludeTabId = '',
    pingTimeoutMs = BRIDGE_PING_TIMEOUT_MS
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
            if (sess.rootClientId === cid) sess.rootClientId = '';
            if (this.clientToTab.get(cid) === tabId) {
              this.clientToTab.delete(cid);
            }
          }
        }
        if (!sess.port) {
          this.removeSession(tabId);
          continue;
        }
        const alive = await this.pingBridgePort(sess, pingTimeoutMs);
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
        const s = this.sessions.get(tabId);
        if (s) {
          s.clientIds.delete(cid);
          if (s.rootClientId === cid) s.rootClientId = '';
        }
      }
    }
    for (const [tabId, sess] of Array.from(this.sessions.entries())) {
      if (sess.clientIds.size > 0) continue;
      if (!sess.port) {
        this.removeSession(tabId);
        continue;
      }
      if (now - sess.attachedAt >= ORPHAN_SESSION_GRACE_MS) {
        const alive = await this.pingBridgePort(sess, BRIDGE_PING_TIMEOUT_MS);
        if (!alive && this.sessions.get(tabId) === sess) {
          this.removeSession(tabId);
        }
      }
    }
  }

  pingBridgePort(session: TabSession, timeoutMs = BRIDGE_PING_TIMEOUT_MS): Promise<boolean> {
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

  evictDeadPort(tabId: string, deadPort: MessagePort | null): void {
    const cur = this.sessions.get(tabId);
    if (!cur) return;
    if (deadPort === null || cur.port === deadPort) {
      this.removeSession(tabId);
    }
  }

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

  async findAnyLiveNestedAppClient(swScope: ServiceWorkerGlobalScope): Promise<Client | null> {
    if (!swScope?.clients || typeof swScope.clients.matchAll !== 'function') {
      return null;
    }
    let list: readonly Client[];
    try {
      list = await swScope.clients.matchAll({ type: 'window', includeUncontrolled: true });
    } catch {
      return null;
    }
    for (const c of list) {
      if (typeof c.url !== 'string') continue;
      try {
        const cu = new URL(c.url);
        if (cu.origin !== swScope.location.origin) continue;
        if (cu.pathname.startsWith('/__p2p_gw__/')) continue;
      } catch {
        continue;
      }
      if ((c as WindowClient).frameType === 'nested') {
        return c;
      }
    }
    return null;
  }
}
