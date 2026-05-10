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
 * Subscription Manager Component
 *
 * CRUD table + dialog for managing notification subscriptions.
 * Used on the project detail page in compact mode.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';
import type { Subscription, SubscriptionScope } from '../../shared/types.js';

interface SubscriptionTemplate {
  id: string;
  name: string;
  scope: string;
  triggerActivities: string[];
  projectId: string;
  createdBy: string;
}

const DEFAULT_TRIGGERS = ['COMPLETED', 'WAITING_FOR_INPUT', 'LIMITS_EXCEEDED'];
const ALL_TRIGGERS = ['COMPLETED', 'WAITING_FOR_INPUT', 'LIMITS_EXCEEDED', 'STALLED', 'ERROR', 'DELETED'];

@customElement('scion-subscription-manager')
export class ScionSubscriptionManager extends LitElement {
  @property() projectId = '';
  @property() agentId?: string;
  @property({ type: Boolean }) compact = false;

  @state() private loading = true;
  @state() private subscriptions: Subscription[] = [];
  @state() private error: string | null = null;

  // Templates
  @state() private templates: SubscriptionTemplate[] = [];

  // Create dialog
  @state() private dialogOpen = false;
  @state() private dialogScope: SubscriptionScope = 'project';
  @state() private dialogAgentId = '';
  @state() private dialogTriggers: Set<string> = new Set(DEFAULT_TRIGGERS);
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Edit state
  @state() private editingId: string | null = null;
  @state() private editTriggers: Set<string> = new Set();
  @state() private editLoading = false;

  // Delete state
  @state() private deletingId: string | null = null;

  static override styles = [resourceStyles];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadSubscriptions();
    void this.loadTemplates();
  }

  private async loadSubscriptions(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      let url = '/api/v1/notifications/subscriptions';
      const params: string[] = [];
      if (this.projectId) {
        params.push(`projectId=${encodeURIComponent(this.projectId)}`);
      }
      if (this.agentId) {
        params.push(`agentId=${encodeURIComponent(this.agentId)}`);
      }
      if (params.length > 0) {
        url += '?' + params.join('&');
      }
      const response = await apiFetch(url);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as Subscription[] | { subscriptions?: Subscription[] } | null;
      if (data == null) {
        this.subscriptions = [];
      } else if (Array.isArray(data)) {
        this.subscriptions = data;
      } else {
        this.subscriptions = (data as { subscriptions?: Subscription[] }).subscriptions || [];
      }
    } catch (err) {
      console.error('Failed to load subscriptions:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load subscriptions';
    } finally {
      this.loading = false;
    }
  }

  private async loadTemplates(): Promise<void> {
    if (!this.projectId) return;
    try {
      const url = `/api/v1/notifications/templates?projectId=${encodeURIComponent(this.projectId)}`;
      const response = await apiFetch(url);
      if (response.ok) {
        const data = (await response.json()) as SubscriptionTemplate[] | null;
        this.templates = data || [];
      }
    } catch {
      // Templates are optional; silently ignore load failures
    }
  }

  private applyTemplate(tmpl: SubscriptionTemplate): void {
    this.dialogScope = tmpl.scope as SubscriptionScope;
    this.dialogTriggers = new Set(tmpl.triggerActivities);
  }

  private openCreateDialog(): void {
    this.dialogScope = this.agentId ? 'agent' : 'project';
    this.dialogAgentId = this.agentId || '';
    this.dialogTriggers = new Set(DEFAULT_TRIGGERS);
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
    this.dialogError = null;
  }

  private async handleCreate(e: Event): Promise<void> {
    e.preventDefault();
    this.dialogLoading = true;
    this.dialogError = null;

    try {
      const body: Record<string, unknown> = {
        scope: this.dialogScope,
        projectId: this.projectId,
        triggerActivities: [...this.dialogTriggers],
      };

      if (this.dialogScope === 'agent') {
        if (!this.dialogAgentId.trim()) {
          throw new Error('Agent ID is required for agent-scoped subscriptions');
        }
        body.agentId = this.dialogAgentId.trim();
      }

      const response = await apiFetch('/api/v1/notifications/subscriptions', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      this.closeDialog();
      await this.loadSubscriptions();
    } catch (err) {
      this.dialogError = err instanceof Error ? err.message : 'Failed to create subscription';
    } finally {
      this.dialogLoading = false;
    }
  }

  private startEdit(sub: Subscription): void {
    this.editingId = sub.id;
    this.editTriggers = new Set(sub.triggerActivities || DEFAULT_TRIGGERS);
  }

  private cancelEdit(): void {
    this.editingId = null;
    this.editTriggers = new Set();
  }

  private async saveEdit(): Promise<void> {
    if (!this.editingId || this.editTriggers.size === 0) return;
    this.editLoading = true;

    try {
      const response = await apiFetch(
        `/api/v1/notifications/subscriptions/${encodeURIComponent(this.editingId)}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ triggerActivities: [...this.editTriggers] }),
        }
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      this.editingId = null;
      await this.loadSubscriptions();
    } catch (err) {
      console.error('Failed to update subscription:', err);
      this.error = err instanceof Error ? err.message : 'Failed to update subscription';
    } finally {
      this.editLoading = false;
    }
  }

  private async handleDelete(id: string): Promise<void> {
    this.deletingId = id;

    try {
      const response = await apiFetch(
        `/api/v1/notifications/subscriptions/${encodeURIComponent(id)}`,
        { method: 'DELETE' }
      );

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      await this.loadSubscriptions();
    } catch (err) {
      console.error('Failed to delete subscription:', err);
      this.error = err instanceof Error ? err.message : 'Failed to delete subscription';
    } finally {
      this.deletingId = null;
    }
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = Date.now() - date.getTime();
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));
      const diffDays = Math.round(diffMs / (1000 * 60 * 60 * 24));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffMinutes) < 60) {
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

  // ---------------------------------------------------------------------------
  // Render
  // ---------------------------------------------------------------------------

  override render() {
    if (this.compact) {
      return this.renderCompact();
    }
    return this.renderFull();
  }

  private renderCompact() {
    return html`
      <div class="section compact">
        <div class="section-header">
          <div class="section-header-info">
            <h2>Your Notification Subscriptions</h2>
            <p>Get notified when agents complete, need input, or encounter issues.</p>
          </div>
          ${this.projectId
            ? html`<sl-button size="small" variant="default" @click=${this.openCreateDialog}>
                <sl-icon slot="prefix" name="bell"></sl-icon>
                Subscribe
              </sl-button>`
            : nothing}
        </div>

        ${this.loading
          ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading subscriptions...</div>`
          : this.error
            ? html`
                <div class="section-error">
                  ${this.error}
                  <sl-button size="small" @click=${() => this.loadSubscriptions()}>Retry</sl-button>
                </div>
              `
            : this.subscriptions.length === 0
              ? html`
                  <div class="empty-state">
                    <sl-icon name="bell-slash"></sl-icon>
                    <h3>No Subscriptions</h3>
                    <p>${this.projectId
                      ? 'Subscribe to get notified about agent activity in this project.'
                      : 'You have no notification subscriptions. Subscribe from a project or agent page.'}</p>
                    ${this.projectId
                      ? html`<sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
                          <sl-icon slot="prefix" name="bell"></sl-icon>
                          Subscribe
                        </sl-button>`
                      : nothing}
                  </div>
                `
              : this.renderTable()}

        ${this.renderDialog()}
      </div>
    `;
  }

  private renderFull() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading subscriptions...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Subscriptions</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadSubscriptions()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="list-header">
        <sl-button size="small" variant="primary" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="bell"></sl-icon>
          Subscribe
        </sl-button>
      </div>

      ${this.subscriptions.length === 0
        ? html`
            <div class="empty-state">
              <sl-icon name="bell-slash"></sl-icon>
              <h3>No Subscriptions</h3>
              <p>Subscribe to get notified about agent activity.</p>
            </div>
          `
        : this.renderTable()}

      ${this.renderDialog()}
    `;
  }

  private renderTable() {
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Scope</th>
              <th>Target</th>
              <th class="hide-mobile">Triggers</th>
              <th class="hide-mobile">Created</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.subscriptions.map((sub) => this.renderRow(sub))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderRow(sub: Subscription) {
    const isDeleting = this.deletingId === sub.id;
    const isEditing = this.editingId === sub.id;
    const target =
      sub.scope === 'project' ? '(all agents)' : sub.agentSlug || sub.agentId || '\u2014';
    const scopeIcon = sub.scope === 'project' ? 'folder' : 'cpu';
    const triggers = sub.triggerActivities?.join(', ') || '\u2014';

    return html`
      <tr>
        <td>
          <span class="key-info">
            <sl-icon name=${scopeIcon} style="color: var(--scion-primary, #3b82f6); flex-shrink: 0;"></sl-icon>
            <span>${sub.scope}</span>
          </span>
        </td>
        <td><span class="meta-text">${target}</span></td>
        <td class="hide-mobile">
          ${isEditing
            ? html`
                <div class="inline-edit-triggers">
                  ${ALL_TRIGGERS.map(
                    (trigger) => html`
                      <label class="checkbox-label compact">
                        <input
                          type="checkbox"
                          .checked=${this.editTriggers.has(trigger)}
                          @change=${(e: Event) => {
                            const checked = (e.target as HTMLInputElement).checked;
                            const next = new Set(this.editTriggers);
                            if (checked) {
                              next.add(trigger);
                            } else {
                              next.delete(trigger);
                            }
                            this.editTriggers = next;
                          }}
                        />
                        <span>${this.triggerLabel(trigger)}</span>
                      </label>
                    `
                  )}
                </div>
              `
            : html`<span class="meta-text">${triggers}</span>`}
        </td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(sub.createdAt)}</span>
        </td>
        <td class="actions-cell">
          ${isEditing
            ? html`
                <sl-icon-button
                  name="check-lg"
                  label="Save"
                  ?disabled=${this.editLoading || this.editTriggers.size === 0}
                  @click=${() => this.saveEdit()}
                ></sl-icon-button>
                <sl-icon-button
                  name="x-lg"
                  label="Cancel"
                  ?disabled=${this.editLoading}
                  @click=${() => this.cancelEdit()}
                ></sl-icon-button>
              `
            : html`
                <sl-icon-button
                  name="pencil"
                  label="Edit"
                  ?disabled=${isDeleting}
                  @click=${() => this.startEdit(sub)}
                ></sl-icon-button>
                <sl-icon-button
                  name="trash"
                  label="Delete"
                  ?disabled=${isDeleting}
                  @click=${() => this.handleDelete(sub.id)}
                ></sl-icon-button>
              `}
        </td>
      </tr>
    `;
  }

  private renderDialog() {
    return html`
      <sl-dialog
        label="Subscribe to Notifications"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
      >
        <form class="dialog-form" @submit=${this.handleCreate}>
          ${this.dialogError
            ? html`<div class="dialog-error">${this.dialogError}</div>`
            : nothing}

          ${!this.agentId
            ? html`
                <div class="radio-field">
                  <span class="radio-field-label">Scope</span>
                  <sl-radio-group
                    .value=${this.dialogScope}
                    @sl-change=${(e: Event) =>
                      (this.dialogScope = (e.target as HTMLInputElement).value as SubscriptionScope)}
                  >
                    <sl-radio-button value="project">Entire Project</sl-radio-button>
                    <sl-radio-button value="agent">Specific Agent</sl-radio-button>
                  </sl-radio-group>
                  <span class="radio-field-help">
                    ${this.dialogScope === 'project'
                      ? 'Receive notifications for all agents in this project.'
                      : 'Receive notifications for a specific agent only.'}
                  </span>
                </div>
              `
            : nothing}

          ${this.dialogScope === 'agent' && !this.agentId
            ? html`
                <sl-input
                  label="Agent ID"
                  placeholder="agent-uuid"
                  .value=${this.dialogAgentId}
                  @sl-input=${(e: Event) =>
                    (this.dialogAgentId = (e.target as HTMLInputElement).value)}
                  required
                ></sl-input>
              `
            : nothing}

          ${this.templates.length > 0
            ? html`
                <div class="radio-field">
                  <span class="radio-field-label">Template</span>
                  <sl-select
                    placeholder="Choose a template (optional)"
                    size="small"
                    clearable
                    @sl-change=${(e: Event) => {
                      const id = (e.target as HTMLSelectElement).value;
                      const tmpl = this.templates.find((t) => t.id === id);
                      if (tmpl) this.applyTemplate(tmpl);
                    }}
                  >
                    ${this.templates.map(
                      (tmpl) => html`
                        <sl-option value=${tmpl.id}>${tmpl.name}</sl-option>
                      `
                    )}
                  </sl-select>
                </div>
              `
            : nothing}

          <div class="radio-field">
            <span class="radio-field-label">Trigger Activities</span>
            <div class="checkbox-group">
              ${ALL_TRIGGERS.map(
                (trigger) => html`
                  <label class="checkbox-label">
                    <input
                      type="checkbox"
                      .checked=${this.dialogTriggers.has(trigger)}
                      @change=${(e: Event) => {
                        const checked = (e.target as HTMLInputElement).checked;
                        const next = new Set(this.dialogTriggers);
                        if (checked) {
                          next.add(trigger);
                        } else {
                          next.delete(trigger);
                        }
                        this.dialogTriggers = next;
                      }}
                    />
                    <span class="checkbox-text">
                      <span>${this.triggerLabel(trigger)}</span>
                      <span class="checkbox-description">${this.triggerDescription(trigger)}</span>
                    </span>
                  </label>
                `
              )}
            </div>
          </div>
        </form>

        <sl-button slot="footer" variant="default" @click=${this.closeDialog}>Cancel</sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.dialogLoading}
          ?disabled=${this.dialogTriggers.size === 0}
          @click=${this.handleCreate}
        >Subscribe</sl-button>
      </sl-dialog>
    `;
  }

  private triggerLabel(trigger: string): string {
    switch (trigger) {
      case 'COMPLETED':
        return 'Completed';
      case 'WAITING_FOR_INPUT':
        return 'Waiting for Input';
      case 'LIMITS_EXCEEDED':
        return 'Limits Exceeded';
      case 'STALLED':
        return 'Stalled';
      case 'ERROR':
        return 'Error';
      case 'DELETED':
        return 'Deleted';
      default:
        return trigger;
    }
  }

  private triggerDescription(trigger: string): string {
    switch (trigger) {
      case 'COMPLETED':
        return 'Agent finished its task.';
      case 'WAITING_FOR_INPUT':
        return 'Agent needs human input to continue.';
      case 'LIMITS_EXCEEDED':
        return 'Agent exceeded turn or model call limits.';
      case 'STALLED':
        return 'Agent has stalled and is no longer making progress.';
      case 'ERROR':
        return 'Agent encountered an error.';
      case 'DELETED':
        return 'Agent was deleted.';
      default:
        return '';
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-subscription-manager': ScionSubscriptionManager;
  }
}
