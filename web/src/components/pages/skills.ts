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
 * Skills list page component
 *
 * Displays all skills with grid/table views, search, scope filter, and sorting.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Skill, SkillScope, Capabilities } from '../../shared/types.js';
import { can } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { listPageStyles } from '../shared/resource-styles.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import '../shared/view-toggle.js';

type SkillSortField = 'name' | 'updated' | 'created';
type SortDir = 'asc' | 'desc';

@customElement('scion-page-skills')
export class ScionPageSkills extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private skills: Skill[] = [];
  @state() private scopeCapabilities: Capabilities | undefined;
  @state() private viewMode: ViewMode = 'grid';
  @state() private searchQuery = '';
  @state() private scopeFilter: SkillScope | '' = '';
  @state() private sortField: SkillSortField = 'updated';
  @state() private sortDir: SortDir = 'desc';

  private searchTimer: ReturnType<typeof setTimeout> | null = null;

  static override styles = [
    listPageStyles,
    css`
      .skill-card {
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        padding: 1.5rem;
        transition: all var(--scion-transition-fast, 150ms ease);
        cursor: pointer;
        text-decoration: none;
        color: inherit;
        display: block;
      }

      .skill-card:hover {
        border-color: var(--scion-primary, #3b82f6);
        box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
        transform: translateY(-2px);
      }

      .skill-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 0.5rem;
      }

      .skill-meta {
        font-size: 0.813rem;
        color: var(--scion-text-muted, #64748b);
        display: flex;
        gap: 0.75rem;
        margin-top: 0.25rem;
      }

      .skill-description {
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
        margin-top: 0.75rem;
        overflow: hidden;
        text-overflow: ellipsis;
        display: -webkit-box;
        -webkit-line-clamp: 2;
        -webkit-box-orient: vertical;
      }

      .skill-tags {
        display: flex;
        flex-wrap: wrap;
        gap: 0.375rem;
        margin-top: 0.75rem;
      }

      .skill-tag {
        display: inline-block;
        font-size: 0.6875rem;
        padding: 0.125rem 0.5rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 9999px;
        color: var(--scion-text-muted, #64748b);
      }

      .skill-footer {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.75rem;
        padding-top: 0.75rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .scope-badge {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      .visibility-badge {
        display: inline-flex;
        align-items: center;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
      }

      .visibility-badge.public {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .visibility-badge.private {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      .filter-bar {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        margin-bottom: 1rem;
        flex-wrap: wrap;
      }

      .filter-bar .search-input {
        min-width: 200px;
      }

      th.sortable {
        cursor: pointer;
        user-select: none;
      }

      th.sortable:hover {
        color: var(--scion-text, #1e293b);
      }

      .sort-indicator {
        display: inline-block;
        margin-left: 0.25rem;
        font-size: 0.625rem;
        vertical-align: middle;
        opacity: 0.4;
      }

      th.sorted .sort-indicator {
        opacity: 1;
      }
    `,
  ];

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.searchTimer) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
  }

  override connectedCallback(): void {
    super.connectedCallback();

    const stored = localStorage.getItem('scion-view-skills') as ViewMode | null;
    if (stored === 'grid' || stored === 'list') {
      this.viewMode = stored;
    }

    const storedSort = localStorage.getItem('scion-sort-skills');
    if (storedSort) {
      try {
        const parsed = JSON.parse(storedSort);
        if (
          parsed &&
          (parsed.field === 'name' || parsed.field === 'updated' || parsed.field === 'created') &&
          (parsed.dir === 'asc' || parsed.dir === 'desc')
        ) {
          this.sortField = parsed.field;
          this.sortDir = parsed.dir;
        }
      } catch { /* ignore */ }
    }

    const ssrData = this.pageData?.data as { skills?: Skill[]; _capabilities?: Capabilities } | undefined;
    if (ssrData?.skills && this.scopeFilter === '' && !this.searchQuery) {
      this.skills = ssrData.skills;
      this.scopeCapabilities = ssrData._capabilities;
      this.loading = false;
    } else {
      void this.loadSkills();
    }
  }

  private async loadSkills(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const params = new URLSearchParams();
      params.set('status', 'active');
      if (this.scopeFilter) params.set('scope', this.scopeFilter);
      if (this.searchQuery) params.set('search', this.searchQuery);

      const response = await apiFetch(`/api/v1/skills?${params.toString()}`);
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { skills?: Skill[]; items?: Skill[]; _capabilities?: Capabilities } | Skill[];
      if (Array.isArray(data)) {
        this.skills = data;
        this.scopeCapabilities = undefined;
      } else {
        this.skills = data.skills || data.items || [];
        this.scopeCapabilities = data._capabilities;
      }
    } catch (err) {
      console.error('Failed to load skills:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load skills';
    } finally {
      this.loading = false;
    }
  }

  private get displaySkills(): Skill[] {
    const sorted = [...this.skills];
    sorted.sort((a, b) => {
      let cmp = 0;
      switch (this.sortField) {
        case 'name':
          cmp = (a.name || '').localeCompare(b.name || '');
          break;
        case 'updated':
          cmp = (a.updated || '').localeCompare(b.updated || '');
          break;
        case 'created':
          cmp = (a.created || '').localeCompare(b.created || '');
          break;
      }
      return this.sortDir === 'asc' ? cmp : -cmp;
    });
    return sorted;
  }

  private formatRelativeTime(isoString: string): string {
    const date = new Date(isoString);
    if (isNaN(date.getTime())) return '—';
    const now = Date.now();
    const diffMs = now - date.getTime();
    if (diffMs < 0) return 'just now';
    const seconds = Math.floor(diffMs / 1000);
    if (seconds < 60) return 'just now';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m ago`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    return `${days}d ago`;
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
  }

  private onSearchInput(e: Event): void {
    const value = (e.target as HTMLElement & { value: string }).value;
    if (this.searchTimer) clearTimeout(this.searchTimer);
    this.searchTimer = setTimeout(() => {
      this.searchQuery = value;
      void this.loadSkills();
    }, 300);
  }

  private onScopeFilterChange(e: Event): void {
    this.scopeFilter = (e.target as HTMLElement & { value: string }).value as SkillScope | '';
    void this.loadSkills();
  }

  private toggleSort(field: SkillSortField): void {
    if (this.sortField === field) {
      this.sortDir = this.sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      this.sortField = field;
      this.sortDir = field === 'name' ? 'asc' : 'desc';
    }
    localStorage.setItem('scion-sort-skills', JSON.stringify({ field: this.sortField, dir: this.sortDir }));
  }

  private sortIndicator(field: SkillSortField): string {
    return this.sortField === field ? (this.sortDir === 'asc' ? '▲' : '▼') : '▲';
  }

  override render() {
    return html`
      <div class="header">
        <h1>Skills</h1>
        <div class="header-actions">
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-skills"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.scopeCapabilities, 'create') ? html`
            <a href="/skills/new" style="text-decoration: none;">
              <sl-button variant="primary" size="small">
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Create Skill
              </sl-button>
            </a>
          ` : nothing}
        </div>
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : html`
        ${this.renderFilterBar()}
        ${this.renderSkills()}
      `}
    `;
  }

  private renderFilterBar() {
    return html`
      <div class="filter-bar">
        <sl-input
          class="search-input"
          size="small"
          placeholder="Search skills..."
          clearable
          @sl-input=${(e: Event) => this.onSearchInput(e)}
        >
          <sl-icon slot="prefix" name="search"></sl-icon>
        </sl-input>
        <sl-select
          size="small"
          placeholder="All scopes"
          clearable
          .value=${this.scopeFilter}
          @sl-change=${(e: Event) => this.onScopeFilterChange(e)}
          style="min-width: 140px;"
        >
          <sl-option value="core">Core</sl-option>
          <sl-option value="global">Global</sl-option>
          <sl-option value="project">Project</sl-option>
          <sl-option value="user">User</sl-option>
        </sl-select>
        ${this.viewMode === 'grid' ? html`
          <sl-dropdown>
            <sl-button slot="trigger" size="small" outline>
              <sl-icon slot="prefix" name=${this.sortDir === 'asc' ? 'sort-alpha-down' : 'sort-alpha-down-alt'}></sl-icon>
              Sort: ${this.sortField}
            </sl-button>
            <sl-menu @sl-select=${(e: CustomEvent<{ item: { value: string } }>) => this.toggleSort(e.detail.item.value as SkillSortField)}>
              <sl-menu-item value="name" ?checked=${this.sortField === 'name'}>Name</sl-menu-item>
              <sl-menu-item value="created" ?checked=${this.sortField === 'created'}>Created</sl-menu-item>
              <sl-menu-item value="updated" ?checked=${this.sortField === 'updated'}>Updated</sl-menu-item>
            </sl-menu>
          </sl-dropdown>
        ` : nothing}
      </div>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading skills...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Skills</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadSkills()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderSkills() {
    if (this.skills.length === 0) {
      return this.renderEmptyState();
    }

    const filtered = this.displaySkills;
    if (filtered.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="funnel"></sl-icon>
          <h2>No Matching Skills</h2>
          <p>No skills match the current filters.</p>
        </div>
      `;
    }

    return this.viewMode === 'grid' ? this.renderGrid() : this.renderTable();
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="lightning-charge"></sl-icon>
        <h2>No Skills Found</h2>
        <p>
          Skills are reusable capabilities for agents.${can(this.scopeCapabilities, 'create') ? ' Create your first skill to get started.' : ''}
        </p>
        ${can(this.scopeCapabilities, 'create') ? html`
          <a href="/skills/new" style="text-decoration: none;">
            <sl-button variant="primary">
              <sl-icon slot="prefix" name="plus-lg"></sl-icon>
              Create Skill
            </sl-button>
          </a>
        ` : nothing}
      </div>
    `;
  }

  private renderGrid() {
    return html`
      <div class="resource-grid">${this.displaySkills.map((skill) => this.renderSkillCard(skill))}</div>
    `;
  }

  private renderSkillCard(skill: Skill) {
    return html`
      <a href="/skills/${skill.id}" class="skill-card">
        <div class="skill-header">
          <div>
            <h3 class="resource-name">
              <sl-icon name="lightning-charge"></sl-icon>
              ${skill.name}
            </h3>
            <div class="skill-meta">
              <span class="scope-badge">${skill.scope}</span>
              <span class="visibility-badge ${skill.visibility}">${skill.visibility}</span>
            </div>
          </div>
        </div>

        ${skill.description ? html`<div class="skill-description">${skill.description}</div>` : nothing}

        ${skill.tags?.length ? html`
          <div class="skill-tags">
            ${skill.tags.map((tag) => html`<span class="skill-tag">${tag}</span>`)}
          </div>
        ` : nothing}

        <div class="skill-footer">
          Updated ${this.formatRelativeTime(skill.updated)}
        </div>
      </a>
    `;
  }

  private renderTable() {
    return html`
      <div class="resource-table-container">
        <table>
          <thead>
            <tr>
              <th
                class="sortable ${this.sortField === 'name' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('name')}
              >Name <span class="sort-indicator">${this.sortIndicator('name')}</span></th>
              <th>Scope</th>
              <th class="hide-mobile">Visibility</th>
              <th class="hide-mobile">Tags</th>
              <th
                class="sortable ${this.sortField === 'updated' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('updated')}
              >Updated <span class="sort-indicator">${this.sortIndicator('updated')}</span></th>
            </tr>
          </thead>
          <tbody>
            ${this.displaySkills.map((skill) => this.renderSkillRow(skill))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderSkillRow(skill: Skill) {
    return html`
      <tr class="clickable" @click=${() => { window.history.pushState({}, '', `/skills/${skill.id}`); window.dispatchEvent(new PopStateEvent('popstate')); }}>
        <td>
          <span class="name-cell">
            <sl-icon name="lightning-charge"></sl-icon>
            <a href="/skills/${skill.id}">${skill.name}</a>
          </span>
        </td>
        <td><span class="scope-badge">${skill.scope}</span></td>
        <td class="hide-mobile"><span class="visibility-badge ${skill.visibility}">${skill.visibility}</span></td>
        <td class="hide-mobile">
          ${skill.tags?.length
            ? skill.tags.map((tag) => html`<span class="skill-tag">${tag}</span> `)
            : '—'}
        </td>
        <td>${skill.updated ? this.formatRelativeTime(skill.updated) : '—'}</td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-skills': ScionPageSkills;
  }
}
