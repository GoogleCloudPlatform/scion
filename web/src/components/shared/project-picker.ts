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
 * Reusable Project Picker Component
 *
 * Provides a search-autocomplete for selecting projects by name or slug.
 * Extracted from the project-search logic in access-boundary-scope-selector.ts
 * for reuse across role-binding forms and other project-scoped UIs.
 *
 * - Searches GET /api/v1/projects?search=...&limit=10
 * - Results display project name and slug
 * - Selection emits canonical project UUID
 * - Typed UUID is accepted unchanged
 * - Typed project slug is accepted as direct input (backend resolves it)
 *
 * Emits `project-change` event with { projectId, displayLabel }
 * when a project is selected or typed.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch } from '../../client/api.js';

/** Event detail emitted when a project is selected or typed. */
export interface ProjectChangeDetail {
  projectId: string;
  displayLabel: string;
}

interface ProjectResult {
  id: string;
  name: string;
  slug: string;
}

@customElement('scion-project-picker')
export class ScionProjectPicker extends LitElement {
  /** Input label. */
  @property() label = 'Project';

  /** Placeholder override. */
  @property() placeholder = 'Search by project name or slug...';

  /** Disabled state. */
  @property({ type: Boolean }) disabled = false;

  /** Current selected project ID. */
  @property() value = '';

  // Project search autocomplete state
  @state() private searchQuery = '';
  @state() private searchResults: ProjectResult[] = [];
  @state() private searchLoading = false;
  @state() private searchOpen = false;
  @state() private activeDescendantIndex = -1;
  private searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private blurTimeoutId: ReturnType<typeof setTimeout> | null = null;
  private searchRequestId = 0;
  /** True after selectProject(); reset on new typed input. Prevents blur from overwriting a resolved selection. */
  private selectedViaDropdown = false;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchDebounceTimer) clearTimeout(this.searchDebounceTimer);
    if (this.blurTimeoutId) clearTimeout(this.blurTimeoutId);
  }

  static override styles = css`
    :host {
      display: block;
    }

    .project-search-container {
      position: relative;
    }

    .project-search-dropdown {
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

    .project-search-option {
      display: flex;
      flex-direction: column;
      padding: 0.5rem 0.75rem;
      min-height: 44px;
      justify-content: center;
      cursor: pointer;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .project-search-option:last-child {
      border-bottom: none;
    }

    .project-search-option:hover,
    .project-search-option.active-descendant {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .project-search-option:focus-visible {
      outline: 2px solid var(--sl-color-primary-600, #2563eb);
      outline-offset: -2px;
    }

    .project-search-option .project-name {
      font-weight: 500;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .project-search-option .project-slug {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .project-search-empty,
    .project-search-loading {
      padding: 0.75rem;
      text-align: center;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    @media (forced-colors: active) {
      .project-search-dropdown {
        border: 2px solid ButtonText;
      }

      .project-search-option:hover,
      .project-search-option.active-descendant {
        outline: 2px solid Highlight;
      }
    }
  `;

  override updated(changed: Map<string, unknown>): void {
    // Reset search state when value is externally cleared.
    if (changed.has('value') && !this.value && this.searchQuery) {
      this.searchQuery = '';
      this.searchResults = [];
      this.searchOpen = false;
      this.activeDescendantIndex = -1;
    }
  }

  // ---------------------------------------------------------------------------
  // Search logic
  // ---------------------------------------------------------------------------

  private handleSearchInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.searchQuery = value;
    this.selectedViaDropdown = false;
    this.activeDescendantIndex = -1;

    if (this.searchDebounceTimer) {
      clearTimeout(this.searchDebounceTimer);
    }

    if (value.trim().length < 2) {
      this.searchResults = [];
      this.searchOpen = false;
      // Emit as-is so form validation works with partial input.
      this.emitChange(value.trim(), value.trim());
      return;
    }

    this.searchDebounceTimer = setTimeout(() => {
      void this.searchProjects(value.trim());
      // Emit raw typed value so the parent's bound value is never stale.
      // If the user later clicks a dropdown result, selectProject() overwrites
      // this with the resolved UUID.
      this.emitChange(value.trim(), '');
    }, 250);
  }

  private async searchProjects(query: string): Promise<void> {
    const requestId = ++this.searchRequestId;
    this.searchLoading = true;
    this.searchOpen = true;
    try {
      const response = await apiFetch(
        `/api/v1/projects?search=${encodeURIComponent(query)}&limit=10`
      );
      if (requestId !== this.searchRequestId) return; // stale
      if (response.ok) {
        const data = (await response.json()) as {
          projects?: ProjectResult[];
        };
        this.searchResults = data.projects || [];
      }
    } catch (err) {
      if (requestId !== this.searchRequestId) return;
      console.error('Failed to search projects:', err);
      this.searchResults = [];
    } finally {
      if (requestId === this.searchRequestId) {
        this.searchLoading = false;
      }
    }
  }

  private handleKeydown(e: KeyboardEvent): void {
    if (!this.searchOpen) return;

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        if (this.activeDescendantIndex < this.searchResults.length - 1) {
          this.activeDescendantIndex++;
        }
        break;
      case 'ArrowUp':
        e.preventDefault();
        if (this.activeDescendantIndex > 0) {
          this.activeDescendantIndex--;
        }
        break;
      case 'Enter':
        e.preventDefault();
        if (
          this.activeDescendantIndex >= 0 &&
          this.activeDescendantIndex < this.searchResults.length
        ) {
          this.selectProject(this.searchResults[this.activeDescendantIndex]);
        }
        break;
      case 'Escape':
        e.preventDefault();
        this.searchOpen = false;
        this.activeDescendantIndex = -1;
        break;
    }
  }

  private selectProject(project: ProjectResult): void {
    this.searchQuery = `${project.name} (${project.slug})`;
    this.searchOpen = false;
    this.searchResults = [];
    this.selectedViaDropdown = true;
    this.activeDescendantIndex = -1;
    // Emit the project's UUID as projectId.
    this.emitChange(project.id, project.name);
  }

  private emitChange(projectId: string, displayLabel: string): void {
    this.dispatchEvent(
      new CustomEvent<ProjectChangeDetail>('project-change', {
        detail: { projectId, displayLabel },
        bubbles: true,
        composed: true,
      })
    );
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="project-search-container">
        <sl-input
          label=${this.label}
          placeholder=${this.placeholder}
          value=${this.searchQuery}
          type="text"
          autocomplete="off"
          role="combobox"
          aria-expanded=${this.searchOpen ? 'true' : 'false'}
          aria-controls="project-picker-listbox"
          aria-activedescendant=${this.activeDescendantIndex >= 0
            ? `project-picker-option-${this.activeDescendantIndex}`
            : ''}
          ?disabled=${this.disabled}
          @sl-input=${(e: Event) => this.handleSearchInput(e)}
          @keydown=${(e: KeyboardEvent) => this.handleKeydown(e)}
          @sl-focus=${() => {
            if (this.searchResults.length > 0) this.searchOpen = true;
          }}
          @sl-blur=${() => {
            this.blurTimeoutId = setTimeout(() => {
              this.searchOpen = false;
              this.activeDescendantIndex = -1;
              // Emit the raw typed value on blur if no dropdown selection was
              // made, so the parent has the latest value even when the
              // debounce timer hasn't fired yet.
              if (!this.selectedViaDropdown && this.searchQuery.trim()) {
                this.emitChange(this.searchQuery.trim(), '');
              }
            }, 200);
          }}
        ></sl-input>
        ${this.searchOpen
          ? html`
              <div
                class="project-search-dropdown"
                id="project-picker-listbox"
                role="listbox"
                aria-label="Project search results"
              >
                ${this.searchLoading
                  ? html`<div class="project-search-loading" role="status" aria-live="polite">
                      <sl-spinner></sl-spinner> Searching...
                    </div>`
                  : this.searchResults.length === 0
                    ? html`<div class="project-search-empty" role="status" aria-live="polite">
                        No projects found
                      </div>`
                    : this.searchResults.map(
                        (project, idx) => html`
                          <div
                            class="project-search-option ${idx === this.activeDescendantIndex
                              ? 'active-descendant'
                              : ''}"
                            id="project-picker-option-${idx}"
                            role="option"
                            aria-selected=${idx === this.activeDescendantIndex ? 'true' : 'false'}
                            @mousedown=${(e: Event) => {
                              e.preventDefault();
                              this.selectProject(project);
                            }}
                          >
                            <span class="project-name">${project.name}</span>
                            <span class="project-slug">${project.slug}</span>
                          </div>
                        `
                      )}
              </div>
            `
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-picker': ScionProjectPicker;
  }
}
