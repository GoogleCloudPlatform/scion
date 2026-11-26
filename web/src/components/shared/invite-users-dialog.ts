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
 * Invite Users / Import CSV dialog component (ptone/scion#4952).
 *
 * Supports:
 * - Single- or multi-address user invites via POST /api/v1/admin/users/invite/bulk
 * - Single-user provisioning with optional Display name via POST /api/v1/users
 * - CSV file import (email,note) parsed client-side and submitted via JSON bulk endpoint
 * - Optional pre-sign-in project memberships (gated on `user.read`) using
 *   `<scion-project-picker>` and `GET /api/v1/projects/:id/members/assignable-roles`
 * - Per-address result summary via `<scion-invite-batch-results>` with retry
 *   support when `complete === false`
 */

import { LitElement, html, css, nothing, type PropertyValues } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { can, type AssignableProjectRole } from '../../shared/types.js';
import { apiFetch, extractApiError, parseApiError } from '../../client/api.js';
import './project-picker.js';
import type { ProjectChangeDetail, ProjectOption } from './project-picker.js';
import './invite-batch-results.js';
import {
  INVITE_BULK_MAX_EMAILS,
  INVITE_BULK_MAX_MEMBERSHIPS,
  INVITE_BULK_MAX_PROJECTS,
  INVITE_BULK_MAX_ROLES_PER_PROJECT,
  formatProjectRefusalLine,
  parseBatchRefusalDetails,
  parseInviteAddressInput,
  parseInviteCsv,
  provisionWarningText,
  type InviteBatchEmailEntry,
  type InviteBatchRequest,
  type InviteBatchResponse,
} from './invite-batch-utils.js';

export type InviteDialogMode = 'invite' | 'csv';
export type InviteFeedbackVariant = 'success' | 'danger' | 'warning' | 'primary';

export interface InviteFeedbackDetail {
  variant: InviteFeedbackVariant;
  message: string;
}

export interface InviteCompletedDetail {
  reload: boolean;
  feedback?: InviteFeedbackDetail;
}

interface ProvisionUserResponse {
  user: { id?: string; email: string; status: string };
  created: boolean;
  warnings?: string[];
}

interface ProjectMembershipRow {
  rowId: number;
  projectId: string;
  projectLabel: string;
  selectedRoleIds: string[];
  assignableRoles: AssignableProjectRole[];
  loadingRoles: boolean;
  rolesError: string | null;
}

@customElement('scion-invite-users-dialog')
export class ScionInviteUsersDialog extends LitElement {
  @property({ type: Boolean })
  open = false;

  @property({ type: String })
  mode: InviteDialogMode = 'invite';

  /**
   * Whether the current caller holds `user.read`. Project memberships on invite
   * require `user.read` on the Hub (otherwise refused 403 `user_read_required`).
   */
  @property({ type: Boolean })
  canReadUsers = false;

  // Textarea invite mode state
  @state() private rawAddresses = '';
  @state() private displayName = '';
  @state() private note = '';

  // CSV import mode state
  @state() private csvFileName = '';
  @state() private csvEntries: InviteBatchEmailEntry[] = [];
  @state() private csvDuplicateCount = 0;
  @state() private csvError: string | null = null;

  // Shared project memberships state
  @state() private projectRows: ProjectMembershipRow[] = [];
  private nextRowId = 1;
  private roleFetchSeq = new Map<number, number>();

  // Submission & results state
  @state() private submitting = false;
  @state() private submitError: string | null = null;
  @state() private refusalLines: string[] = [];
  @state() private batchResponse: InviteBatchResponse | null = null;
  private lastBatchRequest: InviteBatchRequest | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    sl-dialog::part(panel) {
      width: min(640px, calc(100vw - 2rem));
      max-width: 100%;
    }

    .invite-form {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .address-summary {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      margin-top: -0.5rem;
    }

    .csv-description {
      margin: 0;
      font-size: 0.875rem;
      color: var(--scion-text-muted);
    }

    .csv-file-row {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .csv-file-input {
      font-size: 0.875rem;
      color: var(--scion-text);
    }

    .csv-error {
      font-size: 0.8125rem;
      color: var(--sl-color-danger-700);
    }

    .projects-section {
      display: flex;
      flex-direction: column;
      gap: 0.625rem;
      padding: 0.75rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius);
      background: var(--scion-bg-subtle);
    }

    .projects-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 0.5rem;
    }

    .projects-title {
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text);
    }

    .projects-hint {
      margin: 0;
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .project-row {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
      padding: 0.625rem;
      border: 1px solid var(--scion-border);
      border-radius: var(--scion-radius-sm);
      background: var(--scion-surface);
    }

    .project-row-top {
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
    }

    .project-row-picker {
      flex: 1;
      min-width: 0;
    }

    .roles-loading,
    .roles-empty {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-size: 0.75rem;
      color: var(--scion-text-muted);
    }

    .roles-error {
      font-size: 0.75rem;
      color: var(--sl-color-danger-700);
    }

    .roles-list {
      display: flex;
      flex-direction: column;
      gap: 0.375rem;
    }

    .roles-label {
      font-size: 0.75rem;
      font-weight: 500;
      color: var(--scion-text-secondary);
    }

    .role-option {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
    }

    .role-option-reason {
      font-size: 0.75rem;
      color: var(--scion-text-muted);
      padding-left: 1.5rem;
    }

    .invite-role-hint {
      margin: 0;
      font-size: 0.8125rem;
      color: var(--scion-text-muted);
    }

    .refusal-list {
      margin: 0.5rem 0 0;
      padding-left: 1.25rem;
      font-size: 0.8125rem;
    }

    .footer-actions {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      gap: 0.5rem;
      width: 100%;
    }
  `;

  protected override willUpdate(changed: PropertyValues<this>): void {
    if ((changed.has('open') && this.open) || (changed.has('mode') && this.open)) {
      this.resetState();
    }
  }

  private resetState(): void {
    this.rawAddresses = '';
    this.displayName = '';
    this.note = '';
    this.csvFileName = '';
    this.csvEntries = [];
    this.csvDuplicateCount = 0;
    this.csvError = null;
    this.projectRows = [];
    this.roleFetchSeq.clear();
    this.submitting = false;
    this.submitError = null;
    this.refusalLines = [];
    this.batchResponse = null;
    this.lastBatchRequest = null;
  }

  private get parsedAddressInput() {
    return parseInviteAddressInput(this.rawAddresses);
  }

  private get effectiveEmailCount(): number {
    return this.mode === 'csv' ? this.csvEntries.length : this.parsedAddressInput.addresses.length;
  }

  private get showDisplayNameField(): boolean {
    return (
      this.mode === 'invite' &&
      this.parsedAddressInput.addresses.length <= 1 &&
      this.projectRows.length === 0
    );
  }

  private get totalMemberships(): number {
    return this.effectiveEmailCount * this.projectRows.length;
  }

  private get capError(): string | null {
    if (this.effectiveEmailCount > INVITE_BULK_MAX_EMAILS) {
      return `Maximum ${INVITE_BULK_MAX_EMAILS} email addresses per batch (${this.effectiveEmailCount} entered). Split the list into smaller batches.`;
    }
    if (this.totalMemberships > INVITE_BULK_MAX_MEMBERSHIPS) {
      return `Maximum ${INVITE_BULK_MAX_MEMBERSHIPS} total project memberships per batch (${this.effectiveEmailCount} addresses × ${this.projectRows.length} projects = ${this.totalMemberships}). Reduce addresses or projects.`;
    }
    return null;
  }

  private get projectRowsValid(): boolean {
    return this.projectRows.every(
      (row) =>
        row.projectId.length > 0 &&
        !row.loadingRoles &&
        !row.rolesError &&
        row.selectedRoleIds.length > 0 &&
        row.selectedRoleIds.length <= INVITE_BULK_MAX_ROLES_PER_PROJECT
    );
  }

  private get canSubmit(): boolean {
    if (this.submitting) return false;
    if (this.capError) return false;
    if (this.effectiveEmailCount === 0) return false;
    if (this.mode === 'invite' && this.parsedAddressInput.addresses.length === 1) {
      // Single-address fast check: require '@' when only one token is typed
      // (mirrors the previous single-email ergonomics; multi-address lists let
      // the server return per-address `invalid` statuses).
      if (!this.parsedAddressInput.addresses[0].includes('@')) return false;
    }
    if (!this.projectRowsValid) return false;
    return true;
  }

  private buildProjectLabels(): Record<string, string> {
    const labels: Record<string, string> = {};
    for (const row of this.projectRows) {
      if (row.projectId) {
        labels[row.projectId] = row.projectLabel || row.projectId;
      }
    }
    return labels;
  }

  private buildRoleLabels(): Record<string, string> {
    const labels: Record<string, string> = {};
    for (const row of this.projectRows) {
      for (const role of row.assignableRoles) {
        labels[role.id] = role.name;
      }
    }
    return labels;
  }

  private emitClose(): void {
    this.dispatchEvent(new CustomEvent('invite-dialog-close', { bubbles: true, composed: true }));
  }

  private emitCompleted(detail: InviteCompletedDetail): void {
    this.dispatchEvent(
      new CustomEvent<InviteCompletedDetail>('invite-completed', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  private emitFeedback(detail: InviteFeedbackDetail): void {
    this.dispatchEvent(
      new CustomEvent<InviteFeedbackDetail>('invite-feedback', {
        detail,
        bubbles: true,
        composed: true,
      })
    );
  }

  private addProjectRow(): void {
    if (this.projectRows.length >= INVITE_BULK_MAX_PROJECTS) return;
    const rowId = this.nextRowId++;
    this.projectRows = [
      ...this.projectRows,
      {
        rowId,
        projectId: '',
        projectLabel: '',
        selectedRoleIds: [],
        assignableRoles: [],
        loadingRoles: false,
        rolesError: null,
      },
    ];
  }

  private removeProjectRow(rowId: number): void {
    this.roleFetchSeq.delete(rowId);
    this.projectRows = this.projectRows.filter((r) => r.rowId !== rowId);
  }

  private updateProjectRow(
    rowId: number,
    updater: (row: ProjectMembershipRow) => ProjectMembershipRow
  ): void {
    this.projectRows = this.projectRows.map((r) => (r.rowId === rowId ? updater(r) : r));
  }

  private handleProjectChange(rowId: number, e: CustomEvent<ProjectChangeDetail>): void {
    const { projectId, displayLabel } = e.detail;
    if (!projectId) {
      this.updateProjectRow(rowId, (r) => ({
        ...r,
        projectId: '',
        projectLabel: '',
        selectedRoleIds: [],
        assignableRoles: [],
        loadingRoles: false,
        rolesError: null,
      }));
      return;
    }

    this.updateProjectRow(rowId, (r) => ({
      ...r,
      projectId,
      projectLabel: displayLabel || projectId,
      selectedRoleIds: [],
      assignableRoles: [],
      loadingRoles: true,
      rolesError: null,
    }));
    void this.loadAssignableRoles(rowId, projectId);
  }

  private async loadAssignableRoles(rowId: number, projectId: string): Promise<void> {
    const seq = (this.roleFetchSeq.get(rowId) ?? 0) + 1;
    this.roleFetchSeq.set(rowId, seq);

    try {
      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(projectId)}/members/assignable-roles`,
        { suppressAccessDeniedToast: true }
      );
      if (this.roleFetchSeq.get(rowId) !== seq) return;
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as { items?: AssignableProjectRole[] };
      const items = data.items ?? [];
      this.updateProjectRow(rowId, (r) => ({
        ...r,
        assignableRoles: items,
        loadingRoles: false,
        rolesError: null,
      }));
    } catch (err) {
      if (this.roleFetchSeq.get(rowId) !== seq) return;
      this.updateProjectRow(rowId, (r) => ({
        ...r,
        assignableRoles: [],
        loadingRoles: false,
        rolesError: err instanceof Error ? err.message : 'Failed to load assignable roles',
      }));
    }
  }

  private toggleRoleSelection(rowId: number, roleId: string, checked: boolean): void {
    this.updateProjectRow(rowId, (r) => {
      if (checked) {
        if (r.selectedRoleIds.includes(roleId)) return r;
        if (r.selectedRoleIds.length >= INVITE_BULK_MAX_ROLES_PER_PROJECT) return r;
        return { ...r, selectedRoleIds: [...r.selectedRoleIds, roleId] };
      }
      return { ...r, selectedRoleIds: r.selectedRoleIds.filter((id) => id !== roleId) };
    });
  }

  private filterProjectForRow(rowId: number): (project: ProjectOption) => boolean {
    const otherIds = new Set(
      this.projectRows.filter((r) => r.rowId !== rowId && r.projectId).map((r) => r.projectId)
    );
    return (project: ProjectOption) => {
      if (otherIds.has(project.id)) return false;
      if (project._capabilities && !can(project._capabilities, 'manage')) {
        return false;
      }
      return true;
    };
  }

  private async handleCsvFileChange(e: Event): Promise<void> {
    const input = e.target as HTMLInputElement;
    const file = input.files?.[0];
    this.csvError = null;
    this.submitError = null;
    this.refusalLines = [];

    if (!file) {
      this.csvFileName = '';
      this.csvEntries = [];
      this.csvDuplicateCount = 0;
      return;
    }

    this.csvFileName = file.name;
    try {
      const text = await file.text();
      const parsed = parseInviteCsv(text);
      this.csvEntries = parsed.entries;
      this.csvDuplicateCount = parsed.duplicateCount;
      if (parsed.entries.length === 0) {
        this.csvError = 'No email addresses found in the selected file.';
      }
    } catch (err) {
      this.csvEntries = [];
      this.csvDuplicateCount = 0;
      this.csvError = err instanceof Error ? err.message : 'Failed to read CSV file.';
    }
  }

  private async handleSubmit(): Promise<void> {
    if (!this.canSubmit) return;

    this.submitError = null;
    this.refusalLines = [];

    // Single-user provisioning with Display name (and no projects) uses POST /api/v1/users.
    const trimmedDisplayName = this.displayName.trim();
    if (
      this.mode === 'invite' &&
      this.parsedAddressInput.addresses.length === 1 &&
      this.projectRows.length === 0 &&
      trimmedDisplayName.length > 0
    ) {
      await this.submitSingleProvision(
        this.parsedAddressInput.addresses[0].trim().toLowerCase(),
        trimmedDisplayName
      );
      return;
    }

    const trimmedNote = this.note.trim();
    const emails: InviteBatchEmailEntry[] =
      this.mode === 'csv'
        ? this.csvEntries
        : this.parsedAddressInput.addresses.map((email) =>
            trimmedNote ? { email, note: trimmedNote } : { email }
          );

    const request: InviteBatchRequest = { emails };
    if (this.projectRows.length > 0) {
      request.projects = this.projectRows.map((row) => ({
        projectId: row.projectId,
        roleDefinitionIds: [...row.selectedRoleIds],
      }));
    }

    this.lastBatchRequest = request;
    await this.submitBatchRequest(request);
  }

  private async submitSingleProvision(email: string, displayName: string): Promise<void> {
    this.submitting = true;
    try {
      const body: { email: string; displayName: string; note?: string } = { email, displayName };
      if (this.note) body.note = this.note;
      const response = await apiFetch('/api/v1/users', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (response.status === 409) {
        const { details } = await parseApiError(response, '');
        const reason = typeof details?.reason === 'string' ? details.reason : undefined;
        let message = 'User already exists.';
        if (reason === 'pending_user_exists') {
          message = 'A pending record for this email exists with different details.';
        } else if (reason === 'user_suspended_exists') {
          message = 'This email belongs to a suspended user.';
        }
        this.submitError = message;
        this.emitFeedback({ variant: 'danger', message });
        return;
      }
      if (!response.ok) {
        const message = await extractApiError(response, `HTTP ${response.status}`);
        this.submitError = message;
        this.emitFeedback({ variant: 'danger', message });
        return;
      }

      let result: ProvisionUserResponse | null = null;
      try {
        result = (await response.json()) as ProvisionUserResponse;
      } catch {
        // A success response without a readable body: report the success.
      }
      const warnings = (result?.warnings ?? []).map(provisionWarningText);
      const baseMessage =
        result && !result.created
          ? `${email} is already pre-registered with these details.`
          : `Invited ${email}.`;
      const feedback: InviteFeedbackDetail =
        warnings.length > 0
          ? { variant: 'warning', message: `${baseMessage} ${warnings.join(' ')}` }
          : {
              variant: result && !result.created ? 'primary' : 'success',
              message: baseMessage,
            };

      this.emitCompleted({ reload: true, feedback });
      this.emitClose();
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Failed to invite user';
      this.submitError = message;
      this.emitFeedback({ variant: 'danger', message });
    } finally {
      this.submitting = false;
    }
  }

  private async submitBatchRequest(request: InviteBatchRequest): Promise<void> {
    this.submitting = true;
    this.submitError = null;
    this.refusalLines = [];

    try {
      const response = await apiFetch('/api/v1/admin/users/invite/bulk', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request),
        suppressAccessDeniedToast: true,
      });

      if (!response.ok) {
        const parsed = await parseApiError(response, `HTTP ${response.status}`);
        if (response.status === 403) {
          const { isProjectRefusal, isUserReadRequired, refusals } = parseBatchRefusalDetails(
            parsed.details
          );
          if (isProjectRefusal || isUserReadRequired) {
            const projectLabels = this.buildProjectLabels();
            const roleLabels = this.buildRoleLabels();
            this.submitError =
              parsed.message ||
              'One or more requested project memberships cannot be granted; nothing was created.';
            this.refusalLines =
              refusals.length > 0
                ? refusals.map((r) => formatProjectRefusalLine(r, projectLabels, roleLabels))
                : isUserReadRequired
                  ? ['Assigning project memberships on invite requires user.read permission.']
                  : [];
            return;
          }
        }
        this.submitError = parsed.message || `HTTP ${response.status}`;
        return;
      }

      const data = (await response.json()) as InviteBatchResponse;
      this.batchResponse = data;
      this.emitCompleted({ reload: true });
    } catch (err) {
      this.submitError = err instanceof Error ? err.message : 'Failed to invite users';
    } finally {
      this.submitting = false;
    }
  }

  private async handleRetryBatch(): Promise<void> {
    if (!this.lastBatchRequest || this.submitting) return;
    await this.submitBatchRequest(this.lastBatchRequest);
  }

  private handleDialogRequestClose(e: Event): void {
    if (e.target !== e.currentTarget) return;
    if (this.submitting) {
      e.preventDefault();
      return;
    }
    this.emitClose();
  }

  private renderProjectRow(row: ProjectMembershipRow) {
    const grantableRoles = row.assignableRoles.filter((r) => r.grantable);
    const maxRolesReached = row.selectedRoleIds.length >= INVITE_BULK_MAX_ROLES_PER_PROJECT;

    return html`
      <div class="project-row" data-project-row=${row.rowId}>
        <div class="project-row-top">
          <scion-project-picker
            class="project-row-picker"
            label=""
            placeholder="Select a project..."
            size="small"
            browse-on-focus
            hoist
            require-selection
            .value=${row.projectId}
            .filterProject=${this.filterProjectForRow(row.rowId)}
            ?disabled=${this.submitting}
            @project-change=${(e: CustomEvent<ProjectChangeDetail>) =>
              this.handleProjectChange(row.rowId, e)}
          ></scion-project-picker>
          <sl-icon-button
            name="x-lg"
            label="Remove project"
            ?disabled=${this.submitting}
            @click=${() => this.removeProjectRow(row.rowId)}
          ></sl-icon-button>
        </div>

        ${row.projectId
          ? row.loadingRoles
            ? html`
                <div class="roles-loading">
                  <sl-spinner></sl-spinner>
                  <span>Loading assignable roles...</span>
                </div>
              `
            : row.rolesError
              ? html`<div class="roles-error">${row.rolesError}</div>`
              : row.assignableRoles.length === 0
                ? html`<div class="roles-empty">
                    No assignable roles available for this project.
                  </div>`
                : html`
                    <div class="roles-list">
                      <span class="roles-label">
                        Project roles (select 1–${INVITE_BULK_MAX_ROLES_PER_PROJECT}):
                        ${grantableRoles.length === 0 ? ' None grantable.' : ''}
                      </span>
                      ${row.assignableRoles.map((role) => {
                        const checked = row.selectedRoleIds.includes(role.id);
                        const disabled =
                          this.submitting || !role.grantable || (!checked && maxRolesReached);
                        return html`
                          <div class="role-option">
                            <sl-checkbox
                              size="small"
                              ?checked=${checked}
                              ?disabled=${disabled}
                              data-role-id=${role.id}
                              @sl-change=${(e: Event) => {
                                const target = e.target as HTMLInputElement;
                                this.toggleRoleSelection(row.rowId, role.id, target.checked);
                              }}
                            >
                              ${role.name}
                            </sl-checkbox>
                            ${!role.grantable
                              ? html`
                                  <span class="role-option-reason">
                                    ${role.reason || 'Not grantable by your account'}
                                  </span>
                                `
                              : nothing}
                          </div>
                        `;
                      })}
                    </div>
                  `
          : nothing}
      </div>
    `;
  }

  private renderProjectsSection() {
    if (!this.canReadUsers) {
      return html`
        <p class="projects-hint" data-testid="projects-permission-hint">
          Assigning project memberships on invite requires <code>user.read</code> permission.
        </p>
      `;
    }

    return html`
      <div class="projects-section">
        <div class="projects-header">
          <span class="projects-title">Project memberships (optional)</span>
          <sl-button
            size="small"
            variant="default"
            ?disabled=${this.submitting || this.projectRows.length >= INVITE_BULK_MAX_PROJECTS}
            @click=${() => this.addProjectRow()}
          >
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add project
          </sl-button>
        </div>

        ${this.projectRows.length === 0
          ? html`
              <p class="projects-hint">
                Optionally pre-assign project roles to invited users. Roles take effect once they
                sign in.
              </p>
            `
          : this.projectRows.map((row) => this.renderProjectRow(row))}
      </div>
    `;
  }

  private renderFormView() {
    const parsed = this.parsedAddressInput;
    const capErr = this.capError;

    return html`
      <div class="invite-form">
        ${this.mode === 'invite'
          ? html`
              <sl-textarea
                label="Email addresses"
                placeholder="user@example.com&#10;alice@example.com, bob@example.com"
                help-text="Enter one or more email addresses separated by new lines or commas."
                rows="3"
                resize="auto"
                .value=${this.rawAddresses}
                ?disabled=${this.submitting}
                @sl-input=${(e: Event) => {
                  this.rawAddresses = (e.target as HTMLTextAreaElement).value;
                  this.submitError = null;
                  this.refusalLines = [];
                }}
                required
              ></sl-textarea>
              ${parsed.totalTokens > 0
                ? html`
                    <div class="address-summary" data-testid="address-summary">
                      ${parsed.addresses.length}
                      ${parsed.addresses.length === 1 ? 'address' : 'addresses'}
                      ${parsed.duplicateCount > 0
                        ? ` (${parsed.duplicateCount} ${parsed.duplicateCount === 1 ? 'duplicate' : 'duplicates'} removed)`
                        : ''}
                    </div>
                  `
                : nothing}
              ${this.showDisplayNameField
                ? html`
                    <sl-input
                      label="Display name (optional)"
                      placeholder="e.g., Alice Smith"
                      help-text="Replaced at first sign-in by the name from the sign-in provider, if it supplies one."
                      maxlength="128"
                      .value=${this.displayName}
                      ?disabled=${this.submitting}
                      @sl-input=${(e: Event) => {
                        this.displayName = (e.target as HTMLInputElement).value;
                      }}
                    ></sl-input>
                  `
                : nothing}
              <sl-input
                label="Note (optional)"
                placeholder="e.g., New hire, Q3 contractor"
                .value=${this.note}
                ?disabled=${this.submitting}
                @sl-input=${(e: Event) => {
                  this.note = (e.target as HTMLInputElement).value;
                }}
              ></sl-input>
            `
          : html`
              <p class="csv-description">
                Upload a CSV file with one email per line. An optional second column can contain
                notes. Users will be created with <strong>invited</strong> status.
              </p>
              <div class="csv-file-row">
                <input
                  type="file"
                  accept=".csv,.txt"
                  id="import-file-input"
                  class="csv-file-input"
                  ?disabled=${this.submitting}
                  @change=${(e: Event) => this.handleCsvFileChange(e)}
                />
                ${this.csvFileName && this.csvEntries.length > 0
                  ? html`
                      <div class="address-summary" data-testid="csv-summary">
                        Loaded ${this.csvEntries.length}
                        ${this.csvEntries.length === 1 ? 'address' : 'addresses'} from
                        ${this.csvFileName}
                        ${this.csvDuplicateCount > 0
                          ? ` (${this.csvDuplicateCount} ${this.csvDuplicateCount === 1 ? 'duplicate' : 'duplicates'} removed)`
                          : ''}
                      </div>
                    `
                  : nothing}
                ${this.csvError ? html`<div class="csv-error">${this.csvError}</div>` : nothing}
              </div>
            `}
        ${this.renderProjectsSection()}

        <p class="invite-role-hint">
          Role is assigned at first sign-in using the hub default (Admin &gt; Server Config).
        </p>

        ${capErr
          ? html`
              <sl-alert variant="warning" open>
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                ${capErr}
              </sl-alert>
            `
          : nothing}
        ${this.submitError
          ? html`
              <sl-alert class="submit-error-alert" variant="danger" open>
                <sl-icon slot="icon" name="exclamation-octagon"></sl-icon>
                <div>${this.submitError}</div>
                ${this.refusalLines.length > 0
                  ? html`
                      <ul class="refusal-list">
                        ${this.refusalLines.map((line) => html`<li>${line}</li>`)}
                      </ul>
                    `
                  : nothing}
              </sl-alert>
            `
          : nothing}
      </div>
    `;
  }

  private renderResultsView(resp: InviteBatchResponse) {
    return html`
      <scion-invite-batch-results
        .response=${resp}
        .projectLabels=${this.buildProjectLabels()}
        .roleLabels=${this.buildRoleLabels()}
      ></scion-invite-batch-results>
    `;
  }

  private get dialogLabel(): string {
    if (this.batchResponse) {
      return this.mode === 'csv' ? 'Import Results' : 'Invite Results';
    }
    return this.mode === 'csv' ? 'Import Users from CSV' : 'Invite Users';
  }

  private get submitButtonLabel(): string {
    if (this.mode === 'csv') {
      return this.csvEntries.length > 0 ? `Import ${this.csvEntries.length}` : 'Import';
    }
    const count = this.parsedAddressInput.addresses.length;
    if (count > 1) return `Invite ${count} Users`;
    return 'Invite User';
  }

  override render() {
    if (!this.open) return nothing;

    const resp = this.batchResponse;

    return html`
      <sl-dialog
        label=${this.dialogLabel}
        open
        @sl-request-close=${(e: Event) => this.handleDialogRequestClose(e)}
      >
        ${resp ? this.renderResultsView(resp) : this.renderFormView()}

        <div slot="footer" class="footer-actions">
          ${resp
            ? html`
                ${!resp.complete
                  ? html`
                      <sl-button
                        variant="default"
                        ?disabled=${this.submitting}
                        @click=${() => {
                          this.batchResponse = null;
                        }}
                      >
                        Back
                      </sl-button>
                      <sl-button
                        variant="primary"
                        ?loading=${this.submitting}
                        @click=${() => this.handleRetryBatch()}
                      >
                        <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                        Retry batch
                      </sl-button>
                    `
                  : html`
                      <sl-button
                        variant="default"
                        @click=${() => {
                          this.resetState();
                        }}
                      >
                        ${this.mode === 'csv' ? 'Import another' : 'Invite more'}
                      </sl-button>
                    `}
                <sl-button
                  variant=${resp.complete ? 'primary' : 'default'}
                  ?disabled=${this.submitting}
                  @click=${() => this.emitClose()}
                >
                  Done
                </sl-button>
              `
            : html`
                <sl-button
                  variant="default"
                  ?disabled=${this.submitting}
                  @click=${() => this.emitClose()}
                >
                  Cancel
                </sl-button>
                <sl-button
                  variant="primary"
                  ?loading=${this.submitting}
                  ?disabled=${!this.canSubmit}
                  @click=${() => this.handleSubmit()}
                >
                  ${this.submitButtonLabel}
                </sl-button>
              `}
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-invite-users-dialog': ScionInviteUsersDialog;
  }
}
