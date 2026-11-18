const fs = require('fs');
const path = require('path');

const repoRoot = path.resolve(__dirname, '../..');
const docsRoot = path.resolve(__dirname, '..');

const scanFiles = [
  path.join(repoRoot, 'README.md'),
  path.join(repoRoot, 'README_en.md'),
  path.join(repoRoot, 'CHANGELOG.md'),
];

function getMarkdownFiles(dir) {
  let results = [];
  const list = fs.readdirSync(dir);
  for (const file of list) {
    if (file === 'node_modules' || file === '.vitepress' || file === 'dist') continue;
    const fullPath = path.join(dir, file);
    const stat = fs.statSync(fullPath);
    if (stat && stat.isDirectory()) {
      results = results.concat(getMarkdownFiles(fullPath));
    } else if (file.endsWith('.md')) {
      results.push(fullPath);
    }
  }
  return results;
}

const allMdFiles = [...new Set([...scanFiles, ...getMarkdownFiles(docsRoot)])];

console.log(`[Docs Linter] Scanning ${allMdFiles.length} Markdown files for engineering hygiene...`);

let hasError = false;

// Regex for emoji in headers
const emojiRegex = /[\uD83C-\uDBFF\uDC00-\uDFFF]|[\u2600-\u27BF]/;

// Blacklist of banned hyperbolic buzzwords from STYLE_GUIDE.md Section 3
const bannedPhrases = [
  '颠覆传统',
  '军工级',
  '完全无敌',
  '秒杀',
  '吊打',
  '绝对安全',
  '完美无瑕',
];

for (const filePath of allMdFiles) {
  if (!fs.existsSync(filePath)) continue;
  const relPath = path.relative(repoRoot, filePath);
  const content = fs.readFileSync(filePath, 'utf-8');
  const lines = content.split('\n');

  let inCodeBlock = false;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];

    if (line.trim().startsWith('```')) {
      inCodeBlock = !inCodeBlock;
      continue;
    }

    if (inCodeBlock) continue;

    // 1. Check title emoji hygiene
    const headerMatch = line.match(/^(\s*#{1,6}\s+)(.*)$/);
    if (headerMatch) {
      const headerText = headerMatch[2];
      if (emojiRegex.test(headerText)) {
        console.error(`❌ [Title Hygiene Error] ${relPath}:${i + 1} Heading contains emoji: "${line.trim()}"`);
        hasError = true;
      }
    }

    // 2. Check banned marketing buzzwords (skip STYLE_GUIDE itself which discusses them)
    if (!relPath.includes('STYLE_GUIDE.md')) {
      for (const phrase of bannedPhrases) {
        if (line.includes(phrase)) {
          console.error(`❌ [Terminology Error] ${relPath}:${i + 1} Contains banned buzzword "${phrase}": "${line.trim()}"`);
          hasError = true;
        }
      }
    }
  }
}

if (hasError) {
  console.error('\n🚨 Documentation engineering hygiene check FAILED! Please align with docs/STYLE_GUIDE.md.\n');
  process.exit(1);
} else {
  console.log('✅ Documentation engineering hygiene check PASSED (0 title emojis, 0 banned buzzwords).\n');
  process.exit(0);
}
