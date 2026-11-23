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

import type { FilterRow, Query, Term } from './types.js';

function matchesAnyText(haystacks: string[], needle: string): boolean {
  const lower = needle.toLowerCase();
  for (const h of haystacks) {
    if (h.toLowerCase().includes(lower)) {
      return true;
    }
  }
  return false;
}

function termMatchesRow(term: Term, row: FilterRow): boolean {
  const key = term.key.toLowerCase();
  const rowVals = row.values(key);
  if (rowVals.length === 0) return false;
  const exactCase = key === 'label';
  for (const want of term.values) {
    if (exactCase) {
      if (rowVals.includes(want)) return true;
    } else {
      const wantLower = want.toLowerCase();
      if (rowVals.some((rv) => rv.toLowerCase() === wantLower)) return true;
    }
  }
  return false;
}

/**
 * Evaluate a parsed Query against a single row in memory.
 * Semantics match Go's filterquery.Match().
 */
export function match(q: Query, row: FilterRow): boolean {
  if (q.mode === 'text') {
    if (q.text === '') return true;
    return matchesAnyText(row.text(), q.text);
  }

  for (const term of q.terms) {
    const matched = termMatchesRow(term, row);
    if (term.negate) {
      if (matched) return false;
    } else if (!matched) {
      return false;
    }
  }

  if (q.text && !matchesAnyText(row.text(), q.text)) {
    return false;
  }

  if (q.excludeText) {
    for (const ex of q.excludeText) {
      if (matchesAnyText(row.text(), ex)) {
        return false;
      }
    }
  }

  return true;
}
