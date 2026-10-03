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

import { LitElement, css, html, nothing } from 'lit';
import type { PropertyValues, TemplateResult } from 'lit';
import { customElement, property, query } from 'lit/decorators.js';

/** One row of an action sheet. */
export interface ActionSheetItem {
  id: string;
  label: string;
  icon?: string;
  destructive?: boolean;
  disabled?: boolean;
}

/** `detail` of the `action-sheet-select` event. */
export interface ActionSheetSelectDetail {
  id: string;
}

/**
 * A bottom action sheet: the touch presentation of the chat's row menus.
 *
 * It is a native `<dialog>` opened with `showModal()`, so it sits in the top
 * layer above the transformed, clipped mobile panels, with Esc, a focus trap
 * and an inert background for free. It is full width, anchored to the
 * bottom, with 48px rows and a separate Cancel row.
 *
 * Touch events are stopped at the dialog: a horizontal drag across the sheet
 * must not reach the chat page's panel-swipe handlers.
 *
 * Open it with `open = true` (or `show()`). It emits:
 * - `action-sheet-select` with `{ id }` when an enabled item is chosen. The
 *   sheet then closes itself.
 * - `action-sheet-close` every time it closes, whatever the cause (an item,
 *   Cancel, a backdrop tap, Esc, or the host setting `open = false`).
 */
@customElement('scion-action-sheet')
export class ScionActionSheet extends LitElement {
  @property({ attribute: false }) items: ActionSheetItem[] = [];

  /** Shown above the items; also the dialog's accessible name. */
  @property() heading = '';

  @property({ type: Boolean, reflect: true }) open = false;

  @query('dialog') private dialog!: HTMLDialogElement;

  static override styles = css`
    :host {
      display: contents;
    }

    dialog {
      position: fixed;
      inset: auto 0 0 0;
      margin: 0;
      width: 100%;
      max-width: 100%;
      max-height: 70vh;
      max-height: 70dvh;
      box-sizing: border-box;
      /* Clear the home indicator, and in landscape the notch and rounded
         corners (the page uses viewport-fit=cover). The bottom inset stays
         even with the keyboard up: opening the modal sheet moves focus
         into it, which closes the keyboard. */
      padding: 0 env(safe-area-inset-right, 0px) env(safe-area-inset-bottom, 0px)
        env(safe-area-inset-left, 0px);
      border: none;
      border-radius: 0.875rem 0.875rem 0 0;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
      box-shadow: 0 -4px 24px rgba(0, 0, 0, 0.18);
      overflow: hidden;
      flex-direction: column;
      -webkit-tap-highlight-color: transparent;
      -webkit-touch-callout: none;
      -webkit-user-select: none;
      user-select: none;
      /* Only vertical pans: a sideways drag on the sheet must not become a
         browser gesture either (Chromium turns an unconsumed horizontal
         overscroll into history navigation, which tears the page down). */
      touch-action: pan-y pinch-zoom;
    }

    dialog[open] {
      display: flex;
      animation: sheet-in 0.18s ease-out;
    }

    dialog::backdrop {
      background: rgba(15, 23, 42, 0.45);
    }

    @keyframes sheet-in {
      from {
        transform: translateY(100%);
      }
      to {
        transform: translateY(0);
      }
    }

    @media (prefers-reduced-motion: reduce) {
      dialog[open] {
        animation: none;
      }
    }

    .heading {
      flex: none;
      padding: 0.75rem 1rem 0.5rem;
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .items {
      flex: 1 1 auto;
      min-height: 0;
      overflow-y: auto;
      overscroll-behavior: contain;
      /* Restated: touch-action does not carry across a scroll container. */
      touch-action: pan-y pinch-zoom;
      padding: 0.25rem 0;
    }

    .item,
    .cancel {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      box-sizing: border-box;
      width: 100%;
      min-height: 48px;
      padding: 0.5rem 1rem;
      border: none;
      background: none;
      color: inherit;
      font: inherit;
      font-size: max(16px, var(--chat-fs-md, 1rem));
      line-height: 1.3;
      text-align: left;
      cursor: pointer;
    }

    .item sl-icon {
      flex: none;
      font-size: 1.25rem;
      color: var(--scion-text-muted, #64748b);
    }

    .item .label {
      flex: 1 1 auto;
      min-width: 0;
      overflow-wrap: anywhere;
    }

    .item.destructive,
    .item.destructive sl-icon {
      color: var(--scion-danger-600, #dc2626);
    }

    .item:disabled {
      opacity: 0.4;
      cursor: default;
    }

    .item:not(:disabled):active,
    .cancel:active {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .item:focus-visible,
    .cancel:focus-visible {
      outline: 2px solid var(--scion-primary-500, #3b82f6);
      outline-offset: -2px;
    }

    .cancel {
      flex: none;
      justify-content: center;
      font-weight: 600;
      border-top: 0.5rem solid var(--scion-bg-subtle, #f1f5f9);
      min-height: calc(48px + 0.5rem);
    }
  `;

  /** Open the sheet. */
  show(): void {
    this.open = true;
  }

  /** Close the sheet without choosing an item. */
  close(): void {
    this.open = false;
  }

  override updated(changed: PropertyValues<this>): void {
    super.updated(changed);
    if (!changed.has('open') || !this.dialog) return;
    if (this.open && !this.dialog.open) {
      this.dialog.showModal();
      // Start every opening at the top of the list.
      const list = this.renderRoot.querySelector('.items');
      if (list) list.scrollTop = 0;
    } else if (!this.open && this.dialog.open) {
      this.dialog.close();
    }
  }

  override render(): TemplateResult {
    return html`
      <dialog
        aria-label=${this.heading || 'Actions'}
        @close=${this.handleDialogClose}
        @click=${this.handleDialogClick}
        @touchstart=${this.stopTouch}
        @touchmove=${this.stopTouch}
        @touchend=${this.stopTouch}
        @touchcancel=${this.stopTouch}
      >
        ${this.heading ? html`<div class="heading">${this.heading}</div>` : nothing}
        <div class="items">
          ${this.items.map(
            (item): TemplateResult => html`
              <button
                type="button"
                class="item ${item.destructive ? 'destructive' : ''}"
                data-id=${item.id}
                ?disabled=${item.disabled === true}
                @click=${(): void => this.select(item)}
              >
                ${item.icon ? html`<sl-icon name=${item.icon}></sl-icon>` : nothing}
                <span class="label">${item.label}</span>
              </button>
            `
          )}
        </div>
        <button type="button" class="cancel" @click=${(): void => this.close()}>Cancel</button>
      </dialog>
    `;
  }

  private select(item: ActionSheetItem): void {
    if (item.disabled) return;
    this.dispatchEvent(
      new CustomEvent<ActionSheetSelectDetail>('action-sheet-select', {
        detail: { id: item.id },
        bubbles: true,
        composed: true,
      })
    );
    this.close();
  }

  /** The dialog closed, by Esc or by `close()`: keep `open` in step and tell the host. */
  private handleDialogClose = (): void => {
    this.open = false;
    this.dispatchEvent(new CustomEvent('action-sheet-close', { bubbles: true, composed: true }));
  };

  /**
   * A click on the dialog element itself that lies outside its box landed on
   * the backdrop. (A click on the dialog's own bottom padding also targets
   * the dialog, hence the rect check.) Clicks inside the sheet stay inside:
   * hosts close their menus on any outside document click, and a tap on the
   * heading or a disabled row must not count as one.
   */
  private handleDialogClick = (e: MouseEvent): void => {
    e.stopPropagation();
    if (e.target !== this.dialog) return;
    const r = this.dialog.getBoundingClientRect();
    const inside =
      e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
    if (!inside) this.close();
  };

  /**
   * Keep touches on the sheet from reaching the panel-swipe handlers.
   * Passive, because nothing here calls preventDefault. Lit hands a listener
   * object to addEventListener as the options argument as well, so
   * `passive` here is applied to the registration.
   */
  private stopTouch = {
    handleEvent: (e: TouchEvent): void => e.stopPropagation(),
    passive: true,
  };
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-action-sheet': ScionActionSheet;
  }
}
