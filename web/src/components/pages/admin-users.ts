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
 * Admin Users page component
 *
 * View of all users with admin actions: promote/demote, suspend/reactivate, delete.
 * Supports invite-based user creation and status filtering (invited/active/suspended).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { AdminUser, UserRole } from '../../shared/types.js';
import '../shared/status-badge.js';
import { apiFetch, extractApiError } from '../../client/api.js';

type SortField = 'name' | 'created';
type SortDir = 'asc' | 'desc';
type AdminTab = 'users' | 'invites';
type StatusFilter = 'all' | 'invited' | 'active' | 'suspended';

interface ConfirmAction {
  title: string;
  message: string;
  variant: 'primary' | 'danger' | 'warning';
  confirmLabel: string;
  user: AdminUser;
  action: () => Promise<void>;
}

interface InviteCodeEntry {
  id: string;
  codePrefix: string;
  maxUses: number;
  useCount: number;
  expiresAt: string;
  revoked: boolean;
  createdBy: string;
  note: string;
  created: string;
}

interface InviteCreateResult {
  code: string;
  inviteUrl: string;
  invite: InviteCodeEntry;
}

const EXPIRY_PRESETS = [
  { label: '5 minutes', value: '5m' },
  { label: '15 minutes', value: '15m' },
  { label: '30 minutes', value: '30m' },
  { label: '1 hour', value: '1h' },
  { label: '4 hours', value: '4h' },
  { label: '12 hours', value: '12h' },
  { label: '24 hours', value: '24h' },
  { label: '3 days', value: '72h' },
  { label: '5 days', value: '120h' },
];

const PAGE_SIZE = 50;

@customElement('scion-page-admin-users')
export class ScionPageAdminUsers extends LitElement {
  @state()
  private loading = true;

  @state()
  private users: AdminUser[] = [];

  @state()
  private error: string | null = null;

  @state()
  private sortField: SortField = 'name';

  @state()
  private sortDir: SortDir = 'asc';

  @state()
  private totalCount = 0;

  @state()
  private currentPage = 1;

  @state()
  private nextCursor: string | null = null;

  @state()
  private cursorHistory: string[] = [];

  @state()
  private currentUserId: string | null = null;

  @state()
  private confirmAction: ConfirmAction | null = null;

  @state()
  private actionInProgress = false;

  @state()
  private actionFeedback: { message: string; variant: 'success' | 'danger' } | null = null;

  @state()
  private activeTab: AdminTab = 'users';

  @state()
  private statusFilter: StatusFilter = 'all';

  // Invite user dialog state
  @state()
  private showInviteUserDialog = false;

  @state()
  private inviteUserEmail = '';

  @state()
  private inviteUserNote = '';

  @state()
  private inviteUserInProgress = false;

  // Bulk import dialog state
  @state()
  private showImportDialog = false;

  @state()
  private importInProgress = false;

  // Invite codes tab state
  @state()
  private invites: InviteCodeEntry[] = [];

  @state()
  private invitesLoading = false;

  @state()
  private invitesTotalCount = 0;

  @state()
  private showCreateInviteDialog = false;

  @state()
  private createInviteExpiry = '1h';

  @state()
  private createInviteMaxUses = 1;

  @state()
  private createInviteNote = '';

  @state()
  private createInviteInProgress = false;

  @state()
  private createdInviteResult: InviteCreateResult | null = null;

  @state()
  private inviteCopied = false;

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

    .user-count {
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

    th.sortable {
      cursor: pointer;
      user-select: none;
    }

    th.sortable:hover {
      color: var(--scion-text, #1e293b);
    }

    .sort-indicator {
      display: inline-block;
      margin-left: 0.25rem;
      font-size: 0.625rem;
      vertical-align: middle;
      opacity: 0.4;
    }

    th.sorted .sort-indicator {
      opacity: 1;
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

    .user-identity {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .user-avatar {
      width: 2rem;
      height: 2rem;
      border-radius: 50%;
      background: var(--scion-primary, #3b82f6);
      color: white;
      display: flex;
      align-items: center;
      justify-content: center;
      font-size: 0.75rem;
      font-weight: 600;
      flex-shrink: 0;
      overflow: hidden;
    }

    .user-avatar img {
      width: 100%;
      height: 100%;
      object-fit: cover;
    }

    .user-avatar.invited {
      background: var(--sl-color-primary-200, #bfdbfe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .user-info {
      display: flex;
      flex-direction: column;
      min-width: 0;
    }

    .user-name {
      font-weight: 500;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .user-email {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .user-invited-by {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
    }

    .role-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .role-badge.admin {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .role-badge.member {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .role-badge.viewer {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .status-badge {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .status-badge.active {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .status-badge.invited {
      background: var(--sl-color-primary-100, #dbeafe);
      color: var(--sl-color-primary-700, #1d4ed8);
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
    }

    .status-badge.suspended {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .status-cell {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
    }

    .status-dot {
      display: inline-flex;
      align-items: center;
      gap: 0.375rem;
      font-size: 0.8125rem;
    }

    .status-dot::before {
      content: '';
      width: 0.5rem;
      height: 0.5rem;
      border-radius: 50%;
      flex-shrink: 0;
    }

    .status-dot.active::before {
      background: var(--sl-color-success-500, #22c55e);
    }

    .status-dot.suspended::before {
      background: var(--sl-color-danger-500, #ef4444);
    }

    .last-seen-text {
      font-size: 0.6875rem;
      color: var(--scion-text-muted, #64748b);
      padding-left: 0.875rem;
    }

    .meta-text {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .id-text {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
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

    .pagination {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f1f5f9);
    }

    .pagination-info {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .pagination-controls {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .pagination-controls sl-button::part(base) {
      font-size: 0.8125rem;
    }

    .page-indicator {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0 0.5rem;
    }

    .actions-cell {
      text-align: right;
      width: 3rem;
    }

    sl-dropdown sl-button::part(base) {
      padding: 0.25rem;
      min-height: unset;
    }

    sl-menu-item::part(base) {
      font-size: 0.8125rem;
    }

    sl-menu-item sl-icon {
      font-size: 1rem;
    }

    .menu-item-danger::part(base) {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .menu-item-danger::part(base):hover {
      background: var(--sl-color-danger-50, #fef2f2);
    }

    .confirm-body {
      font-size: 0.875rem;
      line-height: 1.5;
      color: var(--scion-text, #1e293b);
    }

    .confirm-user {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem;
      margin: 0.75rem 0;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .feedback-alert {
      margin-bottom: 1rem;
    }

    .tabs {
      display: flex;
      gap: 0;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      margin-bottom: 1.5rem;
    }

    .tab-btn {
      padding: 0.625rem 1.25rem;
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      background: none;
      border: none;
      border-bottom: 2px solid transparent;
      cursor: pointer;
      transition:
        color 0.15s,
        border-color 0.15s;
    }

    .tab-btn:hover {
      color: var(--scion-text, #1e293b);
    }

    .tab-btn.active {
      color: var(--scion-primary, #3b82f6);
      border-bottom-color: var(--scion-primary, #3b82f6);
    }

    .users-toolbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
      flex-wrap: wrap;
      gap: 0.5rem;
    }

    .users-toolbar-left {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .users-toolbar-right {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .invite-form {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .invite-status {
      display: inline-flex;
      align-items: center;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      font-size: 0.75rem;
      font-weight: 500;
    }

    .invite-status.active {
      background: var(--sl-color-success-100, #dcfce7);
      color: var(--sl-color-success-700, #15803d);
    }

    .invite-status.expired {
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .invite-status.revoked {
      background: var(--sl-color-danger-100, #fee2e2);
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .invite-status.exhausted {
      background: var(--sl-color-warning-100, #fef3c7);
      color: var(--sl-color-warning-700, #a16207);
    }

    .create-invite-form {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .reveal-code {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .reveal-code .code-display {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.8125rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      word-break: break-all;
      user-select: all;
    }

    .reveal-code .link-display {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.75rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      word-break: break-all;
      user-select: all;
    }

    .reveal-warning {
      font-size: 0.8125rem;
      color: var(--sl-color-warning-700, #a16207);
      background: var(--sl-color-warning-50, #fffbeb);
      padding: 0.5rem 0.75rem;
      border-radius: var(--scion-radius, 0.5rem);
      border: 1px solid var(--sl-color-warning-200, #fde68a);
    }

    .invites-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .invites-header span {
      font-size: 0.875rem;
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
    void this.loadCurrentUser();
    void this.loadUsers();
  }

  private async loadCurrentUser(): Promise<void> {
    try {
      const res = await fetch('/auth/me', { credentials: 'include' });
      if (res.ok) {
        const data = (await res.json()) as { id?: string };
        this.currentUserId = data.id || null;
      }
    } catch {
      // Non-critical — actions will still work, just can't prevent self-actions
    }
  }

  private async loadUsers(cursor?: string): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const params = new URLSearchParams({
        limit: String(PAGE_SIZE),
        sort: this.sortField,
        dir: this.sortDir,
      });
      if (cursor) {
        params.set('cursor', cursor);
      }
      if (this.statusFilter !== 'all') {
        params.set('status', this.statusFilter);
      }

      const response = await fetch(`/api/v1/users?${params.toString()}`, {
        credentials: 'include',
      });

      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }

      const data = (await response.json()) as {
        users?: AdminUser[];
        nextCursor?: string;
        totalCount?: number;
      };
      this.users = Array.isArray(data) ? data : data.users || [];
      this.nextCursor = (data as { nextCursor?: string }).nextCursor || null;
      this.totalCount = (data as { totalCount?: number }).totalCount ?? this.users.length;
    } catch (err) {
      console.error('Failed to load users:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load users';
    } finally {
      this.loading = false;
    }
  }

  private goToNextPage(): void {
    if (!this.nextCursor) return;
    this.cursorHistory = [...this.cursorHistory, this.nextCursor];
    this.currentPage++;
    void this.loadUsers(this.nextCursor);
  }

  private goToPrevPage(): void {
    if (this.currentPage <= 1) return;
    this.currentPage--;
    // Remove the last cursor from history; the one before it is what we navigate to
    const history = [...this.cursorHistory];
    history.pop();
    this.cursorHistory = history;
    const cursor = this.currentPage === 1 ? undefined : history[history.length - 1];
    void this.loadUsers(cursor);
  }

  private async updateUser(
    userId: string,
    updates: { role?: string; status?: string }
  ): Promise<void> {
    const response = await fetch(`/api/v1/users/${userId}`, {
      method: 'PATCH',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(updates),
    });
    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }
  }

  private async deleteUser(userId: string): Promise<void> {
    const response = await fetch(`/api/v1/users/${userId}`, {
      method: 'DELETE',
      credentials: 'include',
    });
    if (!response.ok) {
      throw new Error(await extractApiError(response, `HTTP ${response.status}`));
    }
  }

  private promptChangeRole(user: AdminUser, newRole: UserRole): void {
    const action = newRole === 'admin' ? 'Promote' : 'Change role';
    const roleLabel = newRole === 'admin' ? 'an admin' : `a ${newRole}`;
    this.confirmAction = {
      title: `${action} to ${newRole}`,
      message: `Are you sure you want to make this user ${roleLabel}?`,
      variant: newRole === 'admin' ? 'warning' : 'primary',
      confirmLabel: action,
      user,
      action: async () => {
        await this.updateUser(user.id, { role: newRole });
        this.showFeedback('success', `${user.displayName || user.email} is now ${roleLabel}.`);
        void this.loadUsers(
          this.currentPage > 1 ? this.cursorHistory[this.cursorHistory.length - 1] : undefined
        );
      },
    };
  }

  private promptToggleSuspend(user: AdminUser): void {
    const suspending = user.status === 'active';
    this.confirmAction = {
      title: suspending ? 'Suspend user' : 'Reactivate user',
      message: suspending
        ? 'This user will be unable to sign in or use the system while suspended.'
        : "This will restore the user's access to the system.",
      variant: suspending ? 'warning' : 'primary',
      confirmLabel: suspending ? 'Suspend' : 'Reactivate',
      user,
      action: async () => {
        const newStatus = suspending ? 'suspended' : 'active';
        await this.updateUser(user.id, { status: newStatus });
        this.showFeedback(
          'success',
          `${user.displayName || user.email} has been ${suspending ? 'suspended' : 'reactivated'}.`
        );
        void this.loadUsers(
          this.currentPage > 1 ? this.cursorHistory[this.cursorHistory.length - 1] : undefined
        );
      },
    };
  }

  private promptDelete(user: AdminUser): void {
    const label = user.status === 'invited' ? 'Remove invited user' : 'Delete user';
    const message =
      user.status === 'invited'
        ? 'This will remove the invitation. The user will no longer be able to sign in.'
        : 'This action is permanent and cannot be undone. All data associated with this user will be removed.';
    this.confirmAction = {
      title: label,
      message,
      variant: 'danger',
      confirmLabel: user.status === 'invited' ? 'Remove' : 'Delete',
      user,
      action: async () => {
        await this.deleteUser(user.id);
        this.showFeedback(
          'success',
          `${user.displayName || user.email} has been ${user.status === 'invited' ? 'removed' : 'deleted'}.`
        );
        void this.loadUsers(
          this.currentPage > 1 ? this.cursorHistory[this.cursorHistory.length - 1] : undefined
        );
      },
    };
  }

  private async executeConfirmedAction(): Promise<void> {
    if (!this.confirmAction) return;
    this.actionInProgress = true;
    try {
      await this.confirmAction.action();
    } catch (err) {
      this.showFeedback('danger', err instanceof Error ? err.message : 'Action failed');
    } finally {
      this.actionInProgress = false;
      this.confirmAction = null;
    }
  }

  private showFeedback(variant: 'success' | 'danger', message: string): void {
    this.actionFeedback = { variant, message };
    setTimeout(() => {
      this.actionFeedback = null;
    }, 5000);
  }

  private isSelf(user: AdminUser): boolean {
    return !!this.currentUserId && user.id === this.currentUserId;
  }

  private formatRelativeTime(dateString: string | undefined): string {
    if (!dateString) return 'Never';
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return 'Never';
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

  private getInitials(name: string): string {
    return name
      .split(/\s+/)
      .map((w) => w[0])
      .join('')
      .toUpperCase()
      .slice(0, 2);
  }

  private toggleSort(field: SortField): void {
    if (this.sortField === field) {
      this.sortDir = this.sortDir === 'asc' ? 'desc' : 'asc';
    } else {
      this.sortField = field;
      this.sortDir = field === 'name' ? 'asc' : 'desc';
    }
    // Reset pagination and re-fetch with new sort applied server-side
    this.currentPage = 1;
    this.cursorHistory = [];
    this.nextCursor = null;
    void this.loadUsers();
  }

  private sortIndicator(field: SortField): string {
    return this.sortField === field ? (this.sortDir === 'asc' ? '▲' : '▼') : '▲';
  }

  private get totalPages(): number {
    return Math.max(1, Math.ceil(this.totalCount / PAGE_SIZE));
  }

  private get rangeStart(): number {
    return (this.currentPage - 1) * PAGE_SIZE + 1;
  }

  private get rangeEnd(): number {
    return Math.min(this.currentPage * PAGE_SIZE, this.totalCount);
  }

  private setStatusFilter(filter: StatusFilter): void {
    if (this.statusFilter === filter) return;
    this.statusFilter = filter;
    this.currentPage = 1;
    this.cursorHistory = [];
    this.nextCursor = null;
    void this.loadUsers();
  }

  // ==================== Invite User ====================

  private async inviteUser(): Promise<void> {
    const email = this.inviteUserEmail.trim().toLowerCase();
    if (!email || !email.includes('@')) return;

    this.inviteUserInProgress = true;
    try {
      const response = await apiFetch('/api/v1/admin/users/invite', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, note: this.inviteUserNote }),
      });
      if (response.status === 409) {
        this.showFeedback('danger', 'User already exists.');
        return;
      }
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.showFeedback('success', `Invited ${email}.`);
      this.showInviteUserDialog = false;
      this.inviteUserEmail = '';
      this.inviteUserNote = '';
      void this.loadUsers(
        this.currentPage > 1 ? this.cursorHistory[this.cursorHistory.length - 1] : undefined
      );
    } catch (err) {
      this.showFeedback('danger', err instanceof Error ? err.message : 'Failed to invite user');
    } finally {
      this.inviteUserInProgress = false;
    }
  }

  // ==================== Bulk Import ====================

  private async importCSV(): Promise<void> {
    const input = this.shadowRoot?.querySelector('#import-file-input') as HTMLInputElement;
    if (!input?.files?.length) {
      this.showFeedback('danger', 'Please select a file.');
      return;
    }

    const file = input.files[0];
    const formData = new FormData();
    formData.append('file', file);

    this.importInProgress = true;
    try {
      const response = await apiFetch('/api/v1/admin/users/invite/bulk', {
        method: 'POST',
        body: formData,
      });
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      const result = (await response.json()) as { invited: number; skipped: number; total: number };
      this.showFeedback(
        'success',
        `Import complete: ${result.invited} invited, ${result.skipped} skipped.`
      );
      this.showImportDialog = false;
      void this.loadUsers(
        this.currentPage > 1 ? this.cursorHistory[this.cursorHistory.length - 1] : undefined
      );
    } catch (err) {
      this.showFeedback('danger', err instanceof Error ? err.message : 'Import failed');
    } finally {
      this.importInProgress = false;
    }
  }

  // ==================== Invite Codes Tab ====================

  private async loadInvites(): Promise<void> {
    this.invitesLoading = true;
    try {
      const response = await apiFetch('/api/v1/admin/invites');
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      const data = (await response.json()) as {
        items?: InviteCodeEntry[];
        totalCount?: number;
      } | null;
      this.invites = data?.items || [];
      this.invitesTotalCount = data?.totalCount ?? 0;
    } catch (err) {
      this.showFeedback('danger', err instanceof Error ? err.message : 'Failed to load invites');
    } finally {
      this.invitesLoading = false;
    }
  }

  private getInviteStatus(invite: InviteCodeEntry): string {
    if (invite.revoked) return 'revoked';
    if (new Date() > new Date(invite.expiresAt)) return 'expired';
    if (invite.maxUses > 0 && invite.useCount >= invite.maxUses) return 'exhausted';
    return 'active';
  }

  private async createInvite(): Promise<void> {
    this.createInviteInProgress = true;
    try {
      const body: Record<string, unknown> = {
        expiresIn: this.createInviteExpiry,
        maxUses: this.createInviteMaxUses,
        note: this.createInviteNote,
      };
      const response = await apiFetch('/api/v1/admin/invites', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      const result = (await response.json()) as InviteCreateResult;
      this.createdInviteResult = result;
      this.showCreateInviteDialog = false;
      this.createInviteExpiry = '1h';
      this.createInviteMaxUses = 1;
      this.createInviteNote = '';
      void this.loadInvites();
    } catch (err) {
      this.showFeedback('danger', err instanceof Error ? err.message : 'Failed to create invite');
    } finally {
      this.createInviteInProgress = false;
    }
  }

  private async revokeInvite(id: string): Promise<void> {
    try {
      const response = await apiFetch(`/api/v1/admin/invites/${encodeURIComponent(id)}/revoke`, {
        method: 'POST',
      });
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.showFeedback('success', 'Invite code revoked.');
      void this.loadInvites();
    } catch (err) {
      this.showFeedback('danger', err instanceof Error ? err.message : 'Failed to revoke invite');
    }
  }

  private async deleteInvite(id: string): Promise<void> {
    try {
      const response = await apiFetch(`/api/v1/admin/invites/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      });
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      this.showFeedback('success', 'Invite code deleted.');
      void this.loadInvites();
    } catch (err) {
      this.showFeedback('danger', err instanceof Error ? err.message : 'Failed to delete invite');
    }
  }

  private async copyInviteLink(): Promise<void> {
    if (!this.createdInviteResult) return;
    try {
      await navigator.clipboard.writeText(this.createdInviteResult.inviteUrl);
      this.inviteCopied = true;
      setTimeout(() => {
        this.inviteCopied = false;
      }, 2000);
    } catch {
      const input = document.createElement('input');
      input.value = this.createdInviteResult.inviteUrl;
      document.body.appendChild(input);
      input.select();
      document.execCommand('copy');
      document.body.removeChild(input);
      this.inviteCopied = true;
      setTimeout(() => {
        this.inviteCopied = false;
      }, 2000);
    }
  }

  // ==================== Render Methods ====================

  override render() {
    return html`
      <div class="header">
        <h1>Users</h1>
      </div>

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
        : nothing}

      <div class="tabs" role="tablist">
        <button
          role="tab"
          aria-selected=${this.activeTab === 'users'}
          aria-controls="panel-users"
          class="tab-btn ${this.activeTab === 'users' ? 'active' : ''}"
          @click=${() => {
            this.activeTab = 'users';
          }}
        >
          Users ${!this.loading ? `(${this.totalCount})` : ''}
        </button>
        <button
          role="tab"
          aria-selected=${this.activeTab === 'invites'}
          aria-controls="panel-invites"
          class="tab-btn ${this.activeTab === 'invites' ? 'active' : ''}"
          @click=${() => {
            this.activeTab = 'invites';
            void this.loadInvites();
          }}
        >
          Invite Codes ${this.invitesTotalCount > 0 ? `(${this.invitesTotalCount})` : ''}
        </button>
      </div>

      ${this.activeTab === 'users'
        ? this.loading
          ? this.renderLoading()
          : this.error
            ? this.renderError()
            : this.renderUsers()
        : this.renderInvitesTab()}
      ${this.renderConfirmDialog()} ${this.renderInviteUserDialog()} ${this.renderImportDialog()}
      ${this.renderCreateInviteDialog()} ${this.renderInviteRevealDialog()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading users...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Users</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadUsers()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderStatusFilter() {
    const filters: { label: string; value: StatusFilter }[] = [
      { label: 'All', value: 'all' },
      { label: 'Invited', value: 'invited' },
      { label: 'Active', value: 'active' },
      { label: 'Suspended', value: 'suspended' },
    ];
    return html`
      <sl-select
        size="small"
        value=${this.statusFilter}
        @sl-change=${(e: Event) => {
          this.setStatusFilter((e.target as HTMLSelectElement).value as StatusFilter);
        }}
        style="min-width: 8rem"
      >
        ${filters.map((f) => html`<sl-option value=${f.value}>${f.label}</sl-option>`)}
      </sl-select>
    `;
  }

  private renderUsers() {
    if (this.users.length === 0 && this.statusFilter === 'all') {
      return html`
        <div class="empty-state">
          <sl-icon name="people"></sl-icon>
          <h2>No Users Found</h2>
          <p>There are no users registered in the system. Invite users to get started.</p>
          <sl-button
            variant="primary"
            style="margin-top: 1rem"
            @click=${() => {
              this.showInviteUserDialog = true;
            }}
          >
            <sl-icon slot="prefix" name="person-plus"></sl-icon>
            Invite User
          </sl-button>
        </div>
      `;
    }

    const hasPagination = this.totalCount > PAGE_SIZE;

    return html`
      <div class="users-toolbar">
        <div class="users-toolbar-left">
          ${this.renderStatusFilter()}
          <span class="meta-text">${this.totalCount} user${this.totalCount !== 1 ? 's' : ''}</span>
        </div>
        <div class="users-toolbar-right">
          <sl-button
            size="small"
            variant="default"
            @click=${() => {
              this.showImportDialog = true;
            }}
          >
            <sl-icon slot="prefix" name="upload"></sl-icon>
            Import CSV
          </sl-button>
          <sl-button
            size="small"
            variant="primary"
            @click=${() => {
              this.showInviteUserDialog = true;
            }}
          >
            <sl-icon slot="prefix" name="person-plus"></sl-icon>
            Invite User
          </sl-button>
        </div>
      </div>

      ${this.users.length === 0
        ? html`
            <div class="empty-state">
              <sl-icon name="people"></sl-icon>
              <h2>No Users Found</h2>
              <p>No users match the selected filter.</p>
            </div>
          `
        : html`
            <div class="table-container">
              <table>
                <thead>
                  <tr>
                    <th
                      class="sortable ${this.sortField === 'name' ? 'sorted' : ''}"
                      @click=${() => this.toggleSort('name')}
                    >
                      User
                      <span class="sort-indicator">${this.sortIndicator('name')}</span>
                    </th>
                    <th>Role</th>
                    <th>Status</th>
                    <th class="hide-mobile">Last Login</th>
                    <th
                      class="hide-mobile sortable ${this.sortField === 'created' ? 'sorted' : ''}"
                      @click=${() => this.toggleSort('created')}
                    >
                      Created
                      <span class="sort-indicator">${this.sortIndicator('created')}</span>
                    </th>
                    <th class="actions-cell"></th>
                  </tr>
                </thead>
                <tbody>
                  ${this.users.map((user) => this.renderUserRow(user))}
                </tbody>
              </table>
              ${hasPagination ? this.renderPagination() : ''}
            </div>
          `}
    `;
  }

  private renderPagination() {
    return html`
      <div class="pagination">
        <span class="pagination-info">
          Showing ${this.rangeStart}-${this.rangeEnd} of ${this.totalCount}
        </span>
        <div class="pagination-controls">
          <sl-button
            size="small"
            variant="default"
            ?disabled=${this.currentPage <= 1}
            @click=${() => this.goToPrevPage()}
          >
            <sl-icon slot="prefix" name="chevron-left"></sl-icon>
            Previous
          </sl-button>
          <span class="page-indicator">Page ${this.currentPage} of ${this.totalPages}</span>
          <sl-button
            size="small"
            variant="default"
            ?disabled=${!this.nextCursor}
            @click=${() => this.goToNextPage()}
          >
            Next
            <sl-icon slot="suffix" name="chevron-right"></sl-icon>
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderUserRow(user: AdminUser) {
    const self = this.isSelf(user);
    const isInvited = user.status === 'invited';
    return html`
      <tr>
        <td>
          <div class="user-identity">
            ${isInvited
              ? html`
                  <div class="user-avatar invited">
                    <sl-icon name="envelope"></sl-icon>
                  </div>
                `
              : html`
                  <div class="user-avatar">
                    ${user.avatarUrl
                      ? html`<img src="${user.avatarUrl}" alt="${user.displayName}" />`
                      : this.getInitials(user.displayName || user.email)}
                  </div>
                `}
            <div class="user-info">
              <span class="user-name"
                >${isInvited ? user.email : user.displayName || user.email}</span
              >
              ${!isInvited && user.displayName
                ? html`<span class="user-email">${user.email}</span>`
                : nothing}
              ${isInvited && user.invitedBy
                ? html`<span class="user-invited-by">Invited by ${user.invitedBy}</span>`
                : nothing}
            </div>
          </div>
        </td>
        <td>
          <span class="role-badge ${user.role}">${user.role}</span>
        </td>
        <td>
          <span class="status-badge ${user.status}">${user.status}</span>
        </td>
        <td class="hide-mobile">
          <span class="meta-text"
            >${isInvited ? '—' : this.formatRelativeTime(user.lastLogin)}</span
          >
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(user.created)}</span>
        </td>
        <td class="actions-cell">${self ? nothing : this.renderUserActions(user)}</td>
      </tr>
    `;
  }

  private renderUserActions(user: AdminUser) {
    if (user.status === 'invited') {
      // Invited users: only Remove action
      return html`
        <sl-dropdown placement="bottom-end" hoist>
          <sl-button slot="trigger" size="small" variant="text" caret>
            <sl-icon name="three-dots-vertical"></sl-icon>
          </sl-button>
          <sl-menu>
            <sl-menu-item class="menu-item-danger" @click=${() => this.promptDelete(user)}>
              <sl-icon slot="prefix" name="trash"></sl-icon>
              Remove
            </sl-menu-item>
          </sl-menu>
        </sl-dropdown>
      `;
    }

    if (user.status === 'suspended') {
      // Suspended users: Reactivate, Delete
      return html`
        <sl-dropdown placement="bottom-end" hoist>
          <sl-button slot="trigger" size="small" variant="text" caret>
            <sl-icon name="three-dots-vertical"></sl-icon>
          </sl-button>
          <sl-menu>
            <sl-menu-item @click=${() => this.promptToggleSuspend(user)}>
              <sl-icon slot="prefix" name="check-circle"></sl-icon>
              Reactivate
            </sl-menu-item>
            <sl-divider></sl-divider>
            <sl-menu-item class="menu-item-danger" @click=${() => this.promptDelete(user)}>
              <sl-icon slot="prefix" name="trash"></sl-icon>
              Delete
            </sl-menu-item>
          </sl-menu>
        </sl-dropdown>
      `;
    }

    // Active users: Change role, Suspend, Delete
    return html`
      <sl-dropdown placement="bottom-end" hoist>
        <sl-button slot="trigger" size="small" variant="text" caret>
          <sl-icon name="three-dots-vertical"></sl-icon>
        </sl-button>
        <sl-menu>
          ${user.role !== 'admin'
            ? html`<sl-menu-item @click=${() => this.promptChangeRole(user, 'admin')}>
                <sl-icon slot="prefix" name="shield-check"></sl-icon>
                Promote to Admin
              </sl-menu-item>`
            : nothing}
          ${user.role === 'admin'
            ? html`<sl-menu-item @click=${() => this.promptChangeRole(user, 'member')}>
                <sl-icon slot="prefix" name="person"></sl-icon>
                Demote to Member
              </sl-menu-item>`
            : nothing}
          ${user.role !== 'viewer'
            ? html`<sl-menu-item @click=${() => this.promptChangeRole(user, 'viewer')}>
                <sl-icon slot="prefix" name="eye"></sl-icon>
                Set as Viewer
              </sl-menu-item>`
            : nothing}
          <sl-divider></sl-divider>
          <sl-menu-item @click=${() => this.promptToggleSuspend(user)}>
            <sl-icon slot="prefix" name="slash-circle"></sl-icon>
            Suspend
          </sl-menu-item>
          <sl-divider></sl-divider>
          <sl-menu-item class="menu-item-danger" @click=${() => this.promptDelete(user)}>
            <sl-icon slot="prefix" name="trash"></sl-icon>
            Delete
          </sl-menu-item>
        </sl-menu>
      </sl-dropdown>
    `;
  }

  private renderConfirmDialog() {
    const action = this.confirmAction;
    if (!action) return nothing;
    return html`
      <sl-dialog
        label=${action.title}
        open
        @sl-request-close=${() => {
          if (!this.actionInProgress) this.confirmAction = null;
        }}
      >
        <div class="confirm-body">
          <div class="confirm-user">
            ${action.user.status === 'invited'
              ? html`<div class="user-avatar invited"><sl-icon name="envelope"></sl-icon></div>`
              : html`
                  <div class="user-avatar">
                    ${action.user.avatarUrl
                      ? html`<img
                          src="${action.user.avatarUrl}"
                          alt="${action.user.displayName}"
                        />`
                      : this.getInitials(action.user.displayName || action.user.email)}
                  </div>
                `}
            <div class="user-info">
              <span class="user-name">${action.user.displayName || action.user.email}</span>
              <span class="user-email">${action.user.email}</span>
            </div>
          </div>
          <p>${action.message}</p>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.actionInProgress}
          @click=${() => {
            this.confirmAction = null;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant=${action.variant}
          ?loading=${this.actionInProgress}
          @click=${() => this.executeConfirmedAction()}
          >${action.confirmLabel}</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderInviteUserDialog() {
    if (!this.showInviteUserDialog) return nothing;

    return html`
      <sl-dialog
        label="Invite User"
        open
        @sl-request-close=${() => {
          if (!this.inviteUserInProgress) this.showInviteUserDialog = false;
        }}
      >
        <div class="invite-form">
          <sl-input
            label="Email address"
            type="email"
            placeholder="user@example.com"
            .value=${this.inviteUserEmail}
            @sl-input=${(e: Event) => {
              this.inviteUserEmail = (e.target as HTMLInputElement).value;
            }}
            required
          ></sl-input>
          <sl-input
            label="Note (optional)"
            placeholder="e.g., New hire, Q3 contractor"
            .value=${this.inviteUserNote}
            @sl-input=${(e: Event) => {
              this.inviteUserNote = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.inviteUserInProgress}
          @click=${() => {
            this.showInviteUserDialog = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.inviteUserInProgress}
          ?disabled=${!this.inviteUserEmail.trim().includes('@')}
          @click=${() => this.inviteUser()}
          >Invite User</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderImportDialog() {
    if (!this.showImportDialog) return nothing;
    return html`
      <sl-dialog
        label="Import Users from CSV"
        open
        @sl-request-close=${() => {
          if (!this.importInProgress) this.showImportDialog = false;
        }}
      >
        <div class="import-form">
          <p style="margin: 0 0 1rem; font-size: 0.875rem; color: var(--scion-text-muted)">
            Upload a CSV file with one email per line. An optional second column can contain notes.
            Users will be created with <strong>invited</strong> status.
          </p>
          <input
            type="file"
            accept=".csv,.txt"
            id="import-file-input"
            style="margin-bottom: 1rem"
          />
        </div>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.importInProgress}
          @click=${() => {
            this.showImportDialog = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.importInProgress}
          @click=${() => this.importCSV()}
          >Import</sl-button
        >
      </sl-dialog>
    `;
  }

  // ==================== Invite Codes Tab Rendering ====================

  private renderInvitesTab() {
    if (this.invitesLoading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading invite codes...</p>
        </div>
      `;
    }

    return html`
      <div class="invites-header">
        <span>${this.invitesTotalCount} invite code${this.invitesTotalCount !== 1 ? 's' : ''}</span>
        <sl-button
          size="small"
          variant="primary"
          @click=${() => {
            this.showCreateInviteDialog = true;
          }}
        >
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Create Invite Code
        </sl-button>
      </div>

      ${this.invites.length === 0
        ? html`
            <div class="empty-state">
              <sl-icon name="envelope-open"></sl-icon>
              <h2>No Invite Codes</h2>
              <p>Create invite codes to allow new users to join the hub.</p>
            </div>
          `
        : html`
            <div class="table-container">
              <table>
                <thead>
                  <tr>
                    <th>Code</th>
                    <th>Status</th>
                    <th>Uses</th>
                    <th class="hide-mobile">Expires</th>
                    <th class="hide-mobile">Note</th>
                    <th class="actions-cell"></th>
                  </tr>
                </thead>
                <tbody>
                  ${this.invites.map((invite) => this.renderInviteRow(invite))}
                </tbody>
              </table>
            </div>
          `}
    `;
  }

  private renderInviteRow(invite: InviteCodeEntry) {
    const status = this.getInviteStatus(invite);
    const uses = invite.maxUses > 0 ? `${invite.useCount}/${invite.maxUses}` : `${invite.useCount}`;
    return html`
      <tr>
        <td>
          <code style="font-size: 0.8125rem">${invite.codePrefix}...</code>
        </td>
        <td>
          <span class="invite-status ${status}">${status}</span>
        </td>
        <td><span class="meta-text">${uses}</span></td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(invite.expiresAt)}</span>
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${invite.note || '-'}</span>
        </td>
        <td class="actions-cell">
          <sl-dropdown placement="bottom-end" hoist>
            <sl-button slot="trigger" size="small" variant="text" caret>
              <sl-icon name="three-dots-vertical"></sl-icon>
            </sl-button>
            <sl-menu>
              ${status === 'active'
                ? html`<sl-menu-item @click=${() => this.revokeInvite(invite.id)}>
                      <sl-icon slot="prefix" name="slash-circle"></sl-icon>
                      Revoke
                    </sl-menu-item>
                    <sl-divider></sl-divider>`
                : nothing}
              <sl-menu-item class="menu-item-danger" @click=${() => this.deleteInvite(invite.id)}>
                <sl-icon slot="prefix" name="trash"></sl-icon>
                Delete
              </sl-menu-item>
            </sl-menu>
          </sl-dropdown>
        </td>
      </tr>
    `;
  }

  private renderCreateInviteDialog() {
    if (!this.showCreateInviteDialog) return nothing;
    return html`
      <sl-dialog
        label="Create Invite Code"
        open
        @sl-request-close=${() => {
          if (!this.createInviteInProgress) this.showCreateInviteDialog = false;
        }}
      >
        <div class="create-invite-form">
          <sl-select
            label="Expiration"
            .value=${this.createInviteExpiry}
            @sl-change=${(e: Event) => {
              this.createInviteExpiry = (e.target as HTMLSelectElement).value;
            }}
          >
            ${EXPIRY_PRESETS.map((p) => html` <sl-option value=${p.value}>${p.label}</sl-option> `)}
          </sl-select>
          <sl-select
            label="Max uses"
            .value=${String(this.createInviteMaxUses)}
            @sl-change=${(e: Event) => {
              this.createInviteMaxUses = parseInt((e.target as HTMLSelectElement).value, 10);
            }}
          >
            <sl-option value="1">Single use</sl-option>
            <sl-option value="5">5 uses</sl-option>
            <sl-option value="10">10 uses</sl-option>
            <sl-option value="25">25 uses</sl-option>
            <sl-option value="0">Unlimited</sl-option>
          </sl-select>
          <sl-input
            label="Note (optional)"
            placeholder="e.g., Workshop, new team member"
            .value=${this.createInviteNote}
            @sl-input=${(e: Event) => {
              this.createInviteNote = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          ?disabled=${this.createInviteInProgress}
          @click=${() => {
            this.showCreateInviteDialog = false;
          }}
          >Cancel</sl-button
        >
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.createInviteInProgress}
          @click=${() => this.createInvite()}
          >Create</sl-button
        >
      </sl-dialog>
    `;
  }

  private renderInviteRevealDialog() {
    if (!this.createdInviteResult) return nothing;
    return html`
      <sl-dialog
        label="Invite Created"
        open
        @sl-request-close=${() => {
          this.createdInviteResult = null;
          this.inviteCopied = false;
        }}
      >
        <div class="reveal-code">
          <p style="margin: 0; font-size: 0.875rem">
            Your invite link has been created. Copy it now — it will not be shown again.
          </p>
          <div>
            <label style="font-size: 0.75rem; font-weight: 600; color: var(--scion-text-muted)"
              >Invite Link</label
            >
            <div class="link-display">${this.createdInviteResult.inviteUrl}</div>
          </div>
          <div class="reveal-warning">
            This link will not be shown again. Make sure to copy it before closing.
          </div>
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.createdInviteResult = null;
            this.inviteCopied = false;
          }}
          >Close</sl-button
        >
        <sl-button slot="footer" variant="primary" @click=${() => this.copyInviteLink()}>
          <sl-icon slot="prefix" name=${this.inviteCopied ? 'check' : 'clipboard'}></sl-icon>
          ${this.inviteCopied ? 'Copied!' : 'Copy Link'}
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-users': ScionPageAdminUsers;
  }
}
