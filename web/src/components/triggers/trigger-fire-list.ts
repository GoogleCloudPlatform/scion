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
 * Project-wide Trigger Fires History tab (Stage 2 §9.1 / §9.2, GAR G7 carry #20).
 *
 * Renders all fire statuses generically, including `waiting` (with `waitUntil`),
 * `expired`, `coalescing` (with `notBefore`), and `deferred` (with `deferredAt`
 * and `notBefore`).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { resourceStyles } from '../shared/resource-styles.js';
import {
  fireStatusBadgeVariant,
  type ListTriggerFiresResponse,
  type TriggerDefinition,
  type TriggerFire,
} from './types.js';

const ALL_FIRE_STATUSES = [
  'fired',
  'waiting',
  'coalescing',
  'deferred',
  'firing',
  'failed',
  'refused',
  'skipped',
  'expired',
] as const;

@customElement('scion-trigger-fire-list')
export class ScionTriggerFireList extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: String })
  projectId = '';

  @property({ type: Array })
  triggers: TriggerDefinition[] = [];

  @state() private fires: TriggerFire[] = [];
  @state() private nextCursor = '';
  @state() private totalCount = 0;
  @state() private loading = true;
  @state() private loadingMore = false;
  @state() private error: string | null = null;

  // Filters
  @state() private filterTriggerId = '';
  @state() private filterStatus = '';
  @state() private filterSince = '';
  @state() private filterReason = '';

  static override styles = [
    resourceStyles,
    css`
      .filter-bar {
        display: flex;
        flex-wrap: wrap;
        align-items: flex-end;
        gap: 0.75rem;
        margin-bottom: 1rem;
      }

      .filter-bar sl-select,
      .filter-bar sl-input {
        min-width: 10.5rem;
      }

      .mono {
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
      }

      .muted {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
      }

      .link-btn {
        background: none;
        border: none;
        padding: 0;
        color: var(--scion-primary);
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
        cursor: pointer;
        text-decoration: underline;
      }

      .agent-link {
        color: var(--scion-primary);
        text-decoration: none;
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
      }

      .agent-link:hover {
        text-decoration: underline;
      }

      .status-meta {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
        margin-top: 0.25rem;
      }

      .load-more {
        display: flex;
        justify-content: center;
        padding: 1rem 0;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      void this.loadFires(false);
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('projectId') && this.projectId) {
      void this.loadFires(false);
    }
  }

  private computeSinceIso(): string {
    if (!this.filterSince) return '';
    const now = Date.now();
    const hours =
      this.filterSince === '1h'
        ? 1
        : this.filterSince === '24h'
          ? 24
          : this.filterSince === '7d'
            ? 168
            : 0;
    if (!hours) return '';
    return new Date(now - hours * 3600 * 1000).toISOString();
  }

  async loadFires(append: boolean): Promise<void> {
    if (!this.projectId) return;
    if (append) {
      this.loadingMore = true;
    } else {
      this.loading = true;
      this.error = null;
    }
    try {
      const params = new URLSearchParams({ limit: '50' });
      if (this.filterTriggerId) params.set('triggerId', this.filterTriggerId);
      if (this.filterStatus) params.set('status', this.filterStatus);
      const sinceIso = this.computeSinceIso();
      if (sinceIso) params.set('since', sinceIso);
      if (append && this.nextCursor) params.set('cursor', this.nextCursor);

      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/trigger-fires?${params.toString()}`
      );
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      const data = (await res.json()) as ListTriggerFiresResponse;
      const pageItems = Array.isArray(data.fires) ? data.fires : [];
      this.fires = append ? [...this.fires, ...pageItems] : pageItems;
      this.nextCursor = data.nextCursor ?? '';
      this.totalCount = data.totalCount ?? this.fires.length;
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load fire history';
    } finally {
      this.loading = false;
      this.loadingMore = false;
    }
  }

  private triggerNameFor(triggerId: string): string {
    const found = this.triggers.find((t) => t.id === triggerId);
    return found ? found.name : triggerId.slice(0, 8);
  }

  private filteredFires(): TriggerFire[] {
    const q = this.filterReason.trim().toLowerCase();
    if (!q) return this.fires;
    return this.fires.filter(
      (f) =>
        (f.error && f.error.toLowerCase().includes(q)) ||
        f.status.toLowerCase().includes(q) ||
        (f.decidedDialect && f.decidedDialect.toLowerCase().includes(q))
    );
  }

  private renderStatusDetails(fire: TriggerFire) {
    return html`
      <sl-badge variant=${fireStatusBadgeVariant(fire.status)} data-fire-status=${fire.status}>
        ${fire.status}
      </sl-badge>
      ${fire.waitUntil
        ? html`
            <div class="status-meta" data-fire-wait-until=${fire.id}>
              Wait until: ${formatInstantWithZone(fire.waitUntil)}
            </div>
          `
        : nothing}
      ${fire.notBefore
        ? html`
            <div class="status-meta" data-fire-not-before=${fire.id}>
              Not before: ${formatInstantWithZone(fire.notBefore)}
            </div>
          `
        : nothing}
      ${fire.deferredAt
        ? html`
            <div class="status-meta" data-fire-deferred-at=${fire.id}>
              Deferred at: ${formatInstantWithZone(fire.deferredAt)}
            </div>
          `
        : nothing}
    `;
  }

  private renderOutcome(fire: TriggerFire) {
    return html`
      <div>
        ${fire.resultRef
          ? fire.actionType === 'dispatch_agent'
            ? html`
                <a
                  class="agent-link"
                  href="/projects/${encodeURIComponent(this.projectId)}/agents/${encodeURIComponent(
                    fire.resultRef
                  )}"
                  data-fire-agent-link=${fire.resultRef}
                >
                  Agent ${fire.resultRef.slice(0, 8)}
                </a>
              `
            : html`<span class="mono" data-fire-result-ref=${fire.id}>${fire.resultRef}</span>`
          : nothing}
        ${fire.execSummary
          ? html`
              <div class="muted" data-fire-exec-summary=${fire.id}>
                exec: ${fire.execSummary.outcome}
                ${fire.execSummary.exitCode !== undefined
                  ? ` (exit ${fire.execSummary.exitCode})`
                  : ''}
                · ${fire.execSummary.durationMs}ms
              </div>
            `
          : nothing}
        ${fire.error
          ? html`<div class="muted" data-fire-error=${fire.id}>${fire.error}</div>`
          : nothing}
        ${fire.matchedThrough
          ? html`
              <div class="muted">
                Matched:
                ${fire.matchedThrough}${fire.decidedDialect ? ` (${fire.decidedDialect})` : ''}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  override render() {
    const visibleFires = this.filteredFires();
    return html`
      <div class="filter-bar">
        <sl-select
          label="Rule"
          size="small"
          hoist
          clearable
          .value=${this.filterTriggerId}
          data-filter-fire-trigger
          @sl-change=${(e: Event) => {
            this.filterTriggerId = (e.target as HTMLSelectElement).value;
            void this.loadFires(false);
          }}
        >
          ${this.triggers.map((t) => html`<sl-option value=${t.id}>${t.name}</sl-option>`)}
        </sl-select>

        <sl-select
          label="Status"
          size="small"
          hoist
          clearable
          .value=${this.filterStatus}
          data-filter-fire-status
          @sl-change=${(e: Event) => {
            this.filterStatus = (e.target as HTMLSelectElement).value;
            void this.loadFires(false);
          }}
        >
          ${ALL_FIRE_STATUSES.map((st) => html`<sl-option value=${st}>${st}</sl-option>`)}
        </sl-select>

        <sl-select
          label="Time range"
          size="small"
          hoist
          clearable
          .value=${this.filterSince}
          data-filter-fire-since
          @sl-change=${(e: Event) => {
            this.filterSince = (e.target as HTMLSelectElement).value;
            void this.loadFires(false);
          }}
        >
          <sl-option value="1h">Last hour</sl-option>
          <sl-option value="24h">Last 24 hours</sl-option>
          <sl-option value="7d">Last 7 days</sl-option>
        </sl-select>

        <sl-input
          label="Outcome reason"
          size="small"
          clearable
          placeholder="Filter error or reason..."
          .value=${this.filterReason}
          data-filter-fire-reason
          @sl-input=${(e: Event) => {
            this.filterReason = (e.target as HTMLInputElement).value;
          }}
        ></sl-input>

        <sl-button size="small" variant="default" @click=${() => this.loadFires(false)}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>

      ${this.error
        ? html`
            <sl-alert variant="danger" open style="margin-bottom: 1rem;">
              <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
              ${this.error}
            </sl-alert>
          `
        : nothing}
      ${this.loading
        ? html`
            <div class="loading-state">
              <sl-spinner></sl-spinner>
              <p>Loading fire history...</p>
            </div>
          `
        : visibleFires.length === 0
          ? html`
              <div class="empty-state">
                <sl-icon name="clock-history"></sl-icon>
                <h3>No trigger fires</h3>
                <p>Recorded trigger executions across this project will appear here.</p>
              </div>
            `
          : html`
              <div class="table-container">
                <table>
                  <thead>
                    <tr>
                      <th>Started</th>
                      <th>Rule</th>
                      <th>Event</th>
                      <th>Status</th>
                      <th>Outcome / Result</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${visibleFires.map(
                      (fire) => html`
                        <tr data-fire-row=${fire.id}>
                          <td title=${formatInstantWithZone(fire.startedAt)}>
                            ${formatRelative(fire.startedAt)}
                          </td>
                          <td>
                            <div>${this.triggerNameFor(fire.triggerId)}</div>
                            ${fire.actionType
                              ? html`
                                  <div class="muted">
                                    ${fire.actionType}${fire.actionIndex !== undefined
                                      ? ` #${fire.actionIndex + 1}`
                                      : ''}
                                  </div>
                                `
                              : nothing}
                          </td>
                          <td>
                            <button
                              type="button"
                              class="link-btn"
                              data-inspect-event=${fire.eventId}
                              @click=${() => {
                                this.dispatchEvent(
                                  new CustomEvent<{ eventId: string }>('inspect-event', {
                                    detail: { eventId: fire.eventId },
                                    bubbles: true,
                                    composed: true,
                                  })
                                );
                              }}
                            >
                              ${fire.eventId.slice(0, 8)}
                            </button>
                          </td>
                          <td>${this.renderStatusDetails(fire)}</td>
                          <td>${this.renderOutcome(fire)}</td>
                        </tr>
                      `
                    )}
                  </tbody>
                </table>
              </div>
              ${this.nextCursor
                ? html`
                    <div class="load-more">
                      <sl-button
                        size="small"
                        ?loading=${this.loadingMore}
                        data-load-more-fires
                        @click=${() => this.loadFires(true)}
                      >
                        Load more (${this.fires.length} of ${this.totalCount})
                      </sl-button>
                    </div>
                  `
                : nothing}
            `}
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-trigger-fire-list': ScionTriggerFireList;
  }
}
