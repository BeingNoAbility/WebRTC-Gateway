#!/usr/bin/env node
/**
 * Release Hygiene & Version Parity Machine Gate (V7)
 *
 * Single Source of Truth: root `VERSION` file (X.Y.Z or X.Y.Z-alpha.N / -beta.N / -rc.N).
 * Go binaries receive `main.Version=v$(cat VERSION)` via `-ldflags`, and Web builds
 * read `VERSION` at build time — eliminating brittle multi-file regex version synchronization.
 *
 * Usage:
 *   node scripts/verify-release-hygiene.mjs [--check-tag vX.Y.Z]
 */

import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const rootDir = process.env.HYGIENE_ROOT_DIR
  ? path.resolve(process.env.HYGIENE_ROOT_DIR)
  : path.resolve(__dirname, '..');

const SEMVER_RE = /^[0-9]+\.[0-9]+\.[0-9]+(?:-(?:alpha|beta|rc)\.[0-9]+)?$/;

let checkTag = null;
for (let i = 2; i < process.argv.length; i++) {
  if (process.argv[i] === '--check-tag' && i + 1 < process.argv.length) {
    checkTag = process.argv[++i];
  }
}

const errors = [];
function check(assertion, message) {
  if (!assertion) {
    errors.push(message);
    console.error(`FAIL: ${message}`);
  } else {
    console.log(`PASS: ${message}`);
  }
}

console.log('Running Release Hygiene & Version Parity Machine Gate (V7)...\n');

// 1. Read Single Source of Truth VERSION file
const versionPath = path.join(rootDir, 'VERSION');
check(fs.existsSync(versionPath), 'Root VERSION file exists');
const baseVersion = fs.existsSync(versionPath)
  ? fs.readFileSync(versionPath, 'utf8').trim()
  : '';
check(SEMVER_RE.test(baseVersion), `VERSION (${baseVersion}) is valid semver`);
const expectedTag = `v${baseVersion}`;

if (checkTag !== null) {
  check(
    checkTag === expectedTag,
    `Requested tag (${checkTag}) matches VERSION (${expectedTag})`
  );
}

// 2. Check package manifests align with VERSION
for (const relPkg of ['web/package.json', 'docs/package.json']) {
  const full = path.join(rootDir, relPkg);
  if (!fs.existsSync(full)) continue;
  const pkg = JSON.parse(fs.readFileSync(full, 'utf8'));
  check(
    pkg.version === baseVersion,
    `${relPkg} version (${pkg.version}) matches VERSION (${baseVersion})`
  );
}

for (const relLock of ['web/package-lock.json', 'docs/package-lock.json']) {
  const full = path.join(rootDir, relLock);
  if (!fs.existsSync(full)) continue;
  const lock = JSON.parse(fs.readFileSync(full, 'utf8'));
  const rootEntryVer = lock.packages?.['']?.version;
  check(
    lock.version === baseVersion && (!rootEntryVer || rootEntryVer === baseVersion),
    `${relLock} version (${lock.version}) matches VERSION (${baseVersion})`
  );
}

// 3. Enforce single-version docs/RELEASE_NOTES.md & CHANGELOG.md parity against VERSION
const releaseNotesPath = path.join(rootDir, 'docs', 'RELEASE_NOTES.md');
if (fs.existsSync(releaseNotesPath)) {
  const releaseNotes = fs.readFileSync(releaseNotesPath, 'utf8');
  const matches = [...releaseNotes.matchAll(/^##\s+\[(v[^\]]+)\]/gm)];
  check(
    matches.length === 1,
    `docs/RELEASE_NOTES.md contains exactly one release section (found ${matches.length})`
  );
  if (matches.length === 1) {
    check(
      matches[0][1] === expectedTag,
      `docs/RELEASE_NOTES.md header (${matches[0][1]}) matches ${expectedTag}`
    );
  }
  const hasAgentTag = releaseNotes.includes(`webrtc-p2p-agent:${expectedTag}`);
  const hasServerTag = releaseNotes.includes(`webrtc-p2p-server:${expectedTag}`);
  check(
    hasAgentTag && hasServerTag,
    `docs/RELEASE_NOTES.md references Docker pull tags for ${expectedTag}`
  );
}

const changelogPath = path.join(rootDir, 'CHANGELOG.md');
if (fs.existsSync(changelogPath)) {
  const changelog = fs.readFileSync(changelogPath, 'utf8');
  check(
    changelog.includes(`## [${expectedTag}]`),
    `CHANGELOG.md contains entry for ## [${expectedTag}]`
  );
}

// 4. Verify web/dist is NOT tracked in Git and no .env file is tracked/unignored
if (!process.env.HYGIENE_ROOT_DIR) {
  try {
    const trackedDist = execFileSync('git', ['ls-files', 'web/dist'], { cwd: rootDir })
      .toString('utf8')
      .trim();
    check(trackedDist === '', 'web/dist/ has zero git-tracked build artifacts');

    const trackedEnv = execFileSync(
      'git',
      ['ls-files', '--cached', '--others', '--exclude-standard', '.env', '.env.*'],
      { cwd: rootDir }
    )
      .toString('utf8')
      .split('\n')
      .map((s) => s.trim())
      .filter((s) => s && s !== '.env.example');
    check(trackedEnv.length === 0, 'No .env secret files are tracked or unignored');

    const trackedLocalDenylist = execFileSync(
      'git',
      ['ls-files', '--cached', '--others', '--exclude-standard', '.hygiene-private-patterns.local'],
      { cwd: rootDir }
    )
      .toString('utf8')
      .trim();
    check(
      trackedLocalDenylist === '',
      'Local private denylist (.hygiene-private-patterns.local) is never tracked or unignored'
    );
  } catch {
    // Non-git test directory
  }
}

// 5. Privacy & Credential Hygiene Scan (Zero Private Data Invariant)
// Generic structural patterns only — NEVER hardcode personal IPs/domains/ports/usernames or
// unsalted hashes here. For local pre-push guarding of personal identifiers, create a gitignored
// `.hygiene-private-patterns.local` file in the repo root (one literal token per line).
const sensitivePatterns = [
  { name: 'Local machine drive path', pattern: /file:\/\/\/[a-z]:\//i },
  { name: 'Local Linux home directory path', pattern: /(?:\\\/|\/)home(?:\\\/|\/)[a-zA-Z0-9_-]+(?:\\\/|\/|\b)/ },
  { name: 'Local macOS home directory path', pattern: /(?:\\\/|\/)Users(?:\\\/|\/)[a-zA-Z0-9_-]+(?:\\\/|\/|\b)/i },
  { name: 'Local Windows user directory path', pattern: /[A-Za-z]:\\+[Uu]sers\\+[a-zA-Z0-9_-]+/i },
  { name: 'Third-party brand leak (stun/cdn host)', pattern: /\b(?:stun\.bilibili\.com|hdslb\.com)\b/i },
  { name: 'Private cryptographic key block', pattern: /-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ED25519 )?PRIVATE KEY-----/ },
  { name: 'AWS Access Key ID', pattern: /\bAKIA[0-9A-Z]{16}\b/ },
  { name: 'GitHub token (classic/fine-grained)', pattern: /\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b/ },
  { name: 'Google API key', pattern: /\bAIza[0-9A-Za-z\-_]{30,}\b/ },
];

const LOCAL_DENYLIST_FILENAME = '.hygiene-private-patterns.local';
function loadLocalPrivateDenylist() {
  const denyPath = path.join(rootDir, LOCAL_DENYLIST_FILENAME);
  if (!fs.existsSync(denyPath)) return [];
  return fs
    .readFileSync(denyPath, 'utf8')
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter((line) => line.length >= 3 && !line.startsWith('#'))
    .map((line) => line.toLowerCase());
}
const localPrivateDenylist = loadLocalPrivateDenylist();

const BINARY_OR_GENERATED_EXTS = /\.(png|jpe?g|gif|ico|bin|wasm|exe|zip|tar\.gz|woff2?|ttf|eot|mp4|webm)$/i;
const SELF_EXEMPT = new Set([
  'scripts/verify-release-hygiene.mjs',
  'test/hygiene/release_hygiene.test.mjs',
]);

function walkFiles(dir) {
  let results = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (
      entry.name === '.git' ||
      entry.name === 'node_modules' ||
      entry.name === 'dist' ||
      entry.name === 'bin' ||
      entry.name.includes('.local.') ||
      entry.name.endsWith('.local')
    ) {
      continue;
    }
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      results = results.concat(walkFiles(full));
    } else if (!BINARY_OR_GENERATED_EXTS.test(entry.name)) {
      results.push(full);
    }
  }
  return results;
}

function getAuditFiles() {
  if (process.env.HYGIENE_ROOT_DIR) {
    return walkFiles(rootDir).filter(
      (f) => !SELF_EXEMPT.has(path.relative(rootDir, f).replace(/\\/g, '/'))
    );
  }
  try {
    const out = execFileSync(
      'git',
      ['ls-files', '-z', '--cached', '--others', '--exclude-standard'],
      { cwd: rootDir }
    );
    const files = [];
    for (const rel of out.toString('utf8').split('\0').filter(Boolean)) {
      if (SELF_EXEMPT.has(rel) || BINARY_OR_GENERATED_EXTS.test(rel)) continue;
      const full = path.join(rootDir, rel);
      if (fs.existsSync(full) && fs.statSync(full).isFile()) {
        files.push(full);
      }
    }
    return files;
  } catch {
    return walkFiles(rootDir).filter(
      (f) => !SELF_EXEMPT.has(path.relative(rootDir, f).replace(/\\/g, '/'))
    );
  }
}

const auditFiles = getAuditFiles();
let leakCount = 0;
for (const fullPath of auditFiles) {
  const relFile = path.relative(rootDir, fullPath).replace(/\\/g, '/');
  const baseName = path.basename(relFile);
  if ((baseName === '.env' || baseName.startsWith('.env.')) && baseName !== '.env.example') {
    errors.push(`Forbidden .env file detected in repository workspace: ${relFile}`);
    leakCount++;
    continue;
  }
  if (baseName === LOCAL_DENYLIST_FILENAME) {
    errors.push(`Forbidden local denylist file detected in repository workspace: ${relFile}`);
    leakCount++;
    continue;
  }
  const buf = fs.readFileSync(fullPath);
  if (buf.includes(0)) continue;
  const content = buf.toString('utf8');
  for (const { name, pattern } of sensitivePatterns) {
    if (pattern.test(content)) {
      errors.push(`Privacy/secret leak detected: ${name} matched in ${relFile}`);
      leakCount++;
    }
  }
  if (localPrivateDenylist.length > 0) {
    const lowerContent = content.toLowerCase();
    for (let idx = 0; idx < localPrivateDenylist.length; idx++) {
      if (lowerContent.includes(localPrivateDenylist[idx])) {
        errors.push(`Privacy/secret leak detected: local denylist entry #${idx + 1} matched in ${relFile}`);
        leakCount++;
      }
    }
  }
  if (/\.md$/i.test(relFile) && /file:\/\/\//i.test(content)) {
    errors.push(`Markdown file:/// link detected in ${relFile}`);
    leakCount++;
  }
}
check(
  leakCount === 0,
  `Zero Private Data Invariant: scanned ${auditFiles.length} workspace files` +
    (localPrivateDenylist.length > 0 ? ` (+ ${localPrivateDenylist.length} local denylist entries)` : '')
);

console.log('\n-------------------------------------------------------------');
if (errors.length > 0) {
  console.error(`Release hygiene verification FAILED with ${errors.length} error(s):`);
  for (const e of errors) console.error(`  - ${e}`);
  process.exit(1);
}
console.log('All release hygiene and version parity checks PASSED.');
