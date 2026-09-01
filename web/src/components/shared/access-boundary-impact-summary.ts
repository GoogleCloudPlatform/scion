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
 * Access Boundary Impact Summary
 *
 * Displays the impact analysis for a boundary mutation:
 * - Current vs proposed effective permission counts
 * - Affected principal count
 * - Losing/regaining principal counts (always separate, never net-only)
 * - Permission diff table showing per-permission loses/regains
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property } from 'lit/decorators.js';

import type {
  BoundaryImpact,
  FutureMostRestrictiveImpact,
  PermissionImpact,
  LockoutAssessment,
  PreviewCompleteness,
} from '../../shared/access-boundaries.js';

@customElement('scion-access-boundary-impact-summary')
export class ScionAccessBoundaryImpactSummary extends LitElement {
  @property({ type: Object }) impact: BoundaryImpact | null = null;
  @property({ type: Object }) lockout: LockoutAssessment | null = null;
  @property({ type: Object }) completeness: PreviewCompleteness | null = null;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .impact-grid {
        display: grid;
        grid-template-columns: repeat(auto-fill, minmax(160px, 1fr));
        gap: 0.75rem;
        margin-bottom: 1.25rem;
      }

      .stat-card {
        padding: 0.75rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        background: var(--scion-surface, #ffffff);
      }

      .stat-label {
        font-size: 0.6875rem;
        font-weight: 600;
        color: var(--scion-text-muted, #64748b);
        text-transform: uppercase;
        letter-spacing: 0.025em;
        margin-bottom: 0.25rem;
      }

      .stat-value {
        font-size: 1.25rem;
        font-weight: 700;
        color: var(--scion-text, #1e293b);
      }

      .stat-value.loses {
        color: var(--sl-color-danger-600, #dc2626);
      }

      .stat-value.regains {
        color: var(--sl-color-success-600, #16a34a);
      }

      .stat-value.neutral {
        color: var(--scion-text-muted, #64748b);
      }

      .stat-detail {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.125rem;
      }

      /* Lockout check */
      .lockout-check {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.625rem 0.75rem;
        border-radius: var(--scion-radius, 0.5rem);
        margin-bottom: 0.75rem;
        font-size: 0.8125rem;
      }

      .lockout-safe {
        background: var(--sl-color-success-50, #f0fdf4);
        color: var(--sl-color-success-700, #15803d);
        border: 1px solid var(--sl-color-success-200, #bbf7d0);
      }

      .lockout-unsafe {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
      }

      .lockout-unknown {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-700, #b45309);
        border: 1px solid var(--sl-color-warning-200, #fde68a);
      }

      .lockout-check sl-icon {
        font-size: 1rem;
        flex-shrink: 0;
      }

      /* Completeness check */
      .completeness-check {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.625rem 0.75rem;
        border-radius: var(--scion-radius, 0.5rem);
        margin-bottom: 0.75rem;
        font-size: 0.8125rem;
      }

      .completeness-ok {
        background: var(--sl-color-success-50, #f0fdf4);
        color: var(--sl-color-success-700, #15803d);
        border: 1px solid var(--sl-color-success-200, #bbf7d0);
      }

      .completeness-degraded {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
      }

      .completeness-check sl-icon {
        font-size: 1rem;
        flex-shrink: 0;
      }

      /* Future most restrictive */
      .future-impact {
        padding: 0.625rem 0.75rem;
        border-radius: var(--scion-radius, 0.5rem);
        background: var(--sl-color-primary-50, #eff6ff);
        border: 1px solid var(--sl-color-primary-200, #bfdbfe);
        margin-bottom: 0.75rem;
        font-size: 0.8125rem;
        color: var(--sl-color-primary-700, #1d4ed8);
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .future-impact sl-icon {
        font-size: 1rem;
        flex-shrink: 0;
      }

      /* Permission diff table */
      .section-heading {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 1rem 0 0.5rem;
      }

      .diff-table {
        width: 100%;
        border-collapse: collapse;
        font-size: 0.8125rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        overflow: hidden;
      }

      .diff-table th {
        text-align: left;
        padding: 0.5rem 0.75rem;
        font-size: 0.6875rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--scion-text-muted, #64748b);
        background: var(--scion-bg-subtle, #f1f5f9);
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }

      .diff-table td {
        padding: 0.375rem 0.75rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        color: var(--scion-text, #1e293b);
      }

      .diff-table tr:last-child td {
        border-bottom: none;
      }

      .diff-table .perm-id {
        font-family: var(--sl-font-mono, monospace);
        font-size: 0.75rem;
      }

      .diff-loses {
        color: var(--sl-color-danger-600, #dc2626);
        font-weight: 500;
      }

      .diff-regains {
        color: var(--sl-color-success-600, #16a34a);
        font-weight: 500;
      }

      .truncated-note {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
        padding: 0.375rem 0.75rem;
        text-align: center;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .diff-table .perm-id {
        overflow-wrap: anywhere;
      }

      @media (max-width: 768px) {
        .impact-grid {
          grid-template-columns: 1fr 1fr;
          gap: 0.5rem;
        }

        .stat-card {
          padding: 0.5rem;
        }

        .stat-value {
          font-size: 1rem;
        }

        .diff-table {
          display: block;
          overflow-x: auto;
          -webkit-overflow-scrolling: touch;
        }
      }

      @media (forced-colors: active) {
        .stat-card {
          border: 1px solid ButtonText;
        }

        .lockout-check,
        .completeness-check,
        .future-impact {
          border: 1px solid ButtonText;
        }

        .diff-table,
        .diff-table th,
        .diff-table td {
          border-color: ButtonText;
        }
      }
    `,
  ];

  private formatDate(iso: string): string {
    try {
      const date = new Date(iso);
      if (isNaN(date.getTime())) return iso;
      return date.toLocaleDateString(undefined, {
        month: 'short',
        day: 'numeric',
        year: 'numeric',
      });
    } catch {
      return iso;
    }
  }

  private renderLockoutCheck() {
    if (!this.lockout) return nothing;

    if (this.lockout.safe === null) {
      return html`
        <div class="lockout-check lockout-unknown">
          <sl-icon name="question-circle"></sl-icon>
          <span>
            Lockout check: undetermined
            ${this.lockout.undeterminedReason ? ` — ${this.lockout.undeterminedReason}` : ''}
          </span>
        </div>
      `;
    }

    if (this.lockout.safe) {
      return html`
        <div class="lockout-check lockout-safe">
          <sl-icon name="check-circle"></sl-icon>
          <span>
            Lockout check: safe
            ${this.lockout.remainingActiveDirectAdmins !== null
              ? ` — ${this.lockout.remainingActiveDirectAdmins} active direct access boundary administrator${this.lockout.remainingActiveDirectAdmins === 1 ? '' : 's'} remain`
              : ''}
          </span>
        </div>
      `;
    }

    return html`
      <div class="lockout-check lockout-unsafe">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <span>
          Lockout check: unsafe — this change may lock out all access boundary administrators
        </span>
      </div>
    `;
  }

  private renderCompletenessCheck() {
    if (!this.completeness) return nothing;

    if (this.completeness.complete && !this.completeness.truncated && !this.completeness.degraded) {
      return html`
        <div class="completeness-check completeness-ok">
          <sl-icon name="check-circle"></sl-icon>
          <span>Complete — all closure paths and grants resolved</span>
        </div>
      `;
    }

    const reasons = this.completeness.reasons.map((r) => r.message).join('; ');
    return html`
      <div class="completeness-check completeness-degraded">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <span>
          ${this.completeness.truncated ? 'Truncated' : ''}${this.completeness.degraded
            ? this.completeness.truncated
              ? ' and degraded'
              : 'Degraded'
            : ''}
          — commit not available${reasons ? `. ${reasons}` : ''}
        </span>
      </div>
    `;
  }

  private renderFutureImpact(fi: FutureMostRestrictiveImpact) {
    return html`
      <div class="future-impact">
        <sl-icon name="calendar-event"></sl-icon>
        <span>
          Future most-restrictive: ${fi.affectedPrincipalCount}
          principal${fi.affectedPrincipalCount === 1 ? '' : 's'}
          lose${fi.affectedPrincipalCount === 1 ? 's' : ''} ${fi.removedPermissionCount} effective
          permission${fi.removedPermissionCount === 1 ? '' : 's'} on ${this.formatDate(fi.at)}
          ${fi.note ? ` — ${fi.note}` : ''}
        </span>
      </div>
    `;
  }

  private renderPermissionDiffs(diffs: PermissionImpact[], truncated: boolean) {
    if (diffs.length === 0) return nothing;

    return html`
      <div class="section-heading">Permissions changed</div>
      <table class="diff-table" role="table" aria-label="Permission changes">
        <caption class="sr-only">
          Permission changes showing loses and regains per permission
        </caption>
        <thead>
          <tr>
            <th scope="col">Permission</th>
            <th scope="col">Loses</th>
            <th scope="col">Regains</th>
          </tr>
        </thead>
        <tbody>
          ${diffs.map(
            (d) => html`
              <tr>
                <td class="perm-id">${d.permissionId}</td>
                <td>
                  ${d.losingCount > 0
                    ? html`<span class="diff-loses">− ${d.losingCount}</span>`
                    : '—'}
                </td>
                <td>
                  ${d.regainingCount > 0
                    ? html`<span class="diff-regains">+ ${d.regainingCount}</span>`
                    : '—'}
                </td>
              </tr>
            `
          )}
        </tbody>
      </table>
      ${truncated
        ? html`<div class="truncated-note">Permission diff list truncated</div>`
        : nothing}
    `;
  }

  override render() {
    if (!this.impact) return nothing;

    const i = this.impact;

    return html`
      ${this.renderCompletenessCheck()} ${this.renderLockoutCheck()}

      <div class="impact-grid" aria-label="Impact statistics">
        <div class="stat-card">
          <div class="stat-label">Current effective</div>
          <div class="stat-value">${i.current.effectivePermissionCount}</div>
          <div class="stat-detail">
            permission${i.current.effectivePermissionCount === 1 ? '' : 's'}
          </div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Proposed effective</div>
          <div class="stat-value">${i.proposed.effectivePermissionCount}</div>
          <div class="stat-detail">
            permission${i.proposed.effectivePermissionCount === 1 ? '' : 's'}
          </div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Affected principals</div>
          <div class="stat-value">
            ${i.affectedPrincipalCount}${i.affectedPrincipalCountExact === false ? '+' : ''}
          </div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Losing authority</div>
          <div class="stat-value loses">${i.losingPrincipalCount}</div>
          <div class="stat-detail">principal${i.losingPrincipalCount === 1 ? '' : 's'}</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">Regaining authority</div>
          <div class="stat-value regains">${i.regainingPrincipalCount}</div>
          <div class="stat-detail">principal${i.regainingPrincipalCount === 1 ? '' : 's'}</div>
        </div>
        ${i.noEffectPrincipalCount > 0
          ? html`
              <div class="stat-card">
                <div class="stat-label">No effect</div>
                <div class="stat-value neutral">${i.noEffectPrincipalCount}</div>
                <div class="stat-detail">principal${i.noEffectPrincipalCount === 1 ? '' : 's'}</div>
              </div>
            `
          : nothing}
      </div>

      ${i.futureMostRestrictive ? this.renderFutureImpact(i.futureMostRestrictive) : nothing}
      ${this.renderPermissionDiffs(i.permissionDiffs, i.permissionDiffsTruncated)}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-impact-summary': ScionAccessBoundaryImpactSummary;
  }
}
