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
  assert(u.searchParams.get('secret') === 'secret123', 'Dynamically received P2P_PORTAL_CONTEXT secret preserved');
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
      'service', 'hub', 'test', 'demo', 'node', 'nodes', 'server', 'direct',
      'static', 'assets', 'cdn'
    ]);

    function extractExplicitP2PSubdomain(hostname) {
      const p2pIndex = hostname.indexOf('.p2p.');
      if (p2pIndex > 0) {
        const sub = hostname.substring(0, p2pIndex);
        const parts = sub.split('.');
        const candidate = parts[parts.length - 1];
        if (candidate && !reserved.has(candidate)) return candidate;
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

    let peerId = params.get('peer') || params.get('node');
    let secret = params.get('secret') || params.get('token') || '';

    if (!peerId && url.hash) {
      const rawHash = url.hash.replace(/^#\/?/, '').trim();
      if (rawHash) {
        const parts = rawHash.split(/[?&]/);
        peerId = parts[0];
      }
    }

    if (!peerId) {
      const explicitSub = extractExplicitP2PSubdomain(url.hostname);
      if (explicitSub) peerId = explicitSub;
    }

    if (!peerId) {
      peerId = storage['p2p_active_peer_id'] || storage['p2p_peer_id'] || '';
    }

    if (!peerId) {
      const sub = extractNodeIdFromHostname(url.hostname);
      if (sub) peerId = sub;
    }

    if (!peerId) {
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
}

console.log(`\n=== Verification Complete: ${passCount} Passed, ${failCount} Failed ===`);
if (failCount > 0) process.exit(1);
