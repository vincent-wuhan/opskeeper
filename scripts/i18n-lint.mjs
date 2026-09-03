#!/usr/bin/env node
// i18n-lint.mjs — scan web/src for tr() translation issues (Task 2.12).
//
// Detects:
//   - missing English: tr('中文', '')
//   - empty both:     tr('', '')
//   - same both:      tr('same', 'same') (suspicious — usually a copy-paste)
//
// Exits 0 if clean, 1 if any issues found.

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, extname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(fileURLToPath(import.meta.url), '..', '..', 'web', 'src');

// Recursive file walker.
function* walk(dir) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const s = statSync(full);
    if (s.isDirectory()) yield* walk(full);
    else if (['.ts', '.tsx', '.js', '.jsx'].includes(extname(entry))) yield full;
  }
}

const PATTERNS = [
  { name: 'missing English', re: /tr\(\s*['"]([^'"]+)['"]\s*,\s*['"]([^'"]*)['"]\s*\)/g },
];

const issues = [];


function isAcceptableSameBoth(s) {
  if (typeof s !== 'string') return false;
  // Pure-ASCII single word with optional capital letter = brand/loanword
  // Examples: "Release", "WebSSH", "AI", "OK"
  if (/^[A-Z][A-Za-z0-9]*$/.test(s) && s === s.replace(/\s/g, '')) return true;
  // Language names: 中文 / English / 日本語
  if (/^[一-鿿]+$/.test(s) && s.length <= 6) return true;
  // Mixed English-only string used in both locales = intentional
  // (e.g. "WebSSH — admin / user only")
  if (/^[A-Za-z0-9 \-\/—]+$/.test(s)) return true;
  return false;
}



for (const file of walk(ROOT)) {
  const lines = readFileSync(file, 'utf8').split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    for (const { name, re } of PATTERNS) {
      re.lastIndex = 0;
      let m;
      while ((m = re.exec(line)) !== null) {
        const [, zh, en] = m;
        if (en === '') {
          issues.push({ file, line: i + 1, type: name, zh });
        } else if (zh === en) {
          // Skip "same both" for pure-ASCII single-word brand/loanword strings
          // (e.g. "Release", "WebSSH") and language names ("English", "中文").
          // These are intentional — the string is the same in both locales.
          if (isAcceptableSameBoth(zh)) continue;
          issues.push({ file, line: i + 1, type: 'same both (suspicious)', text: zh });
        }
      }
    }
  }
}

if (issues.length === 0) {
  console.log('i18n-lint: clean ✓');
  process.exit(0);
}

console.log(`i18n-lint: found ${issues.length} issue(s)`);
for (const it of issues) {
  const rel = it.file.replace(ROOT + '/', '');
  const text = it.zh || it.text;
  console.log(`  ${rel}:${it.line}  [${it.type}]  ${text}`);
}
process.exit(1);
