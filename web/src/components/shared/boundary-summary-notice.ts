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
 * Boundary Summary Notice (F6)
 *
 * Reusable notice component that displays a summary of access boundaries
 * affecting a given entity (group, project, user). Shows boundary count
 * and links to the filtered access boundary inventory.
 *
 * Usage:
 *   <scion-boundary-summary-notice
 *     .boundaries=${this.groupBoundaries}
 *     filterUrl="/admin/access-boundaries?subjectKind=group&subjectId=123"
 *     label="Access boundaries targeting this group"
 *   ></scion-boundary-summary-notice>
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import type { AccessBoundarySummary } from '../../shared/access-boundaries.js';

/* -------------------------------------------------------------------------- */
/* Types                                                                      */
/* -------------------------------------------------------------------------- */

export interface BoundarySummaryGroup {
  /** Section label (e.g. "Exact group", "Group closure"). */
  label: string;
  /** Boundaries in this group. */
  items: AccessBoundarySummary[];
  /** Link to filtered inventory for this specific group. */
  filterUrl?: string;
}

/* -------------------------------------------------------------------------- */
/* Component                                                                  */
/* -------------------------------------------------------------------------- */

@customElement('scion-boundary-summary-notice')
export class ScionBoundarySummaryNotice extends LitElement {
  /** Section heading for this notice. */
  @property() label = 'Access boundaries';

  /** Grouped boundary lists to display. */
  @property({ type: Array }) groups: BoundarySummaryGroup[] = [];

  /** Whether the data is loading. */
  @property({ type: Boolean }) loading = false;

  /** Error message, if loading failed. */
  @property() error = '';

  /** Global filter URL for "View all" link. */
  @property() filterUrl = '';

  @state() private collapsed = false;

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .boundary-section {
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        padding: 1.25rem;
        margin-bottom: 1.5rem;
      }

      .section-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.75rem;
        cursor: pointer;
      }

      .section-header:hover .section-title {
        color: var(--scion-primary, #3b82f6);
      }

      .section-title-area {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        flex: 1;
      }

      .section-icon {
        color: var(--sl-color-warning-600, #d97706);
        font-size: 1.125rem;
      }

      .section-title {
        font-size: 1rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0;
        transition: color 0.15s;
      }

      .section-count {
        display: inline-flex;
        align-items: center;
        justify-content: center;
        min-width: 1.375rem;
        height: 1.375rem;
        padding: 0 0.375rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 600;
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .section-count.zero {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      .collapse-icon {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        transition: transform 0.2s ease;
      }

      .collapse-icon.collapsed {
        transform: rotate(-90deg);
      }

      .section-body {
        margin-top: 1rem;
      }

      .group-label {
        font-size: 0.75rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        color: var(--scion-text-muted, #64748b);
        margin-bottom: 0.5rem;
        margin-top: 0.75rem;
      }

      .group-label:first-child {
        margin-top: 0;
      }

      .boundary-list {
        list-style: none;
        padding: 0;
        margin: 0;
      }

      .boundary-item {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.625rem;
        border-radius: var(--scion-radius, 0.5rem);
        transition: background 0.1s;
      }

      .boundary-item:hover {
        background: var(--scion-bg-subtle, #f1f5f9);
      }

      .boundary-item a {
        color: var(--scion-primary, #3b82f6);
        text-decoration: none;
        font-size: 0.875rem;
        font-weight: 500;
      }

      .boundary-item a:hover {
        text-decoration: underline;
      }

      .boundary-item .boundary-status {
        font-size: 0.6875rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        font-weight: 500;
      }

      .boundary-item .boundary-status.active {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .boundary-item .boundary-status.scheduled {
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #b45309);
      }

      .boundary-item .boundary-status.expired {
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .boundary-item .boundary-status.recovery_disabled {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      .boundary-item .boundary-status.invalid_degraded {
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .empty-notice {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        padding: 0.5rem 0;
      }

      .view-all-link {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        font-size: 0.8125rem;
        color: var(--scion-primary, #3b82f6);
        text-decoration: none;
        margin-top: 0.75rem;
      }

      .view-all-link:hover {
        text-decoration: underline;
      }

      .view-all-link sl-icon {
        font-size: 0.75rem;
      }

      .loading-notice {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        padding: 0.5rem 0;
      }

      .loading-notice sl-spinner {
        font-size: 1rem;
      }

      .error-notice {
        font-size: 0.8125rem;
        color: var(--sl-color-danger-600, #dc2626);
        padding: 0.5rem 0;
      }

      /* Zoom / touch targets */
      .section-header {
        min-height: 44px;
      }

      .boundary-item a {
        overflow-wrap: anywhere;
      }

      .boundary-item-text,
      .boundary-name {
        overflow-wrap: anywhere;
      }

      /* Utility: screen-reader-only */

      /* Responsive: mobile full-width */
      @media (max-width: 768px) {
        .boundary-section {
          border-radius: 0;
          margin-left: -0.75rem;
          margin-right: -0.75rem;
          padding: 1rem;
        }
      }

      /* High contrast mode */
      @media (forced-colors: active) {
        .boundary-section {
          border: 1px solid ButtonText;
        }

        .boundary-item .boundary-status {
          border: 1px solid ButtonText;
        }

        .section-header:focus-visible {
          outline: 2px solid Highlight;
          outline-offset: 2px;
        }

        .collapse-icon {
          color: ButtonText;
        }
      }

      /* Reduced motion */
      @media (prefers-reduced-motion: reduce) {
        .section-title,
        .collapse-icon,
        .boundary-item {
          transition: none;
        }
      }
    `,
  ];

  private get totalCount(): number {
    return this.groups.reduce((sum, g) => sum + g.items.length, 0);
  }

  override render() {
    const total = this.totalCount;

    return html`
      <div class="boundary-section">
        <div
          class="section-header"
          role="button"
          tabindex="0"
          aria-expanded=${!this.collapsed}
          aria-controls="boundary-section-body"
          @click=${() => {
            this.collapsed = !this.collapsed;
          }}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              this.collapsed = !this.collapsed;
            }
          }}
        >
          <div class="section-title-area">
            <sl-icon class="section-icon" name="shield-lock"></sl-icon>
            <h2 class="section-title">${this.label}</h2>
            <span class="section-count ${total === 0 ? 'zero' : ''}">${total}</span>
          </div>
          <sl-icon
            class="collapse-icon ${this.collapsed ? 'collapsed' : ''}"
            name="chevron-down"
          ></sl-icon>
        </div>

        ${this.collapsed
          ? nothing
          : html`
              <div class="section-body" id="boundary-section-body">
                ${this.loading
                  ? html`
                      <div class="loading-notice" role="status" aria-live="polite">
                        <sl-spinner></sl-spinner>
                        Loading access boundaries...
                      </div>
                    `
                  : this.error
                    ? html`<div class="error-notice" role="alert" aria-live="assertive">
                        ${this.error}
                      </div>`
                    : this.renderGroups()}
                ${this.filterUrl
                  ? html`
                      <a class="view-all-link" href=${this.filterUrl}>
                        View in access boundary inventory
                        <sl-icon name="box-arrow-up-right"></sl-icon>
                      </a>
                    `
                  : nothing}
              </div>
            `}
      </div>
    `;
  }

  private renderGroups() {
    if (this.groups.length === 0 || this.totalCount === 0) {
      return html`<p class="empty-notice">No access boundaries affect this entity.</p>`;
    }

    return html`
      ${this.groups.map(
        (group) => html`
          ${this.groups.length > 1 ? html`<div class="group-label">${group.label}</div>` : nothing}
          ${group.items.length > 0
            ? html`
                <ul class="boundary-list">
                  ${group.items.map(
                    (b) => html`
                      <li class="boundary-item">
                        <a href="/admin/access-boundaries/${b.id}">${b.name}</a>
                        <span class="boundary-status ${b.status}"
                          >${this.formatStatus(b.status)}</span
                        >
                      </li>
                    `
                  )}
                </ul>
              `
            : html`<p class="empty-notice">None</p>`}
        `
      )}
    `;
  }

  private formatStatus(status: string): string {
    switch (status) {
      case 'active':
        return 'Active';
      case 'scheduled':
        return 'Scheduled';
      case 'expired':
        return 'Expired';
      case 'recovery_disabled':
        return 'Recovery disabled';
      case 'invalid_degraded':
        return 'Degraded';
      default:
        return status;
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-boundary-summary-notice': ScionBoundarySummaryNotice;
  }
}
