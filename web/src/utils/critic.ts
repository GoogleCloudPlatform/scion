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

/** Escapes text for HTML element content. */
function escapeHtml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/**
 * Turns the sentinels of rendered HTML into elements: insertions as <ins>,
 * deletions as <del>, highlights as <mark>, and each comment as a numbered
 * reference followed by its note, headed "n · author" when author is set.
 * A sentinel inside a tag (in an attribute such as a link's href or an
 * image's alt, raw or percent-encoded) is dropped, so marks never turn into markup inside an
 * attribute; the renderer escapes "<" and ">" in text and attribute
 * values, so "<" always starts a tag. Attribute values are unquoted. The
 * result must be sanitized afterwards.
 */
export function renderCriticSentinels(html: string, author = ''): string {
  const who = author ? ` · ${escapeHtml(author)}` : '';
  // A URL attribute carries sentinels percent-encoded; drop those too.
  html = html.replace(/<[^>]*>/g, (tag) => tag.replace(/%EE%80%8[0-7]/gi, ''));
  let n = 0;
  let out = '';
  let inTag = false;
  for (const ch of html) {
    if (ch === '<') inTag = true;
    else if (ch === '>') inTag = false;
    const code = ch.charCodeAt(0);
    if (code < 0xe000 || code > 0xe007) {
      out += ch;
      continue;
    }
    if (inTag) continue;
    switch (ch) {
      case CRITIC_SENTINELS.insOpen:
        out += '<ins class=critic-ins>';
        break;
      case CRITIC_SENTINELS.insClose:
        out += '</ins>';
        break;
      case CRITIC_SENTINELS.delOpen:
        out += '<del class=critic-del>';
        break;
      case CRITIC_SENTINELS.delClose:
        out += '</del>';
        break;
      case CRITIC_SENTINELS.hlOpen:
        out += '<mark class=critic-hl>';
        break;
      case CRITIC_SENTINELS.hlClose:
        out += '</mark>';
        break;
      case CRITIC_SENTINELS.noteOpen:
        n++;
        out += `<sup class=critic-ref>${n}</sup><span class=critic-note><span class=critic-note-n>${n}${who}</span> `;
        break;
      default:
        out += '</span>';
    }
  }
  return out;
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

/** The CriticMarkup tokens a toolbar selection may not contain. */
const MARK_TOKENS = ['{++', '++}', '{--', '--}', '{~~', '~>', '~~}', '{>>', '<<}', '{==', '==}'];

/**
 * Explains why a toolbar action cannot apply to a selection of doc, or
 * returns null when it can. Marks do not nest, so the selection (or the
 * insertion point) must not touch an existing mark, and the selected text
 * must not contain mark tokens; otherwise the result would read as
 * different marks than intended.
 */
export function criticToolBlocked(
  tool: CriticTool,
  sel: { from: number; to: number; text: string },
  doc: string
): string | null {
  if (tool === 'delete' && sel.text === '') return 'Select the text to delete.';
  if (MARK_TOKENS.some((t) => sel.text.includes(t))) {
    return 'The selection contains CriticMarkup. Select plain text.';
  }
  const at = tool === 'insert' ? { from: sel.to, to: sel.to } : sel;
  for (const seg of parseCritic(doc)) {
    if (seg.kind === 'text') continue;
    const empty = at.from === at.to;
    const touches = empty
      ? at.from > seg.start && at.from < seg.end
      : at.from < seg.end && at.to > seg.start;
    if (touches) return INSIDE_MARK_HINT;
  }
  // The new mark must read as intended in the whole document. Apply the
  // edit, then require (1) that the text with every mark rejected is
  // unchanged, and (2) that the result parses with exactly the inserted
  // mark(s) over the inserted text. An unclosed opener could otherwise pair
  // with the new tokens: one earlier in the text can be closed by the new
  // closer (the clean text changes, and the mark no longer starts at the
  // edit), and one directly before the selection can become the new
  // mark's opener (the mark then starts before the edit). Marks elsewhere
  // need no check: the selection does not touch a mark and the tool
  // inserts a balanced mark, so a mark outside the edit could only change
  // by pairing with the new tokens, which (2) refuses.
  const edit = criticToolEdit(tool, sel);
  if (!edit) return null;
  const result = doc.slice(0, edit.from) + edit.insert + doc.slice(edit.to);
  if (!onlyMarksChanged(result, projectCritic(doc, 'clean'))) return UNCLOSED_MARK_HINT;
  if (!marksAsIntended(result, edit, tool === 'comment' && sel.text !== '' ? 2 : 1)) {
    return UNCLOSED_MARK_HINT;
  }
  return null;
}

/**
 * Reports whether result (doc with edit applied) holds exactly `inserted`
 * marks overlapping the inserted text, tiling it from its first to its
 * last character.
 */
function marksAsIntended(result: string, edit: CriticEdit, inserted: number): boolean {
  const start = edit.from;
  const end = edit.from + edit.insert.length;
  const fresh = parseCritic(result).filter(
    (x) => x.kind !== 'text' && x.start < end && x.end > start
  );
  if (fresh.length !== inserted) return false;
  if (fresh[0].start !== start || fresh[fresh.length - 1].end !== end) return false;
  for (let i = 1; i < fresh.length; i++) {
    if (fresh[i].start !== fresh[i - 1].end) return false;
  }
  return true;
}

const INSIDE_MARK_HINT = 'The selection is inside or across a mark. Select text outside marks.';
const UNCLOSED_MARK_HINT =
  'An unclosed CriticMarkup opener earlier in the text would swallow this mark. Remove or close it first.';
