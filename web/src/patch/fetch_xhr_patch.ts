import { rewriteBackendUrl } from '../shared/url_rewrite';
import type { CookieBridgeState } from './cookie_patch';
import { makeRewriteCtx, type RuntimePatchConfig } from './types';

/**
 * Compares a potentially relative rewritten URL against an existing Request's canonical absolute URL
 * so `fetch(new Request('/api', { method: 'POST', body }))` passes straight through without
 * needlessly cloning/reconstructing the Request body (fixes C-3b!).
 */
export function isSameCanonicalRequestUrl(
  rewrittenUrl: string,
  originalAbsUrl: string,
  baseHref: string
): boolean {
  if (rewrittenUrl === originalAbsUrl) return true;
  try {
    return new URL(rewrittenUrl, baseHref).href === originalAbsUrl;
  } catch {
    return false;
  }
}

export function buildPatchedRequestInit(
  input: Request,
  init: RequestInit | undefined,
  extraHeaders?: Record<string, string>
): RequestInit & { duplex?: 'half' } {
  const method = (init?.method ?? input.method).toUpperCase();
  const headers = init?.headers !== undefined ? new Headers(init.headers) : new Headers(input.headers);
  if (extraHeaders) {
    for (const [k, v] of Object.entries(extraHeaders)) {
      headers.set(k, v);
    }
  }
  const isBodylessMethod = method === 'GET' || method === 'HEAD';
  const body = isBodylessMethod
    ? undefined
    : init?.body !== undefined
      ? init.body
      : input.body;
  const reqInit: RequestInit & { duplex?: 'half' } = {
    method,
    headers,
    body,
    mode: init?.mode ?? (input.mode === 'navigate' ? 'same-origin' : input.mode),
    credentials: init?.credentials ?? input.credentials,
    cache: init?.cache ?? input.cache,
    redirect: init?.redirect ?? input.redirect,
    referrer: init?.referrer ?? input.referrer,
    referrerPolicy: init?.referrerPolicy ?? input.referrerPolicy,
    integrity: init?.integrity ?? input.integrity,
    keepalive: init?.keepalive ?? input.keepalive,
    signal: init?.signal ?? input.signal,
  };
  if (typeof ReadableStream !== 'undefined' && reqInit.body instanceof ReadableStream) {
    reqInit.duplex = 'half';
  }
  return reqInit;
}

export function installWorkerFetchPatch(cfg: RuntimePatchConfig): void {
  const origFetch = globalThis.fetch.bind(globalThis);
  globalThis.fetch = function (input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const ctx = makeRewriteCtx(cfg);
    if (typeof input === 'string' || input instanceof URL) {
      const nextUrl = rewriteBackendUrl(String(input), ctx);
      return origFetch(nextUrl, init);
    }
    const nextUrl = rewriteBackendUrl(input.url, ctx);
    if (isSameCanonicalRequestUrl(nextUrl, input.url, ctx.baseHref || location.href)) {
      return origFetch(input, init);
    }
    return origFetch(new Request(nextUrl, buildPatchedRequestInit(input, init)));
  };
}

export function installFetchAndXhrPatch(
  cfg: RuntimePatchConfig,
  cookieState: CookieBridgeState,
  notifyPortalSwEvicted: () => void
): void {
  const origFetch = window.fetch.bind(window);
  window.fetch = function (input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const ctx = makeRewriteCtx(cfg);
    const isReq = input instanceof Request;
    const rawUrl = isReq ? input.url : String(input);
    const rewrittenUrl = rewriteBackendUrl(rawUrl, ctx);
    const isSameOrigin = (() => {
      try {
        return new URL(rewrittenUrl, document.baseURI).origin === location.origin;
      } catch {
        return false;
      }
    })();

    const credentials = init?.credentials ?? (isReq ? input.credentials : 'same-origin');
    const pending =
      isSameOrigin && credentials !== 'omit'
        ? cookieState.getUnsyncedMutationsPayload()
        : { b64: '', maxSeq: 0, list: [] };
    const attachMuts = Boolean(pending.b64);

    let promise: Promise<Response>;
    if (!isReq) {
      if (!attachMuts) {
        promise = origFetch(rewrittenUrl, init);
      } else {
        const headers = new Headers(init?.headers);
        headers.set('x-p2p-cookie-mutations', pending.b64);
        headers.set('x-p2p-cookie-seq', String(pending.maxSeq));
        promise = origFetch(rewrittenUrl, { ...init, headers });
      }
    } else if (isSameCanonicalRequestUrl(rewrittenUrl, input.url, document.baseURI) && !attachMuts) {
      promise = origFetch(input, init);
    } else {
      const extra = attachMuts
        ? {
            'x-p2p-cookie-mutations': pending.b64,
            'x-p2p-cookie-seq': String(pending.maxSeq),
          }
        : undefined;
      promise = origFetch(new Request(rewrittenUrl, buildPatchedRequestInit(input, init, extra)));
    }

    if (!isSameOrigin) return promise;
    return promise.then((resp) => {
      if (resp.headers.get('x-p2p-sw-evicted') === '1') {
        notifyPortalSwEvicted();
      } else if (attachMuts && resp.status !== 502 && resp.status !== 503) {
        cookieState.pruneUnsyncedUpTo(pending.maxSeq);
      }
      return resp;
    });
  };

  // Patch XMLHttpRequest (URL rewrite + inline cookie mutations + unconditional SW eviction detection).
  const xhrProto = XMLHttpRequest.prototype;
  const origOpen = xhrProto.open;
  const origSend = xhrProto.send;
  const xhrMeta = new WeakMap<XMLHttpRequest, { sameOrigin: boolean }>();

  xhrProto.open = function (
    this: XMLHttpRequest,
    method: string,
    url: string | URL,
    async?: boolean,
    username?: string | null,
    password?: string | null
  ): void {
    const rewritten = rewriteBackendUrl(String(url), makeRewriteCtx(cfg));
    let sameOrigin = false;
    try {
      sameOrigin = new URL(rewritten, document.baseURI).origin === location.origin;
    } catch {
      sameOrigin = false;
    }
    xhrMeta.set(this, { sameOrigin });
    return origOpen.call(this, method, rewritten, async ?? true, username ?? null, password ?? null);
  };

  xhrProto.send = function (
    this: XMLHttpRequest,
    body?: Document | XMLHttpRequestBodyInit | null
  ): void {
    const meta = xhrMeta.get(this);
    if (meta?.sameOrigin) {
      const pending = cookieState.getUnsyncedMutationsPayload();
      const hasMuts = Boolean(pending.b64);
      if (hasMuts) {
        try {
          this.setRequestHeader('X-P2P-Cookie-Mutations', pending.b64);
          this.setRequestHeader('X-P2P-Cookie-Seq', String(pending.maxSeq));
        } catch {
          // ignore
        }
      }
      // Attach eviction recovery listener unconditionally for ALL same-origin XHRs (even when !pending.b64)!
      this.addEventListener(
        'load',
        () => {
          if (this.getResponseHeader('x-p2p-sw-evicted') === '1') {
            notifyPortalSwEvicted();
          } else if (hasMuts && this.status !== 502 && this.status !== 503) {
            cookieState.pruneUnsyncedUpTo(pending.maxSeq);
          }
        },
        { once: true }
      );
    }
    return origSend.call(this, body);
  };
}
