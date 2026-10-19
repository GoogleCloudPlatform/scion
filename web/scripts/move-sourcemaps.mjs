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
 * It fails closed: it exits 1 if any .map file is left under `<clientDir>`,
 * if any .js or .css file there still mentions `sourceMappingURL`, or if no
 * map was found at all (map generation broke). `npm run build` chains it with
 * `&&`, so a failure fails the build.
 *
 * Usage: node scripts/move-sourcemaps.mjs <clientDir> <mapsDir>
 *        (npm run build passes dist/client dist/sourcemaps)
 */

import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, rmSync } from 'node:fs';
import { dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

/** Lists every regular file under dir, recursively, as absolute paths. */
function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...walk(p));
    } else if (entry.isFile()) {
      out.push(p);
    }
  }
  return out;
}

function isInside(child, parent) {
  const rel = relative(parent, child);
  return rel === '' || (rel !== '..' && !rel.startsWith(`..${sep}`) && !isAbsolute(rel));
}

/**
 * Returns the problems that would leave source maps reachable from clientDir:
 * any remaining .map file, and any .js/.css file mentioning sourceMappingURL.
 * Paths are relative to clientDir.
 */
export function findSourcemapLeaks(clientDir) {
  const root = resolve(clientDir);
  const leaks = [];
  for (const file of walk(root)) {
    const rel = relative(root, file);
    if (file.endsWith('.map')) {
      leaks.push(`${rel}: source map file`);
    } else if (
      /\.(m?js|css)$/.test(file) &&
      readFileSync(file, 'utf8').includes('sourceMappingURL')
    ) {
      leaks.push(`${rel}: contains sourceMappingURL`);
    }
  }
  return leaks.sort();
}

/**
 * Moves every .map file under clientDir to the same relative path under
 * mapsDir (emptied first, so maps from earlier builds do not linger).
 * Returns the moved paths, relative to clientDir. Throws if mapsDir and
 * clientDir overlap, if no map was found, or if any leak remains afterwards.
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
  for (const file of walk(client)) {
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

  const leaks = findSourcemapLeaks(client);
  if (leaks.length > 0) {
    throw new Error(`source map references remain under ${client}:\n  ${leaks.join('\n  ')}`);
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
