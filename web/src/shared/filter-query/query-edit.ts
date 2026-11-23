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

import { parse } from './parse.js';
import type { ViewSchema } from './types.js';

/**
 * Return the single positive value for `key` if the query has exactly one
 * non-negated term for `key` with one value and no negated terms for `key`,
 * or `''` otherwise. Used by quick-toggle chips (e.g. agent phase buttons)
 * to highlight their active state from the query text.
 */
export function getActiveSingleKeyValue(input: string, key: string, schema: ViewSchema): string {
  const known = (k: string) => schema.keys.some((sk) => sk.name.toLowerCase() === k);
  const res = parse(input, known);
  if (!res.query || res.query.mode !== 'structured') return '';
  const target = key.toLowerCase();
  const matching = res.query.terms.filter((t) => t.key.toLowerCase() === target);
  if (matching.length !== 1) return '';
  const only = matching[0];
  if (only.negate || only.values.length !== 1) return '';
  return only.values[0].toLowerCase();
}

/**
 * Edit `input` to set or clear a single-valued key (e.g. `phase`).
 * - When `value` is empty, removes all `key:` / `-key:` terms from `input`.
 * - When `value` is non-empty, replaces any existing `key:` / `-key:` terms
 *   with `key:value` (or prepends it if none existed), preserving all other
 *   terms and free text verbatim.
 */
export function setSingleKeyInQuery(
  input: string,
  key: string,
  value: string,
  schema: ViewSchema
): string {
  const known = (k: string) => schema.keys.some((sk) => sk.name.toLowerCase() === k);
  const target = key.toLowerCase();
  const trimmed = input.trim();

  if (!trimmed) {
    return value ? `${target}:${value}` : '';
  }

  const res = parse(input, known);
  if (!res.query || res.query.mode !== 'structured') {
    // Pure-text or currently invalid input: prepend the key term if setting.
    if (!value) return trimmed;
    return `${target}:${value} ${trimmed}`;
  }

  const keyTerms = res.query.terms.filter((t) => t.key.toLowerCase() === target);
  if (keyTerms.length === 0) {
    return value ? `${target}:${value} ${trimmed}` : trimmed;
  }

  // Splice out existing terms for `target` from right to left.
  let out = input;
  const sortedDesc = [...keyTerms].sort((a, b) => b.offset - a.offset);
  for (let idx = 0; idx < sortedDesc.length; idx++) {
    const t = sortedDesc[idx];
    const replacement = value && idx === sortedDesc.length - 1 ? `${target}:${value}` : '';
    out = out.slice(0, t.offset) + replacement + out.slice(t.offset + t.length);
  }
  return out.replace(/\s+/g, ' ').trim();
}
