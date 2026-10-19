/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Run with: node --test scripts/precompress.test.mjs  (from web/)
//
// The unit tests use a temp dir. The "real build" tests check the output of
// `npm run build` in dist/client (decoding and byte budgets); they skip when
// no build is present (fail with REQUIRE_BUILD=1), so run them after a build.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync, utimesSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, basename } from 'node:path';
import { fileURLToPath } from 'node:url';
import { brotliDecompressSync, gunzipSync } from 'node:zlib';
import { precompressDir, isCandidateName, MIN_SIZE } from './precompress.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const script = join(here, 'precompress.mjs');
const distClient = join(here, '..', 'dist', 'client');

// Byte budgets from the webperf design (A1 acceptance criteria).
const MAIN_BR_MAX = 85_000;
const SHOELACE_BR_MAX = 50_000;
const ENTRY_CLOSURE_BR_MAX = 160_000;

// fixture writes a small client build into a new temp dir, removed when the
// test ends.
function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'precompress-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const text = 'export const x = "scion";\n'.repeat(200); // compresses well
  const files = {
    'assets/main.js': text,
    'assets/style.css': 'body { color: red; }\n'.repeat(100),
    'assets/main.js.map': text,
    'assets/tiny.js': 'x'.repeat(MIN_SIZE - 1),
    'assets/exact.js': 'y'.repeat(MIN_SIZE),
    'assets/font.woff2': text,
    'shoelace/assets/icons/a.svg': `<svg>${'<path d="M0 0h16v16H0z"/>'.repeat(60)}</svg>`,
    'manifest.webmanifest': JSON.stringify({ name: 'x'.repeat(2000) }),
    '.vite/manifest.json': JSON.stringify({ k: 'v'.repeat(2000) }),
  };
  for (const [name, data] of Object.entries(files)) {
    mkdirSync(dirname(join(dir, name)), { recursive: true });
    writeFileSync(join(dir, name), data);
  }
  // Incompressible random bytes with a compressible extension.
  const rnd = Buffer.alloc(4096);
  for (let i = 0; i < rnd.length; i++) rnd[i] = Math.floor(Math.random() * 256);
  writeFileSync(join(dir, 'assets', 'random.js'), rnd);
  return dir;
}

test('isCandidateName', () => {
  for (const n of ['a.js', 'a.mjs', 'a.css', 'a.svg', 'a.json', 'a.webmanifest', 'a.html', 'a.txt', 'A.JS']) {
    assert.ok(isCandidateName(n), n);
  }
  for (const n of ['a.js.map', 'a.js.br', 'a.js.gz', 'a.woff2', 'a.png', 'a.ico', 'noext']) {
    assert.ok(!isCandidateName(n), n);
  }
});

test('writes br and gz siblings that decode to the source bytes', (t) => {
  const dir = fixture(t);
  const summary = precompressDir(dir);
  for (const name of ['assets/main.js', 'assets/style.css', 'assets/exact.js', 'shoelace/assets/icons/a.svg', 'manifest.webmanifest', '.vite/manifest.json']) {
    const raw = readFileSync(join(dir, name));
    const br = readFileSync(join(dir, `${name}.br`));
    const gz = readFileSync(join(dir, `${name}.gz`));
    assert.deepEqual(brotliDecompressSync(br), raw, `${name}.br`);
    assert.deepEqual(gunzipSync(gz), raw, `${name}.gz`);
    assert.ok(br.length < raw.length * 0.95 && gz.length < raw.length * 0.95, name);
    assert.equal(gz.readUInt32LE(4), 0, `${name}.gz header mtime must be 0`);
  }
  assert.equal(summary.files, 7, 'six compressible files plus random.js');
});

test('skips source maps, small files, other types and incompressible files', (t) => {
  const dir = fixture(t);
  precompressDir(dir);
  for (const name of ['assets/main.js.map', 'assets/tiny.js', 'assets/font.woff2', 'assets/random.js']) {
    assert.ok(!existsSync(join(dir, `${name}.br`)), `${name}.br`);
    assert.ok(!existsSync(join(dir, `${name}.gz`)), `${name}.gz`);
  }
});

test('is deterministic and idempotent, and removes stale siblings', (t) => {
  const dir = fixture(t);
  precompressDir(dir);
  const first = readFileSync(join(dir, 'assets/main.js.gz'));
  const firstBr = readFileSync(join(dir, 'assets/main.js.br'));
  // A stale sibling next to a file that no longer qualifies is removed.
  writeFileSync(join(dir, 'assets/random.js.br'), 'stale');
  writeFileSync(join(dir, 'assets/tiny.js.gz'), 'stale');
  precompressDir(dir);
  assert.deepEqual(readFileSync(join(dir, 'assets/main.js.gz')), first);
  assert.deepEqual(readFileSync(join(dir, 'assets/main.js.br')), firstBr);
  assert.ok(!existsSync(join(dir, 'assets/random.js.br')));
  assert.ok(!existsSync(join(dir, 'assets/tiny.js.gz')));
  assert.ok(!existsSync(join(dir, 'assets/main.js.br.br')), 'variants are never compressed again');
  assert.ok(!existsSync(join(dir, 'assets/main.js.gz.br')));
});

test('siblings are not older than their source (DiskIndex mtime rule)', (t) => {
  const dir = fixture(t);
  const past = new Date(Date.now() - 3_600_000);
  utimesSync(join(dir, 'assets/main.js'), past, past);
  precompressDir(dir);
  assert.ok(statSync(join(dir, 'assets/main.js.br')).mtimeMs >= statSync(join(dir, 'assets/main.js')).mtimeMs);
});

test('CLI: usage error without a dir, summary line with one', (t) => {
  const bad = spawnSync(process.execPath, [script], { encoding: 'utf8' });
  assert.equal(bad.status, 2);
  assert.match(bad.stderr, /usage/);
  const dir = fixture(t);
  const ok = spawnSync(process.execPath, [script, dir], { encoding: 'utf8' });
  assert.equal(ok.status, 0, ok.stderr);
  assert.match(ok.stdout, /^precompress: .*7 files/);
  assert.ok(existsSync(join(dir, 'assets/main.js.br')));
});

// ---- Real build output (after `npm run build`) ----

// REQUIRE_BUILD=1 (set by the gate) turns a missing build into failures
// instead of skips.
const haveBuild = existsSync(join(distClient, 'assets', 'main.js'));
const requireBuild = process.env.REQUIRE_BUILD === '1';
const skipReason = haveBuild || requireBuild ? false : 'no build in dist/client; run npm run build first';

function assertBuild() {
  assert.ok(haveBuild, `no build in ${distClient} (REQUIRE_BUILD=1)`);
}

function* walk(dir) {
  for (const ent of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, ent.name);
    if (ent.isDirectory()) yield* walk(p);
    else if (ent.isFile()) yield p;
  }
}

function brSize(rel) {
  const p = join(distClient, rel);
  return existsSync(`${p}.br`) ? statSync(`${p}.br`).size : statSync(p).size;
}

test('real build: main.js.br and main.js.gz exist and decode to identical bytes', { skip: skipReason }, () => {
  assertBuild();
  const raw = readFileSync(join(distClient, 'assets/main.js'));
  assert.deepEqual(brotliDecompressSync(readFileSync(join(distClient, 'assets/main.js.br'))), raw);
  assert.deepEqual(gunzipSync(readFileSync(join(distClient, 'assets/main.js.gz'))), raw);
});

test('real build: every .br/.gz decodes to its source', { skip: skipReason }, () => {
  assertBuild();
  let n = 0;
  for (const p of walk(distClient)) {
    if (!p.endsWith('.br') && !p.endsWith('.gz')) continue;
    const src = p.slice(0, -3);
    assert.ok(existsSync(src), `orphan variant ${p}`);
    const decoded = p.endsWith('.br') ? brotliDecompressSync(readFileSync(p)) : gunzipSync(readFileSync(p));
    assert.deepEqual(decoded, readFileSync(src), p);
    n++;
  }
  assert.ok(n > 0);
});

test('real build: byte budgets', { skip: skipReason }, () => {
  assertBuild();
  const mainBr = statSync(join(distClient, 'assets/main.js.br')).size;
  console.log(`main.js.br = ${mainBr} B (budget ${MAIN_BR_MAX})`);
  assert.ok(mainBr <= MAIN_BR_MAX, `main.js.br ${mainBr} > ${MAIN_BR_MAX}`);

  const shoelace = readdirSync(join(distClient, 'assets')).filter((n) => /^shoelace-.*\.js$/.test(n));
  assert.ok(shoelace.length > 0, 'no shoelace-*.js chunk');
  for (const n of shoelace) {
    const size = brSize(`assets/${n}`);
    console.log(`${n}.br = ${size} B (budget ${SHOELACE_BR_MAX})`);
    assert.ok(size <= SHOELACE_BR_MAX, `${n}.br ${size} > ${SHOELACE_BR_MAX}`);
  }

  // Entry closure: the entry chunk, its static imports (transitively) and
  // their CSS, measured as br bytes (raw bytes for a file with no .br).
  const manifest = JSON.parse(readFileSync(join(distClient, '.vite/manifest.json'), 'utf8'));
  const entries = Object.entries(manifest).filter(([, c]) => c.isEntry && c.file === 'assets/main.js');
  assert.equal(entries.length, 1, 'exactly one manifest entry for assets/main.js');
  const files = new Set();
  const seen = new Set();
  const visit = (key) => {
    if (seen.has(key)) return;
    seen.add(key);
    const chunk = manifest[key];
    assert.ok(chunk, `manifest import ${key} missing`);
    files.add(chunk.file);
    for (const css of chunk.css ?? []) files.add(css);
    for (const imp of chunk.imports ?? []) visit(imp);
  };
  visit(entries[0][0]);
  let total = 0;
  for (const f of files) total += brSize(f);
  console.log(`entry closure: ${files.size} files, ${total} B br (budget ${ENTRY_CLOSURE_BR_MAX}): ${[...files].map((f) => basename(f)).join(' ')}`);
  assert.ok(total <= ENTRY_CLOSURE_BR_MAX, `entry closure ${total} > ${ENTRY_CLOSURE_BR_MAX}`);
});
