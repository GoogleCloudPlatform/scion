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
 * The review editor without its UI: the Milkdown core, the suggest-mode
 * plugins (policy, tracking, paste, shortcuts) and the session. The Lit
 * element (review-editor.ts) supplies the note dialog and shows hints.
 */

import { keymap } from '@milkdown/prose/keymap';
import { Fragment } from '@milkdown/prose/model';
import type { Node, Slice } from '@milkdown/prose/model';
import { Transform } from '@milkdown/prose/transform';
import { NodeSelection, Plugin, PluginKey, TextSelection } from '@milkdown/prose/state';
import type { EditorState, Transaction } from '@milkdown/prose/state';
import type { EditorView } from '@milkdown/prose/view';

import { artifactLinkTarget } from '../../../client/artifact-preview.js';
import { createCore } from './create.js';
import type { Core } from './create.js';
import { reviewKeymap } from './keymap.js';
import { guardTransfers, pastePlugin } from './paste.js';
import { containerClass, policyPlugin, touchesEarlier } from './policy.js';
import type { HintKind } from './policy.js';
import { addComment, commentNumber, editComment, planComment, selectedComment } from './review.js';
import type { RenderContext } from './safe-render.js';
import { COMMENT, CRITIC_MARK_NAMES } from './schema.js';
import { ReviewSession } from './session.js';
import { SKIP_TRACK, reconcileComposition, snapshotComposition, trackPlugin } from './track.js';
import type { CompositionSnapshot } from './track.js';

/** What the note dialog is asked to show. */
export interface NoteRequest {
  /** Existing note when editing, '' for a new comment. */
  note: string;
  /** The comment's number when editing, 0 for a new one. */
  number: number;
  /** Selected text for a new comment that highlights it, or '' . */
  quote: string;
  /** Text describing where a comment without highlight goes. */
  context: string;
}

/** The note dialog's answer: a note, or null when cancelled. */
export type AskNote = (req: NoteRequest) => Promise<string | null>;

export interface ControllerOptions {
  root: HTMLElement;
  render: () => RenderContext;
  askNote: AskNote;
  onHint: (hint: HintKind) => void;
  /** Called after each change of the document by the reviewer. */
  onChange: () => void;
}

/** A heading's fragment id, as GitHub writes it: lower case, punctuation dropped, spaces as hyphens. */
export function headingSlug(text: string): string {
  return text
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\s_-]/gu, '')
    .replace(/\s/g, '-');
}

/** Moves the cursor to the heading whose fragment id is id; false if none. */
export function scrollToHeading(view: EditorView, id: string): boolean {
  let want: string;
  try {
    want = decodeURIComponent(id).toLowerCase();
  } catch {
    return false;
  }
  let at = -1;
  view.state.doc.descendants((n, pos) => {
    if (at >= 0) return false;
    if (n.type.name === 'heading' && headingSlug(n.textContent) === want) at = pos + 1;
    return n.isBlock && !n.isTextblock;
  });
  if (at < 0) return false;
  view.dispatch(
    view.state.tr.setSelection(TextSelection.create(view.state.doc, at)).scrollIntoView()
  );
  view.focus();
  return true;
}

export class ReviewController {
  private core!: Core;
  private session!: ReviewSession;
  private loading = false;
  private readonly uiKey = new PluginKey<unknown>('criticUi');
  private composing: CompositionSnapshot | null = null;
  private ended: CompositionSnapshot | null = null;
  /** Whether the document had changed since load when each snapshot was taken. */
  private dirtyAt = new WeakMap<CompositionSnapshot, boolean>();
  private endedTimer: ReturnType<typeof setTimeout> | null = null;
  private unguard: () => void = () => {};

  private constructor(private readonly opts: ControllerOptions) {}

  static async create(opts: ControllerOptions): Promise<ReviewController> {
    const c = new ReviewController(opts);
    await c.init();
    return c;
  }

  get view(): EditorView {
    return this.core.view;
  }

  private async init(): Promise<void> {
    const opts = this.opts;
    const onRefuse = (hint: HintKind): void => opts.onHint(hint);
    const unitCheck = (doc: Node, first: number, last: number): HintKind | null =>
      this.session.unitCheck(doc, first, last);
    const keys = reviewKeymap(onRefuse);
    let parse: (md: string) => Node = () => {
      throw new Error('editor not ready');
    };
    this.core = await createCore({
      root: opts.root,
      render: opts.render,
      plugins: () => [
        pastePlugin((md) => parse(md), onRefuse),
        policyPlugin(onRefuse, unitCheck, (block) => this.session.earlierHint(block)),
        trackPlugin(),
        this.uiPlugin(),
        keymap({
          ...keys,
          'Mod-Alt-m': () => {
            void this.comment();
            return true;
          },
          'Ctrl-Alt-m': () => {
            void this.comment();
            return true;
          },
          Enter: (state, dispatch, view) => {
            const c = selectedComment(state);
            if (!c) return keys.Enter(state, dispatch, view);
            void this.editNote(c.pos);
            return true;
          },
        }),
      ],
    });
    parse = this.core.parse;
    this.session = new ReviewSession(this.core);
    this.unguard = guardTransfers(opts.root, () => this.core.view, this.core.parse, onRefuse);
  }

  private uiPlugin(): Plugin {
    return new Plugin({
      key: this.uiKey,
      props: {
        // Shortcuts act on the selection the reviewer sees, even if the
        // browser has not reported its latest move yet.
        handleKeyDown: (view, event): boolean => {
          // Enter on a comment badge reached with Tab edits that comment.
          const badge = (event.target as Element | null)?.closest?.('.critic-comment');
          if (
            event.key === 'Enter' &&
            !event.shiftKey &&
            !event.ctrlKey &&
            !event.metaKey &&
            !event.altKey &&
            badge &&
            view.dom.contains(badge)
          ) {
            const pos = this.badgePos(badge);
            if (pos !== null) {
              event.preventDefault();
              void this.editNote(pos);
              return true;
            }
          }
          if (!view.composing && event.keyCode !== 229) this.syncSelection();
          // Comment by the key's position (KeyM), so layouts where
          // Ctrl+Alt (AltGr) + M types a character still comment.
          if ((event.ctrlKey || event.metaKey) && event.altKey && event.code === 'KeyM') {
            event.preventDefault();
            void this.comment();
            return true;
          }
          return false;
        },
        // A copy puts the editor's own Markdown of the selection on the
        // clipboard (marks of the review left out), so a paste, which reads
        // text as Markdown, gets back what was copied.
        clipboardTextSerializer: (slice): string => this.sliceMarkdown(slice),
        handleDoubleClickOn: (_view, pos, node): boolean => {
          if (node.type.name !== COMMENT) return false;
          void this.editNote(pos);
          return true;
        },
        handleClick: (view, pos, event): boolean => {
          if (!(event.metaKey || event.ctrlKey)) return false;
          const target = event.target as Element | null;
          const a = target?.closest?.('a');
          if (!a) return false;
          const href = a.getAttribute('href');
          if (href) {
            window.open(href, '_blank', 'noopener,noreferrer');
            return true;
          }
          if (a.classList.contains('fragment-link')) {
            const link = view.state.doc
              .resolve(pos)
              .marks()
              .find((m) => m.type.name === 'link');
            const t = link ? artifactLinkTarget(String(link.attrs.href), this.opts.render()) : null;
            if (t?.kind === 'fragment') return scrollToHeading(view, t.id);
          }
          return false;
        },
        handleDOMEvents: {
          compositionstart: (): boolean => {
            this.beginComposition();
            return false;
          },
          compositionend: (): boolean => {
            this.endComposition();
            return false;
          },
        },
      },
      // A composition whose start the editor did not see (focus entering
      // mid-composition, some mobile keyboards) is snapshotted from the
      // state before its first transaction.
      filterTransaction: (tr, state): boolean => {
        if (
          tr.getMeta('composition') !== undefined &&
          !tr.getMeta(SKIP_TRACK) &&
          !this.composing &&
          !this.ended
        ) {
          this.composing = this.snapshot(state);
        }
        return true;
      },
      state: {
        init: (): unknown => undefined,
        // The id of the latest composition transaction, for endComposition.
        apply: (tr, value): unknown => tr.getMeta('composition') ?? value,
      },
      view: () => ({
        update: (view, prev): void => {
          if (this.loading || view.state.doc.eq(prev.doc)) return;
          this.session.markDirty();
          // Text being composed is not tracked until the composition
          // settles; the settle step's own change is reported then.
          if (view.composing || this.composing || this.ended) return;
          this.opts.onChange();
        },
      }),
    });
  }

  /** The document position of a comment badge element, or null. */
  private badgePos(el: Element): number | null {
    const view = this.core.view;
    let pos: number;
    try {
      pos = view.posAtDOM(el, 0);
    } catch {
      return null;
    }
    for (const p of [pos, pos - 1]) {
      if (p >= 0 && view.state.doc.nodeAt(p)?.type.name === COMMENT) return p;
    }
    return null;
  }

  /** The Markdown of a slice, without review marks or comments. */
  sliceMarkdown(slice: Slice): string {
    const schema = this.core.view.state.schema;
    let content = slice.content;
    let inline = true;
    content.forEach((n) => {
      if (!n.isInline) inline = false;
    });
    if (inline) content = Fragment.from(schema.nodes.paragraph.create(null, content));
    const doc = schema.topNodeType.createAndFill(null, content);
    if (!doc) return slice.content.textBetween(0, slice.content.size, '\n\n');
    const tr = new Transform(doc);
    for (const name of CRITIC_MARK_NAMES) tr.removeMark(0, tr.doc.content.size, schema.marks[name]);
    const comments: number[] = [];
    tr.doc.descendants((n, pos) => {
      if (n.type.name === COMMENT) comments.push(pos);
      return true;
    });
    for (const pos of comments.reverse()) tr.delete(pos, pos + 1);
    return this.core.serialize(tr.doc).replace(/\n+$/, '');
  }

  /**
   * Snapshots the document as a composition starts. A composition that
   * ended but is not settled yet (its settle step is deferred one tick) is
   * settled first, so back-to-back compositions (Korean syllables, some
   * Android keyboards) each get their own snapshot.
   */
  beginComposition(): void {
    this.settleComposition();
    // Still composing in the view (settle kept the older snapshot): that
    // snapshot covers this composition too.
    this.composing ??= this.snapshot(this.core.view.state);
  }

  /**
   * At compositionend: takes the snapshot now and settles it one tick
   * later, after ProseMirror has applied the composition's last change.
   */
  endComposition(): void {
    const snap = this.composing;
    this.composing = null;
    if (!snap) return;
    this.settleComposition();
    this.ended = snap;
    this.endedTimer = setTimeout(() => {
      this.endedTimer = null;
      this.settleComposition();
    }, 0);
  }

  /**
   * Records the ended composition as an insertion, or restores the
   * document from before it (with a hint) when it cannot be recorded.
   * Never runs while a composition is in progress.
   */
  settleComposition(): void {
    if (this.endedTimer) clearTimeout(this.endedTimer);
    this.endedTimer = null;
    const snap = this.ended;
    this.ended = null;
    if (!snap) return;
    const view = this.core.view;
    if (view.composing) {
      // A new composition is running in the same block: keep the oldest
      // snapshot, so its end settles both.
      this.composing = snap;
      return;
    }
    const docBefore = view.state.doc;
    const id: unknown = this.uiKey.getState(view.state);
    const refused = reconcileComposition(view, snap, id, (doc, pos, at, before) => {
      // Composed text in a list item or a table cell is an insertion like
      // any other; code takes no suggestions.
      if (containerClass(view.state.doc.resolve(pos + 1)) === 'code') return 'code';
      // The same hint as for typing at that place.
      if (touchesEarlier(before, at, at))
        return this.session.earlierHint(before.resolve(at).index(0));
      const i = doc.resolve(pos).index(0);
      return this.session.unitCheck(doc, i, i);
    });
    if (refused) this.opts.onHint(refused as HintKind);
    if (view.state.doc.eq(snap.doc)) {
      // The document has the content it had before the composition (it
      // changed nothing, or it was restored): it is as edited as it was
      // then. Compared by content, since a restore builds a new document.
      this.session.setDirty(this.dirtyAt.get(snap) ?? true);
      if (view.state.doc === docBefore) this.opts.onChange();
    }
  }

  /** A composition snapshot, remembering whether the document was edited then. */
  private snapshot(state: EditorState): CompositionSnapshot {
    const snap = snapshotComposition(state);
    this.dirtyAt.set(snap, this.session.isDirty);
    return snap;
  }

  /**
   * Opens md; false when it cannot be shown in this editor. Every mark of
   * the opened text is earlier (design E6): a mark is the reviewer's own
   * only if it is made in this editor after this load. A composition still
   * pending from the previous text is dropped.
   */
  load(md: string): boolean {
    if (this.endedTimer) clearTimeout(this.endedTimer);
    this.endedTimer = null;
    this.composing = null;
    this.ended = null;
    this.loading = true;
    try {
      return this.session.load(md);
    } finally {
      this.loading = false;
    }
  }

  /**
   * The current text, serializing only after an edit; null when it cannot
   * be written faithfully (every unit written must read back as the
   * document and keep every earlier mark, design 4.6 item 3 and E5). A
   * composition that has ended but whose settle step is still pending is
   * settled first, so Save or a switch to Source right after it keeps the
   * composed text as a suggestion; while a composition runs, the text
   * cannot be written (null).
   */
  flush(): string | null {
    if (this.ended && !this.core.view.composing) this.settleComposition();
    return this.session.save();
  }

  /** Whether a mark or comment made in this editor since the last load is in the document. */
  hasOwnMarks(): boolean {
    return this.session.hasOwnMarks();
  }

  /** Top-level blocks that are read-only and saved from their source. */
  readOnlyBlocks(): number[] {
    return this.session.readOnlyBlocks();
  }

  get dirty(): boolean {
    return this.session.isDirty;
  }

  /** Whether the opened text gets the whole-text check on every edit (design E7). */
  get isHazardous(): boolean {
    return this.session.isHazardous;
  }

  /** Whether an input method composition is running in the view. */
  get isComposing(): boolean {
    return this.core.view.composing || this.composing !== null;
  }

  /**
   * Takes the browser's selection into the editor state when it moved
   * without the editor seeing it yet (a shortcut pressed right after a
   * caret move, before the browser reported the selection change).
   */
  private syncSelection(): void {
    const view = this.core.view;
    const root = view.root as Document | (ShadowRoot & { getSelection?: () => Selection | null });
    const sel = root.getSelection?.() ?? document.getSelection();
    if (!sel?.anchorNode || !sel.focusNode) return;
    if (!view.dom.contains(sel.anchorNode) || !view.dom.contains(sel.focusNode)) return;
    let anchor: number;
    let head: number;
    try {
      anchor = view.posAtDOM(sel.anchorNode, sel.anchorOffset);
      head = view.posAtDOM(sel.focusNode, sel.focusOffset);
    } catch {
      return;
    }
    const cur = view.state.selection;
    if (cur.anchor === anchor && cur.head === head) return;
    if (cur instanceof NodeSelection && sel.isCollapsed) return;
    try {
      view.dispatch(view.state.tr.setSelection(TextSelection.create(view.state.doc, anchor, head)));
    } catch {
      // Not a text position; keep the editor's own selection.
    }
  }

  /** Comment on the selection (toolbar button and shortcut). */
  async comment(): Promise<void> {
    const view = this.core.view;
    this.syncSelection();
    const plan = planComment(view.state);
    if ('refuse' in plan) {
      this.opts.onHint(plan.refuse);
      return;
    }
    const context = plan.highlight ? '' : this.commentContext(plan.at);
    const docBefore = view.state.doc;
    const note = await this.opts.askNote({ note: '', number: 0, quote: plan.quote, context });
    view.focus();
    if (note === null || note.trim() === '') return;
    // The comment goes where it was planned; if the document changed while
    // the dialog was open, that place may have moved.
    if (view.state.doc !== docBefore) {
      this.opts.onHint('generic');
      return;
    }
    this.dispatchChecked(addComment(view.state, plan, note), plan.at);
  }

  /**
   * Edit the note of this session's comment at pos (Enter, double-click).
   * Comments already in the text are not edited here.
   */
  async editNote(pos: number): Promise<void> {
    const view = this.core.view;
    const node = view.state.doc.nodeAt(pos);
    if (node?.type.name !== COMMENT) return;
    if (node.attrs.mine !== true) {
      // Earlier notes are read-only in this version (design E5, S-b).
      this.opts.onHint(this.session.earlierHint(view.state.doc.resolve(pos).index(0)));
      return;
    }
    const docBefore = view.state.doc;
    const number = commentNumber(view.state, pos);
    const note = await this.opts.askNote({
      note: String(node.attrs.note),
      number,
      quote: '',
      context: this.commentContext(pos),
    });
    view.focus();
    if (note === null) return;
    // The document may have changed while the dialog was open: the comment
    // must still be at pos, unchanged.
    if (view.state.doc !== docBefore && view.state.doc.nodeAt(pos) !== node) {
      this.opts.onHint('generic');
      return;
    }
    const tr = editComment(view.state, pos, note);
    if (tr) this.dispatchChecked(tr, pos);
  }

  /**
   * Dispatches a comment command unless its save would change text outside
   * marks or misread the marks (the same check as for typed edits); then
   * it shows the generic hint instead.
   */
  private dispatchChecked(tr: Transaction, pos: number): void {
    const view = this.core.view;
    const i = view.state.doc.resolve(Math.min(pos, view.state.doc.content.size)).index(0);
    const refused = this.session.unitCheck(tr.doc, i, i);
    if (refused) {
      this.opts.onHint(refused);
      return;
    }
    view.dispatch(tr);
  }

  /** "After “…text.”": where a comment without highlight sits. */
  private commentContext(pos: number): string {
    const $p = this.core.view.state.doc.resolve(pos);
    const before = $p.parent.textBetween(0, $p.parentOffset, ' ', ' ').trim();
    if (!before) return '';
    const tail = before.length > 40 ? `…${before.slice(-40)}` : before;
    return `After “${tail}”`;
  }

  focus(): void {
    this.core.view.focus();
  }

  async destroy(): Promise<void> {
    if (this.endedTimer) clearTimeout(this.endedTimer);
    this.unguard();
    await this.core.destroy();
  }
}
