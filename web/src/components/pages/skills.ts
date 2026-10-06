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
import { paginateAll, PaginationError, PaginationStoppedError } from '../../client/paginate-all.js';
import { listPageStyles } from '../shared/resource-styles.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import '../shared/view-toggle.js';
import { formatRelative } from '../../utils/time.js';
import { navigateTo } from '../../client/navigation.js';

/** Skills requested per page; the server's maximum list limit. */
const SKILLS_PAGE_SIZE = 200;

type SkillSortField = 'name' | 'updated' | 'created';
type SortDir = 'asc' | 'desc';

@customElement('scion-page-skills')
export class ScionPageSkills extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private loading = true;
  /**
   * True once any load has settled (or the server prefetch was used). After
   * that, reloads keep the current UI rendered with an inline spinner rather
   * than swapping it for the full-page one, so the focused control (search
   * input, Retry button) is not removed (ptone/scion#2948).
   */
  @state() private hasLoaded = false;
  @state() private error: string | null = null;
  @state() private skills: Skill[] = [];
  @state() private scopeCapabilities: Capabilities | undefined;
  /** Set when a page after the first failed; the loaded pages are still shown. */
  @state() private partialLoadError: string | null = null;
  private loadGeneration = 0;
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

      /* The header's only child holds the name and scope. As a flex item it
         defaults to min-width:auto and grows to fit a long unbroken name,
         pushing it past the card edge; min-width:0 lets it shrink so the
         shared wrapping rules can break the name instead. */
      .skill-header > div {
        min-width: 0;
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

      .filter-bar .inline-loading {
        font-size: 1rem;
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
    } else {
      this.viewMode = 'grid';
      localStorage.setItem('scion-view-skills', 'grid');
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
      } catch {
        /* ignore */
      }
    }

    const ssrData = this.pageData?.data as
      | { skills?: Skill[]; nextCursor?: string; _capabilities?: Capabilities }
      | undefined;
    // The server prefetch is a single page; when it has more, walk every
    // page on the client instead (ptone/scion#1949).
    if (ssrData?.skills && !ssrData.nextCursor && this.scopeFilter === '' && !this.searchQuery) {
      this.skills = ssrData.skills;
      this.scopeCapabilities = ssrData._capabilities;
      this.loading = false;
      this.hasLoaded = true;
    } else {
      void this.loadSkills();
    }
  }

  private async loadSkills(): Promise<void> {
    const generation = ++this.loadGeneration;
    this.loading = true;
    // The current error or partial-load notice stays up (with its Retry
    // button) until this load settles, so focus on Retry is not lost.
    const focusWasInside = this.shadowRoot?.activeElement != null;

    // Pages are collected here as they arrive, so a failure after the first
    // page can still show what was loaded (ptone/scion#1949).
    const loaded: Skill[] = [];
    let capabilities: Capabilities | undefined;
    let firstPage = true;

    try {
      const params = new URLSearchParams();
      params.set('status', 'active');
      if (this.scopeFilter) params.set('scope', this.scopeFilter);
      if (this.searchQuery) params.set('search', this.searchQuery);

      await paginateAll<Skill>({
        path: `/api/v1/skills?${params.toString()}`,
        pageSize: SKILLS_PAGE_SIZE,
        label: 'Skills',
        // A newer load (search or scope change) supersedes this one, and a
        // detached page needs no more pages.
        shouldContinue: () => generation === this.loadGeneration && this.isConnected,
        parsePage: (body) => {
          const data = body as {
            skills?: Skill[];
            items?: Skill[];
            nextCursor?: string;
            _capabilities?: Capabilities;
          };
          // Scope capabilities (e.g. create) come with the first page.
          if (firstPage) capabilities = data._capabilities;
          firstPage = false;
          const items = data.skills || data.items || [];
          loaded.push(...items);
          return { items, ...(data.nextCursor ? { nextCursor: data.nextCursor } : {}) };
        },
      });
      if (generation !== this.loadGeneration) return;
      this.skills = loaded;
      this.scopeCapabilities = capabilities;
      this.error = null;
      this.partialLoadError = null;
    } catch (err) {
      if (generation !== this.loadGeneration || err instanceof PaginationStoppedError) return;
      console.error('Failed to load skills:', err);
      const message = err instanceof Error ? err.message : 'Failed to load skills';
      // Lead with the hub's explanation when it sent one (ptone/scion#2949).
      const hubMessage = err instanceof PaginationError ? err.hubMessage : undefined;
      if (firstPage) {
        // paginateAll's errors are terse ("Skills request failed: 403"), so
        // say what failed; other errors (e.g. network) read as they are.
        this.error =
          err instanceof PaginationError
            ? hubMessage
              ? `Failed to load skills: ${hubMessage} (${message})`
              : `Failed to load skills (${message})`
            : message;
        this.partialLoadError = null;
      } else {
        // A later page failed: keep the pages that loaded and say so.
        this.skills = loaded;
        this.scopeCapabilities = capabilities;
        this.error = null;
        this.partialLoadError = hubMessage ? `${hubMessage}; ${message}` : message;
      }
    } finally {
      if (generation === this.loadGeneration) {
        this.loading = false;
        this.hasLoaded = true;
        if (focusWasInside) void this.restoreFocus();
      }
    }
  }

  /**
   * After a load that started with focus inside the page: if the focused
   * control was removed by the re-render (e.g. Retry, once the error or
   * notice clears), move focus to the search input instead of leaving it on
   * the document body (ptone/scion#2948).
   */
  private async restoreFocus(): Promise<void> {
    await this.updateComplete;
    if (!this.isConnected || this.shadowRoot?.activeElement) return;
    const search = this.shadowRoot?.querySelector<HTMLElement>('.search-input');
    search?.focus();
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
    const ms = new Date(isoString).getTime();
    if (Number.isNaN(ms)) return '—';
    // A future instant is clock skew between hub and browser.
    if (ms > Date.now()) return 'just now';
    return formatRelative(isoString, { style: 'narrow' });
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
    localStorage.setItem(
      'scion-sort-skills',
      JSON.stringify({ field: this.sortField, dir: this.sortDir })
    );
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
            .showGraph=${false}
            storageKey="scion-view-skills"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
          ${can(this.scopeCapabilities, 'create')
            ? html`
                <a href="/skills/new" style="text-decoration: none;">
                  <sl-button variant="primary" size="small">
                    <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                    Create Skill
                  </sl-button>
                </a>
              `
            : nothing}
        </div>
      </div>

      ${this.loading && !this.hasLoaded
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : html`
              ${this.renderFilterBar()} ${this.renderPartialLoadNotice()} ${this.renderSkills()}
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
          <sl-option value="">All Scopes</sl-option>
          <sl-option value="core">Core</sl-option>
          <sl-option value="global">Global</sl-option>
          <sl-option value="project">Project</sl-option>
          <sl-option value="user">User</sl-option>
        </sl-select>
        ${this.viewMode === 'grid'
          ? html`
              <sl-dropdown>
                <sl-button slot="trigger" size="small" outline>
                  <sl-icon
                    slot="prefix"
                    name=${this.sortDir === 'asc' ? 'sort-alpha-down' : 'sort-alpha-down-alt'}
                  ></sl-icon>
                  Sort: ${this.sortField}
                </sl-button>
                <sl-menu
                  @sl-select=${(e: CustomEvent<{ item: { value: string } }>) =>
                    this.toggleSort(e.detail.item.value as SkillSortField)}
                >
                  <sl-menu-item value="name" ?checked=${this.sortField === 'name'}
                    >Name</sl-menu-item
                  >
                  <sl-menu-item value="created" ?checked=${this.sortField === 'created'}
                    >Created</sl-menu-item
                  >
                  <sl-menu-item value="updated" ?checked=${this.sortField === 'updated'}
                    >Updated</sl-menu-item
                  >
                </sl-menu>
              </sl-dropdown>
            `
          : nothing}
        ${this.loading
          ? html`<sl-spinner class="inline-loading" aria-label="Loading skills"></sl-spinner>`
          : nothing}
      </div>
    `;
  }

  private renderPartialLoadNotice() {
    if (!this.partialLoadError) return nothing;
    return html`
      <sl-alert class="partial-load-notice" variant="warning" open>
        <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
        Showing ${this.skills.length} skill${this.skills.length === 1 ? '' : 's'}; the rest could
        not be loaded (${this.partialLoadError}).
        <sl-button
          size="small"
          variant="text"
          class="partial-load-retry"
          ?loading=${this.loading}
          @click=${() => this.loadSkills()}
          >Retry</sl-button
        >
      </sl-alert>
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
        <sl-button
          variant="primary"
          class="error-retry"
          ?loading=${this.loading}
          @click=${() => this.loadSkills()}
        >
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
          Skills are reusable capabilities for
          agents.${can(this.scopeCapabilities, 'create')
            ? ' Create your first skill to get started.'
            : ''}
        </p>
        ${can(this.scopeCapabilities, 'create')
          ? html`
              <a href="/skills/new" style="text-decoration: none;">
                <sl-button variant="primary">
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  Create Skill
                </sl-button>
              </a>
            `
          : nothing}
      </div>
    `;
  }

  private renderGrid() {
    return html`
      <div class="resource-grid">
        ${this.displaySkills.map((skill) => this.renderSkillCard(skill))}
      </div>
    `;
  }

  private renderSkillCard(skill: Skill) {
    return html`
      <a href="/skills/${skill.id}" class="skill-card">
        <div class="skill-header">
          <div>
            <h3 class="resource-name">
              <sl-icon name="lightning-charge"></sl-icon>
              <span>${skill.name}</span>
            </h3>
            <div class="skill-meta">
              <span class="scope-badge">${skill.scope}</span>
            </div>
          </div>
        </div>

        ${skill.description
          ? html`<div class="skill-description">${skill.description}</div>`
          : nothing}
        ${skill.tags?.length
          ? html`
              <div class="skill-tags">
                ${skill.tags.map((tag) => html`<span class="skill-tag">${tag}</span>`)}
              </div>
            `
          : nothing}

        <div class="skill-footer">Updated ${this.formatRelativeTime(skill.updated)}</div>
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
              >
                Name <span class="sort-indicator">${this.sortIndicator('name')}</span>
              </th>
              <th>Scope</th>
              <th class="hide-mobile">Tags</th>
              <th
                class="sortable ${this.sortField === 'updated' ? 'sorted' : ''}"
                @click=${() => this.toggleSort('updated')}
              >
                Updated <span class="sort-indicator">${this.sortIndicator('updated')}</span>
              </th>
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
      <tr
        class="clickable"
        @click=${() => {
          navigateTo(`/skills/${skill.id}`);
        }}
      >
        <td>
          <span class="name-cell">
            <sl-icon name="lightning-charge"></sl-icon>
            <a href="/skills/${skill.id}">${skill.name}</a>
          </span>
        </td>
        <td><span class="scope-badge">${skill.scope}</span></td>
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
