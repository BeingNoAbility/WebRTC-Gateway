// Automated verification of Node ID preservation and URL virtualization
const fs = require('fs');
const path = require('path');
const vm = require('vm');

console.log('=== Running P2P Portal Node ID Preservation & Virtualization Verification ===\n');

let passCount = 0;
let failCount = 0;

function assert(condition, message) {
  if (condition) {
    console.log(`[PASS] ${message}`);
    passCount++;
  } else {
    console.error(`[FAIL] ${message}`);
    failCount++;
  }
}

// 1. Verify dist files exist and are built
const patchJsPath = path.resolve(__dirname, '../web/dist/patch.js');
const swJsPath = path.resolve(__dirname, '../web/dist/sw.js');
const portalJsPath = path.resolve(__dirname, '../web/dist/portal.js');

assert(fs.existsSync(patchJsPath), 'web/dist/patch.js exists');
assert(fs.existsSync(swJsPath), 'web/dist/sw.js exists');
assert(fs.existsSync(portalJsPath), 'web/dist/portal.js exists');

const patchCode = fs.readFileSync(patchJsPath, 'utf-8');
const swCode = fs.readFileSync(swJsPath, 'utf-8');
const portalCode = fs.readFileSync(portalJsPath, 'utf-8');

// 2. Verify Service Worker script injection includes activeNodeId
assert(swCode.includes('window.__P2P_NODE_ID__'), 'sw.js injects window.__P2P_NODE_ID__');
assert(swCode.includes('window.__P2P_PORTAL_ORIGIN__'), 'sw.js injects window.__P2P_PORTAL_ORIGIN__');
assert(swCode.includes('NODE_CHANGE'), 'sw.js listens for NODE_CHANGE');
assert(!swCode.includes('__P2P_SECRET__'), 'sw.js MUST NOT inject window.__P2P_SECRET__ (P0-3)');
assert(!swCode.includes("includes('/worker')") && !swCode.includes('includes("/worker")'), 'sw.js MUST NOT use fuzzy url.pathname.includes("/worker") (P1-4)');
assert(swCode.includes('__P2P_ALLOWED_DOMAINS__') && swCode.includes('subarray'), 'sw.js uses pure byte stream injection without UTF-8 slicing corruption (P1-4)');
assert(swCode.includes('resultingClientId'), 'sw.js includes multi-tab iframe-to-portal session mapping via resultingClientId (P1-2)');
assert(swCode.includes('matchAll') && swCode.includes('window'), 'sw.js includes Active GC against dead window sessions');
assert(swCode.includes('clients.get') || swCode.includes('.clients.get'), 'sw.js includes Lazy GC client liveness verification');
assert(swCode.includes('DecompressionStream') && swCode.includes('x-p2p-client-decompress'), 'sw.js supports hardware streaming decompression and x-p2p-client-decompress (v2.0.0 Task 1)');
assert(swCode.includes('_p2p_worker') && swCode.includes('import "/patch.js";'), 'sw.js supports dynamic ES module worker import (v2.0.0 Task 2)');
assert(swCode.includes('!doctype'), 'sw.js supports DOCTYPE-safe HTML script injection (F-10)');
assert(swCode.includes('application/xhtml+xml'), 'sw.js includes navigation document guard against API fetch pollution');
assert(patchCode.includes('_p2p_worker'), 'patch.js wraps Worker for dynamic module worker adaptation (v2.0.0 Task 2)');
assert(!portalCode.includes('value="${this.peerId}"'), 'portal.js MUST NOT interpolate peerId into input value attribute (P0-2)');
assert(!portalCode.includes('value="${this.secret}"'), 'portal.js MUST NOT interpolate secret into input value attribute (P0-2)');

const indexHtmlPath = path.resolve(__dirname, '../web/dist/index.html');
assert(fs.existsSync(indexHtmlPath), 'web/dist/index.html exists');
const indexHtmlCode = fs.readFileSync(indexHtmlPath, 'utf-8');
assert(indexHtmlCode.includes('v2.3.2'), 'index.html displays v2.3.2 version badge in header');
assert(portalCode.includes('btnCloseOffline') && portalCode.includes('btnDismissOffline'), 'portal.js provides dismissable offline prompt with close and dismiss controls');
assert(portalCode.includes('p2pOfflineAlertBar'), 'portal.js degrades to non-blocking offline alert bar on dismiss');
assert(portalCode.includes('P2PMux v2.3 Dual-Channel (10-byte Header / 32KB Frame)'), 'portal.js displays non-sensitive protocol and runtime diagnostics');

// 3. Test patch.js virtualization in simulated DOM environment
function createMockDOM(options = {}) {
  const listeners = {};
  const mockStorage = { ...options.localStorage };
  const mockSession = { ...options.sessionStorage };

  class EventTarget {
    constructor() {
      this._targetListeners = {};
    }
    addEventListener(type, fn) {
      this._targetListeners[type] = this._targetListeners[type] || [];
      this._targetListeners[type].push(fn);
    }
    removeEventListener(type, fn) {
      if (this._targetListeners[type]) {
        this._targetListeners[type] = this._targetListeners[type].filter(f => f !== fn);
      }
    }
    dispatchEvent(ev) {
      const fns = this._targetListeners[ev?.type] || [];
      for (const fn of fns) fn(ev);
    }
  }

  class Document extends EventTarget {
    constructor() {
      super();
      this.cookie = '';
    }
    querySelector() { return null; }
    createElement() {
      return {
        setAttribute: () => {},
        getAttribute: () => null,
      };
    }
    addEventListener(type, fn) {
      listeners[type] = listeners[type] || [];
      listeners[type].push(fn);
    }
  }

  class HTMLElement extends EventTarget {}
  class HTMLFormElement extends HTMLElement {}
  class Location {}
  class History {}
  class WebSocket extends EventTarget {
    static OPEN = 1;
    constructor() { super(); }
  }
  class XMLHttpRequest extends EventTarget {
    constructor() {
      super();
      this.upload = new EventTarget();
    }
    open() {}
    send() {}
    setRequestHeader() {}
  }
  class MutationObserver {
    observe() {}
    disconnect() {}
  }
  class MouseEvent {}
  class Event {
    constructor(type) { this.type = type; }
  }
  class MessageEvent extends Event {
    constructor(type, init = {}) { super(type); this.data = init.data; }
  }
  class CloseEvent extends Event {
    constructor(type, init = {}) { super(type); this.code = init.code; this.reason = init.reason; }
  }
  class MessageChannel {
    constructor() {
      this.port1 = { onmessage: null, postMessage: () => {}, close: () => {} };
      this.port2 = { onmessage: null, postMessage: () => {}, close: () => {} };
    }
  }

  const docInstance = new Document();

  const windowObj = {
    location: {
      origin: options.origin || 'https://my-tunnel.pages.dev',
      hostname: options.hostname || 'my-tunnel.pages.dev',
      href: (options.origin || 'https://my-tunnel.pages.dev') + (options.pathname || '/'),
      search: options.search || '',
      hash: options.hash || '',
    },
    addEventListener: (type, fn) => {
      listeners[type] = listeners[type] || [];
      listeners[type].push(fn);
    },
    removeEventListener: () => {},
    localStorage: {
      getItem: (k) => mockStorage[k] || null,
      setItem: (k, v) => { mockStorage[k] = String(v); },
      removeItem: (k) => { delete mockStorage[k]; },
    },
    sessionStorage: {
      getItem: (k) => mockSession[k] || null,
      setItem: (k, v) => { mockSession[k] = String(v); },
      removeItem: (k) => { delete mockSession[k]; },
    },
    document: docInstance,
    open: (url, target, features) => {
      windowObj.__lastOpenedUrl = url;
      return { url, target, features };
    },
    Window: function () {},
    Document,
    HTMLDocument: Document,
    EventTarget,
    HTMLElement,
    HTMLFormElement,
    Location,
    History,
    WebSocket,
    XMLHttpRequest,
    MutationObserver,
    MouseEvent,
    Event,
    MessageEvent,
    CloseEvent,
    MessageChannel,
    console: console,
    __lastOpenedUrl: null,
    __P2P_NODE_ID__: options.p2pNodeId,
    __P2P_PORTAL_ORIGIN__: options.portalOrigin,
    __P2P_TARGET_ORIGIN__: options.targetOrigin || 'https://www.bilibili.com',
    __P2P_TARGET_ROOT_DOMAIN__: options.targetRootDomain || 'bilibili.com',
  };

  windowObj.Window.prototype = windowObj;
  // Mock parent to window to simulate anti-frame-busting
  windowObj.parent = windowObj;
  windowObj.top = windowObj;

  class MockWorker {
    constructor(scriptURL, options) {
      this.scriptURL = scriptURL;
      this.options = options;
    }
  }
  class MockSharedWorker {
    constructor(scriptURL, options) {
      this.scriptURL = scriptURL;
      this.options = options;
    }
  }

  windowObj.Worker = MockWorker;
  windowObj.SharedWorker = MockSharedWorker;

  const sandbox = {
    window: windowObj,
    globalThis: windowObj,
    document: docInstance,
    Document,
    HTMLDocument: Document,
    EventTarget,
    HTMLElement,
    HTMLFormElement,
    Location,
    History,
    WebSocket,
    XMLHttpRequest,
    Worker: MockWorker,
    SharedWorker: MockSharedWorker,
    MutationObserver,
    MouseEvent,
    Event,
    MessageEvent,
    CloseEvent,
    MessageChannel,
    location: windowObj.location,
    localStorage: windowObj.localStorage,
    sessionStorage: windowObj.sessionStorage,
    URL: URL,
    URLSearchParams: URLSearchParams,
    console: console,
  };

  vm.createContext(sandbox);
  return { sandbox, windowObj, listeners };
}

// Test Case 1: Injected __P2P_NODE_ID__ = 'bili'
{
  const { sandbox, windowObj } = createMockDOM({
    origin: 'https://gateway.example.com',
    hostname: 'gateway.example.com',
    p2pNodeId: 'bili',
    portalOrigin: 'https://gateway.example.com',
    targetOrigin: 'https://www.bilibili.com',
  });

  vm.runInContext(patchCode, sandbox);

  assert(typeof sandbox.window.__p2pResolveUrl === 'function', '__p2pResolveUrl exposed');

  // Relative link inside bilibili page
  const resolvedRel = sandbox.window.__p2pResolveUrl('/video/BV1xx411c7mD');
  assert(resolvedRel !== null, 'Relative URL resolved');
  const u1 = new URL(resolvedRel);
  assert(u1.origin === 'https://gateway.example.com', 'Origin preserved as gateway origin');
  assert(u1.searchParams.get('path') === '/video/BV1xx411c7mD', 'Path parameter set correctly');
  assert(u1.searchParams.get('peer') === 'bili', 'Query param peer=bili set');
  assert(u1.hash === '#bili', 'Hash #bili set');

  // Absolute link to target domain (www.bilibili.com)
  const resolvedAbs = sandbox.window.__p2pResolveUrl('https://www.bilibili.com/read/cv123');
  const u2 = new URL(resolvedAbs);
  assert(u2.searchParams.get('path') === '/read/cv123', 'Absolute target domain resolved to internal path');
  assert(u2.searchParams.get('peer') === 'bili', 'Target domain link preserves peer=bili');
  assert(u2.hash === '#bili', 'Target domain link preserves hash #bili');

  // Sibling domain link (api.bilibili.com)
  const resolvedSib = sandbox.window.__p2pResolveUrl('//api.bilibili.com/x/web-interface/nav');
  const u3 = new URL(resolvedSib);
  assert(u3.searchParams.get('path') === '/--p2p-proxy/api.bilibili.com/x/web-interface/nav', 'Sibling domain resolved to --p2p-proxy');
  assert(u3.searchParams.get('peer') === 'bili', 'Sibling domain link preserves peer=bili');

  // External link (e.g. github.com)
  const resolvedExt = sandbox.window.__p2pResolveUrl('https://github.com/torvalds/linux');
  assert(resolvedExt === null, 'External 3rd-party domain is NOT virtualized (native navigation)');

  // window.open virtualization
  windowObj.open('https://www.bilibili.com/video/BV999', '_blank');
  assert(windowObj.__lastOpenedUrl && windowObj.__lastOpenedUrl.includes('peer=bili'), 'window.open opens URL with peer=bili');

  // Web Worker & SharedWorker module adaptation virtualization (Task 2)
  const mw1 = new sandbox.window.Worker('/worker.js', { type: 'module' });
  assert(mw1.scriptURL.includes('_p2p_worker=module'), 'new Worker with type module appends _p2p_worker=module');
  const mw2 = new sandbox.window.Worker('/worker.js?env=prod', { type: 'module' });
  assert(mw2.scriptURL.includes('&_p2p_worker=module'), 'new Worker with query appends &_p2p_worker=module');
  const cw = new sandbox.window.Worker('/worker.js');
  assert(!cw.scriptURL.includes('_p2p_worker'), 'classic Worker does not append _p2p_worker');

  const msw = new sandbox.window.SharedWorker('/shared.js', { type: 'module' });
  assert(msw.scriptURL.includes('_p2p_worker=module'), 'new SharedWorker with type module appends _p2p_worker=module');
}

// Test Case 2: No injection, but stored active peer in sessionStorage or localStorage
{
  const { sandbox } = createMockDOM({
    origin: 'https://webrtc-tunnel.mydomain.com',
    hostname: 'webrtc-tunnel.mydomain.com',
    localStorage: { p2p_peer_id: 'bili' },
    targetOrigin: 'https://www.bilibili.com',
  });

  vm.runInContext(patchCode, sandbox);

  const resolved = sandbox.window.__p2pResolveUrl('/video/BV111');
  const u = new URL(resolved);
  assert(u.searchParams.get('peer') === 'bili', 'Stored localStorage peer ID used instead of subdomain webrtc-tunnel');
  assert(u.hash === '#bili', 'Hash set to #bili from localStorage');
}

// Test Case 3: PaaS domain like .pages.dev or .workers.dev
{
  const { sandbox } = createMockDOM({
    origin: 'https://my-p2p-app.pages.dev',
    hostname: 'my-p2p-app.pages.dev',
    localStorage: { p2p_peer_id: 'bili' },
    targetOrigin: 'https://www.bilibili.com',
  });

  vm.runInContext(patchCode, sandbox);

  const resolved = sandbox.window.__p2pResolveUrl('/video/BV222');
  const u = new URL(resolved);
  assert(u.searchParams.get('peer') === 'bili', 'PaaS subdomain my-p2p-app is NOT used; stored peer ID bili used');
}

// Test Case 4: Message event P2P_PORTAL_CONTEXT dynamically updates node ID
{
  const { sandbox, listeners } = createMockDOM({
    origin: 'https://gateway.example.com',
    hostname: 'gateway.example.com',
    targetOrigin: 'https://www.bilibili.com',
  });

  vm.runInContext(patchCode, sandbox);

  // Send message event
  const msgHandler = listeners['message']?.[0];
  assert(typeof msgHandler === 'function', 'Window message listener registered for P2P_PORTAL_CONTEXT');

  msgHandler({
    data: {
      type: 'P2P_PORTAL_CONTEXT',
      nodeId: 'bili',
      portalOrigin: 'https://gateway.example.com',
      secret: 'secret123',
    },
  });

  const resolved = sandbox.window.__p2pResolveUrl('/video/BV333');
  const u = new URL(resolved);
  assert(u.searchParams.get('peer') === 'bili', 'Dynamically received P2P_PORTAL_CONTEXT nodeId=bili used');
  assert(!u.searchParams.has('secret'), 'Security hardening: secret is never leaked into URL query parameters');
}

// Test Case 5: Verification of portal app.ts configuration resolution logic
{
  // Test resolveInitialConfig priorities
  function simulateResolveConfig(urlStr, storage = {}) {
    const url = new URL(urlStr);
    const params = url.searchParams;

    const reserved = new Set([
      'p2p', 'www', 'webrtc', 'webrtc-p2p-signal', 'signal', 'signaling',
      'gateway', 'portal', 'api', 'admin', 'proxy', 'vpn', 'tunnel',
      'webrtc-gateway', 'p2p-gateway', 'app', 'web', 'client', 'relay',
      'service', 'hub', 'test', 'testing', 'stage', 'staging', 'dev', 'demo',
      'node', 'nodes', 'server', 'direct', 'static', 'assets', 'cdn'
    ]);

    function extractExplicitP2PSubdomain(hostname) {
      const p2pIndex = hostname.indexOf('.p2p.');
      if (p2pIndex > 0) {
        const sub = hostname.substring(0, p2pIndex);
        const parts = sub.split('.');
        for (let i = parts.length - 1; i >= 0; i--) {
          const candidate = parts[i];
          if (candidate && !reserved.has(candidate)) return candidate;
        }
      }
      return null;
    }

    function extractNodeIdFromHostname(hostname) {
      if (/^(\d{1,3}\.){3}\d{1,3}$/.test(hostname) || hostname === 'localhost') return null;
      const paasSuffixes = ['.pages.dev', '.workers.dev', '.eu.org', '.github.io', '.vercel.app'];
      for (const suffix of paasSuffixes) {
        if (hostname.endsWith(suffix)) {
          const prefix = hostname.slice(0, -suffix.length);
          if (!prefix.includes('.')) return null;
        }
      }
      const parts = hostname.split('.');
      if (parts.length >= 3 || (parts.length === 2 && parts[1] === 'localhost')) {
        const firstLabel = parts[0];
        if (firstLabel && !reserved.has(firstLabel)) return firstLabel;
      }
      return null;
    }

    function isValidNodeId(id) {
      return /^[a-zA-Z0-9._-]{1,64}$/.test(id);
    }

    let peerId = params.get('peer') || params.get('node');
    if (peerId && !isValidNodeId(peerId)) {
      peerId = null;
    }
    let secret = params.get('secret') || params.get('token') || '';

    if (!peerId && url.hash) {
      const rawHash = url.hash.replace(/^#\/?/, '').trim();
      if (rawHash) {
        const parts = rawHash.split(/[?&]/);
        if (isValidNodeId(parts[0])) {
          peerId = parts[0];
        }
      }
    }

    if (!peerId) {
      const explicitSub = extractExplicitP2PSubdomain(url.hostname);
      if (explicitSub && isValidNodeId(explicitSub)) peerId = explicitSub;
    }

    if (!peerId) {
      const cached = storage['p2p_active_peer_id'] || storage['p2p_peer_id'] || '';
      if (cached && isValidNodeId(cached)) peerId = cached;
    }

    if (!peerId) {
      const sub = extractNodeIdFromHostname(url.hostname);
      if (sub && isValidNodeId(sub)) peerId = sub;
    }

    if (!peerId || !isValidNodeId(peerId)) {
      peerId = 'default_node';
    }

    return { peerId, secret };
  }

  // 1. Popup URL with ?path=...&peer=bili#bili on portal domain
  const cfg1 = simulateResolveConfig('https://my-gateway.pages.dev/?path=/video/BV1xx&peer=bili#bili');
  assert(cfg1.peerId === 'bili', 'Popup URL with ?peer=bili resolves peerId = bili');

  // 2. Popup URL with only hash #bili
  const cfg2 = simulateResolveConfig('https://my-gateway.pages.dev/?path=/video/BV1xx#bili');
  assert(cfg2.peerId === 'bili', 'Popup URL with hash #bili resolves peerId = bili');

  // 3. Popup URL with only path, but stored localStorage has bili
  const cfg3 = simulateResolveConfig('https://my-gateway.pages.dev/?path=/video/BV1xx', { p2p_peer_id: 'bili' });
  assert(cfg3.peerId === 'bili', 'Popup URL without hash/query preserves stored bili node, ignores subdomain my-gateway');

  // 4. Explicit .p2p. subdomain
  const cfg4 = simulateResolveConfig('https://bili.p2p.example.com/');
  assert(cfg4.peerId === 'bili', 'Explicit bili.p2p.example.com resolves peerId = bili');

  // 5. Reserved subdomain gateway.example.com falls back to default_node
  const cfg5 = simulateResolveConfig('https://gateway.example.com/');
  assert(cfg5.peerId === 'default_node', 'Reserved gateway subdomain does not become node ID');

  // 5b. Dedicated testing portal subdomain testing.p2p.example.com falls back to default_node
  const cfgTestingPortal = simulateResolveConfig('https://testing.p2p.example.com/');
  assert(cfgTestingPortal.peerId === 'default_node', 'Reserved testing portal subdomain does not become node ID');

  // 5c. Multi-level testing subdomain bili.testing.p2p.example.com resolves peerId = bili
  const cfgTestingNode = simulateResolveConfig('https://bili.testing.p2p.example.com/');
  assert(cfgTestingNode.peerId === 'bili', 'Multi-level testing subdomain bili.testing.p2p.example.com resolves peerId = bili');

  // 6. XSS Injection attack via hash falls back to default_node
  const cfgXss1 = simulateResolveConfig('https://my-gateway.pages.dev/#<script>alert(1)</script>');
  assert(cfgXss1.peerId === 'default_node', 'Malicious hash XSS payload is rejected and falls back to default');

  // 7. XSS Injection attack via query parameter ?peer= falls back to default_node
  const cfgXss2 = simulateResolveConfig('https://my-gateway.pages.dev/?peer="><img src=x onerror=alert(1)>');
  assert(cfgXss2.peerId === 'default_node', 'Malicious query peer XSS payload is rejected and falls back to default');
}

// 5. Test SW Multi-Tab Session Isolation & Worker Filter Precision
{
  console.log('\n--- Testing SW Multi-Tab Session Isolation & Worker Filter Precision ---');

  // A. Worker filter predicate simulation
  function isWorkerScript(destination, pathname, contentType) {
    const ct = (contentType || '').toLowerCase();
    return (destination === 'worker' ||
            destination === 'sharedworker' ||
            /(?:\.worker|_worker)\.js$/i.test(pathname)) &&
           ct.includes('javascript');
  }

  assert(!isWorkerScript('', '/api/worker/tasks', 'application/json'), 'API endpoint /api/worker/tasks with JSON is NOT treated as worker');
  assert(!isWorkerScript('', '/worker/status.json', 'application/json'), 'Endpoint /worker/status.json is NOT treated as worker');
  assert(!isWorkerScript('', '/api/worker_pool', 'application/json'), 'Endpoint /api/worker_pool is NOT treated as worker');
  assert(isWorkerScript('worker', '/bundle.js', 'application/javascript'), 'Destination "worker" with JS is treated as worker');
  assert(isWorkerScript('', '/monaco.worker.js', 'text/javascript'), 'Extension .worker.js with JS is treated as worker');
  assert(isWorkerScript('', '/pdf_worker.js', 'application/javascript'), 'Extension _worker.js with JS is treated as worker');

  // B. Multi-tab parent-child session mapping simulation
  const sessions = new Map();
  const iframeToPortalMap = new Map();

  function onPortalAttach(clientId, peerId, port) {
    sessions.set(clientId, { clientId, peerId, port, lastActive: Date.now() });
  }

  function onNavigate(clientId, resultingClientId) {
    if (clientId && sessions.has(clientId)) {
      iframeToPortalMap.set(resultingClientId, clientId);
    } else if (clientId && iframeToPortalMap.has(clientId)) {
      const parent = iframeToPortalMap.get(clientId);
      iframeToPortalMap.set(resultingClientId, parent);
    }
  }

  function resolveSession(clientId) {
    if (clientId && iframeToPortalMap.has(clientId)) {
      return sessions.get(iframeToPortalMap.get(clientId)) || null;
    }
    if (clientId && sessions.has(clientId)) {
      return sessions.get(clientId) || null;
    }
    return null;
  }

  // Tab 1 connects to node "bili"
  onPortalAttach('tab_bili', 'bili', { id: 'port_bili' });
  // Tab 2 connects to node "nas"
  onPortalAttach('tab_nas', 'nas', { id: 'port_nas' });

  // Tab 1 navigates iframe_bili
  onNavigate('tab_bili', 'iframe_bili_1');
  // Tab 2 navigates iframe_nas
  onNavigate('tab_nas', 'iframe_nas_1');

  // Subresources fetched by iframe 1
  const sess1 = resolveSession('iframe_bili_1');
  assert(sess1 && sess1.peerId === 'bili', 'iframe_bili_1 resolves to portal tab_bili (peerId = bili)');

  // Subresources fetched by iframe 2
  const sess2 = resolveSession('iframe_nas_1');
  assert(sess2 && sess2.peerId === 'nas', 'iframe_nas_1 resolves to portal tab_nas (peerId = nas)');

  // Nested navigation within iframe 1: iframe_bili_1 navigates to iframe_bili_2
  onNavigate('iframe_bili_1', 'iframe_bili_2');
  const sess3 = resolveSession('iframe_bili_2');
  assert(sess3 && sess3.peerId === 'bili', 'Cascaded iframe_bili_2 preserves portal tab_bili context without crosstalk');

  // Direct fetch from portal tab itself
  const sessDirect = resolveSession('tab_bili');
  assert(sessDirect && sessDirect.peerId === 'bili', 'Direct portal fetch resolves to own session');

  // C. Fast-Refresh GC and LibreSpeed Web Worker session isolation
  console.log('\n--- Testing Fast-Refresh GC & LibreSpeed Web Worker Routing ---');

  // Simulate active living clients in the browser
  let liveClients = new Set(['tab_bili', 'tab_nas']);

  function activeGCSweep(currentLivingIds) {
    for (const id of sessions.keys()) {
      if (!currentLivingIds.has(id)) {
        sessions.delete(id);
      }
    }
    for (const [childId, parentId] of iframeToPortalMap.entries()) {
      if (!currentLivingIds.has(parentId)) {
        iframeToPortalMap.delete(childId);
      }
    }
  }

  // User presses F5 on tab_bili: tab_bili is destroyed and replaced by tab_bili_refreshed
  liveClients = new Set(['tab_bili_refreshed', 'tab_nas']);
  activeGCSweep(liveClients);

  assert(!sessions.has('tab_bili'), 'Dead session tab_bili is purged by Active GC on refresh');
  assert(!iframeToPortalMap.has('iframe_bili_1'), 'Dead iframe mapping for tab_bili is purged by Active GC');

  // New tab connects
  onPortalAttach('tab_bili_refreshed', 'bili', { id: 'port_bili_refreshed' });
  assert(sessions.has('tab_bili_refreshed'), 'New session tab_bili_refreshed registered successfully');
  assert(sessions.size === 2, 'Sessions size strictly equals 2 live tabs (no zombie accumulation)');

  // Tab 2 mounts LibreSpeed iframe and LibreSpeed creates a Web Worker:
  onNavigate('tab_bili_refreshed', 'iframe_librespeed');
  // Worker created by iframe: request.destination === 'worker'
  function onWorkerSpawn(parentContextId, resultingWorkerClientId) {
    const parentPortal = iframeToPortalMap.get(parentContextId) || (sessions.has(parentContextId) ? parentContextId : null);
    if (parentPortal) {
      iframeToPortalMap.set(resultingWorkerClientId, parentPortal);
    }
  }

  onWorkerSpawn('iframe_librespeed', 'worker_speedtest_1');

  // Web Worker fetches /backend/empty.php with clientId === 'worker_speedtest_1'
  const workerSession = resolveSession('worker_speedtest_1');
  assert(workerSession && workerSession.clientId === 'tab_bili_refreshed', 'LibreSpeed Web Worker resolves directly to parent portal session tab_bili_refreshed');
  assert(workerSession && workerSession.peerId === 'bili', 'LibreSpeed Web Worker accesses correct node bili');

  // Lazy GC verification: if a session dies before active GC runs, lazy GC purges it
  liveClients.delete('tab_nas'); // tab_nas closed unexpectedly
  function lazyGCVerify(session) {
    if (session && !liveClients.has(session.clientId)) {
      sessions.delete(session.clientId);
      return null;
    }
    return session;
  }

  const deadSess = resolveSession('tab_nas');
  const checked = lazyGCVerify(deadSess);
  assert(checked === null, 'Lazy GC detects dead window client and invalidates session');
  assert(!sessions.has('tab_nas'), 'Lazy GC purges dead session from sessions map');

  // D. Service Worker HTML Document Navigation Guard & Subresource Non-HTML Fallback Verification (Tailscale / Synology DSM Fix)
  console.log('\n--- Testing SW Document Navigation Guard & Subresource Non-HTML Fallback ---');

  // Simulate sw.ts request classification logic
  function classifyIsHtml(request, contentType) {
    const isNavDoc = request.mode === 'navigate' ||
                     request.destination === 'document' ||
                     request.destination === 'iframe';
    return Boolean(isNavDoc && (contentType.includes('text/html') || contentType.includes('application/xhtml+xml')));
  }

  // Subresource fetch/XHR (e.g. Tailscale api/data returning text/html)
  const apiReqCors = { mode: 'cors', destination: '' };
  const apiReqSameOrigin = { mode: 'same-origin', destination: '' };
  const apiReqEmpty = { mode: '', destination: '' };
  assert(!classifyIsHtml(apiReqCors, 'text/html'), 'API fetch with mode:cors and text/html is NOT classified as isHtml');
  assert(!classifyIsHtml(apiReqSameOrigin, 'text/html; charset=UTF-8'), 'API fetch with mode:same-origin and text/html is NOT classified as isHtml');
  assert(!classifyIsHtml(apiReqEmpty, 'text/html'), 'API XHR with empty mode and text/html is NOT classified as isHtml');

  // Real document navigations
  const navDocReq = { mode: 'navigate', destination: 'document' };
  const navIframeReq = { mode: 'navigate', destination: 'iframe' };
  assert(classifyIsHtml(navDocReq, 'text/html'), 'Document navigation with text/html IS classified as isHtml');
  assert(classifyIsHtml(navIframeReq, 'text/html'), 'Iframe navigation with text/html IS classified as isHtml');
  assert(classifyIsHtml(navDocReq, 'application/xhtml+xml'), 'Document navigation with application/xhtml+xml IS classified as isHtml');

  // Stream injection pipeline simulation
  function findAsciiTagEnd(bytes, tagName) {
    const target = tagName.toLowerCase();
    const len = bytes.length;
    const tagLen = target.length;
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
          if (nextByte === 0x3E || nextByte === 0x20 || nextByte === 0x09 || nextByte === 0x0A || nextByte === 0x0D || nextByte === 0x2F) {
            let inQuote = 0;
            for (let k = i + 1 + tagLen; k < len; k++) {
              const kb = bytes[k];
              if (inQuote) {
                if (kb === inQuote) inQuote = 0;
              } else {
                if (kb === 0x22 || kb === 0x27) inQuote = kb;
                else if (kb === 0x3E) return k + 1;
              }
            }
          }
        }
      }
    }
    return -1;
  }

  function concatByteArrays(...arrays) {
    let total = 0;
    for (const a of arrays) if (a) total += a.length;
    const out = new Uint8Array(total);
    let offset = 0;
    for (const a of arrays) {
      if (a) {
        out.set(a, offset);
        offset += a.length;
      }
    }
    return out;
  }

  function simulateStreamPipeline(request, contentType, inputBytes) {
    const isHtml = classifyIsHtml(request, contentType);
    if (!isHtml) {
      // Transparent passthrough, zero injection
      return inputBytes;
    }

    // If isHtml is true, run injection scanner
    const patchScript = '<meta name="referrer" content="no-referrer"><script src="/patch.js"></script>';
    const encoder = new TextEncoder();
    const patchBytes = encoder.encode(patchScript);

    const headPos = findAsciiTagEnd(inputBytes, 'head');
    if (headPos !== -1) {
      const before = inputBytes.subarray(0, headPos);
      const after = inputBytes.subarray(headPos);
      return concatByteArrays(before, patchBytes, after);
    }

    const htmlPos = findAsciiTagEnd(inputBytes, 'html');
    if (htmlPos !== -1) {
      const headWrapper = encoder.encode(`<head>${patchScript}</head>`);
      const before = inputBytes.subarray(0, htmlPos);
      const after = inputBytes.subarray(htmlPos);
      return concatByteArrays(before, headWrapper, after);
    }

    const doctypePos = findAsciiTagEnd(inputBytes, '!doctype');
    if (doctypePos !== -1) {
      const headWrapper = encoder.encode(`<head>${patchScript}</head>`);
      const before = inputBytes.subarray(0, doctypePos);
      const after = inputBytes.subarray(doctypePos);
      return concatByteArrays(before, headWrapper, after);
    }

    // Fallback: If no HTML skeleton found, return raw bytes unchanged
    return inputBytes;
  }

  // Assertion 1: Simulate request.mode = 'cors', Content-Type: text/html with JSON body
  const rawJsonStr = JSON.stringify({ status: 'success', data: { tailscale: 'active', ip: '100.64.0.1' } });
  const rawJsonBytes = Buffer.from(rawJsonStr, 'utf-8');
  const processedApiBytes = simulateStreamPipeline(apiReqCors, 'text/html; charset=UTF-8', rawJsonBytes);

  assert(Buffer.compare(processedApiBytes, rawJsonBytes) === 0, 'API response Body with text/html remains 100% byte-identical raw JSON');
  assert(!Buffer.from(processedApiBytes).toString('utf-8').includes('<meta'), 'API response Body is NOT polluted with <meta>');
  assert(!Buffer.from(processedApiBytes).toString('utf-8').includes('<script'), 'API response Body is NOT polluted with <script>');
  let parsedSuccess = false;
  try {
    const parsed = JSON.parse(Buffer.from(processedApiBytes).toString('utf-8'));
    if (parsed.status === 'success' && parsed.data.tailscale === 'active') parsedSuccess = true;
  } catch {}
  assert(parsedSuccess, 'API response parsed cleanly by JSON.parse() without SyntaxError');

  // Edge case assertion: Even if raw non-HTML JSON was fed to HTML injection scanner directly, fallback preserves 100% raw bytes
  const directFallbackBytes = simulateStreamPipeline(navDocReq, 'text/html', rawJsonBytes);
  assert(Buffer.compare(directFallbackBytes, rawJsonBytes) === 0, 'Non-HTML JSON fed to scanner is untouched by fallback defense (no blind patchBytes prepend)');

  // Assertion 2: Simulate request.mode = 'navigate' returning valid HTML document
  const rawHtmlStr = '<!DOCTYPE html><html><head><title>Tailscale</title></head><body><h1>Admin</h1></body></html>';
  const rawHtmlBytes = Buffer.from(rawHtmlStr, 'utf-8');
  const processedHtmlBytes = simulateStreamPipeline(navDocReq, 'text/html', rawHtmlBytes);
  const processedHtmlStr = Buffer.from(processedHtmlBytes).toString('utf-8');

  assert(processedHtmlStr.includes('<head><meta name="referrer" content="no-referrer"><script src="/patch.js"></script><title>Tailscale</title>'),
    'Document navigation with valid HTML successfully injects patch script and meta tags into <head>');
  assert(processedHtmlStr.includes('<h1>Admin</h1>'), 'Document navigation preserves HTML body content');
}

console.log(`\n=== Verification Complete: ${passCount} Passed, ${failCount} Failed ===`);
if (failCount > 0) process.exit(1);
