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

import { classifyMatch } from '../../utils/palette-match.js';
import { formatValue } from './canonical.js';
import type { ViewSchema } from './types.js';

export interface Suggestion {
  /** Text inserted when accepted (e.g. "phase:" or "running"). */
  insertText: string;
  /** Display label in the listbox. */
  label: string;
  /** Optional one-line description. */
  help?: string;
  /** Start index in the input string to replace. */
  replaceStart: number;
  /** End index in the input string to replace. */
  replaceEnd: number;
  /** True when accepting this suggestion should keep the listbox open for value suggestions. */
  continueSuggest?: boolean;
}

export type ValueSuggestionProvider = (key: string, prefix: string) => string[];

/**
 * Compute key or value autocomplete suggestions at `cursor` within `input`.
 */
export function getSuggestions(
  input: string,
  cursor: number,
  schema: ViewSchema,
  valueSuggestions?: ValueSuggestionProvider
): Suggestion[] {
  const before = input.slice(0, cursor);

  // Find the active token starting after the last whitespace.
  let tokenStart = before.length;
  while (tokenStart > 0 && !/\s/.test(before[tokenStart - 1])) {
    tokenStart--;
  }
  const token = before.slice(tokenStart);
  const negatePrefix = token.startsWith('-') ? '-' : '';
  const body = negatePrefix ? token.slice(1) : token;
  const colonIdx = body.indexOf(':');

  // After `key:` -> value suggestions.
  if (colonIdx > 0) {
    const keyName = body.slice(0, colonIdx).toLowerCase();
    const keyDef = schema.keys.find((k) => k.name.toLowerCase() === keyName);
    if (!keyDef) return [];

    const afterColon = body.slice(colonIdx + 1);
    const lastComma = afterColon.lastIndexOf(',');
    const valPrefix = lastComma >= 0 ? afterColon.slice(lastComma + 1) : afterColon;
    const replaceStart =
      tokenStart + negatePrefix.length + colonIdx + 1 + (lastComma >= 0 ? lastComma + 1 : 0);

    const candidates: string[] = [];
    if (keyDef.enum?.length) {
      candidates.push(...keyDef.enum);
    }
    if (valueSuggestions) {
      for (const dyn of valueSuggestions(keyDef.name, valPrefix)) {
        if (!candidates.includes(dyn)) {
          candidates.push(dyn);
        }
      }
    }

    const q = valPrefix.trim().toLowerCase();
    const scored = candidates
      .map((val) => {
        if (!q) return { val, tier: 0 };
        const tier = classifyMatch(q, val);
        return tier !== null ? { val, tier } : null;
      })
      .filter((x): x is { val: string; tier: number } => x !== null)
      .sort((a, b) => a.tier - b.tier || a.val.localeCompare(b.val));

    return scored.slice(0, 12).map(({ val }) => ({
      insertText: formatValue(val),
      label: val,
      replaceStart,
      replaceEnd: cursor,
    }));
  }

  // At term boundary or typing a key name -> key suggestions.
  const q = body.trim().toLowerCase();
  const scoredKeys = schema.keys
    .map((k) => {
      if (!q) return { key: k, tier: 0 };
      const tier = classifyMatch(q, k.name);
      return tier !== null ? { key: k, tier } : null;
    })
    .filter((x): x is { key: (typeof schema.keys)[number]; tier: number } => x !== null)
    .sort((a, b) => a.tier - b.tier || a.key.name.localeCompare(b.key.name));

  return scoredKeys.map(({ key }) => ({
    insertText: `${negatePrefix}${key.name}:`,
    label: `${negatePrefix}${key.name}:`,
    help: key.help,
    replaceStart: tokenStart,
    replaceEnd: cursor,
    continueSuggest: true,
  }));
}
