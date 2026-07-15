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

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

// ── Type definitions matching the Go API response ──

interface IntegrationStatus {
  connected: boolean;
  version?: string;
  channel_id?: string;
  capabilities?: string[];
  health?: string;
  message?: string;
  details?: Record<string, string>;
}

interface IntegrationSummary {
  name: string;
  platform: string;
  self_managed: boolean;
  deployment_mode?: string;
  has_secrets: Record<string, boolean>;
  status?: IntegrationStatus;
}

interface IntegrationDetail {
  name: string;
  platform: string;
  self_managed: boolean;
  deployment_mode?: string;
  settings: Record<string, string>;
  has_secrets: Record<string, boolean>;
  status?: IntegrationStatus;
}

interface AvailableIntegration {
  name: string;
  platform: string;
}

interface PlatformFieldDef {
  key: string;
  label: string;
  description: string;
  defaultValue: string;
}

function resolvePlatform(name: string): string {
  switch (name) {
    case 'telegram': return 'telegram';
    case 'discord': return 'discord';
    case 'slack': return 'slack';
    case 'chat-app': return 'gchat';
    default: return name;
  }
}

const PLATFORM_FIELDS: Record<string, PlatformFieldDef[]> = {
  telegram: [
    { key: 'inbound_mode', label: 'Inbound Mode', description: 'How Telegram delivers updates (poll or webhook)', defaultValue: 'poll' },
    { key: 'webhook_url', label: 'Webhook URL', description: 'Public URL for Telegram to send webhook updates to', defaultValue: '' },
    { key: 'webhook_listen', label: 'Webhook Listen', description: 'HTTP listen address for webhook mode', defaultValue: ':9094' },
    { key: 'db_path', label: 'Database Path', description: 'Path to SQLite database', defaultValue: 'telegram_v2.db' },
    { key: 'skip_set_webhook', label: 'Skip Webhook Registration', description: 'Set to true when running in HA mode to skip automatic webhook registration', defaultValue: 'false' },
    { key: 'agent_cache_ttl', label: 'Agent Cache TTL', description: 'How long to cache agent info', defaultValue: '5m' },
    { key: 'send_queue_size', label: 'Send Queue Size', description: 'Buffer size for outbound message queue (0 = unbuffered)', defaultValue: '0' },
    { key: 'send_min_delay', label: 'Send Min Delay', description: 'Minimum delay between outbound messages (e.g. 100ms)', defaultValue: '' },
    { key: 'chat_routes', label: 'Chat Routes', description: 'JSON map of Telegram chat IDs to topic patterns (v1 migration seeding only)', defaultValue: '' },
    { key: 'user_mappings', label: 'User Mappings', description: 'JSON map of Telegram usernames to scion user IDs (v1 migration seeding only)', defaultValue: '' },
  ],
  discord: [
    { key: 'application_id', label: 'Application ID', description: 'Discord application ID for slash commands', defaultValue: '' },
  ],
  slack: [
    { key: 'socket_mode', label: 'Socket Mode', description: 'Use Slack Socket Mode instead of HTTP webhooks (no public URL needed)', defaultValue: 'false' },
    { key: 'listen_address', label: 'Listen Address', description: 'HTTP listen address (HTTP mode only)', defaultValue: ':3000' },
    { key: 'db_path', label: 'Database Path', description: 'Path to SQLite database', defaultValue: '~/.scion/scion-slack.db' },
    { key: 'agent_cache_ttl', label: 'Agent Cache TTL', description: 'How long to cache agent info', defaultValue: '5m' },
  ],
};

@customElement('scion-page-admin-integrations')
export class ScionPageAdminIntegrations extends LitElement {
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private successMessage: string | null = null;

  // List view
  @state() private integrations: IntegrationSummary[] = [];

  // Detail view
  @state() private detail: IntegrationDetail | null = null;
  @state() private editedSettings: Record<string, string> = {};
  @state() private editedSecrets: Record<string, string> = {};
  @state() private saving = false;
  @state() private restarting = false;
  @state() private updating = false;

  // HA async update state
  @state() private updateState: string | null = null;
  @state() private updateDetail: string | null = null;
  @state() private updateNewVersion: string | null = null;
  private updatePollTimer: ReturnType<typeof setInterval> | null = null;

  // Available integrations for install
  @state() private availableIntegrations: AvailableIntegration[] = [];
  @state() private installingName: string | null = null;

  static override styles = css`
    :host {
      display: block;
    }

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 1.5rem;
    }

    .header sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0;
    }

    .header-description {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
      margin: 0 0 1.5rem 0;
    }

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section-title {
      font-size: 1rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 1rem 0;
      padding-bottom: 0.75rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .form-grid {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
    }

    @media (max-width: 768px) {
      .form-grid {
        grid-template-columns: 1fr;
      }
    }

    .form-field {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .form-field.full-width {
      grid-column: 1 / -1;
    }

    .form-field label {
      font-size: 0.8125rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-container {
      display: flex;
      justify-content: center;
      align-items: center;
      padding: 4rem;
    }

    .status-message {
      font-size: 0.875rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      margin-bottom: 1rem;
    }

    .status-message.success {
      background: var(--scion-success-bg, #dcfce7);
      color: var(--scion-success-text, #166534);
      border: 1px solid var(--scion-success-border, #86efac);
    }

    .status-message.error {
      background: var(--scion-error-bg, #fef2f2);
      color: var(--scion-error-text, #991b1b);
      border: 1px solid var(--scion-error-border, #fca5a5);
    }

    sl-input::part(base),
    sl-select::part(combobox) {
      font-size: 0.875rem;
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    sl-input::part(input) {
      color: var(--scion-text, #1e293b);
    }

    .actions {
      display: flex;
      align-items: center;
      gap: 1rem;
      padding: 1rem 0;
      border-top: 1px solid var(--scion-border, #e2e8f0);
      margin-top: 1rem;
    }

    .actions sl-button::part(base) {
      font-size: 0.875rem;
    }

    /* List view table */
    .integration-table {
      width: 100%;
      border-collapse: collapse;
    }

    .integration-table th {
      text-align: left;
      font-size: 0.75rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.025em;
      padding: 0.75rem 1rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .integration-table td {
      padding: 0.75rem 1rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .integration-table tr.clickable {
      cursor: pointer;
    }

    .integration-table tr.clickable:hover td {
      background: var(--scion-bg-subtle, #f8fafc);
    }

    .platform-name {
      text-transform: capitalize;
    }

    .empty-state {
      text-align: center;
      padding: 3rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty-state sl-icon {
      font-size: 2.5rem;
      margin-bottom: 1rem;
      display: block;
    }

    /* Detail view */
    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.25rem;
      font-size: 0.875rem;
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      cursor: pointer;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      text-decoration: underline;
    }

    .detail-name {
      font-size: 1.25rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .detail-platform {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      text-transform: capitalize;
      margin: 0 0 1.5rem 0;
    }

    .status-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 0.5rem;
      font-size: 0.875rem;
    }

    .status-label {
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      min-width: 6rem;
    }

    .capabilities-list {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
    }

    .secret-row {
      display: flex;
      align-items: center;
      gap: 1rem;
      padding: 0.75rem 0;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
    }

    .secret-row:last-child {
      border-bottom: none;
    }

    .secret-key {
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
      min-width: 10rem;
    }

    .secret-status {
      font-size: 0.8125rem;
    }

    .secret-input {
      flex: 1;
    }
  `;

  private get currentName(): string | null {
    const path = window.location.pathname;
    const match = path.match(/^\/admin\/integrations\/([^/]+)$/);
    return match ? decodeURIComponent(match[1]) : null;
  }

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadData();
  }

  private async loadData(): Promise<void> {
    const name = this.currentName;
    if (name) {
      await this.loadDetail(name);
    } else {
      await this.loadList();
    }
  }

  private async loadList(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const [listRes, availRes] = await Promise.all([
        apiFetch('/api/v1/admin/integrations'),
        apiFetch('/api/v1/admin/integrations/available'),
      ]);
      if (!listRes.ok) {
        this.error = await extractApiError(listRes, 'Failed to load integrations');
        return;
      }
      this.integrations = (await listRes.json()) as IntegrationSummary[];
      if (availRes.ok) {
        this.availableIntegrations = (await availRes.json()) as AvailableIntegration[];
      }
    } catch {
      this.error = 'Failed to connect to server';
    } finally {
      this.loading = false;
    }
  }

  private async loadDetail(name: string): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await apiFetch(`/api/v1/admin/integrations/${encodeURIComponent(name)}`);
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to load integration');
        return;
      }
      this.detail = (await res.json()) as IntegrationDetail;
      const knownFields = PLATFORM_FIELDS[this.detail.platform] ?? [];
      this.editedSettings = {
        ...Object.fromEntries(knownFields.map((f) => [f.key, f.defaultValue])),
        ...(this.detail.settings || {}),
      };
      this.editedSecrets = {};
    } catch {
      this.error = 'Failed to connect to server';
    } finally {
      this.loading = false;
    }
  }

  private async handleSaveConfig(): Promise<void> {
    if (!this.detail) return;
    this.saving = true;
    this.error = null;
    this.successMessage = null;
    try {
      const body: { settings: Record<string, string>; secrets?: Record<string, string> } = {
        settings: this.editedSettings,
      };
      const changedSecrets = Object.entries(this.editedSecrets).filter(([, v]) => v !== '');
      if (changedSecrets.length > 0) {
        body.secrets = Object.fromEntries(changedSecrets);
      }
      const res = await apiFetch(
        `/api/v1/admin/integrations/${encodeURIComponent(this.detail.name)}/config`,
        {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to save configuration');
        return;
      }
      this.successMessage = 'Configuration saved successfully';
      await this.loadDetail(this.detail.name);
    } catch {
      this.error = 'Failed to save configuration';
    } finally {
      this.saving = false;
    }
  }

  private async handleUpdate(): Promise<void> {
    if (!this.detail) return;

    if (this.detail.deployment_mode === 'ha') {
      await this.handleUpdateHA();
      return;
    }

    this.updating = true;
    this.error = null;
    this.successMessage = null;
    try {
      const res = await apiFetch(
        `/api/v1/admin/integrations/${encodeURIComponent(this.detail.name)}/update`,
        { method: 'POST' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to update integration');
        return;
      }
      this.successMessage = 'Integration updated successfully';
      await this.loadDetail(this.detail.name);
    } catch {
      this.error = 'Failed to update integration';
    } finally {
      this.updating = false;
    }
  }

  private async handleUpdateHA(): Promise<void> {
    if (!this.detail) return;
    this.updating = true;
    this.error = null;
    this.successMessage = null;
    this.updateState = null;
    this.updateDetail = null;
    this.updateNewVersion = null;
    this.stopUpdatePolling();

    try {
      const res = await apiFetch(
        `/api/v1/admin/integrations/${encodeURIComponent(this.detail.name)}/update`,
        { method: 'POST' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to request update');
        this.updating = false;
        return;
      }
      const data = (await res.json()) as { update_id: string };
      this.updateState = 'requested';
      this.startUpdatePolling(this.detail.name, data.update_id);
    } catch {
      this.error = 'Failed to request update';
      this.updating = false;
    }
  }

  private startUpdatePolling(integrationName: string, updateId: string): void {
    this.updatePollTimer = setInterval(() => {
      void this.pollUpdateStatus(integrationName, updateId);
    }, 3000);
  }

  private stopUpdatePolling(): void {
    if (this.updatePollTimer !== null) {
      clearInterval(this.updatePollTimer);
      this.updatePollTimer = null;
    }
  }

  private async pollUpdateStatus(integrationName: string, updateId: string): Promise<void> {
    try {
      const res = await apiFetch(
        `/api/v1/admin/integrations/${encodeURIComponent(integrationName)}/update/${encodeURIComponent(updateId)}`
      );
      if (!res.ok) return;

      const data = (await res.json()) as {
        state: string;
        detail?: string;
        new_version?: string;
      };
      this.updateState = data.state;
      this.updateDetail = data.detail ?? null;
      this.updateNewVersion = data.new_version ?? null;

      if (data.state === 'completed' || data.state === 'failed') {
        this.stopUpdatePolling();
        this.updating = false;
        if (data.state === 'completed') {
          this.successMessage = data.new_version
            ? `Update complete — version: ${data.new_version}`
            : 'Update complete';
          await this.loadDetail(integrationName);
        } else {
          this.error = data.detail
            ? `Update failed: ${data.detail}`
            : 'Update failed';
        }
      }
    } catch {
      // Polling errors are transient; keep polling.
    }
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.stopUpdatePolling();
  }

  private async handleInstall(name: string): Promise<void> {
    this.installingName = name;
    this.error = null;
    this.successMessage = null;
    try {
      const res = await apiFetch(
        `/api/v1/admin/integrations/${encodeURIComponent(name)}/install`,
        { method: 'POST' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to install integration');
        return;
      }
      this.successMessage = `Integration "${name}" installed successfully`;
      await this.loadList();
    } catch {
      this.error = 'Failed to install integration';
    } finally {
      this.installingName = null;
    }
  }

  private async handleRestart(): Promise<void> {
    if (!this.detail) return;
    this.restarting = true;
    this.error = null;
    this.successMessage = null;
    try {
      const res = await apiFetch(
        `/api/v1/admin/integrations/${encodeURIComponent(this.detail.name)}/restart`,
        { method: 'POST' }
      );
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to restart integration');
        return;
      }
      this.successMessage = 'Integration restarted successfully';
      await this.loadDetail(this.detail.name);
    } catch {
      this.error = 'Failed to restart integration';
    } finally {
      this.restarting = false;
    }
  }

  private navigateTo(path: string): void {
    this.dispatchEvent(new CustomEvent('nav-click', { detail: { path }, bubbles: true, composed: true }));
  }

  override render() {
    const isDetail = this.currentName !== null;

    return html`
      ${this.error ? html`<div class="status-message error">${this.error}</div>` : nothing}
      ${this.successMessage
        ? html`<div class="status-message success">${this.successMessage}</div>`
        : nothing}
      ${this.loading
        ? html`<div class="loading-container"><sl-spinner></sl-spinner></div>`
        : isDetail
          ? this.renderDetail()
          : this.renderList()}
    `;
  }

  // ── List View ──

  private renderList() {
    return html`
      <div class="header">
        <sl-icon name="plug"></sl-icon>
        <h1>Integrations</h1>
      </div>
      <p class="header-description">
        Manage chat integrations — Telegram, Discord, Google Chat, and Slack plugins connected to this
        hub.
      </p>

      ${this.integrations.length === 0
        ? html`
            <div class="section">
              <div class="empty-state">
                <sl-icon name="plug"></sl-icon>
                <p>No integrations configured.</p>
                <p style="font-size: 0.8125rem">
                  Chat integrations will appear here once a broker plugin is registered.
                </p>
              </div>
            </div>
          `
        : html`
            <div class="section" style="padding: 0;">
              <table class="integration-table">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Platform</th>
                    <th>Status</th>
                    <th>Mode</th>
                  </tr>
                </thead>
                <tbody>
                  ${this.integrations.map(
                    (i) => html`
                      <tr
                        class="clickable"
                        @click=${() => this.navigateTo(`/admin/integrations/${encodeURIComponent(i.name)}`)}
                      >
                        <td><strong>${i.name}</strong></td>
                        <td><span class="platform-name">${this.platformLabel(i.platform)}</span></td>
                        <td>${this.renderStatusBadge(i.status)}</td>
                        <td>${this.renderDeploymentModeBadge(i.deployment_mode)}</td>
                      </tr>
                    `
                  )}
                </tbody>
              </table>
            </div>
          `}

      ${this.availableIntegrations.length > 0
        ? html`
            <div class="header" style="margin-top: 2rem;">
              <sl-icon name="download"></sl-icon>
              <h1 style="font-size: 1.25rem;">Available Integrations</h1>
            </div>
            <p class="header-description">
              These integrations can be installed from source. After installing, configure
              secrets and restart to activate.
            </p>
            <div class="section" style="padding: 0;">
              <table class="integration-table">
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Platform</th>
                    <th>Action</th>
                  </tr>
                </thead>
                <tbody>
                  ${this.availableIntegrations.map(
                    (a) => html`
                      <tr>
                        <td><strong>${a.name}</strong></td>
                        <td><span class="platform-name">${this.platformLabel(a.platform)}</span></td>
                        <td>
                          <sl-button
                            size="small"
                            variant="primary"
                            ?loading=${this.installingName === a.name}
                            ?disabled=${this.installingName !== null}
                            @click=${() => { void this.handleInstall(a.name); }}
                          >
                            Install
                          </sl-button>
                        </td>
                      </tr>
                    `
                  )}
                </tbody>
              </table>
            </div>
          `
        : nothing}
    `;
  }

  // ── Detail View ──

  private renderDetail() {
    if (!this.detail) {
      return html`<div class="status-message error">Integration not found.</div>`;
    }
    const d = this.detail;
    return html`
      <a class="back-link" href="/admin/integrations">
        <sl-icon name="arrow-left"></sl-icon> Back to Integrations
      </a>

      <h2 class="detail-name">${d.name}</h2>
      <p class="detail-platform">${this.platformLabel(d.platform)} · ${this.deploymentModeLabel(d.deployment_mode)}</p>

      ${this.renderStatusSection(d.status)}
      ${this.renderConfigSection(d)}
      ${this.renderSecretsSection(d)}
      ${this.renderActionsSection()}
    `;
  }

  private renderStatusSection(status?: IntegrationStatus) {
    if (!status) {
      return html`
        <div class="section">
          <h3 class="section-title">Status</h3>
          <p style="color: var(--scion-text-muted); font-size: 0.875rem;">No status information available.</p>
        </div>
      `;
    }

    return html`
      <div class="section">
        <h3 class="section-title">Status</h3>
        <div class="status-row">
          <span class="status-label">Connection</span>
          ${this.renderStatusBadge(status)}
        </div>
        ${status.health
          ? html`
              <div class="status-row">
                <span class="status-label">Health</span>
                <sl-badge variant=${status.health === 'healthy' ? 'success' : status.health === 'unhealthy' ? 'danger' : 'neutral'}>
                  ${status.health}
                </sl-badge>
              </div>
            `
          : nothing}
        ${status.message && status.health === 'unhealthy'
          ? html`
              <sl-alert variant="danger" open style="margin-top: 0.75rem;">
                <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                ${status.message}
              </sl-alert>
            `
          : nothing}
        ${status.version
          ? html`
              <div class="status-row">
                <span class="status-label">Version</span>
                <span>${status.version}</span>
              </div>
            `
          : nothing}
        ${status.channel_id
          ? html`
              <div class="status-row">
                <span class="status-label">Channel ID</span>
                <span style="font-family: var(--sl-font-mono, monospace); font-size: 0.8125rem;">${status.channel_id}</span>
              </div>
            `
          : nothing}
        ${status.capabilities && status.capabilities.length > 0
          ? html`
              <div class="status-row">
                <span class="status-label">Capabilities</span>
                <div class="capabilities-list">
                  ${status.capabilities.map(
                    (c) => html`<sl-badge variant="neutral">${c}</sl-badge>`
                  )}
                </div>
              </div>
            `
          : nothing}
        ${status.details && Object.keys(status.details).length > 0
          ? html`
              <div style="margin-top: 0.75rem; padding-top: 0.75rem; border-top: 1px solid var(--scion-border, #e2e8f0);">
                ${Object.entries(status.details).map(
                  ([k, v]) => html`
                    <div class="status-row">
                      <span class="status-label">${k}</span>
                      <span style="font-size: 0.8125rem;">${v}</span>
                    </div>
                  `
                )}
              </div>
            `
          : nothing}
      </div>
    `;
  }

  private renderConfigSection(d: IntegrationDetail) {
    const platform = resolvePlatform(d.name);
    const fieldDefs = PLATFORM_FIELDS[platform] || [];
    const definedKeys = new Set(fieldDefs.map((f) => f.key));
    const extraKeys = Object.keys(d.settings || {}).filter((k) => !definedKeys.has(k));
    const hasFields = fieldDefs.length > 0 || extraKeys.length > 0;

    if (!hasFields && Object.keys(d.settings || {}).length === 0) {
      return html`
        <div class="section">
          <h3 class="section-title">Configuration</h3>
          <p style="color: var(--scion-text-muted); font-size: 0.875rem;">No configurable settings for this integration.</p>
        </div>
      `;
    }

    return html`
      <div class="section">
        <h3 class="section-title">Configuration</h3>
        <div class="form-grid">
          ${fieldDefs.map(
            (field) => html`
              <div class="form-field">
                <label>${field.label}</label>
                <sl-input
                  .value=${this.editedSettings[field.key] ?? field.defaultValue}
                  placeholder=${field.defaultValue}
                  @sl-change=${(e: Event) => {
                    this.editedSettings = {
                      ...this.editedSettings,
                      [field.key]: (e.target as HTMLInputElement).value,
                    };
                  }}
                ></sl-input>
                <span class="hint">${field.description}</span>
              </div>
            `
          )}
          ${extraKeys.map(
            (key) => html`
              <div class="form-field">
                <label>${key}</label>
                <sl-input
                  .value=${this.editedSettings[key] ?? ''}
                  @sl-change=${(e: Event) => {
                    this.editedSettings = {
                      ...this.editedSettings,
                      [key]: (e.target as HTMLInputElement).value,
                    };
                  }}
                ></sl-input>
              </div>
            `
          )}
        </div>
      </div>
    `;
  }

  private renderSecretsSection(d: IntegrationDetail) {
    const secretKeys = Object.keys(d.has_secrets || {});
    if (secretKeys.length === 0) return nothing;

    return html`
      <div class="section">
        <h3 class="section-title">Secrets</h3>
        ${secretKeys.map(
          (key) => html`
            <div class="secret-row">
              <span class="secret-key">${key}</span>
              <span class="secret-status">
                ${d.has_secrets[key]
                  ? html`<sl-badge variant="success">Set</sl-badge>`
                  : html`<sl-badge variant="warning">Not set</sl-badge>`}
              </span>
              <sl-input
                class="secret-input"
                type="password"
                placeholder=${d.has_secrets[key] ? 'Enter new value to update' : 'Enter value'}
                .value=${this.editedSecrets[key] ?? ''}
                @sl-change=${(e: Event) => {
                  this.editedSecrets = {
                    ...this.editedSecrets,
                    [key]: (e.target as HTMLInputElement).value,
                  };
                }}
              ></sl-input>
            </div>
          `
        )}
      </div>
    `;
  }

  private renderActionsSection() {
    const mode = this.detail?.deployment_mode ?? 'plugin';
    const showUpdate = this.detail && mode !== 'external';
    return html`
      <div class="actions">
        <sl-button
          variant="primary"
          ?loading=${this.saving}
          @click=${() => { void this.handleSaveConfig(); }}
        >
          Save Configuration
        </sl-button>
        <sl-button
          variant="default"
          ?loading=${this.restarting}
          @click=${() => { void this.handleRestart(); }}
        >
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Restart
        </sl-button>
        ${showUpdate
          ? html`
              <sl-button
                variant="default"
                ?loading=${this.updating}
                @click=${() => { void this.handleUpdate(); }}
              >
                <sl-icon slot="prefix" name="arrow-repeat"></sl-icon>
                Update
              </sl-button>
            `
          : nothing}
      </div>
      ${this.updateState ? this.renderUpdateProgress() : nothing}
    `;
  }

  private renderUpdateProgress() {
    let message = '';
    let variant: 'primary' | 'success' | 'danger' = 'primary';

    switch (this.updateState) {
      case 'requested':
        message = 'Update requested...';
        break;
      case 'acknowledged':
        message = 'Integration acknowledged, preparing update...';
        break;
      case 'updating':
        message = 'Updating...';
        break;
      case 'completed':
        message = this.updateNewVersion
          ? `Update complete — version: ${this.updateNewVersion}`
          : 'Update complete';
        variant = 'success';
        break;
      case 'failed':
        message = this.updateDetail
          ? `Update failed: ${this.updateDetail}`
          : 'Update failed';
        variant = 'danger';
        break;
    }

    const showSpinner = this.updateState !== 'completed' && this.updateState !== 'failed';

    return html`
      <sl-alert variant=${variant} open style="margin-top: 0.75rem;">
        ${showSpinner
          ? html`<sl-spinner slot="icon" style="font-size: 1rem;"></sl-spinner>`
          : html`<sl-icon slot="icon" name=${variant === 'success' ? 'check-circle' : 'exclamation-triangle'}></sl-icon>`}
        ${message}
      </sl-alert>
    `;
  }

  // ── Helpers ──

  private renderStatusBadge(status?: IntegrationStatus) {
    if (!status) {
      return html`<sl-badge variant="neutral">Unknown</sl-badge>`;
    }
    return status.connected
      ? html`<sl-badge variant="success">Connected</sl-badge>`
      : html`<sl-badge variant="danger">Disconnected</sl-badge>`;
  }

  private renderDeploymentModeBadge(mode?: string) {
    switch (mode) {
      case 'ha':
        return html`<sl-badge variant="primary">HA</sl-badge>`;
      case 'external':
        return html`<sl-badge variant="neutral">External</sl-badge>`;
      default:
        return html`<sl-badge variant="success">Plugin</sl-badge>`;
    }
  }

  private deploymentModeLabel(mode?: string): string {
    switch (mode) {
      case 'ha':
        return 'HA';
      case 'external':
        return 'External';
      default:
        return 'Plugin';
    }
  }

  private platformLabel(platform: string): string {
    switch (platform) {
      case 'telegram':
        return 'Telegram';
      case 'discord':
        return 'Discord';
      case 'slack':
        return 'Slack';
      case 'gchat':
        return 'Google Chat';
      default:
        return platform;
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-integrations': ScionPageAdminIntegrations;
  }
}
