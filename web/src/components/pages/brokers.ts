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
 * Brokers list page component
 *
 * Displays all runtime brokers with their status, version, and capabilities
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, RuntimeBroker } from '../../shared/types.js';
import { stateManager } from '../../client/state.js';
import { extractApiError } from '../../client/api.js';
import { listPageStyles, brokerTypeBadgeStyles } from '../shared/resource-styles.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import '../shared/view-toggle.js';

@customElement('scion-page-brokers')
export class ScionPageBrokers extends LitElement {
  /**
   * Page data from SSR
   */
  @property({ type: Object })
  pageData: PageData | null = null;

  /**
   * Loading state
   */
  @state()
  private loading = true;

  /**
   * Brokers list
   */
  @state()
  private brokers: RuntimeBroker[] = [];

  /**
   * Error message if loading failed
   */
  @state()
  private error: string | null = null;

  /**
   * Current view mode (grid or list)
   */
  @state()
  private viewMode: ViewMode = 'grid';

  private boundOnBrokersUpdated = this.onBrokersUpdated.bind(this);
  private relativeTimeInterval: ReturnType<typeof setInterval> | null = null;

  static override styles = [
    listPageStyles,
    brokerTypeBadgeStyles,
    css`
      .broker-header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        margin-bottom: 1rem;
      }

      .broker-version {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        margin-top: 0.25rem;
        font-family: var(--scion-font-mono, monospace);
      }

      .broker-details {
        display: flex;
        flex-wrap: wrap;
        gap: 0.5rem;
        margin-top: 1rem;
        padding-top: 1rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      .capability-tag {
        display: inline-flex;
        align-items: center;
        gap: 0.25rem;
        padding: 0.25rem 0.5rem;
        border-radius: var(--scion-radius, 0.5rem);
        font-size: 0.75rem;
        font-weight: 500;
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text-muted, #64748b);
      }

      .capability-tag.enabled {
        background: var(--sl-color-success-100, #dcfce7);
        color: var(--sl-color-success-700, #15803d);
      }

      .broker-meta {
        display: flex;
        gap: 1.5rem;
        margin-top: 1rem;
        padding-top: 1rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
      }

      /* Table-specific inline capability tags */
      .capability-tags-inline {
        display: flex;
        flex-wrap: wrap;
        gap: 0.25rem;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();

    // Read persisted view mode
    const stored = localStorage.getItem('scion-view-brokers') as ViewMode | null;
    if (stored === 'grid' || stored === 'list') {
      this.viewMode = stored;
    }

    void this.loadBrokers();

    // Subscribe to broker SSE events
    stateManager.setScope({ type: 'brokers-list' });
    stateManager.addEventListener('brokers-updated', this.boundOnBrokersUpdated as EventListener);

    // Periodically re-render to keep relative timestamps fresh
    this.relativeTimeInterval = setInterval(() => this.requestUpdate(), 15_000);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener(
      'brokers-updated',
      this.boundOnBrokersUpdated as EventListener
    );
    if (this.relativeTimeInterval) {
      clearInterval(this.relativeTimeInterval);
      this.relativeTimeInterval = null;
    }
  }

  private onBrokersUpdated(): void {
    const updatedBrokers = stateManager.getBrokers();
    const brokerMap = new Map(this.brokers.map((b) => [b.id, b]));

    for (const broker of updatedBrokers) {
      brokerMap.set(broker.id, { ...brokerMap.get(broker.id), ...broker });
    }

    this.brokers = Array.from(brokerMap.values());
  }

  private async loadBrokers(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await fetch('/api/v1/runtime-brokers', {
        credentials: 'include',
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      const data = (await response.json()) as { brokers?: RuntimeBroker[] } | RuntimeBroker[];
      this.brokers = Array.isArray(data) ? data : data.brokers || [];

      // Seed stateManager so SSE delta merging has full baseline data
      stateManager.seedBrokers(this.brokers);
    } catch (err) {
      console.error('Failed to load brokers:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load brokers';
    } finally {
      this.loading = false;
    }
  }

  private getStatusVariant(status: string): 'success' | 'warning' | 'danger' | 'neutral' {
    switch (status) {
      case 'online':
        return 'success';
      case 'degraded':
        return 'warning';
      case 'offline':
        return 'neutral';
      default:
        return 'neutral';
    }
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
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

  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    this.viewMode = e.detail.view;
  }

  override render() {
    return html`
      <div class="header">
        <h1>Brokers</h1>
        <div class="header-actions">
          <scion-view-toggle
            .view=${this.viewMode}
            storageKey="scion-view-brokers"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
        </div>
      </div>

      ${this.loading
        ? this.renderLoading()
        : this.error
          ? this.renderError()
          : this.renderBrokers()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading brokers...</p>
      </div>
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

  private renderBrokers() {
    if (this.brokers.length === 0) {
      return this.renderEmptyState();
    }

    return this.viewMode === 'grid' ? this.renderGrid() : this.renderTable();
  }

  private renderEmptyState() {
    return html`
      <div class="empty-state">
        <sl-icon name="hdd-rack"></sl-icon>
        <h2>No Brokers Found</h2>
        <p>
          Runtime Brokers execute agents on compute nodes. No brokers are currently registered with
          the Hub.
        </p>
      </div>
    `;
  }

  private renderGrid() {
    return html`
      <div class="resource-grid">${this.brokers.map((broker) => this.renderBrokerCard(broker))}</div>
    `;
  }

  private renderBrokerTypeBadge(broker: RuntimeBroker) {
    const brokerType = broker.labels?.['scion.io/broker-type'];
    if (!brokerType) return '';
    const label = brokerType.charAt(0).toUpperCase() + brokerType.slice(1);
    const cssClass = brokerType === 'hosted' ? 'broker-type-badge hosted' : 'broker-type-badge';
    return html`<span class=${cssClass}>${label}</span>`;
  }

  private renderBrokerCard(broker: RuntimeBroker) {
    return html`
      <a href="/brokers/${broker.id}" class="resource-card">
        <div class="broker-header">
          <div>
            <h3 class="resource-name">
              <sl-icon name="hdd-rack"></sl-icon>
              ${broker.name}
              ${this.renderBrokerTypeBadge(broker)}
            </h3>
            ${broker.version ? html`<div class="broker-version">v${broker.version}</div>` : ''}
          </div>
          <scion-status-badge
            status=${this.getStatusVariant(broker.status)}
            label=${broker.status}
            size="small"
          >
          </scion-status-badge>
        </div>
        ${broker.capabilities ? this.renderCapabilities(broker.capabilities) : ''}
        <div class="broker-meta">
          <div class="stat">
            <span class="stat-label">Last Heartbeat</span>
            <span class="stat-value">${this.formatRelativeTime(broker.lastHeartbeat)}</span>
          </div>
          ${broker.profiles
            ? html`
                <div class="stat">
                  <span class="stat-label">Profiles</span>
                  <span class="stat-value">${broker.profiles.length}</span>
                </div>
              `
            : ''}
          ${broker.createdBy
            ? html`
                <div class="stat">
                  <span class="stat-label">Created By</span>
                  <span class="stat-value">${broker.createdByName || broker.createdBy}</span>
                </div>
              `
            : ''}
        </div>
      </a>
    `;
  }

  private renderCapabilities(capabilities: import('../../shared/types.js').BrokerCapabilities) {
    return html`
      <div class="broker-details">
        <span class="capability-tag ${capabilities.webPTY ? 'enabled' : ''}">WebPTY</span>
        <span class="capability-tag ${capabilities.sync ? 'enabled' : ''}">Sync</span>
        <span class="capability-tag ${capabilities.attach ? 'enabled' : ''}">Attach</span>
      </div>
    `;
  }

  private renderTable() {
    return html`
      <div class="resource-table-container">
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th class="hide-mobile">Version</th>
              <th>Status</th>
              <th class="hide-mobile">Capabilities</th>
              <th>Last Heartbeat</th>
              <th class="hide-mobile">Profiles</th>
            </tr>
          </thead>
          <tbody>
            ${this.brokers.map((broker) => this.renderBrokerRow(broker))}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderBrokerRow(broker: RuntimeBroker) {
    return html`
      <tr class="clickable" @click=${() => {
        window.history.pushState({}, '', `/brokers/${broker.id}`);
        window.dispatchEvent(new PopStateEvent('popstate'));
      }}>
        <td>
          <span class="name-cell">
            <sl-icon name="hdd-rack"></sl-icon>
            ${broker.name}
            ${this.renderBrokerTypeBadge(broker)}
          </span>
        </td>
        <td class="hide-mobile">
          ${broker.version ? html`<span class="mono-cell">v${broker.version}</span>` : '\u2014'}
        </td>
        <td>
          <scion-status-badge
            status=${this.getStatusVariant(broker.status)}
            label=${broker.status}
            size="small"
          ></scion-status-badge>
        </td>
        <td class="hide-mobile">
          ${broker.capabilities
            ? html`
                <span class="capability-tags-inline">
                  <span class="capability-tag ${broker.capabilities.webPTY ? 'enabled' : ''}">WebPTY</span>
                  <span class="capability-tag ${broker.capabilities.sync ? 'enabled' : ''}">Sync</span>
                  <span class="capability-tag ${broker.capabilities.attach ? 'enabled' : ''}">Attach</span>
                </span>
              `
            : '\u2014'}
        </td>
        <td>
          <span class="meta-text">${this.formatRelativeTime(broker.lastHeartbeat)}</span>
        </td>
        <td class="hide-mobile">
          ${broker.profiles ? broker.profiles.length : '\u2014'}
        </td>
      </tr>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-brokers': ScionPageBrokers;
  }
}
