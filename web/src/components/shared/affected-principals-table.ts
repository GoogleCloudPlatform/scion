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
 * Affected Principals Table
 *
 * Pageable list of affected principals for a boundary mutation.
 * Displays separate sections for losing and regaining principals —
 * never shows only net effect for mixed operations.
 *
 * Shows: principal type/name, membership path, grants affected.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import type { AffectedPrincipal, PageToken, PrincipalRef } from '../../shared/access-boundaries.js';

/** Event detail for requesting a new page of results. */
export interface PageRequestDetail {
  pageToken: PageToken;
}

@customElement('scion-affected-principals-table')
export class ScionAffectedPrincipalsTable extends LitElement {
  /** The affected principals to display. */
  @property({ type: Array }) principals: AffectedPrincipal[] = [];

  /** Token for next page, absent means last page. */
  @property() nextPageToken: PageToken | undefined;

  /** Total count of affected principals. */
  @property({ type: Number }) totalCount = 0;

  /** Whether total count is exact. */
  @property({ type: Boolean }) totalCountExact = true;

  /** Whether a page is currently loading. */
  @property({ type: Boolean }) loading = false;

  /** Whether this is showing preview results (inline) or detail page results. */
  @property() mode: 'preview' | 'detail' = 'preview';

  /** Internal filter for change kind. */
  @state() private filterKind: '' | 'loses' | 'regains' | 'no_effect' = '';

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .section-heading {
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0 0 0.5rem;
      }

      .filter-bar {
        display: flex;
        gap: 0.5rem;
        margin-bottom: 0.75rem;
        flex-wrap: wrap;
        align-items: center;
      }

      .filter-bar sl-select {
        min-width: 140px;
        max-width: 200px;
      }

      .total-count {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        margin-left: auto;
      }

      /* Section separators for loses/regains */
      .change-section {
        margin-bottom: 1rem;
      }

      .change-section-header {
        font-size: 0.75rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.05em;
        padding: 0.375rem 0.75rem;
        border-radius: var(--scion-radius, 0.5rem) var(--scion-radius, 0.5rem) 0 0;
        margin-bottom: 0;
      }

      .change-section-header.loses {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
        border: 1px solid var(--sl-color-danger-200, #fecaca);
        border-bottom: none;
      }

      .change-section-header.regains {
        background: var(--sl-color-success-50, #f0fdf4);
        color: var(--sl-color-success-700, #15803d);
        border: 1px solid var(--sl-color-success-200, #bbf7d0);
        border-bottom: none;
      }

      /* Table */
      .principals-table {
        width: 100%;
        border-collapse: collapse;
        font-size: 0.8125rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 0 0 var(--scion-radius, 0.5rem) var(--scion-radius, 0.5rem);
        overflow: hidden;
      }

      .principals-table.standalone {
        border-radius: var(--scion-radius, 0.5rem);
      }

      .principals-table th {
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

      .principals-table td {
        padding: 0.5rem 0.75rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        color: var(--scion-text, #1e293b);
        vertical-align: top;
      }

      .principals-table tr:last-child td {
        border-bottom: none;
      }

      /* Principal cell */
      .principal-info {
        display: flex;
        align-items: center;
        gap: 0.375rem;
      }

      .principal-type-icon {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        flex-shrink: 0;
      }

      .principal-name {
        font-weight: 500;
      }

      .principal-id {
        font-family: var(--sl-font-mono, monospace);
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .redacted-principal {
        font-style: italic;
        color: var(--scion-text-muted, #64748b);
      }

      /* Membership path */
      .membership-paths {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }

      .membership-path {
        display: flex;
        align-items: center;
        gap: 0.25rem;
        flex-wrap: wrap;
      }

      .path-hop {
        white-space: nowrap;
      }

      .path-arrow {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.625rem;
      }

      .path-direct {
        font-size: 0.625rem;
        color: var(--sl-color-primary-600, #2563eb);
        font-weight: 500;
      }

      /* Permissions affected */
      .perm-list {
        display: flex;
        flex-wrap: wrap;
        gap: 0.25rem;
      }

      .perm-tag {
        font-family: var(--sl-font-mono, monospace);
        font-size: 0.6875rem;
        padding: 0.0625rem 0.375rem;
        border-radius: var(--scion-radius, 0.5rem);
        white-space: nowrap;
      }

      .perm-tag.removed {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .perm-tag.regained {
        background: var(--sl-color-success-50, #f0fdf4);
        color: var(--sl-color-success-700, #15803d);
      }

      .perm-truncated {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
      }

      /* Change kind badges */
      .change-badge {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 500;
        white-space: nowrap;
      }

      .change-badge.loses {
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      .change-badge.regains {
        background: var(--sl-color-success-50, #f0fdf4);
        color: var(--sl-color-success-700, #15803d);
      }

      .change-badge.mixed {
        background: var(--sl-color-warning-50, #fffbeb);
        color: var(--sl-color-warning-700, #b45309);
      }

      .change-badge.no_effect {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      /* No effect reason */
      .no-effect-reason {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
      }

      /* Pagination */
      .pagination {
        display: flex;
        align-items: center;
        justify-content: center;
        gap: 1rem;
        padding: 0.75rem 0;
      }

      .empty-state {
        text-align: center;
        padding: 1.5rem;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
      }

      .loading-overlay {
        display: flex;
        align-items: center;
        justify-content: center;
        padding: 2rem;
      }

      /* Grant sources */
      .grant-sources {
        font-size: 0.6875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .grant-source {
        display: flex;
        align-items: center;
        gap: 0.25rem;
      }

      .principal-name,
      .principal-id {
        overflow-wrap: anywhere;
      }

      .perm-tag {
        overflow-wrap: anywhere;
      }

      .table-scroll-wrapper {
        overflow-x: auto;
        -webkit-overflow-scrolling: touch;
      }

      @media (max-width: 768px) {
        .principals-table {
          display: none;
        }

        .mobile-card-list {
          display: flex;
          flex-direction: column;
          gap: 0.5rem;
        }

        .mobile-card {
          border: 1px solid var(--scion-border, #e2e8f0);
          border-radius: var(--scion-radius, 0.5rem);
          padding: 0.75rem;
          background: var(--scion-surface, #ffffff);
        }

        .mobile-card-header {
          display: flex;
          align-items: center;
          justify-content: space-between;
          gap: 0.5rem;
          margin-bottom: 0.5rem;
        }

        .mobile-card-field {
          font-size: 0.75rem;
          color: var(--scion-text-muted, #64748b);
          margin-top: 0.375rem;
        }

        .mobile-card-field-label {
          font-weight: 600;
          text-transform: uppercase;
          letter-spacing: 0.025em;
          font-size: 0.625rem;
          margin-bottom: 0.125rem;
        }

        .filter-bar sl-select {
          min-width: 100%;
          max-width: 100%;
        }
      }

      @media (min-width: 769px) {
        .mobile-card-list {
          display: none;
        }
      }

      @media (forced-colors: active) {
        .principals-table,
        .principals-table th,
        .principals-table td {
          border-color: ButtonText;
        }

        .change-section-header {
          border-color: ButtonText;
        }

        .change-badge,
        .perm-tag {
          border: 1px solid ButtonText;
        }

        .mobile-card {
          border-color: ButtonText;
        }
      }
    `,
  ];

  private principalIcon(type: string): string {
    switch (type) {
      case 'user':
        return 'person';
      case 'agent':
        return 'cpu';
      case 'group':
        return 'diagram-3';
      default:
        return 'person';
    }
  }

  private changeKindLabel(kind: string): string {
    switch (kind) {
      case 'loses':
        return 'Loses';
      case 'regains':
        return 'Regains';
      case 'mixed':
        return 'Mixed';
      case 'no_effect':
        return 'No effect';
      default:
        return kind;
    }
  }

  private noEffectReasonLabel(reason: string | undefined): string {
    switch (reason) {
      case 'principal_suspended':
        return 'Principal is suspended';
      case 'no_overlapping_authority':
        return 'No overlapping authority';
      case 'already_constrained_by_other_boundary':
        return 'Already constrained by another boundary';
      default:
        return reason ?? '';
    }
  }

  private principalDisplayName(ref: PrincipalRef | null, redacted: boolean): string {
    if (redacted || !ref) return '(redacted)';
    return ref.displayName ?? ref.id;
  }

  private get filteredPrincipals(): AffectedPrincipal[] {
    if (!this.filterKind) return this.principals;
    return this.principals.filter((p) => p.changeKind === this.filterKind);
  }

  private get losingPrincipals(): AffectedPrincipal[] {
    return this.principals.filter((p) => p.changeKind === 'loses');
  }

  private get regainingPrincipals(): AffectedPrincipal[] {
    return this.principals.filter((p) => p.changeKind === 'regains');
  }

  private get mixedPrincipals(): AffectedPrincipal[] {
    return this.principals.filter((p) => p.changeKind === 'mixed');
  }

  private requestPage(token: PageToken): void {
    this.dispatchEvent(
      new CustomEvent<PageRequestDetail>('page-request', {
        detail: { pageToken: token },
        bubbles: true,
        composed: true,
      })
    );
  }

  private renderPrincipalRow(p: AffectedPrincipal) {
    const isRedacted = !!p.redacted || p.principal === null;

    return html`
      <tr>
        <td>
          ${isRedacted
            ? html`<span class="redacted-principal">(redacted)</span>`
            : html`
                <div class="principal-info">
                  <sl-icon
                    class="principal-type-icon"
                    name="${this.principalIcon(p.principal?.type ?? 'user')}"
                  ></sl-icon>
                  <div>
                    <div class="principal-name">
                      ${this.principalDisplayName(p.principal, isRedacted)}
                    </div>
                    ${p.principal?.id
                      ? html`<div class="principal-id">${p.principal.id}</div>`
                      : nothing}
                  </div>
                </div>
              `}
        </td>
        <td>
          <span class="change-badge ${p.changeKind}"> ${this.changeKindLabel(p.changeKind)} </span>
          ${p.changeKind === 'no_effect' && p.noEffectReason
            ? html`<div class="no-effect-reason">
                ${this.noEffectReasonLabel(p.noEffectReason)}
              </div>`
            : nothing}
        </td>
        <td>
          ${p.removedPermissions.length > 0
            ? html`
                <div class="perm-list">
                  ${p.removedPermissions.map(
                    (perm) => html`<span class="perm-tag removed">− ${perm}</span>`
                  )}
                  ${p.removedPermissionsTruncated
                    ? html`<span class="perm-truncated">
                        +${p.removedPermissionCount - p.removedPermissions.length} more
                      </span>`
                    : nothing}
                </div>
              `
            : nothing}
          ${p.regainedPermissions.length > 0
            ? html`
                <div
                  class="perm-list"
                  style="margin-top: ${p.removedPermissions.length > 0 ? '0.25rem' : '0'}"
                >
                  ${p.regainedPermissions.map(
                    (perm) => html`<span class="perm-tag regained">+ ${perm}</span>`
                  )}
                </div>
              `
            : nothing}
          ${p.removedPermissions.length === 0 && p.regainedPermissions.length === 0
            ? html`<span style="color: var(--scion-text-muted, #64748b)">—</span>`
            : nothing}
        </td>
        <td>
          ${p.membershipPaths.length > 0
            ? html`
                <div class="membership-paths">
                  ${p.membershipPaths.map(
                    (path) => html`
                      <div class="membership-path">
                        ${path.direct
                          ? html`<span class="path-direct">direct</span>`
                          : path.hops.map(
                              (hop, i) => html`
                                ${i > 0 ? html`<span class="path-arrow">→</span>` : nothing}
                                <span class="path-hop">
                                  ${hop.displayName ?? hop.id ?? '(redacted)'}
                                </span>
                              `
                            )}
                        ${path.truncated ? html`<span class="path-arrow">→ …</span>` : nothing}
                      </div>
                    `
                  )}
                </div>
              `
            : html`<span style="color: var(--scion-text-muted, #64748b)">—</span>`}
        </td>
      </tr>
    `;
  }

  private renderPrincipalCard(p: AffectedPrincipal) {
    const isRedacted = !!p.redacted || p.principal === null;

    return html`
      <div class="mobile-card">
        <div class="mobile-card-header">
          ${isRedacted
            ? html`<span class="redacted-principal">(redacted)</span>`
            : html`
                <div class="principal-info">
                  <sl-icon
                    class="principal-type-icon"
                    name="${this.principalIcon(p.principal?.type ?? 'user')}"
                  ></sl-icon>
                  <div>
                    <div
                      class="principal-name"
                      title="${this.principalDisplayName(p.principal, isRedacted)}"
                    >
                      ${this.principalDisplayName(p.principal, isRedacted)}
                    </div>
                    ${p.principal?.id
                      ? html`<div class="principal-id" title="${p.principal.id}">
                          ${p.principal.id}
                        </div>`
                      : nothing}
                  </div>
                </div>
              `}
          <span class="change-badge ${p.changeKind}">${this.changeKindLabel(p.changeKind)}</span>
        </div>
        ${p.removedPermissions.length > 0 || p.regainedPermissions.length > 0
          ? html`
              <div class="mobile-card-field">
                <div class="mobile-card-field-label">Permissions</div>
                <div class="perm-list">
                  ${p.removedPermissions.map(
                    (perm) => html`<span class="perm-tag removed" title="${perm}">− ${perm}</span>`
                  )}
                  ${p.regainedPermissions.map(
                    (perm) => html`<span class="perm-tag regained" title="${perm}">+ ${perm}</span>`
                  )}
                </div>
              </div>
            `
          : nothing}
        ${p.changeKind === 'no_effect' && p.noEffectReason
          ? html`<div class="mobile-card-field">
              <div class="no-effect-reason">${this.noEffectReasonLabel(p.noEffectReason)}</div>
            </div>`
          : nothing}
      </div>
    `;
  }

  private renderTable(principals: AffectedPrincipal[], standalone: boolean) {
    if (principals.length === 0) {
      return html`<div class="empty-state">No principals in this section</div>`;
    }

    return html`
      <div class="table-scroll-wrapper">
        <table
          class="principals-table ${standalone ? 'standalone' : ''}"
          role="table"
          aria-label="Affected principals"
        >
          <caption class="sr-only">
            Affected principals showing effect, permissions, and membership
          </caption>
          <thead>
            <tr>
              <th scope="col">Principal</th>
              <th scope="col">Effect</th>
              <th scope="col">Permissions</th>
              <th scope="col">Membership</th>
            </tr>
          </thead>
          <tbody>
            ${principals.map((p) => this.renderPrincipalRow(p))}
          </tbody>
        </table>
      </div>
      <div class="mobile-card-list">${principals.map((p) => this.renderPrincipalCard(p))}</div>
    `;
  }

  private renderSeparateSections() {
    const losing = this.losingPrincipals;
    const regaining = this.regainingPrincipals;
    const mixed = this.mixedPrincipals;
    const noEffect = this.principals.filter((p) => p.changeKind === 'no_effect');

    return html`
      ${losing.length > 0
        ? html`
            <div class="change-section">
              <div class="change-section-header loses">
                Loses effective permissions (${losing.length})
              </div>
              ${this.renderTable(losing, false)}
            </div>
          `
        : nothing}
      ${mixed.length > 0
        ? html`
            <div class="change-section">
              <div class="change-section-header loses">Mixed effect (${mixed.length})</div>
              ${this.renderTable(mixed, false)}
            </div>
          `
        : nothing}
      ${regaining.length > 0
        ? html`
            <div class="change-section">
              <div class="change-section-header regains">
                Regains effective permissions (${regaining.length})
              </div>
              ${this.renderTable(regaining, false)}
            </div>
          `
        : nothing}
      ${noEffect.length > 0
        ? html`
            <div class="change-section">
              <div class="section-heading">No effect (${noEffect.length})</div>
              ${this.renderTable(noEffect, true)}
            </div>
          `
        : nothing}
    `;
  }

  override render() {
    if (this.loading && this.principals.length === 0) {
      return html`
        <div class="loading-overlay" role="status" aria-live="polite">
          <sl-spinner style="font-size: 1.5rem"></sl-spinner>
          <span class="sr-only">Loading affected principals</span>
        </div>
      `;
    }

    if (this.principals.length === 0) {
      return html`<div class="empty-state">No affected principals</div>`;
    }

    const hasMixed = this.losingPrincipals.length > 0 && this.regainingPrincipals.length > 0;

    return html`
      <div class="section-heading">
        Affected principals
        <span class="total-count">
          (${this.totalCountExact ? this.totalCount : `${this.totalCount}+`} total)
        </span>
      </div>

      ${this.mode === 'detail'
        ? html`
            <div class="filter-bar">
              <sl-select
                placeholder="All effects"
                size="small"
                clearable
                .value=${this.filterKind}
                @sl-change=${(e: Event) => {
                  this.filterKind = (e.target as HTMLSelectElement).value as typeof this.filterKind;
                }}
              >
                <sl-option value="loses">Loses</sl-option>
                <sl-option value="regains">Regains</sl-option>
                <sl-option value="no_effect">No effect</sl-option>
              </sl-select>
            </div>
            ${this.filterKind
              ? this.renderTable(this.filteredPrincipals, true)
              : this.renderSeparateSections()}
          `
        : hasMixed || this.mode === 'preview'
          ? this.renderSeparateSections()
          : this.renderTable(this.principals, true)}
      ${this.nextPageToken
        ? html`
            <div class="pagination">
              <sl-button
                variant="default"
                size="small"
                ?loading=${this.loading}
                @click=${() => this.requestPage(this.nextPageToken!)}
              >
                Load more
              </sl-button>
            </div>
          `
        : nothing}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-affected-principals-table': ScionAffectedPrincipalsTable;
  }
}
