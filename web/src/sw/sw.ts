/**
 * V7 Thin Service Worker Entrypoint (/__p2p_gw__/sw.js, scope: '/')
 * - Strictly validates `event.source` on `SW_ATTACH_PORT` is `/__p2p_gw__/bridge.html` AND
 *   pins `portalOrigin` to server-injected `self.__P2P_GW_PORTAL_ORIGINS__`.
 * - Intercepts `/__p2p_gw__/enter?tab=...&nonce=...&next=...` with one-time 128-bit `enterNonce`.
 * - Routes `SW_WS_OPEN` ONLY for bound clients via `ensureLivePort`, carrying V7 `fetchCtx` & `cookieMutations`.
 * - Routes `SW_COOKIE_MUTATE` to tab `CookieMirror` (never acks when `session == null`).
 */

/// <reference lib="webworker" />

import { FetchCtxFlag } from '../protocol/tlv';
import { isBackendMeta, isClientCookieMutation, isRecord } from '../shared/ipc';
import { syncCookieDeltasToClients } from './cookie_sync';
import {
  GATEWAY_ENTER_PATH,
  handleFetchEvent,
  shouldBypassGatewayRequest,
} from './fetch_router';
import { SessionRegistry } from './session_registry';

const sw = self as unknown as ServiceWorkerGlobalScope;
const registry = new SessionRegistry();
let wsSeq = 0;

function isBridgeWindowSource(source: ExtendableMessageEvent['source']): boolean {
  if (!source || !('url' in source) || typeof source.url !== 'string') return false;
  if (source.type !== 'window') return false;
  try {
    const u = new URL(source.url);
    return u.origin === sw.location.origin && u.pathname === '/__p2p_gw__/bridge.html';
  } catch {
    return false;
  }
}

sw.addEventListener('install', (event: ExtendableEvent) => {
  event.waitUntil(sw.skipWaiting());
});

sw.addEventListener('activate', (event: ExtendableEvent) => {
  event.waitUntil(sw.clients.claim());
});

sw.addEventListener('message', (event: ExtendableMessageEvent) => {
  const data = event.data;
  if (!isRecord(data) || typeof data.type !== 'string') return;

  if (data.type === 'SW_ATTACH_PORT') {
    const port = event.ports?.[0];
    if (
      !isBridgeWindowSource(event.source) ||
      !port ||
      typeof data.tabId !== 'string' ||
      !data.tabId ||
      typeof data.portalOrigin !== 'string' ||
      !data.portalOrigin ||
      !isBackendMeta(data.meta)
    ) {
      port?.close();
      return;
    }
    registry.attachPort(data.tabId, data.portalOrigin, data.meta, port);
    return;
  }

  const clientId =
    event.source && typeof (event.source as Client).id === 'string'
      ? (event.source as Client).id
      : '';

  if (data.type === 'SW_WS_OPEN') {
    const wsPort = event.ports?.[0];
    if (!wsPort) return;
    const target = typeof data.target === 'string' ? data.target : '';
    const protocols = Array.isArray(data.protocols)
      ? data.protocols.filter((p): p is string => typeof p === 'string').slice(0, 16)
      : [];
    const inlineMutations = Array.isArray(data.mutations)
      ? data.mutations.filter(isClientCookieMutation)
      : [];

    if (!target || !target.startsWith('/')) {
      try {
        wsPort.postMessage({ type: 'WS_ERROR', message: 'Invalid WebSocket target' });
      } catch {
        // ignore
      }
      wsPort.close();
      return;
    }

    event.waitUntil(
      (async () => {
        await registry.pruneDeadClientWebSockets(sw);
        if (registry.getActiveSessionCount() === 0 && clientId) {
          await registry.recoverColdStartClient(sw, clientId, '');
        }
        const bound = registry.getBoundSession(clientId, '');
        if (!bound) {
          try {
            wsPort.postMessage({ type: 'WS_ERROR', message: 'Forbidden: unbound WebSocket client' });
          } catch {
            // ignore
          }
          wsPort.close();
          return;
        }

        let liveSession = bound;
        try {
          if (!liveSession.port || !(await registry.pingBridgePort(liveSession, 500))) {
            registry.evictDeadPort(liveSession.tabId, liveSession.port);
            liveSession = await registry.ensureLivePort(sw, liveSession.tabId);
          }
        } catch {
          try {
            wsPort.postMessage({ type: 'WS_ERROR', message: 'P2P Gateway session unavailable' });
          } catch {
            // ignore
          }
          wsPort.close();
          return;
        }

        const pendingMuts = liveSession.cookieMirror.drainPendingMutations();
        if (inlineMutations.length > 0) {
          liveSession.cookieMirror.applyClientMutations(inlineMutations, {
            clientId,
            queueForUpstream: false,
          });
        }
        const cookieMutations = pendingMuts.concat(inlineMutations);
        const wsId = `ws_${Date.now().toString(36)}_${(++wsSeq).toString(36)}`;
        registry.registerClientWebSocket(liveSession, clientId, wsId);

        try {
          liveSession.port!.postMessage(
            {
              type: 'TUNNEL_WS_OPEN',
              wsId,
              tabId: liveSession.tabId,
              target,
              protocols,
              fetchCtx: FetchCtxFlag.SAME_ORIGIN_CLIENT,
              cookieMutations,
            },
            [wsPort]
          );
        } catch {
          registry.evictDeadPort(liveSession.tabId, liveSession.port);
          try {
            wsPort.postMessage({ type: 'WS_ERROR', message: 'Bridge port closed' });
          } catch {
            // ignore
          }
          wsPort.close();
        }
      })()
    );
    return;
  }

  if (data.type === 'SW_COOKIE_MUTATE') {
    const rawMuts = Array.isArray(data.mutations) ? data.mutations.filter(isClientCookieMutation) : [];
    const seq = typeof data.seq === 'number' ? data.seq : undefined;
    const ackPort = event.ports?.[0];

    event.waitUntil(
      (async () => {
        if (registry.getActiveSessionCount() === 0 && clientId) {
          await registry.recoverColdStartClient(sw, clientId, '');
        }
        const session = registry.getBoundSession(clientId, '');
        if (!session) {
          // NEVER ack when session is missing so caller retains mutation for inline fetch piggybacking!
          try {
            ackPort?.close();
          } catch {
            // ignore
          }
          return;
        }
        if (rawMuts.length > 0) {
          const deltas = session.cookieMirror.applyClientMutations(rawMuts, {
            clientId,
            seq,
            queueForUpstream: true,
          });
          if (deltas.length > 0) {
            const others = Array.from(session.clientIds).filter((id) => id !== clientId);
            await syncCookieDeltasToClients(sw, '', others, deltas);
          }
        }
        if (ackPort) {
          try {
            ackPort.postMessage({ type: 'COOKIE_MUTATE_ACK', seq });
            ackPort.close();
          } catch {
            // ignore
          }
        }
      })()
    );
  }
});

sw.addEventListener('fetch', (event: FetchEvent) => {
  let reqUrl: URL;
  try {
    reqUrl = new URL(event.request.url);
  } catch {
    return;
  }
  if (reqUrl.origin === sw.location.origin && reqUrl.pathname === GATEWAY_ENTER_PATH) {
    event.respondWith(
      Promise.resolve(
        registry.handleEnterNavigation(reqUrl, event.resultingClientId, event.clientId)
      )
    );
    return;
  }
  if (shouldBypassGatewayRequest(reqUrl, sw.location.origin)) {
    return;
  }
  event.respondWith(handleFetchEvent(sw, registry, event));
});
