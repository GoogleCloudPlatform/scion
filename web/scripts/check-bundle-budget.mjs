#!/usr/bin/env node
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
 * Bundle budget check for the built web client (ptone/scion#5306).
 *
 * Reads the Vite manifest (dist/client/.vite/manifest.json) and the built
 * files, and fails (exit 1) if any limit in web/bundle-budget.json is
 * exceeded:
 *
 *   - files:              raw and gzip byte limits per built file;
 *   - chunks:             raw and gzip byte limits for every built JS chunk
 *                         whose name starts with the given prefix (chunk
 *                         names carry a content hash); at least one chunk
 *                         must match. Keys starting with $ are comments;
 *   - entryClosure:       gzip limit on the entry chunk plus every chunk and
 *                         stylesheet it imports statically (what every route
 *                         downloads before the first render);
 *   - requiredChunks:     chunk file name prefixes that must exist (proves a
 *                         lazy split still happens);
 *   - forbiddenModules:   source modules that must not be in the static
 *                         closure of the given routes. Module lists come from
 *                         each chunk's source map, looked up next to the chunk
 *                         or under dist/sourcemaps/ (where hidden maps may be
 *                         moved), so the check works with either policy.
 *
 * Gzip sizes use Node's zlib default level (6), the level Vite's own build
 * report uses, so the numbers match the "gzip:" column of `vite build`.
 *
 * Usage (from web/, after `vite build`):
 *   node scripts/check-bundle-budget.mjs [--dist dist/client] [--budget bundle-budget.json]
 *
 * The functions are exported for scripts/check-bundle-budget.test.mjs.
 */

import { existsSync, readFileSync } from 'node:fs';
import { dirname, join, posix, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

/** Manifest keys of the chunks `rootKey` loads statically, root included, in visit order. */
export function staticClosure(manifest, rootKey) {
  if (!manifest[rootKey]) throw new Error(`manifest has no entry ${rootKey}`);
  const seen = new Set();
  const visit = (key) => {
    if (seen.has(key)) return;
    const chunk = manifest[key];
    if (!chunk) throw new Error(`manifest import ${key} has no entry`);
    seen.add(key);
    for (const dep of chunk.imports ?? []) visit(dep);
  };
  visit(rootKey);
  return [...seen];
}

/** Built files (JS chunks and their CSS) of a static closure, deduplicated. */
export function closureFiles(manifest, keys) {
  const files = new Set();
  for (const key of keys) {
    files.add(manifest[key].file);
    for (const css of manifest[key].css ?? []) files.add(css);
  }
  return [...files];
}

/**
 * Source modules of a chunk from its source map, as paths relative to the
 * web/ directory with forward slashes (for example src/client/main.ts).
 * `mapPath` is where the map was read from: per the source map spec, its
 * sources (after sourceRoot) are relative to the map's own location.
 */
export function mapSources(map, mapPath, webDir) {
  const root = map.sourceRoot ?? '';
  return (map.sources ?? []).map((src) =>
    relative(webDir, resolve(dirname(mapPath), root, src))
      .split('\\')
      .join('/')
  );
}

/** Where a chunk's map is: next to it, else under the sibling dist/sourcemaps/. */
export function findSourceMap(distDir, file) {
  const candidates = [
    join(distDir, `${file}.map`),
    join(dirname(distDir), 'sourcemaps', `${file}.map`),
  ];
  return candidates.find((p) => existsSync(p)) ?? null;
}

/**
 * Modules in `modules` whose base name matches `pattern` and that are not in
 * the exact-path allowlist `allow` (no globs: each entry is one path).
 */
export function forbiddenIn(modules, pattern, allow) {
  const re = new RegExp(pattern);
  const allowed = new Set(allow.map((a) => a.path));
  return modules.filter((m) => re.test(posix.basename(m)) && !allowed.has(m));
}

function gzipSize(buf) {
  return gzipSync(buf).length;
}

/**
 * Runs every check. `readFile(distRelativePath)` returns a Buffer or null;
 * `listFiles()` returns the dist-relative paths of the built files;
 * `readMapModules(distRelativePath)` returns the chunk's source modules or
 * null when it has no source map. Returns { failures, report } (string lists).
 */
export function checkBudget({ manifest, budget, readFile, listFiles, readMapModules }) {
  const failures = [];
  const report = [];

  const checkSize = (file, buf, limit) => {
    const raw = buf.length;
    const gz = gzipSize(buf);
    report.push(`${file}: raw ${raw} / ${limit.raw}, gzip ${gz} / ${limit.gzip}`);
    if (limit.raw !== undefined && raw > limit.raw) {
      failures.push(`${file}: raw ${raw} B exceeds ${limit.raw} B`);
    }
    if (limit.gzip !== undefined && gz > limit.gzip) {
      failures.push(`${file}: gzip ${gz} B exceeds ${limit.gzip} B`);
    }
  };

  for (const [file, limit] of Object.entries(budget.files ?? {})) {
    const buf = readFile(file);
    if (!buf) {
      failures.push(`${file}: missing from the build`);
      continue;
    }
    checkSize(file, buf, limit);
  }

  const built = listFiles();
  for (const [prefix, limit] of Object.entries(budget.chunks ?? {})) {
    if (prefix.startsWith('$')) continue; // "$comment" keys
    const found = built.filter((f) => f.startsWith(prefix) && f.endsWith('.js'));
    if (!found.length) {
      failures.push(`no chunk matching ${prefix}*.js`);
      continue;
    }
    for (const file of found) {
      const buf = readFile(file);
      if (!buf) {
        failures.push(`${file}: missing from the build`);
        continue;
      }
      checkSize(file, buf, limit);
    }
  }

  if (budget.entryClosure) {
    const { entry, gzip: limit } = budget.entryClosure;
    const files = closureFiles(manifest, staticClosure(manifest, entry));
    let total = 0;
    for (const file of files) {
      const buf = readFile(file);
      if (!buf) {
        failures.push(`${file}: in the entry closure but missing from the build`);
        continue;
      }
      total += gzipSize(buf);
    }
    report.push(`entry closure of ${entry} (${files.join(', ')}): gzip ${total} / ${limit}`);
    if (total > limit)
      failures.push(`entry closure of ${entry}: gzip ${total} B exceeds ${limit} B`);
  }

  for (const prefix of budget.requiredChunks ?? []) {
    const found = built.filter((f) => f.startsWith(prefix) && f.endsWith('.js'));
    report.push(`required chunk ${prefix}*.js: ${found.length ? found.join(', ') : 'none'}`);
    if (!found.length) failures.push(`no chunk matching ${prefix}*.js`);
  }

  for (const rule of budget.forbiddenModules ?? []) {
    const allow = rule.allow ?? [];
    for (const a of allow) {
      if (typeof a.path !== 'string' || /[*?[\]{}]/.test(a.path) || !a.reason) {
        failures.push(
          `forbiddenModules allow entry ${JSON.stringify(a)}: needs an exact path and a reason`
        );
      }
    }
    for (const root of rule.roots) {
      const keys = staticClosure(manifest, root);
      const bad = new Set();
      for (const key of keys) {
        const file = manifest[key].file;
        const modules = readMapModules(file);
        if (modules === null) {
          failures.push(`${file}: no source map, cannot check modules in the closure of ${root}`);
          continue;
        }
        for (const m of forbiddenIn(modules, rule.pattern, allow)) bad.add(`${m} (in ${file})`);
      }
      report.push(
        `static closure of ${root} (${keys.length} chunks): ${bad.size} module(s) matching ${rule.pattern}`
      );
      for (const b of bad) failures.push(`${root}: static closure contains ${b}`);
    }
  }

  return { failures, report };
}

function parseArgs(argv, webDir) {
  const args = { dist: join(webDir, 'dist/client'), budget: join(webDir, 'bundle-budget.json') };
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i];
    const value = argv[i + 1];
    if ((flag === '--dist' || flag === '--budget') && value) {
      args[flag.slice(2)] = resolve(value);
      i++;
    } else {
      throw new Error(`unknown argument ${flag}`);
    }
  }
  return args;
}

function main() {
  const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..');
  const { dist, budget: budgetPath } = parseArgs(process.argv.slice(2), webDir);
  const budget = JSON.parse(readFileSync(budgetPath, 'utf8'));
  const manifest = JSON.parse(readFileSync(join(dist, '.vite/manifest.json'), 'utf8'));
  const builtFiles = [
    ...new Set(Object.values(manifest).flatMap((c) => [c.file, ...(c.css ?? [])])),
  ];
  const result = checkBudget({
    manifest,
    budget,
    readFile: (file) => {
      const p = join(dist, file);
      return existsSync(p) ? readFileSync(p) : null;
    },
    listFiles: () => builtFiles,
    readMapModules: (file) => {
      const mapPath = findSourceMap(dist, file);
      if (!mapPath) return null;
      return mapSources(JSON.parse(readFileSync(mapPath, 'utf8')), mapPath, webDir);
    },
  });
  for (const line of result.report) console.log(`bundle-budget: ${line}`);
  if (result.failures.length) {
    for (const f of result.failures) console.error(`bundle-budget: FAIL ${f}`);
    process.exit(1);
  }
  console.log('bundle-budget: OK');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main();
}
