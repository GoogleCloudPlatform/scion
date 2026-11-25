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
 * The note dialog of the review editor: "Add comment" for a new comment,
 * "Edit comment N" for an existing one. Enter saves, Shift+Enter adds a
 * line, Esc cancels. Saving an empty note deletes the comment.
 */

import { LitElement, css, html, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, query, state } from 'lit/decorators.js';

import type { NoteRequest } from './editor/controller.js';

@customElement('scion-review-note-dialog')
export class ScionReviewNoteDialog extends LitElement {
  @state() private req: NoteRequest | null = null;
  @state() private open = false;
  @state() private value = '';

  @query('textarea') private textarea?: HTMLTextAreaElement;

  private resolve: ((note: string | null) => void) | null = null;
  private trackingViewport = false;

  static override styles = css`
    sl-dialog::part(panel) {
      width: min(32rem, calc(100vw - 1.5rem));
    }
    .quote {
      border-left: 3px solid var(--scion-review-hl-line, #ca8a04);
      background: var(--scion-review-hl-bg, #fef08a);
      color: var(--scion-review-hl-text, #0f172a);
      padding: 0.3rem 0.55rem;
      border-radius: 0 var(--scion-radius-sm) var(--scion-radius-sm) 0;
      margin-bottom: 0.6rem;
      font-size: 0.8125rem;
      overflow-wrap: anywhere;
    }
    .context {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      margin-bottom: 0.6rem;
    }
    textarea {
      box-sizing: border-box;
      width: 100%;
      min-height: 5.5rem;
      font: inherit;
      font-size: 0.875rem;
      padding: 0.45rem 0.6rem;
      border: 1px solid var(--sl-input-border-color, #cbd5e1);
      border-radius: var(--sl-input-border-radius-medium, 0.3rem);
      background: var(--sl-input-background-color, #fff);
      color: var(--sl-input-color, #0f172a);
      resize: vertical;
    }
    /* 16px or more on a touch screen: iOS Safari zooms in on a field
       under 16px when it takes focus and keeps the zoom after the keyboard
       closes, leaving the pinned review toolbar above the visible area
       (viewport.ts stands down while the page is zoomed). */
    @media (pointer: coarse) {
      textarea {
        font-size: max(16px, 0.875rem);
      }
    }
    /* On phones and touch screens, size and position the dialog against the
       visual viewport (tracking window.visualViewport height and offsetTop
       while open, with 100dvh as fallback) so the on-screen keyboard on
       iOS Safari and Android Chrome never covers the textarea, hint row,
       or action buttons. Desktop layout is unaffected. */
    @media (max-width: 768px), (pointer: coarse) {
      sl-dialog {
        --header-spacing: 0.875rem 1rem;
        --body-spacing: 0.75rem 1rem;
        --footer-spacing: 0.75rem 1rem;
      }
      sl-dialog::part(base) {
        top: var(--note-dialog-vv-top, 0px);
        bottom: auto;
        height: var(--note-dialog-vv-height, 100dvh);
        padding: max(env(safe-area-inset-top), 0.5rem) 0.75rem
          max(0.5rem, calc(env(safe-area-inset-bottom, 0px) * (1 - var(--scion-kb-open, 0))));
        box-sizing: border-box;
      }
      sl-dialog::part(panel) {
        max-width: 100%;
        max-height: 100%;
      }
      sl-dialog::part(body) {
        min-height: 0;
      }
    }
    textarea:focus {
      outline: none;
      border-color: var(--sl-color-primary-600, #0284c7);
      box-shadow: 0 0 0 3px var(--sl-focus-ring-color, rgba(2, 132, 199, 0.25));
    }
    .help {
      display: flex;
      flex-wrap: wrap;
      gap: 0.75rem;
      margin-top: 0.4rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }
    kbd {
      font-family: var(--scion-font-mono, ui-monospace, monospace);
      font-size: 0.6875rem;
      border: 1px solid var(--scion-border, #cbd5e1);
      border-bottom-width: 2px;
      border-radius: var(--scion-radius-sm);
      padding: 0 0.3rem;
    }
    .footer {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      width: 100%;
    }
    .footer .delete {
      margin-right: auto;
    }
    .footer .delete::part(base) {
      color: var(--sl-color-danger-600, #dc2626);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.open) {
      this.startVisualViewportTracking();
    }
  }

  override disconnectedCallback(): void {
    this.stopVisualViewportTracking();
    super.disconnectedCallback();
  }

  /** Opens the dialog for req; resolves with the note, or null when cancelled. */
  ask(req: NoteRequest): Promise<string | null> {
    this.finish(null);
    this.req = req;
    this.value = req.note;
    this.open = true;
    this.startVisualViewportTracking();
    return new Promise((resolve) => {
      this.resolve = resolve;
    });
  }

  private finish(note: string | null): void {
    const r = this.resolve;
    this.resolve = null;
    this.open = false;
    this.stopVisualViewportTracking();
    r?.(note);
  }

  private syncVisualViewport(): void {
    if (!this.open) return;
    const vv = window.visualViewport;
    if (!vv) return;
    this.style.setProperty('--note-dialog-vv-height', `${vv.height}px`);
    this.style.setProperty('--note-dialog-vv-top', `${vv.offsetTop}px`);
  }

  private scrollTextareaIntoView(): void {
    if (!this.open || !this.textarea) return;
    if (typeof this.textarea.scrollIntoView === 'function') {
      this.textarea.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    }
  }

  private readonly onVisualViewportChange = (): void => {
    if (!this.open) return;
    this.syncVisualViewport();
    if (this.shadowRoot?.activeElement === this.textarea) {
      this.scrollTextareaIntoView();
    }
  };

  private startVisualViewportTracking(): void {
    const vv = window.visualViewport;
    if (!vv) return;
    this.syncVisualViewport();
    if (this.trackingViewport) return;
    this.trackingViewport = true;
    vv.addEventListener('resize', this.onVisualViewportChange);
    vv.addEventListener('scroll', this.onVisualViewportChange);
  }

  private stopVisualViewportTracking(): void {
    const vv = window.visualViewport;
    if (vv && this.trackingViewport) {
      vv.removeEventListener('resize', this.onVisualViewportChange);
      vv.removeEventListener('scroll', this.onVisualViewportChange);
    }
    this.trackingViewport = false;
    this.style.removeProperty('--note-dialog-vv-height');
    this.style.removeProperty('--note-dialog-vv-top');
  }

  private onKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      this.finish(this.value);
    }
  }

  private onTextareaFocus = (): void => {
    if (!this.open) return;
    this.syncVisualViewport();
    this.scrollTextareaIntoView();
  };

  private focusTextarea(): void {
    if (!this.open || !this.textarea) return;
    if (this.shadowRoot?.activeElement !== this.textarea) {
      this.textarea.focus({ preventScroll: true });
      this.textarea.select();
    }
  }

  private onInitialFocus = (e: Event): void => {
    if (e.target !== e.currentTarget) return;
    e.preventDefault();
    this.focusTextarea();
  };

  private onAfterShow = (e: Event): void => {
    if (e.target !== e.currentTarget || !this.open) return;
    this.syncVisualViewport();
    this.focusTextarea();
  };

  private onAfterHide = (e: Event): void => {
    if (e.target !== e.currentTarget || this.open) return;
    this.stopVisualViewportTracking();
  };

  override render(): TemplateResult {
    const req = this.req;
    const editing = !!req && req.number > 0;
    const label = editing ? `Edit comment ${req.number}` : 'Add comment';
    return html`<sl-dialog
      label=${label}
      ?open=${this.open}
      @sl-initial-focus=${this.onInitialFocus}
      @sl-after-show=${this.onAfterShow}
      @sl-after-hide=${this.onAfterHide}
      @sl-request-close=${(): void => this.finish(null)}
    >
      ${req?.quote
        ? html`<div class="quote">${req.quote}</div>`
        : req?.context
          ? html`<div class="context">${req.context}</div>`
          : nothing}
      <textarea
        aria-label="Note"
        .value=${this.value}
        @focus=${this.onTextareaFocus}
        @input=${(e: Event): void => {
          this.value = (e.target as HTMLTextAreaElement).value;
        }}
        @keydown=${this.onKeydown}
      ></textarea>
      <div class="help">
        <span><kbd>Enter</kbd> saves</span><span><kbd>Shift+Enter</kbd> new line</span
        ><span><kbd>Esc</kbd> cancels</span>
      </div>
      ${editing
        ? html`<div class="help">Saving an empty note deletes the comment and its highlight.</div>`
        : nothing}
      <div slot="footer" class="footer">
        ${editing
          ? html`<sl-button
              class="delete"
              variant="text"
              size="small"
              @click=${(): void => this.finish('')}
            >
              <sl-icon slot="prefix" name="trash"></sl-icon>
              Delete comment
            </sl-button>`
          : nothing}
        <sl-button size="small" @click=${(): void => this.finish(null)}>Cancel</sl-button>
        <sl-button size="small" variant="primary" @click=${(): void => this.finish(this.value)}
          >${editing ? 'Save' : 'Add comment'}</sl-button
        >
      </div>
    </sl-dialog>`;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-review-note-dialog': ScionReviewNoteDialog;
  }
}
