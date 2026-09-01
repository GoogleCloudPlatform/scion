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
 * Access Boundary Status Badge
 *
 * Reusable status/classification badge component for access boundaries.
 * Displays status, classification, and risk labels with appropriate
 * icons and colors.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property } from 'lit/decorators.js';

import type {
  AccessBoundaryStatus,
  AccessBoundaryRisk,
  MutationClassificationDisplay,
} from '../../shared/access-boundaries.js';

@customElement('scion-access-boundary-status')
export class ScionAccessBoundaryStatus extends LitElement {
  /** The boundary status to display. */
  @property() status: AccessBoundaryStatus | '' = '';

  /** Optional classification label. */
  @property() classification: MutationClassificationDisplay | '' = '';

  /** Optional risk labels. */
  @property({ type: Array }) risk: AccessBoundaryRisk[] = [];

  /** Size variant. */
  @property() size: 'small' | 'default' = 'default';

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: inline-flex;
        align-items: center;
        gap: 0.375rem;
        flex-wrap: wrap;
      }

      .badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.125rem 0.5rem;
        border-radius: 9999px;
        font-weight: 500;
        white-space: nowrap;
      }

      .badge sl-icon {
        font-size: 0.75rem;
      }

      /* Size variants */
      :host([size='small']) .badge {
        font-size: 0.6875rem;
        padding: 0.0625rem 0.375rem;
      }

      :host([size='small']) .badge sl-icon {
        font-size: 0.625rem;
      }

      .badge:not([class*='small']) {
        font-size: 0.75rem;
      }

      /* Status colors */
      .status-active {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .status-scheduled {
        background: var(--sl-color-primary-100, #dbeafe);
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .status-expired {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      .status-recovery_disabled {
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .status-invalid_degraded {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      /* Classification colors */
      .classification-tighten {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .classification-relax {
        background: var(--sl-color-primary-100, #dbeafe);
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .classification-mixed {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-700, #b45309);
        border: 1px solid var(--sl-color-warning-200, #fde68a);
      }

      .classification-no_effect {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      /* Risk colors */
      .risk-tightening {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-700, #b45309);
      }

      .risk-relaxation_scheduled {
        background: var(--sl-color-primary-50, #eff6ff);
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .risk-mixed {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .risk-lockout_sensitive {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .risk-degraded {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-600, #d97706);
      }

      @media (forced-colors: active) {
        .badge {
          border: 1px solid ButtonText;
        }
      }
    `,
  ];

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

  private classificationLabel(c: MutationClassificationDisplay): string {
    switch (c) {
      case 'tighten':
        return 'Tightening';
      case 'relax':
        return 'Relaxation';
      case 'mixed':
        return 'Mixed';
      case 'no_effect':
        return 'No effect';
      default:
        return c;
    }
  }

  private classificationIcon(c: MutationClassificationDisplay): string {
    switch (c) {
      case 'tighten':
        return 'shield-lock';
      case 'relax':
        return 'shield-check';
      case 'mixed':
        return 'shield-exclamation';
      case 'no_effect':
        return 'shield';
      default:
        return 'shield';
    }
  }

  private riskLabel(r: AccessBoundaryRisk): string {
    switch (r) {
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
        return r;
    }
  }

  override connectedCallback(): void {
    super.connectedCallback();
    if (!this.hasAttribute('role')) {
      this.setAttribute('role', 'status');
    }
  }

  override render() {
    return html`
      ${this.status
        ? html`
            <span class="badge status-${this.status}">
              <sl-icon name="${this.statusIcon(this.status)}"></sl-icon>
              ${this.statusLabel(this.status)}
            </span>
          `
        : nothing}
      ${this.classification
        ? html`
            <span class="badge classification-${this.classification}">
              <sl-icon name="${this.classificationIcon(this.classification)}"></sl-icon>
              ${this.classificationLabel(this.classification)}
            </span>
          `
        : nothing}
      ${this.risk.map((r) => html` <span class="badge risk-${r}">${this.riskLabel(r)}</span> `)}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-access-boundary-status': ScionAccessBoundaryStatus;
  }
}
