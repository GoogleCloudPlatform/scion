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
 * Admin Roles page component
 *
 * CRUD management for role definitions. System roles are read-only;
 * custom roles support create, edit, and delete with CanDelegate enforcement.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface RoleDefinition {
  id: string;
  name: string;
  description: string;
  scopeType: string;
  permissions: string[];
  system: boolean;
  createdAt: string;
  updatedAt: string;
}

interface Permission {
  ID: string;
  Resource: string;
  Action: string;
  Description: string;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-page-admin-roles')
export class ScionPageAdminRoles extends LitElement {
  @state() private loading = true;
  @state() private roles: RoleDefinition[] = [];
  @state() private error: string | null = null;
  @state() private permissions: Permission[] = [];

  // Dialog state
  @state() private showCreateDialog = false;
  @state() private showEditDialog = false;
  @state() private showDeleteDialog = false;
  @state() private editingRole: RoleDefinition | null = null;
  @state() private deletingRole: RoleDefinition | null = null;
  @state() private showViewDialog = false;
  @state() private viewingRole: RoleDefinition | null = null;

  // Form fields
  @state() private formName = '';
  @state() private formDescription = '';
  @state() private formScopeType = 'system';
  @state() private formPermissions: Set<string> = new Set();

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

    .role-count {
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

    .role-name {
      font-weight: 500;
    }

    .role-description {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      max-width: 300px;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .type-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .type-badge.system {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .type-badge.custom {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
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

    .perm-count {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .actions {
      display: flex;
      gap: 0.5rem;
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

    .permissions-section {
      margin-top: 1rem;
    }

    .permissions-section h4 {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .permission-group {
      margin-bottom: 0.75rem;
    }

    .permission-group-title {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.25rem;
      padding-bottom: 0.25rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .permission-item {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      padding: 0.25rem 0;
    }

    .permission-item sl-checkbox {
      flex-shrink: 0;
    }

    .permission-label {
      font-size: 0.8125rem;
      color: var(--scion-text, #1e293b);
    }

    .permission-desc {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .permissions-scroll {
      max-height: 400px;
      overflow-y: auto;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem;
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
      const [rolesRes, permsRes] = await Promise.all([
        apiFetch('/api/v1/admin/roles'),
        apiFetch('/api/v1/admin/permissions'),
      ]);

      if (!rolesRes.ok) {
        throw new Error(await extractApiError(rolesRes, `HTTP ${rolesRes.status}`));
      }
      if (!permsRes.ok) {
        throw new Error(await extractApiError(permsRes, `HTTP ${permsRes.status}`));
      }

      const rolesData = (await rolesRes.json()) as { items: RoleDefinition[] };
      const permsData = (await permsRes.json()) as { items: Permission[] };

      this.roles = rolesData.items || [];
      this.permissions = permsData.items || [];
    } catch (err) {
      console.error('Failed to load roles:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load roles';
    } finally {
      this.loading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  /** Group permissions by resource for the multi-select UI. */
  private groupPermissions(perms: Permission[] = this.permissions): Map<string, Permission[]> {
    const groups = new Map<string, Permission[]>();
    for (const perm of perms) {
      const resource = perm.Resource || 'other';
      if (!groups.has(resource)) {
        groups.set(resource, []);
      }
      groups.get(resource)!.push(perm);
    }
    return groups;
  }

  /** Human-friendly resource label. */
  private resourceLabel(resource: string): string {
    return resource
      .split('_')
      .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
      .join(' ');
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
  // Form management
  // ---------------------------------------------------------------------------

  private openCreateDialog(): void {
    this.formName = '';
    this.formDescription = '';
    this.formScopeType = 'system';
    this.formPermissions = new Set();
    this.showCreateDialog = true;
  }

  private openEditDialog(role: RoleDefinition): void {
    this.editingRole = role;
    this.formName = role.name;
    this.formDescription = role.description;
    this.formScopeType = role.scopeType;
    this.formPermissions = new Set(role.permissions);
    this.showEditDialog = true;
  }

  private openDeleteDialog(role: RoleDefinition): void {
    this.deletingRole = role;
    this.showDeleteDialog = true;
  }

  private openViewDialog(role: RoleDefinition): void {
    this.viewingRole = role;
    this.showViewDialog = true;
  }

  private togglePermission(permId: string): void {
    const next = new Set(this.formPermissions);
    if (next.has(permId)) {
      next.delete(permId);
    } else {
      next.add(permId);
    }
    this.formPermissions = next;
  }

  // ---------------------------------------------------------------------------
  // API actions
  // ---------------------------------------------------------------------------

  private async createRole(): Promise<void> {
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch('/api/v1/admin/roles', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: this.formName.trim(),
          description: this.formDescription.trim(),
          scopeType: this.formScopeType,
          permissions: [...this.formPermissions],
        }),
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      this.showCreateDialog = false;
      this.actionFeedback = { message: `Role "${this.formName}" created`, variant: 'success' };
      void this.loadData();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to create role',
        variant: 'danger',
      };
    } finally {
      this.actionInProgress = false;
    }
  }

  private async updateRole(): Promise<void> {
    if (!this.editingRole) return;
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch(`/api/v1/admin/roles/${this.editingRole.id}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: this.formName.trim(),
          description: this.formDescription.trim(),
          permissions: [...this.formPermissions],
        }),
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      this.showEditDialog = false;
      this.editingRole = null;
      this.actionFeedback = { message: `Role "${this.formName}" updated`, variant: 'success' };
      void this.loadData();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to update role',
        variant: 'danger',
      };
    } finally {
      this.actionInProgress = false;
    }
  }

  private async deleteRole(): Promise<void> {
    if (!this.deletingRole) return;
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch(`/api/v1/admin/roles/${this.deletingRole.id}`, {
        method: 'DELETE',
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      const roleName = this.deletingRole.name;
      this.showDeleteDialog = false;
      this.deletingRole = null;
      this.actionFeedback = { message: `Role "${roleName}" deleted`, variant: 'success' };
      void this.loadData();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to delete role',
        variant: 'danger',
      };
    } finally {
      this.actionInProgress = false;
    }
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
        <h1>Roles</h1>
        <div class="header-right">
          ${!this.loading && !this.error
            ? html`<span class="role-count"
                >${this.roles.length} role${this.roles.length !== 1 ? 's' : ''}</span
              >`
            : ''}
          <sl-button variant="primary" size="small" @click=${() => this.openCreateDialog()}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Create Role
          </sl-button>
        </div>
      </div>

      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderRoles()}
      ${this.renderCreateDialog()} ${this.renderEditDialog()} ${this.renderDeleteDialog()}
      ${this.renderViewPermissionsDialog()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading roles...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Roles</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadData()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderRoles() {
    if (this.roles.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="shield-lock"></sl-icon>
          <h2>No Roles Found</h2>
          <p>Create a custom role to get started.</p>
        </div>
      `;
    }

    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Scope</th>
              <th class="hide-mobile">Permissions</th>
              <th>Type</th>
              <th class="hide-mobile">Updated</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.roles.map((role) => this.renderRoleRow(role))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRoleRow(role: RoleDefinition) {
    return html`
      <tr>
        <td>
          <div class="role-name">${role.name}</div>
          <div class="role-description">${role.description || '—'}</div>
        </td>
        <td><span class="scope-badge">${role.scopeType}</span></td>
        <td class="hide-mobile">
          <span class="perm-count">${role.permissions?.length ?? 0}</span>
        </td>
        <td>
          <span class="type-badge ${role.system ? 'system' : 'custom'}">
            ${role.system ? 'System' : 'Custom'}
          </span>
        </td>
        <td class="hide-mobile">
          <span class="perm-count">${this.formatRelativeTime(role.updatedAt)}</span>
        </td>
        <td>
          <div class="actions">
            <sl-icon-button
              name="eye"
              label="View permissions"
              @click=${() => this.openViewDialog(role)}
            ></sl-icon-button>
            ${role.system
              ? nothing
              : html`
                  <sl-icon-button
                    name="pencil"
                    label="Edit role"
                    @click=${() => this.openEditDialog(role)}
                  ></sl-icon-button>
                  <sl-icon-button
                    name="trash"
                    label="Delete role"
                    @click=${() => this.openDeleteDialog(role)}
                  ></sl-icon-button>
                `}
          </div>
        </td>
      </tr>
    `;
  }

  // ---------------------------------------------------------------------------
  // Permission selector (shared between create and edit)
  // ---------------------------------------------------------------------------

  private renderPermissionSelector() {
    const groups = this.groupPermissions();

    return html`
      <div class="permissions-section">
        <h4>Permissions</h4>
        <div class="permissions-scroll">
          ${[...groups.entries()].map(
            ([resource, perms]) => html`
              <div class="permission-group">
                <div class="permission-group-title">${this.resourceLabel(resource)}</div>
                ${perms.map(
                  (perm) => html`
                    <div class="permission-item">
                      <sl-checkbox
                        ?checked=${this.formPermissions.has(perm.ID)}
                        @sl-change=${() => this.togglePermission(perm.ID)}
                      ></sl-checkbox>
                      <div>
                        <div class="permission-label">${perm.ID}</div>
                        <div class="permission-desc">${perm.Description}</div>
                      </div>
                    </div>
                  `
                )}
              </div>
            `
          )}
        </div>
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
        label="Create Role"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) this.showCreateDialog = false;
        }}
      >
        <div class="form-group">
          <sl-input
            label="Name"
            placeholder="e.g., project-viewer"
            .value=${this.formName}
            @sl-input=${(e: Event) => {
              this.formName = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>
        </div>
        <div class="form-group">
          <sl-input
            label="Description"
            placeholder="A brief description of this role"
            .value=${this.formDescription}
            @sl-input=${(e: Event) => {
              this.formDescription = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
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
        ${this.renderPermissionSelector()}
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
          ?disabled=${!this.formName.trim()}
          @click=${() => this.createRole()}
          >Create Role</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderEditDialog() {
    if (!this.showEditDialog || !this.editingRole) return nothing;

    return html`
      <sl-dialog
        label="Edit Role"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) {
            this.showEditDialog = false;
            this.editingRole = null;
          }
        }}
      >
        <div class="form-group">
          <sl-input
            label="Name"
            .value=${this.formName}
            @sl-input=${(e: Event) => {
              this.formName = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>
        </div>
        <div class="form-group">
          <sl-input
            label="Description"
            .value=${this.formDescription}
            @sl-input=${(e: Event) => {
              this.formDescription = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>
        <div class="form-group">
          <sl-select label="Scope Type" .value=${this.formScopeType} disabled>
            <sl-option value="system">System</sl-option>
            <sl-option value="project">Project</sl-option>
          </sl-select>
        </div>
        ${this.renderPermissionSelector()}
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showEditDialog = false;
            this.editingRole = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.actionInProgress}
          ?disabled=${!this.formName.trim()}
          @click=${() => this.updateRole()}
          >Save Changes</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderDeleteDialog() {
    if (!this.showDeleteDialog || !this.deletingRole) return nothing;

    return html`
      <sl-dialog
        label="Delete Role"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) {
            this.showDeleteDialog = false;
            this.deletingRole = null;
          }
        }}
      >
        <p>
          Are you sure you want to delete the role
          <strong>${this.deletingRole.name}</strong>?
        </p>
        <p class="delete-warning">
          Any active role bindings using this role will also be removed. This action cannot be
          undone.
        </p>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showDeleteDialog = false;
            this.deletingRole = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="danger"
          ?loading=${this.actionInProgress}
          @click=${() => this.deleteRole()}
          >Delete Role</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderViewPermissionsDialog() {
    if (!this.showViewDialog || !this.viewingRole) return nothing;

    const rolePermIds = new Set(this.viewingRole.permissions ?? []);
    const rolePerms = this.permissions.filter((p) => rolePermIds.has(p.ID));

    // Group the filtered permissions by resource (reuses shared helper)
    const groups = this.groupPermissions(rolePerms);

    const permCount = rolePerms.length;
    const resourceCount = groups.size;

    return html`
      <sl-dialog
        label="Permissions: ${this.viewingRole.name}"
        open
        @sl-request-close=${() => {
          this.showViewDialog = false;
          this.viewingRole = null;
        }}
      >
        ${permCount === 0
          ? html`<p>This role has no permissions assigned.</p>`
          : html`
              <div class="permissions-scroll">
                ${[...groups.entries()].map(
                  ([resource, perms]) => html`
                    <div class="permission-group">
                      <div class="permission-group-title">${this.resourceLabel(resource)}</div>
                      ${perms.map(
                        (perm) => html`
                          <div class="permission-item">
                            <div>
                              <div class="permission-label">${perm.ID}</div>
                              <div class="permission-desc">${perm.Description}</div>
                            </div>
                          </div>
                        `
                      )}
                    </div>
                  `
                )}
              </div>
              <p>
                ${permCount} permission${permCount !== 1 ? 's' : ''} across ${resourceCount}
                resource${resourceCount !== 1 ? 's' : ''}
              </p>
            `}
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.showViewDialog = false;
            this.viewingRole = null;
          }}
          >Close</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-roles': ScionPageAdminRoles;
  }
}
