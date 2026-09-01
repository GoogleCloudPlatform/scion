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
 * Access Boundary Scope Selector (Step 3)
 *
 * Radio selection between System and Project scope. Project mode shows a
 * permission-filtered project search/autocomplete.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';
import { classMap } from 'lit/directives/class-map.js';

import { apiFetch } from '../../client/api.js';
import type { ConstraintScope } from '../../shared/access-boundaries.js';

export interface ScopeChangeDetail {
  scope: ConstraintScope | null;
  displayLabel: string;
}

interface ProjectResult {
  id: string;
  name: string;
  slug: string;
}

@customElement('scion-access-boundary-scope-selector')
export class ScionAccessBoundaryScopeSelector extends LitElement {
  /** Current scope type. */
  @property() scopeType: 'system' | 'project' = 'system';

  /** Selected project ID (if scope is project). */
  @property() projectId = '';

  /** Display label for selected project. */
  @property() projectLabel = '';

  // Project search state
  @state() private projectSearchQuery = '';
  @state() private projectSearchResults: ProjectResult[] = [];
  @state() private projectSearchLoading = false;
  @state() private projectSearchOpen = false;
  @state() private activeDescendantIndex = -1;
  private searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private blurTimeoutId: ReturnType<typeof setTimeout> | null = null;
  private searchRequestId = 0;
  private selectedViaDropdown = false;

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchDebounceTimer) clearTimeout(this.searchDebounceTimer);
    if (this.blurTimeoutId) clearTimeout(this.blurTimeoutId);
  }

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .scope-selector {
        display: flex;
        flex-direction: column;
        gap: 1.5rem;
      }

      .scope-options {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
      }

      .scope-card {
        display: flex;
        align-items: flex-start;
        gap: 0.75rem;
        padding: 0.875rem 1rem;
        min-height: 44px;
        border: 2px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        cursor: pointer;
        transition:
          border-color 0.15s ease,
          background-color 0.15s ease;
        background: var(--scion-surface, #ffffff);
      }

      .scope-card:hover {
        border-color: var(--sl-color-primary-300, #93c5fd);
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .scope-card:focus-visible {
        outline: 2px solid var(--sl-color-primary-600, #2563eb);
        outline-offset: 2px;
      }

      .scope-card.selected {
        border-color: var(--sl-color-primary-600, #2563eb);
        background: var(--sl-color-primary-50, #eff6ff);
      }

      .scope-icon {
        flex-shrink: 0;
        font-size: 1.125rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.0625rem;
      }

      .scope-card.selected .scope-icon {
        color: var(--sl-color-primary-600, #2563eb);
      }

      .scope-content {
        flex: 1;
        min-width: 0;
      }

      .scope-label {
        font-size: 0.875rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .scope-description {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
      }

      .project-search {
        position: relative;
      }

      .search-dropdown {
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

      .search-option {
        display: flex;
        flex-direction: column;
        padding: 0.5rem 0.75rem;
        min-height: 44px;
        justify-content: center;
        cursor: pointer;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .search-option:focus-visible {
        outline: 2px solid var(--sl-color-primary-600, #2563eb);
        outline-offset: -2px;
      }

      .search-option.active-descendant {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .search-option:last-child {
        border-bottom: none;
      }

      .search-option:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .search-option .project-name {
        font-weight: 500;
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
      }

      .search-option .project-slug {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .search-empty,
      .search-loading {
        padding: 0.75rem;
        text-align: center;
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }

      .summary {
        padding: 0.75rem 1rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
      }

      @media (max-width: 768px) {
        .scope-card {
          width: 100%;
        }

        .project-search {
          width: 100%;
        }

        .search-dropdown {
          max-height: 180px;
        }
      }

      @media (forced-colors: active) {
        .scope-card {
          border: 2px solid ButtonText;
        }

        .scope-card.selected {
          border-color: Highlight;
          background: none;
        }

        .scope-card:focus-visible {
          outline: 2px solid Highlight;
        }

        .search-dropdown {
          border: 2px solid ButtonText;
        }

        .search-option:hover,
        .search-option.active-descendant {
          outline: 2px solid Highlight;
        }
      }

      @media (prefers-reduced-motion: reduce) {
        .scope-card {
          transition: none;
        }
      }
    `,
  ];

  private handleScopeSelect(type: 'system' | 'project'): void {
    if (type !== this.scopeType) {
      this.scopeType = type;
      if (type === 'system') {
        this.projectId = '';
        this.projectLabel = '';
        this.projectSearchQuery = '';
        this.projectSearchResults = [];
        this.projectSearchOpen = false;
      }
    }
    this.emitChange();
  }

  private handleProjectSearchInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.projectSearchQuery = value;
    this.selectedViaDropdown = false;

    if (this.searchDebounceTimer) {
      clearTimeout(this.searchDebounceTimer);
    }

    if (value.trim().length < 2) {
      this.projectSearchResults = [];
      this.projectSearchOpen = false;
      return;
    }

    this.searchDebounceTimer = setTimeout(() => {
      void this.searchProjects(value.trim());
    }, 250);
  }

  private async searchProjects(query: string): Promise<void> {
    const requestId = ++this.searchRequestId;
    this.projectSearchLoading = true;
    this.projectSearchOpen = true;
    try {
      const response = await apiFetch(
        `/api/v1/projects?search=${encodeURIComponent(query)}&limit=10`
      );
      if (requestId !== this.searchRequestId) return;
      if (response.ok) {
        const data = (await response.json()) as {
          projects?: ProjectResult[];
        };
        this.projectSearchResults = data.projects || [];
      }
    } catch (err) {
      if (requestId !== this.searchRequestId) return;
      console.error('Failed to search projects:', err);
      this.projectSearchResults = [];
    } finally {
      if (requestId === this.searchRequestId) {
        this.projectSearchLoading = false;
      }
    }
  }

  private handleProjectSearchKeydown(e: KeyboardEvent): void {
    if (!this.projectSearchOpen) return;

    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        if (this.activeDescendantIndex < this.projectSearchResults.length - 1) {
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
          this.activeDescendantIndex < this.projectSearchResults.length
        ) {
          this.selectProject(this.projectSearchResults[this.activeDescendantIndex]);
        }
        break;
      case 'Escape':
        e.preventDefault();
        this.projectSearchOpen = false;
        this.activeDescendantIndex = -1;
        break;
    }
  }

  private selectProject(project: ProjectResult): void {
    this.projectSearchQuery = `${project.name} (${project.slug})`;
    this.projectSearchOpen = false;
    this.projectSearchResults = [];
    this.selectedViaDropdown = true;
    this.projectId = project.id;
    this.projectLabel = project.name;
    this.emitChange();
  }

  private buildScope(): ConstraintScope | null {
    if (this.scopeType === 'system') {
      return { type: 'system' };
    }
    return this.projectId ? { type: 'project', projectId: this.projectId } : null;
  }

  private emitChange(): void {
    const scope = this.buildScope();
    this.dispatchEvent(
      new CustomEvent<ScopeChangeDetail>('scope-change', {
        detail: {
          scope,
          displayLabel:
            this.scopeType === 'system' ? 'System-wide' : this.projectLabel || this.projectId || '',
        },
        bubbles: true,
        composed: true,
      })
    );
  }

  private getSummaryText(): string {
    if (this.scopeType === 'system') {
      return 'This access boundary applies system-wide across all projects.';
    }
    if (!this.projectId) {
      return 'Select a project to continue.';
    }
    return `This access boundary applies only within project "${this.projectLabel || this.projectId}".`;
  }

  override render() {
    return html`
      <div class="scope-selector">
        <div class="scope-options" role="radiogroup" aria-label="Scope type">
          <div
            class=${classMap({ 'scope-card': true, selected: this.scopeType === 'system' })}
            role="radio"
            aria-checked=${this.scopeType === 'system' ? 'true' : 'false'}
            tabindex="0"
            @click=${() => this.handleScopeSelect('system')}
            @keydown=${(e: KeyboardEvent) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                this.handleScopeSelect('system');
              }
            }}
          >
            <sl-icon class="scope-icon" name="globe2"></sl-icon>
            <div class="scope-content">
              <div class="scope-label">System</div>
              <div class="scope-description">
                Applies across all projects and system-level resources.
              </div>
            </div>
          </div>

          <div
            class=${classMap({ 'scope-card': true, selected: this.scopeType === 'project' })}
            role="radio"
            aria-checked=${this.scopeType === 'project' ? 'true' : 'false'}
            tabindex="0"
            @click=${() => this.handleScopeSelect('project')}
            @keydown=${(e: KeyboardEvent) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                this.handleScopeSelect('project');
              }
            }}
          >
            <sl-icon class="scope-icon" name="folder"></sl-icon>
            <div class="scope-content">
              <div class="scope-label">Project</div>
              <div class="scope-description">Applies only within a specific project.</div>
            </div>
          </div>
        </div>

        ${this.scopeType === 'project'
          ? html`
              <div class="project-search">
                <sl-input
                  label="Search for project"
                  placeholder="Search by project name..."
                  value=${this.projectSearchQuery}
                  type="text"
                  autocomplete="off"
                  role="combobox"
                  aria-expanded=${this.projectSearchOpen ? 'true' : 'false'}
                  aria-controls="project-search-listbox"
                  aria-activedescendant=${this.activeDescendantIndex >= 0
                    ? `project-option-${this.activeDescendantIndex}`
                    : ''}
                  @sl-input=${(e: Event) => this.handleProjectSearchInput(e)}
                  @keydown=${(e: KeyboardEvent) => this.handleProjectSearchKeydown(e)}
                  @sl-focus=${() => {
                    if (this.projectSearchResults.length > 0) this.projectSearchOpen = true;
                  }}
                  @sl-blur=${() => {
                    this.blurTimeoutId = setTimeout(() => {
                      this.projectSearchOpen = false;
                      this.activeDescendantIndex = -1;
                      if (!this.selectedViaDropdown && this.projectSearchQuery.trim()) {
                        // Typed but didn't select — clear project
                        this.projectId = '';
                        this.projectLabel = '';
                        this.emitChange();
                      }
                    }, 200);
                  }}
                ></sl-input>
                ${this.projectSearchOpen
                  ? html`
                      <div
                        class="search-dropdown"
                        id="project-search-listbox"
                        role="listbox"
                        aria-label="Project search results"
                      >
                        ${this.projectSearchLoading
                          ? html`<div class="search-loading" role="status" aria-live="polite">
                              <sl-spinner></sl-spinner> Searching...
                            </div>`
                          : this.projectSearchResults.length === 0
                            ? html`<div class="search-empty" role="status" aria-live="polite">
                                No projects found
                              </div>`
                            : this.projectSearchResults.map(
                                (project, idx) => html`
                                  <div
                                    class="search-option ${idx === this.activeDescendantIndex
                                      ? 'active-descendant'
                                      : ''}"
                                    id="project-option-${idx}"
                                    role="option"
                                    aria-selected=${idx === this.activeDescendantIndex
                                      ? 'true'
                                      : 'false'}
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
            `
          : nothing}

        <div class="summary">${this.getSummaryText()}</div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-scope-selector': ScionAccessBoundaryScopeSelector;
  }
}
