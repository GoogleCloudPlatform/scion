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
 * Shared Group Member Editor Component
 *
 * Reusable component for viewing and managing group members.
 * Used by both the admin group detail page and the project settings page.
 *
 * Supports adding users (by email), groups (by name/slug), and agents.
 * Displays human-friendly names for all member types.
 *
 * When the optional `capabilities` property is set, add/remove affordances
 * are gated by the server-provided capability actions (`addMember`,
 * `removeMember`).  When `capabilities` is unset, the component falls back
 * to the legacy `readOnly` flag for full backward compatibility with the
 * project settings page.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { GroupMember } from '../../shared/types.js';
import type { Capabilities } from '../../shared/groups.js';
import type { PrincipalChangeDetail } from './principal-picker.js';
import type { SecurityReviewDetail } from './security-review-dialog.js';
import { parseSecurityReviewResponse, parseLockoutResponse } from './security-review-dialog.js';
import { listMembers, addMember, removeMember, GroupsApiError } from '../../client/groups-api.js';
import { showToast } from '../../utils/toast.js';
import { formatRelativeTime } from '../../utils/time.js';
import { showConfirm } from './confirm-dialog.js';
import './principal-picker.js';
import './security-review-dialog.js';

@customElement('scion-group-member-editor')
export class ScionGroupMemberEditor extends LitElement {
  /** The group ID to manage members for */
  @property() groupId = '';

  /** Whether the editor is read-only (no add/remove actions) */
  @property({ type: Boolean }) readOnly = false;

  /** Whether to render in compact section layout */
  @property({ type: Boolean }) compact = false;

  /** Section title override */
  @property() sectionTitle = 'Members';

  /** Section description override */
  @property() sectionDescription = '';

  /** Server capabilities for the owning group; splits add vs remove affordances. */
  @property({ attribute: false }) capabilities?: Capabilities;

  @state() private loading = true;
  @state() private members: GroupMember[] = [];
  @state() private error: string | null = null;

  // Add dialog state
  @state() private addDialogOpen = false;
  @state() private addMemberType = 'user';
  @state() private addMemberInput = '';
  @state() private addMemberRole = 'member';
  @state() private addMemberLoading = false;
  @state() private addMemberError: string | null = null;
  @state() private addMemberErrorTitle: string | null = null;
  @state() private addMemberErrorHint: string | null = null;

  // Removing state
  @state() private removingMember: string | null = null;

  // Security review dialog state
  @state() private securityReviewDetail: SecurityReviewDetail | null = null;
  @state() private showSecurityReview = false;

  /* ------------------------------------------------------------------ */
  /* Capability-gated affordances                                       */
  /* ------------------------------------------------------------------ */

  /**
   * Whether the "Add Member" button should render.
   *
   * When `capabilities` is unset, falls back to `!readOnly` for backward
   * compatibility with the project settings page.
   */
  private get canAdd(): boolean {
    if (this.readOnly) return false;
    // When capabilities is provided (even if empty), use capability-driven logic (fail-closed).
    // When capabilities is not provided, fall back to !readOnly for backward compat (project settings).
    if (this.capabilities !== undefined) {
      return this.capabilities.actions.includes('addMember');
    }
    return true;
  }

  /**
   * Whether per-row remove buttons should render.
   *
   * When `capabilities` is unset, falls back to `!readOnly` for backward
   * compatibility with the project settings page.
   */
  private get canRemove(): boolean {
    if (this.readOnly) return false;
    // When capabilities is provided (even if empty), use capability-driven logic (fail-closed).
    // When capabilities is not provided, fall back to !readOnly for backward compat (project settings).
    if (this.capabilities !== undefined) {
      return this.capabilities.actions.includes('removeMember');
    }
    return true;
  }

  /**
   * Whether a member is the sole user-type owner of this group.
   *
   * When true, the remove button is disabled with a tooltip explaining
   * that a group must keep at least one owner.  The server enforces
   * this constraint as well; the proactive disable prevents a dead-end click.
   */
  private isSoleUserOwner(member: GroupMember): boolean {
    if (member.role !== 'owner' || member.memberType !== 'user') return false;
    const userOwnerCount = this.members.filter(
      (m) => m.role === 'owner' && m.memberType === 'user'
    ).length;
    return userOwnerCount === 1;
  }

  static override styles = css`
    :host {
      display: block;
    }

    /* Screen-reader-only utility */
    .sr-only {
      position: absolute;
      width: 1px;
      height: 1px;
      padding: 0;
      margin: -1px;
      overflow: hidden;
      clip: rect(0, 0, 0, 0);
      white-space: nowrap;
      border: 0;
    }

    /* Section layout (compact mode) */
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

    /* Standalone header */
    .list-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .list-header h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .member-count {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin-left: 0.5rem;
      font-weight: 400;
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
      color: var(--scion-text-secondary, #475569);
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
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
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

    .member-detail .member-id {
      font-family: var(--scion-font-mono, monospace);
    }

    /* Role badge */
    .role-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .role-badge.member {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .role-badge.admin {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #b45309);
    }

    .role-badge.owner {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .actions-cell {
      text-align: right;
    }

    /* Empty state */
    .empty-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .compact .empty-state {
      padding: 2rem 1.5rem;
      border: none;
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

    /* Dialog */
    .dialog-form {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .dialog-error {
      color: var(--sl-color-danger-600, #dc2626);
      font-size: 0.875rem;
      padding: 0.5rem 0.75rem;
      background: var(--sl-color-danger-50, #fef2f2);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .dialog-error strong {
      display: block;
      margin-bottom: 0.25rem;
    }

    .dialog-hint {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    @media (max-width: 768px) {
      .hide-mobile {
        display: none;
      }
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.groupId) {
      void this.loadMembers();
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('groupId') && this.groupId) {
      void this.loadMembers();
    }
  }

  /* ------------------------------------------------------------------ */
  /* Data loading — uses groups-api adapter                              */
  /* ------------------------------------------------------------------ */

  private async loadMembers(): Promise<void> {
    if (!this.groupId) return;

    this.loading = true;
    this.error = null;

    try {
      this.members = await listMembers(this.groupId);
    } catch (err) {
      console.error('Failed to load members:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load members';
    } finally {
      this.loading = false;
    }
  }

  /* ------------------------------------------------------------------ */
  /* Add-member dialog                                                  */
  /* ------------------------------------------------------------------ */

  private handlePrincipalChange(e: CustomEvent<PrincipalChangeDetail>): void {
    this.addMemberInput = e.detail.principalId;
    this.clearAddErrors();

    // Inline self-membership rejection for groups.
    if (this.addMemberType === 'group' && e.detail.principalId === this.groupId) {
      this.addMemberError = 'A group cannot contain itself';
    }
  }

  private openAddDialog(): void {
    this.addMemberType = 'user';
    this.addMemberInput = '';
    this.addMemberRole = 'member';
    this.clearAddErrors();
    this.addDialogOpen = true;
  }

  private closeAddDialog(): void {
    this.addDialogOpen = false;
    // Return focus to the Add Member button
    requestAnimationFrame(() => {
      const addBtn = this.shadowRoot?.querySelector<HTMLElement>(
        '.list-header sl-button, .section-header sl-button'
      );
      addBtn?.focus();
    });
  }

  private clearAddErrors(): void {
    this.addMemberError = null;
    this.addMemberErrorTitle = null;
    this.addMemberErrorHint = null;
  }

  private async handleAddMember(e: Event): Promise<void> {
    e.preventDefault();

    if (!this.addMemberInput.trim()) {
      this.addMemberError =
        this.addMemberType === 'user'
          ? 'Please search for and select a user'
          : this.addMemberType === 'group'
            ? 'Please select a group'
            : 'Member ID is required';
      return;
    }

    // Self-membership guard (inline, before server round-trip).
    if (this.addMemberType === 'group' && this.addMemberInput.trim() === this.groupId) {
      this.addMemberError = 'A group cannot contain itself';
      return;
    }

    this.addMemberLoading = true;
    this.clearAddErrors();

    try {
      await addMember(this.groupId, {
        memberType: this.addMemberType as 'user' | 'group' | 'agent',
        memberId: this.addMemberInput.trim(),
        role: this.addMemberRole as 'member' | 'admin' | 'owner',
      });

      this.closeAddDialog();
      await this.loadMembers();
    } catch (err) {
      console.error('Failed to add member:', err);
      if (err instanceof GroupsApiError) {
        this.surfaceAddError(err);
      } else {
        this.addMemberError = err instanceof Error ? err.message : 'Failed to add member';
      }
    } finally {
      this.addMemberLoading = false;
    }
  }

  /**
   * Map a discriminated {@link GroupsApiError} to user-facing inline copy
   * per the design spec (section 6.5 error table).
   */
  private surfaceAddError(err: GroupsApiError): void {
    this.clearAddErrors();
    switch (err.kind) {
      case 'cycle':
        this.addMemberError =
          'Adding this group would create a membership cycle ' +
          '(it already contains this group, directly or nested).';
        break;
      case 'quota':
        this.addMemberError =
          'This group has reached its member limit (max_members_per_group). ' +
          'Remove a member or ask an operator to raise the quota.';
        break;
      case 'conflict_member':
        this.addMemberError = 'Already a member of this group.';
        break;
      case 'hierarchy':
        // Server copy is already user-appropriate — show verbatim.
        this.addMemberError = err.message;
        break;
      case 'delegation':
        this.addMemberErrorTitle = 'Insufficient authority to grant this membership';
        this.addMemberError = err.message;
        this.addMemberErrorHint =
          "Membership grants this group's role-binding authority; " +
          'you can only grant authority you hold.';
        break;
      case 'validation':
        this.addMemberError = err.message;
        break;
      default:
        this.addMemberError = err.message;
    }
  }

  /* ------------------------------------------------------------------ */
  /* Remove-member flow                                                 */
  /* ------------------------------------------------------------------ */

  private async handleRemoveMember(member: GroupMember): Promise<void> {
    const displayName = member.displayName || member.memberId;
    if (!(await showConfirm(`Remove ${member.memberType} "${displayName}" from this group?`))) {
      return;
    }

    const memberKey = `${member.memberType}/${member.memberId}`;
    this.removingMember = memberKey;

    try {
      const result = await removeMember(this.groupId, member.memberType, member.memberId);

      if (result.outcome === 'ok') {
        await this.loadMembers();
      } else if (result.outcome === 'lockout') {
        const lockout = parseLockoutResponse(result.rawBody);
        const detail: SecurityReviewDetail = {
          entityLabel: displayName,
          contextLabel: `group ${this.groupId}`,
          boundaries: [],
          canCommit: false,
        };
        if (lockout) {
          detail.lockout = lockout;
        }
        this.securityReviewDetail = detail;
        this.showSecurityReview = true;
      } else if (result.outcome === 'security_review') {
        const reviewDetail = parseSecurityReviewResponse(
          result.rawBody,
          displayName,
          `group ${this.groupId}`
        );
        if (reviewDetail) {
          this.securityReviewDetail = reviewDetail;
        } else {
          // Fallback: the adapter identified security_review but parsing failed
          this.securityReviewDetail = {
            entityLabel: displayName,
            contextLabel: `group ${this.groupId}`,
            boundaries: [],
            canCommit: false,
          };
        }
        this.showSecurityReview = true;
      }
    } catch (err) {
      console.error('Failed to remove member:', err);
      showToast(err instanceof Error ? err.message : 'Failed to remove member');
    } finally {
      this.removingMember = null;
    }
  }

  /* ------------------------------------------------------------------ */
  /* Helpers                                                            */
  /* ------------------------------------------------------------------ */

  private formatRelativeTime(dateString: string): string {
    return formatRelativeTime(dateString);
  }

  private getMemberIcon(memberType: string): string {
    switch (memberType) {
      case 'user':
        return 'person';
      case 'group':
        return 'diagram-3';
      case 'agent':
        return 'cpu';
      default:
        return 'question-circle';
    }
  }

  /* ------------------------------------------------------------------ */
  /* Render                                                             */
  /* ------------------------------------------------------------------ */

  override render() {
    return html`
      ${this.compact ? this.renderCompact() : this.renderStandalone()}
      ${this.renderSecurityReviewDialog()}
    `;
  }

  private renderSecurityReviewDialog() {
    return html`
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

  private renderStandalone() {
    return html`
      <div class="list-header">
        <h2>
          ${this.sectionTitle}
          <span class="member-count" aria-live="polite">(${this.members.length})</span>
        </h2>
        ${this.canAdd
          ? html`
              <sl-button variant="primary" size="small" @click=${() => this.openAddDialog()}>
                <sl-icon slot="prefix" name="person-plus" aria-hidden="true"></sl-icon>
                Add Member
              </sl-button>
            `
          : nothing}
      </div>

      ${this.error ? html`<div class="error-state" role="alert">${this.error}</div>` : nothing}
      ${this.loading
        ? html`<div class="loading-state"><sl-spinner></sl-spinner> Loading members...</div>`
        : this.members.length === 0
          ? this.renderEmptyMembers()
          : this.renderMembersTable()}
      ${this.renderAddDialog()}
    `;
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>
              ${this.sectionTitle}
              <span class="member-count" aria-live="polite">(${this.members.length})</span>
            </h2>
            ${this.sectionDescription ? html`<p>${this.sectionDescription}</p>` : nothing}
          </div>
          ${this.canAdd
            ? html`
                <sl-button size="small" variant="default" @click=${() => this.openAddDialog()}>
                  <sl-icon slot="prefix" name="person-plus" aria-hidden="true"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>

        ${this.error ? html`<div class="error-state" role="alert">${this.error}</div>` : nothing}
        ${this.loading
          ? html`<div class="loading-state"><sl-spinner></sl-spinner> Loading members...</div>`
          : this.members.length === 0
            ? this.renderEmptyMembers()
            : this.renderMembersTable()}
        ${this.renderAddDialog()}
      </div>
    `;
  }

  private renderMembersTable() {
    return html`
      <div class="table-container">
        <table role="table" aria-label="Group members">
          <caption class="sr-only">
            List of group members showing name, role, and date added.
          </caption>
          <thead>
            <tr>
              <th scope="col">Member</th>
              <th scope="col">Role</th>
              <th scope="col" class="hide-mobile">Added</th>
              ${this.canRemove
                ? html`<th scope="col" class="actions-cell">
                    <span class="sr-only">Actions</span>
                  </th>`
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

  private renderMemberRow(member: GroupMember) {
    const memberKey = `${member.memberType}/${member.memberId}`;
    const isRemoving = this.removingMember === memberKey;
    const displayName = member.displayName || member.memberId;
    const showId = member.displayName && member.displayName !== member.memberId;

    return html`
      <tr>
        <td>
          <div class="member-identity">
            <div class="member-icon ${member.memberType}" aria-hidden="true">
              <sl-icon name="${this.getMemberIcon(member.memberType)}"></sl-icon>
            </div>
            <div class="member-info">
              <span class="member-name">${displayName}</span>
              <span class="member-detail">
                ${member.memberType}${showId
                  ? html` &middot; <span class="member-id">${member.memberId}</span>`
                  : nothing}
              </span>
            </div>
          </div>
        </td>
        <td>
          <span class="role-badge ${member.role}">${member.role}</span>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(member.addedAt)}</span>
        </td>
        ${this.canRemove
          ? html`
              <td class="actions-cell">
                ${this.isSoleUserOwner(member)
                  ? html`
                      <sl-tooltip content="A group must keep at least one owner.">
                        <sl-icon-button
                          name="trash"
                          label="Remove member"
                          disabled
                        ></sl-icon-button>
                      </sl-tooltip>
                    `
                  : html`
                      <sl-icon-button
                        name="trash"
                        label="Remove member"
                        ?disabled=${isRemoving}
                        @click=${() => this.handleRemoveMember(member)}
                      ></sl-icon-button>
                    `}
              </td>
            `
          : nothing}
      </tr>
    `;
  }

  private renderEmptyMembers() {
    return html`
      <div class="empty-state">
        <sl-icon name="people" aria-hidden="true"></sl-icon>
        <h3>No Members</h3>
        <p>This group doesn't have any members yet.</p>
        ${this.canAdd
          ? html`
              <sl-button variant="primary" size="small" @click=${() => this.openAddDialog()}>
                <sl-icon slot="prefix" name="person-plus" aria-hidden="true"></sl-icon>
                Add Member
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  private renderAddDialog() {
    if (!this.canAdd) return nothing;

    const inputLabel =
      this.addMemberType === 'user'
        ? 'User'
        : this.addMemberType === 'group'
          ? 'Group'
          : 'Agent ID';

    return html`
      <sl-dialog
        label="Add Member"
        ?open=${this.addDialogOpen}
        @sl-request-close=${(e: Event) => {
          if (this.addMemberLoading) {
            e.preventDefault();
            return;
          }
          this.closeAddDialog();
        }}
      >
        <form class="dialog-form" @submit=${(e: Event) => this.handleAddMember(e)}>
          <sl-select
            label="Member Type"
            value=${this.addMemberType}
            @sl-change=${(e: Event) => {
              this.addMemberType = (e.target as HTMLSelectElement).value;
              this.addMemberInput = '';
              this.clearAddErrors();
            }}
          >
            <sl-option value="user">
              <sl-icon slot="prefix" name="person" aria-hidden="true"></sl-icon>
              User
            </sl-option>
            <sl-option value="group">
              <sl-icon slot="prefix" name="diagram-3" aria-hidden="true"></sl-icon>
              Group
            </sl-option>
            <sl-option value="agent">
              <sl-icon slot="prefix" name="cpu" aria-hidden="true"></sl-icon>
              Agent
            </sl-option>
          </sl-select>

          ${this.addMemberType === 'group'
            ? html`
                <scion-principal-picker
                  principalType="group"
                  label=${inputLabel}
                  @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) =>
                    this.handlePrincipalChange(e)}
                ></scion-principal-picker>
              `
            : this.addMemberType === 'user'
              ? html`
                  <scion-principal-picker
                    principalType="user"
                    label=${inputLabel}
                    @principal-change=${(e: CustomEvent<PrincipalChangeDetail>) =>
                      this.handlePrincipalChange(e)}
                  ></scion-principal-picker>
                `
              : html`
                  <sl-input
                    label=${inputLabel}
                    placeholder="Enter the agent ID"
                    value=${this.addMemberInput}
                    type="text"
                    @sl-input=${(e: Event) => {
                      this.addMemberInput = (e.target as HTMLInputElement).value;
                    }}
                    required
                  ></sl-input>
                `}

          <sl-select
            label="Role"
            value=${this.addMemberRole}
            help-text=${'Governance role inside this group — controls who may manage members. It does not grant resource permissions.'}
            @sl-change=${(e: Event) => {
              this.addMemberRole = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="member">Member</sl-option>
            <sl-option value="admin">Admin</sl-option>
            <sl-option value="owner">Owner</sl-option>
          </sl-select>

          ${this.addMemberError
            ? html`<div class="dialog-error" role="alert">
                ${this.addMemberErrorTitle
                  ? html`<strong>${this.addMemberErrorTitle}</strong>`
                  : nothing}
                ${this.addMemberError}
              </div>`
            : nothing}
          ${this.addMemberErrorHint
            ? html`<div class="dialog-hint">${this.addMemberErrorHint}</div>`
            : nothing}
        </form>

        <sl-button
          slot="footer"
          variant="default"
          @click=${() => this.closeAddDialog()}
          ?disabled=${this.addMemberLoading}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.addMemberLoading}
          ?disabled=${this.addMemberLoading}
          @click=${(e: Event) => this.handleAddMember(e)}
        >
          Add Member
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-group-member-editor': ScionGroupMemberEditor;
  }
}
