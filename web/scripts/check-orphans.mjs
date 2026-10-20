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
 * Fails if dist/client/assets holds a .js file that the Vite manifest
 * (dist/client/.vite/manifest.json) does not reference. Such files are stale
 * chunks from earlier builds (#3842) that would be embedded in the hub binary.
 *
 * Run after a build: npm run check:orphans [-- clientDir]
 */

import { readFileSync, readdirSync } from 'fs';
import { resolve, dirname } from 'path';
import { fileURLToPath } from 'url';

/**
 * Returns the sorted list of .js files directly in clientDir/assets that no
 * manifest entry's `file` names. Throws if the manifest or assets dir is
 * missing or the manifest references no .js file.
 */
export function findOrphans(clientDir) {
  const manifest = JSON.parse(readFileSync(resolve(clientDir, '.vite/manifest.json'), 'utf8'));
  const referenced = new Set();
  for (const entry of Object.values(manifest)) {
    if (entry && typeof entry.file === 'string') referenced.add(entry.file);
  }
  if (![...referenced].some((f) => f.endsWith('.js'))) {
    throw new Error('manifest references no .js file');
  }
  return readdirSync(resolve(clientDir, 'assets'), { withFileTypes: true })
    .filter((d) => d.isFile() && d.name.endsWith('.js'))
    .map((d) => `assets/${d.name}`)
    .filter((f) => !referenced.has(f))
    .sort();
}

function main() {
  const defaultDir = resolve(dirname(fileURLToPath(import.meta.url)), '../dist/client');
  const clientDir = resolve(process.argv[2] ?? defaultDir);
  let orphans;
  try {
    orphans = findOrphans(clientDir);
  } catch (err) {
    console.error(`check-orphans: ${err.message}`);
    process.exit(1);
  }
  if (orphans.length > 0) {
    console.error(`check-orphans: ${orphans.length} .js file(s) not referenced by the manifest:`);
    for (const f of orphans) console.error(`  ${f}`);
    process.exit(1);
  }
  console.log('check-orphans: OK, every .js in assets/ is referenced by the manifest');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
