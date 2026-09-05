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
 * Access Boundary Preview
 *
 * Full preview/commit flow component.
 *
 * 1. Starts a preview via the API client with the current draft.
 *    Handles both sync (201) and async (202) responses.
 * 2. Displays preview results: validity timer, classification,
 *    completeness, lockout check, definition summary, impact
 *    counts, permission diffs, affected principals, intersecting
 *    boundaries, and temporal states.
 * 3. Commit controls with proper gating:
 *    - Enabled only when preview is valid, complete, safe, and capable.
 *    - Commits with preview token + If-Match + full draft.
 *    - Handles unknown outcome, success (navigate), and error.
 * 4. Cancel returns to editor with draft preserved.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import * as accessBoundariesApi from '../../client/access-boundaries-api.js';
import type {
  AccessConstraintDraft,
  AccessBoundaryPreview as PreviewData,
  AccessBoundaryPreviewRequest,
  AccessBoundaryPreviewJob,
  PreviewOperation,
  BoundaryRevision,
  IntersectingBoundary,
  AffectedPrincipal,
  PageToken,
} from '../../shared/access-boundaries.js';
import { canAccessBoundary } from '../../shared/access-boundaries.js';

// Import sub-components
import './access-boundary-status.js';
import './access-boundary-impact-summary.js';
import './affected-principals-table.js';

import type { PageRequestDetail } from './affected-principals-table.js';

type PreviewPhase = 'idle' | 'loading' | 'polling' | 'ready' | 'committing' | 'error';

/** Event fired when commit succeeds; detail is the boundary ID to navigate to. */
export interface PreviewCommitSuccessDetail {
  boundaryId: string;
}

/** Event fired when user clicks cancel/back to editor. */
export interface PreviewCancelDetail {
  reason: 'cancel' | 'error';
}

@customElement('scion-access-boundary-preview')
export class ScionAccessBoundaryPreview extends LitElement {
  /** The draft being previewed. */
  @property({ type: Object }) draft: AccessConstraintDraft | null = null;

  /** The preview operation type. */
  @property() operation: PreviewOperation = 'create';

  /** The constraint ID (for update/delete). */
  @property() constraintId = '';

  /** The base revision (for update/delete). */
  @property() baseRevision: BoundaryRevision = '';

  /** Whether to auto-start the preview on connect. */
  @property({ type: Boolean }) autoStart = false;

  // --- Internal state ---
  @state() private phase: PreviewPhase = 'idle';
  @state() private preview: PreviewData | null = null;
  @state() private jobProgress: AccessBoundaryPreviewJob['progress'] | null = null;
  @state() private error = '';
  @state() private commitError = '';
  @state() private expiryTimeLeft = '';
  @state() private isExpired = false;

  // Affected principals pagination
  @state() private allPrincipals: AffectedPrincipal[] = [];
  @state() private principalsNextToken: PageToken | undefined;
  @state() private principalsTotalCount = 0;
  @state() private principalsTotalCountExact = true;
  @state() private loadingMorePrincipals = false;

  private abortController: AbortController | null = null;
  private expiryTimer: ReturnType<typeof setInterval> | null = null;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .preview-container {
        display: flex;
        flex-direction: column;
        gap: 1.25rem;
      }

      /* Header */
      .preview-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        flex-wrap: wrap;
        gap: 0.5rem;
      }

      .preview-title {
        font-size: 1rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .expiry-badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
      }

      .expiry-badge.valid {
        background: var(--sl-color-success-50, #f0fdf4);
        color: var(--sl-color-success-700, #15803d);
        border: 1px solid var(--sl-color-success-200, #bbf7d0);
      }

      .expiry-badge.expiring {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-700, #b45309);
        border: 1px solid var(--sl-color-warning-200, #fde68a);
      }

      .expiry-badge.expired {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
      }

      /* Sections */
      .preview-section {
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        overflow: hidden;
      }

      .section-title {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        padding: 0.625rem 1rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .section-body {
        padding: 0.75rem 1rem;
      }

      /* Warnings */
      .warnings {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
      }

      .warning-item {
        display: flex;
        align-items: flex-start;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.8125rem;
      }

      .warning-item sl-icon {
        flex-shrink: 0;
        margin-top: 0.125rem;
      }

      .warning-item.info {
        background: var(--sl-color-primary-50, #eff6ff);
        color: var(--sl-color-primary-700, #1d4ed8);
        border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      }

      .warning-item.warning {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-700, #b45309);
        border: 1px solid var(--sl-color-warning-200, #fde68a);
      }

      .warning-item.error {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
      }

      /* Definition summary */
      .definition-grid {
        display: grid;
        grid-template-columns: auto 1fr;
        gap: 0.25rem 1rem;
        font-size: 0.8125rem;
      }

      .def-label {
        color: var(--scion-text-muted, #64748b);
        font-weight: 500;
        white-space: nowrap;
      }

      .def-value {
        color: var(--scion-text, #1e293b);
      }

      /* Intersecting boundaries */
      .intersecting-list {
        list-style: none;
        padding: 0;
        margin: 0;
      }

      .intersecting-item {
        display: flex;
        align-items: flex-start;
        gap: 0.5rem;
        padding: 0.375rem 0;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        font-size: 0.8125rem;
      }

      .intersecting-item:last-child {
        border-bottom: none;
      }

      .intersecting-name {
        font-weight: 500;
        color: var(--scion-text, #1e293b);
      }

      .intersecting-note {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .intersecting-relationship {
        font-size: 0.6875rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
        font-weight: 500;
        white-space: nowrap;
      }

      /* Temporal states */
      .temporal-states {
        display: flex;
        flex-direction: column;
        gap: 0.5rem;
      }

      .temporal-state {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        padding: 0.375rem 0.5rem;
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.8125rem;
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .temporal-label {
        font-weight: 500;
        color: var(--scion-text, #1e293b);
        min-width: 80px;
      }

      .temporal-time {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      /* Commit blocked message */
      .commit-blocked {
        padding: 0.625rem 0.75rem;
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.8125rem;
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .commit-blocked sl-icon {
        flex-shrink: 0;
      }

      /* Commit error */
      .commit-error {
        padding: 0.625rem 0.75rem;
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.8125rem;
      }

      /* Actions */
      .preview-actions {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding-top: 1rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      /* Loading state */
      .loading-state {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 0.75rem;
        padding: 3rem 2rem;
        text-align: center;
        color: var(--scion-text-muted, #64748b);
      }

      .loading-state sl-spinner {
        font-size: 2rem;
      }

      .loading-state .progress-info {
        font-size: 0.8125rem;
      }

      /* Error state */
      .error-state {
        text-align: center;
        padding: 2rem;
      }

      .error-state sl-icon {
        font-size: 2rem;
        color: var(--sl-color-danger-500, #ef4444);
        margin-bottom: 0.5rem;
      }

      .error-state p {
        color: var(--sl-color-danger-700, #b91c1c);
        margin: 0 0 1rem;
      }

      .error-actions {
        display: flex;
        gap: 0.5rem;
        justify-content: center;
      }

      .def-value {
        overflow-wrap: anywhere;
      }

      .intersecting-name {
        overflow-wrap: anywhere;
      }

      @media (max-width: 768px) {
        .preview-header {
          flex-direction: column;
          align-items: flex-start;
        }

        .preview-actions {
          flex-direction: column;
          gap: 0.75rem;
        }

        .preview-actions sl-button {
          width: 100%;
        }

        .definition-grid {
          grid-template-columns: 1fr;
          gap: 0.125rem;
        }

        .def-label {
          font-weight: 600;
          margin-top: 0.375rem;
        }

        .section-body {
          padding: 0.5rem 0.75rem;
        }

        .temporal-state {
          flex-wrap: wrap;
          gap: 0.375rem;
        }

        .intersecting-item {
          flex-direction: column;
          gap: 0.25rem;
        }

        .warning-item {
          font-size: 0.75rem;
        }

        .commit-blocked {
          font-size: 0.75rem;
        }
      }

      @media (forced-colors: active) {
        .preview-section {
          border-color: ButtonText;
        }

        .section-title {
          border-bottom-color: ButtonText;
        }

        .expiry-badge {
          border: 1px solid ButtonText;
        }

        .warning-item {
          border: 1px solid ButtonText;
        }

        .commit-blocked {
          border: 1px solid ButtonText;
        }

        .commit-error {
          border: 1px solid ButtonText;
        }

        .preview-actions {
          border-top-color: ButtonText;
        }

        .temporal-state {
          border: 1px solid ButtonText;
        }
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.autoStart) {
      void this.startPreview();
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.cleanup();
  }

  private cleanup(): void {
    this.abortController?.abort();
    this.abortController = null;
    if (this.expiryTimer !== null) {
      clearInterval(this.expiryTimer);
      this.expiryTimer = null;
    }
  }

  /** Start or restart a preview. */
  async startPreview(): Promise<void> {
    if (!this.draft && this.operation !== 'delete') {
      this.error = 'No draft provided for preview';
      this.phase = 'error';
      return;
    }

    this.cleanup();
    this.phase = 'loading';
    this.error = '';
    this.commitError = '';
    this.preview = null;
    this.jobProgress = null;

    this.abortController = new AbortController();

    try {
      const request = this.buildPreviewRequest();
      const result = await accessBoundariesApi.preview(request, {
        signal: this.abortController.signal,
      });

      if (result.kind === 'preview') {
        this.handlePreviewReady(result.preview);
      } else {
        // Async preview — poll
        this.phase = 'polling';
        const job = await accessBoundariesApi.pollPreviewJobUntilDone(result.job.jobId, {
          signal: this.abortController.signal,
          onProgress: (j) => {
            this.jobProgress = j.progress ?? null;
          },
        });

        if (job.status === 'succeeded' && job.preview) {
          this.handlePreviewReady(job.preview);
        } else if (job.status === 'failed') {
          this.error = job.error?.message ?? 'Preview job failed';
          this.phase = 'error';
        } else if (job.status === 'cancelled') {
          this.error = 'Preview was cancelled';
          this.phase = 'error';
        }
      }
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') {
        return;
      }
      this.error = err instanceof Error ? err.message : 'Failed to generate preview';
      this.phase = 'error';
      console.error('Preview failed:', err);
    }
  }

  private buildPreviewRequest(): AccessBoundaryPreviewRequest {
    switch (this.operation) {
      case 'create':
        return { operation: 'create', draft: this.draft! };
      case 'update':
        return {
          operation: 'update',
          constraintId: this.constraintId,
          baseRevision: this.baseRevision,
          draft: this.draft!,
        };
      case 'delete':
        return {
          operation: 'delete',
          constraintId: this.constraintId,
          baseRevision: this.baseRevision,
        };
    }
  }

  private handlePreviewReady(preview: PreviewData): void {
    this.preview = preview;
    this.phase = 'ready';

    // Initialize principals from preview's inline page
    if (preview.principalsPage) {
      this.allPrincipals = [...preview.principalsPage.items];
      this.principalsNextToken = preview.principalsPage.nextPageToken;
      this.principalsTotalCount = preview.principalsPage.totalCount;
      this.principalsTotalCountExact = preview.principalsPage.totalCountExact;
    }

    // Start expiry timer
    this.startExpiryTimer();
  }

  private startExpiryTimer(): void {
    if (this.expiryTimer !== null) {
      clearInterval(this.expiryTimer);
    }
    this.updateExpiryTime();
    this.expiryTimer = setInterval(() => this.updateExpiryTime(), 1000);
  }

  private updateExpiryTime(): void {
    if (!this.preview?.expiresAt) return;

    const expiresMs = new Date(this.preview.expiresAt).getTime();
    const nowMs = Date.now();
    const diff = expiresMs - nowMs;

    if (diff <= 0) {
      this.isExpired = true;
      this.expiryTimeLeft = 'Expired';
      if (this.expiryTimer !== null) {
        clearInterval(this.expiryTimer);
        this.expiryTimer = null;
      }
      return;
    }

    this.isExpired = false;
    const totalSeconds = Math.floor(diff / 1000);
    const minutes = Math.floor(totalSeconds / 60);
    const seconds = totalSeconds % 60;
    this.expiryTimeLeft = `${minutes}:${seconds.toString().padStart(2, '0')}`;
  }

  private get canCommit(): boolean {
    if (!this.preview) return false;
    if (this.isExpired) return false;
    if (!this.preview.completeness.complete) return false;
    if (this.preview.completeness.truncated || this.preview.completeness.degraded) return false;
    if (this.preview.lockout?.safe === false) return false;
    if (!canAccessBoundary(this.preview._capabilities, 'commit')) return false;
    if (this.preview.commitBlocked) return false;
    return true;
  }

  private get commitBlockedReason(): string {
    if (!this.preview) return '';
    if (this.isExpired) return 'Preview has expired. Refresh to generate a new preview.';
    if (this.preview.commitBlocked) return this.preview.commitBlocked.message;
    if (!this.preview.completeness.complete) return 'Preview is incomplete — commit not available.';
    if (this.preview.completeness.truncated)
      return 'Preview results are truncated — commit not available.';
    if (this.preview.completeness.degraded) return 'Preview is degraded — commit not available.';
    if (this.preview.lockout?.safe === false)
      return 'This change would lock out all access boundary administrators.';
    if (!canAccessBoundary(this.preview._capabilities, 'commit'))
      return 'You do not have permission to commit this change.';
    return '';
  }

  private async handleCommit(): Promise<void> {
    if (!this.preview || !this.canCommit) return;

    this.phase = 'committing';
    this.commitError = '';

    try {
      const acknowledgements = {
        acknowledgedClassification: this.preview.classification,
        acknowledgedLosingPrincipalCount: this.preview.impact.losingPrincipalCount,
        acknowledgedRegainingPrincipalCount: this.preview.impact.regainingPrincipalCount,
      };

      let response;
      if (this.operation === 'create') {
        response = await accessBoundariesApi.commitCreate({
          previewToken: this.preview.previewToken,
          draft: this.draft!,
          acknowledgements,
        });
      } else if (this.operation === 'delete') {
        response = await accessBoundariesApi.commitDelete(this.constraintId, this.baseRevision, {
          previewToken: this.preview.previewToken,
          acknowledgements,
        });
      } else {
        response = await accessBoundariesApi.commitUpdate(this.constraintId, this.baseRevision, {
          previewToken: this.preview.previewToken,
          draft: this.draft!,
          acknowledgements,
        });
      }

      // Success — emit event with the boundary ID
      const boundaryId =
        'id' in response.constraint
          ? (response.constraint as { id: string }).id
          : this.constraintId;

      this.dispatchEvent(
        new CustomEvent<PreviewCommitSuccessDetail>('preview-commit-success', {
          detail: { boundaryId },
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      // Check if unknown outcome (no HTTP response received) — refetch.
      // All browsers throw TypeError for fetch network failures. If the error
      // has no httpStatus, no HTTP response was received (unknown outcome).
      const isNetworkError =
        err instanceof TypeError || (err instanceof Error && !('httpStatus' in err));
      if (isNetworkError) {
        try {
          if (this.constraintId) {
            const refetchResult = await accessBoundariesApi.refetchAfterUnknownOutcome(
              this.constraintId,
              this.baseRevision
            );
            if (refetchResult.revisionChanged) {
              // The mutation likely applied
              this.dispatchEvent(
                new CustomEvent<PreviewCommitSuccessDetail>('preview-commit-success', {
                  detail: { boundaryId: this.constraintId },
                  bubbles: true,
                  composed: true,
                })
              );
              return;
            }
          }
        } catch {
          // Refetch also failed — fall through to error
        }
      }

      this.phase = 'ready';

      if (err instanceof accessBoundariesApi.AccessBoundaryAPIError) {
        this.commitError = `${err.code ? `${err.code}: ` : ''}${err.message}`;

        if (accessBoundariesApi.isRetryableAfterRepreview(err)) {
          this.commitError += ' — refresh preview to try again.';
        }
      } else {
        this.commitError = err instanceof Error ? err.message : 'Commit failed';
      }

      console.error('Commit failed:', err);
    }
  }

  private handleCancel(): void {
    this.cleanup();
    this.dispatchEvent(
      new CustomEvent<PreviewCancelDetail>('preview-cancel', {
        detail: { reason: 'cancel' },
        bubbles: true,
        composed: true,
      })
    );
  }

  private async handleLoadMorePrincipals(e: CustomEvent<PageRequestDetail>): Promise<void> {
    if (!this.preview || this.loadingMorePrincipals) return;

    // In preview mode there is no preview-specific pagination endpoint.
    // Fetching from listAffected() would return committed-state data, silently
    // mixing two snapshots. Guard against this by returning early.
    if (this.preview.previewToken) {
      return;
    }

    this.loadingMorePrincipals = true;
    try {
      if (this.constraintId) {
        const page = await accessBoundariesApi.listAffected(this.constraintId, {
          pageToken: e.detail.pageToken,
        });
        this.allPrincipals = [...this.allPrincipals, ...page.items];
        this.principalsNextToken = page.nextPageToken;
        this.principalsTotalCount = page.totalCount;
        this.principalsTotalCountExact = page.totalCountExact;
      }
    } catch (err) {
      console.error('Failed to load more principals:', err);
    } finally {
      this.loadingMorePrincipals = false;
    }
  }

  private relationshipLabel(r: IntersectingBoundary['relationship']): string {
    switch (r) {
      case 'narrows':
        return 'Narrows';
      case 'overlaps':
        return 'Overlaps';
      case 'limits_relaxation':
        return 'Limits relaxation';
      case 'blocks_relaxation':
        return 'Blocks relaxation';
      default:
        return r;
    }
  }

  private formatDatetime(iso: string): string {
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

  private commitButtonLabel(): string {
    switch (this.operation) {
      case 'create':
        return 'Create boundary';
      case 'update':
        return 'Update boundary';
      case 'delete':
        return 'Delete boundary';
      default:
        return 'Commit';
    }
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    switch (this.phase) {
      case 'idle':
        return this.renderIdle();
      case 'loading':
      case 'polling':
        return this.renderLoading();
      case 'ready':
      case 'committing':
        return this.renderPreview();
      case 'error':
        return this.renderError();
      default:
        return nothing;
    }
  }

  private renderIdle() {
    return html`
      <div class="loading-state">
        <sl-icon
          name="shield-check"
          style="font-size: 2rem; color: var(--sl-color-primary-500)"
        ></sl-icon>
        <p>Click below to start impact preview</p>
        <sl-button variant="primary" @click=${() => void this.startPreview()}>
          Generate preview
        </sl-button>
      </div>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state" role="status" aria-live="polite">
        <sl-spinner></sl-spinner>
        <p>${this.phase === 'polling' ? 'Computing impact analysis...' : 'Starting preview...'}</p>
        ${this.jobProgress
          ? html`
              <div class="progress-info">
                ${this.jobProgress.phase}
                ${this.jobProgress.determinate && this.jobProgress.totalCount !== null
                  ? `: ${this.jobProgress.processedCount} / ${this.jobProgress.totalCount}`
                  : `: ${this.jobProgress.processedCount} processed`}
              </div>
            `
          : nothing}
        <sl-button variant="default" size="small" @click=${() => this.handleCancel()}>
          Cancel
        </sl-button>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state" role="alert">
        <sl-icon name="exclamation-circle"></sl-icon>
        <p>${this.error}</p>
        <div class="error-actions">
          <sl-button variant="default" @click=${() => this.handleCancel()}>
            Back to editor
          </sl-button>
          <sl-button variant="primary" @click=${() => void this.startPreview()}>
            Retry preview
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderPreview() {
    const p = this.preview;
    if (!p) return nothing;

    const expiryClass = this.isExpired
      ? 'expired'
      : this.expiryTimeLeft.startsWith('0:')
        ? 'expiring'
        : 'valid';

    return html`
      <div class="preview-container">
        <!-- Header -->
        <div class="preview-header">
          <div class="preview-title">
            Impact Preview
            <scion-access-boundary-status
              classification=${p.classification}
              size="small"
            ></scion-access-boundary-status>
          </div>
          <div style="display: flex; align-items: center; gap: 0.5rem;">
            <span class="expiry-badge ${expiryClass}">
              <sl-icon name="${this.isExpired ? 'x-circle' : 'clock'}"></sl-icon>
              ${this.isExpired ? 'Expired' : this.expiryTimeLeft}
            </span>
            <sl-button variant="text" size="small" @click=${() => void this.startPreview()}>
              <sl-icon name="arrow-clockwise" slot="prefix"></sl-icon>
              Refresh
            </sl-button>
          </div>
        </div>

        <!-- Warnings -->
        ${p.warnings.length > 0 ? this.renderWarnings() : nothing}

        <!-- Definition summary -->
        ${this.operation !== 'delete' && this.draft ? this.renderDefinitionSummary() : nothing}

        <!-- Impact -->
        <div class="preview-section">
          <div class="section-title">Impact</div>
          <div class="section-body">
            <scion-access-boundary-impact-summary
              .impact=${p.impact}
              .lockout=${p.lockout}
              .completeness=${p.completeness}
            ></scion-access-boundary-impact-summary>
          </div>
        </div>

        <!-- Temporal states -->
        ${p.temporalStates.length > 0
          ? html`
              <div class="preview-section">
                <div class="section-title">Timeline</div>
                <div class="section-body">
                  <div class="temporal-states">
                    ${p.temporalStates.map(
                      (ts) => html`
                        <div class="temporal-state">
                          <scion-access-boundary-status
                            classification=${ts.classification}
                            size="small"
                          ></scion-access-boundary-status>
                          <span class="temporal-label">${ts.label}</span>
                          <span class="temporal-time">
                            from ${this.formatDatetime(ts.from)}
                            ${ts.until ? ` until ${this.formatDatetime(ts.until)}` : ''}
                          </span>
                          <span style="font-size: 0.75rem; color: var(--scion-text-muted)">
                            ${ts.affectedPrincipalCount} affected, ${ts.removedPermissionCount}
                            removed
                          </span>
                        </div>
                      `
                    )}
                  </div>
                </div>
              </div>
            `
          : nothing}

        <!-- Intersecting boundaries -->
        ${p.intersectingBoundaries.length > 0
          ? html`
              <div class="preview-section">
                <div class="section-title">
                  Intersecting boundaries (${p.intersectingBoundaries.length})
                </div>
                <div class="section-body">
                  <ul class="intersecting-list">
                    ${p.intersectingBoundaries.map(
                      (b) => html`
                        <li class="intersecting-item">
                          <span class="intersecting-relationship">
                            ${this.relationshipLabel(b.relationship)}
                          </span>
                          <div>
                            <span
                              class="intersecting-name"
                              title="${b.name ?? '(name unavailable)'}"
                            >
                              ${b.name ?? '(name unavailable)'}
                            </span>
                            <div class="intersecting-note">
                              ${b.netEffectNote} (${b.overlappingPermissionCount} overlapping
                              permission${b.overlappingPermissionCount === 1 ? '' : 's'})
                            </div>
                          </div>
                        </li>
                      `
                    )}
                  </ul>
                </div>
              </div>
            `
          : nothing}

        <!-- Affected principals -->
        <div class="preview-section">
          <div class="section-title">Affected principals</div>
          <div class="section-body">
            <scion-affected-principals-table
              .principals=${this.allPrincipals}
              .nextPageToken=${undefined}
              .totalCount=${this.principalsTotalCount}
              .totalCountExact=${this.principalsTotalCountExact}
              .loading=${this.loadingMorePrincipals}
              mode="preview"
              @page-request=${(e: CustomEvent<PageRequestDetail>) =>
                void this.handleLoadMorePrincipals(e)}
            ></scion-affected-principals-table>
            ${this.principalsNextToken || this.principalsTotalCount > this.allPrincipals.length
              ? html`<p
                  style="font-size: 0.75rem; color: var(--scion-text-muted, #64748b); margin: 0.5rem 0 0; text-align: center;"
                >
                  Showing first ${this.allPrincipals.length} of
                  ${this.principalsTotalCountExact
                    ? this.principalsTotalCount
                    : `${this.principalsTotalCount}+`}
                  affected principals.
                </p>`
              : nothing}
          </div>
        </div>

        <!-- Commit blocked message -->
        ${!this.canCommit && this.commitBlockedReason
          ? html`
              <div class="commit-blocked" role="status" aria-live="polite">
                <sl-icon name="lock"></sl-icon>
                <span>${this.commitBlockedReason}</span>
              </div>
            `
          : nothing}

        <!-- Commit error -->
        ${this.commitError
          ? html`<div class="commit-error" role="alert">${this.commitError}</div>`
          : nothing}

        <!-- Actions -->
        <div class="preview-actions">
          <sl-button variant="default" @click=${() => this.handleCancel()}>
            <sl-icon name="arrow-left" slot="prefix"></sl-icon>
            Back to editor
          </sl-button>
          <sl-button
            variant="${this.operation === 'delete' ? 'danger' : 'primary'}"
            ?disabled=${!this.canCommit}
            ?loading=${this.phase === 'committing'}
            @click=${() => void this.handleCommit()}
          >
            ${this.commitButtonLabel()}
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderWarnings() {
    if (!this.preview) return nothing;
    return html`
      <div class="warnings">
        ${this.preview.warnings.map(
          (w) => html`
            <div class="warning-item ${w.severity}">
              <sl-icon
                name="${w.severity === 'error'
                  ? 'exclamation-circle'
                  : w.severity === 'warning'
                    ? 'exclamation-triangle'
                    : 'info-circle'}"
              ></sl-icon>
              <span>${w.message}</span>
            </div>
          `
        )}
      </div>
    `;
  }

  private renderDefinitionSummary() {
    if (!this.draft) return nothing;
    const d = this.draft;

    return html`
      <div class="preview-section">
        <div class="section-title">Definition</div>
        <div class="section-body">
          <div class="definition-grid">
            <span class="def-label">Name</span>
            <span class="def-value" title="${d.name}">${d.name}</span>
            <span class="def-label">Purpose</span>
            <span class="def-value">${d.purpose}</span>
            <span class="def-label">Subject</span>
            <span class="def-value">${this.subjectDescription(d.subject)}</span>
            <span class="def-label">Scope</span>
            <span class="def-value"
              >${d.scope.type === 'system'
                ? 'System-wide'
                : `Project: ${d.scope.type === 'project' ? d.scope.projectId : ''}`}</span
            >
            <span class="def-label">Retained</span>
            <span class="def-value"
              >${d.maximumPermissions.length}
              permission${d.maximumPermissions.length === 1 ? '' : 's'}</span
            >
            ${d.appliesWhen?.notBefore
              ? html`
                  <span class="def-label">Starts</span>
                  <span class="def-value">${this.formatDatetime(d.appliesWhen.notBefore)}</span>
                `
              : nothing}
            ${d.appliesWhen?.expiresAt
              ? html`
                  <span class="def-label">Expires</span>
                  <span class="def-value">${this.formatDatetime(d.appliesWhen.expiresAt)}</span>
                `
              : nothing}
          </div>
        </div>
      </div>
    `;
  }

  private subjectDescription(subject: AccessConstraintDraft['subject']): string {
    switch (subject.kind) {
      case 'principal':
        return `${subject.principal.type}: ${subject.principal.id}`;
      case 'group_closure':
        return `Group closure: ${subject.groupId}`;
      case 'all_principals':
        return 'All principals';
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-preview': ScionAccessBoundaryPreview;
  }
}
