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
 * View Toggle Component
 *
 * A compact two-button toggle for switching between grid (card) and list (table) views.
 * Persists the selected view in localStorage and dispatches a `view-change` CustomEvent.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

export type ViewMode = 'grid' | 'list';

@customElement('scion-view-toggle')
export class ScionViewToggle extends LitElement {
  /**
   * Current view mode
   */
  @property({ type: String })
  view: ViewMode = 'grid';

  /**
   * localStorage key for persistence (e.g. 'scion-view-projects')
   */
  @property({ type: String })
  storageKey = '';

  static override styles = css`
    :host {
      display: inline-flex;
    }

    .toggle-group {
      display: inline-flex;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      overflow: hidden;
    }

    button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 2rem;
      height: 2rem;
      border: none;
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text-muted, #64748b);
      cursor: pointer;
      padding: 0;
      transition: all 150ms ease;
    }

    button:first-child {
      border-right: 1px solid var(--scion-border, #e2e8f0);
    }

    button:hover:not(.active) {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    button.active {
      background: var(--scion-primary, #3b82f6);
      color: white;
    }

    button sl-icon {
      font-size: 0.875rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.storageKey) {
      const stored = localStorage.getItem(this.storageKey) as ViewMode | null;
      if (stored === 'grid' || stored === 'list') {
        this.view = stored;
      }
    }
  }

  private setView(mode: ViewMode): void {
    if (this.view === mode) return;
    this.view = mode;
    if (this.storageKey) {
      localStorage.setItem(this.storageKey, mode);
    }
    this.dispatchEvent(
      new CustomEvent('view-change', {
        detail: { view: mode },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render() {
    return html`
      <div class="toggle-group">
        <button
          class=${this.view === 'grid' ? 'active' : ''}
          title="Grid view"
          @click=${() => this.setView('grid')}
        >
          <sl-icon name="grid-3x3-gap"></sl-icon>
        </button>
        <button
          class=${this.view === 'list' ? 'active' : ''}
          title="List view"
          @click=${() => this.setView('list')}
        >
          <sl-icon name="list-ul"></sl-icon>
        </button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-view-toggle': ScionViewToggle;
  }
}
