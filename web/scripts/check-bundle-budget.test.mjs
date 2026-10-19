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

// Unit tests for check-bundle-budget.mjs: node --test scripts/check-bundle-budget.test.mjs

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';

import {
  checkBudget,
  closureFiles,
  forbiddenIn,
  mapSources,
  staticClosure,
} from './check-bundle-budget.mjs';

const EVENTS = 'src/client/terminal-workspace-events.ts';
const LOADER = 'src/client/terminal-workspace-loader.ts';

const manifest = {
  'src/client/main.ts': {
    file: 'assets/main.js',
    isEntry: true,
    imports: ['_lit.js', '_shoelace.js'],
    dynamicImports: ['src/components/pages/login.ts', 'src/client/terminal-workspace-entry.ts'],
  },
  '_lit.js': { file: 'assets/lit-1.js' },
  '_shoelace.js': {
    file: 'assets/shoelace-1.js',
    imports: ['_lit.js'],
    css: ['assets/shoelace-1.css'],
  },
  'src/components/pages/login.ts': {
    file: 'assets/login-1.js',
    isDynamicEntry: true,
    imports: ['src/client/main.ts', '_lit.js'],
  },
  'src/client/terminal-workspace-entry.ts': {
    file: 'assets/terminal-workspace-entry-1.js',
    isDynamicEntry: true,
    imports: ['src/client/main.ts'],
  },
};

const allow = [
  { path: EVENTS, reason: 'constants only' },
  { path: LOADER, reason: 'the loader' },
];

function budget(overrides = {}) {
  return {
    files: { 'assets/main.js': { raw: 1000, gzip: 1000 } },
    entryClosure: { entry: 'src/client/main.ts', gzip: 10_000 },
    requiredChunks: ['assets/terminal-workspace-'],
    forbiddenModules: [{ roots: ['src/components/pages/login.ts'], pattern: '^terminal-', allow }],
    ...overrides,
  };
}

function run({ sizes = {}, modules = {}, b = budget(), files } = {}) {
  const all = Object.values(manifest).flatMap((c) => [c.file, ...(c.css ?? [])]);
  return checkBudget({
    manifest,
    budget: b,
    readFile: (f) => (all.includes(f) ? Buffer.alloc(sizes[f] ?? 100, 'a') : null),
    listFiles: () => files ?? all,
    readMapModules: (f) => (f in modules ? modules[f] : ['src/client/main.ts']),
  });
}

test('staticClosure follows static imports only, once each', () => {
  assert.deepEqual(staticClosure(manifest, 'src/components/pages/login.ts'), [
    'src/components/pages/login.ts',
    'src/client/main.ts',
    '_lit.js',
    '_shoelace.js',
  ]);
  assert.throws(() => staticClosure(manifest, 'nope.ts'), /no entry nope\.ts/);
});

test('closureFiles includes chunk CSS', () => {
  assert.deepEqual(closureFiles(manifest, staticClosure(manifest, 'src/client/main.ts')), [
    'assets/main.js',
    'assets/lit-1.js',
    'assets/shoelace-1.js',
    'assets/shoelace-1.css',
  ]);
});

test('mapSources resolves sources against the map location and sourceRoot', () => {
  const web = '/w/web';
  const map = { sources: ['../../../src/client/main.ts', '../../../node_modules/lit/index.js'] };
  assert.deepEqual(mapSources(map, '/w/web/dist/client/assets/main.js.map', web), [
    'src/client/main.ts',
    'node_modules/lit/index.js',
  ]);
  // A map moved to dist/sourcemaps/assets/ keeps the same depth.
  assert.deepEqual(
    mapSources(map, '/w/web/dist/sourcemaps/assets/main.js.map', web)[0],
    'src/client/main.ts'
  );
  assert.deepEqual(
    mapSources(
      { sourceRoot: '../../../', sources: ['src/a.ts'] },
      '/w/web/dist/client/assets/a.js.map',
      web
    ),
    ['src/a.ts']
  );
});

test('forbiddenIn matches the base name and honours exact allowlist paths only', () => {
  const modules = [
    'src/client/main.ts',
    EVENTS,
    LOADER,
    'src/client/terminal-coordinator.ts',
    'src/components/terminal/terminal-pane.ts',
    'src/client/open-terminal.ts',
    'src/other/terminal-workspace-events.ts',
  ];
  assert.deepEqual(forbiddenIn(modules, '^terminal-', allow), [
    'src/client/terminal-coordinator.ts',
    'src/components/terminal/terminal-pane.ts',
    'src/other/terminal-workspace-events.ts',
  ]);
});

test('passes within budget', () => {
  const { failures } = run();
  assert.deepEqual(failures, []);
});

test('fails when main.js exceeds its raw budget', () => {
  const { failures } = run({ sizes: { 'assets/main.js': 1001 } });
  assert.deepEqual(failures, ['assets/main.js: raw 1001 B exceeds 1000 B']);
});

test('fails when main.js exceeds its gzip budget', () => {
  const big = randomBytes(5000);
  const { failures } = checkBudget({
    manifest,
    budget: budget({ files: { 'assets/main.js': { raw: 10_000, gzip: 1000 } } }),
    readFile: (f) => (f === 'assets/main.js' ? big : Buffer.alloc(10)),
    listFiles: () => ['assets/terminal-workspace-entry-1.js'],
    readMapModules: () => [],
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /^assets\/main\.js: gzip \d+ B exceeds 1000 B$/);
});

test('fails when the entry closure exceeds its gzip budget, counting CSS', () => {
  const big = randomBytes(3000);
  const read = (f) => (f === 'assets/shoelace-1.css' ? big : Buffer.alloc(10));
  const args = {
    manifest,
    listFiles: () => ['assets/terminal-workspace-entry-1.js'],
    readMapModules: () => [],
    readFile: read,
  };
  const ok = checkBudget({
    ...args,
    budget: budget({ files: {}, entryClosure: { entry: 'src/client/main.ts', gzip: 4000 } }),
  });
  assert.deepEqual(ok.failures, []);
  const over = checkBudget({
    ...args,
    budget: budget({ files: {}, entryClosure: { entry: 'src/client/main.ts', gzip: 2000 } }),
  });
  assert.equal(over.failures.length, 1);
  assert.match(
    over.failures[0],
    /^entry closure of src\/client\/main\.ts: gzip \d+ B exceeds 2000 B$/
  );
});

test('fails when no terminal-workspace chunk was emitted', () => {
  const { failures } = run({ files: ['assets/main.js', 'assets/login-1.js'] });
  assert.deepEqual(failures, ['no chunk matching assets/terminal-workspace-*.js']);
});

test('allowlisted terminal modules in the login closure pass', () => {
  const { failures } = run({
    modules: { 'assets/main.js': ['src/client/main.ts', EVENTS, LOADER] },
  });
  assert.deepEqual(failures, []);
});

test('any other terminal module in the login closure fails', () => {
  const { failures } = run({
    modules: {
      'assets/main.js': ['src/client/main.ts', EVENTS, 'src/client/terminal-coordinator.ts'],
    },
  });
  assert.deepEqual(failures, [
    'src/components/pages/login.ts: static closure contains src/client/terminal-coordinator.ts (in assets/main.js)',
  ]);
});

test('terminal modules in a lazy chunk are not in the static closure', () => {
  const { failures } = run({
    modules: { 'assets/terminal-workspace-entry-1.js': ['src/client/terminal-coordinator.ts'] },
  });
  assert.deepEqual(failures, []);
});

test('a chunk without a source map fails the module check', () => {
  const { failures } = run({ modules: { 'assets/lit-1.js': null } });
  assert.deepEqual(failures, [
    'assets/lit-1.js: no source map, cannot check modules in the closure of src/components/pages/login.ts',
  ]);
});

test('allowlist entries must be exact paths with a reason', () => {
  const b = budget({
    forbiddenModules: [
      {
        roots: ['src/components/pages/login.ts'],
        pattern: '^terminal-',
        allow: [{ path: 'src/client/terminal-*.ts', reason: 'glob' }, { path: EVENTS }],
      },
    ],
  });
  const { failures } = run({ b });
  assert.equal(failures.length, 2);
  assert.match(failures[0], /needs an exact path and a reason/);
  assert.match(failures[1], /needs an exact path and a reason/);
});
