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

// node:test coverage for clean-public-generated.mjs. Run: npm run test:build-scripts

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync, execFileSync } from 'child_process';
import {
  mkdtempSync,
  mkdirSync,
  writeFileSync,
  existsSync,
  rmSync,
  symlinkSync,
  chmodSync,
} from 'fs';
import { tmpdir } from 'os';
import { join, resolve, dirname } from 'path';
import { fileURLToPath } from 'url';
import { cleanPublicGenerated } from './clean-public-generated.mjs';

const SCRIPT = resolve(dirname(fileURLToPath(import.meta.url)), 'clean-public-generated.mjs');

// A temp dir outside any git checkout. GIT_CEILING_DIRECTORIES stops git from
// finding a repo above the temp root (for example if TMPDIR is inside one).
function tempRoot(t) {
  const dir = mkdtempSync(join(tmpdir(), 'clean-pub-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function withCeiling(t, dir) {
  const prev = process.env.GIT_CEILING_DIRECTORIES;
  process.env.GIT_CEILING_DIRECTORIES = dirname(dir);
  t.after(() => {
    if (prev === undefined) delete process.env.GIT_CEILING_DIRECTORIES;
    else process.env.GIT_CEILING_DIRECTORIES = prev;
  });
}

function seedAssets(webRoot) {
  mkdirSync(join(webRoot, 'public/assets/nested'), { recursive: true });
  writeFileSync(join(webRoot, 'public/assets/main-old.js'), '// stale');
  writeFileSync(join(webRoot, 'public/assets/nested/chunk.js'), '// stale');
  writeFileSync(join(webRoot, 'public/favicon.svg'), '<svg/>');
}

function git(cwd, ...args) {
  execFileSync(
    'git',
    ['-c', 'user.email=t@example.com', '-c', 'user.name=t', '-c', 'commit.gpgsign=false', ...args],
    {
      cwd,
      stdio: 'pipe',
    }
  );
}

test('no public/assets: no-op', (t) => {
  const root = tempRoot(t);
  withCeiling(t, root);
  assert.deepEqual(cleanPublicGenerated(root), { removed: false, tracked: [] });
});

test('outside a git checkout: removes public/assets, keeps the rest of public/', (t) => {
  const root = tempRoot(t);
  withCeiling(t, root);
  seedAssets(root);
  assert.deepEqual(cleanPublicGenerated(root), { removed: true, tracked: [] });
  assert.equal(existsSync(join(root, 'public/assets')), false);
  assert.equal(existsSync(join(root, 'public/favicon.svg')), true);
});

test('git checkout with untracked debris: removes public/assets', (t) => {
  const root = tempRoot(t);
  withCeiling(t, root);
  git(root, 'init', '-q');
  seedAssets(root);
  git(root, 'add', 'public/favicon.svg');
  git(root, 'commit', '-q', '-m', 'init');
  assert.deepEqual(cleanPublicGenerated(root), { removed: true, tracked: [] });
  assert.equal(existsSync(join(root, 'public/assets')), false);
  assert.equal(existsSync(join(root, 'public/favicon.svg')), true);
});

test('git tracks a file under public/assets: refuses and deletes nothing', (t) => {
  const root = tempRoot(t);
  withCeiling(t, root);
  git(root, 'init', '-q');
  seedAssets(root);
  git(root, 'add', 'public/assets/nested/chunk.js');
  git(root, 'commit', '-q', '-m', 'track one');
  const res = cleanPublicGenerated(root);
  assert.deepEqual(res, { removed: false, tracked: ['public/assets/nested/chunk.js'] });
  assert.equal(existsSync(join(root, 'public/assets/main-old.js')), true);
  assert.equal(existsSync(join(root, 'public/assets/nested/chunk.js')), true);
});

test('webRoot is a subdirectory of the checkout (like web/): refuses on tracked file', (t) => {
  const repo = tempRoot(t);
  withCeiling(t, repo);
  git(repo, 'init', '-q');
  const root = join(repo, 'web');
  seedAssets(root);
  git(repo, 'add', 'web/public/assets/main-old.js');
  git(repo, 'commit', '-q', '-m', 'track one');
  assert.deepEqual(cleanPublicGenerated(root).tracked, ['public/assets/main-old.js']);
  assert.equal(existsSync(join(root, 'public/assets/nested/chunk.js')), true);
});

test('CLI: exits 1 and lists the tracked file on refusal, 0 after removal', (t) => {
  const root = tempRoot(t);
  withCeiling(t, root);
  git(root, 'init', '-q');
  seedAssets(root);
  git(root, 'add', 'public/assets/main-old.js');
  git(root, 'commit', '-q', '-m', 'track one');
  const refused = spawnSync(process.execPath, [SCRIPT, root], { encoding: 'utf8' });
  assert.equal(refused.status, 1);
  assert.match(refused.stderr, /refusing to delete public\/assets\//);
  assert.match(refused.stderr, /public\/assets\/main-old\.js/);
  assert.equal(existsSync(join(root, 'public/assets/main-old.js')), true);

  git(root, 'rm', '-q', '--cached', 'public/assets/main-old.js');
  git(root, 'commit', '-q', '-m', 'untrack');
  const ok = spawnSync(process.execPath, [SCRIPT, root], { encoding: 'utf8' });
  assert.equal(ok.status, 0, ok.stderr);
  assert.match(ok.stdout, /removed public\/assets\//);
  assert.equal(existsSync(join(root, 'public/assets')), false);
});

test('public/ is a symlink to a tracked dir: refuses and deletes nothing', (t) => {
  const repo = tempRoot(t);
  withCeiling(t, repo);
  git(repo, 'init', '-q');
  mkdirSync(join(repo, 'real/assets'), { recursive: true });
  writeFileSync(join(repo, 'real/assets/a.js'), '// tracked');
  git(repo, 'add', 'real/assets/a.js');
  git(repo, 'commit', '-q', '-m', 'track');
  const root = join(repo, 'web');
  mkdirSync(root);
  symlinkSync('../real', join(root, 'public'));
  assert.throws(() => cleanPublicGenerated(root), /public\/ resolves through a symlink/);
  assert.equal(existsSync(join(repo, 'real/assets/a.js')), true);
});

test('public/assets itself is a symlink: removes the link, not its target', (t) => {
  const root = tempRoot(t);
  withCeiling(t, root);
  git(root, 'init', '-q');
  mkdirSync(join(root, 'public'));
  mkdirSync(join(root, 'elsewhere'));
  writeFileSync(join(root, 'elsewhere/keep.js'), '// keep');
  symlinkSync('../elsewhere', join(root, 'public/assets'));
  assert.deepEqual(cleanPublicGenerated(root), { removed: true, tracked: [] });
  assert.equal(existsSync(join(root, 'public/assets')), false);
  assert.equal(existsSync(join(root, 'elsewhere/keep.js')), true);
});

test('CLI: git not installed (empty PATH): proceeds and removes public/assets', (t) => {
  const root = tempRoot(t);
  seedAssets(root);
  const emptyBin = join(root, 'empty-bin');
  mkdirSync(emptyBin);
  const res = spawnSync(process.execPath, [SCRIPT, root], {
    encoding: 'utf8',
    env: { ...process.env, PATH: emptyBin },
  });
  assert.equal(res.status, 0, res.stderr);
  assert.equal(existsSync(join(root, 'public/assets')), false);
});

test('CLI: unexpected git failure fails closed and deletes nothing', (t) => {
  const root = tempRoot(t);
  seedAssets(root);
  const bin = join(root, 'stub-bin');
  mkdirSync(bin);
  writeFileSync(join(bin, 'git'), '#!/bin/sh\necho "fatal: boom" >&2\nexit 128\n');
  chmodSync(join(bin, 'git'), 0o755);
  const res = spawnSync(process.execPath, [SCRIPT, root], {
    encoding: 'utf8',
    env: { ...process.env, PATH: bin },
  });
  assert.equal(res.status, 1);
  assert.match(res.stderr, /git ls-files failed \(exit 128\): fatal: boom/);
  assert.equal(existsSync(join(root, 'public/assets/main-old.js')), true);
});
