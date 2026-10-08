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
 * CriticMarkup parsing and projections, the browser twin of
 * pkg/artifacts/critic. The two follow the same rules and are checked
 * against one corpus (pkg/artifacts/critic/testdata/corpus.json):
 *
 * - Marks do not nest: a mark ends at the first closing token of its kind.
 * - An opening token with no closing token of its kind, or a substitution
 *   with no "~>" before its closing token, is literal text.
 * - The first "~>" separates a substitution's sides; either may be empty.
 * - Marks may span lines; code spans and fences are not special.
 *
 * Parsing is one forward pass: each closing-token search keeps a cursor
 * that only moves forward and remembers a failed search.
 */

export type CriticKind =
  | 'text'
  | 'insertion'
  | 'deletion'
  | 'substitution'
  | 'comment'
  | 'highlight';

/** One piece of parsed text. start/end are offsets of the whole segment. */
export interface CriticSegment {
  kind: CriticKind;
  /** Unmarked text, the mark's content, or a substitution's old side. */
  text: string;
  /** A substitution's new side. */
  next?: string;
  start: number;
  end: number;
}

export type CriticMode = 'raw' | 'clean' | 'accept';

const MARKS: { kind: CriticKind; open: string; close: string }[] = [
  { kind: 'insertion', open: '+', close: '++}' },
  { kind: 'deletion', open: '-', close: '--}' },
  { kind: 'substitution', open: '~', close: '~~}' },
  { kind: 'comment', open: '>', close: '<<}' },
  { kind: 'highlight', open: '=', close: '==}' },
];

const SUB_SEP = '~>';

/** Finds a fixed token at or after a position; see the file comment. */
class Cursor {
  private found = -1;
  private exhausted = false;
  constructor(private readonly tok: string) {}

  next(src: string, from: number, steps: { n: number }): number {
    if (this.found >= from) return this.found;
    if (this.exhausted || from >= src.length) return -1;
    const i = src.indexOf(this.tok, from);
    if (i < 0) {
      steps.n += src.length - from;
      this.exhausted = true;
      return -1;
    }
    steps.n += i - from + this.tok.length;
    this.found = i;
    return i;
  }
}

/** Parses src; with steps, also counts the characters examined. */
export function parseCritic(src: string, steps: { n: number } = { n: 0 }): CriticSegment[] {
  const cursors = MARKS.map((m) => new Cursor(m.close));
  const sep = new Cursor(SUB_SEP);
  const segs: CriticSegment[] = [];
  let textStart = 0;
  const flush = (end: number): void => {
    if (end > textStart) {
      segs.push({ kind: 'text', text: src.slice(textStart, end), start: textStart, end });
    }
  };
  let i = 0;
  while (i < src.length) {
    const j = src.indexOf('{', i);
    if (j < 0) {
      steps.n += src.length - i;
      break;
    }
    steps.n += j - i + 1;
    i = j;
    let slot = -1;
    if (i + 2 < src.length && src[i + 1] === src[i + 2]) {
      slot = MARKS.findIndex((m) => m.open === src[i + 1]);
    }
    if (slot < 0) {
      i++;
      continue;
    }
    const body = i + 3;
    const end = cursors[slot].next(src, body, steps);
    if (end < 0) {
      i++;
      continue;
    }
    const kind = MARKS[slot].kind;
    const seg: CriticSegment = { kind, text: '', start: i, end: end + 3 };
    if (kind === 'substitution') {
      const s = sep.next(src, body, steps);
      if (s < 0 || s + SUB_SEP.length > end) {
        i++;
        continue;
      }
      seg.text = src.slice(body, s);
      seg.next = src.slice(s + SUB_SEP.length, end);
    } else {
      seg.text = src.slice(body, end);
    }
    flush(i);
    segs.push(seg);
    i = seg.end;
    textStart = i;
  }
  flush(src.length);
  return segs;
}

/** Applies a projection: clean rejects every mark, accept accepts them. */
export function projectCritic(src: string, mode: CriticMode): string {
  if (mode === 'raw') return src;
  let out = '';
  for (const s of parseCritic(src)) {
    switch (s.kind) {
      case 'text':
      case 'highlight':
        out += s.text;
        break;
      case 'insertion':
        if (mode === 'accept') out += s.text;
        break;
      case 'deletion':
        if (mode === 'clean') out += s.text;
        break;
      case 'substitution':
        out += mode === 'clean' ? s.text : (s.next ?? '');
        break;
      case 'comment':
        break;
    }
  }
  return out;
}

/** Counts of a text's marks, for summaries. */
export interface CriticCounts {
  /** Insertions, deletions and substitutions. */
  suggestions: number;
  comments: number;
  highlights: number;
}

export function countCritic(src: string): CriticCounts {
  const c: CriticCounts = { suggestions: 0, comments: 0, highlights: 0 };
  for (const s of parseCritic(src)) {
    if (s.kind === 'insertion' || s.kind === 'deletion' || s.kind === 'substitution') {
      c.suggestions++;
    } else if (s.kind === 'comment') {
      c.comments++;
    } else if (s.kind === 'highlight') {
      c.highlights++;
    }
  }
  return c;
}

/**
 * The comparison form of review checks, as the hub computes it: Unicode
 * NFC with CRLF and lone CR line endings read as LF.
 */
export function normalizeCritic(src: string): string {
  return src.replace(/\r\n?/g, '\n').normalize('NFC');
}

/**
 * Reports whether review changes nothing outside marks relative to
 * baseline (the base version's text with its own marks rejected), the
 * check the hub makes when a review is saved.
 */
export function onlyMarksChanged(review: string, baseline: string): boolean {
  return normalizeCritic(projectCritic(review, 'clean')) === normalizeCritic(baseline);
}

/** Private-use characters that stand for mark boundaries while rendering. */
export const CRITIC_SENTINELS = {
  insOpen: '\uE000',
  insClose: '\uE001',
  delOpen: '\uE002',
  delClose: '\uE003',
  hlOpen: '\uE004',
  hlClose: '\uE005',
  noteOpen: '\uE006',
  noteClose: '\uE007',
} as const;

const SENTINEL_RE = /[\uE000-\uE007]/g;

/**
 * Replaces every mark with sentinel-delimited content, for a markdown
 * renderer to carry through; renderCriticSentinels turns them into
 * elements afterwards. Sentinel characters already in src are replaced by
 * U+FFFD first, so text cannot pose as a mark.
 */
export function criticToSentinels(src: string): string {
  const S = CRITIC_SENTINELS;
  const clean = src.replace(SENTINEL_RE, '\uFFFD');
  let out = '';
  for (const s of parseCritic(clean)) {
    switch (s.kind) {
      case 'text':
        out += s.text;
        break;
      case 'insertion':
        out += S.insOpen + s.text + S.insClose;
        break;
      case 'deletion':
        out += S.delOpen + s.text + S.delClose;
        break;
      case 'substitution':
        out += S.delOpen + s.text + S.delClose + S.insOpen + (s.next ?? '') + S.insClose;
        break;
      case 'highlight':
        out += S.hlOpen + s.text + S.hlClose;
        break;
      case 'comment':
        out += S.noteOpen + s.text + S.noteClose;
        break;
    }
  }
  return out;
}

/**
 * Turns the sentinels of rendered HTML into elements: insertions as <ins>,
 * deletions as <del>, highlights as <mark>, and each comment as a numbered
 * reference followed by its note. Attribute values are unquoted so that a
 * sentinel inside an attribute cannot end it. The result must be
 * sanitized afterwards.
 */
export function renderCriticSentinels(html: string): string {
  let n = 0;
  return html.replace(SENTINEL_RE, (ch) => {
    switch (ch) {
      case CRITIC_SENTINELS.insOpen:
        return '<ins class=critic-ins>';
      case CRITIC_SENTINELS.insClose:
        return '</ins>';
      case CRITIC_SENTINELS.delOpen:
        return '<del class=critic-del>';
      case CRITIC_SENTINELS.delClose:
        return '</del>';
      case CRITIC_SENTINELS.hlOpen:
        return '<mark class=critic-hl>';
      case CRITIC_SENTINELS.hlClose:
        return '</mark>';
      case CRITIC_SENTINELS.noteOpen:
        n++;
        return `<sup class=critic-ref>${n}</sup><span class=critic-note><span class=critic-note-n>${n}</span> `;
      default:
        return '</span>';
    }
  });
}

/** The Review toolbar's actions. */
export type CriticTool = 'comment' | 'suggest' | 'insert' | 'delete';

/** An edit to apply to the editor: replace [from, to) and select a range. */
export interface CriticEdit {
  from: number;
  to: number;
  insert: string;
  /** Selection after the edit, as offsets into the new content. */
  selectFrom: number;
  selectTo: number;
}

/**
 * The edit a toolbar action makes for a selection. The placeholder text it
 * inserts (the comment, the replacement, the insertion) is left selected
 * so typing replaces it. Delete needs a selection; it returns null without
 * one.
 *
 * - Comment: {>>comment<<} at the cursor, or {==selection==}{>>comment<<}.
 * - Suggest: {~~selection~>selection~~} with the new side selected, or
 *   {~~~>replacement~~} at the cursor.
 * - Insert: {++text++} after the selection, which stays as it is.
 * - Delete: {--selection--}.
 */
export function criticToolEdit(
  tool: CriticTool,
  sel: { from: number; to: number; text: string }
): CriticEdit | null {
  const { from, to, text } = sel;
  const edit = (
    at: number,
    end: number,
    before: string,
    placeholder: string,
    after: string
  ): CriticEdit => ({
    from: at,
    to: end,
    insert: before + placeholder + after,
    selectFrom: at + before.length,
    selectTo: at + before.length + placeholder.length,
  });
  switch (tool) {
    case 'comment':
      return text === ''
        ? edit(from, to, '{>>', 'comment', '<<}')
        : edit(from, to, `{==${text}==}{>>`, 'comment', '<<}');
    case 'suggest':
      return text === ''
        ? edit(from, to, '{~~~>', 'replacement', '~~}')
        : edit(from, to, `{~~${text}~>`, text, '~~}');
    case 'insert':
      return edit(to, to, '{++', 'text', '++}');
    case 'delete': {
      if (text === '') return null;
      const insert = `{--${text}--}`;
      return { from, to, insert, selectFrom: from + insert.length, selectTo: from + insert.length };
    }
  }
}
