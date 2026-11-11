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
 * The ESLint override that keeps components on client/navigation.ts
 * (ptone/scion#2857, ptone/scion#3118): which files it covers, and that it
 * catches each way of reaching client/main or history.pushState/replaceState.
 */

import { describe, it, expect } from 'vitest';
import { resolve } from 'node:path';
import { ESLint, Linter } from 'eslint';

const webRoot = resolve(__dirname, '../..');
const eslint = new ESLint({ cwd: webRoot });

type RuleEntry = Linter.RuleEntry | undefined;

async function rulesFor(file: string): Promise<Record<string, RuleEntry>> {
  const config = (await eslint.calculateConfigForFile(resolve(webRoot, file))) as {
    rules?: Record<string, RuleEntry>;
  };
  return config.rules ?? {};
}

function isError(entry: RuleEntry): boolean {
  const level = Array.isArray(entry) ? entry[0] : entry;
  return level === 'error' || level === 2;
}

/** Whether an enabled no-restricted-syntax entry has a selector matching `re`. */
function hasSelector(entry: RuleEntry, re: RegExp): boolean {
  if (!isError(entry) || !Array.isArray(entry)) return false;
  return entry
    .slice(1)
    .some(
      (o) =>
        typeof o === 'object' &&
        o !== null &&
        re.test(String((o as { selector?: unknown }).selector))
    );
}

const HISTORY_SELECTOR = /\(push\|replace\)State/;
const LOCATION_SELECTOR = /left\.property\.name="href"/;

describe('navigation lint rule coverage', () => {
  it.each([
    'src/components/pages/skills.ts',
    'src/components/pages/admin-groups.ts',
    'src/components/shared/notification-tray.ts',
  ])('applies to component %s', async (file) => {
    const rules = await rulesFor(file);
    expect(isError(rules['no-restricted-imports'])).toBe(true);
    expect(isError(rules['no-restricted-syntax'])).toBe(true);
  });

  it.each([
    // The router itself.
    'src/client/main.ts',
    'src/client/route-history.ts',
    'src/client/navigation.ts',
    // Tests vi.mock client/main legitimately.
    'src/components/pages/skills.test.ts',
    // Temporary chat exception (migrated by the chat lane).
    'src/components/pages/chat.ts',
    'src/components/shared/chat/chat-thread.ts',
  ])('does not apply to %s', async (file) => {
    const rules = await rulesFor(file);
    expect(isError(rules['no-restricted-imports'])).toBe(false);
    // The location-write rule (below) may apply; the history rule must not.
    expect(hasSelector(rules['no-restricted-syntax'], HISTORY_SELECTOR)).toBe(false);
  });
});

// Raw window.location writes drop the base path (ptone/scion#5535): every
// source file outside navigation.ts is covered, components included
// (where the rule shares no-restricted-syntax with the history rule).
describe('location write lint rule', () => {
  it.each([
    'src/client/api.ts',
    'src/client/main.ts',
    'src/utils/auth.ts',
    'src/components/pages/skills.ts',
    'src/components/pages/chat.ts',
    'src/components/shared/chat/chat-thread.ts',
  ])('applies to %s', async (file) => {
    const rules = await rulesFor(file);
    expect(hasSelector(rules['no-restricted-syntax'], LOCATION_SELECTOR)).toBe(true);
  });

  it('keeps the history rule on components alongside it', async () => {
    const rules = await rulesFor('src/components/pages/skills.ts');
    expect(hasSelector(rules['no-restricted-syntax'], HISTORY_SELECTOR)).toBe(true);
  });

  it.each(['src/client/navigation.ts', 'src/client/api.test.ts'])(
    'does not apply to %s',
    async (file) => {
      const rules = await rulesFor(file);
      expect(hasSelector(rules['no-restricted-syntax'], LOCATION_SELECTOR)).toBe(false);
    }
  );

  async function lintClient(code: string): Promise<string[]> {
    const rules = await rulesFor('src/client/api.ts');
    const linter = new Linter();
    return linter
      .verify(code, {
        languageOptions: { ecmaVersion: 'latest', sourceType: 'module' },
        rules: { 'no-restricted-syntax': rules['no-restricted-syntax'] } as Linter.RulesRecord,
      })
      .map((m) => m.ruleId ?? 'parse-error');
  }

  it.each([
    "window.location.href = '/brokers';",
    'location.href = `/login?returnTo=${r}`;',
    'globalThis.location.href = url;',
    "window.location = '/x';",
    "window.location.assign('/y');",
    'location.replace(target);',
  ])('flags %s', async (code) => {
    expect(await lintClient(code)).toEqual(['no-restricted-syntax']);
  });

  it('allows the helpers and reading the location', async () => {
    expect(
      await lintClient(
        "loadPage('/a'); navigateTo('/b'); const p = window.location.pathname;" +
          ' const h = location.href; window.location.reload();'
      )
    ).toEqual([]);
  });
});

describe('navigation lint rule matching', () => {
  async function lint(code: string): Promise<string[]> {
    const rules = await rulesFor('src/components/pages/skills.ts');
    const linter = new Linter();
    const messages = linter.verify(code, {
      languageOptions: { ecmaVersion: 'latest', sourceType: 'module' },
      rules: {
        'no-restricted-imports': rules['no-restricted-imports'],
        'no-restricted-syntax': rules['no-restricted-syntax'],
      } as Linter.RulesRecord,
    });
    return messages.map((m) => m.ruleId ?? 'parse-error');
  }

  it.each([
    "import { navigateTo } from '../../client/main.js';",
    "import { stateManager } from '../client/main';",
    "export { navigateTo } from '../../client/main.js';",
  ])('flags importing client/main: %s', async (code) => {
    expect(await lint(code)).toEqual(['no-restricted-imports']);
  });

  it.each([
    "void import('../../client/main.js');",
    "const main = await import('../client/main');",
    'void import(`../../client/main.js`);',
  ])('flags dynamic import of client/main: %s', async (code) => {
    expect(await lint(code)).toEqual(['no-restricted-syntax']);
  });

  it('allows dynamic imports of other modules', async () => {
    expect(await lint("void import('../../client/navigation.js');")).toEqual([]);
    expect(await lint('void import(`../../client/${name}.js`);')).toEqual([]);
  });

  it.each([
    "window.history.pushState({}, '', '/x');",
    "history.replaceState(null, '', '/y');",
    "window.history['pushState']({}, '', '/z');",
    'const push = window.history.pushState;',
    'window.history[`pushState`]({}, "", "/t");',
    'const { pushState } = window.history;',
    'const { replaceState: replace } = history;',
    "const { 'pushState': p } = history;",
  ])('flags raw history calls: %s', async (code) => {
    expect(await lint(code)).toEqual(['no-restricted-syntax']);
  });

  it('allows the navigation helpers', async () => {
    expect(
      await lint(
        "import { navigateTo, pushUrl, replaceSearch } from '../../client/navigation.js';\n" +
          "navigateTo('/a'); pushUrl('/b'); replaceSearch('q=1'); window.history.back();"
      )
    ).toEqual([]);
  });
});
