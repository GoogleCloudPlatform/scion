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
 * Admin Role Bindings page component
 *
 * List, create, and delete role bindings. Supports pagination and
 * CanDelegate enforcement via server-side 403 handling.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface RoleBinding {
  id: string;
  roleDefinitionId: string;
  principalType: string;
  principalId: string;
  scopeType: string;
  scopeId: string;
  createdBy: string;
  createdAt: string;
}

interface RoleDefinition {
  id: string;
  name: string;
  scopeType: string;
  system: boolean;
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const PAGE_SIZE = 25;

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-page-admin-role-bindings')
export class ScionPageAdminRoleBindings extends LitElement {
  @state() private loading = true;
  @state() private bindings: RoleBinding[] = [];
  @state() private roles: RoleDefinition[] = [];
  @state() private totalCount = 0;
  @state() private currentPage = 1;
  @state() private error: string | null = null;

  // Role name lookup cache
  @state() private roleNameMap: Record<string, string> = {};

  // Dialog state
  @state() private showCreateDialog = false;
  @state() private showDeleteDialog = false;
  @state() private deletingBinding: RoleBinding | null = null;

  // Form fields
  @state() private formPrincipalType = 'user';
  @state() private formPrincipalId = '';
  @state() private formRoleId = '';
  @state() private formScopeType = 'system';
  @state() private formScopeId = '';

  // Action state
  @state() private actionInProgress = false;
  @state() private actionFeedback: { message: string; variant: 'success' | 'danger' } | null = null;

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

    .binding-count {
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

    .principal-info {
      display: flex;
      flex-direction: column;
    }

    .principal-id {
      font-weight: 500;
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
    }

    .principal-type {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .scope-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .scope-id {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .pagination {
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 1rem;
      padding: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .pagination-info {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

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

    /* Dialog form styles */
    .form-group {
      margin-bottom: 1rem;
    }

    .form-group:last-child {
      margin-bottom: 0;
    }

    .feedback-alert {
      margin-bottom: 1rem;
    }

    .delete-warning {
      color: var(--sl-color-danger-600, #dc2626);
      font-weight: 500;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

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
      const offset = (this.currentPage - 1) * PAGE_SIZE;
      const [bindingsRes, rolesRes] = await Promise.all([
        apiFetch(`/api/v1/admin/role-bindings?limit=${PAGE_SIZE}&offset=${offset}`),
        apiFetch('/api/v1/admin/roles'),
      ]);

      if (!bindingsRes.ok) {
        throw new Error(await extractApiError(bindingsRes, `HTTP ${bindingsRes.status}`));
      }
      if (!rolesRes.ok) {
        throw new Error(await extractApiError(rolesRes, `HTTP ${rolesRes.status}`));
      }

      const bindingsData = (await bindingsRes.json()) as {
        items: RoleBinding[];
        totalCount: number;
      };
      const rolesData = (await rolesRes.json()) as { items: RoleDefinition[] };

      this.bindings = bindingsData.items || [];
      this.totalCount = bindingsData.totalCount || 0;
      this.roles = rolesData.items || [];

      // Build role name lookup
      const map: Record<string, string> = {};
      for (const role of this.roles) {
        map[role.id] = role.name;
      }
      this.roleNameMap = map;
    } catch (err) {
      console.error('Failed to load role bindings:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load role bindings';
    } finally {
      this.loading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  private getRoleName(roleDefinitionId: string): string {
    return this.roleNameMap[roleDefinitionId] || roleDefinitionId;
  }

  private get totalPages(): number {
    return Math.max(1, Math.ceil(this.totalCount / PAGE_SIZE));
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) return rtf.format(-diffSeconds, 'second');
      if (Math.abs(diffMinutes) < 60) return rtf.format(-diffMinutes, 'minute');
      if (Math.abs(diffHours) < 24) return rtf.format(-diffHours, 'hour');
      return rtf.format(-diffDays, 'day');
    } catch {
      return dateString;
    }
  }

  // ---------------------------------------------------------------------------
  // Pagination
  // ---------------------------------------------------------------------------

  private goToPage(page: number): void {
    if (page < 1 || page > this.totalPages) return;
    this.currentPage = page;
    void this.loadData();
  }

  // ---------------------------------------------------------------------------
  // Form management
  // ---------------------------------------------------------------------------

  private openCreateDialog(): void {
    this.formPrincipalType = 'user';
    this.formPrincipalId = '';
    this.formRoleId = this.roles.length > 0 ? this.roles[0].id : '';
    this.formScopeType = 'system';
    this.formScopeId = '';
    this.showCreateDialog = true;
  }

  private openDeleteDialog(binding: RoleBinding): void {
    this.deletingBinding = binding;
    this.showDeleteDialog = true;
  }

  // ---------------------------------------------------------------------------
  // API actions
  // ---------------------------------------------------------------------------

  private async createBinding(): Promise<void> {
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch('/api/v1/admin/role-bindings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          roleDefinitionId: this.formRoleId,
          principalType: this.formPrincipalType,
          principalId: this.formPrincipalId.trim(),
          scopeType: this.formScopeType,
          scopeId: this.formScopeType === 'project' ? this.formScopeId.trim() : '',
        }),
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      this.showCreateDialog = false;
      this.actionFeedback = { message: 'Role binding created', variant: 'success' };
      void this.loadData();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to create binding',
        variant: 'danger',
      };
    } finally {
      this.actionInProgress = false;
    }
  }

  private async deleteBinding(): Promise<void> {
    if (!this.deletingBinding) return;
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch(`/api/v1/admin/role-bindings/${this.deletingBinding.id}`, {
        method: 'DELETE',
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      this.showDeleteDialog = false;
      this.deletingBinding = null;
      this.actionFeedback = { message: 'Role binding deleted', variant: 'success' };
      void this.loadData();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to delete binding',
        variant: 'danger',
      };
    } finally {
      this.actionInProgress = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Validation
  // ---------------------------------------------------------------------------

  private get createFormValid(): boolean {
    if (!this.formPrincipalId.trim()) return false;
    if (!this.formRoleId) return false;
    if (this.formScopeType === 'project' && !this.formScopeId.trim()) return false;
    return true;
  }

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    return html`
      ${this.actionFeedback
        ? html`
            <sl-alert
              class="feedback-alert"
              variant=${this.actionFeedback.variant}
              open
              closable
              duration="5000"
              @sl-after-hide=${() => {
                this.actionFeedback = null;
              }}
            >
              <sl-icon
                slot="icon"
                name=${this.actionFeedback.variant === 'success'
                  ? 'check-circle'
                  : 'exclamation-triangle'}
              ></sl-icon>
              ${this.actionFeedback.message}
            </sl-alert>
          `
        : ''}

      <div class="header">
        <h1>Role Bindings</h1>
        <div class="header-right">
          ${!this.loading && !this.error
            ? html`<span class="binding-count"
                >${this.totalCount} binding${this.totalCount !== 1 ? 's' : ''}</span
              >`
            : ''}
          <sl-button variant="primary" size="small" @click=${() => this.openCreateDialog()}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Create Binding
          </sl-button>
        </div>
      </div>

      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : this.renderBindings()}
      ${this.renderCreateDialog()} ${this.renderDeleteDialog()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading role bindings...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Role Bindings</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderBindings() {
    if (this.bindings.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="link-45deg"></sl-icon>
          <h2>No Role Bindings Found</h2>
          <p>Create a role binding to assign roles to users.</p>
        </div>
      `;
    }

    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Principal</th>
              <th>Role</th>
              <th>Scope</th>
              <th class="hide-mobile">Scope ID</th>
              <th class="hide-mobile">Created</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.bindings.map((binding) => this.renderBindingRow(binding))}
          </tbody>
        </table>
        ${this.totalPages > 1 ? this.renderPagination() : ''}
      </div>
    `;
  }

  private renderBindingRow(binding: RoleBinding) {
    return html`
      <tr>
        <td>
          <div class="principal-info">
            <span class="principal-id">${binding.principalId}</span>
            <span class="principal-type">${binding.principalType}</span>
          </div>
        </td>
        <td>${this.getRoleName(binding.roleDefinitionId)}</td>
        <td><span class="scope-badge">${binding.scopeType}</span></td>
        <td class="hide-mobile">
          ${binding.scopeId
            ? html`<span class="scope-id">${binding.scopeId}</span>`
            : html`<span class="meta-text">—</span>`}
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(binding.createdAt)}</span>
        </td>
        <td>
          <sl-icon-button
            name="trash"
            label="Delete binding"
            @click=${() => this.openDeleteDialog(binding)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderPagination() {
    const start = (this.currentPage - 1) * PAGE_SIZE + 1;
    const end = Math.min(this.currentPage * PAGE_SIZE, this.totalCount);

    return html`
      <div class="pagination">
        <sl-button
          variant="default"
          size="small"
          ?disabled=${this.currentPage <= 1}
          @click=${() => this.goToPage(this.currentPage - 1)}
        >
          <sl-icon name="chevron-left"></sl-icon>
        </sl-button>
        <span class="pagination-info"> ${start}–${end} of ${this.totalCount} </span>
        <sl-button
          variant="default"
          size="small"
          ?disabled=${this.currentPage >= this.totalPages}
          @click=${() => this.goToPage(this.currentPage + 1)}
        >
          <sl-icon name="chevron-right"></sl-icon>
        </sl-button>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Dialogs
  // ---------------------------------------------------------------------------

  private renderCreateDialog() {
    if (!this.showCreateDialog) return nothing;

    return html`
      <sl-dialog
        label="Create Role Binding"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) this.showCreateDialog = false;
        }}
      >
        <div class="form-group">
          <sl-select
            label="Principal Type"
            .value=${this.formPrincipalType}
            @sl-change=${(e: Event) => {
              this.formPrincipalType = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="user">User</sl-option>
            <sl-option value="agent">Agent</sl-option>
          </sl-select>
        </div>
        <div class="form-group">
          <sl-input
            label="Principal ID"
            placeholder="User or agent ID"
            .value=${this.formPrincipalId}
            @sl-input=${(e: Event) => {
              this.formPrincipalId = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>
        </div>
        <div class="form-group">
          <sl-select
            label="Role"
            .value=${this.formRoleId}
            @sl-change=${(e: Event) => {
              this.formRoleId = (e.target as HTMLSelectElement).value;
            }}
          >
            ${this.roles.map(
              (role) => html` <sl-option value=${role.id}>${role.name}</sl-option> `
            )}
          </sl-select>
        </div>
        <div class="form-group">
          <sl-select
            label="Scope Type"
            .value=${this.formScopeType}
            @sl-change=${(e: Event) => {
              this.formScopeType = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="system">System</sl-option>
            <sl-option value="project">Project</sl-option>
          </sl-select>
        </div>
        ${this.formScopeType === 'project'
          ? html`
              <div class="form-group">
                <sl-input
                  label="Scope ID (Project ID)"
                  placeholder="Project ID"
                  .value=${this.formScopeId}
                  @sl-input=${(e: Event) => {
                    this.formScopeId = (e.target as HTMLInputElement).value;
                  }}
                  required
                ></sl-input>
              </div>
            `
          : ''}
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showCreateDialog = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.actionInProgress}
          ?disabled=${!this.createFormValid}
          @click=${() => this.createBinding()}
          >Create Binding</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderDeleteDialog() {
    if (!this.showDeleteDialog || !this.deletingBinding) return nothing;

    return html`
      <sl-dialog
        label="Delete Role Binding"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) {
            this.showDeleteDialog = false;
            this.deletingBinding = null;
          }
        }}
      >
        <p>
          Are you sure you want to delete this role binding for
          <strong>${this.deletingBinding.principalId}</strong>
          (${this.getRoleName(this.deletingBinding.roleDefinitionId)})?
        </p>
        <p class="delete-warning">This action cannot be undone.</p>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showDeleteDialog = false;
            this.deletingBinding = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="danger"
          ?loading=${this.actionInProgress}
          @click=${() => this.deleteBinding()}
          >Delete Binding</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-role-bindings': ScionPageAdminRoleBindings;
  }
}
