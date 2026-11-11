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
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  checkBudget,
  closureFiles,
  findSourceMap,
  forbiddenIn,
  staticImportersOf,
  importsOutside,
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

test('forbiddenIn matches a full-path pattern, alone or with a base-name pattern', () => {
  const modules = [
    'src/client/main.ts',
    'node_modules/@milkdown/core/lib/index.js',
    'node_modules/prosemirror-view/dist/index.js',
    'node_modules/lit/index.js',
    'src/components/artifact-review/review-editor.ts',
    'src/components/artifact-review/review-editor-loader.ts',
    'src/client/terminal-coordinator.ts',
  ];
  const pathPattern = '(node_modules/(@milkdown/|prosemirror-))|src/components/artifact-review/';
  const loader = [
    { path: 'src/components/artifact-review/review-editor-loader.ts', reason: 'loader' },
  ];
  assert.deepEqual(forbiddenIn(modules, undefined, loader, pathPattern), [
    'node_modules/@milkdown/core/lib/index.js',
    'node_modules/prosemirror-view/dist/index.js',
    'src/components/artifact-review/review-editor.ts',
  ]);
  assert.deepEqual(forbiddenIn(modules, '^terminal-', loader, pathPattern), [
    'node_modules/@milkdown/core/lib/index.js',
    'node_modules/prosemirror-view/dist/index.js',
    'src/components/artifact-review/review-editor.ts',
    'src/client/terminal-coordinator.ts',
  ]);
  // The base-name pattern alone does not see package paths.
  assert.deepEqual(forbiddenIn(modules, '^index', [], undefined), [
    'node_modules/@milkdown/core/lib/index.js',
    'node_modules/prosemirror-view/dist/index.js',
    'node_modules/lit/index.js',
  ]);
});

test('a pathPattern rule fails a static closure that holds a matching module', () => {
  const b = budget({
    forbiddenModules: [
      {
        roots: ['src/components/pages/login.ts'],
        pathPattern: 'node_modules/@milkdown/',
      },
    ],
  });
  const { failures } = run({
    b,
    modules: { 'assets/lit-1.js': ['node_modules/@milkdown/core/lib/index.js'] },
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /static closure contains node_modules\/@milkdown\/core/);
});

test('a forbiddenModules rule without any pattern fails', () => {
  const b = budget({ forbiddenModules: [{ roots: ['src/components/pages/login.ts'] }] });
  const { failures } = run({ b });
  assert.deepEqual(failures, [
    'forbiddenModules rule for src/components/pages/login.ts: needs a pattern or a pathPattern',
  ]);
});

test('staticImportersOf finds static imports of a prefix from outside it, not dynamic ones', () => {
  const m = {
    'src/client/main.ts': {
      file: 'assets/main.js',
      imports: ['_lit.js'],
      dynamicImports: ['src/x.ts'],
    },
    'src/x.ts': { file: 'assets/x-1.js', dynamicImports: ['src/r.ts'] },
    'src/r.ts': { file: 'assets/review-editor-1.js', imports: ['_review-editor-2.js'] },
    '_review-editor-2.js': { file: 'assets/review-editor-2.js' },
    '_lit.js': { file: 'assets/lit-1.js' },
    '_codemirror.js': { file: 'assets/codemirror-1.js', imports: ['_review-editor-2.js'] },
  };
  assert.deepEqual(staticImportersOf(m, 'assets/review-editor-'), [
    { from: 'assets/codemirror-1.js', to: 'assets/review-editor-2.js' },
  ]);
  delete m['_codemirror.js'];
  assert.deepEqual(staticImportersOf(m, 'assets/review-editor-'), []);
});

test('an isolated chunk imported statically from outside fails', () => {
  const b = budget({ isolatedChunks: ['assets/lit-'] });
  const { failures } = run({ b });
  assert.ok(failures.length > 0);
  assert.match(failures[0], /imports assets\/lit-1\.js statically \(isolated assets\/lit-\*\)/);
});

test('importsOutside lists static imports of a prefix outside its allowed list, not dynamic ones', () => {
  const m = {
    'src/r.ts': {
      file: 'assets/review-editor-1.js',
      imports: ['_review-editor-2.js', '_lit.js'],
      dynamicImports: ['_xterm.js'],
    },
    '_review-editor-2.js': { file: 'assets/review-editor-2.js', imports: ['_xterm.js'] },
    '_lit.js': { file: 'assets/lit-1.js', imports: ['_xterm.js'] },
    '_xterm.js': { file: 'assets/xterm-1.js' },
  };
  const allowed = ['assets/review-editor-', 'assets/lit-'];
  assert.deepEqual(importsOutside(m, 'assets/review-editor-', allowed), [
    { from: 'assets/review-editor-2.js', to: 'assets/xterm-1.js' },
  ]);
  m['_review-editor-2.js'].imports = [];
  assert.deepEqual(importsOutside(m, 'assets/review-editor-', allowed), []);
});

test('a chunk importing a chunk outside its chunkImports list fails', () => {
  const ok = run({
    b: budget({ chunkImports: { 'assets/main': ['assets/lit-', 'assets/shoelace-'] } }),
  });
  assert.deepEqual(ok.failures, []);
  const { failures } = run({
    b: budget({ chunkImports: { 'assets/main': ['assets/shoelace-'] } }),
  });
  assert.equal(failures.length, 1);
  assert.match(
    failures[0],
    /assets\/main\.js imports assets\/lit-1\.js statically \(not allowed for assets\/main\*\)/
  );
});

test('a combined chunk limit adds up every chunk of the prefix', () => {
  const sizes = { 'assets/codemirror-1.js': 300, 'assets/codemirror-2.js': 300 };
  const files = ['assets/main.js', 'assets/codemirror-1.js', 'assets/codemirror-2.js'];
  const each = runChunks2(sizes, files, { raw: 500, gzip: 100000 });
  assert.deepEqual(each.failures, []);
  const together = runChunks2(sizes, files, { raw: 500, gzip: 100000, combined: true });
  assert.equal(together.failures.length, 1);
  assert.match(together.failures[0], /combined .*raw 600 B exceeds 500 B/);
});

test('a forbiddenModules rule can name the packages of a manual chunk', () => {
  const b = budget({
    manualChunks: { editor: 'node_modules/@milkdown/' },
    forbiddenModules: [{ roots: ['src/components/pages/login.ts'], chunkPackages: 'editor' }],
  });
  const { failures } = run({
    b,
    modules: { 'assets/lit-1.js': ['node_modules/@milkdown/core/lib/index.js'] },
  });
  assert.equal(failures.length, 1);
  assert.match(failures[0], /static closure contains node_modules\/@milkdown\/core/);
  const missing = budget({
    forbiddenModules: [{ roots: ['src/components/pages/login.ts'], chunkPackages: 'nope' }],
  });
  assert.match(run({ b: missing }).failures[0], /no manualChunks entry nope/);
});

/** Runs the chunk limits alone over files of the given sizes (others 10 bytes). */
function runChunks2(sizes, files, limit) {
  return checkBudget({
    manifest: { 'src/client/main.ts': { file: 'assets/main.js', imports: [] } },
    budget: { chunks: { 'assets/codemirror-': limit } },
    readFile: (f) => (files.includes(f) ? Buffer.alloc(sizes[f] ?? 10, 'a') : null),
    listFiles: () => files,
    readMapModules: () => [],
  });
}

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

/** checkBudget over a flat file list with the given raw sizes (filled, so gzip is tiny). */
function runChunks(sizes, chunks) {
  return checkBudget({
    manifest,
    budget: budget({
      files: {},
      entryClosure: undefined,
      requiredChunks: [],
      forbiddenModules: [],
      chunks,
    }),
    readFile: (f) => (f in sizes ? Buffer.alloc(sizes[f], 'a') : null),
    listFiles: () => Object.keys(sizes),
    readMapModules: () => [],
  });
}

test('chunk limits apply to every chunk matching the prefix', () => {
  const chunks = { $comment: 'why', 'assets/codemirror-': { raw: 1000, gzip: 1000 } };
  const sizes = {
    'assets/codemirror-a1.js': 1000,
    'assets/codemirror-b2.js': 1001,
    'assets/codemirror-a1.js.map': 5000,
    'assets/lang-go-c3.js': 5000,
  };
  const { failures, report } = runChunks(sizes, chunks);
  assert.deepEqual(failures, ['assets/codemirror-b2.js: raw 1001 B exceeds 1000 B']);
  assert.equal(report.filter((r) => r.startsWith('assets/codemirror-')).length, 2);
  assert.deepEqual(runChunks({ 'assets/codemirror-a1.js': 1000 }, chunks).failures, []);
});

test('fails when no chunk matches a sized prefix', () => {
  const { failures } = runChunks(
    { 'assets/lang-go-c3.js': 10 },
    { 'assets/codemirror-': { raw: 1000, gzip: 1000 } }
  );
  assert.deepEqual(failures, ['no chunk matching assets/codemirror-*.js']);
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

test('the entry is checked as a root even when no route chunk imports it', () => {
  // Same build, but login no longer lists main.ts among its imports: only the
  // main.ts root still sees a terminal module inlined into main.js.
  const detached = {
    ...manifest,
    'src/components/pages/login.ts': {
      ...manifest['src/components/pages/login.ts'],
      imports: ['_lit.js'],
    },
  };
  const all = Object.values(detached).flatMap((c) => [c.file, ...(c.css ?? [])]);
  const args = {
    manifest: detached,
    readFile: (f) => (all.includes(f) ? Buffer.alloc(100, 'a') : null),
    listFiles: () => all,
    readMapModules: (f) =>
      f === 'assets/main.js' ? ['src/client/main.ts', 'src/client/terminal-coordinator.ts'] : [],
  };
  const loginOnly = budget({
    forbiddenModules: [{ roots: ['src/components/pages/login.ts'], pattern: '^terminal-', allow }],
  });
  assert.deepEqual(checkBudget({ ...args, budget: loginOnly }).failures, []);
  const withEntry = budget({
    forbiddenModules: [
      {
        roots: ['src/client/main.ts', 'src/components/pages/login.ts'],
        pattern: '^terminal-',
        allow,
      },
    ],
  });
  assert.deepEqual(checkBudget({ ...args, budget: withEntry }).failures, [
    'src/client/main.ts: static closure contains src/client/terminal-coordinator.ts (in assets/main.js)',
  ]);
});

test('findSourceMap prefers the map next to the chunk, then dist/sourcemaps, else null', () => {
  const root = mkdtempSync(join(tmpdir(), 'bundle-budget-'));
  try {
    const dist = join(root, 'dist', 'client');
    const moved = join(root, 'dist', 'sourcemaps');
    mkdirSync(join(dist, 'assets'), { recursive: true });
    mkdirSync(join(moved, 'assets'), { recursive: true });
    writeFileSync(join(dist, 'assets', 'both.js.map'), '{}');
    writeFileSync(join(moved, 'assets', 'both.js.map'), '{}');
    writeFileSync(join(moved, 'assets', 'moved.js.map'), '{}');

    assert.equal(findSourceMap(dist, 'assets/both.js'), join(dist, 'assets', 'both.js.map'));
    assert.equal(findSourceMap(dist, 'assets/moved.js'), join(moved, 'assets', 'moved.js.map'));
    assert.equal(findSourceMap(dist, 'assets/none.js'), null);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('requiredStrings: each marker occurs exactly once in the build, in a chunk of the prefix', () => {
  const texts = {
    'assets/review-editor-a1.js': 'x("raw-html");x("twice");x("twice")',
    'assets/review-editor-b2.js': 'new P("criticPolicy")',
    'assets/main.js': 'image-placeholder;criticPolicy-copy-elsewhere',
  };
  const check = (requiredStrings, files = Object.keys(texts)) =>
    checkBudget({
      manifest,
      budget: budget({
        files: {},
        entryClosure: undefined,
        requiredChunks: [],
        forbiddenModules: [],
        requiredStrings,
      }),
      readFile: (f) => (f in texts ? Buffer.from(texts[f]) : null),
      listFiles: () => files,
      readMapModules: () => [],
    });
  // Strings from either chunk of the prefix count; comment keys are skipped.
  const ok = check({ $comment: 'why', 'assets/review-editor-': ['raw-html'] });
  assert.deepEqual(ok.failures, []);
  assert.ok(
    ok.report.includes(
      'required strings in assets/review-editor-*.js: 1 of 1 present, each exactly once in the build'
    )
  );
  // Missing; only in another chunk; twice; once in the prefix and once elsewhere.
  assert.deepEqual(
    check({
      'assets/review-editor-': ['nowhere', 'image-placeholder', 'twice', 'criticPolicy'],
    }).failures,
    [
      'assets/review-editor-*.js: missing required string "nowhere"',
      'assets/review-editor-*.js: required string "image-placeholder" occurs 1 time(s), in assets/main.js; EXPECT once, in assets/review-editor-*.js',
      'assets/review-editor-*.js: required string "twice" occurs 2 time(s), in assets/review-editor-a1.js; EXPECT once, in assets/review-editor-*.js',
      'assets/review-editor-*.js: required string "criticPolicy" occurs 2 time(s), in assets/review-editor-b2.js, assets/main.js; EXPECT once, in assets/review-editor-*.js',
    ]
  );
  // No chunk of the prefix at all.
  assert.deepEqual(check({ 'assets/review-editor-': ['raw-html'] }, ['assets/main.js']).failures, [
    'assets/review-editor-*.js: missing required string "raw-html"',
    'no chunk matching assets/review-editor-*.js',
  ]);
});
