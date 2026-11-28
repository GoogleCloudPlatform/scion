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
 * Provides a search-autocomplete for selecting one or more projects by name or slug.
 * Extracted from the project-search logic in access-boundary-scope-selector.ts
 * for reuse across role-binding forms, skill creation, and project-scoped filters.
 *
 * Single-select mode (default):
 * - Searches GET /api/v1/projects?search=...&limit=10 (or limit=50 when browseOnFocus is enabled)
 * - Results display project name and slug
 * - Selection emits canonical project UUID via `project-change` ({ projectId, displayLabel })
 * - When `requireSelection` is false (default), typed UUID or slug is accepted as direct input
 * - Optional `filterProject` predicate filters displayed options (e.g. by `_capabilities`)
 *
 * Multi-select mode (`multiple`):
 * - Displays selected projects as removable tags
 * - Clicking or pressing Enter on an option toggles it without closing the dropdown
 * - Emits `projects-change` ({ projectIds, projects }) whenever selection changes
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { Capabilities } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { syncSlInputComboboxAria } from '../../utils/sl-input-combobox-aria.js';

/** Project item returned by /api/v1/projects and exposed to filter predicates / events. */
export interface ProjectOption {
  id: string;
  name: string;
  slug: string;
  _capabilities?: Capabilities;
}

/** Event detail emitted in single-select mode when a project is selected or typed. */
export interface ProjectChangeDetail {
  projectId: string;
  displayLabel: string;
}

/** Event detail emitted in multi-select mode when the project selection changes. */
export interface ProjectsChangeDetail {
  projectIds: string[];
  projects: ProjectOption[];
}

function formatProjectDisplay(project: Pick<ProjectOption, 'name' | 'slug'>): string {
  return project.slug ? `${project.name} (${project.slug})` : project.name;
}

@customElement('scion-project-picker')
export class ScionProjectPicker extends LitElement {
  /** Input label. Pass empty string to omit the visible label (e.g. inline filter bars). */
  @property() label = 'Project';

  /** Placeholder override. */
  @property() placeholder = 'Search by project name or slug...';

  /** Disabled state. */
  @property({ type: Boolean }) disabled = false;

  /** Hoist the results popup with fixed positioning to escape overflow clipping inside dialogs/details. */
  @property({ type: Boolean }) hoist = false;

  /** Input control size. */
  @property() size: 'small' | 'medium' | 'large' = 'medium';

  /** Current selected project ID (single-select mode). */
  @property() value = '';

  /** Enable multi-select mode. */
  @property({ type: Boolean }) multiple = false;

  /** Current selected project IDs (multi-select mode). */
  @property({ type: Array }) values: string[] = [];

  /**
   * When true, focusing the input loads available projects immediately and
   * allows filtering with 1+ characters (instead of waiting for 2+ characters).
   */
  @property({ type: Boolean, attribute: 'browse-on-focus' }) browseOnFocus = false;

  /**
   * When true (single-select mode), only projects selected from the dropdown are
   * emitted as `projectId`; unchosen typed text is not emitted on blur.
   */
  @property({ type: Boolean, attribute: 'require-selection' }) requireSelection = false;

  /** Optional client-side filter applied to fetched project results (e.g. capability check). */
  @property({ attribute: false }) filterProject?: (project: ProjectOption) => boolean;

  /** Empty-state text shown when no projects match. */
  @property({ attribute: 'empty-text' }) emptyText = 'No projects found';

  // Project search autocomplete state
  @state() private searchQuery = '';
  @state() private searchResults: ProjectOption[] = [];
  @state() private searchLoading = false;
  @state() private searchOpen = false;
  @state() private activeDescendantIndex = -1;
  private resolvedProjects = new Map<string, ProjectOption>();
  private resolvingIds = new Set<string>();
  private searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private blurTimeoutId: ReturnType<typeof setTimeout> | null = null;
  private searchRequestId = 0;
  /** True after selectProject(); reset on new typed input. Prevents blur from overwriting a resolved selection. */
  private selectedViaDropdown = false;
  /** Display text of the last confirmed selection (for requireSelection blur restore). */
  private lastSelectedDisplay = '';

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

    .selected-tags {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
      margin-top: 0.375rem;
    }

    .selected-tags sl-tag::part(base) {
      max-width: 220px;
    }

    .selected-tags sl-tag::part(content) {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
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
      max-height: 240px;
      overflow-y: auto;
      margin-top: 0.25rem;
    }

    .project-search-dropdown.hoisted {
      position: static;
      margin-top: 0;
    }

    .project-search-option {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      min-height: 44px;
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

    .project-search-option.is-selected {
      background: var(--sl-color-primary-50, #eff6ff);
    }

    .project-search-option:focus-visible {
      outline: 2px solid var(--sl-color-primary-600, #2563eb);
      outline-offset: -2px;
    }

    .project-option-info {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .project-search-option .project-name {
      font-weight: 500;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .project-search-option .project-slug {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .project-check-icon {
      flex-shrink: 0;
      color: var(--scion-primary, #3b82f6);
      font-size: 1rem;
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
    if (changed.has('value') && !this.multiple) {
      if (!this.value && this.searchQuery) {
        this.searchQuery = '';
        this.lastSelectedDisplay = '';
        this.selectedViaDropdown = false;
        this.searchResults = [];
        this.searchOpen = false;
        this.activeDescendantIndex = -1;
      } else if (this.value && (this.browseOnFocus || this.requireSelection)) {
        const cached = this.resolvedProjects.get(this.value);
        if (cached) {
          if (this.filterProject && !this.filterProject(cached)) {
            this.clearDisallowedSelection();
          } else {
            const display = formatProjectDisplay(cached);
            this.searchQuery = display;
            this.lastSelectedDisplay = display;
            this.selectedViaDropdown = true;
          }
        } else {
          void this.resolveSingleProject(this.value);
        }
      }
    }

    if (changed.has('values') && this.multiple && this.values.length > 0) {
      const missing = this.values.filter(
        (id) => id && !this.resolvedProjects.has(id) && !this.resolvingIds.has(id)
      );
      if (missing.length > 0) {
        void this.resolveMultipleProjects(missing);
      }
    }

    syncSlInputComboboxAria(this.shadowRoot?.querySelector('sl-input'), {
      expanded: this.searchOpen,
      controls: 'project-picker-listbox',
      activeDescendant:
        this.activeDescendantIndex >= 0
          ? `project-picker-option-${this.activeDescendantIndex}`
          : undefined,
    });
  }

  private clearDisallowedSelection(): void {
    this.searchQuery = '';
    this.lastSelectedDisplay = '';
    this.selectedViaDropdown = false;
    this.emitChange('', '');
  }

  private async fetchProjectById(id: string): Promise<ProjectOption | null> {
    try {
      const response = await apiFetch(`/api/v1/projects/${encodeURIComponent(id)}`);
      if (!response.ok) return null;
      const data = (await response.json()) as { project?: ProjectOption } & Partial<ProjectOption>;
      const proj = data.project ?? (data.id && data.name ? (data as ProjectOption) : null);
      if (proj && proj.id && proj.name) {
        return {
          id: proj.id,
          name: proj.name,
          slug: proj.slug ?? '',
          ...(proj._capabilities ? { _capabilities: proj._capabilities } : {}),
        };
      }
      return null;
    } catch {
      return null;
    }
  }

  private async resolveSingleProject(id: string): Promise<void> {
    if (this.resolvingIds.has(id)) return;
    this.resolvingIds.add(id);
    try {
      const proj = await this.fetchProjectById(id);
      if (this.value !== id) return; // value changed while resolving
      if (!proj) {
        if (this.requireSelection) {
          this.clearDisallowedSelection();
        }
        return;
      }
      this.resolvedProjects.set(proj.id, proj);
      if (this.filterProject && !this.filterProject(proj)) {
        this.clearDisallowedSelection();
        return;
      }
      const display = formatProjectDisplay(proj);
      this.searchQuery = display;
      this.lastSelectedDisplay = display;
      this.selectedViaDropdown = true;
    } finally {
      this.resolvingIds.delete(id);
    }
  }

  private async resolveMultipleProjects(ids: string[]): Promise<void> {
    for (const id of ids) this.resolvingIds.add(id);
    try {
      const results = await Promise.all(ids.map((id) => this.fetchProjectById(id)));
      let updated = false;
      for (const proj of results) {
        if (proj) {
          this.resolvedProjects.set(proj.id, proj);
          updated = true;
        }
      }
      if (updated) {
        this.requestUpdate();
      }
    } finally {
      for (const id of ids) this.resolvingIds.delete(id);
    }
  }

  // ---------------------------------------------------------------------------
  // Search logic
  // ---------------------------------------------------------------------------

  private get isBrowseEnabled(): boolean {
    return this.browseOnFocus || this.multiple;
  }

  private handleSearchInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.searchQuery = value;
    this.selectedViaDropdown = false;
    this.activeDescendantIndex = -1;

    if (this.searchDebounceTimer) {
      clearTimeout(this.searchDebounceTimer);
    }

    const trimmed = value.trim();

    if (this.multiple) {
      if (trimmed.length === 0) {
        void this.searchProjects('');
        return;
      }
      this.searchDebounceTimer = setTimeout(() => {
        void this.searchProjects(trimmed);
      }, 250);
      return;
    }

    if (this.isBrowseEnabled) {
      if (trimmed.length === 0) {
        this.lastSelectedDisplay = '';
        this.emitChange('', '');
        void this.searchProjects('');
        return;
      }
      this.searchDebounceTimer = setTimeout(() => {
        void this.searchProjects(trimmed);
        if (!this.requireSelection) {
          this.emitChange(trimmed, '');
        }
      }, 250);
      return;
    }

    if (trimmed.length < 2) {
      this.searchResults = [];
      this.searchOpen = false;
      // Emit as-is so form validation works with partial input.
      if (!this.requireSelection) {
        this.emitChange(trimmed, trimmed);
      } else if (trimmed.length === 0) {
        this.lastSelectedDisplay = '';
        this.emitChange('', '');
      }
      return;
    }

    this.searchDebounceTimer = setTimeout(() => {
      void this.searchProjects(trimmed);
      // Emit raw typed value so the parent's bound value is never stale.
      // If the user later clicks a dropdown result, selectProject() overwrites
      // this with the resolved UUID.
      if (!this.requireSelection) {
        this.emitChange(trimmed, '');
      }
    }, 250);
  }

  private handleClear(): void {
    if (this.searchDebounceTimer) {
      clearTimeout(this.searchDebounceTimer);
    }
    this.searchQuery = '';
    this.activeDescendantIndex = -1;
    if (this.multiple) {
      if (this.searchOpen) {
        void this.searchProjects('');
      }
    } else {
      this.selectedViaDropdown = false;
      this.lastSelectedDisplay = '';
      this.emitChange('', '');
      if (this.isBrowseEnabled && this.searchOpen) {
        void this.searchProjects('');
      }
    }
  }

  private async searchProjects(query: string): Promise<void> {
    const requestId = ++this.searchRequestId;
    this.searchLoading = true;
    this.searchOpen = true;
    const limit = this.isBrowseEnabled || this.filterProject ? 50 : 10;
    const params = new URLSearchParams();
    if (query) params.set('search', query);
    params.set('limit', String(limit));
    try {
      const response = await apiFetch(`/api/v1/projects?${params.toString()}`);
      if (requestId !== this.searchRequestId) return; // stale
      if (response.ok) {
        const data = (await response.json()) as {
          projects?: ProjectOption[];
        };
        const projects = data.projects || [];
        for (const p of projects) {
          this.resolvedProjects.set(p.id, p);
        }
        this.searchResults = this.filterProject ? projects.filter(this.filterProject) : projects;
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
          const item = this.searchResults[this.activeDescendantIndex];
          if (this.multiple) {
            this.toggleProject(item);
          } else {
            this.selectProject(item);
          }
        }
        break;
      case 'Escape':
        e.preventDefault();
        e.stopPropagation();
        this.searchOpen = false;
        this.activeDescendantIndex = -1;
        break;
    }
  }

  private selectProject(project: ProjectOption): void {
    const display = formatProjectDisplay(project);
    this.resolvedProjects.set(project.id, project);
    this.searchQuery = display;
    this.lastSelectedDisplay = display;
    this.searchOpen = false;
    this.searchResults = [];
    this.selectedViaDropdown = true;
    this.activeDescendantIndex = -1;
    // Emit the project's UUID as projectId.
    this.emitChange(project.id, project.name);
  }

  private toggleProject(project: ProjectOption): void {
    this.resolvedProjects.set(project.id, project);
    const exists = this.values.includes(project.id);
    const nextIds = exists
      ? this.values.filter((id) => id !== project.id)
      : [...this.values, project.id];
    this.values = nextIds;
    const hadSearchQuery = this.searchQuery.trim().length > 0;
    if (this.searchQuery) {
      if (this.searchDebounceTimer) {
        clearTimeout(this.searchDebounceTimer);
        this.searchDebounceTimer = null;
      }
      this.searchQuery = '';
      this.activeDescendantIndex = -1;
    }
    this.emitMultiChange(nextIds);
    if (hadSearchQuery) {
      void this.searchProjects('');
    }
  }

  private removeProject(projectId: string): void {
    const nextIds = this.values.filter((id) => id !== projectId);
    this.values = nextIds;
    this.emitMultiChange(nextIds);
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

  private emitMultiChange(projectIds: string[]): void {
    const projects = projectIds.map(
      (id) => this.resolvedProjects.get(id) ?? { id, name: id, slug: '' }
    );
    this.dispatchEvent(
      new CustomEvent<ProjectsChangeDetail>('projects-change', {
        detail: { projectIds, projects },
        bubbles: true,
        composed: true,
      })
    );
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  private renderSearchInput(asAnchor: boolean) {
    const clearable = this.isBrowseEnabled || this.requireSelection;
    return html`
      <sl-input
        slot=${asAnchor ? 'anchor' : nothing}
        label=${this.label || nothing}
        aria-label=${this.label ? nothing : this.placeholder}
        placeholder=${this.placeholder}
        size=${this.size}
        value=${this.searchQuery}
        type="text"
        autocomplete="off"
        ?clearable=${clearable}
        ?disabled=${this.disabled}
        @sl-input=${(e: Event) => this.handleSearchInput(e)}
        @sl-clear=${() => this.handleClear()}
        @keydown=${(e: KeyboardEvent) => this.handleKeydown(e)}
        @sl-focus=${() => {
          if (this.blurTimeoutId) {
            clearTimeout(this.blurTimeoutId);
            this.blurTimeoutId = null;
          }
          if (this.isBrowseEnabled) {
            const query = this.selectedViaDropdown ? '' : this.searchQuery.trim();
            void this.searchProjects(query);
          } else if (this.searchResults.length > 0) {
            this.searchOpen = true;
          }
        }}
        @sl-blur=${() => {
          this.blurTimeoutId = setTimeout(() => {
            this.searchOpen = false;
            this.activeDescendantIndex = -1;
            if (this.multiple) {
              if (this.searchDebounceTimer) {
                clearTimeout(this.searchDebounceTimer);
                this.searchDebounceTimer = null;
              }
              this.searchQuery = '';
              return;
            }
            if (this.requireSelection) {
              if (!this.selectedViaDropdown) {
                this.searchQuery = this.lastSelectedDisplay;
                if (this.lastSelectedDisplay) {
                  this.selectedViaDropdown = true;
                }
              }
              return;
            }
            // Emit the raw typed value on blur if no dropdown selection was
            // made, so the parent has the latest value even when the
            // debounce timer hasn't fired yet.
            if (!this.selectedViaDropdown && this.searchQuery.trim()) {
              this.emitChange(this.searchQuery.trim(), '');
            }
          }, 200);
        }}
      ></sl-input>
    `;
  }

  private renderSearchDropdown(hoisted: boolean) {
    if (!this.searchOpen) return nothing;
    return html`
      <div
        class="project-search-dropdown ${hoisted ? 'hoisted' : ''}"
        id="project-picker-listbox"
        role="listbox"
        aria-label="Project search results"
        aria-multiselectable=${this.multiple ? 'true' : nothing}
      >
        ${this.searchLoading
          ? html`<div class="project-search-loading" role="status" aria-live="polite">
              <sl-spinner></sl-spinner> Searching...
            </div>`
          : this.searchResults.length === 0
            ? html`<div class="project-search-empty" role="status" aria-live="polite">
                ${this.emptyText}
              </div>`
            : this.searchResults.map((project, idx) => {
                const isSelected = this.multiple
                  ? this.values.includes(project.id)
                  : idx === this.activeDescendantIndex;
                return html`
                  <div
                    class="project-search-option ${idx === this.activeDescendantIndex
                      ? 'active-descendant'
                      : ''} ${this.multiple && isSelected ? 'is-selected' : ''}"
                    id="project-picker-option-${idx}"
                    role="option"
                    aria-selected=${isSelected ? 'true' : 'false'}
                    @mousedown=${(e: Event) => {
                      e.preventDefault();
                      if (this.multiple) {
                        this.toggleProject(project);
                      } else {
                        this.selectProject(project);
                      }
                    }}
                  >
                    <div class="project-option-info">
                      <span class="project-name">${project.name}</span>
                      <span class="project-slug">${project.slug}</span>
                    </div>
                    ${this.multiple && isSelected
                      ? html`<sl-icon class="project-check-icon" name="check-lg"></sl-icon>`
                      : nothing}
                  </div>
                `;
              })}
      </div>
    `;
  }

  private renderSelectedTags() {
    if (!this.multiple || this.values.length === 0) return nothing;
    return html`
      <div class="selected-tags" role="list" aria-label="Selected projects">
        ${this.values.map((id) => {
          const proj = this.resolvedProjects.get(id);
          const label = proj?.name || id;
          return html`
            <sl-tag
              role="listitem"
              size="small"
              variant="primary"
              removable
              @sl-remove=${() => this.removeProject(id)}
              >${label}</sl-tag
            >
          `;
        })}
      </div>
    `;
  }

  override render() {
    if (this.hoist) {
      return html`
        <div class="project-search-container">
          <sl-popup
            placement="bottom-start"
            strategy="fixed"
            sync="width"
            distance="4"
            flip
            ?active=${this.searchOpen}
          >
            ${this.renderSearchInput(true)} ${this.renderSearchDropdown(true)}
          </sl-popup>
          ${this.renderSelectedTags()}
        </div>
      `;
    }

    return html`
      <div class="project-search-container">
        ${this.renderSearchInput(false)} ${this.renderSearchDropdown(false)}
        ${this.renderSelectedTags()}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-picker': ScionProjectPicker;
  }
}
