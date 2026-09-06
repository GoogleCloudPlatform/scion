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
 *
 * - Assigned roles: direct/group provenance, active/scheduled/expired
 *   assignment state.
 * - Mutation controls: add/remove direct role bindings with capability
 *   gating (only shown when the current user has the required
 *   permissions).
 *
 * The former "Effective access composition" section was removed because
 * the backend only returns a system-scope activeBindingCount — not a
 * real per-permission composition.  A future standalone Effective
 * Permission Viewer is tracked in .design/effective-permission-viewer.md.
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { showConfirm } from './confirm-dialog.js';
import { getLifecycleStatus, formatDateTime } from './role-binding-utils.js';

import type { AssignmentFormValues } from './role-binding-assignment-form.js';
import './role-binding-assignment-form.js';

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

/** Minimal role definition for the add-binding role selector. */
interface RoleDefinitionSummary {
  id: string;
  name: string;
  scopeType: string;
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

  // ---------------------------------------------------------------------------
  // Mutation state: delete direct bindings / add new binding
  // ---------------------------------------------------------------------------

  /** Whether the current user can create role bindings (role_binding.create). */
  @state() private _canCreate = false;
  /** Whether the current user can delete role bindings (role_binding.delete). */
  @state() private _canDelete = false;
  /** Whether capability pre-check for create/delete has resolved. */
  @state() private _mutationPreChecked = false;
  /** Whether a mutation (delete/create) is currently in progress. */
  @state() private _mutationInProgress = false;
  /** Feedback message after a mutation attempt. */
  @state() private _mutationFeedback: { message: string; variant: 'success' | 'danger' } | null =
    null;

  // Add-binding dialog state
  @state() private _showAddDialog = false;
  @state() private _addRoles: RoleDefinitionSummary[] = [];
  @state() private _addRoleId = '';
  @state() private _addScopeType = 'system';
  @state() private _addScopeId = '';

  static override styles = [
    srOnlyStyles,
    css`
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

      .role-name {
        overflow-wrap: anywhere;
      }

      .scope-tag {
        overflow-wrap: anywhere;
      }

      /* Delete icon on role cards */
      .role-card-actions {
        display: flex;
        align-items: center;
        gap: 0.25rem;
        margin-left: 0.5rem;
      }

      .role-card-actions sl-icon-button::part(base) {
        color: var(--sl-color-danger-600, #dc2626);
        padding: 0.125rem;
      }

      .role-card-actions sl-icon-button::part(base):hover {
        color: var(--sl-color-danger-700, #b91c1c);
      }

      /* Header with add button */
      .header-actions {
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      /* Mutation feedback */
      .mutation-feedback {
        font-size: 0.8125rem;
        padding: 0.5rem 0.75rem;
        border-radius: var(--scion-radius, 0.5rem);
        margin-bottom: 0.75rem;
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .mutation-feedback.success {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .mutation-feedback.danger {
        background: var(--sl-color-danger-100, #fee2e2);
        color: var(--sl-color-danger-700, #b91c1c);
      }

      /* Add binding dialog styles are in the shared
         scion-role-binding-assignment-form component. */

      @media (forced-colors: active) {
        .section {
          border-color: ButtonText;
        }

        .role-card {
          border-color: ButtonText;
        }

        .status-badge {
          border: 1px solid ButtonText;
        }

        .error-state {
          border: 1px solid ButtonText;
        }
      }
    `,
  ];

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
    if ((changed.has('principalId') || changed.has('principalType')) && this.principalId) {
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
    this._mutationFeedback = null;

    // Pre-click capability gate: check create/delete authorization concurrently
    // with binding load so action buttons only appear for authorized users.
    void this.preCheckMutationAccess();

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
          throw new Error(await extractApiError(fallbackRes, `HTTP ${fallbackRes.status}`));
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
      this.error = err instanceof Error ? err.message : 'Failed to load effective roles';
    } finally {
      this.loading = false;
    }
  }

  /**
   * Pre-check whether the current user can create and/or delete role bindings.
   *
   * role_binding.create and role_binding.delete are independent permissions
   * in the backend (permissions/registry.go). Custom roles can grant one
   * without the other, so each action is probed separately:
   *
   *   - POST /api/v1/admin/role-bindings with empty body:
   *       400 → authorized (role_binding.create); 403 → not.
   *   - DELETE /api/v1/admin/role-bindings/00000000-0000-0000-0000-000000000000:
   *       404 → authorized (role_binding.delete, binding not found);
   *       403 → not.
   *
   * Both probes run concurrently. Buttons remain hidden until both resolve
   * (_mutationPreChecked). Toast noise from 403 responses is suppressed
   * since these are expected authorization probes.
   */
  private async preCheckMutationAccess(): Promise<void> {
    if (this._mutationPreChecked) return;

    const probeCreate = async (): Promise<boolean> => {
      try {
        const res = await apiFetch('/api/v1/admin/role-bindings', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({}),
          suppressAccessDeniedToast: true,
        });
        // 400 = authorized but invalid body; 403 = not authorized.
        return res.status !== 403;
      } catch {
        // Network error — default to visible, server will reject if needed.
        return true;
      }
    };

    const probeDelete = async (): Promise<boolean> => {
      try {
        // Probe with a well-formed but nonexistent UUID. The backend checks
        // role_binding.delete authorization before looking up the binding, so
        // 404 means authorized (binding not found) and 403 means not.
        const sentinelId = '00000000-0000-0000-0000-000000000000';
        const res = await apiFetch(`/api/v1/admin/role-bindings/${sentinelId}`, {
          method: 'DELETE',
          suppressAccessDeniedToast: true,
        });
        return res.status !== 403;
      } catch {
        return true;
      }
    };

    try {
      const [canCreate, canDelete] = await Promise.all([probeCreate(), probeDelete()]);
      this._canCreate = canCreate;
      this._canDelete = canDelete;
    } catch {
      // Fallback: show both, server is authoritative.
      this._canCreate = true;
      this._canDelete = true;
    } finally {
      this._mutationPreChecked = true;
    }
  }

  // ---------------------------------------------------------------------------
  // Mutation actions: delete binding / add binding
  // ---------------------------------------------------------------------------

  private async deleteBinding(binding: EffectiveRoleBinding): Promise<void> {
    const roleName = binding.roleName || binding.roleDefinitionId;
    const confirmed = await showConfirm(
      `Remove the "${roleName}" role assignment from this ${this.principalType}?`,
      {
        title: 'Remove Role Assignment',
        confirmText: 'Remove Assignment',
        variant: 'danger',
      }
    );
    if (!confirmed) return;

    this._mutationInProgress = true;
    this._mutationFeedback = null;
    try {
      const res = await apiFetch(`/api/v1/admin/role-bindings/${binding.id}`, {
        method: 'DELETE',
      });
      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this._mutationFeedback = { message: msg, variant: 'danger' };
        return;
      }
      this._mutationFeedback = { message: `Removed "${roleName}" assignment`, variant: 'success' };
      // Refresh the binding list
      void this.loadEffectiveRoles();
    } catch (err) {
      this._mutationFeedback = {
        message: err instanceof Error ? err.message : 'Failed to remove binding',
        variant: 'danger',
      };
    } finally {
      this._mutationInProgress = false;
    }
  }

  private async openAddDialog(): Promise<void> {
    // Load available roles if not yet loaded
    if (this._addRoles.length === 0) {
      try {
        const res = await apiFetch('/api/v1/admin/roles');
        if (res.ok) {
          const data = (await res.json()) as { items?: RoleDefinitionSummary[] };
          this._addRoles = data.items || [];
        }
      } catch {
        // Roles will remain empty — the select will show "No roles available"
      }
    }
    this._addRoleId = '';
    this._addScopeType = 'system';
    this._addScopeId = '';
    this._showAddDialog = true;

    // Reset the shared form after it renders
    await this.updateComplete;
    const form = this.shadowRoot?.querySelector('scion-role-binding-assignment-form');
    if (form) {
      (form as import('./role-binding-assignment-form.js').ScionRoleBindingAssignmentForm).reset();
    }
  }

  private get _addFormValid(): boolean {
    if (!this._addRoleId) return false;
    if (this._addScopeType === 'project' && !this._addScopeId) return false;
    return true;
  }

  private async createBinding(): Promise<void> {
    this._mutationInProgress = true;
    this._mutationFeedback = null;
    try {
      const body: Record<string, string> = {
        roleDefinitionId: this._addRoleId,
        principalType: this.principalType,
        principalId: this.principalId,
        scopeType: this._addScopeType,
      };
      if (this._addScopeType === 'project' && this._addScopeId) {
        body.scopeId = this._addScopeId;
      }

      const res = await apiFetch('/api/v1/admin/role-bindings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this._mutationFeedback = { message: msg, variant: 'danger' };
        return;
      }

      const roleName =
        this._addRoles.find((r) => r.id === this._addRoleId)?.name || this._addRoleId;
      this._mutationFeedback = { message: `Assigned "${roleName}" role`, variant: 'success' };
      this._showAddDialog = false;
      // Refresh the binding list
      void this.loadEffectiveRoles();
    } catch (err) {
      this._mutationFeedback = {
        message: err instanceof Error ? err.message : 'Failed to assign role',
        variant: 'danger',
      };
    } finally {
      this._mutationInProgress = false;
    }
  }

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
        ${this.renderAddButton()}
      </div>
      ${this.renderMutationFeedback()} ${this.renderContent()} ${this.renderAddDialog()}
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
          ${this.renderAddButton()}
        </div>
        ${this.renderMutationFeedback()} ${this.renderContent()} ${this.renderAddDialog()}
      </div>
    `;
  }

  private renderAddButton() {
    if (!this._mutationPreChecked || !this._canCreate) return nothing;
    return html`
      <sl-button
        size="small"
        variant="primary"
        @click=${() => this.openAddDialog()}
        ?disabled=${this._mutationInProgress}
      >
        <sl-icon slot="prefix" name="plus-lg"></sl-icon>
        Add Binding
      </sl-button>
    `;
  }

  private renderMutationFeedback() {
    if (!this._mutationFeedback) return nothing;
    return html`
      <div class="mutation-feedback ${this._mutationFeedback.variant}" role="status">
        <sl-icon
          name=${this._mutationFeedback.variant === 'success'
            ? 'check-circle'
            : 'exclamation-triangle'}
        ></sl-icon>
        ${this._mutationFeedback.message}
      </div>
    `;
  }

  private renderContent() {
    if (this.loading) {
      return html`
        <div class="loading-state"><sl-spinner></sl-spinner> Loading effective roles...</div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state" role="alert">
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => this.loadEffectiveRoles()}> Retry </sl-button>
        </div>
      `;
    }

    if (this.bindings.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="shield"></sl-icon>
          <h3>No Roles Assigned</h3>
          <p>This ${this.principalType} does not have any role assignments.</p>
        </div>
      `;
    }

    return html`
      <div class="role-list">${this.bindings.map((binding) => this.renderRoleCard(binding))}</div>
    `;
  }

  private renderRoleCard(binding: EffectiveRoleBinding) {
    const status = getLifecycleStatus(binding);
    const isDirect = binding.source === 'direct';

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
          <div class="provenance ${isDirect ? 'direct' : 'group'}">
            ${isDirect
              ? html`<sl-icon name="person-check"></sl-icon> Direct`
              : html`<sl-icon name="diagram-3"></sl-icon> Via group:
                  ${binding.sourceGroupName || binding.source}`}
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
            ${status === 'active' ? 'Active' : status === 'expired' ? 'Expired' : 'Scheduled'}
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
        ${isDirect && this._canDelete && this._mutationPreChecked
          ? html`
              <div class="role-card-actions">
                <sl-icon-button
                  name="trash"
                  label="Remove this direct role assignment"
                  ?disabled=${this._mutationInProgress}
                  @click=${() => this.deleteBinding(binding)}
                ></sl-icon-button>
              </div>
            `
          : nothing}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Add Binding Dialog
  // ---------------------------------------------------------------------------

  private renderAddDialog() {
    if (!this._showAddDialog) return nothing;

    return html`
      <sl-dialog
        label="Assign Role"
        open
        @sl-request-close=${() => {
          if (!this._mutationInProgress) this._showAddDialog = false;
        }}
      >
        <scion-role-binding-assignment-form
          .roles=${this._addRoles}
          .lockedPrincipalType=${this.principalType}
          .lockedPrincipalId=${this.principalId}
          ?disabled=${this._mutationInProgress}
          @form-change=${(e: CustomEvent<AssignmentFormValues>) => {
            this._addRoleId = e.detail.roleId;
            this._addScopeType = e.detail.scopeType;
            this._addScopeId = e.detail.scopeId;
          }}
        ></scion-role-binding-assignment-form>

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this._mutationInProgress}
          @click=${() => {
            this._showAddDialog = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this._mutationInProgress}
          ?disabled=${!this._addFormValid}
          @click=${() => this.createBinding()}
          >Assign Role</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-effective-role-provenance': ScionEffectiveRoleProvenance;
  }
}
