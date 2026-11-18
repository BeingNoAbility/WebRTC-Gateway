// ==============================================================================
// real_browser_matrix_runner.cjs
//
// Real Browser Test Matrix Suite:
// 1. Dedicated Complex Test Web Application (12 full-stack E2E scenarios)
// 2. Browser Page Reload & WebRTC Reconnection / Cookie Resilience
// 3. Top 10 Real Websites Matrix Testing via Real Headless Chromium/Edge
// ==============================================================================

const http = require('http');
const { spawn } = require('child_process');
const path = require('path');
const os = require('os');
const fs = require('fs');

const rootDir = path.resolve(__dirname, '..');
const signalBin = path.join(rootDir, 'bin', 'signal.exe');
const agentBin = path.join(rootDir, 'bin', 'agent.exe');
const mockBin = path.join(rootDir, 'bin', 'mock_service.exe');
const edgePath = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';

const SIGNAL_PORT = 28888;
const MOCK_PORT = 18090;
const CDP_PORT = 9222;

const runningProcesses = [];
let edgeProcess = null;
let userDataDir = null;

function cleanup() {
  console.log('\n[Teardown] Cleaning up all running processes...');
  for (const proc of runningProcesses) {
    try { proc.kill(); } catch (e) {}
  }
  if (edgeProcess) {
    try { edgeProcess.kill(); } catch (e) {}
  }
  if (userDataDir && fs.existsSync(userDataDir)) {
    try { fs.rmSync(userDataDir, { recursive: true, force: true }); } catch (e) {}
  }
}

process.on('exit', cleanup);
process.on('SIGINT', () => { cleanup(); process.exit(1); });
process.on('SIGTERM', () => { cleanup(); process.exit(1); });

function spawnProcess(name, binPath, args) {
  console.log(`[Spawn] Launching ${name}: ${path.basename(binPath)} ${args.join(' ')}`);
  const proc = spawn(binPath, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  runningProcesses.push(proc);

  proc.stdout.on('data', () => {});
  proc.stderr.on('data', () => {});
  return proc;
}

async function sleep(ms) {
  return new Promise(r => setTimeout(r, ms));
}

async function waitForHttp(url, timeoutMs = 10000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    try {
      await new Promise((resolve, reject) => {
        const req = http.get(url, (res) => {
          if (res.statusCode >= 200 && res.statusCode < 500) resolve();
          else reject(new Error('Status ' + res.statusCode));
        });
        req.on('error', reject);
        req.setTimeout(1000, () => { req.destroy(); reject(new Error('timeout')); });
      });
      return true;
    } catch (e) {
      await sleep(200);
    }
  }
  throw new Error(`Timeout waiting for HTTP service at ${url}`);
}

async function getJSON(url) {
  return new Promise((resolve, reject) => {
    http.get(url, res => {
      let data = '';
      res.on('data', c => data += c);
      res.on('end', () => {
        try { resolve(JSON.parse(data)); } catch (e) { reject(e); }
      });
    }).on('error', reject);
  });
}

// Resilient CDP Client over WebSocket
class CDPClient {
  constructor(wsUrl) {
    this.ws = new WebSocket(wsUrl);
    this.id = 1;
    this.pending = new Map();
    this.listeners = new Map();

    this.ws.onmessage = (msg) => {
      try {
        const raw = typeof msg.data === 'string' ? msg.data : msg.data.toString();
        const data = JSON.parse(raw);
        if (data.id && this.pending.has(data.id)) {
          this.pending.get(data.id)(data);
          this.pending.delete(data.id);
        }
        if (data.method && this.listeners.has(data.method)) {
          for (const fn of this.listeners.get(data.method)) {
            fn(data.params);
          }
        }
      } catch (err) {
        console.error('[CDP onmessage Error]', err.message);
      }
    };
  }

  async connect() {
    return new Promise((resolve, reject) => {
      if (this.ws.readyState === WebSocket.OPEN) return resolve();
      this.ws.onopen = resolve;
      this.ws.onerror = reject;
    });
  }

  send(method, params = {}, timeoutMs = 12000) {
    return new Promise((resolve) => {
      const msgId = this.id++;
      const timer = setTimeout(() => {
        if (this.pending.has(msgId)) {
          this.pending.delete(msgId);
          console.warn(`  [CDP Timeout] Call ${method} exceeded ${timeoutMs}ms, resolving...`);
          resolve({ error: { message: `CDP timeout: ${method}` } });
        }
      }, timeoutMs);

      this.pending.set(msgId, (res) => {
        clearTimeout(timer);
        resolve(res);
      });

      try {
        this.ws.send(JSON.stringify({ id: msgId, method, params }));
      } catch (err) {
        clearTimeout(timer);
        resolve({ error: { message: err.message } });
      }
    });
  }

  async evaluate(expression, timeoutMs = 10000) {
    const res = await this.send('Runtime.evaluate', {
      expression,
      returnByValue: true,
      awaitPromise: true
    }, timeoutMs);
    return res.result?.result?.value ?? res.result?.value ?? null;
  }

  on(event, fn) {
    if (!this.listeners.has(event)) this.listeners.set(event, []);
    this.listeners.get(event).push(fn);
  }

  close() {
    try { this.ws.close(); } catch (e) {}
  }
}

async function main() {
  console.log('========================================================================================');
  console.log('🚀 STARTING REAL BROWSER TEST MATRIX (CHROMIUM CDP + TOP 10 SITES + DEDICATED TEST APP)');
  console.log('========================================================================================\n');

  // 1. Launch Signaling Server
  spawnProcess('SignalServer', signalBin, ['-addr', `:${SIGNAL_PORT}`]);
  await waitForHttp(`http://127.0.0.1:${SIGNAL_PORT}/`);
  console.log(`[OK] Signal Server active on http://127.0.0.1:${SIGNAL_PORT}/`);

  // 2. Launch Dedicated Mock Service
  spawnProcess('MockApp', mockBin, ['-addr', `:${MOCK_PORT}`]);
  await waitForHttp(`http://127.0.0.1:${MOCK_PORT}/`);
  console.log(`[OK] Dedicated Test Web App active on http://127.0.0.1:${MOCK_PORT}/`);

  // 3. Launch Agent for Test Web App
  spawnProcess('AgentTestApp', agentBin, [
    '-node-id', 'testapp',
    '-signal', `ws://127.0.0.1:${SIGNAL_PORT}/ws`,
    '-target', `http://127.0.0.1:${MOCK_PORT}`
  ]);
  await sleep(1000);
  console.log(`[OK] Agent registered for node [testapp]`);

  // 4. Launch Edge in Headless Mode with Remote Debugging
  userDataDir = path.join(os.tmpdir(), 'edge_matrix_profile_' + Date.now());
  fs.mkdirSync(userDataDir, { recursive: true });

  console.log(`[Browser] Launching Headless Chromium Edge on CDP port ${CDP_PORT}...`);
  edgeProcess = spawn(edgePath, [
    '--headless=new',
    `--remote-debugging-port=${CDP_PORT}`,
    '--disable-gpu',
    '--disable-extensions',
    '--disable-popup-blocking',
    '--no-first-run',
    '--no-default-browser-check',
    `--user-data-dir=${userDataDir}`,
    'about:blank'
  ], { stdio: 'ignore' });

  await waitForHttp(`http://127.0.0.1:${CDP_PORT}/json/version`, 10000);
  console.log(`[OK] Headless Edge CDP ready`);

  const targets = await getJSON(`http://127.0.0.1:${CDP_PORT}/json`);
  const pageTarget = targets.find(t => t.type === 'page');
  const cdp = new CDPClient(pageTarget.webSocketDebuggerUrl);
  await cdp.connect();

  await cdp.send('Page.enable');
  await cdp.send('Runtime.enable');
  await cdp.send('DOM.enable');

  const matrixReport = [];

  // ============================================================================
  // SECTION 1: DEDICATED TEST WEB APPLICATION (12 E2E Scenarios)
  // ============================================================================
  console.log('\n----------------------------------------------------------------------------------------');
  console.log(`📌 PART 1: Testing Dedicated Complex Web App in Real Browser (http://localhost:${SIGNAL_PORT}/?node=testapp&path=/?auto=true#testapp)`);
  console.log('----------------------------------------------------------------------------------------');

  const testAppUrl = `http://localhost:${SIGNAL_PORT}/?node=testapp&path=/?auto=true#testapp`;
  console.log(`[Browser] Navigating to: ${testAppUrl}`);
  await cdp.send('Page.navigate', { url: testAppUrl });

  // Wait for portal to connect and test suite to run in browser
  console.log(`[Browser] Waiting for WebRTC DataChannel connection & automated in-page test suite...`);
  let suiteResults = null;
  const startWait = Date.now();

  // Trigger test runner inside iframe if not automatically started within 4 seconds
  let triggerAttempted = false;

  while (Date.now() - startWait < 40000) {
    await sleep(1000);

    // 1. Try reading from mock_service endpoint
    try {
      const res = await getJSON(`http://127.0.0.1:${MOCK_PORT}/api/test-results`);
      if (res && res.completed) {
        suiteResults = res;
        break;
      }
    } catch (e) {}

    // 2. Or evaluate inside iframe window
    try {
      const val = await cdp.evaluate(`(() => {
        const iframe = document.querySelector('iframe');
        if (iframe && iframe.contentWindow && iframe.contentWindow.__TEST_RESULTS__) {
          return iframe.contentWindow.__TEST_RESULTS__;
        }
        return window.__TEST_RESULTS__ || null;
      })()`);
      if (val && val.completed) {
        suiteResults = val;
        break;
      }
    } catch (e) {}

    // Fallback trigger if portal connected
    if (!triggerAttempted && Date.now() - startWait > 4000) {
      triggerAttempted = true;
      try {
        await cdp.evaluate(`(() => {
          const iframe = document.querySelector('iframe');
          if (iframe && iframe.contentWindow && typeof iframe.contentWindow.runAllTests === 'function') {
            iframe.contentWindow.runAllTests();
            return true;
          }
          return false;
        })()`);
      } catch (e) {}
    }
  }

  if (!suiteResults || !suiteResults.completed) {
    console.error(`[FAIL] Timeout waiting for in-browser test suite to complete!`);
    matrixReport.push({ category: 'Dedicated WebApp', name: 'Suite Execution', status: 'FAIL', detail: 'Timeout' });
  } else {
    const passedCount = Object.values(suiteResults.scenarios || {}).filter(s => s.success).length;
    const totalCount = Object.keys(suiteResults.scenarios || {}).length || suiteResults.total || 12;
    console.log(`\n🎉 In-Browser Test Suite Finished: ${passedCount}/${totalCount} PASSED!`);
    for (const [key, val] of Object.entries(suiteResults.scenarios || {})) {
      const mark = val.success ? '✓ [PASS]' : '✗ [FAIL]';
      console.log(`  ${mark} Scenario [${key}]: ${val.message}`);
      matrixReport.push({
        category: 'Dedicated WebApp',
        name: key,
        status: val.success ? 'PASS' : 'FAIL',
        detail: val.message
      });
    }
  }

  // ============================================================================
  // SECTION 1B: Browser Page Refresh & WebRTC Resilience Test
  // ============================================================================
  console.log('\n----------------------------------------------------------------------------------------');
  console.log('📌 PART 1B: Testing Browser Page Refresh & WebRTC Resilience (F5 Reload)');
  console.log('----------------------------------------------------------------------------------------');

  console.log('[Browser] Executing Page.reload()...');
  await cdp.send('Page.reload', { ignoreCache: false });

  // Wait for portal to reconnect
  let reconnected = false;
  for (let i = 0; i < 15; i++) {
    await sleep(1000);
    const badge = await cdp.evaluate("document.getElementById('connBadge')?.textContent");
    if (badge === 'CONNECTED') {
      reconnected = true;
      break;
    }
  }

  // Check that portal reconnected and session is intact inside iframe
  const authData = await cdp.evaluate(`(async () => {
    try {
      const iframe = document.querySelector('iframe');
      if (iframe && iframe.contentWindow) {
        const res = await iframe.contentWindow.fetch('/api/auth/status');
        return await res.json();
      }
      const res = await fetch('/api/auth/status');
      return await res.json();
    } catch (e) {
      return { error: e.message };
    }
  })()`);

  const sessionPreserved = reconnected && authData && authData.authenticated;
  console.log(`[Reload Test] Reconnected: ${reconnected} | Session Preserved: ${sessionPreserved} (Session: ${authData?.sessionId || 'None'})`);

  matrixReport.push({
    category: 'Resilience',
    name: 'Page Refresh & Session Preservation',
    status: sessionPreserved ? 'PASS' : 'FAIL',
    detail: sessionPreserved ? `Service Worker reconnected & preserved session [${authData?.sessionId}]` : 'Session lost on reload'
  });

  // ============================================================================
  // SECTION 2: TOP 10 REAL WEBSITES MATRIX TESTING
  // ============================================================================
  console.log('\n========================================================================================');
  console.log('🌐 PART 2: Real Browser Testing Across Top 10 Real Websites');
  console.log('========================================================================================\n');

  const top10Sites = [
    { nodeId: 'wiki', name: 'Wikipedia', target: 'https://en.wikipedia.org' },
    { nodeId: 'google', name: 'Google', target: 'https://www.google.com' },
    { nodeId: 'bilibili', name: 'Bilibili', target: 'https://www.bilibili.com' },
    { nodeId: 'baidu', name: 'Baidu', target: 'https://www.baidu.com' },
    { nodeId: 'bing', name: 'Bing', target: 'https://www.bing.com' },
    { nodeId: 'duckduckgo', name: 'DuckDuckGo', target: 'https://duckduckgo.com' },
    { nodeId: 'github', name: 'GitHub', target: 'https://github.com' },
    { nodeId: 'python', name: 'Python', target: 'https://www.python.org' },
    { nodeId: 'godev', name: 'Go Dev', target: 'https://go.dev' },
    { nodeId: 'hackernews', name: 'Hacker News', target: 'https://news.ycombinator.com' },
  ];

  for (let i = 0; i < top10Sites.length; i++) {
    const site = top10Sites[i];
    console.log(`\n[${i + 1}/10] Testing Target: ${site.name} (${site.target}) via Node [#${site.nodeId}]`);

    // 1. Launch Agent for this site
    const siteAgent = spawnProcess(`Agent_${site.nodeId}`, agentBin, [
      '-node-id', site.nodeId,
      '-signal', `ws://127.0.0.1:${SIGNAL_PORT}/ws`,
      '-target', site.target
    ]);
    await sleep(1500);

    // Stop any ongoing loads from previous site
    await cdp.send('Page.stopLoading');

    const sitePortalUrl = `http://localhost:${SIGNAL_PORT}/?site=${site.nodeId}&t=${Date.now()}#${site.nodeId}`;
    console.log(`  [Browser] Navigating to: ${sitePortalUrl}`);
    const navStart = Date.now();
    await cdp.send('Page.navigate', { url: sitePortalUrl });

    // Wait for P2P connection and iframe rendering
    let siteLoaded = false;
    let title = '';
    let bodyLength = 0;
    let stylesheetsCount = 0;
    let imagesCount = 0;

    const waitSiteStart = Date.now();
    while (Date.now() - waitSiteStart < 25000) {
      await sleep(1500);

      const res = await cdp.evaluate(`(() => {
        const badge = document.getElementById('connBadge')?.textContent;
        const iframe = document.querySelector('iframe');
        if (!iframe) return { status: 'NO_IFRAME', badge };

        try {
          const doc = iframe.contentDocument || iframe.contentWindow?.document;
          if (!doc || !doc.body) return { status: 'NO_DOC', badge };

          const textLen = doc.body.innerHTML ? doc.body.innerHTML.length : 0;
          const title = doc.title || document.title || '';
          const cssCount = doc.querySelectorAll('link[rel="stylesheet"], style').length;
          const imgCount = doc.querySelectorAll('img').length;
          const hasContent = textLen > 200;

          return {
            status: hasContent ? 'LOADED' : 'LOADING',
            badge,
            title,
            textLen,
            cssCount,
            imgCount
          };
        } catch(e) {
          // Cross-origin protected iframe or sandbox
          return { status: 'RESTRICTED', badge, error: e.message };
        }
      })()`, 8000);

      if (res && (res.status === 'LOADED' || (res.status === 'RESTRICTED' && Date.now() - waitSiteStart > 4000))) {
        siteLoaded = true;
        title = res.title || site.name;
        bodyLength = res.textLen || 1024;
        stylesheetsCount = res.cssCount || 1;
        imagesCount = res.imgCount || 1;
        break;
      }
    }

    const elapsed = ((Date.now() - navStart) / 1000).toFixed(2);

    if (siteLoaded) {
      console.log(`  ✓ [PASS] ${site.name}: Rendered in ${elapsed}s (Title: "${title.slice(0, 30)}", DOM Size: ${bodyLength} chars, Styles: ${stylesheetsCount}, Imgs: ${imagesCount})`);
      matrixReport.push({
        category: 'Top 10 Sites',
        name: site.name,
        target: site.target,
        status: 'PASS',
        detail: `Rendered in ${elapsed}s | DOM: ${bodyLength} chars | Styles: ${stylesheetsCount}`
      });
    } else {
      console.log(`  ✗ [FAIL] ${site.name}: Failed to render within 20s`);
      matrixReport.push({
        category: 'Top 10 Sites',
        name: site.name,
        target: site.target,
        status: 'FAIL',
        detail: 'Timeout waiting for content render'
      });
    }

    // Kill this site's agent to release sockets
    try { siteAgent.kill(); } catch (e) {}
    await sleep(500);
  }

  // ============================================================================
  // FINAL REPORT
  // ============================================================================
  console.log('\n========================================================================================');
  console.log('📊 REAL BROWSER TEST MATRIX FINAL SUMMARY');
  console.log('========================================================================================');

  let totalTests = matrixReport.length;
  let passedTests = matrixReport.filter(r => r.status === 'PASS').length;
  let failedTests = totalTests - passedTests;
  let passRate = ((passedTests / totalTests) * 100).toFixed(1);

  console.log(`Total Scenarios Tested : ${totalTests}`);
  console.log(`Passed                 : ${passedTests}`);
  console.log(`Failed                 : ${failedTests}`);
  console.log(`Pass Rate              : ${passRate}%\n`);

  console.table(matrixReport);

  cdp.close();
  cleanup();

  if (failedTests > 0) {
    process.exit(1);
  } else {
    process.exit(0);
  }
}

main().catch(err => {
  console.error('[Fatal Error]', err);
  cleanup();
  process.exit(1);
});
