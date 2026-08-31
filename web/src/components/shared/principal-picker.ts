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
 * Reusable Principal Picker Component
 *
 * Provides a user-search autocomplete for selecting principals (users or agents).
 * Extracted from the inline user-search in group-member-editor.ts.
 *
 * - `user` type: renders a search-autocomplete calling GET /api/v1/users?search=...
 * - `agent` type: renders a plain text input (no autocomplete API exists yet)
 *
 * Emits `principal-change` event with { principalType, principalId, displayLabel }
 * when a principal is selected.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';

/** Event detail emitted when a principal is selected. */
export interface PrincipalChangeDetail {
  principalType: string;
  principalId: string;
  displayLabel: string;
}

@customElement('scion-principal-picker')
export class ScionPrincipalPicker extends LitElement {
  /** Controls which input mode to show: 'user' (autocomplete), 'agent' (plain text), or 'group' (autocomplete). */
  @property() principalType: 'user' | 'agent' | 'group' = 'user';

  /** Current selected principal ID. */
  @property() value = '';

  /** Input label. */
  @property() label = '';

  /** Placeholder override. */
  @property() placeholder = '';

  /** Disabled state. */
  @property({ type: Boolean }) disabled = false;

  // User search autocomplete state
  @state() private searchQuery = '';
  @state() private searchResults: Array<{ id: string; email: string; displayName: string }> = [];
  @state() private searchLoading = false;
  @state() private searchOpen = false;
  private searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private blurTimeoutId: ReturnType<typeof setTimeout> | null = null;
  private searchRequestId = 0;
  /** True after selectUser(); reset on new typed input. Prevents blur from overwriting a resolved selection. */
  private selectedViaDropdown = false;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchDebounceTimer) clearTimeout(this.searchDebounceTimer);
    if (this.blurTimeoutId) clearTimeout(this.blurTimeoutId);
    if (this.groupSearchDebounceTimer) clearTimeout(this.groupSearchDebounceTimer);
    if (this.groupBlurTimeoutId) clearTimeout(this.groupBlurTimeoutId);
  }

  static override styles = css`
    :host {
      display: block;
    }

    .user-search-container {
      position: relative;
    }

    .user-search-dropdown {
      position: absolute;
      top: 100%;
      left: 0;
      right: 0;
      z-index: 1000;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: 0 4px 12px rgba(0, 0, 0, 0.1);
      max-height: 200px;
      overflow-y: auto;
      margin-top: 0.25rem;
    }

    .user-search-option {
      display: flex;
      flex-direction: column;
      padding: 0.5rem 0.75rem;
      cursor: pointer;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .user-search-option:last-child {
      border-bottom: none;
    }

    .user-search-option:hover,
    .user-search-option.focused {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .user-search-option .user-name {
      font-weight: 500;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .user-search-option .user-email {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .user-search-empty,
    .user-search-loading {
      padding: 0.75rem;
      text-align: center;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    // Reset search state when principal type changes.
    if (changed.has('principalType')) {
      this.searchQuery = '';
      this.searchResults = [];
      this.searchOpen = false;
      this.groupSearchQuery = '';
      this.groupSearchResults = [];
      this.groupSearchOpen = false;
    }
  }

  // Group search autocomplete state
  @state() private groupSearchQuery = '';
  @state() private groupSearchResults: Array<{ id: string; name: string; slug: string }> = [];
  @state() private groupSearchLoading = false;
  @state() private groupSearchOpen = false;
  private groupSearchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private groupBlurTimeoutId: ReturnType<typeof setTimeout> | null = null;
  private groupSearchRequestId = 0;
  private groupSelectedViaDropdown = false;

  private get resolvedLabel(): string {
    if (this.label) return this.label;
    if (this.principalType === 'user') return 'User';
    if (this.principalType === 'group') return 'Group';
    return 'Agent ID';
  }

  private get resolvedPlaceholder(): string {
    if (this.placeholder) return this.placeholder;
    if (this.principalType === 'user') return 'Search by name or email...';
    if (this.principalType === 'group') return 'Search by group name...';
    return 'Enter agent ID';
  }

  // ---------------------------------------------------------------------------
  // User search logic
  // ---------------------------------------------------------------------------

  private handleSearchInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.searchQuery = value;
    this.selectedViaDropdown = false;

    if (this.searchDebounceTimer) {
      clearTimeout(this.searchDebounceTimer);
    }

    if (value.trim().length < 2) {
      this.searchResults = [];
      this.searchOpen = false;
      // For partial input, emit as-is so form validation works.
      this.emitChange(value.trim(), value.trim());
      return;
    }

    this.searchDebounceTimer = setTimeout(() => {
      void this.searchUsers(value.trim());
      // Also emit the raw typed value so the parent's bound value is never stale.
      // If the user later clicks a dropdown result, selectUser() will overwrite
      // this with the resolved UUID.
      this.emitChange(value.trim(), '');
    }, 250);
  }

  private async searchUsers(query: string): Promise<void> {
    const requestId = ++this.searchRequestId;
    this.searchLoading = true;
    this.searchOpen = true;
    try {
      const response = await apiFetch(`/api/v1/users?search=${encodeURIComponent(query)}&limit=10`);
      if (requestId !== this.searchRequestId) return; // stale response, a newer request has superseded this one
      if (response.ok) {
        const data = (await response.json()) as {
          users?: Array<{ id: string; email: string; displayName: string }>;
        };
        this.searchResults = data.users || [];
      }
    } catch (err) {
      if (requestId !== this.searchRequestId) return;
      console.error('Failed to search users:', err);
      this.searchResults = [];
    } finally {
      if (requestId === this.searchRequestId) {
        this.searchLoading = false;
      }
    }
  }

  private selectUser(user: { id: string; email: string; displayName: string }): void {
    // Show display-friendly label in the input.
    this.searchQuery = user.displayName ? `${user.displayName} (${user.email})` : user.email;
    this.searchOpen = false;
    this.searchResults = [];
    this.selectedViaDropdown = true;
    // Emit the user's UUID as principalId (not email).
    this.emitChange(user.id, user.displayName || user.email);
  }

  private handleAgentInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.emitChange(value.trim(), value.trim());
  }

  private emitChange(principalId: string, displayLabel: string): void {
    this.dispatchEvent(
      new CustomEvent<PrincipalChangeDetail>('principal-change', {
        detail: {
          principalType: this.principalType,
          principalId,
          displayLabel,
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.principalType === 'user') {
      return this.renderUserSearch();
    }
    if (this.principalType === 'group') {
      return this.renderGroupSearch();
    }
    return this.renderAgentInput();
  }

  private renderUserSearch() {
    return html`
      <div class="user-search-container">
        <sl-input
          label=${this.resolvedLabel}
          placeholder=${this.resolvedPlaceholder}
          value=${this.searchQuery}
          type="text"
          autocomplete="off"
          ?disabled=${this.disabled}
          @sl-input=${this.handleSearchInput}
          @sl-focus=${() => {
            if (this.searchResults.length > 0) this.searchOpen = true;
          }}
          @sl-blur=${() => {
            // Delay to allow click on dropdown.
            this.blurTimeoutId = setTimeout(() => {
              this.searchOpen = false;
              // Emit the raw typed value on blur if no dropdown selection was
              // made, so the parent has the latest value even when the
              // debounce timer hasn't fired yet (e.g. user typed and
              // immediately tabbed away or clicked submit).
              if (!this.selectedViaDropdown && this.searchQuery.trim()) {
                this.emitChange(this.searchQuery.trim(), '');
              }
            }, 200);
          }}
        ></sl-input>
        ${this.searchOpen
          ? html`
              <div class="user-search-dropdown">
                ${this.searchLoading
                  ? html`<div class="user-search-loading">
                      <sl-spinner></sl-spinner> Searching...
                    </div>`
                  : this.searchResults.length === 0
                    ? html`<div class="user-search-empty">No users found</div>`
                    : this.searchResults.map(
                        (user) => html`
                          <div
                            class="user-search-option"
                            @mousedown=${(e: Event) => {
                              e.preventDefault();
                              this.selectUser(user);
                            }}
                          >
                            <span class="user-name">${user.displayName || user.email}</span>
                            ${user.displayName
                              ? html`<span class="user-email">${user.email}</span>`
                              : nothing}
                          </div>
                        `
                      )}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Group search logic
  // ---------------------------------------------------------------------------

  private handleGroupSearchInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.groupSearchQuery = value;
    this.groupSelectedViaDropdown = false;

    if (this.groupSearchDebounceTimer) {
      clearTimeout(this.groupSearchDebounceTimer);
    }

    if (value.trim().length < 2) {
      this.groupSearchResults = [];
      this.groupSearchOpen = false;
      this.emitChange(value.trim(), value.trim());
      return;
    }

    this.groupSearchDebounceTimer = setTimeout(() => {
      void this.searchGroups(value.trim());
      this.emitChange(value.trim(), '');
    }, 250);
  }

  private async searchGroups(query: string): Promise<void> {
    const requestId = ++this.groupSearchRequestId;
    this.groupSearchLoading = true;
    this.groupSearchOpen = true;
    try {
      const response = await apiFetch(`/api/v1/groups?search=${encodeURIComponent(query)}&limit=10`);
      if (requestId !== this.groupSearchRequestId) return;
      if (response.ok) {
        const data = (await response.json()) as {
          groups?: Array<{ id: string; name: string; slug: string }>;
        };
        this.groupSearchResults = data.groups || [];
      }
    } catch (err) {
      if (requestId !== this.groupSearchRequestId) return;
      console.error('Failed to search groups:', err);
      this.groupSearchResults = [];
    } finally {
      if (requestId === this.groupSearchRequestId) {
        this.groupSearchLoading = false;
      }
    }
  }

  private selectGroup(group: { id: string; name: string; slug: string }): void {
    this.groupSearchQuery = `${group.name} (${group.slug})`;
    this.groupSearchOpen = false;
    this.groupSearchResults = [];
    this.groupSelectedViaDropdown = true;
    this.emitChange(group.id, group.name);
  }

  private renderGroupSearch() {
    return html`
      <div class="user-search-container">
        <sl-input
          label=${this.resolvedLabel}
          placeholder=${this.resolvedPlaceholder}
          value=${this.groupSearchQuery}
          type="text"
          autocomplete="off"
          ?disabled=${this.disabled}
          @sl-input=${this.handleGroupSearchInput}
          @sl-focus=${() => {
            if (this.groupSearchResults.length > 0) this.groupSearchOpen = true;
          }}
          @sl-blur=${() => {
            this.groupBlurTimeoutId = setTimeout(() => {
              this.groupSearchOpen = false;
              if (!this.groupSelectedViaDropdown && this.groupSearchQuery.trim()) {
                this.emitChange(this.groupSearchQuery.trim(), '');
              }
            }, 200);
          }}
        ></sl-input>
        ${this.groupSearchOpen
          ? html`
              <div class="user-search-dropdown">
                ${this.groupSearchLoading
                  ? html`<div class="user-search-loading">
                      <sl-spinner></sl-spinner> Searching...
                    </div>`
                  : this.groupSearchResults.length === 0
                    ? html`<div class="user-search-empty">No groups found</div>`
                    : this.groupSearchResults.map(
                        (group) => html`
                          <div
                            class="user-search-option"
                            @mousedown=${(e: Event) => {
                              e.preventDefault();
                              this.selectGroup(group);
                            }}
                          >
                            <span class="user-name">${group.name}</span>
                            <span class="user-email">${group.slug}</span>
                          </div>
                        `
                      )}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderAgentInput() {
    return html`
      <sl-input
        label=${this.resolvedLabel}
        placeholder=${this.resolvedPlaceholder}
        value=${this.value}
        type="text"
        ?disabled=${this.disabled}
        @sl-input=${this.handleAgentInput}
      ></sl-input>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-principal-picker': ScionPrincipalPicker;
  }
}
