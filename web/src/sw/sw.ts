/// <reference lib="webworker" />

declare const self: ServiceWorkerGlobalScope;

const BYPASS_PATHS = [
  '/portal.js',
  '/portal.js.map',
  '/sw.js',
  '/sw.js.map',
  '/patch.js',
  '/patch.js.map',
  '/favicon.ico',
];

self.addEventListener('install', (event: ExtendableEvent) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', (event: ExtendableEvent) => {
  event.waitUntil(self.clients.claim());
});

// Dedicated communication channel to top-level portal window
let portalPort: MessagePort | null = null;
let activePortalClientId: string | null = null;
let activeNodeId: string | null = null;
let activePortalSecret: string | null = null;
let currentBackendOrigin: string | null = null;
let currentBackendRootDomain: string | null = null;

self.addEventListener('message', (event: ExtendableMessageEvent) => {
  const data = event.data;
  if (data?.type === 'PORTAL_ATTACH') {
    if (data.peerId) activeNodeId = data.peerId;
    if (data.secret !== undefined) activePortalSecret = data.secret;
    if (event.ports && event.ports[0]) {
      portalPort = event.ports[0];
      if (event.source) {
        activePortalClientId = (event.source as Client).id;
      }
    }
  } else if (data?.type === 'NODE_CHANGE') {
    if (data.peerId) activeNodeId = data.peerId;
    if (data.secret !== undefined) activePortalSecret = data.secret;
  }
});

// Helper to find the active top-level portal window client (never selects an iframe)
async function getPortalClient(): Promise<Client | null> {
  if (activePortalClientId) {
    const client = await self.clients.get(activePortalClientId);
    if (client) return client;
  }
  const allClients = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
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

self.addEventListener('fetch', (event: FetchEvent) => {
  const url = new URL(event.request.url);

  // 1. Check bypass rules:
  // Tunnel same-origin requests, proxy paths (/--p2p-proxy/), backend origin, sibling subdomains, and media streams
  const isProxyPath = url.pathname.startsWith('/--p2p-proxy/');
  const isTargetOrigin = currentBackendOrigin && url.origin === currentBackendOrigin;
  const isSiblingDomain = currentBackendRootDomain && (
    url.hostname === currentBackendRootDomain ||
    url.hostname.endsWith('.' + currentBackendRootDomain)
  );

  const brand = currentBackendRootDomain ? currentBackendRootDomain.split('.')[0] : '';
  const brandShort = brand.length > 4 ? brand.slice(0, 4).toLowerCase() : brand.toLowerCase();
  const isRelatedBrand = brandShort ? url.hostname.toLowerCase().includes(brandShort) : false;
  const lowerPath = url.pathname.toLowerCase();
  const isMedia = lowerPath.includes('.m4s') || lowerPath.includes('.mp4') || lowerPath.includes('.ts') ||
                  lowerPath.includes('.m3u8') || lowerPath.includes('.webm') || lowerPath.includes('/upgcxcode/');

  if (url.origin !== self.location.origin && !isProxyPath && !isTargetOrigin && !isSiblingDomain && !isRelatedBrand && !isMedia) {
    return; // Bypass truly external cross-origin requests
  }

  // Bypass portal shell assets
  for (const p of BYPASS_PATHS) {
    if (url.pathname === p) {
      return;
    }
  }

  // Bypass initial portal entry page:
  // Root path requests from top-level window (destination !== 'iframe') bypass to fetch index.html
  if (url.pathname === '/' && event.request.destination !== 'iframe') {
    return;
  }

  event.respondWith(handleTunneledRequest(event.request));
});

async function handleTunneledRequest(request: Request): Promise<Response> {
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
  if (url.origin !== self.location.origin) {
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
  };

  // Dispatch request to Portal Shell: prioritize dedicated portalPort, fallback to client.postMessage
  if (portalPort) {
    try {
      portalPort.postMessage(initMsg, [messageChannel.port2]);
    } catch {
      portalPort = null; // Channel broken, fallback to getPortalClient
    }
  }

  if (!portalPort) {
    const client = await getPortalClient();
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

        if (msg.headers?.['x-p2p-target-origin']) {
          currentBackendOrigin = msg.headers['x-p2p-target-origin'];
        }
        if (msg.headers?.['x-p2p-target-root-domain']) {
          currentBackendRootDomain = msg.headers['x-p2p-target-root-domain'];
        }

        const contentType = (msg.headers?.['content-type'] || '').toLowerCase();
        const isHtml = contentType.includes('text/html');
        const isWorker = request.destination === 'worker' ||
                         request.destination === 'sharedworker' ||
                         url.pathname.endsWith('.worker.js') ||
                         url.pathname.endsWith('_worker.js') ||
                         url.pathname.includes('/worker');

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
        respHeaders.set('access-control-allow-origin', self.location.origin);
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
            finalStream = injectScriptIntoHtmlStream(stream);
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
function injectScriptIntoHtmlStream(inputStream: ReadableStream<Uint8Array>): ReadableStream<Uint8Array> {
  const reader = inputStream.getReader();
  const metaReferrer = '<meta name="referrer" content="no-referrer">';
  const contextTag = `<script>` +
    `window.__P2P_NODE_ID__ = ${JSON.stringify(activeNodeId || '')};` +
    `window.__P2P_PORTAL_ORIGIN__ = ${JSON.stringify(self.location.origin)};` +
    (activePortalSecret ? `window.__P2P_SECRET__ = ${JSON.stringify(activePortalSecret)};` : '') +
    `</script>`;
  const originTag = currentBackendOrigin
    ? `<script>window.__P2P_TARGET_ORIGIN__ = ${JSON.stringify(currentBackendOrigin)};window.__P2P_TARGET_ROOT_DOMAIN__ = ${JSON.stringify(currentBackendRootDomain || '')};</script>`
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
        let text = decoder.decode(value);
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
