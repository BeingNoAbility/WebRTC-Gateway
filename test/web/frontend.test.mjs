import test from 'node:test';
import assert from 'node:assert/strict';
import { loadTsModule } from './_loader.mjs';

const [
  htmlMod,
  urlMod,
  sandboxMod,
  fetchMod,
  sessionRegMod,
  decompMod,
  streamBridgeMod,
  cookieSyncMod,
  patchMod,
  bridgeHtmlMod,
  cryptoMod,
  identityMod,
  sdpMod,
  configMod,
  authClientMod,
  portalBridgeMod,
  muxMod,
  frameMod,
  tlvMod,
] = await Promise.all([
  loadTsModule('web/src/sw/html_injector.ts'),
  loadTsModule('web/src/shared/url_rewrite.ts'),
  loadTsModule('web/src/shared/sandbox_origin.ts'),
  loadTsModule('web/src/sw/fetch_router.ts'),
  loadTsModule('web/src/sw/session_registry.ts'),
  loadTsModule('web/src/sw/decompress_pipeline.ts'),
  loadTsModule('web/src/sw/stream_bridge.ts'),
  loadTsModule('web/src/sw/cookie_sync.ts'),
  loadTsModule('web/src/patch/ws_patch.ts'),
  loadTsModule('web/src/bridge/bridge.ts'),
  loadTsModule('web/src/shared/crypto.ts'),
  loadTsModule('web/src/shared/identity.ts'),
  loadTsModule('web/src/shared/sdp.ts'),
  loadTsModule('web/src/portal/core/config.ts'),
  loadTsModule('web/src/portal/core/auth_client.ts'),
  loadTsModule('web/src/portal/core/bridge.ts'),
  loadTsModule('web/src/protocol/mux.ts'),
  loadTsModule('web/src/protocol/frame.ts'),
  loadTsModule('web/src/protocol/tlv.ts'),
]);

function runHtmlInjector(chunks, cfgOverrides = {}, opts = { hasHttpCharset: true }) {
  const inj = new htmlMod.StreamingHtmlInjector('https://sbx-1.sandbox.example', {
    ...opts,
    scriptNonce: cfgOverrides.scriptNonce ?? opts.scriptNonce,
  });
  const parts = [];
  for (const c of chunks) {
    const bytes = typeof c === 'string' ? new TextEncoder().encode(c) : c;
    parts.push(...inj.push(bytes));
  }
  parts.push(...inj.finish());
  const total = parts.reduce((acc, p) => acc + p.byteLength, 0);
  const merged = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    merged.set(p, off);
    off += p.byteLength;
  }
  return merged;
}

test('html_injector V8: external boot.js + patch.js (zero inline data-p2p-boot), nonce propagation, HTML5 comments, <template>/<svg>, UTF-16 skip', async () => {
  const html =
    '<!doctype html>' +
    '<!--><!---><!-- fake <head> --!>' +
    '<html data-raw=a"b>c>' +
    '<template><head>fake</head><script>ignored()</script></template>' +
    '<svg><head>fake</head><title/><script>svgScript()</script></svg>' +
    '<noscript><head>fake</head></noscript>' +
    '<style>/* <head> */</style>' +
    '<head data-expr="a>b" data-sq=\'c>d\'><base href="/sub/"><meta charset="utf-8" /></head>' +
    '<body>ok</body></html>';

  const chunks = [];
  for (let i = 0; i < html.length; i += 3) {
    chunks.push(html.slice(i, i + 3));
  }
  const out = new TextDecoder().decode(runHtmlInjector(chunks));
  const realHeadTag = '<head data-expr="a>b" data-sq=\'c>d\'>';
  const realHeadEnd = out.indexOf(realHeadTag) + realHeadTag.length;
  const expectedSnippet =
    '<script src="https://sbx-1.sandbox.example/__p2p_gw__/boot.js"></script>' +
    '<script src="https://sbx-1.sandbox.example/__p2p_gw__/assets/patch.js"></script>';
  const snippetStart = out.indexOf(expectedSnippet);
  assert.equal(snippetStart, realHeadEnd, 'snippet must inject immediately after real <head>');
  assert.ok(!out.includes('data-p2p-boot'), 'H-16: must NOT embed 22 KiB inline data-p2p-boot in HTML');

  // H-16: CSP scriptNonce propagation onto both injected external scripts!
  const noncedOut = new TextDecoder().decode(
    runHtmlInjector(['<!doctype html><head></head><body>x</body>'], {}, { hasHttpCharset: true, scriptNonce: 'abc123XYZ' })
  );
  assert.ok(
    noncedOut.includes('<script src="https://sbx-1.sandbox.example/__p2p_gw__/boot.js" nonce="abc123XYZ"></script>'),
    'must propagate upstream CSP nonce to boot.js'
  );
  assert.ok(
    noncedOut.includes('<script src="https://sbx-1.sandbox.example/__p2p_gw__/assets/patch.js" nonce="abc123XYZ"></script>'),
    'must propagate upstream CSP nonce to patch.js'
  );

  // buildBootScriptPayload escapes </script> and bounds expired cookies
  const bootJs = htmlMod.buildBootScriptPayload({
    nodeId: 'demo',
    portalOrigin: 'https://portal.example',
    backendOrigin: 'http://app.local',
    initCookies: [
      { op: 'set', name: 'fresh', value: '</script><x>', path: '/' },
      { op: 'set', name: 'old', value: 'expired', path: '/', expires: 1000 },
    ],
  });
  assert.ok(bootJs.startsWith('globalThis.__P2P_GW_BOOT__='));
  assert.ok(!bootJs.includes('</script>'), 'boot.js must escape < as \\u003c');
  assert.ok(bootJs.includes('fresh') && !bootJs.includes('expired'));

  // Executable <script> before <head> MUST inject strictly BEFORE that <script> so patch runs first!
  const scriptFirst = '<!doctype html><html><script>window.x=1;</script><head><title>t</title></head></html>';
  const scriptFirstOut = new TextDecoder().decode(runHtmlInjector([scriptFirst]));
  assert.equal(
    scriptFirstOut.indexOf('<script src="https://sbx-1.sandbox.example/__p2p_gw__/boot.js"'),
    scriptFirst.indexOf('<script>window.x=1;'),
    'snippet must inject before first executable <script>'
  );

  // Headless HTML with DOCTYPE MUST inject AFTER <!doctype html> (zero Quirks Mode!).
  const headless = new TextDecoder().decode(runHtmlInjector(['<!doctype html><div>hello</div>']));
  assert.ok(headless.startsWith('<!doctype html><script src='), `must keep DOCTYPE at byte 0: ${headless}`);

  // UTF-16 LE BOM must pass through untouched without UTF-8 corruption.
  const utf16 = new Uint8Array([0xff, 0xfe, 0x3c, 0x00, 0x68, 0x00]);
  const utf16Out = runHtmlInjector([utf16]);
  assert.deepEqual(Array.from(utf16Out), Array.from(utf16));

  // Split UTF-8 BOM stays at byte 0.
  const bomOut = runHtmlInjector([new Uint8Array([0xef]), new Uint8Array([0xbb]), new Uint8Array([0xbf]), '<div>hi</div>']);
  assert.deepEqual(Array.from(bomOut.slice(0, 3)), [0xef, 0xbb, 0xbf]);
  assert.ok(new TextDecoder().decode(bomOut.slice(3)).startsWith('<script src='));

  // Worker bootstrap preserves 'use strict' and guards importScripts for ES Module Workers.
  const workerOut = htmlMod.injectWorkerBootstrap(
    `// banner\n'use strict';\nimport { x } from './m.js';`,
    'https://sbx-1.sandbox.example',
    { backendOrigin: 'http://app.local' }
  );
  assert.ok(workerOut.indexOf('__P2P_GW_WORKER_CFG__') > workerOut.indexOf("'use strict';"));
  assert.ok(workerOut.includes("typeof importScripts==='function'"));

  // Runtime behavior verification of ESM Worker inline globalThis.fetch rewrite (no importScripts).
  const esmCode = htmlMod.injectWorkerBootstrap(
    `await fetch('http://app.local/api/esm?k=1'); await fetch('https://other.example/keep');`,
    'https://sbx-1.sandbox.example',
    { backendOrigin: 'http://app.local' }
  );
  const seenUrls = [];
  const fakeGlobal = {
    fetch: async (u) => {
      seenUrls.push(typeof u === 'string' ? u : u.url);
      return { ok: true };
    },
  };
  const runEsm = new Function(
    'globalThis',
    'location',
    'Request',
    'importScripts',
    `return (async () => {\nconst fetch = (...a) => globalThis.fetch(...a);\n${esmCode}\n})();`
  );
  await runEsm(fakeGlobal, { href: 'https://sbx-1.sandbox.example/worker.js' }, Request, undefined);
  assert.equal(fakeGlobal.__P2P_GW_ESM_PATCH__, true);
  assert.deepEqual(seenUrls, ['/api/esm?k=1', 'https://other.example/keep']);
});

test('url_rewrite V7: exact single-target origin only, blocks prefix/port/external spoofing, preserves pinned pk= in popups', () => {
  const ctx = {
    currentOrigin: 'https://sbx-abc.sandbox.example',
    portalOrigin: 'https://portal.example',
    nodeId: 'nas',
    targetOrigin: 'http://app.local:8080',
    pinnedPk: 'PINNED_HASH_22_CHARS__',
    baseHref: 'https://sbx-abc.sandbox.example/docs/index.html',
  };

  assert.equal(urlMod.rewriteBackendUrl('http://app.local.evil.com/steal', ctx), 'http://app.local.evil.com/steal');
  assert.equal(urlMod.rewriteBackendUrl('http://app.local:9999/admin', ctx), 'http://app.local:9999/admin');
  assert.equal(urlMod.rewriteBackendUrl('http://app.local:8080/api/v1?a=1#h', ctx), '/api/v1?a=1#h');
  // External domains are never rewritten (strict 1 Agent = 1 Target Origin).
  assert.equal(urlMod.rewriteBackendUrl('https://img.cdn.local/pic.png', ctx), 'https://img.cdn.local/pic.png');
  assert.equal(urlMod.rewriteBackendUrl('https://api.internal:9000/v1', ctx), 'https://api.internal:9000/v1');
  // WebSocket target rewriting accepts only sandbox or exact target origin.
  assert.equal(urlMod.rewriteWebSocketTarget('ws://app.local:8080/ws?x=1', ctx), '/ws?x=1');
  assert.equal(urlMod.rewriteWebSocketTarget('wss://sbx-abc.sandbox.example/live', ctx), '/live');
  assert.equal(urlMod.rewriteWebSocketTarget('wss://app.local:8080/ws', ctx), null);
  assert.equal(urlMod.rewriteWebSocketTarget('wss://evil.example/ws', ctx), null);
  assert.equal(
    urlMod.resolvePortalPopupUrl('detail.html?id=7', ctx),
    'https://portal.example/#nas?pk=PINNED_HASH_22_CHARS__&path=%2Fdocs%2Fdetail.html%3Fid%3D7'
  );
  // V7: Portal URLs must NEVER be double-wrapped!
  assert.equal(urlMod.resolvePortalPopupUrl('https://portal.example/#other', ctx), null);
  // V7: Cross-origin <base href> emits absolute sandbox origin URL so relative path does not drift!
  assert.equal(
    urlMod.rewriteBackendUrl('http://app.local:8080/api/v2', {
      ...ctx,
      baseHref: 'https://foreign.cdn.example/sub/',
    }),
    'https://sbx-abc.sandbox.example/api/v2'
  );
  // Relative <base href="/app/"> stays same-origin -> relative rewrite!
  assert.equal(
    urlMod.rewriteBackendUrl('http://app.local:8080/api/v3', {
      ...ctx,
      baseHref: '/app/',
    }),
    '/api/v3'
  );
  // Opaque "null" origin (e.g., sandboxed iframe) MUST NOT throw and keeps absolute URL untouched!
  assert.equal(
    urlMod.rewriteBackendUrl('http://app.local:8080/api/v4', {
      ...ctx,
      currentOrigin: 'null',
      baseHref: '',
    }),
    'http://app.local:8080/api/v4'
  );
  assert.equal(
    urlMod.resolvePortalPopupUrl('detail.html', { ...ctx, currentOrigin: 'null', baseHref: '' }),
    null
  );
});

test('sandbox_origin V7: 3-tier derivation (Tier 1 fixed, Tier 3 HMAC device salt + reset, 128-bit capability)', async () => {
  // Tier 1 Fixed Dual-Domain returns exact sandboxHost!
  const fixedOrigin = await sandboxMod.computeSandboxOrigin(
    {
      portalDomain: 'portal.example',
      sandboxMode: 'fixed',
      sandboxDomain: 'app.sandbox.example',
      sandboxHost: 'app.sandbox.example',
      sameSiteMode: false,
      localDevMode: false,
    },
    'https:',
    'nas',
    'http://127.0.0.1:8080',
    false
  );
  assert.equal(fixedOrigin, 'https://app.sandbox.example');

  // Tier 3 Wildcard Owner derives 96-bit (24-hex) HMAC label from device salt, rotatable per node!
  const crossSite1 = await sandboxMod.computeSandboxOrigin(
    { portalDomain: 'portal.example', sandboxMode: 'wildcard', sandboxDomain: 'sbx.example.net', sameSiteMode: false, localDevMode: false },
    'https:',
    'nas',
    'http://127.0.0.1:8080',
    false
  );
  const crossSite2 = await sandboxMod.computeSandboxOrigin(
    { portalDomain: 'portal.example', sandboxMode: 'wildcard', sandboxDomain: 'sbx.example.net', sameSiteMode: false, localDevMode: false },
    'https:',
    'nas',
    'http://127.0.0.1:8080',
    false
  );
  assert.match(crossSite1, /^https:\/\/sbx-[0-9a-f]{24}\.sbx\.example\.net$/);
  assert.equal(crossSite1, crossSite2, 'same node+target+salt must stay stable');

  sandboxMod.resetNodeSandboxSalt('nas');
  const crossSiteRotated = await sandboxMod.computeSandboxOrigin(
    { portalDomain: 'portal.example', sandboxMode: 'wildcard', sandboxDomain: 'sbx.example.net', sameSiteMode: false, localDevMode: false },
    'https:',
    'nas',
    'http://127.0.0.1:8080',
    false
  );
  assert.match(crossSiteRotated, /^https:\/\/sbx-[0-9a-f]{24}\.sbx\.example\.net$/);
  assert.notEqual(crossSiteRotated, crossSite1, 'resetNodeSandboxSalt must rotate sandbox origin');

  // Capability visitors get 128-bit (32-hex) ephemeral random label!
  const cap1 = await sandboxMod.computeSandboxOrigin(
    { portalDomain: 'portal.example', sandboxMode: 'wildcard', sandboxDomain: 'sbx.example.net', sameSiteMode: false, localDevMode: false },
    'https:',
    'nas',
    'http://127.0.0.1:8080',
    true
  );
  const cap2 = await sandboxMod.computeSandboxOrigin(
    { portalDomain: 'portal.example', sandboxMode: 'wildcard', sandboxDomain: 'sbx.example.net', sameSiteMode: false, localDevMode: false },
    'https:',
    'nas',
    'http://127.0.0.1:8080',
    true
  );
  assert.match(cap1, /^https:\/\/sbx-[0-9a-f]{32}\.sbx\.example\.net$/);
  assert.notEqual(cap1, cap2);
});

test('fetch_router H-16/M-25: CSP directive rewriter preserves upstream CSP, extracts nonce, strips transformed ETag & checks multi Content-Encoding', () => {
  // Two CSP headers — first has `sandbox` (preserved verbatim), second is rewritten (preserving directives).
  const { headers, preserveFileSandboxCSP } = fetchMod.buildSafeResponseHeaders(
    [
      { name: 'content-security-policy', value: "SANDBOX; default-src 'none'" },
      { name: 'content-security-policy', value: "default-src 'self'; object-src 'none'; report-to sandbox" },
      { name: 'content-disposition', value: 'attachment; filename="年度财务报告 2026.pdf"' },
      { name: 'x-emoji-note', value: 'Hello 🚀 世界!' },
      { name: 'service-worker-allowed', value: '/' },
      { name: 'x-p2p-cookie-delta', value: 'eyJ0ZXN0IjoxfQ' },
      { name: 'etag', value: '"strong-123"' },
    ],
    { stripContentLength: true, stripContentEncoding: false, backendOrigin: 'http://app.local' }
  );

  assert.equal(preserveFileSandboxCSP, true);
  assert.ok(headers.get('content-security-policy')?.includes("SANDBOX; default-src 'none'"));
  assert.ok(headers.get('content-security-policy')?.includes("object-src 'none'"), 'H-16: upstream CSP directives must be preserved');
  assert.equal(headers.get('etag'), null, 'M-25: strong ETag must be stripped when response body is transformed');
  assert.equal(headers.get('service-worker-allowed'), null, 'service-worker-allowed must be stripped');
  assert.equal(headers.get('x-p2p-cookie-delta'), null, 'internal cookie delta header must be stripped');
  assert.ok(headers.get('x-emoji-note')?.includes('%F0%9F%9A%80'));

  // H-16: rewriteUpstreamCsp extracts nonce, maps backendOrigin -> 'self', strips frame-ancestors!
  const rewritten = fetchMod.rewriteUpstreamCsp(
    "default-src 'none'; script-src 'nonce-secret99' 'strict-dynamic'; connect-src http://app.local ws://app.local; frame-ancestors https://old.example",
    { swOrigin: 'https://sbx-1.sandbox.example', backendOrigin: 'http://app.local' }
  );
  assert.equal(rewritten.scriptNonce, 'secret99');
  assert.ok(!rewritten.policy.includes('frame-ancestors'), 'upstream frame-ancestors must be replaced');
  assert.ok(rewritten.policy.includes("connect-src 'self'"), 'backendOrigin must rewrite to self');
  assert.ok(rewritten.policy.includes("'nonce-secret99'"), 'nonce must be preserved');

  // M-25: Multi-header Content-Encoding detection!
  assert.equal(fetchMod.hasUndecodedContentEncoding(['gzip'], true), false);
  assert.equal(fetchMod.hasUndecodedContentEncoding(['gzip', 'br'], true), true);
  assert.equal(fetchMod.hasUndecodedContentEncoding(['br, gzip'], true), true);
  assert.equal(fetchMod.hasUndecodedContentEncoding(['identity'], false), false);
  assert.equal(fetchMod.hasUndecodedContentEncoding(['br'], false), true);

  // Negative test: `report-to sandbox` must not be mistaken for a `sandbox` directive.
  assert.equal(fetchMod.hasSandboxCspDirective("default-src 'self'; report-to sandbox"), false);
  assert.equal(fetchMod.hasSandboxCspDirective("img-src https://sandbox.example"), false);
  assert.equal(fetchMod.hasSandboxCspDirective("default-src 'none'; sandbox allow-downloads"), true);

  // Tier 2 sameSiteMode emits Origin-Agent-Cluster: ?1 and strips any upstream origin-agent-cluster: ?0!
  const oacRes = fetchMod.buildSafeResponseHeaders(
    [
      { name: 'content-type', value: 'text/html' },
      { name: 'origin-agent-cluster', value: '?0' },
    ],
    { stripContentLength: false, originAgentCluster: true }
  );
  assert.equal(oacRes.headers.get('origin-agent-cluster'), '?1');

  const noOacRes = fetchMod.buildSafeResponseHeaders(
    [{ name: 'origin-agent-cluster', value: '?0' }],
    { stripContentLength: false, originAgentCluster: false }
  );
  assert.equal(noOacRes.headers.get('origin-agent-cluster'), null, 'upstream origin-agent-cluster must be stripped');
});

test('CookieMirror & parseDocumentCookieAssignment: JS set + delete (Max-Age=0), UTF-8 safe, zero document.cookie', () => {
  const mirror = new cookieSyncMod.CookieMirror();
  mirror.applyDeltas([{ op: 'set', name: 'theme', value: 'dark', path: '/' }]);
  assert.equal(mirror.serializeForPath('/app'), 'theme=dark');

  const setMut = patchMod.parseDocumentCookieAssignment('csrf=中文🚀tok; Path=/; Max-Age=3600', '/app/page');
  assert.equal(setMut.op, 'set');
  assert.equal(setMut.value, '中文🚀tok');

  mirror.applyClientMutations([setMut], { clientId: 'c1', seq: 1 });
  assert.ok(mirror.serializeForPath('/app').includes('csrf=中文🚀tok'));
  const pending1 = mirror.drainPendingMutations();
  assert.equal(pending1.length, 1);
  assert.equal(pending1[0].op, 'set');

  // Delete mutation (`Max-Age=0`) removes cookie cleanly (no zombie cookie!).
  const delMut = patchMod.parseDocumentCookieAssignment('theme=; Max-Age=0; Path=/', '/');
  assert.equal(delMut.op, 'del');
  mirror.applyClientMutations([delMut], { clientId: 'c1', seq: 2 });
  assert.ok(!mirror.serializeForPath('/').includes('theme='));
  const pending2 = mirror.drainPendingMutations();
  assert.equal(pending2.length, 1);
  assert.equal(pending2[0].op, 'del');

  // Out-of-order lower seq is ignored.
  mirror.applyClientMutations([{ op: 'set', name: 'theme', value: 'stale', path: '/' }], { clientId: 'c1', seq: 1 });
  assert.ok(!mirror.serializeForPath('/').includes('theme=stale'));

  // V7 RFC 6265 strict validation in parseDocumentCookieAssignment:
  assert.equal(patchMod.parseDocumentCookieAssignment('sid=1; HttpOnly', '/'), null, 'JS must not set HttpOnly');
  assert.equal(
    patchMod.parseDocumentCookieAssignment('a=1; Domain=evil.example', { pathname: '/', allowedHosts: ['app.local'] }),
    null,
    'mismatched Domain must be rejected'
  );
  assert.equal(
    patchMod.parseDocumentCookieAssignment('__Host-x=1; Secure; Path=/app', { pathname: '/', isSecureContext: true }),
    null,
    '__Host- with non-root Path must be rejected'
  );
  assert.equal(
    patchMod.parseDocumentCookieAssignment('__Secure-x=1; Secure', { pathname: '/', isSecureContext: false }),
    null,
    '__Secure- on HTTP must be rejected'
  );
  const badMaxAge = patchMod.parseDocumentCookieAssignment('k=v; Max-Age=10abc', '/');
  assert.equal(badMaxAge?.expires, undefined, 'non-integer Max-Age must be ignored per RFC 6265');

  // Non-destructive peekPendingMutations / commitPendingMutations preserves mutations on failed send!
  mirror.applyClientMutations([{ op: 'set', name: 'k1', value: 'v1', path: '/' }], { clientId: 'c1', seq: 3 });
  const peek1 = mirror.peekPendingMutations();
  assert.equal(peek1.mutations.length, 1);
  // Before commit, another peek still sees k1!
  assert.equal(mirror.peekPendingMutations().mutations.length, 1);
  // New mutation arrives while first request is in-flight; committing old watermark preserves newer mutation!
  mirror.applyClientMutations([{ op: 'set', name: 'k2', value: 'v2', path: '/' }], { clientId: 'c1', seq: 4 });
  mirror.commitPendingMutations(peek1.watermark);
  const peek2 = mirror.peekPendingMutations();
  assert.deepEqual(peek2.mutations.map((m) => m.name), ['k2']);
});

test('bridge.html strict Portal origin allowlist (Fail-Closed)', () => {
  const empty = new Set();
  assert.equal(bridgeHtmlMod.isAllowedPortalOrigin('https://portal.example', empty), false);

  const allow = new Set(['https://portal.example']);
  assert.equal(bridgeHtmlMod.isAllowedPortalOrigin('https://portal.example', allow), true);
  assert.equal(bridgeHtmlMod.isAllowedPortalOrigin('https://evil.portal.example', allow), false);
  assert.equal(bridgeHtmlMod.isAllowedPortalOrigin('https://sbx-1.sandbox.example', allow), false);
});

test('decompress_pipeline V7: native gzip bomb aborts, rejects zstd, safe transferable slice', async () => {
  assert.throws(
    () => decompMod.pipeThroughBoundedDecompression(new ReadableStream(), 'zstd'),
    /Unsupported tunnel compression/i
  );

  if (typeof CompressionStream === 'function' && typeof DecompressionStream === 'function') {
    const zeros = new Uint8Array(64 * 1024);
    const compressedStream = new Blob([zeros]).stream().pipeThrough(new CompressionStream('gzip'));
    const guarded = decompMod.pipeThroughBoundedDecompression(compressedStream, 'gzip', {
      maxRatio: 4,
      minOutputForRatio: 8 * 1024,
      maxOutputBytes: 1024 * 1024,
    });
    await assert.rejects(() => new Response(guarded).arrayBuffer(), /bomb|exceeded/i);

    // V7 Negative test: 0-byte empty EOF stream MUST NOT crash DecompressionStream('gzip')!
    const emptyStream = new ReadableStream({
      start(c) {
        c.close();
      },
    });
    const emptyOut = await new Response(decompMod.pipeThroughBoundedDecompression(emptyStream, 'gzip')).arrayBuffer();
    assert.equal(emptyOut.byteLength, 0, '0-byte gzip stream must resolve cleanly to 0 bytes');

    // Negative test: corrupt gzip cancels upstream srcReader (no leaked stream/slot!).
    let srcCancelled = false;
    let pulled = 0;
    const corruptSrc = new ReadableStream({
      pull(ctrl) {
        if (pulled++ === 0) {
          ctrl.enqueue(new Uint8Array([0x1f, 0x8b, 0xff, 0xff, 0x00, 0x01, 0x02, 0x03]));
        } else {
          ctrl.enqueue(new Uint8Array(1024));
        }
      },
      cancel() {
        srcCancelled = true;
      },
    });
    await assert.rejects(() => new Response(decompMod.pipeThroughBoundedDecompression(corruptSrc, 'gzip')).arrayBuffer());
    assert.equal(srcCancelled, true, 'corrupt gzip error must cancel upstream srcReader');

    // Negative test: gzip stream with empty 0-byte input chunks between header and payload must not hang!
    const realGz = new Uint8Array(
      await new Response(new Blob(['hello-zero-chunk']).stream().pipeThrough(new CompressionStream('gzip'))).arrayBuffer()
    );
    const interleavedSrc = new ReadableStream({
      start(ctrl) {
        ctrl.enqueue(realGz.subarray(0, 4));
        ctrl.enqueue(new Uint8Array(0));
        ctrl.enqueue(realGz.subarray(4, 10));
        ctrl.enqueue(new Uint8Array(0));
        ctrl.enqueue(realGz.subarray(10));
        ctrl.close();
      },
    });
    const textOut = await new Response(decompMod.pipeThroughBoundedDecompression(interleavedSrc, 'gzip')).text();
    assert.equal(textOut, 'hello-zero-chunk');

    // H-17 Negative test: cancelling BEFORE first pull() MUST still cancel upstream source!
    let prePullCancelled = false;
    const lazySrc = new ReadableStream({
      pull(c) {
        c.enqueue(realGz);
        c.close();
      },
      cancel() {
        prePullCancelled = true;
      },
    });
    const decomp = decompMod.pipeThroughBoundedDecompression(lazySrc, 'gzip');
    await decomp.cancel('abort-early');
    assert.equal(prePullCancelled, true, 'H-17: cancel() before first pull() must cancel upstream source');
  }

  // C-3a: Firefox request.body === undefined fallback via arrayBuffer()!
  const fakeFirefoxReq = {
    method: 'POST',
    body: undefined,
    async arrayBuffer() {
      return new TextEncoder().encode('firefox-post-body').buffer;
    },
  };
  const extracted = await streamBridgeMod.createTransferableRequestBody(fakeFirefoxReq);
  assert.equal(extracted.hasBody, true);
  assert.equal(await new Response(extracted.bodyStream).text(), 'firefox-post-body');

  const emptyFirefoxReq = {
    method: 'POST',
    body: undefined,
    async arrayBuffer() {
      return new ArrayBuffer(0);
    },
  };
  const emptyExtracted = await streamBridgeMod.createTransferableRequestBody(emptyFirefoxReq);
  assert.equal(emptyExtracted.hasBody, false);

  // C-3b: Canonical URL comparison avoids rebuilding same-origin Request objects!
  assert.equal(
    patchMod.isSameCanonicalRequestUrl('/api/v1?x=1', 'https://sbx-1.sandbox.example/api/v1?x=1', 'https://sbx-1.sandbox.example/page'),
    true
  );
  assert.equal(
    patchMod.isSameCanonicalRequestUrl('/api/v2', 'https://sbx-1.sandbox.example/api/v1', 'https://sbx-1.sandbox.example/page'),
    false
  );
});

test('sdp & identity V7: structured SessionDescriptor, Pure Ed25519 verify, TOFU vs ?pk=evil block', async () => {
  identityMod.clearMemoryTofuStoreForTests();
  const kp = await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify']);
  const rawPub = new Uint8Array(await crypto.subtle.exportKey('raw', kp.publicKey));
  const pkB64 = cryptoMod.base64UrlEncode(rawPub);

  const offerDesc = {
    fp: 'a'.repeat(64),
    ice_ufrag: 'ufrag1234',
    ice_pwd: 'password1234567890123456',
    setup: 'actpass',
    mid: '0',
    sctp_port: 5000,
    max_msg_size: 262144,
    sdp_ver: 2,
  };
  const answerDesc = {
    fp: 'b'.repeat(64),
    ice_ufrag: 'ufrag5678',
    ice_pwd: 'password6543210987654321',
    setup: 'active',
    mid: '0',
    sctp_port: 5000,
    max_msg_size: 65536,
    sdp_ver: 2,
  };

  // Synthesize -> Extract round-trip MUST be lossless!
  const synth = sdpMod.synthesizeSDP(offerDesc, true);
  assert.deepEqual(sdpMod.extractSessionDescriptor(synth, 2), offerDesc);

  // Negative test: multi-fingerprint SDP must be rejected.
  assert.throws(
    () =>
      sdpMod.extractSessionDescriptor(
        synth.replace('a=mid:0', `a=fingerprint:sha-256 ${sdpMod.formatColonFingerprint('c'.repeat(64))}\r\na=mid:0`),
        2
      ),
    /Conflicting/i
  );

  // Pure Ed25519 length-prefixed transcript signature verify
  const nonce = cryptoMod.randomBase64Url(32);
  const transcript = sdpMod.encodeAnswerTranscript('mynode', nonce, offerDesc, answerDesc);
  const sig = new Uint8Array(await crypto.subtle.sign({ name: 'Ed25519' }, kp.privateKey, transcript));
  const sigB64 = cryptoMod.base64UrlEncode(sig);

  assert.equal(await sdpMod.verifyAnswerTranscript(pkB64, 'mynode', nonce, offerDesc, answerDesc, sigB64), true);
  assert.equal(await sdpMod.verifyAnswerTranscript(pkB64, 'mynode', 'wrong-nonce', offerDesc, answerDesc, sigB64), false);

  // Deferred TOFU commit + `?pk=<evil>` cannot override an already-trusted node in TOFU!
  const pkHash = await identityMod.pkHashFromRawPublicKey(rawPub);
  const pre = await identityMod.verifyPinnedOrTofuKey('https://portal.example', 'tofunode', rawPub, '', { commitNew: false });
  assert.equal(pre.status, 'tofu_new');
  assert.equal(await identityMod.getTofuKey('https://portal.example::tofunode'), null, 'pre-check must not commit TOFU');

  await identityMod.commitTofuKey('https://portal.example', 'tofunode', pkHash);
  const kp2 = await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify']);
  const rawPub2 = new Uint8Array(await crypto.subtle.exportKey('raw', kp2.publicKey));
  const evilHash = await identityMod.pkHashFromRawPublicKey(rawPub2);

  const overrideAttempt = await identityMod.verifyPinnedOrTofuKey('https://portal.example', 'tofunode', rawPub2, evilHash);
  assert.equal(overrideAttempt.ok, false, '?pk=<evil> MUST NOT bypass existing TOFU record');
  assert.equal(overrideAttempt.status, 'tofu_mismatch');

  // Negative test: commitTofuKey MUST throw on conflicting existing TOFU key (TOCTOU protection)!
  await assert.rejects(
    () => identityMod.commitTofuKey('https://portal.example', 'tofunode', evilHash),
    /conflict|mismatch|already/i
  );
  // Idempotent commit of identical hash succeeds!
  await identityMod.commitTofuKey('https://portal.example', 'tofunode', pkHash);

  // H-20: verifyPeerDtlsCertificate unit tests (match, mismatch, Fail-Closed when supported, degrade on Safari).
  const certBytes = new Uint8Array([0x30, 0x82, 0x01, 0x0a, 1, 2, 3, 4]);
  const certFp = await cryptoMod.sha256Hex(certBytes);
  assert.equal(
    await sdpMod.verifyPeerDtlsCertificate(
      { sctp: { transport: { getRemoteCertificates: () => [certBytes.buffer] } } },
      certFp
    ),
    true
  );
  assert.equal(
    await sdpMod.verifyPeerDtlsCertificate(
      { sctp: { transport: { getRemoteCertificates: () => [certBytes.buffer] } } },
      '0'.repeat(64)
    ),
    false
  );
  const origRtcDtls = globalThis.RTCDtlsTransport;
  try {
    globalThis.RTCDtlsTransport = class {
      getRemoteCertificates() { return []; }
    };
    assert.equal(
      await sdpMod.verifyPeerDtlsCertificate({ sctp: null }, certFp),
      false,
      'must fail closed when RTCDtlsTransport.getRemoteCertificates is supported but transport is missing'
    );
    delete globalThis.RTCDtlsTransport;
    assert.equal(
      await sdpMod.verifyPeerDtlsCertificate({ sctp: null }, certFp),
      true,
      'degrades only on engines lacking RTCDtlsTransport.getRemoteCertificates (Safari)'
    );
  } finally {
    if (origRtcDtls !== undefined) globalThis.RTCDtlsTransport = origRtcDtls;
    else delete globalThis.RTCDtlsTransport;
  }
});

test('config V7: relayOnly omits STUN, scrubSensitiveHashParams removes cap=, %63ap redacted in logs', () => {
  const servers = configMod.buildIceServers({
    customStun: '',
    turnUrl: 'turn:turn.example.com:3478?transport=udp',
    turnUser: 'u',
    turnPass: 'p',
    relayOnly: true,
  });
  assert.equal(servers.length, 1);
  assert.equal(servers[0].urls, 'turn:turn.example.com:3478?transport=udp');

  assert.equal(
    configMod.scrubSensitiveHashParams('#nas?pk=abc&cap=SECRET123&path=/home'),
    '#nas?pk=abc&path=/home'
  );
  const redacted = configMod.sanitizeHashForLog('#nas?%63ap=SUPER_SECRET&pk=pub1');
  assert.ok(!redacted.includes('SUPER_SECRET'));
  assert.ok(redacted.includes('[REDACTED]'));
});

test('AuthClient V7: rejects SPAKE2+ -> public downgrade, missing/wrong confirm_v, duplicate frames & never commits TOFU', async () => {
  identityMod.clearMemoryTofuStoreForTests();
  const spakeMod = await loadTsModule('web/src/shared/spake2p.ts');
  const te = new TextEncoder();

  const makeMockCtrlMux = () => {
    const sent = [];
    return {
      sent,
      setMaxMessageSize() {},
      sendControlFrame(type, payload) {
        sent.push({ type, payload });
      },
    };
  };

  const offerDesc = {
    fp: 'a'.repeat(64),
    ice_ufrag: 'ufrag1',
    ice_pwd: 'password1234567890123456',
    setup: 'actpass',
    mid: '0',
    sctp_port: 5000,
    max_msg_size: 262144,
    sdp_ver: 2,
  };
  const answerDesc = { ...offerDesc, fp: 'b'.repeat(64), setup: 'active' };
  const agentPkB64 = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA';
  const fakePkHash = 'FAKE_TOFU_HASH_22CHARS';

  async function runDowngradeScenario(resultPayload) {
    identityMod.clearMemoryTofuStoreForTests();
    const fatalErrors = [];
    const authed = [];
    const auth = new authClientMod.AuthClient(
      'nas-downgrade',
      'https://portal.example',
      false,
      '',
      '',
      {
        onStatus() {},
        onLog() {},
        onAuthRequired() {},
        onAuthFailed() {},
        onFatalSecurityError(reason) {
          fatalErrors.push(reason);
        },
        onAuthenticated(info) {
          authed.push(info);
        },
      }
    );
    await auth.deriveOwnerCredentials('owner-pass', agentPkB64);
    const mockMux = makeMockCtrlMux();
    const gen = auth.beginChannel({
      mux: mockMux,
      offerDesc,
      answerDesc,
      visitorNonce: 'vnonce123',
      agentPkB64,
      verifiedPkHash: fakePkHash,
      verifiedKeyStatus: 'tofu_new',
    });

    // Valid P-256 point for share_v
    const shareV = spakeMod.spake2pComputeShareVForTest(12345n, 67890n);
    const chal = {
      agent_nonce: 'anonce123',
      auth_methods: ['spake2p'],
      share_v: cryptoMod.base64UrlEncode(shareV),
    };
    await auth.handleControlFrame(gen, {
      gen: 0,
      type: frameMod.FrameType.AUTH_CHALLENGE,
      flags: 0,
      payload: te.encode(JSON.stringify(chal)),
    });
    assert.equal(auth.getState(), 'WAIT_RESULT');
    assert.equal(mockMux.sent.length, 1);

    await auth.handleControlFrame(gen, {
      gen: 0,
      type: frameMod.FrameType.AUTH_RESULT,
      flags: 0,
      payload: te.encode(JSON.stringify(resultPayload)),
    });
    assert.equal(authed.length, 0, 'must NOT authenticate on downgrade/bad confirm_v');
    assert.ok(fatalErrors.length >= 1, 'must fire fatal security error');
    assert.equal(
      await identityMod.getTofuKey('https://portal.example::nas-downgrade'),
      null,
      'must NEVER commit TOFU on failed mutual auth'
    );
  }

  // Case 1: Malicious MITM Agent sends role: "public" with empty confirm_v after client sent SPAKE2+!
  await runDowngradeScenario({
    ok: true,
    role: 'public',
    meta: { nodeId: 'nas-downgrade', backendOrigin: 'http://evil', isStaticMode: false },
  });

  // Case 2: Agent sends role: "owner" with missing confirm_v!
  await runDowngradeScenario({
    ok: true,
    role: 'owner',
    meta: { nodeId: 'nas-downgrade', backendOrigin: 'http://evil', isStaticMode: false },
  });

  // Case 3: Agent sends role: "owner" with forged wrong confirm_v!
  await runDowngradeScenario({
    ok: true,
    role: 'owner',
    confirm_v: cryptoMod.base64UrlEncode(new Uint8Array(32).fill(0x99)),
    meta: { nodeId: 'nas-downgrade', backendOrigin: 'http://evil', isStaticMode: false },
  });
});

test('TunnelBridge V8: BRIDGE_PING, Transferable ReadableStream HTTP, immediate REQ_ACK, header strip, TLV cookies & early 204 cleanup', async () => {
  function makePair() {
    const mk = () => ({
      readyState: 'open',
      bufferedAmount: 0,
      bufferedAmountLowThreshold: 0,
      binaryType: 'arraybuffer',
      peer: null,
      onmessage: null,
      onopen: null,
      onclose: null,
      onerror: null,
      onbufferedamountlow: null,
      send(data) {
        if (this.readyState !== 'open') throw new Error('closed');
        const cp = new Uint8Array(data).slice();
        const p = this.peer;
        if (p && p.readyState === 'open') {
          queueMicrotask(() => p.onmessage?.({ data: cp.buffer }));
        }
      },
      close() {
        this.readyState = 'closed';
        this.onclose?.({});
      },
    });
    const a = mk();
    const b = mk();
    a.peer = b;
    b.peer = a;
    return [a, b];
  }

  const [cCtrl, sCtrl] = makePair();
  const [cLane, sLane] = makePair();

  const receivedReqs = [];
  sLane.onmessage = (ev) => {
    const f = frameMod.decodeFrame(new Uint8Array(ev.data));
    if (f.type === frameMod.FrameType.HEADERS) {
      const req = tlvMod.decodeRequestHeaders(f.payload);
      receivedReqs.push(req);
      if (req.target === '/beacon') {
        const resp = tlvMod.encodeResponseHeaders({ status: 204, headers: [] });
        sLane.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.FIN, resp));
        return;
      }
      const resp = tlvMod.encodeResponseHeaders({
        status: 200,
        respFlags: tlvMod.RespFlag.NONE,
        headers: [{ name: 'content-type', value: 'text/plain' }],
        cookieDeltas: [{ op: tlvMod.CookieOp.SET, name: 'theme', value: 'dark', path: '/', expiresMs: 0 }],
      });
      sLane.send(frameMod.encodeFrame(f.streamId, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, resp));
      sLane.send(
        frameMod.encodeFrame(f.streamId, frameMod.FrameType.DATA, frameMod.FrameFlag.FIN, new TextEncoder().encode('hello-v8'))
      );
    }
  };

  const clientMux = new muxMod.PoolMux(1, {});
  clientMux.attachControlDC(cCtrl);
  clientMux.attachLaneDC(0, cLane);

  const bridge = new portalBridgeMod.TunnelBridge();
  bridge.setContext(
    clientMux,
    'https://sbx-1.sandbox.example',
    {
      nodeId: 'demo',
      backendOrigin: 'http://app.local',
      isStaticMode: false,
      sameSiteMode: false,
    },
    null
  );

  // 1. BRIDGE_PING -> BRIDGE_PONG
  const bCh = new MessageChannel();
  bridge.attachBridgePort(bCh.port1);
  const pongP = new Promise((r) => {
    bCh.port2.onmessage = (ev) => r(ev.data);
  });
  bCh.port2.postMessage({ type: 'BRIDGE_PING', pingId: 'p1' });
  assert.deepEqual(await pongP, { type: 'BRIDGE_PONG', pingId: 'p1' });
  bCh.port2.close();

  // 2. HTTP fetch request via Transferable ReadableStream: strips spoofed headers, carries TLV cookieMutations & receives cookieDeltas!
  const reqCh = new MessageChannel();
  const events = [];
  let respBodyText = '';
  const doneP = new Promise((r) => {
    reqCh.port1.onmessage = async (ev) => {
      events.push(ev.data);
      if (ev.data.type === 'RESP_START' && ev.data.bodyStream) {
        respBodyText = await new Response(ev.data.bodyStream).text();
        r();
      } else if (ev.data.type === 'RESP_ERROR') {
        r();
      }
    };
  });
  await bridge.handleFetchRequest(
    {
      type: 'TUNNEL_FETCH_REQUEST',
      reqId: 'req-1',
      tabId: 'tab-1',
      method: 'GET',
      target: '/hello',
      fetchCtx: tlvMod.FetchCtxFlag.SAME_ORIGIN_CLIENT,
      headers: [
        { name: 'x-forwarded-for', value: '1.2.3.4' },
        { name: 'cookie', value: 'spoof=1' },
        { name: 'x-client-cookie', value: 'legacy=1' },
        { name: 'accept', value: 'text/plain' },
      ],
      cookieMutations: [{ op: 'del', name: 'sid', path: '/' }],
      hasBody: false,
    },
    reqCh.port2
  );
  await doneP;
  reqCh.port1.close();

  assert.equal(events[0].type, 'REQ_ACK');
  assert.equal(events[1].type, 'RESP_START');
  assert.equal(events[1].status, 200);
  assert.equal(respBodyText, 'hello-v8');
  assert.deepEqual(events[1].cookieDeltas, [{ op: 'set', name: 'theme', value: 'dark', path: '/' }]);
  assert.deepEqual(receivedReqs[0].headers, [{ name: 'accept', value: 'text/plain' }]);
  assert.equal(receivedReqs[0].fetchCtx, tlvMod.FetchCtxFlag.SAME_ORIGIN_CLIENT);
  assert.deepEqual(receivedReqs[0].cookieMutations, [
    { op: tlvMod.CookieOp.DEL, name: 'sid', value: '', path: '/', expiresMs: 0 },
  ]);

  // 3. 40 sequential POSTs returning early 204 No Content MUST NOT deadlock or leak active streams!
  for (let i = 0; i < 40; i++) {
    const ch = new MessageChannel();
    const neverEndingUpload = new ReadableStream({ start() {} });
    const got204 = new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`204 POST #${i} deadlocked!`)), 2000);
      ch.port1.onmessage = (ev) => {
        if (ev.data.type === 'RESP_START') {
          clearTimeout(t);
          resolve(ev.data.status);
        }
      };
    });
    await bridge.handleFetchRequest(
      {
        type: 'TUNNEL_FETCH_REQUEST',
        reqId: `post-204-${i}`,
        tabId: 'tab-1',
        method: 'POST',
        target: '/beacon',
        headers: [],
        hasBody: true,
        bodyStream: neverEndingUpload,
      },
      ch.port2
    );
    assert.equal(await got204, 204);
    ch.port1.close();
  }
  assert.equal(clientMux.activeStreamCount, 0, 'all V8 streams must be cleanly released after early 204');

  bridge.destroy();
  clientMux.close();
  sCtrl.close();
  sLane.close();
});

test('SW Anti-CSRF Gate (P0-2), SessionRegistry Fail-Closed / pruneDeadSessions, TOP_LEVEL_NAV, CREDENTIALS_OMIT & Brotli', async () => {
  const swOrigin = 'https://sbx-a.sandbox.example';
  const portalOrigin = 'https://portal.example';

  // 0. Empty allowlist MUST fail closed, and CSP-injection strings MUST be rejected!
  const emptyReg = new sessionRegMod.SessionRegistry(new Set());
  assert.equal(emptyReg.isAllowedPortalOrigin(portalOrigin), false, 'empty allowlist must fail closed');
  const reg = new sessionRegMod.SessionRegistry(new Set([portalOrigin]));
  assert.equal(reg.isAllowedPortalOrigin(`${portalOrigin}; script-src 'unsafe-inline'`), false);

  const openPorts = [];
  const makeCh = () => {
    const ch = new MessageChannel();
    openPorts.push(ch.port1, ch.port2);
    return ch;
  };

  try {
    // 1. Forged portalOrigin outside server-injected allowlist MUST be rejected!
    const badCh = makeCh();
    assert.equal(
      reg.attachPort(
        'tab-evil',
        'https://evil.example',
        { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
        badCh.port1
      ),
      false
    );

    // Attach legitimate tab-1 (proxy `-target` mode: isStaticMode = false)
    const ch1 = makeCh();
    assert.equal(
      reg.attachPort(
        'tab-1',
        portalOrigin,
        {
          nodeId: 'demo',
          backendOrigin: 'http://app.local',
          isStaticMode: false,
          sameSiteMode: false,
          initialCookies: [{ op: 'set', name: 'theme', value: 'dark', path: '/' }],
        },
        ch1.port1
      ),
      true
    );

    // PORTAL_PING -> PORTAL_PONG liveness handshake on bridgePort!
    const pongP = new Promise((resolve) => {
      ch1.port2.onmessage = (ev) => {
        if (ev.data?.type === 'PORTAL_PONG') resolve(ev.data.pingId);
      };
    });
    ch1.port2.postMessage({ type: 'PORTAL_PING', pingId: 'alive-1' });
    assert.equal(await pongP, 'alive-1');

    // 2. handleEnterNavigation: requires registered one-time enterNonce bound to nextPath + GET/navigate/iframe!
    const noNonce = reg.handleEnterNavigation(new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&next=/app`), 'c1', '');
    assert.equal(noNonce.status, 403, 'enter without nonce must return 403');

    // M-24: tampered ?next= path or non-GET / non-iframe enter MUST fail with 403!
    reg.registerEnterNonce('nonce-bound-path-1234', 'tab-1', '/dashboard');
    const tamperedNext = reg.handleEnterNavigation(
      new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&nonce=nonce-bound-path-1234&next=/admin`),
      'c-tamper',
      '',
      { method: 'GET', mode: 'navigate', destination: 'iframe' }
    );
    assert.equal(tamperedNext.status, 403, 'M-24: enterNonce must reject tampered ?next= path');

    reg.registerEnterNonce('nonce-doc-dest-1234', 'tab-1', '/');
    const docEnter = reg.handleEnterNavigation(
      new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&nonce=nonce-doc-dest-1234&next=/`),
      'c-doc-enter',
      '',
      { method: 'GET', mode: 'navigate', destination: 'document' }
    );
    assert.equal(docEnter.status, 403, 'M-24: /enter must reject destination=document');

    reg.registerEnterNonce('nonce-tab-redirect-1234', 'tab-1', '/');
    const tabAttack = reg.handleEnterNavigation(
      new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&nonce=nonce-tab-redirect-1234&next=%2F%09%2Fevil.example`),
      'c-tab',
      ''
    );
    assert.equal(tabAttack.status, 302);
    assert.equal(tabAttack.headers.get('location'), '/', '\\t open-redirect must collapse to /');

    // Replaying consumed nonce MUST fail with 403!
    const replay = reg.handleEnterNavigation(
      new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&nonce=nonce-tab-redirect-1234&next=/app`),
      'c-replay',
      ''
    );
    assert.equal(replay.status, 403, 'replayed enterNonce must be rejected');

    // Chinese path & query must succeed cleanly with ASCII percent-encoded Location header!
    const cnNext = '/目录/文档?q=中文#节';
    reg.registerEnterNonce('nonce-cn-path-567890', 'tab-1', cnNext);
    const cnResp = reg.handleEnterNavigation(
      new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&nonce=nonce-cn-path-567890&next=${encodeURIComponent(cnNext)}`),
      'c-app',
      '',
      { method: 'GET', mode: 'navigate', destination: 'iframe' }
    );
    assert.equal(cnResp.status, 302);
    assert.equal(cnResp.headers.get('location'), '/%E7%9B%AE%E5%BD%95/%E6%96%87%E6%A1%A3?q=%E4%B8%AD%E6%96%87#%E8%8A%82');

    // 3. C-1 Negative Test: destination === 'document' MUST be rejected EVEN WHEN clientId IS BOUND!
    const boundDocEscape = fetchMod.authorizeFetchRequest(reg, {
      clientId: 'c-app',
      resultingClientId: 'r-popup-escape',
      mode: 'navigate',
      destination: 'document',
      method: 'GET',
      referrer: `${swOrigin}/`,
      swOrigin,
    });
    assert.equal(boundDocEscape.allowed, false, 'C-1: destination=document MUST be blocked even for a bound clientId');

    // W3C Fetch Anti-CSRF Gate:
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: '',
        mode: 'no-cors',
        destination: 'image',
        method: 'GET',
        referrer: 'https://evil.example/',
        swOrigin,
      }).allowed,
      false
    );
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: 'r-post',
        mode: 'navigate',
        destination: 'iframe',
        method: 'POST',
        referrer: '',
        swOrigin,
      }).allowed,
      false
    );
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: 'r-ext',
        mode: 'navigate',
        destination: 'iframe',
        method: 'GET',
        referrer: 'https://evil.example/frame',
        swOrigin,
      }).allowed,
      false
    );

    // M-24 Negative test: proxy `-target` mode rejects unbound iframe even with portalOrigin referrer!
    const portalRefProxy = fetchMod.authorizeFetchRequest(reg, {
      clientId: '',
      resultingClientId: 'r-portalref',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: `${portalOrigin}/`,
      swOrigin,
    });
    assert.equal(portalRefProxy.allowed, false, 'M-24: proxy mode must NOT authorize unbound iframe via referrer fallback');

    // M-24 Positive test: W3C `replacesClientId` authorizes Back/Forward/Reload strictly via bound client!
    const reloadAuth = fetchMod.authorizeFetchRequest(reg, {
      clientId: '',
      resultingClientId: 'c-app-reloaded',
      replacesClientId: 'c-app',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: '',
      swOrigin,
    });
    assert.equal(reloadAuth.allowed, true, 'replacesClientId must preserve bound session across Back/Forward/Reload');
    assert.equal(reloadAuth.sameOriginClient, true);
    assert.equal(reg.getSessionByTabId('tab-1').rootClientId, 'c-app-reloaded', 'rootClientId must transfer on reload');

    // Static `-dir`/`-file` (cookie-less, isStaticMode=true) allows opaque-origin iframe navigation
    // AND binds resultingClientId so subresources work!
    const staticReg = new sessionRegMod.SessionRegistry(new Set([portalOrigin]));
    const chStatic = makeCh();
    staticReg.attachPort(
      'tab-dir',
      portalOrigin,
      { nodeId: 'docs', backendOrigin: 'http://static.local', isStaticMode: true, sameSiteMode: false },
      chStatic.port1
    );
    const emptyRefStatic = fetchMod.authorizeFetchRequest(staticReg, {
      clientId: '',
      resultingClientId: 'r-emptyref-dir',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: '',
      swOrigin,
    });
    assert.equal(emptyRefStatic.allowed, true, 'static -dir mode must allow opaque-origin iframe navigation');
    assert.equal(emptyRefStatic.sameOriginClient, false);
    assert.equal(staticReg.isBoundClient('r-emptyref-dir'), true, 'static -dir mode must bind resultingClientId for subresources');

    // Bound appFrame client spawning Worker -> inherits session, sameOriginClient === true, binds resultingClientId!
    const workerAuth = fetchMod.authorizeFetchRequest(reg, {
      clientId: 'c-app-reloaded',
      resultingClientId: 'c-worker-1',
      mode: 'same-origin',
      destination: 'worker',
      method: 'GET',
      referrer: `${swOrigin}/app`,
      swOrigin,
    });
    assert.equal(workerAuth.allowed, true);
    assert.equal(workerAuth.sameOriginClient, true);
    assert.equal(reg.isBoundClient('c-worker-1'), true, 'Worker resultingClientId must be bound to parent tab session');

    // C-2 Negative test: Tab-2 flooding 200 child clients MUST NEVER evict Tab-1's root appFrame!
    const ch2 = makeCh();
    reg.attachPort(
      'tab-2',
      portalOrigin,
      { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
      ch2.port1
    );
    reg.bindClient('tab2-root', 'tab-2', true);
    for (let i = 0; i < 200; i++) {
      reg.bindClient(`tab2-spam-${i}`, 'tab-2', false);
    }
    assert.equal(reg.isBoundClient('c-app-reloaded'), true, 'C-2: Tab-2 flood MUST NOT evict Tab-1 root appFrame');
    assert.equal(reg.isBoundClient('tab2-root'), true, 'C-2: Tab-2 flood MUST NOT evict its own root appFrame');
    assert.ok(reg.getSessionByTabId('tab-2').clientIds.size <= sessionRegMod.MAX_CLIENTS_PER_TAB);

    // 4. pruneDeadSessions deletes closed/reloaded tab whose bridgePort does not answer BRIDGE_PING!
    ch2.port2.onmessage = (ev) => {
      if (ev.data?.type === 'BRIDGE_PING') {
        ch2.port2.postMessage({ type: 'BRIDGE_PONG', pingId: ev.data.pingId });
      }
    };
    ch1.port2.onmessage = null;
    const pruneScope = {
      location: { origin: swOrigin },
      clients: { get: async () => undefined },
    };
    await reg.pruneDeadSessions(pruneScope, '', 100);
    assert.equal(reg.getSessionByTabId('tab-1'), null, 'dead tab-1 session must be removed on pruneDeadSessions');
    assert.equal(reg.getActiveSessionCount(), 1, 'only live tab-2 must remain');
    assert.equal(reg.getSingleActiveSession()?.tabId, 'tab-2', 'getSingleActiveSession must recover after reload');

    // 5. isLiveNestedAppClient: accepts nested appFrame & worker, rejects bridge.html / top-level / empty!
    const mockClients = new Map([
      ['nested-app', { id: 'nested-app', type: 'window', frameType: 'nested', url: `${swOrigin}/dashboard` }],
      ['app-worker', { id: 'app-worker', type: 'worker', url: `${swOrigin}/sw-worker.js` }],
      ['top-doc', { id: 'top-doc', type: 'window', frameType: 'top-level', url: `${swOrigin}/dashboard` }],
      ['bridge-win', { id: 'bridge-win', type: 'window', frameType: 'nested', url: `${swOrigin}/__p2p_gw__/bridge.html?tab=tab-cold` }],
      ['cross-origin', { id: 'cross-origin', type: 'window', frameType: 'nested', url: 'https://evil.example/x' }],
    ]);
    const mockSwScope = {
      location: { origin: swOrigin },
      clients: { get: async (id) => mockClients.get(id) },
    };
    assert.equal(await reg.isLiveNestedAppClient(mockSwScope, ''), null);
    assert.equal(await reg.isLiveNestedAppClient(mockSwScope, 'bridge-win'), null);
    assert.equal(await reg.isLiveNestedAppClient(mockSwScope, 'top-doc'), null);
    assert.equal(await reg.isLiveNestedAppClient(mockSwScope, 'cross-origin'), null);
    assert.ok(await reg.isLiveNestedAppClient(mockSwScope, 'nested-app'));
    assert.ok(await reg.isLiveNestedAppClient(mockSwScope, 'app-worker'));

    // 6. P0-2 / P1-23 / P3-2 collectRequestHeaders: removed legacy TOP_LEVEL_NAV bit (0x02) is NEVER set,
    //    and credentials==='omit' sets CREDENTIALS_OMIT and suppresses mutations!
    const iframeNavReq = {
      mode: 'navigate',
      destination: 'iframe',
      credentials: 'include',
      referrer: `${swOrigin}/`,
      headers: new Headers(),
    };
    const iframeCtx = fetchMod.collectRequestHeaders(iframeNavReq, swOrigin, [], true);
    assert.equal(
      (iframeCtx.fetchCtx & 0x02) !== 0,
      false,
      'removed bit 0x02 must NEVER be set'
    );
    const docNavReq = {
      mode: 'navigate',
      destination: 'document',
      credentials: 'include',
      referrer: `${swOrigin}/`,
      headers: new Headers(),
    };
    const docCtx = fetchMod.collectRequestHeaders(docNavReq, swOrigin, [], true);
    assert.equal((docCtx.fetchCtx & 0x02) !== 0, false, 'removed bit 0x02 must NEVER be set even on document');

    const omitReq = {
      mode: 'cors',
      destination: '',
      credentials: 'omit',
      referrer: `${swOrigin}/`,
      headers: new Headers(),
    };
    const omitCtx = fetchMod.collectRequestHeaders(
      omitReq,
      swOrigin,
      [{ op: 'set', name: 'k', value: 'v', path: '/' }],
      true
    );
    assert.equal((omitCtx.fetchCtx & tlvMod.FetchCtxFlag.CREDENTIALS_OMIT) !== 0, true);
    assert.equal(omitCtx.combinedMutations.length, 0, 'credentials:omit must not send cookie mutations');

    // 7. buildSafeResponseHeaders preserves Brotli Content-Encoding & upstream CSP alongside frame-ancestors!
    const brRes = fetchMod.buildSafeResponseHeaders(
      [
        { name: 'content-type', value: 'text/html; charset=utf-8' },
        { name: 'content-encoding', value: 'br' },
        { name: 'content-security-policy', value: "default-src 'self'" },
      ],
      { stripContentLength: false, stripContentEncoding: false, frameAncestorsOrigin: portalOrigin }
    );
    assert.equal(brRes.headers.get('content-encoding'), 'br', 'non-gzip Content-Encoding must not be stripped');
    assert.ok(brRes.headers.get('content-security-policy')?.includes("default-src 'self'"), 'upstream CSP preserved');
    assert.ok(brRes.headers.get('content-security-policy')?.includes(`frame-ancestors 'self' ${portalOrigin}`));
  } finally {
    for (const p of openPorts) {
      try {
        p.onmessage = null;
        p.close();
      } catch {
        // ignore
      }
    }
  }
});

test('stream_bridge V8: pre-aborted Request.signal rejects with AbortError & onAck fires before RESP_START (H-18a)', async () => {
  const reg = new sessionRegMod.SessionRegistry(new Set(['https://portal.example']));
  const ch = new MessageChannel();
  try {
    reg.attachPort(
      'tab-abort',
      'https://portal.example',
      { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
      ch.port1
    );
    const sess = reg.getSessionByTabId('tab-abort');
    const ac = new AbortController();
    ac.abort();
    const req = new Request('https://sbx-a.sandbox.example/api', { method: 'GET', signal: ac.signal });
    await assert.rejects(
      () => streamBridgeMod.executeTunnelRequest({}, reg, sess, req, '/api', [], 0, []),
      (err) => err && (err.name === 'AbortError' || /abort/i.test(err.message))
    );

    // H-18a: verify onAck callback triggers as soon as REQ_ACK arrives (even while RESP_START is delayed!).
    let ackedBeforeResp = false;
    ch.port2.onmessage = (ev) => {
      if (ev.data?.type === 'TUNNEL_FETCH_REQUEST') {
        const rp = ev.ports[0];
        rp.postMessage({ type: 'REQ_ACK', reqId: ev.data.reqId });
        setTimeout(() => {
          rp.postMessage({ type: 'RESP_START', status: 200, headers: [], hasBody: false });
        }, 40);
      }
    };
    const okReq = new Request('https://sbx-a.sandbox.example/slow', { method: 'GET' });
    const resP = streamBridgeMod.executeTunnelRequest({}, reg, sess, okReq, '/slow', [], 0, [], () => {
      ackedBeforeResp = true;
    });
    await new Promise((r) => setTimeout(r, 15));
    assert.equal(ackedBeforeResp, true, 'H-18a: onAck must fire immediately on REQ_ACK before RESP_START');
    const res = await resP;
    assert.equal(res.response.status, 200);

    // C-3a Negative test: Firefox FetchEvent.request where request.body === undefined MUST fall back to arrayBuffer()!
    let uploadedText = '';
    ch.port2.onmessage = async (ev) => {
      if (ev.data?.type === 'TUNNEL_FETCH_REQUEST') {
        const rp = ev.ports[0];
        assert.equal(ev.data.hasBody, true, 'C-3a: Firefox POST fallback must mark hasBody=true');
        assert.ok(ev.data.bodyStream, 'C-3a: Firefox POST fallback must transfer a ReadableStream');
        uploadedText = await new Response(ev.data.bodyStream).text();
        rp.postMessage({ type: 'REQ_ACK', reqId: ev.data.reqId });
        rp.postMessage({ type: 'RESP_START', status: 201, headers: [], hasBody: false });
      }
    };
    const firefoxMockReq = {
      method: 'POST',
      body: undefined,
      signal: undefined,
      async arrayBuffer() {
        return new TextEncoder().encode('firefox-post-body').buffer;
      },
    };
    const ffRes = await streamBridgeMod.executeTunnelRequest({}, reg, sess, firefoxMockReq, '/submit', [], 0, []);
    assert.equal(ffRes.response.status, 201);
    assert.equal(uploadedText, 'firefox-post-body', 'C-3a: Firefox POST body must NOT be silently dropped');
  } finally {
    ch.port1.onmessage = null;
    ch.port1.close();
    ch.port2.close();
  }
});

test('TunnelBridge V8: multiplexed WS concurrency cap (64) & outbound >16MiB WS closes with 1008', async () => {
  function makePair() {
    const mk = () => ({
      readyState: 'open',
      bufferedAmount: 0,
      bufferedAmountLowThreshold: 0,
      binaryType: 'arraybuffer',
      peer: null,
      onmessage: null,
      onopen: null,
      onclose: null,
      onerror: null,
      onbufferedamountlow: null,
      send(data) {
        if (this.readyState !== 'open') throw new Error('closed');
        const cp = new Uint8Array(data).slice();
        const p = this.peer;
        if (p && p.readyState === 'open') {
          queueMicrotask(() => p.onmessage?.({ data: cp.buffer }));
        }
      },
      close() {
        this.readyState = 'closed';
        this.onclose?.({});
      },
    });
    const a = mk();
    const b = mk();
    a.peer = b;
    b.peer = a;
    return [a, b];
  }

  const [cCtrl, sCtrl] = makePair();
  const [cLane, sLane] = makePair();
  sLane.onmessage = (ev) => {
    const f = frameMod.decodeFrame(new Uint8Array(ev.data));
    if (f.type === frameMod.FrameType.HEADERS) {
      sLane.send(
        frameMod.encodeFrame(
          f.streamId,
          frameMod.FrameType.HEADERS,
          frameMod.FrameFlag.NONE,
          tlvMod.encodeResponseHeaders({ status: 101, headers: [] })
        )
      );
    }
  };

  const clientMux = new muxMod.PoolMux(1, {});
  clientMux.attachControlDC(cCtrl);
  clientMux.attachLaneDC(0, cLane);

  const bridge = new portalBridgeMod.TunnelBridge();
  bridge.setContext(
    clientMux,
    'https://sbx-1.sandbox.example',
    { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
    null
  );

  const openChannels = [];
  try {
    // 1. Open 64 WS streams with IDENTICAL empty wsId '' -> internal wsKeySeq still tracks all 64,
    //    so the 65th MUST fail with WS_ERROR!
    for (let i = 0; i < 64; i++) {
      const ch = new MessageChannel();
      openChannels.push(ch);
      await bridge.handleWsOpen(
        { type: 'TUNNEL_WS_OPEN', wsId: '', tabId: 'tab-1', target: '/ws', protocols: [] },
        ch.port2
      );
    }
    const ch65 = new MessageChannel();
    openChannels.push(ch65);
    const rej65 = new Promise((resolve) => {
      ch65.port1.onmessage = (ev) => {
        if (ev.data?.type === 'WS_CLOSE' || ev.data?.type === 'WS_ERROR') resolve(ev.data);
      };
    });
    await bridge.handleWsOpen(
      { type: 'TUNNEL_WS_OPEN', wsId: '', tabId: 'tab-1', target: '/ws', protocols: [] },
      ch65.port2
    );
    const err65 = await rej65;
    assert.equal(err65.type, 'WS_ERROR', '65th WS with empty wsId must hit MAX_CONCURRENT_WS_STREAMS cap');
    assert.match(err65.message || err65.error || '', /Too many concurrent/i);

    // 2. Outbound WS_SEND queue cap (>16 MiB) closes the WS with 1008 Policy Violation!
    const firstPort = openChannels[0].port1;
    const capCloseP = new Promise((resolve) => {
      firstPort.onmessage = (ev) => {
        if (ev.data?.type === 'WS_CLOSE') resolve(ev.data);
      };
    });
    await new Promise((r) => setTimeout(r, 20));
    for (let i = 0; i < 18; i++) {
      const buf = new ArrayBuffer(1024 * 1024);
      firstPort.postMessage({ type: 'WS_SEND', opcode: 2, data: buf }, [buf]);
    }
    const capClose = await capCloseP;
    assert.equal(capClose.code, 1008, 'outbound WS pending queue >16 MiB must close with 1008');
  } finally {
    for (const ch of openChannels) {
      ch.port1.onmessage = null;
      ch.port1.close();
      ch.port2.close();
    }
    bridge.destroy();
    clientMux.close();
    sCtrl.close();
    sLane.close();
  }
});

test('PeerSession V7 regressions: PEER_OFFLINE teardown, timer cancellation, GOAWAY & post-auth signaling WS close', async () => {
  const peerMod = await loadTsModule('web/src/portal/core/peer_session.ts');

  const sampleOfferSdp = [
    'v=0',
    'o=- 4611731400430051336 2 IN IP4 127.0.0.1',
    's=-',
    't=0 0',
    'a=group:BUNDLE 0',
    'a=msid-semantic: WMS',
    'm=application 9 UDP/DTLS/SCTP webrtc-datachannel',
    'c=IN IP4 0.0.0.0',
    'a=ice-ufrag:ufrg1234',
    'a=ice-pwd:password123456789012345678',
    'a=ice-options:trickle',
    `a=fingerprint:sha-256 ${Array(32).fill('AA').join(':')}`,
    'a=setup:actpass',
    'a=mid:0',
    'a=sctp-port:5000',
    'a=max-message-size:262144',
    '',
  ].join('\r\n');

  const sockets = [];
  const pcs = [];

  class MockDC extends EventTarget {
    constructor(label) {
      super();
      this.label = label;
      this.readyState = 'connecting';
      this.binaryType = 'arraybuffer';
      this.bufferedAmount = 0;
      this.bufferedAmountLowThreshold = 0;
      this.sent = [];
      this.onopen = null;
      this.onclose = null;
      this.onmessage = null;
      this.onerror = null;
      this.onbufferedamountlow = null;
    }
    send(data) {
      this.sent.push(new Uint8Array(data));
    }
    close() {
      if (this.readyState === 'closed') return;
      this.readyState = 'closed';
      this.onclose?.();
      this.dispatchEvent(new Event('close'));
    }
    _open() {
      this.readyState = 'open';
      this.onopen?.();
      this.dispatchEvent(new Event('open'));
    }
    _recv(buf) {
      const ev = new MessageEvent('message', { data: buf });
      this.onmessage?.(ev);
      this.dispatchEvent(ev);
    }
  }

  class MockPC {
    constructor() {
      this.connectionState = 'new';
      this.localDescription = null;
      this.onicecandidate = null;
      this.onconnectionstatechange = null;
      this.dcs = new Map();
      this.closed = false;
      pcs.push(this);
    }
    createDataChannel(label) {
      const dc = new MockDC(label);
      this.dcs.set(label, dc);
      return dc;
    }
    async createOffer() {
      return { type: 'offer', sdp: sampleOfferSdp };
    }
    async setLocalDescription(desc) {
      this.localDescription = desc;
    }
    async setRemoteDescription() {}
    async addIceCandidate() {}
    close() {
      this.closed = true;
      this.connectionState = 'closed';
    }
  }

  class MockWS {
    static OPEN = 1;
    static CLOSED = 3;
    constructor(url) {
      this.url = url;
      this.readyState = MockWS.OPEN;
      this.sent = [];
      this.onopen = null;
      this.onmessage = null;
      this.onerror = null;
      this.onclose = null;
      sockets.push(this);
      queueMicrotask(() => this.onopen?.());
    }
    send(str) {
      this.sent.push(JSON.parse(str));
    }
    close() {
      this.readyState = MockWS.CLOSED;
    }
    _recv(obj) {
      this.onmessage?.({ data: JSON.stringify(obj) });
    }
  }

  const origWS = globalThis.WebSocket;
  const origPC = globalThis.RTCPeerConnection;
  const origLoc = globalThis.location;
  globalThis.WebSocket = MockWS;
  globalThis.RTCPeerConnection = MockPC;
  globalThis.location = { origin: 'https://portal.example.com', protocol: 'https:', host: 'portal.example.com' };

  let session = null;
  try {
    const logs = [];
    const authedEvents = [];
    session = new peerMod.PeerSession(
      'demo',
      'https://portal.example.com',
      false,
      '',
      '',
      { customStun: '', turnUrl: '', turnUser: '', turnPass: '', relayOnly: false },
      {
        onStatus: () => {},
        onLog: (l) => logs.push(l),
        onAuthRequired: () => {},
        onAuthFailed: () => {},
        onKeyMismatch: () => {},
        onAuthenticated: (info) => authedEvents.push(info),
        onDisconnected: () => {},
      }
    );

    // 1. PEER_OFFLINE on in-flight Offer MUST tear down unanswered pc so subsequent peer_status(online=true) immediately dials!
    const connectPromise = session.connect();
    await new Promise((r) => setTimeout(r, 10));
    assert.equal(sockets.length, 1);
    const ws1 = sockets[0];
    ws1._recv({ type: 'joined', node_id: 'demo', session_id: 's1', online: true, require_visitor_auth: false, agent_pk: '' });
    await connectPromise;
    await new Promise((r) => setTimeout(r, 20));
    assert.equal(pcs.length, 1, 'initial joined(online=true) must create PC #1');
    assert.ok(session.getPeerConnection() === pcs[0]);
    assert.ok(session.handshakeWatchdogTimer !== null, 'first-connect handshake watchdog timer must be armed');

    // Signaling reports PEER_OFFLINE (Offer raced with Agent shutdown) -> PC #1 MUST be torn down!
    ws1._recv({ type: 'error', code: 'PEER_OFFLINE', error: 'agent offline' });
    assert.equal(session.getPeerConnection(), null, 'PEER_OFFLINE must tear down unanswered PeerConnection');
    assert.equal(pcs[0].closed, true);

    // Agent returns online -> MUST immediately start PC #2 instead of deadlocking on zombie PC #1!
    ws1._recv({ type: 'peer_status', node_id: 'demo', online: true, require_visitor_auth: false, agent_pk: '' });
    await new Promise((r) => setTimeout(r, 20));
    assert.equal(pcs.length, 2, 'peer_status(online=true) after PEER_OFFLINE must immediately start PC #2');
    assert.ok(session.getPeerConnection() === pcs[1]);

    // 2. ctrlDC.onclose schedules silentReconnectTimer (500..800ms); peer_status(online=true) MUST cancel that timer!
    session.everAuthenticated = true;
    const ctrl2 = pcs[1].dcs.get('p2pctrl');
    ctrl2.close();
    assert.equal(session.getPeerConnection(), null, 'ctrlDC.onclose must tear down PC #2');
    assert.ok(session.silentReconnectTimer !== null, 'ctrlDC.onclose must schedule silentReconnectTimer');

    ws1._recv({ type: 'peer_status', node_id: 'demo', online: true, require_visitor_auth: false, agent_pk: '' });
    await new Promise((r) => setTimeout(r, 20));
    assert.equal(pcs.length, 3, 'peer_status(online=true) must immediately create PC #3');
    assert.equal(session.silentReconnectTimer, null, 'peer_status(online=true) MUST cancel pending silentReconnectTimer so PC #3 is not killed mid-handshake');

    // 3. GOAWAY frame on p2pctrl MUST immediately tear down PC & schedule silent reconnect!
    const pc3 = pcs[2];
    session.currentOfferDesc = { fp: 'a'.repeat(64), ice_ufrag: 'ufrg1234', ice_pwd: 'password123456789012345678', setup: 'actpass', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 };
    session.currentAnswerDesc = { fp: 'b'.repeat(64), ice_ufrag: 'ufrg5678', ice_pwd: 'password123456789012345678', setup: 'active', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 };
    const ctrl3 = pc3.dcs.get('p2pctrl');
    ctrl3._open();
    const goAwayBytes = frameMod.encodeFrame(0, frameMod.FrameType.GOAWAY, frameMod.FrameFlag.NONE, frameMod.encodeGoAwayPayload(frameMod.ErrorCode.GOING_AWAY));
    ctrl3._recv(goAwayBytes.buffer);
    assert.equal(session.getPeerConnection(), null, 'GOAWAY must immediately tear down current PeerConnection');
    assert.equal(pc3.closed, true);

    // 4. Once in-DTLS authentication succeeds, Portal MUST immediately close signaling WS!
    ws1._recv({ type: 'peer_status', node_id: 'demo', online: true, require_visitor_auth: false, agent_pk: '' });
    await new Promise((r) => setTimeout(r, 20));
    assert.equal(pcs.length, 4);
    const pc4 = pcs[3];
    session.currentOfferDesc = { fp: 'a'.repeat(64), ice_ufrag: 'ufrg1234', ice_pwd: 'password123456789012345678', setup: 'actpass', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 };
    session.currentAnswerDesc = { fp: 'b'.repeat(64), ice_ufrag: 'ufrg5678', ice_pwd: 'password123456789012345678', setup: 'active', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 };
    session.verifiedPkHash = 'PK_HASH_DEMO_22_CHARS_';
    session.verifiedKeyStatus = 'tofu_matched';
    const ctrl4 = pc4.dcs.get('p2pctrl');
    ctrl4._open();
    await new Promise((r) => setTimeout(r, 15));
    const te = new TextEncoder();
    const chalFrame = frameMod.encodeFrame(
      0,
      frameMod.FrameType.AUTH_CHALLENGE,
      frameMod.FrameFlag.NONE,
      te.encode(JSON.stringify({ agent_nonce: 'anonce-ok', auth_methods: ['public'] }))
    );
    ctrl4._recv(chalFrame.buffer);
    await new Promise((r) => setTimeout(r, 15));
    const resFrame = frameMod.encodeFrame(
      0,
      frameMod.FrameType.AUTH_RESULT,
      frameMod.FrameFlag.NONE,
      te.encode(
        JSON.stringify({
          ok: true,
          role: 'public',
          meta: { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false },
        })
      )
    );
    ctrl4._recv(resFrame.buffer);
    await new Promise((r) => setTimeout(r, 20));
    assert.equal(authedEvents.length, 1, 'public auth must complete');
    assert.equal(ws1.readyState, MockWS.CLOSED, 'P1-01: signaling WS MUST be closed immediately after authentication');
    assert.equal(session.handshakeWatchdogTimer, null, 'handshake watchdog must be cleared on authentication');

    // Subsequent ctrlDC.onclose MUST tear down pc4 and schedule silent reconnect (which opens a fresh WS)!
    ctrl4.close();
    assert.equal(pc4.closed, true);
    assert.ok(session.silentReconnectTimer !== null, 'post-auth ctrlDC.onclose must schedule silent reconnect');
  } finally {
    session?.destroy();
    globalThis.WebSocket = origWS;
    globalThis.RTCPeerConnection = origPC;
    globalThis.location = origLoc;
  }
});

test('Audit Regressions (P1-2, P1-5, P2-2, P2-4, P2-5, P2-6, P2-9): Cross-Browser Nav, Gzip ETag, Cookie Parity, Multi-CSP, Nested SW Eviction, Worker Hashbang & Pinned TOFU', async () => {
  const swOrigin = 'https://sbx-1.sandbox.example';
  const portalOrigin = 'https://portal.example';

  // 1. P1-2: Firefox & Safari (`replacesClientId === ''`) link click + reload/Back/Forward + CSRF immunity!
  const reg = new sessionRegMod.SessionRegistry(new Set([portalOrigin]));
  const ch1 = new MessageChannel();
  const ch2 = new MessageChannel();
  try {
    reg.attachPort(
      'tab-1',
      portalOrigin,
      { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
      ch1.port1
    );
    reg.registerEnterNonce('nonce-ff-safari-1234', 'tab-1', '/home');
    const enterRes = reg.handleEnterNavigation(
      new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&nonce=nonce-ff-safari-1234&next=/home`),
      'c-root-1',
      '',
      { method: 'GET', mode: 'navigate', destination: 'iframe' }
    );
    assert.equal(enterRes.status, 302);
    assert.equal(reg.getSessionByTabId('tab-1').rootClientId, 'c-root-1');

    // Unbound client or child iframe MUST NOT be allowed to arm expected root nav!
    reg.bindClient('c-child-iframe', 'tab-1', false);
    assert.equal(reg.markExpectedRootNav('c-unknown'), false);
    assert.equal(reg.markExpectedRootNav('c-child-iframe'), false);

    // Without P2P_EXPECT_NAV, external unbound GET iframe navigation STILL returns 403 (CSRF intact!).
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: 'c-csrf',
        replacesClientId: '',
        mode: 'navigate',
        destination: 'iframe',
        method: 'GET',
        referrer: '',
        swOrigin,
      }).allowed,
      false
    );

    // Bound root appFrame fires beforeunload -> P2P_EXPECT_NAV -> link click without replacesClientId transfers rootClientId!
    assert.equal(reg.markExpectedRootNav('c-root-1'), true);
    const clickNav = fetchMod.authorizeFetchRequest(reg, {
      clientId: 'c-root-1',
      resultingClientId: 'c-root-2',
      replacesClientId: '',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: `${swOrigin}/home`,
      swOrigin,
    });
    assert.equal(clickNav.allowed, true);
    assert.equal(reg.getSessionByTabId('tab-1').rootClientId, 'c-root-2', 'rootClientId must transfer on Firefox/Safari link click');

    // Reload / Back / Forward in Firefox/Safari: clientId='', replacesClientId='' succeeds ONCE and re-binds rootClientId!
    // First prove a cross-site / same-site attacker iframe CANNOT race-steal an armed P2P_EXPECT_NAV token!
    assert.equal(reg.markExpectedRootNav('c-root-2'), true);
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: 'c-csrf-race',
        replacesClientId: '',
        mode: 'navigate',
        destination: 'iframe',
        method: 'GET',
        referrer: '',
        secFetchSite: 'cross-site',
        swOrigin,
      }).allowed,
      false,
      'cross-site iframe MUST NOT steal armed P2P_EXPECT_NAV token'
    );
    const reloadNav = fetchMod.authorizeFetchRequest(reg, {
      clientId: '',
      resultingClientId: 'c-root-3',
      replacesClientId: '',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: '',
      secFetchSite: 'none',
      swOrigin,
    });
    assert.equal(reloadNav.allowed, true);
    assert.equal(reloadNav.sameOriginClient, true);
    assert.equal(reg.getSessionByTabId('tab-1').rootClientId, 'c-root-3');

    // Replay of consumed single-use expectRootNav MUST fail closed (403)!
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: 'c-replay',
        replacesClientId: '',
        mode: 'navigate',
        destination: 'iframe',
        method: 'GET',
        referrer: '',
        swOrigin,
      }).allowed,
      false
    );

    // Multi-tab ambiguity (2 tabs armed simultaneously) MUST fail closed (403)!
    reg.attachPort(
      'tab-2',
      portalOrigin,
      { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
      ch2.port1
    );
    reg.bindClient('c-tab2-root', 'tab-2', true);
    assert.equal(reg.markExpectedRootNav('c-root-3'), true);
    assert.equal(reg.markExpectedRootNav('c-tab2-root'), true);
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: 'c-ambig',
        replacesClientId: '',
        mode: 'navigate',
        destination: 'iframe',
        method: 'GET',
        referrer: '',
        swOrigin,
      }).allowed,
      false
    );

    // P2P_NAV_SETTLED (`pageshow` / BFCache restore) must disarm pending expectation & restore rootClientId!
    reg.clearExpectedRootNav('c-root-3');
    reg.clearExpectedRootNav('c-tab2-root');
    assert.equal(reg.consumeExpectedRootNav(), null, 'clearExpectedRootNav must disarm pending tokens');

    // Once Chromium reports a non-empty replacesClientId, P2P_EXPECT_NAV fallback is permanently disabled!
    const chromNav = fetchMod.authorizeFetchRequest(reg, {
      clientId: 'c-root-3',
      resultingClientId: 'c-root-4',
      replacesClientId: 'c-root-3',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: `${swOrigin}/home`,
      swOrigin,
    });
    assert.equal(chromNav.allowed, true);
    assert.equal(reg.markExpectedRootNav('c-root-4'), false, 'Chromium must ignore P2P_EXPECT_NAV once replacesClientId is observed');

    // 2. P1-5: Transparent gzipped non-HTML/non-Worker asset MUST preserve ETag!
    ch1.port2.onmessage = (ev) => {
      if (ev.data?.type === 'TUNNEL_FETCH_REQUEST') {
        const reqPort = ev.ports[0];
        reqPort.postMessage({ type: 'REQ_ACK', reqId: ev.data.reqId });
        const gz = new Blob(['console.log("cached");'.repeat(64)])
          .stream()
          .pipeThrough(new CompressionStream('gzip'));
        reqPort.postMessage(
          {
            type: 'RESP_START',
            status: 200,
            respFlags: tlvMod.RespFlag.DECOMPRESS_GZIP,
            headers: [
              { name: 'content-type', value: 'application/javascript' },
              { name: 'etag', value: '"v1-preserved-etag"' },
            ],
            cookieDeltas: [],
            hasBody: true,
            bodyStream: gz,
          },
          [gz]
        );
        reqPort.close();
      }
    };
    const gzResp = await fetchMod.handleFetchEvent(
      { location: { origin: swOrigin }, clients: { get: async () => undefined } },
      reg,
      {
        clientId: 'c-root-3',
        resultingClientId: '',
        replacesClientId: '',
        request: new Request(`${swOrigin}/bundle.js`, { method: 'GET' }),
        waitUntil() {},
      }
    );
    assert.equal(gzResp.status, 200);
    assert.equal(gzResp.headers.get('etag'), '"v1-preserved-etag"', 'P1-5: ETag must be preserved on transparent gzip');
    assert.ok((await gzResp.text()).includes('console.log("cached")'));
  } finally {
    ch1.port1.close();
    ch1.port2.close();
    ch2.port1.close();
    ch2.port2.close();
  }

  // 3. P2-2: Cookie prefix parity with Go ProxyCookieJar (__Host- & __Secure- rejected even on secure context).
  assert.equal(
    patchMod.parseDocumentCookieAssignment('__Host-x=1; Secure; Path=/', { pathname: '/', isSecureContext: true }),
    null
  );
  assert.equal(
    patchMod.parseDocumentCookieAssignment('__Secure-x=1; Secure', { pathname: '/', isSecureContext: true }),
    null
  );

  // 4. P2-4: Multi-header CSP with distinct nonces shares Policy 1's nonce into Policy 2!
  const multiCsp = fetchMod.buildSafeResponseHeaders(
    [
      { name: 'content-security-policy', value: "default-src 'self'; script-src 'nonce-AAA111'" },
      { name: 'content-security-policy', value: "default-src 'self'; script-src 'nonce-BBB222'" },
    ],
    { stripContentLength: true, swOrigin, backendOrigin: 'http://app.local' }
  );
  assert.equal(multiCsp.scriptNonce, 'AAA111');
  const combinedCsp = multiCsp.headers.get('content-security-policy') || '';
  assert.ok(combinedCsp.includes("'nonce-BBB222' 'nonce-AAA111'"), `Policy 2 must permit shared gateway nonce: ${combinedCsp}`);

  // 5. P2-5: TunnelBridge accepts P2P_SW_EVICTED from nested child iframe on sandboxOrigin,
  //    while keeping P2P_ROUTE_SYNC / P2P_TITLE_SYNC strictly restricted to appFrame.contentWindow!
  let evictedCount = 0;
  let routeSynced = '';
  const bridge = new portalBridgeMod.TunnelBridge({
    onSwEvicted: () => evictedCount++,
    onRouteSync: (p) => { routeSynced = p; },
  });
  const fakeAppWin = {};
  const fakeNestedChildWin = {};
  bridge.setContext(
    null,
    swOrigin,
    { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
    { contentWindow: fakeAppWin }
  );
  bridge.handleWindowMessage({ isTrusted: true, origin: swOrigin, source: fakeNestedChildWin, data: { type: 'P2P_SW_EVICTED' } });
  assert.equal(evictedCount, 1, 'P2-5: nested child iframe on sandboxOrigin must trigger onSwEvicted');
  bridge.handleWindowMessage({ isTrusted: true, origin: 'https://evil.example', source: fakeNestedChildWin, data: { type: 'P2P_SW_EVICTED' } });
  assert.equal(evictedCount, 1, 'foreign origin P2P_SW_EVICTED must be ignored');
  bridge.handleWindowMessage({ isTrusted: true, origin: swOrigin, source: fakeNestedChildWin, data: { type: 'P2P_ROUTE_SYNC', path: '/hijack' } });
  assert.equal(routeSynced, '', 'nested child iframe MUST NOT spoof P2P_ROUTE_SYNC');

  // 6. P2-6: Worker script with ES2023 `#!` hashbang preserves hashbang on line 1!
  const workerWithHashbang = htmlMod.injectWorkerBootstrap(
    '#!/usr/bin/env worker\n"use strict";\npostMessage("ok");',
    swOrigin,
    { backendOrigin: 'http://app.local' }
  );
  assert.ok(workerWithHashbang.startsWith('#!/usr/bin/env worker\n"use strict";\n'), 'hashbang + use strict must stay before preamble');

  // 7. P2-9: Verified first visit with pinned `?pk=<good>` commits TOFU so subsequent bare `#node` blocks rogue keys!
  identityMod.clearMemoryTofuStoreForTests();
  const pinnedHash = 'GOOD_PINNED_PK_HASH_22';
  let pinnedAuthedInfo = null;
  const authCli = new authClientMod.AuthClient(
    'pinned-node',
    portalOrigin,
    false,
    pinnedHash,
    '',
    {
      onStatus() {},
      onLog() {},
      onAuthRequired() {},
      onAuthFailed() {},
      onFatalSecurityError(msg) { throw new Error(msg); },
      onAuthenticated(info) { pinnedAuthedInfo = info; },
    }
  );
  const mockCtrlMux = { sendControlFrame() {} };
  const gen = authCli.beginChannel({
    mux: mockCtrlMux,
    offerDesc: { fp: 'a'.repeat(64), ice_ufrag: 'u1', ice_pwd: 'p'.repeat(24), setup: 'actpass', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 },
    answerDesc: { fp: 'b'.repeat(64), ice_ufrag: 'u2', ice_pwd: 'q'.repeat(24), setup: 'active', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 },
    visitorNonce: 'vnonce',
    agentPkB64: cryptoMod.base64UrlEncode(new Uint8Array(32).fill(7)),
    verifiedPkHash: pinnedHash,
    verifiedKeyStatus: 'pinned_ok',
  });
  const te = new TextEncoder();
  await authCli.handleControlFrame(gen, {
    type: frameMod.FrameType.AUTH_CHALLENGE,
    flags: 0,
    payload: te.encode(JSON.stringify({ agent_nonce: 'anonce', auth_methods: ['public'] })),
  });
  await authCli.handleControlFrame(gen, {
    type: frameMod.FrameType.AUTH_RESULT,
    flags: 0,
    payload: te.encode(
      JSON.stringify({
        ok: true,
        role: 'public',
        meta: { nodeId: 'pinned-node', backendOrigin: 'http://app.local', isStaticMode: false, cookieSecureContext: false },
      })
    ),
  });
  assert.ok(pinnedAuthedInfo, 'pinned visit must authenticate cleanly');
  assert.equal(pinnedAuthedInfo.meta.cookieSecureContext, false, 'cookieSecureContext must propagate through BackendMeta');

  // Verify TOFU was actually committed by attempting a bare `#pinned-node` auth with a DIFFERENT key
  // in the same module instance: commitTofuKey MUST detect the conflict and fire onFatalSecurityError!
  const conflictErrors = [];
  const bareRogueAuth = new authClientMod.AuthClient(
    'pinned-node',
    portalOrigin,
    false,
    '',
    '',
    {
      onStatus() {},
      onLog() {},
      onAuthRequired() {},
      onAuthFailed() {},
      onFatalSecurityError(msg) { conflictErrors.push(msg); },
      onAuthenticated() { throw new Error('rogue key must NOT authenticate after pinned TOFU commit'); },
    }
  );
  const gen2 = bareRogueAuth.beginChannel({
    mux: mockCtrlMux,
    offerDesc: { fp: 'a'.repeat(64), ice_ufrag: 'u1', ice_pwd: 'p'.repeat(24), setup: 'actpass', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 },
    answerDesc: { fp: 'b'.repeat(64), ice_ufrag: 'u2', ice_pwd: 'q'.repeat(24), setup: 'active', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 },
    visitorNonce: 'vnonce2',
    agentPkB64: cryptoMod.base64UrlEncode(new Uint8Array(32).fill(9)),
    verifiedPkHash: 'EVIL_ROGUE_PK_HASH_22_',
    verifiedKeyStatus: 'tofu_new',
  });
  await bareRogueAuth.handleControlFrame(gen2, {
    type: frameMod.FrameType.AUTH_CHALLENGE,
    flags: 0,
    payload: te.encode(JSON.stringify({ agent_nonce: 'anonce2', auth_methods: ['public'] })),
  });
  await bareRogueAuth.handleControlFrame(gen2, {
    type: frameMod.FrameType.AUTH_RESULT,
    flags: 0,
    payload: te.encode(
      JSON.stringify({
        ok: true,
        role: 'public',
        meta: { nodeId: 'pinned-node', backendOrigin: 'http://app.local', isStaticMode: false },
      })
    ),
  });
  assert.ok(
    conflictErrors.some((e) => /TOFU key conflict/i.test(e)),
    `P2-9: subsequent bare #node visit with rogue key must fail with TOFU conflict, got: ${JSON.stringify(conflictErrors)}`
  );
});

test('Chief-Architect Audit Regressions: WSStream credit deadlock (P0-4), close ordering (P2-2), SW eviction reload (P1-5), multi-tab -dir (P1-6), CSP default-src none (P2-1), Cookie broadcast (P1-4), Ed25519 NotSupportedError (P2-3), CAP_RESP.revoked (P3-1)', async () => {
  const [cookiePatchMod, fetchPatchMod] = await Promise.all([
    loadTsModule('web/src/patch/cookie_patch.ts'),
    loadTsModule('web/src/patch/fetch_xhr_patch.ts'),
  ]);

  const makeFakeDC = (onSend) => ({
    readyState: 'open',
    bufferedAmount: 0,
    bufferedAmountLowThreshold: 0,
    onbufferedamountlow: null,
    onmessage: null,
    onopen: null,
    onclose: null,
    onerror: null,
    send(data) {
      onSend(data instanceof Uint8Array ? data : new Uint8Array(data));
    },
    close() {
      this.readyState = 'closed';
    },
  });

  // 1. P0-4: WSStream MUST flush deferred credit for in-progress asmChunks when onMessage() attaches.
  {
    const sentFrames = [];
    const laneDC = makeFakeDC((raw) => sentFrames.push(frameMod.decodeFrame(raw)));
    const mux = new muxMod.PoolMux(1);
    mux.attachControlDC(makeFakeDC(() => {}));
    mux.attachLaneDC(0, laneDC);

    const wsReq = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.WS, method: 'GET', target: '/ws', headers: [] });
    const wsStream = await mux.openWSStream(wsReq);
    const resp101 = tlvMod.encodeResponseHeaders({ status: 101, headers: [] });
    laneDC.onmessage({ data: frameMod.encodeFrame(wsStream.streamId, frameMod.FrameType.HEADERS, 0, resp101) });
    await wsStream.waitResponseHeaders();

    // Msg 1 arrives while msgCb === null, then 512 KiB partial Msg 2 arrives before onMessage() is attached.
    laneDC.onmessage({
      data: frameMod.encodeFrame(
        wsStream.streamId,
        frameMod.FrameType.WS_TEXT,
        frameMod.FrameFlag.MSG_FIN,
        new TextEncoder().encode('msg1')
      ),
    });
    const chunk16k = new Uint8Array(16384).fill(0x41);
    for (let i = 0; i < 32; i++) {
      laneDC.onmessage({
        data: frameMod.encodeFrame(wsStream.streamId, frameMod.FrameType.WS_BINARY, 0, chunk16k),
      });
    }
    sentFrames.length = 0;
    const delivered = [];
    wsStream.onMessage((ev) => delivered.push(ev));
    const credited = sentFrames
      .filter((f) => f.type === frameMod.FrameType.CREDIT)
      .reduce((acc, f) => acc + frameMod.decodeCreditPayload(f.payload), 0);
    assert.equal(delivered.length, 1);
    // 4 bytes from msg1 (below threshold earlier) + 512 KiB deferred from partial msg2 = 524292 exact!
    assert.equal(credited, 512 * 1024 + 4, 'P0-4: onMessage() must flush exact deferred asmChunks credit immediately');
    mux.close();
  }

  // 2. P2-2: WSStream.close() MUST serialize after in-flight multi-chunk sendMessage().
  {
    const sentFrames = [];
    const laneDC = makeFakeDC((raw) => sentFrames.push(frameMod.decodeFrame(raw)));
    const mux = new muxMod.PoolMux(1);
    mux.attachControlDC(makeFakeDC(() => {}));
    mux.attachLaneDC(0, laneDC);

    const wsReq = tlvMod.encodeRequestHeaders({ kind: tlvMod.StreamKind.WS, method: 'GET', target: '/ws', headers: [] });
    const wsStream = await mux.openWSStream(wsReq);
    sentFrames.length = 0;

    laneDC.bufferedAmount = 1024 * 1024;
    const sendPromise = wsStream.sendMessage(new Uint8Array(64 * 1024).fill(0x55));
    wsStream.close(1000, 'bye');
    laneDC.bufferedAmount = 0;
    if (laneDC.onbufferedamountlow) laneDC.onbufferedamountlow();
    await sendPromise;
    await new Promise((r) => setTimeout(r, 0));

    const types = sentFrames.map((f) => f.type);
    const binIdx = types.indexOf(frameMod.FrameType.WS_BINARY);
    const closeIdx = types.indexOf(frameMod.FrameType.WS_CLOSE);
    assert.ok(binIdx >= 0, 'P2-2: in-flight WS_BINARY chunks must be transmitted before WS_CLOSE');
    assert.ok(closeIdx > binIdx, `P2-2: WS_CLOSE must follow WS_BINARY (got ${JSON.stringify(types)})`);
    mux.close();
  }

  // 3. P2-1: rewriteUpstreamCsp("default-src 'none'") MUST NOT relax default-src to 'self'.
  {
    const res = fetchMod.rewriteUpstreamCsp("default-src 'none'", {
      swOrigin: 'https://sbx-1.sandbox.example',
    });
    assert.equal(
      res.policy,
      "default-src 'none'; script-src-elem 'self'",
      'P2-1: default-src none must stay none while synthesizing minimal script-src-elem self'
    );
  }

  // 4. P1-6: Multi-tab same-node static `-dir` mode succeeds; mixed nodes or non-static fail closed.
  {
    const reg = new sessionRegMod.SessionRegistry(new Set(['https://portal.example']));
    const mkPort = () => ({ postMessage() {}, close() {}, onmessage: null });
    reg.attachPort('tab-1', 'https://portal.example', { nodeId: 'docs', backendOrigin: 'http://static.invalid', isStaticMode: true }, mkPort());
    reg.attachPort('tab-2', 'https://portal.example', { nodeId: 'docs', backendOrigin: 'http://static.invalid', isStaticMode: true }, mkPort());
    const authSameNode = fetchMod.authorizeFetchRequest(reg, {
      clientId: '',
      resultingClientId: 'r-1',
      replacesClientId: '',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: '',
      secFetchSite: 'same-origin',
      swOrigin: 'https://sbx.example',
    });
    assert.equal(authSameNode.allowed, true, 'P1-6: multi-tab same static node must allow opaque-origin navigation');

    // Adding a DIFFERENT static node must fail closed!
    reg.attachPort('tab-3', 'https://portal.example', { nodeId: 'other-docs', backendOrigin: 'http://static.invalid', isStaticMode: true }, mkPort());
    const authMixedNode = fetchMod.authorizeFetchRequest(reg, {
      clientId: '',
      resultingClientId: 'r-2',
      replacesClientId: '',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: '',
      secFetchSite: 'same-origin',
      swOrigin: 'https://sbx.example',
    });
    assert.equal(authMixedNode.allowed, false, 'P1-6: mixed static nodes sharing origin must still fail closed');
  }

  // 5. P1-5: SW-evicted iframe reload with clientId === '' recovers via 503 SW_EVICTED (while cross-site stays 403).
  {
    const reg = new sessionRegMod.SessionRegistry(new Set(['https://portal.example']));
    const makeNavReq = (secFetchSite = 'same-origin') => {
      const req = new Request('https://sbx-1.sandbox.example/dashboard', {
        method: 'GET',
        headers: secFetchSite ? { 'sec-fetch-site': secFetchSite } : {},
      });
      Object.defineProperty(req, 'mode', { value: 'navigate' });
      Object.defineProperty(req, 'destination', { value: 'iframe' });
      return req;
    };
    const posted = [];
    const nestedClient = {
      id: 'old-iframe-client-1',
      type: 'window',
      frameType: 'nested',
      url: 'https://sbx-1.sandbox.example/dashboard',
      postMessage(m) { posted.push(m); },
    };
    const fakeSw = {
      location: { origin: 'https://sbx-1.sandbox.example' },
      clients: {
        async get(id) { return id === nestedClient.id ? nestedClient : undefined; },
        async matchAll() { return [nestedClient]; },
      },
    };
    const respReplaces = await fetchMod.handleFetchEvent(fakeSw, reg, {
      request: makeNavReq('same-origin'),
      clientId: '',
      resultingClientId: 'new-client-2',
      replacesClientId: 'old-iframe-client-1',
      waitUntil() {},
    });
    assert.equal(respReplaces.status, 503, 'P1-5: SW-evicted iframe reload must return 503 recovery instead of 403');
    assert.equal(respReplaces.headers.get('x-p2p-sw-evicted'), '1');

    // Cross-site embedder with empty replacesClientId must STILL get 403!
    const respCrossSite = await fetchMod.handleFetchEvent(fakeSw, reg, {
      request: makeNavReq('cross-site'),
      clientId: '',
      resultingClientId: 'evil-client-3',
      replacesClientId: '',
      waitUntil() {},
    });
    assert.equal(respCrossSite.status, 403, 'P1-5: cross-site iframe must stay 403');
  }

  // 6. P1-4: Cookie patch name/value parity + unified Portal/sibling broadcast helper.
  {
    assert.equal(cookiePatchMod.parseDocumentCookieAssignment('bad name=1')?.name ?? null, null);
    assert.equal(cookiePatchMod.parseDocumentCookieAssignment('prefs=en,zh; Path=/')?.value, 'en,zh');
    assert.equal(cookiePatchMod.parseDocumentCookieAssignment('ok=' + 'a'.repeat(4097)), null);

    const portalMsgs = [];
    const siblingMsgs = [];
    const fakeSw = {
      clients: {
        async get(id) {
          if (id === 'sib-1') {
            return { id, url: 'https://sbx.example/sub', postMessage(m) { siblingMsgs.push(m); } };
          }
          return undefined;
        },
      },
    };
    await cookieSyncMod.broadcastClientCookieDeltas(
      fakeSw,
      {
        clientIds: new Set(['origin-1', 'sib-1']),
        port: { postMessage(m) { portalMsgs.push(m); } },
      },
      'origin-1',
      [{ op: 'set', name: 'xsrf', value: 'tok1', path: '/' }]
    );
    assert.equal(portalMsgs.length, 1);
    assert.equal(portalMsgs[0].type, 'BRIDGE_CLIENT_COOKIE_DELTAS');
    assert.equal(siblingMsgs.length, 1);
    assert.equal(siblingMsgs[0].type, 'P2P_COOKIE_MIRROR_UPDATE');
  }

  // 7. P2-3: WebCrypto Ed25519 NotSupportedError throws UnsupportedCryptoError (not silent false).
  {
    const origImport = crypto.subtle.importKey;
    try {
      crypto.subtle.importKey = async () => {
        const err = new Error('Algorithm: Unrecognized name');
        err.name = 'NotSupportedError';
        throw err;
      };
      await assert.rejects(
        () => cryptoMod.verifyEd25519(new Uint8Array(32), new Uint8Array([1]), new Uint8Array(64)),
        (err) => err instanceof cryptoMod.UnsupportedCryptoError && /Chrome 137\+/i.test(err.message)
      );
    } finally {
      crypto.subtle.importKey = origImport;
    }
  }

  // 8. P3-1: AuthClient CAP_RESP preserves boolean `revoked` field.
  {
    let sentReqId = '';
    const te = new TextEncoder();
    const mockMux = {
      sendControlFrame(_type, payload) {
        sentReqId = JSON.parse(new TextDecoder().decode(payload)).req_id;
      },
      setMaxMessageSize() {},
    };
    const ac = new authClientMod.AuthClient('n1', 'https://portal.example', false, '', '', {
      onStatus() {},
      onLog() {},
      onAuthRequired() {},
      onAuthFailed() {},
      onFatalSecurityError(m) { throw new Error(m); },
      onAuthenticated() {},
    });
    const gen = ac.beginChannel({
      mux: mockMux,
      offerDesc: { fp: 'a'.repeat(64), ice_ufrag: 'u1', ice_pwd: 'p'.repeat(24), setup: 'actpass', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 },
      answerDesc: { fp: 'b'.repeat(64), ice_ufrag: 'u2', ice_pwd: 'q'.repeat(24), setup: 'active', mid: '0', sctp_port: 5000, max_msg_size: 262144, sdp_ver: 2 },
      visitorNonce: 'vn',
      agentPkB64: cryptoMod.base64UrlEncode(new Uint8Array(32).fill(1)),
      verifiedPkHash: 'HASH_12345678901234567',
      verifiedKeyStatus: 'tofu_match',
    });
    await ac.handleControlFrame(gen, {
      type: frameMod.FrameType.AUTH_CHALLENGE,
      flags: 0,
      payload: te.encode(JSON.stringify({ agent_nonce: 'an', auth_methods: ['public'] })),
    });
    await ac.handleControlFrame(gen, {
      type: frameMod.FrameType.AUTH_RESULT,
      flags: 0,
      payload: te.encode(JSON.stringify({ ok: true, role: 'public', meta: { nodeId: 'n1', backendOrigin: 'http://app.local', isStaticMode: false } })),
    });
    const capPromise = ac.requestCapabilityOp({ action: 'revoke', jti: 'jti-123' });
    await ac.handleControlFrame(gen, {
      type: frameMod.FrameType.CAP_RESP,
      flags: 0,
      payload: te.encode(JSON.stringify({ req_id: sentReqId, ok: true, jti: 'jti-123', revoked: false })),
    });
    const capRes = await capPromise;
    assert.equal(capRes.ok, true);
    assert.equal(capRes.revoked, false, 'P3-1: CAP_RESP must preserve boolean revoked field');
  }

  // 9. P2-4: isSameCanonicalRequestUrl confirms Request passthrough helper stays sound.
  assert.equal(
    fetchPatchMod.isSameCanonicalRequestUrl('/api/x', 'https://sbx.example/api/x', 'https://sbx.example/app/'),
    true
  );
});
