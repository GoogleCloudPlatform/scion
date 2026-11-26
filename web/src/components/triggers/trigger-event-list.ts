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
 * Trigger Events tab (Stage 2 §9.1, GAR G7 per-source column & detail hooks).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { formatInstantWithZone, formatRelative } from '../../utils/time.js';
import { resourceStyles } from '../shared/resource-styles.js';
import {
  fireStatusBadgeVariant,
  type ListTriggerEventsResponse,
  type ListTriggerFiresResponse,
  type TriggerEventDetail,
  type TriggerEventSummary,
  type TriggerFire,
  type WebhookEndpoint,
} from './types.js';
import {
  ensureExtensionLoaded,
  getEventColumns,
  getEventColumnsRegistration,
  getEventDetail,
  type EventColumnDef,
} from './source-extensions.js';

@customElement('scion-trigger-event-list')
export class ScionTriggerEventList extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: String })
  projectId = '';

  @property({ type: Array })
  endpoints: WebhookEndpoint[] = [];

  @state() private events: TriggerEventSummary[] = [];
  @state() private nextCursor = '';
  @state() private totalCount = 0;
  @state() private loading = true;
  @state() private loadingMore = false;
  @state() private error: string | null = null;

  // Filters
  @state() private filterSourceId = '';
  @state() private filterStatus = '';
  @state() private filterSince = '';

  // Event detail dialog
  @state() private detailOpen = false;
  @state() private detailLoading = false;
  @state() private detailError: string | null = null;
  @state() private detailEvent: TriggerEventDetail | null = null;
  @state() private detailFires: TriggerFire[] = [];

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

      .filter-bar sl-select {
        min-width: 11rem;
      }

      .mono {
        font-family: var(--scion-font-mono);
        font-size: 0.8125rem;
      }

      .muted {
        font-size: 0.75rem;
        color: var(--scion-text-muted);
      }

      .clickable-row {
        cursor: pointer;
      }

      .load-more {
        display: flex;
        justify-content: center;
        padding: 1rem 0;
      }

      .detail-section {
        margin-bottom: 1rem;
      }

      .detail-title {
        font-size: 0.75rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: var(--scion-text-muted);
        margin-bottom: 0.375rem;
      }

      .json-viewer {
        margin: 0;
        padding: 0.75rem;
        background: var(--scion-bg-subtle);
        border: 1px solid var(--scion-border);
        border-radius: var(--scion-radius);
        font-family: var(--scion-font-mono);
        font-size: 0.75rem;
        max-height: 20rem;
        overflow: auto;
        white-space: pre-wrap;
        word-break: break-word;
      }

      .kv-table {
        width: 100%;
        border-collapse: collapse;
        font-size: 0.8125rem;
      }

      .kv-table td {
        padding: 0.375rem 0.5rem;
        border-bottom: 1px solid var(--scion-border);
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    if (this.projectId) {
      void this.loadEvents(false);
    }
  }

  override updated(changed: Map<string, unknown>): void {
    if (changed.has('projectId') && this.projectId) {
      void this.loadEvents(false);
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

  async loadEvents(append: boolean): Promise<void> {
    if (!this.projectId) return;
    if (append) {
      this.loadingMore = true;
    } else {
      this.loading = true;
      this.error = null;
    }
    try {
      const params = new URLSearchParams({ limit: '50' });
      if (this.filterSourceId) params.set('sourceId', this.filterSourceId);
      if (this.filterStatus) params.set('status', this.filterStatus);
      const sinceIso = this.computeSinceIso();
      if (sinceIso) params.set('since', sinceIso);
      if (append && this.nextCursor) params.set('cursor', this.nextCursor);

      const res = await apiFetch(
        `/api/v1/projects/${encodeURIComponent(this.projectId)}/trigger-events?${params.toString()}`
      );
      if (!res.ok) {
        this.error = await extractApiError(res, `HTTP ${res.status}`);
        return;
      }
      const data = (await res.json()) as ListTriggerEventsResponse;
      const pageItems = Array.isArray(data.events) ? data.events : [];
      this.events = append ? [...this.events, ...pageItems] : pageItems;
      this.nextCursor = data.nextCursor ?? '';
      this.totalCount = data.totalCount ?? this.events.length;

      // Lazily load any registered column extensions for source kinds in the result.
      const kinds = new Set(this.events.map((e) => e.source.kind));
      for (const k of kinds) {
        const reg = getEventColumnsRegistration(k);
        if (reg?.load) {
          void ensureExtensionLoaded(reg).then(() => this.requestUpdate());
        }
      }
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load events';
    } finally {
      this.loading = false;
      this.loadingMore = false;
    }
  }

  async openEventDetail(eventId: string): Promise<void> {
    if (!this.projectId || !eventId) return;
    this.detailOpen = true;
    this.detailLoading = true;
    this.detailError = null;
    this.detailEvent = null;
    this.detailFires = [];

    try {
      const base = `/api/v1/projects/${encodeURIComponent(this.projectId)}`;
      const [evRes, firesRes] = await Promise.all([
        apiFetch(`${base}/trigger-events/${encodeURIComponent(eventId)}`),
        apiFetch(`${base}/trigger-fires?eventId=${encodeURIComponent(eventId)}&limit=50`),
      ]);
      if (!evRes.ok) {
        this.detailError = await extractApiError(evRes, `HTTP ${evRes.status}`);
        return;
      }
      const ev = (await evRes.json()) as TriggerEventDetail;
      this.detailEvent = ev;

      const detailExt = getEventDetail(ev.source.kind);
      if (detailExt?.load) {
        void ensureExtensionLoaded(detailExt).then(() => this.requestUpdate());
      }

      if (firesRes.ok) {
        const firesData = (await firesRes.json()) as ListTriggerFiresResponse;
        this.detailFires = Array.isArray(firesData.fires) ? firesData.fires : [];
      }
    } catch (err) {
      this.detailError = err instanceof Error ? err.message : 'Failed to load event detail';
    } finally {
      this.detailLoading = false;
    }
  }

  private collectExtraColumns(): EventColumnDef[] {
    const seen = new Set<string>();
    const cols: EventColumnDef[] = [];
    const kinds = new Set(this.events.map((e) => e.source.kind));
    for (const kind of kinds) {
      for (const col of getEventColumns(kind)) {
        const key = `${kind}:${col.id}`;
        if (!seen.has(key)) {
          seen.add(key);
          cols.push(col);
        }
      }
    }
    return cols;
  }

  private statusVariant(status: string): 'success' | 'danger' | 'warning' | 'neutral' {
    switch (status) {
      case 'processed':
        return 'success';
      case 'failed':
        return 'danger';
      case 'pending':
        return 'warning';
      default:
        return 'neutral';
    }
  }

  private formatPayload(payload: unknown): string {
    if (payload === undefined || payload === null) return '(no inline payload)';
    if (typeof payload === 'string') return payload;
    try {
      return JSON.stringify(payload, null, 2);
    } catch {
      return '(unserializable payload)';
    }
  }

  override render() {
    const extraCols = this.collectExtraColumns();
    return html`
      <div class="filter-bar">
        <sl-select
          label="Source endpoint"
          size="small"
          hoist
          clearable
          .value=${this.filterSourceId}
          data-filter-event-source
          @sl-change=${(e: Event) => {
            this.filterSourceId = (e.target as HTMLSelectElement).value;
            void this.loadEvents(false);
          }}
        >
          ${this.endpoints.map((ep) => html`<sl-option value=${ep.id}>${ep.name}</sl-option>`)}
        </sl-select>

        <sl-select
          label="Status"
          size="small"
          hoist
          clearable
          .value=${this.filterStatus}
          data-filter-event-status
          @sl-change=${(e: Event) => {
            this.filterStatus = (e.target as HTMLSelectElement).value;
            void this.loadEvents(false);
          }}
        >
          <sl-option value="pending">Pending</sl-option>
          <sl-option value="processed">Processed</sl-option>
          <sl-option value="failed">Failed</sl-option>
        </sl-select>

        <sl-select
          label="Time range"
          size="small"
          hoist
          clearable
          .value=${this.filterSince}
          data-filter-event-since
          @sl-change=${(e: Event) => {
            this.filterSince = (e.target as HTMLSelectElement).value;
            void this.loadEvents(false);
          }}
        >
          <sl-option value="1h">Last hour</sl-option>
          <sl-option value="24h">Last 24 hours</sl-option>
          <sl-option value="7d">Last 7 days</sl-option>
        </sl-select>

        <sl-button size="small" variant="default" @click=${() => this.loadEvents(false)}>
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
              <p>Loading trigger events...</p>
            </div>
          `
        : this.events.length === 0
          ? html`
              <div class="empty-state">
                <sl-icon name="clock-history"></sl-icon>
                <h3>No trigger events</h3>
                <p>Events received on your project sources will appear here.</p>
              </div>
            `
          : html`
              <div class="table-container">
                <table>
                  <thead>
                    <tr>
                      <th>Received</th>
                      <th>Type</th>
                      <th>Source</th>
                      <th>Subject</th>
                      <th>Status</th>
                      <th>Matches / Fires</th>
                      ${extraCols.map(
                        (col) => html`<th data-ext-col-header=${col.id}>${col.header}</th>`
                      )}
                    </tr>
                  </thead>
                  <tbody>
                    ${this.events.map(
                      (ev) => html`
                        <tr
                          class="clickable-row"
                          data-event-row=${ev.id}
                          @click=${() => this.openEventDetail(ev.id)}
                        >
                          <td title=${formatInstantWithZone(ev.receivedAt)}>
                            ${formatRelative(ev.receivedAt)}
                          </td>
                          <td class="mono">${ev.type}</td>
                          <td>
                            <span class="mono">${ev.source.kind}</span>
                            <div class="muted">
                              ${ev.attributes?.['endpoint_name'] || ev.source.id}
                            </div>
                          </td>
                          <td class="mono">${ev.subject || '—'}</td>
                          <td>
                            <sl-badge variant=${this.statusVariant(ev.status)}
                              >${ev.status}</sl-badge
                            >
                          </td>
                          <td>${ev.matchCount} / ${ev.fireCount}</td>
                          ${extraCols.map(
                            (col) => html`<td data-ext-col-cell=${col.id}>${col.render(ev)}</td>`
                          )}
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
                        data-load-more-events
                        @click=${() => this.loadEvents(true)}
                      >
                        Load more (${this.events.length} of ${this.totalCount})
                      </sl-button>
                    </div>
                  `
                : nothing}
            `}
      ${this.renderDetailDialog()}
    `;
  }

  private renderDetailDialog() {
    const ev = this.detailEvent;
    const detailExt = ev ? getEventDetail(ev.source.kind) : undefined;
    const overflowDownloadUrl = ev
      ? `/api/v1/projects/${encodeURIComponent(this.projectId)}/trigger-events/${encodeURIComponent(ev.id)}?payload=1`
      : '';

    return html`
      <sl-dialog
        label=${ev ? `Event: ${ev.type}` : 'Event Detail'}
        style="--width: 48rem;"
        ?open=${this.detailOpen}
        data-event-detail-dialog
        @sl-after-hide=${(e: Event) => {
          if (e.target !== e.currentTarget) return;
          this.detailOpen = false;
          this.detailEvent = null;
          this.detailFires = [];
        }}
      >
        ${this.detailLoading
          ? html`
              <div class="loading-state">
                <sl-spinner></sl-spinner>
                <p>Loading event detail...</p>
              </div>
            `
          : this.detailError
            ? html`
                <sl-alert variant="danger" open>
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  ${this.detailError}
                </sl-alert>
              `
            : ev
              ? html`
                  <div class="detail-section">
                    <div class="detail-title">Metadata</div>
                    <table class="kv-table">
                      <tr>
                        <td class="muted">Event ID</td>
                        <td class="mono">${ev.id}</td>
                      </tr>
                      <tr>
                        <td class="muted">Received</td>
                        <td>${formatInstantWithZone(ev.receivedAt)}</td>
                      </tr>
                      <tr>
                        <td class="muted">Subject</td>
                        <td class="mono">${ev.subject || '—'}</td>
                      </tr>
                      <tr>
                        <td class="muted">Dedup Key</td>
                        <td class="mono">${ev.dedupKey}</td>
                      </tr>
                      <tr>
                        <td class="muted">Payload Size</td>
                        <td>${ev.payloadBytes} bytes</td>
                      </tr>
                    </table>
                  </div>

                  ${detailExt
                    ? html`
                        <div class="detail-section" data-ext-event-detail>
                          ${detailExt.render(ev)}
                        </div>
                      `
                    : nothing}
                  ${ev.attributes && Object.keys(ev.attributes).length > 0
                    ? html`
                        <div class="detail-section">
                          <div class="detail-title">Attributes</div>
                          <table class="kv-table">
                            ${Object.entries(ev.attributes).map(
                              ([k, v]) => html`
                                <tr>
                                  <td class="mono">${k}</td>
                                  <td class="mono">${v}</td>
                                </tr>
                              `
                            )}
                          </table>
                        </div>
                      `
                    : nothing}
                  ${ev.headers && Object.keys(ev.headers).length > 0
                    ? html`
                        <div class="detail-section">
                          <div class="detail-title">Headers</div>
                          <table class="kv-table">
                            ${Object.entries(ev.headers).map(
                              ([k, v]) => html`
                                <tr>
                                  <td class="mono">${k}</td>
                                  <td class="mono">${v}</td>
                                </tr>
                              `
                            )}
                          </table>
                        </div>
                      `
                    : nothing}

                  <div class="detail-section">
                    <div class="detail-title">External Payload</div>
                    ${ev.payloadTruncated || ev.payloadRef
                      ? html`
                          <sl-alert variant="warning" open style="margin-bottom: 0.5rem;">
                            Payload exceeds inline display limit.
                            <a
                              href=${overflowDownloadUrl}
                              target="_blank"
                              rel="noopener noreferrer"
                              style="margin-left: 0.5rem;"
                              >Download full payload</a
                            >
                          </sl-alert>
                        `
                      : nothing}
                    <pre class="json-viewer" data-event-payload>
${this.formatPayload(ev.payload)}</pre
                    >
                  </div>

                  <div class="detail-section">
                    <div class="detail-title">Fires Produced (${this.detailFires.length})</div>
                    ${this.detailFires.length === 0
                      ? html`<div class="muted">No fires recorded for this event.</div>`
                      : html`
                          <table class="kv-table">
                            ${this.detailFires.map(
                              (f) => html`
                                <tr>
                                  <td class="mono">${f.triggerId}</td>
                                  <td>
                                    <sl-badge variant=${fireStatusBadgeVariant(f.status)}>
                                      ${f.status}
                                    </sl-badge>
                                  </td>
                                  <td class="muted">${f.error || f.resultRef || '—'}</td>
                                </tr>
                              `
                            )}
                          </table>
                        `}
                  </div>
                `
              : nothing}

        <sl-button
          slot="footer"
          variant="primary"
          @click=${() => {
            this.detailOpen = false;
          }}
        >
          Close
        </sl-button>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-trigger-event-list': ScionTriggerEventList;
  }
}
