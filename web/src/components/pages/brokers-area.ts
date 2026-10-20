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
 * Brokers area list page (broker UI Option A, slice A1).
 *
 * Rendered for /brokers while the hub.flat_runtime_brokers experiment is on
 * (client/broker-routes.ts); the existing brokers page renders otherwise.
 * Columns: Name (with a marker for brokers hosted by this server), Kind,
 * Runtime target, Connection, Health (users holding hub.health.read only),
 * Agents / Cap. Filter chips: All, Flat, Legacy, Needs attention.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, RuntimeBroker } from '../../shared/types.js';
import { isFlatBroker, runtimeTargetLabel } from '../../shared/runtime-target.js';
import {
  BROKER_FILTERS,
  HEALTH_READ_PERMISSION,
  failingChecks,
  healthProblem,
  matchesBrokerFilter,
  parseBrokerFilter,
  type BrokerFilter,
  type BrokerHealthInfo,
} from '../../shared/broker-area-filters.js';
import { stateManager } from '../../client/state.js';
import { extractApiError } from '../../client/api.js';
import { loadAdminStatus } from '../../client/admin-status.js';
import { hasAnyPermission } from '../../lib/admin-permissions.js';
import { listPageStyles, brokerTypeBadgeStyles } from '../shared/resource-styles.js';
import { brokerAreaStyles } from '../shared/broker-area-styles.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import '../shared/view-toggle.js';
import { formatRelative } from '../../utils/time.js';
import { navigateTo, replaceSearch } from '../../client/navigation.js';
import { visibleInterval } from '../../client/visible-interval.js';

/** One runtime broker row of the health summary, as far as this page reads it. */
interface HealthSummaryBrokerRow {
  id: string;
  health?: BrokerHealthInfo | null;
}

/** The part of the health summary this page reads. */
interface HealthSummaryRows {
  runtime_brokers?: { items?: HealthSummaryBrokerRow[] };
}

/** The legacy target cell: "— (3 profiles)", or "—" with no profiles. */
export function legacyTargetText(broker: Pick<RuntimeBroker, 'profiles'>): string {
  const n = broker.profiles?.length ?? 0;
  if (n === 0) return '—';
  return `— (${n} profile${n === 1 ? '' : 's'})`;
}

@customElement('scion-page-brokers-area')
export class ScionPageBrokersArea extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @state()
  private loading = true;

  @state()
  private brokers: RuntimeBroker[] = [];

  @state()
  private error: string | null = null;

  @state()
  private viewMode: ViewMode = 'list';

  @state()
  private filter: BrokerFilter = 'all';

  /**
   * Self-reported health per broker ID, when the user may read the health
   * summary; null when the Health column is not shown.
   */
  @state()
  private health: Map<string, BrokerHealthInfo | null> | null = null;

  private boundOnBrokersUpdated = this.onBrokersUpdated.bind(this);
  private stopRelativeTimeTick: (() => void) | null = null;

  static override styles = [
    listPageStyles,
    brokerTypeBadgeStyles,
    brokerAreaStyles,
    css`
      .filter-row {
        display: flex;
        flex-wrap: wrap;
        align-items: center;
        gap: 0.75rem;
        margin-bottom: 1rem;
      }

      .list-stats {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
      }

      .not-enforced-marker {
        display: inline-flex;
        align-items: center;
        margin-left: 0.375rem;
        padding: 0.0625rem 0.375rem;
        border-radius: 9999px;
        font-size: 0.6875rem;
        font-weight: 600;
        background: var(--sl-color-warning-100, #fef3c7);
        color: var(--sl-color-warning-700, #a16207);
      }

      .health-warn {
        color: var(--sl-color-warning-700, #a16207);
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();

    const stored = localStorage.getItem('scion-view-brokers-area') as ViewMode | null;
    if (stored === 'grid' || stored === 'list') {
      this.viewMode = stored;
    }
    if (typeof window !== 'undefined') {
      this.filter = parseBrokerFilter(new URLSearchParams(window.location.search).get('filter'));
    }

    void this.loadBrokers();
    void this.loadHealth();

    stateManager.setScope({ type: 'brokers-list' });
    stateManager.addEventListener('brokers-updated', this.boundOnBrokersUpdated);

    this.stopRelativeTimeTick = visibleInterval(() => this.requestUpdate(), 15_000);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('brokers-updated', this.boundOnBrokersUpdated);
    this.stopRelativeTimeTick?.();
    this.stopRelativeTimeTick = null;
  }

  private onBrokersUpdated(): void {
    const brokerMap = new Map(this.brokers.map((b) => [b.id, b]));
    for (const broker of stateManager.getBrokers()) {
      brokerMap.set(broker.id, { ...brokerMap.get(broker.id), ...broker });
    }
    this.brokers = Array.from(brokerMap.values());
  }

  private async loadBrokers(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const response = await fetch('/api/v1/runtime-brokers', { credentials: 'include' });
      if (!response.ok) {
        throw new Error(
          await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`)
        );
      }
      const data = (await response.json()) as { brokers?: RuntimeBroker[] } | RuntimeBroker[];
      this.brokers = Array.isArray(data) ? data : data.brokers || [];
      stateManager.seedBrokers(this.brokers);
    } catch (err) {
      console.error('Failed to load brokers:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load brokers';
    } finally {
      this.loading = false;
    }
  }

  /**
   * Loads the self-reported health of every broker, only for a user who
   * holds hub.health.read: exactly the set the summary endpoint authorizes.
   * Anyone else sends no request and sees no Health column. A 401/403 also
   * hides the column.
   */
  private async loadHealth(): Promise<void> {
    const status = await loadAdminStatus(stateManager.getCurrentUserId());
    const canReadHealth = hasAnyPermission(status, [HEALTH_READ_PERMISSION]);
    if (!canReadHealth) return;
    try {
      const res = await fetch('/api/v1/admin/health/summary', { credentials: 'include' });
      // A 401/403, or any other failure, leaves the Health column hidden.
      if (!res.ok) return;
      const body = (await res.json()) as HealthSummaryRows;
      const map = new Map<string, BrokerHealthInfo | null>();
      for (const item of body.runtime_brokers?.items ?? []) {
        map.set(item.id, item.health ?? null);
      }
      this.health = map;
    } catch (err) {
      console.error('Failed to load broker health:', err);
    }
  }

  private setFilter(filter: BrokerFilter): void {
    this.filter = filter;
    const params = new URLSearchParams(window.location.search);
    if (filter === 'all') {
      params.delete('filter');
    } else {
      params.set('filter', filter);
    }
    replaceSearch(params);
  }

  private get visibleBrokers(): RuntimeBroker[] {
    return this.brokers.filter((b) => matchesBrokerFilter(b, this.filter, this.health?.get(b.id)));
  }

  private getStatusVariant(status: string): 'success' | 'warning' | 'danger' | 'neutral' {
    switch (status) {
      case 'online':
        return 'success';
      case 'degraded':
        return 'warning';
      default:
        return 'neutral';
    }
  }

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
  }

  private renderName(broker: RuntimeBroker): TemplateResult {
    return html`
      <span class="name-cell">
        <sl-icon name="hdd-rack"></sl-icon>
        <span>${broker.name}</span>
        ${broker.hostedByThisServer
          ? html`<sl-icon
              class="hosted-marker"
              name="house"
              label="Runs in this server's process"
              title="Runs in this server's process"
            ></sl-icon>`
          : nothing}
        ${this.renderBrokerTypeBadge(broker)}
      </span>
    `;
  }

  private renderBrokerTypeBadge(broker: RuntimeBroker) {
    const brokerType = broker.labels?.['scion.io/broker-type'];
    if (!brokerType) return nothing;
    const label = brokerType.charAt(0).toUpperCase() + brokerType.slice(1);
    const cssClass = brokerType === 'hosted' ? 'broker-type-badge hosted' : 'broker-type-badge';
    return html`<span class=${cssClass}>${label}</span>`;
  }

  private renderKind(broker: RuntimeBroker): TemplateResult {
    return isFlatBroker(broker)
      ? html`<span class="kind-badge flat">Flat</span>`
      : html`<span class="kind-badge">Legacy</span>`;
  }

  private targetText(broker: RuntimeBroker): string {
    return isFlatBroker(broker) ? runtimeTargetLabel(broker) : legacyTargetText(broker);
  }

  private renderHealth(broker: RuntimeBroker): TemplateResult {
    const h = this.health?.get(broker.id);
    if (!h) return html`<span class="meta-text">–</span>`;
    if (!healthProblem(h)) return html`<span class="health-ok">ok</span>`;
    const failing = failingChecks(h);
    const text = failing.length > 0 ? String(failing.length) : h.status || 'problem';
    return html`<span class="health-warn" title=${failing.join(', ') || (h.status ?? '')}
      ><sl-icon name="exclamation-triangle"></sl-icon> ${text}</span
    >`;
  }

  private renderAgentCapacity(broker: RuntimeBroker): TemplateResult {
    if (broker.agentCount == null) {
      return html`<span class="meta-text">—</span>`;
    }
    const capLabel = broker.agentLimit != null ? String(broker.agentLimit) : 'unlimited';
    return html`
      <span class="mono-cell" title="Source: ${broker.agentLimitSource || 'unknown'}"
        >${broker.agentCount} / ${capLabel}</span
      >
      ${broker.agentLimitSource === 'not_enforced'
        ? html`<span class="not-enforced-marker">not enforced</span>`
        : nothing}
    `;
  }

  override render() {
    return html`
      <div class="header">
        <h1>Brokers</h1>
        <div class="header-actions">
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-brokers-area"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
        </div>
      </div>
      ${this.loading
        ? html`<div class="loading-state">
            <sl-spinner></sl-spinner>
            <p>Loading brokers...</p>
          </div>`
        : this.error
          ? this.renderError()
          : this.renderBody()}
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Brokers</h2>
        <p>There was a problem connecting to the API.</p>
        <div class="error-details">${this.error}</div>
        <sl-button variant="primary" @click=${() => this.loadBrokers()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }

  private renderBody() {
    if (this.brokers.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="hdd-rack"></sl-icon>
          <h2>No Brokers Found</h2>
          <p>
            Runtime Brokers execute agents on compute nodes. No brokers are currently registered
            with the Hub.
          </p>
        </div>
      `;
    }
    const flatCount = this.brokers.filter((b) => isFlatBroker(b)).length;
    const rows = this.visibleBrokers;
    return html`
      <div class="filter-row">
        <sl-icon name="funnel" label="Filter"></sl-icon>
        <sl-radio-group
          size="small"
          value=${this.filter}
          @sl-change=${(e: Event) =>
            this.setFilter(parseBrokerFilter((e.target as HTMLInputElement).value))}
        >
          ${BROKER_FILTERS.map(
            (f) => html`<sl-radio-button value=${f.id}>${f.label}</sl-radio-button>`
          )}
        </sl-radio-group>
        <span class="list-stats">
          ${this.brokers.length} broker${this.brokers.length === 1 ? '' : 's'} ·
          <span class="flat-instances-stat">${flatCount}</span> flat
          instance${flatCount === 1 ? '' : 's'}
        </span>
      </div>
      ${rows.length === 0
        ? html`<p class="tab-note">No brokers match this filter.</p>`
        : this.viewMode === 'grid'
          ? this.renderGrid(rows)
          : this.renderTable(rows)}
    `;
  }

  private renderGrid(rows: RuntimeBroker[]) {
    return html`
      <div class="resource-grid">
        ${rows.map(
          (broker) => html`
            <a href="/brokers/${broker.id}" class="resource-card broker-row" data-id=${broker.id}>
              <h3 class="resource-name">${this.renderName(broker)}</h3>
              <div class="broker-meta">
                ${this.renderKind(broker)}
                <span class="target-cell">${this.targetText(broker)}</span>
                <scion-status-badge
                  status=${this.getStatusVariant(broker.status)}
                  label=${broker.status}
                  size="small"
                ></scion-status-badge>
                ${this.health ? this.renderHealth(broker) : nothing}
                ${this.renderAgentCapacity(broker)}
              </div>
            </a>
          `
        )}
      </div>
    `;
  }

  private renderTable(rows: RuntimeBroker[]) {
    return html`
      <div class="resource-table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Kind</th>
              <th>Runtime target</th>
              <th>Connection</th>
              ${this.health ? html`<th class="health-col">Health</th>` : nothing}
              <th class="hide-mobile">Agents / Cap</th>
              <th class="hide-mobile">Last Heartbeat</th>
            </tr>
          </thead>
          <tbody>
            ${rows.map(
              (broker) => html`
                <tr
                  class="clickable broker-row"
                  data-id=${broker.id}
                  @click=${() => navigateTo(`/brokers/${broker.id}`)}
                >
                  <td>${this.renderName(broker)}</td>
                  <td>${this.renderKind(broker)}</td>
                  <td class="target-cell">${this.targetText(broker)}</td>
                  <td>
                    <scion-status-badge
                      status=${this.getStatusVariant(broker.status)}
                      label=${broker.status}
                      size="small"
                    ></scion-status-badge>
                  </td>
                  ${this.health ? html`<td>${this.renderHealth(broker)}</td>` : nothing}
                  <td class="hide-mobile">${this.renderAgentCapacity(broker)}</td>
                  <td class="hide-mobile">
                    <span class="meta-text">${formatRelative(broker.lastHeartbeat)}</span>
                  </td>
                </tr>
              `
            )}
          </tbody>
        </table>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-brokers-area': ScionPageBrokersArea;
  }
}
