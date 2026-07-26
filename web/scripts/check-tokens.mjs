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

import { lstatSync, readFileSync, readdirSync } from 'node:fs';
import { dirname, extname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..');
const ROOTS = ['src'];
const EXT = /\.(ts|css|html)$/;

// ---------------------------------------------------------------------------
// Detection — pure, so it can be self-tested
// ---------------------------------------------------------------------------

/**
 * Remove comments before scanning so dead code cannot define a token or create a false reference.
 * Quoted strings and template literals are preserved because they can contain live inline styles.
 */
export function stripComments(file, text) {
  // Angular templates can be standalone HTML or embedded in a TypeScript template literal.
  const clean = text.replace(/<!--[\s\S]*?-->/g, '');
  const allowLineComments = extname(file) === '.ts';
  let out = '';
  let quote = '';

  for (let i = 0; i < clean.length; i++) {
    const ch = clean[i];
    const next = clean[i + 1];

    if (quote) {
      out += ch;
      if (ch === '\\' && i + 1 < clean.length) out += clean[++i];
      else if (ch === quote) quote = '';
      continue;
    }

    if (ch === "'" || ch === '"' || ch === '`') {
      quote = ch;
      out += ch;
      continue;
    }
    if (ch === '/' && next === '*') {
      i += 2;
      while (i < clean.length && !(clean[i] === '*' && clean[i + 1] === '/')) {
        if (clean[i] === '\n') out += '\n';
        i++;
      }
      i++;
      continue;
    }
    if (allowLineComments && ch === '/' && next === '/') {
      i += 2;
      while (i < clean.length && clean[i] !== '\n') i++;
      if (i < clean.length) out += '\n';
      continue;
    }
    out += ch;
  }
  return out;
}

/** Every `--mf-foo:` definition across the given sources. */
export function collectDefined(sources) {
  const defined = new Set();
  for (const { file, text: raw } of sources) {
    const text = stripComments(file, raw);
    for (const m of text.matchAll(/(--mf-[a-z0-9-]+)\s*:/g)) defined.add(m[1]);
  }
  return defined;
}

/**
 * References to `--mf-*` with NO fallback that have no definition.
 * `var(--x, fallback)` is skipped: a fallback makes an undefined token a deliberate choice.
 */
export function findUndefined(sources, defined) {
  const bad = [];
  for (const { file, text: raw } of sources) {
    const text = stripComments(file, raw);
    for (const m of text.matchAll(/var\(\s*(--mf-[a-z0-9-]+)\s*\)/g)) {
      if (!defined.has(m[1])) bad.push(`${file}: var(${m[1]})`);
    }
  }
  return bad;
}

// ---------------------------------------------------------------------------
// Self-test
//
// CI otherwise only ever runs this against a CLEAN tree, which proves the checker reports success
// and nothing else — a checker whose detection had silently stopped working would look identical.
// These fixtures exercise the paths that matter, including a broken REFERENCE regex, which the
// vacuity guard below structurally cannot catch.
// ---------------------------------------------------------------------------

function selfTest() {
  const cases = [
    { name: 'undefined token is reported', src: 'var(--mf-nope)', defs: [], expect: 1 },
    {
      name: 'defined token is accepted',
      src: '--mf-ok: red; color: var(--mf-ok)',
      defs: null,
      expect: 0,
    },
    { name: 'a fallback makes it deliberate', src: 'var(--mf-nope, red)', defs: [], expect: 0 },
    {
      name: 'whitespace inside var() still matches',
      src: 'var(  --mf-nope  )',
      defs: [],
      expect: 1,
    },
    { name: 'non-mf tokens are ignored', src: 'var(--other-thing)', defs: [], expect: 0 },
    {
      name: 'multiple undefined tokens are all reported',
      src: 'var(--mf-a) var(--mf-b)',
      defs: [],
      expect: 2,
    },
    {
      name: 'a commented definition cannot satisfy a live reference',
      src: '/* --mf-nope: red */ color: var(--mf-nope)',
      defs: null,
      expect: 1,
    },
    {
      name: 'a CSS-commented reference is ignored',
      src: '/* color: var(--mf-nope) */',
      defs: [],
      expect: 0,
    },
    {
      name: 'an HTML-commented reference is ignored',
      file: 'fixture.html',
      src: '<!-- style="color: var(--mf-nope)" -->',
      defs: [],
      expect: 0,
    },
    {
      name: 'a TypeScript line-commented reference is ignored',
      file: 'fixture.ts',
      src: '// const style = "var(--mf-nope)";',
      defs: [],
      expect: 0,
    },
  ];
  let failed = 0;
  for (const c of cases) {
    const sources = [{ file: c.file ?? 'fixture.css', text: c.src }];
    const defined = c.defs === null ? collectDefined(sources) : new Set(c.defs);
    const got = findUndefined(sources, defined).length;
    if (got !== c.expect) {
      console.error(`check-tokens self-test FAILED: ${c.name} — expected ${c.expect}, got ${got}`);
      failed++;
    }
  }
  if (failed) {
    console.error(`\ncheck-tokens: ${failed} self-test(s) failed; the checker itself is broken.`);
    process.exit(3);
  }
  console.log(`check-tokens: self-test ok (${cases.length} cases)`);
}

// ---------------------------------------------------------------------------
// Scan
// ---------------------------------------------------------------------------

function walk(dir, out = []) {
  for (const entry of readdirSync(dir)) {
    if (entry === 'node_modules' || entry === '.angular') continue;
    const p = join(dir, entry);
    // lstat, not stat: stat FOLLOWS symlinks, so a symlinked directory pointing at an ancestor
    // would recurse forever. A source tree has no need for symlinks here, so skipping them is both
    // safe and simpler than tracking visited inodes.
    const st = lstatSync(p);
    if (st.isSymbolicLink()) continue;
    if (st.isDirectory()) walk(p, out);
    else if (EXT.test(entry)) out.push(p);
  }
  return out;
}

selfTest();

const files = ROOTS.flatMap((r) => walk(join(WEB, r)));
const sources = files.map((f) => ({ file: relative(WEB, f), text: readFileSync(f, 'utf8') }));
const defined = collectDefined(sources);

// Vacuity guard for the DEFINITION side and the walk: if either breaks, the scan could examine
// nothing and still report success. The reference side is covered by the self-test above — that
// split is deliberate, because this guard cannot detect a reference regex that matches nothing.
const BASELINE = ['--mf-text', '--mf-surface', '--mf-border'];
const missingBaseline = BASELINE.filter((t) => !defined.has(t));
if (files.length < 20 || defined.size < 20 || missingBaseline.length) {
  console.error(
    `check-tokens: the scan itself looks broken — ${files.length} files, ${defined.size} tokens,` +
      ` missing baseline [${missingBaseline.join(', ')}]. Refusing to report success.`,
  );
  process.exit(2);
}

const bad = findUndefined(sources, defined);
if (bad.length) {
  console.error('check-tokens: undefined CSS custom properties\n');
  for (const b of bad) console.error(`  ${b}`);
  console.error(
    "\nThese resolve to the property's initial value and fail SILENTLY — the page still renders," +
      '\njust wrong, and often only in one theme. Either define the token or give var() a fallback.',
  );
  process.exit(1);
}

console.log(`check-tokens: ok — ${defined.size} tokens defined, ${files.length} files scanned`);
