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
  const cfg = {
    nodeId: 'demo',
    portalOrigin: 'https://portal.example',
    backendOrigin: 'http://app.local:8080',
    initCookies: [],
    ...cfgOverrides,
  };
  const inj = new htmlMod.StreamingHtmlInjector('https://sbx-1.sandbox.example', cfg, opts);
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

test('html_injector V7: HTML5 comments (<!-->, <!--->, --!>), unquoted attrs, <template>/<svg>, UTF-16 skip, post-DOCTYPE fallback', async () => {
  const html =
    '<!doctype html>' +
    '<!--><!---><!-- fake <head> --!>' +
    '<html data-raw=a"b>c>' +
    '<template><head>fake</head><script>ignored()</script></template>' +
    '<svg><head>fake</head><title/></svg>' +
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
  const snippetStart = out.indexOf('<script src="https://sbx-1.sandbox.example/__p2p_gw__/assets/patch.js" data-p2p-boot="');
  assert.equal(snippetStart, realHeadEnd, 'snippet must inject immediately after real <head>');

  // Executable <script> before <head> MUST inject strictly BEFORE that <script> so patch runs first!
  const scriptFirst = '<!doctype html><html><script>window.x=1;</script><head><title>t</title></head></html>';
  const scriptFirstOut = new TextDecoder().decode(runHtmlInjector([scriptFirst]));
  assert.equal(
    scriptFirstOut.indexOf('<script src="https://sbx-1.sandbox.example/__p2p_gw__/assets/patch.js"'),
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

test('fetch_router: multi-value CSP sandbox append, strict directive tokenizer, Emoji header safe', () => {
  // Two CSP headers — first has `sandbox`, second does not; first must survive.
  const { headers, preserveFileSandboxCSP } = fetchMod.buildSafeResponseHeaders(
    [
      { name: 'content-security-policy', value: "SANDBOX; default-src 'none'" },
      { name: 'content-security-policy', value: "default-src 'self'; report-to sandbox" },
      { name: 'content-disposition', value: 'attachment; filename="年度财务报告 2026.pdf"' },
      { name: 'x-emoji-note', value: 'Hello 🚀 世界!' },
      { name: 'service-worker-allowed', value: '/' },
      { name: 'x-p2p-cookie-delta', value: 'eyJ0ZXN0IjoxfQ' },
    ],
    { stripContentLength: false }
  );

  assert.equal(preserveFileSandboxCSP, true);
  assert.equal(headers.get('content-security-policy'), "SANDBOX; default-src 'none'");
  assert.equal(headers.get('service-worker-allowed'), null, 'service-worker-allowed must be stripped');
  assert.equal(headers.get('x-p2p-cookie-delta'), null, 'internal cookie delta header must be stripped');
  // Emoji surrogate pair must encode cleanly without throwing URIError.
  assert.ok(headers.get('x-emoji-note')?.includes('%F0%9F%9A%80'));

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
  }

  const big = new Uint8Array(64);
  big.set([1, 2, 3, 4], 10);
  const xfer = streamBridgeMod.toSafeTransferableBuffer(big.subarray(10, 14));
  assert.notEqual(xfer.buffer, big.buffer);
  assert.deepEqual(Array.from(new Uint8Array(xfer.buffer)), [1, 2, 3, 4]);
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

  // RFC 8841 max-message-size=0 means unlimited -> clamped to 262144; 1..4095 rejected; huge sdp_ver rejected!
  const zeroMmsSdp = synth.replace('a=max-message-size:262144', 'a=max-message-size:0');
  assert.equal(sdpMod.extractSessionDescriptor(zeroMmsSdp, 2).max_msg_size, 262144);
  const tinyMmsSdp = synth.replace('a=max-message-size:262144', 'a=max-message-size:1024');
  assert.throws(() => sdpMod.extractSessionDescriptor(tinyMmsSdp, 2), /max-message-size|invalid|4096/i);
  assert.equal(sdpMod.validateSessionDescriptor({ ...offerDesc, sdp_ver: 2 ** 51 }, true), false);
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

test('TunnelBridge V7: BRIDGE_PING, immediate REQ_ACK, header strip, V7 TLV cookieMutations & cookieDeltas', async () => {
  function makePair() {
    const mk = () => {
      const listeners = new Map();
      return {
        readyState: 'open',
        bufferedAmount: 0,
        bufferedAmountLowThreshold: 0,
        binaryType: 'arraybuffer',
        peer: null,
        addEventListener(type, fn) {
          let s = listeners.get(type);
          if (!s) {
            s = new Set();
            listeners.set(type, s);
          }
          s.add(fn);
        },
        removeEventListener(type, fn) {
          listeners.get(type)?.delete(fn);
        },
        _emit(type, ev) {
          for (const fn of Array.from(listeners.get(type) || [])) fn(ev);
        },
        send(data) {
          if (this.readyState !== 'open') throw new Error('closed');
          const cp = new Uint8Array(data).slice();
          const p = this.peer;
          if (p && p.readyState === 'open') {
            queueMicrotask(() => p._emit('message', { data: cp.buffer }));
          }
        },
        close() {
          this.readyState = 'closed';
          this._emit('close', {});
        },
      };
    };
    const a = mk();
    const b = mk();
    a.peer = b;
    b.peer = a;
    return [a, b];
  }

  const [cCtrl, sCtrl] = makePair();
  const [cSlot, sSlot] = makePair();

  const receivedReqs = [];
  sSlot.addEventListener('message', (ev) => {
    const f = frameMod.decodeFrame(new Uint8Array(ev.data));
    if (f.type === frameMod.FrameType.HEADERS) {
      receivedReqs.push(tlvMod.decodeRequestHeaders(f.payload));
      const resp = tlvMod.encodeResponseHeaders({
        status: 200,
        respFlags: tlvMod.RespFlag.NONE,
        headers: [{ name: 'content-type', value: 'text/plain' }],
        cookieDeltas: [{ op: tlvMod.CookieOp.SET, name: 'theme', value: 'dark', path: '/', expiresMs: 0 }],
      });
      sSlot.send(frameMod.encodeFrame(f.gen, frameMod.FrameType.HEADERS, frameMod.FrameFlag.NONE, resp));
      sSlot.send(
        frameMod.encodeFrame(f.gen, frameMod.FrameType.DATA, frameMod.FrameFlag.FIN, new TextEncoder().encode('hello-v7'))
      );
    }
  });

  const clientMux = new muxMod.PoolMux({}, 1);
  clientMux.attachControlDC(cCtrl);
  clientMux.attachPoolDC(0, cSlot);

  const bridge = new portalBridgeMod.TunnelBridge();
  bridge.setContext(
    clientMux,
    () => makePair()[0],
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

  // 2. HTTP fetch request strips spoofed headers, carries V7 TLV cookieMutations & receives cookieDeltas!
  const reqCh = new MessageChannel();
  const events = [];
  const doneP = new Promise((r) => {
    reqCh.port1.onmessage = (ev) => {
      events.push(ev.data);
      if (ev.data.type === 'RESP_END' || ev.data.type === 'RESP_ERROR') r();
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
  assert.deepEqual(events[1].cookieDeltas, [{ op: 'set', name: 'theme', value: 'dark', path: '/' }]);
  assert.deepEqual(receivedReqs[0].headers, [{ name: 'accept', value: 'text/plain' }]);
  assert.equal(receivedReqs[0].fetchCtx, tlvMod.FetchCtxFlag.SAME_ORIGIN_CLIENT);
  assert.deepEqual(receivedReqs[0].cookieMutations, [
    { op: tlvMod.CookieOp.DEL, name: 'sid', value: '', path: '/', expiresMs: 0 },
  ]);

  bridge.destroy();
  clientMux.close();
  sCtrl.close();
  sSlot.close();
});

test('TunnelBridge V7: early 204 POST pool slot release & untrusted bridgePort chunk bounds defense', async () => {
  function makePair() {
    const mk = () => {
      const listeners = new Map();
      return {
        readyState: 'open',
        bufferedAmount: 0,
        bufferedAmountLowThreshold: 0,
        binaryType: 'arraybuffer',
        peer: null,
        addEventListener(type, fn) {
          let s = listeners.get(type);
          if (!s) {
            s = new Set();
            listeners.set(type, s);
          }
          s.add(fn);
        },
        removeEventListener(type, fn) {
          listeners.get(type)?.delete(fn);
        },
        _emit(type, ev) {
          for (const fn of Array.from(listeners.get(type) || [])) fn(ev);
        },
        send(data) {
          if (this.readyState !== 'open') throw new Error('closed');
          const cp = new Uint8Array(data).slice();
          const p = this.peer;
          if (p && p.readyState === 'open') {
            queueMicrotask(() => p._emit('message', { data: cp.buffer }));
          }
        },
        close() {
          this.readyState = 'closed';
          this._emit('close', {});
        },
      };
    };
    const a = mk();
    const b = mk();
    a.peer = b;
    b.peer = a;
    return [a, b];
  }

  const [cCtrl, sCtrl] = makePair();
  const poolSize = 4;
  const cSlots = [];
  const sSlots = [];
  for (let i = 0; i < poolSize; i++) {
    const [a, b] = makePair();
    cSlots.push(a);
    sSlots.push(b);
    // Server immediately replies 204 No Content (FIN) on POST BEFORE client finishes sending body!
    b.addEventListener('message', (ev) => {
      const f = frameMod.decodeFrame(new Uint8Array(ev.data));
      if (f.type === frameMod.FrameType.HEADERS) {
        const resp = tlvMod.encodeResponseHeaders({ status: 204, headers: [] });
        b.send(frameMod.encodeFrame(f.gen, frameMod.FrameType.HEADERS, frameMod.FrameFlag.FIN, resp));
      }
    });
  }

  const clientMux = new muxMod.PoolMux({}, poolSize);
  clientMux.attachControlDC(cCtrl);
  for (let i = 0; i < poolSize; i++) clientMux.attachPoolDC(i, cSlots[i]);

  const bridge = new portalBridgeMod.TunnelBridge();
  bridge.setContext(
    clientMux,
    () => makePair()[0],
    'https://sbx-1.sandbox.example',
    { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
    null
  );

  // 40 sequential POSTs returning early 204 No Content on a 4-slot pool MUST NOT deadlock or leak slots!
  for (let i = 0; i < 40; i++) {
    const ch = new MessageChannel();
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
        hasBody: true, // Intentionally never send REQ_BODY_END — simulates early 204 before upload finishes!
      },
      ch.port2
    );
    assert.equal(await got204, 204);
    ch.port1.close();
  }
  assert.equal(clientMux.getActiveStreamCount(), 0, 'all 4 pool slots must be cleanly released');

  // Untrusted bridgePort out-of-bounds REQ_BODY_CHUNK MUST fail with 400 and release stream!
  const [cSlow, sSlow] = makePair();
  const slowMux = new muxMod.PoolMux({}, 1);
  slowMux.attachControlDC(cCtrl);
  slowMux.attachPoolDC(0, cSlow);
  const bridge2 = new portalBridgeMod.TunnelBridge();
  bridge2.setContext(
    slowMux,
    () => makePair()[0],
    'https://sbx-1.sandbox.example',
    { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
    null
  );
  const badCh = new MessageChannel();
  const errP = new Promise((resolve) => {
    badCh.port1.onmessage = (ev) => {
      if (ev.data.type === 'RESP_ERROR') resolve(ev.data);
    };
  });
  await bridge2.handleFetchRequest(
    {
      type: 'TUNNEL_FETCH_REQUEST',
      reqId: 'oob-1',
      tabId: 'tab-1',
      method: 'POST',
      target: '/upload',
      headers: [],
      hasBody: true,
    },
    badCh.port2
  );
  const tiny = new ArrayBuffer(8);
  badCh.port1.postMessage({ type: 'REQ_BODY_CHUNK', buffer: tiny, offset: 4, length: 9999 }, [tiny]);
  const badResp = await errP;
  assert.equal(badResp.status, 400, 'out-of-bounds REQ_BODY_CHUNK must return 400');
  badCh.port1.close();

  bridge.destroy();
  bridge2.destroy();
  clientMux.close();
  slowMux.close();
  sCtrl.close();
  sSlow.close();
  for (const s of sSlots) s.close();
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

    // 2. handleEnterNavigation: requires registered one-time enterNonce, blocks \t open-redirect, handles Chinese path!
    const noNonce = reg.handleEnterNavigation(new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&next=/app`), 'c1', '');
    assert.equal(noNonce.status, 403, 'enter without nonce must return 403');

    reg.registerEnterNonce('nonce-tab-redirect-1234', 'tab-1');
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
    reg.registerEnterNonce('nonce-cn-path-567890', 'tab-1');
    const cnResp = reg.handleEnterNavigation(
      new URL(`${swOrigin}/__p2p_gw__/enter?tab=tab-1&nonce=nonce-cn-path-567890&next=${encodeURIComponent('/目录/文档?q=中文#节')}`),
      'c-app',
      ''
    );
    assert.equal(cnResp.status, 302);
    assert.equal(cnResp.headers.get('location'), '/%E7%9B%AE%E5%BD%95/%E6%96%87%E6%A1%A3?q=%E4%B8%AD%E6%96%87#%E8%8A%82');

    // 3. W3C Fetch Anti-CSRF Gate (authorizeFetchRequest):
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
        resultingClientId: 'r-doc',
        mode: 'navigate',
        destination: 'document',
        method: 'GET',
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

    // P0-2 Negative test: unbound empty-referrer iframe in `-target` proxy mode MUST return 403!
    const emptyRefProxy = fetchMod.authorizeFetchRequest(reg, {
      clientId: '',
      resultingClientId: 'r-emptyref-proxy',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: '',
      swOrigin,
    });
    assert.equal(emptyRefProxy.allowed, false, 'P0-2: unbound empty-referrer iframe MUST be blocked in -target proxy mode');
    assert.equal(reg.isBoundClient('r-emptyref-proxy'), false);

    // P0-2 Positive test: static `-dir`/`-file` (cookie-less, isStaticMode=true) allows empty-referrer iframe
    // AND binds resultingClientId so subresources (<img>/<link>) work!
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
    assert.equal(emptyRefStatic.allowed, true, 'static -dir mode must allow opaque-origin empty-referrer iframe navigation');
    assert.equal(emptyRefStatic.sameOriginClient, false);
    assert.equal(staticReg.isBoundClient('r-emptyref-dir'), true, 'static -dir mode must bind resultingClientId for subresources');

    // Unbound navigate iframe with trusted portalOrigin referrer when sessions.size === 1 IS allowed!
    const portalRefAuth = fetchMod.authorizeFetchRequest(reg, {
      clientId: '',
      resultingClientId: 'r-portalref',
      mode: 'navigate',
      destination: 'iframe',
      method: 'GET',
      referrer: `${portalOrigin}/`,
      swOrigin,
    });
    assert.equal(portalRefAuth.allowed, true);
    assert.equal(portalRefAuth.sameOriginClient, true);

    // Bound appFrame client spawning Worker -> inherits session, sameOriginClient === true, binds resultingClientId!
    const workerAuth = fetchMod.authorizeFetchRequest(reg, {
      clientId: 'c-app',
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

    // Multi-tab Tier 1 tightening: when sessions.size === 2, unbound iframe navigation MUST return 403!
    const ch2 = makeCh();
    reg.attachPort(
      'tab-2',
      portalOrigin,
      { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
      ch2.port1
    );
    assert.equal(
      fetchMod.authorizeFetchRequest(reg, {
        clientId: '',
        resultingClientId: 'r-multitab',
        mode: 'navigate',
        destination: 'iframe',
        method: 'GET',
        referrer: `${portalOrigin}/`,
        swOrigin,
      }).allowed,
      false,
      'multi-tab sessions.size > 1 must reject unbound iframe navigation'
    );

    // 4. P1-25: pruneDeadSessions deletes closed/reloaded tab whose bridgePort does not answer BRIDGE_PING,
    //    restoring getSingleActiveSession() cleanly!
    ch2.port2.onmessage = (ev) => {
      if (ev.data?.type === 'BRIDGE_PING') {
        ch2.port2.postMessage({ type: 'BRIDGE_PONG', pingId: ev.data.pingId });
      }
    };
    // tab-1 port2 does NOT answer BRIDGE_PING -> simulates closed/reloaded Portal tab!
    ch1.port2.onmessage = null;
    const pruneScope = {
      location: { origin: swOrigin },
      clients: { get: async () => undefined },
    };
    await reg.pruneDeadSessions(pruneScope);
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

    // 6. P0-2 / P1-23 collectRequestHeaders: TOP_LEVEL_NAV set ONLY for destination==='document' (NEVER iframe),
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
      (iframeCtx.fetchCtx & tlvMod.FetchCtxFlag.TOP_LEVEL_NAV) !== 0,
      false,
      'iframe navigation must NEVER set TOP_LEVEL_NAV'
    );
    const docNavReq = {
      mode: 'navigate',
      destination: 'document',
      credentials: 'include',
      referrer: `${swOrigin}/`,
      headers: new Headers(),
    };
    const docCtx = fetchMod.collectRequestHeaders(docNavReq, swOrigin, [], true);
    assert.equal((docCtx.fetchCtx & tlvMod.FetchCtxFlag.TOP_LEVEL_NAV) !== 0, true);

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

    // 7. buildSafeResponseHeaders preserves Brotli Content-Encoding when stripContentEncoding=false & adds frame-ancestors!
    const brRes = fetchMod.buildSafeResponseHeaders(
      [
        { name: 'content-type', value: 'text/html; charset=utf-8' },
        { name: 'content-encoding', value: 'br' },
        { name: 'content-security-policy', value: "default-src 'self'" },
      ],
      { stripContentLength: false, stripContentEncoding: false, frameAncestorsOrigin: portalOrigin }
    );
    assert.equal(brRes.headers.get('content-encoding'), 'br', 'non-gzip Content-Encoding must not be stripped');
    assert.equal(brRes.headers.get('content-security-policy'), `frame-ancestors 'self' ${portalOrigin}`);
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

test('stream_bridge V7: pre-aborted Request.signal rejects with AbortError (no TDZ ReferenceError)', async () => {
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
      () => streamBridgeMod.executeTunnelRequest({}, reg, sess, req, '/api', [], false, 0, []),
      (err) => err && (err.name === 'AbortError' || /abort/i.test(err.message))
    );
  } finally {
    ch.port1.onmessage = null;
    ch.port1.close();
    ch.port2.close();
  }
});

test('TunnelBridge V7: duplicate/empty wsId still caps MAX_CONCURRENT_WS_STREAMS & outbound >16MiB WS closes with 1008', async () => {
  function makePair() {
    const mk = () => {
      const listeners = new Map();
      return {
        readyState: 'open',
        bufferedAmount: 0,
        bufferedAmountLowThreshold: 0,
        binaryType: 'arraybuffer',
        peer: null,
        addEventListener(type, fn) {
          let s = listeners.get(type);
          if (!s) {
            s = new Set();
            listeners.set(type, s);
          }
          s.add(fn);
        },
        removeEventListener(type, fn) {
          listeners.get(type)?.delete(fn);
        },
        _emit(type, ev) {
          for (const fn of Array.from(listeners.get(type) || [])) fn(ev);
        },
        send(data) {
          if (this.readyState !== 'open') throw new Error('closed');
          const cp = new Uint8Array(data).slice();
          const p = this.peer;
          if (p && p.readyState === 'open') {
            queueMicrotask(() => p._emit('message', { data: cp.buffer }));
          }
        },
        close() {
          this.readyState = 'closed';
          this._emit('close', {});
        },
      };
    };
    const a = mk();
    const b = mk();
    a.peer = b;
    b.peer = a;
    return [a, b];
  }

  const [cCtrl, sCtrl] = makePair();
  const [cSlot, sSlot] = makePair();
  const clientMux = new muxMod.PoolMux({}, 1);
  clientMux.attachControlDC(cCtrl);
  clientMux.attachPoolDC(0, cSlot);

  const serverWsDCs = [];
  const bridge = new portalBridgeMod.TunnelBridge();
  bridge.setContext(
    clientMux,
    () => {
      const [cWs, sWs] = makePair();
      serverWsDCs.push(sWs);
      sWs.addEventListener('message', (ev) => {
        const f = frameMod.decodeFrame(new Uint8Array(ev.data));
        if (f.type === frameMod.FrameType.HEADERS) {
          sWs.send(
            frameMod.encodeFrame(
              1,
              frameMod.FrameType.HEADERS,
              frameMod.FrameFlag.NONE,
              tlvMod.encodeResponseHeaders({ status: 101, headers: [] })
            )
          );
        }
      });
      return cWs;
    },
    'https://sbx-1.sandbox.example',
    { nodeId: 'demo', backendOrigin: 'http://app.local', isStaticMode: false, sameSiteMode: false },
    null
  );

  const openChannels = [];
  try {
    // 1. Open 64 WS streams with IDENTICAL empty wsId '' -> internal wsKeySeq still tracks all 64,
    //    so the 65th MUST fail with WS_ERROR (cannot bypass concurrency cap with empty/duplicate wsId)!
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
    // Use the first opened WS channel (whose peer never sends CREDIT so outbound queue builds up).
    const firstPort = openChannels[0].port1;
    const capCloseP = new Promise((resolve) => {
      firstPort.onmessage = (ev) => {
        if (ev.data?.type === 'WS_CLOSE') resolve(ev.data);
      };
    });
    // Wait for WS_OPEN handshake microtask then flood >16 MiB
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
    sSlot.close();
    for (const s of serverWsDCs) s.close();
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
    ctrl3.dispatchEvent(new MessageEvent('message', { data: goAwayBytes.buffer }));
    assert.equal(session.getPeerConnection(), null, 'GOAWAY must immediately tear down current PeerConnection');
    assert.equal(pc3.closed, true);

    // 4. P1-01: Once in-DTLS authentication succeeds, Portal MUST immediately close signaling WS
    //    so signaling never stays idle until server's 45s client_idle_timeout!
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
    ctrl4.dispatchEvent(new MessageEvent('message', { data: chalFrame.buffer }));
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
    ctrl4.dispatchEvent(new MessageEvent('message', { data: resFrame.buffer }));
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
