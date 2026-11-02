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
 * The review editor's Milkdown dependency is pinned to one exact version,
 * in package.json and in the lockfile, and the editor imports only the
 * packages it is meant to use. An upgrade is a deliberate change that
 * edits MILKDOWN_VERSION here and re-runs the editor's rendering tests.
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

const MILKDOWN_VERSION = '7.22.3';

/** The Milkdown packages the editor depends on directly. */
const DIRECT = [
  '@milkdown/core',
  '@milkdown/ctx',
  '@milkdown/plugin-history',
  '@milkdown/preset-commonmark',
  '@milkdown/preset-gfm',
  '@milkdown/prose',
  // The editor builds a document from a Markdown tree it has already parsed
  // (ParserState), as Milkdown's own parser does; @milkdown/core ships it.
  '@milkdown/transformer',
  '@milkdown/utils',
];

/** Milkdown import specifiers the editor may use (the prose subpaths exactly). */
const ALLOWED_IMPORT =
  /^@milkdown\/(core|ctx|utils|preset-commonmark|preset-gfm|plugin-history|transformer|prose\/(model|state|view|transform|commands|history|keymap))$/;

/**
 * Milkdown names written in quotes that are not imports, per file: the
 * entity table test locates @milkdown/transformer's package directory to
 * follow the Markdown parser's dependency chain (it imports nothing from it).
 */
const NOT_IMPORTS: Record<string, number> = { 'src/utils/critic-sentinels.test.ts': 1 };

/** Packages that come only through Milkdown: never imported by name. */
const TRANSITIVE = /^(prosemirror-|remark|micromark|mdast-|unified)/;

/** The module specifiers of a source file's imports and re-exports. */
function importSpecs(text: string): { spec: string; sideEffect: boolean }[] {
  const re =
    /(?:^|[\s;])(?:import|export)\s*(?:type\s+)?(?:([\w*{}\s,$]+?)\s*from\s*)?['"]([^'"]+)['"]|import\(\s*['"]([^'"]+)['"]\s*\)/g;
  return [...text.matchAll(re)].map((m) => ({
    spec: m[2] ?? m[3],
    sideEffect: m[2] !== undefined && m[1] === undefined,
  }));
}

/**
 * The problems with the Milkdown-related imports of one source file: a
 * Milkdown specifier outside ALLOWED_IMPORT, any import of a package that
 * only comes through Milkdown, a side-effect import of either, or a
 * Milkdown import outside the editor's folder (inEditor false).
 */
function importProblems(text: string, inEditor: boolean): string[] {
  const out: string[] = [];
  for (const { spec, sideEffect } of importSpecs(text)) {
    if (TRANSITIVE.test(spec)) out.push(`transitive package ${spec}`);
    if (!spec.startsWith('@milkdown/')) continue;
    if (!ALLOWED_IMPORT.test(spec)) out.push(`not allowed ${spec}`);
    if (sideEffect) out.push(`side-effect import ${spec}`);
    if (!inEditor) out.push(`outside the editor ${spec}`);
  }
  return out;
}

const web = join(__dirname, '../../..');

describe('Milkdown dependency', () => {
  it('pins every direct Milkdown package exactly in package.json', () => {
    const pkg = JSON.parse(readFileSync(join(web, 'package.json'), 'utf-8')) as {
      dependencies: Record<string, string>;
      devDependencies: Record<string, string>;
    };
    const all = { ...pkg.dependencies, ...pkg.devDependencies };
    const milkdown = Object.keys(all).filter((k) => k.startsWith('@milkdown/'));
    expect(milkdown.sort()).toEqual(DIRECT);
    for (const name of DIRECT) expect(all[name], name).toBe(MILKDOWN_VERSION);
  });

  it('has every @milkdown entry of the lockfile at that version', () => {
    const lock = JSON.parse(readFileSync(join(web, 'package-lock.json'), 'utf-8')) as {
      packages: Record<string, { version?: string }>;
    };
    const entries = Object.entries(lock.packages).filter(([path]) =>
      /(^|\/)node_modules\/@milkdown\/[^/]+$/.test(path)
    );
    expect(entries.length).toBeGreaterThanOrEqual(DIRECT.length);
    for (const [path, entry] of entries) expect(entry.version, path).toBe(MILKDOWN_VERSION);
    const names = entries.map(([path]) => path.replace(/^.*node_modules\//, ''));
    expect(names).not.toContain('@milkdown/kit');
    expect(names).not.toContain('@milkdown/crepe');
  });

  it('finds imports the editor may not use (negative cases)', () => {
    expect(importProblems("import { EditorView } from '@milkdown/prose/view';", true)).toEqual([]);
    expect(importProblems("import { x } from '@milkdown/prose/tables';", true)).toEqual([
      'not allowed @milkdown/prose/tables',
    ]);
    expect(importProblems("import { x } from '@milkdown/prose';", true)).toEqual([
      'not allowed @milkdown/prose',
    ]);
    expect(importProblems("import '@milkdown/core';", true)).toEqual([
      'side-effect import @milkdown/core',
    ]);
    expect(importProblems("import 'prosemirror-view/style/prosemirror.css';", true)).toEqual([
      'transitive package prosemirror-view/style/prosemirror.css',
    ]);
    expect(importProblems("import { Node } from 'prosemirror-model';", true)).toEqual([
      'transitive package prosemirror-model',
    ]);
    expect(importProblems("export { a } from 'remark-gfm';", true)).toEqual([
      'transitive package remark-gfm',
    ]);
    expect(importProblems("const u = await import('unified');", true)).toEqual([
      'transitive package unified',
    ]);
    expect(importProblems("import type { Root } from 'mdast-util-x';", true)).toEqual([
      'transitive package mdast-util-x',
    ]);
    expect(importProblems("import { a } from '@milkdown/core';", false)).toEqual([
      'outside the editor @milkdown/core',
    ]);
  });

  it('imports only the allowed Milkdown packages, and nothing that comes through them', () => {
    const files: string[] = [];
    const walk = (dir: string): void => {
      for (const name of readdirSync(dir)) {
        const p = join(dir, name);
        if (statSync(p).isDirectory()) walk(p);
        else if (/\.(ts|js|mjs)$/.test(p)) files.push(p);
      }
    };
    walk(join(web, 'src'));
    const problems: string[] = [];
    let mentions = 0;
    let found = 0;
    for (const f of files) {
      const rel = relative(web, f);
      if (rel === 'src/components/artifact-review/dependencies.test.ts') continue;
      const text = readFileSync(f, 'utf-8');
      // Every quoted Milkdown specifier (single quotes or backticks) is
      // found by the import scan, apart from the listed non-imports.
      mentions += (text.match(/['`]@milkdown\//g)?.length ?? 0) - (NOT_IMPORTS[rel] ?? 0);
      found += importSpecs(text).filter((x) => x.spec.startsWith('@milkdown/')).length;
      for (const p of importProblems(text, rel.startsWith('src/components/artifact-review/')))
        problems.push(`${rel}: ${p}`);
    }
    expect(problems).toEqual([]);
    expect(found).toBeGreaterThan(0);
    expect(found).toBe(mentions);
  });
});
