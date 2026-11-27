// Shared Markdown file collector for docs lint scripts (lint-docs.cjs, lint-mermaid.mjs).
// Both linters MUST scan the same file set; change exclusions here only.
const fs = require('fs');
const path = require('path');

const repoRoot = path.resolve(__dirname, '../..');

const EXCLUDED_DIRS = new Set(['node_modules', '.vitepress', 'dist', '.git', 'bin']);

function walk(dir) {
  let results = [];
  for (const name of fs.readdirSync(dir)) {
    if (EXCLUDED_DIRS.has(name)) continue;
    const fullPath = path.join(dir, name);
    const stat = fs.statSync(fullPath);
    if (stat.isDirectory()) {
      results = results.concat(walk(fullPath));
    } else if (name.endsWith('.md')) {
      results.push(fullPath);
    }
  }
  return results;
}

function collectMarkdownFiles() {
  return walk(repoRoot).sort();
}

module.exports = { repoRoot, collectMarkdownFiles };
