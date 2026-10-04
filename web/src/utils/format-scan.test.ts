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
 * Format scan (tz-refactor task 11, design.md §2.4 "Enforcement").
 *
 * CI runs `npm test` but not `npm run lint` (`.github/workflows/ci.yml`), so
 * this is a vitest source scan rather than an ESLint rule. It bans ad-hoc
 * `Date`/locale formatting tokens from every non-test file under
 * `web/src` except `utils/time.ts`, which is the one module allowed to use
 * them.
 *
 * The scan is a plain text match: it deliberately does not strip comments
 * or type-cast expressions, so a reference to one of these tokens in a
 * comment counts too (reworded or removed when that file migrates, same as
 * a real call site). This trades some false positives for not needing a
 * type-checked program in a unit test (see design.md's "Alternative
 * rejected": an AST check using the TypeScript compiler API).
 *
 * The allowlist only shrinks: `ALLOWLIST` below is a file-granular list,
 * generated at the implementation SHA of tz-refactor task 11, of every
 * remaining file with a banned token. A later migration removes its file
 * from this list; the `stale allowlist entry` test below fails if a listed
 * file no longer has a banned token, so the list cannot silently grow back.
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

/**
 * File-granular allowlist, generated at this issue's implementation SHA by
 * scanning non-test `web/src/**\/*.ts` for `BANNED_PATTERN` outside
 * `EXEMPT_FILE`. Paths are relative to `web/src`, forward-slash separated.
 *
 * tz-refactor task 11 removed five files from a larger list by migrating
 * them to `time.ts`: the four native-chat formatters (`chat-message.ts`,
 * `chat-date-divider.ts`, `chat-interagent-marker.ts`,
 * `chat-system-line.ts`) and `access-boundary-schedule-editor.ts` (its
 * `viewerTimeZone` getter). tz-refactor task 13 removed
 * `profile-settings.ts` along with its "Agent timezone" section and that
 * section's zone check.
 *
 * tz-refactor task 19 removed the list pages, `components/shared/*list*`
 * files and every file with a private relative-time helper outside the
 * admin, access-boundary and chat views: `agent-detail.ts`,
 * `project-detail.ts`, `brokers.ts`, `broker-detail.ts`, `home.ts`,
 * `project-settings.ts`, `env-var-list.ts`, `gcp-service-account-list.ts`,
 * `pre-start-hook-list.ts`, `project-template-list.ts`, `schedule-list.ts`,
 * `scheduled-event-list.ts`, `secret-list.ts`, `subscription-manager.ts` and
 * `token-list.ts`.
 *
 * tz-refactor task 20 removed the admin, access-boundary and role-binding
 * views: `admin-access-boundaries.ts`, `admin-access-boundary-detail.ts`,
 * `admin-experiments.ts`, `admin-maintenance.ts`, `admin-quotas.ts`,
 * `admin-role-bindings.ts`, `admin-role-detail.ts`, `admin-roles.ts`,
 * `admin-scheduler.ts`, `admin-server-config.ts`, `admin-users.ts`,
 * `metrics-dashboard.ts`, `access-boundary-audit-timeline.ts`,
 * `access-boundary-definition-summary.ts`,
 * `access-boundary-impact-summary.ts`, `access-boundary-preview.ts` and
 * `role-binding-utils.ts`.
 *
 * The end state (P3c, tz-refactor task 21) is an empty list and the test
 * below deletes itself along with it.
 */
const ALLOWLIST: readonly string[] = [];

/** Recursively lists non-test `.ts` files under `dir`, relative to `SRC_ROOT`. */
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

describe('format scan (tz-refactor task 11)', () => {
  it('bans ad-hoc Date/locale formatting tokens outside time.ts and the allowlist', () => {
    const offenders: string[] = [];
    for (const absPath of listSourceFiles(SRC_ROOT)) {
      const rel = toRelative(absPath);
      if (rel === EXEMPT_FILE) continue;
      if (ALLOWLIST.includes(rel)) continue;
      const content = readFileSync(absPath, 'utf8');
      if (BANNED_PATTERN.test(content)) {
        offenders.push(rel);
      }
    }
    expect(offenders).toEqual([]);
  });

  it('no longer lists profile-settings.ts', () => {
    expect(ALLOWLIST).not.toContain('components/pages/profile-settings.ts');
  });

  it('fails on a stale allowlist entry (a listed file with no banned token)', () => {
    const stale: string[] = [];
    for (const rel of ALLOWLIST) {
      const content = readFileSync(resolve(SRC_ROOT, rel), 'utf8');
      if (!BANNED_PATTERN.test(content)) {
        stale.push(rel);
      }
    }
    expect(stale).toEqual([]);
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
