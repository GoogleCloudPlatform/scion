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
 * Admin Access Boundary Detail Page
 *
 * Full detail page for an access boundary:
 * - Header: name, status badge, risk label, scope/subject summary, edit/delete controls
 * - Definition section: purpose, subject, scope, schedule, created/updated
 * - Permissions section: retained, removed, newly registered exclusions
 * - Affected access section: pageable principals with separate loses/regains
 * - Schedule section: current state, transition timeline
 * - Audit & recovery section: mutation events timeline, recovery state
 *
 * State handling: loading skeleton, error/retry, not found, recovery-disabled (read-only).
 * Delete is a relaxation preview, NOT a direct DELETE button.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { setDocumentTitle } from '../../client/page-title.js';
import { navigateTo } from '../../client/main.js';
import * as accessBoundariesApi from '../../client/access-boundaries-api.js';
import type {
  AccessBoundaryDetail,
  AccessBoundaryAuditEvent,
  AffectedPrincipal,
  PageToken,
} from '../../shared/access-boundaries.js';
import { canAccessBoundary } from '../../shared/access-boundaries.js';
import { isFeatureEnabled, ACCESS_BOUNDARIES_AUTHORING_FLAG } from '../../utils/feature-flags.js';

// Import sub-components
import '../shared/access-boundary-status.js';
import '../shared/access-boundary-impact-summary.js';
import '../shared/affected-principals-table.js';
import '../shared/access-boundary-audit-timeline.js';
import '../shared/access-boundary-preview.js';

import type { PageRequestDetail } from '../shared/affected-principals-table.js';
import type { AuditPageRequestDetail } from '../shared/access-boundary-audit-timeline.js';
import type { PreviewCommitSuccessDetail } from '../shared/access-boundary-preview.js';

type PagePhase = 'loading' | 'ready' | 'error' | 'not_found' | 'deleting' | 'permission_denied';

const STALENESS_THRESHOLD_MS = 5 * 60 * 1000;

@customElement('scion-page-admin-access-boundary-detail')
export class ScionPageAdminAccessBoundaryDetail extends LitElement {
  @state() private boundaryId = '';
  @state() private phase: PagePhase = 'loading';
  @state() private boundary: AccessBoundaryDetail | null = null;
  @state() private errorMessage = '';

  // Affected principals
  @state() private affectedPrincipals: AffectedPrincipal[] = [];
  @state() private affectedNextToken: PageToken | undefined;
  @state() private affectedTotalCount = 0;
  @state() private affectedTotalCountExact = true;
  @state() private loadingAffected = false;

  // Audit events
  @state() private auditEvents: AccessBoundaryAuditEvent[] = [];
  @state() private auditNextToken: PageToken | undefined;
  @state() private auditTotalCount = 0;
  @state() private loadingAudit = false;

  // Delete flow
  @state() private showDeletePreview = false;

  // Misc
  private lastLoadTime = 0;
  private boundVisibilityChange = this.handleVisibilityChange.bind(this);

  static override styles = css`
    :host {
      display: block;
    }

    .detail-page {
      max-width: 56rem;
      margin: 0 auto;
      padding: 1rem 1.5rem 3rem;
    }

    /* Header */
    .page-header {
      margin-bottom: 1.5rem;
    }

    .header-top {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.25rem;
    }

    .back-link {
      font-size: 0.875rem;
      color: var(--sl-color-primary-600, #2563eb);
      text-decoration: none;
      display: flex;
      align-items: center;
      gap: 0.25rem;
      cursor: pointer;
    }

    .back-link:hover {
      text-decoration: underline;
    }

    .header-main {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1rem;
      flex-wrap: wrap;
    }

    .header-info {
      flex: 1;
      min-width: 200px;
    }

    .boundary-name {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.375rem;
      overflow-wrap: anywhere;
    }

    .header-badges {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
      margin-bottom: 0.5rem;
    }

    .header-meta {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .header-meta span + span::before {
      content: ' · ';
    }

    .header-actions {
      display: flex;
      gap: 0.5rem;
      align-items: center;
      flex-shrink: 0;
    }

    /* Sections */
    .detail-sections {
      display: flex;
      flex-direction: column;
      gap: 1.25rem;
    }

    .detail-section {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .section-header {
      padding: 0.75rem 1rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .section-header sl-icon {
      color: var(--scion-text-muted, #64748b);
    }

    .section-body {
      padding: 1rem;
    }

    /* Definition grid */
    .definition-grid {
      display: grid;
      grid-template-columns: auto 1fr;
      gap: 0.5rem 1.5rem;
      font-size: 0.875rem;
    }

    .def-label {
      color: var(--scion-text-muted, #64748b);
      font-weight: 500;
      white-space: nowrap;
    }

    .def-value {
      color: var(--scion-text, #1e293b);
    }

    .def-value.purpose {
      white-space: pre-wrap;
    }

    .def-value.mono {
      font-family: var(--sl-font-mono, monospace);
      font-size: 0.8125rem;
    }

    /* Permission lists */
    .perm-section {
      margin-bottom: 1rem;
    }

    .perm-section:last-child {
      margin-bottom: 0;
    }

    .perm-section-title {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.375rem;
    }

    .perm-list {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
    }

    .perm-tag {
      font-family: var(--sl-font-mono, monospace);
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      border-radius: var(--scion-radius, 0.5rem);
      white-space: nowrap;
    }

    .perm-tag.retained {
      background: var(--sl-color-success-50, #f0fdf4);
      color: var(--sl-color-success-700, #15803d);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
    }

    .perm-tag.removed {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
    }

    .perm-tag.new-since {
      background: var(--sl-color-warning-50, #fffbeb);
      color: var(--sl-color-warning-700, #b45309);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
    }

    .perm-count {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.25rem;
    }

    /* Schedule */
    .schedule-info {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .schedule-item {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.375rem 0;
    }

    .schedule-label {
      font-weight: 500;
      min-width: 80px;
    }

    /* Recovery */
    .recovery-disabled-banner {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
      padding: 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
      margin-bottom: 1.25rem;
    }

    .recovery-disabled-banner sl-icon {
      font-size: 1.5rem;
      color: var(--sl-color-danger-600, #dc2626);
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .recovery-banner-content {
      flex: 1;
    }

    .recovery-banner-title {
      font-weight: 600;
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 0.25rem;
    }

    .recovery-banner-text {
      font-size: 0.8125rem;
      color: var(--sl-color-danger-600, #dc2626);
    }

    .recovery-details {
      font-size: 0.875rem;
    }

    .recovery-detail-row {
      display: flex;
      gap: 1rem;
      padding: 0.25rem 0;
    }

    .recovery-detail-label {
      color: var(--scion-text-muted, #64748b);
      font-weight: 500;
      min-width: 100px;
    }

    .recovery-detail-value {
      color: var(--scion-text, #1e293b);
    }

    /* Effect summary */
    .effect-summary {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(140px, 1fr));
      gap: 0.5rem;
      margin-bottom: 0.75rem;
    }

    .effect-stat {
      padding: 0.5rem 0.75rem;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .effect-stat-label {
      font-size: 0.6875rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.025em;
    }

    .effect-stat-value {
      font-size: 1.125rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
    }

    /* Loading / Error / Not found */
    .loading-state,
    .error-state,
    .not-found-state {
      text-align: center;
      padding: 4rem 2rem;
    }

    .loading-state {
      color: var(--scion-text-muted, #64748b);
    }

    .error-state {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .not-found-state {
      color: var(--scion-text-muted, #64748b);
    }

    .not-found-state h1,
    .error-state h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0.5rem 0;
    }

    /* Skeleton */
    .skeleton-section {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
      margin-bottom: 1.25rem;
    }

    .skeleton-header {
      height: 2.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .skeleton-body {
      padding: 1rem;
    }

    .skeleton-line {
      height: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 0.5rem;
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

    /* Delete preview overlay */
    .delete-preview-overlay {
      margin-top: 1.25rem;
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

    /* Permission denied */
    .permission-denied-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .permission-denied-state sl-icon {
      font-size: 2rem;
      color: var(--sl-color-warning-500, #eab308);
      margin-bottom: 0.5rem;
    }

    .permission-denied-state h1 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .permission-denied-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

    /* Responsive: mobile */
    @media (max-width: 768px) {
      .detail-page {
        padding: 0.5rem 0.75rem 2rem;
      }

      .boundary-name {
        font-size: 1.25rem;
      }

      .header-main {
        flex-direction: column;
      }

      .header-actions {
        width: 100%;
        justify-content: flex-start;
      }

      .definition-grid {
        grid-template-columns: 1fr;
        gap: 0.25rem 0;
      }

      .def-label {
        font-size: 0.75rem;
        margin-top: 0.5rem;
      }

      .effect-summary {
        grid-template-columns: 1fr 1fr;
      }

      .perm-list {
        gap: 0.25rem;
      }
    }

    /* Tablet: affected principals table scroll */
    @media (max-width: 1024px) {
      .affected-table-scroll {
        overflow-x: auto;
        -webkit-overflow-scrolling: touch;
      }
    }

    .affected-table-scroll {
      position: relative;
    }

    /* Forced colors */
    @media (forced-colors: active) {
      .detail-section {
        border: 1px solid ButtonText;
      }

      .perm-tag {
        border: 1px solid ButtonText;
      }

      .recovery-disabled-banner {
        border: 2px solid Highlight;
      }

      .effect-stat {
        border: 1px solid ButtonText;
      }
    }

    /* Reduced motion */
    @media (prefers-reduced-motion: reduce) {
      .skeleton-line {
        animation: none;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    const match = window.location.pathname.match(/\/admin\/access-boundaries\/([^/]+)/);
    if (match) {
      this.boundaryId = match[1];
    }
    setDocumentTitle('Access Boundary');
    void this.loadBoundary();
    document.addEventListener('visibilitychange', this.boundVisibilityChange);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    document.removeEventListener('visibilitychange', this.boundVisibilityChange);
  }

  private handleVisibilityChange(): void {
    if (document.visibilityState === 'visible') {
      const elapsed = Date.now() - this.lastLoadTime;
      if (elapsed >= STALENESS_THRESHOLD_MS) {
        void this.loadBoundary();
      }
    }
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadBoundary(): Promise<void> {
    this.phase = 'loading';
    this.errorMessage = '';

    try {
      const boundary = await accessBoundariesApi.get(this.boundaryId);
      this.boundary = boundary;
      this.phase = 'ready';
      this.lastLoadTime = Date.now();
      setDocumentTitle(`Access Boundary: ${boundary.name}`);

      // Load affected principals
      void this.loadAffectedPrincipals();
      // Load audit events
      void this.loadAuditEvents();
    } catch (err) {
      if (err instanceof accessBoundariesApi.AccessBoundaryAPIError) {
        if (err.httpStatus === 404) {
          this.phase = 'not_found';
          return;
        }
        if (err.httpStatus === 403) {
          this.phase = 'permission_denied';
          return;
        }
      }
      this.errorMessage = err instanceof Error ? err.message : 'Failed to load access boundary';
      this.phase = 'error';
      console.error('Failed to load boundary:', err);
    }
  }

  private async loadAffectedPrincipals(pageToken?: PageToken): Promise<void> {
    this.loadingAffected = true;
    try {
      const params: { pageToken?: PageToken; pageSize?: number } = { pageSize: 25 };
      if (pageToken) params.pageToken = pageToken;
      const page = await accessBoundariesApi.listAffected(this.boundaryId, params);
      if (pageToken) {
        this.affectedPrincipals = [...this.affectedPrincipals, ...page.items];
      } else {
        this.affectedPrincipals = page.items;
      }
      this.affectedNextToken = page.nextPageToken;
      this.affectedTotalCount = page.totalCount;
      this.affectedTotalCountExact = page.totalCountExact;
    } catch (err) {
      console.error('Failed to load affected principals:', err);
    } finally {
      this.loadingAffected = false;
    }
  }

  private async loadAuditEvents(pageToken?: PageToken): Promise<void> {
    this.loadingAudit = true;
    try {
      const auditParams: { pageToken?: PageToken; pageSize?: number } = { pageSize: 20 };
      if (pageToken) auditParams.pageToken = pageToken;
      const page = await accessBoundariesApi.listAudit(this.boundaryId, auditParams);
      if (pageToken) {
        this.auditEvents = [...this.auditEvents, ...page.items];
      } else {
        this.auditEvents = page.items;
      }
      this.auditNextToken = page.nextPageToken;
      this.auditTotalCount = page.totalCount;
    } catch (err) {
      console.error('Failed to load audit events:', err);
    } finally {
      this.loadingAudit = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  private get isRecoveryDisabled(): boolean {
    return this.boundary?.status === 'recovery_disabled';
  }

  private get canEdit(): boolean {
    if (!isFeatureEnabled(ACCESS_BOUNDARIES_AUTHORING_FLAG)) return false;
    if (this.isRecoveryDisabled) return false;
    const caps = this.boundary?._capabilities;
    // Edit requires at least one preview capability
    return canAccessBoundary(caps, 'previewTighten') || canAccessBoundary(caps, 'previewRelax');
  }

  private get canDelete(): boolean {
    if (!isFeatureEnabled(ACCESS_BOUNDARIES_AUTHORING_FLAG)) return false;
    if (this.isRecoveryDisabled) return false;
    return canAccessBoundary(this.boundary?._capabilities, 'delete');
  }

  private formatDatetime(iso: string | null): string {
    if (!iso) return '—';
    try {
      const date = new Date(iso);
      if (isNaN(date.getTime())) return iso;
      return date.toLocaleString(undefined, {
        dateStyle: 'medium',
        timeStyle: 'short',
      });
    } catch {
      return iso;
    }
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffMinutes) < 60) return rtf.format(-diffMinutes, 'minute');
      if (Math.abs(diffHours) < 24) return rtf.format(-diffHours, 'hour');
      return rtf.format(-diffDays, 'day');
    } catch {
      return dateString;
    }
  }

  private subjectDescription(): string {
    const b = this.boundary;
    if (!b) return '';
    const label = b.subjectDisplay?.label;
    if (label) return label;
    switch (b.subject.kind) {
      case 'principal':
        return `${b.subject.principal.type}: ${b.subject.principal.id}`;
      case 'group_closure':
        return `Group closure: ${b.subject.groupId}`;
      case 'all_principals':
        return 'All principals';
    }
  }

  private scopeDescription(): string {
    const b = this.boundary;
    if (!b) return '';
    if (b.scopeDisplay?.label) return b.scopeDisplay.label;
    return b.scope.type === 'system'
      ? 'System-wide'
      : `Project: ${b.scope.type === 'project' ? b.scope.projectId : ''}`;
  }

  private actorDisplay(
    ref: { displayName?: string | null; id: string; type: string } | null
  ): string {
    if (!ref) return '—';
    return ref.displayName ?? ref.id;
  }

  // ---------------------------------------------------------------------------
  // Event handlers
  // ---------------------------------------------------------------------------

  private handleEditClick(): void {
    navigateTo(`/admin/access-boundaries/${encodeURIComponent(this.boundaryId)}/edit`);
  }

  private handleDeleteClick(): void {
    // Delete opens a relaxation preview, NOT a direct delete
    this.showDeletePreview = true;
  }

  private handleDeleteSuccess(e: CustomEvent<PreviewCommitSuccessDetail>): void {
    void e;
    // After successful delete, navigate to inventory
    navigateTo('/admin/access-boundaries');
  }

  private handleDeleteCancel(): void {
    this.showDeletePreview = false;
  }

  private handleAffectedPageRequest(e: CustomEvent<PageRequestDetail>): void {
    void this.loadAffectedPrincipals(e.detail.pageToken);
  }

  private handleAuditPageRequest(e: CustomEvent<AuditPageRequestDetail>): void {
    void this.loadAuditEvents(e.detail.pageToken);
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    switch (this.phase) {
      case 'loading':
        return this.renderLoading();
      case 'error':
        return this.renderError();
      case 'not_found':
        return this.renderNotFound();
      case 'permission_denied':
        return this.renderPermissionDenied();
      case 'ready':
      case 'deleting':
        return this.renderDetail();
      default:
        return nothing;
    }
  }

  private renderPermissionDenied() {
    return html`
      <div class="detail-page">
        <div class="permission-denied-state" role="alert">
          <sl-icon name="shield-lock"></sl-icon>
          <h1>Permission Denied</h1>
          <p>
            You do not have permission to view this access boundary. Contact your administrator.
          </p>
          <sl-button
            variant="default"
            style="margin-top: 1rem;"
            @click=${() => navigateTo('/admin/access-boundaries')}
          >
            Back to inventory
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderLoading() {
    return html`
      <div class="detail-page">
        <div class="skeleton-section">
          <div class="skeleton-header"></div>
          <div class="skeleton-body">
            <div class="skeleton-line" style="width: 60%"></div>
            <div class="skeleton-line" style="width: 40%"></div>
            <div class="skeleton-line" style="width: 80%"></div>
          </div>
        </div>
        <div class="skeleton-section">
          <div class="skeleton-header"></div>
          <div class="skeleton-body">
            <div class="skeleton-line" style="width: 50%"></div>
            <div class="skeleton-line" style="width: 70%"></div>
          </div>
        </div>
        <div class="skeleton-section">
          <div class="skeleton-header"></div>
          <div class="skeleton-body">
            <div class="skeleton-line" style="width: 90%"></div>
            <div class="skeleton-line" style="width: 60%"></div>
            <div class="skeleton-line" style="width: 75%"></div>
          </div>
        </div>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="detail-page">
        <div class="error-state">
          <sl-icon name="exclamation-circle" style="font-size: 2rem"></sl-icon>
          <h1>Failed to Load Access Boundary</h1>
          <p>${this.errorMessage}</p>
          <div style="display: flex; gap: 0.5rem; justify-content: center;">
            <sl-button variant="default" @click=${() => navigateTo('/admin/access-boundaries')}>
              Back to inventory
            </sl-button>
            <sl-button variant="primary" @click=${() => void this.loadBoundary()}>
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              Retry
            </sl-button>
          </div>
        </div>
      </div>
    `;
  }

  private renderNotFound() {
    return html`
      <div class="detail-page">
        <div class="not-found-state">
          <sl-icon name="shield-lock" style="font-size: 2rem"></sl-icon>
          <h1>Access Boundary Not Found</h1>
          <p>
            The access boundary "${this.boundaryId}" does not exist or you do not have permission to
            view it.
          </p>
          <sl-button variant="primary" @click=${() => navigateTo('/admin/access-boundaries')}>
            Back to inventory
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderDetail() {
    const b = this.boundary;
    if (!b) return nothing;

    return html`
      <div class="detail-page">
        <!-- Header -->
        ${this.renderPageHeader(b)}

        <!-- Recovery-disabled banner -->
        ${this.isRecoveryDisabled ? this.renderRecoveryBanner(b) : nothing}

        <!-- Sections -->
        <div class="detail-sections">
          ${this.renderDefinitionSection(b)} ${this.renderPermissionsSection(b)}
          ${this.renderAffectedAccessSection(b)} ${this.renderScheduleSection(b)}
          ${this.renderAuditRecoverySection(b)}
        </div>

        <!-- Delete preview overlay -->
        ${this.showDeletePreview
          ? html`
              <div class="delete-preview-overlay">
                <scion-access-boundary-preview
                  operation="delete"
                  constraintId=${this.boundaryId}
                  baseRevision=${b.revision}
                  autoStart
                  @preview-commit-success=${(e: CustomEvent<PreviewCommitSuccessDetail>) =>
                    this.handleDeleteSuccess(e)}
                  @preview-cancel=${() => this.handleDeleteCancel()}
                ></scion-access-boundary-preview>
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderPageHeader(b: AccessBoundaryDetail) {
    return html`
      <div class="page-header">
        <div class="header-top">
          <a
            class="back-link"
            href="/admin/access-boundaries"
            @click=${(e: Event) => {
              e.preventDefault();
              navigateTo('/admin/access-boundaries');
            }}
          >
            <sl-icon name="arrow-left"></sl-icon>
            Access Boundaries
          </a>
        </div>

        <div class="header-main">
          <div class="header-info">
            <h1 class="boundary-name">${b.name}</h1>
            <div class="header-badges">
              <scion-access-boundary-status
                status=${b.status}
                .risk=${b.risk}
              ></scion-access-boundary-status>
            </div>
            <div class="header-meta">
              <span>${this.scopeDescription()}</span>
              <span>${this.subjectDescription()}</span>
              ${b.updatedAt
                ? html`<span>Updated ${this.formatRelativeTime(b.updatedAt)}</span>`
                : nothing}
              ${b.updatedBy ? html`<span>by ${this.actorDisplay(b.updatedBy)}</span>` : nothing}
            </div>
          </div>

          ${!this.isRecoveryDisabled
            ? html`
                <div class="header-actions">
                  ${this.canEdit
                    ? html`
                        <sl-button
                          variant="default"
                          size="small"
                          @click=${() => this.handleEditClick()}
                        >
                          <sl-icon slot="prefix" name="pencil"></sl-icon>
                          Edit
                        </sl-button>
                      `
                    : nothing}
                  ${this.canDelete
                    ? html`
                        <sl-button
                          variant="text"
                          size="small"
                          style="color: var(--sl-color-danger-600)"
                          @click=${() => this.handleDeleteClick()}
                        >
                          Delete
                        </sl-button>
                      `
                    : nothing}
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private renderRecoveryBanner(b: AccessBoundaryDetail) {
    return html`
      <div class="recovery-disabled-banner" role="alert">
        <sl-icon name="lock"></sl-icon>
        <div class="recovery-banner-content">
          <div class="recovery-banner-title">Recovery-disabled — this boundary is immutable</div>
          <div class="recovery-banner-text">
            This access boundary has been disabled by an operator and cannot be modified through the
            web interface. Contact your system administrator for recovery procedures.
            ${b.recovery.reenableGuidance ? html`<br />${b.recovery.reenableGuidance}` : nothing}
          </div>
        </div>
      </div>
    `;
  }

  private renderDefinitionSection(b: AccessBoundaryDetail) {
    return html`
      <div class="detail-section">
        <div class="section-header">
          <sl-icon name="file-text"></sl-icon>
          Definition
        </div>
        <div class="section-body">
          <div class="definition-grid">
            <span class="def-label">Purpose</span>
            <span class="def-value purpose">${b.purpose}</span>

            <span class="def-label">Subject</span>
            <span class="def-value">${this.subjectDescription()}</span>

            <span class="def-label">Scope</span>
            <span class="def-value">${this.scopeDescription()}</span>

            ${b.appliesWhen?.notBefore
              ? html`
                  <span class="def-label">Starts</span>
                  <span class="def-value">${this.formatDatetime(b.appliesWhen.notBefore)}</span>
                `
              : nothing}
            ${b.appliesWhen?.expiresAt
              ? html`
                  <span class="def-label">Expires</span>
                  <span class="def-value">${this.formatDatetime(b.appliesWhen.expiresAt)}</span>
                `
              : nothing}

            <span class="def-label">Created</span>
            <span class="def-value">
              ${this.formatDatetime(b.createdAt)}
              ${b.createdBy ? ` by ${this.actorDisplay(b.createdBy)}` : ''}
            </span>

            <span class="def-label">Updated</span>
            <span class="def-value">
              ${this.formatDatetime(b.updatedAt)}
              ${b.updatedBy ? ` by ${this.actorDisplay(b.updatedBy)}` : ''}
            </span>

            <span class="def-label">Revision</span>
            <span class="def-value mono">${b.revision}</span>
          </div>
        </div>
      </div>
    `;
  }

  private renderPermissionsSection(b: AccessBoundaryDetail) {
    const retained = b.maximumPermissions;
    const registry = b.permissionRegistry;
    const excludedCount = registry.excludedPermissionCount;
    const newSince = registry.newSincePermissionIds;

    return html`
      <div class="detail-section">
        <div class="section-header">
          <sl-icon name="shield-lock"></sl-icon>
          Permissions
        </div>
        <div class="section-body">
          <div class="perm-section">
            <div class="perm-section-title">
              Retained (${retained.length} of ${registry.totalPermissionCount})
            </div>
            <div class="perm-count">These permissions are available to affected principals</div>
            ${retained.length > 0
              ? html`
                  <div class="perm-list">
                    ${retained.map((p) => html`<span class="perm-tag retained">${p}</span>`)}
                  </div>
                `
              : html`<p style="color: var(--scion-text-muted); font-size: 0.875rem;">
                  No permissions retained
                </p>`}
          </div>

          <div class="perm-section">
            <div class="perm-section-title">Removed (${excludedCount})</div>
            <div class="perm-count">These permissions are removed from all affected principals</div>
          </div>

          ${newSince.length > 0
            ? html`
                <div class="perm-section">
                  <div class="perm-section-title">
                    Newly registered since last edit (${newSince.length})
                  </div>
                  <div class="perm-count">
                    Permissions registered after revision ${registry.newSinceRevision ?? '—'}. These
                    are excluded by default.
                  </div>
                  <div class="perm-list">
                    ${newSince.map((p) => html`<span class="perm-tag new-since">${p}</span>`)}
                  </div>
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private renderAffectedAccessSection(b: AccessBoundaryDetail) {
    return html`
      <div class="detail-section">
        <div class="section-header">
          <sl-icon name="people"></sl-icon>
          Affected access
        </div>
        <div class="section-body">
          ${b.effect
            ? html`
                <div class="effect-summary">
                  <div class="effect-stat">
                    <div class="effect-stat-label">Affected</div>
                    <div class="effect-stat-value">
                      ${b.effect.affectedPrincipalCount}${b.effect.affectedPrincipalCountExact ===
                      false
                        ? '+'
                        : ''}
                    </div>
                  </div>
                  <div class="effect-stat">
                    <div class="effect-stat-label">Losing authority</div>
                    <div class="effect-stat-value" style="color: var(--sl-color-danger-600)">
                      ${b.effect.principalsLosingAuthorityCount}
                    </div>
                  </div>
                  <div class="effect-stat">
                    <div class="effect-stat-label">Intersecting</div>
                    <div class="effect-stat-value">${b.effect.intersectingBoundaryCount}</div>
                  </div>
                </div>
              `
            : nothing}

          <div
            class="affected-table-scroll"
            tabindex="0"
            role="region"
            aria-label="Affected principals, scrollable"
          >
            <scion-affected-principals-table
              .principals=${this.affectedPrincipals}
              .nextPageToken=${this.affectedNextToken}
              .totalCount=${this.affectedTotalCount}
              .totalCountExact=${this.affectedTotalCountExact}
              .loading=${this.loadingAffected}
              mode="detail"
              @page-request=${(e: CustomEvent<PageRequestDetail>) =>
                this.handleAffectedPageRequest(e)}
            ></scion-affected-principals-table>
          </div>

          ${b.intersectingBoundaries.length > 0
            ? html`
                <div style="margin-top: 1rem;">
                  <div class="perm-section-title">
                    Intersecting boundaries (${b.intersectingBoundaries.length})
                  </div>
                  <ul style="list-style: none; padding: 0; margin: 0;">
                    ${b.intersectingBoundaries.map(
                      (ib) => html`
                        <li
                          style="padding: 0.375rem 0; border-bottom: 1px solid var(--scion-border, #e2e8f0); font-size: 0.8125rem;"
                        >
                          <strong>${ib.name ?? '(unavailable)'}</strong>
                          — ${ib.netEffectNote} (${ib.overlappingPermissionCount} overlapping)
                        </li>
                      `
                    )}
                  </ul>
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private renderScheduleSection(b: AccessBoundaryDetail) {
    const hasSchedule = b.appliesWhen?.notBefore || b.appliesWhen?.expiresAt;

    return html`
      <div class="detail-section">
        <div class="section-header">
          <sl-icon name="clock"></sl-icon>
          Schedule
        </div>
        <div class="section-body">
          <div class="schedule-info">
            <div class="schedule-item">
              <span class="schedule-label">Status:</span>
              <scion-access-boundary-status
                status=${b.status}
                size="small"
              ></scion-access-boundary-status>
            </div>

            ${hasSchedule
              ? html`
                  ${b.appliesWhen?.notBefore
                    ? html`
                        <div class="schedule-item">
                          <span class="schedule-label">Starts:</span>
                          <span>${this.formatDatetime(b.appliesWhen.notBefore)}</span>
                        </div>
                      `
                    : nothing}
                  ${b.appliesWhen?.expiresAt
                    ? html`
                        <div class="schedule-item">
                          <span class="schedule-label">Expires:</span>
                          <span>${this.formatDatetime(b.appliesWhen.expiresAt)}</span>
                        </div>
                      `
                    : nothing}
                `
              : html`
                  <div class="schedule-item">
                    <span>Active immediately, no expiration</span>
                  </div>
                `}
            ${b.effect?.note
              ? html`
                  <div
                    class="schedule-item"
                    style="color: var(--scion-text-muted); font-size: 0.8125rem;"
                  >
                    ${b.effect.note}
                  </div>
                `
              : nothing}
          </div>
        </div>
      </div>
    `;
  }

  private renderAuditRecoverySection(b: AccessBoundaryDetail) {
    return html`
      <div class="detail-section">
        <div class="section-header">
          <sl-icon name="journal-text"></sl-icon>
          Audit & recovery
        </div>
        <div class="section-body">
          ${b.recovery.disabled ? this.renderRecoveryDetails(b) : nothing}

          <scion-access-boundary-audit-timeline
            .events=${this.auditEvents}
            .nextPageToken=${this.auditNextToken}
            .totalCount=${this.auditTotalCount}
            .loading=${this.loadingAudit}
            @audit-page-request=${(e: CustomEvent<AuditPageRequestDetail>) =>
              this.handleAuditPageRequest(e)}
          ></scion-access-boundary-audit-timeline>
        </div>
      </div>
    `;
  }

  private renderRecoveryDetails(b: AccessBoundaryDetail) {
    const r = b.recovery;
    return html`
      <div
        style="margin-bottom: 1rem; padding: 0.75rem; background: var(--sl-color-danger-50); border-radius: var(--scion-radius, 0.5rem); border: 1px solid var(--sl-color-danger-200);"
      >
        <div class="recovery-details">
          ${r.disabledAt
            ? html`
                <div class="recovery-detail-row">
                  <span class="recovery-detail-label">Disabled at</span>
                  <span class="recovery-detail-value">${this.formatDatetime(r.disabledAt)}</span>
                </div>
              `
            : nothing}
          ${r.disabledBy
            ? html`
                <div class="recovery-detail-row">
                  <span class="recovery-detail-label">Disabled by</span>
                  <span class="recovery-detail-value">
                    ${this.actorDisplay(r.disabledBy)}
                    ${r.disabledBy.credentialType ? ` (${r.disabledBy.credentialType})` : ''}
                  </span>
                </div>
              `
            : nothing}
          ${r.disabledReason
            ? html`
                <div class="recovery-detail-row">
                  <span class="recovery-detail-label">Reason</span>
                  <span class="recovery-detail-value">${r.disabledReason}</span>
                </div>
              `
            : nothing}
          ${r.auditEventId
            ? html`
                <div class="recovery-detail-row">
                  <span class="recovery-detail-label">Audit event</span>
                  <span
                    class="recovery-detail-value"
                    style="font-family: monospace; font-size: 0.8125rem;"
                  >
                    ${r.auditEventId}
                  </span>
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-access-boundary-detail': ScionPageAdminAccessBoundaryDetail;
  }
}
