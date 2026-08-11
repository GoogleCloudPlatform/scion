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
  placeholder?: string;
  type?: 'text' | 'select' | 'toggle';  // default 'text'
  options?: { value: string; label: string }[];  // for 'select' type
}

interface PlatformSecretDef {
  key: string;
  label: string;
  description: string;
  required?: boolean;
}

/** Shape of a project entry in the projects_json flat config value. */
interface A2AProjectEntry {
  slug: string;
  default_template: string;
  auto_provision: boolean;
  exposed_agents: string[];
}

/** Lightweight project info from GET /api/v1/projects. */
interface ProjectInfo {
  id: string;
  name: string;
  slug?: string;
}

const PLATFORM_SECRETS: Record<string, PlatformSecretDef[]> = {
  telegram: [
    { key: 'bot_token', label: 'Bot Token', description: 'Telegram bot token from @BotFather', required: true },
    { key: 'webhook_secret', label: 'Webhook Secret', description: 'Secret for webhook verification (webhook mode only)' },
  ],
  discord: [
    { key: 'bot_token', label: 'Bot Token', description: 'Discord bot token', required: true },
  ],
  slack: [
    { key: 'bot_token', label: 'Bot Token', description: 'Slack bot token (xoxb-...)', required: true },
    { key: 'app_token', label: 'App Token', description: 'Slack app-level token for Socket Mode (xapp-...)' },
    { key: 'signing_secret', label: 'Signing Secret', description: 'Slack signing secret for HTTP mode' },
  ],
  a2a: [
    { key: 'api_key', label: 'API Key', description: 'Static API key for apiKey/bearer auth schemes' },
  ],
  gchat: [
    { key: 'signing_key', label: 'Hub Signing Key', description: 'Shared signing key for hub authentication (HS256 JWT)', required: true },
  ],
  teams: [
    { key: 'app_secret', label: 'App Secret', description: 'Azure App Registration client secret' },
  ],
};

function resolvePlatform(name: string): string {
  switch (name) {
    case 'telegram': return 'telegram';
    case 'discord': return 'discord';
    case 'slack': return 'slack';
    case 'chat-app': return 'gchat';
    case 'a2a-bridge': return 'a2a';
    case 'teams': return 'teams';
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
    { key: 'guild_ids', label: 'Allowed Guild IDs', description: 'Comma-separated Discord server IDs. Leave empty to register commands globally across all servers the bot joins.', defaultValue: '', placeholder: 'Global — all servers' },
  ],
  slack: [
    { key: 'socket_mode', label: 'Socket Mode', description: 'Use Slack Socket Mode instead of HTTP webhooks (no public URL needed)', defaultValue: 'false' },
    { key: 'listen_address', label: 'Listen Address', description: 'HTTP listen address (HTTP mode only)', defaultValue: ':3000' },
    { key: 'db_path', label: 'Database Path', description: 'Path to SQLite database', defaultValue: '~/.scion/scion-slack.db' },
    { key: 'agent_cache_ttl', label: 'Agent Cache TTL', description: 'How long to cache agent info', defaultValue: '5m' },
  ],
  a2a: [
    { key: 'external_url', label: 'External URL',
      description: 'Public URL where the bridge serves agent cards and JSON-RPC',
      defaultValue: '', placeholder: 'https://a2a.example.com' },
    { key: 'auth_scheme', label: 'Auth Scheme',
      description: 'Authentication method for A2A clients',
      defaultValue: 'none',
      type: 'select',
      options: [
        { value: 'none', label: 'None' },
        { value: 'apiKey', label: 'API Key' },
        { value: 'bearer', label: 'Bearer Token' },
        { value: 'hubUAT', label: 'Hub UAT' },
        { value: 'hubJWT', label: 'Hub JWT' },
      ] },
    { key: 'rate_limit_enabled', label: 'Rate Limiting',
      description: 'Enable request rate limiting',
      defaultValue: 'false', type: 'toggle' },
    { key: 'rate_limit_rps', label: 'Rate Limit (req/s)',
      description: 'Maximum requests per second',
      defaultValue: '10', placeholder: '10' },
    { key: 'rate_limit_burst', label: 'Rate Limit Burst',
      description: 'Maximum burst size for rate limiter',
      defaultValue: '20', placeholder: '20' },
    { key: 'send_message_timeout', label: 'Send Message Timeout',
      description: 'Timeout for sending messages to agents',
      defaultValue: '120s', placeholder: '120s' },
    { key: 'sse_keepalive', label: 'SSE Keepalive Interval',
      description: 'Interval for SSE keepalive pings',
      defaultValue: '30s', placeholder: '30s' },
    { key: 'push_retry_max', label: 'Push Notification Retries',
      description: 'Maximum retries for push notification delivery',
      defaultValue: '3', placeholder: '3' },
    { key: 'provider_org', label: 'Provider Organization',
      description: 'Organization name for agent card metadata',
      defaultValue: '', placeholder: 'My Organization' },
    { key: 'provider_url', label: 'Provider URL',
      description: 'Organization URL for agent card metadata',
      defaultValue: '', placeholder: 'https://example.com' },
    { key: 'uat_cache_ttl', label: 'UAT Cache TTL',
      description: 'How long to cache UAT validation results',
      defaultValue: '60s', placeholder: '60s' },
  ],
  gchat: [
    { key: 'project_id', label: 'GCP Project ID', description: 'Google Cloud project ID hosting the Chat app', defaultValue: '' },
    { key: 'credentials', label: 'Credentials Path', description: 'Path to service account key JSON file (leave empty for ADC)', defaultValue: '' },
    { key: 'listen_address', label: 'Webhook Listen Address', description: 'HTTP listen address for webhook mode', defaultValue: ':8443' },
    { key: 'external_url', label: 'External URL', description: 'Public URL where Google Chat sends events', defaultValue: '' },
    { key: 'service_account_email', label: 'Service Account Email', description: 'Email of the GCP service account (for JWT verification)', defaultValue: '' },
  ],
  teams: [
    { key: 'app_id', label: 'Application (Client) ID', description: 'Azure App Registration Client ID', defaultValue: '', placeholder: 'e.g. 12345678-abcd-1234-efgh-123456789012' },
    { key: 'tenant_id', label: 'Tenant ID', description: 'Azure AD Tenant ID (GUID)', defaultValue: '', placeholder: 'e.g. 12345678-abcd-1234-efgh-123456789012' },
    { key: 'listen_address', label: 'Listen Address', description: 'HTTP listen address for the Teams webhook endpoint', defaultValue: ':3978', placeholder: ':3978' },
    { key: 'db_path', label: 'Database Path', description: 'Path to SQLite database', defaultValue: '~/.scion/scion-teams.db', placeholder: '~/.scion/scion-teams.db' },
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

  // Available projects for A2A project selector
  @state() private availableProjects: ProjectInfo[] = [];


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

    .required-tag {
      display: inline-block;
      font-size: 0.6875rem;
      font-weight: 600;
      color: var(--scion-error-text, #991b1b);
      background: var(--scion-error-bg, #fef2f2);
      border: 1px solid var(--scion-error-border, #fca5a5);
      border-radius: 0.25rem;
      padding: 0.0625rem 0.375rem;
      margin-left: 0.375rem;
      text-transform: uppercase;
      letter-spacing: 0.025em;
      vertical-align: middle;
    }

    .setup-banner {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
      padding: 1rem 1.25rem;
      margin-bottom: 1.5rem;
      background: var(--scion-warning-bg, #fffbeb);
      border: 1px solid var(--scion-warning-border, #fcd34d);
      border-radius: var(--scion-radius-lg, 0.75rem);
      font-size: 0.875rem;
      color: var(--scion-warning-text, #92400e);
    }

    .setup-banner sl-icon {
      font-size: 1.25rem;
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .setup-banner strong {
      display: block;
      margin-bottom: 0.25rem;
    }

    .secret-description {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .invite-link-container {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      flex-wrap: wrap;
    }

    .invite-link-info {
      display: flex;
      align-items: flex-start;
      gap: 0.75rem;
    }

    .invite-link-info sl-icon {
      font-size: 1.25rem;
      color: var(--scion-primary, #3b82f6);
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .invite-link-description {
      margin: 0;
      font-size: 0.875rem;
      font-weight: 500;
      color: var(--scion-text, #1e293b);
    }

    .invite-link-permissions {
      margin: 0.25rem 0 0;
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    /* ── A2A Projects Editor ── */

    .project-card {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 1rem;
      margin-bottom: 0.75rem;
      background: var(--scion-bg-subtle, #f8fafc);
    }

    .project-card-header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 0.75rem;
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text-muted, #64748b);
    }

    .project-card .form-grid {
      gap: 0.75rem;
    }

    .projects-empty {
      padding: 1.5rem;
      text-align: center;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
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

      // For A2A integrations, fetch the project list for the projects editor.
      if (resolvePlatform(name) === 'a2a') {
        void this.loadAvailableProjects();
      }
    } catch {
      this.error = 'Failed to connect to server';
    } finally {
      this.loading = false;
    }
  }

  /** Fetch the hub project list for the A2A project selector. */
  private async loadAvailableProjects(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/projects');
      if (res.ok) {
        const body = (await res.json()) as { projects: ProjectInfo[] };
        this.availableProjects = body.projects ?? [];
      }
    } catch {
      // Non-fatal: the editor still works, but the dropdown will be empty.
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
      ${this.renderSetupBanner(d)}
      ${this.renderSelfManagedSetupSection(d)}
      ${this.renderSecretsSection(d)}
      ${this.renderConfigSection(d)}
      ${this.renderA2AProjectsSection(d)}
      ${this.renderDiscordInviteLink(d)}
      ${this.renderTeamsSetupSection(d)}
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
    // Filter out keys that have dedicated editors (e.g. projects_json for A2A).
    const hiddenExtraKeys = new Set<string>();
    if (platform === 'a2a') {
      hiddenExtraKeys.add('projects_json');
    }
    const extraKeys = Object.keys(d.settings || {}).filter(
      (k) => !definedKeys.has(k) && !hiddenExtraKeys.has(k),
    );
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
                ${this.renderFieldInput(field)}
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

  private renderFieldInput(field: PlatformFieldDef) {
    const fieldType = field.type ?? 'text';

    if (fieldType === 'select' && field.options) {
      const currentValue = this.editedSettings[field.key] ?? field.defaultValue;
      return html`
        <sl-select
          .value=${currentValue}
          @sl-change=${(e: Event) => {
            this.editedSettings = {
              ...this.editedSettings,
              [field.key]: (e.target as HTMLSelectElement).value,
            };
          }}
        >
          ${field.options.map(
            (opt) => html`<sl-option value=${opt.value}>${opt.label}</sl-option>`
          )}
        </sl-select>
      `;
    }

    if (fieldType === 'toggle') {
      const currentValue = this.editedSettings[field.key] ?? field.defaultValue;
      const isChecked = currentValue === 'true';
      return html`
        <sl-switch
          ?checked=${isChecked}
          @sl-change=${(e: Event) => {
            this.editedSettings = {
              ...this.editedSettings,
              [field.key]: (e.target as HTMLInputElement).checked ? 'true' : 'false',
            };
          }}
        ></sl-switch>
      `;
    }

    // Default: text input
    return html`
      <sl-input
        .value=${this.editedSettings[field.key] ?? field.defaultValue}
        placeholder=${field.placeholder ?? field.defaultValue}
        @sl-change=${(e: Event) => {
          this.editedSettings = {
            ...this.editedSettings,
            [field.key]: (e.target as HTMLInputElement).value,
          };
        }}
      ></sl-input>
    `;
  }

  // ── A2A Projects & Agent Exposure Editor ──

  /** Parse the projects_json flat config value into an array of project entries. */
  private parseProjectsJSON(): { projects: A2AProjectEntry[]; warning: string | null } {
    const raw = this.editedSettings['projects_json'] ?? '';
    if (!raw || raw === '[]') {
      return { projects: [], warning: null };
    }
    try {
      const parsed = JSON.parse(raw) as A2AProjectEntry[];
      if (!Array.isArray(parsed)) {
        return { projects: [], warning: null };
      }
      return { projects: parsed, warning: null };
    } catch (e) {
      console.warn('Could not parse projects_json configuration:', e);
      return { projects: [], warning: 'Warning: Could not parse existing projects configuration.' };
    }
  }

  /** Serialize the project entries back to the projects_json flat config value. */
  private serializeProjectsJSON(projects: A2AProjectEntry[]): void {
    this.editedSettings = {
      ...this.editedSettings,
      projects_json: JSON.stringify(projects),
    };
  }

  private renderA2AProjectsSection(d: IntegrationDetail) {
    const platform = resolvePlatform(d.name);
    if (platform !== 'a2a') return nothing;

    const { projects, warning } = this.parseProjectsJSON();

    // Slugs already used — for duplicate-prevention in the dropdown.
    const usedSlugs = new Set(projects.map((p) => p.slug));

    return html`
      <div class="section">
        <h3 class="section-title">Projects & Agent Exposure</h3>
        ${warning
          ? html`<div class="warning-message" style="color: var(--sl-color-warning-600); margin-bottom: 0.5rem;">
              <sl-icon name="exclamation-triangle" style="margin-right: 0.25rem;"></sl-icon>
              ${warning}
            </div>`
          : nothing}
        ${projects.length === 0
          ? html`
              <div class="projects-empty">
                <p>No projects configured. Add a project to expose its agents via A2A.</p>
              </div>
            `
          : projects.map((proj, idx) =>
              this.renderProjectCard(proj, idx, projects, usedSlugs),
            )}
        <sl-button
          variant="default"
          size="small"
          style="margin-top: 0.5rem;"
          @click=${() => this.handleAddProject(projects)}
        >
          <sl-icon slot="prefix" name="plus-lg"></sl-icon>
          Add Project
        </sl-button>
      </div>
    `;
  }

  private renderProjectCard(
    proj: A2AProjectEntry,
    idx: number,
    allProjects: A2AProjectEntry[],
    usedSlugs: Set<string>,
  ) {
    // Build project display name for the header.
    const matchedProject = this.availableProjects.find(
      (p) => (p.slug ?? p.id) === proj.slug,
    );
    const headerLabel = matchedProject
      ? `${matchedProject.name} (${proj.slug})`
      : proj.slug || 'New Project';

    return html`
      <div class="project-card">
        <div class="project-card-header">
          <span>Project ${idx + 1}: ${headerLabel}</span>
          <sl-button
            variant="danger"
            size="small"
            outline
            @click=${() => this.handleRemoveProject(allProjects, idx)}
          >
            Remove
          </sl-button>
        </div>
        <div class="form-grid">
          <div class="form-field">
            <label>Project</label>
            ${this.availableProjects.length > 0
              ? html`
                  <sl-select
                    .value=${proj.slug}
                    placeholder="Select a project"
                    @sl-change=${(e: Event) =>
                      this.handleProjectFieldChange(
                        allProjects,
                        idx,
                        'slug',
                        (e.target as HTMLSelectElement).value,
                      )}
                  >
                    ${this.availableProjects.map((p) => {
                      const slug = p.slug ?? p.id;
                      const disabled = usedSlugs.has(slug) && slug !== proj.slug;
                      return html`
                        <sl-option value=${slug} ?disabled=${disabled}>
                          ${p.name} (${slug})
                        </sl-option>
                      `;
                    })}
                  </sl-select>
                `
              : html`
                  <sl-input
                    .value=${proj.slug}
                    placeholder="project-slug"
                    @sl-change=${(e: Event) =>
                      this.handleProjectFieldChange(
                        allProjects,
                        idx,
                        'slug',
                        (e.target as HTMLInputElement).value,
                      )}
                  ></sl-input>
                `}
            <span class="hint">Project to expose via A2A</span>
          </div>

          <div class="form-field">
            <label>Default Template</label>
            <sl-input
              .value=${proj.default_template}
              placeholder="default"
              @sl-change=${(e: Event) =>
                this.handleProjectFieldChange(
                  allProjects,
                  idx,
                  'default_template',
                  (e.target as HTMLInputElement).value,
                )}
            ></sl-input>
            <span class="hint">Agent template for auto-provisioned agents</span>
          </div>

          <div class="form-field">
            <label>Auto Provision</label>
            <sl-switch
              ?checked=${proj.auto_provision}
              @sl-change=${(e: Event) =>
                this.handleProjectFieldChange(
                  allProjects,
                  idx,
                  'auto_provision',
                  (e.target as HTMLInputElement).checked,
                )}
            ></sl-switch>
            <span class="hint">Automatically create agents from A2A requests</span>
          </div>

          <div class="form-field">
            <label>Exposed Agents</label>
            <sl-input
              .value=${(proj.exposed_agents ?? []).join(', ')}
              placeholder="Leave empty to expose all agents"
              @sl-change=${(e: Event) =>
                this.handleProjectFieldChange(
                  allProjects,
                  idx,
                  'exposed_agents',
                  (e.target as HTMLInputElement).value,
                )}
            ></sl-input>
            <span class="hint">Comma-separated agent names. Leave empty to expose all agents.</span>
          </div>
        </div>
      </div>
    `;
  }

  private handleAddProject(currentProjects: A2AProjectEntry[]): void {
    const updated = [
      ...currentProjects,
      {
        slug: '',
        default_template: 'default',
        auto_provision: false,
        exposed_agents: [] as string[],
      },
    ];
    this.serializeProjectsJSON(updated);
  }

  private handleRemoveProject(
    currentProjects: A2AProjectEntry[],
    idx: number,
  ): void {
    const updated = currentProjects.filter((_, i) => i !== idx);
    this.serializeProjectsJSON(updated);
  }

  private handleProjectFieldChange(
    currentProjects: A2AProjectEntry[],
    idx: number,
    field: keyof A2AProjectEntry,
    value: string | boolean,
  ): void {
    const updated = currentProjects.map((p, i) => {
      if (i !== idx) return { ...p };
      const copy = { ...p };
      switch (field) {
        case 'slug':
          copy.slug = value as string;
          break;
        case 'default_template':
          copy.default_template = value as string;
          break;
        case 'auto_provision':
          copy.auto_provision = value as boolean;
          break;
        case 'exposed_agents': {
          const raw = (value as string).trim();
          copy.exposed_agents = raw
            ? raw.split(',').map((s) => s.trim()).filter((s) => s !== '')
            : [];
          break;
        }
      }
      return copy;
    });
    this.serializeProjectsJSON(updated);
  }

  private renderDiscordInviteLink(d: IntegrationDetail) {
    const platform = resolvePlatform(d.name);
    if (platform !== 'discord') return nothing;

    const appId = (this.editedSettings['application_id'] ?? '').trim();
    if (!appId) return nothing;

    const inviteUrl = `https://discord.com/api/oauth2/authorize?client_id=${encodeURIComponent(appId)}&permissions=329101954112&scope=bot%20applications.commands`;

    return html`
      <div class="section">
        <h3 class="section-title">Bot Setup</h3>
        <div class="invite-link-container">
          <div class="invite-link-info">
            <sl-icon name="robot"></sl-icon>
            <div>
              <p class="invite-link-description">Add the bot to your Discord server</p>
              <p class="invite-link-permissions">
                Grants required permissions: View Channels, Send Messages, Send Messages in Threads, Create Public Threads, Manage Threads, Read Message History, Embed Links, Add Reactions, Manage Webhooks
              </p>
            </div>
          </div>
          <sl-button
            variant="primary"
            size="small"
            href=${inviteUrl}
            target="_blank"
            rel="noopener noreferrer"
          >
            <sl-icon slot="prefix" name="box-arrow-up-right"></sl-icon>
            Invite Bot to Server
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderTeamsSetupSection(d: IntegrationDetail) {
    const platform = resolvePlatform(d.name);
    if (platform !== 'teams') return nothing;

    const appId = (d.settings['app_id'] ?? '').trim();
    const tenantId = (d.settings['tenant_id'] ?? '').trim();
    if (!appId || !tenantId) return nothing;

    const downloadUrl = '/api/v1/admin/integrations/teams/manifest';
    const messagingEndpoint = `${window.location.origin}/api/messages`;

    return html`
      <div class="section">
        <h3 class="section-title">Teams App Package</h3>
        <div class="invite-link-container">
          <div class="invite-link-info">
            <sl-icon name="download"></sl-icon>
            <div>
              <p class="invite-link-description">Download the Teams app package (.zip)</p>
              <p class="invite-link-permissions">
                Upload it to your Microsoft Teams Admin Center or sideload it in Teams to enable the bot integration.
              </p>
            </div>
          </div>
          <sl-button
            variant="primary"
            size="small"
            href=${downloadUrl}
            download="teams-app.zip"
          >
            <sl-icon slot="prefix" name="download"></sl-icon>
            Download App Package
          </sl-button>
        </div>
        <div class="invite-link-container" style="margin-top: 1rem;">
          <div class="invite-link-info">
            <sl-icon name="link-45deg"></sl-icon>
            <div>
              <p class="invite-link-description">Messaging Endpoint</p>
              <p class="invite-link-permissions">
                Set this URL in your Azure Bot resource → Configuration page.
              </p>
            </div>
          </div>
        </div>
        <div style="display: flex; align-items: center; gap: 0.5rem; margin-top: 0.5rem;">
          <sl-input
            readonly
            value=${messagingEndpoint}
            style="flex: 1;"
          ></sl-input>
          <sl-button
            size="small"
            @click=${() => {
              void navigator.clipboard.writeText(messagingEndpoint);
            }}
          >
            <sl-icon slot="prefix" name="clipboard"></sl-icon>
            Copy
          </sl-button>
        </div>
      </div>
    `;
  }

  private renderSelfManagedSetupSection(d: IntegrationDetail) {
    const platform = resolvePlatform(d.name);

    // Determine binary/config/label per self-managed platform.
    let binaryName: string;
    let configFile: string;
    let label: string;
    switch (platform) {
      case 'a2a':
        binaryName = 'scion-a2a-bridge';
        configFile = '~/.scion/scion-a2a-bridge.yaml';
        label = 'A2A bridge';
        break;
      case 'gchat':
        binaryName = 'scion-chat-app';
        configFile = '~/.scion/scion-chat-app.yaml';
        label = 'Google Chat app';
        break;
      default:
        return nothing;
    }

    // Only show the setup section when the process is not connected.
    if (d.status?.connected) return nothing;

    const startCommand = `${binaryName} -config ${configFile}`;

    return html`
      <div class="section">
        <h3 class="section-title">Service Setup</h3>
        <div class="setup-banner">
          <sl-icon name="info-circle"></sl-icon>
          <div>
            <strong>The ${label} process is not connected</strong>
            Start the process and click Reconnect to activate.
          </div>
        </div>
        <div style="font-size: 0.875rem; display: flex; flex-direction: column; gap: 0.5rem;">
          <div class="status-row">
            <span class="status-label">Binary</span>
            <span style="font-family: var(--sl-font-mono, monospace); font-size: 0.8125rem;">${binaryName}</span>
          </div>
          <div class="status-row">
            <span class="status-label">Config File</span>
            <span style="font-family: var(--sl-font-mono, monospace); font-size: 0.8125rem;">${configFile}</span>
          </div>
          <div class="status-row">
            <span class="status-label">Start Command</span>
            <span style="font-family: var(--sl-font-mono, monospace); font-size: 0.8125rem;">${startCommand}</span>
          </div>
        </div>
      </div>
    `;
  }

  private renderSetupBanner(d: IntegrationDetail) {
    const platform = resolvePlatform(d.name);
    const secretDefs = PLATFORM_SECRETS[platform] ?? [];
    const missingRequired = secretDefs.filter(
      (s) => s.required && !d.has_secrets?.[s.key]
    );
    if (missingRequired.length === 0) return nothing;

    const platformName = this.platformLabel(d.platform);
    const fieldNames = missingRequired.map((s) => s.label).join(', ');
    return html`
      <div class="setup-banner">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <div>
          <strong>${platformName} requires setup to operate</strong>
          Enter your ${fieldNames} below to get started.
        </div>
      </div>
    `;
  }

  private renderSecretsSection(d: IntegrationDetail) {
    const platform = resolvePlatform(d.name);
    const secretDefs = PLATFORM_SECRETS[platform] ?? [];
    const secretKeys = Object.keys(d.has_secrets || {});
    if (secretDefs.length === 0 && secretKeys.length === 0) return nothing;

    const secretDefMap = new Map(secretDefs.map((s) => [s.key, s]));
    const sortedKeys = [
      ...secretDefs.map((s) => s.key),
      ...secretKeys.filter((k) => !secretDefMap.has(k)),
    ];

    // For A2A, the api_key secret is only relevant when auth_scheme is apiKey or bearer.
    const authScheme = platform === 'a2a' ? (this.editedSettings['auth_scheme'] ?? 'none') : '';
    const apiKeyRelevant = authScheme === 'apiKey' || authScheme === 'bearer';

    return html`
      <div class="section">
        <h3 class="section-title">Secrets</h3>
        ${sortedKeys.map((key) => {
          // Conditionally hide api_key for A2A when scheme doesn't need it.
          if (platform === 'a2a' && key === 'api_key' && !apiKeyRelevant) return nothing;
          const def = secretDefMap.get(key);
          const label = def?.label ?? key;
          // For A2A, mark api_key as required when the scheme needs it.
          const isRequired = platform === 'a2a' && key === 'api_key' ? apiKeyRelevant : (def?.required ?? false);
          const isConfigured = d.has_secrets?.[key];
          return html`
            <div class="secret-row">
              <div style="min-width: 10rem;">
                <span class="secret-key">
                  ${label}${isRequired ? html`<span class="required-tag">Required</span>` : nothing}
                </span>
                ${def?.description ? html`<div class="secret-description">${def.description}</div>` : nothing}
              </div>
              <span class="secret-status">
                ${isConfigured
                  ? html`<sl-badge variant="success">Configured</sl-badge>`
                  : html`<sl-badge variant="danger">Not configured</sl-badge>`}
              </span>
              <sl-input
                class="secret-input"
                type="password"
                password-toggle
                placeholder=${isConfigured ? 'Enter new value to update' : `Enter ${label.toLowerCase()}`}
                .value=${this.editedSecrets[key] ?? ''}
                @sl-change=${(e: Event) => {
                  this.editedSecrets = {
                    ...this.editedSecrets,
                    [key]: (e.target as HTMLInputElement).value,
                  };
                }}
              ></sl-input>
            </div>
          `;
        })}
      </div>
    `;
  }

  private renderActionsSection() {
    const mode = this.detail?.deployment_mode ?? 'plugin';
    const showUpdate = this.detail && mode !== 'external';
    const restartLabel = mode === 'external' ? 'Reconnect' : 'Restart';
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
          ${restartLabel}
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
      case 'a2a':
        return 'A2A Bridge';
      case 'teams':
        return 'Microsoft Teams';
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
