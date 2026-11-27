/**
 * V7 Service Worker Fetch Router & W3C Fetch Exact Anti-CSRF Gate
 * - Enforces exact W3C Fetch CSRF rules WITHOUT relying on `Sec-Fetch-Site` (always null in SW):
 *   1) Non-navigate (`request.mode !== 'navigate'`): unbound `event.clientId` -> `403`.
 *   2) Worker/SharedWorker: inherits bound `event.clientId` session & binds `event.resultingClientId`.
 *   3) Unbound navigate (without valid `enterNonce`):
 *      - Non-`GET`/`HEAD` (e.g. cross-site `<form POST>`) -> `403`.
 *      - `destination === 'document'` (external `window.open` / top-level nav) -> `403`.
 *      - `destination === 'iframe'` + `GET`/`HEAD`: ONLY when `sessions.size === 1` AND
 *        (`referrer` matches `swOrigin`/`portalOrigin` with `SameOriginClient=true`, OR
 *        `referrer === ''` strictly in cookie-less static `-dir`/`-file` mode (`isStaticMode=true`)
 *        with `resultingClientId` bound so static subresources work; proxy `-target` mode
 *        always rejects unbound empty-referrer iframes to eliminate SameSite=Lax CSRF).
 *        Multi-tab (`sessions.size !== 1`) -> `403`.
 * - Strips `content-encoding` ONLY when `respFlags & DECOMPRESS_GZIP` (fixes Brotli/Deflate corruption).
 * - Always emits `Content-Security-Policy: frame-ancestors <portalOrigin>` on `-target` HTML responses.
 * - Allows HTML injection on `200` and `4xx`/`5xx` UTF-8-compatible `text/html` (excludes `206`, `3xx`,
 *   `Content-Disposition: attachment`, preserved `sandbox` CSP, non-UTF-8 charsets, undecoded `Content-Encoding`).
 * - Caps Worker script injection at 4 MiB (streams raw bytes if larger).
 */

import { FetchCtxFlag, RespFlag, type HeaderField } from '../protocol/tlv';
import type { ClientCookieMutation } from '../shared/ipc';
import {
  decodeClientMutationsHeader,
  syncCookieDeltasToClients,
} from './cookie_sync';
import { pipeThroughBoundedDecompression } from './decompress_pipeline';
import { createHtmlInjectorTransform, injectWorkerBootstrap } from './html_injector';
import type { SessionRegistry, TabSession } from './session_registry';
import { DeadBridgePortError, executeTunnelRequest } from './stream_bridge';

export const GATEWAY_ASSET_PREFIX = '/__p2p_gw__/';
export const GATEWAY_ENTER_PATH = '/__p2p_gw__/enter';

const MAX_WORKER_INJECT_BYTES = 4 * 1024 * 1024;

const MULTI_VALUE_RESPONSE_HEADERS = new Set([
  'vary',
  'link',
  'cache-control',
  'www-authenticate',
  'proxy-authenticate',
  'access-control-expose-headers',
  'content-security-policy',
]);

const STRIPPED_FOR_APP_HEADERS = new Set([
  'content-security-policy-report-only',
  'x-frame-options',
  'origin-agent-cluster',
  'set-cookie',
  'x-p2p-content-encoding',
  'x-p2p-cookie-delta',
  'service-worker-allowed',
  'transfer-encoding',
  'connection',
  'keep-alive',
]);

// Charsets whose ASCII byte range (0x00..0x7F) is identical to UTF-8 so injecting ASCII `<script>` is safe.
const UTF8_COMPATIBLE_CHARSETS = new Set([
  '',
  'utf-8',
  'utf8',
  'us-ascii',
  'ascii',
  'iso-8859-1',
  'latin1',
  'latin-1',
  'windows-1252',
  'cp1252',
]);

function encodeRFC5987(str: string): string {
  try {
    return encodeURIComponent(str).replace(
      /['()*]/g,
      (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`
    );
  } catch {
    return encodeURIComponent(str.replace(/[\uD800-\uDFFF]/g, '_'));
  }
}

export function sanitizeContentDisposition(raw: string): string {
  const cleaned = raw.replace(/[\r\n\0]/g, '');
  let hasNonAscii = false;
  for (let i = 0; i < cleaned.length; i++) {
    if (cleaned.charCodeAt(i) > 0x7e) {
      hasNonAscii = true;
      break;
    }
  }
  if (!hasNonAscii) return cleaned;

  const hasFilenameStar = /;\s*filename\*\s*=/i.test(cleaned);
  let extractedName = '';

  let asciiHeader = cleaned.replace(
    /(filename\s*=\s*)(?:"([^"]*)"|([^;\s]*))/i,
    (_m, prefix: string, quoted: string | undefined, bare: string | undefined) => {
      const orig = quoted ?? bare ?? '';
      extractedName = orig;
      const dotIdx = orig.lastIndexOf('.');
      const rawExt = dotIdx > 0 ? orig.slice(dotIdx).replace(/[^A-Za-z0-9._-]/g, '').slice(0, 16) : '';
      const asciiStem = (dotIdx > 0 ? orig.slice(0, dotIdx) : orig)
        .replace(/[^\x20-\x7e]/g, '_')
        .replace(/["\\;]/g, '_')
        .replace(/_+/g, '_')
        .replace(/^_+|_+$/g, '');
      const fallback = `${asciiStem || 'download'}${rawExt}`;
      return `${prefix}"${fallback}"`;
    }
  );

  asciiHeader = asciiHeader.replace(/[^\x20-\x7e]/g, '_');

  if (!hasFilenameStar && extractedName) {
    asciiHeader = `${asciiHeader}; filename*=UTF-8''${encodeRFC5987(extractedName)}`;
  }
  return asciiHeader;
}

export function sanitizeResponseHeaderValue(name: string, value: string): string {
  const lower = name.toLowerCase();
  if (lower === 'content-disposition') {
    return sanitizeContentDisposition(value);
  }
  let out = '';
  for (const ch of value) {
    const cp = ch.codePointAt(0)!;
    if (cp === 0x09 || (cp >= 0x20 && cp <= 0x7e) || (cp >= 0xa0 && cp <= 0xff)) {
      out += ch;
    } else if (cp > 0xff) {
      try {
        out += encodeURIComponent(ch);
      } catch {
        out += '_';
      }
    }
  }
  return out;
}

export function hasSandboxCspDirective(cspValue: string): boolean {
  for (const policy of cspValue.split(',')) {
    for (const rawDir of policy.split(';')) {
      const trimmed = rawDir.trim();
      if (!trimmed) continue;
      const directiveName = trimmed.split(/\s+/, 1)[0].toLowerCase();
      if (directiveName === 'sandbox') {
        return true;
      }
    }
  }
  return false;
}

export function buildSafeResponseHeaders(
  fields: HeaderField[],
  opts: {
    stripContentLength: boolean;
    stripContentEncoding?: boolean;
    frameAncestorsOrigin?: string;
    originAgentCluster?: boolean;
  }
): {
  headers: Headers;
  preserveFileSandboxCSP: boolean;
} {
  const headers = new Headers();
  let preserveFileSandboxCSP = false;

  for (const f of fields) {
    const k = f.name.toLowerCase();
    if (k === 'content-security-policy' && hasSandboxCspDirective(f.value)) {
      preserveFileSandboxCSP = true;
    }
  }

  const stripCE = opts.stripContentEncoding ?? opts.stripContentLength;
  for (const f of fields) {
    const k = f.name.toLowerCase();
    if (STRIPPED_FOR_APP_HEADERS.has(k)) continue;
    if (opts.stripContentLength && k === 'content-length') continue;
    if (stripCE && k === 'content-encoding') continue;
    if (k === 'content-security-policy') {
      // Preserve ONLY policies containing a real `sandbox` directive (for `-dir`/`-file`).
      if (!hasSandboxCspDirective(f.value)) continue;
    }

    const safeVal = sanitizeResponseHeaderValue(k, f.value);
    if (MULTI_VALUE_RESPONSE_HEADERS.has(k)) {
      headers.append(k, safeVal);
    } else {
      headers.set(k, safeVal);
    }
  }

  // Enforce `frame-ancestors 'self' <portalOrigin>` on all navigation & HTML responses
  // (intersects cleanly with static `CSP: sandbox` under CSP Level 3!).
  if (opts.frameAncestorsOrigin) {
    headers.append('content-security-policy', `frame-ancestors 'self' ${opts.frameAncestorsOrigin}`);
  }

  // Tier 2 same-site isolation: request dedicated origin-keyed agent cluster on every response.
  if (opts.originAgentCluster) {
    headers.set('origin-agent-cluster', '?1');
  }

  if (!headers.has('referrer-policy')) {
    headers.set('referrer-policy', 'same-origin');
  }

  return { headers, preserveFileSandboxCSP };
}

export function shouldBypassGatewayRequest(url: URL, swOrigin: string): boolean {
  if (url.origin !== swOrigin) return true;
  if (url.pathname.startsWith(GATEWAY_ASSET_PREFIX)) return true;
  return false;
}

export interface FetchAuthInput {
  clientId: string;
  resultingClientId: string;
  mode: string;
  destination: string;
  method: string;
  referrer: string;
  swOrigin: string;
}

export type FetchAuthResult =
  | { allowed: true; session: TabSession; sameOriginClient: boolean }
  | { allowed: false; reason: string };

/**
 * Pure W3C Fetch Anti-CSRF Gate (authorizes exclusively by bound clientId, one-time enterNonce,
 * or verified same-origin single-tab iframe navigation). Exported for deterministic unit testing.
 */
export function authorizeFetchRequest(
  registry: SessionRegistry,
  input: FetchAuthInput
): FetchAuthResult {
  // 1. Bound clientId or pre-bound resultingClientId (from `/__p2p_gw__/enter` or spawning appFrame -> Worker).
  const bound = registry.getBoundSession(input.clientId, input.resultingClientId);
  if (bound) {
    return { allowed: true, session: bound, sameOriginClient: true };
  }

  // 2. Non-navigate requests (fetch, XHR, img, script, beacon, etc.) MUST have a bound clientId!
  if (input.mode !== 'navigate') {
    return { allowed: false, reason: 'Cross-site or unbound subresource request blocked' };
  }

  // 3. Unbound navigate requests (without valid one-time enterNonce):
  const method = input.method.toUpperCase();
  if (method !== 'GET' && method !== 'HEAD') {
    return { allowed: false, reason: 'Unbound non-GET/HEAD navigation blocked (CSRF protection)' };
  }
  if (input.destination === 'document') {
    return { allowed: false, reason: 'Direct top-level sandbox navigation blocked (use Portal)' };
  }
  if (input.destination !== 'iframe') {
    return { allowed: false, reason: `Unbound navigation destination "${input.destination}" blocked` };
  }

  // Only permit unbound iframe GET/HEAD (e.g. appFrame back/forward/context-reload) when sessions.size === 1!
  const single = registry.getSingleActiveSession();
  if (!single) {
    return { allowed: false, reason: 'Ambiguous or missing sandbox session for unbound iframe navigation' };
  }

  const rawRef = (input.referrer || '').trim();
  if (rawRef === '' || rawRef === 'about:client') {
    // P0-2 Fail-Closed Anti-CSRF Gate:
    // - Proxy mode (`-target`, `isStaticMode === false`): NEVER permit unbound empty-referrer iframes!
    //   Portal mounts `appFrame` via `/__p2p_gw__/enter` (already bound in step 1); rejecting empty
    //   referrer eliminates cross-site `<iframe referrerpolicy="no-referrer">` SameSite=Lax CSRF completely!
    // - Static mode (`-dir`/`-file`, cookie-less, `CSP: sandbox` opaque origin): permit ONLY when
    //   `sessions.size === 1` AND bind `resultingClientId` so subresources (`<img>`/`<link>`) work!
    if (!single.meta.isStaticMode) {
      return {
        allowed: false,
        reason: 'Unbound empty-referrer iframe navigation blocked in proxy mode (CSRF protection)',
      };
    }
    if (input.resultingClientId) {
      registry.bindClient(input.resultingClientId, single.tabId);
    }
    return { allowed: true, session: single, sameOriginClient: false };
  }

  try {
    const refOrigin = new URL(rawRef).origin;
    if (refOrigin === input.swOrigin || refOrigin === single.portalOrigin) {
      if (input.resultingClientId) {
        registry.bindClient(input.resultingClientId, single.tabId);
      }
      return { allowed: true, session: single, sameOriginClient: true };
    }
  } catch {
    // malformed referrer -> reject
  }
  return { allowed: false, reason: 'External iframe navigation blocked (CSRF protection)' };
}

export function collectRequestHeaders(
  request: Request,
  swOrigin: string,
  extraMutations: ClientCookieMutation[],
  sameOriginClient: boolean
): {
  headers: HeaderField[];
  inlineMutations: ClientCookieMutation[];
  inlineMutationSeq: number;
  combinedMutations: ClientCookieMutation[];
  fetchCtx: number;
  credentialsOmit: boolean;
} {
  const credentialsOmit = request.credentials === 'omit';
  const out: HeaderField[] = [];
  let inlineMutations: ClientCookieMutation[] = [];
  let inlineMutationSeq = 0;
  let hasReferer = false;

  request.headers.forEach((value, name) => {
    const lower = name.toLowerCase();
    if (lower === 'x-p2p-tab-id') return;
    if (lower === 'x-p2p-cookie-seq') {
      const n = Number.parseInt(value, 10);
      if (Number.isFinite(n) && n > 0) inlineMutationSeq = n;
      return;
    }
    if (lower === 'x-p2p-cookie-mutations') {
      if (sameOriginClient && !credentialsOmit) {
        inlineMutations = inlineMutations.concat(decodeClientMutationsHeader(value));
      }
      return;
    }
    if (lower === 'referer') hasReferer = true;
    out.push({ name: lower, value });
  });

  // Forward same-origin request.referrer ONLY when sameOriginClient is true.
  if (sameOriginClient && !hasReferer && request.referrer && request.referrer !== 'about:client') {
    try {
      const refUrl = new URL(request.referrer);
      if (refUrl.origin === swOrigin) {
        out.push({ name: 'referer', value: request.referrer });
      }
    } catch {
      // ignore invalid referrer
    }
  }

  const combinedMutations =
    sameOriginClient && !credentialsOmit ? extraMutations.concat(inlineMutations) : [];
  let fetchCtx = 0;
  if (sameOriginClient) {
    fetchCtx |= FetchCtxFlag.SAME_ORIGIN_CLIENT;
  }
  // Per RFC 6265bis, ONLY top-level document navigations (`destination === 'document'`),
  // NEVER nested `iframe` navigations, qualify for cross-site SameSite=Lax!
  if (request.mode === 'navigate' && request.destination === 'document') {
    fetchCtx |= FetchCtxFlag.TOP_LEVEL_NAV;
  }
  if (typeof DecompressionStream === 'function') {
    fetchCtx |= FetchCtxFlag.CLIENT_DECOMPRESS_GZIP;
  }
  if (credentialsOmit) {
    fetchCtx |= FetchCtxFlag.CREDENTIALS_OMIT;
  }
  return { headers: out, inlineMutations, inlineMutationSeq, combinedMutations, fetchCtx, credentialsOmit };
}

export function isInjectableHtmlContentType(ct: string): boolean {
  const parts = ct.split(';');
  const mime = (parts[0] || '').trim().toLowerCase();
  if (mime !== 'text/html') return false;
  for (let i = 1; i < parts.length; i++) {
    const m = /^\s*charset\s*=\s*"?([^";\s]+)"?/i.exec(parts[i]);
    if (m) {
      const cs = m[1].trim().toLowerCase();
      if (!UTF8_COMPATIBLE_CHARSETS.has(cs)) {
        return false;
      }
    }
  }
  return true;
}

export function hasExplicitHttpCharset(ct: string): boolean {
  return /;\s*charset\s*=\s*[^;\s]+/i.test(ct);
}

export function isAttachmentContentDisposition(cd: string): boolean {
  return /^\s*attachment(?:\s*;|$)/i.test(cd);
}

function isJavaScriptContentType(ct: string): boolean {
  const lower = ct.toLowerCase();
  return (
    lower.includes('javascript') ||
    lower.includes('ecmascript') ||
    lower.includes('application/x-javascript')
  );
}

/**
 * Reads a Worker script body up to `MAX_WORKER_INJECT_BYTES` (4 MiB).
 * If the script fits within 4 MiB, returns its decoded UTF-8 string for safe bootstrap injection;
 * if larger than 4 MiB, returns a passthrough `ReadableStream<Uint8Array>` with zero OOM!
 */
async function readBoundedWorkerBody(
  stream: ReadableStream<Uint8Array>
): Promise<{ kind: 'text'; text: string } | { kind: 'stream'; stream: ReadableStream<Uint8Array> }> {
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;

  while (true) {
    const { done, value } = await reader.read();
    if (done) {
      reader.releaseLock();
      const merged = new Uint8Array(total);
      let off = 0;
      for (const c of chunks) {
        merged.set(c, off);
        off += c.byteLength;
      }
      return { kind: 'text', text: new TextDecoder().decode(merged) };
    }
    if (!value || value.byteLength === 0) continue;
    chunks.push(value);
    total += value.byteLength;
    if (total > MAX_WORKER_INJECT_BYTES) {
      let idx = 0;
      const passthrough = new ReadableStream<Uint8Array>({
        async pull(controller) {
          if (idx < chunks.length) {
            controller.enqueue(chunks[idx++]);
            return;
          }
          const next = await reader.read();
          if (next.done) {
            reader.releaseLock();
            controller.close();
          } else if (next.value) {
            controller.enqueue(next.value);
          }
        },
        cancel(reason) {
          return reader.cancel(reason);
        },
      });
      return { kind: 'stream', stream: passthrough };
    }
  }
}

function buildSwEvictedResponse(isNavigate: boolean): Response {
  if (isNavigate) {
    const html =
      '<!doctype html><html><head><meta charset="utf-8"><title>Reconnecting…</title></head>' +
      '<body><script>try{window.parent.postMessage({type:"P2P_SW_EVICTED"},"*")}catch{}</script></body></html>';
    return new Response(html, {
      status: 503,
      headers: {
        'content-type': 'text/html; charset=utf-8',
        'cache-control': 'no-store',
        'content-security-policy': "default-src 'none'; script-src 'unsafe-inline'",
        'x-p2p-sw-evicted': '1',
      },
    });
  }
  return new Response('Service Worker session evicted — reconnecting', {
    status: 503,
    headers: {
      'content-type': 'text/plain; charset=utf-8',
      'cache-control': 'no-store',
      'x-p2p-sw-evicted': '1',
    },
  });
}

export async function handleFetchEvent(
  swScope: ServiceWorkerGlobalScope,
  registry: SessionRegistry,
  event: FetchEvent
): Promise<Response> {
  const req = event.request;
  const reqUrl = new URL(req.url);

  // Periodically prune closed clients without blocking hot path.
  event.waitUntil(registry.maybePruneClosedClients(swScope).catch(() => {}));

  // If the Service Worker was terminated while idle (sessions.size === 0), check if the caller is
  // a live nested appFrame/worker and signal Portal to cleanly re-bootstrap via ephemeral bridgeFrame.
  if (registry.getActiveSessionCount() === 0 && event.clientId) {
    const liveCaller = await registry.isLiveNestedAppClient(swScope, event.clientId);
    if (liveCaller) {
      try {
        liveCaller.postMessage({ type: 'P2P_SW_EVICTED' });
      } catch {
        // ignore
      }
      return buildSwEvictedResponse(req.mode === 'navigate');
    }
  }

  const authInput = {
    clientId: event.clientId,
    resultingClientId: event.resultingClientId,
    mode: req.mode,
    destination: req.destination,
    method: req.method,
    referrer: req.referrer,
    swOrigin: swScope.location.origin,
  };
  let auth = authorizeFetchRequest(registry, authInput);
  if (
    !auth.allowed &&
    req.mode === 'navigate' &&
    req.destination === 'iframe' &&
    registry.getActiveSessionCount() > 1
  ) {
    // Reap any crashed/reloaded previous Portal tab before rejecting ambiguous multi-tab navigation.
    await registry.pruneDeadSessions(swScope);
    auth = authorizeFetchRequest(registry, authInput);
  }
  if (!auth.allowed) {
    return new Response(`Forbidden: ${auth.reason}`, {
      status: 403,
      headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
    });
  }

  const session = auth.session;
  const target = `${reqUrl.pathname}${reqUrl.search}`;
  const isOmit = req.credentials === 'omit';
  const peeked =
    auth.sameOriginClient && !isOmit
      ? session.cookieMirror.peekPendingMutations()
      : { mutations: [] as ClientCookieMutation[], watermark: 0 };
  const {
    headers: reqHeaders,
    inlineMutations,
    inlineMutationSeq,
    combinedMutations,
    fetchCtx,
    credentialsOmit,
  } = collectRequestHeaders(req, swScope.location.origin, peeked.mutations, auth.sameOriginClient);

  const method = req.method.toUpperCase();
  const hasBody = method !== 'GET' && method !== 'HEAD' && req.body !== null;

  let activeSession = session;
  let tunnelResp;
  try {
    const res = await executeTunnelRequest(
      swScope,
      registry,
      session,
      req,
      target,
      reqHeaders,
      hasBody,
      fetchCtx,
      combinedMutations
    );
    activeSession = res.session;
    tunnelResp = res.response;
  } catch (err) {
    if (err instanceof DeadBridgePortError) {
      const liveCaller = event.clientId
        ? await registry.isLiveNestedAppClient(swScope, event.clientId)
        : null;
      if (liveCaller) {
        try {
          liveCaller.postMessage({ type: 'P2P_SW_EVICTED' });
        } catch {
          // ignore
        }
        return buildSwEvictedResponse(req.mode === 'navigate');
      }
    }
    const msg = err instanceof Error ? err.message : 'Tunnel fetch failed';
    return new Response(msg, {
      status: 502,
      headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
    });
  }

  // Commit queued cookie mutations ONLY AFTER the tunnel accepted the request!
  if (auth.sameOriginClient && !credentialsOmit) {
    if (peeked.mutations.length > 0) {
      activeSession.cookieMirror.commitPendingMutations(peeked.watermark);
    }
    if (inlineMutations.length > 0) {
      activeSession.cookieMirror.applyClientMutations(inlineMutations, {
        clientId: event.clientId,
        seq: inlineMutationSeq,
        queueForUpstream: false,
      });
    }
  }

  event.waitUntil(tunnelResp.done);

  try {
    const status = tunnelResp.status;
    if (status < 200 || status > 599) {
      tunnelResp.cancel();
      return new Response(`Invalid upstream HTTP status: ${status}`, {
        status: 502,
        headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
      });
    }

    const isNullBody = method === 'HEAD' || status === 204 || status === 205 || status === 304;

    let contentType = '';
    let contentDisposition = '';
    let rawContentEncoding = '';
    const decompressGzip = (tunnelResp.respFlags & RespFlag.DECOMPRESS_GZIP) !== 0;
    let preserveSandbox = false;
    for (const f of tunnelResp.headers) {
      const k = f.name.toLowerCase();
      if (k === 'content-type') contentType = f.value;
      else if (k === 'content-disposition') contentDisposition = f.value;
      else if (k === 'content-encoding') rawContentEncoding = f.value.trim().toLowerCase();
      else if (k === 'content-security-policy' && hasSandboxCspDirective(f.value)) {
        preserveSandbox = true;
      }
    }

    const hasUndecodedEncoding = Boolean(rawContentEncoding && rawContentEncoding !== 'identity' && !decompressGzip);
    const isAttachment = isAttachmentContentDisposition(contentDisposition);
    const isHtmlDoc =
      !isNullBody &&
      !preserveSandbox &&
      !isAttachment &&
      !hasUndecodedEncoding &&
      (status === 200 || (status >= 400 && status <= 599)) &&
      isInjectableHtmlContentType(contentType);

    const isNavigateHtml =
      isHtmlDoc &&
      (req.mode === 'navigate' || req.destination === 'document' || req.destination === 'iframe');

    const isWorkerScript =
      status === 200 &&
      !isNullBody &&
      !preserveSandbox &&
      !hasUndecodedEncoding &&
      (req.destination === 'worker' || req.destination === 'sharedworker') &&
      isJavaScriptContentType(contentType);

    const isAnyHtmlResponse = contentType.split(';', 1)[0].trim().toLowerCase() === 'text/html';
    const willTransformBody = decompressGzip || isNavigateHtml || isWorkerScript;
    const { headers } = buildSafeResponseHeaders(tunnelResp.headers, {
      stripContentLength: willTransformBody,
      stripContentEncoding: decompressGzip,
      frameAncestorsOrigin:
        req.mode === 'navigate' || isAnyHtmlResponse ? activeSession.portalOrigin : undefined,
      originAgentCluster: Boolean(activeSession.meta.sameSiteMode),
    });

    const deltas = credentialsOmit ? [] : tunnelResp.cookieDeltas;
    if (deltas.length > 0) {
      activeSession.cookieMirror.applyDeltas(deltas);
      if (!isNavigateHtml) {
        await syncCookieDeltasToClients(
          swScope,
          event.clientId,
          activeSession.clientIds,
          deltas
        );
      }
    }

    if (isNullBody || !tunnelResp.bodyStream) {
      return new Response(null, { status, headers });
    }

    let body: ReadableStream<Uint8Array> | string = tunnelResp.bodyStream;
    if (decompressGzip) {
      body = pipeThroughBoundedDecompression(body, 'gzip');
    }

    if (isNavigateHtml) {
      const cfg = {
        nodeId: activeSession.meta.nodeId,
        portalOrigin: activeSession.portalOrigin,
        backendOrigin: activeSession.meta.backendOrigin,
        pinnedPk: activeSession.meta.pinnedPk,
        initCookies: activeSession.cookieMirror.snapshot(),
      };
      body = body.pipeThrough(
        createHtmlInjectorTransform(swScope.location.origin, cfg, {
          hasHttpCharset: hasExplicitHttpCharset(contentType),
        })
      );
    } else if (isWorkerScript) {
      const bounded = await readBoundedWorkerBody(body);
      if (bounded.kind === 'text') {
        body = injectWorkerBootstrap(bounded.text, swScope.location.origin, {
          backendOrigin: activeSession.meta.backendOrigin,
        });
      } else {
        body = bounded.stream;
      }
    }

    return new Response(body, { status, headers });
  } catch (err) {
    tunnelResp.cancel();
    const msg = err instanceof Error ? err.message : 'Failed to build gateway response';
    return new Response(`Gateway response error: ${msg}`, {
      status: 502,
      headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
    });
  }
}
