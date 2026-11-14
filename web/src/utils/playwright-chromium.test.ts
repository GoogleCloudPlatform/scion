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
 * Verifies shared Chromium resolution for Playwright and guards every
 * browser-launching Playwright config (plus the root suite's global-setup)
 * against drifting back to inconsistent or missing browser env var handling.
 */

import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync } from 'node:fs';
import { join, relative, resolve } from 'node:path';

import {
  CHROMIUM_CONTAINER_ARGS,
  chromiumExecutablePath,
  chromiumLaunchOptions,
} from '../../playwright-chromium.js';

const WEB_ROOT = resolve(__dirname, '../..');
const E2E_CONFIG_NAME = /^playwright.*\.config\.ts$/;

/** Configs that deliberately run no browser tests. */
const NO_BROWSER_CONFIGS = new Set(['e2e/harness-selftest/playwright.config.ts']);

/** Configs that inherit use.launchOptions by spreading a sibling base config. */
const INHERITED_BASE_CONFIGS = new Map<string, string>([
  ['e2e/port-proxy/playwright.view.config.ts', 'e2e/port-proxy/playwright.config.ts'],
]);

function walkConfigs(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) return walkConfigs(full);
    return entry.isFile() && E2E_CONFIG_NAME.test(entry.name) ? [full] : [];
  });
}

function browserPlaywrightConfigs(): string[] {
  const all = [
    join(WEB_ROOT, 'playwright.config.ts'),
    ...walkConfigs(join(WEB_ROOT, 'e2e')).sort(),
    ...walkConfigs(join(WEB_ROOT, 'e2e-perf')).sort(),
  ];
  return all.filter((full) => !NO_BROWSER_CONFIGS.has(relative(WEB_ROOT, full)));
}

describe('chromiumExecutablePath', () => {
  it('returns undefined when no browser environment variable is set', () => {
    expect(chromiumExecutablePath({})).toBeUndefined();
  });

  it('uses PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH in agent containers', () => {
    expect(
      chromiumExecutablePath({
        PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH: '/usr/bin/chromium',
      })
    ).toBe('/usr/bin/chromium');
  });

  it('prefers CHROMIUM_EXECUTABLE over TERMINAL_OWNER_CHROMIUM and PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH', () => {
    expect(
      chromiumExecutablePath({
        CHROMIUM_EXECUTABLE: '/opt/chromium/chrome',
        TERMINAL_OWNER_CHROMIUM: '/usr/local/bin/chromium',
        PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH: '/usr/bin/chromium',
      })
    ).toBe('/opt/chromium/chrome');
  });

  it('prefers explicit TERMINAL_OWNER_CHROMIUM over ambient PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH', () => {
    expect(
      chromiumExecutablePath({
        TERMINAL_OWNER_CHROMIUM: '/usr/local/bin/chromium',
        PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH: '/usr/bin/chromium',
      })
    ).toBe('/usr/local/bin/chromium');
  });

  it('trims whitespace and skips empty values', () => {
    expect(
      chromiumExecutablePath({
        CHROMIUM_EXECUTABLE: '   ',
        TERMINAL_OWNER_CHROMIUM: '',
        PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH: '  /usr/bin/chromium  ',
      })
    ).toBe('/usr/bin/chromium');
    expect(
      chromiumExecutablePath({
        CHROMIUM_EXECUTABLE: '',
        TERMINAL_OWNER_CHROMIUM: '  ',
        PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH: '\t\n',
      })
    ).toBeUndefined();
  });
});

describe('chromiumLaunchOptions', () => {
  it('omits executablePath when no env var is set so Playwright uses its managed bundle', () => {
    const opts = chromiumLaunchOptions({ env: {} });
    expect(opts).toEqual({ args: [...CHROMIUM_CONTAINER_ARGS] });
    expect('executablePath' in opts).toBe(false);
  });

  it('sets executablePath alongside container sandbox flags when an env var is set', () => {
    const opts = chromiumLaunchOptions({
      env: { PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH: '/usr/bin/chromium' },
    });
    expect(opts).toEqual({
      executablePath: '/usr/bin/chromium',
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    });
  });

  it('supports custom args and omits args when given an empty list', () => {
    expect(chromiumLaunchOptions({ args: ['--no-sandbox'], env: {} })).toEqual({
      args: ['--no-sandbox'],
    });
    expect(chromiumLaunchOptions({ args: [], env: {} })).toEqual({});
  });
});

describe('Playwright configs and global-setup use chromiumLaunchOptions', () => {
  const configs = browserPlaywrightConfigs();

  it('discovers the root, e2e, and e2e-perf Playwright configs', () => {
    const rels = configs.map((full) => relative(WEB_ROOT, full));
    expect(rels).toContain('playwright.config.ts');
    expect(rels).toContain('e2e/terminal-owner/playwright.config.ts');
    expect(rels).toContain('e2e-perf/budgets/playwright.config.ts');
    expect(rels.length).toBeGreaterThanOrEqual(25);
  });

  it.each(configs.map((full) => [relative(WEB_ROOT, full), full]))(
    '%s resolves Chromium via chromiumLaunchOptions',
    (rel, full) => {
      const targetRel = INHERITED_BASE_CONFIGS.get(rel) ?? rel;
      const source = readFileSync(join(WEB_ROOT, targetRel), 'utf8');
      if (targetRel !== rel) {
        const wrapper = readFileSync(full, 'utf8');
        expect(wrapper).toMatch(/\.\.\.base\b/);
      }
      expect(source).toMatch(/\bchromiumLaunchOptions\s*\(/);
      expect(source).not.toMatch(
        /process\.env\.(CHROMIUM_EXECUTABLE|TERMINAL_OWNER_CHROMIUM|PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH)\b/
      );
    }
  );

  it('e2e/harness/global-setup.ts launches Chromium with chromiumLaunchOptions()', () => {
    const setupSource = readFileSync(join(WEB_ROOT, 'e2e/harness/global-setup.ts'), 'utf8');
    expect(setupSource).toMatch(/chromium\.launch\(\s*chromiumLaunchOptions\(/);
    expect(setupSource).not.toMatch(/chromium\.launch\(\s*\)/);
  });

  it('playwright.config.ts disables video recording when using a system Chromium binary without bundled ffmpeg', () => {
    const rootSource = readFileSync(join(WEB_ROOT, 'playwright.config.ts'), 'utf8');
    expect(rootSource).toMatch(/video:\s*chromiumExecutablePath\(\)\s*\?\s*'off'/);
  });
});
