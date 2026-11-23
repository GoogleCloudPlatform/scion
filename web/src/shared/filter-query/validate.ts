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

import { scanKeyPrefix } from './parse.js';
import {
  MAX_TERMS,
  MAX_TEXT_RUNES,
  MAX_VALUES_PER_KEY,
  type FilterWarning,
  type KeySchema,
  type Query,
  type Term,
  type ValidationResult,
  type ViewSchema,
} from './types.js';

function lookupKey(schema: ViewSchema, name: string): KeySchema | undefined {
  const lower = name.toLowerCase();
  return schema.keys.find((k) => k.name.toLowerCase() === lower);
}

function levenshtein(a: string, b: string): number {
  const la = a.length;
  const lb = b.length;
  if (la === 0) return lb;
  if (lb === 0) return la;
  let prev = Array.from({ length: lb + 1 }, (_, j) => j);
  let curr = new Array<number>(lb + 1);
  for (let i = 1; i <= la; i++) {
    curr[0] = i;
    for (let j = 1; j <= lb; j++) {
      const cost = a[i - 1] === b[j - 1] ? 0 : 1;
      curr[j] = Math.min(prev[j] + 1, curr[j - 1] + 1, prev[j - 1] + cost);
    }
    const tmp = prev;
    prev = curr;
    curr = tmp;
  }
  return prev[lb];
}

function closestMatch(input: string, candidates: string[]): string | undefined {
  let best: string | undefined;
  let bestDist = 3;
  for (const c of candidates) {
    const d = levenshtein(input, c.toLowerCase());
    if (d < bestDist) {
      bestDist = d;
      best = c;
    }
  }
  return best;
}

/**
 * Scan raw input for unquoted `unknownKey:val` tokens to produce non-blocking
 * warnings ("Unknown key `foo`; searching as text. Keys: ...").
 */
function findUnknownKeyWarnings(rawInput: string, schema: ViewSchema): FilterWarning[] {
  const warnings: FilterWarning[] = [];
  const keyNames = schema.keys.map((k) => k.name).join(', ');
  const n = rawInput.length;
  let i = 0;

  while (i < n) {
    while (i < n && /\s/.test(rawInput[i])) i++;
    if (i >= n) break;
    if (rawInput[i] === '"') {
      i++;
      while (i < n) {
        if (rawInput[i] === '\\' && i + 1 < n) {
          i += 2;
          continue;
        }
        if (rawInput[i] === '"') {
          i++;
          break;
        }
        i++;
      }
      continue;
    }
    const tokenStart = i;
    let scanStart = i;
    if (rawInput[scanStart] === '-') scanStart++;
    while (i < n && !/\s/.test(rawInput[i])) i++;
    const prefix = scanKeyPrefix(rawInput, scanStart);
    if (prefix && !lookupKey(schema, prefix.key)) {
      // Skip single-letter drive prefixes like C:\temp.
      if (prefix.key.length === 1 && rawInput[prefix.colonPos + 1] === '\\') {
        continue;
      }
      warnings.push({
        code: 'unknown_key',
        message: `Unknown key '${prefix.key}'; searching as text. Keys: ${keyNames}`,
        offset: tokenStart,
        length: i - tokenStart,
      });
    }
  }
  return warnings;
}

/**
 * Validate a parsed query against a view schema for client-side use.
 * Returns any blocking error (invalid enum, mutually exclusive flags, limits)
 * and non-blocking warnings (unknown key, repeated single-valued key).
 */
export function validate(query: Query, schema: ViewSchema, rawInput = ''): ValidationResult {
  const warnings: FilterWarning[] = rawInput ? findUnknownKeyWarnings(rawInput, schema) : [];

  if (query.mode === 'text') {
    if ([...query.text].length > MAX_TEXT_RUNES) {
      return {
        error: {
          code: 'limit',
          message: `search text exceeds maximum length of ${MAX_TEXT_RUNES} characters`,
          offset: 0,
          length: rawInput.length || query.text.length,
        },
        warnings,
      };
    }
    return { warnings };
  }

  const excludeCount = query.excludeText?.length ?? 0;
  if (query.terms.length + excludeCount > MAX_TERMS) {
    return {
      error: {
        code: 'limit',
        message: `filter query exceeds maximum of ${MAX_TERMS} terms`,
        offset: 0,
        length: rawInput.length,
      },
      warnings,
    };
  }

  const posFlags = new Map<string, Map<string, Term>>();
  const seenPositiveSingle = new Map<string, Term>();

  for (const term of query.terms) {
    const keyDef = lookupKey(schema, term.key);
    if (!keyDef) {
      return {
        error: {
          code: 'unsupported_key',
          message: `key "${term.key}" is not supported`,
          offset: term.offset,
          length: term.length,
        },
        warnings,
      };
    }
    if (term.values.length > MAX_VALUES_PER_KEY) {
      return {
        error: {
          code: 'limit',
          message: `key "${keyDef.name}" exceeds maximum of ${MAX_VALUES_PER_KEY} values`,
          offset: term.offset,
          length: term.length,
        },
        warnings,
      };
    }

    if (!term.negate && !keyDef.multiValued) {
      const prev = seenPositiveSingle.get(keyDef.name);
      if (prev) {
        const example = `${keyDef.name}:${prev.values[0] ?? 'a'},${term.values[0] ?? 'b'}`;
        warnings.push({
          code: 'repeated_key',
          message: `Repeated key '${keyDef.name}'; use ${example} for either`,
          offset: term.offset,
          length: term.length,
        });
      } else {
        seenPositiveSingle.set(keyDef.name, term);
      }
    }

    if (keyDef.kind === 'enum' || keyDef.kind === 'flag_set') {
      const allowed = keyDef.enum ?? [];
      for (const v of term.values) {
        const lower = v.toLowerCase();
        if (!allowed.some((a) => a.toLowerCase() === lower)) {
          const hint = closestMatch(lower, allowed);
          const msg = hint
            ? `invalid value "${v}" for ${keyDef.name} (did you mean "${hint}"?)`
            : `invalid value "${v}" for ${keyDef.name} (expected one of: ${allowed.join(', ')})`;
          return {
            error: {
              code: 'value',
              message: msg,
              offset: term.offset,
              length: term.length,
            },
            warnings,
          };
        }
        if (keyDef.kind === 'flag_set' && !term.negate) {
          let map = posFlags.get(keyDef.name);
          if (!map) {
            map = new Map();
            posFlags.set(keyDef.name, map);
          }
          map.set(lower, term);
        }
      }
    } else if (keyDef.kind === 'label') {
      for (const v of term.values) {
        if (v === '' || v.startsWith('=')) {
          return {
            error: {
              code: 'value',
              message: `invalid label filter "${v}": label key must not be empty`,
              offset: term.offset,
              length: term.length,
            },
            warnings,
          };
        }
      }
    }
  }

  for (const keyDef of schema.keys) {
    if (!keyDef.exclusive?.length) continue;
    const seen = posFlags.get(keyDef.name);
    if (!seen || seen.size < 2) continue;
    for (const group of keyDef.exclusive) {
      const matched: string[] = [];
      let lastTerm: Term | undefined;
      for (const flag of group) {
        const t = seen.get(flag.toLowerCase());
        if (t) {
          matched.push(flag);
          lastTerm = t;
        }
      }
      if (matched.length >= 2 && lastTerm) {
        return {
          error: {
            code: 'value',
            message: `${keyDef.name}:${matched[0]} and ${keyDef.name}:${matched[1]} are mutually exclusive`,
            offset: lastTerm.offset,
            length: lastTerm.length,
          },
          warnings,
        };
      }
    }
  }

  return { warnings };
}
