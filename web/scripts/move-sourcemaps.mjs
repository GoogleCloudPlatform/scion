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
 * Moves the build's source maps out of the served client tree.
 *
 * Vite builds with `sourcemap: 'hidden'`: it writes the .map files but leaves
 * no `sourceMappingURL` comment in the bundles. This script moves every
 * `<clientDir>/**\/*.map` to the same relative path under `<mapsDir>`, which
 * sits outside the directory the Go binary embeds (web/embed.go embeds
 * dist/client) and the release images copy. CI uploads `<mapsDir>` as a
 * workflow artifact for debugging and bundle attribution.
 *
 * It fails closed: it exits 1 if any .map (or .map.br/.map.gz) file or any
 * symbolic link is left under `<clientDir>`, if any .js or .css file there
 * still has a `sourceMappingURL` comment, or if no
 * map was found at all (map generation broke). `npm run build` chains it with
 * `&&`, so a failure fails the build.
 *
 * Usage: node scripts/move-sourcemaps.mjs <clientDir> <mapsDir>
 *        (npm run build passes dist/client dist/sourcemaps)
 */

import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync } from 'node:fs';
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Lists every entry under dir, recursively, as absolute paths. Directories
 * are descended into; symbolic links are listed but never followed.
 */
function walk(dir, out = { files: [], symlinks: [] }) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isSymbolicLink()) {
      out.symlinks.push(p);
    } else if (entry.isDirectory()) {
      walk(p, out);
    } else if (entry.isFile()) {
      out.files.push(p);
    }
  }
  return out;
}

// A source map, also precompressed (in case a build step that writes .br/.gz
// siblings ever runs before the move).
const MAP_FILE = /\.map(\.br|\.gz)?$/;

// The comment that points a bundle at its map: //# sourceMappingURL=... or
// /*# sourceMappingURL=... */ (also the legacy @ form). Matching the comment
// form, not the bare word, avoids failing on code that merely mentions it.
const MAP_COMMENT = /[#@]\s*sourceMappingURL=/;

function isInside(child, parent) {
  const rel = relative(parent, child);
  return rel === '' || (rel !== '..' && !rel.startsWith(`..${sep}`) && !isAbsolute(rel));
}

/**
 * Returns the problems that would leave source maps reachable from clientDir:
 * any remaining .map (or .map.br/.map.gz) file, any .js/.css file with a
 * sourceMappingURL comment, and any symbolic link (not followed, so whatever
 * it points at cannot be checked). Paths are relative to clientDir.
 */
export function findSourcemapLeftovers(clientDir) {
  const root = resolve(clientDir);
  const leftovers = [];
  const { files, symlinks } = walk(root);
  for (const link of symlinks) {
    leftovers.push(`${relative(root, link)}: symbolic link (not followed)`);
  }
  for (const file of files) {
    const rel = relative(root, file);
    if (MAP_FILE.test(file)) {
      leftovers.push(`${rel}: source map file`);
    } else if (/\.(m?js|css)$/.test(file) && MAP_COMMENT.test(readFileSync(file, 'utf8'))) {
      leftovers.push(`${rel}: contains sourceMappingURL`);
    }
  }
  return leftovers.sort();
}

/**
 * Moves every .map file under clientDir to the same relative path under
 * mapsDir (emptied first, so maps from earlier builds do not linger).
 * Returns the moved paths, relative to clientDir. Throws if mapsDir and
 * clientDir overlap, if no map was found, or if anything is left behind afterwards.
 */
export function moveSourcemaps(clientDir, mapsDir) {
  const client = resolve(clientDir);
  const maps = resolve(mapsDir);
  if (isInside(maps, client) || isInside(client, maps)) {
    throw new Error(`refusing: ${maps} and ${client} overlap`);
  }
  if (!existsSync(client)) {
    throw new Error(`client directory ${client} does not exist`);
  }

  rmSync(maps, { recursive: true, force: true });
  mkdirSync(maps, { recursive: true });

  const moved = [];
  for (const file of walk(client).files) {
    if (!file.endsWith('.map')) continue;
    const rel = relative(client, file);
    const dest = join(maps, rel);
    mkdirSync(dirname(dest), { recursive: true });
    renameSync(file, dest);
    moved.push(rel);
  }
  if (moved.length === 0) {
    throw new Error(`no source maps found under ${client}; is build.sourcemap still 'hidden'?`);
  }

  const leftovers = findSourcemapLeftovers(client);
  if (leftovers.length > 0) {
    throw new Error(
      `source map files or references left behind under ${client}:\n  ${leftovers.join('\n  ')}\n` +
        'Vite copies public/ into the build: if web/public/assets holds chunks from ' +
        'an earlier build, remove that directory and build again.'
    );
  }
  return moved.sort();
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [clientDir, mapsDir] = process.argv.slice(2);
  if (!clientDir || !mapsDir) {
    console.error('usage: node scripts/move-sourcemaps.mjs <clientDir> <mapsDir>');
    process.exit(2);
  }
  try {
    const moved = moveSourcemaps(clientDir, mapsDir);
    console.log(
      `move-sourcemaps: moved ${moved.length} source maps from ${clientDir} to ${mapsDir}`
    );
  } catch (err) {
    console.error(`move-sourcemaps: ${err.message}`);
    process.exit(1);
  }
}
