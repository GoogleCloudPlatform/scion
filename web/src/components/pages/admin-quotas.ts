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
 * Admin Quotas page component
 *
 * Manages limit definitions, entitlement bindings, and usage display.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface LimitDefinition {
  id: string;
  name: string;
  resourceType: string;
  unit: string;
  description: string;
  defaultValue: number;
  system: boolean;
  createdAt: string;
  updatedAt: string;
}

interface EntitlementBinding {
  id: string;
  limitDefinitionId: string;
  subjectType: string;
  subjectId: string;
  scopeType: string;
  scopeId: string;
  value: number;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
}

interface UsageSummaryEntry {
  limitDefinition: LimitDefinition;
  activeCount: number;
}

interface UsageByLimitResponse {
  limitDefinition: LimitDefinition;
  reservations: UsageReservation[];
  totalActive: number;
}

interface UsageReservation {
  id: string;
  limitDefinitionId: string;
  subjectId: string;
  scopeType: string;
  scopeId: string;
  resourceId: string;
  reserved: number;
  createdAt: string;
  releasedAt?: string;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-page-admin-quotas')
export class ScionPageAdminQuotas extends LitElement {
  // --- List state ---
  @state() private loading = true;
  @state() private limits: LimitDefinition[] = [];
  @state() private usageSummary: Map<string, number> = new Map();
  @state() private error: string | null = null;

  // --- Expanded limit (entitlements + usage detail) ---
  @state() private expandedLimitId: string | null = null;
  @state() private entitlements: EntitlementBinding[] = [];
  @state() private entitlementsLoading = false;
  @state() private entitlementsError: string | null = null;
  @state() private usageDetail: UsageByLimitResponse | null = null;

  // --- Create/Edit limit dialog ---
  @state() private showLimitDialog = false;
  @state() private editingLimit: LimitDefinition | null = null;
  @state() private limitForm = { name: '', resourceType: '', unit: '', description: '', defaultValue: 0 };
  @state() private limitDialogError: string | null = null;
  @state() private limitDialogSaving = false;

  // --- Create entitlement dialog ---
  @state() private showEntitlementDialog = false;
  @state() private entitlementForm = { subjectType: 'user', subjectId: '', scopeType: 'system', scopeId: '', value: 0 };
  @state() private entitlementDialogError: string | null = null;
  @state() private entitlementDialogSaving = false;

  // --- Delete confirmation ---
  @state() private showDeleteDialog = false;
  @state() private deleteTarget: { type: 'limit' | 'entitlement'; id: string; name: string } | null = null;
  @state() private deleteLoading = false;
  @state() private deleteDialogError: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-right {
      display: flex;
      align-items: center;
      gap: 1rem;
    }

    .item-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
    }

    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    table {
      width: 100%;
      border-collapse: collapse;
    }

    th {
      text-align: left;
      padding: 0.75rem 1rem;
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      background: var(--scion-bg-subtle, #f1f5f9);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: middle;
    }

    tr:last-child td {
      border-bottom: none;
    }

    tr.clickable {
      cursor: pointer;
    }

    tr.clickable:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    tr.expanded td {
      background: var(--sl-color-primary-50, #eff6ff);
    }

    .type-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .type-badge.agent {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .type-badge.project {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .type-badge.group_member {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .type-badge.default {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .system-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 600;
      background: var(--sl-color-neutral-100, #f1f5f9);
      color: var(--sl-color-neutral-600, #475569);
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .mono {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
    }

    .usage-bar-cell {
      min-width: 140px;
    }

    .usage-info {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .usage-info sl-progress-bar {
      flex: 1;
      min-width: 80px;
      --height: 8px;
    }

    .actions-cell {
      white-space: nowrap;
      text-align: right;
    }

    .actions-cell sl-icon-button::part(base) {
      font-size: 1rem;
      padding: 0.25rem;
    }

    /* Expanded detail panel */
    .detail-panel {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-top: none;
      border-radius: 0 0 var(--scion-radius-lg, 0.75rem) var(--scion-radius-lg, 0.75rem);
      margin-top: -1px;
      margin-bottom: 1rem;
    }

    .detail-section {
      padding: 1rem 1.5rem;
    }

    .detail-section + .detail-section {
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .detail-section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.75rem;
    }

    .detail-section-header h3 {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .entitlement-table {
      width: 100%;
      border-collapse: collapse;
    }

    .entitlement-table th {
      background: transparent;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      padding: 0.5rem 0.75rem;
    }

    .entitlement-table td {
      padding: 0.5rem 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .entitlement-table tr:last-child td {
      border-bottom: none;
    }

    .usage-detail-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
      gap: 0.75rem;
    }

    .usage-card {
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
    }

    .usage-card-header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: 0.5rem;
    }

    .usage-card-subject {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      font-family: var(--scion-font-mono, monospace);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      max-width: 180px;
    }

    .usage-card-count {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
    }

    .usage-card sl-progress-bar {
      --height: 6px;
    }

    /* Dialog form layout */
    .form-row {
      margin-bottom: 1rem;
    }

    .form-row:last-child {
      margin-bottom: 0;
    }

    .form-row sl-input,
    .form-row sl-select,
    .form-row sl-textarea {
      width: 100%;
    }

    .dialog-error {
      margin-bottom: 1rem;
    }

    /* Empty and loading states */
    .empty-state {
      text-align: center;
      padding: 4rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .empty-state > sl-icon {
      font-size: 4rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 1rem;
    }

    .empty-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 3rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 1rem;
    }

    .error-state h2 {
      font-size: 1.25rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .error-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.875rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    .inline-empty {
      text-align: center;
      padding: 1.5rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [limitsRes, usageRes] = await Promise.all([
        apiFetch('/api/v1/admin/limits'),
        apiFetch('/api/v1/admin/usage'),
      ]);

      if (!limitsRes.ok) {
        throw new Error(await extractApiError(limitsRes, `HTTP ${limitsRes.status}`));
      }
      if (!usageRes.ok) {
        throw new Error(await extractApiError(usageRes, `HTTP ${usageRes.status}`));
      }

      const limitsData = (await limitsRes.json()) as { items: LimitDefinition[] };
      const usageData = (await usageRes.json()) as { items: UsageSummaryEntry[] };

      this.limits = limitsData.items || [];

      const summaryMap = new Map<string, number>();
      for (const entry of usageData.items || []) {
        summaryMap.set(entry.limitDefinition.id, entry.activeCount);
      }
      this.usageSummary = summaryMap;
    } catch (err) {
      console.error('Failed to load quotas:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load quotas';
    } finally {
      this.loading = false;
    }
  }

  private async loadEntitlements(limitId: string): Promise<void> {
    this.entitlementsLoading = true;
    this.entitlements = [];
    this.usageDetail = null;
    this.entitlementsError = null;

    try {
      const [entRes, usageRes] = await Promise.all([
        apiFetch(`/api/v1/admin/limits/${limitId}/entitlements`),
        apiFetch(`/api/v1/admin/usage/${limitId}`),
      ]);

      if (entRes.ok) {
        const data = (await entRes.json()) as { items: EntitlementBinding[] };
        this.entitlements = data.items || [];
      } else {
        this.entitlementsError = await extractApiError(entRes, `HTTP ${entRes.status}`);
      }

      if (usageRes.ok) {
        this.usageDetail = (await usageRes.json()) as UsageByLimitResponse;
      } else if (!this.entitlementsError) {
        // Only set if we don't already have an error
        this.entitlementsError = await extractApiError(usageRes, `HTTP ${usageRes.status}`);
      }
    } catch (err) {
      console.error('Failed to load entitlements:', err);
      this.entitlementsError = err instanceof Error ? err.message : 'Failed to load entitlements';
    } finally {
      this.entitlementsLoading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Limit CRUD
  // ---------------------------------------------------------------------------

  private openCreateLimit(): void {
    this.editingLimit = null;
    this.limitForm = { name: '', resourceType: '', unit: '', description: '', defaultValue: 0 };
    this.limitDialogError = null;
    this.showLimitDialog = true;
  }

  private openEditLimit(limit: LimitDefinition, e: Event): void {
    e.stopPropagation();
    this.editingLimit = limit;
    this.limitForm = {
      name: limit.name,
      resourceType: limit.resourceType,
      unit: limit.unit,
      description: limit.description,
      defaultValue: limit.defaultValue,
    };
    this.limitDialogError = null;
    this.showLimitDialog = true;
  }

  private async saveLimitDefinition(): Promise<void> {
    if (!this.limitForm.name.trim()) {
      this.limitDialogError = 'Name is required';
      return;
    }
    if (!this.limitForm.resourceType) {
      this.limitDialogError = 'Resource type is required';
      return;
    }

    this.limitDialogSaving = true;
    this.limitDialogError = null;

    try {
      const body = JSON.stringify({
        name: this.limitForm.name.trim(),
        resourceType: this.limitForm.resourceType,
        unit: this.limitForm.unit.trim(),
        description: this.limitForm.description.trim(),
        defaultValue: Number(this.limitForm.defaultValue) || 0,
      });

      let res: Response;
      if (this.editingLimit) {
        res = await apiFetch(`/api/v1/admin/limits/${this.editingLimit.id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body,
        });
      } else {
        res = await apiFetch('/api/v1/admin/limits', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body,
        });
      }

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this.showLimitDialog = false;
      await this.loadData();
    } catch (err) {
      this.limitDialogError = err instanceof Error ? err.message : 'Failed to save limit';
    } finally {
      this.limitDialogSaving = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Entitlement CRUD
  // ---------------------------------------------------------------------------

  private openCreateEntitlement(): void {
    this.entitlementForm = { subjectType: 'user', subjectId: '', scopeType: 'system', scopeId: '', value: 0 };
    this.entitlementDialogError = null;
    this.showEntitlementDialog = true;
  }

  private async saveEntitlement(): Promise<void> {
    if (!this.entitlementForm.subjectId.trim()) {
      this.entitlementDialogError = 'Subject ID is required';
      return;
    }

    this.entitlementDialogSaving = true;
    this.entitlementDialogError = null;

    try {
      const res = await apiFetch(`/api/v1/admin/limits/${this.expandedLimitId}/entitlements`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          subjectType: this.entitlementForm.subjectType,
          subjectId: this.entitlementForm.subjectId.trim(),
          scopeType: this.entitlementForm.scopeType,
          scopeId: this.entitlementForm.scopeId.trim(),
          value: Number(this.entitlementForm.value) || 0,
        }),
      });

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this.showEntitlementDialog = false;
      if (this.expandedLimitId) {
        await this.loadEntitlements(this.expandedLimitId);
      }
    } catch (err) {
      this.entitlementDialogError = err instanceof Error ? err.message : 'Failed to create entitlement';
    } finally {
      this.entitlementDialogSaving = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Delete
  // ---------------------------------------------------------------------------

  private confirmDeleteLimit(limit: LimitDefinition, e: Event): void {
    e.stopPropagation();
    this.deleteTarget = { type: 'limit', id: limit.id, name: limit.name };
    this.deleteDialogError = null;
    this.showDeleteDialog = true;
  }

  private confirmDeleteEntitlement(binding: EntitlementBinding): void {
    this.deleteTarget = {
      type: 'entitlement',
      id: binding.id,
      name: `${binding.subjectType}:${binding.subjectId}`,
    };
    this.deleteDialogError = null;
    this.showDeleteDialog = true;
  }

  private async executeDelete(): Promise<void> {
    if (!this.deleteTarget) return;

    this.deleteLoading = true;
    this.deleteDialogError = null;

    try {
      let url: string;
      if (this.deleteTarget.type === 'limit') {
        url = `/api/v1/admin/limits/${this.deleteTarget.id}`;
      } else {
        url = `/api/v1/admin/entitlements/${this.deleteTarget.id}`;
      }

      const res = await apiFetch(url, { method: 'DELETE' });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      const wasLimitDelete = this.deleteTarget.type === 'limit';

      // Clear expanded state before reload to avoid referencing deleted limit
      if (wasLimitDelete) {
        this.expandedLimitId = null;
      }

      this.showDeleteDialog = false;
      this.deleteTarget = null;
      this.deleteDialogError = null;

      // Reload appropriate data
      await this.loadData();
      if (!wasLimitDelete && this.expandedLimitId) {
        await this.loadEntitlements(this.expandedLimitId);
      }
    } catch (err) {
      this.deleteDialogError = err instanceof Error ? err.message : 'Failed to delete';
    } finally {
      this.deleteLoading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Row expand/collapse
  // ---------------------------------------------------------------------------

  private async toggleExpand(limitId: string): Promise<void> {
    if (this.expandedLimitId === limitId) {
      this.expandedLimitId = null;
      this.entitlements = [];
      this.usageDetail = null;
      return;
    }

    this.expandedLimitId = limitId;
    await this.loadEntitlements(limitId);
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffMinutes) < 60) {
        return rtf.format(-diffMinutes, 'minute');
      } else if (Math.abs(diffHours) < 24) {
        return rtf.format(-diffHours, 'hour');
      } else {
        return rtf.format(-diffDays, 'day');
      }
    } catch {
      return dateString;
    }
  }

  private resourceTypeBadgeClass(rt: string): string {
    if (rt === 'agent') return 'agent';
    if (rt === 'project') return 'project';
    if (rt === 'group_member') return 'group_member';
    return 'default';
  }

  private formatValue(value: number): string {
    return value === 0 ? 'unlimited' : String(value);
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      <div class="header">
        <h1>Quotas</h1>
        <div class="header-right">
          ${!this.loading && !this.error
            ? html`<span class="item-count">${this.limits.length} limit${this.limits.length !== 1 ? 's' : ''}</span>`
            : ''}
          <sl-button variant="primary" size="small" @click=${this.openCreateLimit}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Create Limit
          </sl-button>
        </div>
      </div>

      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : this.renderLimits()}

      ${this.renderLimitDialog()}
      ${this.renderEntitlementDialog()}
      ${this.renderDeleteDialog()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading quotas...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Quotas</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderLimits() {
    if (this.limits.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="speedometer2"></sl-icon>
          <h2>No Limit Definitions</h2>
          <p>Create a limit definition to start managing quotas.</p>
        </div>
      `;
    }

    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Resource Type</th>
              <th class="hide-mobile">Unit</th>
              <th class="hide-mobile">Default</th>
              <th class="hide-mobile">Usage</th>
              <th class="hide-mobile">Updated</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            ${this.limits.map((limit) => this.renderLimitRow(limit))}
          </tbody>
        </table>
      </div>
      ${this.expandedLimitId
        ? (() => {
            const limit = this.limits.find((l) => l.id === this.expandedLimitId);
            return limit ? this.renderDetailPanel(limit) : nothing;
          })()
        : nothing}
    `;
  }

  private renderLimitRow(limit: LimitDefinition) {
    const activeCount = this.usageSummary.get(limit.id) ?? 0;
    const isExpanded = this.expandedLimitId === limit.id;
    const pct = limit.defaultValue > 0 ? Math.min(100, Math.round((activeCount / limit.defaultValue) * 100)) : 0;

    return html`
      <tr class="clickable ${isExpanded ? 'expanded' : ''}" @click=${() => this.toggleExpand(limit.id)}>
        <td>
          <span style="font-weight: 500">${limit.name}</span>
          ${limit.system ? html`<span class="system-badge" style="margin-left: 0.5rem">system</span>` : nothing}
          ${limit.description ? html`<br><span class="meta-text">${limit.description}</span>` : nothing}
        </td>
        <td>
          <span class="type-badge ${this.resourceTypeBadgeClass(limit.resourceType)}">
            ${limit.resourceType || '—'}
          </span>
        </td>
        <td class="hide-mobile">
          <span class="mono">${limit.unit || '—'}</span>
        </td>
        <td class="hide-mobile">
          <span class="mono">${this.formatValue(limit.defaultValue)}</span>
        </td>
        <td class="hide-mobile usage-bar-cell">
          <div class="usage-info">
            <span>${activeCount}${limit.defaultValue > 0 ? ` / ${limit.defaultValue}` : ''}</span>
            ${limit.defaultValue > 0
              ? html`<sl-progress-bar value=${pct}></sl-progress-bar>`
              : nothing}
          </div>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(limit.updatedAt)}</span>
        </td>
        <td class="actions-cell">
          ${!limit.system ? html`
            <sl-icon-button name="pencil" label="Edit" @click=${(e: Event) => this.openEditLimit(limit, e)}></sl-icon-button>
            <sl-icon-button name="trash" label="Delete" @click=${(e: Event) => this.confirmDeleteLimit(limit, e)}></sl-icon-button>
          ` : nothing}
          <sl-icon-button name=${isExpanded ? 'chevron-up' : 'chevron-down'} label=${isExpanded ? 'Collapse' : 'Expand'}></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderDetailPanel(limit: LimitDefinition) {
    return html`
      <div class="detail-panel">
        <!-- Entitlements section -->
        <div class="detail-section">
          <div class="detail-section-header">
            <h3>Entitlement Bindings</h3>
            <sl-button size="small" @click=${this.openCreateEntitlement}>
              <sl-icon slot="prefix" name="plus-lg"></sl-icon>
              Add Binding
            </sl-button>
          </div>
          ${this.entitlementsLoading
            ? html`<div class="loading-state" style="padding: 1.5rem"><sl-spinner></sl-spinner></div>`
            : this.entitlementsError
              ? html`<sl-alert variant="danger" open class="dialog-error">${this.entitlementsError}</sl-alert>`
              : this.entitlements.length === 0
                ? html`<div class="inline-empty">No entitlement bindings for this limit. The default value (${this.formatValue(limit.defaultValue)}) applies to all subjects.</div>`
                : html`
                <table class="entitlement-table">
                  <thead>
                    <tr>
                      <th>Subject Type</th>
                      <th>Subject ID</th>
                      <th>Scope</th>
                      <th>Value</th>
                      <th class="hide-mobile">Created By</th>
                      <th class="hide-mobile">Created</th>
                      <th></th>
                    </tr>
                  </thead>
                  <tbody>
                    ${this.entitlements.map((b) => html`
                      <tr>
                        <td><span class="type-badge default">${b.subjectType}</span></td>
                        <td><span class="mono">${b.subjectId}</span></td>
                        <td><span class="mono">${b.scopeType}${b.scopeId ? `:${b.scopeId}` : ''}</span></td>
                        <td><span class="mono">${this.formatValue(b.value)}</span></td>
                        <td class="hide-mobile"><span class="meta-text">${b.createdBy || '—'}</span></td>
                        <td class="hide-mobile"><span class="meta-text">${this.formatRelativeTime(b.createdAt)}</span></td>
                        <td class="actions-cell">
                          <sl-icon-button name="trash" label="Delete" @click=${() => this.confirmDeleteEntitlement(b)}></sl-icon-button>
                        </td>
                      </tr>
                    `)}
                  </tbody>
                </table>
              `}
        </div>

        <!-- Usage detail section -->
        <div class="detail-section">
          <div class="detail-section-header">
            <h3>Active Usage</h3>
          </div>
          ${this.entitlementsLoading
            ? html`<div class="loading-state" style="padding: 1.5rem"><sl-spinner></sl-spinner></div>`
            : this.entitlementsError
              ? nothing
              : !this.usageDetail || this.usageDetail.reservations.length === 0
                ? html`<div class="inline-empty">No active usage reservations.</div>`
                : html`
                <div style="margin-bottom: 0.75rem">
                  <span class="meta-text">Total active: <strong>${this.usageDetail.totalActive}</strong></span>
                </div>
                <div class="usage-detail-grid">
                  ${this.usageDetail.reservations.map((r) => html`
                    <div class="usage-card">
                      <div class="usage-card-header">
                        <span class="usage-card-subject" title=${r.subjectId}>${r.subjectId}</span>
                        <span class="usage-card-count">${r.reserved} reserved</span>
                      </div>
                      <div class="meta-text" style="font-size: 0.75rem">
                        Resource: <span class="mono">${r.resourceId}</span>
                      </div>
                    </div>
                  `)}
                </div>
              `}
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Dialogs
  // ---------------------------------------------------------------------------

  private renderLimitDialog() {
    const title = this.editingLimit ? 'Edit Limit Definition' : 'Create Limit Definition';

    return html`
      <sl-dialog
        label=${title}
        ?open=${this.showLimitDialog}
        @sl-hide=${() => { this.showLimitDialog = false; }}
      >
        ${this.limitDialogError
          ? html`<sl-alert variant="danger" open class="dialog-error">${this.limitDialogError}</sl-alert>`
          : nothing}

        <div class="form-row">
          <sl-input
            label="Name"
            placeholder="e.g. max-agents-per-project"
            value=${this.limitForm.name}
            @sl-input=${(e: Event) => { this.limitForm = { ...this.limitForm, name: (e.target as HTMLInputElement).value }; }}
            required
          ></sl-input>
        </div>

        <div class="form-row">
          <sl-select
            label="Resource Type"
            placeholder="Select resource type"
            value=${this.limitForm.resourceType}
            @sl-change=${(e: Event) => { this.limitForm = { ...this.limitForm, resourceType: (e.target as HTMLSelectElement).value }; }}
            required
          >
            <sl-option value="agent">agent</sl-option>
            <sl-option value="project">project</sl-option>
            <sl-option value="group_member">group_member</sl-option>
          </sl-select>
        </div>

        <div class="form-row">
          <sl-input
            label="Unit"
            placeholder="e.g. agents, projects, members"
            value=${this.limitForm.unit}
            @sl-input=${(e: Event) => { this.limitForm = { ...this.limitForm, unit: (e.target as HTMLInputElement).value }; }}
          ></sl-input>
        </div>

        <div class="form-row">
          <sl-input
            label="Default Value"
            type="number"
            min="0"
            placeholder="0 = unlimited"
            value=${String(this.limitForm.defaultValue)}
            @sl-input=${(e: Event) => { this.limitForm = { ...this.limitForm, defaultValue: Number((e.target as HTMLInputElement).value) || 0 }; }}
          ></sl-input>
        </div>

        <div class="form-row">
          <sl-input
            label="Description"
            placeholder="Optional description"
            value=${this.limitForm.description}
            @sl-input=${(e: Event) => { this.limitForm = { ...this.limitForm, description: (e.target as HTMLInputElement).value }; }}
          ></sl-input>
        </div>

        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.limitDialogSaving}
          @click=${this.saveLimitDefinition}
        >
          ${this.editingLimit ? 'Update' : 'Create'}
        </sl-button>
        <sl-button
          slot="footer"
          @click=${() => { this.showLimitDialog = false; }}
        >
          Cancel
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderEntitlementDialog() {
    return html`
      <sl-dialog
        label="Create Entitlement Binding"
        ?open=${this.showEntitlementDialog}
        @sl-hide=${() => { this.showEntitlementDialog = false; }}
      >
        ${this.entitlementDialogError
          ? html`<sl-alert variant="danger" open class="dialog-error">${this.entitlementDialogError}</sl-alert>`
          : nothing}

        <div class="form-row">
          <sl-select
            label="Subject Type"
            value=${this.entitlementForm.subjectType}
            @sl-change=${(e: Event) => { this.entitlementForm = { ...this.entitlementForm, subjectType: (e.target as HTMLSelectElement).value }; }}
          >
            <sl-option value="user">user</sl-option>
            <sl-option value="group">group</sl-option>
            <sl-option value="system_default">system_default</sl-option>
          </sl-select>
        </div>

        <div class="form-row">
          <sl-input
            label="Subject ID"
            placeholder="User or group ID"
            value=${this.entitlementForm.subjectId}
            @sl-input=${(e: Event) => { this.entitlementForm = { ...this.entitlementForm, subjectId: (e.target as HTMLInputElement).value }; }}
            required
          ></sl-input>
        </div>

        <div class="form-row">
          <sl-select
            label="Scope Type"
            value=${this.entitlementForm.scopeType}
            @sl-change=${(e: Event) => { this.entitlementForm = { ...this.entitlementForm, scopeType: (e.target as HTMLSelectElement).value }; }}
          >
            <sl-option value="system">system</sl-option>
            <sl-option value="project">project</sl-option>
          </sl-select>
        </div>

        ${this.entitlementForm.scopeType === 'project' ? html`
          <div class="form-row">
            <sl-input
              label="Scope ID (Project ID)"
              placeholder="Project ID"
              value=${this.entitlementForm.scopeId}
              @sl-input=${(e: Event) => { this.entitlementForm = { ...this.entitlementForm, scopeId: (e.target as HTMLInputElement).value }; }}
            ></sl-input>
          </div>
        ` : nothing}

        <div class="form-row">
          <sl-input
            label="Value"
            type="number"
            min="0"
            placeholder="0 = unlimited"
            value=${String(this.entitlementForm.value)}
            @sl-input=${(e: Event) => { this.entitlementForm = { ...this.entitlementForm, value: Number((e.target as HTMLInputElement).value) || 0 }; }}
          ></sl-input>
        </div>

        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.entitlementDialogSaving}
          @click=${this.saveEntitlement}
        >
          Create
        </sl-button>
        <sl-button
          slot="footer"
          @click=${() => { this.showEntitlementDialog = false; }}
        >
          Cancel
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderDeleteDialog() {
    return html`
      <sl-dialog
        label="Confirm Delete"
        ?open=${this.showDeleteDialog}
        @sl-hide=${() => { this.showDeleteDialog = false; this.deleteTarget = null; this.deleteDialogError = null; }}
      >
        ${this.deleteDialogError
          ? html`<sl-alert variant="danger" open class="dialog-error">${this.deleteDialogError}</sl-alert>`
          : nothing}
        <p>
          Are you sure you want to delete ${this.deleteTarget?.type === 'limit' ? 'limit definition' : 'entitlement binding'}
          <strong>${this.deleteTarget?.name}</strong>? This action cannot be undone.
        </p>
        <sl-button
          slot="footer"
          variant="danger"
          ?loading=${this.deleteLoading}
          @click=${this.executeDelete}
        >
          Delete
        </sl-button>
        <sl-button
          slot="footer"
          @click=${() => { this.showDeleteDialog = false; this.deleteTarget = null; }}
        >
          Cancel
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-quotas': ScionPageAdminQuotas;
  }
}
