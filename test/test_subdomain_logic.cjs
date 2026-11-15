function extractNodeIdFromHostname(hostname) {
  if (!hostname) return null;
  const host = hostname.toLowerCase().split(':')[0];
  if (/^(\d{1,3}\.){3}\d{1,3}$/.test(host) || host === 'localhost') return null;

  const reserved = new Set([
    'p2p', 'www', 'webrtc', 'webrtc-p2p-signal', 'signal', 'gateway', 'portal', 'api', 'admin'
  ]);

  // Pattern 1: Node prefix before ".p2p." (e.g. "bili.p2p.example.com" or "nas.p2p.com")
  const p2pIndex = host.indexOf('.p2p.');
  if (p2pIndex > 0) {
    const sub = host.substring(0, p2pIndex);
    const parts = sub.split('.');
    const candidate = parts[parts.length - 1];
    if (candidate && !reserved.has(candidate)) {
      return candidate;
    }
  }

  // Pattern 2: Multi-level subdomain (e.g. "bili.example.com", "nas.home.net", "bili.localhost")
  const parts = host.split('.');
  if (parts.length >= 2) {
    const firstLabel = parts[0];
    if (firstLabel && !reserved.has(firstLabel)) {
      if (parts[parts.length - 1] === 'localhost' || parts.length >= 3) {
        return firstLabel;
      }
    }
  }

  return null;
}

const tests = [
  { host: 'bili.p2p.example.com', expected: 'bili' },
  { host: 'speedtest.p2p.example.com', expected: 'speedtest' },
  { host: 'vaultwarden.p2p.mydomain.com', expected: 'vaultwarden' },
  { host: 'immich.example.com', expected: 'immich' },
  { host: 'jellyfin.myhome.net', expected: 'jellyfin' },
  { host: 'bili.localhost:8888', expected: 'bili' },
  { host: 'p2p.example.com', expected: null },
  { host: 'webrtc-p2p-signal.example.com', expected: null },
  { host: 'www.example.com', expected: null },
  { host: '192.168.1.100', expected: null },
  { host: 'localhost:8888', expected: null }
];

let allPass = true;
for (const t of tests) {
  const result = extractNodeIdFromHostname(t.host);
  const pass = result === t.expected;
  if (!pass) allPass = false;
  console.log(`[${pass ? 'PASS' : 'FAIL'}] host: ${t.host.padEnd(38)} -> ${result} (expected: ${t.expected})`);
}
console.log('All tests passed:', allPass);
