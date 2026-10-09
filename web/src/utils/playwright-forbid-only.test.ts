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
 * Every Playwright config sets forbidOnly when CI is set, so a committed
 * test.only fails the CI run instead of narrowing it to a single test.
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

const WEB_ROOT = resolve(__dirname, '../..');
const E2E_ROOT = join(WEB_ROOT, 'e2e');

function playwrightConfigs(): string[] {
  const suites = readdirSync(E2E_ROOT, { withFileTypes: true }).filter((d) => d.isDirectory());
  const nested = suites.flatMap((suite) =>
    readdirSync(join(E2E_ROOT, suite.name))
      .filter((name) => /^playwright(\..+)?\.config\.ts$/.test(name))
      .map((name) => join(E2E_ROOT, suite.name, name))
  );
  return [join(WEB_ROOT, 'playwright.config.ts'), ...nested];
}

/** forbidOnly set from the CI env var, directly or through a CI constant. */
const FORBID_ONLY = /^\s*forbidOnly:\s*(!!process\.env\.CI|CI),/m;

describe('Playwright configs', () => {
  const configs = playwrightConfigs();

  it('finds the suite configs', () => {
    expect(configs.length).toBeGreaterThan(1);
  });

  it.each(configs.map((path) => [relative(WEB_ROOT, path), path]))(
    '%s sets forbidOnly when CI is set',
    (_name, path) => {
      expect(readFileSync(path, 'utf8')).toMatch(FORBID_ONLY);
    }
  );
});
