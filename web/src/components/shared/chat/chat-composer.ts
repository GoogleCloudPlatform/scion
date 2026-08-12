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
 * Chat composer component.
 *
 * Textarea with:
 * - Character counter (rune-aware via Intl.Segmenter where available)
 * - 2000 character limit with visual feedback (AC10)
 * - Defaults to plain: false (design section 4.7)
 * - Sends via `chat-send` custom event: {text, plain, interrupt}
 * - The composer knows nothing about the network
 * - Send on Enter (Shift+Enter for newline)
 * - Interrupt toggle
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

/** Maximum message length in rune count. */
const MAX_MESSAGE_LENGTH = 2000;

/** Event detail for the chat-send custom event. */
export interface ChatSendDetail {
  text: string;
  plain: boolean;
  interrupt: boolean;
  onSuccess: () => void;
}

/**
 * Count "runes" (user-perceived characters) in a string.
 * Uses Intl.Segmenter where available, falls back to spread length.
 */
function countRunes(text: string): number {
  if (typeof Intl !== 'undefined' && 'Segmenter' in Intl) {
    const segmenter = new Intl.Segmenter('en', { granularity: 'grapheme' });
    let count = 0;
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    for (const _ of segmenter.segment(text)) count++;
    return count;
  }
  // Fallback: spread into an array (handles surrogate pairs but not all grapheme clusters)
  return [...text].length;
}

@customElement('scion-chat-composer')
export class ScionChatComposer extends LitElement {
  /** Whether the send button should be disabled (e.g. while sending). */
  @property({ type: Boolean })
  disabled = false;

  @state() private text = '';
  @state() private plain = false;
  @state() private interrupt = false;
  @state() private runeCount = 0;

  static override styles = css`
    :host {
      display: block;
    }

    .composer {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      padding: 0.75rem 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    .input-row {
      display: flex;
      align-items: flex-end;
      gap: 0.5rem;
    }

    .textarea-wrapper {
      flex: 1;
      position: relative;
    }

    sl-textarea::part(base) {
      font-size: 0.875rem;
      border-radius: 0.75rem;
    }

    sl-textarea::part(textarea) {
      resize: none;
    }

    .send-btn {
      flex-shrink: 0;
    }

    .footer-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }

    .options {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .options label {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      white-space: nowrap;
    }

    .char-counter {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .char-counter.warn {
      color: var(--scion-warning-600, #d97706);
    }

    .char-counter.over {
      color: var(--scion-danger-600, #dc2626);
      font-weight: 600;
    }
  `;

  override render() {
    const isOverLimit = this.runeCount > MAX_MESSAGE_LENGTH;
    const isNearLimit = this.runeCount > MAX_MESSAGE_LENGTH * 0.9;
    const canSend = this.text.trim().length > 0 && !isOverLimit && !this.disabled;

    const counterClass = isOverLimit ? 'over' : isNearLimit ? 'warn' : '';

    return html`
      <div class="composer">
        <div class="input-row">
          <div class="textarea-wrapper">
            <sl-textarea
              placeholder="Send a message..."
              size="small"
              rows="1"
              resize="auto"
              .value=${this.text}
              @sl-input=${this.handleInput}
              @keydown=${this.handleKeydown}
              ?disabled=${this.disabled}
            ></sl-textarea>
          </div>
          <sl-button
            class="send-btn"
            size="small"
            variant="primary"
            ?disabled=${!canSend}
            @click=${this.handleSend}
          >
            <sl-icon slot="prefix" name="send"></sl-icon>
            Send
          </sl-button>
        </div>
        <div class="footer-row">
          <div class="options">
            <label>
              <sl-checkbox
                size="small"
                ?checked=${this.plain}
                @sl-change=${this.handlePlainToggle}
              ></sl-checkbox>
              Plain
            </label>
            <label>
              <sl-checkbox
                size="small"
                ?checked=${this.interrupt}
                @sl-change=${this.handleInterruptToggle}
              ></sl-checkbox>
              Interrupt
            </label>
          </div>
          ${this.runeCount > 0 || isNearLimit
            ? html`
                <span class="char-counter ${counterClass}">
                  ${this.runeCount} / ${MAX_MESSAGE_LENGTH}
                </span>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private handleInput(e: Event): void {
    const target = e.target as HTMLInputElement;
    this.text = target.value;
    this.runeCount = countRunes(this.text);
  }

  private handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      this.handleSend();
    }
  }

  private handlePlainToggle(e: Event): void {
    this.plain = (e.target as HTMLInputElement).checked;
  }

  private handleInterruptToggle(e: Event): void {
    this.interrupt = (e.target as HTMLInputElement).checked;
  }

  private handleSend(): void {
    const trimmed = this.text.trim();
    if (!trimmed || this.runeCount > MAX_MESSAGE_LENGTH || this.disabled) return;

    this.dispatchEvent(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: trimmed,
          plain: this.plain,
          interrupt: this.interrupt,
          onSuccess: () => {
            this.text = '';
            this.runeCount = 0;
          },
        },
        bubbles: true,
        composed: true,
      })
    );
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-composer': ScionChatComposer;
  }
}
