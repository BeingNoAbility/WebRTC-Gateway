/// <reference lib="webworker" />

const sw = self as unknown as ServiceWorkerGlobalScope;

const BYPASS_PATHS = [
  '/portal.js',
  '/portal.js.map',
  '/sw.js',
  '/sw.js.map',
  '/patch.js',
  '/patch.js.map',
  '/favicon.ico',
  '/favicon.svg',
];

sw.addEventListener('install', (event: ExtendableEvent) => {
  event.waitUntil(sw.skipWaiting());
});

sw.addEventListener('activate', (event: ExtendableEvent) => {
  event.waitUntil(sw.clients.claim());
});

// Dedicated multi-tab session management: isolate node ID, ports, and origins per portal tab
interface PortalSession {
  clientId: string;
  port: MessagePort | null;
  nodeId: string;
  backendOrigin?: string;
  backendRootDomain?: string;
  lastActive: number;
}

const sessions = new Map<string, PortalSession>();
const iframeToPortalMap = new Map<string, string>();
let lastActiveSession: PortalSession | null = null;

sw.addEventListener('message', (event: ExtendableMessageEvent) => {
  const data = event.data;
  const sourceClient = event.source as Client | null;
  const clientId = sourceClient?.id;

  if (data?.type === 'PORTAL_ATTACH') {
    const port = event.ports && event.ports[0];
    const peerId = data.peerId || '';

    // Active GC: Sweep and purge dead sessions from previous page loads/refreshes
    (async () => {
      try {
        const livingClients = await sw.clients.matchAll({ type: 'window' });
        const livingIds = new Set(livingClients.map((c) => c.id));
        for (const id of sessions.keys()) {
          if (!id.startsWith('anon_') && !livingIds.has(id)) {
            sessions.delete(id);
          }
        }
        for (const [childId, parentId] of iframeToPortalMap.entries()) {
          if (!livingIds.has(parentId)) {
            iframeToPortalMap.delete(childId);
          }
        }
      } catch {}
    })();

    if (clientId) {
      let session = sessions.get(clientId);
      if (!session) {
        session = {
          clientId,
          port: port || null,
          nodeId: peerId,
          lastActive: Date.now(),
        };
        sessions.set(clientId, session);
      } else {
        if (port) session.port = port;
        if (peerId) session.nodeId = peerId;
        session.lastActive = Date.now();
      }
      lastActiveSession = session;
    } else if (port) {
      const anonId = 'anon_' + Math.random().toString(36).substring(2);
      const session: PortalSession = {
        clientId: anonId,
        port,
        nodeId: peerId,
        lastActive: Date.now(),
      };
      sessions.set(anonId, session);
      lastActiveSession = session;
    }
  } else if (data?.type === 'NODE_CHANGE') {
    const peerId = data.peerId || '';
    if (clientId && sessions.has(clientId)) {
      const session = sessions.get(clientId)!;
      session.nodeId = peerId;
      session.lastActive = Date.now();
      lastActiveSession = session;
    } else if (lastActiveSession) {
      lastActiveSession.nodeId = peerId;
      lastActiveSession.lastActive = Date.now();
    }
  }
});

// Resolve the owning PortalSession for a given client ID (mapping iframes back to their parent portal tab)
function resolveSession(clientId: string): PortalSession | null {
  // 1. Direct match in iframeToPortalMap (subresources or navigations originating from a child iframe or worker)
  if (clientId && iframeToPortalMap.has(clientId)) {
    const portalId = iframeToPortalMap.get(clientId)!;
    const session = sessions.get(portalId);
    if (session) {
      return session;
    }
  }

  // 2. Direct match in sessions (requests initiated directly by a top-level portal tab)
  if (clientId && sessions.has(clientId)) {
    return sessions.get(clientId)!;
  }

  // 3. Fallback: if only one portal session exists, use it
  if (sessions.size === 1) {
    const session = sessions.values().next().value;
    if (session) {
      return session;
    }
  }

  // 4. Fallback: select the most recently active portal session (without mutating lastActive here)
  if (sessions.size > 1) {
    let best: PortalSession | null = null;
    for (const s of sessions.values()) {
      if (!best || s.lastActive > best.lastActive) {
        best = s;
      }
    }
    if (best) {
      return best;
    }
  }

  return lastActiveSession;
}

// Helper to find the active top-level portal window client (never selects an iframe)
async function getPortalClient(preferredClientId?: string): Promise<Client | null> {
  if (preferredClientId) {
    const client = await sw.clients.get(preferredClientId);
    if (client) return client;
  }
  const allClients = await sw.clients.matchAll({ type: 'window', includeUncontrolled: true });
  // 1. Strictly prioritize top-level browsing context (the portal shell)
  for (const client of allClients) {
    const wc = client as WindowClient;
    if (wc.frameType === 'top-level') {
      return client;
    }
  }
  // 2. Fallback to matching query/hash
  for (const client of allClients) {
    if (client.url.includes('?peer=') || client.url.includes('#') || allClients.length === 1) {
      return client;
    }
  }
  return allClients.length > 0 ? allClients[0] : null;
}

sw.addEventListener('fetch', (event: FetchEvent) => {
  const request = event.request;
  const url = new URL(request.url);

  // Maintain parent-child client mapping across iframe navigation cascade and Web Workers
  const isChildContext = (request.mode === 'navigate' ||
                          request.destination === 'worker' ||
                          request.destination === 'sharedworker') &&
                         Boolean(event.resultingClientId);
  if (isChildContext) {
    if (event.clientId && sessions.has(event.clientId)) {
      iframeToPortalMap.set(event.resultingClientId, event.clientId);
    } else if (event.clientId && iframeToPortalMap.has(event.clientId)) {
      const parentPortalId = iframeToPortalMap.get(event.clientId)!;
      iframeToPortalMap.set(event.resultingClientId, parentPortalId);
    }
  }

  const session = resolveSession(event.clientId);

  // 1. Check bypass rules:
  // Tunnel same-origin requests, proxy paths (/--p2p-proxy/), backend origin, sibling subdomains, and media streams
  const isProxyPath = url.pathname.startsWith('/--p2p-proxy/');
  const currentBackendOrigin = session?.backendOrigin;
  const currentBackendRootDomain = session?.backendRootDomain;

  const isTargetOrigin = currentBackendOrigin && url.origin === currentBackendOrigin;
  const isSiblingDomain = currentBackendRootDomain && (
    url.hostname === currentBackendRootDomain ||
    url.hostname.endsWith('.' + currentBackendRootDomain)
  );

  const brand = currentBackendRootDomain ? currentBackendRootDomain.split('.')[0] : '';
  const brandShort = brand.length > 4 ? brand.slice(0, 4).toLowerCase() : brand.toLowerCase();
  const isRelatedBrand = brandShort ? url.hostname.toLowerCase().includes(brandShort) : false;
  const lowerPath = url.pathname.toLowerCase();
  const isMedia = lowerPath.includes('.m4s') || lowerPath.includes('.mp4') || lowerPath.includes('.m4v') ||
                  lowerPath.includes('.webm') || lowerPath.includes('.mkv') || lowerPath.includes('.ts') ||
                  lowerPath.includes('.m3u8') || lowerPath.includes('.mp3') || lowerPath.includes('.wav') ||
                  lowerPath.includes('.flac') || lowerPath.includes('.aac') || lowerPath.includes('.m4a') ||
                  lowerPath.includes('/upgcxcode/');

  if (url.origin !== sw.location.origin && !isProxyPath && !isTargetOrigin && !isSiblingDomain && !isRelatedBrand && !isMedia) {
    return; // Bypass truly external cross-origin requests
  }

  // Bypass portal shell assets
  for (const p of BYPASS_PATHS) {
    if (url.pathname === p) {
      return;
    }
  }

  // Bypass initial portal entry page:
  // ONLY top-level document navigations to root path bypass SW to load the portal index.html.
  // Video, audio, subresources, and Range requests to '/' or any other path are intercepted and tunneled.
  const isTopLevelDocNav = request.mode === 'navigate' &&
                           (request.destination === 'document' || request.destination === '') &&
                           (request.headers.get('accept') || '').includes('text/html');
  if ((url.pathname === '/' || url.pathname === '/index.html') && isTopLevelDocNav) {
    return;
  }

  event.respondWith(handleTunneledRequest(request, session));
});

async function handleTunneledRequest(request: Request, initialSession: PortalSession | null): Promise<Response> {
  let session = initialSession;

  // Lazy GC: Verify that the selected session's client is still alive
  if (session && session.clientId && !session.clientId.startsWith('anon_')) {
    try {
      const isAlive = await sw.clients.get(session.clientId);
      if (!isAlive) {
        sessions.delete(session.clientId);
        for (const [childId, parentId] of iframeToPortalMap.entries()) {
          if (parentId === session.clientId) {
            iframeToPortalMap.delete(childId);
          }
        }
        session = null; // Stale session purged!
      }
    } catch {}
  }

  // If session was invalid, purged, or missing, discover living portal client
  if (!session) {
    const activeClient = await getPortalClient();
    if (activeClient && sessions.has(activeClient.id)) {
      session = sessions.get(activeClient.id)!;
    }
  }

  const messageChannel = new MessageChannel();
  const port = messageChannel.port1;

  // Extract headers
  const headers: Record<string, string> = {};
  request.headers.forEach((val, key) => {
    headers[key.toLowerCase()] = val;
  });

  const url = new URL(request.url);
  let path = url.pathname + url.search;

  // If request targets a sibling domain directly, route via /--p2p-proxy/<host>/<path>
  if (url.origin !== sw.location.origin) {
    path = `/--p2p-proxy/${url.host}${url.pathname}${url.search}`;
    headers['x-p2p-dest-host'] = url.host;
  }

  const hasBody = request.method !== 'GET' && request.method !== 'HEAD';
  const requestId = Math.random().toString(36).substring(2);

  const initMsg = {
    type: 'TUNNEL_FETCH_REQUEST',
    requestId,
    method: request.method,
    path,
    headers,
    hasBody,
    destination: request.destination,
  };

  // Dispatch request to Portal Shell: prioritize dedicated portalPort for this session
  let dispatched = false;
  if (session && session.port) {
    try {
      session.port.postMessage(initMsg, [messageChannel.port2]);
      dispatched = true;
    } catch {
      session.port = null;
    }
  }

  if (!dispatched) {
    const client = await getPortalClient(session?.clientId);
    if (!client) {
      if (url.pathname === '/' || request.destination === 'document') {
        return fetch(request);
      }
      return new Response(
        '<h3>P2P Gateway: No active tunnel window found. Please open or refresh the portal page.</h3>',
        { status: 503, headers: { 'content-type': 'text/html; charset=utf-8' } }
      );
    }
    client.postMessage(initMsg, [messageChannel.port2]);
  }

  // Stream upload body to portal window if present
  if (hasBody) {
    if (request.body) {
      (async () => {
        try {
          const reader = request.body!.getReader();
          while (true) {
            const { done, value } = await reader.read();
            if (done) {
              port.postMessage({ type: 'REQ_BODY_END' });
              break;
            }
            if (value && value.byteLength > 0) {
              const buf = value.buffer.slice(value.byteOffset, value.byteOffset + value.byteLength);
              port.postMessage({ type: 'REQ_BODY_CHUNK', chunk: buf }, [buf]);
            }
          }
        } catch (err: any) {
          // Fallback: clone request and read ArrayBuffer
          try {
            const cloned = request.clone();
            const ab = await cloned.arrayBuffer();
            if (ab && ab.byteLength > 0) {
              port.postMessage({ type: 'REQ_BODY_CHUNK', chunk: ab }, [ab]);
            }
            port.postMessage({ type: 'REQ_BODY_END' });
          } catch (e: any) {
            port.postMessage({ type: 'REQ_BODY_ERROR', error: err.message });
          }
        }
      })();
    } else {
      port.postMessage({ type: 'REQ_BODY_END' });
    }
  }

  // Await response headers from portal window
  return new Promise<Response>((resolve, reject) => {
    const timeout = setTimeout(() => {
      resolve(
        new Response('P2P Gateway: Gateway Timeout awaiting remote response', {
          status: 504,
          headers: { 'content-type': 'text/plain' },
        })
      );
    }, 30000);

    port.onmessage = (e: MessageEvent) => {
      const msg = e.data;
      if (msg.type === 'RESP_START') {
        clearTimeout(timeout);

        if (session) {
          if (msg.headers?.['x-p2p-target-origin']) {
            session.backendOrigin = msg.headers['x-p2p-target-origin'];
          }
          if (msg.headers?.['x-p2p-target-root-domain']) {
            session.backendRootDomain = msg.headers['x-p2p-target-root-domain'];
          }
        }

        const contentType = (msg.headers?.['content-type'] || '').toLowerCase();
        const isHtml = contentType.includes('text/html');
        // Task 2: Worker script injection strict precision check
        const isWorker = (request.destination === 'worker' ||
                          request.destination === 'sharedworker' ||
                          /(?:\.worker|_worker)\.js$/i.test(url.pathname)) &&
                         contentType.includes('javascript');

        const respHeaders = new Headers();
        if (msg.headers) {
          for (const [k, v] of Object.entries(msg.headers)) {
            try {
              const lk = k.toLowerCase();
              // Strip content-length for modified streams (HTML or Worker script injection)
              // Preserve content-length for media streaming (206 Range), images, and downloads!
              if (lk === 'content-length' && (isHtml || isWorker)) continue;
              // Anti-iframe bypass: strip headers that forbid framing or break virtualization scripts
              if (lk === 'x-frame-options' || lk === 'cross-origin-embedder-policy' || lk === 'cross-origin-opener-policy' ||
                  lk === 'content-security-policy' || lk === 'content-security-policy-report-only') continue;
              respHeaders.set(k, v as string);
            } catch {}
          }
        }

        // Enforce no-referrer policy so external image CDNs with anti-hotlink protection (e.g. Bilibili hdslb.com) succeed
        respHeaders.set('referrer-policy', 'no-referrer');
        // Ensure browser allows controlled browsing context to consume responses without CORS blocks
        respHeaders.set('access-control-allow-origin', sw.location.origin);
        respHeaders.set('access-control-allow-credentials', 'true');

        // Create ReadableStream for response body
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            port.onmessage = (event: MessageEvent) => {
              const dataMsg = event.data;
              switch (dataMsg.type) {
                case 'RESP_CHUNK':
                  if (dataMsg.chunk) {
                    controller.enqueue(new Uint8Array(dataMsg.chunk));
                  }
                  break;
                case 'RESP_END':
                  try {
                    controller.close();
                  } catch {}
                  break;
                case 'RESP_ERROR':
                  try {
                    controller.error(new Error(dataMsg.error || 'Response stream error'));
                  } catch {}
                  break;
              }
            };
          },
          cancel() {
            port.postMessage({ type: 'RESP_CANCEL' });
          },
        });

        // W3C Fetch Spec: Status 204, 205, 304 MUST have a null body; passing a stream throws a TypeError
        const isNullBodyStatus = msg.status === 204 || msg.status === 205 || msg.status === 304;

        // Inject virtualization patch into HTML documents and Web Workers
        let finalStream: ReadableStream<Uint8Array> = stream;
        if (!isNullBodyStatus) {
          if (isHtml) {
            finalStream = injectScriptIntoHtmlStream(stream, session);
          } else if (isWorker) {
            finalStream = injectScriptIntoWorkerStream(stream);
          }
        }

        const responseBody = isNullBodyStatus ? null : finalStream;

        resolve(
          new Response(responseBody, {
            status: msg.status || 200,
            statusText: msg.statusText || 'OK',
            headers: respHeaders,
          })
        );
      } else if (msg.type === 'RESP_ERROR') {
        clearTimeout(timeout);
        resolve(
          new Response(`P2P Gateway Error: ${msg.error}`, {
            status: 502,
            headers: { 'content-type': 'text/plain; charset=utf-8' },
          })
        );
      }
    };
  });
}

// Injects <script src="/patch.js"></script> and <meta name="referrer" content="no-referrer"> into the first chunk of HTML stream
function injectScriptIntoHtmlStream(
  inputStream: ReadableStream<Uint8Array>,
  session: PortalSession | null
): ReadableStream<Uint8Array> {
  const reader = inputStream.getReader();
  const metaReferrer = '<meta name="referrer" content="no-referrer">';
  const activeNodeId = session?.nodeId || '';
  const currentBackendOrigin = session?.backendOrigin || '';
  const currentBackendRootDomain = session?.backendRootDomain || '';

  const contextTag = `<script>` +
    `window.__P2P_NODE_ID__ = ${JSON.stringify(activeNodeId)};` +
    `window.__P2P_PORTAL_ORIGIN__ = ${JSON.stringify(sw.location.origin)};` +
    `</script>`;
  const originTag = currentBackendOrigin
    ? `<script>window.__P2P_TARGET_ORIGIN__ = ${JSON.stringify(currentBackendOrigin)};window.__P2P_TARGET_ROOT_DOMAIN__ = ${JSON.stringify(currentBackendRootDomain)};</script>`
    : '';
  const patchScript = `${metaReferrer}${contextTag}${originTag}<script src="/patch.js"></script>`;
  let injected = false;
  const decoder = new TextDecoder('utf-8');
  const encoder = new TextEncoder();

  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      const { done, value } = await reader.read();
      if (done) {
        controller.close();
        return;
      }

      if (!injected && value) {
        injected = true;
        let text = decoder.decode(value, { stream: true });
        const flushed = decoder.decode();
        if (flushed) text += flushed;

        // Strip any existing restrictive meta referrer tags to ensure no-referrer takes precedence
        text = text.replace(/<meta\s+name=["']referrer["'][^>]*>/gi, '');
        let modified = text;

        if (/<head[^>]*>/i.test(text)) {
          modified = text.replace(/<head[^>]*>/i, (match) => `${match}${patchScript}`);
        } else if (/<html[^>]*>/i.test(text)) {
          modified = text.replace(/<html[^>]*>/i, (match) => `${match}<head>${patchScript}</head>`);
        } else {
          modified = patchScript + text;
        }

        controller.enqueue(encoder.encode(modified));
      } else if (value) {
        controller.enqueue(value);
      }
    },
    cancel() {
      reader.cancel();
    },
  });
}

// Injects importScripts("/patch.js"); into the beginning of worker scripts
function injectScriptIntoWorkerStream(inputStream: ReadableStream<Uint8Array>): ReadableStream<Uint8Array> {
  const reader = inputStream.getReader();
  const patchScript = 'importScripts("/patch.js");\n';
  let injected = false;
  const encoder = new TextEncoder();

  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      const { done, value } = await reader.read();
      if (done) {
        controller.close();
        return;
      }

      if (!injected) {
        injected = true;
        controller.enqueue(encoder.encode(patchScript));
      }
      if (value) {
        controller.enqueue(value);
      }
    },
    cancel(reason) {
      reader.cancel(reason);
    },
  });
}
