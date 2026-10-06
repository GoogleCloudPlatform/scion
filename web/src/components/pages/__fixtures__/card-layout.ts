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

/** Helpers for the card layout tests: read a component's style rules. */

/** Leaf style rules from Lit cssText, keyed by selector. */
export function styleRules(cssText: string): Map<string, string> {
  const rules = new Map<string, string>();
  const stack: string[] = [];
  let buf = '';
  for (const ch of cssText.replace(/\/\*[\s\S]*?\*\//g, '')) {
    if (ch === '{') {
      stack.push(buf.trim());
      buf = '';
    } else if (ch === '}') {
      const selector = stack.pop() ?? '';
      if (!selector.startsWith('@')) {
        for (const part of selector.split(',')) rules.set(part.trim(), buf);
      }
      buf = '';
    } else {
      buf += ch;
    }
  }
  return rules;
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
