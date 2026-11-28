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
 * Read-only Message Policy list, coverage banner, and policy detail drawer
 * (U1 slice — current veto-set model, no authoring or chain UI).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelativeTime } from '../../utils/time.js';
import {
  formatSurfaceLabel,
  hasSystemOneRule,
  type CoverageInfo,
  type ListMessagePoliciesResponse,
  type MessagePolicy,
  type ProjectAgentSummary,
} from './types.js';

const PAGE_LIMIT = 50;

@customElement('scion-message-policy-list')
export class ScionMessagePolicyList extends LitElement {
  private _zone = new DisplayZoneController(this);

  @property({ type: String })
  projectId = '';

  @property({ type: Boolean })
  systemOneEnabled = false;

  @property({ type: Boolean })
  canManage = false;

  @property({ type: Array })
  agents: ProjectAgentSummary[] = [];

  @state() private policies: MessagePolicy[] = [];
  @state() private coverage: CoverageInfo | null = null;
  @state() private totalCount = 0;
  @state() private nextCursor = '';
  @state() private cursorStack: string[] = [''];
  @state() private pageIndex = 0;

  @state() private modeFilter = '';
  @state() private enabledFilter = '';

  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private forbidden = false;

  @state() private detailOpen = false;
  @state() private detailPolicy: MessagePolicy | null = null;
  @state() private detailLoading = false;
  @state() private detailError: string | null = null;

  private loadSeq = 0;
  private detailSeq = 0;

  static override styles = css`
    :host {
      display: block;
    }

    .coverage-card {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      padding: 1rem 1.25rem;
      margin-bottom: 1.25rem;
    }

    .coverage-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.75rem;
      flex-wrap: wrap;
    }

    .coverage-title {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text);
      margin: 0;
    }

    .coverage-grid {
      display: grid;
      grid-template-columns: 150px 1fr;
      gap: 0.5rem 0.75rem;
      font-size: 0.8125rem;
      align-items: start;
    }

    .coverage-label {
      color: var(--scion-text-muted);
      font-weight: 500;
      padding-top: 0.125rem;
    }

    .chip-row {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
    }

    .chip {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: var(--scion-radius-sm);
      font-size: 0.75rem;
      font-family: var(--scion-font-mono);
      border: 1px solid var(--scion-border);
      background: var(--scion-bg-subtle);
      color: var(--scion-text);
    }

    .chip-covered {
      background: var(--scion-success-bg);
      border-color: var(--scion-success-border);
      color: var(--scion-success-text);
    }

    .chip-uncovered {
      background: var(--scion-surface);
      border-style: dashed;
      color: var(--scion-text-muted);
    }

    .toolbar {
      display: flex;
      align-items: flex-end;
      justify-content: space-between;
      gap: 0.75rem;
      margin-bottom: 1rem;
      flex-wrap: wrap;
    }

    .filters {
      display: flex;
      align-items: flex-end;
      gap: 0.75rem;
      flex-wrap: wrap;
    }

    .filters sl-select {
      width: 160px;
    }

    .readonly-banner {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      padding: 0.375rem 0.625rem;
    }

    .table-wrap {
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      overflow-x: auto;
    }

    table {
      width: 100%;
      border-collapse: collapse;
      font-size: 0.875rem;
    }

    th {
      text-align: left;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      color: var(--scion-text-muted);
      padding: 0.625rem 0.875rem;
      border-bottom: 1px solid var(--scion-border);
      background: var(--scion-bg-subtle);
      white-space: nowrap;
    }

    td {
      padding: 0.75rem 0.875rem;
      border-bottom: 1px solid var(--scion-border-subtle);
      vertical-align: top;
      color: var(--scion-text);
    }

    tbody tr:last-child td {
      border-bottom: none;
    }

    tbody tr:hover td {
      background: var(--scion-bg);
    }

    .policy-name-btn {
      background: transparent;
      border: none;
      padding: 0;
      margin: 0;
      font: inherit;
      font-weight: 600;
      color: var(--scion-primary);
      cursor: pointer;
      text-align: left;
    }

    .policy-name-btn:hover {
      text-decoration: underline;
    }

    .policy-desc {
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
      margin-top: 0.125rem;
    }

    .badge-row {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
    }

    .mono {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
    }

    .muted {
      color: var(--scion-text-muted);
    }

    .pagination {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-top: 0.875rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
      flex-wrap: wrap;
      gap: 0.5rem;
    }

    .pagination-controls {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .loading-state,
    .empty-state,
    .forbidden-state {
      text-align: center;
      padding: 3rem 1.5rem;
      background: var(--scion-surface);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-lg);
      color: var(--scion-text-muted);
    }

    .empty-state sl-icon,
    .forbidden-state sl-icon {
      font-size: 2.5rem;
      margin-bottom: 0.75rem;
      color: var(--scion-text-muted);
    }

    .forbidden-state sl-icon {
      color: var(--scion-status-danger);
    }

    .empty-state h3,
    .forbidden-state h3 {
      margin: 0 0 0.5rem 0;
      font-size: 1.0625rem;
      color: var(--scion-text);
    }

    .empty-state p,
    .forbidden-state p {
      margin: 0 auto;
      max-width: 520px;
      font-size: 0.875rem;
    }

    .alert-wrap {
      margin-bottom: 1rem;
    }

    .drawer-section {
      margin-bottom: 1.25rem;
    }

    .drawer-section h4 {
      margin: 0 0 0.5rem 0;
      font-size: 0.8125rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.03em;
      color: var(--scion-text-muted);
    }

    .kv-grid {
      display: grid;
      grid-template-columns: 140px 1fr;
      gap: 0.375rem 0.75rem;
      font-size: 0.875rem;
    }

    .kv-key {
      color: var(--scion-text-muted);
    }

    .code-block {
      margin: 0;
      padding: 0.75rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
      white-space: pre-wrap;
      word-break: break-word;
      overflow-x: auto;
      color: var(--scion-text);
    }

    @media (max-width: 640px) {
      .coverage-grid,
      .kv-grid {
        grid-template-columns: 1fr;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      void this.loadPolicies();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    const prevProjectId = changed.get('projectId');
    if (changed.has('projectId') && prevProjectId !== undefined && this.projectId) {
      this.cursorStack = [''];
      this.pageIndex = 0;
      void this.loadPolicies();
    }
  }

  public async loadPolicies(): Promise<void> {
    if (!this.projectId) return;

    const seq = ++this.loadSeq;
    this.loading = true;
    this.error = null;
    this.forbidden = false;

    const params = new URLSearchParams();
    params.set('limit', String(PAGE_LIMIT));
    if (this.modeFilter) params.set('mode', this.modeFilter);
    if (this.enabledFilter) params.set('enabled', this.enabledFilter);
    const cursor = this.cursorStack[this.pageIndex];
    if (cursor) params.set('cursor', cursor);

    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/message-policies?${params.toString()}`
      );
      if (!this.isConnected || seq !== this.loadSeq) return;
      if (res.status === 403) {
        this.forbidden = true;
        this.policies = [];
        return;
      }
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }

      const data = (await res.json()) as ListMessagePoliciesResponse;
      if (!this.isConnected || seq !== this.loadSeq) return;

      this.policies = Array.isArray(data.policies) ? data.policies : [];
      this.coverage = data.coverage ?? null;
      this.totalCount =
        typeof data.total_count === 'number' ? data.total_count : this.policies.length;
      this.nextCursor = data.next_cursor ?? '';

      this.dispatchEvent(
        new CustomEvent('policies-loaded', {
          detail: {
            policies: this.policies,
            filtered: Boolean(this.modeFilter || this.enabledFilter || this.pageIndex > 0),
          },
          bubbles: true,
          composed: true,
        })
      );
    } catch (err) {
      if (!this.isConnected || seq !== this.loadSeq) return;
      this.error = err instanceof Error ? err.message : 'Failed to load message policies';
    } finally {
      if (this.isConnected && seq === this.loadSeq) {
        this.loading = false;
      }
    }
  }

  private handleFilterChange(kind: 'mode' | 'enabled', value: string): void {
    if (kind === 'mode') {
      this.modeFilter = value;
    } else {
      this.enabledFilter = value;
    }
    this.cursorStack = [''];
    this.pageIndex = 0;
    void this.loadPolicies();
  }

  private handleNextPage(): void {
    if (!this.nextCursor) return;
    const nextIndex = this.pageIndex + 1;
    this.cursorStack = [...this.cursorStack.slice(0, nextIndex), this.nextCursor];
    this.pageIndex = nextIndex;
    void this.loadPolicies();
  }

  private handlePrevPage(): void {
    if (this.pageIndex <= 0) return;
    this.pageIndex -= 1;
    void this.loadPolicies();
  }

  private async openDetail(policy: MessagePolicy): Promise<void> {
    const seq = ++this.detailSeq;
    this.detailPolicy = policy;
    this.detailOpen = true;
    this.detailError = null;
    this.detailLoading = true;

    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/message-policies/${encodeURIComponent(policy.id)}`
      );
      if (!this.isConnected || !this.detailOpen || seq !== this.detailSeq) return;
      if (!res.ok) {
        this.detailError = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      const fresh = (await res.json()) as MessagePolicy;
      if (!this.isConnected || !this.detailOpen || seq !== this.detailSeq) return;
      this.detailPolicy = fresh;
    } catch (err) {
      if (!this.isConnected || !this.detailOpen || seq !== this.detailSeq) return;
      this.detailError = err instanceof Error ? err.message : 'Failed to load policy details';
    } finally {
      if (this.isConnected && seq === this.detailSeq) {
        this.detailLoading = false;
      }
    }
  }

  private resolveAgentName(id: string): string {
    const found = this.agents.find((a) => a.id === id);
    return found ? `${found.name} (${id})` : id;
  }

  private handleViewOutcomes(policyId: string): void {
    this.detailOpen = false;
    this.dispatchEvent(
      new CustomEvent('view-policy-outcomes', {
        detail: { policyId },
        bubbles: true,
        composed: true,
      })
    );
  }

  private renderCoverageBanner() {
    if (!this.coverage) return nothing;
    const enforced = this.coverage.enforced_surfaces ?? [];
    const uncovered = this.coverage.uncovered_agent_paths ?? [];

    return html`
      <section class="coverage-card" data-coverage-banner aria-label="Coverage on this Hub">
        <div class="coverage-header">
          <sl-icon name="info-circle"></sl-icon>
          <h2 class="coverage-title">Coverage on this Hub</h2>
        </div>
        <div class="coverage-grid">
          <div class="coverage-label">Evaluated</div>
          <div class="chip-row" data-coverage-enforced>
            ${enforced.length > 0
              ? enforced.map(
                  (s) => html`<span class="chip chip-covered" data-surface=${s}>${s}</span>`
                )
              : html`<span class="muted">None</span>`}
          </div>
          <div class="coverage-label">NOT evaluated (agent)</div>
          <div class="chip-row" data-coverage-uncovered>
            ${uncovered.length > 0
              ? uncovered.map(
                  (s) => html`<span class="chip chip-uncovered" data-uncovered=${s}>${s}</span>`
                )
              : html`<span class="muted">None</span>`}
          </div>
        </div>
      </section>
    `;
  }

  private renderDetailDrawer() {
    const p = this.detailPolicy;
    return html`
      <sl-drawer
        label=${p ? `Policy: ${p.name}` : 'Policy details'}
        ?open=${this.detailOpen}
        style="--size: 34rem;"
        data-policy-detail-drawer
        @sl-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.detailOpen = false;
        }}
      >
        ${this.detailError
          ? html`
              <div class="alert-wrap">
                <sl-alert variant="danger" open>
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  ${this.detailError}
                </sl-alert>
              </div>
            `
          : nothing}
        ${this.detailLoading && !p
          ? html`<div class="loading-state"><sl-spinner></sl-spinner></div>`
          : p
            ? html`
                <div class="drawer-section">
                  <h4>Overview</h4>
                  <div class="kv-grid">
                    <span class="kv-key">Name</span>
                    <span class="mono">${p.name}</span>
                    <span class="kv-key">ID</span>
                    <span class="mono">${p.id}</span>
                    ${p.description
                      ? html`
                          <span class="kv-key">Description</span>
                          <span>${p.description}</span>
                        `
                      : nothing}
                    <span class="kv-key">Mode</span>
                    <span>
                      <sl-badge variant=${p.mode === 'enforce' ? 'danger' : 'neutral'}>
                        ${p.mode}
                      </sl-badge>
                    </span>
                    <span class="kv-key">Enabled</span>
                    <span>
                      <sl-badge variant=${p.enabled ? 'success' : 'neutral'}>
                        ${p.enabled ? 'on' : 'off'}
                      </sl-badge>
                    </span>
                    <span class="kv-key">Revision</span>
                    <span class="mono">${p.revision}</span>
                    <span class="kv-key">Updated</span>
                    <span>${formatInstantWithZone(p.updated_at)}</span>
                    <span class="kv-key">Created</span>
                    <span>${formatInstantWithZone(p.created_at)}</span>
                    ${p.updated_by
                      ? html`
                          <span class="kv-key">Updated by</span>
                          <span class="mono">${p.updated_by}</span>
                        `
                      : nothing}
                    ${p.created_by
                      ? html`
                          <span class="kv-key">Created by</span>
                          <span class="mono">${p.created_by}</span>
                        `
                      : nothing}
                  </div>
                </div>

                <sl-divider></sl-divider>

                <div class="drawer-section">
                  <h4>Scope</h4>
                  <div class="kv-grid">
                    <span class="kv-key">Surfaces</span>
                    <div class="chip-row">
                      ${(p.scope?.surfaces ?? []).map(
                        (s) => html`<span class="chip">${formatSurfaceLabel(s)}</span>`
                      )}
                    </div>
                    <span class="kv-key">Senders</span>
                    <span>
                      ${p.scope?.sender?.all_agents || !p.scope?.sender?.agent_ids?.length
                        ? 'All project agents'
                        : p.scope.sender.agent_ids
                            .map((id) => this.resolveAgentName(id))
                            .join(', ')}
                    </span>
                    <span class="kv-key">Recipient kinds</span>
                    <span>
                      ${p.scope?.recipient?.kinds?.length
                        ? p.scope.recipient.kinds.join(', ')
                        : 'Any'}
                    </span>
                    ${p.scope?.recipient?.agent_ids?.length
                      ? html`
                          <span class="kv-key">Recipient agents</span>
                          <span>
                            ${p.scope.recipient.agent_ids
                              .map((id) => this.resolveAgentName(id))
                              .join(', ')}
                          </span>
                        `
                      : nothing}
                  </div>
                </div>

                <sl-divider></sl-divider>

                <div class="drawer-section">
                  <h4>Violation rule (CEL)</h4>
                  <pre class="code-block" data-detail-violation-rule>${p.violation_rule}</pre>
                </div>

                ${this.systemOneEnabled && hasSystemOneRule(p)
                  ? html`
                      <sl-divider></sl-divider>
                      <div class="drawer-section" data-detail-system-one-section>
                        <h4>System One</h4>
                        <div class="kv-grid" style="margin-bottom: 0.75rem;">
                          ${p.system_one_refs?.length
                            ? html`
                                <span class="kv-key">Referenced instances</span>
                                <div class="chip-row">
                                  ${p.system_one_refs.map(
                                    (ref) => html`<span class="chip">${ref}</span>`
                                  )}
                                </div>
                              `
                            : nothing}
                          ${p.on_uncertain
                            ? html`
                                <span class="kv-key">On uncertain</span>
                                <span>
                                  <sl-badge
                                    variant=${p.on_uncertain === 'reject' ? 'warning' : 'neutral'}
                                  >
                                    ${p.on_uncertain}
                                  </sl-badge>
                                </span>
                              `
                            : nothing}
                        </div>
                        ${p.system_one_rule
                          ? html`<pre class="code-block" data-detail-system-one-rule>
${p.system_one_rule}</pre
                            >`
                          : nothing}
                      </div>
                    `
                  : nothing}

                <sl-divider></sl-divider>

                <div class="drawer-section">
                  <h4>Failure & rejection handling</h4>
                  <div class="kv-grid">
                    <span class="kv-key">On failed</span>
                    <span>
                      <sl-badge variant=${p.on_failed === 'reject' ? 'warning' : 'neutral'}>
                        ${p.on_failed}
                      </sl-badge>
                    </span>
                    ${p.on_failed_reasons && Object.keys(p.on_failed_reasons).length > 0
                      ? html`
                          <span class="kv-key">Per-reason overrides</span>
                          <div class="chip-row">
                            ${Object.entries(p.on_failed_reasons).map(
                              ([reason, outcome]) =>
                                html`<span class="chip">${reason} → ${outcome}</span>`
                            )}
                          </div>
                        `
                      : nothing}
                    ${p.reject_reason
                      ? html`
                          <span class="kv-key">Reject reason</span>
                          <span>${p.reject_reason}</span>
                        `
                      : nothing}
                    ${p.reject_remediation
                      ? html`
                          <span class="kv-key">Reject remediation</span>
                          <span>${p.reject_remediation}</span>
                        `
                      : nothing}
                  </div>
                </div>

                ${this.canManage
                  ? html`
                      <div slot="footer">
                        <sl-button
                          size="small"
                          data-view-policy-outcomes
                          @click=${() => this.handleViewOutcomes(p.id)}
                        >
                          <sl-icon slot="prefix" name="clock-history"></sl-icon>
                          View policy outcomes
                        </sl-button>
                      </div>
                    `
                  : nothing}
              `
            : nothing}
      </sl-drawer>
    `;
  }

  override render() {
    void this._zone;

    if (this.forbidden) {
      return html`
        <div class="forbidden-state" role="alert" data-policies-forbidden>
          <sl-icon name="shield-x"></sl-icon>
          <h3>Access Denied</h3>
          <p>
            You do not have permission to view message policies for this project (<code
              >project.read_message_policies</code
            >
            required).
          </p>
        </div>
      `;
    }

    const hasFilters = Boolean(this.modeFilter || this.enabledFilter);

    return html`
      ${this.renderCoverageBanner()}

      <div class="toolbar">
        <div class="filters">
          <sl-select
            label="Mode"
            size="small"
            value=${this.modeFilter}
            data-filter-mode
            @sl-change=${(e: Event) => {
              const target = e.target as HTMLSelectElement;
              this.handleFilterChange('mode', target.value);
            }}
          >
            <sl-option value="">All modes</sl-option>
            <sl-option value="shadow">Shadow</sl-option>
            <sl-option value="enforce">Enforce</sl-option>
          </sl-select>

          <sl-select
            label="Enabled"
            size="small"
            value=${this.enabledFilter}
            data-filter-enabled
            @sl-change=${(e: Event) => {
              const target = e.target as HTMLSelectElement;
              this.handleFilterChange('enabled', target.value);
            }}
          >
            <sl-option value="">All</sl-option>
            <sl-option value="true">Enabled</sl-option>
            <sl-option value="false">Disabled</sl-option>
          </sl-select>
        </div>

        ${!this.canManage
          ? html`
              <div class="readonly-banner" data-readonly-indicator>
                <sl-icon name="eye"></sl-icon>
                <span>Read-only access</span>
              </div>
            `
          : nothing}
      </div>

      ${this.error
        ? html`
            <div class="alert-wrap" data-policies-error>
              <sl-alert variant="danger" open>
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                <div>${this.error}</div>
                <sl-button
                  size="small"
                  style="margin-top: 0.5rem;"
                  @click=${() => this.loadPolicies()}
                >
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </sl-alert>
            </div>
          `
        : nothing}
      ${this.loading
        ? html`
            <div class="loading-state">
              <sl-spinner></sl-spinner>
              <p>Loading message policies...</p>
            </div>
          `
        : !this.error && this.policies.length === 0
          ? html`
              <div class="empty-state" data-policies-empty>
                <sl-icon name="shield-check"></sl-icon>
                <h3>
                  ${hasFilters ? 'No matching message policies' : 'No message policies configured'}
                </h3>
                <p>
                  ${hasFilters
                    ? 'Try clearing or adjusting the mode and enabled filters.'
                    : 'Message policies evaluate outbound agent sends on covered surfaces before they are stored or delivered.'}
                </p>
              </div>
            `
          : !this.error
            ? html`
                <div class="table-wrap">
                  <table data-policies-table>
                    <thead>
                      <tr>
                        <th>Name</th>
                        <th>Mode</th>
                        <th>Enabled</th>
                        <th>Surfaces</th>
                        ${this.systemOneEnabled
                          ? html`<th data-col-engines>Rule engines</th>`
                          : nothing}
                        <th>Rev</th>
                        <th>Updated</th>
                      </tr>
                    </thead>
                    <tbody>
                      ${this.policies.map(
                        (p) => html`
                          <tr data-policy-row=${p.id}>
                            <td>
                              <button
                                type="button"
                                class="policy-name-btn"
                                data-policy-name=${p.name}
                                @click=${() => void this.openDetail(p)}
                              >
                                ${p.name}
                              </button>
                              ${p.description
                                ? html`<div class="policy-desc">${p.description}</div>`
                                : nothing}
                            </td>
                            <td>
                              <sl-badge variant=${p.mode === 'enforce' ? 'danger' : 'neutral'}>
                                ${p.mode}
                              </sl-badge>
                            </td>
                            <td>
                              <sl-badge variant=${p.enabled ? 'success' : 'neutral'}>
                                ${p.enabled ? 'on' : 'off'}
                              </sl-badge>
                            </td>
                            <td>
                              <div class="chip-row">
                                ${(p.scope?.surfaces ?? []).map(
                                  (s) => html`<span class="chip">${formatSurfaceLabel(s)}</span>`
                                )}
                              </div>
                            </td>
                            ${this.systemOneEnabled
                              ? html`
                                  <td data-cell-engines>
                                    <div class="badge-row">
                                      <sl-badge variant="primary">CEL</sl-badge>
                                      ${hasSystemOneRule(p)
                                        ? html`<sl-badge variant="warning" data-system-one-badge
                                            >System One</sl-badge
                                          >`
                                        : nothing}
                                    </div>
                                  </td>
                                `
                              : nothing}
                            <td class="mono">${p.revision}</td>
                            <td class="muted" title=${formatInstantWithZone(p.updated_at)}>
                              ${formatRelativeTime(p.updated_at)}
                            </td>
                          </tr>
                        `
                      )}
                    </tbody>
                  </table>
                </div>

                <div class="pagination" data-policies-pagination>
                  <span>
                    Showing ${this.policies.length} of ${this.totalCount}
                    ${this.totalCount === 1 ? 'policy' : 'policies'}
                  </span>
                  <div class="pagination-controls">
                    <sl-button
                      size="small"
                      ?disabled=${this.pageIndex === 0}
                      data-prev-page
                      @click=${() => this.handlePrevPage()}
                    >
                      <sl-icon slot="prefix" name="chevron-left"></sl-icon>
                      Previous
                    </sl-button>
                    <sl-button
                      size="small"
                      ?disabled=${!this.nextCursor}
                      data-next-page
                      @click=${() => this.handleNextPage()}
                    >
                      Next
                      <sl-icon slot="suffix" name="chevron-right"></sl-icon>
                    </sl-button>
                  </div>
                </div>
              `
            : nothing}
      ${this.renderDetailDrawer()}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-message-policy-list': ScionMessagePolicyList;
  }
}
