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
 * - Marks may span lines.
 * - Markdown code is literal text: a token inside an inline code span or a
 *   fenced code block (see criticCodeRanges) is not a token. A mark may
 *   contain code; code never contains a mark.
 *
 * Parsing is one forward pass for code and one for marks: each
 * closing-token search keeps a cursor that only moves forward and
 * remembers a failed search, and steps over code with its own
 * forward-only index into the code ranges, so the work is linear in the
 * input.
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

/** The tokens the Review toolbar writes. */
const TOK = {
  insOpen: '{++',
  insClose: '++}',
  delOpen: '{--',
  delClose: '--}',
  subOpen: '{~~',
  subClose: '~~}',
  noteOpen: '{>>',
  noteClose: '<<}',
  hlOpen: '{==',
  hlClose: '==}',
} as const;

/**
 * The opening token a toolbar action puts before a non-empty selection,
 * which moves the selected text (and any code in it) right by its length.
 * Insert adds after the selection and moves nothing.
 */
const WRAP_OPEN: Record<CriticTool, string> = {
  comment: TOK.hlOpen,
  suggest: TOK.subOpen,
  delete: TOK.delOpen,
  insert: '',
};

/** A half-open range [start, end) of a text. */
export interface CriticRange {
  start: number;
  end: number;
}

/** A run of backticks in a paragraph; escaped when its first is escaped. */
interface BacktickRun {
  start: number;
  n: number;
  escaped: boolean;
}

/**
 * The Markdown code in src, in order and not overlapping: fenced code
 * blocks and inline code spans, with CommonMark's rules for those two
 * constructs, as pkg/artifacts/critic does.
 *
 * - Lines end at LF, CRLF or a lone CR.
 * - A fence is a line of up to three spaces, then at least three backticks
 *   or at least three tildes. A backtick fence's info string holds no
 *   backtick. The block ends at a line of up to three spaces, a run of the
 *   same character at least as long as the opening one, and only spaces
 *   or tabs after it; with no such line it runs to the end of the text.
 *   The range covers the opening line through the closing line's
 *   terminator.
 * - A code span opens at a run of n backticks and closes at the next run of
 *   exactly n backticks. A run with no partner is literal. A backslash
 *   escapes the first backtick of a run that would open (the run is one
 *   shorter); it does not escape a closing run. Spans pair up within a run
 *   of consecutive non-blank lines that are not fence lines, so they may
 *   cross line ends but not a blank line or a fence.
 *
 * Indented code blocks, block quotes and list items are not recognised,
 * nor the precedence of HTML tags and autolinks over code spans. Other
 * block boundaries, such as headings and table rows, do not end a run of
 * lines.
 */
export function criticCodeRanges(src: string, steps: { n: number } = { n: 0 }): CriticRange[] {
  const out: CriticRange[] = [];
  let runs: BacktickRun[] = [];
  const n = src.length;
  let fenceOpen = -1;
  let fenceChar = '';
  let fenceLen = 0;
  const flush = (): void => {
    matchRuns(runs, out, steps);
    runs = [];
  };
  for (let ls = 0; ls < n; ) {
    let le = ls;
    while (le < n && src[le] !== '\n' && src[le] !== '\r') le++;
    const next = le < n && src[le] === '\r' && src[le + 1] === '\n' ? le + 2 : Math.min(le + 1, n);
    steps.n += next - ls;
    const line = src.slice(ls, le);
    const f = fenceRun(line);
    if (fenceOpen >= 0) {
      if (closesFence(line, fenceChar, fenceLen)) {
        out.push({ start: fenceOpen, end: next });
        fenceOpen = -1;
      }
    } else if (f && (f.c === '~' || !f.rest.includes('`'))) {
      flush();
      fenceOpen = ls;
      fenceChar = f.c;
      fenceLen = f.len;
    } else if (isBlank(line)) {
      flush();
    } else {
      for (let i = ls; i < le; ) {
        if (src[i] !== '`') {
          i++;
          continue;
        }
        let j = i;
        while (j < le && src[j] === '`') j++;
        let bs = 0;
        for (let k = i - 1; k >= ls && src[k] === '\\'; k--) bs++;
        steps.n += bs;
        runs.push({ start: i, n: j - i, escaped: bs % 2 === 1 });
        i = j;
      }
    }
    ls = next;
  }
  if (fenceOpen >= 0) {
    out.push({ start: fenceOpen, end: n });
    return out;
  }
  flush();
  return out;
}

/** The fence a line opens with, after up to three spaces, or null. */
function fenceRun(line: string): { c: string; len: number; rest: string } | null {
  let i = 0;
  while (i < line.length && i < 3 && line[i] === ' ') i++;
  const c = line[i];
  if (c !== '`' && c !== '~') return null;
  let j = i;
  while (j < line.length && line[j] === c) j++;
  return j - i < 3 ? null : { c, len: j - i, rest: line.slice(j) };
}

/**
 * Reports whether line closes a fence opened with a run of len c
 * characters: up to three spaces, a run of c at least len long, and only
 * spaces or tabs after it.
 */
function closesFence(line: string, c: string, len: number): boolean {
  const f = fenceRun(line);
  return f !== null && f.c === c && f.len >= len && isBlank(f.rest);
}

function isBlank(s: string): boolean {
  return /^[ \t]*$/.test(s);
}

/**
 * Pairs a paragraph's backtick runs into code spans. byLen lists the runs
 * of each length in order and ptr the first entry not yet passed; the
 * pointers only move forward.
 */
function matchRuns(runs: BacktickRun[], out: CriticRange[], steps: { n: number }): void {
  if (runs.length < 2) return;
  const byLen = new Map<number, number[]>();
  runs.forEach((r, i) => {
    const l = byLen.get(r.n);
    if (l) l.push(i);
    else byLen.set(r.n, [i]);
  });
  const ptr = new Map<number, number>();
  for (let i = 0; i < runs.length; ) {
    steps.n++;
    const r = runs[i];
    const start = r.escaped ? r.start + 1 : r.start;
    const n = r.escaped ? r.n - 1 : r.n;
    const list = n > 0 ? byLen.get(n) : undefined;
    if (!list) {
      i++;
      continue;
    }
    let k = ptr.get(n) ?? 0;
    while (k < list.length && list[k] <= i) {
      k++;
      steps.n++;
    }
    ptr.set(n, k);
    if (k === list.length) {
      i++;
      continue;
    }
    const j = list[k];
    out.push({ start, end: runs[j].start + n });
    i = j + 1;
  }
}

/** Answers whether non-decreasing positions lie in code; moves forward. */
class CodeIndex {
  private i = 0;
  constructor(private readonly code: CriticRange[]) {}

  /** The code range containing p, or null. */
  at(p: number, steps: { n: number }): CriticRange | null {
    while (this.i < this.code.length && this.code[this.i].end <= p) {
      this.i++;
      steps.n++;
    }
    const r = this.code[this.i];
    return r && r.start <= p ? r : null;
  }
}

/**
 * Finds a fixed token outside code at or after a position; see the file
 * comment. A code range starts at a backtick or a line start and ends
 * after a backtick or a line terminator, and no token holds either, so an
 * occurrence that starts outside code lies wholly outside it.
 */
class Cursor {
  private found = -1;
  private exhausted = false;
  private readonly code: CodeIndex;
  constructor(
    private readonly tok: string,
    code: CriticRange[]
  ) {
    this.code = new CodeIndex(code);
  }

  next(src: string, from: number, steps: { n: number }): number {
    if (this.found >= from) return this.found;
    for (;;) {
      if (this.exhausted || from >= src.length) return -1;
      const i = src.indexOf(this.tok, from);
      if (i < 0) {
        steps.n += src.length - from;
        this.exhausted = true;
        return -1;
      }
      steps.n += i - from + this.tok.length;
      const r = this.code.at(i, steps);
      if (r) {
        from = r.end;
        continue;
      }
      this.found = i;
      return i;
    }
  }
}

/** Parses src; with steps, also counts the characters examined. */
export function parseCritic(src: string, steps: { n: number } = { n: 0 }): CriticSegment[] {
  const code = criticCodeRanges(src, steps);
  const cursors = MARKS.map((m) => new Cursor(m.close, code));
  const sep = new Cursor(SUB_SEP, code);
  const open = new CodeIndex(code);
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
    const inCode = open.at(i, steps);
    if (inCode) {
      i = inCode.end;
      continue;
    }
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
        ? edit(from, to, TOK.noteOpen, 'comment', TOK.noteClose)
        : edit(
            from,
            to,
            WRAP_OPEN.comment + text + TOK.hlClose + TOK.noteOpen,
            'comment',
            TOK.noteClose
          );
    case 'suggest':
      return text === ''
        ? edit(from, to, TOK.subOpen + SUB_SEP, 'replacement', TOK.subClose)
        : edit(from, to, WRAP_OPEN.suggest + text + SUB_SEP, text, TOK.subClose);
    case 'insert':
      return edit(to, to, TOK.insOpen, 'text', TOK.insClose);
    case 'delete': {
      if (text === '') return null;
      const insert = WRAP_OPEN.delete + text + TOK.delClose;
      return { from, to, insert, selectFrom: from + insert.length, selectTo: from + insert.length };
    }
  }
}

/** The CriticMarkup tokens a toolbar selection may not contain outside code. */
const MARK_TOKENS = [...Object.values(TOK), SUB_SEP];

/**
 * Explains why a toolbar action cannot apply to a selection of doc, or
 * returns null when it can. Marks do not nest, so the selection (or the
 * insertion point) must not touch an existing mark, and the selected text
 * must not contain mark tokens outside code; otherwise the result would
 * read as different marks than intended. Markdown code is literal text, so
 * the selection must not cut into code, and the new mark must not change
 * which text is code.
 */
export function criticToolBlocked(
  tool: CriticTool,
  sel: { from: number; to: number; text: string },
  doc: string
): string | null {
  if (tool === 'delete' && sel.text === '') return 'Select the text to delete.';
  const code = criticCodeRanges(doc);
  const inCode = (p: number): boolean => code.some((r) => r.start <= p && p < r.end);
  for (const t of MARK_TOKENS) {
    for (let k = sel.text.indexOf(t); k >= 0; k = sel.text.indexOf(t, k + 1)) {
      if (!inCode(sel.from + k)) return 'The selection contains CriticMarkup. Select plain text.';
    }
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
  // Fenced code blocks: a mark token on a fence line, or inside the block,
  // would change where the block starts or ends, or put the token into
  // code. The toolbar refuses any boundary that touches the block, from
  // the start of its opening line to the end of its closing line (to the
  // end of the text for a block with no closing fence). The line before
  // and the line after are fine.
  for (const r of code) {
    if (!isFenceStart(doc, r.start)) continue;
    const last = fenceLast(doc, r);
    if (at.from <= last && at.to >= r.start) return FENCE_HINT;
  }
  // A boundary strictly inside a code span would put mark tokens into
  // code, where they are literal text. Whole code spans may be selected.
  const cuts = (p: number): boolean => code.some((r) => r.start < p && p < r.end);
  if (cuts(at.from) || cuts(at.to)) return INSIDE_CODE_HINT;
  // The new mark must read as intended in the whole document. Apply the
  // edit, then require:
  // (1) that the code of the result is the code of doc moved by the edit.
  //     New tokens can change code: a closing token between a backslash
  //     and the backtick it escaped makes a code span appear, and tildes
  //     the tool adds after tildes at a line start can open a fence. Code
  //     does not depend on marks, so this check runs first and tells a
  //     change in code apart from an unclosed opener;
  // (2) that the text with every mark rejected is unchanged; and
  // (3) that the result parses with exactly the inserted mark(s) over the
  //     inserted text.
  // An unclosed opener could otherwise pair with the new tokens: one
  // earlier in the text can be closed by the new closer (the clean text
  // changes, and the mark no longer starts at the edit), and one directly
  // before the selection can become the new mark's opener (the mark then
  // starts before the edit). Marks elsewhere need no check: the selection
  // does not touch a mark and the tool inserts a balanced mark, so a mark
  // outside the edit could only change by pairing with the new tokens,
  // which (3) refuses.
  const edit = criticToolEdit(tool, sel);
  if (!edit) return null;
  const result = doc.slice(0, edit.from) + edit.insert + doc.slice(edit.to);
  if (!sameCode(code, criticCodeRanges(result), edit, tool)) return CODE_STRUCTURE_HINT;
  if (!onlyMarksChanged(result, projectCritic(doc, 'clean'))) return UNCLOSED_MARK_HINT;
  if (!marksAsIntended(result, edit, tool === 'comment' && sel.text !== '' ? 2 : 1)) {
    return UNCLOSED_MARK_HINT;
  }
  return null;
}

/**
 * Reports whether after, the code ranges of the edited text, are before,
 * the code ranges of the text the edit applied to, moved by the edit.
 * Code before the edit stays; code after it moves by the edit's change in
 * length; code inside the edited range (a whole code span the selection
 * held) moves by the opening token the tool wraps the selection in
 * (WRAP_OPEN), and a suggestion copies it into the new side after the old
 * side and SUB_SEP. The checks before this one keep code ranges from
 * crossing the edit's ends.
 */
function sameCode(
  before: CriticRange[],
  after: CriticRange[],
  edit: CriticEdit,
  tool: CriticTool
): boolean {
  const len = edit.to - edit.from;
  const delta = edit.insert.length - len;
  const open = WRAP_OPEN[tool].length;
  const copy = open + len + SUB_SEP.length;
  const moved: CriticRange[] = [];
  const copies: CriticRange[] = [];
  for (const r of before) {
    if (r.end <= edit.from) moved.push(r);
    else if (r.start >= edit.to) moved.push({ start: r.start + delta, end: r.end + delta });
    else {
      moved.push({ start: r.start + open, end: r.end + open });
      if (tool === 'suggest') copies.push({ start: r.start + copy, end: r.end + copy });
    }
  }
  const want = [...moved, ...copies].sort((x, y) => x.start - y.start);
  const key = (rs: CriticRange[]) => rs.map((r) => `${r.start}-${r.end}`).join(',');
  return key(want) === key(after);
}

/**
 * Reports whether a code range starting at start is a fenced code block:
 * it starts a line, and that line opens a fence. (An inline code span that
 * starts a line never has a fence-opening first line, or the line would
 * open a fence instead.)
 */
function isFenceStart(doc: string, start: number): boolean {
  if (start > 0 && doc[start - 1] !== '\n' && doc[start - 1] !== '\r') return false;
  let end = start;
  while (end < doc.length && doc[end] !== '\n' && doc[end] !== '\r') end++;
  const f = fenceRun(doc.slice(start, end));
  return f !== null && (f.c === '~' || !f.rest.includes('`'));
}

/**
 * The last offset at which a toolbar boundary would touch the fenced
 * block r: the end of its closing line, before the line terminator; or
 * the end of the text when the block has no closing fence, since text
 * added there would join the block.
 */
function fenceLast(doc: string, r: CriticRange): number {
  const text = doc.slice(r.start, r.end);
  const term = /(\r\n|\r|\n)$/.exec(text)?.[0] ?? '';
  const lines = text.slice(0, text.length - term.length).split(/\r\n|\r|\n/);
  const open = fenceRun(lines[0]);
  const closed =
    open !== null && lines.length > 1 && closesFence(lines[lines.length - 1], open.c, open.len);
  return closed ? r.end - term.length : r.end;
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
const INSIDE_CODE_HINT =
  'The selection is inside or across code. Text in code is literal, so a mark there would change it. Select a whole code span, or mark beside the code.';
const FENCE_HINT =
  'The selection touches a fenced code block. The toolbar does not mark fenced blocks: type the marks in the editor, or comment on the text beside the block.';
const CODE_STRUCTURE_HINT = 'This mark would change which text is code. Place it elsewhere.';
const UNCLOSED_MARK_HINT =
  'An unclosed CriticMarkup opener earlier in the text would swallow this mark. Remove or close it first.';
