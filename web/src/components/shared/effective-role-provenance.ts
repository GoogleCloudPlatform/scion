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
 * Effective Role Provenance & Access Composition Display
 *
 * Shows a principal's effective roles with provenance and layered
 * effective-access composition:
 *
 * 1. Assigned roles / Potential permissions: direct/group provenance,
 *    active/scheduled/expired assignment state, union of permissions
 *    from active grants.
 * 2. Access boundaries: named active/scheduled boundaries with membership
 *    paths and their impact.
 * 3. Intrinsic restrictions: credential scope, principal status, delegation
 *    ceiling.
 * 4. Effective permissions: final set after all layers applied.
 *
 * TERMINOLOGY: layers are descriptive, not an override order. Overlapping
 * removal is "removed by both," not "won by." Never use "priority",
 * "override", or "winner."
 */

import { LitElement, html, css, nothing } from 'lit';
import { srOnlyStyles } from './styles.js';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { showConfirm } from './confirm-dialog.js';
import { getLifecycleStatus, formatDateTime } from './role-binding-utils.js';

import type { RedactionNotice } from '../../shared/access-boundaries.js';

import type {
  BoundaryLayer,
  IntrinsicRestriction,
  DeniedPermission,
  PermissionDenialReason,
} from './authorization-layer-stack.js';
import './authorization-layer-stack.js';

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

/** Shape of the access-explain API response. */
interface AccessExplainResponse {
  scopeType?: string;
  activeBindingCount?: number;
  boundaries?: AccessExplainBoundary[];
  restrictions?: AccessExplainRestriction[];
  deniedPermissions?: AccessExplainDeniedPermission[];
  redacted?: RedactionNotice;
}

interface AccessExplainBoundary {
  id: string;
  name?: string | null;
  status?: string;
  redacted?: RedactionNotice;
}

interface AccessExplainRestriction {
  kind?: string;
  label?: string;
  detail?: string;
}

interface AccessExplainDeniedPermission {
  permissionId?: string;
  reasons?: Array<{
    type?: string;
    grantStatus?: string;
    boundaryNames?: (string | null)[];
    restrictionLabel?: string;
    correlationId?: string;
  }>;
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

  // Explain layer state
  @state() private explainLoading = false;
  @state() private explainError: string | null = null;
  @state() private potentialCount = 0;
  @state() private effectiveCount = 0;
  @state() private boundaries: BoundaryLayer[] = [];
  @state() private restrictions: IntrinsicRestriction[] = [];
  @state() private deniedPermissions: DeniedPermission[] = [];
  @state() private explainRedacted?: RedactionNotice;
  @state() private showLayers = false;
  /** Whether explain layers have been successfully loaded at least once. */
  @state() private _explainLoaded = false;
  /**
   * Whether the effective-access endpoint returned 403 (insufficient
   * permissions). When true, the layers toggle is hidden entirely rather
   * than showing a dead error control. hub-admin users have
   * role_binding.read/user.update but not hub.audit.read, so this gate
   * prevents a confusing 403 composition control (R4-fix).
   */
  @state() private _explainForbidden = false;
  /**
   * Whether the pre-click capability check has resolved (either allowed or
   * denied). The composition toggle is hidden until this is true, so the
   * user never sees a toggle that will be removed moments later (R6).
   */
  @state() private _explainPreChecked = false;

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

      /* Layers section */
      .layers-toggle {
        margin-top: 1rem;
        padding-top: 0.75rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .layers-toggle-header {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        cursor: pointer;
        user-select: none;
        font-size: 0.8125rem;
        font-weight: 600;
        color: var(--sl-color-primary-600, #2563eb);
        padding: 0.25rem 0;
      }

      .layers-toggle-header:hover {
        color: var(--sl-color-primary-700, #1d4ed8);
      }

      .layers-toggle-header sl-icon {
        font-size: 0.75rem;
        transition: transform 0.2s ease;
      }

      .layers-toggle-header sl-icon.open {
        transform: rotate(90deg);
      }

      .layers-content {
        margin-top: 0.75rem;
      }

      .explain-error {
        font-size: 0.8125rem;
        color: var(--sl-color-danger-600, #dc2626);
        padding: 0.5rem;
        display: flex;
        align-items: center;
        gap: 0.5rem;
      }

      .explain-loading {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem;
      }

      .redaction-notice {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
        font-style: italic;
        padding: 0.375rem 0;
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

      /* Add binding dialog */
      .add-form-group {
        margin-bottom: 0.75rem;
      }

      .add-form-group label {
        display: block;
        font-size: 0.8125rem;
        font-weight: 500;
        color: var(--scion-text, #1e293b);
        margin-bottom: 0.25rem;
      }

      .locked-principal {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.5rem 0.75rem;
        background: var(--scion-bg-subtle, #f8fafc);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }

      .locked-principal sl-icon {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
      }

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

        .layers-toggle {
          border-top-color: ButtonText;
        }
      }

      @media (prefers-reduced-motion: reduce) {
        .layers-toggle-header sl-icon {
          transition: none;
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

    // Pre-click capability gate (R6): check effective-access authorization
    // concurrently with binding load. If the current user lacks hub.audit.read,
    // the composition toggle is hidden before the user ever sees it — no
    // wasted click and no 403 after interaction.
    void this.preCheckExplainAccess();

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
   * Pre-click capability gate for the effective-access composition toggle
   * (R6). Fires a lightweight HEAD request to the explain endpoint. The
   * backend short-circuits after the authorization check (no full effective-
   * access computation). If the server responds with 403, the toggle is
   * hidden before the user can interact with it — no wasted click and no
   * dead error control. The 403 toast is suppressed since it's an expected
   * authorization probe, not an unexpected denial.
   *
   * The toggle is rendered only after this check resolves (_explainPreChecked).
   */
  private async preCheckExplainAccess(): Promise<void> {
    if (this._explainForbidden || this._explainLoaded) return;
    try {
      const url = `/api/v1/admin/effective-access?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(this.principalId)}`;
      const res = await apiFetch(url, {
        method: 'HEAD',
        suppressAccessDeniedToast: true,
      });
      if (res.status === 403) {
        this._explainForbidden = true;
      }
    } catch {
      // Network errors are not authorization failures — leave the toggle
      // visible so the user can retry after connectivity is restored.
    } finally {
      this._explainPreChecked = true;
    }
  }

  /**
   * Pre-check whether the current user can create/delete role bindings.
   * Probes the create endpoint (POST with empty body → 400 means authorized,
   * 403 means not). Uses suppressAccessDeniedToast since this is an
   * authorization probe.  For delete, the same role_binding permission
   * scope governs both create and delete, so a single check suffices.
   */
  private async preCheckMutationAccess(): Promise<void> {
    if (this._mutationPreChecked) return;
    try {
      const res = await apiFetch('/api/v1/admin/role-bindings', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({}),
        suppressAccessDeniedToast: true,
      });
      // 400 = authorized but invalid body → user can create/delete
      // 403 = not authorized
      if (res.status === 400) {
        this._canCreate = true;
        this._canDelete = true;
      } else if (res.status === 403) {
        this._canCreate = false;
        this._canDelete = false;
      } else {
        // Unexpected — default to visible so user gets server feedback
        this._canCreate = true;
        this._canDelete = true;
      }
    } catch {
      // Network error — leave buttons visible, server will reject if needed
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
  }

  private get _filteredAddRoles(): RoleDefinitionSummary[] {
    return this._addRoles.filter((r) => r.scopeType === this._addScopeType);
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

  private async loadExplainLayers(): Promise<void> {
    if (!this.principalId) return;

    this.explainLoading = true;
    this.explainError = null;

    try {
      const url = `/api/v1/admin/effective-access?principalType=${encodeURIComponent(this.principalType)}&principalId=${encodeURIComponent(this.principalId)}`;
      const res = await apiFetch(url);

      if (!res.ok) {
        // Gate on authorization: if the endpoint requires hub.audit.read and
        // the current user doesn't have it, silently hide the composition
        // section instead of showing an error (R4-fix: prevents dead 403
        // composition control for hub-admin users).
        if (res.status === 403) {
          this._explainForbidden = true;
          this.showLayers = false;
          return;
        }
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      const data = (await res.json()) as AccessExplainResponse;

      // The endpoint returns activeBindingCount (role bindings, not permission
      // counts). Display it as a binding indicator — do NOT label it as
      // "permissions" since per-permission aggregation is not computed.
      this.potentialCount = data.activeBindingCount ?? 0;
      // Effective count is set to the same binding count; the system-scope
      // endpoint does not compute per-permission subtraction.  The component
      // labels are patched to say "bindings" so this is truthful.
      this.effectiveCount = data.activeBindingCount ?? 0;

      // Map boundaries — preserve redaction. removedCount is unknown at this
      // scope so it stays 0 (the layer-stack renders "applied" instead of a
      // fabricated count when removedCount is 0).
      this.boundaries = (data.boundaries ?? []).map((b): BoundaryLayer => {
        const layer: BoundaryLayer = {
          id: b.id,
          name: b.redacted ? null : (b.name ?? null),
          status: b.status ?? 'active',
          removedCount: 0,
          overlapCount: 0,
        };
        if (b.redacted !== undefined) layer.redacted = b.redacted;
        return layer;
      });

      // Map restrictions — removedCount unknown, same treatment.
      this.restrictions = (data.restrictions ?? []).map((r): IntrinsicRestriction => {
        const restriction: IntrinsicRestriction = {
          kind: r.kind ?? 'unknown',
          label: r.label ?? r.kind ?? 'Unknown restriction',
          removedCount: 0,
        };
        if (r.detail !== undefined) restriction.detail = r.detail;
        return restriction;
      });

      // Map denied permissions with reasons
      this.deniedPermissions = (data.deniedPermissions ?? []).map(
        (dp): DeniedPermission => ({
          permissionId: dp.permissionId ?? '',
          reasons: (dp.reasons ?? []).map((r): PermissionDenialReason => {
            switch (r.type) {
              case 'never_granted':
                return { type: 'never_granted' };
              case 'inactive_grant':
                return { type: 'inactive_grant', grantStatus: r.grantStatus ?? 'inactive' };
              case 'removed_by_boundaries':
                return { type: 'removed_by_boundaries', boundaryNames: r.boundaryNames ?? [] };
              case 'removed_by_restriction':
                return {
                  type: 'removed_by_restriction',
                  restrictionLabel: r.restrictionLabel ?? '',
                };
              case 'evaluation_failed':
                return { type: 'evaluation_failed', correlationId: r.correlationId ?? '' };
              default:
                return { type: 'never_granted' };
            }
          }),
        })
      );

      if (data.redacted !== undefined) {
        this.explainRedacted = data.redacted;
      }

      this._explainLoaded = true;
    } catch (err) {
      console.error('Failed to load access explain:', err);
      this.explainError = err instanceof Error ? err.message : 'Failed to load access layers';
    } finally {
      this.explainLoading = false;
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
      ${this.renderLayersSection()}
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
        <!-- Principal (locked) -->
        <div class="add-form-group">
          <label>Principal</label>
          <div class="locked-principal">
            <sl-icon name=${this.principalType === 'user' ? 'person' : 'cpu'}></sl-icon>
            <span>${this.principalType}: ${this.principalId}</span>
            <sl-icon name="lock" style="margin-left: auto;"></sl-icon>
          </div>
        </div>

        <!-- Scope -->
        <div class="add-form-group">
          <sl-select
            label="Scope"
            .value=${this._addScopeType}
            @sl-change=${(e: Event) => {
              this._addScopeType = (e.target as HTMLSelectElement).value;
              // Reset role if scope type changed
              if (
                this._addRoleId &&
                this._addRoles.find((r) => r.id === this._addRoleId)?.scopeType !==
                  this._addScopeType
              ) {
                this._addRoleId = '';
              }
            }}
          >
            <sl-option value="system">System</sl-option>
            <sl-option value="project">Project</sl-option>
          </sl-select>
        </div>
        ${this._addScopeType === 'project'
          ? html`
              <div class="add-form-group">
                <sl-input
                  label="Project ID"
                  placeholder="Enter project ID"
                  .value=${this._addScopeId}
                  @sl-input=${(e: Event) => {
                    this._addScopeId = (e.target as HTMLInputElement).value;
                  }}
                  required
                ></sl-input>
              </div>
            `
          : ''}

        <!-- Role -->
        <div class="add-form-group">
          <sl-select
            label="Role"
            .value=${this._addRoleId}
            @sl-change=${(e: Event) => {
              this._addRoleId = (e.target as HTMLSelectElement).value;
            }}
          >
            ${this._filteredAddRoles.length === 0
              ? html`<sl-option value="" disabled>No roles available for this scope</sl-option>`
              : this._filteredAddRoles.map(
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

  // ---------------------------------------------------------------------------
  // Layers section — effective-access composition
  // ---------------------------------------------------------------------------

  private renderLayersSection() {
    // Hide the composition toggle entirely if the effective-access endpoint
    // returned 403 — the user lacks hub.audit.read (R4-fix).
    // Also hide until the pre-check has resolved so the user never sees a
    // toggle that will be removed moments later (R6).
    if (this._explainForbidden || !this._explainPreChecked) {
      return nothing;
    }

    return html`
      <div class="layers-toggle">
        <div
          class="layers-toggle-header"
          @click=${() => this.handleLayersToggle()}
          @keydown=${(e: KeyboardEvent) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              this.handleLayersToggle();
            }
          }}
          tabindex="0"
          role="button"
          aria-expanded=${this.showLayers}
        >
          <sl-icon name="chevron-right" class=${this.showLayers ? 'open' : ''}></sl-icon>
          Effective access composition
        </div>
        ${this.showLayers ? this.renderLayersContent() : nothing}
      </div>
    `;
  }

  private handleLayersToggle(): void {
    this.showLayers = !this.showLayers;
    if (this.showLayers && !this.explainLoading && !this._explainLoaded && !this.explainError) {
      void this.loadExplainLayers();
    }
  }

  private renderLayersContent() {
    if (this.explainLoading) {
      return html`
        <div class="layers-content">
          <div class="explain-loading"><sl-spinner></sl-spinner> Loading access layers...</div>
        </div>
      `;
    }

    if (this.explainError) {
      return html`
        <div class="layers-content">
          <div class="explain-error" role="alert">
            <sl-icon name="exclamation-triangle"></sl-icon>
            ${this.explainError}
            <sl-button size="small" @click=${() => this.loadExplainLayers()}> Retry </sl-button>
          </div>
        </div>
      `;
    }

    return html`
      <div class="layers-content">
        ${this.explainRedacted
          ? html`<div class="redaction-notice">${this.explainRedacted.message}</div>`
          : nothing}
        <scion-authorization-layer-stack
          mode="bindings"
          .potentialCount=${this.potentialCount}
          .boundaries=${this.boundaries}
          .restrictions=${this.restrictions}
          .effectiveCount=${this.effectiveCount}
          .deniedPermissions=${this.deniedPermissions}
        ></scion-authorization-layer-stack>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-effective-role-provenance': ScionEffectiveRoleProvenance;
  }
}
