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
 * WCAG contrast helpers for the health dashboard tests: resolve colour
 * tokens from theme.css in the light and dark theme blocks, composite
 * translucent badge backgrounds and compute contrast ratios.
 */

import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

export type RGBA = [number, number, number, number];

export function block(cssText: string, selectorStart: string): string {
  const at = cssText.indexOf(selectorStart);
  if (at < 0) throw new Error(`no block ${selectorStart}`);
  const open = cssText.indexOf('{', at);
  let depth = 0;
  for (let i = open; i < cssText.length; i++) {
    if (cssText[i] === '{') depth++;
    if (cssText[i] === '}' && --depth === 0) return cssText.slice(open + 1, i);
  }
  throw new Error('unbalanced');
}

export function decls(body: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of body.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
    out.set(m[1], m[2].trim());
  }
  return out;
}

export function parseColor(v: string): RGBA {
  const hex = /^#([0-9a-f]{6})$/i.exec(v);
  if (hex) {
    const n = parseInt(hex[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255, 1];
  }
  const rgba = /^rgba?\(\s*([\d.]+)\s*,\s*([\d.]+)\s*,\s*([\d.]+)\s*(?:,\s*([\d.]+)\s*)?\)$/.exec(
    v
  );
  if (rgba) return [+rgba[1], +rgba[2], +rgba[3], rgba[4] === undefined ? 1 : +rgba[4]];
  throw new Error(`unparsed colour ${v}`);
}

export function resolver(vars: Map<string, string>) {
  const get = (name: string, depth = 0): RGBA => {
    const v = vars.get(name);
    if (v === undefined || depth > 10) throw new Error(`unresolved ${name}`);
    const ref = /^var\((--[\w-]+)\)$/.exec(v);
    return ref ? get(ref[1], depth + 1) : parseColor(v);
  };
  return get;
}

export function over(fg: RGBA, bg: RGBA): RGBA {
  const a = fg[3];
  return [0, 1, 2].map((i) => fg[i] * a + bg[i] * (1 - a)).concat(1) as RGBA;
}

export function luminance([r, g, b]: RGBA): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

export function contrast(a: RGBA, b: RGBA): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/** The light and dark token maps of theme.css (dark overrides light). */
export function themeTokens(): { light: Map<string, string>; dark: Map<string, string> } {
  const themeCss = readFileSync(resolve(__dirname, '../../../styles/theme.css'), 'utf8');
  const light = decls(block(themeCss, '.sl-theme-light {'));
  const dark = new Map([...light, ...decls(block(themeCss, '.sl-theme-dark,'))]);
  return { light, dark };
}
