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
 * Chat system line component.
 *
 * Renders system/state-change messages in a centered, muted style.
 * Used for agent state transitions and other non-conversational events.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

@customElement('scion-chat-system-line')
export class ScionChatSystemLine extends LitElement {
  /** The system message text. */
  @property()
  message = '';

  /** Timestamp for the event. */
  @property()
  timestamp = '';

  /** Optional category icon name (from metadata.system_category). */
  @property()
  category = '';

  static override styles = css`
    :host {
      display: block;
      padding: 0.25rem 1rem;
    }

    .system-line {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 0.5rem;
      padding: 0.375rem 0.75rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-align: center;
    }

    .system-line sl-icon {
      font-size: 0.75rem;
      flex-shrink: 0;
    }

    .system-text {
      line-height: 1.4;
    }

    .system-time {
      font-size: 0.6875rem;
      opacity: 0.7;
      white-space: nowrap;
    }
  `;

  override render() {
    const iconName = this.getCategoryIcon();
    const timeStr = this.formatTime();

    return html`
      <div class="system-line">
        ${iconName
          ? html`<sl-icon name=${iconName}></sl-icon>`
          : null}
        <span class="system-text">${this.message}</span>
        ${timeStr
          ? html`<span class="system-time">${timeStr}</span>`
          : null}
      </div>
    `;
  }

  private getCategoryIcon(): string {
    switch (this.category) {
      case 'lifecycle':
        return 'arrow-repeat';
      case 'error':
        return 'exclamation-triangle';
      case 'config':
        return 'gear';
      default:
        return 'info-circle';
    }
  }

  private formatTime(): string {
    if (!this.timestamp) return '';
    try {
      const d = new Date(this.timestamp);
      return d.toLocaleTimeString('en', {
        hour12: false,
        hour: '2-digit',
        minute: '2-digit',
      });
    } catch {
      return '';
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-system-line': ScionChatSystemLine;
  }
}
