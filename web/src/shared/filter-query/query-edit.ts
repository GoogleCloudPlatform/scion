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

import { formatText } from './canonical.js';
import { parse, trimASCIISpace } from './parse.js';
import type { ViewSchema } from './types.js';

/**
 * Return the active chip state for `key`:
 * - `''` when the query is valid and has no terms for `key` (so an "All" chip is active),
 * - the lowercased single value when there is exactly one positive term for `key` with one value,
 * - `null` when `key` has a negated term, multiple values, multiple terms, or the query has a syntax error
 *   (so no chip, not even "All", is highlighted).
 */
export function getActiveSingleKeyValue(
  input: string,
  key: string,
  schema: ViewSchema
): string | null {
  const known = (k: string) => schema.keys.some((sk) => sk.name.toLowerCase() === k);
  const res = parse(input, known);
  if (res.error || !res.query) return null;
  if (res.query.mode !== 'structured') return '';
  const target = key.toLowerCase();
  const matching = res.query.terms.filter((t) => t.key.toLowerCase() === target);
  if (matching.length === 0) return '';
  if (matching.length !== 1) return null;
  const only = matching[0];
  if (only.negate || only.values.length !== 1) return null;
  return only.values[0].toLowerCase();
}

/**
 * Edit `input` to set or clear a single-valued key (e.g. `phase` or `is`).
 * - When `value` is empty, removes all `key:` / `-key:` terms from `input`.
 * - When `value` is non-empty, replaces any existing `key:` / `-key:` terms
 *   with `key:value` (or prepends it if none existed), preserving all other
 *   terms and free text. When promoting pure-text input to structured mode,
 *   the text remainder is quoted if needed so its literal meaning is preserved.
 */
export function setSingleKeyInQuery(
  input: string,
  key: string,
  value: string,
  schema: ViewSchema
): string {
  const known = (k: string) => schema.keys.some((sk) => sk.name.toLowerCase() === k);
  const target = key.toLowerCase();
  const trimmed = trimASCIISpace(input);

  if (!trimmed) {
    return value ? `${target}:${value}` : '';
  }

  const res = parse(input, known);
  if (res.error || !res.query) {
    // Currently invalid input: prepend the key term if setting.
    if (!value) return trimmed;
    return `${target}:${value} ${trimmed}`;
  }
  if (res.query.mode !== 'structured') {
    // Promoting pure text to structured mode: quote the remainder when needed
    // so leading '-', 'OR'/'AND', quotes, spaces, or parens keep their literal meaning.
    if (!value) return trimmed;
    return `${target}:${value} ${formatText(res.query.text)}`;
  }

  const keyTerms = res.query.terms.filter((t) => t.key.toLowerCase() === target);
  if (keyTerms.length === 0) {
    return value ? `${target}:${value} ${trimmed}` : trimmed;
  }

  // Splice out existing terms for `target` from right to left without touching
  // whitespace inside other terms or quoted values.
  let out = input;
  const sortedDesc = [...keyTerms].sort((a, b) => b.offset - a.offset);
  for (let idx = 0; idx < sortedDesc.length; idx++) {
    const t = sortedDesc[idx];
    const isLastRemaining = idx === sortedDesc.length - 1;
    if (value && isLastRemaining) {
      out = out.slice(0, t.offset) + `${target}:${value}` + out.slice(t.offset + t.length);
    } else {
      const before = out.slice(0, t.offset);
      const after = out.slice(t.offset + t.length);
      const beforeTrimmed = before.replace(/[ \t\n\r]+$/, '');
      const afterTrimmed = after.replace(/^[ \t\n\r]+/, '');
      if (beforeTrimmed && afterTrimmed) {
        out = `${beforeTrimmed} ${afterTrimmed}`;
      } else {
        out = beforeTrimmed + afterTrimmed;
      }
    }
  }
  return trimASCIISpace(out);
}
