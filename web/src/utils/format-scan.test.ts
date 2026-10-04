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
 * Format scan (design.md §2.4 "Enforcement", AC17).
 *
 * CI runs `npm test` but not `npm run lint` (`.github/workflows/ci.yml`), so
 * this is a vitest source scan rather than an ESLint rule. It bans ad-hoc
 * `Date`/locale formatting tokens from every non-test file under
 * `web/src` except `utils/time.ts`, which is the one module allowed to use
 * them. There are no other exceptions: a banned token anywhere else fails
 * `npm test`, and the fix is to route the call through `time.ts` (or
 * `formatNumber` for numbers), not to exempt the file.
 *
 * The scan is a plain text match: it deliberately does not strip comments
 * or type-cast expressions, so a reference to one of these tokens in a
 * comment counts too. This trades some false positives for not needing a
 * type-checked program in a unit test (see design.md's "Alternative
 * rejected": an AST check using the TypeScript compiler API).
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { resolve, relative, sep } from 'node:path';

/** `web/src`, relative to this file (`web/src/utils/`). */
const SRC_ROOT = resolve(__dirname, '..');

/** The one file allowed to use the banned tokens below. */
const EXEMPT_FILE = 'utils/time.ts';

/**
 * Banned tokens (design.md §2.4 "Enforcement"): the `Date` locale-string
 * methods, `Intl.DateTimeFormat`, `Intl.RelativeTimeFormat` and `hour12`.
 * `Intl.NumberFormat` and `Intl.supportedValuesOf` are not banned.
 */
const BANNED_PATTERN =
  /toLocaleString\(|toLocaleDateString\(|toLocaleTimeString\(|Intl\.DateTimeFormat|Intl\.RelativeTimeFormat|hour12/;

/** Recursively lists non-test `.ts` files under `dir`, as absolute paths. */
function listSourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = resolve(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...listSourceFiles(full));
    } else if (entry.isFile() && entry.name.endsWith('.ts') && !entry.name.endsWith('.test.ts')) {
      out.push(full);
    }
  }
  return out;
}

function toRelative(absPath: string): string {
  return relative(SRC_ROOT, absPath).split(sep).join('/');
}

/** Files under `web/src`, other than `EXEMPT_FILE`, whose source matches `pattern`. */
function filesMatching(pattern: RegExp): string[] {
  const offenders: string[] = [];
  for (const absPath of listSourceFiles(SRC_ROOT)) {
    const rel = toRelative(absPath);
    if (rel === EXEMPT_FILE) continue;
    if (pattern.test(readFileSync(absPath, 'utf8'))) {
      offenders.push(rel);
    }
  }
  return offenders;
}

describe('format scan (AC17)', () => {
  it('finds banned Date/locale formatting tokens only in utils/time.ts', () => {
    expect(filesMatching(BANNED_PATTERN)).toEqual([]);
  });

  it('finds hour12 in no file other than utils/time.ts', () => {
    expect(filesMatching(/hour12/)).toEqual([]);
  });

  it('scans the exempt file itself (the path is not stale)', () => {
    expect(listSourceFiles(SRC_ROOT).map(toRelative)).toContain(EXEMPT_FILE);
  });

  it('does not flag formatNumber or isValidTimeZone call sites', () => {
    const clean = `
      import { formatNumber } from './format-number.js';
      import { isValidTimeZone, listTimeZones } from './time.js';
      const n = formatNumber(1234.5);
      const ok = isValidTimeZone('Asia/Tokyo');
      const zones = listTimeZones();
      const totals = new Intl.NumberFormat('en').format(42);
    `;
    expect(BANNED_PATTERN.test(clean)).toBe(false);
  });

  it('flags each banned token', () => {
    expect(BANNED_PATTERN.test('d.toLocaleString()')).toBe(true);
    expect(BANNED_PATTERN.test('d.toLocaleDateString()')).toBe(true);
    expect(BANNED_PATTERN.test('d.toLocaleTimeString()')).toBe(true);
    expect(BANNED_PATTERN.test("new Intl.DateTimeFormat('en')")).toBe(true);
    expect(BANNED_PATTERN.test("new Intl.RelativeTimeFormat('en')")).toBe(true);
    expect(BANNED_PATTERN.test('{ hour12: false }')).toBe(true);
  });
});
