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

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { PaginationError } from '../../client/paginate-all.js';
import { resourceStyles } from './resource-styles.js';
import {
  effectiveTimeZone,
  formatInstantWithZone,
  formatRelative,
  parseWallClock,
} from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { FEATURE_FLAGS_CHANGED_EVENT, execSchedulesEnabled } from '../../utils/feature-flags.js';
import {
  type ExecAgentOption,
  type ExecRunSummary,
  loadProjectExecAgents,
  parseExecPayload,
  parseExecTimeoutValue,
  renderExecAgentPicker,
  renderExecArgvEditor,
  renderExecDetailPayload,
  renderExecRunSummaryRows,
  renderExecTimeoutInput,
  scheduleExecStyles,
  validateExecArgv,
  validateExecTarget,
} from './schedule-exec.js';

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
  execSummary?: ExecRunSummary;
}

interface ListResponse {
  events?: ScheduledEvent[];
  nextCursor?: string;
  totalCount?: number;
  serverTime?: string;
}

/** Page size requested per page when loading scheduled events. */
export const SCHEDULED_EVENT_PAGE_SIZE = 25;

export type ScheduledEventTab = 'pending' | 'past';

interface TabState {
  events: ScheduledEvent[];
  nextCursor: string;
  totalCount: number | null;
  loading: boolean;
  loadingMore: boolean;
  loaded: boolean;
  error: string | null;
  loadMoreError: string | null;
  generation: number;
}

function createTabState(loading = false): TabState {
  return {
    events: [],
    nextCursor: '',
    totalCount: null,
    loading,
    loadingMore: false,
    loaded: false,
    error: null,
    loadMoreError: null,
    generation: 0,
  };
}

@customElement('scion-scheduled-event-list')
export class ScionScheduledEventList extends LitElement {
  /**
   * Re-renders this list when the effective display zone changes (review
   * R4-1), so the create dialog's "Times in: <zone>" label never shows a
   * zone other than the one `handleCreate`'s `parseWallClock` call parses
   * `dialogDatetime` in. Unlike `access-boundary-schedule-editor.ts`,
   * `dialogDatetime` is raw typed text with no instant cached from it
   * until submit, so there is no stale cached value to re-derive here —
   * keeping the label in sync with the live parse zone is the whole fix.
   */
  readonly _zone = new DisplayZoneController(this);

  @property() projectId = '';
  @property({ type: Boolean }) compact = false;

  @state() private activeTab: ScheduledEventTab = 'pending';
  @state() private tabs: Record<ScheduledEventTab, TabState> = {
    pending: createTabState(true),
    past: createTabState(false),
  };

  // Create dialog
  @state() private dialogOpen = false;
  @state() private dialogEventType = 'message';
  @state() private dialogAgent = '';
  @state() private dialogMessage = '';
  @state() private dialogTimingMode: 'in' | 'at' = 'in';
  @state() private dialogDuration = '30m';
  @state() private dialogDatetime = '';
  @state() private dialogInterrupt = false;
  @state() private dialogWake = false;
  @state() private dialogExecAgentId = '';
  @state() private dialogExecCommand: string[] = [''];
  @state() private dialogExecTimeout = '';
  @state() private dialogLoading = false;
  @state() private dialogError: string | null = null;

  // Project agents for exec picker (loaded lazily when exec is chosen)
  @state() private execAgents: ExecAgentOption[] = [];
  @state() private execAgentsLoading = false;
  @state() private execAgentsError: string | null = null;
  private execAgentsLoadedForProject = '';
  private execAgentsGeneration = 0;

  // Cancel state
  @state() private cancellingId: string | null = null;

  // Detail dialog
  @state() private detailEvent: ScheduledEvent | null = null;
  @state() private detailOpen = false;
  @state() private outputLoading = false;
  @state() private outputError: string | null = null;
  @state() private outputText: string | null = null;
  @state() private outputFetched = false;

  private readonly boundOnFlagsChanged = (): void => {
    if (!execSchedulesEnabled() && this.dialogEventType === 'exec') {
      this.dialogEventType = 'message';
    }
    this.requestUpdate();
  };

  static override styles = [
    resourceStyles,
    scheduleExecStyles,
    css`
      .detail-row {
        padding: 0.375rem 0;
        font-size: 0.875rem;
        color: var(--scion-text, #1e293b);
        line-height: 1.5;
      }
      .detail-row strong {
        display: inline-block;
        min-width: 100px;
        color: var(--scion-text-muted, #64748b);
        font-weight: 600;
        font-size: 0.8125rem;
      }
      .events-tab-group {
        margin-top: 0.25rem;
      }
      .load-more-footer {
        display: flex;
        flex-direction: column;
        align-items: center;
        gap: 0.5rem;
        padding: 0.75rem 0 0.25rem;
      }
      .load-more-controls {
        display: flex;
        align-items: center;
        gap: 0.75rem;
      }
      .load-more-count {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
      }
      .load-more-error {
        font-size: 0.8125rem;
        color: var(--scion-danger, #dc2626);
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    void this.loadEvents();
  }

  override disconnectedCallback(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    super.disconnectedCallback();
  }

  private updateTab(tab: ScheduledEventTab, patch: Partial<TabState>): void {
    this.tabs = {
      ...this.tabs,
      [tab]: { ...this.tabs[tab], ...patch },
    };
  }

  private handleTabShow(e: CustomEvent): void {
    if (e.target !== e.currentTarget) return;
    e.stopPropagation();
    const name = (e.detail as { name?: string } | undefined)?.name;
    if (name !== 'pending' && name !== 'past') return;
    if (this.activeTab === name) return;
    this.activeTab = name;
    const state = this.tabs[name];
    if (!state.loaded && !state.loading) {
      void this.loadTab(name);
    }
  }

  private async loadEvents(): Promise<void> {
    await this.loadTab(this.activeTab);
  }

  private async loadTab(tab: ScheduledEventTab, append = false): Promise<void> {
    if (!this.projectId) return;
    const current = this.tabs[tab];
    if (append && (current.loadingMore || !current.nextCursor)) return;

    const generation = current.generation + 1;
    const cursor = append ? current.nextCursor : '';
    this.updateTab(tab, {
      generation,
      ...(append
        ? { loadingMore: true, loadMoreError: null }
        : { loading: true, loadingMore: false, error: null, loadMoreError: null }),
    });

    const sort = tab === 'pending' ? 'fire_at_asc' : 'completed_desc';
    const params = new URLSearchParams({
      group: tab,
      sort,
      limit: String(SCHEDULED_EVENT_PAGE_SIZE),
    });
    if (cursor) {
      params.set('cursor', cursor);
    }

    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/scheduled-events?${params.toString()}`
      );
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}`));
      }
      const data = (await response.json()) as ListResponse;
      if (this.tabs[tab].generation !== generation) return;
      const pageItems = data.events ?? [];
      const nextEvents = append ? [...this.tabs[tab].events, ...pageItems] : pageItems;
      this.updateTab(tab, {
        events: nextEvents,
        nextCursor: data.nextCursor ?? '',
        totalCount: typeof data.totalCount === 'number' ? data.totalCount : null,
        loaded: true,
        loading: false,
        loadingMore: false,
        error: null,
        loadMoreError: null,
      });
    } catch (err) {
      if (this.tabs[tab].generation !== generation) return;
      console.error(`Failed to load ${tab} scheduled events:`, err);
      const message = err instanceof Error ? err.message : 'Failed to load scheduled events';
      if (append) {
        this.updateTab(tab, {
          loadingMore: false,
          loadMoreError: message,
        });
      } else {
        this.updateTab(tab, {
          loading: false,
          loadingMore: false,
          error: message,
        });
      }
    }
  }

  private async ensureExecAgentsLoaded(): Promise<void> {
    if (!this.projectId) return;
    if (
      this.execAgentsLoadedForProject === this.projectId &&
      !this.execAgentsError &&
      this.execAgents.length > 0
    ) {
      return;
    }
    const gen = ++this.execAgentsGeneration;
    this.execAgentsLoading = true;
    this.execAgentsError = null;
    try {
      const agents = await loadProjectExecAgents(
        this.projectId,
        () => gen === this.execAgentsGeneration
      );
      if (gen !== this.execAgentsGeneration) return;
      this.execAgents = agents;
      this.execAgentsLoadedForProject = this.projectId;
    } catch (err) {
      if (gen !== this.execAgentsGeneration) return;
      this.execAgentsError =
        err instanceof PaginationError && err.hubMessage
          ? err.hubMessage
          : err instanceof Error
            ? err.message
            : 'Failed to load agents';
    } finally {
      if (gen === this.execAgentsGeneration) this.execAgentsLoading = false;
    }
  }

  private openCreateDialog(): void {
    this.dialogEventType = 'message';
    this.dialogAgent = '';
    this.dialogMessage = '';
    this.dialogTimingMode = 'in';
    this.dialogDuration = '30m';
    this.dialogDatetime = '';
    this.dialogInterrupt = false;
    this.dialogWake = false;
    this.dialogExecAgentId = '';
    this.dialogExecCommand = [''];
    this.dialogExecTimeout = '';
    this.dialogError = null;
    this.dialogOpen = true;
  }

  private closeDialog(): void {
    this.dialogOpen = false;
    this.dialogError = null;
  }

  private handleEventTypeChange(nextType: string): void {
    this.dialogEventType = nextType;
    this.dialogError = null;
    if (nextType === 'exec') {
      void this.ensureExecAgentsLoaded();
    }
  }

  private async handleCreate(e: Event): Promise<void> {
    e.preventDefault();
    this.dialogLoading = true;
    this.dialogError = null;

    try {
      const isExec = execSchedulesEnabled() && this.dialogEventType === 'exec';
      let body: Record<string, unknown>;

      if (isExec) {
        const targetErr = validateExecTarget(this.dialogExecAgentId);
        if (targetErr) {
          this.dialogError = targetErr;
          return;
        }
        const argvErr = validateExecArgv(this.dialogExecCommand);
        if (argvErr) {
          this.dialogError = argvErr;
          return;
        }
        const timeoutRes = parseExecTimeoutValue(this.dialogExecTimeout);
        if ('error' in timeoutRes) {
          this.dialogError = timeoutRes.error;
          return;
        }
        const target = this.dialogExecAgentId.trim();
        const pickedById = this.execAgents.some((a) => a.id === target);
        body = {
          eventType: 'exec',
          ...(pickedById || !this.execAgentsError ? { agentId: target } : { agentName: target }),
          command: [...this.dialogExecCommand],
          ...(timeoutRes.value > 0 ? { timeoutSeconds: timeoutRes.value } : {}),
        };
      } else {
        body = {
          eventType: 'message',
          agentName: this.dialogAgent,
          message: this.dialogMessage,
          interrupt: this.dialogInterrupt,
          wake: this.dialogWake,
        };
      }

      if (this.dialogTimingMode === 'in') {
        body.fireIn = this.dialogDuration;
      } else {
        // Interpret the datetime-local value as wall-clock time in the
        // effective display zone (not the browser's zone, which is what
        // `new Date(value).toISOString()` would use).
        const iso = parseWallClock(this.dialogDatetime, effectiveTimeZone());
        if (!iso) {
          this.dialogError = 'Enter a valid date and time';
          return;
        }
        body.fireAt = iso;
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
      const otherTab: ScheduledEventTab = this.activeTab === 'pending' ? 'past' : 'pending';
      this.updateTab(otherTab, { loaded: false });
      await this.loadTab(this.activeTab);
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

      this.updateTab('past', { loaded: false });
      await this.loadTab(this.activeTab);
      if (this.detailEvent?.id === eventId) {
        this.detailEvent = this.tabs[this.activeTab].events.find((e) => e.id === eventId) ?? null;
        if (!this.detailEvent) this.detailOpen = false;
      }
    } catch (err) {
      console.error('Failed to cancel event:', err);
      const message = err instanceof Error ? err.message : 'Failed to cancel event';
      this.updateTab(this.activeTab, { error: message });
    } finally {
      this.cancellingId = null;
    }
  }

  private showDetail(evt: ScheduledEvent): void {
    this.detailEvent = evt;
    this.detailOpen = true;
    this.outputLoading = false;
    this.outputError = null;
    this.outputText = null;
    this.outputFetched = false;
  }

  private closeDetail(): void {
    this.detailOpen = false;
    this.detailEvent = null;
    this.outputLoading = false;
    this.outputError = null;
    this.outputText = null;
    this.outputFetched = false;
  }

  private async handleViewOutput(eventId: string): Promise<void> {
    this.outputLoading = true;
    this.outputError = null;
    this.outputText = null;
    this.outputFetched = false;

    try {
      const response = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/scheduled-events/${encodeURIComponent(eventId)}/output`
      );
      if (this.detailEvent?.id !== eventId) return;
      if (response.status === 403) {
        this.outputError = 'Output unavailable: requires attach on the agent.';
        return;
      }
      if (!response.ok) {
        this.outputError = await extractApiError(response, `HTTP ${response.status}`);
        return;
      }
      const data = (await response.json()) as {
        captureMode?: string;
        outputStatus?: string;
        output?: unknown;
      };
      this.outputFetched = true;
      if (data.output === undefined) {
        this.outputText = null;
      } else if (data.captureMode === 'json' || typeof data.output !== 'string') {
        this.outputText = JSON.stringify(data.output, null, 2);
      } else {
        this.outputText = data.output;
      }
    } catch (err) {
      if (this.detailEvent?.id !== eventId) return;
      this.outputError = err instanceof Error ? err.message : 'Failed to load output';
    } finally {
      if (this.detailEvent?.id === eventId) {
        this.outputLoading = false;
      }
    }
  }

  /** Relative fire time; an overdue pending instant reads "now", as before. */
  private formatFutureTime(dateString: string): string {
    const ms = new Date(dateString).getTime();
    if (!Number.isNaN(ms) && ms <= Date.now()) return 'now';
    return formatRelative(dateString);
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

  private formatTabLabel(tab: ScheduledEventTab): string {
    const base = tab === 'pending' ? 'Pending' : 'Past';
    const state = this.tabs[tab];
    if (state.loaded && state.totalCount !== null) {
      return `${base} (${state.totalCount})`;
    }
    return base;
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

        ${this.renderTabGroup()} ${this.renderDialog()} ${this.renderDetailDialog()}
      </div>
    `;
  }

  private renderFull() {
    return html`
      <div class="list-header">
        <sl-button size="small" variant="primary" @click=${this.openCreateDialog}>
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          New Event
        </sl-button>
      </div>

      ${this.renderTabGroup()} ${this.renderDialog()} ${this.renderDetailDialog()}
    `;
  }

  private renderTabGroup() {
    return html`
      <sl-tab-group
        class="events-tab-group"
        @sl-tab-show=${(e: CustomEvent): void => this.handleTabShow(e)}
      >
        <sl-tab slot="nav" panel="pending" ?active=${this.activeTab === 'pending'}
          >${this.formatTabLabel('pending')}</sl-tab
        >
        <sl-tab slot="nav" panel="past" ?active=${this.activeTab === 'past'}
          >${this.formatTabLabel('past')}</sl-tab
        >

        <sl-tab-panel name="pending" ?active=${this.activeTab === 'pending'}>
          ${this.renderTabPanel('pending')}
        </sl-tab-panel>

        <sl-tab-panel name="past" ?active=${this.activeTab === 'past'}>
          ${this.renderTabPanel('past')}
        </sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private renderTabPanel(tab: ScheduledEventTab) {
    const state = this.tabs[tab];
    if (state.loading) {
      return this.compact
        ? html`<div class="section-loading"><sl-spinner></sl-spinner> Loading events...</div>`
        : html`
            <div class="loading-state">
              <sl-spinner></sl-spinner>
              <p>Loading ${tab === 'pending' ? 'pending' : 'past'} events...</p>
            </div>
          `;
    }

    if (state.error) {
      return this.compact
        ? html`
            <div class="section-error">
              ${state.error}
              <sl-button size="small" @click=${() => void this.loadTab(tab)}>Retry</sl-button>
            </div>
          `
        : html`
            <div class="error-state">
              <sl-icon name="exclamation-triangle"></sl-icon>
              <h2>Failed to Load Events</h2>
              <div class="error-details">${state.error}</div>
              <sl-button variant="primary" @click=${() => void this.loadTab(tab)}>
                <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                Retry
              </sl-button>
            </div>
          `;
    }

    if (state.events.length === 0) {
      return tab === 'pending'
        ? html`
            <div class="empty-state">
              <sl-icon name="clock"></sl-icon>
              <h3>No Pending Events</h3>
              <p>Create a scheduled event to send messages at a future time.</p>
              ${this.compact
                ? html`
                    <sl-button variant="primary" size="small" @click=${this.openCreateDialog}>
                      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
                      Schedule Event
                    </sl-button>
                  `
                : nothing}
            </div>
          `
        : html`
            <div class="empty-state">
              <sl-icon name="clock"></sl-icon>
              <h3>No Past Events</h3>
              <p>Completed, failed, and cancelled events from the last 7 days appear here.</p>
            </div>
          `;
    }

    return this.renderTable(tab, state);
  }

  private renderTable(tab: ScheduledEventTab, state: TabState) {
    const timeHeader = tab === 'pending' ? 'Fire At' : 'Completed';
    return html`
      <div class="table-container">
        <table>
          <thead>
            <tr>
              <th>Type</th>
              <th>Status</th>
              <th>${timeHeader}</th>
              <th class="hide-mobile">Agent</th>
              <th class="hide-mobile">Created</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${state.events.map((evt) => this.renderEventRow(evt))}
          </tbody>
        </table>
      </div>
      ${state.nextCursor || state.loadMoreError
        ? html`
            <div class="load-more-footer">
              ${state.loadMoreError
                ? html`<div class="load-more-error">${state.loadMoreError}</div>`
                : nothing}
              <div class="load-more-controls">
                ${state.totalCount !== null
                  ? html`<span class="load-more-count"
                      >Showing ${state.events.length} of ${state.totalCount}</span
                    >`
                  : nothing}
                ${state.nextCursor
                  ? html`
                      <sl-button
                        size="small"
                        class="load-more-btn"
                        ?loading=${state.loadingMore}
                        @click=${(): void => void this.loadTab(tab, true)}
                        >Load more</sl-button
                      >
                    `
                  : nothing}
              </div>
            </div>
          `
        : nothing}
    `;
  }

  private renderEventRow(evt: ScheduledEvent) {
    const isPending = evt.status === 'pending';
    const isActivePending = evt.status === 'pending' || evt.status === 'firing';
    const fireInstant = isActivePending ? evt.fireAt : (evt.firedAt ?? evt.createdAt);
    const fireTimeDisplay = isActivePending
      ? this.formatFutureTime(evt.fireAt)
      : formatRelative(fireInstant);
    const agent = this.getPayloadAgent(evt.payload);
    const isCancelling = this.cancellingId === evt.id;

    return html`
      <tr @click=${(): void => this.showDetail(evt)} style="cursor: pointer;">
        <td><span class="type-badge environment">${evt.eventType}</span></td>
        <td><span class="badge ${this.statusBadgeClass(evt.status)}">${evt.status}</span></td>
        <td>
          <span class="meta-text" title=${formatInstantWithZone(fireInstant)}
            >${fireTimeDisplay}</span
          >
        </td>
        <td class="hide-mobile"><span class="meta-text">${agent}</span></td>
        <td class="hide-mobile">
          <span class="meta-text">${formatRelative(evt.createdAt)}</span>
        </td>
        <td class="actions-cell" @click=${(e: Event): void => e.stopPropagation()}>
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
      case 'firing':
        return 'sensitive';
      case 'fired':
        return 'variable';
      case 'failed':
        return 'secret';
      case 'skipped':
      case 'cancelled':
      case 'expired':
        return 'inject-as-needed';
      default:
        return '';
    }
  }

  private renderDialog() {
    const execFlagOn = execSchedulesEnabled();
    const isExec = execFlagOn && this.dialogEventType === 'exec';

    return html`
      <sl-dialog
        label="Schedule Event"
        ?open=${this.dialogOpen}
        @sl-request-close=${this.closeDialog}
      >
        <form class="dialog-form" @submit=${this.handleCreate}>
          ${this.dialogError ? html`<div class="dialog-error">${this.dialogError}</div>` : nothing}
          ${execFlagOn
            ? html`
                <sl-select
                  label="Event Type"
                  class="event-type-select"
                  .value=${this.dialogEventType}
                  @sl-change=${(e: Event): void =>
                    this.handleEventTypeChange((e.target as HTMLSelectElement).value)}
                >
                  <sl-option value="message">Message</sl-option>
                  <sl-option value="exec">Exec Command</sl-option>
                </sl-select>
              `
            : nothing}
          ${isExec
            ? html`
                ${renderExecAgentPicker({
                  agents: this.execAgents,
                  loading: this.execAgentsLoading,
                  loadError: this.execAgentsError,
                  value: this.dialogExecAgentId,
                  onChange: (v) => {
                    this.dialogExecAgentId = v;
                  },
                })}
                ${renderExecArgvEditor(this.dialogExecCommand, (updater) => {
                  this.dialogExecCommand = updater(this.dialogExecCommand);
                })}
                ${renderExecTimeoutInput(this.dialogExecTimeout, (v) => {
                  this.dialogExecTimeout = v;
                })}
              `
            : html`
                <sl-input
                  label="Target Agent"
                  placeholder="agent-name"
                  .value=${this.dialogAgent}
                  @sl-input=${(e: Event) =>
                    (this.dialogAgent = (e.target as HTMLInputElement).value)}
                  required
                ></sl-input>

                <sl-textarea
                  label="Message"
                  placeholder="Message to send"
                  .value=${this.dialogMessage}
                  @sl-input=${(e: Event) =>
                    (this.dialogMessage = (e.target as HTMLTextAreaElement).value)}
                  required
                ></sl-textarea>
              `}

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
                  help-text="Times in: ${effectiveTimeZone()}"
                  type="datetime-local"
                  .value=${this.dialogDatetime}
                  @sl-input=${(e: Event) =>
                    (this.dialogDatetime = (e.target as HTMLInputElement).value)}
                  required
                ></sl-input>
              `}
          ${isExec
            ? nothing
            : html`
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

                <label class="checkbox-label">
                  <input
                    type="checkbox"
                    .checked=${this.dialogWake}
                    @change=${(e: Event) =>
                      (this.dialogWake = (e.target as HTMLInputElement).checked)}
                  />
                  <span class="checkbox-text">
                    <span>Wake agent</span>
                    <span class="checkbox-description"
                      >Resume the agent if it is suspended when the message fires. Requires
                      permission to start the agent.</span
                    >
                  </span>
                </label>
              `}
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

  private renderDetailDialog(): TemplateResult | typeof nothing {
    const evt = this.detailEvent;
    if (!evt) return nothing;

    const isExec = evt.eventType === 'exec';
    const execPayload = isExec ? parseExecPayload(evt.payload) : null;
    const agent = execPayload
      ? execPayload.agentName || execPayload.agentId || '-'
      : this.getPayloadAgent(evt.payload);

    let payloadDetails: Record<string, unknown> = {};
    if (!isExec) {
      try {
        const parsed: unknown = JSON.parse(evt.payload);
        if (parsed && typeof parsed === 'object') {
          payloadDetails = parsed as Record<string, unknown>;
        }
      } catch {
        // ignore
      }
    }

    const isPending = evt.status === 'pending';

    return html`
      <sl-dialog
        label="Scheduled Event"
        ?open=${this.detailOpen}
        @sl-request-close=${this.closeDetail}
      >
        <div class="dialog-form">
          <div class="detail-row">
            <strong>ID:</strong>
            <span style="font-family: var(--scion-font-mono, monospace); font-size: 0.8125rem;"
              >${evt.id}</span
            >
          </div>
          <div class="detail-row">
            <strong>Status:</strong>
            <span class="badge ${this.statusBadgeClass(evt.status)}">${evt.status}</span>
          </div>
          <div class="detail-row"><strong>Event Type:</strong> ${evt.eventType}</div>
          <div class="detail-row">
            <strong>Fire At:</strong> ${isPending ? `${this.formatFutureTime(evt.fireAt)} · ` : ''}
            <span class="fire-at-absolute">${formatInstantWithZone(evt.fireAt)}</span>
          </div>
          ${evt.firedAt
            ? html`<div class="detail-row">
                <strong>Fired At:</strong> ${formatRelative(evt.firedAt)} ·
                <span class="fired-at-absolute">${formatInstantWithZone(evt.firedAt)}</span>
              </div>`
            : nothing}
          <div class="detail-row"><strong>Target Agent:</strong> ${agent}</div>
          ${isExec && execPayload
            ? html`
                ${renderExecRunSummaryRows(evt.execSummary)} ${renderExecDetailPayload(execPayload)}
                ${evt.execSummary?.captureMode
                  ? html`
                      <div class="exec-output-section">
                        <div>
                          <sl-button
                            size="small"
                            variant="default"
                            class="exec-view-output-btn"
                            ?loading=${this.outputLoading}
                            @click=${() => this.handleViewOutput(evt.id)}
                          >
                            View output
                          </sl-button>
                        </div>
                        ${this.outputError
                          ? html`<div class="exec-output-error">${this.outputError}</div>`
                          : this.outputFetched
                            ? this.outputText !== null
                              ? html`<pre class="exec-output-pre">${this.outputText}</pre>`
                              : html`<div class="exec-no-output-note">
                                  No captured output (${evt.execSummary.outputStatus ?? 'none'}).
                                </div>`
                            : nothing}
                      </div>
                    `
                  : nothing}
              `
            : nothing}
          ${evt.eventType === 'message' && payloadDetails.message
            ? html`<div class="detail-row">
                <strong>Message:</strong> ${payloadDetails.message}
              </div>`
            : nothing}
          ${evt.eventType === 'message'
            ? html`
                <div class="detail-row">
                  <strong>Wake:</strong> ${payloadDetails.wake === true ? 'Yes' : 'No'}
                </div>
                <div class="detail-row">
                  <strong>Interrupt:</strong> ${payloadDetails.interrupt === true ? 'Yes' : 'No'}
                </div>
              `
            : nothing}
          ${evt.eventType === 'dispatch_agent' && payloadDetails.template
            ? html`<div class="detail-row">
                <strong>Template:</strong> ${payloadDetails.template}
              </div>`
            : nothing}
          ${evt.eventType === 'dispatch_agent' && payloadDetails.task
            ? html`<div class="detail-row"><strong>Task:</strong> ${payloadDetails.task}</div>`
            : nothing}
          ${evt.error
            ? html`<div class="detail-row">
                <strong>Error:</strong>
                <span style="color: var(--sl-color-danger-600, #dc2626);">${evt.error}</span>
              </div>`
            : nothing}
          <div class="detail-row">
            <strong>Created:</strong> ${formatRelative(evt.createdAt)}${evt.createdBy
              ? ` by ${evt.createdBy}`
              : ''}
          </div>
        </div>

        <sl-button slot="footer" variant="default" @click=${this.closeDetail}>Close</sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-scheduled-event-list': ScionScheduledEventList;
  }
}
