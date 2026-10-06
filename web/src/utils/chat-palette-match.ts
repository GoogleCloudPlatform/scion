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

/**
 * Pure normalization, match classification, deterministic comparator and
 * highlight-range helpers for the native chat quick command palette.
 *
 * No dependency on CodeMirror, an external fuzzy-match package, or any DOM
 * API — every export here is a plain function over strings and plain
 * objects so it can be unit tested without mounting a component.
 */

import { PALETTE_GROUP_ORDER, type PaletteCandidate } from '../client/chat-palette-types.js';

/** Match tiers, best (0) to worst (3). `null` means "no match". */
export type MatchTier = 0 | 1 | 2 | 3;

/** A [start, end) range of indices into the *original* (non-normalized) field string. */
export interface HighlightRange {
  start: number;
  end: number;
}

/**
 * Stable reading order for palette groups, derived from the single shared
 * {@link PALETTE_GROUP_ORDER} constant so this comparator and the palette's
 * own Tab/Shift+Tab group cycling (`quick-palette.ts`) can never disagree
 * about group order. Unknown groups (there are none today) sort last.
 */
function groupRank(group: string): number {
  const index = PALETTE_GROUP_ORDER.indexOf(group as (typeof PALETTE_GROUP_ORDER)[number]);
  return index === -1 ? PALETTE_GROUP_ORDER.length : index;
}

/**
 * NFC-normalize and lowercase a string for matching purposes.
 *
 * `toLowerCase()` is not NFC-closed — some uppercase letters
 * with a combining mark have no precomposed uppercase form, but their
 * lowercase form does (e.g. "J̌" U+004A U+030C has no single precomposed
 * code point, but its lowercase "ǰ" is the single precomposed U+01F0).
 * `'J̌'.normalize('NFC').toLowerCase()` yields the *decomposed* "ǰ"
 * (U+006A U+030C), while a query typed as "ǰ" normalizes to the
 * *precomposed* U+01F0 — the two never compare equal even though they're
 * the same letter. A second `.normalize('NFC')` after lowercasing recomposes
 * the result, closing the loop.
 */
export function normalizeMatchText(input: string): string {
  return input.normalize('NFC').toLowerCase().normalize('NFC');
}

/** Normalize a query: NFC + lowercase, with edges trimmed. A whitespace-only query is empty. */
export function normalizeQuery(input: string): string {
  return normalizeMatchText(input.trim());
}

/** A normalized field plus, for every character of `normalized`, the [start, end) span in the original string its source grapheme cluster occupies. */
interface NormalizedField {
  normalized: string;
  starts: number[];
  ends: number[];
}

/**
 * Build a normalized (NFC + lowercased) copy of `original` together with a
 * position map back to `original`, so highlight ranges can be expressed in
 * original-string coordinates even though matching happens against the
 * normalized form.
 *
 * NFC composition is context-sensitive: a base character only composes with
 * a *following* combining mark (e.g. "e" + U+0301 -> "é"), so normalizing
 * code points independently — which a naive per-character loop would do —
 * never actually composes anything. This walks `original` one grapheme
 * cluster at a time (a base character plus any combining marks attached to
 * it, via `Intl.Segmenter`) so composition sees the same context a person
 * reads it in. Every output character produced by a cluster's NFC+lowercase
 * expansion maps back to that whole cluster's original span, so a highlight
 * range can never land inside a multi-unit character.
 */
function buildNormalizedField(original: string): NormalizedField {
  const clusters = Array.from(graphemeClusters(original));

  // Compose the whole string (context-sensitive at composition time) purely
  // to lowercase it in one call below — never used for the position map
  // itself.
  let composed = '';
  for (const { cluster } of clusters) {
    composed += cluster.normalize('NFC');
  }

  // Lowercase the whole *composed* string in one call, not cluster by
  // cluster: `toLowerCase()` is itself context-sensitive for some scripts
  // (Greek final sigma — "ΟΔΟΣ".toLowerCase() produces "οδος" with a final
  // ς, but the lone cluster "Σ".toLowerCase() produces σ instead, since it
  // never sees that it's word-final). A second `.normalize('NFC')` after
  // lowercasing closes the loop for uppercase letters + combining marks
  // whose lowercase recomposes into a single precomposed code point
  // (`toLowerCase()` is not NFC-closed). This must use the exact same
  // pipeline as `normalizeMatchText`, or a query normalized by one and a
  // field by the other could disagree.
  const lowered = composed.toLowerCase().normalize('NFC');

  // Build the position map from each cluster's *own* lowercased length,
  // computed in isolation. A character's lowercase *length* is intrinsic to
  // that character (e.g. İ, U+0130, always lowercases to "i" + a combining
  // dot above, 1->2 code units, regardless of neighbors); only its *value*
  // is ever context-sensitive (final sigma: always 1 code unit either way,
  // just σ vs ς). So per-cluster length + whole-string value gets both
  // length-changing casing (İ) and context-sensitive casing (final sigma)
  // right at once, with no fallback needed for either — re-lowercasing per
  // cluster whenever *any* character in the whole string changed length
  // would silently lose final-sigma correctness for any label that also
  // happened to contain an İ. The per-cluster length must go through the
  // identical NFC-lowercase-NFC pipeline as `lowered` above, or the final
  // `.normalize('NFC')`'s own length changes (recomposing a decomposed mark
  // sequence into fewer code units) would desync the two the same way an
  // uncorrected length-changing character would.
  const starts: number[] = [];
  const ends: number[] = [];
  for (const { cluster, start, end } of clusters) {
    const clusterLoweredLength = cluster.normalize('NFC').toLowerCase().normalize('NFC').length;
    for (let i = 0; i < clusterLoweredLength; i++) {
      starts.push(start);
      ends.push(end);
    }
  }

  // Safety net: if the per-cluster-length reasoning above is ever wrong for
  // some case it didn't anticipate, fall back to a fully per-cluster
  // (context-blind but internally self-consistent) mapping rather than let
  // `normalized`/`starts`/`ends` desync — the same failure mode the
  // position map above is built to avoid.
  if (starts.length !== lowered.length) {
    const normalized: string[] = [];
    const fallbackStarts: number[] = [];
    const fallbackEnds: number[] = [];
    for (const { cluster, start, end } of clusters) {
      const expanded = cluster.normalize('NFC').toLowerCase().normalize('NFC');
      for (let i = 0; i < expanded.length; i++) {
        normalized.push(expanded[i]);
        fallbackStarts.push(start);
        fallbackEnds.push(end);
      }
    }
    return { normalized: normalized.join(''), starts: fallbackStarts, ends: fallbackEnds };
  }

  return { normalized: lowered, starts, ends };
}

type GraphemeSegmenter = { segment(s: string): Iterable<{ segment: string; index: number }> };

/**
 * Lazily-constructed, module-level `Intl.Segmenter` singleton: a segmenter
 * has no per-string state, so constructing a fresh one on every
 * `graphemeClusters` call — which runs once per candidate per ranking pass
 * — would be pure waste. `undefined` (rather than never attempting
 * construction again) distinguishes "not yet built" from "environment has
 * no Intl.Segmenter", so the fallback path below still runs every time in
 * the latter case instead of only once.
 */
let cachedSegmenter: GraphemeSegmenter | null | undefined;

function getSegmenter(): GraphemeSegmenter | null {
  if (cachedSegmenter === undefined) {
    const SegmenterCtor = (
      Intl as unknown as {
        Segmenter?: new (locale: undefined, opts: { granularity: 'grapheme' }) => GraphemeSegmenter;
      }
    ).Segmenter;
    cachedSegmenter = SegmenterCtor
      ? new SegmenterCtor(undefined, { granularity: 'grapheme' })
      : null;
  }
  return cachedSegmenter;
}

/**
 * Yields each grapheme cluster of `input` with its [start, end) span of
 * UTF-16 code units in `input`. Uses `Intl.Segmenter` where available (every
 * target browser and Node 18+); falls back to one Unicode code point per
 * "cluster" otherwise — combining marks then won't compose with their base
 * character, but every match/highlight position is still correct for the
 * far more common single-code-point-per-character case.
 */
function* graphemeClusters(
  input: string
): Generator<{ cluster: string; start: number; end: number }> {
  const segmenter = getSegmenter();
  if (segmenter) {
    for (const { segment, index } of segmenter.segment(input)) {
      yield { cluster: segment, start: index, end: index + segment.length };
    }
    return;
  }
  let index = 0;
  for (const ch of input) {
    yield { cluster: ch, start: index, end: index + ch.length };
    index += ch.length;
  }
}

/** True when every character of `needle` appears in `haystack`, in order (not necessarily contiguous). */
function isSubsequence(needle: string, haystack: string): boolean {
  let i = 0;
  for (let j = 0; j < haystack.length && i < needle.length; j++) {
    if (haystack[j] === needle[i]) i++;
  }
  return i === needle.length;
}

/**
 * Classify how well `field` matches `query`. `query` must already be
 * normalized (see {@link normalizeQuery}); `field` is normalized here.
 *
 * An empty query matches everything at the best tier (0) — the palette's
 * empty-query state ranks purely by recency.
 */
export function classifyMatch(query: string, field: string): MatchTier | null {
  if (query === '') return 0;
  const normalizedField = normalizeMatchText(field);
  if (normalizedField === query) return 0;
  if (normalizedField.startsWith(query)) return 1;
  if (normalizedField.includes(query)) return 2;
  if (isSubsequence(query, normalizedField)) return 3;
  return null;
}

/** A field's match tier plus which field (by exact string) produced it. */
interface FieldMatch {
  tier: MatchTier;
  field: string;
}

/**
 * The best (lowest) match tier across every search field, together with the
 * exact field string that produced it — so a caller can highlight the field
 * that actually matched, instead of always highlighting `label` regardless
 * of whether the match came from a slug, email, or other search field.
 * Returns `null` if no field matches.
 */
function bestFieldMatch(query: string, fields: string[]): FieldMatch | null {
  let best: FieldMatch | null = null;
  for (const field of fields) {
    const tier = classifyMatch(query, field);
    if (tier === null) continue;
    if (best === null || tier < best.tier) best = { tier, field };
    if (best.tier === 0) break;
  }
  return best;
}

/** The best (lowest) match tier across every search field, or `null` if none match. */
export function bestTierAcrossFields(query: string, fields: string[]): MatchTier | null {
  return bestFieldMatch(query, fields)?.tier ?? null;
}

/**
 * Merge adjacent/overlapping ranges (both sorted and unsorted input are
 * handled) into contiguous blocks. Exported only so it can be unit tested
 * directly — every element of `ranges` is copied, never returned by
 * reference, so mutating a merged range (`last.end = ...` below) can never
 * mutate a caller's input range object.
 */
export function mergeAdjacent(ranges: HighlightRange[]): HighlightRange[] {
  if (ranges.length === 0) return ranges;
  const sorted = [...ranges].sort((a, b) => a.start - b.start);
  const merged: HighlightRange[] = [{ ...sorted[0] }];
  for (let i = 1; i < sorted.length; i++) {
    const last = merged[merged.length - 1];
    const cur = sorted[i];
    if (cur.start <= last.end) {
      last.end = Math.max(last.end, cur.end);
    } else {
      merged.push({ ...cur });
    }
  }
  return merged;
}

/**
 * Compute highlight ranges (in original-string coordinates) for the matched
 * code points of `query` within `field`. Returns `[]` for an empty query or
 * no match. Exact/prefix/substring matches produce one contiguous range;
 * subsequence matches produce one range per matched character, merged where
 * adjacent.
 */
export function highlightRangesFor(query: string, field: string): HighlightRange[] {
  const q = normalizeQuery(query);
  if (!q) return [];
  const { normalized, starts, ends } = buildNormalizedField(field);

  const idx = normalized.indexOf(q);
  if (idx !== -1) {
    return [{ start: starts[idx], end: ends[idx + q.length - 1] }];
  }

  const ranges: HighlightRange[] = [];
  let qi = 0;
  for (let ni = 0; ni < normalized.length && qi < q.length; ni++) {
    if (normalized[ni] === q[qi]) {
      ranges.push({ start: starts[ni], end: ends[ni] });
      qi++;
    }
  }
  return qi === q.length ? mergeAdjacent(ranges) : [];
}

/**
 * Convert a Hub timestamp to epoch milliseconds for recency ranking.
 * Returns 0 for missing, unparseable, or Go zero-value timestamps (Go
 * marshals a zero `time.Time` as a string starting with `0001-`).
 */
export function activityMsFromTimestamp(value?: string): number {
  if (!value || value.startsWith('0001')) return 0;
  const parsed = Date.parse(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : 0;
}

/** A candidate together with its computed match tier and highlight ranges for the current query. */
export interface RankedCandidate<T extends PaletteCandidate = PaletteCandidate> {
  candidate: T;
  tier: MatchTier;
  /**
   * Which rendered field the query actually matched — `null` for an empty
   * query (tier 0 catch-all with nothing to highlight) or a match that came
   * from some other search field (e.g. an email address) that isn't shown
   * as either the primary or secondary line.
   */
  highlightField: 'label' | 'secondaryLabel' | null;
  /** Highlight ranges into whichever field `highlightField` names, in original-string coordinates. */
  highlight: HighlightRange[];
}

/**
 * Compares two strings by Unicode code point, not by UTF-16 code unit (what
 * `<`/`>` do natively). The two orders agree everywhere except across the
 * surrogate-pair range (U+E000-U+FFFF sort before supplementary-plane
 * characters under code-unit order but after them under code-point order) —
 * the label tiebreak uses code-point order.
 */
function compareByCodePoint(a: string, b: string): number {
  const ai = a[Symbol.iterator]();
  const bi = b[Symbol.iterator]();
  for (;;) {
    const an = ai.next();
    const bn = bi.next();
    if (an.done && bn.done) return 0;
    if (an.done) return -1;
    if (bn.done) return 1;
    const ac = an.value.codePointAt(0) ?? 0;
    const bc = bn.value.codePointAt(0) ?? 0;
    if (ac !== bc) return ac - bc;
  }
}

/**
 * `normalizedLabels` maps candidate ID -> `normalizeMatchText(candidate.label)`,
 * precomputed once per {@link rankCandidates} call — a naive
 * `normalizeMatchText(candidate.label)` inline here would re-normalize the
 * same label on every comparison a candidate takes part in during `.sort()`
 * (O(N log N) calls total, not O(N)).
 */
function compareRanked(
  a: RankedCandidate,
  b: RankedCandidate,
  normalizedLabels: Map<string, string>
): number {
  if (a.tier !== b.tier) return a.tier - b.tier;
  if (a.candidate.activityMs !== b.candidate.activityMs) {
    return b.candidate.activityMs - a.candidate.activityMs;
  }
  const groupA = groupRank(a.candidate.group);
  const groupB = groupRank(b.candidate.group);
  if (groupA !== groupB) return groupA - groupB;
  const labelA = normalizedLabels.get(a.candidate.id) ?? '';
  const labelB = normalizedLabels.get(b.candidate.id) ?? '';
  if (labelA !== labelB) return compareByCodePoint(labelA, labelB);
  if (a.candidate.id !== b.candidate.id) return a.candidate.id < b.candidate.id ? -1 : 1;
  return 0;
}

/**
 * Rank candidates against a query: filter out non-matches, classify each
 * remaining candidate's best tier and highlight ranges, and sort by
 * (tier asc, activityMs desc, group order, normalized label asc, id asc).
 *
 * An empty (or whitespace-only) query matches every candidate at tier 0, so
 * the result is effectively "most recent first" with the same stable ties.
 */
export function rankCandidates<T extends PaletteCandidate>(
  query: string,
  candidates: T[]
): Array<RankedCandidate<T>> {
  const q = normalizeQuery(query);
  const ranked: Array<RankedCandidate<T>> = [];
  const normalizedLabels = new Map<string, string>();
  for (const candidate of candidates) {
    const match = bestFieldMatch(q, candidate.searchFields);
    if (match === null) continue;
    let highlightField: 'label' | 'secondaryLabel' | null = null;
    let highlight: HighlightRange[] = [];
    if (q) {
      if (match.field === candidate.label) {
        highlightField = 'label';
      } else if (candidate.secondaryLabel && match.field === candidate.secondaryLabel) {
        highlightField = 'secondaryLabel';
      }
      if (highlightField) {
        const target = highlightField === 'label' ? candidate.label : candidate.secondaryLabel;
        highlight = highlightRangesFor(q, target);
      }
    }
    ranked.push({ candidate, tier: match.tier, highlightField, highlight });
    normalizedLabels.set(candidate.id, normalizeMatchText(candidate.label));
  }
  ranked.sort((a, b) => compareRanked(a, b, normalizedLabels));
  return ranked;
}
