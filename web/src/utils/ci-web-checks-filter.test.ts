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
 * Guard for the `web_checks` path filter in .github/workflows/ci.yml
 * (ptone/scion#4308).
 *
 * CI runs the web typecheck, e2e-perf and Vitest steps only when a changed
 * path matches the `web_checks` regex. Web code also reads files outside
 * web/ (pkg/**\/testdata fixtures, harnesses/), so the regex has to cover
 * those paths too. Otherwise a Go-only PR that changes such a file skips the
 * web checks that read it.
 *
 * This test finds every path outside web/ that the web sources reference and
 * fails if the regex does not match one of them. It parses each source file
 * with the TypeScript compiler and collects:
 *   - relative string literals ('./', '../'): imports (JSON included),
 *     vi.mock targets, import.meta.glob patterns, path arguments. Each is
 *     resolved against its file's directory;
 *   - path expressions it can evaluate: resolve()/join() over __dirname,
 *     import.meta.dirname, dirname(fileURLToPath(import.meta.url)),
 *     new URL(..., import.meta.url) and constants built from them (such as
 *     a REPO_ROOT constant);
 *   - string-literal arguments of fs calls, also resolved against the web/
 *     directory, which is the working directory for npm scripts and Vitest;
 *   - relative paths in tsconfig*.json and package.json.
 *
 * The text before a template substitution or a glob character is checked
 * as a path prefix. A reference counts only if it exists on disk (for a
 * prefix: it or its parent directory), so string data that only looks like
 * a path (e.g. '../../../OTHER-PROJECT/...' in URL tests) is skipped.
 */

import { describe, it, expect } from 'vitest';
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, extname, join, relative, resolve, sep } from 'node:path';
import yaml from 'js-yaml';
import ts from 'typescript';

const WEB_ROOT = resolve(__dirname, '../..');
const REPO_ROOT = resolve(WEB_ROOT, '..');
const CI_WORKFLOW = resolve(REPO_ROOT, '.github/workflows/ci.yml');

/** A path outside web/ that a web source file references. */
interface OutOfTreeRef {
  /**
   * Repo-relative path, or the path prefix for a partial reference. A
   * directory ends in '/', the prefix every changed file inside it has.
   */
  path: string;
  /** True when only a prefix is known (template substitution or glob). */
  partial: boolean;
  /** Repo-relative source file and 1-based line. */
  from: string;
}

/** A statically evaluated path. */
interface PathValue {
  abs: string;
  partial: boolean;
}

// ---------------------------------------------------------------------------
// The web_checks regex from ci.yml.
// ---------------------------------------------------------------------------

/**
 * The step script line that sets web_checks: an `if grep -qE '<regex>'`
 * test on the changed-file list whose then-branch writes web_checks=true.
 */
const WEB_CHECKS_IF =
  /if grep -qE '([^']+)' <<<"\$changed"; then\s*\n\s*echo "web_checks=true" >> "\$GITHUB_OUTPUT"/g;

interface CiWorkflow {
  jobs?: Record<string, { steps?: Array<{ run?: unknown }> }>;
}

/** Returns the web_checks regex source, or throws if it cannot be found. */
function webChecksRegexSource(workflowText: string): string {
  const workflow = yaml.load(workflowText) as CiWorkflow | undefined;
  const found: string[] = [];
  for (const job of Object.values(workflow?.jobs ?? {})) {
    for (const step of job.steps ?? []) {
      if (typeof step.run !== 'string') continue;
      for (const m of step.run.matchAll(WEB_CHECKS_IF)) found.push(m[1]!);
    }
  }
  if (found.length !== 1) {
    throw new Error(
      `expected exactly one \`if grep -qE '<regex>' <<<"$changed"; then echo "web_checks=true"\` ` +
        `in a step of ${relative(REPO_ROOT, CI_WORKFLOW)}, found ${found.length}. ` +
        'If the web_checks filter was restructured, update WEB_CHECKS_IF in this test.'
    );
  }
  return found[0]!;
}

// ---------------------------------------------------------------------------
// Source scanning.
// ---------------------------------------------------------------------------

const CODE_EXT = new Set(['.ts', '.tsx', '.mts', '.cts', '.js', '.mjs', '.cjs']);
const SKIP_DIRS = new Set(['node_modules', 'dist', 'test-results', 'playwright-report']);

function isConfigJson(name: string): boolean {
  return /^tsconfig.*\.json$/.test(name) || name === 'package.json';
}

function isScanned(name: string): boolean {
  return CODE_EXT.has(extname(name)) || isConfigJson(name);
}

function walk(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return SKIP_DIRS.has(entry.name) ? [] : walk(path);
    return entry.isFile() && isScanned(entry.name) ? [path] : [];
  });
}

/** web/src, web/e2e*, web/scripts, and the files at the web/ root. */
function scannedFiles(): string[] {
  const files: string[] = [];
  for (const entry of readdirSync(WEB_ROOT, { withFileTypes: true })) {
    const path = join(WEB_ROOT, entry.name);
    if (entry.isDirectory()) {
      if (entry.name === 'src' || entry.name === 'scripts' || entry.name.startsWith('e2e')) {
        files.push(...walk(path));
      }
    } else if (entry.isFile() && isScanned(entry.name)) {
      files.push(path);
    }
  }
  return files.sort();
}

const RELATIVE = /^\.\.?(\/|$)/;

/** Cuts a path literal at its first glob character. */
function cutGlob(text: string): { text: string; partial: boolean } {
  const i = text.search(/[*?[{]/);
  return i < 0 ? { text, partial: false } : { text: text.slice(0, i), partial: true };
}

const FS_CALLS = new Set([
  'readFileSync',
  'readFile',
  'readdirSync',
  'readdir',
  'existsSync',
  'statSync',
  'stat',
  'lstatSync',
  'accessSync',
  'access',
  'opendirSync',
  'createReadStream',
  'globSync',
  'glob',
]);

function calleeName(expr: ts.Expression): string | undefined {
  if (ts.isIdentifier(expr)) return expr.text;
  if (ts.isPropertyAccessExpression(expr)) return expr.name.text;
  return undefined;
}

function unwrap(node: ts.Expression): ts.Expression {
  let n = node;
  while (
    ts.isParenthesizedExpression(n) ||
    ts.isAsExpression(n) ||
    ts.isNonNullExpression(n) ||
    ts.isSatisfiesExpression(n)
  ) {
    n = n.expression;
  }
  return n;
}

function isImportMeta(node: ts.Expression, prop: string): boolean {
  return (
    ts.isPropertyAccessExpression(node) &&
    node.name.text === prop &&
    ts.isMetaProperty(node.expression) &&
    node.expression.keywordToken === ts.SyntaxKind.ImportKeyword
  );
}

/**
 * Collects the out-of-tree references in one source text. `file` is the
 * absolute path the text is attributed to; relative paths resolve against
 * its directory.
 */
function scanSource(file: string, text: string): OutOfTreeRef[] {
  const fileDir = dirname(file);
  const refs: OutOfTreeRef[] = [];

  const add = (abs: string, partial: boolean, pos: number, sf?: ts.SourceFile): void => {
    const line = sf
      ? sf.getLineAndCharacterOfPosition(pos).line + 1
      : text.slice(0, pos).split('\n').length;
    const ref = outOfTree(abs, partial);
    if (ref) refs.push({ ...ref, from: `${relative(REPO_ROOT, file)}:${line}` });
  };

  if (extname(file) === '.json') {
    for (const m of text.matchAll(/(?<=["\s])(\.\.?\/[^"\s]*)/g)) {
      const cut = cutGlob(m[1]!);
      add(resolve(fileDir, cut.text), cut.partial, m.index!);
    }
    return refs;
  }

  const kind = extname(file).endsWith('x') ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  const sf = ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true, kind);
  const consts = new Map<string, PathValue>();

  /** One path segment argument: a literal, a template prefix, or a path. */
  type Segment = { text: string; partial: boolean } | PathValue;

  const segment = (node: ts.Expression): Segment | undefined => {
    const n = unwrap(node);
    if (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n)) return cutGlob(n.text);
    if (ts.isTemplateExpression(n)) return { text: cutGlob(n.head.text).text, partial: true };
    return evalPath(n);
  };

  /** Evaluates `node` to an absolute path, when it is statically known. */
  const evalPath = (node: ts.Expression): PathValue | undefined => {
    const n = unwrap(node);
    if (ts.isIdentifier(n)) {
      if (n.text === '__dirname') return { abs: fileDir, partial: false };
      if (n.text === '__filename') return { abs: file, partial: false };
      return consts.get(n.text);
    }
    if (isImportMeta(n, 'dirname')) return { abs: fileDir, partial: false };
    if (isImportMeta(n, 'url') || isImportMeta(n, 'filename')) return { abs: file, partial: false };
    if (ts.isPropertyAccessExpression(n) && n.name.text === 'pathname') {
      return evalPath(n.expression);
    }
    if (ts.isNewExpression(n) && calleeName(n.expression) === 'URL' && n.arguments?.length === 2) {
      const base = evalPath(n.arguments[1]!);
      const rel = segment(n.arguments[0]!);
      if (!base || !rel || 'abs' in rel) return undefined;
      // URL resolution: a file base resolves against its directory.
      const baseDir = base.abs === file ? fileDir : base.abs;
      return { abs: resolve(baseDir, rel.text), partial: base.partial || rel.partial };
    }
    if (!ts.isCallExpression(n)) return undefined;
    const name = calleeName(n.expression);
    if (name === 'fileURLToPath' && n.arguments.length === 1) return evalPath(n.arguments[0]!);
    if (name === 'dirname' && n.arguments.length === 1) {
      const inner = evalPath(n.arguments[0]!);
      return inner && !inner.partial ? { abs: dirname(inner.abs), partial: false } : undefined;
    }
    if (name !== 'resolve' && name !== 'join') return undefined;
    // path.resolve()/path.join(): fold the segments until one is unknown.
    // resolve() without an absolute base resolves against the working
    // directory, which is web/ for npm scripts and Vitest.
    let acc: string | undefined = name === 'resolve' ? WEB_ROOT : undefined;
    let partial = false;
    for (const arg of n.arguments) {
      const seg = segment(arg);
      if (!seg) {
        partial = true;
        break;
      }
      if ('abs' in seg) {
        acc = seg.abs;
      } else if (acc !== undefined) {
        acc = join(acc, seg.text);
      } else {
        return undefined;
      }
      if (seg.partial) {
        partial = true;
        break;
      }
    }
    // A join() with no absolute segment is a relative path: the literal
    // rule below already covers its leading './' or '../' segment.
    if (acc === undefined || (name === 'join' && !n.arguments.some((a) => evalPath(a)))) {
      return undefined;
    }
    return { abs: resolve(acc), partial };
  };

  const visit = (node: ts.Node): void => {
    if (
      ts.isVariableDeclaration(node) &&
      ts.isIdentifier(node.name) &&
      node.initializer !== undefined
    ) {
      const value = evalPath(node.initializer);
      if (value) consts.set(node.name.text, value);
    }
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) {
      if (RELATIVE.test(node.text)) {
        const cut = cutGlob(node.text);
        add(resolve(fileDir, cut.text), cut.partial, node.getStart(sf), sf);
      }
    } else if (ts.isTemplateExpression(node)) {
      if (RELATIVE.test(node.head.text)) {
        add(resolve(fileDir, cutGlob(node.head.text).text), true, node.getStart(sf), sf);
      }
    } else if (ts.isCallExpression(node) || ts.isNewExpression(node)) {
      const value = evalPath(node);
      if (value) add(value.abs, value.partial, node.getStart(sf), sf);
      const name = calleeName(node.expression);
      const first = node.arguments?.[0];
      if (name && FS_CALLS.has(name) && first) {
        const seg = segment(first);
        if (seg && !('abs' in seg) && seg.text !== '' && !seg.text.startsWith('/')) {
          add(resolve(WEB_ROOT, seg.text), seg.partial, first.getStart(sf), sf);
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return refs;
}

/**
 * The repo-relative form of `abs` when it is a reference outside web/ that
 * exists on disk, or undefined. web/ itself and its ancestors (the repo
 * root, used as a base such as REPO_ROOT) are not references.
 */
function outOfTree(abs: string, partial: boolean): Omit<OutOfTreeRef, 'from'> | undefined {
  const fromWeb = relative(WEB_ROOT, abs);
  const insideWeb = fromWeb === '' || (!fromWeb.startsWith('..') && !fromWeb.startsWith(sep));
  if (insideWeb) return undefined;
  if ((WEB_ROOT + sep).startsWith(abs.endsWith(sep) ? abs : abs + sep)) return undefined;
  if (!existsSync(abs) && !(partial && existsSync(dirname(abs)))) return undefined;
  const path = relative(REPO_ROOT, abs).split(sep).join('/');
  const isDir = existsSync(abs) && statSync(abs).isDirectory();
  return { path: isDir ? `${path}/` : path, partial };
}

/** Every out-of-tree reference, one per path and source line. */
function scanTree(): OutOfTreeRef[] {
  const seen = new Set<string>();
  return scannedFiles()
    .flatMap((file) => scanSource(file, readFileSync(file, 'utf8')))
    .filter((ref) => {
      const key = `${ref.path}\0${ref.from}`;
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
}

function describeRef(ref: OutOfTreeRef): string {
  return `  ${ref.path}${ref.partial ? '* (prefix)' : ''}  <- ${ref.from}`;
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

describe('CI web_checks path filter', () => {
  const regexSource = webChecksRegexSource(readFileSync(CI_WORKFLOW, 'utf8'));
  const webChecks = new RegExp(regexSource);
  const refs = scanTree();

  it('reads a usable web_checks regex from ci.yml', () => {
    expect(webChecks.test('web/src/client/main.ts')).toBe(true);
    expect(webChecks.test('cmd/root.go')).toBe(false);
  });

  it('fails loudly when ci.yml has no web_checks regex', () => {
    expect(() =>
      webChecksRegexSource('jobs:\n  changes:\n    steps:\n      - run: true\n')
    ).toThrow(/expected exactly one/);
  });

  it('finds the known out-of-tree fixture (scanner self-check)', () => {
    // terminal-close-codes.test.ts imports this JSON fixture from pkg/. If the
    // scanner stops finding it, the coverage check below could pass vacuously.
    expect(refs.map((r) => r.path)).toContain('pkg/wsprotocol/testdata/pty_close_codes.json');
  });

  it('covers every path outside web/ that web sources reference', () => {
    const uncovered = refs.filter((r) => !webChecks.test(r.path));
    expect(
      uncovered,
      `The web_checks regex in .github/workflows/ci.yml,\n  ${regexSource}\n` +
        'does not match these paths that web sources read. A PR that changes only them ' +
        'would skip the web typecheck, e2e-perf and Vitest steps. Widen the regex (and ' +
        'its comment) to cover them:\n' +
        uncovered.map(describeRef).join('\n')
    ).toEqual([]);
  });
});

describe('out-of-tree reference scanner', () => {
  const fake = join(WEB_ROOT, 'src/utils/fake.test.ts');
  const paths = (text: string): string[] => scanSource(fake, text).map((r) => r.path);

  it('resolves relative imports and literals against the file directory', () => {
    expect(paths("import f from '../../../pkg/wsprotocol/testdata/pty_close_codes.json';")).toEqual(
      ['pkg/wsprotocol/testdata/pty_close_codes.json']
    );
    expect(paths("import { x } from '../shared/types.js';")).toEqual([]);
  });

  it('evaluates REPO_ROOT-style constants and path calls', () => {
    const text = [
      "const REPO_ROOT = resolve(__dirname, '../../..');",
      "const dir = path.join(REPO_ROOT, 'harnesses');",
      "readFileSync(resolve(dir, entry.name, 'config.yaml'));",
    ].join('\n');
    expect(paths(text)).toContain('harnesses/');
  });

  it('evaluates fileURLToPath(import.meta.url) and new URL bases', () => {
    const text = [
      'const here = path.dirname(fileURLToPath(import.meta.url));',
      "const p = path.join(here, 'x', 'y');",
      "const root = new URL('../../../harnesses/', import.meta.url).pathname;",
    ].join('\n');
    expect(paths(text)).toContain('harnesses/');
  });

  it('keeps the prefix before a template substitution or glob', () => {
    const refs = scanSource(
      fake,
      [
        'readFileSync(path.join(__dirname, `../../../pkg/hub/testdata/x-${name}.json`));',
        "import.meta.glob('../../../pkg/artifacts/testdata/*.json');",
      ].join('\n')
    );
    expect(refs).toContainEqual(
      expect.objectContaining({ path: 'pkg/hub/testdata/x-', partial: true })
    );
    expect(refs).toContainEqual(
      expect.objectContaining({ path: 'pkg/artifacts/testdata/', partial: true })
    );
  });

  it('resolves bare fs-call arguments against web/', () => {
    expect(paths("readFileSync('../harnesses/claude/config.yaml', 'utf8');")).toContain(
      'harnesses/claude/config.yaml'
    );
  });

  it('skips paths that do not exist and the repo root itself', () => {
    expect(paths("const s = '../../../OTHER-PROJECT/workspace/files/secret.txt';")).toEqual([]);
    expect(paths("const ROOT = resolve(__dirname, '../../..');")).toEqual([]);
  });

  it('reads relative paths from tsconfig and package.json', () => {
    const json = join(WEB_ROOT, 'tsconfig.fake.json');
    expect(
      scanSource(json, '{ "include": ["src/**/*", "../pkg/wsprotocol/testdata/*.json"] }').map(
        (r) => r.path
      )
    ).toEqual(['pkg/wsprotocol/testdata/']);
  });
});
