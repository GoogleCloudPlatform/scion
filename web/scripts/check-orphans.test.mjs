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

// node:test coverage for check-orphans.mjs. Run: npm run test:build-scripts

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'child_process';
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'fs';
import { tmpdir } from 'os';
import { join, resolve, dirname } from 'path';
import { fileURLToPath } from 'url';
import { findOrphans } from './check-orphans.mjs';

const SCRIPT = resolve(dirname(fileURLToPath(import.meta.url)), 'check-orphans.mjs');

// Shape of a Vite manifest: one entry, one dynamic route chunk, one shared
// vendor chunk (key starts with "_"), CSS listed on the entry.
const MANIFEST = {
  'src/client/main.ts': {
    file: 'assets/main.js',
    isEntry: true,
    imports: ['_lit-AbC123.js'],
    dynamicImports: ['src/components/pages/agents.ts'],
    css: ['assets/main-Xyz.css'],
  },
  'src/components/pages/agents.ts': {
    file: 'assets/agents-Def456.js',
    isDynamicEntry: true,
    imports: ['_lit-AbC123.js'],
  },
  '_lit-AbC123.js': { file: 'assets/lit-AbC123.js' },
};

function clientDir(t, files, manifest = MANIFEST) {
  const dir = mkdtempSync(join(tmpdir(), 'orphans-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  mkdirSync(join(dir, '.vite'));
  mkdirSync(join(dir, 'assets'));
  writeFileSync(join(dir, '.vite/manifest.json'), JSON.stringify(manifest));
  for (const f of files) writeFileSync(join(dir, f), '');
  return dir;
}

const CLEAN = [
  'assets/main.js',
  'assets/agents-Def456.js',
  'assets/lit-AbC123.js',
  'assets/main.js.map',
  'assets/main-Xyz.css',
];

test('clean build: no orphans (maps and css are ignored)', (t) => {
  assert.deepEqual(findOrphans(clientDir(t, CLEAN)), []);
});

test('stale chunks from an earlier build are reported, sorted', (t) => {
  const dir = clientDir(t, [...CLEAN, 'assets/lit-OLD999.js', 'assets/agents-OLD111.js']);
  assert.deepEqual(findOrphans(dir), ['assets/agents-OLD111.js', 'assets/lit-OLD999.js']);
});

test('missing manifest throws', (t) => {
  const dir = clientDir(t, CLEAN);
  rmSync(join(dir, '.vite'), { recursive: true });
  assert.throws(() => findOrphans(dir), /ENOENT/);
});

test('manifest without any .js file throws', (t) => {
  const dir = clientDir(t, CLEAN, { 'x.css': { file: 'assets/x.css' } });
  assert.throws(() => findOrphans(dir), /references no \.js file/);
});

test('CLI: exit 1 listing orphans, exit 0 when clean', (t) => {
  const bad = spawnSync(process.execPath, [SCRIPT, clientDir(t, [...CLEAN, 'assets/stale.js'])], {
    encoding: 'utf8',
  });
  assert.equal(bad.status, 1);
  assert.match(bad.stderr, /1 \.js file\(s\) not referenced/);
  assert.match(bad.stderr, /assets\/stale\.js/);
  const ok = spawnSync(process.execPath, [SCRIPT, clientDir(t, CLEAN)], { encoding: 'utf8' });
  assert.equal(ok.status, 0, ok.stderr);
});
