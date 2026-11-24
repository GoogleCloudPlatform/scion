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

import { match, type FilterRow, type Query } from '../../shared/filter-query/index.js';
import type { FileEntry } from './file-browser.js';

/**
 * Extract the lowercase file extension without a leading dot from a relative file path.
 * Returns '' for extensionless names (e.g. "Makefile", "dir.v1/README") and dotfiles
 * without a secondary extension (e.g. ".gitignore").
 */
export function extractFileExtension(filePath: string): string {
  const slash = filePath.lastIndexOf('/');
  const base = slash >= 0 ? filePath.slice(slash + 1) : filePath;
  const dot = base.lastIndexOf('.');
  if (dot <= 0 || dot === base.length - 1) {
    return '';
  }
  return base.slice(dot + 1).toLowerCase();
}

/**
 * Compile a file-path pattern matcher using today's regex -> fuzzy fallback:
 * tries case-insensitive RegExp first; if invalid regex, falls back to
 * ordered-character case-insensitive subsequence match.
 */
export function compileFilePattern(pattern: string): (filePath: string) => boolean {
  try {
    const re = new RegExp(pattern, 'i');
    return (filePath: string) => re.test(filePath);
  } catch {
    const lower = pattern.toLowerCase();
    return (filePath: string) => {
      const name = filePath.toLowerCase();
      let ni = 0;
      for (let pi = 0; pi < lower.length; pi++) {
        ni = name.indexOf(lower[pi], ni);
        if (ni === -1) return false;
        ni++;
      }
      return true;
    };
  }
}

/** Report whether a parsed file query imposes no filter constraints. */
export function isFileQueryEmpty(query: Query): boolean {
  if (query.mode === 'text') {
    return query.text === '';
  }
  return (
    query.terms.length === 0 &&
    !query.text &&
    (!query.excludeText || query.excludeText.length === 0)
  );
}

/** Return the text portion sent to the server's `?q=` parameter (empty if none). */
export function extractFileQueryText(query: Query): string {
  return query.text ?? '';
}

function makeFileRow(filePath: string): FilterRow {
  const ext = extractFileExtension(filePath);
  return {
    values(key: string): string[] {
      if (key === 'ext' && ext) {
        return [ext, `.${ext}`];
      }
      return [];
    },
    text(): string[] {
      return [];
    },
  };
}

/**
 * Post-filter files by structured key terms (e.g. `ext:`) and excluded text terms
 * (`-word`), leaving positive text matching to the caller (e.g. server `?q=` results).
 */
export function postFilterFilesByQuery(files: readonly FileEntry[], query: Query): FileEntry[] {
  if (query.mode === 'text') {
    return [...files];
  }
  const hasTerms = query.terms.length > 0;
  const excludeMatchers = (query.excludeText ?? []).map(compileFilePattern);
  if (!hasTerms && excludeMatchers.length === 0) {
    return [...files];
  }
  const termsOnly: Query = { mode: 'structured', terms: query.terms };
  return files.filter((f) => {
    if (hasTerms && !match(termsOnly, makeFileRow(f.path))) {
      return false;
    }
    for (const matchesExclude of excludeMatchers) {
      if (matchesExclude(f.path)) {
        return false;
      }
    }
    return true;
  });
}

/**
 * Filter files locally using the full parsed query:
 * - Pure-text mode: regex -> fuzzy fallback verbatim.
 * - Structured mode: key terms (AND/OR/negation via shared `match`), positive text
 *   (regex -> fuzzy fallback), and excluded text terms (negated regex -> fuzzy fallback).
 */
export function filterFilesByQuery(files: readonly FileEntry[], query: Query): FileEntry[] {
  if (isFileQueryEmpty(query)) {
    return [...files];
  }
  if (query.mode === 'text') {
    const matchesText = compileFilePattern(query.text);
    return files.filter((f) => matchesText(f.path));
  }
  const positiveTextMatcher = query.text ? compileFilePattern(query.text) : null;
  const excludeMatchers = (query.excludeText ?? []).map(compileFilePattern);
  const hasTerms = query.terms.length > 0;
  const termsOnly: Query = { mode: 'structured', terms: query.terms };

  return files.filter((f) => {
    if (hasTerms && !match(termsOnly, makeFileRow(f.path))) {
      return false;
    }
    if (positiveTextMatcher && !positiveTextMatcher(f.path)) {
      return false;
    }
    for (const matchesExclude of excludeMatchers) {
      if (matchesExclude(f.path)) {
        return false;
      }
    }
    return true;
  });
}

/** Collect sorted unique lowercase extensions (without leading dot) for autocomplete. */
export function collectFileExtensions(files: readonly FileEntry[]): string[] {
  const exts = new Set<string>();
  for (const f of files) {
    const ext = extractFileExtension(f.path);
    if (ext) {
      exts.add(ext);
    }
  }
  return Array.from(exts).sort((a, b) => a.localeCompare(b));
}
