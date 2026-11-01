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
 * Review mark, comment badge and comment quote colours (ptone/scion#6424): each is a text
 * colour on its own background, from the --scion-review-* tokens of
 * theme.css, and meets WCAG AA (4.5:1) in the light theme, the dark theme
 * and the system dark preference. The editor and the comment dialog read
 * those tokens, with the light values as fallbacks.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  block,
  contrast,
  decls,
  resolver,
  themeTokens,
} from '../pages/__fixtures__/theme-contrast.js';

const themeCss = readFileSync(resolve(__dirname, '../../styles/theme.css'), 'utf8');
const { light, dark } = themeTokens();
const systemDark = new Map([
  ...light,
  ...decls(block(themeCss, ":root:not([data-theme='light']) {")),
]);

const THEMES = [
  ['light', light],
  ['dark', dark],
  ['system dark', systemDark],
] as const;

const PAIRS = [
  ['insertion', '--scion-review-ins-text', '--scion-review-ins-bg'],
  ['deletion', '--scion-review-del-text', '--scion-review-del-bg'],
  ['highlight and comment quote', '--scion-review-hl-text', '--scion-review-hl-bg'],
  ['comment badge', '--scion-review-note-text', '--scion-review-note-bg'],
] as const;

const REVIEW_TOKEN = /^--scion-review-/;

describe('review mark colours', () => {
  for (const [theme, vars] of THEMES) {
    it(`meet AA contrast in the ${theme} theme`, () => {
      const get = resolver(vars);
      for (const [what, fg, bg] of PAIRS) {
        expect(contrast(get(fg), get(bg)), `${what} (${theme})`).toBeGreaterThanOrEqual(4.5);
        // Opaque, so the page behind does not change the ratio.
        expect(get(bg)[3], `${bg} (${theme})`).toBe(1);
      }
    });
  }

  it('differ between the light and dark themes, and both dark blocks agree', () => {
    const names = [...light.keys()].filter((k) => REVIEW_TOKEN.test(k));
    expect(names.length).toBe(12);
    for (const n of names) {
      expect(dark.get(n), n).toBe(systemDark.get(n));
      expect(dark.get(n), n).not.toBe(light.get(n));
    }
  });
});

describe('review styles read the theme tokens', () => {
  const read = (f: string): string => readFileSync(resolve(__dirname, f), 'utf8');
  const editor = read('review-editor-styles.ts');
  const dialog = read('review-note-dialog.ts');
  const sources: [string, string][] = [
    ['review-editor-styles.ts', editor],
    ['review-note-dialog.ts', dialog],
  ];

  it.each(sources)('%s falls back to the light values', (_f, src) => {
    const uses = [...src.matchAll(/var\((--scion-review-[\w-]+),\s*([^)]+)\)/g)];
    expect(uses.length).toBeGreaterThan(0);
    for (const [, name, fallback] of uses) expect(fallback.trim(), name).toBe(light.get(name));
  });

  it('use the tokens for every mark and the dialog quote', () => {
    const flat = (s: string): string => s.replace(/\s+/g, ' ');
    expect(flat(editor)).toMatch(
      /ins\.critic-sub-new \{ color: var\(--scion-review-ins-text, [^)]+\); background: var\(--scion-review-ins-bg/
    );
    expect(flat(editor)).toMatch(
      /del\.critic-sub-old \{ color: var\(--scion-review-del-text, [^)]+\); background: var\(--scion-review-del-bg/
    );
    expect(flat(editor)).toMatch(
      /mark\.critic-hl \{ background: var\(--scion-review-hl-bg, [^)]+\); color: var\(--scion-review-hl-text/
    );
    expect(flat(editor)).toMatch(
      /\.critic-comment \{[^}]*color: var\(--scion-review-note-text, [^)]+\); background: var\(--scion-review-note-bg/
    );
    expect(flat(editor)).not.toContain('--sl-color-violet');
    expect(flat(dialog)).toMatch(
      /\.quote \{[^}]*background: var\(--scion-review-hl-bg, [^)]+\); color: var\(--scion-review-hl-text/
    );
    // Strikethrough keeps the text colour and no background, so it differs
    // from a suggested deletion by more than colour in either theme.
    expect(flat(editor)).toContain('s.gfm-strike { color: inherit; background: none;');
  });
});
