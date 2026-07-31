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
 * Quick-message dialog component.
 *
 * A modal dialog for sending a quick message to an agent. Opened by message
 * buttons on the agent detail page, agent list view, and graph/tree view.
 * Always sends as formatted (plain: false), no urgent toggle.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../client/api.js';

@customElement('scion-quick-message-dialog')
export class ScionQuickMessageDialog extends LitElement {
  /** The agent ID to send the message to. */
  @property({ type: String }) agentId = '';

  /** The agent name, used in the dialog title. */
  @property({ type: String }) agentName = '';

  /** Whether the dialog is open. */
  @property({ type: Boolean, reflect: true }) open = false;

  @state() private messageText = '';
  @state() private sending = false;
  @state() private sendError: string | null = null;

  static styles = css`
    .dialog-body {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    sl-textarea::part(textarea) {
      min-height: 100px;
      resize: vertical;
    }

    .dialog-error {
      color: var(--sl-color-danger-600);
      font-size: var(--sl-font-size-small);
    }
  `;

  protected updated(changed: Map<string, unknown>): void {
    if (changed.has('open') && this.open) {
      // Reset state when opening
      this.messageText = '';
      this.sendError = null;
      this.sending = false;
      // Auto-focus the textarea after the dialog animation completes
      this.updateComplete.then(() => {
        const textarea = this.shadowRoot?.querySelector('sl-textarea');
        if (textarea) {
          textarea.focus();
        }
      });
    }
  }

  private close(): void {
    this.open = false;
    this.dispatchEvent(new CustomEvent('sl-request-close'));
  }

  private async handleSend(): Promise<void> {
    const text = this.messageText.trim();
    if (!text || this.sending) return;

    this.sending = true;
    this.sendError = null;

    try {
      const url = `/api/v1/agents/${this.agentId}/message`;
      const body = {
        structured_message: { msg: text, plain: false },
        interrupt: false,
      };

      const res = await apiFetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        this.sendError = await extractApiError(res, 'Failed to send message');
        return;
      }

      // Success — close dialog
      this.close();
    } catch (err) {
      this.sendError =
        err instanceof Error ? err.message : 'Failed to send message';
    } finally {
      this.sending = false;
    }
  }

  private handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      void this.handleSend();
    }
  }

  render() {
    const label = this.agentName
      ? `Message ${this.agentName}`
      : 'Send Message';

    return html`
      <sl-dialog
        label=${label}
        ?open=${this.open}
        @sl-request-close=${this.close}
      >
        <div class="dialog-body">
          <sl-textarea
            placeholder="Type your message…"
            rows="4"
            .value=${this.messageText}
            @sl-input=${(e: Event) => {
              this.messageText = (e.target as HTMLInputElement).value;
            }}
            @keydown=${this.handleKeydown}
            ?disabled=${this.sending}
          ></sl-textarea>
          ${this.sendError
            ? html`<div class="dialog-error">${this.sendError}</div>`
            : nothing}
        </div>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.close}
          ?disabled=${this.sending}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.sending}
          ?disabled=${this.sending || !this.messageText.trim()}
          @click=${() => void this.handleSend()}
        >
          Send
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-quick-message-dialog': ScionQuickMessageDialog;
  }
}
