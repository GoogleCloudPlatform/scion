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
 * Project Members Editor — RoleBinding-backed (PM1)
 *
 * Manages project membership via the project-scoped members API:
 *  - Adding a member = POST /api/v1/projects/{id}/members
 *  - Changing a member's role = PATCH /api/v1/projects/{id}/members/{bindingID}
 *  - Removing a member = DELETE /api/v1/projects/{id}/members/{bindingID}
 *  - Shows provenance (direct vs. group-derived)
 *  - Owner protection: prevents removing the last direct owner
 *
 * The server returns enriched bindings with roleName and source fields,
 * and the PATCH endpoint performs atomic role changes.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import type { PrincipalChangeDetail } from './principal-picker.js';
import { showConfirm } from './confirm-dialog.js';
import './principal-picker.js';
import {
  PROJECT_DIRECT_USER_ONLY_ROLES,
  PROJECT_OWNER_ROLE_NAMES,
  getPrincipalIcon,
} from './role-binding-utils.js';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

interface ProjectMemberBinding {
  id: string;
  roleDefinitionId: string;
  roleName: string;
  principalType: string;
  principalId: string;
  principalDisplayName?: string;
  scopeType: string;
  scopeId: string;
  createdAt: string;
  notBefore?: string;
  expiresAt?: string;
  /** 'direct' or the group name this was inherited through. */
  source: 'direct' | string;
  sourceGroupName?: string;
}

interface ProjectRole {
  id: string;
  name: string;
  scopeType: string;
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

@customElement('scion-project-members-editor')
export class ScionProjectMembersEditor extends LitElement {
  /** The project ID. */
  @property() projectId = '';

  /** Whether the editor is read-only. */
  @property({ type: Boolean }) readOnly = false;

  /** Whether to render in compact card layout. */
  @property({ type: Boolean }) compact = false;

  /** Section title override. */
  @property() sectionTitle = 'Members';

  /** Section description override. */
  @property() sectionDescription = '';

  @state() private loading = true;
  @state() private members: ProjectMemberBinding[] = [];
  @state() private projectRoles: ProjectRole[] = [];
  @state() private error: string | null = null;

  // Add dialog state
  @state() private addDialogOpen = false;
  @state() private addPrincipalType = 'user';
  @state() private addPrincipalId = '';
  @state() private addRoleId = '';
  @state() private addLoading = false;
  @state() private addError: string | null = null;

  // Change role dialog state
  @state() private changeDialogOpen = false;
  @state() private changeMember: ProjectMemberBinding | null = null;
  @state() private changeRoleId = '';
  @state() private changeLoading = false;

  // Remove state
  @state() private removingMemberId: string | null = null;

  // Action feedback
  @state() private actionFeedback: { message: string; variant: 'success' | 'danger' } | null =
    null;

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
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1rem;
      gap: 1rem;
    }

    .section-header-info h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section-header-info p {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0;
    }

    .member-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      font-weight: 400;
      margin-left: 0.5rem;
    }

    /* Table */
    .table-container {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
    }

    .compact .table-container {
      border: none;
      border-radius: 0;
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

    tr:hover td {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    /* Member identity */
    .member-identity {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .member-icon {
      width: 2rem;
      height: 2rem;
      border-radius: 50%;
      display: flex;
      align-items: center;
      justify-content: center;
      flex-shrink: 0;
    }

    .member-icon.user {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-600, #2563eb);
    }

    .member-icon.group {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-600, #d97706);
    }

    .member-icon.agent {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-600, #16a34a);
    }

    .member-icon sl-icon {
      font-size: 0.875rem;
    }

    .member-info {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .member-name {
      font-weight: 500;
      font-size: 0.875rem;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .member-detail {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Role badge */
    .role-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    /* Provenance badge */
    .provenance-badge {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.6875rem;
    }

    .provenance-badge.direct {
      color: var(--sl-color-primary-600, #2563eb);
    }

    .provenance-badge.group-derived {
      color: var(--sl-color-warning-600, #d97706);
    }

    .provenance-badge sl-icon {
      font-size: 0.6875rem;
    }

    .actions-cell {
      text-align: right;
      white-space: nowrap;
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* Empty state */
    .empty-state {
      text-align: center;
      padding: 3rem 2rem;
    }

    .compact .empty-state {
      padding: 2rem 1.5rem;
    }

    .empty-state > sl-icon {
      font-size: 3rem;
      color: var(--scion-text-muted, #64748b);
      opacity: 0.5;
      margin-bottom: 0.75rem;
    }

    .empty-state h3 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem 0;
    }

    .empty-state p {
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1.25rem 0;
      font-size: 0.875rem;
    }

    /* Loading / Error */
    .loading-state {
      display: flex;
      align-items: center;
      justify-content: center;
      padding: 2rem;
      color: var(--scion-text-muted, #64748b);
      gap: 0.75rem;
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

    .feedback-alert {
      margin-bottom: 1rem;
    }

    /* Dialog */
    .form-group {
      margin-bottom: 1rem;
    }

    .form-group:last-child {
      margin-bottom: 0;
    }

    .dialog-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
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
    }

    .validation-warning sl-icon {
      flex-shrink: 0;
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }

      .section {
        padding: 1rem;
      }

      th, td {
        padding: 0.5rem 0.75rem;
      }
    }
  `;

  /** Guard to prevent double-fetch when connectedCallback and updated both fire. */
  private _initialLoadDone = false;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      this._initialLoadDone = true;
      void this.loadData();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('projectId') && this.projectId) {
      // Skip if connectedCallback already triggered the initial load.
      if (!this._initialLoadDone) {
        void this.loadData();
      }
      this._initialLoadDone = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Data loading
  // ---------------------------------------------------------------------------

  private async loadData(): Promise<void> {
    if (!this.projectId) return;

    this.loading = true;
    this.error = null;

    try {
      // PM1: Use project-scoped members endpoint + roles list in parallel.
      const [membersRes, rolesRes] = await Promise.all([
        apiFetch(
          `/api/v1/projects/${encodeURIComponent(this.projectId)}/members`
        ),
        apiFetch('/api/v1/admin/roles'),
      ]);

      if (!membersRes.ok) {
        throw new Error(
          await extractApiError(membersRes, `HTTP ${membersRes.status}`)
        );
      }

      const data = (await membersRes.json()) as {
        items?: ProjectMemberBinding[];
      };
      // Server returns enriched items with roleName and source.
      this.members = (data.items || []).map((b) => ({
        ...b,
        source: b.source || 'direct',
      }));

      // Load project roles for the role picker.
      if (rolesRes.ok) {
        const rolesData = (await rolesRes.json()) as { items?: ProjectRole[] };
        this.projectRoles = (rolesData.items || []).filter(
          (r) => r.scopeType === 'project'
        );
      }
    } catch (err) {
      console.error('Failed to load project members:', err);
      this.error =
        err instanceof Error ? err.message : 'Failed to load project members';
    } finally {
      this.loading = false;
    }
  }

  // ---------------------------------------------------------------------------
  // Helpers
  // ---------------------------------------------------------------------------

  // getPrincipalIcon is imported from ./role-binding-utils.js

  /** Returns true if a role name represents project ownership. */
  private isOwnerRole(roleName: string): boolean {
    return PROJECT_OWNER_ROLE_NAMES.includes(roleName);
  }

  private get directOwnerCount(): number {
    return this.members.filter(
      (m) =>
        m.source === 'direct' &&
        m.principalType === 'user' &&
        this.isOwnerRole(m.roleName)
    ).length;
  }

  private isLastDirectOwner(member: ProjectMemberBinding): boolean {
    if (
      member.source !== 'direct' ||
      member.principalType !== 'user'
    )
      return false;
    if (!this.isOwnerRole(member.roleName)) return false;
    return this.directOwnerCount <= 1;
  }

  private get addFilteredRoles(): ProjectRole[] {
    let roles = this.projectRoles;
    if (this.addPrincipalType === 'group') {
      roles = roles.filter((r) => !PROJECT_DIRECT_USER_ONLY_ROLES.includes(r.name));
    }
    return roles;
  }

  // ---------------------------------------------------------------------------
  // Actions
  // ---------------------------------------------------------------------------

  private openAddDialog(): void {
    this.addPrincipalType = 'user';
    this.addPrincipalId = '';
    this.addRoleId = this.projectRoles.length > 0 ? this.projectRoles[0].id : '';
    this.addError = null;
    this.addDialogOpen = true;
  }

  private async handleAddMember(): Promise<void> {
    if (!this.addPrincipalId.trim()) {
      this.addError = 'Please select a principal';
      return;
    }
    if (!this.addRoleId) {
      this.addError = 'Please select a role';
      return;
    }

    this.addLoading = true;
    this.addError = null;

    try {
      // PM1: Use project-scoped members endpoint.
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/members`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            roleDefinitionId: this.addRoleId,
            principalType: this.addPrincipalType,
            principalId: this.addPrincipalId.trim(),
          }),
        }
      );

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this.addDialogOpen = false;
      this.actionFeedback = { message: 'Member added', variant: 'success' };
      void this.loadData();
    } catch (err) {
      console.error('Failed to add member:', err);
      this.addError =
        err instanceof Error ? err.message : 'Failed to add member';
    } finally {
      this.addLoading = false;
    }
  }

  private openChangeRoleDialog(member: ProjectMemberBinding): void {
    this.changeMember = member;
    this.changeRoleId = member.roleDefinitionId;
    this.changeDialogOpen = true;
  }

  /** Look up a role name from its definition ID. */
  private getRoleNameById(roleId: string): string {
    const role = this.projectRoles.find((r) => r.id === roleId);
    return role?.name ?? '';
  }

  private async handleChangeRole(): Promise<void> {
    if (!this.changeMember || !this.changeRoleId) return;

    // R1: Prevent demoting the last direct owner to a non-owner role.
    const newRoleName = this.getRoleNameById(this.changeRoleId);
    if (
      this.isLastDirectOwner(this.changeMember) &&
      !this.isOwnerRole(newRoleName)
    ) {
      this.actionFeedback = {
        message:
          'Cannot change the last direct project owner to a non-owner role. Transfer ownership first.',
        variant: 'danger',
      };
      this.changeDialogOpen = false;
      this.changeMember = null;
      return;
    }

    this.changeLoading = true;

    try {
      // PM1: Atomic role change via PATCH endpoint.
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/members/${this.changeMember.id}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            roleDefinitionId: this.changeRoleId,
          }),
        }
      );

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this.changeDialogOpen = false;
      this.changeMember = null;
      this.actionFeedback = { message: 'Role updated', variant: 'success' };
      void this.loadData();
    } catch (err) {
      console.error('Failed to change role:', err);
      this.actionFeedback = {
        message:
          err instanceof Error ? err.message : 'Failed to change role',
        variant: 'danger',
      };
    } finally {
      this.changeLoading = false;
    }
  }

  private async handleRemoveMember(
    member: ProjectMemberBinding
  ): Promise<void> {
    if (this.isLastDirectOwner(member)) {
      this.actionFeedback = {
        message:
          'Cannot remove the last direct project owner. Transfer ownership first.',
        variant: 'danger',
      };
      return;
    }

    const displayName =
      member.principalDisplayName || member.principalId;
    if (
      !(await showConfirm(
        `Remove ${member.principalType} "${displayName}" from this project?`
      ))
    ) {
      return;
    }

    this.removingMemberId = member.id;

    try {
      // PM1: Use project-scoped members endpoint.
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/members/${member.id}`,
        { method: 'DELETE' }
      );
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this.actionFeedback = { message: 'Member removed', variant: 'success' };
      void this.loadData();
    } catch (err) {
      console.error('Failed to remove member:', err);
      this.actionFeedback = {
        message:
          err instanceof Error ? err.message : 'Failed to remove member',
        variant: 'danger',
      };
    } finally {
      this.removingMemberId = null;
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
      ${this.renderFeedback()}
      <div class="section-header">
        <div class="section-header-info">
          <h2>
            ${this.sectionTitle}
            <span class="member-count">(${this.members.length})</span>
          </h2>
          ${this.sectionDescription
            ? html`<p>${this.sectionDescription}</p>`
            : nothing}
        </div>
        ${!this.readOnly
          ? html`
              <sl-button
                variant="primary"
                size="small"
                @click=${this.openAddDialog}
              >
                <sl-icon slot="prefix" name="person-plus"></sl-icon>
                Add Member
              </sl-button>
            `
          : nothing}
      </div>
      ${this.renderBody()} ${this.renderAddDialog()}
      ${this.renderChangeRoleDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        ${this.renderFeedback()}
        <div class="section-header">
          <div class="section-header-info">
            <h2>
              ${this.sectionTitle}
              <span class="member-count">(${this.members.length})</span>
            </h2>
            ${this.sectionDescription
              ? html`<p>${this.sectionDescription}</p>`
              : nothing}
          </div>
          ${!this.readOnly
            ? html`
                <sl-button
                  size="small"
                  variant="default"
                  @click=${this.openAddDialog}
                >
                  <sl-icon slot="prefix" name="person-plus"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>
        ${this.renderBody()} ${this.renderAddDialog()}
        ${this.renderChangeRoleDialog()}
      </div>
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

  private renderBody() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner> Loading members...
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <span>${this.error}</span>
          <sl-button size="small" @click=${() => this.loadData()}>
            Retry
          </sl-button>
        </div>
      `;
    }

    if (this.members.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="people"></sl-icon>
          <h3>No Members</h3>
          <p>Add members to grant access to this project.</p>
          ${!this.readOnly
            ? html`
                <sl-button
                  variant="primary"
                  size="small"
                  @click=${this.openAddDialog}
                >
                  <sl-icon slot="prefix" name="person-plus"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>
      `;
    }

    return this.renderMembersTable();
  }

  private renderMembersTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Member</th>
              <th>Role</th>
              <th class="hide-mobile">Source</th>
              ${!this.readOnly
                ? html`<th class="actions-cell">Actions</th>`
                : nothing}
            </tr>
          </thead>
          <tbody>
            ${this.members.map((member) => this.renderMemberRow(member))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderMemberRow(member: ProjectMemberBinding) {
    const isRemoving = this.removingMemberId === member.id;
    const displayName =
      member.principalDisplayName || member.principalId;
    const isGroupDerived = member.source !== 'direct';
    const lastOwner = this.isLastDirectOwner(member);

    return html`
      <tr>
        <td>
          <div class="member-identity">
            <div class="member-icon ${member.principalType}">
              <sl-icon
                name="${getPrincipalIcon(member.principalType)}"
              ></sl-icon>
            </div>
            <div class="member-info">
              <span class="member-name">${displayName}</span>
              <span class="member-detail">${member.principalType}</span>
            </div>
          </div>
        </td>
        <td>
          <span class="role-badge">${member.roleName}</span>
        </td>
        <td class="hide-mobile">
          <span
            class="provenance-badge ${isGroupDerived ? 'group-derived' : 'direct'}"
          >
            ${isGroupDerived
              ? html`<sl-icon name="diagram-3"></sl-icon> Via group:
                  ${member.sourceGroupName || member.source}`
              : html`<sl-icon name="person-check"></sl-icon> Direct`}
          </span>
        </td>
        ${!this.readOnly
          ? html`
              <td class="actions-cell">
                ${!isGroupDerived
                  ? html`
                      ${this.projectRoles.length > 0
                        ? html`
                            <sl-icon-button
                              name="pencil"
                              label="Change role"
                              ?disabled=${isRemoving || lastOwner}
                              @click=${() =>
                                this.openChangeRoleDialog(member)}
                            ></sl-icon-button>
                          `
                        : ''}
                      <sl-icon-button
                        name="trash"
                        label="Remove member"
                        ?disabled=${isRemoving || lastOwner}
                        @click=${() => this.handleRemoveMember(member)}
                      ></sl-icon-button>
                      ${lastOwner
                        ? html`<sl-tooltip
                            content="Last direct owner — cannot change role or remove"
                          >
                            <sl-icon
                              name="shield-lock"
                              style="color: var(--sl-color-warning-500)"
                            ></sl-icon>
                          </sl-tooltip>`
                        : ''}
                    `
                  : html`<span class="meta-text">Inherited</span>`}
              </td>
            `
          : nothing}
      </tr>
    `;
  }

  // ---------------------------------------------------------------------------
  // Dialogs
  // ---------------------------------------------------------------------------

  private renderAddDialog() {
    if (!this.addDialogOpen) return nothing;

    return html`
      <sl-dialog
        label="Add Project Member"
        open
        @sl-request-close=${() => {
          if (!this.addLoading) this.addDialogOpen = false;
        }}
      >
        <div class="form-group">
          <sl-select
            label="Member Type"
            .value=${this.addPrincipalType}
            @sl-change=${(e: Event) => {
              this.addPrincipalType = (e.target as HTMLSelectElement).value;
              this.addPrincipalId = '';
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
            .principalType=${this.addPrincipalType as 'user' | 'agent' | 'group'}
            @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) => {
              this.addPrincipalId = e.detail.principalId;
            }}
          ></scion-principal-picker>
        </div>

        ${this.projectRoles.length > 0
          ? html`
              <div class="form-group">
                <sl-select
                  label="Project Role"
                  .value=${this.addRoleId}
                  @sl-change=${(e: Event) => {
                    this.addRoleId = (e.target as HTMLSelectElement).value;
                  }}
                >
                  ${this.addFilteredRoles.map(
                    (role) => html`
                      <sl-option value=${role.id}>${role.name}</sl-option>
                    `
                  )}
                </sl-select>
              </div>
              ${this.addPrincipalType === 'group'
                ? html`
                    <div class="validation-warning">
                      <sl-icon name="info-circle"></sl-icon>
                      Group members will inherit this project role. Owner role
                      is not available for groups.
                    </div>
                  `
                : ''}
            `
          : ''}

        ${this.addError
          ? html`<div class="dialog-error">${this.addError}</div>`
          : nothing}

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.addLoading}
          @click=${() => {
            this.addDialogOpen = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.addLoading}
          ?disabled=${!this.addPrincipalId.trim()}
          @click=${() => this.handleAddMember()}
          >Add Member</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderChangeRoleDialog() {
    if (!this.changeDialogOpen || !this.changeMember) return nothing;

    return html`
      <sl-dialog
        label="Change Role"
        open
        @sl-request-close=${() => {
          if (!this.changeLoading) {
            this.changeDialogOpen = false;
            this.changeMember = null;
          }
        }}
      >
        <p>
          Change role for
          <strong
            >${this.changeMember.principalDisplayName ||
            this.changeMember.principalId}</strong
          >:
        </p>
        <div class="form-group">
          <sl-select
            label="New Role"
            .value=${this.changeRoleId}
            @sl-change=${(e: Event) => {
              this.changeRoleId = (e.target as HTMLSelectElement).value;
            }}
          >
            ${this.projectRoles
              .filter(
                (r) =>
                  this.changeMember?.principalType !== 'group' ||
                  !PROJECT_DIRECT_USER_ONLY_ROLES.includes(r.name)
              )
              .map(
                (role) => html`
                  <sl-option value=${role.id}>${role.name}</sl-option>
                `
              )}
          </sl-select>
        </div>

        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.changeLoading}
          @click=${() => {
            this.changeDialogOpen = false;
            this.changeMember = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.changeLoading}
          ?disabled=${!this.changeRoleId ||
          this.changeRoleId === this.changeMember.roleDefinitionId}
          @click=${() => this.handleChangeRole()}
          >Update Role</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-project-members-editor': ScionProjectMembersEditor;
  }
}
