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

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { KNOWN_HARNESS_NAMES, harnessDisplayName } from './harness-utils.js';

/** Repository root, relative to web/src/shared/. */
const REPO_ROOT = resolve(__dirname, '../../..');

/** Components that render the harness fallback list. */
const CONSUMERS = [
  'src/components/pages/project-settings.ts',
  'src/components/pages/admin-server-config.ts',
  'src/components/pages/agent-create.ts',
];

function readWebSource(relPath: string): string {
  return readFileSync(resolve(REPO_ROOT, 'web', relPath), 'utf8');
}

/**
 * Harness identifiers a redeclared local list could be built from: the
 * canonical seven plus the bogus `gemini` that Phase 0 removed.
 */
const FORBIDDEN_IDENTS: readonly string[] = [...KNOWN_HARNESS_NAMES, 'gemini'];

/** Single-quoted harness identifier, e.g. `'gemini-cli'`. */
const IDENT = String.raw`'(?:${FORBIDDEN_IDENTS.join('|')})'`;

/**
 * Matches an array literal containing two or more harness identifiers in a row,
 * anywhere in the array -- so reordering the elements, or leading with a name
 * that is not a harness, does not defeat it.
 */
const HARNESS_ARRAY_LITERAL = new RegExp(
  String.raw`\[[^\]]*${IDENT}\s*,\s*${IDENT}`
);

describe('KNOWN_HARNESS_NAMES', () => {
  it('matches the harnesses/ directory, which is the source of truth', () => {
    const harnessesDir = resolve(REPO_ROOT, 'harnesses');
    // Guard: if the checkout does not include harnesses/ (e.g. a partial
    // clone), fail loudly rather than silently passing an empty comparison.
    expect(existsSync(harnessesDir)).toBe(true);

    const fromTree = readdirSync(harnessesDir, { withFileTypes: true })
      .filter((e) => e.isDirectory() && existsSync(resolve(harnessesDir, e.name, 'config.yaml')))
      .map((e) => e.name)
      .sort();

    expect([...KNOWN_HARNESS_NAMES].sort()).toEqual(fromTree);
  });

  it('uses gemini-cli, not the non-existent "gemini" harness', () => {
    expect(KNOWN_HARNESS_NAMES).toContain('gemini-cli');
    expect(KNOWN_HARNESS_NAMES).not.toContain('gemini');
  });

  it('contains no duplicates', () => {
    expect(new Set(KNOWN_HARNESS_NAMES).size).toBe(KNOWN_HARNESS_NAMES.length);
  });
});

describe('harnessDisplayName', () => {
  it('gives every canonical harness a non-empty label', () => {
    for (const name of KNOWN_HARNESS_NAMES) {
      expect(harnessDisplayName(name).trim()).not.toBe('');
    }
  });

  it('preserves the labels the components rendered before the refactor', () => {
    expect(harnessDisplayName('gemini-cli')).toBe('Gemini CLI');
    expect(harnessDisplayName('claude')).toBe('Claude');
    expect(harnessDisplayName('codex')).toBe('Codex');
    expect(harnessDisplayName('copilot')).toBe('Copilot');
    expect(harnessDisplayName('opencode')).toBe('OpenCode');
  });

  it('passes unknown identifiers through unchanged', () => {
    expect(harnessDisplayName('my-custom-harness')).toBe('my-custom-harness');
  });
});

describe('harness fallback list is not duplicated in components', () => {
  it.each(CONSUMERS)('%s imports KNOWN_HARNESS_NAMES from shared/harness-utils', (relPath) => {
    const src = readWebSource(relPath);
    expect(src).toMatch(
      /import\s*\{[^}]*\bKNOWN_HARNESS_NAMES\b[^}]*\}\s*from\s*'(\.\.\/)+shared\/harness-utils\.js'/
    );
  });

  it.each(CONSUMERS)('%s declares no local harness-name array', (relPath) => {
    const src = readWebSource(relPath);
    // The old shape was e.g.
    //   const fallbackNames = ['gemini-cli', 'claude', 'codex', ...];
    expect(src).not.toMatch(/\bfallbackNames\s*=/);
    // Any array literal pairing two or more harness identifiers is a redeclared
    // list, regardless of variable name, element order, or which name comes
    // first. The alternation deliberately includes the bogus 'gemini' -- a
    // reintroduced ['gemini', 'claude', ...] is the exact bug this phase fixed,
    // so it must be caught even though 'gemini' is not a canonical name.
    expect(src).not.toMatch(HARNESS_ARRAY_LITERAL);
  });

  it.each(CONSUMERS)('%s hardcodes no harness sl-option elements', (relPath) => {
    const src = readWebSource(relPath);
    // Fallback options must be produced by mapping over KNOWN_HARNESS_NAMES,
    // not written out one <sl-option value="claude"> at a time. Both quote
    // styles are matched; dynamic `value=${name}` bindings are intentionally
    // not matched, since those are the correct, mapped form.
    const hardcoded = [...src.matchAll(/<sl-option\s+value=("([^"]*)"|'([^']*)')/g)].map(
      (m) => m[2] ?? m[3]
    );
    const harnessOptions = hardcoded.filter((v) => FORBIDDEN_IDENTS.includes(v));
    expect(harnessOptions).toEqual([]);
  });
});
