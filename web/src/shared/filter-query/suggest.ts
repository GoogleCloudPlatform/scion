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
import { trimASCIISpace } from './parse.js';
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

export type ValueSuggestion = string | { value: string; help?: string; pinned?: boolean };

export type ValueSuggestionProvider = (key: string, prefix: string) => ValueSuggestion[];

export interface RemoteValueSuggestionsResult {
  values: ValueSuggestion[];
  total?: number;
}

export type RemoteValueSuggestionProvider = (
  key: string,
  prefix: string,
  signal: AbortSignal
) => Promise<ValueSuggestion[] | RemoteValueSuggestionsResult> | null;

export interface ValueSuggestionContext {
  key: string;
  prefix: string;
  replaceStart: number;
  replaceEnd: number;
}

interface NormalizedValueSuggestion {
  value: string;
  help?: string;
  pinned?: boolean;
}

function normalizeValueSuggestion(raw: ValueSuggestion): NormalizedValueSuggestion {
  if (typeof raw === 'string') {
    return { value: raw };
  }
  return {
    value: raw.value,
    ...(raw.help ? { help: raw.help } : {}),
    ...(raw.pinned ? { pinned: true } : {}),
  };
}

/**
 * If `cursor` is positioned inside the value part of a known `key:value` token,
 * returns the key name, current value prefix (after the last comma), and the
 * replacement range; otherwise returns `null`.
 */
export function getValueSuggestionContext(
  input: string,
  cursor: number,
  schema: ViewSchema
): ValueSuggestionContext | null {
  const before = input.slice(0, cursor);
  let tokenStart = before.length;
  while (tokenStart > 0 && !/\s/.test(before[tokenStart - 1])) {
    tokenStart--;
  }
  const token = before.slice(tokenStart);
  const negatePrefix = token.startsWith('-') ? '-' : '';
  const body = negatePrefix ? token.slice(1) : token;
  const colonIdx = body.indexOf(':');
  if (colonIdx <= 0) return null;

  const keyName = body.slice(0, colonIdx).toLowerCase();
  const keyDef = schema.keys.find((k) => k.name.toLowerCase() === keyName);
  if (!keyDef) return null;

  const afterColon = body.slice(colonIdx + 1);
  const lastComma = afterColon.lastIndexOf(',');
  const prefix = lastComma >= 0 ? afterColon.slice(lastComma + 1) : afterColon;
  const replaceStart =
    tokenStart + negatePrefix.length + colonIdx + 1 + (lastComma >= 0 ? lastComma + 1 : 0);

  return {
    key: keyDef.name,
    prefix,
    replaceStart,
    replaceEnd: cursor,
  };
}

/**
 * Converts pre-ranked value suggestions (e.g. from a remote endpoint) into
 * filter-box `Suggestion` items without re-ranking them.
 */
export function buildSuggestionsFromValues(
  values: ValueSuggestion[],
  replaceStart: number,
  replaceEnd: number,
  limit = 12
): Suggestion[] {
  const out: Suggestion[] = [];
  const seen = new Set<string>();
  for (const raw of values) {
    if (out.length >= limit) break;
    const item = normalizeValueSuggestion(raw);
    const dedupKey = `${item.value}\0${item.help ?? ''}`;
    if (seen.has(dedupKey)) continue;
    seen.add(dedupKey);
    out.push({
      insertText: formatValue(item.value),
      label: item.value,
      ...(item.help ? { help: item.help } : {}),
      replaceStart,
      replaceEnd,
    });
  }
  return out;
}

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

    const candidates: NormalizedValueSuggestion[] = [];
    const seen = new Set<string>();
    const addCandidate = (raw: ValueSuggestion): void => {
      const item = normalizeValueSuggestion(raw);
      const dedupKey = `${item.value}\0${item.help ?? ''}`;
      if (!seen.has(dedupKey)) {
        seen.add(dedupKey);
        candidates.push(item);
      }
    };
    if (keyDef.enum?.length) {
      for (const e of keyDef.enum) {
        addCandidate(e);
      }
    }
    if (valueSuggestions) {
      for (const dyn of valueSuggestions(keyDef.name, valPrefix)) {
        addCandidate(dyn);
      }
    }

    const q = trimASCIISpace(valPrefix).toLowerCase();
    const scored = candidates
      .map((item) => {
        if (!q) return { item, tier: 0 };
        const valTier = classifyMatch(q, item.value);
        if (valTier !== null) return { item, tier: valTier };
        if (item.help) {
          const helpTier = classifyMatch(q, item.help);
          if (helpTier !== null) return { item, tier: helpTier + 4 };
        }
        return null;
      })
      .filter((x): x is { item: NormalizedValueSuggestion; tier: number } => x !== null)
      .sort(
        (a, b) =>
          a.tier - b.tier ||
          (a.item.pinned ? 0 : 1) - (b.item.pinned ? 0 : 1) ||
          a.item.value.localeCompare(b.item.value) ||
          (a.item.help ?? '').localeCompare(b.item.help ?? '')
      );

    return scored.slice(0, 12).map(({ item }) => ({
      insertText: formatValue(item.value),
      label: item.value,
      ...(item.help ? { help: item.help } : {}),
      replaceStart,
      replaceEnd: cursor,
    }));
  }

  // At term boundary or typing a key name -> key suggestions.
  const q = trimASCIISpace(body).toLowerCase();
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
