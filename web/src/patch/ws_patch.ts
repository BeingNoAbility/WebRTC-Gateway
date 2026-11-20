// Global monkey patch for WebSocket, Cookie, Anti-Frame-Busting, and XMLHttpRequest upload progress
(() => {
  const globalScope = (typeof globalThis !== 'undefined'
    ? globalThis
    : typeof window !== 'undefined'
    ? window
    : self) as any;

  if (globalScope.__p2p_patched) return;
  globalScope.__p2p_patched = true;

  console.log('[P2P Tunnel] Injecting transparent WebSocket, Cookie, XHR & Anti-Frame-Busting patch...');

  // Extracts effective root domain from hostname (available in Window and Worker scopes)
  const extractRootDomain = (hostname: string): string => {
    try {
      const host = hostname.toLowerCase().split(':')[0];
      if (/^(\d{1,3}\.){3}\d{1,3}$/.test(host) || host === 'localhost') return host;
      const parts = host.split('.');
      if (parts.length <= 2) return host;
      const last = parts[parts.length - 1];
      const secondLast = parts[parts.length - 2];
      if (last.length === 2 && ['co', 'com', 'net', 'org', 'edu', 'gov'].includes(secondLast)) {
        if (parts.length >= 3) return parts.slice(-3).join('.');
        return host;
      }
      return parts.slice(-2).join('.');
    } catch {
      return hostname;
    }
  };

  const isDomainInList = (host: string, list?: string[]): boolean => {
    if (!list || list.length === 0) return false;
    const h = host.toLowerCase().split(':')[0];
    for (const pattern of list) {
      const p = pattern.toLowerCase().trim();
      if (p.startsWith('*.')) {
        const suffix = p.slice(1);
        if (h.endsWith(suffix) || h === p.slice(2)) return true;
      } else if (h === p) {
        return true;
      }
    }
    return false;
  };

  // Store the real portal window reference for inter-frame tunneling BEFORE anti-frame-busting
  const realPortalWindow: Window | null =
    typeof window !== 'undefined' && window.parent && window.parent !== window
      ? window.parent
      : null;

  // 1. Anti-Frame-Busting & Window Virtualization (Window scope only)
  if (typeof window !== 'undefined') {
    try {
      Object.defineProperty(window, 'frameElement', {
        get: () => null,
        configurable: true,
      });
    } catch (e) {}

    try {
      Object.defineProperty(window, 'top', {
        get: () => window,
        set: () => {},
        configurable: true,
      });
    } catch (e) {}

    try {
      Object.defineProperty(window, 'parent', {
        get: () => window,
        set: () => {},
        configurable: true,
      });
    } catch (e) {}

    // Sync document title to outer portal tab
    try {
      const syncTitle = () => {
        if (document.title && window.parent && window.parent !== window) {
          window.parent.document.title = document.title;
        }
      };
      window.addEventListener('load', syncTitle);
      const titleEl = document.querySelector('title');
      if (titleEl) {
        new MutationObserver(syncTitle).observe(titleEl, { childList: true, subtree: true });
      }
    } catch (e) {}

    // Ensure document has no-referrer policy so anti-hotlink CDN images load smoothly
    try {
      if (typeof document !== 'undefined') {
        let meta = document.querySelector('meta[name="referrer"]');
        if (!meta) {
          meta = document.createElement('meta');
          meta.setAttribute('name', 'referrer');
          meta.setAttribute('content', 'no-referrer');
          if (document.head) {
            document.head.prepend(meta);
          }
        } else if (meta.getAttribute('content') !== 'no-referrer') {
          meta.setAttribute('content', 'no-referrer');
        }
      }
    } catch (e) {}

    // Sync active portal context (nodeId, origin, secret) via window message from parent shell
    try {
      window.addEventListener('message', (e) => {
        try {
          if (e.data && e.data.type === 'P2P_PORTAL_CONTEXT') {
            if (e.data.nodeId) {
              globalScope.__P2P_NODE_ID__ = e.data.nodeId;
              try { sessionStorage.setItem('p2p_active_peer_id', e.data.nodeId); } catch {}
            }
            if (e.data.portalOrigin) {
              globalScope.__P2P_PORTAL_ORIGIN__ = e.data.portalOrigin;
            }
          }
        } catch {}
      });
    } catch (e) {}

    // Extracts node ID from subdomain (2nd/3rd level)
    const extractSubdomainNode = (hostname: string): string | null => {
      try {
        const host = hostname.toLowerCase().split(':')[0];
        if (/^(\d{1,3}\.){3}\d{1,3}$/.test(host) || host === 'localhost') return null;

        const paasSuffixes = [
          '.pages.dev', '.workers.dev', '.eu.org', '.github.io',
          '.vercel.app', '.netlify.app', '.onrender.com', '.fly.dev'
        ];
        for (const suffix of paasSuffixes) {
          if (host.endsWith(suffix)) {
            const prefix = host.slice(0, -suffix.length);
            if (!prefix.includes('.')) {
              return null;
            }
          }
        }

        const reserved = new Set([
          'p2p', 'www', 'webrtc', 'webrtc-p2p-signal', 'signal', 'signaling', 'gateway',
          'portal', 'api', 'admin', 'proxy', 'vpn', 'tunnel', 'webrtc-gateway',
          'p2p-gateway', 'app', 'web', 'client', 'relay', 'service', 'hub', 'test',
          'testing', 'stage', 'staging', 'dev', 'demo', 'node', 'nodes', 'server',
          'direct', 'static', 'assets', 'cdn'
        ]);

        const p2pIdx = host.indexOf('.p2p.');
        if (p2pIdx > 0) {
          const parts = host.substring(0, p2pIdx).split('.');
          for (let i = parts.length - 1; i >= 0; i--) {
            const cand = parts[i];
            if (cand && !reserved.has(cand)) return cand;
          }
        }

        const parts = host.split('.');
        if (parts.length >= 3) {
          const first = parts[0];
          if (first && !reserved.has(first)) {
            return first;
          }
        }
      } catch {}
      return null;
    };

    // Helper to extract portal base origin, active nodeId, and secret
    const getPortalBaseUrl = (): { origin: string; nodeId: string; secret: string } => {
      let origin = (globalScope.__P2P_PORTAL_ORIGIN__ as string) || window.location.origin;
      let nodeId = (globalScope.__P2P_NODE_ID__ as string) || '';
      let secret = (globalScope.__P2P_SECRET__ as string) || '';

      // Check current window search or hash
      if (!nodeId) {
        try {
          const sp = new URLSearchParams(window.location.search);
          nodeId = sp.get('peer') || sp.get('node') || '';
          // Security Hardening: secrets are never read from query string, only from hash or storage
          if (!nodeId && window.location.hash) {
            const rawHash = window.location.hash.replace(/^#\/?/, '').trim();
            const parts = rawHash.split(/[?&]/);
            if (parts[0] && !parts[0].includes('=')) {
              nodeId = parts[0];
            }
            for (let i = 1; i < parts.length; i++) {
              const [k, v] = parts[i].split('=');
              if ((k === 'secret' || k === 'token') && v && !secret) {
                secret = decodeURIComponent(v);
              }
            }
          }
        } catch {}
      }

      // Check session or local storage
      if (!nodeId) {
        try {
          nodeId = sessionStorage.getItem('p2p_active_peer_id') || localStorage.getItem('p2p_peer_id') || '';
        } catch {}
      }

      // Subdomain extraction as last resort
      if (!nodeId) {
        nodeId = extractSubdomainNode(window.location.hostname) || '';
      }

      return { origin, nodeId, secret };
    };

    // Resolves a relative or backend URL into a P2P Portal URL maintaining the tunnel
    const resolveP2PUrl = (rawUrl: string): string | null => {
      if (!rawUrl || rawUrl.startsWith('#') || rawUrl.startsWith('javascript:') || rawUrl.startsWith('data:') || rawUrl.startsWith('mailto:')) {
        return null;
      }

      const targetOrigin = globalScope.__P2P_TARGET_ORIGIN__ as string | undefined;
      const { origin: portalOrigin, nodeId, secret } = getPortalBaseUrl();
      if (!portalOrigin) return null;

      let targetPath = '';

      // Protocol-relative (e.g. //www.bilibili.com/video/...)
      if (rawUrl.startsWith('//')) {
        if (targetOrigin) {
          try {
            const targetUrl = new URL(targetOrigin);
            const targetHost = targetUrl.host.toLowerCase();
            const targetRootDomain = (globalScope.__P2P_TARGET_ROOT_DOMAIN__ as string) || extractRootDomain(targetUrl.hostname);
            const parsed = new URL('https:' + rawUrl);
            if (parsed.host.toLowerCase() === targetHost) {
              targetPath = parsed.pathname + parsed.search + parsed.hash;
            } else {
              const resHost = parsed.hostname.toLowerCase();
              const isSibling = targetRootDomain && (resHost === targetRootDomain.toLowerCase() || resHost.endsWith('.' + targetRootDomain.toLowerCase()));
              const isAllowed = isDomainInList(resHost, globalScope.__P2P_ALLOWED_DOMAINS__ as string[]);
              if (isSibling || isAllowed) {
                targetPath = `/--p2p-proxy/${parsed.host}${parsed.pathname}${parsed.search}${parsed.hash}`;
              }
            }
          } catch {}
        }
      } else if (rawUrl.startsWith('/') && !rawUrl.startsWith('//')) {
        // Relative URL (e.g. /video/BV... or /anime/)
        targetPath = rawUrl;
      } else {
        try {
          const resolved = new URL(rawUrl, window.location.href);

          // Check if URL belongs to target origin or same portal origin
          if (targetOrigin) {
            const targetUrl = new URL(targetOrigin);
            const targetHost = targetUrl.host.toLowerCase();
            const targetRootDomain = (globalScope.__P2P_TARGET_ROOT_DOMAIN__ as string) || extractRootDomain(targetUrl.hostname);

            if (resolved.origin.toLowerCase() === targetOrigin.toLowerCase() || resolved.host.toLowerCase() === targetHost) {
              targetPath = resolved.pathname + resolved.search + resolved.hash;
            } else {
              const resHost = resolved.hostname.toLowerCase();
              const isSibling = targetRootDomain && (resHost === targetRootDomain.toLowerCase() || resHost.endsWith('.' + targetRootDomain.toLowerCase()));
              const isAllowed = isDomainInList(resHost, globalScope.__P2P_ALLOWED_DOMAINS__ as string[]);
              if (isSibling || isAllowed) {
                targetPath = `/--p2p-proxy/${resolved.host}${resolved.pathname}${resolved.search}${resolved.hash}`;
              }
            }
          }

          if (!targetPath && resolved.origin === portalOrigin) {
            // Already same portal origin, check if it's not already a portal wrapper
            if (!resolved.searchParams.has('path') && resolved.pathname !== '/') {
              targetPath = resolved.pathname + resolved.search + resolved.hash;
            }
          }
        } catch {
          return null;
        }
      }

      if (!targetPath) {
        return null; // External 3rd-party domain (e.g. github.com), leave native!
      }

      // Build P2P Portal URL preserving the tunnel and node ID
      const p2pUrl = new URL(portalOrigin);
      p2pUrl.searchParams.set('path', targetPath);
      if (nodeId) {
        p2pUrl.searchParams.set('peer', nodeId);
        p2pUrl.hash = nodeId;
      }
      return p2pUrl.href;
    };

    // Expose for testing & internal inspection
    globalScope.__p2pResolveUrl = resolveP2PUrl;

    // Virtualize window.open to keep P2P tunnel active in new windows/tabs
    const origWindowOpen = window.open || Window.prototype.open;
    const patchedOpen = function (url?: string | URL, target?: string, features?: string) {
      let finalUrl = url;
      if (url) {
        const p2pUrl = resolveP2PUrl(url.toString());
        if (p2pUrl) {
          finalUrl = p2pUrl;
        }
      }
      globalScope.__lastOpenedUrl = finalUrl;
      return origWindowOpen.call(window, finalUrl as any, target || '_blank', features);
    };
    try {
      window.open = patchedOpen;
      Window.prototype.open = patchedOpen;
    } catch {}

    // Virtualize in-page <a> links opening in new windows (target="_blank", middle click, Ctrl+click)
    const handleLinkP2P = (e: Event) => {
      const anchor = (e.target as HTMLElement)?.closest?.('a');
      if (!anchor) return;
      const target = anchor.getAttribute('target');
      const mouseEv = e as MouseEvent;
      const isNewWindow = target === '_blank' || target === '_new' || mouseEv.ctrlKey || mouseEv.metaKey || mouseEv.button === 1;

      const rawHref = anchor.getAttribute('href');
      if (rawHref) {
        const p2pUrl = resolveP2PUrl(rawHref);
        if (p2pUrl) {
          if (isNewWindow) {
            anchor.href = p2pUrl;
          } else {
            // For same-viewport navigation, normalize absolute target origin or sibling links
            const rewritten = rewriteBackendUrl(rawHref);
            if (rewritten !== rawHref) {
              anchor.href = rewritten.toString();
            }
          }
        }
      }
    };

    document.addEventListener('pointerdown', handleLinkP2P, true);
    document.addEventListener('click', handleLinkP2P, true);

    // Virtualize forms with target="_blank"
    document.addEventListener('submit', (e: Event) => {
      const form = e.target as HTMLFormElement;
      if (!form || form.getAttribute('target') !== '_blank') return;
      const action = form.getAttribute('action') || '';
      const p2pUrl = resolveP2PUrl(action);
      if (p2pUrl) {
        form.action = p2pUrl;
      }
    }, true);

    // Virtualize History pushState & replaceState (prevents SecurityError on cross-origin SPA routing)
    // and synchronize active sub-route to top-level Portal URL Hash
    try {
      const notifyRouteSync = (subUrl?: string | URL | null) => {
        try {
          const target = realPortalWindow || (typeof window !== 'undefined' && window.parent && window.parent !== window ? window.parent : null);
          if (target && target.postMessage) {
            let currentPath = '';
            if (typeof subUrl === 'string' && subUrl.startsWith('/')) {
              currentPath = subUrl;
            } else {
              currentPath = window.location.pathname + window.location.search + window.location.hash;
            }
            const targetOrigin = (globalScope.__P2P_PORTAL_ORIGIN__ as string) || (typeof window !== 'undefined' ? window.location.origin : '*');
            target.postMessage({
              type: 'P2P_ROUTE_SYNC',
              path: currentPath,
            }, targetOrigin);
          }
        } catch {}
      };

      if (typeof history !== 'undefined') {
        const origPushState = history.pushState;
        const origReplaceState = history.replaceState;
        history.pushState = function (state: any, unused: string, url?: string | URL | null) {
          let finalUrl = url;
          if (url) {
            const rewritten = rewriteBackendUrl(url);
            finalUrl = (typeof rewritten === 'string' && rewritten.startsWith('/')) ? rewritten : url;
          }
          const res = origPushState.call(history, state, unused, finalUrl);
          notifyRouteSync(finalUrl);
          return res;
        };
        history.replaceState = function (state: any, unused: string, url?: string | URL | null) {
          let finalUrl = url;
          if (url) {
            const rewritten = rewriteBackendUrl(url);
            finalUrl = (typeof rewritten === 'string' && rewritten.startsWith('/')) ? rewritten : url;
          }
          const res = origReplaceState.call(history, state, unused, finalUrl);
          notifyRouteSync(finalUrl);
          return res;
        };
      }

      if (typeof window !== 'undefined') {
        window.addEventListener('popstate', () => notifyRouteSync());
        window.addEventListener('hashchange', () => notifyRouteSync());
      }
    } catch {}

    // Virtualize Location.assign & Location.replace
    try {
      if (typeof Location !== 'undefined' && Location.prototype) {
        const origAssign = Location.prototype.assign;
        const origReplace = Location.prototype.replace;
        Location.prototype.assign = function (url: string) {
          const rewritten = rewriteBackendUrl(url);
          return origAssign.call(this, rewritten.toString());
        };
        Location.prototype.replace = function (url: string) {
          const rewritten = rewriteBackendUrl(url);
          return origReplace.call(this, rewritten.toString());
        };
      }
    } catch {}
  }

  // 2. Hook document.cookie (Window / Document scope only) with multi-node virtual isolation
  if (typeof document !== 'undefined') {
    try {
      const originalCookieDesc = Object.getOwnPropertyDescriptor(Document.prototype, 'cookie') ||
                                 Object.getOwnPropertyDescriptor(HTMLDocument.prototype, 'cookie');
      if (originalCookieDesc && originalCookieDesc.set) {
        Object.defineProperty(document, 'cookie', {
          get() {
            const raw = originalCookieDesc.get?.call(document) || '';
            const nodeId = (globalScope.__P2P_NODE_ID__ as string) || '';
            if (!nodeId) return raw;

            const prefix = `__p2p_${nodeId}_`;
            const cookies = raw.split(';');
            const filtered: string[] = [];
            for (const item of cookies) {
              const trimmed = item.trim();
              const eqIdx = trimmed.indexOf('=');
              if (eqIdx === -1) continue;
              const k = trimmed.slice(0, eqIdx).trim();
              const v = trimmed.slice(eqIdx + 1);
              if (k.startsWith(prefix)) {
                filtered.push(`${k.slice(prefix.length)}=${v}`);
              }
            }
            return filtered.join('; ');
          },
          set(val: string) {
            const nodeId = (globalScope.__P2P_NODE_ID__ as string) || '';
            let rewrittenVal = val;
            if (nodeId) {
              const prefix = `__p2p_${nodeId}_`;
              const parts = val.split(';');
              const firstPart = parts[0].trim();
              const eqIdx = firstPart.indexOf('=');
              if (eqIdx !== -1) {
                const k = firstPart.slice(0, eqIdx).trim();
                const v = firstPart.slice(eqIdx + 1);
                const remainder = parts.slice(1).join(';');
                rewrittenVal = `${prefix}${k}=${v}${remainder ? ';' + remainder : ''}`;
              }
            }
            originalCookieDesc.set?.call(document, rewrittenVal);
            try {
              const target = realPortalWindow || (typeof window !== 'undefined' && window.parent && window.parent !== window ? window.parent : null);
              if (target) {
                const targetOrigin = (globalScope.__P2P_PORTAL_ORIGIN__ as string) || (typeof window !== 'undefined' ? window.location.origin : '*');
                target.postMessage({ type: 'TUNNEL_COOKIE_UPDATE', cookie: rewrittenVal }, targetOrigin);
              }
            } catch {}
          },
          configurable: true,
        });
      }
    } catch (err) {
      console.warn('[P2P Tunnel] Failed to wrap document.cookie:', err);
    }
  }

  // 3. Universal Backend Origin & Sibling Domain URL Normalization (Windows & Web Workers)
  // Rewrites requests targeting the backend origin or sibling subdomains to avoid CORS issues
  function rewriteBackendUrl(url: string | URL): string | URL {
    const targetOrigin = globalScope.__P2P_TARGET_ORIGIN__ as string | undefined;
    if (!targetOrigin) return url;

    let urlStr: string;
    if (typeof url === 'string') {
      urlStr = url;
    } else if (url instanceof URL) {
      urlStr = url.href;
    } else {
      return url;
    }

    // Already a proxied path
    if (urlStr.startsWith('/--p2p-proxy/')) {
      return url;
    }

    // Same target origin exact prefix: normalize to relative path
    if (urlStr.startsWith(targetOrigin)) {
      return urlStr.slice(targetOrigin.length) || '/';
    }

    try {
      const base = (typeof window !== 'undefined' && window.location) ? window.location.href : 'http://localhost';
      const resolved = new URL(urlStr, base);
      const targetUrl = new URL(targetOrigin);

      if (resolved.origin.toLowerCase() === targetUrl.origin.toLowerCase()) {
        return resolved.pathname + resolved.search + resolved.hash;
      }

      // Check sibling subdomain under root domain, or whitelisted proxy domain
      const targetRootDomain = (globalScope.__P2P_TARGET_ROOT_DOMAIN__ as string) || extractRootDomain(targetUrl.hostname);
      const resHost = resolved.hostname.toLowerCase();
      const isSibling = targetRootDomain && (resHost === targetRootDomain.toLowerCase() || resHost.endsWith('.' + targetRootDomain.toLowerCase()));
      const isAllowed = isDomainInList(resHost, globalScope.__P2P_ALLOWED_DOMAINS__ as string[]);

      if (isSibling || isAllowed) {
        return `/--p2p-proxy/${resolved.host}${resolved.pathname}${resolved.search}${resolved.hash}`;
      }
    } catch {}

    return url;
  }

  if (globalScope.fetch) {
    const origFetch = globalScope.fetch;
    globalScope.fetch = function (input: RequestInfo | URL, init?: RequestInit) {
      if (typeof input === 'string' || input instanceof URL) {
        input = rewriteBackendUrl(input) as any;
      } else if (typeof Request !== 'undefined' && input instanceof Request) {
        const rewritten = rewriteBackendUrl(input.url);
        if (rewritten !== input.url) {
          input = new Request(rewritten, input);
        }
      }

      // Attach client-side document.cookie via X-Client-Cookie header so Agent merges it
      if (typeof document !== 'undefined' && document.cookie) {
        init = init || {};
        const headers = new Headers(init.headers || {});
        if (!headers.has('X-Client-Cookie')) {
          headers.set('X-Client-Cookie', document.cookie);
        }
        init.headers = headers;
      }

      // Attach x-p2p-body-length if upload body length is computable
      if (init && init.body) {
        let totalBytes = 0;
        const b = init.body as any;
        if (typeof b === 'string') {
          totalBytes = new Blob([b]).size;
        } else if (b instanceof Blob) {
          totalBytes = b.size;
        } else if (b instanceof ArrayBuffer) {
          totalBytes = b.byteLength;
        } else if (ArrayBuffer.isView(b)) {
          totalBytes = b.byteLength;
        }
        if (totalBytes > 0) {
          init = init || {};
          const headers = new Headers(init.headers || {});
          if (!headers.has('x-p2p-body-length')) {
            headers.set('x-p2p-body-length', totalBytes.toString());
          }
          init.headers = headers;
        }
      }

      return origFetch.call(this, input, init);
    };
  }

  // Hook navigator.sendBeacon
  if (typeof navigator !== 'undefined' && navigator.sendBeacon) {
    const origBeacon = navigator.sendBeacon;
    navigator.sendBeacon = function (url: string | URL, data?: BodyInit | null) {
      const rewritten = rewriteBackendUrl(url);
      return origBeacon.call(this, rewritten.toString(), data);
    };
  }

  // Hook EventSource (Server-Sent Events)
  if (typeof EventSource !== 'undefined') {
    const OriginalEventSource = EventSource;
    class PatchedEventSource extends OriginalEventSource {
      constructor(url: string | URL, eventSourceInitDict?: EventSourceInit) {
        const rewritten = rewriteBackendUrl(url);
        super(rewritten.toString(), eventSourceInitDict);
      }
    }
    globalScope.EventSource = PatchedEventSource;
  }

  // 4. Universal XMLHttpRequest Upload Progress Virtualization (Windows & Web Workers)
  // In Chromium, when an XHR is intercepted by a Service Worker, Chromium's ServiceWorkerSubresourceLoader
  // fails to dispatch ProgressEvent to xhr.upload. We transparently synthesize W3C-standard ProgressEvents
  // to ensure upload progress bars and upload speed measurement work across all applications.
  if (typeof XMLHttpRequest !== 'undefined') {
    const OriginalXHR = XMLHttpRequest;
    const originalOpen = OriginalXHR.prototype.open;
    const originalSend = OriginalXHR.prototype.send;
    const originalAbort = OriginalXHR.prototype.abort;

    let lastKnownSpeed = 15 * 1024 * 1024; // Default seed: 15 MB/s (~120 Mbps)

    OriginalXHR.prototype.open = function (method: string, url: string | URL, async: boolean = true, ...rest: any[]) {
      const rewrittenUrl = rewriteBackendUrl(url);
      (this as any)._p2pMethod = method;
      (this as any)._p2pUrl = rewrittenUrl;
      return originalOpen.call(this, method, rewrittenUrl, async, ...rest);
    };

    OriginalXHR.prototype.send = function (body?: any) {
      if (typeof document !== 'undefined' && document.cookie) {
        try {
          this.setRequestHeader('X-Client-Cookie', document.cookie);
        } catch (e) {}
      }

      if (!body) {
        return originalSend.call(this, body);
      }

      // Determine upload body length if computable (using native Blob.size avoids TextEncoder string duplicate in V8 heap)
      let totalBytes = 0;
      if (typeof body === 'string') {
        totalBytes = new Blob([body]).size;
      } else if (body instanceof Blob) {
        totalBytes = body.size;
      } else if (body instanceof ArrayBuffer) {
        totalBytes = body.byteLength;
      } else if (ArrayBuffer.isView(body)) {
        totalBytes = body.byteLength;
      }

      if (totalBytes > 0) {
        try {
          this.setRequestHeader('x-p2p-body-length', totalBytes.toString());
        } catch (e) {}
      }

      if (totalBytes <= 0 || !this.upload) {
        return originalSend.call(this, body);
      }

      const upload = this.upload;
      let loadedBytes = 0;
      let timer: any = null;
      let completed = false;
      const startTime = performance.now();

      const dispatchProgress = (loaded: number) => {
        if (completed) return;
        loadedBytes = Math.min(loaded, totalBytes);
        try {
          const ev = new ProgressEvent('progress', {
            lengthComputable: true,
            loaded: loadedBytes,
            total: totalBytes,
          });
          upload.dispatchEvent(ev);
        } catch {}
      };

      const finishUpload = () => {
        if (completed) return;
        completed = true;
        if (timer) {
          clearInterval(timer);
          timer = null;
        }

        // Calibrate lastKnownSpeed based on actual transfer time
        const elapsed = (performance.now() - startTime) / 1000;
        if (elapsed > 0.05 && totalBytes > 0) {
          const measuredSpeed = totalBytes / elapsed;
          lastKnownSpeed = (lastKnownSpeed * 0.3) + (measuredSpeed * 0.7);
        }

        // Dispatch final 100% progress
        dispatchProgress(totalBytes);

        try {
          upload.dispatchEvent(new ProgressEvent('load', {
            lengthComputable: true,
            loaded: totalBytes,
            total: totalBytes,
          }));
          upload.dispatchEvent(new ProgressEvent('loadend', {
            lengthComputable: true,
            loaded: totalBytes,
            total: totalBytes,
          }));
        } catch {}
      };

      // Dispatched when request headers arrive or upload finishes
      const onHeadersOrComplete = () => {
        finishUpload();
      };

      this.addEventListener('readystatechange', () => {
        if (this.readyState >= 2) {
          onHeadersOrComplete();
        }
      });
      this.addEventListener('load', onHeadersOrComplete);
      this.addEventListener('error', () => {
        if (completed) return;
        completed = true;
        if (timer) clearInterval(timer);
        try {
          upload.dispatchEvent(new ProgressEvent('error'));
          upload.dispatchEvent(new ProgressEvent('loadend'));
        } catch {}
      });
      this.addEventListener('abort', () => {
        if (completed) return;
        completed = true;
        if (timer) clearInterval(timer);
        try {
          upload.dispatchEvent(new ProgressEvent('abort'));
          upload.dispatchEvent(new ProgressEvent('loadend'));
        } catch {}
      });

      // Dispatch initial loadstart
      try {
        upload.dispatchEvent(new ProgressEvent('loadstart', {
          lengthComputable: true,
          loaded: 0,
          total: totalBytes,
        }));
      } catch {}

      // Periodically synthesize upload progress events matching network transfer speed
      timer = setInterval(() => {
        if (this.readyState >= 2 || completed) {
          finishUpload();
          return;
        }
        const elapsed = (performance.now() - startTime) / 1000;
        const estLoaded = Math.floor(elapsed * lastKnownSpeed);
        // Cap estimated progress at 98% until headers/readyState >= 2 confirms delivery
        const progressTarget = Math.min(estLoaded, Math.floor(totalBytes * 0.98));
        if (progressTarget > loadedBytes) {
          dispatchProgress(progressTarget);
        }
      }, 50);

      (this as any)._p2pUploadTimer = timer;

      return originalSend.call(this, body);
    };

    OriginalXHR.prototype.abort = function () {
      if ((this as any)._p2pUploadTimer) {
        clearInterval((this as any)._p2pUploadTimer);
      }
      return originalAbort.call(this);
    };
  }

  // 4. Hook WebSocket (Window and Worker scope)
  if (typeof WebSocket !== 'undefined') {
    class PatchedWebSocket extends EventTarget implements WebSocket {
      public static readonly CONNECTING = 0;
      public static readonly OPEN = 1;
      public static readonly CLOSING = 2;
      public static readonly CLOSED = 3;

      public readonly CONNECTING = 0;
      public readonly OPEN = 1;
      public readonly CLOSING = 2;
      public readonly CLOSED = 3;

      public url: string;
      public readyState: 0 | 1 | 2 | 3 = PatchedWebSocket.CONNECTING;
      public bufferedAmount: number = 0;
      public extensions: string = '';
      public protocol: string = '';
      public binaryType: BinaryType = 'blob';

      public onopen: ((this: WebSocket, ev: Event) => any) | null = null;
      public onmessage: ((this: WebSocket, ev: MessageEvent) => any) | null = null;
      public onerror: ((this: WebSocket, ev: Event) => any) | null = null;
      public onclose: ((this: WebSocket, ev: CloseEvent) => any) | null = null;

      private port: MessagePort | null = null;
      private channel: MessageChannel | null = null;

      constructor(url: string | URL, protocols?: string | string[]) {
        super();
        this.url = url.toString();

        const baseHref = typeof window !== 'undefined'
          ? window.location.href
          : (typeof location !== 'undefined' ? location.href : '');
        const rewritten = rewriteBackendUrl(this.url);
        let path: string;
        if (typeof rewritten === 'string' && rewritten.startsWith('/')) {
          path = rewritten;
        } else {
          const parsed = new URL(this.url, baseHref);
          path = parsed.pathname + parsed.search;
        }

        const requestedProtocols = protocols
          ? Array.isArray(protocols)
            ? protocols
            : [protocols]
          : [];

        this.channel = new MessageChannel();
        this.port = this.channel.port1;

        this.port.onmessage = (event: MessageEvent) => {
          const msg = event.data;
          switch (msg.type) {
            case 'WS_OPENED':
              this.readyState = PatchedWebSocket.OPEN;
              this.protocol = msg.protocol || '';
              const openEv = new Event('open');
              this.dispatchEvent(openEv);
              this.onopen?.(openEv);
              break;

            case 'WS_MSG':
              let data = msg.data;
              if (this.binaryType === 'blob' && data instanceof ArrayBuffer) {
                data = new Blob([data]);
              }
              const msgEv = new MessageEvent('message', { data });
              this.dispatchEvent(msgEv);
              this.onmessage?.(msgEv);
              break;

            case 'WS_CLOSED':
              this.readyState = PatchedWebSocket.CLOSED;
              const closeEv = new CloseEvent('close', {
                code: msg.code || 1000,
                reason: msg.reason || '',
                wasClean: true,
              });
              this.dispatchEvent(closeEv);
              this.onclose?.(closeEv);
              this.port?.close();
              break;

            case 'WS_ERROR':
              const errEv = new Event('error');
              this.dispatchEvent(errEv);
              this.onerror?.(errEv);
              break;
          }
        };

        const targetWindow = realPortalWindow || (typeof window !== 'undefined'
          ? (window.parent !== window ? window.parent : window)
          : null);

        if (targetWindow) {
          const targetOrigin = (globalScope.__P2P_PORTAL_ORIGIN__ as string) || (typeof window !== 'undefined' ? window.location.origin : '*');
          targetWindow.postMessage(
            {
              type: 'TUNNEL_WS_CONNECT',
              path,
              protocols: requestedProtocols,
            },
            targetOrigin,
            [this.channel.port2]
          );
        }
      }

      public send(data: string | ArrayBufferLike | Blob | ArrayBufferView): void {
        if (this.readyState !== PatchedWebSocket.OPEN) {
          throw new DOMException('WebSocket is not open', 'InvalidStateError');
        }

        if (typeof data === 'string') {
          this.port?.postMessage({ type: 'WS_SEND', opcode: 1, data });
        } else if (data instanceof ArrayBuffer) {
          this.port?.postMessage({ type: 'WS_SEND', opcode: 2, data }, [data]);
        } else if (data instanceof Blob) {
          data.arrayBuffer().then((buf) => {
            this.port?.postMessage({ type: 'WS_SEND', opcode: 2, data: buf }, [buf]);
          });
        } else if (ArrayBuffer.isView(data)) {
          const buf = data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength);
          this.port?.postMessage({ type: 'WS_SEND', opcode: 2, data: buf }, [buf]);
        }
      }

      public close(code: number = 1000, reason: string = ''): void {
        if (this.readyState === PatchedWebSocket.CLOSING || this.readyState === PatchedWebSocket.CLOSED) {
          return;
        }
        this.readyState = PatchedWebSocket.CLOSING;
        this.port?.postMessage({ type: 'WS_CLOSE', code, reason });
      }
    }

    globalScope.WebSocket = PatchedWebSocket;
  }

  // 5. Web Worker & SharedWorker virtualization (Module Worker dynamic adaptation)
  // When constructing a module worker (e.g. new Worker(url, { type: 'module' })),
  // W3C spec forbids importScripts(). We append ?_p2p_worker=module so Service Worker
  // dynamically injects `import "/patch.js";` instead of `importScripts("/patch.js");`.
  const appendWorkerTypeParam = (url: string | URL, options?: WorkerOptions | string): string | URL => {
    const isModule = typeof options === 'object' && options !== null && (options as WorkerOptions).type === 'module';
    if (isModule) {
      if (typeof url === 'string') {
        const hashIdx = url.indexOf('#');
        const hash = hashIdx !== -1 ? url.substring(hashIdx) : '';
        const base = hashIdx !== -1 ? url.substring(0, hashIdx) : url;
        const sep = base.includes('?') ? '&' : '?';
        return `${base}${sep}_p2p_worker=module${hash}`;
      } else if (url instanceof URL) {
        const u = new URL(url.href);
        u.searchParams.set('_p2p_worker', 'module');
        return u;
      }
    }
    return url;
  };

  if (typeof globalScope.Worker !== 'undefined') {
    const OrigWorker = globalScope.Worker;
    class PatchedWorker extends OrigWorker {
      constructor(scriptURL: string | URL, options?: WorkerOptions) {
        const rewritten = appendWorkerTypeParam(scriptURL, options);
        super(rewritten, options);
      }
    }
    globalScope.Worker = PatchedWorker;
  }

  if (typeof globalScope.SharedWorker !== 'undefined') {
    const OrigSharedWorker = globalScope.SharedWorker;
    class PatchedSharedWorker extends OrigSharedWorker {
      constructor(scriptURL: string | URL, options?: string | WorkerOptions) {
        const rewritten = appendWorkerTypeParam(scriptURL, options);
        super(rewritten as any, options as any);
      }
    }
    globalScope.SharedWorker = PatchedSharedWorker;
  }
})();
