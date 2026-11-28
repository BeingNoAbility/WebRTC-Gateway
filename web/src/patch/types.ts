import type { CookieDelta } from '../shared/ipc';
import type { UrlRewriteContext } from '../shared/url_rewrite';

export interface RuntimePatchConfig {
  nodeId: string;
  portalOrigin: string;
  backendOrigin: string;
  pinnedPk?: string;
  initCookies: CookieDelta[];
}

export interface PrivateSWHandle {
  getController: () => ServiceWorker | null;
  getReady: () => Promise<ServiceWorkerRegistration>;
  addMessageListener: (fn: (ev: MessageEvent) => void) => void;
}

export function makeRewriteCtx(cfg: RuntimePatchConfig): UrlRewriteContext {
  const baseHref = typeof document !== 'undefined' ? document.baseURI : location.href;
  return {
    currentOrigin: location.origin,
    portalOrigin: cfg.portalOrigin,
    nodeId: cfg.nodeId,
    targetOrigin: cfg.backendOrigin,
    pinnedPk: cfg.pinnedPk,
    baseHref,
  };
}
