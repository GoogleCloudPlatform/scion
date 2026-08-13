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
 * Inline collapsed inter-agent message marker.
 *
 * Renders a compact marker in the DM timeline showing that the DM agent
 * exchanged messages with another agent. Collapsed by default, it shows
 * a count + peer agent name. On expand it lazy-loads and displays the
 * individual messages in a compact sender->recipient format.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { apiFetch } from '../../../client/api.js';
import type { Message } from '../../../shared/types.js';

@customElement('scion-chat-interagent-marker')
export class ScionChatInteragentMarker extends LitElement {
  /** Peer agent display name. */
  @property()
  peerAgent = '';

  /** Peer agent UUID (for potential avatar colouring). */
  @property()
  peerAgentId = '';

  /** Number of messages in this exchange. */
  @property({ type: Number })
  messageCount = 0;

  /** The DM conversation key (for fetching). */
  @property()
  conversationKey = '';

  /** ISO timestamp of the first message in the exchange. */
  @property()
  timeStart = '';

  /** ISO timestamp of the last message in the exchange. */
  @property()
  timeEnd = '';

  /** Whether this marker is expanded (shows individual messages). */
  @property({ type: Boolean, reflect: true })
  expanded = false;

  /** Externally-controlled global expand/collapse override. */
  @property({ type: Boolean, attribute: 'global-expanded' })
  globalExpanded = false;

  /** The loaded messages (populated on first expand). */
  @state()
  private messages: Message[] = [];

  /** Whether messages are currently being fetched. */
  @state()
  private loading = false;

  /** Whether messages have been fetched at least once (cache). */
  private fetched = false;

  static override styles = css`
    :host {
      display: block;
      padding: 0.125rem 1rem;
    }

    .marker {
      display: flex;
      flex-direction: column;
      border-left: 2px solid var(--scion-border, #e2e8f0);
      margin-left: 1rem;
      padding-left: 0.75rem;
    }

    .marker-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      cursor: pointer;
      padding: 0.25rem 0;
      user-select: none;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.75rem;
      transition: color 0.15s;
    }

    .marker-header:hover {
      color: var(--scion-text, #1e293b);
    }

    .marker-count {
      font-weight: 500;
    }

    .marker-header sl-icon {
      font-size: 0.75rem;
      flex-shrink: 0;
    }

    /* Expanded message list */
    .interagent-messages {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
      padding: 0.25rem 0 0.25rem 0;
    }

    .ia-msg {
      display: flex;
      align-items: baseline;
      gap: 0.25rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      line-height: 1.4;
      padding: 0.125rem 0;
    }

    .ia-sender {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .ia-arrow {
      color: var(--scion-text-muted, #94a3b8);
      flex-shrink: 0;
    }

    .ia-recipient {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      white-space: nowrap;
    }

    .ia-body {
      color: var(--scion-text-muted, #64748b);
      overflow: hidden;
      text-overflow: ellipsis;
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
    }

    .loading-indicator {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      padding: 0.25rem 0;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-indicator sl-spinner {
      font-size: 0.75rem;
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    // React to global expand/collapse toggle changes.
    if (changed.has('globalExpanded')) {
      const wasGlobal = changed.get('globalExpanded') as boolean | undefined;
      if (wasGlobal !== undefined && wasGlobal !== this.globalExpanded) {
        this.expanded = this.globalExpanded;
        if (this.expanded && !this.fetched) {
          void this.fetchMessages();
        }
      }
    }
  }

  /** Format a sender/recipient like "agent:slug" to just "slug". */
  private formatParticipant(value: string): string {
    if (value.startsWith('agent:')) return value.slice(6);
    if (value.startsWith('user:')) return value.slice(5);
    return value;
  }

  /** Toggle expanded state and lazy-load on first expand. */
  private toggle(): void {
    this.expanded = !this.expanded;
    if (this.expanded && !this.fetched) {
      void this.fetchMessages();
    }
  }

  /** Fetch inter-agent messages from the API for this exchange's time range. */
  private async fetchMessages(): Promise<void> {
    if (this.loading || this.fetched) return;
    this.loading = true;

    try {
      const params = new URLSearchParams({ limit: '200' });
      if (this.timeStart) {
        // Subtract 1ms to ensure the first message is included (After is exclusive).
        const afterDate = new Date(new Date(this.timeStart).getTime() - 1);
        params.set('after', afterDate.toISOString());
      }
      if (this.timeEnd) {
        // Add 1ms to ensure the last message is included (Before is exclusive).
        const beforeDate = new Date(new Date(this.timeEnd).getTime() + 1);
        params.set('before', beforeDate.toISOString());
      }

      const res = await apiFetch(
        `/api/v1/chat/conversations/${encodeURIComponent(this.conversationKey)}/interagent?${params.toString()}`
      );

      if (res.ok) {
        const data = (await res.json()) as { messages?: Message[] };
        const allMessages = data?.messages ?? [];
        // Filter to only messages with this specific peer agent.
        this.messages = allMessages.filter(
          (m) =>
            m.senderId === this.peerAgentId ||
            m.recipientId === this.peerAgentId ||
            m.sender === `agent:${this.peerAgent}` ||
            m.recipient === `agent:${this.peerAgent}`
        );
        this.fetched = true;
      }
    } catch {
      // Non-critical — leave messages empty.
    } finally {
      this.loading = false;
    }
  }

  override render() {
    return html`
      <div class="marker">
        <div class="marker-header" @click=${this.toggle}>
          <span class="marker-count">
            ${this.messageCount} message${this.messageCount !== 1 ? 's' : ''} with
            ${this.peerAgent}
          </span>
          <sl-icon name=${this.expanded ? 'chevron-down' : 'chevron-right'}></sl-icon>
        </div>
        ${this.expanded ? this.renderExpanded() : nothing}
      </div>
    `;
  }

  private renderExpanded() {
    if (this.loading) {
      return html`
        <div class="loading-indicator">
          <sl-spinner></sl-spinner>
          <span>Loading messages...</span>
        </div>
      `;
    }

    if (this.messages.length === 0) {
      return html`
        <div class="loading-indicator">
          <span>No messages loaded.</span>
        </div>
      `;
    }

    return html`
      <div class="interagent-messages">
        ${this.messages.map(
          (m) => html`
            <div class="ia-msg">
              <span class="ia-sender">${this.formatParticipant(m.sender)}</span>
              <span class="ia-arrow">&rarr;</span>
              <span class="ia-recipient">${this.formatParticipant(m.recipient)}</span>:
              <span class="ia-body">${m.msg}</span>
            </div>
          `
        )}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-interagent-marker': ScionChatInteragentMarker;
  }
}
