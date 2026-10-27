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
 *   - isolatedChunks:     chunk file name prefixes that no chunk outside the
 *                         prefix may import statically (a lazily loaded
 *                         chunk stays lazy wherever it is reached from,
 *                         including other lazy chunks);
 *   - chunkImports:       per chunk file name prefix, the only chunk file
 *                         name prefixes its chunks may import statically
 *                         (so a lazy chunk never pulls in an unrelated
 *                         large one through a shared helper);
 *   - requiredChunks:     chunk file name prefixes that must exist (proves a
 *                         lazy split still happens);
 *   - requiredStrings:    per chunk file name prefix, strings that must
 *                         occur exactly once in the built JS, in a chunk of
 *                         that prefix: marker strings of modules that must
 *                         survive tree shaking, such as the review editor's
 *                         rendering and input rules (a second copy anywhere
 *                         could stand in for a module the build left out);
 *   - forbiddenModules:   source modules that must not be in the static
 *                         closure of the given routes, by base name
 *                         (pattern) or by full path (pathPattern, for
 *                         example node_modules/<package>/), or as packages
 *                         of a named chunk in manualChunks (chunkPackages);
 *   - manualChunks:       the package pattern of each named chunk, which
 *                         vite.config.ts uses to place those packages;
 *   - a chunks entry with combined: true limits all chunks of the prefix
 *                         together (otherwise each one). Module lists come from
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
 * Modules in `modules` whose base name matches `pattern` or whose path
 * (relative to web/) matches `pathPattern`, and that are not in the
 * exact-path allowlist `allow` (no globs: each entry is one path). Either
 * pattern may be absent.
 */
export function forbiddenIn(modules, pattern, allow, pathPattern) {
  const re = pattern ? new RegExp(pattern) : null;
  const pathRe = pathPattern ? new RegExp(pathPattern) : null;
  const allowed = new Set(allow.map((a) => a.path));
  return modules.filter(
    (m) =>
      ((re !== null && re.test(posix.basename(m))) || (pathRe !== null && pathRe.test(m))) &&
      !allowed.has(m)
  );
}

/**
 * Static imports of chunks named `prefix`* by chunks not named so: the
 * manifest entries outside the prefix whose `imports` (not
 * `dynamicImports`) name an entry whose file starts with `prefix`.
 */
export function staticImportersOf(manifest, prefix) {
  const out = [];
  for (const chunk of Object.values(manifest)) {
    if (chunk.file.startsWith(prefix)) continue;
    for (const dep of chunk.imports ?? []) {
      const target = manifest[dep];
      if (target && target.file.startsWith(prefix)) out.push({ from: chunk.file, to: target.file });
    }
  }
  return out;
}

/**
 * Static imports from chunks named `prefix`* of chunks whose file starts
 * with none of `allowed`.
 */
export function importsOutside(manifest, prefix, allowed) {
  const out = [];
  for (const chunk of Object.values(manifest)) {
    if (!chunk.file.startsWith(prefix)) continue;
    for (const dep of chunk.imports ?? []) {
      const target = manifest[dep];
      const file = target ? target.file : dep;
      if (!allowed.some((a) => file.startsWith(a))) out.push({ from: chunk.file, to: file });
    }
  }
  return out;
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
    if (limit.combined) {
      // One limit for all chunks of the prefix: raw bytes and gzip sizes add up.
      let raw = 0;
      let gz = 0;
      for (const file of found) {
        const buf = readFile(file);
        if (!buf) {
          failures.push(`${file}: missing from the build`);
          continue;
        }
        raw += buf.length;
        gz += gzipSize(buf);
      }
      const label = `${prefix}* combined (${found.join(', ')})`;
      report.push(`${label}: raw ${raw} / ${limit.raw}, gzip ${gz} / ${limit.gzip}`);
      if (limit.raw !== undefined && raw > limit.raw)
        failures.push(`${label}: raw ${raw} B exceeds ${limit.raw} B`);
      if (limit.gzip !== undefined && gz > limit.gzip)
        failures.push(`${label}: gzip ${gz} B exceeds ${limit.gzip} B`);
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

  for (const prefix of budget.isolatedChunks ?? []) {
    const bad = staticImportersOf(manifest, prefix);
    report.push(`isolated chunk ${prefix}*: ${bad.length} static importer(s) outside it`);
    for (const b of bad)
      failures.push(`${b.from} imports ${b.to} statically (isolated ${prefix}*)`);
  }

  for (const [prefix, allowed] of Object.entries(budget.chunkImports ?? {})) {
    if (prefix.startsWith('$')) continue;
    const bad = importsOutside(manifest, prefix, allowed);
    report.push(`static imports of ${prefix}*: ${bad.length} outside the allowed list`);
    for (const b of bad)
      failures.push(`${b.from} imports ${b.to} statically (not allowed for ${prefix}*)`);
  }

  for (const prefix of budget.requiredChunks ?? []) {
    const found = built.filter((f) => f.startsWith(prefix) && f.endsWith('.js'));
    report.push(`required chunk ${prefix}*.js: ${found.length ? found.join(', ') : 'none'}`);
    if (!found.length) failures.push(`no chunk matching ${prefix}*.js`);
  }

  for (const [prefix, strings] of Object.entries(budget.requiredStrings ?? {})) {
    if (prefix.startsWith('$')) continue;
    const found = built.filter((f) => f.startsWith(prefix) && f.endsWith('.js'));
    const count = (text, x) => text.split(x).length - 1;
    const texts = built
      .filter((f) => f.endsWith('.js'))
      .map((f) => ({ f, text: readFile(f)?.toString('utf-8') ?? '' }));
    let ok = 0;
    for (const x of strings) {
      const where = texts.filter((t) => count(t.text, x) > 0);
      const total = where.reduce((n, t) => n + count(t.text, x), 0);
      if (total === 0) failures.push(`${prefix}*.js: missing required string ${JSON.stringify(x)}`);
      else if (total > 1 || !where[0].f.startsWith(prefix)) {
        failures.push(
          `${prefix}*.js: required string ${JSON.stringify(x)} occurs ${total} time(s), in ${where.map((t) => t.f).join(', ')}; EXPECT once, in ${prefix}*.js`
        );
      } else ok++;
    }
    report.push(
      `required strings in ${prefix}*.js: ${ok} of ${strings.length} present, each exactly once in the build`
    );
    if (!found.length) failures.push(`no chunk matching ${prefix}*.js`);
  }

  for (const rule of budget.forbiddenModules ?? []) {
    const allow = rule.allow ?? [];
    const chunkPkgs = rule.chunkPackages ? budget.manualChunks?.[rule.chunkPackages] : undefined;
    if (rule.chunkPackages && !chunkPkgs) {
      failures.push(
        `forbiddenModules rule for ${rule.roots}: no manualChunks entry ${rule.chunkPackages}`
      );
      continue;
    }
    const pathPattern = [rule.pathPattern, chunkPkgs]
      .filter(Boolean)
      .map((p) => `(${p})`)
      .join('|');
    if (!rule.pattern && !pathPattern) {
      failures.push(`forbiddenModules rule for ${rule.roots}: needs a pattern or a pathPattern`);
      continue;
    }
    const label = [rule.pattern, pathPattern].filter(Boolean).join(' or path ');
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
        for (const m of forbiddenIn(modules, rule.pattern, allow, pathPattern || undefined)) {
          bad.add(`${m} (in ${file})`);
        }
      }
      report.push(
        `static closure of ${root} (${keys.length} chunks): ${bad.size} module(s) matching ${label}`
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
