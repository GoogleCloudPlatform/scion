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
 * Admin Role Detail page component
 *
 * Shows role metadata (name, description, scope, type) with two tabs:
 *   - Permissions: grouped permission list (read-only view for system roles)
 *   - Bindings: role bindings filtered to this role, with delete and add actions
 *
 * Capability/permission gating:
 *   - Edit button: visible for custom (non-system) roles only
 *   - Delete button: visible for custom (non-system) roles only
 *   - Add Binding: visible when user has role_binding.create
 *   - Delete binding: available per-row
 *
 * Route: /admin/roles/:id
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { navigateTo } from '../../client/main.js';
import { setDocumentTitle } from '../../client/page-title.js';
import {
  getPrincipalIcon,
  formatDateTime,
} from '../shared/role-binding-utils.js';

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

interface RoleBinding {
  id: string;
  roleDefinitionId: string;
  principalType: string;
  principalId: string;
  principalDisplayName?: string;
  scopeType: string;
  scopeId: string;
  scopeDisplayName?: string;
  createdBy: string;
  createdByDisplayName?: string;
  createdAt: string;
  notBefore?: string;
  expiresAt?: string;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-page-admin-role-detail')
export class ScionPageAdminRoleDetail extends LitElement {
  // Core state
  @state() private roleId = '';
  @state() private roleData: RoleDefinition | null = null;
  @state() private loading = true;
  @state() private error: string | null = null;

  // Permissions tab
  @state() private allPermissions: Permission[] = [];

  // Bindings tab
  @state() private bindings: RoleBinding[] = [];
  @state() private bindingsLoading = false;
  @state() private bindingsError: string | null = null;

  // Tab management
  @state() private activeTab: 'permissions' | 'bindings' = 'permissions';

  // Dialog state
  @state() private showEditDialog = false;
  @state() private showDeleteDialog = false;
  @state() private showDeleteBindingDialog = false;
  @state() private deletingBinding: RoleBinding | null = null;

  // Form fields (edit dialog)
  @state() private formName = '';
  @state() private formDescription = '';
  @state() private formPermissions: Set<string> = new Set();

  // Action state
  @state() private actionInProgress = false;
  @state() private actionFeedback: { message: string; variant: 'success' | 'danger' } | null =
    null;

  // Add binding form
  @state() private showAddBindingForm = false;
  @state() private addBindingPrincipalType = 'user';
  @state() private addBindingPrincipalId = '';
  @state() private addBindingScopeType = 'system';
  @state() private addBindingScopeId = '';

  static override styles = css`
    :host {
      display: block;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      gap: 1rem;
    }

    .header-info {
      flex: 1;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .header-description {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

    .header-actions {
      display: flex;
      gap: 0.5rem;
      flex-shrink: 0;
    }

    .badges {
      display: flex;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
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

    .metadata-row {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    /* Tabs */
    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-panel {
      padding-top: 1rem;
    }

    /* Permissions tab */
    .permissions-section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .permission-group {
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .permission-group:last-child {
      border-bottom: none;
    }

    .permission-group-title {
      font-size: 0.75rem;
      font-weight: 600;
      text-transform: uppercase;
      letter-spacing: 0.05em;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.5rem;
      padding-bottom: 0.25rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .permission-item {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      padding: 0.375rem 0;
    }

    .permission-label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .permission-desc {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .permission-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.75rem;
    }

    /* Bindings tab */
    .bindings-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
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

    .principal-cell {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .principal-name {
      font-weight: 500;
    }

    .principal-id {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Empty / loading / error states */
    .empty-state {
      text-align: center;
      padding: 3rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state sl-icon {
      font-size: 3rem;
      opacity: 0.5;
      margin-bottom: 0.75rem;
    }

    .empty-state h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
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
      font-size: 1.125rem;
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

    .feedback-alert {
      margin-bottom: 1rem;
    }

    .delete-warning {
      color: var(--sl-color-danger-600, #dc2626);
      font-weight: 500;
    }

    /* Edit dialog permissions */
    .permissions-scroll {
      max-height: 400px;
      overflow-y: auto;
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem;
    }

    .form-group {
      margin-bottom: 1rem;
    }

    .form-group:last-child {
      margin-bottom: 0;
    }

    .permissions-edit-section h4 {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    /* Add binding form */
    .add-binding-form {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.25rem;
      margin-bottom: 1rem;
    }

    .add-binding-form h3 {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1rem 0;
    }

    .add-binding-fields {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 1rem;
      margin-bottom: 1rem;
    }

    .add-binding-actions {
      display: flex;
      gap: 0.5rem;
      justify-content: flex-end;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }

      .header {
        flex-direction: column;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();

    const match = window.location.pathname.match(/\/admin\/roles\/([^/]+)/);
    if (match) {
      this.roleId = decodeURIComponent(match[1]);
    }
    setDocumentTitle('Role');
    void this.loadRole();
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadRole(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [roleRes, permsRes] = await Promise.all([
        apiFetch(`/api/v1/admin/roles/${encodeURIComponent(this.roleId)}`),
        apiFetch('/api/v1/admin/permissions'),
      ]);

      if (!roleRes.ok) {
        if (roleRes.status === 404) {
          this.error = 'not_found';
          return;
        }
        throw new Error(await extractApiError(roleRes, `HTTP ${roleRes.status}`));
      }
      if (!permsRes.ok) {
        throw new Error(await extractApiError(permsRes, `HTTP ${permsRes.status}`));
      }

      this.roleData = (await roleRes.json()) as RoleDefinition;
      const permsData = (await permsRes.json()) as { items: Permission[] };
      this.allPermissions = permsData.items || [];

      setDocumentTitle(`Role: ${this.roleData.name}`);

      // Auto-load bindings for the Bindings tab
      void this.loadBindings();
    } catch (err) {
      console.error('Failed to load role:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load role';
    } finally {
      this.loading = false;
    }
  }

  private async loadBindings(): Promise<void> {
    this.bindingsLoading = true;
    this.bindingsError = null;

    try {
      // Fetch all bindings and filter client-side by roleDefinitionId.
      // Backend does not currently support a roleId filter parameter.
      const res = await apiFetch('/api/v1/admin/role-bindings?limit=500&offset=0');
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      const data = (await res.json()) as { items: RoleBinding[] };
      this.bindings = (data.items || []).filter((b) => b.roleDefinitionId === this.roleId);
    } catch (err) {
      console.error('Failed to load bindings:', err);
      this.bindingsError = err instanceof Error ? err.message : 'Failed to load bindings';
    } finally {
      this.bindingsLoading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  private groupPermissions(permIds: string[]): Map<string, Permission[]> {
    const idSet = new Set(permIds);
    const rolePerms = this.allPermissions.filter((p) => idSet.has(p.ID));
    const groups = new Map<string, Permission[]>();
    for (const perm of rolePerms) {
      const resource = perm.Resource || 'other';
      if (!groups.has(resource)) {
        groups.set(resource, []);
      }
      groups.get(resource)!.push(perm);
    }
    return groups;
  }

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
  // Dialog management
  // ---------------------------------------------------------------------------

  private openEditDialog(): void {
    if (!this.roleData || this.roleData.system) return;
    this.formName = this.roleData.name;
    this.formDescription = this.roleData.description;
    this.formPermissions = new Set(this.roleData.permissions);
    this.showEditDialog = true;
  }

  private openDeleteDialog(): void {
    if (!this.roleData || this.roleData.system) return;
    this.showDeleteDialog = true;
  }

  private openDeleteBindingDialog(binding: RoleBinding): void {
    this.deletingBinding = binding;
    this.showDeleteBindingDialog = true;
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

  private async updateRole(): Promise<void> {
    if (!this.roleData) return;
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch(`/api/v1/admin/roles/${encodeURIComponent(this.roleData.id)}`, {
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
      this.actionFeedback = { message: `Role "${this.formName}" updated`, variant: 'success' };
      void this.loadRole();
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
    if (!this.roleData) return;
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch(`/api/v1/admin/roles/${encodeURIComponent(this.roleData.id)}`, {
        method: 'DELETE',
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      // Navigate back to roles list after successful delete
      navigateTo('/admin/roles');
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to delete role',
        variant: 'danger',
      };
      this.actionInProgress = false;
    }
  }

  private async deleteBinding(): Promise<void> {
    if (!this.deletingBinding) return;
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch(
        `/api/v1/admin/role-bindings/${encodeURIComponent(this.deletingBinding.id)}`,
        { method: 'DELETE' }
      );

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      const name =
        this.deletingBinding.principalDisplayName || this.deletingBinding.principalId;
      this.showDeleteBindingDialog = false;
      this.deletingBinding = null;
      this.actionFeedback = { message: `Binding for "${name}" deleted`, variant: 'success' };
      void this.loadBindings();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to delete binding',
        variant: 'danger',
      };
    } finally {
      this.actionInProgress = false;
    }
  }

  private async createBinding(): Promise<void> {
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const res = await apiFetch('/api/v1/admin/role-bindings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          roleDefinitionId: this.roleId,
          principalType: this.addBindingPrincipalType,
          principalId: this.addBindingPrincipalId.trim(),
          scopeType: this.addBindingScopeType,
          scopeId: this.addBindingScopeType === 'project' ? this.addBindingScopeId.trim() : '',
        }),
      });

      if (!res.ok) {
        const msg = await extractApiError(res, `HTTP ${res.status}`);
        this.actionFeedback = { message: msg, variant: 'danger' };
        return;
      }

      this.showAddBindingForm = false;
      this.addBindingPrincipalId = '';
      this.addBindingScopeId = '';
      this.actionFeedback = { message: 'Binding created', variant: 'success' };
      void this.loadBindings();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to create binding',
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
      ${this.renderFeedback()}
      <a
        class="back-link"
        href="/admin/roles"
        @click=${(e: Event) => {
          e.preventDefault();
          navigateTo('/admin/roles');
        }}
      >
        <sl-icon name="arrow-left"></sl-icon>
        Roles
      </a>

      ${this.loading
        ? this.renderLoading()
        : this.error === 'not_found'
          ? this.renderNotFound()
          : this.error
            ? this.renderError()
            : this.renderDetail()}
      ${this.renderEditDialog()} ${this.renderDeleteDialog()}
      ${this.renderDeleteBindingDialog()}
    `;
  }

  private renderFeedback() {
    if (!this.actionFeedback) return nothing;
    return html`
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
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state" role="status" aria-label="Loading role">
        <sl-spinner></sl-spinner>
        <p>Loading role...</p>
      </div>
    `;
  }

  private renderNotFound() {
    return html`
      <div class="empty-state">
        <sl-icon name="shield-lock"></sl-icon>
        <h2>Role Not Found</h2>
        <p>
          The role "${this.roleId}" does not exist or you do not have permission to view it.
        </p>
        <sl-button
          variant="primary"
          style="margin-top: 1rem"
          @click=${() => navigateTo('/admin/roles')}
        >
          Back to Roles
        </sl-button>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state" role="alert">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Role</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadRole()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderDetail() {
    const role = this.roleData;
    if (!role) return nothing;

    return html`
      <div class="header">
        <div class="header-info">
          <div class="badges">
            <span class="type-badge ${role.system ? 'system' : 'custom'}">
              ${role.system ? 'System' : 'Custom'}
            </span>
            <span class="scope-badge">${role.scopeType}</span>
          </div>
          <h1>${role.name}</h1>
          ${role.description
            ? html`<p class="header-description">${role.description}</p>`
            : nothing}
          <div class="metadata-row">
            Updated ${this.formatRelativeTime(role.updatedAt)} · Created
            ${this.formatRelativeTime(role.createdAt)}
          </div>
        </div>
        <div class="header-actions">
          ${role.system
            ? nothing
            : html`
                <sl-button variant="default" size="small" @click=${() => this.openEditDialog()}>
                  <sl-icon slot="prefix" name="pencil"></sl-icon>
                  Edit
                </sl-button>
                <sl-button variant="danger" size="small" outline @click=${() => this.openDeleteDialog()}>
                  <sl-icon slot="prefix" name="trash"></sl-icon>
                  Delete
                </sl-button>
              `}
        </div>
      </div>

      <sl-tab-group @sl-tab-show=${(e: CustomEvent) => this.handleTabChange(e)}>
        <sl-tab slot="nav" panel="permissions" ?active=${this.activeTab === 'permissions'}>
          Permissions (${role.permissions?.length ?? 0})
        </sl-tab>
        <sl-tab slot="nav" panel="bindings" ?active=${this.activeTab === 'bindings'}>
          Bindings (${this.bindings.length})
        </sl-tab>

        <sl-tab-panel name="permissions">${this.renderPermissionsTab()}</sl-tab-panel>
        <sl-tab-panel name="bindings">${this.renderBindingsTab()}</sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private handleTabChange(e: CustomEvent): void {
    const panel = (e.detail as { name: string }).name;
    if (panel === 'permissions' || panel === 'bindings') {
      this.activeTab = panel;
    }
  }

  // ---------------------------------------------------------------------------
  // Permissions tab
  // ---------------------------------------------------------------------------

  private renderPermissionsTab() {
    const role = this.roleData;
    if (!role) return nothing;

    const groups = this.groupPermissions(role.permissions ?? []);
    const permCount = role.permissions?.length ?? 0;

    if (permCount === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="shield-lock"></sl-icon>
          <h2>No Permissions</h2>
          <p>This role has no permissions assigned.</p>
        </div>
      `;
    }

    return html`
      <div class="permissions-section">
        ${[...groups.entries()].map(
          ([resource, perms]) => html`
            <div class="permission-group">
              <div class="permission-group-title">${this.resourceLabel(resource)}</div>
              ${perms.map(
                (perm) => html`
                  <div class="permission-item">
                    <sl-icon name="check-lg" style="color: var(--sl-color-success-600, #16a34a); flex-shrink: 0; margin-top: 2px;"></sl-icon>
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
      <div class="permission-count">
        ${permCount} permission${permCount !== 1 ? 's' : ''} across ${groups.size}
        resource${groups.size !== 1 ? 's' : ''}
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Bindings tab
  // ---------------------------------------------------------------------------

  private renderBindingsTab() {
    return html`
      <div class="bindings-header">
        <span class="binding-count">
          ${this.bindingsLoading
            ? 'Loading...'
            : `${this.bindings.length} binding${this.bindings.length !== 1 ? 's' : ''}`}
        </span>
        <sl-button
          variant="primary"
          size="small"
          @click=${() => {
            this.showAddBindingForm = !this.showAddBindingForm;
          }}
        >
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Binding
        </sl-button>
      </div>

      ${this.showAddBindingForm ? this.renderAddBindingForm() : nothing}

      ${this.bindingsLoading
        ? html`
            <div class="loading-state" role="status">
              <sl-spinner></sl-spinner>
              <p>Loading bindings...</p>
            </div>
          `
        : this.bindingsError
          ? html`
              <div class="error-state" role="alert">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <h2>Failed to Load Bindings</h2>
                <div class="error-details">${this.bindingsError}</div>
                <sl-button variant="primary" @click=${() => this.loadBindings()}>
                  <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                  Retry
                </sl-button>
              </div>
            `
          : this.bindings.length === 0
            ? html`
                <div class="empty-state">
                  <sl-icon name="link-45deg"></sl-icon>
                  <h2>No Bindings</h2>
                  <p>No principals are bound to this role. Use "Add Binding" to assign it.</p>
                </div>
              `
            : this.renderBindingsTable()}
    `;
  }

  private renderBindingsTable() {
    return html`
      <div class="table-container">
        <table aria-label="Role bindings">
          <thead>
            <tr>
              <th>Principal</th>
              <th>Scope</th>
              <th class="hide-mobile">Created By</th>
              <th class="hide-mobile">Created</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.bindings.map((b) => this.renderBindingRow(b))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderBindingRow(binding: RoleBinding) {
    const principalName = binding.principalDisplayName || binding.principalId;
    const scopeLabel =
      binding.scopeType === 'system'
        ? 'System'
        : binding.scopeDisplayName || binding.scopeId || binding.scopeType;
    const createdByLabel = binding.createdByDisplayName || binding.createdBy || '—';

    return html`
      <tr>
        <td>
          <div class="principal-cell">
            <sl-icon name=${getPrincipalIcon(binding.principalType)}></sl-icon>
            <div>
              <div class="principal-name">${principalName}</div>
              ${binding.principalDisplayName
                ? html`<div class="principal-id">${binding.principalId}</div>`
                : nothing}
            </div>
          </div>
        </td>
        <td><span class="scope-badge">${scopeLabel}</span></td>
        <td class="hide-mobile">${createdByLabel}</td>
        <td class="hide-mobile">${formatDateTime(binding.createdAt)}</td>
        <td>
          <sl-icon-button
            name="trash"
            label="Delete binding"
            @click=${() => this.openDeleteBindingDialog(binding)}
          ></sl-icon-button>
        </td>
      </tr>
    `;
  }

  private renderAddBindingForm() {
    const scopeType = this.roleData?.scopeType ?? 'system';
    return html`
      <div class="add-binding-form">
        <h3>Add Binding</h3>
        <div class="add-binding-fields">
          <sl-select
            label="Principal Type"
            value=${this.addBindingPrincipalType}
            @sl-change=${(e: Event) => {
              this.addBindingPrincipalType = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="user">User</sl-option>
            <sl-option value="agent">Agent</sl-option>
            <sl-option value="group">Group</sl-option>
          </sl-select>

          <sl-input
            label="Principal ID"
            placeholder="Enter user/agent/group ID"
            value=${this.addBindingPrincipalId}
            @sl-input=${(e: Event) => {
              this.addBindingPrincipalId = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>

          ${scopeType === 'project'
            ? html`
                <sl-input
                  label="Project ID"
                  placeholder="Enter project ID"
                  value=${this.addBindingScopeId}
                  @sl-input=${(e: Event) => {
                    this.addBindingScopeId = (e.target as HTMLInputElement).value;
                  }}
                  required
                ></sl-input>
              `
            : nothing}
        </div>

        <div class="add-binding-actions">
          <sl-button
            variant="default"
            size="small"
            @click=${() => {
              this.showAddBindingForm = false;
            }}
            >Cancel</sl-button
          >
          <sl-button
            variant="primary"
            size="small"
            ?loading=${this.actionInProgress}
            ?disabled=${!this.addBindingPrincipalId.trim()}
            @click=${() => this.createBinding()}
            >Create Binding</sl-button
          >
        </div>
      </div>
    `;
  }

  // ---------------------------------------------------------------------------
  // Dialogs
  // ---------------------------------------------------------------------------

  private renderEditDialog() {
    if (!this.showEditDialog || !this.roleData) return nothing;

    const groups = new Map<string, Permission[]>();
    for (const perm of this.allPermissions) {
      const resource = perm.Resource || 'other';
      if (!groups.has(resource)) {
        groups.set(resource, []);
      }
      groups.get(resource)!.push(perm);
    }

    return html`
      <sl-dialog
        label="Edit Role"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) this.showEditDialog = false;
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
        <div class="permissions-edit-section">
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
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showEditDialog = false;
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
    if (!this.showDeleteDialog || !this.roleData) return nothing;

    return html`
      <sl-dialog
        label="Delete Role"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) this.showDeleteDialog = false;
        }}
      >
        <p>
          Are you sure you want to delete the role
          <strong>${this.roleData.name}</strong>?
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

  private renderDeleteBindingDialog() {
    if (!this.showDeleteBindingDialog || !this.deletingBinding) return nothing;

    const name =
      this.deletingBinding.principalDisplayName || this.deletingBinding.principalId;

    return html`
      <sl-dialog
        label="Delete Binding"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) {
            this.showDeleteBindingDialog = false;
            this.deletingBinding = null;
          }
        }}
      >
        <p>
          Are you sure you want to remove the binding for
          <strong>${name}</strong>?
        </p>
        <p class="delete-warning">
          This principal will lose the permissions granted by this role binding.
        </p>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showDeleteBindingDialog = false;
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
    'scion-page-admin-role-detail': ScionPageAdminRoleDetail;
  }
}
