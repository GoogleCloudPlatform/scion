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
 * Renders a compact pill in the DM timeline showing that inter-agent
 * messages occurred between two user-facing DM messages. Collapsed by
 * default, it shows "(n agent-agent messages)". On expand it displays the
 * individual messages in a compact sender->recipient format.
 *
 * Messages are passed directly via the `.messages` property (no lazy loading).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property } from 'lit/decorators.js';
import type { Message } from '../../../shared/types.js';

@customElement('scion-chat-interagent-marker')
export class ScionChatInteragentMarker extends LitElement {
  /** Number of messages in this group. */
  @property({ type: Number })
  messageCount = 0;

  /** The messages to display when expanded (passed directly from parent). */
  @property({ type: Array })
  messages: Message[] = [];

  /** Whether this marker is expanded (shows individual messages). */
  @property({ type: Boolean, reflect: true })
  expanded = false;

  /** Externally-controlled global expand/collapse override. */
  @property({ type: Boolean, attribute: 'global-expanded' })
  globalExpanded = false;

  /** Whether this marker is hidden (eye toggle off). */
  @property({ type: Boolean, reflect: true })
  override hidden = false;

  static override styles = css`
    :host {
      display: block;
    }

    :host([hidden]) {
      display: none;
    }

    /* Collapsed pill — centered compact badge */
    .marker-pill {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      max-width: 40%;
      margin-left: auto;
      margin-right: auto;
      padding: 0.375rem 1rem;
      margin-top: 0.25rem;
      margin-bottom: 0.25rem;
      background: rgba(148, 163, 184, 0.1);
      border: 1px solid var(--scion-border, rgba(148, 163, 184, 0.2));
      border-radius: 9999px;
      cursor: pointer;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      transition: background 0.15s;
      user-select: none;
      text-align: center;
    }

    .marker-pill:hover {
      background: rgba(148, 163, 184, 0.18);
    }

    /* Expanded state — bordered container, centered to match collapsed pill */
    .marker-expanded {
      max-width: min(70%, 600px);  /* match .bubble max-width in chat-message.ts */
      margin-left: auto;   /* centered */
      margin-right: auto;
      margin-top: 0.25rem;
      margin-bottom: 0.25rem;
      padding: 0.5rem 1rem;
      background: rgba(148, 163, 184, 0.05);
      border: 2px solid var(--scion-border, rgba(148, 163, 184, 0.2));
      border-radius: 0.5rem;
      cursor: pointer;
      user-select: none;
    }

    .marker-expanded:hover {
      background: rgba(148, 163, 184, 0.1);
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
  `;

  override updated(changed: Map<string, unknown>): void {
    // React to global expand/collapse toggle changes.
    if (changed.has('globalExpanded')) {
      const wasGlobal = changed.get('globalExpanded') as boolean | undefined;
      if (wasGlobal !== undefined && wasGlobal !== this.globalExpanded) {
        this.expanded = this.globalExpanded;
      }
    }
  }

  /** Format a sender/recipient like "agent:slug" to just "slug". */
  private formatParticipant(value: string): string {
    if (value.startsWith('agent:')) return value.slice(6);
    if (value.startsWith('user:')) return value.slice(5);
    return value;
  }

  /** Toggle expanded state — click anywhere on the pill. */
  private toggle(): void {
    this.expanded = !this.expanded;
  }

  override render() {
    if (this.expanded) {
      return html`
        <sl-tooltip content="Click to collapse">
          <div class="marker-expanded" @click=${this.toggle}>
            ${this.messages.length > 0
              ? this.messages.map(
                  (m) => html`
                    <div class="ia-msg">
                      <span class="ia-sender">${this.formatParticipant(m.sender)}</span>
                      <span class="ia-arrow">&rarr;</span>
                      <span class="ia-recipient">${this.formatParticipant(m.recipient)}</span>:
                      <span class="ia-body">${m.msg}</span>
                    </div>
                  `
                )
              : nothing}
          </div>
        </sl-tooltip>
      `;
    }

    return html`
      <sl-tooltip content="Click to expand">
        <div class="marker-pill" @click=${this.toggle}>
          ${this.messageCount} agent-agent message${this.messageCount !== 1 ? 's' : ''}
        </div>
      </sl-tooltip>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-interagent-marker': ScionChatInteragentMarker;
  }
}
