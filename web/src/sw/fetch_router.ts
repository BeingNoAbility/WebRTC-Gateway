/**
 * Service Worker Fetch Router, W3C Anti-CSRF Gate & CSP Directive Rewriter
 * - Unconditionally rejects `destination === 'document'` BEFORE `getBoundSession` (closes top-level popup escape).
 * - Authorizes proxy mode exclusively via bound `clientId`, pre-bound `resultingClientId` (`/enter`),
 *   or W3C `replacesClientId` (Back/Forward/Reload) — zero `referrer` or `sessions.size === 1` heuristics.
 * - Preserves upstream CSP directives while rewriting `frame-ancestors`, `backendOrigin` -> `'self'`,
 *   and propagating script nonces for SW-synthesized `/__p2p_gw__/boot.js` + `patch.<hash>.js`.
 * - Inspects ALL `Content-Encoding` headers, strips `ETag` on transformed bodies, and commits
 *   pending cookie mutations immediately on `REQ_ACK`.
 */

import { FetchCtxFlag, RespFlag, type HeaderField } from '../protocol/tlv';
import type { ClientCookieMutation } from '../shared/ipc';
import {
  decodeClientMutationsHeader,
  syncCookieDeltasToClients,
} from './cookie_sync';
import { pipeThroughBoundedDecompression } from './decompress_pipeline';
import {
  GATEWAY_BOOT_SCRIPT_PATH,
  buildBootScriptPayload,
  createHtmlInjectorTransform,
  injectWorkerBootstrap,
} from './html_injector';
import type { SessionRegistry, TabSession } from './session_registry';
import { DeadBridgePortError, executeTunnelRequest } from './stream_bridge';

export const GATEWAY_ASSET_PREFIX = '/__p2p_gw__/';
export const GATEWAY_ENTER_PATH = '/__p2p_gw__/enter';
export { GATEWAY_BOOT_SCRIPT_PATH };

const MAX_WORKER_INJECT_BYTES = 4 * 1024 * 1024;
const CSP_NONCE_TOKEN_RE = /^'nonce-([A-Za-z0-9+/=_-]{1,128})'$/i;

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

const CSP_SOURCE_DIRECTIVES = new Set([
  'default-src',
  'script-src',
  'script-src-elem',
  'style-src',
  'style-src-elem',
  'img-src',
  'font-src',
  'connect-src',
  'media-src',
  'frame-src',
  'child-src',
  'worker-src',
  'form-action',
  'manifest-src',
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

/**
 * Rewrites an upstream non-sandbox Content-Security-Policy value so upstream XSS protections
 * remain intact while removing conflicting `frame-ancestors`, mapping `backendOrigin` -> `'self'`,
 * and permitting gateway external scripts (`/boot.js` + `/assets/patch.*.js`) via nonce or `'self'`.
 */
export function rewriteUpstreamCsp(
  rawValue: string,
  opts: { swOrigin?: string; backendOrigin?: string; preferredNonce?: string } = {}
): { policy: string; scriptNonce?: string } {
  const backendOrigin = (opts.backendOrigin || '').replace(/\/+$/, '').toLowerCase();
  let backendWsOrigin = '';
  if (backendOrigin) {
    try {
      const bu = new URL(backendOrigin);
      backendWsOrigin = `${bu.protocol === 'https:' ? 'wss:' : 'ws:'}//${bu.host.toLowerCase()}`;
    } catch {
      // ignore invalid backendOrigin
    }
  }
  const isHttpSw = Boolean(opts.swOrigin && opts.swOrigin.startsWith('http://'));
  let detectedNonce: string | undefined = opts.preferredNonce;

  const rewrittenPolicies: string[] = [];
  for (const rawPolicy of rawValue.split(',')) {
    const directives: Array<{ name: string; tokens: string[] }> = [];
    for (const rawDir of rawPolicy.split(';')) {
      const trimmed = rawDir.trim();
      if (!trimmed) continue;
      const parts = trimmed.split(/\s+/);
      const name = parts[0].toLowerCase();
      const tokens = parts.slice(1);
      if (name === 'frame-ancestors') continue;
      if (isHttpSw && name === 'upgrade-insecure-requests') continue;
      directives.push({ name, tokens });
    }
    if (directives.length === 0) continue;

    const hasScriptSrc = directives.some((d) => d.name === 'script-src');
    const hasScriptSrcElem = directives.some((d) => d.name === 'script-src-elem');

    const renderedDirs: string[] = [];
    for (const dir of directives) {
      let tokens = dir.tokens;
      if (CSP_SOURCE_DIRECTIVES.has(dir.name) && backendOrigin) {
        tokens = tokens.map((tok) => {
          const low = tok.replace(/\/+$/, '').toLowerCase();
          if (low === backendOrigin || (dir.name === 'connect-src' && backendWsOrigin && low === backendWsOrigin)) {
            return "'self'";
          }
          return tok;
        });
      }

      const controlsExternalScript =
        dir.name === 'script-src-elem' ||
        dir.name === 'script-src' ||
        (dir.name === 'default-src' && !hasScriptSrc && !hasScriptSrcElem);

      if (controlsExternalScript) {
        let dirNonce: string | undefined;
        for (const tok of tokens) {
          const m = CSP_NONCE_TOKEN_RE.exec(tok);
          if (m) {
            dirNonce = m[1];
            break;
          }
        }
        if (dirNonce && (!detectedNonce || detectedNonce === dirNonce)) {
          detectedNonce = dirNonce;
        } else if (dirNonce && detectedNonce) {
          // Multi-header/comma-separated CSP with distinct nonces: permit the already-chosen
          // gateway script nonce alongside this policy's nonce so injected scripts pass ALL policies.
          const sharedTok = `'nonce-${detectedNonce}'`;
          if (!tokens.includes(sharedTok)) {
            tokens = [...tokens, sharedTok];
          }
        } else {
          // No reusable nonce -> allow external same-origin gateway scripts via 'self' (never 'unsafe-inline').
          tokens = tokens.filter((tok) => {
            const low = tok.toLowerCase();
            return low !== "'none'" && low !== "'strict-dynamic'";
          });
          if (!tokens.some((tok) => tok.toLowerCase() === "'self'")) {
            tokens.unshift("'self'");
          }
        }
      }

      const deduped: string[] = [];
      for (const tok of tokens) {
        if (!deduped.includes(tok)) deduped.push(tok);
      }
      renderedDirs.push(deduped.length > 0 ? `${dir.name} ${deduped.join(' ')}` : dir.name);
    }

    if (renderedDirs.length > 0) {
      rewrittenPolicies.push(renderedDirs.join('; '));
    }
  }

  return {
    policy: rewrittenPolicies.join(', '),
    scriptNonce: detectedNonce,
  };
}

export function buildSafeResponseHeaders(
  fields: HeaderField[],
  opts: {
    stripContentLength: boolean;
    stripContentEncoding?: boolean;
    stripEtag?: boolean;
    frameAncestorsOrigin?: string;
    originAgentCluster?: boolean;
    swOrigin?: string;
    backendOrigin?: string;
  }
): {
  headers: Headers;
  preserveFileSandboxCSP: boolean;
  scriptNonce?: string;
} {
  const headers = new Headers();
  let preserveFileSandboxCSP = false;
  let scriptNonce: string | undefined;

  for (const f of fields) {
    const k = f.name.toLowerCase();
    if (k === 'content-security-policy' && hasSandboxCspDirective(f.value)) {
      preserveFileSandboxCSP = true;
    }
  }

  const stripCE = opts.stripContentEncoding ?? opts.stripContentLength;
  const stripEtag = opts.stripEtag ?? opts.stripContentLength;
  for (const f of fields) {
    const k = f.name.toLowerCase();
    if (STRIPPED_FOR_APP_HEADERS.has(k)) continue;
    if (opts.stripContentLength && k === 'content-length') continue;
    if (stripCE && k === 'content-encoding') continue;
    if (stripEtag && k === 'etag') continue;

    if (k === 'content-security-policy') {
      if (hasSandboxCspDirective(f.value)) {
        headers.append(k, sanitizeResponseHeaderValue(k, f.value));
      } else {
        const rewritten = rewriteUpstreamCsp(f.value, {
          swOrigin: opts.swOrigin,
          backendOrigin: opts.backendOrigin,
          preferredNonce: scriptNonce,
        });
        if (rewritten.scriptNonce && !scriptNonce) {
          scriptNonce = rewritten.scriptNonce;
        }
        if (rewritten.policy) {
          headers.append(k, sanitizeResponseHeaderValue(k, rewritten.policy));
        }
      }
      continue;
    }

    const safeVal = sanitizeResponseHeaderValue(k, f.value);
    if (MULTI_VALUE_RESPONSE_HEADERS.has(k)) {
      headers.append(k, safeVal);
    } else {
      headers.set(k, safeVal);
    }
  }

  // Enforce `frame-ancestors 'self' <portalOrigin>` on all navigation & HTML responses.
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

  return { headers, preserveFileSandboxCSP, scriptNonce };
}

export function shouldBypassGatewayRequest(url: URL, swOrigin: string): boolean {
  if (url.origin !== swOrigin) return true;
  if (url.pathname.startsWith(GATEWAY_ASSET_PREFIX)) return true;
  return false;
}

export interface FetchAuthInput {
  clientId: string;
  resultingClientId: string;
  replacesClientId?: string;
  mode: string;
  destination: string;
  method: string;
  referrer: string;
  secFetchSite?: string;
  swOrigin: string;
}

export type FetchAuthResult =
  | { allowed: true; session: TabSession; sameOriginClient: boolean }
  | { allowed: false; reason: string };

/**
 * Strict W3C Fetch Anti-CSRF & Sandbox Containment Gate:
 *  0. Unconditionally rejects `destination === 'document'` BEFORE `getBoundSession`
 *     (eliminates top-level sandbox escape via `window.open` / `allow-popups-to-escape-sandbox`).
 *  1. Bound `clientId`, pre-bound `resultingClientId` (from `/__p2p_gw__/enter`), or W3C
 *     `replacesClientId` (Back/Forward/Reload) -> authorized.
 *  2. Non-navigate or non-GET/HEAD or non-iframe -> rejected (`403`).
 *  3. Unbound iframe GET/HEAD -> rejected in proxy `-target` mode (`isStaticMode === false`);
 *     permitted ONLY in cookie-less read-only static `-dir`/`-file` (`isStaticMode === true`,
 *     `sessions.size === 1`, `sameOriginClient: false`) where `CSP: sandbox` opaque origins omit
 *     both `clientId` and `replacesClientId`.
 */
export function authorizeFetchRequest(
  registry: SessionRegistry,
  input: FetchAuthInput
): FetchAuthResult {
  // 0. Unconditional top-level document block (MUST precede getBoundSession!).
  if (input.destination === 'document') {
    return { allowed: false, reason: 'Direct top-level sandbox navigation blocked (use Portal)' };
  }

  // 1. Bound clientId, pre-bound resultingClientId (from `/__p2p_gw__/enter`), or replacesClientId.
  const isIframeNav = input.mode === 'navigate' && input.destination === 'iframe';
  const bound = registry.getBoundSession(
    input.clientId,
    input.resultingClientId,
    input.replacesClientId || '',
    isIframeNav
  );
  if (bound) {
    return { allowed: true, session: bound, sameOriginClient: true };
  }

  // 2. Non-navigate requests (fetch, XHR, img, script, beacon, etc.) MUST have a bound clientId!
  if (input.mode !== 'navigate') {
    return { allowed: false, reason: 'Cross-site or unbound subresource request blocked' };
  }

  // 3. Unbound navigate requests:
  const method = input.method.toUpperCase();
  if (method !== 'GET' && method !== 'HEAD') {
    return { allowed: false, reason: 'Unbound non-GET/HEAD navigation blocked (CSRF protection)' };
  }
  if (input.destination !== 'iframe') {
    return { allowed: false, reason: `Unbound navigation destination "${input.destination}" blocked` };
  }

  // 3a. Cross-browser Reload / Back / Forward fallback for engines lacking W3C `replacesClientId`
  //     (Firefox & Safari): consumes a single-use 3s `P2P_EXPECT_NAV` token previously posted by
  //     the bound top-level `appFrame` on `beforeunload`, and rejects any cross-site/same-site
  //     embedder (`Sec-Fetch-Site: cross-site | same-site`) so attacker iframes can never race-steal it.
  const fetchSite = (input.secFetchSite || '').toLowerCase();
  const isReloadOrHistorySite =
    fetchSite === '' || fetchSite === 'same-origin' || fetchSite === 'none';
  if (!input.clientId && !input.replacesClientId && isReloadOrHistorySite) {
    const expected = registry.consumeExpectedRootNav();
    if (expected) {
      if (input.resultingClientId) {
        registry.bindClient(input.resultingClientId, expected.tabId, true);
      }
      return { allowed: true, session: expected, sameOriginClient: true };
    }
  }

  // 3b. Only cookie-less read-only static `-dir`/`-file` (`CSP: sandbox` opaque origin) lacks
  //     clientId/replacesClientId on in-iframe directory link clicks. Proxy `-target` mode NEVER falls back!
  const single = registry.getSingleActiveSession();
  if (!single || !single.meta.isStaticMode) {
    return {
      allowed: false,
      reason: 'Unbound iframe navigation blocked (missing enter token or bound client)',
    };
  }
  if (input.resultingClientId) {
    registry.bindClient(input.resultingClientId, single.tabId, false);
  }
  return { allowed: true, session: single, sameOriginClient: false };
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
 * Evaluates ALL `Content-Encoding` header values (comma-separated or repeated headers)
 * to determine whether any undecoded encoding remains after optional gzip tunnel decompression.
 */
export function hasUndecodedContentEncoding(values: string[], decompressGzip: boolean): boolean {
  const tokens: string[] = [];
  for (const raw of values) {
    for (const part of raw.split(',')) {
      const tok = part.trim().toLowerCase();
      if (tok && tok !== 'identity') {
        tokens.push(tok);
      }
    }
  }
  if (tokens.length === 0) return false;
  if (!decompressGzip) return true;
  // When tunnel decompresses a single gzip layer, any additional/different encoding (e.g. `br, gzip`) remains undecoded.
  if (tokens.length > 1) return true;
  return tokens[0] !== 'gzip' && tokens[0] !== 'x-gzip';
}

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

/**
 * Serves SW-synthesized `/__p2p_gw__/boot.js` exclusively to bound sandbox appFrame clients.
 */
export async function handleBootScriptRequest(
  swScope: ServiceWorkerGlobalScope,
  registry: SessionRegistry,
  event: FetchEvent
): Promise<Response> {
  const req = event.request;
  const dest = (req.destination || '').toLowerCase();
  if (
    req.method.toUpperCase() !== 'GET' ||
    dest === 'serviceworker' ||
    dest === 'worker' ||
    dest === 'sharedworker'
  ) {
    return new Response('Forbidden', {
      status: 403,
      headers: {
        'content-type': 'text/plain; charset=utf-8',
        'cache-control': 'no-store',
        'cross-origin-resource-policy': 'same-origin',
      },
    });
  }

  const session = registry.getBoundSession(event.clientId, '');
  if (!session) {
    if (event.clientId) {
      const liveCaller = await registry.isLiveNestedAppClient(swScope, event.clientId);
      if (liveCaller) {
        try {
          liveCaller.postMessage({ type: 'P2P_SW_EVICTED' });
        } catch {
          // ignore
        }
        return buildSwEvictedResponse(false);
      }
    }
    return new Response('Forbidden: unbound boot script request', {
      status: 403,
      headers: {
        'content-type': 'text/plain; charset=utf-8',
        'cache-control': 'no-store',
        'cross-origin-resource-policy': 'same-origin',
      },
    });
  }

  const body = buildBootScriptPayload({
    nodeId: session.meta.nodeId,
    portalOrigin: session.portalOrigin,
    backendOrigin: session.meta.backendOrigin,
    cookieSecureContext: session.meta.cookieSecureContext,
    pinnedPk: session.meta.pinnedPk,
    initCookies: session.cookieMirror.snapshot(),
  });
  return new Response(body, {
    status: 200,
    headers: {
      'content-type': 'application/javascript; charset=utf-8',
      'cache-control': 'no-store',
      'cross-origin-resource-policy': 'same-origin',
      vary: 'Sec-Fetch-Dest',
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

  event.waitUntil(registry.maybePruneClosedClients(swScope).catch(() => {}));

  const replacesClientId = (event as FetchEvent & { replacesClientId?: string }).replacesClientId || '';
  const authInput: FetchAuthInput = {
    clientId: event.clientId,
    resultingClientId: event.resultingClientId,
    replacesClientId,
    mode: req.mode,
    destination: req.destination,
    method: req.method,
    referrer: req.referrer,
    secFetchSite: req.headers.get('sec-fetch-site') || '',
    swOrigin: swScope.location.origin,
  };
  let auth = authorizeFetchRequest(registry, authInput);
  if (
    !auth.allowed &&
    req.mode === 'navigate' &&
    req.destination === 'iframe' &&
    registry.getActiveSessionCount() > 1
  ) {
    await registry.pruneDeadSessions(swScope);
    auth = authorizeFetchRequest(registry, authInput);
  }
  if (!auth.allowed) {
    // Emit per-tab P2P_SW_EVICTED recovery hint whenever an existing live nested appFrame/worker
    // lost its session binding — even if ANOTHER tab already re-attached (sessions.size > 0)!
    if (event.clientId && req.destination !== 'document') {
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

  let mutationsCommitted = false;
  const commitMutationsOnAck = () => {
    if (mutationsCommitted) return;
    mutationsCommitted = true;
    if (auth.sameOriginClient && !credentialsOmit) {
      if (peeked.mutations.length > 0) {
        session.cookieMirror.commitPendingMutations(peeked.watermark);
      }
      if (inlineMutations.length > 0) {
        session.cookieMirror.applyClientMutations(inlineMutations, {
          clientId: event.clientId,
          seq: inlineMutationSeq,
          queueForUpstream: false,
        });
      }
    }
  };

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
      fetchCtx,
      combinedMutations,
      commitMutationsOnAck
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

  // Fallback commit if RESP_START arrived without a separate REQ_ACK.
  commitMutationsOnAck();
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
    const contentEncodingValues: string[] = [];
    const decompressGzip = (tunnelResp.respFlags & RespFlag.DECOMPRESS_GZIP) !== 0;
    let preserveSandbox = false;
    for (const f of tunnelResp.headers) {
      const k = f.name.toLowerCase();
      if (k === 'content-type') contentType = f.value;
      else if (k === 'content-disposition') contentDisposition = f.value;
      else if (k === 'content-encoding') contentEncodingValues.push(f.value);
      else if (k === 'content-security-policy' && hasSandboxCspDirective(f.value)) {
        preserveSandbox = true;
      }
    }

    const hasUndecodedEncoding = hasUndecodedContentEncoding(contentEncodingValues, decompressGzip);
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
    const willMutateBody = isNavigateHtml || isWorkerScript;
    const willTransformBody = decompressGzip || willMutateBody;
    const { headers, scriptNonce } = buildSafeResponseHeaders(tunnelResp.headers, {
      stripContentLength: willTransformBody,
      stripContentEncoding: decompressGzip,
      // Preserve ETag on transparent hop-by-hop gzip decompression (matches Go http_proxy.go);
      // strip ETag ONLY when HTML or Worker JS response bodies are actually rewritten!
      stripEtag: willMutateBody,
      frameAncestorsOrigin:
        req.mode === 'navigate' || isAnyHtmlResponse ? activeSession.portalOrigin : undefined,
      originAgentCluster: Boolean(activeSession.meta.sameSiteMode),
      swOrigin: swScope.location.origin,
      backendOrigin: activeSession.meta.backendOrigin,
    });

    const deltas = credentialsOmit ? [] : tunnelResp.cookieDeltas;
    if (deltas.length > 0) {
      activeSession.cookieMirror.applyDeltas(deltas);
      if (isNavigateHtml) {
        // Still sync sibling clients (e.g. parent frame when a child iframe navigation sets cookies!).
        const siblings = Array.from(activeSession.clientIds).filter(
          (id) => id !== event.resultingClientId
        );
        if (siblings.length > 0) {
          await syncCookieDeltasToClients(swScope, '', siblings, deltas);
        }
      } else {
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
      body = body.pipeThrough(
        createHtmlInjectorTransform(swScope.location.origin, {
          hasHttpCharset: hasExplicitHttpCharset(contentType),
          scriptNonce,
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
