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
 * Sentinel form of Markdown with CriticMarkup, for the graphical review
 * editor (components/artifact-review).
 *
 * parseCritic (critic.ts, unchanged) reads the marks. Each mark token is
 * then replaced by one sentinel character, so a Markdown parser that knows
 * nothing about CriticMarkup can read the rest and keep the sentinels as
 * text; the editor turns them into marks afterwards and back into tokens
 * when it saves. A comment becomes a single sentinel, and its note is kept
 * aside, so a note is never read as Markdown.
 *
 * The sentinels are chosen per document from a list of Unicode punctuation
 * characters (general category Po), skipping every character the document
 * already contains. Punctuation keeps emphasis flanking the same as around
 * "{", and because none of the chosen characters occurs in the document,
 * its text can never be read as a mark boundary.
 *
 * This module does not reuse CRITIC_SENTINELS (critic.ts): those are
 * private-use characters for the read-only renderer, which replaces any
 * copy already in the text. The editor must keep every character.
 */

import { parseCritic } from './critic.js';

/** The twelve sentinels of one document. */
export interface SentinelAlphabet {
  insOpen: string;
  insClose: string;
  delOpen: string;
  delClose: string;
  subOpen: string;
  subSep: string;
  subClose: string;
  hlOpen: string;
  hlClose: string;
  note: string;
  /**
   * Binary digits that, in the form the editor opens (tagged), follow
   * each note sentinel with that comment's ordinal, so the editor takes
   * every note from its own place in the source.
   */
  tag0: string;
  tag1: string;
}

const ROLES: (keyof SentinelAlphabet)[] = [
  'insOpen',
  'insClose',
  'delOpen',
  'delClose',
  'subOpen',
  'subSep',
  'subClose',
  'hlOpen',
  'hlClose',
  'note',
  'tag0',
  'tag1',
];

/**
 * Candidate sentinels, in order of preference: the characters of
 * U+2E00..U+2E7F and then U+2CF9..U+2CFF whose general category is Po in
 * every Unicode version since 11.
 */
export const SENTINEL_CANDIDATES: readonly string[] = [
  0x2e00, 0x2e01, 0x2e06, 0x2e07, 0x2e08, 0x2e0b, 0x2e0e, 0x2e0f, 0x2e10, 0x2e11, 0x2e12, 0x2e13,
  0x2e14, 0x2e15, 0x2e16, 0x2e18, 0x2e19, 0x2e1b, 0x2e1e, 0x2e1f, 0x2e2a, 0x2e2b, 0x2e2c, 0x2e2d,
  0x2e2e, 0x2e30, 0x2e31, 0x2e32, 0x2e33, 0x2e34, 0x2e35, 0x2e36, 0x2e37, 0x2e38, 0x2e39, 0x2e3c,
  0x2e3d, 0x2e3e, 0x2e3f, 0x2e41, 0x2e43, 0x2e44, 0x2e45, 0x2e46, 0x2e47, 0x2e48, 0x2e49, 0x2cf9,
  0x2cfa, 0x2cfb, 0x2cfc, 0x2cfe, 0x2cff,
].map((c) => String.fromCodePoint(c));

/**
 * The alphabet for md: the first twelve candidates md does not contain,
 * or null when fewer than twelve are free (the editor then does not open).
 */
export function chooseAlphabet(md: string): SentinelAlphabet | null {
  return alphabetOf(SENTINEL_CANDIDATES.filter((c) => !md.includes(c)));
}

/** The alphabet made of the first twelve of chars, or null when there are fewer. */
export function alphabetOf(chars: readonly string[]): SentinelAlphabet | null {
  if (chars.length < ROLES.length) return null;
  const a = {} as SentinelAlphabet;
  ROLES.forEach((role, i) => {
    a[role] = chars[i];
  });
  return a;
}

/**
 * The alphabet for opening md in the editor, where the marks are read from
 * the Markdown parser's reading of the sentinel form, not of md itself.
 * The sentinel form drops comment notes, so text the parser keeps literal
 * in md (inside raw HTML or indented code that a note opens) can decode in
 * the sentinel form, for example a character reference to a candidate.
 * So a probe alphabet (the last twelve candidates free in md) builds the
 * sentinel form first; every candidate the parser yields from it, other
 * than the probe's own, came from the source and is excluded. Swapping one
 * candidate for another does not change how the text parses (all are
 * punctuation with no Markdown meaning), so the real sentinel form parses
 * the same way. shown(text) returns every string the parser yields for
 * text (the tagged form, as the editor opens it). Needs twenty-four free
 * candidates; null otherwise. Without a numeric character reference in md,
 * the probe's result is known without calling shown (see below).
 */
export function chooseOpeningAlphabet(
  md: string,
  shown: (sentinelText: string) => string
): SentinelAlphabet | null {
  const free = SENTINEL_CANDIDATES.filter((c) => !md.includes(c));
  if (free.length < 2 * ROLES.length) return null;
  // The parser yields a candidate that is not in its input only by
  // decoding a numeric character reference: a backslash only keeps ASCII
  // punctuation literal, and no named reference decodes to a candidate
  // (critic-sentinels.test.ts checks the installed table). Without one, the
  // probe would exclude nothing but its own characters, so its result is
  // known without parsing.
  if (!md.includes('&#')) return alphabetOf(free.slice(0, -ROLES.length));
  return probeOpeningAlphabet(md, shown);
}

/** chooseOpeningAlphabet by the probe parse, whatever md holds. */
export function probeOpeningAlphabet(
  md: string,
  shown: (sentinelText: string) => string
): SentinelAlphabet | null {
  const free = SENTINEL_CANDIDATES.filter((c) => !md.includes(c));
  if (free.length < 2 * ROLES.length) return null;
  const probeChars = free.slice(-ROLES.length);
  const probe = alphabetOf(probeChars)!;
  const yielded = shown(criticToSentinelText(md, probe, true).text);
  return alphabetOf(free.slice(0, -ROLES.length).filter((c) => !yielded.includes(c)));
}

/** The sentinels of an alphabet, as a set. */
export function sentinelSet(a: SentinelAlphabet): Set<string> {
  return new Set(ROLES.map((r) => a[r]));
}

/** The token each sentinel stands for (the note sentinel has none). */
export function tokenOf(a: SentinelAlphabet): Map<string, string> {
  return new Map([
    [a.insOpen, '{++'],
    [a.insClose, '++}'],
    [a.delOpen, '{--'],
    [a.delClose, '--}'],
    [a.subOpen, '{~~'],
    [a.subSep, '~>'],
    [a.subClose, '~~}'],
    [a.hlOpen, '{=='],
    [a.hlClose, '==}'],
  ]);
}

/** The sentinel form of a document. */
export interface SentinelText {
  /** md with each token replaced by its sentinel and each comment by the note sentinel. */
  text: string;
  /** The notes of the comments, in order. */
  notes: string[];
  /** starts[i] and ends[i] delimit the part of md that text[i] stands for. */
  starts: number[];
  ends: number[];
}

/**
 * Converts md to its sentinel form under alphabet a. With tagged, each
 * note sentinel is followed by the comment's ordinal (0, 1, …) in binary
 * digits tag0/tag1, which stand for no source text.
 */
export function criticToSentinelText(
  md: string,
  a: SentinelAlphabet,
  tagged = false
): SentinelText {
  const out: string[] = [];
  const notes: string[] = [];
  const starts: number[] = [];
  const ends: number[] = [];
  const put = (c: string, from: number, to: number): void => {
    out.push(c);
    starts.push(from);
    ends.push(to);
  };
  const putText = (t: string, at: number): void => {
    for (let k = 0; k < t.length; k++) put(t[k], at + k, at + k + 1);
  };
  for (const s of parseCritic(md)) {
    const body = s.start + 3;
    switch (s.kind) {
      case 'text':
        putText(s.text, s.start);
        break;
      case 'insertion':
      case 'deletion':
      case 'highlight': {
        const [open, close] =
          s.kind === 'insertion'
            ? [a.insOpen, a.insClose]
            : s.kind === 'deletion'
              ? [a.delOpen, a.delClose]
              : [a.hlOpen, a.hlClose];
        put(open, s.start, body);
        putText(s.text, body);
        put(close, s.end - 3, s.end);
        break;
      }
      case 'substitution': {
        const sep = body + s.text.length;
        put(a.subOpen, s.start, body);
        putText(s.text, body);
        put(a.subSep, sep, sep + 2);
        putText(s.next ?? '', sep + 2);
        put(a.subClose, s.end - 3, s.end);
        break;
      }
      case 'comment':
        put(a.note, s.start, s.end);
        if (tagged) {
          for (const d of notes.length.toString(2)) put(d === '0' ? a.tag0 : a.tag1, s.end, s.end);
        }
        notes.push(s.text);
        break;
    }
  }
  return { text: out.join(''), notes, starts, ends };
}

/**
 * Reads the ordinal tag that follows a note sentinel at text[at] (tagged
 * form): the ordinal and the number of digits, or null when there is none.
 */
export function readTag(
  text: string,
  at: number,
  a: SentinelAlphabet
): { ordinal: number; digits: number } | null {
  let n = 0;
  let digits = 0;
  for (let i = at + 1; i < text.length; i++) {
    const c = text[i];
    if (c !== a.tag0 && c !== a.tag1) break;
    n = n * 2 + (c === a.tag1 ? 1 : 0);
    digits++;
  }
  return digits ? { ordinal: n, digits } : null;
}

/** Converts sentinel text back to CriticMarkup, taking comment notes in order. */
export function sentinelTextToCritic(text: string, notes: string[], a: SentinelAlphabet): string {
  const tokens = tokenOf(a);
  let n = 0;
  let out = '';
  for (const c of text) {
    if (c === a.note) out += `{>>${notes[n++] ?? ''}<<}`;
    else out += tokens.get(c) ?? c;
  }
  return out;
}
