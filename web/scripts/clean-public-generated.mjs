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

/**
 * Removes web/public/assets/, the debris left by the old copy:client step.
 *
 * copy:client copied dist/client/assets/* into public/assets/, and Vite's
 * publicDir copied public/ back into dist/client on the next build, so every
 * build's chunks piled up in later builds (#3842). The copy is gone; this
 * script runs as the prebuild hook to clean trees that still hold the debris.
 *
 * It refuses to delete anything if git tracks a file under public/assets/, or
 * if public/assets resolves through a symlinked parent (git would not see the
 * tracked files behind the link). When git is not installed or the tree is not
 * a git checkout (for example the Docker frontend stage), no file can be
 * tracked, so it proceeds. Any other git failure fails closed.
 *
 * Usage: node scripts/clean-public-generated.mjs [webRoot]
 */

import { spawnSync } from 'child_process';
import { existsSync, realpathSync, rmSync } from 'fs';
import { resolve, dirname } from 'path';
import { fileURLToPath } from 'url';

const TARGET = 'public/assets';

/**
 * Lists git-tracked files under dir (relative to webRoot).
 * Returns [] when git is missing or webRoot is not inside a git work tree.
 * Throws on any other git failure so the caller does not delete blindly.
 */
export function trackedFiles(webRoot, dir) {
  // LC_ALL=C keeps git's error text untranslated for the check below.
  const res = spawnSync('git', ['ls-files', '-z', '--', dir], {
    cwd: webRoot,
    encoding: 'utf8',
    env: { ...process.env, LC_ALL: 'C', LANGUAGE: '' },
  });
  if (res.error) {
    if (res.error.code === 'ENOENT') return [];
    throw res.error;
  }
  if (res.status !== 0) {
    if (/not a git repository/i.test(res.stderr)) return [];
    throw new Error(`git ls-files failed (exit ${res.status}): ${res.stderr.trim()}`);
  }
  return res.stdout.split('\0').filter(Boolean);
}

/**
 * Removes webRoot/public/assets unless git tracks a file in it.
 * Returns { removed: boolean, tracked: string[] }. Throws if the directory
 * resolves through a symlinked parent, or on an unexpected git failure.
 */
export function cleanPublicGenerated(webRoot) {
  const target = resolve(webRoot, TARGET);
  if (!existsSync(target)) return { removed: false, tracked: [] };
  // A symlink at public/assets itself is fine: git lists it if tracked, and
  // rmSync unlinks it without following it. A symlinked parent (public/ ->
  // elsewhere) hides tracked files from git ls-files, so refuse that.
  const parent = realpathSync(dirname(target));
  if (parent !== resolve(realpathSync(webRoot), dirname(TARGET))) {
    throw new Error(
      `refusing to delete ${TARGET}/: public/ resolves through a symlink to ${parent}`
    );
  }
  const tracked = trackedFiles(webRoot, TARGET);
  if (tracked.length > 0) return { removed: false, tracked };
  rmSync(target, { recursive: true, force: true });
  return { removed: true, tracked: [] };
}

function main() {
  const defaultRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
  const webRoot = resolve(process.argv[2] ?? defaultRoot);
  let result;
  try {
    result = cleanPublicGenerated(webRoot);
  } catch (err) {
    console.error(`clean-public-generated: ${err.message}`);
    process.exit(1);
  }
  if (result.tracked.length > 0) {
    console.error(
      `clean-public-generated: refusing to delete ${TARGET}/: git tracks ${result.tracked.length} file(s) in it:`
    );
    for (const f of result.tracked) console.error(`  ${f}`);
    process.exit(1);
  }
  if (result.removed) console.log(`clean-public-generated: removed ${TARGET}/`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
