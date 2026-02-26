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
import '../shared/status-badge.js';

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

  private boundOnBrokersUpdated = this.onBrokersUpdated.bind(this);
  private relativeTimeInterval: ReturnType<typeof setInterval> | null = null;

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

    .broker-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
      gap: 1.5rem;
    }

    .broker-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      transition: all var(--scion-transition-fast, 150ms ease);
    }

    .broker-card:hover {
      border-color: var(--scion-primary, #3b82f6);
      box-shadow: var(--scion-shadow-md, 0 4px 6px -1px rgba(0, 0, 0, 0.1));
      transform: translateY(-2px);
    }

    .broker-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      margin-bottom: 1rem;
    }

    .broker-name {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0;
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .broker-name sl-icon {
      color: var(--scion-primary, #3b82f6);
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

    .stat {
      display: flex;
      flex-direction: column;
    }

    .stat-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.05em;
    }

    .stat-value {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
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
  `;

  override connectedCallback(): void {
    super.connectedCallback();
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
        const errorData = (await response.json().catch(() => ({}))) as { message?: string };
        throw new Error(errorData.message || `HTTP ${response.status}: ${response.statusText}`);
      }

      const data = (await response.json()) as { brokers?: RuntimeBroker[] } | RuntimeBroker[];
      this.brokers = Array.isArray(data) ? data : data.brokers || [];
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

  override render() {
    return html`
      <div class="header">
        <h1>Brokers</h1>
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

    return html`
      <div class="broker-grid">${this.brokers.map((broker) => this.renderBrokerCard(broker))}</div>
    `;
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

  private renderBrokerCard(broker: RuntimeBroker) {
    return html`
      <div class="broker-card">
        <div class="broker-header">
          <div>
            <h3 class="broker-name">
              <sl-icon name="hdd-rack"></sl-icon>
              ${broker.name}
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
        </div>
      </div>
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
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-brokers': ScionPageBrokers;
  }
}
