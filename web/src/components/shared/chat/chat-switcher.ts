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
 * Chat quick-switcher component (Cmd/Ctrl-K).
 *
 * Renders a modal overlay with a search input and a filterable list of
 * conversations. Keyboard navigation (Arrow Up/Down, Enter, Escape) is
 * supported. Conversations are sorted by most recent activity first when
 * the search field is empty.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state, query } from 'lit/decorators.js';

/** A conversation entry displayable in the switcher. */
export interface SwitcherConversation {
  /** Topic UUID or DM key. */
  conversationKey: string;
  /** Display name: thread name, DM peer name, etc. */
  name: string;
  /** Project/space name for context. */
  spaceName: string;
  /** True for DMs, false for threads. */
  isDM: boolean;
  /** ISO timestamp for sorting by recency. */
  lastActivityAt?: string;
  /** Project ID for thread navigation (not set for DMs). */
  projectId?: string;
}

/** Detail emitted on `switcher-select`. */
export interface SwitcherSelectDetail {
  conversationKey: string;
}

@customElement('scion-chat-switcher')
export class ScionChatSwitcher extends LitElement {
  /** Full list of conversations to search through. */
  @property({ type: Array })
  conversations: SwitcherConversation[] = [];

  @state() private searchTerm = '';
  @state() private selectedIndex = 0;

  @query('#switcher-input')
  private inputEl!: HTMLInputElement;

  static override styles = css`
    :host {
      display: block;
    }

    .overlay {
      position: fixed;
      inset: 0;
      z-index: 9999;
      display: flex;
      align-items: flex-start;
      justify-content: center;
      padding-top: 15vh;
      background: rgba(0, 0, 0, 0.4);
    }

    .panel {
      width: min(520px, 90vw);
      max-height: 60vh;
      display: flex;
      flex-direction: column;
      background: var(--scion-surface, #fff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.5rem;
      box-shadow: 0 8px 32px rgba(0, 0, 0, 0.2);
      overflow: hidden;
    }

    .search-row {
      display: flex;
      align-items: center;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      gap: 0.5rem;
    }

    .search-row sl-icon {
      color: var(--scion-text-muted, #64748b);
      font-size: 1rem;
    }

    .search-row input {
      flex: 1;
      border: none;
      outline: none;
      font-size: 0.9375rem;
      background: transparent;
      color: var(--scion-text, #1e293b);
    }

    .search-row input::placeholder {
      color: var(--scion-text-muted, #94a3b8);
    }

    .results {
      overflow-y: auto;
      max-height: calc(60vh - 3.5rem);
    }

    .empty {
      padding: 2rem 1rem;
      text-align: center;
      color: var(--scion-text-muted, #94a3b8);
      font-size: 0.875rem;
    }

    .item {
      display: flex;
      flex-direction: column;
      padding: 0.5rem 1rem;
      cursor: pointer;
      gap: 0.125rem;
      border-left: 3px solid transparent;
    }

    .item:hover,
    .item.selected {
      background: var(--scion-bg-subtle, #f1f5f9);
      border-left-color: var(--scion-primary, #3b82f6);
    }

    .item-name {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .item-context {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #94a3b8);
    }

    .dm-badge {
      display: inline-block;
      font-size: 0.6875rem;
      padding: 0 0.25rem;
      border-radius: 0.125rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      margin-left: 0.25rem;
    }

    .shortcut-hint {
      padding: 0.375rem 1rem;
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #94a3b8);
      border-top: 1px solid var(--scion-border, #e2e8f0);
      display: flex;
      gap: 1rem;
    }

    kbd {
      display: inline-block;
      padding: 0 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: 0.125rem;
      font-family: inherit;
      font-size: 0.625rem;
      background: var(--scion-bg-subtle, #f1f5f9);
    }
  `;

  override firstUpdated(): void {
    // Autofocus the input after render.
    requestAnimationFrame(() => {
      this.inputEl?.focus();
    });
  }

  private get filtered(): SwitcherConversation[] {
    const term = this.searchTerm.toLowerCase().trim();
    let list = this.conversations;

    if (term) {
      list = list.filter(
        (c) =>
          c.name.toLowerCase().includes(term) ||
          c.spaceName.toLowerCase().includes(term)
      );
    }

    // Sort by most recent activity first.
    return [...list].sort((a, b) => {
      const ta = a.lastActivityAt ? new Date(a.lastActivityAt).getTime() : 0;
      const tb = b.lastActivityAt ? new Date(b.lastActivityAt).getTime() : 0;
      return tb - ta;
    });
  }

  private handleInput(e: InputEvent): void {
    this.searchTerm = (e.target as HTMLInputElement).value;
    this.selectedIndex = 0;
  }

  private handleKeydown(e: KeyboardEvent): void {
    const items = this.filtered;
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        this.selectedIndex = Math.min(this.selectedIndex + 1, items.length - 1);
        this.scrollSelectedIntoView();
        break;
      case 'ArrowUp':
        e.preventDefault();
        this.selectedIndex = Math.max(this.selectedIndex - 1, 0);
        this.scrollSelectedIntoView();
        break;
      case 'Enter':
        e.preventDefault();
        if (items.length > 0 && this.selectedIndex < items.length) {
          this.selectConversation(items[this.selectedIndex]);
        }
        break;
      case 'Escape':
        e.preventDefault();
        this.close();
        break;
    }
  }

  private scrollSelectedIntoView(): void {
    requestAnimationFrame(() => {
      const selected = this.shadowRoot?.querySelector('.item.selected');
      selected?.scrollIntoView({ block: 'nearest' });
    });
  }

  private selectConversation(conv: SwitcherConversation): void {
    this.dispatchEvent(
      new CustomEvent<SwitcherSelectDetail>('switcher-select', {
        detail: { conversationKey: conv.conversationKey },
        bubbles: true,
        composed: true,
      })
    );
  }

  private close(): void {
    this.dispatchEvent(
      new CustomEvent('switcher-close', { bubbles: true, composed: true })
    );
  }

  private handleOverlayClick(e: MouseEvent): void {
    // Close only when clicking the backdrop, not the panel.
    if ((e.target as HTMLElement)?.classList.contains('overlay')) {
      this.close();
    }
  }

  override render() {
    const items = this.filtered;
    return html`
      <div class="overlay" @click=${this.handleOverlayClick} @keydown=${this.handleKeydown}>
        <div class="panel">
          <div class="search-row">
            <sl-icon name="search"></sl-icon>
            <input
              id="switcher-input"
              type="text"
              placeholder="Search conversations..."
              .value=${this.searchTerm}
              @input=${this.handleInput}
              autocomplete="off"
            />
          </div>
          <div class="results">
            ${items.length === 0
              ? html`<div class="empty">
                  ${this.searchTerm ? 'No matching conversations' : 'No conversations'}
                </div>`
              : items.map(
                  (item, i) => html`
                    <div
                      class="item ${i === this.selectedIndex ? 'selected' : ''}"
                      @click=${() => this.selectConversation(item)}
                      @mouseenter=${() => { this.selectedIndex = i; }}
                    >
                      <div class="item-name">
                        ${item.name}
                        ${item.isDM ? html`<span class="dm-badge">DM</span>` : nothing}
                      </div>
                      <div class="item-context">${item.spaceName}</div>
                    </div>
                  `
                )}
          </div>
          <div class="shortcut-hint">
            <span><kbd>↑↓</kbd> navigate</span>
            <span><kbd>↵</kbd> select</span>
            <span><kbd>esc</kbd> close</span>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-chat-switcher': ScionChatSwitcher;
  }
}
