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
 * Diagnostics page component.
 *
 * Provides a system-wide admin view for unified log streaming from all
 * Scion components (hub, broker, agent, messages) via Cloud Logging.
 * Includes a status banner and the unified log viewer.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { apiFetch } from '../../client/api.js';
import '../shared/unified-log-viewer.js';

interface HealthResponse {
  status: string;
  version?: string;
  brokerCount?: number;
  agentCount?: number;
  [key: string]: unknown;
}

@customElement('scion-page-diagnostics')
export class ScionPageDiagnostics extends LitElement {
  @state() private hubHealth: HealthResponse | null = null;
  @state() private cloudLoggingAvailable = false;
  @state() private cloudLoggingChecked = false;
  @state() private gcpProjectId = '';
  @state() private currentSeverity = 'INFO';

  static override styles = css`
    :host {
      display: block;
      padding: 1.5rem;
    }

    .page-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 1.5rem;
    }

    .page-header h1 {
      font-size: 1.5rem;
      font-weight: 600;
      margin: 0;
      color: var(--scion-text, #1e293b);
    }

    .status-banner {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.75rem 1rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--sl-border-radius-medium, 0.5rem);
      margin-bottom: 1rem;
      font-size: 0.875rem;
      gap: 1rem;
      flex-wrap: wrap;
    }

    .status-left {
      display: flex;
      align-items: center;
      gap: 1rem;
      flex-wrap: wrap;
    }

    .status-item {
      display: flex;
      align-items: center;
      gap: 0.375rem;
      color: var(--scion-text-secondary, #475569);
    }

    .status-dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
    }

    .status-dot.healthy {
      background: var(--scion-success-500, #22c55e);
    }

    .status-dot.unhealthy {
      background: var(--scion-danger-500, #ef4444);
    }

    .status-dot.unknown {
      background: var(--scion-neutral-400, #94a3b8);
    }

    .status-dot.active {
      background: var(--scion-success-500, #22c55e);
    }

    .status-dot.inactive {
      background: var(--scion-neutral-400, #94a3b8);
    }

    .status-separator {
      color: var(--scion-border, #e2e8f0);
    }

    .status-info {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }

    .status-right {
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .health-link {
      font-size: 0.8125rem;
      color: var(--scion-primary-600, #2563eb);
      text-decoration: none;
      cursor: pointer;
    }
    .health-link:hover {
      text-decoration: underline;
    }

    /* Degradation UI */
    .degradation-panel {
      background: var(--scion-warning-50, #fffbeb);
      border: 1px solid var(--scion-warning-200, #fde68a);
      border-radius: var(--sl-border-radius-medium, 0.5rem);
      padding: 1.5rem 2rem;
      margin-bottom: 1rem;
    }

    .degradation-panel h3 {
      margin: 0 0 0.75rem;
      font-size: 1rem;
      color: var(--scion-warning-800, #92400e);
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .degradation-panel p {
      margin: 0 0 1rem;
      color: var(--scion-text-secondary, #475569);
      font-size: 0.875rem;
      line-height: 1.6;
    }

    .degradation-panel ol {
      margin: 0 0 1rem;
      padding-left: 1.5rem;
      color: var(--scion-text-secondary, #475569);
      font-size: 0.875rem;
      line-height: 1.8;
    }

    .degradation-panel code {
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.125rem 0.375rem;
      border-radius: 0.25rem;
      font-size: 0.8125rem;
      font-family: var(--scion-font-mono, monospace);
    }

    .degradation-note {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
      font-style: italic;
    }

    .popout-button {
      display: flex;
      align-items: center;
      gap: 0.375rem;
    }
  `;

  override async connectedCallback(): Promise<void> {
    super.connectedCallback();
    this.fetchHealth();
    this.checkCloudLogging();
  }

  private async fetchHealth(): Promise<void> {
    try {
      const res = await apiFetch('/healthz');
      if (res.ok) {
        this.hubHealth = (await res.json()) as HealthResponse;
      }
      // Non-OK or missing health is non-fatal — banner shows "Unknown" status
    } catch {
      // Health check failure is non-fatal — banner shows "Unknown" status
    }
  }

  private async checkCloudLogging(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/admin/diagnostics/logs?tail=1');
      if (res.ok) {
        const data = (await res.json()) as { gcpProjectId?: string };
        this.cloudLoggingAvailable = true;
        this.gcpProjectId = data.gcpProjectId || '';
      } else if (res.status === 501) {
        this.cloudLoggingAvailable = false;
      } else {
        // Other error, but Cloud Logging might still be available
        this.cloudLoggingAvailable = false;
      }
    } catch {
      this.cloudLoggingAvailable = false;
    } finally {
      this.cloudLoggingChecked = true;
    }
  }

  private buildCloudLoggingUrl(severity: string): string {
    if (!this.gcpProjectId) return '';
    const filterParts = [
      `logName != "projects/${this.gcpProjectId}/logs/scion_request_log"`,
    ];
    if (severity && severity !== 'DEFAULT') {
      filterParts.push(`severity >= ${severity}`);
    }
    const filter = filterParts.join('\n');
    const encoded = encodeURIComponent(filter);
    return `https://console.cloud.google.com/logs/query;query=${encoded}?project=${this.gcpProjectId}`;
  }

  private handleNavClick(path: string): void {
    this.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path },
        bubbles: true,
        composed: true,
      })
    );
  }

  override render() {
    return html`
      <div class="page-header">
        <h1>Diagnostics</h1>
        ${this.gcpProjectId
          ? html`
              <sl-button
                size="small"
                variant="default"
                class="popout-button"
                href=${this.buildCloudLoggingUrl(this.currentSeverity)}
                target="_blank"
              >
                <sl-icon slot="prefix" name="box-arrow-up-right"></sl-icon>
                Cloud Logging
              </sl-button>
            `
          : nothing}
      </div>
      ${this.renderStatusBanner()}
      ${this.cloudLoggingChecked
        ? this.cloudLoggingAvailable
          ? this.renderLogViewer()
          : this.renderDegradation()
        : html`
            <div style="text-align: center; padding: 3rem;">
              <sl-spinner style="font-size: 1.5rem;"></sl-spinner>
            </div>
          `}
    `;
  }

  private renderStatusBanner() {
    const health = this.hubHealth;
    const status = health?.status || 'unknown';
    const statusClass = status === 'ok' || status === 'healthy' ? 'healthy' : status === 'unknown' ? 'unknown' : 'unhealthy';
    const statusLabel = status === 'ok' || status === 'healthy' ? 'Healthy' : status === 'unknown' ? 'Unknown' : status;

    const cloudStatus = this.cloudLoggingChecked
      ? this.cloudLoggingAvailable
        ? 'active'
        : 'inactive'
      : 'unknown';
    const cloudLabel = this.cloudLoggingChecked
      ? this.cloudLoggingAvailable
        ? 'Active'
        : 'Unavailable'
      : 'Checking...';

    return html`
      <div class="status-banner">
        <div class="status-left">
          <span class="status-item">
            System Status:
            <span class="status-dot ${statusClass}"></span>
            ${statusLabel}
          </span>
          <span class="status-separator">|</span>
          <span class="status-item">
            Cloud Logging:
            <span class="status-dot ${cloudStatus}"></span>
            ${cloudLabel}
          </span>
          ${health
            ? html`
                <span class="status-info">
                  ${health.version ? `Hub ${health.version}` : ''}
                  ${health.brokerCount != null ? ` · ${health.brokerCount} brokers` : ''}
                  ${health.agentCount != null ? ` · ${health.agentCount} agents` : ''}
                </span>
              `
            : nothing}
        </div>
        <div class="status-right">
          <a class="health-link" @click=${() => this.handleNavClick('/health')}>
            View Health →
          </a>
        </div>
      </div>
    `;
  }

  private handleSeverityChange(e: Event): void {
    const detail = (e as CustomEvent<{ severity: string }>).detail;
    this.currentSeverity = detail.severity;
  }

  private renderLogViewer() {
    return html`
      <scion-unified-log-viewer
        .gcpProjectId=${this.gcpProjectId}
        initialSeverity="INFO"
        @severity-change=${this.handleSeverityChange}
      ></scion-unified-log-viewer>
    `;
  }

  private renderDegradation() {
    return html`
      <div class="degradation-panel">
        <h3>
          <sl-icon name="exclamation-triangle"></sl-icon>
          Cloud Logging is not configured
        </h3>
        <p>
          The diagnostics log viewer requires Google Cloud Logging to
          aggregate logs from hub, broker, and agent components.
        </p>
        <p>To enable:</p>
        <ol>
          <li>Set <code>SCION_CLOUD_LOGGING=true</code></li>
          <li>Configure <code>SCION_GCP_PROJECT_ID</code> or <code>GOOGLE_CLOUD_PROJECT</code></li>
          <li>Ensure the service account has <code>roles/logging.viewer</code></li>
        </ol>
        <p class="degradation-note">
          Individual agent logs are still available on each agent's
          detail page via the broker log relay.
        </p>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-diagnostics': ScionPageDiagnostics;
  }
}
