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
 * Tests for the chat palette's pure matching/ranking helpers: one table per
 * ranking tier, cross-group tie contract, empty query, invalid/Go-zero
 * times, case normalization, and deterministic IDs.
 */

import { describe, it, expect } from 'vitest';
import {
  classifyMatch,
  bestTierAcrossFields,
  highlightRangesFor,
  activityMsFromTimestamp,
  rankCandidates,
  normalizeQuery,
  mergeAdjacent,
  type HighlightRange,
} from './chat-palette-match.js';
import { dmCandidateId, type PaletteCandidate } from '../client/chat-palette-types.js';

function agentCandidate(
  overrides: Partial<PaletteCandidate> & { id: string; label: string }
): PaletteCandidate {
  return {
    group: 'agents',
    searchFields: [overrides.label],
    secondaryLabel: '',
    activityMs: 0,
    target: { kind: 'dm', peerKind: 'agent', peerId: overrides.id, displayName: overrides.label },
    ...overrides,
  };
}

describe('classifyMatch: ranking tiers', () => {
  it.each`
    query       | field          | expected
    ${'coder'}  | ${'Coder'}     | ${0}
    ${'cod'}    | ${'Coder One'} | ${1}
    ${'er one'} | ${'Coder One'} | ${2}
    ${'cdrn'}   | ${'Coder One'} | ${3}
    ${'zzz'}    | ${'Coder One'} | ${null}
    ${''}       | ${'Coder One'} | ${0}
  `('classifies "$query" against "$field" as tier $expected', ({ query, field, expected }) => {
    expect(classifyMatch(normalizeQuery(query), field)).toBe(expected);
  });

  it('prefers an exact match over a prefix match', () => {
    expect(classifyMatch(normalizeQuery('bot'), 'Bot')).toBe(0);
    expect(classifyMatch(normalizeQuery('bot'), 'Bot Helper')).toBe(1);
  });

  it('subsequence requires in-order characters, not just presence', () => {
    // "or" appears in "Coder" as a subsequence (o...r) — but "ro" does not, in order.
    expect(classifyMatch(normalizeQuery('ro'), 'Coder')).toBeNull();
    expect(classifyMatch(normalizeQuery('or'), 'Coder')).toBe(3);
  });
});

describe('bestTierAcrossFields', () => {
  it('takes the best tier across multiple fields', () => {
    // Exact on the second field beats substring on the first.
    expect(bestTierAcrossFields(normalizeQuery('coder-one'), ['Coder One', 'coder-one'])).toBe(0);
  });

  it('returns null when no field matches', () => {
    expect(bestTierAcrossFields(normalizeQuery('zzz'), ['Coder One', 'coder-one'])).toBeNull();
  });

  it('a later field with a strictly better tier overwrites an already-set (non-null) best', () => {
    // bestFieldMatch's `if (best === null || tier < best.tier) best = ...`
    // has two independent conditions. The test
    // above ("Exact on the second field beats substring on the first")
    // doesn't actually discriminate the `tier < best.tier` half: its first
    // field doesn't match at all (tier null, `continue`s), so `best` is
    // still null when the second field is checked, and `best === null`
    // alone already explains the result. Here the first field *does*
    // match (tier 1, prefix) before the second field's better (tier 0,
    // exact) match is found, so only `tier < best.tier` can produce the
    // correct answer.
    expect(bestTierAcrossFields(normalizeQuery('coder-one'), ['coder-one-x', 'coder-one'])).toBe(0);
  });
});

describe('case normalization', () => {
  it('matches regardless of query case', () => {
    expect(classifyMatch(normalizeQuery('CODER'), 'coder one')).toBe(1);
  });

  it('matches regardless of field case', () => {
    expect(classifyMatch(normalizeQuery('coder'), 'CODER ONE')).toBe(1);
  });

  it('normalizes composed vs decomposed Unicode forms to the same match', () => {
    // "café" as a precomposed é (U+00E9) vs. e + combining acute (U+0065 U+0301).
    const precomposed = 'café';
    const decomposed = 'café';
    expect(classifyMatch(normalizeQuery(decomposed), precomposed)).toBe(0);
  });
});

describe('empty query', () => {
  it('matches every candidate at tier 0', () => {
    expect(classifyMatch('', 'anything')).toBe(0);
  });

  it('a whitespace-only query is treated as empty', () => {
    expect(normalizeQuery('   ')).toBe('');
    expect(classifyMatch(normalizeQuery('   '), 'anything')).toBe(0);
  });
});

describe('activityMsFromTimestamp: invalid/Go-zero times', () => {
  it('returns 0 for a missing timestamp', () => {
    expect(activityMsFromTimestamp(undefined)).toBe(0);
  });

  it('returns 0 for an empty string', () => {
    expect(activityMsFromTimestamp('')).toBe(0);
  });

  it('returns 0 for a Go zero-value timestamp', () => {
    expect(activityMsFromTimestamp('0001-01-01T00:00:00Z')).toBe(0);
  });

  it('returns 0 for an unparseable timestamp', () => {
    expect(activityMsFromTimestamp('not-a-date')).toBe(0);
  });

  it('returns epoch ms for a valid timestamp', () => {
    expect(activityMsFromTimestamp('2026-01-01T00:00:00Z')).toBe(
      Date.parse('2026-01-01T00:00:00Z')
    );
  });

  it('clamps a valid but pre-epoch (negative epoch ms) timestamp to 0, not a negative activityMs', () => {
    // The Go-zero `startsWith('0001')` check and `Number.isFinite` are both
    // subsumed by `parsed > 0` ("0001-..." parses to a deeply negative
    // epoch ms, and `NaN > 0` is false), and so is `!value` for an empty
    // string. `!value` is still needed to keep `value.startsWith` from
    // throwing on `undefined`. Only a genuinely negative *finite* epoch ms
    // (a valid pre-1970 date, not Go's zero value) discriminates
    // `parsed > 0` on its own.
    expect(activityMsFromTimestamp('1950-01-01T00:00:00Z')).toBe(0);
  });
});

describe('highlightRangesFor: preserves original labels', () => {
  it('returns a contiguous range for a prefix match', () => {
    expect(highlightRangesFor('cod', 'Coder One')).toEqual([{ start: 0, end: 3 }]);
  });

  it('returns a contiguous range for a substring match', () => {
    // "coder one": c(0) o(1) d(2) e(3) r(4) ' '(5) o(6) n(7) e(8) — "er on" starts at 3.
    expect(highlightRangesFor('er on', 'Coder One')).toEqual([{ start: 3, end: 8 }]);
  });

  it('prefers a contiguous exact-substring match over a scattered greedy-subsequence match', () => {
    // "tab robot": t(0) a(1) b(2) ' '(3) r(4) o(5) b(6) o(7) t(8) — "bot" is
    // an exact, contiguous substring of "Robot" at [6, 9]. A naive greedy
    // per-character subsequence scan would instead take the *first*
    // occurrence of each letter — b(2), o(5), t(8) — three scattered
    // single-character ranges from "Tab" and "Robot" combined. The
    // exact-substring fast path must win over the greedy-subsequence scan —
    // the two approaches are not equivalent, and this is the counter-example
    // that proves it.
    expect(highlightRangesFor('bot', 'Tab Robot')).toEqual([{ start: 6, end: 9 }]);
  });

  it('returns per-character ranges for a subsequence match, merging adjacent characters', () => {
    // "coder": c(0) o(1) d(2) e(3) r(4) — c, d, r are not adjacent to each other.
    const ranges = highlightRangesFor('cdr', 'Coder');
    expect(ranges).toEqual([
      { start: 0, end: 1 },
      { start: 2, end: 3 },
      { start: 4, end: 5 },
    ]);
  });

  it('merges adjacent matched characters into one range for a subsequence match', () => {
    // "coder": c(0) o(1) d(2) e(3) r(4) — d(2) and e(3) are adjacent and merge.
    const ranges = highlightRangesFor('cde', 'Coder');
    expect(ranges).toEqual([
      { start: 0, end: 1 },
      { start: 2, end: 4 },
    ]);
  });

  it('returns [] for an empty query', () => {
    expect(highlightRangesFor('', 'Coder One')).toEqual([]);
  });

  it('is case-insensitive but the ranges index into the original (non-lowercased) string', () => {
    const ranges = highlightRangesFor('CODER', 'Coder One');
    expect(ranges).toEqual([{ start: 0, end: 5 }]);
  });

  it('highlights a decomposed (combining-accent) character matched by its composed query form', () => {
    // "caf" + "e" + a combining acute mark (U+0301), built from explicit
    // \u escapes so this doesn't depend on which byte form an editor
    // happens to save a literal accented character as. The result is 5
    // UTF-16 units, not 4, because the final character is two code units
    // (base + combining mark) here \u2014 it reads visually as "cafe" with an
    // accent over the last letter.
    const decomposed = 'caf' + 'e\u0301';
    expect(decomposed).toHaveLength(5);
    const composedQuery = '\u00e9'; // the single precomposed character U+00E9
    const ranges = highlightRangesFor(composedQuery, decomposed);
    // The whole 2-unit "e + combining acute" cluster is the highlight, not
    // just one code unit of it (which would slice the character in half).
    expect(ranges).toEqual([{ start: 3, end: 5 }]);
  });

  it('highlights the full decomposed cluster for a substring match spanning it', () => {
    const decomposed = 'caf' + 'e\u0301' + ' bot';
    const ranges = highlightRangesFor('f\u00e9 b', decomposed);
    expect(ranges).toEqual([{ start: 2, end: 7 }]);
    expect(decomposed.slice(ranges[0].start, ranges[0].end)).toBe('f' + 'e\u0301' + ' b');
  });

  // An astral character (any emoji) is a surrogate pair \u2014 two
  // UTF-16 code units for one code point. A position map with one entry per
  // code *point* while `normalized` is indexed by code *unit* would desync
  // on any emoji before or inside a match, producing an out-of-bounds
  // (`undefined`) end past it \u2014 which, once it flowed into
  // `renderHighlighted`'s `text.slice(undefined, undefined)`, would
  // re-render the entire label a second time inside a `<mark>`.
  const ROBOT = '\u{1F916}'; // \ud83e\udd16 \u2014 one astral code point, 2 UTF-16 units
  const THUMBS_MEDIUM = '\u{1F44D}\u{1F3FD}'; // \ud83d\udc4d\ud83c\udffd \u2014 base + skin-tone modifier, one grapheme cluster, 4 UTF-16 units

  it('highlights a match correctly after an astral-character (emoji) label prefix', () => {
    const label = `${ROBOT} Coder`;
    const ranges = highlightRangesFor('coder', label);
    expect(ranges).toEqual([{ start: 3, end: 8 }]);
    expect(label.slice(ranges[0].start, ranges[0].end)).toBe('Coder');
  });

  it('highlights a subsequence match correctly around an astral character in the middle of the label', () => {
    const label = `Co${ROBOT}der`; // C(0) o(1) robot(2-3) d(4) e(5) r(6)
    const ranges = highlightRangesFor('cdr', label);
    expect(ranges).toEqual([
      { start: 0, end: 1 },
      { start: 4, end: 5 },
      { start: 6, end: 7 },
    ]);
    expect(ranges.map((r) => label.slice(r.start, r.end))).toEqual(['C', 'd', 'r']);
  });

  it('does not bold the entire label when a multi-code-point grapheme cluster (skin-tone emoji) precedes the match', () => {
    const label = `Coder ${THUMBS_MEDIUM} x`;
    const ranges = highlightRangesFor('x', label);
    expect(ranges).toEqual([{ start: label.length - 1, end: label.length }]);
    expect(label.slice(ranges[0].start, ranges[0].end)).toBe('x');
  });

  it('the concatenation of highlighted + unhighlighted spans always reconstructs the original label exactly', () => {
    const label = `${ROBOT}Co${THUMBS_MEDIUM}der`;
    const ranges = highlightRangesFor('cdr', label);
    let cursor = 0;
    let rebuilt = '';
    for (const r of ranges) {
      rebuilt += label.slice(cursor, r.start) + label.slice(r.start, r.end);
      cursor = r.end;
    }
    rebuilt += label.slice(cursor);
    expect(rebuilt).toBe(label);
  });

  it('highlights a Greek word ending in a final sigma (context-sensitive lowercasing)', () => {
    // "ΟΔΟΣ".toLowerCase() -> "οδος" (final ς), but lowercasing the lone
    // cluster "Σ" in isolation gives σ instead — classifyMatch already uses
    // whole-string normalizeMatchText and matches this correctly; a highlight
    // map (buildNormalizedField) that lowercased cluster by cluster instead
    // would silently produce highlight: [] for a real, tier-0 match.
    const label = 'ΟΔΟΣ';
    expect(classifyMatch('οδος', label)).toBe(0);
    const ranges = highlightRangesFor('οδος', label);
    expect(ranges).toEqual([{ start: 0, end: 4 }]);
    expect(label.slice(ranges[0].start, ranges[0].end)).toBe('ΟΔΟΣ');
  });

  it('final-sigma highlighting still works when the label also contains a length-changing lowercase (İ)', () => {
    // Falling back to per-cluster lowercasing whenever the *whole-string*
    // lowered length differs from the composed length — which happens
    // whenever the label contains any length-changing lowercase (e.g. İ,
    // U+0130 -> "i" + combining dot, 1->2 code units), regardless of
    // whether that character has anything to do with the sigma — would lose
    // final-sigma correctness for any label containing both. "ΟΔΟΣ
    // İ".toLowerCase() === "οδος i̇" (final sigma still preserved; a space
    // breaks the "followed by a cased letter" lookahead that would
    // otherwise suppress it).
    const label = 'ΟΔΟΣ İ';
    expect(classifyMatch('οδος', label)).toBe(1); // prefix match, not exact
    const ranges = highlightRangesFor('οδος', label);
    expect(ranges).toEqual([{ start: 0, end: 4 }]);
    expect(label.slice(ranges[0].start, ranges[0].end)).toBe('ΟΔΟΣ');
  });

  it('matches a label whose lowercase only recomposes to the query form after a second NFC pass', () => {
    // "J̌" (J + combining caron U+030C) has no precomposed *uppercase*
    // form, so `.normalize('NFC')` leaves it decomposed. But its lowercase
    // ("j" + caron) *does* have a precomposed form, U+01F0 "ǰ" — and
    // `.toLowerCase()` alone does not recompose it: "J̌".toLowerCase()
    // produces the still-decomposed "ǰ", 2 code units, not the
    // precomposed "ǰ", 1 code unit. A query typed as "ǰ" normalizes to the
    // precomposed form, so without a second `.normalize('NFC')` after
    // lowercasing, the two never compare equal even though they're the same
    // letter.
    const label = 'J̌'; // decomposed uppercase "J̌", no precomposed uppercase exists
    const query = 'ǰ'; // precomposed lowercase "ǰ"
    expect(classifyMatch(normalizeQuery(query), label)).toBe(0);
    const ranges = highlightRangesFor(normalizeQuery(query), label);
    expect(ranges).toEqual([{ start: 0, end: label.length }]);
    expect(label.slice(ranges[0].start, ranges[0].end)).toBe(label);
  });
});

describe('mergeAdjacent: does not mutate its input', () => {
  it('leaves the caller-owned range objects untouched after merging adjacent ranges', () => {
    // Without the copy, `merged` would start as `[sorted[0]]` — a reference
    // to the same object as `ranges[0]` (the `[...ranges].sort(...)` spread
    // copies the array, not its elements). Merging a later adjacent range
    // would then do `last.end = ...` directly on that shared object,
    // mutating the caller's input in place.
    const first: HighlightRange = { start: 0, end: 2 };
    const second: HighlightRange = { start: 2, end: 4 };
    const input = [first, second];

    const merged = mergeAdjacent(input);

    expect(merged).toEqual([{ start: 0, end: 4 }]);
    expect(first).toEqual({ start: 0, end: 2 });
    expect(input[0]).toEqual({ start: 0, end: 2 });
  });
});

describe('rankCandidates: cross-group tie contract and deterministic IDs', () => {
  it('sorts by tier ascending first', () => {
    const exact = agentCandidate({ id: dmCandidateId('agent', 'a1'), label: 'Bot' });
    const prefix = agentCandidate({ id: dmCandidateId('agent', 'a2'), label: 'Bot Helper' });
    const ranked = rankCandidates('bot', [prefix, exact]);
    expect(ranked.map((r) => r.candidate.id)).toEqual([exact.id, prefix.id]);
  });

  it('breaks ties by activityMs descending within the same tier', () => {
    const older = agentCandidate({
      id: dmCandidateId('agent', 'a1'),
      label: 'Bot',
      activityMs: 100,
    });
    const newer = agentCandidate({
      id: dmCandidateId('agent', 'a2'),
      label: 'Bot',
      activityMs: 200,
    });
    const ranked = rankCandidates('bot', [older, newer]);
    expect(ranked.map((r) => r.candidate.id)).toEqual([newer.id, older.id]);
  });

  it('breaks further ties by normalized label ascending, then by stable ID ascending', () => {
    const b = agentCandidate({ id: dmCandidateId('agent', 'z'), label: 'Beta' });
    const a = agentCandidate({ id: dmCandidateId('agent', 'y'), label: 'Alpha' });
    const rankedByLabel = rankCandidates('', [b, a]);
    expect(rankedByLabel.map((r) => r.candidate.label)).toEqual(['Alpha', 'Beta']);

    const sameLabelHigherId = agentCandidate({ id: dmCandidateId('agent', 'b'), label: 'Same' });
    const sameLabelLowerId = agentCandidate({ id: dmCandidateId('agent', 'a'), label: 'Same' });
    const rankedById = rankCandidates('', [sameLabelHigherId, sameLabelLowerId]);
    expect(rankedById.map((r) => r.candidate.id)).toEqual([
      sameLabelLowerId.id,
      sameLabelHigherId.id,
    ]);
  });

  it('a label that is a strict prefix of another sorts first (compareByCodePoint length branches)', () => {
    // compareByCodePoint has "an.done"-only and "bn.done"-only branches for
    // when one string is a strict prefix of the other, distinct from the
    // "both done" (equal strings) branch — which is actually unreachable
    // from compareRanked's only call site, since it's guarded by
    // `if (labelA !== labelB)` (never invoked with equal labels). Covers
    // both prefix branches: an 'Alpha'/'Beta'-style pair differs at the
    // first code point and never reaches either iterator's exhaustion.
    const helper = agentCandidate({ id: dmCandidateId('agent', 'a'), label: 'Bot Helper' });
    const bot = agentCandidate({ id: dmCandidateId('agent', 'b'), label: 'Bot' });
    // Both input orderings are tested, since a 2-element sort's comparator
    // argument order is implementation-defined — each ordering covers one
    // of the two (otherwise-symmetric) length-comparison branches.
    expect(rankCandidates('', [helper, bot]).map((r) => r.candidate.label)).toEqual([
      'Bot',
      'Bot Helper',
    ]);
    expect(rankCandidates('', [bot, helper]).map((r) => r.candidate.label)).toEqual([
      'Bot',
      'Bot Helper',
    ]);
  });

  it('breaks label ties by code-point order, not code-unit order', () => {
    // U+FF21 (FULLWIDTH LATIN CAPITAL LETTER A) is a single UTF-16 code unit,
    // 0xFF21. U+1D400 (MATHEMATICAL BOLD CAPITAL A) is astral: as UTF-16 it's
    // the surrogate pair 0xD835 0xDC00. Naive `<`/`>` (code-unit) comparison
    // compares the first code units directly: 0xD835 < 0xFF21, so it would
    // sort U+1D400's label *first* even though its actual code point
    // (0x1D400 = 119808) is larger than U+FF21's (0xFF21 = 65313). The
    // correct code-point comparator must sort U+FF21 first.
    const fullwidthA = 'Ａ';
    const mathBoldA = '\u{1D400}';
    expect(fullwidthA < mathBoldA).toBe(false); // code-unit order gets this backwards
    const withFullwidth = agentCandidate({
      id: dmCandidateId('agent', 'fullwidth'),
      label: fullwidthA,
    });
    const withMathBold = agentCandidate({
      id: dmCandidateId('agent', 'mathbold'),
      label: mathBoldA,
    });
    const ranked = rankCandidates('', [withMathBold, withFullwidth]);
    expect(ranked.map((r) => r.candidate.label)).toEqual([fullwidthA, mathBoldA]);
  });

  it('discards candidates with no match for a nonempty query', () => {
    const match = agentCandidate({ id: dmCandidateId('agent', 'a1'), label: 'Coder' });
    const noMatch = agentCandidate({ id: dmCandidateId('agent', 'a2'), label: 'Reviewer' });
    const ranked = rankCandidates('cod', [match, noMatch]);
    expect(ranked.map((r) => r.candidate.id)).toEqual([match.id]);
  });

  it('an empty query includes every candidate, ranked by recency', () => {
    const recent = agentCandidate({
      id: dmCandidateId('agent', 'a1'),
      label: 'A',
      activityMs: 500,
    });
    const stale = agentCandidate({ id: dmCandidateId('agent', 'a2'), label: 'B', activityMs: 10 });
    const ranked = rankCandidates('', [stale, recent]);
    expect(ranked.map((r) => r.candidate.id)).toEqual([recent.id, stale.id]);
  });

  it('produces deterministic, JSON-tuple candidate IDs for DM targets', () => {
    expect(dmCandidateId('agent', 'agent-123')).toBe('["dm","agent","agent-123"]');
    expect(dmCandidateId('user', 'user-456')).toBe('["dm","user","user-456"]');
  });

  it('highlights the label when the match came from the label field', () => {
    const candidate = agentCandidate({
      id: dmCandidateId('agent', 'a1'),
      label: 'Coder One',
      secondaryLabel: 'coder-one',
      searchFields: ['Coder One', 'coder-one'],
    });
    const [ranked] = rankCandidates('coder one', [candidate]);
    expect(ranked.highlightField).toBe('label');
    expect(ranked.highlight).toEqual([{ start: 0, end: 9 }]);
  });

  it('highlights the secondary label (not the label) when only the slug matches', () => {
    // "zeta" only appears in the slug, not the display name.
    const candidate = agentCandidate({
      id: dmCandidateId('agent', 'a1'),
      label: 'Automation Bot',
      secondaryLabel: 'zeta-bot',
      searchFields: ['Automation Bot', 'zeta-bot'],
    });
    const [ranked] = rankCandidates('zeta', [candidate]);
    expect(ranked.highlightField).toBe('secondaryLabel');
    expect(ranked.highlight).toEqual([{ start: 0, end: 4 }]);
  });

  it('a match from a third search field (neither label nor secondaryLabel) leaves highlightField null and highlight empty, even when secondaryLabel is present', () => {
    // Per RankedCandidate's own doc comment, highlightField is `null` "for
    // ... a match that came from some other search field (e.g. an email
    // address) that isn't shown as either the primary or secondary line".
    // rankCandidates' `else if (candidate.secondaryLabel && match.field ===
    // candidate.secondaryLabel)` has two independent conditions. Covers a
    // candidate that HAS a secondaryLabel, but whose actual match came from
    // a third field (an extra searchField beyond label/secondaryLabel).
    const candidate = agentCandidate({
      id: dmCandidateId('agent', 'a1'),
      label: 'Automation Bot',
      secondaryLabel: 'automation-bot',
      searchFields: ['Automation Bot', 'automation-bot', 'bot@example.com'],
    });
    const [ranked] = rankCandidates('example', [candidate]);
    expect(ranked.highlightField).toBeNull();
    expect(ranked.highlight).toEqual([]);
  });

  it('highlight stays empty (not computed against secondaryLabel) when the winning match is a third field that also happens to share characters with secondaryLabel', () => {
    // Distinct from the test above: here `candidate.secondaryLabel` itself
    // *would* produce a non-empty (if weaker, tier-2 substring) match for
    // the query, so computing `highlight = highlightRangesFor(q, target)`
    // against secondaryLabel unconditionally (dropping the
    // `if (highlightField)` gate) would silently produce a
    // spurious non-empty range even though `highlightField` correctly
    // stays null — only asserting `highlightField === null` (as the test
    // above does) wouldn't catch that, since it doesn't also check
    // `highlight`. A third field with a *better* (tier-1 prefix) match
    // wins bestFieldMatch over secondaryLabel's weaker tier-2 substring
    // match, so this exercises exactly that gap.
    const candidate = agentCandidate({
      id: dmCandidateId('agent', 'a1'),
      label: 'Zeta Bot',
      secondaryLabel: 'zex-bot', // tier 2 (substring: "z-EX-bot") for query "ex"
      searchFields: ['Zeta Bot', 'zex-bot', 'ex-changed@example.com'], // tier 1 (prefix) — wins
    });
    const [ranked] = rankCandidates('ex', [candidate]);
    expect(ranked.highlightField).toBeNull();
    expect(ranked.highlight).toEqual([]);
  });

  it('leaves highlightField null (and highlight empty) for an empty query', () => {
    const candidate = agentCandidate({ id: dmCandidateId('agent', 'a1'), label: 'Coder One' });
    const [ranked] = rankCandidates('', [candidate]);
    expect(ranked.highlightField).toBeNull();
    expect(ranked.highlight).toEqual([]);
  });
});
