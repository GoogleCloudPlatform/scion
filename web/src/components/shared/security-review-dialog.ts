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
 * Security Review Dialog (F6)
 *
 * Replaces the ordinary confirmation dialog when the backend returns
 * SECURITY_REVIEW_REQUIRED for operations that may restore access:
 *   - Group member removal/deletion
 *   - User suspension/deletion
 *   - RoleBinding removal/replacement/expiry
 *   - Nested group membership changes
 *
 * There is NO "I understand, continue" bypass. The server is authoritative.
 *
 * If the actor has the commit capability, "Review in Access boundaries" opens
 * the boundary preview for the proposed change. Otherwise, shows read-only
 * impact and "Contact a security administrator."
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

/* -------------------------------------------------------------------------- */
/* Types                                                                      */
/* -------------------------------------------------------------------------- */

/** One affected boundary entry returned by the server in a SECURITY_REVIEW_REQUIRED response. */
export interface SecurityReviewBoundary {
  /** Boundary ID for navigation. */
  boundaryId: string;
  /** Display name of the boundary. */
  name: string;
  /** Number of principals that may regain access. */
  principalCount: number;
  /** Label for the principal kind (e.g. "principals", "agents"). */
  principalLabel: string;
  /** Number of permissions that may be regained. */
  permissionCount: number;
}

/** Lockout conflict details returned by the server. */
export interface LockoutConflict {
  /** Affected scope description. */
  affectedScope: string;
  /** What invariant failed. */
  invariantDescription: string;
  /** Human-readable administrator records/paths. */
  adminRecords: string[];
  /** Suggested resolutions. */
  suggestions: string[];
}

export interface SecurityReviewDetail {
  /** The entity being changed (e.g. "user@example.com"). */
  entityLabel: string;
  /** The group or context (e.g. "Admins group"). */
  contextLabel: string;
  /** Affected boundaries. */
  boundaries: SecurityReviewBoundary[];
  /** Whether the actor has the commit capability. */
  canCommit: boolean;
  /** Optional preview URL. */
  impactPreviewUrl?: string;
  /** Optional lockout conflict info. */
  lockout?: LockoutConflict;
}

/* -------------------------------------------------------------------------- */
/* Component                                                                  */
/* -------------------------------------------------------------------------- */

@customElement('scion-security-review-dialog')
export class ScionSecurityReviewDialog extends LitElement {
  /** Whether the dialog is open. */
  @property({ type: Boolean }) open = false;

  /** The review detail payload. */
  @property({ type: Object }) detail: SecurityReviewDetail | null = null;

  /** Loading state for any in-progress action. */
  @state() private actionInProgress = false;

  /** The element that had focus before the dialog opened. */
  private _previouslyFocusedElement: HTMLElement | null = null;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: contents;
      }

      .review-header {
        display: flex;
        align-items: center;
        gap: 0.75rem;
        margin-bottom: 1rem;
      }

      .review-header sl-icon {
        font-size: 1.5rem;
        color: var(--sl-color-warning-600, #d97706);
      }

      .review-header h3 {
        margin: 0;
        font-size: 1rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .review-description {
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
        line-height: 1.5;
        margin-bottom: 1rem;
      }

      .boundary-list {
        list-style: none;
        padding: 0;
        margin: 0 0 1rem 0;
      }

      .boundary-item {
        display: flex;
        align-items: flex-start;
        gap: 0.5rem;
        padding: 0.625rem 0.75rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        margin-bottom: 0.5rem;
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .boundary-item sl-icon {
        flex-shrink: 0;
        color: var(--sl-color-warning-600, #d97706);
        margin-top: 0.125rem;
      }

      .boundary-item-text {
        flex: 1;
        font-size: 0.8125rem;
        color: var(--scion-text, #1e293b);
        line-height: 1.4;
      }

      .boundary-name {
        font-weight: 600;
      }

      .review-note {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        line-height: 1.5;
        padding: 0.75rem;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        border-left: 3px solid var(--sl-color-warning-500, #eab308);
        margin-bottom: 1rem;
      }

      .contact-admin {
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
        padding: 0.75rem;
        background: var(--sl-color-neutral-100, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
        border: 1px solid var(--scion-border, #e2e8f0);
        margin-bottom: 1rem;
      }

      .contact-admin strong {
        display: block;
        margin-bottom: 0.25rem;
      }

      /* Lockout conflict display */
      .lockout-section {
        margin-bottom: 1rem;
      }

      .lockout-header {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        font-size: 0.875rem;
        font-weight: 600;
        color: var(--sl-color-danger-700, #b91c1c);
        margin-bottom: 0.5rem;
      }

      .lockout-header sl-icon {
        font-size: 1.125rem;
        color: var(--sl-color-danger-600, #dc2626);
      }

      .lockout-detail {
        font-size: 0.8125rem;
        color: var(--scion-text, #1e293b);
        line-height: 1.5;
        margin-bottom: 0.5rem;
      }

      .lockout-scope {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
        background: var(--sl-color-danger-50, #fef2f2);
        padding: 0.25rem 0.5rem;
        border-radius: var(--scion-radius, 0.5rem);
        display: inline-block;
        margin-bottom: 0.5rem;
      }

      .lockout-admins {
        list-style: none;
        padding: 0;
        margin: 0.5rem 0;
      }

      .lockout-admins li {
        font-size: 0.8125rem;
        padding: 0.25rem 0;
        color: var(--scion-text, #1e293b);
        font-family: var(--scion-font-mono, monospace);
      }

      .lockout-admins li::before {
        content: '• ';
        color: var(--sl-color-danger-500, #ef4444);
      }

      .lockout-suggestions {
        list-style: none;
        padding: 0;
        margin: 0.5rem 0;
      }

      .lockout-suggestions li {
        font-size: 0.8125rem;
        padding: 0.375rem 0.625rem;
        margin-bottom: 0.25rem;
        color: var(--scion-text, #1e293b);
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: var(--scion-radius, 0.5rem);
      }

      .lockout-suggestions li sl-icon {
        font-size: 0.75rem;
        margin-right: 0.25rem;
        color: var(--scion-text-muted, #64748b);
      }

      .lockout-recovery-note {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
        margin-top: 0.5rem;
      }

      /* Zoom / touch targets */
      sl-button {
        min-height: 44px;
      }

      .boundary-item-text,
      .lockout-detail,
      .review-description {
        overflow-wrap: anywhere;
      }

      /* Utility: screen-reader-only */

      /* Responsive: full-width dialog on mobile */
      @media (max-width: 768px) {
        sl-dialog::part(panel) {
          width: 100vw;
          max-width: 100vw;
          margin: 0;
          border-radius: 0;
          max-height: 100vh;
        }

        sl-dialog::part(body) {
          overflow-y: auto;
          max-height: calc(100vh - 8rem);
        }

        sl-dialog::part(footer) {
          position: sticky;
          bottom: 0;
          background: var(--scion-surface, #ffffff);
          border-top: 1px solid var(--scion-border, #e2e8f0);
          padding: 0.75rem;
        }
      }

      /* High contrast mode */
      @media (forced-colors: active) {
        .boundary-item {
          border: 1px solid ButtonText;
        }

        .review-note {
          border-left: 3px solid ButtonText;
        }

        .lockout-scope {
          border: 1px solid ButtonText;
        }

        sl-button::part(base) {
          border: 1px solid ButtonText;
        }

        .contact-admin {
          border: 1px solid ButtonText;
        }
      }
    `,
  ];

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('open')) {
      if (this.open) {
        // Store the element that had focus before the dialog opened
        this._previouslyFocusedElement = (document.activeElement as HTMLElement) ?? null;
      } else if (changed.get('open') === true) {
        // Dialog just closed -- restore focus to the trigger element
        this._restoreFocus();
      }
    }
  }

  private _restoreFocus(): void {
    if (this._previouslyFocusedElement) {
      // Defer focus restoration to after the dialog close animation
      requestAnimationFrame(() => {
        this._previouslyFocusedElement?.focus();
        this._previouslyFocusedElement = null;
      });
    }
  }

  private _handleKeydown(e: KeyboardEvent): void {
    if (e.key === 'Escape' && !this.actionInProgress) {
      e.preventDefault();
      e.stopPropagation();
      this.dispatchEvent(new CustomEvent('security-review-cancel'));
    }
  }

  override render() {
    if (!this.open || !this.detail) return nothing;

    return html`
      <sl-dialog
        label="Security review required"
        open
        @sl-request-close=${(e: Event) => {
          e.preventDefault();
          if (!this.actionInProgress) {
            this.dispatchEvent(new CustomEvent('security-review-cancel'));
          }
        }}
        @keydown=${(e: KeyboardEvent) => this._handleKeydown(e)}
      >
        ${this.detail.lockout
          ? this.renderLockoutConflict(this.detail.lockout)
          : this.renderBoundaryReview()}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${this.handleCancel}
        >
          Cancel
        </sl-button>

        ${this.detail.lockout
          ? nothing
          : this.detail.canCommit
            ? html`
                <sl-button slot="footer" variant="primary" @click=${this.handleReview}>
                  <sl-icon slot="prefix" name="shield-check"></sl-icon>
                  Review in Access boundaries
                </sl-button>
              `
            : nothing}
      </sl-dialog>
    `;
  }

  private renderBoundaryReview() {
    const d = this.detail!;

    return html`
      <div class="review-header">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h3>This membership change may restore access</h3>
      </div>

      <p class="review-description">
        Removing <strong>${d.entityLabel}</strong> from <strong>${d.contextLabel}</strong> reduces
        coverage of:
      </p>

      ${d.boundaries.length > 0
        ? html`
            <ul class="boundary-list">
              ${d.boundaries.map(
                (b) => html`
                  <li class="boundary-item">
                    <sl-icon name="shield-exclamation"></sl-icon>
                    <span class="boundary-item-text">
                      <span class="boundary-name">${b.name}</span> &mdash; ${b.principalCount}
                      ${b.principalLabel} may regain ${b.permissionCount}
                      permission${b.permissionCount !== 1 ? 's' : ''}
                    </span>
                  </li>
                `
              )}
            </ul>
          `
        : nothing}

      <div class="review-note">
        This change requires Access boundary administration. The server will re-check membership,
        grants, boundaries, status, and remaining administrators atomically.
      </div>

      ${!d.canCommit
        ? html`
            <div class="contact-admin">
              <strong>Insufficient permissions</strong>
              You do not have the required access boundary administration capability. Contact a
              security administrator to proceed with this change.
            </div>
          `
        : nothing}
    `;
  }

  private renderLockoutConflict(lockout: LockoutConflict) {
    return html`
      <div class="lockout-section">
        <div class="lockout-header">
          <sl-icon name="shield-x"></sl-icon>
          Lockout conflict detected
        </div>

        <div class="lockout-detail">
          <strong>Affected scope:</strong>
          <span class="lockout-scope">${lockout.affectedScope}</span>
        </div>

        <div class="lockout-detail">
          <strong>Failed invariant:</strong> ${lockout.invariantDescription}
        </div>

        ${lockout.adminRecords.length > 0
          ? html`
              <div class="lockout-detail">
                <strong>Administrator records:</strong>
                <ul class="lockout-admins">
                  ${lockout.adminRecords.map((rec) => html`<li>${rec}</li>`)}
                </ul>
              </div>
            `
          : nothing}
        ${lockout.suggestions.length > 0
          ? html`
              <div class="lockout-detail">
                <strong>Resolution suggestions:</strong>
                <ul class="lockout-suggestions">
                  ${lockout.suggestions.map(
                    (s) => html`
                      <li>
                        <sl-icon name="lightbulb"></sl-icon>
                        ${s}
                      </li>
                    `
                  )}
                </ul>
              </div>
            `
          : nothing}

        <p class="lockout-recovery-note">
          Offline recovery is available via operator documentation only and is not an in-application
          action.
        </p>
      </div>
    `;
  }

  private handleCancel(): void {
    this.dispatchEvent(new CustomEvent('security-review-cancel'));
  }

  private handleReview(): void {
    const d = this.detail;
    if (!d) return;

    if (d.impactPreviewUrl) {
      window.location.href = d.impactPreviewUrl;
    } else {
      // Default: navigate to the first affected boundary's detail page
      const firstBoundary = d.boundaries[0];
      if (firstBoundary) {
        window.location.href = `/admin/access-boundaries/${firstBoundary.boundaryId}`;
      } else {
        window.location.href = '/admin/access-boundaries';
      }
    }
  }
}

/* -------------------------------------------------------------------------- */
/* Helper: parse SECURITY_REVIEW_REQUIRED response                            */
/* -------------------------------------------------------------------------- */

/**
 * Parse a SECURITY_REVIEW_REQUIRED error response into SecurityReviewDetail.
 * Returns null if the error is not a security review error.
 */
export function parseSecurityReviewResponse(
  errorBody: Record<string, unknown>,
  entityLabel: string,
  contextLabel: string
): SecurityReviewDetail | null {
  const error = errorBody?.error as Record<string, unknown> | undefined;
  if (!error) return null;
  if (error.code !== 'SECURITY_REVIEW_REQUIRED') return null;

  const details = (error.details ?? {}) as Record<string, unknown>;
  const impactPreviewUrl = details.impactPreviewUrl as string | undefined;
  const canCommit = (details.canCommit as boolean) ?? false;

  const rawBoundaries = (details.affectedBoundaries ?? []) as Array<Record<string, unknown>>;
  const boundaries: SecurityReviewBoundary[] = rawBoundaries.map((b) => ({
    boundaryId: (b.boundaryId as string) ?? '',
    name: (b.name as string) ?? 'Unknown boundary',
    principalCount: (b.principalCount as number) ?? 0,
    principalLabel: (b.principalLabel as string) ?? 'principals',
    permissionCount: (b.permissionCount as number) ?? 0,
  }));

  const result: SecurityReviewDetail = {
    entityLabel,
    contextLabel,
    boundaries,
    canCommit,
  };
  if (impactPreviewUrl) {
    result.impactPreviewUrl = impactPreviewUrl;
  }
  return result;
}

/**
 * Parse a CONSTRAINT_ADMIN_LOCKOUT error response into LockoutConflict.
 * Returns null if the error is not a lockout error.
 */
export function parseLockoutResponse(errorBody: Record<string, unknown>): LockoutConflict | null {
  const error = errorBody?.error as Record<string, unknown> | undefined;
  if (!error) return null;
  if (error.code !== 'CONSTRAINT_ADMIN_LOCKOUT') return null;

  const details = (error.details ?? {}) as Record<string, unknown>;

  return {
    affectedScope: (details.affectedScope as string) ?? 'Unknown scope',
    invariantDescription:
      (details.invariantDescription as string) ??
      (error.message as string) ??
      'Administrator lockout would occur',
    adminRecords: (details.adminRecords as string[]) ?? [],
    suggestions: (details.suggestions as string[]) ?? [
      'Retain or reactivate a direct user administrator',
      'Adjust the access boundary to retain the full admin set',
      'Cancel the operation',
    ],
  };
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-security-review-dialog': ScionSecurityReviewDialog;
  }
}
