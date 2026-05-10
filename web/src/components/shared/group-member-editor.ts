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
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { GroupMember, AdminGroup } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';

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

  // Available groups for the dropdown
  @state() private availableGroups: AdminGroup[] = [];
  @state() private groupsLoading = false;

  // User search autocomplete state
  @state() private userSearchQuery = '';
  @state() private userSearchResults: Array<{ id: string; email: string; displayName: string }> = [];
  @state() private userSearchLoading = false;
  @state() private userSearchOpen = false;
  private userSearchDebounceTimer: ReturnType<typeof setTimeout> | null = null;

  // Removing state
  @state() private removingMember: string | null = null;

  static override styles = css`
    :host {
      display: block;
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

    .dialog-hint {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0.5rem 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    /* User search autocomplete */
    .user-search-container {
      position: relative;
    }

    .user-search-dropdown {
      position: absolute;
      top: 100%;
      left: 0;
      right: 0;
      z-index: 1000;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      box-shadow: 0 4px 12px rgba(0, 0, 0, 0.1);
      max-height: 200px;
      overflow-y: auto;
      margin-top: 0.25rem;
    }

    .user-search-option {
      display: flex;
      flex-direction: column;
      padding: 0.5rem 0.75rem;
      cursor: pointer;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .user-search-option:last-child {
      border-bottom: none;
    }

    .user-search-option:hover,
    .user-search-option.focused {
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .user-search-option .user-name {
      font-weight: 500;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .user-search-option .user-email {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .user-search-empty,
    .user-search-loading {
      padding: 0.75rem;
      text-align: center;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
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

  private async loadMembers(): Promise<void> {
    if (!this.groupId) return;

    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(
        `/api/v1/groups/${encodeURIComponent(this.groupId)}/members`
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      const data = (await response.json()) as { members?: GroupMember[] } | GroupMember[];
      this.members = Array.isArray(data) ? data : data.members || [];
    } catch (err) {
      console.error('Failed to load members:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load members';
    } finally {
      this.loading = false;
    }
  }

  private async loadAvailableGroups(): Promise<void> {
    this.groupsLoading = true;
    try {
      const response = await apiFetch('/api/v1/groups?groupType=explicit&limit=100');
      if (response.ok) {
        const data = (await response.json()) as { groups?: AdminGroup[] } | AdminGroup[];
        const groups = Array.isArray(data) ? data : data.groups || [];
        // Filter out the current group to prevent self-membership
        this.availableGroups = groups.filter((g) => g.id !== this.groupId);
      }
    } catch (err) {
      console.error('Failed to load groups:', err);
    } finally {
      this.groupsLoading = false;
    }
  }

  private handleUserSearchInput(e: Event): void {
    const value = (e.target as HTMLInputElement).value;
    this.userSearchQuery = value;
    this.addMemberInput = value;

    if (this.userSearchDebounceTimer) {
      clearTimeout(this.userSearchDebounceTimer);
    }

    if (value.trim().length < 2) {
      this.userSearchResults = [];
      this.userSearchOpen = false;
      return;
    }

    this.userSearchDebounceTimer = setTimeout(() => {
      void this.searchUsers(value.trim());
    }, 250);
  }

  private async searchUsers(query: string): Promise<void> {
    this.userSearchLoading = true;
    this.userSearchOpen = true;
    try {
      const response = await apiFetch(
        `/api/v1/users?search=${encodeURIComponent(query)}&limit=10`
      );
      if (response.ok) {
        const data = (await response.json()) as { users?: Array<{ id: string; email: string; displayName: string }> };
        this.userSearchResults = data.users || [];
      }
    } catch (err) {
      console.error('Failed to search users:', err);
      this.userSearchResults = [];
    } finally {
      this.userSearchLoading = false;
    }
  }

  private selectUser(user: { id: string; email: string; displayName: string }): void {
    this.addMemberInput = user.email;
    this.userSearchQuery = user.displayName ? `${user.displayName} (${user.email})` : user.email;
    this.userSearchOpen = false;
    this.userSearchResults = [];
  }

  private openAddDialog(): void {
    this.addMemberType = 'user';
    this.addMemberInput = '';
    this.addMemberRole = 'member';
    this.addMemberError = null;
    this.userSearchQuery = '';
    this.userSearchResults = [];
    this.userSearchOpen = false;
    this.addDialogOpen = true;
    void this.loadAvailableGroups();
  }

  private closeAddDialog(): void {
    this.addDialogOpen = false;
  }

  private async handleAddMember(e: Event): Promise<void> {
    e.preventDefault();

    if (!this.addMemberInput.trim()) {
      this.addMemberError = this.addMemberType === 'user'
        ? 'Please search for and select a user'
        : this.addMemberType === 'group'
          ? 'Please select a group'
          : 'Member ID is required';
      return;
    }

    this.addMemberLoading = true;
    this.addMemberError = null;

    try {
      const response = await apiFetch(
        `/api/v1/groups/${encodeURIComponent(this.groupId)}/members`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            memberType: this.addMemberType,
            memberId: this.addMemberInput.trim(),
            role: this.addMemberRole,
          }),
        }
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      this.closeAddDialog();
      await this.loadMembers();
    } catch (err) {
      console.error('Failed to add member:', err);
      this.addMemberError = err instanceof Error ? err.message : 'Failed to add member';
    } finally {
      this.addMemberLoading = false;
    }
  }

  private async handleRemoveMember(member: GroupMember): Promise<void> {
    const displayName = member.displayName || member.memberId;
    if (!confirm(`Remove ${member.memberType} "${displayName}" from this group?`)) {
      return;
    }

    const memberKey = `${member.memberType}/${member.memberId}`;
    this.removingMember = memberKey;

    try {
      const response = await apiFetch(
        `/api/v1/groups/${encodeURIComponent(this.groupId)}/members/${encodeURIComponent(member.memberType)}/${encodeURIComponent(member.memberId)}`,
        { method: 'DELETE' }
      );

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, `Failed to remove member (HTTP ${response.status})`));
      }

      await this.loadMembers();
    } catch (err) {
      console.error('Failed to remove member:', err);
      alert(err instanceof Error ? err.message : 'Failed to remove member');
    } finally {
      this.removingMember = null;
    }
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

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(-diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(-diffMinutes, 'minute');
      } else if (Math.abs(diffHours) < 24) {
        return rtf.format(-diffHours, 'hour');
      } else {
        return rtf.format(-diffDays, 'day');
      }
    } catch {
      return dateString;
    }
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

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderStandalone();
  }

  private renderStandalone() {
    return html`
      <div class="list-header">
        <h2>
          ${this.sectionTitle}
          <span class="member-count">(${this.members.length})</span>
        </h2>
        ${!this.readOnly
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openAddDialog}>
                <sl-icon slot="prefix" name="person-plus"></sl-icon>
                Add Member
              </sl-button>
            `
          : nothing}
      </div>

      ${this.error
        ? html`<div class="error-state">${this.error}</div>`
        : nothing}

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
            <h2>${this.sectionTitle} <span class="member-count">(${this.members.length})</span></h2>
            ${this.sectionDescription
              ? html`<p>${this.sectionDescription}</p>`
              : nothing}
          </div>
          ${!this.readOnly
            ? html`
                <sl-button size="small" variant="default" @click=${this.openAddDialog}>
                  <sl-icon slot="prefix" name="person-plus"></sl-icon>
                  Add Member
                </sl-button>
              `
            : nothing}
        </div>

        ${this.error
          ? html`<div class="error-state">${this.error}</div>`
          : nothing}

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
        <table>
          <thead>
            <tr>
              <th>Member</th>
              <th>Role</th>
              <th class="hide-mobile">Added</th>
              ${!this.readOnly ? html`<th class="actions-cell"></th>` : nothing}
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
            <div class="member-icon ${member.memberType}">
              <sl-icon name="${this.getMemberIcon(member.memberType)}"></sl-icon>
            </div>
            <div class="member-info">
              <span class="member-name">${displayName}</span>
              <span class="member-detail">
                ${member.memberType}${showId ? html` &middot; <span class="member-id">${member.memberId}</span>` : nothing}
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
        ${!this.readOnly
          ? html`
              <td class="actions-cell">
                <sl-icon-button
                  name="trash"
                  label="Remove member"
                  ?disabled=${isRemoving}
                  @click=${() => this.handleRemoveMember(member)}
                ></sl-icon-button>
              </td>
            `
          : nothing}
      </tr>
    `;
  }

  private renderEmptyMembers() {
    return html`
      <div class="empty-state">
        <sl-icon name="people"></sl-icon>
        <h3>No Members</h3>
        <p>This group doesn't have any members yet.</p>
        ${!this.readOnly
          ? html`
              <sl-button variant="primary" size="small" @click=${this.openAddDialog}>
                <sl-icon slot="prefix" name="person-plus"></sl-icon>
                Add Member
              </sl-button>
            `
          : nothing}
      </div>
    `;
  }

  private renderAddDialog() {
    if (this.readOnly) return nothing;

    const inputLabel = this.addMemberType === 'user'
      ? 'User'
      : this.addMemberType === 'group'
        ? 'Group'
        : 'Agent ID';

    const inputHint = this.addMemberType === 'user'
      ? 'Enter the user\'s email address'
      : this.addMemberType === 'group'
        ? 'Select a group to add as a member'
        : 'Enter the agent ID';

    return html`
      <sl-dialog
        label="Add Member"
        ?open=${this.addDialogOpen}
        @sl-request-close=${this.closeAddDialog}
      >
        <form class="dialog-form" @submit=${this.handleAddMember}>
          <sl-select
            label="Member Type"
            value=${this.addMemberType}
            @sl-change=${(e: Event) => {
              this.addMemberType = (e.target as HTMLSelectElement).value;
              this.addMemberInput = '';
              this.addMemberError = null;
              this.userSearchQuery = '';
              this.userSearchResults = [];
              this.userSearchOpen = false;
            }}
          >
            <sl-option value="user">
              <sl-icon slot="prefix" name="person"></sl-icon>
              User
            </sl-option>
            <sl-option value="group">
              <sl-icon slot="prefix" name="diagram-3"></sl-icon>
              Group
            </sl-option>
            <sl-option value="agent">
              <sl-icon slot="prefix" name="cpu"></sl-icon>
              Agent
            </sl-option>
          </sl-select>

          ${this.addMemberType === 'group'
            ? html`
                <sl-select
                  label=${inputLabel}
                  placeholder="Select a group..."
                  value=${this.addMemberInput}
                  ?disabled=${this.groupsLoading}
                  @sl-change=${(e: Event) => {
                    this.addMemberInput = (e.target as HTMLSelectElement).value;
                  }}
                >
                  ${this.groupsLoading
                    ? html`<sl-option value="" disabled>Loading groups...</sl-option>`
                    : this.availableGroups.length === 0
                      ? html`<sl-option value="" disabled>No groups available</sl-option>`
                      : this.availableGroups.map(
                          (g) => html`<sl-option value=${g.id}>${g.name} <small>(${g.slug})</small></sl-option>`
                        )}
                </sl-select>
              `
            : this.addMemberType === 'user'
              ? html`
                  <div class="user-search-container">
                    <sl-input
                      label=${inputLabel}
                      placeholder="Search by name or email..."
                      value=${this.userSearchQuery}
                      type="text"
                      autocomplete="off"
                      @sl-input=${this.handleUserSearchInput}
                      @sl-focus=${() => {
                        if (this.userSearchResults.length > 0) this.userSearchOpen = true;
                      }}
                      @sl-blur=${() => {
                        // Delay to allow click on dropdown
                        setTimeout(() => { this.userSearchOpen = false; }, 200);
                      }}
                      required
                    ></sl-input>
                    ${this.userSearchOpen
                      ? html`
                          <div class="user-search-dropdown">
                            ${this.userSearchLoading
                              ? html`<div class="user-search-loading"><sl-spinner></sl-spinner> Searching...</div>`
                              : this.userSearchResults.length === 0
                                ? html`<div class="user-search-empty">No users found</div>`
                                : this.userSearchResults.map(
                                    (user) => html`
                                      <div
                                        class="user-search-option"
                                        @mousedown=${(e: Event) => {
                                          e.preventDefault();
                                          this.selectUser(user);
                                        }}
                                      >
                                        <span class="user-name">${user.displayName || user.email}</span>
                                        ${user.displayName
                                          ? html`<span class="user-email">${user.email}</span>`
                                          : nothing}
                                      </div>
                                    `
                                  )}
                          </div>
                        `
                      : nothing}
                  </div>
                `
              : html`
                  <sl-input
                    label=${inputLabel}
                    placeholder=${inputHint}
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
            @sl-change=${(e: Event) => {
              this.addMemberRole = (e.target as HTMLSelectElement).value;
            }}
          >
            <sl-option value="member">Member</sl-option>
            <sl-option value="admin">Admin</sl-option>
            <sl-option value="owner">Owner</sl-option>
          </sl-select>

          ${this.addMemberError
            ? html`<div class="dialog-error">${this.addMemberError}</div>`
            : nothing}
        </form>

        <sl-button
          slot="footer"
          variant="default"
          @click=${this.closeAddDialog}
          ?disabled=${this.addMemberLoading}
        >
          Cancel
        </sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.addMemberLoading}
          ?disabled=${this.addMemberLoading}
          @click=${this.handleAddMember}
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
