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

import { comparisonPrefixLen, parse, scanKeyPrefix, trimASCIISpace } from './parse.js';
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
 * Render `q` as a query string that preserves its match semantics when parsed
 * against `known`.
 *
 * - Pure-text queries render as trimmed text.
 * - Structured queries with at least one key term render via `canonical(q)`.
 * - Structured queries with zero remaining key terms (e.g. after stripping
 *   client-only keys or clearing the last chip): if `excludeText` is non-empty,
 *   no pure-text string can represent exclusions without a known key, so `''`
 *   is returned; otherwise the unquoted `text` is returned only if it
 *   re-parses as identical pure text under `known` (else `''`).
 */
export function renderQueryPreservingMeaning(q: Query, known: (key: string) => boolean): string {
  if (q.mode === 'text') {
    return trimASCIISpace(q.text);
  }
  if (q.terms.length > 0) {
    return canonical(q);
  }
  if (q.excludeText && q.excludeText.length > 0) {
    return '';
  }
  if (!q.text) {
    return '';
  }
  const candidate = trimASCIISpace(q.text);
  if (!candidate) {
    return '';
  }
  const reparsed = parse(candidate, known);
  if (!reparsed.error && reparsed.query?.mode === 'text' && reparsed.query.text === q.text) {
    return candidate;
  }
  return '';
}

/**
 * Strip client-only keys from q and return a hub-safe `?q=` query string whose
 * parsed semantics on the server never contradict `q`. When stripping leaves
 * no server key terms and the remaining text/exclusions cannot be represented
 * faithfully in pure-text mode, returns `''` (the client drains and evaluates
 * the full query locally).
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
  const knownForHub = (k: string): boolean => schema.keys.some((sk) => sk.name.toLowerCase() === k);
  return renderQueryPreservingMeaning(next, knownForHub);
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
