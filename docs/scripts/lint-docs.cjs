// Documentation engineering lint. Enforces the machine-checkable rules of docs/STYLE_GUIDE.md.
//
// Errors (exit 1):
//   E1 heading contains emoji
//   E2 banned marketing phrase (STYLE_GUIDE section 3); config.mts text is scanned too
//   E3 fenced code block without a language tag
//   E4 shell prompt ("$ ") inside a bash/sh/shell code block
//   E5 `TestXxx` referenced in docs but no `func TestXxx(` exists in the Go sources
//   E6 relative link from docs/ that escapes docs/ (VitePress cannot resolve it; use a GitHub URL)
//   E7 zh/en mirror missing (bidirectional) or structure drift (heading / code block / Mermaid counts differ)
//   E8 broken internal relative Markdown target or #anchor link (matches VitePress slugify)
//   E9 retired architecture / configuration term in live docs
// Warnings (reported, non-fatal):
//   W1 page length over the soft cap (500 lines / 40 KB)
//
// Historical records (CHANGELOG entries below the latest release, docs/rfcs)
// are exempt from E2/E5/E9 because they describe the project as it was.
const fs = require('fs');
const path = require('path');
const { repoRoot, collectMarkdownFiles } = require('./collect-md.cjs');

const docsRoot = path.join(repoRoot, 'docs');

// ---- Banned phrases (keep in sync with docs/STYLE_GUIDE.md section 3) ----
const BANNED_ZH = [
  '颠覆传统', '军工级', '金融级', '完全无敌', '秒杀', '吊打', '绝对安全', '完美无瑕', '完美适配',
  '彻底告别', '能跑多快', '高科技', '最强', '无懈可击', '狂飙', '首创', '划时代', '全息', '秒级',
  '深水区', '护城河', '物理级', '数学上保证', '世界上最严苛', '一票否决', '100% 零配置',
];
// English phrases are matched case-insensitively on word boundaries.
const BANNED_EN = [
  'military-grade', 'bulletproof', 'revolutionary', 'pioneered', 'holographic', 'blazing',
  'world-class', "world's most", 'game-changing', 'game changer', 'unbeatable', 'flawless',
  'lightning-fast', 'indestructible', 'ultimate',
  // "magic" as a marketing adjective; technical terms such as "magic number/bytes" are allowed.
  'magic(?!\\s+(?:number|numbers|byte|bytes|string|value|cookie|header))',
];
const BANNED_EN_RE = BANNED_EN.map((p) => [p, new RegExp(`\\b${p}\\b`, 'i')]);

// Files that legitimately quote banned phrases (the style guide itself lists them).
const BANNED_EXEMPT = new Set(['docs/STYLE_GUIDE.md', 'docs/en/STYLE_GUIDE.md']);

const HISTORICAL_PREFIXES = [
  'docs/rfcs/',
  'docs/en/rfcs/',
];

// Retired terms that must not creep back into live documentation (E9).
const RETIRED_TERMS = [
  ['P2PMux V5', /\bP2PMux\s+V5\b/i],
  ['P2PMux V6', /\bP2PMux\s+V6\b/i],
  ['IceRestartFSM', /\bIceRestartFSM\b/],
  ['RebindProof', /\bRebindProof\b/],
  ['CAP_MINT', /\bCAP_MINT\b/],
  ['SINGLE_DOMAIN_TRUSTED_APPS', /\bSINGLE_DOMAIN_TRUSTED_APPS\b/],
  ['SIGNAL_ORIGIN', /\bSIGNAL_ORIGIN\b/],
];

const emojiRegex = /\p{Extended_Pictographic}/u;

// Exact VitePress (@mdit-vue/shared) heading slugify rules for E8 anchor validation.
const rControl = /[\u0000-\u001f]/g;
const rSpecial = /[\s~`!@#$%^&*()\-_+=[\]{}|\\;:"'“”‘’<>,.?/]+/g;
const rCombining = /[\u0300-\u036F]/g;
function vitepressSlugify(rawHeading) {
  const plain = rawHeading
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/<\/?[^>]+>/g, '')
    .trim();
  return plain
    .normalize('NFKD')
    .replace(rCombining, '')
    .replace(rControl, '')
    .replace(rSpecial, '-')
    .replace(/-{2,}/g, '-')
    .replace(/^-+|-+$/g, '')
    .replace(/^(\d)/, '_$1')
    .toLowerCase();
}

const errors = [];
const warnings = [];
const err = (code, file, line, msg) => errors.push(`❌ [${code}] ${file}:${line} ${msg}`);
const warn = (code, file, msg) => warnings.push(`⚠️  [${code}] ${file} ${msg}`);

// ---- Go test function index (for E5) ----
function collectGoTests(dir, set) {
  for (const name of fs.readdirSync(dir)) {
    if (name === 'node_modules' || name === '.git' || name === 'bin') continue;
    const full = path.join(dir, name);
    const stat = fs.statSync(full);
    if (stat.isDirectory()) collectGoTests(full, set);
    else if (name.endsWith('_test.go')) {
      const src = fs.readFileSync(full, 'utf-8');
      for (const m of src.matchAll(/^func (Test\w+)\(/gm)) set.add(m[1]);
    }
  }
  return set;
}
const goTests = collectGoTests(repoRoot, new Set());

function isHistorical(rel) {
  return HISTORICAL_PREFIXES.some((p) => rel.startsWith(p));
}

// For CHANGELOG.md only the [Unreleased] section and the latest release section are "live".
function changelogLiveLineLimit(lines) {
  let releaseHeaders = 0;
  for (let i = 0; i < lines.length; i++) {
    if (/^## \[/.test(lines[i]) && !/^## \[Unreleased\]/i.test(lines[i])) {
      releaseHeaders++;
      if (releaseHeaders === 2) return i;
    }
  }
  return lines.length;
}

function checkRetired(rel, lineNo, text) {
  if (BANNED_EXEMPT.has(rel)) return;
  for (const [label, re] of RETIRED_TERMS) {
    if (re.test(text)) err('E9', rel, lineNo, `retired term "${label}" in live documentation`);
  }
}

function checkBanned(rel, lineNo, text) {
  if (BANNED_EXEMPT.has(rel)) return;
  for (const p of BANNED_ZH) {
    if (text.includes(p)) err('E2', rel, lineNo, `banned phrase "${p}"`);
  }
  for (const [p, re] of BANNED_EN_RE) {
    if (re.test(text)) err('E2', rel, lineNo, `banned phrase "${p.split('(')[0]}"`);
  }
}

const files = collectMarkdownFiles();
const structure = new Map(); // rel -> {headings, code, mermaid}
const slugsByRel = new Map(); // rel -> Set<string>
const internalLinks = []; // { rel, file, lineNo, rawTarget, inDocs }

for (const file of files) {
  const rel = path.relative(repoRoot, file).split(path.sep).join('/');
  const content = fs.readFileSync(file, 'utf-8');
  const lines = content.split('\n');
  const historical = isHistorical(rel);
  const liveLimit = rel === 'CHANGELOG.md' ? changelogLiveLineLimit(lines) : lines.length;
  const inDocs = rel.startsWith('docs/');

  let fence = null; // { marker, lang }
  const stats = { headings: 0, code: 0, mermaid: 0 };
  const slugCounts = new Map();
  const slugSet = new Set();

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const lineNo = i + 1;
    const live = !historical && i < liveLimit;
    const fenceMatch = line.match(/^\s*(`{3,}|~{3,})\s*([^\s`]*)/);

    if (fenceMatch) {
      if (!fence) {
        fence = { marker: fenceMatch[1], lang: fenceMatch[2].toLowerCase() };
        stats.code++;
        if (fence.lang === 'mermaid') stats.mermaid++;
        if (!fence.lang) err('E3', rel, lineNo, 'code block without a language tag (use text/console if it is plain output)');
        continue;
      }
      if (line.trim().startsWith(fence.marker) && line.trim().replace(/[`~]/g, '') === '') {
        fence = null;
        continue;
      }
    }

    if (fence) {
      if (live) checkRetired(rel, lineNo, line);
      if (['bash', 'sh', 'shell', 'zsh'].includes(fence.lang) && /^\s*\$ /.test(line)) {
        err('E4', rel, lineNo, 'shell prompt "$ " in command block (commands must be copy-pasteable)');
      }
      continue;
    }

    const heading = line.match(/^(#{1,6})\s+(.*)$/);
    if (heading) {
      if (heading[1].length >= 2 && heading[1].length <= 3) stats.headings++;
      if (emojiRegex.test(heading[2])) err('E1', rel, lineNo, `heading contains emoji: "${line.trim()}"`);
      const baseSlug = vitepressSlugify(heading[2]);
      if (baseSlug) {
        const count = slugCounts.get(baseSlug) || 0;
        slugCounts.set(baseSlug, count + 1);
        slugSet.add(count === 0 ? baseSlug : `${baseSlug}-${count}`);
      }
    }

    if (live) {
      checkBanned(rel, lineNo, line);
      checkRetired(rel, lineNo, line);
      for (const m of line.matchAll(/`(Test[A-Z]\w*)`/g)) {
        if (!goTests.has(m[1])) err('E5', rel, lineNo, `references non-existent Go test \`${m[1]}\``);
      }
    }

    // Strip inline code spans before scanning Markdown links so examples in backticks are ignored.
    const lineNoCode = line.replace(/`[^`]*`/g, '');
    if (inDocs) {
      for (const m of lineNoCode.matchAll(/\]\((\.{1,2}\/[^)\s#]+)(#[^)\s]*)?\)/g)) {
        const target = path.resolve(path.dirname(file), m[1]);
        if (!target.startsWith(docsRoot + path.sep)) {
          err('E6', rel, lineNo, `relative link escapes docs/ ("${m[1]}"); use https://github.com/BeingNoAbility/WebRTC-Gateway/blob/main/<path>`);
        }
      }
    }
    if (live) {
      for (const m of lineNoCode.matchAll(/(!?)\[[^\]]*\]\(([^)\s]+)\)/g)) {
        if (m[1] === '!') continue; // skip image links
        const rawTarget = m[2];
        if (/^(?:https?:|mailto:|tel:|data:|javascript:)/i.test(rawTarget)) continue;
        internalLinks.push({ rel, file, lineNo, rawTarget, inDocs });
      }
    }
  }

  structure.set(rel, stats);
  slugsByRel.set(rel, slugSet);

  if (rel !== 'CHANGELOG.md' && !historical) {
    const bytes = Buffer.byteLength(content, 'utf-8');
    if (lines.length > 500 || bytes > 40 * 1024) {
      warn('W1', rel, `is ${lines.length} lines / ${(bytes / 1024).toFixed(1)} KB (soft cap 500 lines / 40 KB)`);
    }
  }
}

// ---- config.mts user-visible text (nav, sidebar, meta) ----
const configPath = path.join(docsRoot, '.vitepress', 'config.mts');
if (fs.existsSync(configPath)) {
  const rel = 'docs/.vitepress/config.mts';
  fs.readFileSync(configPath, 'utf-8').split('\n').forEach((line, i) => checkBanned(rel, i + 1, line));
}

// ---- zh/en mirror structure (E7: fatal bidirectional parity gate) ----
const pairs = [['README.md', 'README_en.md']];
const seenZh = new Set(['README.md']);
for (const rel of structure.keys()) {
  if (rel.startsWith('docs/') && !rel.startsWith('docs/en/') && rel !== 'docs/RELEASE_NOTES.md') {
    pairs.push([rel, rel.replace(/^docs\//, 'docs/en/')]);
    seenZh.add(rel);
  }
}
for (const rel of structure.keys()) {
  if (rel.startsWith('docs/en/')) {
    const zhRel = rel.replace(/^docs\/en\//, 'docs/');
    if (!seenZh.has(zhRel)) {
      err('E7', rel, 1, `has no Chinese mirror (${zhRel})`);
    }
  }
}
for (const [zh, en] of pairs) {
  const a = structure.get(zh);
  const b = structure.get(en);
  if (!b) {
    err('E7', zh, 1, `has no English mirror (${en})`);
    continue;
  }
  const diffs = ['headings', 'code', 'mermaid'].filter((k) => a[k] !== b[k]).map((k) => `${k} ${a[k]}≠${b[k]}`);
  if (diffs.length) err('E7', zh, 1, `zh/en drift vs ${en}: ${diffs.join(', ')}`);
}

// ---- E8: internal relative Markdown link & #anchor validation ----
function resolveInternalTarget(link) {
  const hashIdx = link.rawTarget.indexOf('#');
  const rawPath = hashIdx >= 0 ? link.rawTarget.slice(0, hashIdx) : link.rawTarget;
  const rawAnchor = hashIdx >= 0 ? link.rawTarget.slice(hashIdx + 1) : '';
  const cleanPath = rawPath.split('?')[0];

  if (!cleanPath) {
    return { targetAbs: link.file, anchor: rawAnchor };
  }
  let baseAbs;
  if (cleanPath.startsWith('/')) {
    if (!link.inDocs) return null; // non-docs root-relative links (e.g. URL examples)
    baseAbs = path.join(docsRoot, cleanPath.slice(1));
  } else {
    baseAbs = path.resolve(path.dirname(link.file), cleanPath);
  }
  const candidates = [];
  if (cleanPath.endsWith('/')) {
    candidates.push(path.join(baseAbs, 'index.md'));
  } else {
    candidates.push(baseAbs);
    if (!path.extname(baseAbs)) {
      candidates.push(`${baseAbs}.md`);
      candidates.push(path.join(baseAbs, 'index.md'));
    }
  }
  for (const cand of candidates) {
    if (fs.existsSync(cand)) {
      if (fs.statSync(cand).isDirectory()) {
        const idx = path.join(cand, 'index.md');
        if (fs.existsSync(idx)) return { targetAbs: idx, anchor: rawAnchor };
        return { targetAbs: cand, anchor: rawAnchor };
      }
      return { targetAbs: cand, anchor: rawAnchor };
    }
  }
  return { targetAbs: null, anchor: rawAnchor, attempted: baseAbs };
}

for (const link of internalLinks) {
  const resolved = resolveInternalTarget(link);
  if (!resolved) continue;
  if (!resolved.targetAbs) {
    // Avoid duplicating E6 when a docs/ relative link already escaped docsRoot.
    if (link.inDocs && resolved.attempted && !resolved.attempted.startsWith(docsRoot + path.sep)) continue;
    err('E8', link.rel, link.lineNo, `broken internal link "${link.rawTarget}"`);
    continue;
  }
  if (resolved.anchor && resolved.targetAbs.endsWith('.md')) {
    const targetRel = path.relative(repoRoot, resolved.targetAbs).split(path.sep).join('/');
    const targetSlugs = slugsByRel.get(targetRel);
    let decodedAnchor = resolved.anchor;
    try {
      decodedAnchor = decodeURIComponent(resolved.anchor);
    } catch {
      // keep raw anchor
    }
    if (targetSlugs && !targetSlugs.has(decodedAnchor)) {
      err('E8', link.rel, link.lineNo, `broken anchor "#${resolved.anchor}" in "${link.rawTarget}" (target ${targetRel})`);
    }
  }
}

console.log(`[Docs Linter] Scanned ${files.length} Markdown files + config.mts; indexed ${goTests.size} Go tests.`);
if (warnings.length) {
  console.log(`\n${warnings.length} warning(s):`);
  for (const w of warnings) console.log(w);
}
if (errors.length) {
  console.error(`\n${errors.length} error(s):`);
  for (const e of errors) console.error(e);
  console.error('\n🚨 Documentation lint FAILED. See docs/STYLE_GUIDE.md.\n');
  process.exit(1);
}
console.log('\n✅ Documentation lint PASSED (0 errors).\n');
