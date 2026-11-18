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
  allowedDomains?: string[];
  lastActive: number;
  lastVerified?: number;
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
          lastVerified: Date.now(),
        };
        sessions.set(clientId, session);
      } else {
        if (port) session.port = port;
        if (peerId) session.nodeId = peerId;
        session.lastActive = Date.now();
        session.lastVerified = Date.now();
      }
      lastActiveSession = session;
    } else if (port) {
      const anonId = 'anon_' + Math.random().toString(36).substring(2);
      const session: PortalSession = {
        clientId: anonId,
        port,
        nodeId: peerId,
        lastActive: Date.now(),
        lastVerified: Date.now(),
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

function isDomainAllowed(hostname: string, allowedDomains?: string[], rootDomain?: string): boolean {
  const host = hostname.toLowerCase();
  if (rootDomain && (host === rootDomain || host.endsWith('.' + rootDomain))) {
    return true;
  }
  if (!allowedDomains || allowedDomains.length === 0) {
    return false;
  }
  for (const pattern of allowedDomains) {
    const p = pattern.toLowerCase().trim();
    if (p.startsWith('*.')) {
      const suffix = p.slice(1);
      if (host.endsWith(suffix) || host === p.slice(2)) {
        return true;
      }
    } else if (host === p) {
      return true;
    }
  }
  return false;
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
  // Tunnel same-origin requests, proxy paths (/--p2p-proxy/), backend origin, and whitelisted proxy domains
  const isProxyPath = url.pathname.startsWith('/--p2p-proxy/');
  const currentBackendOrigin = session?.backendOrigin;
  const currentBackendRootDomain = session?.backendRootDomain;

  const isTargetOrigin = currentBackendOrigin && url.origin === currentBackendOrigin;
  const isAllowed = isDomainAllowed(url.hostname, session?.allowedDomains, currentBackendRootDomain);

  if (url.origin !== sw.location.origin && !isProxyPath && !isTargetOrigin && !isAllowed) {
    return; // Bypass non-whitelisted cross-origin requests directly to native visitor network
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

  // Lazy GC: Verify that the selected session's client is still alive (throttled to 5s window to avoid IPC storms)
  const now = Date.now();
  if (session && session.clientId && !session.clientId.startsWith('anon_')) {
    if (!session.lastVerified || now - session.lastVerified > 5000) {
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
        } else {
          session.lastVerified = now;
        }
      } catch {}
    }
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

  // Chromium SW privacy gate workaround: FetchEvent.request.headers omits Accept-Language.
  // Backfill client's real browser language preferences if omitted.
  if (!headers['accept-language'] && typeof navigator !== 'undefined') {
    headers['accept-language'] = (navigator.languages && navigator.languages.length)
      ? navigator.languages.join(', ')
      : (navigator.language || 'zh-CN,zh;q=0.9,en;q=0.8');
  }

  if (typeof DecompressionStream !== 'undefined') {
    headers['x-p2p-client-decompress'] = 'gzip';
  }

  const url = new URL(request.url);
  let path = url.pathname + url.search;

  // If request targets a sibling domain directly, route via /--p2p-proxy/<host>/<path>
  if (url.origin !== sw.location.origin) {
    path = `/--p2p-proxy/${url.host}${url.pathname}${url.search}`;
    headers['x-p2p-dest-host'] = url.host;
  } else {
    // Normalize leading duplicate slashes for same-origin requests (e.g. "//synoscgi.sock/..." -> "/synoscgi.sock/...")
    while (path.startsWith('//')) {
      path = path.substring(1);
    }
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
          if (msg.headers?.['x-p2p-allowed-domains']) {
            const raw = msg.headers['x-p2p-allowed-domains'];
            session.allowedDomains = raw ? raw.split(',').map((d: string) => d.trim()).filter(Boolean) : [];
          }
        }

        const contentType = (msg.headers?.['content-type'] || '').toLowerCase();
        const isHtml = contentType.includes('text/html');
        // Task 2: Worker script injection strict precision check
        const isWorker = (request.destination === 'worker' ||
                          request.destination === 'sharedworker' ||
                          /(?:\.worker|_worker)\.js$/i.test(url.pathname)) &&
                         contentType.includes('javascript');
        const isModuleWorker = isWorker && url.searchParams.get('_p2p_worker') === 'module';

        const p2pEncoding = (msg.headers?.['x-p2p-content-encoding'] || '').toLowerCase();
        const isGzipDecompressed = p2pEncoding === 'gzip' && typeof DecompressionStream !== 'undefined';

        const respHeaders = new Headers();
        if (msg.headers) {
          for (const [k, v] of Object.entries(msg.headers)) {
            try {
              const lk = k.toLowerCase();
              // Strip content-length for modified streams (HTML, Worker script injection, or client-decompressed)
              // Preserve content-length for media streaming (206 Range), images, and downloads!
              if (lk === 'content-length' && (isHtml || isWorker || isGzipDecompressed)) continue;
              // Strip internal p2p header so consumer web apps don't get confused
              if (lk === 'x-p2p-content-encoding') continue;
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
        let isStreamClosed = false;
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            port.onmessage = (event: MessageEvent) => {
              const dataMsg = event.data;
              switch (dataMsg.type) {
                case 'RESP_CHUNK':
                  if (dataMsg.chunk && !isStreamClosed) {
                    try {
                      controller.enqueue(new Uint8Array(dataMsg.chunk));
                    } catch {
                      isStreamClosed = true;
                    }
                  }
                  break;
                case 'RESP_END':
                  if (!isStreamClosed) {
                    isStreamClosed = true;
                    try {
                      controller.close();
                    } catch {}
                  }
                  break;
                case 'RESP_ERROR':
                  if (!isStreamClosed) {
                    isStreamClosed = true;
                    try {
                      controller.error(new Error(dataMsg.error || 'Response stream error'));
                    } catch {}
                  }
                  break;
              }
            };
          },
          cancel() {
            isStreamClosed = true;
            port.postMessage({ type: 'RESP_CANCEL' });
          },
        });

        // W3C Fetch Spec: Status 204, 205, 304 and HEAD requests MUST have a null body; passing a stream throws a TypeError
        const isNullBodyStatus = msg.status === 204 || msg.status === 205 || msg.status === 304 || request.method === 'HEAD';

        // Task 1: Hardware-accelerated streaming decompression
        // Pipe through DecompressionStream before patch injection or returning synthetic Response
        let finalStream: ReadableStream<Uint8Array> = stream;
        if (!isNullBodyStatus && request.method !== 'HEAD' && isGzipDecompressed) {
          finalStream = (stream as any).pipeThrough(new DecompressionStream('gzip')) as ReadableStream<Uint8Array>;
        }

        // Inject virtualization patch into HTML documents and Web Workers
        if (!isNullBodyStatus) {
          if (isHtml) {
            finalStream = injectScriptIntoHtmlStream(finalStream, session);
          } else if (isWorker) {
            finalStream = injectScriptIntoWorkerStream(finalStream, isModuleWorker);
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

// Helper to scan a byte buffer for an ASCII HTML tag opening (case-insensitive) and return the index right after '>'
function findAsciiTagEnd(bytes: Uint8Array, tagName: string): number {
  const target = tagName.toLowerCase();
  const len = bytes.length;
  const tagLen = target.length; // e.g. 4 for "head", 4 for "html"

  for (let i = 0; i <= len - tagLen - 1; i++) {
    if (bytes[i] === 0x3C) { // '<'
      let match = true;
      for (let j = 0; j < tagLen; j++) {
        const b = bytes[i + 1 + j];
        if ((b >= 0x41 && b <= 0x5A ? b + 0x20 : b) !== target.charCodeAt(j)) {
          match = false;
          break;
        }
      }
      if (match) {
        const nextByte = bytes[i + 1 + tagLen];
        // Must be followed by space, tab, newline, '>', or '/'
        if (
          nextByte === 0x3E || // '>'
          nextByte === 0x20 || // ' '
          nextByte === 0x09 || // '\t'
          nextByte === 0x0A || // '\n'
          nextByte === 0x0D || // '\r'
          nextByte === 0x2F    // '/'
        ) {
          // Scan forward for closing '>'
          let inQuote = 0;
          for (let k = i + 1 + tagLen; k < len; k++) {
            const kb = bytes[k];
            if (inQuote) {
              if (kb === inQuote) inQuote = 0;
            } else {
              if (kb === 0x22 || kb === 0x27) {
                inQuote = kb;
              } else if (kb === 0x3E) { // '>'
                return k + 1; // Position right after '>'
              }
            }
          }
        }
      }
    }
  }
  return -1;
}

function concatByteArrays(...arrays: (Uint8Array | undefined)[]): Uint8Array {
  let totalLen = 0;
  for (const arr of arrays) {
    if (arr) totalLen += arr.length;
  }
  const out = new Uint8Array(totalLen);
  let offset = 0;
  for (const arr of arrays) {
    if (arr) {
      out.set(arr, offset);
      offset += arr.length;
    }
  }
  return out;
}

// Injects <script src="/patch.js"></script> and virtualization context into HTML stream using byte-level scanning
function injectScriptIntoHtmlStream(
  inputStream: ReadableStream<Uint8Array>,
  session: PortalSession | null
): ReadableStream<Uint8Array> {
  const reader = inputStream.getReader();
  const metaReferrer = '<meta name="referrer" content="no-referrer">';
  const activeNodeId = session?.nodeId || '';
  const currentBackendOrigin = session?.backendOrigin || '';
  const currentBackendRootDomain = session?.backendRootDomain || '';
  const allowedDomains = session?.allowedDomains || [];

  const contextTag = `<script>` +
    `window.__P2P_NODE_ID__ = ${JSON.stringify(activeNodeId)};` +
    `window.__P2P_PORTAL_ORIGIN__ = ${JSON.stringify(sw.location.origin)};` +
    `window.__P2P_ALLOWED_DOMAINS__ = ${JSON.stringify(allowedDomains)};` +
    `</script>`;
  const originTag = currentBackendOrigin
    ? `<script>window.__P2P_TARGET_ORIGIN__ = ${JSON.stringify(currentBackendOrigin)};window.__P2P_TARGET_ROOT_DOMAIN__ = ${JSON.stringify(currentBackendRootDomain)};</script>`
    : '';
  const patchScript = `${metaReferrer}${contextTag}${originTag}<script src="/patch.js"></script>`;
  let injected = false;
  const encoder = new TextEncoder();
  const initChunks: Uint8Array[] = [];
  let initBytes = 0;

  return new ReadableStream<Uint8Array>({
    async pull(controller) {
      if (!injected) {
        while (true) {
          const { done, value } = await reader.read();
          if (value && value.byteLength > 0) {
            initChunks.push(value);
            initBytes += value.byteLength;
          }

          const combined = initChunks.length === 1 ? initChunks[0] : concatByteArrays(...initChunks);

          // 1. Check if <head> is present in accumulated bytes
          const headPos = findAsciiTagEnd(combined, 'head');
          if (headPos !== -1) {
            injected = true;
            const patchBytes = encoder.encode(patchScript);
            const before = combined.subarray(0, headPos);
            const after = combined.subarray(headPos);
            controller.enqueue(concatByteArrays(before, patchBytes, after));
            if (done) controller.close();
            return;
          }

          // 2. If accumulated bytes >= 2048 or stream ended, evaluate fallback injection
          if (initBytes >= 2048 || done) {
            injected = true;
            const patchBytes = encoder.encode(patchScript);

            const htmlPos = findAsciiTagEnd(combined, 'html');
            if (htmlPos !== -1) {
              const headWrapper = encoder.encode(`<head>${patchScript}</head>`);
              const before = combined.subarray(0, htmlPos);
              const after = combined.subarray(htmlPos);
              controller.enqueue(concatByteArrays(before, headWrapper, after));
            } else {
              const doctypePos = findAsciiTagEnd(combined, '!doctype');
              if (doctypePos !== -1) {
                const headWrapper = encoder.encode(`<head>${patchScript}</head>`);
                const before = combined.subarray(0, doctypePos);
                const after = combined.subarray(doctypePos);
                controller.enqueue(concatByteArrays(before, headWrapper, after));
              } else {
                controller.enqueue(concatByteArrays(patchBytes, combined));
              }
            }

            if (done) controller.close();
            return;
          }

          if (done) {
            injected = true;
            if (combined.byteLength > 0) {
              controller.enqueue(combined);
            }
            controller.close();
            return;
          }
        }
      }

      // Subsequent chunks pass through directly without string conversion or UTF-8 slicing corruption
      const { done, value } = await reader.read();
      if (done) {
        controller.close();
        return;
      }
      if (value && value.byteLength > 0) {
        controller.enqueue(value);
      }
    },
    cancel() {
      reader.cancel();
    },
  });
}

// Injects importScripts("/patch.js"); (for classic workers) or import "/patch.js"; (for ES module workers)
function injectScriptIntoWorkerStream(
  inputStream: ReadableStream<Uint8Array>,
  isModuleWorker: boolean
): ReadableStream<Uint8Array> {
  const reader = inputStream.getReader();
  const patchScript = isModuleWorker ? 'import "/patch.js";\n' : 'importScripts("/patch.js");\n';
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
