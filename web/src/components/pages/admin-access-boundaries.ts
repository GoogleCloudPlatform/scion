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
 * Admin Access Boundaries inventory page component (F2).
 *
 * Full-featured list page with search, filters, cursor-based pagination,
 * sorting, responsive layouts, and capability-driven actions.
 *
 * Types are imported exclusively from the shared contract module
 * (`web/src/shared/access-boundaries.ts`). Data fetching goes through the
 * API client (`web/src/client/access-boundaries-api.ts`).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { setDocumentTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/main.js';
import {
  list,
  resetAllSequences,
  StaleResponseError,
  AccessBoundaryAPIError,
} from '../../client/access-boundaries-api.js';
import type {
  AccessBoundarySummary,
  AccessBoundaryListFilters,
  AccessBoundaryListResponse,
  AccessBoundaryStatus,
  AccessBoundaryRisk,
  ConstraintSubject,
  ConstraintSubjectDisplay,
  ConstraintScopeDisplay,
  SubjectSelection,
  PageToken,
} from '../../shared/access-boundaries.js';
import { canAccessBoundary } from '../../shared/access-boundaries.js';
import { isFeatureEnabled, ACCESS_BOUNDARIES_AUTHORING_FLAG } from '../../utils/feature-flags.js';

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const PAGE_SIZE = 25;
const SEARCH_DEBOUNCE_MS = 300;
const STALENESS_THRESHOLD_MS = 5 * 60 * 1000; // 5 minutes

// ---------------------------------------------------------------------------
// Helper types
// ---------------------------------------------------------------------------

interface SortConfig {
  field: string;
  direction: 'asc' | 'desc';
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-page-admin-access-boundaries')
export class ScionPageAdminAccessBoundaries extends LitElement {
  // --- Data state ---
  @state() private loading = true;
  @state() private items: AccessBoundarySummary[] = [];
  @state() private totalCount = 0;
  @state() private totalCountExact = true;
  @state() private nextPageToken: PageToken | undefined;
  @state() private error: string | null = null;
  @state() private permissionDenied = false;
  @state() private listCapabilities: AccessBoundaryListResponse['_capabilities'] | null = null;

  // --- Filter state ---
  @state() private searchQuery = '';
  @state() private filterScopeType: '' | 'system' | 'project' = '';
  @state() private filterSubjectKind: '' | SubjectSelection = '';
  @state() private filterStatus: '' | AccessBoundaryStatus = '';
  @state() private filterRisk: '' | AccessBoundaryRisk = '';

  // --- Pagination ---
  @state() private pageTokenStack: PageToken[] = [];
  @state() private currentPageToken: PageToken | undefined;

  // --- Sort ---
  @state() private sort: SortConfig = { field: 'risk_status', direction: 'desc' };

  // --- Mobile card expansion ---
  @state() private expandedCardIds: Set<string> = new Set();

  // --- Misc ---
  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private abortController: AbortController | null = null;
  private lastLoadTime = 0;
  private boundVisibilityChange = this.handleVisibilityChange.bind(this);

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
      flex: 0 1 160px;
      min-width: 120px;
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

    .table-scroll {
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
      min-width: 800px;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
      user-select: none;
    }

    th.sortable {
      cursor: pointer;
    }

    th.sortable:hover {
      color: var(--scion-text, #1e293b);
    }

    th .sort-indicator {
      display: inline-block;
      margin-left: 0.25rem;
      font-size: 0.625rem;
      vertical-align: middle;
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

    tr.clickable-row {
      cursor: pointer;
      transition: background-color 0.15s ease;
    }

    tr.clickable-row:hover {
      background-color: var(--scion-bg-subtle, #f1f5f9);
    }

    tr.clickable-row:focus-within {
      outline: 2px solid var(--sl-color-primary-600, #2563eb);
      outline-offset: -2px;
    }

    /* Row link (visually hidden, used for keyboard activation) */
    .row-link {
      color: inherit;
      text-decoration: none;
      display: contents;
    }

    /* Status badges */
    .status-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 500;
      white-space: nowrap;
    }

    .status-badge.active {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.scheduled {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .status-badge.expired {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .status-badge.recovery_disabled {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .status-badge.invalid_degraded {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #b45309);
    }

    .status-badge sl-icon {
      font-size: 0.75rem;
    }

    /* Risk badges */
    .risk-badges {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
      margin-top: 0.25rem;
    }

    .risk-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.0625rem 0.375rem;
      border-radius: 9999px;
      font-size: 0.625rem;
      font-weight: 500;
      white-space: nowrap;
    }

    .risk-badge.tightening {
      background: var(--sl-color-warning-50, #fffbeb);
      color: var(--sl-color-warning-700, #b45309);
    }

    .risk-badge.relaxation_scheduled {
      background: var(--sl-color-primary-50, #eff6ff);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .risk-badge.mixed {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #b45309);
    }

    .risk-badge.lockout_sensitive {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .risk-badge.degraded {
      background: var(--sl-color-warning-50, #fffbeb);
      color: var(--sl-color-warning-600, #d97706);
    }

    /* Name cell */
    .name-cell {
      min-width: 160px;
    }

    .boundary-name {
      font-weight: 500;
      font-size: 0.875rem;
      overflow-wrap: anywhere;
    }

    /* Subject display */
    .subject-display {
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }

    .subject-kind-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.0625rem 0.375rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.6875rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .subject-kind-badge sl-icon {
      font-size: 0.625rem;
    }

    .subject-label {
      font-size: 0.8125rem;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
      max-width: 200px;
    }

    /* Scope display */
    .scope-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    .scope-badge sl-icon {
      font-size: 0.625rem;
    }

    .scope-name {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Permissions summary */
    .perm-summary {
      font-size: 0.8125rem;
      white-space: nowrap;
    }

    /* Affected count */
    .affected-count {
      font-size: 0.8125rem;
      white-space: nowrap;
    }

    .affected-count.incomplete {
      color: var(--sl-color-warning-600, #d97706);
      font-style: italic;
    }

    /* Schedule */
    .schedule-display {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
    }

    /* Updated time */
    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
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

    /* Visually hidden (screen-reader accessible) */
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

    /* Live region for result counts */
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

    /* Responsive: tablet */
    @media (max-width: 1024px) {
      table {
        min-width: 0;
      }

      .hide-tablet {
        display: none;
      }
    }

    /* Responsive: mobile — card list */
    @media (max-width: 768px) {
      .desktop-table {
        display: none;
      }

      .mobile-cards {
        display: block;
      }

      .header h1 {
        font-size: 1.25rem;
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

      .pagination {
        flex-wrap: wrap;
        padding: 0.75rem;
        gap: 0.5rem;
      }
    }

    @media (min-width: 769px) {
      .mobile-cards {
        display: none;
      }
    }

    /* Mobile card styles */
    .mobile-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1rem;
      margin-bottom: 0.75rem;
      cursor: pointer;
      transition: background-color 0.15s ease;
    }

    .mobile-card:hover {
      background-color: var(--scion-bg-subtle, #f1f5f9);
    }

    .mobile-card:focus-within {
      outline: 2px solid var(--sl-color-primary-600, #2563eb);
      outline-offset: -2px;
    }

    .mobile-card-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
    }

    .mobile-card-name {
      font-weight: 600;
      font-size: 0.9375rem;
      color: var(--scion-text, #1e293b);
      overflow-wrap: anywhere;
    }

    .mobile-card-meta {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 0.375rem 1rem;
      font-size: 0.8125rem;
    }

    .mobile-card-label {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.6875rem;
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }

    /* Mobile card expandable details */
    .mobile-card-expand-btn {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      margin-top: 0.5rem;
      padding: 0.25rem 0;
      background: none;
      border: none;
      font-size: 0.75rem;
      color: var(--sl-color-primary-600, #2563eb);
      cursor: pointer;
      min-height: 44px;
      min-width: 44px;
    }

    .mobile-card-expand-btn:focus-visible {
      outline: 2px solid var(--sl-color-primary-600, #2563eb);
      outline-offset: 2px;
      border-radius: var(--scion-radius, 0.5rem);
    }

    .mobile-card-details {
      margin-top: 0.5rem;
      padding-top: 0.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .mobile-card-detail-row {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding: 0.25rem 0;
      font-size: 0.8125rem;
    }

    .mobile-card-detail-label {
      color: var(--scion-text-muted, #64748b);
    }

    .mobile-card-detail-value {
      color: var(--scion-text, #1e293b);
    }

    /* Forced colors (high-contrast mode) */
    @media (forced-colors: active) {
      .status-badge,
      .risk-badge,
      .scope-badge,
      .subject-kind-badge {
        border: 1px solid ButtonText;
      }

      .mobile-card,
      .table-container {
        border: 1px solid ButtonText;
      }

      tr.clickable-row:focus-within {
        outline: 2px solid Highlight;
      }

      .skeleton-cell {
        border: 1px solid GrayText;
      }
    }

    /* Reduced motion */
    @media (prefers-reduced-motion: reduce) {
      tr.clickable-row {
        transition: none;
      }

      .mobile-card {
        transition: none;
      }

      .skeleton-cell {
        animation: none;
      }
    }
  `;

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override connectedCallback(): void {
    super.connectedCallback();
    setDocumentTitle('Access Boundaries');
    this.readFiltersFromURL();
    resetAllSequences();
    void this.loadData();
    document.addEventListener('visibilitychange', this.boundVisibilityChange);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.cancelPendingSearch();
    this.abortController?.abort();
    document.removeEventListener('visibilitychange', this.boundVisibilityChange);
  }

  private handleVisibilityChange(): void {
    if (document.visibilityState === 'visible') {
      const elapsed = Date.now() - this.lastLoadTime;
      if (elapsed >= STALENESS_THRESHOLD_MS) {
        void this.loadData();
      }
    }
  }

  // ---------------------------------------------------------------------------
  // URL state management
  // ---------------------------------------------------------------------------

  private readFiltersFromURL(): void {
    const params = new URLSearchParams(window.location.search);
    this.searchQuery = params.get('q') ?? '';
    this.filterScopeType = (params.get('scopeType') ?? '') as typeof this.filterScopeType;
    this.filterSubjectKind = (params.get('subjectKind') ?? '') as typeof this.filterSubjectKind;
    this.filterStatus = (params.get('status') ?? '') as typeof this.filterStatus;
    this.filterRisk = (params.get('risk') ?? '') as typeof this.filterRisk;
    this.currentPageToken = params.get('pageToken') ?? undefined;
  }

  private syncFiltersToURL(): void {
    const params = new URLSearchParams();
    if (this.searchQuery) params.set('q', this.searchQuery);
    if (this.filterScopeType) params.set('scopeType', this.filterScopeType);
    if (this.filterSubjectKind) params.set('subjectKind', this.filterSubjectKind);
    if (this.filterStatus) params.set('status', this.filterStatus);
    if (this.filterRisk) params.set('risk', this.filterRisk);
    if (this.currentPageToken) params.set('pageToken', this.currentPageToken);

    const qs = params.toString();
    const newUrl = `${window.location.pathname}${qs ? `?${qs}` : ''}`;
    window.history.replaceState({}, '', newUrl);
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private buildFilters(): AccessBoundaryListFilters {
    const filters: AccessBoundaryListFilters = {
      pageSize: PAGE_SIZE,
    };
    if (this.searchQuery) filters.q = this.searchQuery;
    if (this.filterScopeType) filters.scopeType = this.filterScopeType;
    if (this.filterSubjectKind) filters.subjectKind = this.filterSubjectKind;
    if (this.filterStatus) filters.status = this.filterStatus;
    if (this.filterRisk) filters.risk = this.filterRisk;
    if (this.currentPageToken) filters.pageToken = this.currentPageToken;
    if (this.sort.field !== 'risk_status') {
      filters.sort = `${this.sort.field}:${this.sort.direction}`;
    }
    return filters;
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    // Abort any in-flight request
    this.abortController?.abort();
    this.abortController = new AbortController();

    try {
      const filters = this.buildFilters();
      const data = await list(filters, { signal: this.abortController.signal });

      this.items = data.items ?? [];
      this.totalCount = data.totalCount ?? 0;
      this.totalCountExact = data.totalCountExact ?? true;
      this.nextPageToken = data.nextPageToken;
      this.listCapabilities = data._capabilities ?? null;
      this.lastLoadTime = Date.now();
      this.permissionDenied = false;
    } catch (err) {
      if (err instanceof StaleResponseError) {
        // A newer request superseded this one — do nothing
        return;
      }
      if (err instanceof DOMException && err.name === 'AbortError') {
        return;
      }
      console.error('Failed to load access boundaries:', err);
      if (err instanceof AccessBoundaryAPIError) {
        if (err.httpStatus === 403) {
          this.permissionDenied = true;
          this.error = null;
        } else {
          this.error = err.message || `HTTP ${err.httpStatus}`;
        }
      } else {
        this.error = err instanceof Error ? err.message : 'Failed to load access boundaries';
      }
    } finally {
      this.loading = false;
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

  private onFilterChange(
    field: 'filterScopeType' | 'filterSubjectKind' | 'filterStatus' | 'filterRisk',
    e: Event
  ): void {
    const value = (e.target as HTMLSelectElement).value;
    // eslint-disable-next-line @typescript-eslint/no-explicit-any, @typescript-eslint/no-unsafe-member-access
    (this as any)[field] = value;
    this.resetPagination();
    this.syncFiltersToURL();
    void this.loadData();
  }

  private get hasActiveFilters(): boolean {
    return !!(
      this.searchQuery ||
      this.filterScopeType ||
      this.filterSubjectKind ||
      this.filterStatus ||
      this.filterRisk
    );
  }

  private get activeFilterCount(): number {
    let count = 0;
    if (this.searchQuery) count++;
    if (this.filterScopeType) count++;
    if (this.filterSubjectKind) count++;
    if (this.filterStatus) count++;
    if (this.filterRisk) count++;
    return count;
  }

  private clearFilters(): void {
    this.searchQuery = '';
    this.filterScopeType = '';
    this.filterSubjectKind = '';
    this.filterStatus = '';
    this.filterRisk = '';
    this.resetPagination();
    this.syncFiltersToURL();
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Pagination
  // ---------------------------------------------------------------------------

  private resetPagination(): void {
    this.currentPageToken = undefined;
    this.nextPageToken = undefined;
    this.pageTokenStack = [];
  }

  private goNextPage(): void {
    if (!this.nextPageToken) return;
    if (this.currentPageToken) {
      this.pageTokenStack = [...this.pageTokenStack, this.currentPageToken];
    } else {
      this.pageTokenStack = [...this.pageTokenStack, ''];
    }
    this.currentPageToken = this.nextPageToken;
    this.syncFiltersToURL();
    void this.loadData();
  }

  private goPreviousPage(): void {
    if (this.pageTokenStack.length === 0) return;
    const prevToken = this.pageTokenStack[this.pageTokenStack.length - 1];
    this.pageTokenStack = this.pageTokenStack.slice(0, -1);
    this.currentPageToken = prevToken || undefined;
    this.syncFiltersToURL();
    void this.loadData();
  }

  private get currentPageNumber(): number {
    return this.pageTokenStack.length + 1;
  }

  private get hasPreviousPage(): boolean {
    return this.pageTokenStack.length > 0;
  }

  private get hasNextPage(): boolean {
    return !!this.nextPageToken;
  }

  // ---------------------------------------------------------------------------
  // Sorting
  // ---------------------------------------------------------------------------

  private toggleSort(field: string): void {
    if (this.sort.field === field) {
      this.sort = { field, direction: this.sort.direction === 'asc' ? 'desc' : 'asc' };
    } else {
      this.sort = { field, direction: 'asc' };
    }
    this.resetPagination();
    this.syncFiltersToURL();
    void this.loadData();
  }

  private renderSortIndicator(field: string) {
    if (this.sort.field !== field) return nothing;
    return html`<span class="sort-indicator">${this.sort.direction === 'asc' ? '▲' : '▼'}</span>`;
  }

  // ---------------------------------------------------------------------------
  // Refresh
  // ---------------------------------------------------------------------------

  private refresh(): void {
    // Refresh the current page without resetting pagination
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Display helpers
  // ---------------------------------------------------------------------------

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) return rtf.format(-diffSeconds, 'second');
      if (Math.abs(diffMinutes) < 60) return rtf.format(-diffMinutes, 'minute');
      if (Math.abs(diffHours) < 24) return rtf.format(-diffHours, 'hour');
      return rtf.format(-diffDays, 'day');
    } catch {
      return dateString;
    }
  }

  private statusLabel(status: AccessBoundaryStatus): string {
    switch (status) {
      case 'active':
        return 'Active';
      case 'scheduled':
        return 'Scheduled';
      case 'expired':
        return 'Expired';
      case 'recovery_disabled':
        return 'Recovery-disabled';
      case 'invalid_degraded':
        return 'Invalid / Degraded';
      default:
        return status;
    }
  }

  private statusIcon(status: AccessBoundaryStatus): string {
    switch (status) {
      case 'active':
        return 'check-circle';
      case 'scheduled':
        return 'clock';
      case 'expired':
        return 'circle';
      case 'recovery_disabled':
        return 'lock';
      case 'invalid_degraded':
        return 'exclamation-triangle';
      default:
        return 'circle';
    }
  }

  private riskLabel(risk: AccessBoundaryRisk): string {
    switch (risk) {
      case 'tightening':
        return 'Tightening';
      case 'relaxation_scheduled':
        return 'Relaxation scheduled';
      case 'mixed':
        return 'Mixed';
      case 'lockout_sensitive':
        return 'Lockout sensitive';
      case 'degraded':
        return 'Degraded';
      default:
        return risk;
    }
  }

  private subjectKindIcon(subject: ConstraintSubject): string {
    switch (subject.kind) {
      case 'principal':
        switch (subject.principal.type) {
          case 'user':
            return 'person';
          case 'agent':
            return 'cpu';
          case 'group':
            return 'diagram-3';
          default:
            return 'person';
        }
      case 'group_closure':
        return 'diagram-3';
      case 'all_principals':
        return 'people';
      default:
        return 'person';
    }
  }

  private subjectKindLabel(subject: ConstraintSubject): string {
    switch (subject.kind) {
      case 'principal':
        switch (subject.principal.type) {
          case 'user':
            return 'User';
          case 'agent':
            return 'Agent';
          case 'group':
            return 'Exact group';
          default:
            return 'Principal';
        }
      case 'group_closure':
        return 'Group closure';
      case 'all_principals':
        return 'All principals';
      default:
        return 'Unknown';
    }
  }

  private subjectDisplayLabel(
    display: ConstraintSubjectDisplay | undefined,
    subject: ConstraintSubject
  ): string {
    if (display?.label) return display.label;
    // Fallback when server hasn't resolved the display
    switch (subject.kind) {
      case 'principal':
        return subject.principal.id;
      case 'group_closure':
        return subject.groupId;
      case 'all_principals':
        return 'All principals';
      default:
        return 'Unknown';
    }
  }

  private scopeDisplayLabel(display: ConstraintScopeDisplay | undefined): string {
    if (!display) return 'Unknown';
    if (display.type === 'system') return 'System';
    return display.projectName ?? display.label ?? 'Project';
  }

  private formatSchedule(item: AccessBoundarySummary): string {
    const schedule = item.appliesWhen;
    if (!schedule) return 'Always';
    const { notBefore, expiresAt } = schedule;
    if (!notBefore && !expiresAt) return 'Always';
    const parts: string[] = [];
    if (notBefore) {
      try {
        parts.push(
          `From ${new Date(notBefore).toLocaleDateString('en', { month: 'short', day: 'numeric' })}`
        );
      } catch {
        parts.push(`From ${notBefore}`);
      }
    }
    if (expiresAt) {
      try {
        parts.push(
          `Until ${new Date(expiresAt).toLocaleDateString('en', { month: 'short', day: 'numeric' })}`
        );
      } catch {
        parts.push(`Until ${expiresAt}`);
      }
    }
    return parts.join(' ');
  }

  private get canCreate(): boolean {
    // Visible only when authoring flag is enabled AND list-level capabilities include previewCreate
    if (!isFeatureEnabled(ACCESS_BOUNDARIES_AUTHORING_FLAG)) return false;
    return canAccessBoundary(this.listCapabilities ?? undefined, 'previewCreate');
  }

  // ---------------------------------------------------------------------------
  // Navigation
  // ---------------------------------------------------------------------------

  private navigateToBoundary(id: string): void {
    navigateTo(`/admin/access-boundaries/${encodeURIComponent(id)}`);
  }

  private navigateToCreate(): void {
    navigateTo('/admin/access-boundaries/new');
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="header">
        <h1>Access Boundaries</h1>
        <div class="header-actions">
          ${this.canCreate
            ? html`
                <sl-button variant="primary" size="small" @click=${() => this.navigateToCreate()}>
                  <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                  Create boundary
                </sl-button>
              `
            : nothing}
        </div>
      </div>
      <p class="header-subtitle">
        Limit the maximum permissions available across all role assignments.
      </p>

      ${this.renderFilterBar()}
      <div class="result-count-live" role="status" aria-live="polite" aria-atomic="true">
        ${!this.loading && this.items.length > 0
          ? html`${this.totalCountExact ? this.totalCount : `${this.totalCount}+`}
            ${this.totalCount === 1 ? 'boundary' : 'boundaries'}${this.hasActiveFilters
              ? ' matching filters'
              : ''}`
          : nothing}
      </div>
      ${this.loading && this.items.length === 0
        ? this.renderLoadingSkeleton()
        : this.permissionDenied
          ? this.renderPermissionDenied()
          : this.error
            ? this.renderError()
            : this.items.length === 0
              ? this.renderEmpty()
              : this.renderContent()}
    `;
  }

  // ---------------------------------------------------------------------------
  // Filter bar
  // ---------------------------------------------------------------------------

  private renderFilterBar() {
    return html`
      <div class="filter-bar">
        <sl-input
          placeholder="Search name, subject, project..."
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
          <sl-icon name="search" slot="prefix"></sl-icon>
        </sl-input>

        <sl-select
          placeholder="Scope"
          size="small"
          clearable
          .value=${this.filterScopeType}
          @sl-change=${(e: Event) => this.onFilterChange('filterScopeType', e)}
        >
          <sl-option value="system">System</sl-option>
          <sl-option value="project">Project</sl-option>
        </sl-select>

        <sl-select
          placeholder="Subject"
          size="small"
          clearable
          .value=${this.filterSubjectKind}
          @sl-change=${(e: Event) => this.onFilterChange('filterSubjectKind', e)}
        >
          <sl-option value="exact_user">User</sl-option>
          <sl-option value="exact_agent">Agent</sl-option>
          <sl-option value="exact_group">Exact group</sl-option>
          <sl-option value="group_closure">Group closure</sl-option>
          <sl-option value="all_principals">All principals</sl-option>
        </sl-select>

        <sl-select
          placeholder="Status"
          size="small"
          clearable
          .value=${this.filterStatus}
          @sl-change=${(e: Event) => this.onFilterChange('filterStatus', e)}
        >
          <sl-option value="active">Active</sl-option>
          <sl-option value="scheduled">Scheduled</sl-option>
          <sl-option value="expired">Expired</sl-option>
          <sl-option value="recovery_disabled">Recovery-disabled</sl-option>
          <sl-option value="invalid_degraded">Invalid / Degraded</sl-option>
        </sl-select>

        <sl-select
          placeholder="Risk"
          size="small"
          clearable
          .value=${this.filterRisk}
          @sl-change=${(e: Event) => this.onFilterChange('filterRisk', e)}
        >
          <sl-option value="tightening">Tightening</sl-option>
          <sl-option value="relaxation_scheduled">Relaxation scheduled</sl-option>
          <sl-option value="mixed">Mixed</sl-option>
          <sl-option value="lockout_sensitive">Lockout sensitive</sl-option>
          <sl-option value="degraded">Degraded</sl-option>
        </sl-select>

        <div class="filter-actions">
          ${this.hasActiveFilters
            ? html`
                <span class="active-filter-count"
                  >${this.activeFilterCount} filter${this.activeFilterCount !== 1 ? 's' : ''}</span
                >
                <sl-button variant="text" size="small" @click=${() => this.clearFilters()}>
                  <sl-icon slot="prefix" name="x-lg"></sl-icon>
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
      <div
        class="skeleton-table"
        role="status"
        aria-label="Loading access boundaries"
        aria-live="polite"
      >
        ${Array.from(
          { length: 5 },
          (_, i) => html`
            <div class="skeleton-row" key=${i}>
              <div class="skeleton-cell" style="flex: 2; min-width: 120px;"></div>
              <div class="skeleton-cell" style="flex: 1; min-width: 60px;"></div>
              <div class="skeleton-cell" style="flex: 1.5; min-width: 80px;"></div>
              <div class="skeleton-cell" style="flex: 1; min-width: 60px;"></div>
              <div class="skeleton-cell" style="flex: 1; min-width: 50px;"></div>
              <div class="skeleton-cell" style="flex: 0.5; min-width: 40px;"></div>
              <div class="skeleton-cell" style="flex: 0.75; min-width: 50px;"></div>
            </div>
          `
        )}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Error state
  // ---------------------------------------------------------------------------

  private renderError() {
    return html`
      <div class="error-state" role="alert">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Access Boundaries</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderPermissionDenied() {
    return html`
      <div class="permission-denied-state" role="alert">
        <sl-icon name="shield-lock"></sl-icon>
        <h2>Permission Denied</h2>
        <p>
          You do not have the required permission to view access boundaries. Contact your
          administrator to request access.
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
          <sl-icon name="funnel"></sl-icon>
          <h2>No Boundaries Match These Filters</h2>
          <p>Try adjusting your search or filter criteria.</p>
          <sl-button variant="primary" size="small" @click=${() => this.clearFilters()}>
            <sl-icon slot="prefix" name="x-lg"></sl-icon>
            Clear filters
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="empty-state">
        <sl-icon name="shield-lock"></sl-icon>
        <h2>No Access Boundaries</h2>
        <p>
          Access boundaries limit the maximum permissions available to principals across all their
          role assignments. They do not grant access — roles and role bindings are the positive
          authority surfaces.
        </p>
        <p>
          Create a boundary to cap what permissions a user, agent, group, or all principals can
          hold.
        </p>
        ${this.canCreate
          ? html`
              <sl-button variant="primary" @click=${() => this.navigateToCreate()}>
                <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                Create boundary
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Content: table + cards
  // ---------------------------------------------------------------------------

  private renderContent() {
    return html`
      <div class="desktop-table">${this.renderTable()}</div>
      <div class="mobile-cards">${this.renderCards()} ${this.renderPagination()}</div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Desktop table
  // ---------------------------------------------------------------------------

  private sortAriaLabel(field: string): 'ascending' | 'descending' | 'none' {
    if (this.sort.field !== field) return 'none';
    return this.sort.direction === 'asc' ? 'ascending' : 'descending';
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <div
          class="table-scroll"
          tabindex="0"
          role="region"
          aria-label="Access boundaries table, scrollable"
        >
          <table role="table" aria-label="Access boundaries">
            <caption class="sr-only">
              List of access boundaries showing name, scope, subject, schedule, retained
              permissions, affected principals, and last updated time.
            </caption>
            <thead>
              <tr>
                <th
                  class="sortable"
                  aria-sort=${this.sortAriaLabel('name')}
                  @click=${() => this.toggleSort('name')}
                  @keydown=${(e: KeyboardEvent) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      this.toggleSort('name');
                    }
                  }}
                  tabindex="0"
                  role="columnheader"
                >
                  Name / Status ${this.renderSortIndicator('name')}
                </th>
                <th role="columnheader">Scope</th>
                <th role="columnheader">Subject</th>
                <th class="hide-tablet" role="columnheader">Schedule</th>
                <th class="hide-tablet" role="columnheader">Retained permissions</th>
                <th
                  class="sortable hide-tablet"
                  aria-sort=${this.sortAriaLabel('affected_count')}
                  @click=${() => this.toggleSort('affected_count')}
                  @keydown=${(e: KeyboardEvent) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      this.toggleSort('affected_count');
                    }
                  }}
                  tabindex="0"
                  role="columnheader"
                >
                  Affected ${this.renderSortIndicator('affected_count')}
                </th>
                <th
                  class="sortable"
                  aria-sort=${this.sortAriaLabel('updated_at')}
                  @click=${() => this.toggleSort('updated_at')}
                  @keydown=${(e: KeyboardEvent) => {
                    if (e.key === 'Enter' || e.key === ' ') {
                      e.preventDefault();
                      this.toggleSort('updated_at');
                    }
                  }}
                  tabindex="0"
                  role="columnheader"
                >
                  Updated ${this.renderSortIndicator('updated_at')}
                </th>
              </tr>
            </thead>
            <tbody>
              ${this.items.map((item) => this.renderTableRow(item))}
            </tbody>
          </table>
        </div>
        ${this.renderPagination()}
      </div>
    `;
  }

  private renderTableRow(item: AccessBoundarySummary) {
    return html`
      <tr
        class="clickable-row"
        tabindex="0"
        role="link"
        aria-label="View boundary: ${item.name}"
        @click=${() => this.navigateToBoundary(item.id)}
        @keydown=${(e: KeyboardEvent) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            this.navigateToBoundary(item.id);
          }
        }}
      >
        <td class="name-cell">
          <div class="boundary-name" title="${item.name}">${item.name}</div>
          <span class="status-badge ${item.status}">
            <sl-icon name="${this.statusIcon(item.status)}"></sl-icon>
            ${this.statusLabel(item.status)}
          </span>
          ${item.risk?.length
            ? html`
                <div class="risk-badges">
                  ${item.risk.map(
                    (r) => html`<span class="risk-badge ${r}">${this.riskLabel(r)}</span>`
                  )}
                </div>
              `
            : nothing}
        </td>
        <td>
          <span class="scope-badge">
            <sl-icon
              name="${item.scopeDisplay?.type === 'project' ? 'building' : 'globe'}"
            ></sl-icon>
            ${item.scopeDisplay?.type === 'project' ? 'Project' : 'System'}
          </span>
          ${item.scopeDisplay?.type === 'project' && item.scopeDisplay.projectName
            ? html`<br /><span class="scope-name">${item.scopeDisplay.projectName}</span>`
            : nothing}
        </td>
        <td>
          <div class="subject-display">
            <span class="subject-kind-badge">
              <sl-icon name="${this.subjectKindIcon(item.subject)}"></sl-icon>
              ${this.subjectKindLabel(item.subject)}
            </span>
            <span class="subject-label"
              >${this.subjectDisplayLabel(item.subjectDisplay, item.subject)}</span
            >
          </div>
        </td>
        <td class="hide-tablet">
          <span class="schedule-display">${this.formatSchedule(item)}</span>
        </td>
        <td class="hide-tablet">
          <span class="perm-summary">${this.renderPermissionSummary(item)}</span>
        </td>
        <td class="hide-tablet">${this.renderAffectedCount(item)}</td>
        <td>
          <span class="meta-text">${this.formatRelativeTime(item.updatedAt)}</span>
        </td>
      </tr>
    `;
  }

  private renderPermissionSummary(item: AccessBoundarySummary) {
    const count = item.maximumPermissionCount ?? 0;
    return html`${count} retained`;
  }

  private renderAffectedCount(item: AccessBoundarySummary) {
    if (item.status === 'recovery_disabled') {
      return html`<span class="affected-count">--</span>`;
    }
    if (item.health?.state === 'unresolvable' || item.health?.state === 'degraded') {
      return html`<span class="affected-count incomplete">
        ${item.health.state === 'unresolvable' ? 'Unavailable' : 'Degraded'}
      </span>`;
    }
    const count = item.affectedPrincipalCount ?? 0;
    const exact = item.affectedPrincipalCountExact !== false;
    return html`<span class="affected-count">${exact ? count : `${count}+`}</span>`;
  }

  // ---------------------------------------------------------------------------
  // Mobile cards
  // ---------------------------------------------------------------------------

  private toggleCardExpand(id: string, e: Event): void {
    e.stopPropagation();
    const next = new Set(this.expandedCardIds);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    this.expandedCardIds = next;
  }

  private renderCards() {
    return html`
      <div role="list" aria-label="Access boundaries">
        ${this.items.map((item) => {
          const expanded = this.expandedCardIds.has(item.id);
          return html`
            <article
              class="mobile-card"
              role="listitem"
              tabindex="0"
              aria-label="View boundary: ${item.name}"
              @click=${() => this.navigateToBoundary(item.id)}
              @keydown=${(e: KeyboardEvent) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  this.navigateToBoundary(item.id);
                }
              }}
            >
              <div class="mobile-card-header">
                <span class="mobile-card-name" title="${item.name}">${item.name}</span>
                <span class="status-badge ${item.status}">
                  <sl-icon name="${this.statusIcon(item.status)}"></sl-icon>
                  ${this.statusLabel(item.status)}
                </span>
              </div>
              <div class="mobile-card-meta">
                <div>
                  <div class="mobile-card-label">Scope</div>
                  <span class="scope-badge">
                    <sl-icon
                      name="${item.scopeDisplay?.type === 'project' ? 'building' : 'globe'}"
                    ></sl-icon>
                    ${this.scopeDisplayLabel(item.scopeDisplay)}
                  </span>
                </div>
                <div>
                  <div class="mobile-card-label">Subject</div>
                  <div class="subject-display">
                    <span class="subject-kind-badge">
                      <sl-icon name="${this.subjectKindIcon(item.subject)}"></sl-icon>
                      ${this.subjectKindLabel(item.subject)}
                    </span>
                  </div>
                </div>
                <div>
                  <div class="mobile-card-label">Retained</div>
                  <span class="perm-summary">${this.renderPermissionSummary(item)}</span>
                </div>
                <div>
                  <div class="mobile-card-label">Updated</div>
                  <span class="meta-text">${this.formatRelativeTime(item.updatedAt)}</span>
                </div>
              </div>
              ${item.risk?.length
                ? html`
                    <div class="risk-badges" style="margin-top: 0.5rem;">
                      ${item.risk.map(
                        (r) => html`<span class="risk-badge ${r}">${this.riskLabel(r)}</span>`
                      )}
                    </div>
                  `
                : nothing}
              <button
                class="mobile-card-expand-btn"
                aria-expanded=${expanded ? 'true' : 'false'}
                aria-controls="card-details-${item.id}"
                @click=${(e: Event) => this.toggleCardExpand(item.id, e)}
                @keydown=${(e: KeyboardEvent) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault();
                    e.stopPropagation();
                    this.toggleCardExpand(item.id, e);
                  }
                }}
              >
                <sl-icon name=${expanded ? 'chevron-up' : 'chevron-down'}></sl-icon>
                ${expanded ? 'Less details' : 'More details'}
              </button>
              ${expanded
                ? html`
                    <div class="mobile-card-details" id="card-details-${item.id}">
                      <div class="mobile-card-detail-row">
                        <span class="mobile-card-detail-label">Scope type</span>
                        <span class="mobile-card-detail-value">
                          ${item.scopeDisplay?.type === 'project' ? 'Project' : 'System'}
                        </span>
                      </div>
                      <div class="mobile-card-detail-row">
                        <span class="mobile-card-detail-label">Subject kind</span>
                        <span class="mobile-card-detail-value">
                          ${this.subjectKindLabel(item.subject)}
                        </span>
                      </div>
                      <div class="mobile-card-detail-row">
                        <span class="mobile-card-detail-label">Subject</span>
                        <span class="mobile-card-detail-value" style="overflow-wrap: anywhere;">
                          ${this.subjectDisplayLabel(item.subjectDisplay, item.subject)}
                        </span>
                      </div>
                      <div class="mobile-card-detail-row">
                        <span class="mobile-card-detail-label">Schedule</span>
                        <span class="mobile-card-detail-value"> ${this.formatSchedule(item)} </span>
                      </div>
                      <div class="mobile-card-detail-row">
                        <span class="mobile-card-detail-label">Affected</span>
                        <span class="mobile-card-detail-value">
                          ${this.renderAffectedCount(item)}
                        </span>
                      </div>
                      ${item.risk?.length
                        ? html`
                            <div class="mobile-card-detail-row">
                              <span class="mobile-card-detail-label">Risk</span>
                              <span class="mobile-card-detail-value">
                                ${item.risk.map((r) => this.riskLabel(r)).join(', ')}
                              </span>
                            </div>
                          `
                        : nothing}
                    </div>
                  `
                : nothing}
            </article>
          `;
        })}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Pagination
  // ---------------------------------------------------------------------------

  private renderPagination() {
    // Only show pagination when there are results
    if (this.items.length === 0) return nothing;

    const start = (this.currentPageNumber - 1) * PAGE_SIZE + 1;
    const end = start + this.items.length - 1;

    return html`
      <div class="pagination">
        <sl-button
          variant="default"
          size="small"
          ?disabled=${!this.hasPreviousPage}
          @click=${() => this.goPreviousPage()}
        >
          <sl-icon name="chevron-left"></sl-icon>
          Previous
        </sl-button>
        <span class="pagination-info">
          ${this.totalCountExact
            ? html`${start}–${end} of ${this.totalCount}`
            : html`${start}–${end} of ${this.totalCount}+`}
        </span>
        <sl-button
          variant="default"
          size="small"
          ?disabled=${!this.hasNextPage}
          @click=${() => this.goNextPage()}
        >
          Next
          <sl-icon name="chevron-right"></sl-icon>
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-access-boundaries': ScionPageAdminAccessBoundaries;
  }
}
