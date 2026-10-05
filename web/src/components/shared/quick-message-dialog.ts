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
 *
 * When native chat is available, an "Open agent DM" footer button closes the
 * dialog and opens the agent's DM in chat, carrying any unsent text into
 * the DM composer's draft.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { getDenialMessage } from '../../shared/message-mode.js';
import { buildAgentDMKey, chatConversationPath } from '../../client/chat-routes.js';
import { seedChatDraft } from '../../client/chat-drafts.js';
import { stateManager } from '../../client/state.js';
import { isFeatureEnabled } from '../../utils/feature-flags.js';

@customElement('scion-quick-message-dialog')
export class ScionQuickMessageDialog extends LitElement {
  /** The agent ID to send the message to. */
  @property({ type: String }) agentId = '';

  /** The agent name, used in the dialog title. */
  @property({ type: String }) agentName = '';

  /** Optional project name for cross-project context (shown as "project / agent"). */
  @property({ type: String }) projectName = '';

  /**
   * The signed-in user's id, used to address the agent DM. Hosts that know
   * it should pass it so the "Open agent DM" button appears as soon as the
   * user loads; otherwise the app-wide current user is read on render.
   */
  @property({ type: String }) userId = '';

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

    .dialog-footer {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: var(--sl-spacing-x-small);
    }

    .dialog-footer-actions {
      display: flex;
      gap: var(--sl-spacing-x-small);
      margin-inline-start: auto;
    }

    .open-dm sl-icon {
      font-size: 1rem;
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

  /**
   * Path of this agent's DM in chat, or null when chat is unavailable or the
   * signed-in user is not yet known. Built the same way as the terminal
   * pane's "Open in chat" button.
   */
  private dmPath(): { key: string; path: string } | null {
    if (!isFeatureEnabled('web.native_chat')) return null;
    const key = buildAgentDMKey(this.agentId, this.userId || stateManager.getCurrentUserId());
    if (!key) return null;
    const path = chatConversationPath({ conversationKey: key });
    return path ? { key, path } : null;
  }

  /** Close the dialog and open the agent's DM, keeping any unsent text. */
  private openDM(): void {
    const target = this.dmPath();
    if (!target) return;
    seedChatDraft(target.key, this.messageText);
    this.close();
    this.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path: target.path },
        bubbles: true,
        composed: true,
      })
    );
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
        // Try to parse structured rejection error
        try {
          const errorBody = (await res.clone().json()) as {
            error?: { code?: string; details?: { reason?: string } };
          };
          if (errorBody.error?.code === 'message_denied' && errorBody.error?.details?.reason) {
            this.sendError = getDenialMessage(errorBody.error.details.reason, this.agentName);
            return;
          }
        } catch {
          /* fall through to generic */
        }
        this.sendError = await extractApiError(res, 'Failed to send message');
        return;
      }

      // Success — close dialog
      this.close();
    } catch (err) {
      this.sendError = err instanceof Error ? err.message : 'Failed to send message';
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
    const displayName =
      this.projectName && this.agentName
        ? `${this.projectName} / ${this.agentName}`
        : this.agentName;
    const label = displayName ? `Message ${displayName}` : 'Send Message';
    const dm = this.dmPath();

    return html`
      <sl-dialog label=${label} ?open=${this.open} @sl-request-close=${this.close}>
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
          ${this.sendError ? html`<div class="dialog-error">${this.sendError}</div>` : nothing}
        </div>

        <div slot="footer" class="dialog-footer">
          ${dm
            ? html`<sl-button
                class="open-dm"
                variant="text"
                ?disabled=${this.sending}
                @click=${() => this.openDM()}
              >
                <sl-icon slot="prefix" name="chat-dots"></sl-icon>
                Open agent DM
              </sl-button>`
            : nothing}
          <div class="dialog-footer-actions">
            <sl-button variant="default" @click=${this.close} ?disabled=${this.sending}>
              Cancel
            </sl-button>
            <sl-button
              variant="primary"
              ?loading=${this.sending}
              ?disabled=${this.sending || !this.messageText.trim()}
              @click=${() => void this.handleSend()}
            >
              Send
            </sl-button>
          </div>
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-quick-message-dialog': ScionQuickMessageDialog;
  }
}
