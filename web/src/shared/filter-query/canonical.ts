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

import { comparisonPrefixLen, scanKeyPrefix, trimASCIISpace } from './parse.js';
import type { Query, ViewSchema } from './types.js';

function quoteString(s: string): string {
  return `"${s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`;
}

function needsQuotingValue(v: string): boolean {
  if (v === '' || comparisonPrefixLen(v) > 0) return true;
  for (let i = 0; i < v.length; i++) {
    const c = v[i];
    if (
      c === ' ' ||
      c === '\t' ||
      c === '\n' ||
      c === '\r' ||
      c === ',' ||
      c === '"' ||
      c === '\\' ||
      c === '(' ||
      c === ')'
    ) {
      return true;
    }
  }
  return false;
}

function needsQuotingText(t: string): boolean {
  if (t === '' || t === 'OR' || t === 'AND' || t.startsWith('-')) return true;
  if (scanKeyPrefix(t, 0)) return true;
  for (let i = 0; i < t.length; i++) {
    const c = t[i];
    if (
      c === ' ' ||
      c === '\t' ||
      c === '\n' ||
      c === '\r' ||
      c === '"' ||
      c === '\\' ||
      c === '(' ||
      c === ')'
    ) {
      return true;
    }
  }
  return false;
}

export function formatValue(v: string): string {
  return needsQuotingValue(v) ? quoteString(v) : v;
}

export function formatText(t: string): string {
  return needsQuotingText(t) ? quoteString(t) : t;
}

function compareStrings(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * Deterministic string serialization of a parsed Query.
 * Matches Go's filterquery.Canonical().
 */
export function canonical(q: Query): string {
  if (q.mode === 'text') {
    return trimASCIISpace(q.text);
  }

  const normTerms = q.terms.map((t) => {
    const key = t.key.toLowerCase();
    const vals = t.values.map((v) => (key === 'label' ? v : v.toLowerCase()));
    vals.sort(compareStrings);
    const unique = vals.filter((v, idx) => idx === 0 || v !== vals[idx - 1]);
    const joined = unique.map(formatValue).join(',');
    return { key, negate: t.negate, joined };
  });

  normTerms.sort((a, b) => {
    const kc = compareStrings(a.key, b.key);
    if (kc !== 0) return kc;
    if (a.negate !== b.negate) return a.negate ? 1 : -1;
    return compareStrings(a.joined, b.joined);
  });

  const parts: string[] = normTerms.map((t) => `${t.negate ? '-' : ''}${t.key}:${t.joined}`);

  if (q.text) {
    parts.push(formatText(q.text));
  }

  if (q.excludeText?.length) {
    const excl = [...q.excludeText].sort(compareStrings);
    const unique = excl.filter((v, idx) => idx === 0 || v !== excl[idx - 1]);
    for (const ex of unique) {
      parts.push(`-${formatText(ex)}`);
    }
  }

  return parts.join(' ');
}

/**
 * Strip client-only keys from q and return the canonical server query string
 * suitable for `?q=`.
 */
export function serverSubset(q: Query, schema: ViewSchema): string {
  if (q.mode === 'text') {
    return trimASCIISpace(q.text);
  }
  const serverKeys = new Set(
    schema.keys.filter((k) => k.exec === 'server').map((k) => k.name.toLowerCase())
  );
  const filteredTerms = q.terms.filter((t) => serverKeys.has(t.key.toLowerCase()));
  const next: Query = {
    mode: 'structured',
    terms: filteredTerms,
  };
  if (q.text !== undefined) next.text = q.text;
  if (q.excludeText !== undefined) next.excludeText = q.excludeText;
  return canonical(next);
}

/**
 * Returns true if q contains any key marked `exec: "client"` in schema.
 */
export function hasClientOnlyKeys(q: Query, schema: ViewSchema): boolean {
  if (q.mode === 'text') return false;
  const clientKeys = new Set(
    schema.keys.filter((k) => k.exec === 'client').map((k) => k.name.toLowerCase())
  );
  return q.terms.some((t) => clientKeys.has(t.key.toLowerCase()));
}
