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
  };

  parseRuleList(clean, false);
  return rules;
}

/**
 * Match and compute CSS selector specificity for html/:root and descendant
 * .sl-theme-* containers. Throws on unrecognized selector syntax so new
 * combinators or pseudo-classes in theme.css cannot be skipped silently.
 */
function matchSelector(sel: string, el: ElementState): Specificity | null {
  if (sel === ':host') return null;

  // Descendant selectors: `html .sl-theme-light` or `html .sl-theme-dark`
  if (sel.startsWith('html ')) {
    const rest = sel.slice(5).trim();
    const parsed = parseCompoundSelector(rest);
    if (el.isRoot) return null;
    const sub = evaluateCompound(parsed, { ...el, isRoot: false });
    return sub ? [sub[0], sub[1], sub[2] + 1] : null;
  }

  return evaluateCompound(parseCompoundSelector(sel), el);
}

interface CompoundPart {
  kind: 'html' | 'root' | 'notDataTheme' | 'dataTheme' | 'class';
  value?: string;
}

/**
 * Extend `parseCompoundSelector` when adding new custom-property selector
 * forms to `theme.css`. Non-token utility rules (e.g. `.scion-focus-ring:focus-visible`)
 * have no `--*` declarations and are already ignored by `parseThemeRules`.
 */
function parseCompoundSelector(sel: string): CompoundPart[] {
  const parts: CompoundPart[] = [];
  let rest = sel;

  if (rest.startsWith('html')) {
    parts.push({ kind: 'html' });
    rest = rest.slice(4);
  }

  while (rest.length > 0) {
    if (rest.startsWith(':root')) {
      parts.push({ kind: 'root' });
      rest = rest.slice(5);
      continue;
    }
    const notAttr = /^:not\(\[data-theme=['"](\w+)['"]\]\)/.exec(rest);
    if (notAttr) {
      parts.push({ kind: 'notDataTheme', value: notAttr[1] });
      rest = rest.slice(notAttr[0].length);
      continue;
    }
    const attr = /^\[data-theme=['"](\w+)['"]\]/.exec(rest);
    if (attr) {
      parts.push({ kind: 'dataTheme', value: attr[1] });
      rest = rest.slice(attr[0].length);
      continue;
    }
    const cls = /^\.([\w-]+)/.exec(rest);
    if (cls) {
      parts.push({ kind: 'class', value: cls[1] });
      rest = rest.slice(cls[0].length);
      continue;
    }
    throw new Error(`Unsupported theme selector syntax: "${sel}" (at "${rest}")`);
  }

  if (parts.length === 0) {
    throw new Error(`Empty compound selector: "${sel}"`);
  }
  return parts;
}

function evaluateCompound(parts: CompoundPart[], el: ElementState): Specificity | null {
  let b = 0;
  let c = 0;
  for (const part of parts) {
    switch (part.kind) {
      case 'html':
        if (!el.isRoot) return null;
        c += 1;
        break;
      case 'root':
        if (!el.isRoot) return null;
        b += 1;
        break;
      case 'notDataTheme':
        if (el.dataTheme === part.value) return null;
        b += 1;
        break;
      case 'dataTheme':
        if (el.dataTheme !== part.value) return null;
        b += 1;
        break;
      case 'class':
        if (!part.value || !el.classes.has(part.value)) return null;
        b += 1;
        break;
    }
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

  it('derives dark --sl-color-primary-900/950 from --scion-primary-50 without overriding --scion-primary-50', () => {
    for (const darkBlock of [explicitDark, systemDark]) {
      expect(darkBlock.has('--scion-primary-50')).toBe(false);
      expect(darkBlock.get('--sl-color-primary-900')).toBe('var(--scion-primary-50)');
      expect(darkBlock.get('--sl-color-primary-950')).toBe('var(--scion-primary-50)');
    }
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

  it('preserves stock Scion light-mode palette and Shoelace non-palette values', () => {
    const get = resolver(light);
    expect(get('--sl-color-neutral-0')).toEqual([255, 255, 255, 1]);
    expect(get('--sl-color-neutral-700')).toEqual([0x33, 0x41, 0x55, 1]);
    expect(get('--sl-color-primary-600')).toEqual([0x25, 0x63, 0xeb, 1]);
    expect(get('--scion-bg')).toEqual([0xf8, 0xfa, 0xfc, 1]);
    expect(get('--scion-surface')).toEqual([255, 255, 255, 1]);
    // Light-mode equivalence for tokens rewritten to semantic / color-mix() forms in PR 2c.
    expect(get('--scion-primary')).toEqual([59, 130, 246, 1]);
    expect(get('--scion-primary-tint')).toEqual([0xef, 0xf6, 0xff, 1]);
    expect(get('--scion-primary-text')).toEqual([255, 255, 255, 1]);
    expect(get('--scion-error-bg')).toEqual([0xfe, 0xf2, 0xf2, 1]);
    expect(get('--scion-badge-danger-border')).toEqual([0xfe, 0xca, 0xca, 1]);
    expect(get('--scion-error-text')).toEqual([0x99, 0x1b, 0x1b, 1]);
    expect(get('--scion-error-text-hover')).toEqual([0x7f, 0x1d, 0x1d, 1]);
    expect(get('--scion-action-hover-danger-bg')).toEqual([239, 68, 68, 0.1]);
    expect(get('--scion-action-hover-warning-bg')).toEqual([245, 158, 11, 0.1]);
    expect(get('--scion-action-hover-success-bg')).toEqual([34, 197, 94, 0.1]);
    expect(get('--scion-action-hover-primary-bg')).toEqual([59, 130, 246, 0.1]);
    // Explicit scoped light-mode contrast-improvement exceptions (PR 2c):
    // unauthorized.ts .help-text moved from #64748b on #fef3c7 (4.27:1) to warning tokens (>= 4.5:1 AA).
    expect(
      contrast(get('--scion-warning-text'), get('--scion-warning-bg')),
      'unauthorized .help-text warning-text on warning-bg (light AA improvement)'
    ).toBeGreaterThanOrEqual(4.5);

    // Every non-palette --sl-* declaration in the light block must match Shoelace's light.css
    // (except Scion's intentional font-family and border-radius overrides) so light mode never
    // drifts on dialog overlays, panels, inputs, focus rings, or tooltips.
    const slLight = decls(block(shoelaceLightCss, '.sl-theme-light {'));
    const scionLight = decls(block(themeCss, '.sl-theme-light {'));
    const intentionalNonColorOverrides = new Set([
      '--sl-font-sans',
      '--sl-font-mono',
      '--sl-border-radius-small',
      '--sl-border-radius-medium',
      '--sl-border-radius-large',
      '--sl-border-radius-x-large',
    ]);
    for (const [prop, val] of scionLight) {
      if (!prop.startsWith('--sl-') || prop.startsWith('--sl-color-')) continue;
      if (intentionalNonColorOverrides.has(prop)) continue;
      expect(val, `${prop} in light theme`).toBe(slLight.get(prop)?.replace(/\s+/g, ' '));
    }
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

      it('keeps sl-menu-item and sl-option hover visibly distinct from panel and card surfaces', () => {
        // Hover fill (neutral-100) must visibly step away from panel and card surfaces
        if (themeName !== 'light') {
          expect(
            contrast(get('--sl-color-neutral-100'), get('--sl-panel-background-color'))
          ).toBeGreaterThanOrEqual(1.2);
          expect(
            contrast(get('--sl-color-neutral-100'), get('--scion-surface'))
          ).toBeGreaterThanOrEqual(1.2);
        } else {
          expect(get('--sl-color-neutral-100')).not.toEqual(get('--sl-panel-background-color'));
        }
        // Hovered menu/option item text (neutral-1000) on hover fill (neutral-100)
        expect(
          contrast(get('--sl-color-neutral-1000'), get('--sl-color-neutral-100'))
        ).toBeGreaterThanOrEqual(4.5);
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
        const primaryTintBg = over(get('--scion-primary-tint'), ground);
        expect(
          contrast(get('--scion-text'), primaryTintBg),
          'text on primary-tint over surface'
        ).toBeGreaterThanOrEqual(4.5);
        const errBg = over(get('--scion-error-bg'), ground);
        expect(
          contrast(get('--scion-error-text'), errBg),
          'error-text on error-bg'
        ).toBeGreaterThanOrEqual(4.5);
        expect(
          contrast(get('--scion-error-text-hover'), errBg),
          'error-text-hover on error-bg'
        ).toBeGreaterThanOrEqual(4.5);
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

  it('never pairs an inverted --sl-color-*-500/600/700 or --scion-status-* fill with a bare white foreground (including descendant rules)', () => {
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

    const invertedBgRe =
      /background(?:-color)?\s*:\s*var\(\s*(?:--sl-color-(?:primary|success|warning|danger|neutral)-(?:500|600|700)|--scion-status-(?:primary|success|warning|danger|info))/;
    const bareWhiteFgRe = /(?:^|[;\s{])color\s*:\s*(?:#fff(?:fff)?\b|white\b)/i;

    const violations: string[] = [];
    for (const file of walk(srcDir).sort()) {
      const rel = relative(srcDir, file).split(sep).join('/');
      const clean = readFileSync(file, 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
      const rules = [...clean.matchAll(/([^{};]+)\{([^{}]*)\}/g)].map((m) => ({
        selectors: m[1]
          .split(',')
          .map((s) => s.trim())
          .filter(Boolean),
        body: m[2],
        raw: `${m[1].trim()} { ${m[2].trim().replace(/\s+/g, ' ')} }`,
      }));

      const filledPrefixes = rules
        .filter((r) => invertedBgRe.test(r.body))
        .flatMap((r) => r.selectors)
        .filter((sel) => sel.length > 0 && !sel.includes('${'));

      for (const r of rules) {
        if (!bareWhiteFgRe.test(r.body)) continue;
        const sameRule = invertedBgRe.test(r.body);
        const childOfFilled =
          !sameRule &&
          r.selectors.some((sel) =>
            filledPrefixes.some((p) => sel.startsWith(`${p} `) || sel.startsWith(`${p}>`))
          );
        if (sameRule || childOfFilled) {
          violations.push(`${rel}: ${r.raw}`);
        }
      }
    }
    expect(violations).toEqual([]);
  });
});

/** Strip block and line comments without disturbing URLs (`https://...`). */
function stripComments(text: string): string {
  return text.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/(^|[ \t])\/\/[^\n]*/gm, '$1');
}

/** Replace balanced `var(...)` expressions so token fallbacks are not treated as bare literals. */
function stripVarExpressions(text: string): string {
  let out = '';
  let i = 0;
  while (i < text.length) {
    if (text.startsWith('var(', i)) {
      let depth = 0;
      let j = i + 3;
      let found = false;
      for (; j < text.length; j++) {
        if (text[j] === '(') depth++;
        else if (text[j] === ')') {
          depth--;
          if (depth === 0) {
            j++;
            found = true;
            break;
          }
        }
      }
      if (found) {
        out += 'var()';
        i = j;
      } else {
        out += text[i++];
      }
    } else {
      out += text[i++];
    }
  }
  return out;
}

const NEUTRAL_SHADOW_RE =
  /^(?:rgba\(\s*(?:0\s*,\s*0\s*,\s*0|15\s*,\s*23\s*,\s*42)\s*,|rgb\(\s*(?:0\s+0\s+0|15\s+23\s+42)\s*\/)/i;
const BARE_HEX_OR_FUNC_RE =
  /(?<![\w&])#(?:[0-9a-fA-F]{3}|(?=[0-9a-fA-F]*[a-fA-F])[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})\b|\b(?:rgba?|hsla?)\s*\([^)]*\)/g;
const NAMED_CSS_COLORS =
  'white|black|red|green|blue|yellow|orange|purple|pink|gray|grey|silver|maroon|navy|teal|aqua|cyan|magenta|fuchsia|lime|olive|brown|coral|crimson|gold|indigo|ivory|khaki|lavender|linen|orchid|plum|salmon|sienna|tan|thistle|tomato|turquoise|violet|wheat|rebeccapurple|slategray|slategrey|lightgray|lightgrey|darkgray|darkgrey|darkred|darkblue|darkgreen|lightblue|lightgreen|whitesmoke';
const BARE_NAMED_COLOR_RE = new RegExp(
  `(?:^|[;\\s{])(?:color|background(?:-color)?|border(?:-(?:top|right|bottom|left))?(?:-color)?|fill|stroke)\\s*:\\s*[^;{}]*?(?<![-\\w])(${NAMED_CSS_COLORS})(?![-\\w])`,
  'gi'
);

function findBareColorLiterals(text: string): string[] {
  const clean = stripVarExpressions(stripComments(text));
  const found: string[] = [];
  for (const m of clean.matchAll(BARE_HEX_OR_FUNC_RE)) {
    if (NEUTRAL_SHADOW_RE.test(m[0])) continue;
    found.push(m[0]);
  }
  for (const m of clean.matchAll(BARE_NAMED_COLOR_RE)) {
    found.push(m[1]);
  }
  return found;
}

/**
 * Explicit allowlist of remaining bare color literals outside `styles/theme.css`.
 *
 * Categories:
 * - Intentional non-theme palettes: terminal ANSI, CodeMirror syntax tokens,
 *   deterministic avatar wheels, Chart.js categorical series, QR quiet zones,
 *   OAuth provider SVGs, offline HTML exports, `<meta name="theme-color">`
 *   defaults, sandboxed iframe fallback tables, and neutral switch-thumb/scrim
 *   literals.
 */
const BARE_COLOR_ALLOWLIST: Record<string, { max: number; reason: string }> = {
  // Intentional non-theme palettes & fallbacks
  'client/artifact-preview.ts': {
    max: 20,
    reason: 'DEFAULT_THEME fallback table for sandboxed srcdoc iframes without computed styles',
  },
  'client/branding-runtime.ts': {
    max: 1,
    reason: 'DEFAULT_THEME_COLOR (#1e293b) for <meta name="theme-color"> HTML tag reset',
  },
  'components/app-shell.ts': {
    max: 1,
    reason: 'Translucent white loading overlay',
  },
  'components/pages/admin-maintenance.ts': {
    max: 1,
    reason: 'Switch toggle thumb knob (white)',
  },
  'components/pages/login.ts': {
    max: 6,
    reason: 'Google and GitHub OAuth provider brand SVG colors',
  },
  'components/pages/metrics-dashboard.ts': {
    max: 10,
    reason: 'CHART_COLORS categorical data-visualization series palette',
  },
  'components/pages/profile-telegram.ts': {
    max: 3,
    reason: 'QR code dark/light quiet zone required for scanner readability',
  },
  'components/shared/chat/chat-avatar.ts': {
    max: 27,
    reason:
      'Deterministic 24-hue light/dark avatar wheel (24 hsl), initials (#fff), and presence dots (#22c55e, #f59e0b)',
  },
  'components/shared/chat/chat-message.ts': {
    max: 2,
    reason:
      'Avatar initials (#fff) on deterministic HSL wheel and lightbox icon (#ffffff) on dark scrim',
  },
  'components/shared/chat/chat-space-rail.ts': {
    max: 1,
    reason: 'Mention badge foreground (#fff) on --scion-danger-500 fill',
  },
  'components/shared/chat/chat-thread.ts': {
    max: 3,
    reason: 'Standalone offline HTML export stylesheet',
  },
  'components/shared/code-editor.ts': {
    max: 21,
    reason: 'CodeMirror One Dark syntax highlighting token palette',
  },
  'components/shared/decisions-format.ts': {
    max: 6,
    reason: 'AVATAR_PALETTE deterministic avatar colors',
  },
  'components/shared/decisions-views.ts': {
    max: 1,
    reason: 'Avatar initials (#ffffff) on deterministic avatar background',
  },
  'components/shared/header.ts': {
    max: 1,
    reason: 'Theme switch toggle thumb knob (white)',
  },
  'components/shared/untrusted-view-frame.ts': {
    max: 1,
    reason: 'Sandboxed artifact iframe canvas background (white)',
  },
  'components/terminal/terminal-pane.ts': {
    max: 35,
    reason: 'xterm.js ANSI color palette and terminal viewport chrome',
  },
  'client/terminal-workspace-root.ts': {
    max: 2,
    reason: 'Intentional dark terminal-pane host background (#111827) and pane border (#333)',
  },
};

describe('web/src bare color literal guard (ptone/scion#8410)', () => {
  const srcDir = resolve(__dirname, '..');
  const walk = (dir: string): string[] => {
    const out: string[] = [];
    for (const name of readdirSync(dir)) {
      if (name === '__fixtures__') continue;
      const p = join(dir, name);
      if (statSync(p).isDirectory()) out.push(...walk(p));
      else if (
        /\.(ts|css)$/.test(name) &&
        !/\.test\.ts$/.test(name) &&
        !/\.d\.ts$/.test(name) &&
        name !== 'theme.css'
      ) {
        out.push(p);
      }
    }
    return out;
  };

  it('rejects new bare color literals outside the explicit allowlist', () => {
    const sourceFiles = walk(srcDir).sort();
    const seen = new Set<string>();
    const violations: string[] = [];

    for (const file of sourceFiles) {
      const rel = relative(srcDir, file).split(sep).join('/');
      seen.add(rel);
      const hits = findBareColorLiterals(readFileSync(file, 'utf8'));
      const entry = BARE_COLOR_ALLOWLIST[rel];
      const maxAllowed = entry?.max ?? 0;
      if (hits.length > maxAllowed) {
        violations.push(
          `${rel}: found ${hits.length} bare color literal(s) (allowed ${maxAllowed}): ${hits.join(', ')}`
        );
      } else if (hits.length < maxAllowed) {
        violations.push(
          `${rel}: allowlist entry has max ${maxAllowed} but only found ${hits.length} bare color literal(s); tighten BARE_COLOR_ALLOWLIST to prevent decay`
        );
      }
    }

    expect(violations).toEqual([]);
    for (const allowedFile of Object.keys(BARE_COLOR_ALLOWLIST)) {
      expect(seen.has(allowedFile), `stale allowlist entry: ${allowedFile}`).toBe(true);
    }
  });

  it('flags planted bare hex, 4-digit hex, functional, and named colors while allowing var() fallbacks and neutral shadows', () => {
    expect(
      findBareColorLiterals('.a { color: #fef08a; background: rgba(59, 130, 246, 0.2); }')
    ).toEqual(['#fef08a', 'rgba(59, 130, 246, 0.2)']);
    expect(findBareColorLiterals('.a { color: #abcd; }')).toEqual(['#abcd']);
    expect(
      findBareColorLiterals('.a { color: white; background: red; border-color: gray; }')
    ).toEqual(['white', 'red', 'gray']);
    expect(findBareColorLiterals("const s = 'var('; .a { color: #ff0000; }")).toEqual(['#ff0000']);
    expect(
      findBareColorLiterals(
        '/* #ff0000 */\n// #00ff00\n.a { color: var(--scion-text, #1e293b); box-shadow: 0 2px 4px rgba(0, 0, 0, 0.1); }'
      )
    ).toEqual([]);
    expect(findBareColorLiterals('<span>&#9654;</span> /* ptone/scion#3067 */')).toEqual([]);
  });
});

/** Convert a CSS `#rrggbb` or Shoelace `hsl(h s% l%)` value to `[r, g, b]`. */
function parseCssColorToRgb(val: string): [number, number, number] {
  const hex = /^#([0-9a-f]{6})$/i.exec(val.trim());
  if (hex) {
    const n = parseInt(hex[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
  }
  const hsl = /^hsl\(\s*([\d.]+)\s+([\d.]+)%\s+([\d.]+)%\s*\)$/i.exec(val.trim());
  if (hsl) {
    const h = parseFloat(hsl[1]) / 360;
    const s = parseFloat(hsl[2]) / 100;
    const l = parseFloat(hsl[3]) / 100;
    const q = l < 0.5 ? l * (1 + s) : l + s - l * s;
    const p = 2 * l - q;
    const ch = (t: number): number => {
      const x = ((t % 1) + 1) % 1;
      if (x < 1 / 6) return p + (q - p) * 6 * x;
      if (x < 1 / 2) return q;
      if (x < 2 / 3) return p + (q - p) * (2 / 3 - x) * 6;
      return p;
    };
    return [
      Math.round(ch(h + 1 / 3) * 255),
      Math.round(ch(h) * 255),
      Math.round(ch(h - 1 / 3) * 255),
    ];
  }
  throw new Error(`Unsupported color format for equivalence check: ${val}`);
}

describe('PR 2d light-mode token equivalence (ptone/scion#8410)', () => {
  const lightVars = cascade([shoelaceLightCss, shoelaceDarkCss, themeCss], {
    isRoot: true,
    classes: new Set(['sl-theme-light']),
    dataTheme: 'light',
    prefersDark: false,
  });

  const resolveLightVal = (name: string): string => {
    let cur = name;
    for (let depth = 0; depth < 8; depth++) {
      const val = lightVars.get(cur);
      if (!val) throw new Error(`Missing light-mode token: ${cur}`);
      const ref = /^var\(\s*(--[\w-]+)\s*\)$/.exec(val.trim());
      if (!ref) return val.trim();
      cur = ref[1];
    }
    throw new Error(`Unresolved token cycle: ${name}`);
  };

  // Every non-exception bare light-mode hex replaced in PR 2d must resolve to the exact
  // original light-mode RGB (within ±1 for Shoelace HSL->sRGB rounding).
  // Scoped exception (disclosed in PR #8543): agent-detail.ts `.port-link` (#4ade80/#22c55e
  // -> --sl-color-success-600/#16a34a and -700/#15803d) for light-mode contrast improvement.
  const exactMappings: Array<[string, string, string]> = [
    ['artifact-preview borderHover', '--scion-border-hover', '#cbd5e1'],
    ['artifact-preview errorBg', '--scion-error-bg', '#fef2f2'],
    ['artifact-preview errorBorder', '--scion-error-border', '#fca5a5'],
    ['artifact-preview errorText', '--scion-error-text', '#991b1b'],
    ['artifact-preview reviewInsText', '--scion-review-ins-text', '#166534'],
    ['artifact-preview reviewInsBg', '--scion-review-ins-bg', '#dcfce7'],
    ['artifact-preview reviewDelText', '--scion-review-del-text', '#991b1b'],
    ['artifact-preview reviewDelBg', '--scion-review-del-bg', '#fee2e2'],
    ['artifact-preview/chat-search reviewHlBg', '--scion-review-hl-bg', '#fef08a'],
    ['artifact-preview/artifact-detail reviewNoteText', '--scion-review-note-text', '#6d28d9'],
    ['artifact-preview/artifact-detail reviewNoteBg', '--scion-review-note-bg', '#ede9fe'],
    ['artifact-preview reviewNoteBlockText', '--sl-color-violet-900', '#4c1d95'],
    ['artifact-preview reviewNoteBlockBg', '--sl-color-violet-50', '#f5f3ff'],
    ['artifact-preview reviewNoteLine', '--scion-review-note-line', '#8b5cf6'],
    ['artifact-detail kind-review border', '--sl-color-violet-200', '#ddd6fe'],
    ['artifact-detail kind-publish bg', '--sl-color-sky-100', '#e0f2fe'],
    ['artifact-detail kind-publish text', '--sl-color-sky-800', '#075985'],
    ['artifact-detail kind-publish border', '--sl-color-sky-200', '#bae6fd'],
    ['project-settings ok/online dot', '--sl-color-success-600', '#16a34a'],
    ['project-settings degraded dot', '--sl-color-yellow-500', '#eab308'],
    ['project-settings error dot', '--sl-color-danger-600', '#dc2626'],
    ['project-settings unchecked/offline dot', '--scion-text-disabled', '#94a3b8'],
    ['project-settings perm write bg', '--scion-badge-primary-bg', '#dbeafe'],
    ['project-settings perm write border', '--sl-color-primary-300', '#93c5fd'],
    ['project-settings perm write text', '--scion-badge-primary-text', '#1e40af'],
    ['project-settings perm read bg', '--sl-color-success-50', '#f0fdf4'],
    ['project-settings perm read border', '--scion-success-border', '#86efac'],
    ['project-settings perm read text', '--scion-success-text', '#166534'],
    ['project-settings warning bg', '--scion-badge-warning-bg', '#fef3c7'],
    ['project-settings warning text', '--scion-warning-text', '#92400e'],
    ['project-settings warning border', '--scion-warning-border', '#fcd34d'],
    ['unified-log-viewer hub color', '--sl-color-primary-600', '#2563eb'],
    ['unified-log-viewer hub bg', '--sl-color-primary-50', '#eff6ff'],
    ['unified-log-viewer broker color', '--sl-color-emerald-600', '#059669'],
    ['unified-log-viewer broker bg', '--sl-color-emerald-50', '#ecfdf5'],
    ['unified-log-viewer agent color', '--sl-color-warning-600', '#d97706'],
    ['unified-log-viewer agent bg', '--sl-color-warning-50', '#fffbeb'],
    ['unified-log-viewer messages color', '--scion-log-messages-text', '#7c3aed'],
    ['unified-log-viewer messages bg', '--scion-log-messages-bg', '#f5f3ff'],
    ['unified-log-viewer artifacts color', '--sl-color-pink-600', '#db2777'],
    ['unified-log-viewer artifacts bg', '--sl-color-pink-50', '#fdf2f8'],
    ['unified-log-viewer server color', '--scion-log-server-text', '#4b5563'],
    ['unified-log-viewer server bg', '--scion-log-server-bg', '#f9fafb'],
    ['admin-server-config active bg', '--scion-badge-success-bg', '#dcfce7'],
    ['admin-server-config active text', '--scion-badge-success-text', '#166534'],
    ['admin-maintenance toggle hover', '--sl-color-warning-600', '#d97706'],
    ['json-browser val-boolean', '--sl-color-violet-600', '#7c3aed'],
    ['not-found gradient stop', '--sl-color-violet-500', '#8b5cf6'],
    ['metrics-dashboard period-btn active text', '--scion-primary-text', '#ffffff'],
    ['metrics-dashboard y grid', '--scion-border-subtle', '#f1f5f9'],
    ['terminal-workspace-root default dot', '--scion-text-disabled', '#94a3b8'],
    ['terminal-workspace-root green dot', '--scion-status-success', '#22c55e'],
    ['terminal-workspace-root red dot', '--scion-status-danger', '#ef4444'],
    ['terminal-workspace-root amber dot', '--scion-status-warning', '#f59e0b'],
  ];

  it('resolves every replaced non-exception token to the exact original light-mode RGB', () => {
    for (const [label, tokenName, expectedHex] of exactMappings) {
      const actualRgb = parseCssColorToRgb(resolveLightVal(tokenName));
      const expectedRgb = parseCssColorToRgb(expectedHex);
      for (let i = 0; i < 3; i++) {
        expect(
          Math.abs(actualRgb[i] - expectedRgb[i]),
          `${label} (${tokenName}): actual ${actualRgb.join(',')} vs expected ${expectedHex} (${expectedRgb.join(',')})`
        ).toBeLessThanOrEqual(1);
      }
    }
  });

  it('keeps all unified-log-viewer SOURCE_CONFIG badge pairs AA-readable in dark mode and faithful in light mode', () => {
    const darkVars = cascade([shoelaceLightCss, shoelaceDarkCss, themeCss], {
      isRoot: true,
      classes: new Set(['sl-theme-dark']),
      dataTheme: 'dark',
      prefersDark: true,
    });
    const resolveFrom = (
      vars: Map<string, string>,
      name: string
    ): [number, number, number, number] => {
      let cur = name;
      for (let depth = 0; depth < 8; depth++) {
        const val = vars.get(cur);
        if (!val) throw new Error(`Missing token: ${cur}`);
        const ref = /^var\(\s*(--[\w-]+)\s*\)$/.exec(val.trim());
        if (!ref) {
          const [r, g, b] = parseCssColorToRgb(val.trim());
          return [r, g, b, 1];
        }
        cur = ref[1];
      }
      throw new Error(`Unresolved token cycle: ${name}`);
    };

    // [source, fgToken, bgToken, lightMinContrast]
    // Light-mode values are locked to the original Tailwind literals (broker/agent/artifacts
    // were 3.05:1–4.33:1 originally); dark mode must meet WCAG AA >= 4.5:1 across all 6.
    const sources: Array<[string, string, string, number]> = [
      ['hub', '--sl-color-primary-600', '--sl-color-primary-50', 4.5],
      ['broker', '--sl-color-emerald-600', '--sl-color-emerald-50', 3.0],
      ['agent', '--sl-color-warning-600', '--sl-color-warning-50', 3.0],
      ['messages', '--scion-log-messages-text', '--scion-log-messages-bg', 4.5],
      ['artifacts', '--sl-color-pink-600', '--sl-color-pink-50', 4.0],
      ['server', '--scion-log-server-text', '--scion-log-server-bg', 4.5],
    ];

    for (const [source, fgToken, bgToken, lightMin] of sources) {
      expect(
        contrast(resolveFrom(lightVars, fgToken), resolveFrom(lightVars, bgToken)),
        `unified-log-viewer ${source} light contrast`
      ).toBeGreaterThanOrEqual(lightMin);
      expect(
        contrast(resolveFrom(darkVars, fgToken), resolveFrom(darkVars, bgToken)),
        `unified-log-viewer ${source} dark contrast`
      ).toBeGreaterThanOrEqual(4.5);
    }
  });
});
