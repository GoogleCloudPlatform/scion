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
 * Effective Role Provenance Display
 *
 * Shows a principal's effective roles with provenance:
 * - Direct: assigned directly to this user/agent
 * - Via group: inherited through group membership
 *
 * Displays lifecycle status (active/expired/scheduled) and scope.
 * Used on user and agent detail pages.
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { getLifecycleStatus, formatDateTime } from './role-binding-utils.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface EffectiveRoleBinding {
  id: string;
  roleDefinitionId: string;
  roleName: string;
  principalType: string;
  principalId: string;
  principalDisplayName?: string;
  scopeType: string;
  scopeId: string;
  scopeDisplayName?: string;
  createdAt: string;
  notBefore?: string;
  expiresAt?: string;
  /** How the binding was obtained: 'direct' or the group that grants it. */
  source: 'direct' | string;
  /** When source is not 'direct', this holds the group display name. */
  sourceGroupName?: string;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-effective-role-provenance')
export class ScionEffectiveRoleProvenance extends LitElement {
  /** The principal type: 'user' or 'agent'. */
  @property() principalType: 'user' | 'agent' = 'user';

  /** The principal's ID. */
  @property() principalId = '';

  /** Whether to render in compact card layout. */
  @property({ type: Boolean }) compact = false;

  /** Section title override. */
  @property() sectionTitle = 'Effective Roles';

  @state() private loading = true;
  @state() private bindings: EffectiveRoleBinding[] = [];
  @state() private error: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .section-header h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .role-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      font-weight: 400;
      margin-left: 0.5rem;
    }

    .standalone-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .standalone-header h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    /* Role cards list */
    .role-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .role-card {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      padding: 0.75rem 1rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      gap: 1rem;
    }

    .role-card:hover {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .role-card-left {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
      min-width: 0;
      flex: 1;
    }

    .role-name {
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .role-scope {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .scope-tag {
      display: inline-flex;
      align-items: center;
      padding: 0.0625rem 0.375rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    /* Provenance badge */
    .provenance {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
    }

    .provenance.direct {
      color: var(--sl-color-primary-600, #2563eb);
    }

    .provenance.group {
      color: var(--sl-color-warning-600, #d97706);
    }

    .provenance sl-icon {
      font-size: 0.75rem;
    }

    /* Lifecycle status badge */
    .role-card-right {
      display: flex;
      flex-direction: column;
      align-items: flex-end;
      gap: 0.25rem;
      flex-shrink: 0;
    }

    .status-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 500;
    }

    .status-badge.active {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.expired {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .status-badge.pending {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #b45309);
    }

    .lifecycle-info {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Empty state */
    .empty-state {
      text-align: center;
      padding: 2rem 1.5rem;
    }

    .empty-state sl-icon {
      font-size: 2.5rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.4;
      margin-bottom: 0.75rem;
    }

    .empty-state h3 {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    /* Loading / Error */
    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      gap: 0.75rem;
      font-size: 0.875rem;
    }

    .error-state {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }

    @media (max-width: 768px) {
      .role-card {
        flex-direction: column;
        gap: 0.5rem;
      }

      .role-card-right {
        align-items: flex-start;
      }
    }
  `;

  /** Guard to prevent double-fetch when connectedCallback and updated both fire. */
  private _initialLoadDone = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.principalId) {
      this._initialLoadDone = true;
      void this.loadEffectiveRoles();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (
      (changed.has('principalId') || changed.has('principalType')) &&
      this.principalId
    ) {
      // Skip if connectedCallback already triggered the initial load.
      if (!this._initialLoadDone) {
        void this.loadEffectiveRoles();
      }
      this._initialLoadDone = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadEffectiveRoles(): Promise<void> {
    if (!this.principalId) return;

    this.loading = true;
    this.error = null;

    try {
      // Fetch role bindings for this principal (direct and group-derived)
      const url = `/api/v1/admin/role-bindings?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(this.principalId)}&includeGroupDerived=true`;
      const res = await apiFetch(url);

      if (!res.ok) {
        // Fall back to fetching just the direct bindings without the
        // includeGroupDerived parameter (which may not be supported yet).
        const fallbackUrl = `/api/v1/admin/role-bindings?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(this.principalId)}`;
        const fallbackRes = await apiFetch(fallbackUrl);

        if (!fallbackRes.ok) {
          throw new Error(
            await extractApiError(fallbackRes, `HTTP ${fallbackRes.status}`)
          );
        }

        const data = (await fallbackRes.json()) as {
          items?: EffectiveRoleBinding[];
        };
        this.bindings = (data.items || []).map((b) => ({
          ...b,
          source: b.source || 'direct',
        }));
      } else {
        const data = (await res.json()) as {
          items?: EffectiveRoleBinding[];
        };
        this.bindings = (data.items || []).map((b) => ({
          ...b,
          source: b.source || 'direct',
        }));
      }
    } catch (err) {
      console.error('Failed to load effective roles:', err);
      this.error =
        err instanceof Error ? err.message : 'Failed to load effective roles';
    } finally {
      this.loading = false;
    }
  }

  // getLifecycleStatus and formatDateTime are imported from ./role-binding-utils.js

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderStandalone();
  }

  private renderStandalone() {
    return html`
      <div class="standalone-header">
        <h2>
          ${this.sectionTitle}
          <span class="role-count">(${this.bindings.length})</span>
        </h2>
      </div>
      ${this.renderContent()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section">
        <div class="section-header">
          <h2>
            ${this.sectionTitle}
            <span class="role-count">(${this.bindings.length})</span>
          </h2>
        </div>
        ${this.renderContent()}
      </div>
    `;
  }

  private renderContent() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner> Loading effective roles...
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => this.loadEffectiveRoles()}>
            Retry
          </sl-button>
        </div>
      `;
    }

    if (this.bindings.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="shield"></sl-icon>
          <h3>No Roles Assigned</h3>
          <p>
            This ${this.principalType} does not have any role assignments.
          </p>
        </div>
      `;
    }

    return html`
      <div class="role-list">
        ${this.bindings.map((binding) => this.renderRoleCard(binding))}
      </div>
    `;
  }

  private renderRoleCard(binding: EffectiveRoleBinding) {
    const status = getLifecycleStatus(binding);

    return html`
      <div class="role-card">
        <div class="role-card-left">
          <span class="role-name">${binding.roleName || binding.roleDefinitionId}</span>
          <div class="role-scope">
            <span class="scope-tag">${binding.scopeType}</span>
            ${binding.scopeId
              ? html`<span>${binding.scopeDisplayName || binding.scopeId}</span>`
              : ''}
          </div>
          <div class="provenance ${binding.source === 'direct' ? 'direct' : 'group'}">
            ${binding.source === 'direct'
              ? html`<sl-icon name="person-check"></sl-icon> Direct`
              : html`<sl-icon name="diagram-3"></sl-icon> Via group: ${binding.sourceGroupName || binding.source}`}
          </div>
        </div>
        <div class="role-card-right">
          <span class="status-badge ${status}">
            <sl-icon
              name=${status === 'active'
                ? 'check-circle'
                : status === 'expired'
                  ? 'x-circle'
                  : 'clock'}
            ></sl-icon>
            ${status === 'active'
              ? 'Active'
              : status === 'expired'
                ? 'Expired'
                : 'Scheduled'}
          </span>
          ${binding.expiresAt && status !== 'expired'
            ? html`<span class="lifecycle-info">
                Expires ${formatDateTime(binding.expiresAt)}
              </span>`
            : ''}
          ${binding.notBefore && status === 'pending'
            ? html`<span class="lifecycle-info">
                Activates ${formatDateTime(binding.notBefore)}
              </span>`
            : ''}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-effective-role-provenance': ScionEffectiveRoleProvenance;
  }
}
