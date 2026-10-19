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

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { findSourcemapLeftovers, moveSourcemaps } from './move-sourcemaps.mjs';

const SCRIPT = join(dirname(fileURLToPath(import.meta.url)), 'move-sourcemaps.mjs');

function fixture(t, files) {
  const root = mkdtempSync(join(tmpdir(), 'move-sourcemaps-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const client = join(root, 'dist', 'client');
  for (const [rel, body] of Object.entries(files)) {
    const p = join(client, rel);
    mkdirSync(dirname(p), { recursive: true });
    writeFileSync(p, body);
  }
  return { root, client, maps: join(root, 'dist', 'sourcemaps') };
}

const BUILD = {
  'index.html': '<script type="module" src="/assets/main.js"></script>',
  'assets/main.js': 'console.log(1);\n',
  'assets/main.js.map': '{"version":3,"file":"main.js"}',
  'assets/lit-abc123.js': 'export {};\n',
  'assets/lit-abc123.js.map': '{"version":3,"file":"lit-abc123.js"}',
  'assets/main-def456.css': 'body{}\n',
  'assets/main-def456.css.map': '{"version":3}',
  'assets/workers/deep/pdf-ghi789.js': 'export {};\n',
  'assets/workers/deep/pdf-ghi789.js.map': '{"version":3,"file":"pdf-ghi789.js"}',
  '.vite/manifest.json': '{}',
};

test('moves every .map to the same relative path outside the client tree', (t) => {
  const { client, maps } = fixture(t, BUILD);
  const moved = moveSourcemaps(client, maps);
  assert.deepEqual(moved, [
    'assets/lit-abc123.js.map',
    'assets/main-def456.css.map',
    'assets/main.js.map',
    'assets/workers/deep/pdf-ghi789.js.map',
  ]);
  for (const rel of moved) {
    assert.equal(existsSync(join(client, rel)), false, `${rel} left in client`);
    assert.equal(readFileSync(join(maps, rel), 'utf8'), BUILD[rel]);
  }
  // Non-map files are untouched.
  assert.equal(readFileSync(join(client, 'assets/main.js'), 'utf8'), BUILD['assets/main.js']);
  assert.ok(existsSync(join(client, '.vite/manifest.json')));
  assert.deepEqual(findSourcemapLeftovers(client), []);
});

test('empties the maps directory first so stale maps do not linger', (t) => {
  const { client, maps } = fixture(t, BUILD);
  mkdirSync(join(maps, 'assets'), { recursive: true });
  writeFileSync(join(maps, 'assets', 'old-zzz.js.map'), '{}');
  moveSourcemaps(client, maps);
  assert.equal(existsSync(join(maps, 'assets', 'old-zzz.js.map')), false);
});

test('fails when a bundle still carries a sourceMappingURL comment', (t) => {
  const { client, maps } = fixture(t, {
    ...BUILD,
    'assets/main.js': 'console.log(1);\n//# sourceMappingURL=main.js.map\n',
  });
  assert.throws(() => moveSourcemaps(client, maps), /assets\/main\.js: contains sourceMappingURL/);
});

test('fails when a stylesheet still carries a sourceMappingURL comment', (t) => {
  const { client, maps } = fixture(t, {
    ...BUILD,
    'assets/main-def456.css': 'body{}\n/*# sourceMappingURL=main-def456.css.map */\n',
  });
  assert.throws(() => moveSourcemaps(client, maps), /main-def456\.css: contains sourceMappingURL/);
});

test('fails when no source maps were generated', (t) => {
  const { client, maps } = fixture(t, { 'assets/main.js': 'console.log(1);\n' });
  assert.throws(() => moveSourcemaps(client, maps), /no source maps found/);
});

test('refuses overlapping directories', (t) => {
  const { root, client } = fixture(t, BUILD);
  assert.throws(() => moveSourcemaps(client, join(client, 'maps')), /overlap/);
  assert.throws(() => moveSourcemaps(client, client), /overlap/);
  assert.throws(() => moveSourcemaps(client, join(root, 'dist')), /overlap/);
  // Nothing was moved or deleted.
  assert.ok(existsSync(join(client, 'assets/main.js.map')));
});

test('findSourcemapLeftovers reports map files left behind', (t) => {
  const { client } = fixture(t, BUILD);
  assert.deepEqual(findSourcemapLeftovers(client), [
    'assets/lit-abc123.js.map: source map file',
    'assets/main-def456.css.map: source map file',
    'assets/main.js.map: source map file',
    'assets/workers/deep/pdf-ghi789.js.map: source map file',
  ]);
});

test('findSourcemapLeftovers reports precompressed map siblings', (t) => {
  const { client } = fixture(t, {
    'assets/main.js': 'console.log(1);\n',
    'assets/main.js.map.br': 'x',
    'assets/main.js.map.gz': 'x',
  });
  assert.deepEqual(findSourcemapLeftovers(client), [
    'assets/main.js.map.br: source map file',
    'assets/main.js.map.gz: source map file',
  ]);
});

test('fails on a symlinked .map, which is neither followed nor moved', (t) => {
  const { root, client, maps } = fixture(t, BUILD);
  writeFileSync(join(root, 'outside.js.map'), '{}');
  symlinkSync(join(root, 'outside.js.map'), join(client, 'assets', 'linked.js.map'));
  assert.throws(
    () => moveSourcemaps(client, maps),
    /assets\/linked\.js\.map: symbolic link \(not followed\)/
  );
  assert.ok(existsSync(join(root, 'outside.js.map')), 'link target untouched');
});

test('the leftover error points at stale files in public/assets', (t) => {
  const { client, maps } = fixture(t, {
    ...BUILD,
    'assets/old-zzz.js': 'x;\n//# sourceMappingURL=old-zzz.js.map\n',
  });
  assert.throws(() => moveSourcemaps(client, maps), /remove that directory and build again/);
});

test('code that only mentions sourceMappingURL is not a leftover', (t) => {
  const { client, maps } = fixture(t, {
    ...BUILD,
    'assets/lit-abc123.js': 'const k = "sourceMappingURL";\nexport { k };\n',
  });
  assert.equal(moveSourcemaps(client, maps).length, 4);
  assert.deepEqual(findSourcemapLeftovers(client), []);
});

test('fails on the legacy //@ sourceMappingURL comment', (t) => {
  const { client, maps } = fixture(t, {
    ...BUILD,
    'assets/main.js': 'console.log(1);\n//@ sourceMappingURL=main.js.map\n',
  });
  assert.throws(() => moveSourcemaps(client, maps), /assets\/main\.js: contains sourceMappingURL/);
});

test('CLI exits non-zero on a leftover and zero on a clean move', (t) => {
  const bad = fixture(t, { ...BUILD, 'assets/main.js': '//# sourceMappingURL=main.js.map\n' });
  const r1 = spawnSync(process.execPath, [SCRIPT, bad.client, bad.maps], { encoding: 'utf8' });
  assert.equal(r1.status, 1);
  assert.match(r1.stderr, /contains sourceMappingURL/);

  const good = fixture(t, BUILD);
  const r2 = spawnSync(process.execPath, [SCRIPT, good.client, good.maps], { encoding: 'utf8' });
  assert.equal(r2.status, 0, r2.stderr);
  assert.match(r2.stdout, /moved 4 source maps/);

  const r3 = spawnSync(process.execPath, [SCRIPT], { encoding: 'utf8' });
  assert.equal(r3.status, 2);
});
