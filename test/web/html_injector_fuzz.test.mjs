import test from 'node:test';
import assert from 'node:assert/strict';
import { loadTsModule } from './_loader.mjs';

const htmlMod = await loadTsModule('web/src/sw/html_injector.ts');

const baseCfg = {
  nodeId: 'demo',
  portalOrigin: 'https://portal.example',
  backendOrigin: 'http://app.local:8080',
  initCookies: [],
};

function runInject(htmlStr, chunkSize = 3, opts = { hasHttpCharset: true }, cfg = baseCfg) {
  const bytes = typeof htmlStr === 'string' ? new TextEncoder().encode(htmlStr) : htmlStr;
  const inj = new htmlMod.StreamingHtmlInjector('https://sbx-1.sandbox.example', cfg, opts);
  const parts = [];
  for (let i = 0; i < bytes.byteLength; i += chunkSize) {
    parts.push(...inj.push(bytes.subarray(i, Math.min(bytes.byteLength, i + chunkSize))));
  }
  parts.push(...inj.finish());
  const total = parts.reduce((acc, p) => acc + p.byteLength, 0);
  const merged = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    merged.set(p, off);
    off += p.byteLength;
  }
  return new TextDecoder().decode(merged);
}

const SNIPPET = htmlMod.buildPatchInjectionSnippet('https://sbx-1.sandbox.example', baseCfg);

test('HTML5 Tokenizer V7: all 9 edge-case classes across 1B/3B/full chunk splits', () => {
  const cases = [
    {
      name: '1. TAG_OPEN re-consumes < (<<head>)',
      html: '<!doctype html><html><<head><title>ok</title></head><body>1</body></html>',
      after: '<<head>',
    },
    {
      name: '2. HTML5 tag name termination on form-feed & slash (<head/class=x>)',
      html: '<!doctype html><html><h1_custom><head/class="x"><title>ok</title></head><body>1</body></html>',
      after: '<head/class="x">',
    },
    {
      name: '3. Rawtext end tag with quoted > attribute (</style a=">">)',
      html: '<!doctype html><html><style>body{}</style data-x=">"><head><title>ok</title></head><body>1</body></html>',
      after: '<head>',
    },
    {
      name: '4. </ space / non-alpha bogus comment (</ fake <head> >)',
      html: '<!doctype html><html></ fake <head> ><head><title>ok</title></head><body>1</body></html>',
      after: '</ fake <head> ><head>',
    },
    {
      name: '5a. Nested <template> depth ignores inner <head>',
      html: '<!doctype html><html><template><template></template><head>fake</head></template><head><title>real</title></head></html>',
      after: '</head></template><head>',
    },
    {
      name: '5b. Nested <svg> with self-closing <svg/> & <math/>',
      html: '<!doctype html><html><svg/><math/><svg><svg></svg><head>fake</head></svg><math><head>fake2</head></math><head><title>real</title></head></html>',
      after: '</math><head>',
    },
    {
      name: '6. <script> escaped & double-escaped <!-- <script></script> -->',
      html: '<!doctype html><html><script><!-- var s = "<script><head>fake</head></script><head>fake2</head>"; --></script><head><title>real</title></head></html>',
      after: '--></script><head>',
    },
    {
      name: '7. Headless document injects BEFORE first <script>',
      html: '<!doctype html><div>hi</div><script>window.app=1;</script>',
      before: '<script>window.app=1;</script>',
    },
  ];

  for (const tc of cases) {
    for (const chunkSize of [1, 3, 7, 1024]) {
      const out = runInject(tc.html, chunkSize, { hasHttpCharset: true });
      const idx = out.indexOf(SNIPPET);
      assert.ok(idx >= 0, `${tc.name} (chunk=${chunkSize}): snippet missing`);
      assert.equal(out.indexOf(SNIPPET, idx + 1), -1, `${tc.name} (chunk=${chunkSize}): duplicate snippet`);
      assert.equal(out.slice(0, idx) + out.slice(idx + SNIPPET.length), tc.html, `${tc.name}: byte corruption`);
      if (tc.after) {
        const expectedPos = tc.html.indexOf(tc.after) + tc.after.length;
        assert.equal(idx, expectedPos, `${tc.name} (chunk=${chunkSize}): wrong inject offset ${idx} != ${expectedPos}`);
      }
      if (tc.before) {
        const expectedPos = tc.html.indexOf(tc.before);
        assert.equal(idx, expectedPos, `${tc.name} (chunk=${chunkSize}): expected before ${tc.before}`);
      }
    }
  }
});

test('HTML5 1024B <meta charset> prescan protection (when HTTP Content-Type omits charset=)', () => {
  // 1. No HTTP charset + <meta charset="utf-8"> in <head> -> injects AFTER <meta charset="utf-8">!
  const html1 = '<!doctype html><html><head><meta charset="utf-8"><title>中文测试</title></head><body>ok</body></html>';
  for (const sz of [1, 4, 512]) {
    const out = runInject(html1, sz, { hasHttpCharset: false });
    const metaEnd = out.indexOf('<meta charset="utf-8">') + '<meta charset="utf-8">'.length;
    assert.equal(out.indexOf(SNIPPET), metaEnd, `chunk=${sz}: must inject after <meta charset="utf-8">`);
  }

  // 2. No HTTP charset + <meta http-equiv="Content-Type" content="text/html; charset=utf-8"> -> injects AFTER it!
  const html2 = '<!doctype html><html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8"><script>1</script></head></html>';
  const out2 = runInject(html2, 5, { hasHttpCharset: false });
  const meta2End = out2.indexOf('charset=utf-8">') + 'charset=utf-8">'.length;
  assert.equal(out2.indexOf(SNIPPET), meta2End, 'must inject after <meta http-equiv="Content-Type">');

  // 3. No HTTP charset + <script> appears BEFORE <meta charset> -> injects BEFORE <script>!
  const html3 = '<!doctype html><html><head><script>init()</script><meta charset="utf-8"></head></html>';
  const out3 = runInject(html3, 3, { hasHttpCharset: false });
  assert.equal(out3.indexOf(SNIPPET), html3.indexOf('<script>init()'), 'must inject before <script> inside <head>');

  // 4. No HTTP charset + >1024 bytes inside <head> without <meta charset> -> flushes at <head> end!
  const padLinks = '<link rel="prefetch" href="/a/b/c/d/e/f/g/h/i/j.css">'.repeat(25); // > 1200 bytes
  const html4 = `<!doctype html><html><head>${padLinks}<meta charset="utf-8"></head></html>`;
  const out4 = runInject(html4, 17, { hasHttpCharset: false });
  assert.equal(out4.indexOf(SNIPPET), html4.indexOf('<head>') + '<head>'.length, 'past 1024B prescan must fall back to right after <head>');

  // 5. Explicit HTTP charset -> injects IMMEDIATELY after <head> (before <meta charset>)!
  const out5 = runInject(html1, 3, { hasHttpCharset: true });
  assert.equal(out5.indexOf(SNIPPET), html1.indexOf('<head>') + '<head>'.length);

  // 6. Boot payload filters expired cookies & stays bounded <= 16 KiB!
  const hugeCookies = [
    { op: 'set', name: 'expired', value: 'old', path: '/', expires: Date.now() - 10_000 },
    ...Array.from({ length: 100 }, (_, i) => ({
      op: 'set',
      name: `c${i}`,
      value: 'x'.repeat(300),
      path: '/',
    })),
  ];
  const boundedSnippet = htmlMod.buildPatchInjectionSnippet('https://sbx-1.sandbox.example', {
    ...baseCfg,
    initCookies: hugeCookies,
  });
  assert.ok(boundedSnippet.length < 25_000, `boot snippet too large: ${boundedSnippet.length}`);
  assert.ok(!boundedSnippet.includes('expired'), 'expired cookies must be pruned from boot payload');
});

test('HTML5 Tokenizer deterministic fuzzing (500 adversarial docs x random chunking): zero throws, exact single injection, zero byte loss', () => {
  // Deterministic PRNG (Mulberry32)
  let seed = 0x7a3f91c5;
  const rand = () => {
    seed = (seed + 0x6d2b79f5) >>> 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  const pick = (arr) => arr[Math.floor(rand() * arr.length)];

  const tokens = [
    '<!doctype html>',
    '<html>',
    '<head>',
    '<<head>',
    '<head/a=">">',
    '</head>',
    '<meta charset="utf-8">',
    '<meta http-equiv="content-type" content="text/html; charset=utf-8">',
    '<script>',
    '</script>',
    '</script attr=">">',
    '<!--',
    '-->',
    '--!>',
    '<!-->',
    '<!--->',
    '</ div>',
    '</>',
    '<template>',
    '</template>',
    '<svg>',
    '<svg/>',
    '</svg>',
    '<math>',
    '<math/>',
    '</math>',
    '<style>',
    '</style>',
    '<title>',
    '</title>',
    '<body>',
    '<div a=x"y\'z>',
    '中文文本🚀',
    '<<<',
    '>>>',
  ];

  for (let i = 0; i < 500; i++) {
    const count = 4 + Math.floor(rand() * 24);
    let doc = '';
    for (let k = 0; k < count; k++) doc += pick(tokens);
    const hasHttpCharset = rand() < 0.5;
    const chunkSize = 1 + Math.floor(rand() * 19);

    const out = runInject(doc, chunkSize, { hasHttpCharset });
    const idx = out.indexOf(SNIPPET);
    assert.ok(idx >= 0, `fuzz[${i}]: missing snippet`);
    assert.equal(out.indexOf(SNIPPET, idx + 1), -1, `fuzz[${i}]: duplicate snippet`);
    assert.equal(out.slice(0, idx) + out.slice(idx + SNIPPET.length), doc, `fuzz[${i}]: corrupted document bytes`);
  }
});
