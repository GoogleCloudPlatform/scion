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

/** Helpers for the layout tests: read a component's style rules. */

/**
 * Leaf style rules from Lit cssText.
 *
 * A top-level rule is keyed by its selector alone, for example `.card`. A
 * rule nested in at-rules (`@media`, `@supports`, `@container`, ...) is keyed
 * by its at-rule preludes followed by the selector, separated by single
 * spaces, for example `@media (max-width: 600px) .card`. So a top-level
 * rule and an `@media` rule with the same selector never overwrite each
 * other. Whitespace in keys is collapsed to single spaces.
 *
 * A selector list such as `.a, .b` yields one entry per selector. Only
 * top-level commas separate selectors, so `:is(.a, .b) .c` or
 * `[data-x="a,b"]` stays a single selector. If a key
 * appears more than once (for example `.a, .b { ... }` followed by
 * `.a { ... }`, which is ordinary cascade CSS), the bodies are joined with
 * `;` in source order rather than the later one replacing the earlier. The
 * separator keeps declarations apart when the earlier body has no trailing
 * semicolon. Every declaration stays visible, and later declarations come
 * last, as in the cascade.
 */
export function styleRules(cssText: string): Map<string, string> {
  const rules = new Map<string, string>();
  const stack: string[] = [];
  let buf = '';
  for (const ch of cssText.replace(/\/\*[\s\S]*?\*\//g, '')) {
    if (ch === '{') {
      stack.push(normalize(buf));
      buf = '';
    } else if (ch === '}') {
      const selector = stack.pop() ?? '';
      if (!selector.startsWith('@')) {
        const context = stack.filter((s) => s.startsWith('@'));
        for (const part of splitSelectorList(selector)) {
          const key = [...context, part.trim()].join(' ');
          const prev = rules.get(key);
          rules.set(key, prev === undefined ? buf : prev + ';' + buf);
        }
      }
      buf = '';
    } else {
      buf += ch;
    }
  }
  return rules;
}

/**
 * Split a selector list on top-level commas only. Commas inside parentheses
 * (`:is(...)`, `:not(...)`, `:where(...)`, nested to any depth) or attribute
 * brackets belong to a single selector.
 */
export function splitSelectorList(selector: string): string[] {
  const parts: string[] = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < selector.length; i++) {
    const ch = selector[i];
    if (ch === '(' || ch === '[') {
      depth++;
    } else if ((ch === ')' || ch === ']') && depth > 0) {
      depth--;
    } else if (ch === ',' && depth === 0) {
      parts.push(selector.slice(start, i));
      start = i + 1;
    }
  }
  parts.push(selector.slice(start));
  return parts;
}

function normalize(text: string): string {
  return text.trim().replace(/\s+/g, ' ');
}

type CssLike = { cssText?: string } | undefined;
type StylesLike = CssLike | readonly StylesLike[];

/**
 * Style rules of a registered custom element. Lit's static `styles` can be
 * a single CSSResult or an arbitrarily nested array, so flatten it fully.
 */
export function elementStyleRules(tagName: string): Map<string, string> {
  const ctor = customElements.get(tagName) as unknown as { styles?: StylesLike };
  const raw = ctor.styles;
  const list = (Array.isArray(raw) ? raw.flat(Infinity) : [raw]) as CssLike[];
  return styleRules(list.map((s) => s?.cssText ?? '').join('\n'));
}
