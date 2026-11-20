#!/usr/bin/env node
/**
 * Release Hygiene & Version Parity Machine Gate
 *
 * Enforces strict consistency across all version declarations, release notes,
 * and changelog entries. Prevents human omission of checklist tasks.
 *
 * Rules:
 * 1. Base version defined in web/package.json must match docs/package.json.
 * 2. Go binaries cmd/agent/main.go and cmd/signal/main.go must match vX.Y.Z.
 * 3. Web portal app.ts portalVer must match vX.Y.Z.
 * 4. docs/RELEASE_NOTES.md must contain EXACTLY ONE version header matching vX.Y.Z.
 *    Historical versions must NOT be present in RELEASE_NOTES.md (they belong in CHANGELOG.md).
 * 5. docs/RELEASE_NOTES.md Docker pull commands must reference :vX.Y.Z.
 * 6. CHANGELOG.md must contain an entry for ## [vX.Y.Z].
 */

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const rootDir = path.resolve(__dirname, '..');

let errors = [];

function check(assertion, message) {
  if (!assertion) {
    errors.push(message);
    console.error(`❌ FAIL: ${message}`);
  } else {
    console.log(`✅ PASS: ${message}`);
  }
}

console.log('🔍 Running Release Hygiene & Version Parity Machine Gate...\n');

// 1. Read base version from web/package.json
const webPkgPath = path.join(rootDir, 'web', 'package.json');
const webPkg = JSON.parse(fs.readFileSync(webPkgPath, 'utf8'));
const baseVersion = webPkg.version;
const expectedTag = `v${baseVersion}`;
console.log(`📌 Target Project Version: ${baseVersion} (${expectedTag})\n`);

// 2. Check docs/package.json
const docsPkgPath = path.join(rootDir, 'docs', 'package.json');
const docsPkg = JSON.parse(fs.readFileSync(docsPkgPath, 'utf8'));
check(
  docsPkg.version === baseVersion,
  `docs/package.json version (${docsPkg.version}) matches web/package.json (${baseVersion})`
);

// 3. Check cmd/agent/main.go
const agentMainPath = path.join(rootDir, 'cmd', 'agent', 'main.go');
const agentMain = fs.readFileSync(agentMainPath, 'utf8');
const agentVerMatch = agentMain.match(/var\s+Version\s*=\s*"([^"]+)"/);
check(
  agentVerMatch && agentVerMatch[1] === expectedTag,
  `cmd/agent/main.go Version (${agentVerMatch ? agentVerMatch[1] : 'null'}) matches ${expectedTag}`
);

// 4. Check cmd/signal/main.go
const signalMainPath = path.join(rootDir, 'cmd', 'signal', 'main.go');
const signalMain = fs.readFileSync(signalMainPath, 'utf8');
const signalVerMatch = signalMain.match(/var\s+Version\s*=\s*"([^"]+)"/);
check(
  signalVerMatch && signalVerMatch[1] === expectedTag,
  `cmd/signal/main.go Version (${signalVerMatch ? signalVerMatch[1] : 'null'}) matches ${expectedTag}`
);

// 5. Check web/src/portal/app.ts portalVer
const portalAppPath = path.join(rootDir, 'web', 'src', 'portal', 'app.ts');
const portalApp = fs.readFileSync(portalAppPath, 'utf8');
const portalVerMatch = portalApp.match(/portalVer\s*=\s*['"]([^'"]+)['"]/);
check(
  portalVerMatch && portalVerMatch[1] === expectedTag,
  `web/src/portal/app.ts portalVer (${portalVerMatch ? portalVerMatch[1] : 'null'}) matches ${expectedTag}`
);

// 6. Check docs/RELEASE_NOTES.md
const releaseNotesPath = path.join(rootDir, 'docs', 'RELEASE_NOTES.md');
const releaseNotes = fs.readFileSync(releaseNotesPath, 'utf8');
const releaseSectionMatches = [...releaseNotes.matchAll(/^##\s+\[(v[^\]]+)\]/gm)];

check(
  releaseSectionMatches.length === 1,
  `docs/RELEASE_NOTES.md contains EXACTLY ONE release section (found: ${releaseSectionMatches.length}: ${releaseSectionMatches.map(m => m[1]).join(', ')})`
);

if (releaseSectionMatches.length > 0) {
  const noteVersion = releaseSectionMatches[0][1];
  check(
    noteVersion === expectedTag,
    `docs/RELEASE_NOTES.md release version (${noteVersion}) matches ${expectedTag}`
  );
}

// 7. Check Docker pull tags in docs/RELEASE_NOTES.md
const dockerAgentMatch = releaseNotes.includes(`ghcr.io/beingnoability/webrtc-p2p-agent:${expectedTag}`);
const dockerServerMatch = releaseNotes.includes(`ghcr.io/beingnoability/webrtc-p2p-server:${expectedTag}`);
check(
  dockerAgentMatch && dockerServerMatch,
  `docs/RELEASE_NOTES.md references correct docker pull tags for ${expectedTag}`
);

// 8. Check CHANGELOG.md contains expected version entry
const changelogPath = path.join(rootDir, 'CHANGELOG.md');
const changelog = fs.readFileSync(changelogPath, 'utf8');
const changelogHasVersion = changelog.includes(`## [${expectedTag}]`);
check(
  changelogHasVersion,
  `CHANGELOG.md contains historical entry for ## [${expectedTag}]`
);

// 9. Privacy & Anonymization Audit (Zero Private Data Invariant)
const decodeB64 = (s) => Buffer.from(s, 'base64').toString('utf8');
const sensitivePatterns = [
  { name: 'Private domain', pattern: new RegExp(decodeB64('bG9saVwuZm8='), 'i') },
  { name: 'Private IP prefix', pattern: new RegExp(decodeB64('MjdcLjQwXC4='), 'i') },
  { name: 'Private node identifier', pattern: new RegExp(decodeB64('c3lubzE2OA=='), 'i') },
  { name: 'Private instance IP pattern 1', pattern: new RegExp(decodeB64('MTkyXC4xNjhcLjFcLjY1'), 'i') },
  { name: 'Private instance IP pattern 2', pattern: new RegExp(decodeB64('MTkyXC4xNjhcLjlcLjQw'), 'i') },
  { name: 'Private instance IP pattern 3', pattern: new RegExp(decodeB64('MTkyXC4xNjhcLjFcLjIyMg=='), 'i') },
  { name: 'Private instance IP pattern 4', pattern: new RegExp(decodeB64('MTkyXC4xNjhcLjFcLjM0'), 'i') },
  { name: 'Private instance IP pattern 5', pattern: new RegExp(decodeB64('MTkyXC4xNjhcLjFcLjk5'), 'i') },
  { name: 'Private custom port pattern', pattern: new RegExp(decodeB64('MjIzNTU='), 'i') },
  { name: 'Private domain pattern 2', pattern: new RegExp(decodeB64('bHljb3Jpc3JlY29pbA=='), 'i') },
];

let leakCount = 0;
const filesToScan = [
  'cmd/signal/main.go',
  'cmd/signal/broker_test.go',
  'cmd/agent/main.go',
  'web/src/portal/app.ts',
  'web/wrangler.toml',
  '.github/workflows/release.yml',
  '.github/workflows/deploy-testing.yml',
  'docs/RELEASE_NOTES.md',
  'CHANGELOG.md',
  'configs/server.example.yaml',
];

for (const relFile of filesToScan) {
  const fullPath = path.join(rootDir, relFile);
  if (fs.existsSync(fullPath)) {
    const content = fs.readFileSync(fullPath, 'utf8');
    for (const { name, pattern } of sensitivePatterns) {
      if (pattern.test(content)) {
        errors.push(`Privacy leak detected: ${name} matched pattern ${pattern} in ${relFile}`);
        leakCount++;
      }
    }
  }
}
check(leakCount === 0, `Zero Private Data Invariant: No sensitive personal domains/IPs/nodes in core files`);

console.log('\n-------------------------------------------------------------');
if (errors.length > 0) {
  console.error(`💥 Release hygiene verification FAILED with ${errors.length} error(s).`);
  process.exit(1);
} else {
  console.log(`✨ All release hygiene and version parity checks PASSED with 100% precision!`);
  process.exit(0);
}
