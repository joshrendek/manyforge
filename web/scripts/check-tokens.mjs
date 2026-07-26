#!/usr/bin/env node
// manyforge-fn1x — fail the build on a CSS custom property that is never defined.
//
// `background: var(--mf-surface-1)` with no fallback is invalid at computed-value time: the
// declaration is dropped and the property falls back to its initial value. The page still RENDERS,
// which is what makes this worth automating — it is invisible to code review (nothing looks wrong)
// and survives a real-browser check (transparent over a white page in light mode looks like a
// white card). It shipped to master exactly that way.
//
// Deliberately a plain Node script rather than a vitest spec: the Angular specs compile for a
// browser target, so `node:fs` is unavailable there, and `import.meta.glob` must be referenced
// literally, which makes the same check considerably more awkward to express.

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..');
const ROOTS = ['src'];
const EXT = /\.(ts|css|html)$/;

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    if (entry === 'node_modules' || entry === '.angular') continue;
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (EXT.test(entry)) out.push(p);
  }
  return out;
}

const files = ROOTS.flatMap((r) => walk(join(WEB, r)));
const sources = files.map((f) => ({ file: f, text: readFileSync(f, 'utf8') }));

// Every `--mf-foo:` definition, wherever it lives.
const defined = new Set();
for (const { text } of sources) {
  for (const m of text.matchAll(/(--mf-[a-z0-9-]+)\s*:/g)) defined.add(m[1]);
}

// Vacuity guard. If the walk or the regex breaks, `defined` empties and every reference below
// would appear to resolve — the check would go green precisely when it stopped working.
const BASELINE = ['--mf-text', '--mf-surface', '--mf-border'];
const missingBaseline = BASELINE.filter((t) => !defined.has(t));
if (files.length < 20 || defined.size < 20 || missingBaseline.length) {
  console.error(
    `check-tokens: the scan itself looks broken — ${files.length} files, ${defined.size} tokens,` +
      ` missing baseline [${missingBaseline.join(', ')}]. Refusing to report success.`,
  );
  process.exit(2);
}

// `var(--x, fallback)` is skipped: a fallback makes an undefined token a deliberate choice.
const bad = [];
for (const { file, text } of sources) {
  for (const m of text.matchAll(/var\(\s*(--mf-[a-z0-9-]+)\s*\)/g)) {
    if (!defined.has(m[1])) bad.push(`${relative(WEB, file)}: var(${m[1]})`);
  }
}

if (bad.length) {
  console.error('check-tokens: undefined CSS custom properties\n');
  for (const b of bad) console.error(`  ${b}`);
  console.error(
    '\nThese resolve to the property\'s initial value and fail SILENTLY — the page still renders,' +
      '\njust wrong, and often only in one theme. Either define the token or give var() a fallback.',
  );
  process.exit(1);
}

console.log(`check-tokens: ok — ${defined.size} tokens defined, ${files.length} files scanned`);
