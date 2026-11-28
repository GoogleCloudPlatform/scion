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
 * Content-free Policy Outcomes view over `GET /api/v1/projects/{id}/message-policies/decisions`.
 *
 * Labelled "Policy outcomes" in the UI because `/projects/{id}/decisions` is
 * reserved for choice-prompt Decisions. Carries no message body, prompt text,
 * or copy facts.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstant, formatInstantWithZone } from '../../utils/time.js';
import {
  KNOWN_ENFORCED_SURFACES,
  formatSurfaceLabel,
  type EvaluationProjection,
  type ListMessagePolicyDecisionsResponse,
  type MessagePolicy,
  type MessagePolicyDecision,
  type ProjectAgentSummary,
} from './types.js';

const PAGE_LIMIT = 50;

@customElement('scion-message-policy-outcomes')
export class ScionMessagePolicyOutcomes extends LitElement {
  private _zone = new DisplayZoneController(this);

  @property({ type: String })
  projectId = '';

  @property({ type: Boolean })
  systemOneEnabled = false;

  @property({ type: Array })
  policies: MessagePolicy[] = [];

  @property({ type: Array })
  agents: ProjectAgentSummary[] = [];

  @property({ type: String })
  initialPolicyId = '';

  @state() private decisions: MessagePolicyDecision[] = [];
  @state() private totalCount = 0;
  @state() private nextCursor = '';
  @state() private cursorStack: string[] = [''];
  @state() private pageIndex = 0;

  @state() private policyFilter = '';
  @state() private modeFilter = '';
  @state() private surfaceFilter = '';
  @state() private classFilter = '';
  @state() private actionFilter = '';

  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private forbidden = false;

  @state() private selectedDecision: MessagePolicyDecision | null = null;
  @state() private drawerOpen = false;

  private loadSeq = 0;

  static override styles = css`
    :host {
      display: block;
    }

    .notice-bar {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      padding: 0.75rem 1rem;
      margin-bottom: 1rem;
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      font-size: 0.8125rem;
      color: var(--scion-text-secondary);
    }

    .notice-bar sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
      color: var(--scion-primary);
    }

    .filters {
      display: flex;
      align-items: flex-end;
      gap: 0.75rem;
      margin-bottom: 1rem;
      flex-wrap: wrap;
    }

    .filters sl-select {
      width: 170px;
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
      padding: 0.625rem 0.75rem;
      border-bottom: 1px solid var(--scion-border);
      background: var(--scion-bg-subtle);
      white-space: nowrap;
    }

    td {
      padding: 0.6875rem 0.75rem;
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

    .row-trigger {
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

    .row-trigger:hover {
      text-decoration: underline;
    }

    .mono {
      font-family: var(--scion-font-mono);
      font-size: 0.8125rem;
    }

    .muted {
      color: var(--scion-text-muted);
    }

    .chip {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.4375rem;
      border-radius: var(--scion-radius-sm);
      font-size: 0.75rem;
      border: 1px solid var(--scion-border);
      background: var(--scion-bg-subtle);
      color: var(--scion-text);
      white-space: nowrap;
    }

    .msg-ref {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      font-family: var(--scion-font-mono);
      color: var(--scion-text-muted);
      margin-left: 0.375rem;
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

    .retention-footer {
      margin-top: 0.75rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
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
      grid-template-columns: 145px 1fr;
      gap: 0.375rem 0.75rem;
      font-size: 0.875rem;
    }

    .kv-key {
      color: var(--scion-text-muted);
    }

    .sub-card {
      background: var(--scion-bg-subtle);
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      padding: 0.625rem 0.75rem;
      margin-bottom: 0.5rem;
      font-size: 0.8125rem;
    }

    @media (max-width: 640px) {
      .kv-grid {
        grid-template-columns: 1fr;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.initialPolicyId) {
      this.policyFilter = this.initialPolicyId;
    }
    if (this.projectId) {
      void this.loadDecisions();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    const prevPolicyId = changed.get('initialPolicyId');
    if (
      changed.has('initialPolicyId') &&
      prevPolicyId !== undefined &&
      this.initialPolicyId !== this.policyFilter
    ) {
      this.policyFilter = this.initialPolicyId;
      this.cursorStack = [''];
      this.pageIndex = 0;
      if (this.projectId) {
        void this.loadDecisions();
      }
      return;
    }
    const prevProjectId = changed.get('projectId');
    if (changed.has('projectId') && prevProjectId !== undefined && this.projectId) {
      this.cursorStack = [''];
      this.pageIndex = 0;
      void this.loadDecisions();
    }
  }

  public filterByPolicy(policyId: string): void {
    this.policyFilter = policyId;
    this.cursorStack = [''];
    this.pageIndex = 0;
    void this.loadDecisions();
  }

  public async loadDecisions(): Promise<void> {
    if (!this.projectId) return;

    const seq = ++this.loadSeq;
    this.loading = true;
    this.error = null;
    this.forbidden = false;

    const params = new URLSearchParams();
    params.set('limit', String(PAGE_LIMIT));
    if (this.policyFilter) params.set('policy_id', this.policyFilter);
    if (this.modeFilter) params.set('mode', this.modeFilter);
    if (this.surfaceFilter) params.set('surface', this.surfaceFilter);
    if (this.classFilter) params.set('class', this.classFilter);
    if (this.actionFilter) params.set('action', this.actionFilter);
    const cursor = this.cursorStack[this.pageIndex];
    if (cursor) params.set('cursor', cursor);

    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/message-policies/decisions?${params.toString()}`
      );
      if (!this.isConnected || seq !== this.loadSeq) return;
      if (res.status === 403) {
        this.forbidden = true;
        this.decisions = [];
        return;
      }
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }

      const data = (await res.json()) as ListMessagePolicyDecisionsResponse;
      if (!this.isConnected || seq !== this.loadSeq) return;

      this.decisions = Array.isArray(data.decisions) ? data.decisions : [];
      this.totalCount =
        typeof data.total_count === 'number' ? data.total_count : this.decisions.length;
      this.nextCursor = data.next_cursor ?? '';
    } catch (err) {
      if (!this.isConnected || seq !== this.loadSeq) return;
      this.error = err instanceof Error ? err.message : 'Failed to load policy outcomes';
    } finally {
      if (this.isConnected && seq === this.loadSeq) {
        this.loading = false;
      }
    }
  }

  private setFilter(kind: 'policy' | 'mode' | 'surface' | 'class' | 'action', value: string): void {
    switch (kind) {
      case 'policy':
        this.policyFilter = value;
        break;
      case 'mode':
        this.modeFilter = value;
        break;
      case 'surface':
        this.surfaceFilter = value;
        break;
      case 'class':
        this.classFilter = value;
        break;
      case 'action':
        this.actionFilter = value;
        break;
    }
    this.cursorStack = [''];
    this.pageIndex = 0;
    void this.loadDecisions();
  }

  private handleNextPage(): void {
    if (!this.nextCursor) return;
    const nextIndex = this.pageIndex + 1;
    this.cursorStack = [...this.cursorStack.slice(0, nextIndex), this.nextCursor];
    this.pageIndex = nextIndex;
    void this.loadDecisions();
  }

  private handlePrevPage(): void {
    if (this.pageIndex <= 0) return;
    this.pageIndex -= 1;
    void this.loadDecisions();
  }

  private formatParty(kind?: string, id?: string): string {
    if (!kind && !id) return '—';
    let label = id ?? '';
    if (kind === 'agent' && id) {
      const found = this.agents.find((a) => a.id === id);
      if (found) label = found.name;
    }
    return kind && label ? `${kind}:${label}` : kind || label;
  }

  private classBadgeVariant(cls: string): 'success' | 'danger' | 'warning' | 'neutral' {
    switch (cls) {
      case 'pass':
        return 'success';
      case 'violation':
        return 'danger';
      case 'uncertain':
      case 'failed':
        return 'warning';
      default:
        return 'neutral';
    }
  }

  private actionBadgeVariant(action: string): 'success' | 'danger' | 'warning' | 'neutral' {
    switch (action) {
      case 'admitted':
        return 'success';
      case 'rejected':
        return 'danger';
      case 'shadow':
        return 'warning';
      default:
        return 'neutral';
    }
  }

  private actionLabel(d: MessagePolicyDecision): string {
    return d.action;
  }

  private openRowDrawer(d: MessagePolicyDecision): void {
    this.selectedDecision = d;
    this.drawerOpen = true;
  }

  private renderEvaluationProjection(ev: EvaluationProjection) {
    const attempts = ev.attempts ?? [];
    const totalRequests =
      ev.usage?.requests ?? attempts.reduce((sum, a) => sum + (a.requests ?? 0), 0);
    const totalRetries =
      ev.usage?.retries ?? attempts.reduce((sum, a) => sum + (a.retries ?? 0), 0);
    const hasUsage =
      Boolean(ev.usage) || totalRequests > 0 || totalRetries > 0 || attempts.length > 0;

    return html`
      <sl-divider></sl-divider>
      <div class="drawer-section" data-evaluation-projection>
        <h4>Evaluation projection (content-free)</h4>
        <div class="kv-grid">
          ${ev.eval_id
            ? html`
                <span class="kv-key">Eval ID</span>
                <span class="mono">${ev.eval_id}</span>
              `
            : nothing}
          ${ev.class
            ? html`
                <span class="kv-key">Class</span>
                <span>
                  <sl-badge variant=${this.classBadgeVariant(ev.class)}>${ev.class}</sl-badge>
                </span>
              `
            : nothing}
          ${ev.reason
            ? html`
                <span class="kv-key">Reason</span>
                <span class="mono">${ev.reason}</span>
              `
            : nothing}
          ${ev.rule
            ? html`
                <span class="kv-key">Rule ref</span>
                <span class="mono" data-eval-rule-ref>
                  ${[
                    ev.rule.rule_id,
                    ev.rule.revision ? `rev ${ev.rule.revision}` : '',
                    ev.rule.dialect,
                    ev.rule.consumer,
                  ]
                    .filter(Boolean)
                    .join(' · ') || '—'}
                </span>
              `
            : nothing}
          ${ev.source_eval_id
            ? html`
                <span class="kv-key">Source eval ID</span>
                <span class="mono" data-eval-source-id>${ev.source_eval_id}</span>
              `
            : nothing}
          ${ev.cache_hit
            ? html`
                <span class="kv-key">Cache hit</span>
                <span>yes</span>
              `
            : nothing}
          ${ev.instance
            ? html`
                <span class="kv-key">Instance</span>
                <span class="mono">${ev.instance}</span>
              `
            : nothing}
          ${ev.configured_model
            ? html`
                <span class="kv-key">Configured model</span>
                <span class="mono">${ev.configured_model}</span>
              `
            : nothing}
          ${ev.resolved_model
            ? html`
                <span class="kv-key">Resolved model</span>
                <span class="mono">${ev.resolved_model}</span>
              `
            : nothing}
          ${ev.fallback_used
            ? html`
                <span class="kv-key">Fallback</span>
                <span>${ev.fallback_cause || 'used'}</span>
              `
            : nothing}
          ${typeof ev.elapsed_ms === 'number'
            ? html`
                <span class="kv-key">Eval elapsed</span>
                <span class="mono">${ev.elapsed_ms} ms</span>
              `
            : nothing}
        </div>

        ${hasUsage
          ? html`
              <div style="margin-top: 0.75rem;" data-eval-usage>
                <h4>Usage</h4>
                <div class="kv-grid">
                  ${typeof ev.usage?.input_tokens === 'number'
                    ? html`
                        <span class="kv-key">Input tokens</span>
                        <span class="mono">${ev.usage.input_tokens}</span>
                      `
                    : nothing}
                  ${typeof ev.usage?.output_tokens === 'number'
                    ? html`
                        <span class="kv-key">Output tokens</span>
                        <span class="mono">${ev.usage.output_tokens}</span>
                      `
                    : nothing}
                  ${typeof ev.usage?.total_tokens === 'number'
                    ? html`
                        <span class="kv-key">Total tokens</span>
                        <span class="mono">${ev.usage.total_tokens}</span>
                      `
                    : nothing}
                  ${totalRequests > 0
                    ? html`
                        <span class="kv-key">Requests</span>
                        <span class="mono">${totalRequests}</span>
                      `
                    : nothing}
                  ${totalRetries > 0
                    ? html`
                        <span class="kv-key">Retries</span>
                        <span class="mono">${totalRetries}</span>
                      `
                    : nothing}
                </div>
              </div>
            `
          : nothing}
        ${ev.input_fields && ev.input_fields.length > 0
          ? html`
              <div style="margin-top: 0.75rem;">
                <h4>Input field sizes</h4>
                ${ev.input_fields.map(
                  (f) => html`
                    <div class="sub-card mono">
                      ${f.name}: ${f.bytes} bytes${f.truncated ? ' (truncated)' : ''}
                    </div>
                  `
                )}
              </div>
            `
          : nothing}
        ${ev.answers && Object.keys(ev.answers).length > 0
          ? html`
              <div style="margin-top: 0.75rem;">
                <h4>Answers</h4>
                ${Object.values(ev.answers).map(
                  (a) => html`
                    <div class="sub-card">
                      <div>
                        <strong class="mono">${a.question_id}</strong>
                        <span class="muted"> (${a.primitive}, ${a.status})</span>
                      </div>
                      <div class="mono">
                        ${a.bool !== undefined
                          ? `bool=${String(a.bool)}`
                          : a.choice !== undefined
                            ? `choice=${a.choice}`
                            : a.abstained
                              ? 'abstained'
                              : '—'}
                      </div>
                    </div>
                  `
                )}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderOutcomeDrawer() {
    const d = this.selectedDecision;
    return html`
      <sl-drawer
        label="Policy outcome"
        ?open=${this.drawerOpen}
        style="--size: 32rem;"
        data-outcome-drawer
        @sl-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.drawerOpen = false;
        }}
      >
        ${d
          ? html`
              <div class="drawer-section">
                <h4>Decision record</h4>
                <div class="kv-grid">
                  <span class="kv-key">Record ID</span>
                  <span class="mono">${d.id}</span>
                  <span class="kv-key">Time</span>
                  <span>${formatInstantWithZone(d.created_at)}</span>
                  <span class="kv-key">Policy</span>
                  <span class="mono">
                    ${d.policy_name || d.policy_id} (rev ${d.policy_revision})
                  </span>
                  <span class="kv-key">Mode</span>
                  <span>
                    <sl-badge variant=${d.mode === 'enforce' ? 'danger' : 'neutral'}>
                      ${d.mode}
                    </sl-badge>
                  </span>
                  <span class="kv-key">Surface</span>
                  <span>${formatSurfaceLabel(d.surface)}</span>
                  <span class="kv-key">Sender</span>
                  <span class="mono">${this.formatParty(d.sender_kind, d.sender_id)}</span>
                  <span class="kv-key">Recipient</span>
                  <span class="mono">${this.formatParty(d.recipient_kind, d.recipient_id)}</span>
                  <span class="kv-key">Class</span>
                  <span>
                    <sl-badge variant=${this.classBadgeVariant(d.class)}>${d.class}</sl-badge>
                  </span>
                  <span class="kv-key">Action</span>
                  <span>
                    <sl-badge variant=${this.actionBadgeVariant(d.action)}>
                      ${this.actionLabel(d)}
                    </sl-badge>
                  </span>
                  ${d.reason
                    ? html`
                        <span class="kv-key">Reason</span>
                        <span class="mono">${d.reason}</span>
                      `
                    : nothing}
                  ${d.cel_error_reason
                    ? html`
                        <span class="kv-key">CEL error</span>
                        <span class="mono">${d.cel_error_reason}</span>
                      `
                    : nothing}
                  ${d.message_id && d.action !== 'rejected'
                    ? html`
                        <span class="kv-key">Message ID</span>
                        <span class="mono" data-drawer-message-id>${d.message_id}</span>
                      `
                    : nothing}
                  <span class="kv-key">Elapsed</span>
                  <span class="mono">${d.elapsed_ms} ms</span>
                </div>
              </div>

              ${this.systemOneEnabled && d.evaluation
                ? this.renderEvaluationProjection(d.evaluation)
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
        <div class="forbidden-state" role="alert" data-outcomes-forbidden>
          <sl-icon name="shield-x"></sl-icon>
          <h3>Access Denied</h3>
          <p>
            You do not have permission to view policy outcomes for this project (<code
              >project.manage_message_policies</code
            >
            required).
          </p>
        </div>
      `;
    }

    return html`
      <div class="notice-bar">
        <sl-icon name="info-circle"></sl-icon>
        <span>
          Rejected sends are never stored or delivered; these content-free records are the only
          trace. No message content is kept.
        </span>
      </div>

      <div class="filters">
        <sl-select
          label="Policy"
          size="small"
          value=${this.policyFilter}
          data-outcome-filter-policy
          @sl-change=${(e: Event) => {
            this.setFilter('policy', (e.target as HTMLSelectElement).value);
          }}
        >
          <sl-option value="">All policies</sl-option>
          ${this.policies.map((p) => html`<sl-option value=${p.id}>${p.name}</sl-option>`)}
          ${this.policyFilter && !this.policies.some((p) => p.id === this.policyFilter)
            ? html`<sl-option value=${this.policyFilter} data-active-policy-fallback
                >${this.policyFilter}</sl-option
              >`
            : nothing}
        </sl-select>

        <sl-select
          label="Mode"
          size="small"
          value=${this.modeFilter}
          data-outcome-filter-mode
          @sl-change=${(e: Event) => {
            this.setFilter('mode', (e.target as HTMLSelectElement).value);
          }}
        >
          <sl-option value="">All modes</sl-option>
          <sl-option value="shadow">Shadow</sl-option>
          <sl-option value="enforce">Enforce</sl-option>
        </sl-select>

        <sl-select
          label="Surface"
          size="small"
          value=${this.surfaceFilter}
          data-outcome-filter-surface
          @sl-change=${(e: Event) => {
            this.setFilter('surface', (e.target as HTMLSelectElement).value);
          }}
        >
          <sl-option value="">All surfaces</sl-option>
          ${KNOWN_ENFORCED_SURFACES.map(
            (s) => html`<sl-option value=${s}>${formatSurfaceLabel(s)}</sl-option>`
          )}
        </sl-select>

        <sl-select
          label="Class"
          size="small"
          value=${this.classFilter}
          data-outcome-filter-class
          @sl-change=${(e: Event) => {
            this.setFilter('class', (e.target as HTMLSelectElement).value);
          }}
        >
          <sl-option value="">All classes</sl-option>
          <sl-option value="pass">Pass</sl-option>
          <sl-option value="violation">Violation</sl-option>
          <sl-option value="uncertain">Uncertain</sl-option>
          <sl-option value="failed">Failed</sl-option>
        </sl-select>

        <sl-select
          label="Action"
          size="small"
          value=${this.actionFilter}
          data-outcome-filter-action
          @sl-change=${(e: Event) => {
            this.setFilter('action', (e.target as HTMLSelectElement).value);
          }}
        >
          <sl-option value="">All actions</sl-option>
          <sl-option value="admitted">Admitted</sl-option>
          <sl-option value="rejected">Rejected</sl-option>
          <sl-option value="shadow">Shadow</sl-option>
        </sl-select>
      </div>

      ${this.error
        ? html`
            <div class="alert-wrap" data-outcomes-error>
              <sl-alert variant="danger" open>
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                <div>${this.error}</div>
                <sl-button
                  size="small"
                  style="margin-top: 0.5rem;"
                  @click=${() => this.loadDecisions()}
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
              <p>Loading policy outcomes...</p>
            </div>
          `
        : !this.error && this.decisions.length === 0
          ? html`
              <div class="empty-state" data-outcomes-empty>
                <sl-icon name="clock-history"></sl-icon>
                <h3>No policy outcomes recorded</h3>
                <p>No content-free decision records match the current filters.</p>
              </div>
            `
          : !this.error
            ? html`
                <div class="table-wrap">
                  <table data-outcomes-table>
                    <thead>
                      <tr>
                        <th>Time</th>
                        <th>Policy (rev)</th>
                        <th>Mode</th>
                        <th>Surface</th>
                        <th>Sender → Recipient</th>
                        <th>Class</th>
                        <th>Action</th>
                        <th>Elapsed</th>
                      </tr>
                    </thead>
                    <tbody>
                      ${this.decisions.map(
                        (d) => html`
                          <tr data-outcome-row=${d.id}>
                            <td class="mono" title=${formatInstantWithZone(d.created_at)}>
                              ${formatInstant(d.created_at)}
                            </td>
                            <td>
                              <button
                                type="button"
                                class="row-trigger"
                                data-outcome-open=${d.id}
                                @click=${() => this.openRowDrawer(d)}
                              >
                                ${d.policy_name || d.policy_id}
                              </button>
                              <span class="muted mono"> (rev ${d.policy_revision})</span>
                            </td>
                            <td>
                              <sl-badge variant=${d.mode === 'enforce' ? 'danger' : 'neutral'}>
                                ${d.mode}
                              </sl-badge>
                            </td>
                            <td>
                              <span class="chip">${formatSurfaceLabel(d.surface)}</span>
                            </td>
                            <td class="mono">
                              ${this.formatParty(d.sender_kind, d.sender_id)} →
                              ${this.formatParty(d.recipient_kind, d.recipient_id)}
                            </td>
                            <td>
                              <sl-badge variant=${this.classBadgeVariant(d.class)}>
                                ${d.class}
                              </sl-badge>
                            </td>
                            <td>
                              <sl-badge variant=${this.actionBadgeVariant(d.action)}>
                                ${this.actionLabel(d)}
                              </sl-badge>
                              ${d.message_id && d.action !== 'rejected'
                                ? html`<span
                                    class="msg-ref"
                                    data-outcome-message-ref=${d.message_id}
                                    >msg:${d.message_id}</span
                                  >`
                                : nothing}
                            </td>
                            <td class="mono muted">${d.elapsed_ms} ms</td>
                          </tr>
                        `
                      )}
                    </tbody>
                  </table>
                </div>

                <div class="pagination" data-outcomes-pagination>
                  <span>
                    Showing ${this.decisions.length} of ${this.totalCount}
                    ${this.totalCount === 1 ? 'record' : 'records'}
                  </span>
                  <div class="pagination-controls">
                    <sl-button
                      size="small"
                      ?disabled=${this.pageIndex === 0}
                      data-outcomes-prev
                      @click=${() => this.handlePrevPage()}
                    >
                      <sl-icon slot="prefix" name="chevron-left"></sl-icon>
                      Previous
                    </sl-button>
                    <sl-button
                      size="small"
                      ?disabled=${!this.nextCursor}
                      data-outcomes-next
                      @click=${() => this.handleNextPage()}
                    >
                      Next
                      <sl-icon slot="suffix" name="chevron-right"></sl-icon>
                    </sl-button>
                  </div>
                </div>
              `
            : nothing}

      <p class="retention-footer" data-retention-footer>
        Records older than 7 days, or beyond 100k per project, are trimmed.
      </p>

      ${this.renderOutcomeDrawer()}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-message-policy-outcomes': ScionMessagePolicyOutcomes;
  }
}
