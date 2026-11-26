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
 * Shared per-address result summary for batch user invites and CSV imports
 * (`POST /api/v1/admin/users/invite/bulk`, ptone/scion#4952).
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import {
  formatAddressReason,
  formatAddressStatusLabel,
  formatAddressStatusVariant,
  formatMembershipDetail,
  formatMembershipStatusLabel,
  formatMembershipStatusVariant,
  isIssueResult,
  type InviteBatchMembership,
  type InviteBatchResponse,
  type InviteBatchResult,
  type ShoelaceTagVariant,
} from './invite-batch-utils.js';

interface CountChip {
  key: string;
  label: string;
  count: number;
  variant: ShoelaceTagVariant;
}

@customElement('scion-invite-batch-results')
export class ScionInviteBatchResults extends LitElement {
  @property({ attribute: false })
  response: InviteBatchResponse | null = null;

  @property({ attribute: false })
  projectLabels: Record<string, string> = {};

  @property({ attribute: false })
  roleLabels: Record<string, string> = {};

  @state()
  private filterMode: 'all' | 'issues' = 'all';

  protected override willUpdate(changed: PropertyValues<this>): void {
    if (changed.has('response')) {
      this.filterMode = 'all';
    }
  }

  static override styles = css`
    :host {
      display: block;
    }

    .results-container {
      display: flex;
      flex-direction: column;
      gap: 0.75rem;
    }

    .incomplete-alert {
      margin: 0;
    }

    .counts-bar {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.375rem;
    }

    .filter-row {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
    }

    .filter-buttons {
      display: inline-flex;
      gap: 0.25rem;
    }

    .results-list {
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      max-height: 300px;
      overflow-y: auto;
      background: var(--scion-surface);
    }

    .result-row {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border-subtle);
    }

    .result-row:last-child {
      border-bottom: none;
    }

    .result-main {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .result-address {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      color: var(--scion-text);
      word-break: break-all;
    }

    .result-reason {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .memberships-list {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      padding-left: 0.75rem;
      border-left: 2px solid var(--scion-border);
    }

    .membership-row {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 0.375rem;
      font-size: 0.75rem;
    }

    .membership-project {
      font-weight: 500;
      color: var(--scion-text-secondary);
    }

    .membership-detail {
      color: var(--scion-text-muted);
    }

    .empty-filter {
      padding: 1rem;
      text-align: center;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
    }
  `;

  private buildCountChips(resp: InviteBatchResponse): CountChip[] {
    const c = resp.counts;
    const chips: CountChip[] = [];
    if (!c) return chips;

    if (c.created > 0 || resp.total > 0) {
      chips.push({ key: 'created', label: 'Created', count: c.created, variant: 'success' });
    }
    if (c.already_invited > 0) {
      chips.push({
        key: 'already_invited',
        label: 'Already invited',
        count: c.already_invited,
        variant: 'primary',
      });
    }
    if (c.already_active > 0) {
      chips.push({
        key: 'already_active',
        label: 'Already active',
        count: c.already_active,
        variant: 'neutral',
      });
    }
    if (c.suspended > 0) {
      chips.push({ key: 'suspended', label: 'Suspended', count: c.suspended, variant: 'warning' });
    }
    if ((c.exists ?? 0) > 0) {
      chips.push({
        key: 'exists',
        label: 'Already exists',
        count: c.exists ?? 0,
        variant: 'neutral',
      });
    }
    if (c.invalid > 0) {
      chips.push({ key: 'invalid', label: 'Invalid', count: c.invalid, variant: 'danger' });
    }
    if (c.duplicate > 0) {
      chips.push({ key: 'duplicate', label: 'Duplicate', count: c.duplicate, variant: 'neutral' });
    }
    if (c.failed > 0) {
      chips.push({ key: 'failed', label: 'Failed', count: c.failed, variant: 'danger' });
    }
    if (c.membership_failed > 0) {
      chips.push({
        key: 'membership_failed',
        label: 'Membership failed',
        count: c.membership_failed,
        variant: 'danger',
      });
    }
    return chips;
  }

  private renderMembership(m: InviteBatchMembership) {
    const projectName = this.projectLabels[m.projectId] || m.projectId;
    const detail = formatMembershipDetail(m, this.roleLabels);
    return html`
      <div class="membership-row" data-project-id=${m.projectId} data-status=${m.status}>
        <span class="membership-project">${projectName}</span>
        <sl-tag size="small" variant=${formatMembershipStatusVariant(m.status)}>
          ${formatMembershipStatusLabel(m.status)}
        </sl-tag>
        ${detail ? html`<span class="membership-detail">${detail}</span>` : nothing}
      </div>
    `;
  }

  private renderResultRow(r: InviteBatchResult) {
    const displayAddr = r.email || r.input || '(empty)';
    const reasonText = formatAddressReason(r.reason, r.duplicateOf);
    return html`
      <div class="result-row" data-status=${r.status}>
        <div class="result-main">
          <span class="result-address">${displayAddr}</span>
          <sl-tag size="small" variant=${formatAddressStatusVariant(r.status)}>
            ${formatAddressStatusLabel(r.status)}
          </sl-tag>
        </div>
        ${reasonText ? html`<div class="result-reason">${reasonText}</div>` : nothing}
        ${r.memberships && r.memberships.length > 0
          ? html`
              <div class="memberships-list">
                ${r.memberships.map((m) => this.renderMembership(m))}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  override render() {
    const resp = this.response;
    if (!resp) return nothing;

    const chips = this.buildCountChips(resp);
    const results = resp.results ?? [];
    const issueCount = results.filter(isIssueResult).length;
    const effectiveFilterMode = issueCount > 0 ? this.filterMode : 'all';
    const visibleResults =
      effectiveFilterMode === 'issues' ? results.filter(isIssueResult) : results;

    return html`
      <div class="results-container">
        ${!resp.complete
          ? html`
              <sl-alert class="incomplete-alert" variant="warning" open>
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                <strong>Batch did not finish completely.</strong>
                Some addresses or project memberships were not processed. Retrying is safe — already
                created invites and applied memberships will not be duplicated.
              </sl-alert>
            `
          : nothing}

        <div class="counts-bar" aria-label="Batch invite summary counts">
          ${chips.map(
            (chip) => html`
              <sl-tag size="small" variant=${chip.variant} data-count-key=${chip.key}>
                ${chip.label}: ${chip.count}
              </sl-tag>
            `
          )}
        </div>

        ${results.length > 0
          ? html`
              <div class="filter-row">
                <span>
                  Showing ${visibleResults.length} of ${results.length}
                  ${results.length === 1 ? 'address' : 'addresses'}
                </span>
                ${issueCount > 0 && issueCount < results.length
                  ? html`
                      <div class="filter-buttons">
                        <sl-button
                          size="small"
                          variant=${this.filterMode === 'all' ? 'primary' : 'default'}
                          @click=${() => {
                            this.filterMode = 'all';
                          }}
                        >
                          All (${results.length})
                        </sl-button>
                        <sl-button
                          size="small"
                          variant=${this.filterMode === 'issues' ? 'primary' : 'default'}
                          @click=${() => {
                            this.filterMode = 'issues';
                          }}
                        >
                          Issues (${issueCount})
                        </sl-button>
                      </div>
                    `
                  : nothing}
              </div>

              <div class="results-list" role="list" aria-label="Per-address invite results">
                ${visibleResults.length > 0
                  ? visibleResults.map((r) => this.renderResultRow(r))
                  : html`<div class="empty-filter">No matching addresses.</div>`}
              </div>
            `
          : nothing}
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-invite-batch-results': ScionInviteBatchResults;
  }
}
