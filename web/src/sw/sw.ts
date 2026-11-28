/**
 * Thin Service Worker Entrypoint (/__p2p_gw__/sw.js, scope: '/')
 * - Strictly validates `event.source` on `SW_ATTACH_PORT` is `/__p2p_gw__/bridge.html` AND
 *   pins `portalOrigin` to server-injected `self.__P2P_GW_PORTAL_ORIGINS__`.
 * - Intercepts `/__p2p_gw__/enter` (GET + navigate + iframe + one-time nonce bound to nextPath)
 *   and `/__p2p_gw__/boot.js` (bound appFrame clients only).
 * - Routes `SW_WS_OPEN` ONLY for bound clients with background-tab-safe 3000ms liveness check.
 * - Eliminates untrusted `SW_COOKIE_SYNC`; Portal pushes WS cookie deltas over trusted `bridgePort`.
 */

/// <reference lib="webworker" />

import { FetchCtxFlag } from '../protocol/tlv';
import { isBackendMeta, isClientCookieMutation, isRecord } from '../shared/ipc';
import { broadcastClientCookieDeltas, syncCookieDeltasToClients } from './cookie_sync';
import {
  GATEWAY_BOOT_SCRIPT_PATH,
  GATEWAY_ENTER_PATH,
  handleBootScriptRequest,
  handleFetchEvent,
  shouldBypassGatewayRequest,
} from './fetch_router';
import { BRIDGE_PING_TIMEOUT_MS, SessionRegistry } from './session_registry';

const sw = self as unknown as ServiceWorkerGlobalScope;
const registry = new SessionRegistry(undefined, {
  onBridgeCookieDeltas: (session, deltas) => {
    void syncCookieDeltasToClients(sw, '', session.clientIds, deltas).catch(() => {});
  },
});
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
    if (registry.attachPort(data.tabId, data.portalOrigin, data.meta, port)) {
      event.waitUntil(registry.pruneDeadSessions(sw, data.tabId).catch(() => {}));
    }
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
    const inlineSeq = typeof data.seq === 'number' && Number.isFinite(data.seq) ? data.seq : undefined;

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
        const bound = registry.getBoundSession(clientId, '');
        if (!bound) {
          if (clientId) {
            const liveCaller = await registry.isLiveNestedAppClient(sw, clientId);
            if (liveCaller) {
              try {
                liveCaller.postMessage({ type: 'P2P_SW_EVICTED' });
              } catch {
                // ignore
              }
            }
          }
          try {
            wsPort.postMessage({ type: 'WS_ERROR', message: 'Forbidden: unbound WebSocket client' });
          } catch {
            // ignore
          }
          wsPort.close();
          return;
        }

        if (!bound.port || !(await registry.pingBridgePort(bound, BRIDGE_PING_TIMEOUT_MS))) {
          registry.evictDeadPort(bound.tabId, bound.port);
          const liveCaller = await registry.isLiveNestedAppClient(sw, clientId);
          if (liveCaller) {
            try {
              liveCaller.postMessage({ type: 'P2P_SW_EVICTED' });
            } catch {
              // ignore
            }
          }
          try {
            wsPort.postMessage({ type: 'WS_ERROR', message: 'P2P Gateway session unavailable' });
          } catch {
            // ignore
          }
          wsPort.close();
          return;
        }

        const { mutations: pendingMuts, watermark } = bound.cookieMirror.peekPendingMutations();
        const cookieMutations = pendingMuts.concat(inlineMutations);
        const wsId = `ws_${Date.now().toString(36)}_${(++wsSeq).toString(36)}`;
        registry.registerClientWebSocket(bound, clientId, wsId);

        try {
          bound.port.postMessage(
            {
              type: 'TUNNEL_WS_OPEN',
              wsId,
              tabId: bound.tabId,
              target,
              protocols,
              fetchCtx: FetchCtxFlag.SAME_ORIGIN_CLIENT,
              cookieMutations,
            },
            [wsPort]
          );
          bound.cookieMirror.commitPendingMutations(watermark);
          if (inlineMutations.length > 0) {
            bound.cookieMirror.applyClientMutations(inlineMutations, {
              clientId,
              seq: inlineSeq,
              queueForUpstream: false,
            });
          }
        } catch {
          registry.evictDeadPort(bound.tabId, bound.port);
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

  if (data.type === 'P2P_EXPECT_NAV') {
    registry.markExpectedRootNav(clientId);
    return;
  }

  if (data.type === 'P2P_NAV_SETTLED') {
    registry.clearExpectedRootNav(clientId);
    return;
  }

  if (data.type === 'SW_COOKIE_MUTATE') {
    const rawMuts = Array.isArray(data.mutations) ? data.mutations.filter(isClientCookieMutation) : [];
    const seq = typeof data.seq === 'number' ? data.seq : undefined;
    const ackPort = event.ports?.[0];

    event.waitUntil(
      (async () => {
        const session = registry.getBoundSession(clientId, '');
        if (!session) {
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
            await broadcastClientCookieDeltas(sw, session, clientId, deltas);
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
        registry.handleEnterNavigation(reqUrl, event.resultingClientId, event.clientId, {
          method: event.request.method,
          mode: event.request.mode,
          destination: event.request.destination,
        })
      )
    );
    return;
  }
  if (reqUrl.origin === sw.location.origin && reqUrl.pathname === GATEWAY_BOOT_SCRIPT_PATH) {
    event.respondWith(handleBootScriptRequest(sw, registry, event));
    return;
  }
  if (shouldBypassGatewayRequest(reqUrl, sw.location.origin)) {
    return;
  }
  event.respondWith(handleFetchEvent(sw, registry, event));
});
