import DOMPurify from '../node_modules/dompurify/dist/purify.es.mjs';
DOMPurify.addHook = () => {};
DOMPurify.sanitize = (s) => s;

import mermaid from '../node_modules/mermaid/dist/mermaid.core.mjs';
import fs from 'fs';
import path from 'path';
import { createRequire } from 'module';

const require = createRequire(import.meta.url);
const { repoRoot, collectMarkdownFiles } = require('./collect-md.cjs');

mermaid.initialize({ startOnLoad: false, suppressErrorRendering: true });

const files = collectMarkdownFiles();
let errorCount = 0;
let totalDiagrams = 0;
const failedDiagrams = [];

for (const file of files) {
  const content = fs.readFileSync(file, 'utf-8');
  const lines = content.split('\n');
  let inMermaid = false;
  let currentDiagram = [];
  let diagramStartLine = 0;
  let diagramIndex = 0;

  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line.trim().startsWith('```mermaid')) {
      inMermaid = true;
      currentDiagram = [];
      diagramStartLine = i + 1;
      diagramIndex++;
    } else if (inMermaid && line.trim().startsWith('```')) {
      inMermaid = false;
      totalDiagrams++;
      const code = currentDiagram.join('\n').trim();
      try {
        await mermaid.parse(code);
      } catch (err) {
        errorCount++;
        failedDiagrams.push({
          file: path.relative(repoRoot, file),
          line: diagramStartLine,
          index: diagramIndex,
          error: err.message || String(err),
          code
        });
      }
    } else if (inMermaid) {
      currentDiagram.push(line);
    }
  }
}

console.log(`[Mermaid Linter] Scanned ${files.length} Markdown files.`);
console.log(`[Mermaid Linter] Verified ${totalDiagrams} Mermaid diagrams.`);

if (errorCount === 0) {
  console.log(`✅ All ${totalDiagrams} Mermaid diagrams parsed successfully with 0 errors!\n`);
  process.exit(0);
} else {
  console.error(`🚨 Total invalid Mermaid diagrams: ${errorCount}\n`);
  for (const f of failedDiagrams) {
    console.error(`❌ [Mermaid Syntax Error] ${f.file}:${f.line} (Diagram #${f.index}):`);
    console.error(`   ${f.error.split('\n')[0]}`);
    console.error('--- Diagram Content ---');
    console.error(f.code);
    console.error('-----------------------\n');
  }
  process.exit(1);
}
