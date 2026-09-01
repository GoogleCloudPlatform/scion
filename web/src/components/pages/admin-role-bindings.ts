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
 * Unified role assignment workflow. Supports:
 * - Principal selection (user/agent/group) with autocomplete
 * - Role selection filtered by scope type
 * - Assignment lifecycle (NotBefore/ExpiresAt) via Advanced section
 * - Group principal validation (prevents super-admin/project-owner for groups)
 * - Pagination, create, delete with CanDelegate enforcement via server-side 403
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import type { PrincipalChangeDetail } from '../shared/principal-picker.js';
import type { SecurityReviewDetail } from '../shared/security-review-dialog.js';
import {
  parseSecurityReviewResponse,
  parseLockoutResponse,
} from '../shared/security-review-dialog.js';
import '../shared/principal-picker.js';
import '../shared/security-review-dialog.js';
import {
  SYSTEM_DIRECT_USER_ONLY_ROLES,
  getLifecycleStatus,
  formatDateTime,
  getPrincipalIcon,
} from '../shared/role-binding-utils.js';
import '../shared/effective-access-boundary-notice.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

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
  // Role scope type lookup
  @state() private roleScopeMap: Record<string, string> = {};

  // Access boundary notice
  @state() private activeBoundaryCount = 0;

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

  // Advanced lifecycle fields
  @state() private showAdvanced = false;
  @state() private formNotBefore = '';
  @state() private formExpiresAt = '';

  // Action state
  @state() private actionInProgress = false;
  @state() private actionFeedback: { message: string; variant: 'success' | 'danger' } | null = null;

  // Validation warning (e.g. group assigned to direct-user-only role)
  @state() private formValidationWarning = '';

  // Security review dialog state
  @state() private securityReviewDetail: SecurityReviewDetail | null = null;
  @state() private showSecurityReview = false;

  // Atomic replace binding state
  @state() private showReplaceDialog = false;
  @state() private replacingBinding: RoleBinding | null = null;
  @state() private replaceRoleId = '';

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1.5rem;
      flex-wrap: wrap;
      gap: 0.75rem;
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
      align-items: center;
      gap: 0.5rem;
    }

    .principal-icon {
      width: 1.75rem;
      height: 1.75rem;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }

    .principal-icon.user {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-600, #2563eb);
    }

    .principal-icon.group {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-600, #d97706);
    }

    .principal-icon.agent {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-600, #16a34a);
    }

    .principal-icon sl-icon {
      font-size: 0.75rem;
    }

    .principal-details {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .principal-name {
      font-weight: 500;
      font-size: 0.875rem;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .principal-type-label {
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

    /* Lifecycle status badges */
    .lifecycle-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.6875rem;
      font-weight: 500;
    }

    .lifecycle-badge.active {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .lifecycle-badge.expired {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .lifecycle-badge.pending {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #b45309);
    }

    .lifecycle-detail {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
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

    .validation-warning {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-warning-50, #fffbeb);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-warning-700, #b45309);
      font-size: 0.8125rem;
      margin-bottom: 1rem;
    }

    .validation-warning sl-icon {
      flex-shrink: 0;
    }

    /* Advanced section */
    .advanced-toggle {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      cursor: pointer;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0.5rem 0;
      border: none;
      background: none;
      width: 100%;
      text-align: left;
    }

    .advanced-toggle:hover {
      color: var(--scion-text, #1e293b);
    }

    .advanced-toggle sl-icon {
      transition: transform 0.2s ease;
    }

    .advanced-toggle.open sl-icon {
      transform: rotate(90deg);
    }

    .advanced-content {
      padding: 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 1rem;
    }

    .advanced-content .form-group {
      margin-bottom: 0.75rem;
    }

    .advanced-content .form-group:last-child {
      margin-bottom: 0;
    }

    .lifecycle-hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .lifecycle-indicator {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.75rem;
      color: var(--sl-color-primary-600, #2563eb);
      margin-top: 0.25rem;
    }

    .lifecycle-indicator sl-icon {
      font-size: 0.75rem;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }

      .header h1 {
        font-size: 1.25rem;
      }

      th,
      td {
        padding: 0.5rem 0.75rem;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
    void this.loadBoundaryCount();
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

      // Build role name and scope lookups
      const nameMap: Record<string, string> = {};
      const scopeMap: Record<string, string> = {};
      for (const role of this.roles) {
        nameMap[role.id] = role.name;
        scopeMap[role.id] = role.scopeType;
      }
      this.roleNameMap = nameMap;
      this.roleScopeMap = scopeMap;
    } catch (err) {
      console.error('Failed to load role bindings:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load role bindings';
    } finally {
      this.loading = false;
    }
  }

  /**
   * Fetch active access boundary count. Non-critical — failure is silently
   * ignored and the notice simply does not appear.
   */
  private async loadBoundaryCount(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/admin/access-constraints?status=active&pageSize=0');
      if (res.ok) {
        const data = (await res.json()) as { totalCount?: number };
        this.activeBoundaryCount = data.totalCount ?? 0;
      }
    } catch {
      // Silently ignore — boundary notice is non-critical
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

  // formatDateTime, getLifecycleStatus, and getPrincipalIcon are imported
  // from ../shared/role-binding-utils.js

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
    this.formRoleId = '';
    this.formScopeType = 'system';
    this.formScopeId = '';
    this.showAdvanced = false;
    this.formNotBefore = '';
    this.formExpiresAt = '';
    this.formValidationWarning = '';
    this.showCreateDialog = true;
  }

  private openDeleteDialog(binding: RoleBinding): void {
    this.deletingBinding = binding;
    this.showDeleteDialog = true;
  }

  /**
   * Roles available for the current principal type, filtered by scope type.
   * Group principals cannot be assigned direct-user-only roles.
   */
  private get filteredRoles(): RoleDefinition[] {
    let filtered = this.roles;

    // Filter by scope type compatibility
    if (this.formScopeType === 'system') {
      filtered = filtered.filter((r) => r.scopeType === 'system');
    } else if (this.formScopeType === 'project') {
      filtered = filtered.filter((r) => r.scopeType === 'project');
    }

    // Groups cannot be assigned to direct-user-only roles
    if (this.formPrincipalType === 'group') {
      filtered = filtered.filter((r) => !SYSTEM_DIRECT_USER_ONLY_ROLES.includes(r.name));
    }

    return filtered;
  }

  /**
   * Update validation state when principal type or role changes.
   */
  private updateValidation(): void {
    if (this.formPrincipalType === 'group' && this.formRoleId) {
      const roleName = this.roleNameMap[this.formRoleId];
      if (roleName && SYSTEM_DIRECT_USER_ONLY_ROLES.includes(roleName)) {
        this.formValidationWarning = `"${roleName}" can only be assigned to individual users, not groups.`;
        this.formRoleId = '';
        return;
      }
    }
    this.formValidationWarning = '';
  }

  // ---------------------------------------------------------------------------
  // API actions
  // ---------------------------------------------------------------------------

  private async createBinding(): Promise<void> {
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      const body: Record<string, string> = {
        roleDefinitionId: this.formRoleId,
        principalType: this.formPrincipalType,
        principalId: this.formPrincipalId.trim(),
        scopeType: this.formScopeType,
        scopeId: this.formScopeType === 'project' ? this.formScopeId.trim() : '',
      };

      // Include lifecycle fields only when set
      if (this.formNotBefore) {
        body.notBefore = new Date(this.formNotBefore).toISOString();
      }
      if (this.formExpiresAt) {
        body.expiresAt = new Date(this.formExpiresAt).toISOString();
      }

      const res = await apiFetch('/api/v1/admin/role-bindings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
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
        // Check for security review or lockout responses
        const errorBody = (await res.json().catch(() => null)) as Record<string, unknown> | null;
        if (errorBody) {
          const binding = this.deletingBinding;
          const principal = binding.principalDisplayName || binding.principalId;
          const role = this.getRoleName(binding.roleDefinitionId);

          const lockout = parseLockoutResponse(errorBody);
          if (lockout) {
            this.showDeleteDialog = false;
            this.securityReviewDetail = {
              entityLabel: `${role} binding for ${principal}`,
              contextLabel:
                binding.scopeType === 'project'
                  ? `project ${binding.scopeDisplayName || binding.scopeId}`
                  : 'system',
              boundaries: [],
              canCommit: false,
              lockout,
            };
            this.showSecurityReview = true;
            return;
          }

          const reviewDetail = parseSecurityReviewResponse(
            errorBody,
            `${role} binding for ${principal}`,
            binding.scopeType === 'project'
              ? `project ${binding.scopeDisplayName || binding.scopeId}`
              : 'system'
          );
          if (reviewDetail) {
            this.showDeleteDialog = false;
            this.securityReviewDetail = reviewDetail;
            this.showSecurityReview = true;
            return;
          }

          const msg = (errorBody.error as Record<string, unknown>)?.message as string | undefined;
          this.actionFeedback = { message: msg ?? `HTTP ${res.status}`, variant: 'danger' };
          return;
        }

        // Body already consumed by res.json() above — use direct fallback
        this.actionFeedback = { message: `HTTP ${res.status}`, variant: 'danger' };
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

  /**
   * Open the replace binding dialog for atomic role replacement.
   */
  private openReplaceDialog(binding: RoleBinding): void {
    this.replacingBinding = binding;
    this.replaceRoleId = '';
    this.showReplaceDialog = true;
  }

  /**
   * Atomically replace one role binding with another using a single transaction.
   * Uses the B5 atomic endpoint when available, falling back to create-then-delete.
   */
  private async replaceBinding(): Promise<void> {
    if (!this.replacingBinding || !this.replaceRoleId) return;
    this.actionInProgress = true;
    this.actionFeedback = null;
    try {
      // Attempt atomic replacement via B5 endpoint
      const body = {
        existingBindingId: this.replacingBinding.id,
        newRoleDefinitionId: this.replaceRoleId,
        principalType: this.replacingBinding.principalType,
        principalId: this.replacingBinding.principalId,
        scopeType: this.replacingBinding.scopeType,
        scopeId: this.replacingBinding.scopeId,
      };

      const res = await apiFetch('/api/v1/admin/role-bindings:replace', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (res.status === 404 || res.status === 405) {
        // Atomic endpoint not available — do not attempt non-atomic create-then-delete
        this.actionFeedback = {
          message:
            'Atomic role replacement is not available. Please delete the existing binding first, then create a new one.',
          variant: 'danger',
        };
        return;
      }

      if (!res.ok) {
        // Check for security review
        const errorBody = (await res.json().catch(() => null)) as Record<string, unknown> | null;
        if (errorBody) {
          const binding = this.replacingBinding;
          const principal = binding.principalDisplayName || binding.principalId;
          const oldRole = this.getRoleName(binding.roleDefinitionId);
          const newRole = this.getRoleName(this.replaceRoleId);

          const lockout = parseLockoutResponse(errorBody);
          if (lockout) {
            this.showReplaceDialog = false;
            this.securityReviewDetail = {
              entityLabel: `${oldRole} → ${newRole} for ${principal}`,
              contextLabel:
                binding.scopeType === 'project'
                  ? `project ${binding.scopeDisplayName || binding.scopeId}`
                  : 'system',
              boundaries: [],
              canCommit: false,
              lockout,
            };
            this.showSecurityReview = true;
            return;
          }

          const reviewDetail = parseSecurityReviewResponse(
            errorBody,
            `${oldRole} → ${newRole} for ${principal}`,
            binding.scopeType === 'project'
              ? `project ${binding.scopeDisplayName || binding.scopeId}`
              : 'system'
          );
          if (reviewDetail) {
            this.showReplaceDialog = false;
            this.securityReviewDetail = reviewDetail;
            this.showSecurityReview = true;
            return;
          }

          const msg = (errorBody.error as Record<string, unknown>)?.message as string | undefined;
          this.actionFeedback = { message: msg ?? `HTTP ${res.status}`, variant: 'danger' };
          return;
        }

        // Body already consumed by res.json() above — use direct fallback
        this.actionFeedback = { message: `HTTP ${res.status}`, variant: 'danger' };
        return;
      }

      this.showReplaceDialog = false;
      this.replacingBinding = null;
      this.actionFeedback = { message: 'Role binding replaced', variant: 'success' };
      void this.loadData();
    } catch (err) {
      this.actionFeedback = {
        message: err instanceof Error ? err.message : 'Failed to replace binding',
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

    // Block group principals for direct-user-only roles
    if (this.formPrincipalType === 'group' && this.formRoleId) {
      const roleName = this.roleNameMap[this.formRoleId];
      if (roleName && SYSTEM_DIRECT_USER_ONLY_ROLES.includes(roleName)) return false;
    }

    // Validate lifecycle dates: expiresAt must be after notBefore
    if (this.formNotBefore && this.formExpiresAt) {
      const nb = new Date(this.formNotBefore).getTime();
      const ea = new Date(this.formExpiresAt).getTime();
      if (!isNaN(nb) && !isNaN(ea) && ea <= nb) return false;
    }

    return true;
  }

  /** True when any lifecycle condition is set. */
  private get hasLifecycleConditions(): boolean {
    return !!(this.formNotBefore || this.formExpiresAt);
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
            Assign Role
          </sl-button>
        </div>
      </div>

      ${this.renderBoundaryNotice()}
      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : this.renderBindings()}
      ${this.renderCreateDialog()} ${this.renderDeleteDialog()} ${this.renderReplaceDialog()}
      <scion-security-review-dialog
        ?open=${this.showSecurityReview}
        .detail=${this.securityReviewDetail}
        @security-review-cancel=${() => {
          this.showSecurityReview = false;
          this.securityReviewDetail = null;
        }}
      ></scion-security-review-dialog>
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

  private renderBoundaryNotice() {
    if (this.activeBoundaryCount <= 0) return nothing;

    return html`
      <div
        style="display: flex; align-items: center; gap: 0.5rem; padding: 0.5rem 0.75rem; background: var(--sl-color-neutral-50, #f8fafc); border: 1px solid var(--scion-border, #e2e8f0); border-radius: var(--scion-radius, 0.5rem); font-size: 0.8125rem; color: var(--scion-text-muted, #64748b); margin-bottom: 1rem;"
      >
        <sl-icon
          name="shield-exclamation"
          style="font-size: 0.875rem; color: var(--sl-color-warning-500, #f59e0b); flex-shrink: 0;"
        ></sl-icon>
        <span style="flex: 1;">
          Effective access may be reduced by ${this.activeBoundaryCount} access
          ${this.activeBoundaryCount === 1 ? 'boundary' : 'boundaries'}
        </span>
        <a
          href="/admin/access-boundaries"
          style="color: var(--sl-color-primary-600, #2563eb); text-decoration: none; font-weight: 500; white-space: nowrap;"
          >View boundaries</a
        >
      </div>
    `;
  }

  private renderBindings() {
    if (this.bindings.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="link-45deg"></sl-icon>
          <h2>No Role Bindings Found</h2>
          <p>Assign a role to grant access to users, agents, or groups.</p>
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
              <th class="hide-mobile">Status</th>
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
    const lifecycleStatus = getLifecycleStatus(binding);
    const hasLifecycle = !!(binding.notBefore || binding.expiresAt);

    return html`
      <tr>
        <td>
          <div class="principal-info">
            <div class="principal-icon ${binding.principalType}">
              <sl-icon name="${getPrincipalIcon(binding.principalType)}"></sl-icon>
            </div>
            <div class="principal-details">
              <span class="principal-name"
                >${binding.principalDisplayName || binding.principalId}</span
              >
              <span class="principal-type-label">${binding.principalType}</span>
            </div>
          </div>
        </td>
        <td>${this.getRoleName(binding.roleDefinitionId)}</td>
        <td>
          <span class="scope-badge">${binding.scopeType}</span>
          ${binding.scopeId
            ? html`<br /><span class="scope-id"
                  >${binding.scopeDisplayName || binding.scopeId}</span
                >`
            : ''}
        </td>
        <td class="hide-mobile">
          ${hasLifecycle
            ? html`
                <span class="lifecycle-badge ${lifecycleStatus}">
                  <sl-icon
                    name=${lifecycleStatus === 'active'
                      ? 'check-circle'
                      : lifecycleStatus === 'expired'
                        ? 'x-circle'
                        : 'clock'}
                  ></sl-icon>
                  ${lifecycleStatus === 'active'
                    ? 'Active'
                    : lifecycleStatus === 'expired'
                      ? 'Expired'
                      : 'Scheduled'}
                </span>
                ${binding.expiresAt && lifecycleStatus !== 'expired'
                  ? html`<div class="lifecycle-detail">
                      Expires ${formatDateTime(binding.expiresAt)}
                    </div>`
                  : ''}
                ${binding.notBefore && lifecycleStatus === 'pending'
                  ? html`<div class="lifecycle-detail">
                      Activates ${formatDateTime(binding.notBefore)}
                    </div>`
                  : ''}
              `
            : html`<span class="lifecycle-badge active">
                <sl-icon name="check-circle"></sl-icon> Active
              </span>`}
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(binding.createdAt)}</span>
        </td>
        <td>
          <sl-icon-button
            name="arrow-repeat"
            label="Replace role"
            @click=${() => this.openReplaceDialog(binding)}
          ></sl-icon-button>
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
  // Create Dialog — "Assign Role" form
  // ---------------------------------------------------------------------------

  private renderCreateDialog() {
    if (!this.showCreateDialog) return nothing;

    return html`
      <sl-dialog
        label="Assign Role"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) this.showCreateDialog = false;
        }}
      >
        ${this.formValidationWarning
          ? html`
              <div class="validation-warning">
                <sl-icon name="exclamation-triangle"></sl-icon>
                ${this.formValidationWarning}
              </div>
            `
          : ''}

        <!-- Step 1: Select principal -->
        <div class="form-group">
          <sl-select
            label="Principal Type"
            .value=${this.formPrincipalType}
            @sl-change=${(e: Event) => {
              this.formPrincipalType = (e.target as HTMLSelectElement).value;
              this.formPrincipalId = '';
              this.updateValidation();
            }}
          >
            <sl-option value="user">
              <sl-icon slot="prefix" name="person"></sl-icon>
              User
            </sl-option>
            <sl-option value="agent">
              <sl-icon slot="prefix" name="cpu"></sl-icon>
              Agent
            </sl-option>
            <sl-option value="group">
              <sl-icon slot="prefix" name="diagram-3"></sl-icon>
              Group
            </sl-option>
          </sl-select>
        </div>
        <div class="form-group">
          <scion-principal-picker
            .principalType=${this.formPrincipalType as 'user' | 'agent' | 'group'}
            @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) => {
              this.formPrincipalId = e.detail.principalId;
            }}
          ></scion-principal-picker>
        </div>

        <!-- Step 2: Select role -->
        <div class="form-group">
          <sl-select
            label="Role"
            .value=${this.formRoleId}
            @sl-change=${(e: Event) => {
              this.formRoleId = (e.target as HTMLSelectElement).value;
              // Auto-set scope type to match the role's scope type
              const scopeType = this.roleScopeMap[this.formRoleId];
              if (scopeType) {
                this.formScopeType = scopeType;
              }
              this.updateValidation();
            }}
          >
            ${this.filteredRoles.length === 0
              ? html`<sl-option value="" disabled>No roles available for this scope</sl-option>`
              : this.filteredRoles.map(
                  (role) => html`
                    <sl-option value=${role.id}>
                      ${role.name}
                      <small style="color: var(--scion-text-muted, #64748b)">
                        (${role.scopeType})
                      </small>
                    </sl-option>
                  `
                )}
          </sl-select>
        </div>

        <!-- Step 3: Select scope -->
        <div class="form-group">
          <sl-select
            label="Scope"
            .value=${this.formScopeType}
            @sl-change=${(e: Event) => {
              this.formScopeType = (e.target as HTMLSelectElement).value;
              // Re-filter roles when scope type changes
              if (this.formRoleId && this.roleScopeMap[this.formRoleId] !== this.formScopeType) {
                this.formRoleId = '';
              }
              this.updateValidation();
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
                  label="Project ID"
                  placeholder="Enter project ID"
                  .value=${this.formScopeId}
                  @sl-input=${(e: Event) => {
                    this.formScopeId = (e.target as HTMLInputElement).value;
                  }}
                  required
                ></sl-input>
              </div>
            `
          : ''}

        <!-- Advanced: Assignment lifecycle -->
        <button
          class="advanced-toggle ${this.showAdvanced ? 'open' : ''}"
          @click=${() => {
            this.showAdvanced = !this.showAdvanced;
          }}
        >
          <sl-icon name="chevron-right"></sl-icon>
          Advanced
          ${this.hasLifecycleConditions
            ? html`<span class="lifecycle-indicator">
                <sl-icon name="clock-history"></sl-icon>
                Lifecycle conditions set
              </span>`
            : ''}
        </button>

        ${this.showAdvanced
          ? html`
              <div class="advanced-content">
                <div class="form-group">
                  <sl-input
                    label="Activate After (Not Before)"
                    type="datetime-local"
                    .value=${this.formNotBefore}
                    @sl-input=${(e: Event) => {
                      this.formNotBefore = (e.target as HTMLInputElement).value;
                    }}
                    clearable
                  ></sl-input>
                  <div class="lifecycle-hint">
                    Optional. The binding will not take effect until this date/time.
                  </div>
                </div>
                <div class="form-group">
                  <sl-input
                    label="Expires On"
                    type="datetime-local"
                    .value=${this.formExpiresAt}
                    @sl-input=${(e: Event) => {
                      this.formExpiresAt = (e.target as HTMLInputElement).value;
                    }}
                    clearable
                  ></sl-input>
                  <div class="lifecycle-hint">
                    Optional. The binding will automatically expire after this date/time.
                  </div>
                </div>
                ${this.formExpiresAt
                  ? (() => {
                      const ea = new Date(this.formExpiresAt).getTime();
                      if (!isNaN(ea) && ea < Date.now()) {
                        return html`
                          <div class="validation-warning">
                            <sl-icon name="exclamation-triangle"></sl-icon>
                            This expiration date is in the past. The binding will be created already
                            expired.
                          </div>
                        `;
                      }
                      if (this.formNotBefore) {
                        const nb = new Date(this.formNotBefore).getTime();
                        if (!isNaN(nb) && !isNaN(ea) && ea <= nb) {
                          return html`
                            <div class="validation-warning">
                              <sl-icon name="exclamation-triangle"></sl-icon>
                              Expiration must be after the activation date.
                            </div>
                          `;
                        }
                      }
                      return nothing;
                    })()
                  : ''}
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
          >Assign Role</sl-button
        >
      </sl-dialog>
    `;
  }

  // ---------------------------------------------------------------------------
  // Delete Dialog
  // ---------------------------------------------------------------------------

  private renderDeleteDialog() {
    if (!this.showDeleteDialog || !this.deletingBinding) return nothing;

    return html`
      <sl-dialog
        label="Remove Role Assignment"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) {
            this.showDeleteDialog = false;
            this.deletingBinding = null;
          }
        }}
      >
        <p>
          Are you sure you want to remove the
          <strong>${this.getRoleName(this.deletingBinding.roleDefinitionId)}</strong>
          role from
          <strong
            >${this.deletingBinding.principalDisplayName ||
            this.deletingBinding.principalId}</strong
          >?
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
          >Remove Assignment</sl-button
        >
      </sl-dialog>
    `;
  }

  // ---------------------------------------------------------------------------
  // Replace Dialog — atomic role replacement
  // ---------------------------------------------------------------------------

  private renderReplaceDialog() {
    if (!this.showReplaceDialog || !this.replacingBinding) return nothing;

    const binding = this.replacingBinding;
    const scopeType = this.roleScopeMap[binding.roleDefinitionId] ?? binding.scopeType;
    const availableRoles = this.roles.filter(
      (r) =>
        r.scopeType === scopeType &&
        r.id !== binding.roleDefinitionId &&
        !(binding.principalType === 'group' && SYSTEM_DIRECT_USER_ONLY_ROLES.includes(r.name))
    );

    return html`
      <sl-dialog
        label="Replace Role Assignment"
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) {
            this.showReplaceDialog = false;
            this.replacingBinding = null;
          }
        }}
      >
        <p>
          Replace the
          <strong>${this.getRoleName(binding.roleDefinitionId)}</strong>
          role for
          <strong>${binding.principalDisplayName || binding.principalId}</strong>
          with a different role. This will be performed as a single atomic operation.
        </p>

        <div class="form-group">
          <sl-select
            label="New Role"
            .value=${this.replaceRoleId}
            @sl-change=${(e: Event) => {
              this.replaceRoleId = (e.target as HTMLSelectElement).value;
            }}
          >
            ${availableRoles.length === 0
              ? html`<sl-option value="" disabled>No other roles available</sl-option>`
              : availableRoles.map(
                  (role) => html`
                    <sl-option value=${role.id}>
                      ${role.name}
                      <small style="color: var(--scion-text-muted, #64748b)">
                        (${role.scopeType})
                      </small>
                    </sl-option>
                  `
                )}
          </sl-select>
        </div>

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.showReplaceDialog = false;
            this.replacingBinding = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.actionInProgress}
          ?disabled=${!this.replaceRoleId}
          @click=${() => this.replaceBinding()}
          >Replace Role</sl-button
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
