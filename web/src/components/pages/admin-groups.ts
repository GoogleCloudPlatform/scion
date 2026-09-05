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
 * Admin Groups list page (G2 + G3 wiring).
 *
 * Full-featured list page with debounced server-side search, type filter,
 * "Owned by me" checkbox, cursor pagination with page-token back-stack,
 * All groups / My groups tabs, capability-gated header, and create dialog.
 *
 * Data goes through groups-api.ts; capabilities use canGroup() from
 * shared/groups.ts. No raw fetch calls, no role-name checks.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { setDocumentTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/main.js';
import { listGroups, listMyGroups, GroupsApiError } from '../../client/groups-api.js';
import type {
  AdminGroup,
  GroupType,
  Capabilities,
  ListGroupsResponse,
} from '../../shared/groups.js';
import { canGroup } from '../../shared/groups.js';
import { showToast } from '../../utils/toast.js';
import { formatRelativeTime } from '../../utils/time.js';

// Import shared form dialog
import '../shared/group-form-dialog.js';

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const PAGE_SIZE = 25;
const SEARCH_DEBOUNCE_MS = 300;

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-page-admin-groups')
export class ScionPageAdminGroups extends LitElement {
  // --- Data state ---
  @state() private loading = true;
  @state() private groups: AdminGroup[] = [];
  @state() private totalCount = 0;
  @state() private nextCursor: string | undefined;
  @state() private error: string | null = null;
  @state() private permissionDenied = false;
  @state() private listCapabilities: Capabilities | undefined;

  // --- Filter state ---
  @state() private searchQuery = '';
  @state() private filterGroupType: '' | GroupType = '';
  @state() private filterOwnedByMe = false;

  // --- Tab state ---
  @state() private activeTab: 'all' | 'mine' = 'all';
  @state() private myGroups: AdminGroup[] = [];
  @state() private myGroupsLoading = false;
  @state() private myGroupsError: string | null = null;

  // --- Pagination ---
  @state() private pageTokenStack: string[] = [];
  @state() private currentCursor: string | undefined;

  // --- Dialog state ---
  @state() private showCreateDialog = false;

  // --- Misc ---
  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private abortController: AbortController | null = null;
  private currentUserId: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    /* Header */
    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.5rem;
      flex-wrap: wrap;
      gap: 0.75rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-subtitle {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .header-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    /* Tabs */
    sl-tab-group {
      margin-bottom: 1rem;
    }

    /* Improve active tab text contrast (4.5:1 on white) */
    sl-tab::part(base) {
      color: var(--scion-text-secondary, #475569);
    }

    sl-tab[active]::part(base) {
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    /* Filters */
    .filter-bar {
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
      margin-bottom: 1rem;
      align-items: flex-end;
    }

    .filter-bar sl-input {
      flex: 1 1 200px;
      min-width: 200px;
      max-width: 400px;
    }

    .filter-bar sl-select {
      flex: 0 1 180px;
      min-width: 140px;
    }

    .filter-bar sl-checkbox {
      align-self: center;
    }

    .filter-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-left: auto;
    }

    .active-filter-count {
      font-size: 0.75rem;
      color: var(--sl-color-primary-600, #2563eb);
      font-weight: 500;
    }

    /* Table */
    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-secondary, #475569);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    tr.clickable {
      cursor: pointer;
      transition: background-color 0.15s ease;
    }

    tr.clickable:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    tr.clickable:focus-within {
      outline: 2px solid var(--sl-color-primary-600, #2563eb);
      outline-offset: -2px;
    }

    .group-identity {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .group-icon {
      width: 2rem;
      height: 2rem;
      border-radius: 0.5rem;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }

    .group-icon.explicit {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-600, #2563eb);
    }

    .group-icon.project_agents {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-600, #16a34a);
    }

    .group-icon sl-icon {
      font-size: 1rem;
    }

    .group-info {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .group-name-link {
      font-weight: 500;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      color: var(--scion-text, #1e293b);
      text-decoration: none;
    }

    .group-name-link:hover {
      text-decoration: underline;
      color: var(--sl-color-primary-600, #2563eb);
    }

    .group-name-link:focus-visible {
      outline: 2px solid var(--sl-color-primary-600, #2563eb);
      outline-offset: 2px;
      border-radius: 2px;
    }

    .group-slug {
      font-size: 0.75rem;
      font-family: var(--scion-font-mono, monospace);
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .type-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .type-badge.explicit {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .type-badge.project_agents {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .description-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      max-width: 300px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .labels-container {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
    }

    .label-tag {
      display: inline-flex;
      align-items: center;
      padding: 0.0625rem 0.375rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.6875rem;
      font-family: var(--scion-font-mono, monospace);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-secondary, #475569);
    }

    /* Pagination */
    .pagination {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 1rem;
      padding: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .pagination-info {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* State screens */
    .empty-state {
      text-align: center;
      padding: 4rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .empty-state > sl-icon {
      font-size: 4rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 1rem;
    }

    .empty-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 0.5rem 0;
      max-width: 480px;
      margin-left: auto;
      margin-right: auto;
    }

    .empty-state sl-button {
      margin-top: 1rem;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    /* Loading skeleton */
    .skeleton-table {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .skeleton-row {
      display: flex;
      gap: 1rem;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .skeleton-row:last-child {
      border-bottom: none;
    }

    .skeleton-cell {
      height: 1rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      animation: skeleton-pulse 1.5s ease-in-out infinite;
    }

    @keyframes skeleton-pulse {
      0%,
      100% {
        opacity: 1;
      }
      50% {
        opacity: 0.4;
      }
    }

    /* Visually hidden */
    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    /* Live region */
    .result-count-live {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.5rem;
    }

    /* Permission denied state */
    .permission-denied-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .permission-denied-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-warning-500, #eab308);
      margin-bottom: 1rem;
    }

    .permission-denied-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .permission-denied-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }

      .filter-bar {
        flex-direction: column;
      }

      .filter-bar sl-input,
      .filter-bar sl-select {
        flex: 1 1 auto;
        min-width: 0;
        max-width: none;
      }

      .filter-actions {
        margin-left: 0;
        width: 100%;
        justify-content: flex-end;
      }
    }

    @media (prefers-reduced-motion: reduce) {
      tr.clickable {
        transition: none;
      }

      .skeleton-cell {
        animation: none;
      }
    }

    @media (forced-colors: active) {
      .type-badge {
        border: 1px solid ButtonText;
      }

      .table-container {
        border: 1px solid ButtonText;
      }

      tr.clickable:focus-within {
        outline: 2px solid Highlight;
      }

      .skeleton-cell {
        border: 1px solid GrayText;
      }
    }
  `;

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override connectedCallback(): void {
    super.connectedCallback();
    setDocumentTitle('Groups');
    this.readFiltersFromURL();
    void this.loadCurrentUser();
    void this.loadData();
    // If deep-linking to ?tab=mine, load my groups immediately.
    if (this.activeTab === 'mine') {
      void this.loadMyGroups();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.cancelPendingSearch();
    this.abortController?.abort();
  }

  // ---------------------------------------------------------------------------
  // Current user
  // ---------------------------------------------------------------------------

  private async loadCurrentUser(): Promise<void> {
    try {
      const res = await fetch('/auth/me', { credentials: 'include' });
      if (res.ok) {
        const data = (await res.json()) as { id?: string };
        this.currentUserId = data.id || null;
      }
    } catch {
      // Non-critical — "Owned by me" filter just won't work
    }
  }

  // ---------------------------------------------------------------------------
  // URL state management
  // ---------------------------------------------------------------------------

  readFiltersFromURL(): void {
    const params = new URLSearchParams(window.location.search);
    this.searchQuery = params.get('q') ?? '';
    this.filterGroupType = (params.get('groupType') ?? '') as typeof this.filterGroupType;
    this.filterOwnedByMe = params.get('owner') === 'me';
    this.activeTab = params.get('tab') === 'mine' ? 'mine' : 'all';
    const cursor = params.get('cursor') ?? undefined;
    this.currentCursor = cursor;
    // When deep-linking with a cursor, seed the page-token stack so the pager
    // knows the user is past page 1 and enables the Previous button.
    if (cursor) {
      this.pageTokenStack = [''];
    }
  }

  syncFiltersToURL(): void {
    const params = new URLSearchParams();
    if (this.searchQuery) params.set('q', this.searchQuery);
    if (this.filterGroupType) params.set('groupType', this.filterGroupType);
    if (this.filterOwnedByMe) params.set('owner', 'me');
    if (this.activeTab === 'mine') params.set('tab', 'mine');
    if (this.currentCursor) params.set('cursor', this.currentCursor);

    const qs = params.toString();
    const newUrl = `${window.location.pathname}${qs ? `?${qs}` : ''}`;
    window.history.replaceState({}, '', newUrl);
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;
    this.permissionDenied = false;

    this.abortController?.abort();
    this.abortController = new AbortController();

    try {
      const filter: import('../../shared/groups.js').GroupsFilter = {
        limit: PAGE_SIZE,
      };
      if (this.searchQuery) filter.search = this.searchQuery;
      if (this.filterGroupType) filter.groupType = this.filterGroupType;
      if (this.filterOwnedByMe && this.currentUserId) filter.ownerId = this.currentUserId;
      if (this.currentCursor) filter.cursor = this.currentCursor;

      const data: ListGroupsResponse = await listGroups(filter, this.abortController.signal);

      this.groups = data.groups ?? [];
      this.totalCount = data.totalCount ?? 0;
      this.nextCursor = data.nextCursor;
      this.listCapabilities = data._capabilities;
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') {
        return;
      }
      console.error('Failed to load groups:', err);
      if (err instanceof GroupsApiError) {
        if (err.httpStatus === 403) {
          this.permissionDenied = true;
          this.error = null;
        } else {
          this.error = err.message || `HTTP ${err.httpStatus}`;
        }
      } else {
        this.error = err instanceof Error ? err.message : 'Failed to load groups';
      }
    } finally {
      this.loading = false;
    }
  }

  private async loadMyGroups(): Promise<void> {
    this.myGroupsLoading = true;
    this.myGroupsError = null;

    try {
      this.myGroups = await listMyGroups();
    } catch (err) {
      console.error('Failed to load my groups:', err);
      this.myGroupsError = err instanceof Error ? err.message : 'Failed to load your groups';
    } finally {
      this.myGroupsLoading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Search
  // ---------------------------------------------------------------------------

  private cancelPendingSearch(): void {
    if (this.searchTimer !== null) {
      clearTimeout(this.searchTimer);
      this.searchTimer = null;
    }
  }

  private onSearchInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.cancelPendingSearch();
    this.searchTimer = setTimeout(() => {
      this.searchQuery = value;
      this.resetPagination();
      this.syncFiltersToURL();
      void this.loadData();
    }, SEARCH_DEBOUNCE_MS);
  }

  // ---------------------------------------------------------------------------
  // Filters
  // ---------------------------------------------------------------------------

  private onGroupTypeChange(e: Event): void {
    this.filterGroupType = (e.target as HTMLSelectElement).value as typeof this.filterGroupType;
    this.resetPagination();
    this.syncFiltersToURL();
    void this.loadData();
  }

  private onOwnedByMeChange(e: Event): void {
    this.filterOwnedByMe = (e.target as HTMLInputElement).checked;
    this.resetPagination();
    this.syncFiltersToURL();
    void this.loadData();
  }

  get hasActiveFilters(): boolean {
    return !!(this.searchQuery || this.filterGroupType || this.filterOwnedByMe);
  }

  get activeFilterCount(): number {
    let count = 0;
    if (this.searchQuery) count++;
    if (this.filterGroupType) count++;
    if (this.filterOwnedByMe) count++;
    return count;
  }

  private clearFilters(): void {
    this.searchQuery = '';
    this.filterGroupType = '';
    this.filterOwnedByMe = false;
    this.resetPagination();
    this.syncFiltersToURL();
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Tabs
  // ---------------------------------------------------------------------------

  private onTabChange(e: CustomEvent): void {
    const tab = (e.detail as { name: string }).name;
    this.activeTab = tab === 'mine' ? 'mine' : 'all';
    this.syncFiltersToURL();
    if (this.activeTab === 'mine' && this.myGroups.length === 0 && !this.myGroupsLoading) {
      void this.loadMyGroups();
    }
  }

  // ---------------------------------------------------------------------------
  // Pagination
  // ---------------------------------------------------------------------------

  resetPagination(): void {
    this.currentCursor = undefined;
    this.nextCursor = undefined;
    this.pageTokenStack = [];
  }

  goNextPage(): void {
    if (!this.nextCursor) return;
    if (this.currentCursor) {
      this.pageTokenStack = [...this.pageTokenStack, this.currentCursor];
    } else {
      this.pageTokenStack = [...this.pageTokenStack, ''];
    }
    this.currentCursor = this.nextCursor;
    this.syncFiltersToURL();
    void this.loadData();
  }

  goPreviousPage(): void {
    if (this.pageTokenStack.length === 0) return;
    const prevToken = this.pageTokenStack[this.pageTokenStack.length - 1];
    this.pageTokenStack = this.pageTokenStack.slice(0, -1);
    this.currentCursor = prevToken || undefined;
    this.syncFiltersToURL();
    void this.loadData();
  }

  get currentPageNumber(): number {
    return this.pageTokenStack.length + 1;
  }

  get hasPreviousPage(): boolean {
    return this.pageTokenStack.length > 0;
  }

  get hasNextPage(): boolean {
    return !!this.nextCursor;
  }

  // ---------------------------------------------------------------------------
  // Display helpers
  // ---------------------------------------------------------------------------

  private formatRelativeTime(dateString: string): string {
    return formatRelativeTime(dateString);
  }

  private get canCreate(): boolean {
    return canGroup(this.listCapabilities, 'create');
  }

  // ---------------------------------------------------------------------------
  // Navigation
  // ---------------------------------------------------------------------------

  private navigateToGroup(id: string): void {
    navigateTo(`/admin/groups/${encodeURIComponent(id)}`);
  }

  // ---------------------------------------------------------------------------
  // Create dialog
  // ---------------------------------------------------------------------------

  private openCreateDialog(): void {
    this.showCreateDialog = true;
  }

  private onGroupSaved(e: CustomEvent<AdminGroup>): void {
    this.showCreateDialog = false;
    showToast('Group created', 'success');
    navigateTo(`/admin/groups/${encodeURIComponent(e.detail.id)}`);
  }

  private onGroupFormCancel(): void {
    this.showCreateDialog = false;
    this.returnFocusToCreateBtn();
  }

  /** Return focus to the create button after the dialog closes. */
  private returnFocusToCreateBtn(): void {
    requestAnimationFrame(() => {
      const btn = this.shadowRoot?.querySelector<HTMLElement>('#create-group-btn');
      btn?.focus();
    });
  }

  // ---------------------------------------------------------------------------
  // Refresh
  // ---------------------------------------------------------------------------

  private refresh(): void {
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="header">
        <h1>Groups</h1>
        <div class="header-actions">
          ${this.canCreate
            ? html`
                <sl-button
                  id="create-group-btn"
                  variant="primary"
                  size="small"
                  @click=${() => this.openCreateDialog()}
                >
                  <sl-icon slot="prefix" name="plus-lg" aria-hidden="true"></sl-icon>
                  Create group
                </sl-button>
              `
            : nothing}
        </div>
      </div>
      <p class="header-subtitle">
        Manage membership groups used across projects and access control.
      </p>

      <sl-tab-group @sl-tab-show=${(e: CustomEvent) => this.onTabChange(e)}>
        <sl-tab slot="nav" panel="all" ?active=${this.activeTab === 'all'}>All groups</sl-tab>
        <sl-tab slot="nav" panel="mine" ?active=${this.activeTab === 'mine'}>My groups</sl-tab>

        <sl-tab-panel name="all"> ${this.renderAllGroupsTab()} </sl-tab-panel>
        <sl-tab-panel name="mine"> ${this.renderMyGroupsTab()} </sl-tab-panel>
      </sl-tab-group>

      ${this.showCreateDialog
        ? html`
            <scion-group-form-dialog
              mode="create"
              .open=${true}
              @group-saved=${(e: CustomEvent<AdminGroup>) => this.onGroupSaved(e)}
              @group-form-cancel=${() => this.onGroupFormCancel()}
            ></scion-group-form-dialog>
          `
        : nothing}
    `;
  }

  // ---------------------------------------------------------------------------
  // All groups tab
  // ---------------------------------------------------------------------------

  private renderAllGroupsTab() {
    return html`
      ${this.renderFilterBar()}
      <div class="result-count-live" role="status" aria-live="polite" aria-atomic="true">
        ${!this.loading && this.groups.length > 0
          ? html`${this.totalCount}
            group${this.totalCount !== 1 ? 's' : ''}${this.hasActiveFilters
              ? ' matching filters'
              : ''}`
          : nothing}
      </div>
      ${this.loading && this.groups.length === 0
        ? this.renderLoadingSkeleton()
        : this.permissionDenied
          ? this.renderPermissionDenied()
          : this.error
            ? this.renderError()
            : this.groups.length === 0
              ? this.renderEmpty()
              : this.renderGroupsTable(this.groups, true)}
    `;
  }

  // ---------------------------------------------------------------------------
  // My groups tab
  // ---------------------------------------------------------------------------

  private renderMyGroupsTab() {
    if (this.myGroupsLoading && this.myGroups.length === 0) {
      return html`
        <div class="loading-state" role="status" aria-label="Loading your groups">
          <sl-spinner></sl-spinner>
          <p>Loading your groups...</p>
        </div>
      `;
    }

    if (this.myGroupsError) {
      return html`
        <div class="error-state" role="alert">
          <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
          <h2>Failed to Load Your Groups</h2>
          <p>There was a problem loading your group memberships.</p>
          <div class="error-details">${this.myGroupsError}</div>
          <sl-button variant="primary" @click=${() => this.loadMyGroups()}>
            <sl-icon slot="prefix" name="arrow-clockwise" aria-hidden="true"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    if (this.myGroups.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="diagram-3" aria-hidden="true"></sl-icon>
          <h2>No Group Memberships</h2>
          <p>You are not a member of any groups yet.</p>
        </div>
      `;
    }

    return html`
      <div class="result-count-live" role="status" aria-live="polite" aria-atomic="true">
        ${this.myGroups.length} group${this.myGroups.length !== 1 ? 's' : ''}
      </div>
      ${this.renderGroupsTable(this.myGroups, false)}
    `;
  }

  // ---------------------------------------------------------------------------
  // Filter bar
  // ---------------------------------------------------------------------------

  private renderFilterBar() {
    return html`
      <div class="filter-bar">
        <sl-input
          placeholder="Search name, slug, description..."
          size="small"
          clearable
          .value=${this.searchQuery}
          @sl-input=${(e: Event) => this.onSearchInput(e)}
          @sl-clear=${() => {
            this.searchQuery = '';
            this.resetPagination();
            this.syncFiltersToURL();
            void this.loadData();
          }}
        >
          <sl-icon name="search" slot="prefix" aria-hidden="true"></sl-icon>
        </sl-input>

        <sl-select
          placeholder="Type"
          size="small"
          clearable
          .value=${this.filterGroupType}
          @sl-change=${(e: Event) => this.onGroupTypeChange(e)}
        >
          <sl-option value="explicit">Explicit</sl-option>
          <sl-option value="project_agents">Project agents</sl-option>
        </sl-select>

        <sl-checkbox
          size="small"
          ?checked=${this.filterOwnedByMe}
          @sl-change=${(e: Event) => this.onOwnedByMeChange(e)}
        >
          Owned by me
        </sl-checkbox>

        <div class="filter-actions">
          ${this.hasActiveFilters
            ? html`
                <span class="active-filter-count"
                  >${this.activeFilterCount} filter${this.activeFilterCount !== 1 ? 's' : ''}</span
                >
                <sl-button variant="text" size="small" @click=${() => this.clearFilters()}>
                  <sl-icon slot="prefix" name="x-lg" aria-hidden="true"></sl-icon>
                  Clear filters
                </sl-button>
              `
            : nothing}
          <sl-icon-button
            name="arrow-clockwise"
            label="Refresh"
            @click=${() => this.refresh()}
          ></sl-icon-button>
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Loading skeleton
  // ---------------------------------------------------------------------------

  private renderLoadingSkeleton() {
    return html`
      <div class="skeleton-table" role="status" aria-label="Loading groups" aria-live="polite">
        ${Array.from(
          { length: 5 },
          (_, i) => html`
            <div class="skeleton-row" key=${i}>
              <div class="skeleton-cell" style="flex: 2; min-width: 120px;"></div>
              <div class="skeleton-cell" style="flex: 1; min-width: 60px;"></div>
              <div class="skeleton-cell" style="flex: 1.5; min-width: 80px;"></div>
              <div class="skeleton-cell" style="flex: 1; min-width: 60px;"></div>
              <div class="skeleton-cell" style="flex: 0.75; min-width: 50px;"></div>
            </div>
          `
        )}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Error / permission denied
  // ---------------------------------------------------------------------------

  private renderError() {
    return html`
      <div class="error-state" role="alert">
        <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
        <h2>Failed to Load Groups</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise" aria-hidden="true"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderPermissionDenied() {
    return html`
      <div class="permission-denied-state" role="alert">
        <sl-icon name="shield-lock" aria-hidden="true"></sl-icon>
        <h2>Permission Denied</h2>
        <p>
          You do not have the required permission to view groups. Contact your administrator to
          request access.
        </p>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Empty states
  // ---------------------------------------------------------------------------

  private renderEmpty() {
    if (this.hasActiveFilters) {
      return html`
        <div class="empty-state">
          <sl-icon name="funnel" aria-hidden="true"></sl-icon>
          <h2>No Groups Match These Filters</h2>
          <p>Try adjusting your search or filter criteria.</p>
          <sl-button variant="primary" size="small" @click=${() => this.clearFilters()}>
            <sl-icon slot="prefix" name="x-lg" aria-hidden="true"></sl-icon>
            Clear filters
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="empty-state">
        <sl-icon name="diagram-3" aria-hidden="true"></sl-icon>
        <h2>No Groups</h2>
        <p>
          Groups organize users, agents, and other groups. They can be granted roles and used in
          access boundaries.
        </p>
        ${this.canCreate
          ? html`
              <sl-button variant="primary" @click=${() => this.openCreateDialog()}>
                <sl-icon slot="prefix" name="plus-lg" aria-hidden="true"></sl-icon>
                Create your first group
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Groups table
  // ---------------------------------------------------------------------------

  private renderGroupsTable(groups: AdminGroup[], showPagination: boolean) {
    return html`
      <div class="table-container">
        <table role="table" aria-label="Groups">
          <caption class="sr-only">
            List of groups showing name, type, description, labels, and last updated time.
          </caption>
          <thead>
            <tr>
              <th scope="col">Group</th>
              <th scope="col">Type</th>
              <th scope="col" class="hide-mobile">Description</th>
              <th scope="col" class="hide-mobile">Labels</th>
              <th scope="col" class="hide-mobile">Updated</th>
            </tr>
          </thead>
          <tbody>
            ${groups.map((group) => this.renderGroupRow(group))}
          </tbody>
        </table>
        ${showPagination ? this.renderPagination() : nothing}
      </div>
    `;
  }

  private renderGroupRow(group: AdminGroup) {
    const labels = group.labels ? Object.entries(group.labels) : [];

    return html`
      <tr
        class="clickable"
        @click=${(e: MouseEvent) => {
          // Don't navigate if clicking the link itself (it handles it)
          if ((e.target as HTMLElement).closest('a')) return;
          this.navigateToGroup(group.id);
        }}
      >
        <td>
          <div class="group-identity">
            <div class="group-icon ${group.groupType}" aria-hidden="true">
              <sl-icon name="${group.groupType === 'project_agents' ? 'cpu' : 'people'}"></sl-icon>
            </div>
            <div class="group-info">
              <a
                class="group-name-link"
                href="/admin/groups/${encodeURIComponent(group.id)}"
                @click=${(e: MouseEvent) => {
                  e.preventDefault();
                  this.navigateToGroup(group.id);
                }}
              >
                ${group.name}
              </a>
              <span class="group-slug">${group.slug}</span>
            </div>
          </div>
        </td>
        <td>
          <span class="type-badge ${group.groupType}">
            ${group.groupType === 'project_agents' ? 'project agents' : 'explicit'}
          </span>
        </td>
        <td class="hide-mobile">
          <span class="description-text">${group.description || '—'}</span>
        </td>
        <td class="hide-mobile">
          ${labels.length > 0
            ? html`
                <div class="labels-container">
                  ${labels
                    .slice(0, 3)
                    .map(([key, value]) => html`<span class="label-tag">${key}=${value}</span>`)}
                  ${labels.length > 3
                    ? html`<span class="label-tag">+${labels.length - 3}</span>`
                    : ''}
                </div>
              `
            : html`<span class="meta-text">—</span>`}
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(group.updated)}</span>
        </td>
      </tr>
    `;
  }

  // ---------------------------------------------------------------------------
  // Pagination
  // ---------------------------------------------------------------------------

  private renderPagination() {
    if (this.groups.length === 0) return nothing;

    const start = (this.currentPageNumber - 1) * PAGE_SIZE + 1;
    const end = start + this.groups.length - 1;

    return html`
      <div class="pagination">
        <sl-button
          variant="default"
          size="small"
          ?disabled=${!this.hasPreviousPage}
          @click=${() => this.goPreviousPage()}
        >
          <sl-icon name="chevron-left" aria-hidden="true"></sl-icon>
          Previous
        </sl-button>
        <span class="pagination-info"> Showing ${start}–${end} · ${this.totalCount} total </span>
        <sl-button
          variant="default"
          size="small"
          ?disabled=${!this.hasNextPage}
          @click=${() => this.goNextPage()}
        >
          Next
          <sl-icon name="chevron-right" aria-hidden="true"></sl-icon>
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-groups': ScionPageAdminGroups;
  }
}
