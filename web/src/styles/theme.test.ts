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
 * Shoelace theme integration, stylesheet load-order independence, dark-block
 * parity, and WCAG AA contrast checks for web/src/styles/theme.css
 * (ptone/scion#6800, ptone/scion#7774, ptone/scion#3773, ptone/scion#8353).
 */

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, relative, resolve, sep } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
  block,
  contrast,
  decls,
  over,
  resolver,
  themeTokens,
} from '../components/pages/__fixtures__/theme-contrast.js';

const webDir = resolve(__dirname, '../..');
const themeCss = readFileSync(resolve(__dirname, 'theme.css'), 'utf8');
const shoelaceLightCss = readFileSync(
  resolve(webDir, 'node_modules/@shoelace-style/shoelace/dist/themes/light.css'),
  'utf8'
);
const shoelaceDarkCss = readFileSync(
  resolve(webDir, 'node_modules/@shoelace-style/shoelace/dist/themes/dark.css'),
  'utf8'
);

type Specificity = [number, number, number];

interface ElementState {
  isRoot: boolean;
  classes: Set<string>;
  dataTheme?: 'light' | 'dark';
  prefersDark: boolean;
}

interface ParsedRule {
  selectors: string[];
  prefersDarkOnly: boolean;
  props: Map<string, string>;
}

/** Parse top-level and @media (prefers-color-scheme: dark) custom-property rules. */
function parseThemeRules(cssText: string): ParsedRule[] {
  const clean = cssText.replace(/\/\*[\s\S]*?\*\//g, '');
  const rules: ParsedRule[] = [];
  let i = 0;

  const parseRuleList = (src: string, prefersDarkOnly: boolean): void => {
    let pos = 0;
    while (pos < src.length) {
      const open = src.indexOf('{', pos);
      if (open < 0) break;
      const prelude = src.slice(pos, open).trim();
      let depth = 0;
      let close = open;
      for (; close < src.length; close++) {
        if (src[close] === '{') depth++;
        if (src[close] === '}' && --depth === 0) break;
      }
      const inner = src.slice(open + 1, close);
      if (prelude.startsWith('@media')) {
        if (prelude.includes('prefers-color-scheme: dark')) {
          parseRuleList(inner, true);
        }
      } else if (!prelude.startsWith('@')) {
        const props = decls(inner);
        if (props.size > 0) {
          rules.push({
            selectors: prelude
              .split(',')
              .map((s) => s.trim())
              .filter(Boolean),
            prefersDarkOnly,
            props,
          });
        }
      }
      pos = close + 1;
    }
    i = pos;
  };

  parseRuleList(clean, false);
  void i;
  return rules;
}

/** Match and compute CSS selector specificity for html/:root and descendant .sl-theme-* containers. */
function matchSelector(sel: string, el: ElementState): Specificity | null {
  if (sel === ':host') return null;

  // Descendant selectors: `html .sl-theme-light` or `html .sl-theme-dark`
  if (sel.startsWith('html ')) {
    if (el.isRoot) return null;
    const sub = matchSimpleCompound(sel.slice(5).trim(), { ...el, isRoot: false });
    return sub ? [sub[0], sub[1], sub[2] + 1] : null;
  }

  return matchSimpleCompound(sel, el);
}

function matchSimpleCompound(sel: string, el: ElementState): Specificity | null {
  let rest = sel;
  let b = 0;
  let c = 0;

  if (rest.startsWith('html')) {
    if (!el.isRoot) return null;
    c += 1;
    rest = rest.slice(4);
  }

  while (rest.length > 0) {
    if (rest.startsWith(':root')) {
      if (!el.isRoot) return null;
      b += 1;
      rest = rest.slice(5);
      continue;
    }
    const notAttr = /^:not\(\[data-theme=['"](\w+)['"]\]\)/.exec(rest);
    if (notAttr) {
      if (el.dataTheme === notAttr[1]) return null;
      b += 1;
      rest = rest.slice(notAttr[0].length);
      continue;
    }
    const attr = /^\[data-theme=['"](\w+)['"]\]/.exec(rest);
    if (attr) {
      if (el.dataTheme !== attr[1]) return null;
      b += 1;
      rest = rest.slice(attr[0].length);
      continue;
    }
    const cls = /^\.([\w-]+)/.exec(rest);
    if (cls) {
      if (!el.classes.has(cls[1])) return null;
      b += 1;
      rest = rest.slice(cls[0].length);
      continue;
    }
    return null;
  }

  return [0, b, c];
}

function cmpSpec(x: Specificity, y: Specificity): number {
  return x[0] - y[0] || x[1] - y[1] || x[2] - y[2];
}

function cascade(sheets: string[], el: ElementState): Map<string, string> {
  const winners = new Map<string, { val: string; spec: Specificity; order: number }>();
  let order = 0;
  for (const sheet of sheets) {
    for (const rule of parseThemeRules(sheet)) {
      if (rule.prefersDarkOnly && !el.prefersDark) continue;
      let best: Specificity | null = null;
      for (const sel of rule.selectors) {
        const sp = matchSelector(sel, el);
        if (sp && (!best || cmpSpec(sp, best) > 0)) best = sp;
      }
      if (!best) continue;
      order++;
      for (const [k, val] of rule.props) {
        const prev = winners.get(k);
        if (!prev || cmpSpec(best, prev.spec) >= 0) {
          winners.set(k, { val, spec: best, order });
        }
      }
    }
  }
  return new Map([...winners.entries()].map(([k, v]) => [k, v.val]));
}

describe('theme.css cascade & load-order independence (#6800)', () => {
  const orderDev = [shoelaceLightCss, shoelaceDarkCss, themeCss];
  const orderProdLazy = [themeCss, shoelaceLightCss, shoelaceDarkCss];

  const states: Array<[string, ElementState]> = [
    [
      'explicit light mode',
      { isRoot: true, classes: new Set(), dataTheme: 'light', prefersDark: false },
    ],
    [
      'forced light mode under OS dark preference',
      {
        isRoot: true,
        classes: new Set(['sl-theme-light']),
        dataTheme: 'light',
        prefersDark: true,
      },
    ],
    [
      'explicit dark mode under OS light preference',
      {
        isRoot: true,
        classes: new Set(['sl-theme-dark']),
        dataTheme: 'dark',
        prefersDark: false,
      },
    ],
    [
      'explicit dark mode under OS dark preference',
      {
        isRoot: true,
        classes: new Set(['sl-theme-dark']),
        dataTheme: 'dark',
        prefersDark: true,
      },
    ],
    [
      'system dark preference without data-theme attribute',
      { isRoot: true, classes: new Set(), prefersDark: true },
    ],
    [
      'descendant .sl-theme-dark container (e.g. confirm-dialog)',
      { isRoot: false, classes: new Set(['sl-theme-dark']), prefersDark: false },
    ],
  ];

  for (const [label, state] of states) {
    it(`resolves identical custom properties regardless of stylesheet order (${label})`, () => {
      const devVars = cascade(orderDev, state);
      const prodVars = cascade(orderProdLazy, state);
      expect(Object.fromEntries(prodVars)).toEqual(Object.fromEntries(devVars));
    });
  }
});

describe('theme.css dark block parity', () => {
  const explicitDark = decls(block(themeCss, '.sl-theme-dark,'));
  const systemDark = decls(block(themeCss, ":root:not([data-theme='light']) {"));

  it('declares the exact same properties and values in explicit dark and system dark blocks', () => {
    expect(Object.fromEntries(systemDark)).toEqual(Object.fromEntries(explicitDark));
  });
});

describe('Shoelace & neutral dark/light WCAG AA contrast (#7774, #6800, #3773)', () => {
  const { light, dark } = themeTokens();
  const systemDark = new Map([
    ...light,
    ...decls(block(themeCss, ":root:not([data-theme='light']) {")),
  ]);

  const themes = [
    ['light', light],
    ['dark', dark],
    ['system dark', systemDark],
  ] as const;

  const variants = ['primary', 'success', 'warning', 'danger', 'neutral'] as const;

  it('preserves stock Scion light-mode palette values', () => {
    const get = resolver(light);
    expect(get('--sl-color-neutral-0')).toEqual([255, 255, 255, 1]);
    expect(get('--sl-color-neutral-700')).toEqual([0x33, 0x41, 0x55, 1]);
    expect(get('--sl-color-primary-600')).toEqual([0x25, 0x63, 0xeb, 1]);
    expect(get('--scion-bg')).toEqual([0xf8, 0xfa, 0xfc, 1]);
    expect(get('--scion-surface')).toEqual([255, 255, 255, 1]);
  });

  for (const [themeName, vars] of themes) {
    describe(`${themeName} theme`, () => {
      const get = resolver(vars);

      it('keeps default sl-button and sl-input text & border readable (#6800, #3773)', () => {
        // Standard default sl-button & sl-input: neutral-700 text on neutral-0 fill
        expect(
          contrast(get('--sl-color-neutral-700'), get('--sl-color-neutral-0'))
        ).toBeGreaterThanOrEqual(4.5);
        // Placeholder (neutral-500) on neutral-0 input fill
        expect(
          contrast(get('--sl-color-neutral-500'), get('--sl-color-neutral-0'))
        ).toBeGreaterThanOrEqual(4.5);
        // Help text on page & card surfaces
        expect(
          contrast(get('--sl-input-help-text-color'), get('--scion-bg'))
        ).toBeGreaterThanOrEqual(4.5);
        expect(
          contrast(get('--sl-input-help-text-color'), get('--scion-surface'))
        ).toBeGreaterThanOrEqual(4.5);
        // Filled input: --sl-input-filled-color on --sl-input-filled-background-color
        expect(
          contrast(get('--sl-input-filled-color'), get('--sl-input-filled-background-color'))
        ).toBeGreaterThanOrEqual(4.5);
        if (themeName !== 'light') {
          // Non-text UI boundary (WCAG 1.4.11 >= 3:1) for input/default-button border in dark mode
          expect(
            contrast(get('--sl-input-border-color'), get('--scion-bg'))
          ).toBeGreaterThanOrEqual(3);
          expect(
            contrast(get('--sl-input-border-color'), get('--scion-surface'))
          ).toBeGreaterThanOrEqual(3);
        }
      });

      it('keeps outline buttons and neutral text colours AA-readable on page and card surfaces (#3773)', () => {
        for (const surface of ['--scion-bg', '--scion-surface', '--scion-surface-raised']) {
          expect(
            contrast(get('--sl-color-neutral-700'), get(surface)),
            `neutral-700 on ${surface}`
          ).toBeGreaterThanOrEqual(4.5);
          expect(
            contrast(get('--sl-color-neutral-600'), get(surface)),
            `neutral-600 on ${surface}`
          ).toBeGreaterThanOrEqual(4.5);
        }
        // Stock light-mode keeps Scion's pre-existing Tailwind success-600 (#16a34a) and
        // warning-600 (#d97706) unchanged per #3773 ("Light mode looks the same as before");
        // dark & system-dark invert all 5 scales to meet >= 4.5:1 AA across every variant.
        const outlineVariants =
          themeName === 'light' ? (['primary', 'danger', 'neutral'] as const) : variants;
        for (const surface of ['--scion-bg', '--scion-surface']) {
          expect(
            contrast(get('--sl-color-neutral-500'), get(surface)),
            `neutral-500 on ${surface}`
          ).toBeGreaterThanOrEqual(4.5);
          for (const v of outlineVariants) {
            expect(
              contrast(get(`--sl-color-${v}-600`), get(surface)),
              `${v}-600 outline/text on ${surface}`
            ).toBeGreaterThanOrEqual(4.5);
          }
        }
      });

      it('keeps filled sl-button and sl-badge variants AA-readable (#7774)', () => {
        const filledVariants =
          themeName === 'light' ? (['primary', 'danger', 'neutral'] as const) : variants;
        for (const v of filledVariants) {
          expect(
            contrast(get('--sl-color-neutral-0'), get(`--sl-color-${v}-600`)),
            `neutral-0 label on ${v}-600`
          ).toBeGreaterThanOrEqual(4.5);
          if (themeName !== 'light') {
            expect(
              contrast(get('--sl-color-neutral-0'), get(`--sl-color-${v}-500`)),
              `neutral-0 hover label on ${v}-500`
            ).toBeGreaterThanOrEqual(4.5);
          }
        }
      });

      it('keeps sl-alert panel surface and message text AA-readable (#7774)', () => {
        const panelBg = get('--sl-panel-background-color');
        expect(contrast(get('--sl-color-neutral-700'), panelBg)).toBeGreaterThanOrEqual(4.5);
        if (themeName !== 'light') {
          // Dark alert panel must be a dark surface, not light #f8fafc/#ffffff
          expect(contrast(get('--scion-text'), panelBg)).toBeGreaterThanOrEqual(4.5);
        }
      });

      it('keeps sl-tag and subtle callout pairs AA-readable', () => {
        const ground = get('--scion-surface');
        for (const v of variants) {
          const bg50 = over(get(`--sl-color-${v}-50`), ground);
          const bg100 = over(get(`--sl-color-${v}-100`), ground);
          expect(
            contrast(get(`--sl-color-${v}-800`), bg50),
            `${v}-800 on ${v}-50`
          ).toBeGreaterThanOrEqual(4.5);
          expect(
            contrast(get(`--sl-color-${v}-700`), bg100),
            `${v}-700 on ${v}-100`
          ).toBeGreaterThanOrEqual(4.5);
        }
      });
    });
  }

  it('never pairs an inverted --sl-color-*-500/600/700 fill with a bare white foreground', () => {
    const srcDir = resolve(__dirname, '..');
    const walk = (dir: string): string[] => {
      const out: string[] = [];
      for (const name of readdirSync(dir)) {
        if (name === '__fixtures__') continue;
        const p = join(dir, name);
        if (statSync(p).isDirectory()) out.push(...walk(p));
        else if (/\.(ts|css)$/.test(name) && !/\.test\.ts$/.test(name) && name !== 'theme.css') {
          out.push(p);
        }
      }
      return out;
    };

    const violations: string[] = [];
    for (const file of walk(srcDir).sort()) {
      const clean = readFileSync(file, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
      for (const m of clean.matchAll(/([^{}]*\{[^{}]*\})/g)) {
        const ruleText = m[1];
        if (
          /background(?:-color)?\s*:\s*var\(\s*--sl-color-(?:primary|success|warning|danger|neutral)-(?:500|600|700)/.test(
            ruleText
          ) &&
          /(?:^|[;\s{])color\s*:\s*(?:#fff(?:fff)?\b|white\b)/i.test(ruleText)
        ) {
          violations.push(
            `${relative(srcDir, file).split(sep).join('/')}: ${ruleText.trim().replace(/\s+/g, ' ')}`
          );
        }
      }
    }
    expect(violations).toEqual([]);
  });
});
