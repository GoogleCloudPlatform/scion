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
 * Shared Scheduled Event List Component
 *
 * Displays scheduled events for a project with create and cancel actions.
 * Used by the project detail page (compact mode) and potentially standalone.
 */

import { LitElement, html, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { resourceStyles } from './resource-styles.js';

interface ScheduledEvent {
  id: string;
  projectId: string;
  eventType: string;
  fireAt: string;
  payload: string;
  status: string;
  createdAt: string;
  createdBy: string;
  firedAt?: string;
  error?: string;
}

interface ListResponse {
  events: ScheduledEvent[];
  totalCount?: number;
  serverTime?: string;
}

@customElement('scion-scheduled-event-list')
export class ScionScheduledEventList extends LitElement {
  @property() projectId = '';
  @property({ type: Boolean }) compact = false;

  @state() private loading = true;
  @state() private events: ScheduledEvent[] = [];
  @state() private error: string | null = null;

  // Create dialog
  @state() private dialogOpen = false;
  @state() private dialogAgent = '';
  @state() private dialogMessage = '';
  @state() private dialogTimingMode: 'in' | 'at' = 'in';
  @state() private dialogDuration = '30m';
  @state() private dialogDatetime = '';
  @state() private dialogInterrupt = false;
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Cancel state
  @state() private cancellingId: string | null = null;

  static override styles = [resourceStyles];

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadEvents();
  }

  private async loadEvents(): Promise<void> {
    if (!this.projectId) return;
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/scheduled-events`
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as ListResponse;
      this.events = data.events || [];
    } catch (err) {
      console.error('Failed to load scheduled events:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load scheduled events';
    } finally {
      this.loading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogAgent = '';
    this.dialogMessage = '';
    this.dialogTimingMode = 'in';
    this.dialogDuration = '30m';
    this.dialogDatetime = '';
    this.dialogInterrupt = false;
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
        eventType: 'message',
        agentName: this.dialogAgent,
        message: this.dialogMessage,
        interrupt: this.dialogInterrupt,
      };

      if (this.dialogTimingMode === 'in') {
        body.fireIn = this.dialogDuration;
      } else {
        // Convert local datetime to ISO 8601 UTC
        const dt = new Date(this.dialogDatetime);
        body.fireAt = dt.toISOString();
      }

      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/scheduled-events`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      this.closeDialog();
      await this.loadEvents();
    } catch (err) {
      this.dialogError = err instanceof Error ? err.message : 'Failed to create event';
    } finally {
      this.dialogLoading = false;
    }
  }

  private async handleCancel(eventId: string): Promise<void> {
    this.cancellingId = eventId;

    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/scheduled-events/${encodeURIComponent(eventId)}`,
        { method: 'DELETE' }
      );

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }

      await this.loadEvents();
    } catch (err) {
      console.error('Failed to cancel event:', err);
      this.error = err instanceof Error ? err.message : 'Failed to cancel event';
    } finally {
      this.cancellingId = null;
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

  private formatFutureTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return dateString;
      const diffMs = date.getTime() - Date.now();
      if (diffMs <= 0) return 'now';
      const diffSeconds = Math.round(diffMs / 1000);
      const diffMinutes = Math.round(diffMs / (1000 * 60));
      const diffHours = Math.round(diffMs / (1000 * 60 * 60));

      const rtf = new Intl.RelativeTimeFormat('en', { numeric: 'auto' });

      if (Math.abs(diffSeconds) < 60) {
        return rtf.format(diffSeconds, 'second');
      } else if (Math.abs(diffMinutes) < 60) {
        return rtf.format(diffMinutes, 'minute');
      } else {
        return rtf.format(diffHours, 'hour');
      }
    } catch {
      return dateString;
    }
  }

  private getPayloadAgent(payload: string): string {
    try {
      const p = JSON.parse(payload) as Record<string, unknown>;
      return (p.agentName as string) || (p.agentId as string) || '-';
    } catch {
      return '-';
    }
  }

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
            <h2>Scheduled Events</h2>
            <p>One-shot timed events for this project.</p>
          </div>
          <sl-button size="small" variant="default" @click=${this.openCreateDialog}>
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            New Event
          </sl-button>
        </div>

        ${this.loading
          ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading events...</div>`
          : this.error
            ? html`
                <div class="section-error">
                  ${this.error}
                  <sl-button size="small" @click=${() => this.loadEvents()}>Retry</sl-button>
                </div>
              `
            : this.events.length === 0
              ? html`
                  <div class="empty-state">
                    <sl-icon name="clock"></sl-icon>
                    <h3>No Scheduled Events</h3>
                    <p>Create a scheduled event to send messages at a future time.</p>
                    <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
                      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                      Schedule Event
                    </sl-button>
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
          <p>Loading scheduled events...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Events</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" @click=${() => this.loadEvents()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="list-header">
        <sl-button size="small" variant="primary" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          New Event
        </sl-button>
      </div>

      ${this.events.length === 0
        ? html`
            <div class="empty-state">
              <sl-icon name="clock"></sl-icon>
              <h3>No Scheduled Events</h3>
              <p>Create a scheduled event to send messages at a future time.</p>
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
              <th>Type</th>
              <th>Status</th>
              <th>Fire At</th>
              <th class="hide-mobile">Agent</th>
              <th class="hide-mobile">Created</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.events.map((evt) => this.renderEventRow(evt))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderEventRow(evt: ScheduledEvent) {
    const isPending = evt.status === 'pending';
    const fireTimeDisplay = isPending
      ? this.formatFutureTime(evt.fireAt)
      : this.formatRelativeTime(evt.firedAt ?? evt.fireAt);
    const agent = this.getPayloadAgent(evt.payload);
    const isCancelling = this.cancellingId === evt.id;

    return html`
      <tr>
        <td><span class="type-badge environment">${evt.eventType}</span></td>
        <td><span class="badge ${this.statusBadgeClass(evt.status)}">${evt.status}</span></td>
        <td><span class="meta-text">${fireTimeDisplay}</span></td>
        <td class="hide-mobile"><span class="meta-text">${agent}</span></td>
        <td class="hide-mobile">
          <span class="meta-text">${this.formatRelativeTime(evt.createdAt)}</span>
        </td>
        <td class="actions-cell">
          ${isPending
            ? html`
                <sl-icon-button
                  name="x-circle"
                  label="Cancel"
                  ?disabled=${isCancelling}
                  @click=${() => this.handleCancel(evt.id)}
                ></sl-icon-button>
              `
            : nothing}
        </td>
      </tr>
    `;
  }

  private statusBadgeClass(status: string): string {
    switch (status) {
      case 'pending':
        return 'inject-always';
      case 'fired':
        return 'variable';
      case 'cancelled':
        return 'inject-as-needed';
      case 'expired':
        return 'inject-as-needed';
      default:
        return '';
    }
  }

  private renderDialog() {
    return html`
      <sl-dialog
        label="Schedule Event"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
      >
        <form class="dialog-form" @submit=${this.handleCreate}>
          ${this.dialogError
            ? html`<div class="dialog-error">${this.dialogError}</div>`
            : nothing}

          <sl-input
            label="Target Agent"
            placeholder="agent-name"
            .value=${this.dialogAgent}
            @sl-input=${(e: Event) => (this.dialogAgent = (e.target as HTMLInputElement).value)}
            required
          ></sl-input>

          <sl-textarea
            label="Message"
            placeholder="Message to send"
            .value=${this.dialogMessage}
            @sl-input=${(e: Event) => (this.dialogMessage = (e.target as HTMLTextAreaElement).value)}
            required
          ></sl-textarea>

          <div class="radio-field">
            <span class="radio-field-label">Timing</span>
            <sl-radio-group
              .value=${this.dialogTimingMode}
              @sl-change=${(e: Event) =>
                (this.dialogTimingMode = (e.target as HTMLInputElement).value as 'in' | 'at')}
            >
              <sl-radio-button value="in">In duration</sl-radio-button>
              <sl-radio-button value="at">At time</sl-radio-button>
            </sl-radio-group>
          </div>

          ${this.dialogTimingMode === 'in'
            ? html`
                <sl-input
                  label="Duration"
                  placeholder="30m, 1h, 2h30m"
                  .value=${this.dialogDuration}
                  @sl-input=${(e: Event) =>
                    (this.dialogDuration = (e.target as HTMLInputElement).value)}
                  required
                ></sl-input>
              `
            : html`
                <sl-input
                  label="Date & Time"
                  type="datetime-local"
                  .value=${this.dialogDatetime}
                  @sl-input=${(e: Event) =>
                    (this.dialogDatetime = (e.target as HTMLInputElement).value)}
                  required
                ></sl-input>
              `}

          <label class="checkbox-label">
            <input
              type="checkbox"
              .checked=${this.dialogInterrupt}
              @change=${(e: Event) =>
                (this.dialogInterrupt = (e.target as HTMLInputElement).checked)}
            />
            <span class="checkbox-text">
              <span>Interrupt agent</span>
              <span class="checkbox-description"
                >Interrupt the agent's current task before delivering the message.</span
              >
            </span>
          </label>
        </form>

        <sl-button slot="footer" variant="default" @click=${this.closeDialog}>Cancel</sl-button>
        <sl-button
          slot="footer"
          variant="primary"
          ?loading=${this.dialogLoading}
          @click=${this.handleCreate}
          >Create</sl-button
        >
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-scheduled-event-list': ScionScheduledEventList;
  }
}
