import { resolvePortalPopupUrl, rewriteBackendUrl } from '../shared/url_rewrite';
import { makeRewriteCtx, type PrivateSWHandle, type RuntimePatchConfig } from './types';

export function installNavigationPatch(
  cfg: RuntimePatchConfig,
  swHandle: PrivateSWHandle | null = null
): void {
  const origWindowOpen = window.open.bind(window);
  window.open = function (url?: string | URL, target?: string, features?: string): Window | null {
    if (url) {
      const ctx = makeRewriteCtx(cfg);
      const normTarget = target === '_top' || target === '_parent' ? '_self' : target;
      if (!normTarget || normTarget === '_blank') {
        const popup = resolvePortalPopupUrl(String(url), ctx);
        if (popup) return origWindowOpen(popup, normTarget || '_blank', features);
      } else {
        return origWindowOpen(rewriteBackendUrl(String(url), ctx), normTarget, features);
      }
    }
    return origWindowOpen(url, target, features);
  };

  // Bubble phase so SPA routers calling `ev.preventDefault()` run first.
  document.addEventListener(
    'click',
    (ev: MouseEvent) => {
      if (ev.defaultPrevented || ev.button !== 0) return;
      const anchor = (ev.target as Element | null)?.closest?.('a[href]') as HTMLAnchorElement | null;
      if (!anchor) return;
      const rawHref = anchor.getAttribute('href') || '';
      if (!rawHref || rawHref.startsWith('#')) return;

      const ctx = makeRewriteCtx(cfg);
      const tgt = (anchor.getAttribute('target') || '').toLowerCase();
      if (tgt === '_top' || tgt === '_parent') {
        anchor.setAttribute('target', '_self');
      }
      if (!anchor.hasAttribute('download') && (tgt === '_blank' || ev.ctrlKey || ev.metaKey)) {
        const popup = resolvePortalPopupUrl(rawHref, ctx);
        if (popup) {
          ev.preventDefault();
          origWindowOpen(popup, '_blank', 'noopener');
        }
        return;
      }
      const rewritten = rewriteBackendUrl(rawHref, ctx);
      if (rewritten !== rawHref) {
        anchor.setAttribute('href', rewritten);
      }
    },
    false
  );

  document.addEventListener(
    'submit',
    (ev: SubmitEvent) => {
      const form = ev.target as HTMLFormElement | null;
      if (!form || form.tagName !== 'FORM') return;
      const tgt = (form.getAttribute('target') || '').toLowerCase();
      if (tgt === '_top' || tgt === '_parent') {
        form.setAttribute('target', '_self');
      }
      const rawAction = form.getAttribute('action');
      if (!rawAction) return;
      const rewritten = rewriteBackendUrl(rawAction, makeRewriteCtx(cfg));
      if (rewritten !== rawAction) {
        form.setAttribute('action', rewritten);
      }
    },
    true
  );

  // Sync top-level appFrame route & document title to Portal ONLY from direct child of Portal (`window.parent === window.top`),
  // never from nested sub-iframes inside the app!
  const isDirectAppFrame =
    window.parent && window.parent !== window && window.parent === window.top;
  if (isDirectAppFrame && cfg.portalOrigin) {
    const postToPortal = (msg: Record<string, unknown>) => {
      try {
        window.parent.postMessage(msg, cfg.portalOrigin);
      } catch {
        // ignore
      }
    };
    const syncRoute = () => {
      postToPortal({
        type: 'P2P_ROUTE_SYNC',
        path: `${location.pathname}${location.search}${location.hash}`,
      });
    };
    const syncTitle = () => {
      if (document.title) postToPortal({ type: 'P2P_TITLE_SYNC', title: document.title });
    };

    const origPush = history.pushState.bind(history);
    const origReplace = history.replaceState.bind(history);
    history.pushState = function (...args: Parameters<History['pushState']>) {
      const res = origPush(...args);
      syncRoute();
      return res;
    };
    history.replaceState = function (...args: Parameters<History['replaceState']>) {
      const res = origReplace(...args);
      syncRoute();
      return res;
    };
    window.addEventListener('popstate', syncRoute);
    window.addEventListener('hashchange', syncRoute);
    // Notify the Service Worker around top-level appFrame navigations so engines without W3C
    // `FetchEvent.replacesClientId` (Firefox & Safari) preserve `rootClientId` ownership and
    // authorize legitimate iframe reload / history traversal. `pageshow` immediately disarms any
    // pending expectation (e.g. on BFCache restore) and restores `rootClientId` to the active page.
    if (swHandle) {
      const notifySwNav = (type: 'P2P_EXPECT_NAV' | 'P2P_NAV_SETTLED') => {
        try {
          swHandle.getController()?.postMessage({ type });
        } catch {
          // ignore
        }
      };
      window.addEventListener('beforeunload', () => notifySwNav('P2P_EXPECT_NAV'));
      window.addEventListener('pageshow', () => notifySwNav('P2P_NAV_SETTLED'));
    }
    syncRoute();
    syncTitle();
    window.addEventListener('load', syncTitle, { once: true });
  }
}
