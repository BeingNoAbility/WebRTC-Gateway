/**
 * V7 Strict Single-Target URL Rewriting
 * - Exact parsed origin equality only (zero `.startsWith()` prefix confusion, zero cross-origin proxying).
 * - Handles cross-origin `<base href>` by emitting absolute sandbox URLs when `baseHref` origin differs
 *   from `currentOrigin`.
 * - Avoids double-wrapping Portal URLs in `resolvePortalPopupUrl`.
 */

export interface UrlRewriteContext {
  currentOrigin: string; // e.g. https://sbx-abc.sandbox.example.net
  portalOrigin: string;  // e.g. https://portal.example.com
  nodeId: string;
  targetOrigin: string;  // e.g. https://app.local:8443
  pinnedPk?: string;
  baseHref?: string;
}

const NON_HTTP_SCHEMES_RE = /^\s*(?:javascript|data|blob|about|mailto|tel|vbscript|file):/i;

export function isNonHttpScheme(rawUrl: string): boolean {
  return NON_HTTP_SCHEMES_RE.test(rawUrl);
}

function parseCurrentOriginUrl(currentOrigin: string): URL | null {
  if (!currentOrigin || currentOrigin === 'null') return null;
  try {
    const u = new URL(currentOrigin);
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
    return u;
  } catch {
    return null;
  }
}

/**
 * Resolves the effective document base URL safely:
 * - Guards opaque `"null"` `currentOrigin` (e.g. `CSP: sandbox` static files).
 * - Resolves relative `<base href="/app/">` against `currentOrigin` first so `new URL(raw, base)`
 *   never throws `TypeError: Invalid URL` on relative `<base href>`.
 */
function resolveEffectiveBase(ctx: UrlRewriteContext): { curUrl: URL; baseHref: string; crossOriginBase: boolean } | null {
  const curUrl = parseCurrentOriginUrl(ctx.currentOrigin);
  if (!curUrl) return null;
  const curBase = `${curUrl.origin}/`;
  if (!ctx.baseHref) {
    return { curUrl, baseHref: curBase, crossOriginBase: false };
  }
  try {
    const resolvedBase = new URL(ctx.baseHref, curBase);
    return {
      curUrl,
      baseHref: resolvedBase.href,
      crossOriginBase: resolvedBase.origin !== curUrl.origin,
    };
  } catch {
    return { curUrl, baseHref: curBase, crossOriginBase: false };
  }
}

/**
 * Rewrites an HTTP/HTTPS URL targeting the exact sandbox origin or exact backend target origin.
 * Returns a root-relative path when `<base href>` is same-origin, or an absolute sandbox origin URL
 * when `<base href>` is cross-origin (so browser resolution never drifts to the foreign base!).
 */
export function rewriteBackendUrl(rawUrl: string, ctx: UrlRewriteContext): string {
  if (!rawUrl || isNonHttpScheme(rawUrl)) return rawUrl;
  const trimmed = rawUrl.trim();
  if (trimmed.startsWith('#')) return rawUrl;

  const eff = resolveEffectiveBase(ctx);
  if (!eff) return rawUrl;

  let parsed: URL;
  try {
    parsed = new URL(trimmed, eff.baseHref);
  } catch {
    return rawUrl;
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return rawUrl;
  }

  const rel = `${parsed.pathname}${parsed.search}${parsed.hash}`;
  const prefix = eff.crossOriginBase ? eff.curUrl.origin : '';

  if (parsed.origin === eff.curUrl.origin) {
    return `${prefix}${rel}`;
  }

  if (ctx.targetOrigin) {
    try {
      const targetUrl = new URL(ctx.targetOrigin);
      if (parsed.origin === targetUrl.origin) {
        return `${prefix}${rel}`;
      }
    } catch {
      // ignore invalid targetOrigin
    }
  }

  return rawUrl;
}

/**
 * Rewrites a WebSocket URL (`ws:` / `wss:`) targeting the sandbox origin or exact backend target origin
 * into a canonical root-relative path. Returns `null` for all other origins.
 */
export function rewriteWebSocketTarget(rawUrl: string, ctx: UrlRewriteContext): string | null {
  if (!rawUrl || isNonHttpScheme(rawUrl)) return null;
  const eff = resolveEffectiveBase(ctx);
  if (!eff) return null;

  let parsed: URL;
  try {
    parsed = new URL(rawUrl.trim(), eff.baseHref);
  } catch {
    return null;
  }
  const proto = parsed.protocol.toLowerCase();
  if (proto !== 'ws:' && proto !== 'wss:' && proto !== 'http:' && proto !== 'https:') {
    return null;
  }
  const httpScheme = proto === 'wss:' || proto === 'https:' ? 'https' : 'http';
  const curHost = eff.curUrl.host.toLowerCase();
  const hostPort = parsed.host.toLowerCase();

  if (hostPort === curHost) {
    return `${parsed.pathname}${parsed.search}`;
  }
  if (ctx.targetOrigin) {
    try {
      const t = new URL(ctx.targetOrigin);
      if (hostPort === t.host.toLowerCase() && httpScheme === t.protocol.slice(0, -1)) {
        return `${parsed.pathname}${parsed.search}`;
      }
    } catch {
      // ignore
    }
  }
  return null;
}

/**
 * Resolves a link/window.open target into a proper top-level Portal URL so relative links
 * like `<a target="_blank" href="detail.html">` inside a cross-origin sandbox open Portal
 * instead of an orphaned bare sandbox tab.
 * Links already targeting `portalOrigin` are NEVER double-wrapped (returns `null` to open as-is).
 */
export function resolvePortalPopupUrl(rawUrl: string, ctx: UrlRewriteContext): string | null {
  if (!rawUrl || isNonHttpScheme(rawUrl)) return null;
  const eff = resolveEffectiveBase(ctx);
  if (!eff) return null;

  let resolved: URL;
  try {
    resolved = new URL(rawUrl.trim(), eff.baseHref);
  } catch {
    return null;
  }
  if (resolved.protocol !== 'http:' && resolved.protocol !== 'https:') {
    return null;
  }
  // Never wrap links that already target Portal itself!
  if (ctx.portalOrigin && resolved.origin === ctx.portalOrigin && ctx.portalOrigin !== eff.curUrl.origin) {
    return null;
  }

  let subPath: string | null = null;
  if (resolved.origin === eff.curUrl.origin) {
    subPath = `${resolved.pathname}${resolved.search}${resolved.hash}`;
  } else if (ctx.targetOrigin) {
    try {
      const targetUrl = new URL(ctx.targetOrigin);
      if (resolved.origin === targetUrl.origin) {
        subPath = `${resolved.pathname}${resolved.search}${resolved.hash}`;
      }
    } catch {
      // ignore
    }
  }
  if (!subPath) return null;

  const params = [`path=${encodeURIComponent(subPath)}`];
  if (ctx.pinnedPk) {
    params.unshift(`pk=${encodeURIComponent(ctx.pinnedPk)}`);
  }
  return `${ctx.portalOrigin}/#${encodeURIComponent(ctx.nodeId)}?${params.join('&')}`;
}
