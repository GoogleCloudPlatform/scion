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
 * <scion-review-editor>: the graphical view of a Markdown review
 * (experiment web.artifact_review_editor). It shows the rendered document
 * with its CriticMarkup marks inline; typing, deleting and replacing are
 * recorded as suggestions, and Comment adds a note. The page owns the text
 * and the save: this element reports the text with review-change and
 * returns it from flush().
 *
 * Loaded lazily by the artifact page (review-editor-loader.ts), so the
 * editor stays out of every other page's download.
 *
 * Events:
 * - review-change {text}: the text after an edit (150 ms after typing
 *   pauses, and on blur);
 * - review-hint {kind}: an edit was refused (policy.ts HintKind);
 * - review-ready: the document is shown;
 * - review-load-failed: the document cannot be shown here.
 */

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, query, state } from 'lit/decorators.js';

import type { ArtifactFile } from '../../client/artifacts.js';
import { ReviewController } from './editor/controller.js';
import type { NoteRequest } from './editor/controller.js';
import type { HintKind } from './editor/policy.js';
import { commentNumber } from './editor/review.js';
import { reviewEditorStyles } from './review-editor-styles.js';
import './review-note-dialog.js';
import type { ScionReviewNoteDialog } from './review-note-dialog.js';

/** How long review-change waits after typing pauses. */
export const REVIEW_CHANGE_DELAY_MS = 150;

interface CardState {
  anchor: Element;
  title: string;
  note: string;
}

@customElement('scion-review-editor')
export class ScionReviewEditor extends LitElement {
  /** The review text to open (Markdown with CriticMarkup, LF line ends). */
  @property({ type: String }) content = '';
  @property({ type: String }) artifactId = '';
  /** Version whose files links and images resolve in. */
  @property({ type: Number }) seq = 0;
  @property({ type: String }) entryPath = '';
  @property({ attribute: false }) files: ArtifactFile[] = [];
  /** Shown as the author of comments already in the text. */
  @property({ type: String }) noteAuthor = '';

  @state() private card: CardState | null = null;

  @query('.editor-root') private root?: HTMLElement;
  @query('scion-review-note-dialog') private dialog?: ScionReviewNoteDialog;

  private controller: ReviewController | null = null;
  /** The text last loaded or reported, to tell outside changes from echoes. */
  private known = '';
  private changeTimer: ReturnType<typeof setTimeout> | null = null;
  private starting: Promise<void> | null = null;

  static override styles = [
    reviewEditorStyles,
    css`
      :host {
        display: block;
      }
    `,
  ];

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.clearTimer();
    void this.controller?.destroy();
    this.controller = null;
    this.starting = null;
  }

  override firstUpdated(): void {
    this.starting = this.start();
  }

  override updated(changed: PropertyValues<this>): void {
    if (changed.has('content') && this.controller && this.content !== this.known) {
      this.open(this.content);
    }
  }

  private async start(): Promise<void> {
    if (!this.root) return;
    try {
      this.controller = await ReviewController.create({
        root: this.root,
        render: () => ({
          id: this.artifactId,
          seq: this.seq,
          entryPath: this.entryPath,
          files: this.files,
        }),
        askNote: (req) => this.askNote(req),
        onHint: (kind) => this.emit('review-hint', { kind }),
        onChange: () => this.scheduleChange(),
      });
    } catch (err) {
      console.error('review editor: could not start', err);
      this.emit('review-load-failed', {});
      return;
    }
    if (!this.isConnected) {
      // Removed while the editor was starting (a quick toggle): nothing uses it.
      void this.controller.destroy();
      this.controller = null;
      return;
    }
    const view = this.controller.view;
    view.dom.setAttribute('role', 'textbox');
    view.dom.setAttribute('aria-multiline', 'true');
    view.dom.setAttribute('aria-label', `Review editor: ${this.entryPath.split('/').pop() ?? ''}`);
    view.dom.setAttribute('aria-describedby', 'mode-note');
    view.dom.addEventListener('blur', () => this.flushChange());
    view.dom.addEventListener('mouseover', this.onPointer);
    view.dom.addEventListener('focusin', this.onPointer);
    view.dom.addEventListener('mouseleave', this.hideCard);
    view.dom.addEventListener('focusout', this.hideCard);
    this.open(this.content);
  }

  private open(text: string): void {
    if (!this.controller) return;
    this.clearTimer();
    this.known = text;
    if (this.controller.load(text)) {
      this.emit('review-ready', {});
    } else {
      this.emit('review-load-failed', {});
    }
  }

  /** Resolves once the editor has opened its document (or failed to). */
  get ready(): Promise<void> {
    return this.starting ?? Promise.resolve();
  }

  /** Whether the reviewer has made a suggestion or comment here since the text was opened. */
  hasOwnMarks(): boolean {
    return this.controller?.hasOwnMarks() ?? false;
  }

  /** Whether an input method composition is still running (flush() gives null until it ends). */
  isComposing(): boolean {
    return this.controller?.isComposing ?? false;
  }

  /** The current text; serializes only after an edit. Null if it cannot be written. */
  flush(): string | null {
    if (!this.controller) return this.content;
    this.clearTimer();
    const text = this.controller.flush();
    if (text !== null) this.known = text;
    return text;
  }

  /** Comment on the selection (toolbar button). */
  async comment(): Promise<void> {
    await this.controller?.comment();
  }

  /** Moves keyboard focus into the document. */
  focusEditor(): void {
    this.controller?.focus();
  }

  private emit(name: string, detail: object): void {
    this.dispatchEvent(new CustomEvent(name, { detail, bubbles: true, composed: true }));
  }

  private clearTimer(): void {
    if (this.changeTimer) clearTimeout(this.changeTimer);
    this.changeTimer = null;
  }

  private scheduleChange(): void {
    this.clearTimer();
    this.changeTimer = setTimeout(() => this.flushChange(), REVIEW_CHANGE_DELAY_MS);
  }

  private flushChange(): void {
    if (!this.controller?.dirty) return;
    const text = this.flush();
    if (text !== null) this.emit('review-change', { text });
  }

  private askNote(req: NoteRequest): Promise<string | null> {
    this.card = null;
    return this.dialog ? this.dialog.ask(req) : Promise.resolve(null);
  }

  /** Shows the comment card for a comment badge under the pointer or focus. */
  private onPointer = (e: Event): void => {
    const el = (e.target as Element | null)?.closest?.('.critic-comment');
    if (!el || !this.controller) {
      // The card shows only while a comment badge is hovered or focused.
      if (this.card) this.card = null;
      return;
    }
    if (this.card?.anchor === el) return;
    const view = this.controller.view;
    let pos: number;
    try {
      pos = view.posAtDOM(el, 0);
    } catch {
      return;
    }
    const node = view.state.doc.nodeAt(pos);
    if (node?.type.name !== 'critic_comment') return;
    const number = commentNumber(view.state, pos);
    const who = node.attrs.mine ? 'You' : this.noteAuthor;
    this.card = {
      anchor: el,
      title: who ? `Comment ${number} · ${who}` : `Comment ${number}`,
      note: String(node.attrs.note),
    };
  };

  private hideCard = (): void => {
    this.card = null;
  };

  override render(): TemplateResult {
    return html`
      <span id="mode-note" class="visually-hidden"
        >Suggest mode: your edits are recorded as suggestions.</span
      >
      <div class="editor-root"></div>
      ${this.card
        ? html`<sl-popup
            active
            placement="bottom-start"
            distance="6"
            .anchor=${this.card.anchor}
            role="tooltip"
          >
            <div class="card">
              <div class="card-title">
                <sl-icon name="chat-text" aria-hidden="true"></sl-icon>${this.card.title}
              </div>
              <div class="card-note">${this.card.note}</div>
            </div>
          </sl-popup>`
        : nothing}
      <scion-review-note-dialog></scion-review-note-dialog>
    `;
  }
}

export type { HintKind };

declare global {
  interface HTMLElementTagNameMap {
    'scion-review-editor': ScionReviewEditor;
  }
}
