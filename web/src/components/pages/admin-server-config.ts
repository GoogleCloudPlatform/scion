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
 * Admin server configuration page.
 *
 * Full settings editor for ~/.scion/settings.yaml. Organized into
 * tabs matching the top-level sections of the settings schema.
 * Saves changes back to disk and reloads applicable runtime settings.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';

// ── Type definitions matching the Go API response ──

interface V1CORSConfig {
  enabled?: boolean;
  allowed_origins?: string[];
  allowed_methods?: string[];
  allowed_headers?: string[];
  max_age?: number;
}

interface V1ServerHubConfig {
  port?: number;
  host?: string;
  public_url?: string;
  read_timeout?: string;
  write_timeout?: string;
  cors?: V1CORSConfig;
  admin_emails?: string[];
  soft_delete_retention?: string;
  soft_delete_retain_files?: boolean;
}

interface V1BrokerConfig {
  enabled?: boolean;
  port?: number;
  host?: string;
  read_timeout?: string;
  write_timeout?: string;
  hub_endpoint?: string;
  container_hub_endpoint?: string;
  broker_id?: string;
  broker_name?: string;
  broker_nickname?: string;
  broker_token?: string;
  auto_provide?: boolean;
  cors?: V1CORSConfig;
}

interface V1DatabaseConfig {
  driver?: string;
  url?: string;
}

interface V1AuthConfig {
  dev_mode?: boolean;
  dev_token?: string;
  dev_token_file?: string;
  authorized_domains?: string[];
  user_access_mode?: string;
}

interface V1OAuthProviderConfig {
  client_id?: string;
  client_secret?: string;
}

interface V1OAuthClientConfig {
  google?: V1OAuthProviderConfig;
  github?: V1OAuthProviderConfig;
}

interface V1OAuthConfig {
  web?: V1OAuthClientConfig;
  cli?: V1OAuthClientConfig;
  device?: V1OAuthClientConfig;
}

interface V1StorageConfig {
  provider?: string;
  bucket?: string;
  local_path?: string;
}

interface V1SecretsConfig {
  backend?: string;
  gcp_project_id?: string;
  gcp_credentials?: string;
}

interface V1NotificationChannelConfig {
  type: string;
  params?: Record<string, string>;
  filter_types?: string[];
  filter_urgent_only?: boolean;
}

interface V1MessageBrokerConfig {
  enabled?: boolean;
  type?: string;
}

interface V1GitHubAppConfig {
  app_id?: number;
  api_base_url?: string;
  webhooks_enabled?: boolean;
  installation_url?: string;
}

interface V1ServerConfig {
  mode?: string;
  log_level?: string;
  log_format?: string;
  hub?: V1ServerHubConfig;
  broker?: V1BrokerConfig;
  database?: V1DatabaseConfig;
  auth?: V1AuthConfig;
  oauth?: V1OAuthConfig;
  storage?: V1StorageConfig;
  secrets?: V1SecretsConfig;
  notification_channels?: V1NotificationChannelConfig[];
  message_broker?: V1MessageBrokerConfig;
  github_app?: V1GitHubAppConfig;
}

interface V1TelemetryCloudConfig {
  enabled?: boolean;
  endpoint?: string;
  protocol?: string;
  headers?: Record<string, string>;
  provider?: string;
}

interface V1TelemetryHubConfig {
  enabled?: boolean;
  report_interval?: string;
}

interface V1TelemetryLocalConfig {
  enabled?: boolean;
  file?: string;
  console?: boolean;
}

interface V1TelemetryConfig {
  enabled?: boolean;
  cloud?: V1TelemetryCloudConfig;
  hub?: V1TelemetryHubConfig;
  local?: V1TelemetryLocalConfig;
}

interface V1RuntimeConfig {
  type?: string;
  host?: string;
  context?: string;
  namespace?: string;
  sync?: string;
}

interface ResourceSpec {
  requests?: { cpu?: string; memory?: string };
  limits?: { cpu?: string; memory?: string };
  disk?: string;
}

interface ServerConfigResponse {
  scion_version?: string;
  scion_commit?: string;
  scion_build_time?: string;
  schema_version: string;
  active_profile?: string;
  default_template?: string;
  default_harness_config?: string;
  image_registry?: string;
  workspace_path?: string;
  server?: V1ServerConfig;
  telemetry?: V1TelemetryConfig;
  runtimes?: Record<string, V1RuntimeConfig>;
  harness_configs?: Record<string, unknown>;
  profiles?: Record<string, unknown>;
  default_max_turns?: number;
  default_max_model_calls?: number;
  default_max_duration?: string;
  default_resources?: ResourceSpec;
}

interface ReloadResult {
  applied?: string[];
  requires_restart?: string[];
  error?: string;
}

interface UpdateCommitInfo {
  hash: string;
  subject: string;
}

interface UpdateCheckResult {
  update_available: boolean;
  current_commit: string;
  latest_commit: string;
  current_branch: string;
  tracking_ref: string;
  commits_behind: number;
  new_commits?: UpdateCommitInfo[];
}

interface GitHubInstallationInfo {
  installation_id: number;
  account_login: string;
  account_type: string;
  repositories: string[];
  status: string;
}

interface RateLimitInfo {
  limit: number;
  remaining: number;
  reset: string;
  used: number;
}

interface GitHubAppConfigData {
  app_id: number;
  api_base_url?: string;
  webhooks_enabled: boolean;
  configured: boolean;
  has_private_key: boolean;
  has_webhook_secret: boolean;
  installation_url?: string;
  rate_limit?: RateLimitInfo;
}

@customElement('scion-page-admin-server-config')
export class ScionPageAdminServerConfig extends LitElement {
  @state() private loading = true;
  @state() private saving = false;
  @state() private error: string | null = null;
  @state() private successMessage: string | null = null;
  @state() private activeTab = 'general';
  @state() private reloadResult: ReloadResult | null = null;

  // ── Read-only server build info ──
  @state() private scionVersion = '';
  @state() private scionCommit = '';
  @state() private scionBuildTime = '';

  // ── Update check state ──
  @state() private updateCheckLoading = false;
  @state() private updateCheckError: string | null = null;
  @state() private updateCheckResult: UpdateCheckResult | null = null;
  @state() private updateRunning = false;
  @state() private showUpdateConfirm = false;

  // ── Form state (mirrors settings.yaml) ──

  // General
  @state() private activeProfile = '';
  @state() private defaultTemplate = '';
  @state() private defaultHarnessConfig = '';
  @state() private imageRegistry = '';
  @state() private workspacePath = '';

  // Default agent limits
  @state() private defaultMaxTurns = 0;
  @state() private defaultMaxModelCalls = 0;
  @state() private defaultMaxDuration = '';
  @state() private defaultResCpuReq = '';
  @state() private defaultResMemReq = '';
  @state() private defaultResCpuLim = '';
  @state() private defaultResMemLim = '';
  @state() private defaultResDisk = '';

  // Server
  @state() private serverMode = '';
  @state() private logLevel = '';
  @state() private logFormat = '';

  // Hub Server
  @state() private hubPort = 0;
  @state() private hubHost = '';
  @state() private hubPublicUrl = '';
  @state() private hubReadTimeout = '';
  @state() private hubWriteTimeout = '';
  @state() private hubAdminEmails = '';
  @state() private hubSoftDeleteRetention = '';
  @state() private hubSoftDeleteRetainFiles = false;

  // Runtime Broker
  @state() private brokerEnabled = false;
  @state() private brokerPort = 0;
  @state() private brokerHost = '';
  @state() private brokerHubEndpoint = '';
  @state() private brokerContainerHubEndpoint = '';
  @state() private brokerName = '';
  @state() private brokerNickname = '';
  @state() private brokerAutoProvide = false;

  // Database
  @state() private dbDriver = '';
  @state() private dbUrl = '';

  // Auth
  @state() private authDevMode = false;
  @state() private authDevToken = '';
  @state() private authAuthorizedDomains = '';
  @state() private authUserAccessMode = 'open';

  // Storage
  @state() private storageProvider = '';
  @state() private storageBucket = '';
  @state() private storageLocalPath = '';

  // Secrets
  @state() private secretsBackend = '';
  @state() private secretsGCPProjectId = '';

  // Telemetry
  @state() private telemetryEnabled = false;
  @state() private telemetryCloudEnabled = false;
  @state() private telemetryCloudEndpoint = '';
  @state() private telemetryCloudProtocol = '';
  @state() private telemetryCloudProvider = '';
  @state() private telemetryHubEnabled = false;
  @state() private telemetryHubReportInterval = '';
  @state() private telemetryLocalEnabled = false;
  @state() private telemetryLocalFile = '';
  @state() private telemetryLocalConsole = false;

  // Message Broker
  @state() private messageBrokerEnabled = false;
  @state() private messageBrokerType = '';

  // GitHub App
  @state() private githubAppConfigured = false;
  @state() private githubAppId = 0;
  @state() private githubAppApiBaseUrl = '';
  @state() private githubAppWebhooksEnabled = false;
  @state() private githubAppHasPrivateKey = false;
  @state() private githubAppHasWebhookSecret = false;
  @state() private githubAppPrivateKey = '';
  @state() private githubAppWebhookSecret = '';
  @state() private githubAppInstallationUrl = '';
  @state() private githubAppSaving = false;
  @state() private githubAppError: string | null = null;
  @state() private githubAppSuccess: string | null = null;
  @state() private githubAppInstallations: GitHubInstallationInfo[] = [];
  @state() private githubAppInstallationsLoading = false;
  @state() private githubAppSyncLoading = false;
  @state() private githubAppSyncResult: string | null = null;
  @state() private githubAppDiscoverLoading = false;
  @state() private githubAppRateLimit: RateLimitInfo | null = null;

  // GCP Identity Quota
  @state() private gcpQuotaLoading = false;
  @state() private gcpQuotaData: {
    minting_configured: boolean;
    gcp_project_id?: string;
    global_minted: number;
    global_cap: number;
    per_project_cap: number;
    projects?: { project_id: string; project_name: string; minted: number }[];
  } | null = null;

  // Keep raw data for sections we don't fully edit
  private rawConfig: ServerConfigResponse | null = null;

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

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(base) {
      background: transparent;
    }

    sl-tab::part(base) {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      padding: 0.75rem 1rem;
    }

    sl-tab::part(base):hover {
      color: var(--scion-text, #1e293b);
    }

    sl-tab[active]::part(base) {
      color: var(--scion-primary, #3b82f6);
    }

    sl-tab-panel::part(base) {
      padding: 1.5rem 0;
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

    .version-info {
      display: flex;
      flex-wrap: wrap;
      gap: 1.5rem;
    }

    .version-item {
      display: flex;
      flex-direction: column;
      gap: 0.125rem;
    }

    .version-label {
      font-size: 0.75rem;
      font-weight: 500;
      color: var(--scion-text-muted, #64748b);
      text-transform: uppercase;
      letter-spacing: 0.025em;
    }

    .version-value {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .version-value code {
      font-family: var(--sl-font-mono, monospace);
      font-size: 0.8125rem;
      background: var(--scion-bg, #f8fafc);
      padding: 0.125rem 0.375rem;
      border-radius: 0.25rem;
      border: 1px solid var(--scion-border, #e2e8f0);
    }

    .version-actions {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-left: auto;
      align-self: flex-start;
    }

    .update-banner {
      margin-top: 0.75rem;
      padding: 0.75rem 1rem;
      border-radius: 0.375rem;
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
      background: var(--sl-color-primary-50, #eff6ff);
    }

    .update-banner-header {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--sl-color-primary-700, #1d4ed8);
    }

    .update-banner-header sl-icon {
      font-size: 1rem;
    }

    .update-commits {
      margin-top: 0.5rem;
      max-height: 10rem;
      overflow-y: auto;
      font-size: 0.8125rem;
      font-family: var(--sl-font-mono, monospace);
      line-height: 1.5;
      color: var(--scion-text, #1e293b);
    }

    .update-commits .commit-hash {
      color: var(--sl-color-primary-600, #2563eb);
      margin-right: 0.5rem;
    }

    .update-banner-actions {
      margin-top: 0.75rem;
      display: flex;
      gap: 0.5rem;
    }

    .update-current {
      margin-top: 0.5rem;
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
    }

    .update-error {
      margin-top: 0.5rem;
      font-size: 0.8125rem;
      color: var(--sl-color-danger-700, #b91c1c);
    }

    sl-input::part(base),
    sl-select::part(combobox),
    sl-textarea::part(base) {
      font-size: 0.875rem;
      border-color: var(--scion-border, #e2e8f0);
      background: var(--scion-surface, #ffffff);
    }

    sl-input::part(input),
    sl-textarea::part(textarea) {
      color: var(--scion-text, #1e293b);
    }

    sl-switch {
      --sl-color-primary-600: var(--scion-primary, #3b82f6);
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

    .reload-info {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.5rem;
    }

    .reload-info .applied {
      color: var(--scion-success-text, #166534);
    }

    .reload-info .restart {
      color: var(--scion-warning-text, #92400e);
    }

    .loading-container {
      display: flex;
      justify-content: center;
      align-items: center;
      padding: 4rem;
    }

    .masked-value {
      color: var(--scion-text-muted, #64748b);
      font-style: italic;
      font-size: 0.8125rem;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    void this.loadConfig();
    void this.loadGitHubAppInstallations();
  }

  private async loadConfig(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      const res = await apiFetch('/api/v1/admin/server-config');
      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to load server configuration');
        return;
      }
      const data = (await res.json()) as ServerConfigResponse;
      this.rawConfig = data;
      this.populateForm(data);
      // Load GitHub App config before releasing the loading gate so values
      // are present when the form first renders (avoids Shoelace timing issues).
      await this.loadGitHubAppConfig();
    } catch (e) {
      this.error = 'Failed to connect to server';
    } finally {
      this.loading = false;
    }
  }

  private populateForm(data: ServerConfigResponse): void {
    // Server build info
    this.scionVersion = data.scion_version || '';
    this.scionCommit = data.scion_commit || '';
    this.scionBuildTime = data.scion_build_time || '';

    // General
    this.activeProfile = data.active_profile || '';
    this.defaultTemplate = data.default_template || '';
    this.defaultHarnessConfig = data.default_harness_config || '';
    this.imageRegistry = data.image_registry || '';
    this.workspacePath = data.workspace_path || '';

    // Default agent limits
    this.defaultMaxTurns = data.default_max_turns || 0;
    this.defaultMaxModelCalls = data.default_max_model_calls || 0;
    this.defaultMaxDuration = data.default_max_duration || '';
    const defRes = data.default_resources;
    this.defaultResCpuReq = defRes?.requests?.cpu || '';
    this.defaultResMemReq = defRes?.requests?.memory || '';
    this.defaultResCpuLim = defRes?.limits?.cpu || '';
    this.defaultResMemLim = defRes?.limits?.memory || '';
    this.defaultResDisk = defRes?.disk || '';

    // Server
    const srv = data.server;
    if (srv) {
      this.serverMode = srv.mode || '';
      this.logLevel = srv.log_level || '';
      this.logFormat = srv.log_format || '';

      // Hub
      if (srv.hub) {
        this.hubPort = srv.hub.port || 0;
        this.hubHost = srv.hub.host || '';
        this.hubPublicUrl = srv.hub.public_url || '';
        this.hubReadTimeout = srv.hub.read_timeout || '';
        this.hubWriteTimeout = srv.hub.write_timeout || '';
        this.hubAdminEmails = (srv.hub.admin_emails || []).join(', ');
        this.hubSoftDeleteRetention = srv.hub.soft_delete_retention || '';
        this.hubSoftDeleteRetainFiles = srv.hub.soft_delete_retain_files || false;
      }

      // Broker
      if (srv.broker) {
        this.brokerEnabled = srv.broker.enabled || false;
        this.brokerPort = srv.broker.port || 0;
        this.brokerHost = srv.broker.host || '';
        this.brokerHubEndpoint = srv.broker.hub_endpoint || '';
        this.brokerContainerHubEndpoint = srv.broker.container_hub_endpoint || '';
        this.brokerName = srv.broker.broker_name || '';
        this.brokerNickname = srv.broker.broker_nickname || '';
        this.brokerAutoProvide = srv.broker.auto_provide || false;
      }

      // Database
      if (srv.database) {
        this.dbDriver = srv.database.driver || '';
        this.dbUrl = srv.database.url || '';
      }

      // Auth
      if (srv.auth) {
        this.authDevMode = srv.auth.dev_mode || false;
        this.authDevToken = srv.auth.dev_token || '';
        this.authAuthorizedDomains = (srv.auth.authorized_domains || []).join(', ');
        this.authUserAccessMode = srv.auth.user_access_mode || 'open';
      }

      // Storage
      if (srv.storage) {
        this.storageProvider = srv.storage.provider || '';
        this.storageBucket = srv.storage.bucket || '';
        this.storageLocalPath = srv.storage.local_path || '';
      }

      // Secrets
      if (srv.secrets) {
        this.secretsBackend = srv.secrets.backend || '';
        this.secretsGCPProjectId = srv.secrets.gcp_project_id || '';
      }

      // Message Broker
      if (srv.message_broker) {
        this.messageBrokerEnabled = srv.message_broker.enabled || false;
        this.messageBrokerType = srv.message_broker.type || '';
      }
    }

    // Telemetry
    const tel = data.telemetry;
    if (tel) {
      this.telemetryEnabled = tel.enabled || false;
      if (tel.cloud) {
        this.telemetryCloudEnabled = tel.cloud.enabled || false;
        this.telemetryCloudEndpoint = tel.cloud.endpoint || '';
        this.telemetryCloudProtocol = tel.cloud.protocol || '';
        this.telemetryCloudProvider = tel.cloud.provider || '';
      }
      if (tel.hub) {
        this.telemetryHubEnabled = tel.hub.enabled || false;
        this.telemetryHubReportInterval = tel.hub.report_interval || '';
      }
      if (tel.local) {
        this.telemetryLocalEnabled = tel.local.enabled || false;
        this.telemetryLocalFile = tel.local.file || '';
        this.telemetryLocalConsole = tel.local.console || false;
      }
    }

    // Runtimes, harness_configs, profiles preserved via rawConfig
  }

  private buildPayload(): Record<string, unknown> {
    const payload: Record<string, unknown> = {};

    // General
    payload.active_profile = this.activeProfile || undefined;
    payload.default_template = this.defaultTemplate || undefined;
    payload.default_harness_config = this.defaultHarnessConfig || undefined;
    payload.image_registry = this.imageRegistry || undefined;
    payload.workspace_path = this.workspacePath || undefined;

    // Default agent limits
    payload.default_max_turns = this.defaultMaxTurns || undefined;
    payload.default_max_model_calls = this.defaultMaxModelCalls || undefined;
    payload.default_max_duration = this.defaultMaxDuration || undefined;
    if (
      this.defaultResCpuReq ||
      this.defaultResMemReq ||
      this.defaultResCpuLim ||
      this.defaultResMemLim ||
      this.defaultResDisk
    ) {
      const defaultResources: Record<string, unknown> = {};
      if (this.defaultResCpuReq || this.defaultResMemReq) {
        defaultResources.requests = {
          cpu: this.defaultResCpuReq || undefined,
          memory: this.defaultResMemReq || undefined,
        };
      }
      if (this.defaultResCpuLim || this.defaultResMemLim) {
        defaultResources.limits = {
          cpu: this.defaultResCpuLim || undefined,
          memory: this.defaultResMemLim || undefined,
        };
      }
      if (this.defaultResDisk) defaultResources.disk = this.defaultResDisk;
      payload.default_resources = defaultResources;
    }

    // Server
    const server: Record<string, unknown> = {};
    server.mode = this.serverMode || undefined;
    server.log_level = this.logLevel || undefined;
    server.log_format = this.logFormat || undefined;

    // Hub server
    const hub: Record<string, unknown> = {};
    if (this.hubPort) hub.port = this.hubPort;
    if (this.hubHost) hub.host = this.hubHost;
    if (this.hubPublicUrl) hub.public_url = this.hubPublicUrl;
    if (this.hubReadTimeout) hub.read_timeout = this.hubReadTimeout;
    if (this.hubWriteTimeout) hub.write_timeout = this.hubWriteTimeout;
    if (this.hubAdminEmails) {
      hub.admin_emails = this.hubAdminEmails
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);
    }
    if (this.hubSoftDeleteRetention) hub.soft_delete_retention = this.hubSoftDeleteRetention;
    hub.soft_delete_retain_files = this.hubSoftDeleteRetainFiles;
    server.hub = hub;

    // Broker
    const broker: Record<string, unknown> = {};
    broker.enabled = this.brokerEnabled;
    if (this.brokerPort) broker.port = this.brokerPort;
    if (this.brokerHost) broker.host = this.brokerHost;
    if (this.brokerHubEndpoint) broker.hub_endpoint = this.brokerHubEndpoint;
    if (this.brokerContainerHubEndpoint)
      broker.container_hub_endpoint = this.brokerContainerHubEndpoint;
    if (this.brokerName) broker.broker_name = this.brokerName;
    if (this.brokerNickname) broker.broker_nickname = this.brokerNickname;
    broker.auto_provide = this.brokerAutoProvide;
    server.broker = broker;

    // Database — only send driver, not masked URL
    const database: Record<string, unknown> = {};
    if (this.dbDriver) database.driver = this.dbDriver;
    // Don't send masked URL back
    if (this.dbUrl && this.dbUrl !== '********') database.url = this.dbUrl;
    server.database = database;

    // Auth
    const auth: Record<string, unknown> = {};
    auth.dev_mode = this.authDevMode;
    // Don't send masked token back
    if (this.authDevToken && this.authDevToken !== '********') auth.dev_token = this.authDevToken;
    if (this.authAuthorizedDomains) {
      auth.authorized_domains = this.authAuthorizedDomains
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);
    }
    if (this.authUserAccessMode) {
      auth.user_access_mode = this.authUserAccessMode;
    }
    server.auth = auth;

    // Storage
    const storage: Record<string, unknown> = {};
    if (this.storageProvider) storage.provider = this.storageProvider;
    if (this.storageBucket) storage.bucket = this.storageBucket;
    if (this.storageLocalPath) storage.local_path = this.storageLocalPath;
    server.storage = storage;

    // Secrets
    const secrets: Record<string, unknown> = {};
    if (this.secretsBackend) secrets.backend = this.secretsBackend;
    if (this.secretsGCPProjectId) secrets.gcp_project_id = this.secretsGCPProjectId;
    server.secrets = secrets;

    // Message Broker
    server.message_broker = {
      enabled: this.messageBrokerEnabled,
      type: this.messageBrokerType || undefined,
    };

    // Preserve notification channels, OAuth, and GitHub App from raw config
    if (this.rawConfig?.server?.notification_channels) {
      server.notification_channels = this.rawConfig.server.notification_channels;
    }
    if (this.rawConfig?.server?.oauth) {
      server.oauth = this.rawConfig.server.oauth;
    }
    if (this.rawConfig?.server?.github_app) {
      server.github_app = this.rawConfig.server.github_app;
    }

    payload.server = server;

    // Telemetry
    const telemetry: Record<string, unknown> = {
      enabled: this.telemetryEnabled,
    };
    telemetry.cloud = {
      enabled: this.telemetryCloudEnabled,
      endpoint: this.telemetryCloudEndpoint || undefined,
      protocol: this.telemetryCloudProtocol || undefined,
      provider: this.telemetryCloudProvider || undefined,
    };
    telemetry.hub = {
      enabled: this.telemetryHubEnabled,
      report_interval: this.telemetryHubReportInterval || undefined,
    };
    telemetry.local = {
      enabled: this.telemetryLocalEnabled,
      file: this.telemetryLocalFile || undefined,
      console: this.telemetryLocalConsole,
    };
    payload.telemetry = telemetry;

    // Preserve runtimes, harness_configs, profiles from raw config
    if (this.rawConfig?.runtimes) payload.runtimes = this.rawConfig.runtimes;
    if (this.rawConfig?.harness_configs) payload.harness_configs = this.rawConfig.harness_configs;
    if (this.rawConfig?.profiles) payload.profiles = this.rawConfig.profiles;

    return payload;
  }

  private async handleSave(): Promise<void> {
    this.saving = true;
    this.error = null;
    this.successMessage = null;
    this.reloadResult = null;

    try {
      const payload = this.buildPayload();
      const res = await apiFetch('/api/v1/admin/server-config', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });

      if (!res.ok) {
        this.error = await extractApiError(res, 'Failed to save settings');
        return;
      }

      const result = (await res.json()) as { reload?: ReloadResult };
      this.reloadResult = result.reload ?? null;
      this.successMessage = 'Settings saved successfully';

      // Reload the form with fresh data
      await this.loadConfig();
      this.successMessage = 'Settings saved successfully';
    } catch {
      this.error = 'Failed to save settings';
    } finally {
      this.saving = false;
    }
  }

  override render() {
    return html`
      <div class="header">
        <sl-icon name="sliders"></sl-icon>
        <h1>Server Configuration</h1>
      </div>
      <p class="header-description">
        Edit the global server settings (settings.yaml). Some changes take effect immediately;
        others require a server restart.
      </p>

      ${this.error ? html`<div class="status-message error">${this.error}</div>` : nothing}
      ${this.successMessage
        ? html`<div class="status-message success">
            ${this.successMessage} ${this.reloadResult ? this.renderReloadInfo() : nothing}
          </div>`
        : nothing}
      ${this.loading
        ? html`<div class="loading-container"><sl-spinner></sl-spinner></div>`
        : this.renderForm()}
    `;
  }

  private renderReloadInfo() {
    const r = this.reloadResult;
    if (!r) return nothing;
    return html`
      <div class="reload-info">
        ${r.applied && r.applied.length > 0
          ? html`<div class="applied">Reloaded: ${r.applied.join(', ')}</div>`
          : nothing}
        ${r.requires_restart && r.requires_restart.length > 0
          ? html`<div class="restart">Requires restart: ${r.requires_restart.join(', ')}</div>`
          : nothing}
        ${r.error ? html`<div class="error">Reload error: ${r.error}</div>` : nothing}
      </div>
    `;
  }

  private renderForm() {
    return html`
      <sl-tab-group
        @sl-tab-show=${(e: CustomEvent) => {
          this.activeTab = (e.detail as { name: string }).name;
        }}
      >
        <sl-tab slot="nav" panel="general" ?active=${this.activeTab === 'general'}>General</sl-tab>
        <sl-tab slot="nav" panel="hub-server" ?active=${this.activeTab === 'hub-server'}
          >Hub Server</sl-tab
        >
        <sl-tab slot="nav" panel="broker" ?active=${this.activeTab === 'broker'}
          >Runtime Broker</sl-tab
        >
        <sl-tab slot="nav" panel="data" ?active=${this.activeTab === 'data'}>Data & Storage</sl-tab>
        <sl-tab slot="nav" panel="auth" ?active=${this.activeTab === 'auth'}>Authentication</sl-tab>
        <sl-tab slot="nav" panel="telemetry" ?active=${this.activeTab === 'telemetry'}
          >Telemetry</sl-tab
        >
        <sl-tab slot="nav" panel="github-app" ?active=${this.activeTab === 'github-app'}
          >GitHub App</sl-tab
        >
        <sl-tab slot="nav" panel="gcp-identity" ?active=${this.activeTab === 'gcp-identity'}
          >GCP Identity</sl-tab
        >

        <sl-tab-panel name="general">${this.renderGeneralTab()}</sl-tab-panel>
        <sl-tab-panel name="hub-server">${this.renderHubServerTab()}</sl-tab-panel>
        <sl-tab-panel name="broker">${this.renderBrokerTab()}</sl-tab-panel>
        <sl-tab-panel name="data">${this.renderDataTab()}</sl-tab-panel>
        <sl-tab-panel name="auth">${this.renderAuthTab()}</sl-tab-panel>
        <sl-tab-panel name="telemetry">${this.renderTelemetryTab()}</sl-tab-panel>
        <sl-tab-panel name="github-app">${this.renderGitHubAppTab()}</sl-tab-panel>
        <sl-tab-panel name="gcp-identity">${this.renderGCPIdentityTab()}</sl-tab-panel>
      </sl-tab-group>

      <div class="actions">
        <sl-button
          variant="primary"
          ?loading=${this.saving}
          @click=${() => {
            void this.handleSave();
          }}
        >
          Save & Reload
        </sl-button>
        <sl-button
          variant="default"
          @click=${() => {
            void this.loadConfig();
          }}
        >
          Reset
        </sl-button>
      </div>
    `;
  }

  // ── Tab renderers ──

  private renderVersionInfo() {
    if (!this.scionVersion && !this.scionCommit) return nothing;
    const r = this.updateCheckResult;
    return html`
      <div class="section">
        <h3 class="section-title">Scion Server Version</h3>
        <div class="version-info">
          ${this.scionVersion
            ? html`<div class="version-item">
                <span class="version-label">Version</span>
                <span class="version-value">${this.scionVersion}</span>
              </div>`
            : nothing}
          ${this.scionCommit
            ? html`<div class="version-item">
                <span class="version-label">Git Commit</span>
                <span class="version-value"><code>${this.scionCommit}</code></span>
              </div>`
            : nothing}
          ${this.scionBuildTime
            ? html`<div class="version-item">
                <span class="version-label">Build Time</span>
                <span class="version-value">${this.scionBuildTime}</span>
              </div>`
            : nothing}
          <div class="version-actions">
            <sl-button
              size="small"
              variant="default"
              ?loading=${this.updateCheckLoading}
              @click=${() => this.checkForUpdates()}
            >
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              Check for Updates
            </sl-button>
          </div>
        </div>
        ${this.updateCheckError
          ? html`<div class="update-error">${this.updateCheckError}</div>`
          : nothing}
        ${r && r.update_available
          ? html`
              <div class="update-banner">
                <div class="update-banner-header">
                  <sl-icon name="info-circle"></sl-icon>
                  Update available${r.current_branch && r.current_branch !== 'main' ? html` on <code>${r.current_branch}</code>` : nothing} &mdash; ${r.commits_behind} new commit${r.commits_behind === 1 ? '' : 's'}
                </div>
                ${r.new_commits && r.new_commits.length > 0
                  ? html`
                      <div class="update-commits">
                        ${r.new_commits.map(
                          (c) => html`
                            <div>
                              <span class="commit-hash">${c.hash}</span>${c.subject}
                            </div>
                          `,
                        )}
                      </div>
                    `
                  : nothing}
                <div class="update-banner-actions">
                  <sl-button
                    size="small"
                    variant="primary"
                    ?loading=${this.updateRunning}
                    @click=${() => (this.showUpdateConfirm = true)}
                  >
                    <sl-icon slot="prefix" name="download"></sl-icon>
                    Update Now
                  </sl-button>
                </div>
              </div>
            `
          : nothing}
        ${r && !r.update_available
          ? html`<div class="update-current">Server is up to date${r.current_branch && r.current_branch !== 'main' ? html` on <code>${r.current_branch}</code>` : nothing}.</div>`
          : nothing}
      </div>
      ${this.showUpdateConfirm ? this.renderUpdateConfirmDialog() : nothing}
    `;
  }

  private renderGeneralTab() {
    return html`
      ${this.renderVersionInfo()}
      <div class="section">
        <h3 class="section-title">General Settings</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>Server Mode</label>
            <span class="hint">Operating mode: workstation or production</span>
            <sl-select
              value=${this.serverMode || 'workstation'}
              @sl-change=${(e: Event) => {
                this.serverMode = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="workstation">Workstation</sl-option>
              <sl-option value="production">Production</sl-option>
            </sl-select>
          </div>
          <div class="form-field">
            <label>Log Level</label>
            <sl-select
              value=${this.logLevel || 'info'}
              @sl-change=${(e: Event) => {
                this.logLevel = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="debug">Debug</sl-option>
              <sl-option value="info">Info</sl-option>
              <sl-option value="warn">Warn</sl-option>
              <sl-option value="error">Error</sl-option>
            </sl-select>
          </div>
          <div class="form-field">
            <label>Log Format</label>
            <sl-select
              value=${this.logFormat || 'text'}
              @sl-change=${(e: Event) => {
                this.logFormat = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="text">Text</sl-option>
              <sl-option value="json">JSON</sl-option>
            </sl-select>
          </div>
          <div class="form-field">
            <label>Active Profile</label>
            <span class="hint">Default runtime profile for agents</span>
            <sl-input
              value=${this.activeProfile}
              placeholder="default"
              @sl-input=${(e: Event) => {
                this.activeProfile = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Default Template</label>
            <sl-input
              value=${this.defaultTemplate}
              placeholder="default"
              @sl-input=${(e: Event) => {
                this.defaultTemplate = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Default Harness Config</label>
            <sl-input
              value=${this.defaultHarnessConfig}
              @sl-input=${(e: Event) => {
                this.defaultHarnessConfig = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Image Registry</label>
            <span class="hint"
              >Container image registry for agent images (e.g., ghcr.io/myorg)</span
            >
            <sl-input
              value=${this.imageRegistry}
              placeholder="ghcr.io/myorg"
              @sl-input=${(e: Event) => {
                this.imageRegistry = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Workspace Path</label>
            <span class="hint">Override default workspace path for agent worktrees</span>
            <sl-input
              value=${this.workspacePath}
              @sl-input=${(e: Event) => {
                this.workspacePath = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Default Agent Limits</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>Default Max Turns</label>
            <span class="hint">Maximum conversation turns for new agents</span>
            <sl-input
              type="number"
              value=${this.defaultMaxTurns ? String(this.defaultMaxTurns) : ''}
              placeholder="No limit"
              @sl-input=${(e: Event) => {
                this.defaultMaxTurns = parseInt((e.target as HTMLInputElement).value) || 0;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Default Max Model Calls</label>
            <span class="hint">Maximum LLM API calls for new agents</span>
            <sl-input
              type="number"
              value=${this.defaultMaxModelCalls ? String(this.defaultMaxModelCalls) : ''}
              placeholder="No limit"
              @sl-input=${(e: Event) => {
                this.defaultMaxModelCalls = parseInt((e.target as HTMLInputElement).value) || 0;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Default Max Duration</label>
            <span class="hint">Maximum execution time (Go duration, e.g. 2h, 30m)</span>
            <sl-input
              value=${this.defaultMaxDuration}
              placeholder="e.g. 2h, 30m"
              @sl-input=${(e: Event) => {
                this.defaultMaxDuration = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Default Agent Resources</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>CPU Request</label>
            <sl-input
              value=${this.defaultResCpuReq}
              placeholder="e.g. 500m, 1"
              @sl-input=${(e: Event) => {
                this.defaultResCpuReq = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Memory Request</label>
            <sl-input
              value=${this.defaultResMemReq}
              placeholder="e.g. 512Mi, 1Gi"
              @sl-input=${(e: Event) => {
                this.defaultResMemReq = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>CPU Limit</label>
            <sl-input
              value=${this.defaultResCpuLim}
              placeholder="e.g. 1, 2"
              @sl-input=${(e: Event) => {
                this.defaultResCpuLim = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Memory Limit</label>
            <sl-input
              value=${this.defaultResMemLim}
              placeholder="e.g. 1Gi, 2Gi"
              @sl-input=${(e: Event) => {
                this.defaultResMemLim = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Disk</label>
            <sl-input
              value=${this.defaultResDisk}
              placeholder="e.g. 10Gi"
              @sl-input=${(e: Event) => {
                this.defaultResDisk = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      ${this.renderMessageBrokerSection()}
    `;
  }

  private renderMessageBrokerSection() {
    return html`
      <div class="section">
        <h3 class="section-title">Message Broker</h3>
        <div class="form-grid">
          <div class="form-field">
            <sl-switch
              ?checked=${this.messageBrokerEnabled}
              @sl-change=${(e: Event) => {
                this.messageBrokerEnabled = (e.target as HTMLInputElement).checked;
              }}
              >Enable Message Broker</sl-switch
            >
          </div>
          <div class="form-field">
            <label>Type</label>
            <sl-select
              value=${this.messageBrokerType || 'inprocess'}
              @sl-change=${(e: Event) => {
                this.messageBrokerType = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="inprocess">In-Process</sl-option>
            </sl-select>
          </div>
        </div>
      </div>
    `;
  }

  private renderHubServerTab() {
    return html`
      <div class="section">
        <h3 class="section-title">Hub API Server</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>Port</label>
            <span class="hint">Requires restart</span>
            <sl-input
              type="number"
              value=${String(this.hubPort || 9810)}
              @sl-input=${(e: Event) => {
                this.hubPort = parseInt((e.target as HTMLInputElement).value) || 0;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Host</label>
            <span class="hint">Requires restart</span>
            <sl-input
              value=${this.hubHost || '0.0.0.0'}
              @sl-input=${(e: Event) => {
                this.hubHost = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Public URL</label>
            <span class="hint">Endpoint URL for agents to call back to the Hub</span>
            <sl-input
              value=${this.hubPublicUrl}
              placeholder="https://hub.example.com"
              @sl-input=${(e: Event) => {
                this.hubPublicUrl = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Read Timeout</label>
            <sl-input
              value=${this.hubReadTimeout || '30s'}
              placeholder="30s"
              @sl-input=${(e: Event) => {
                this.hubReadTimeout = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Write Timeout</label>
            <sl-input
              value=${this.hubWriteTimeout || '60s'}
              placeholder="60s"
              @sl-input=${(e: Event) => {
                this.hubWriteTimeout = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Admin Emails</label>
            <span class="hint"
              >Comma-separated list of email addresses to auto-promote to admin</span
            >
            <sl-input
              value=${this.hubAdminEmails}
              placeholder="admin@example.com, ops@example.com"
              @sl-input=${(e: Event) => {
                this.hubAdminEmails = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Agent Lifecycle</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>Soft Delete Retention</label>
            <span class="hint"
              >How long soft-deleted agents are retained (e.g., 72h). Empty disables
              soft-delete.</span
            >
            <sl-input
              value=${this.hubSoftDeleteRetention}
              placeholder="72h"
              @sl-input=${(e: Event) => {
                this.hubSoftDeleteRetention = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <sl-switch
              ?checked=${this.hubSoftDeleteRetainFiles}
              @sl-change=${(e: Event) => {
                this.hubSoftDeleteRetainFiles = (e.target as HTMLInputElement).checked;
              }}
              >Retain files on soft delete</sl-switch
            >
          </div>
        </div>
      </div>
    `;
  }

  private renderBrokerTab() {
    return html`
      <div class="section">
        <h3 class="section-title">Runtime Broker</h3>
        <div class="form-grid">
          <div class="form-field full-width">
            <sl-switch
              ?checked=${this.brokerEnabled}
              @sl-change=${(e: Event) => {
                this.brokerEnabled = (e.target as HTMLInputElement).checked;
              }}
              >Enable Runtime Broker</sl-switch
            >
            <span class="hint">Requires restart</span>
          </div>
          <div class="form-field">
            <label>Port</label>
            <span class="hint">Requires restart</span>
            <sl-input
              type="number"
              value=${String(this.brokerPort || 9800)}
              @sl-input=${(e: Event) => {
                this.brokerPort = parseInt((e.target as HTMLInputElement).value) || 0;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Host</label>
            <span class="hint">Requires restart</span>
            <sl-input
              value=${this.brokerHost || '0.0.0.0'}
              @sl-input=${(e: Event) => {
                this.brokerHost = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Hub Endpoint</label>
            <span class="hint"
              >Hub API endpoint for status reporting (when Hub not co-located)</span
            >
            <sl-input
              value=${this.brokerHubEndpoint}
              placeholder="https://hub.example.com"
              @sl-input=${(e: Event) => {
                this.brokerHubEndpoint = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Container Hub Endpoint</label>
            <span class="hint"
              >Override Hub URL injected into agent containers (e.g.,
              host.containers.internal)</span
            >
            <sl-input
              value=${this.brokerContainerHubEndpoint}
              @sl-input=${(e: Event) => {
                this.brokerContainerHubEndpoint = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Broker Name</label>
            <sl-input
              value=${this.brokerName}
              @sl-input=${(e: Event) => {
                this.brokerName = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Broker Nickname</label>
            <sl-input
              value=${this.brokerNickname}
              @sl-input=${(e: Event) => {
                this.brokerNickname = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <sl-switch
              ?checked=${this.brokerAutoProvide}
              @sl-change=${(e: Event) => {
                this.brokerAutoProvide = (e.target as HTMLInputElement).checked;
              }}
              >Auto-provide to hub projects</sl-switch
            >
          </div>
        </div>
      </div>
    `;
  }

  private renderDataTab() {
    return html`
      <div class="section">
        <h3 class="section-title">Database</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>Driver</label>
            <span class="hint">Requires restart</span>
            <sl-select
              value=${this.dbDriver || 'sqlite'}
              @sl-change=${(e: Event) => {
                this.dbDriver = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="sqlite">SQLite</sl-option>
              <sl-option value="postgres">PostgreSQL</sl-option>
            </sl-select>
          </div>
          <div class="form-field">
            <label>URL</label>
            <span class="hint"
              >Requires restart. ${this.dbUrl === '********' ? 'Value is masked.' : ''}</span
            >
            <sl-input
              value=${this.dbUrl}
              placeholder="Path or connection string"
              @sl-input=${(e: Event) => {
                this.dbUrl = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Storage</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>Provider</label>
            <sl-select
              value=${this.storageProvider || 'local'}
              @sl-change=${(e: Event) => {
                this.storageProvider = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="local">Local</sl-option>
              <sl-option value="gcs">Google Cloud Storage</sl-option>
            </sl-select>
          </div>
          <div class="form-field">
            <label>Bucket</label>
            <sl-input
              value=${this.storageBucket}
              @sl-input=${(e: Event) => {
                this.storageBucket = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Local Path</label>
            <sl-input
              value=${this.storageLocalPath}
              @sl-input=${(e: Event) => {
                this.storageLocalPath = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Secrets Backend</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>Backend</label>
            <span class="hint">Requires restart</span>
            <sl-select
              value=${this.secretsBackend || 'local'}
              @sl-change=${(e: Event) => {
                this.secretsBackend = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="local">Local</sl-option>
              <sl-option value="gcpsm">GCP Secret Manager</sl-option>
            </sl-select>
          </div>
          <div class="form-field">
            <label>GCP Project ID</label>
            <sl-input
              value=${this.secretsGCPProjectId}
              @sl-input=${(e: Event) => {
                this.secretsGCPProjectId = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>
    `;
  }

  private renderAuthTab() {
    return html`
      <div class="section">
        <h3 class="section-title">User Access Mode</h3>
        <div class="form-grid">
          <div class="form-field full-width">
            <label>Access Mode</label>
            <span class="hint">Controls who can log in to this hub. Takes effect immediately (hot-reloaded).</span>
            <sl-select
              value=${this.authUserAccessMode}
              @sl-change=${(e: Event) => {
                this.authUserAccessMode = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="open">Open (all authenticated users)</sl-option>
              <sl-option value="domain_restricted">Domain Restricted (authorized domains only)</sl-option>
              <sl-option value="invite_only">Invite Only (allow list + authorized domains)</sl-option>
            </sl-select>
            ${this.authUserAccessMode === 'invite_only'
              ? html`<sl-alert variant="warning" open style="margin-top: 0.75rem">
                  <sl-icon slot="icon" name="exclamation-triangle"></sl-icon>
                  Only emails on the <strong>Allow List</strong> (and admin emails) will be able to log in.
                  Manage the allow list from the <a href="/admin/users">Users page</a>.
                  ${this.authAuthorizedDomains
                    ? html`<br/><br/>Authorized domains are also enforced — users must match both the allow list <em>and</em> an authorized domain.`
                    : ''}
                </sl-alert>`
              : ''}
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Development Auth</h3>
        <div class="form-grid">
          <div class="form-field full-width">
            <sl-switch
              ?checked=${this.authDevMode}
              @sl-change=${(e: Event) => {
                this.authDevMode = (e.target as HTMLInputElement).checked;
              }}
              >Enable Dev Auth</sl-switch
            >
            <span class="hint">Requires restart. NOT for production use.</span>
          </div>
          <div class="form-field full-width">
            <label>Dev Token</label>
            <span class="hint"
              >${this.authDevToken === '********'
                ? 'Value is masked. Clear to auto-generate.'
                : 'Leave empty to auto-generate.'}</span
            >
            <sl-input
              value=${this.authDevToken}
              @sl-input=${(e: Event) => {
                this.authDevToken = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Authorized Domains</label>
            <span class="hint"
              >Comma-separated list of email domains allowed to authenticate (empty = all)</span
            >
            <sl-input
              value=${this.authAuthorizedDomains}
              placeholder="example.com, corp.example.com"
              @sl-input=${(e: Event) => {
                this.authAuthorizedDomains = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">OAuth Providers</h3>
        <p class="hint" style="margin: 0 0 1rem 0">
          OAuth client credentials are managed via the settings file or environment variables.
          Secrets are masked in this view.
        </p>
        ${this.renderOAuthDisplay()}
      </div>
    `;
  }

  private renderOAuthDisplay() {
    const oauth = this.rawConfig?.server?.oauth;
    if (!oauth) {
      return html`<p class="hint">No OAuth providers configured.</p>`;
    }

    const renderProvider = (label: string, cfg?: V1OAuthClientConfig) => {
      if (!cfg) return nothing;
      const providers: { name: string; p: V1OAuthProviderConfig | undefined }[] = [
        { name: 'Google', p: cfg.google },
        { name: 'GitHub', p: cfg.github },
      ];
      const configured = providers.filter((p) => p.p?.client_id);
      if (configured.length === 0) return nothing;

      return html`
        <div style="margin-bottom: 1rem">
          <strong style="font-size: 0.875rem">${label}</strong>
          ${configured.map(
            (p) => html`
              <div style="margin-left: 1rem; font-size: 0.8125rem; color: var(--scion-text-muted)">
                ${p.name}: ${p.p?.client_id || ''} / <span class="masked-value">********</span>
              </div>
            `
          )}
        </div>
      `;
    };

    return html`
      ${renderProvider('Web', oauth.web)} ${renderProvider('CLI', oauth.cli)}
      ${renderProvider('Device', oauth.device)}
    `;
  }

  private renderTelemetryTab() {
    return html`
      <div class="section">
        <h3 class="section-title">Telemetry</h3>
        <div class="form-grid">
          <div class="form-field full-width">
            <sl-switch
              ?checked=${this.telemetryEnabled}
              @sl-change=${(e: Event) => {
                this.telemetryEnabled = (e.target as HTMLInputElement).checked;
              }}
              >Enable Telemetry Collection</sl-switch
            >
            <span class="hint">Default opt-in state for new agents</span>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Cloud Export (OTLP)</h3>
        <div class="form-grid">
          <div class="form-field full-width">
            <sl-switch
              ?checked=${this.telemetryCloudEnabled}
              @sl-change=${(e: Event) => {
                this.telemetryCloudEnabled = (e.target as HTMLInputElement).checked;
              }}
              >Enable Cloud Export</sl-switch
            >
          </div>
          <div class="form-field full-width">
            <label>Endpoint</label>
            <sl-input
              value=${this.telemetryCloudEndpoint}
              placeholder="https://otel-collector.example.com:4317"
              @sl-input=${(e: Event) => {
                this.telemetryCloudEndpoint = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>Protocol</label>
            <sl-select
              value=${this.telemetryCloudProtocol || 'grpc'}
              @sl-change=${(e: Event) => {
                this.telemetryCloudProtocol = (e.target as HTMLSelectElement).value;
              }}
            >
              <sl-option value="grpc">gRPC</sl-option>
              <sl-option value="http/protobuf">HTTP/Protobuf</sl-option>
              <sl-option value="http/json">HTTP/JSON</sl-option>
            </sl-select>
          </div>
          <div class="form-field">
            <label>Provider</label>
            <sl-input
              value=${this.telemetryCloudProvider}
              placeholder="e.g., gcp"
              @sl-input=${(e: Event) => {
                this.telemetryCloudProvider = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Hub Reporting</h3>
        <div class="form-grid">
          <div class="form-field">
            <sl-switch
              ?checked=${this.telemetryHubEnabled}
              @sl-change=${(e: Event) => {
                this.telemetryHubEnabled = (e.target as HTMLInputElement).checked;
              }}
              >Enable Hub Reporting</sl-switch
            >
          </div>
          <div class="form-field">
            <label>Report Interval</label>
            <sl-input
              value=${this.telemetryHubReportInterval}
              placeholder="30s"
              @sl-input=${(e: Event) => {
                this.telemetryHubReportInterval = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>

      <div class="section">
        <h3 class="section-title">Local Debug Output</h3>
        <div class="form-grid">
          <div class="form-field">
            <sl-switch
              ?checked=${this.telemetryLocalEnabled}
              @sl-change=${(e: Event) => {
                this.telemetryLocalEnabled = (e.target as HTMLInputElement).checked;
              }}
              >Enable Local Output</sl-switch
            >
          </div>
          <div class="form-field">
            <sl-switch
              ?checked=${this.telemetryLocalConsole}
              @sl-change=${(e: Event) => {
                this.telemetryLocalConsole = (e.target as HTMLInputElement).checked;
              }}
              >Console Output</sl-switch
            >
          </div>
          <div class="form-field full-width">
            <label>Log File</label>
            <sl-input
              value=${this.telemetryLocalFile}
              placeholder="/var/log/scion/telemetry.log"
              @sl-input=${(e: Event) => {
                this.telemetryLocalFile = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>
      </div>
    `;
  }

  // ── GCP Identity Tab ──

  private renderGCPIdentityTab() {
    return html`
      <div class="section">
        <h3 class="section-title">GCP Service Account Minting</h3>
        ${this.gcpQuotaLoading
          ? html`<div style="text-align: center; padding: 1rem;"><sl-spinner></sl-spinner></div>`
          : this.gcpQuotaData
            ? this.renderGCPQuotaContent()
            : html`<p style="color: var(--scion-text-muted);">
                Click "Load Quota" to fetch current minting statistics.
              </p>
              <sl-button size="small" @click=${() => this.loadGCPQuota()}>
                <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
                Load Quota
              </sl-button>`}
      </div>
    `;
  }

  private renderGCPQuotaContent() {
    const q = this.gcpQuotaData!;

    if (!q.minting_configured) {
      return html`
        <p style="color: var(--scion-text-muted);">
          GCP service account minting is not configured on this Hub.
          Set <code>GCPProjectID</code> and ensure the Hub SA has
          <code>roles/iam.serviceAccountCreator</code> to enable minting.
        </p>
      `;
    }

    return html`
      <div class="form-grid">
        <div class="form-field">
          <label>GCP Project</label>
          <span style="font-size: 0.875rem; font-family: monospace;">${q.gcp_project_id}</span>
        </div>
        <div class="form-field">
          <label>Global Minted</label>
          <span style="font-size: 0.875rem;">
            ${q.global_minted}${q.global_cap > 0 ? ` / ${q.global_cap}` : ' (no limit)'}
          </span>
        </div>
        <div class="form-field">
          <label>Per-Project Cap</label>
          <span style="font-size: 0.875rem;">
            ${q.per_project_cap > 0 ? q.per_project_cap : 'Unlimited'}
          </span>
        </div>
        <div class="form-field">
          <label>Global Cap</label>
          <span style="font-size: 0.875rem;">
            ${q.global_cap > 0 ? q.global_cap : 'Unlimited'}
          </span>
        </div>
      </div>

      ${q.projects && q.projects.length > 0 ? html`
        <h3 class="section-title" style="margin-top: 1.5rem;">Per-Project Usage</h3>
        <div style="display: flex; flex-direction: column; gap: 0.5rem;">
          ${q.projects.map(p => html`
            <div style="display: flex; align-items: center; gap: 0.75rem; padding: 0.75rem; background: var(--scion-bg-subtle, #f8fafc); border: 1px solid var(--scion-border, #e2e8f0); border-radius: var(--scion-radius, 0.5rem);">
              <sl-icon name="folder"></sl-icon>
              <div style="flex: 1;">
                <strong>${p.project_name}</strong>
              </div>
              <span style="font-size: 0.875rem; font-weight: 500;">
                ${p.minted} minted${q.per_project_cap > 0 ? ` / ${q.per_project_cap}` : ''}
              </span>
            </div>
          `)}
        </div>
      ` : html`
        <p style="color: var(--scion-text-muted); margin-top: 1rem;">
          No service accounts have been minted yet.
        </p>
      `}

      <div style="margin-top: 1rem;">
        <sl-button size="small" @click=${() => this.loadGCPQuota()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Refresh
        </sl-button>
      </div>
    `;
  }

  private async loadGCPQuota(): Promise<void> {
    this.gcpQuotaLoading = true;
    try {
      const res = await apiFetch('/api/v1/admin/gcp-quota');
      if (res.ok) {
        this.gcpQuotaData = await res.json();
      }
    } catch {
      // Non-critical
    } finally {
      this.gcpQuotaLoading = false;
    }
  }

  // ── GitHub App Tab ──

  private renderGitHubAppTab() {
    return html`
      ${this.githubAppError ? html`<div class="status-message error">${this.githubAppError}</div>` : ''}
      ${this.githubAppSuccess ? html`<div class="status-message success">${this.githubAppSuccess}</div>` : ''}

      <div class="section">
        <h3 class="section-title">GitHub App Configuration</h3>
        <div class="form-grid">
          <div class="form-field">
            <label>App ID</label>
            <span class="hint">The numeric ID of your registered GitHub App</span>
            <sl-input
              .value=${this.githubAppId ? String(this.githubAppId) : ''}
              placeholder="e.g. 123456"
              inputmode="numeric"
              @sl-input=${(e: Event) => {
                this.githubAppId = parseInt((e.target as HTMLInputElement).value) || 0;
              }}
            ></sl-input>
          </div>
          <div class="form-field">
            <label>API Base URL</label>
            <span class="hint">Override for GitHub Enterprise Server (leave empty for github.com)</span>
            <sl-input
              .value=${this.githubAppApiBaseUrl}
              placeholder="https://api.github.com"
              @sl-input=${(e: Event) => {
                this.githubAppApiBaseUrl = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
          <div class="form-field full-width">
            <label>Private Key (PEM)</label>
            <span class="hint">
              ${this.githubAppHasPrivateKey
                ? 'A private key is configured. Paste a new key to replace it, or leave empty to keep the current key.'
                : 'Paste the PEM-encoded private key from your GitHub App settings.'}
            </span>
            <sl-textarea
              rows=${4}
              .value=${this.githubAppPrivateKey}
              placeholder=${this.githubAppHasPrivateKey ? '(configured — leave empty to keep current)' : '-----BEGIN RSA PRIVATE KEY-----\n...'}
              @sl-input=${(e: Event) => {
                this.githubAppPrivateKey = (e.target as HTMLTextAreaElement).value;
              }}
            ></sl-textarea>
            ${this.githubAppHasPrivateKey
              ? html`<span style="font-size: 0.75rem; color: var(--scion-success-text, #166534);">Stored as hub secret: GITHUB_APP_PRIVATE_KEY</span>`
              : ''}
          </div>
          <div class="form-field">
            <label>Webhook Secret</label>
            <span class="hint">
              ${this.githubAppHasWebhookSecret
                ? 'A webhook secret is configured. Enter a new value to replace it.'
                : 'Secret for validating incoming GitHub webhook payloads.'}
            </span>
            <sl-input
              type="password"
              password-toggle
              .value=${this.githubAppWebhookSecret}
              placeholder=${this.githubAppHasWebhookSecret ? '(configured — leave empty to keep current)' : 'whsec_...'}
              @sl-input=${(e: Event) => {
                this.githubAppWebhookSecret = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            ${this.githubAppHasWebhookSecret
              ? html`<span style="font-size: 0.75rem; color: var(--scion-success-text, #166534);">Stored as hub secret: GITHUB_APP_WEBHOOK_SECRET</span>`
              : ''}
          </div>
          <div class="form-field">
            <label>Webhooks</label>
            <span class="hint">Enable to receive installation lifecycle events from GitHub</span>
            <sl-switch
              .checked=${this.githubAppWebhooksEnabled}
              @sl-change=${(e: Event) => {
                this.githubAppWebhooksEnabled = (e.target as HTMLInputElement).checked;
              }}
            >
              ${this.githubAppWebhooksEnabled ? 'Enabled' : 'Disabled'}
            </sl-switch>
          </div>
          <div class="form-field full-width">
            <label>Public Installation URL</label>
            <span class="hint">The public link where users can install this GitHub App on their org or account</span>
            <sl-input
              .value=${this.githubAppInstallationUrl}
              placeholder="https://github.com/apps/your-app-name/installations/new"
              @sl-input=${(e: Event) => {
                this.githubAppInstallationUrl = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
          </div>
        </div>

        ${this.githubAppRateLimit ? html`
          <div style="margin-top: 1rem;">
            <span class="hint">Rate Limit</span>
            <div style="font-size: 0.875rem; margin-top: 0.25rem;">
              ${this.githubAppRateLimit.remaining}/${this.githubAppRateLimit.limit} remaining
              ${this.githubAppRateLimit.remaining < this.githubAppRateLimit.limit / 5
                ? html`<span style="color: var(--sl-color-danger-600);">  (low)</span>`
                : ''}
            </div>
          </div>
        ` : ''}

        <div class="actions">
          <sl-button
            variant="primary"
            ?loading=${this.githubAppSaving}
            @click=${() => this.handleSaveGitHubApp()}
          >
            <sl-icon slot="prefix" name="check-lg"></sl-icon>
            Save GitHub App Configuration
          </sl-button>
          ${this.githubAppConfigured
            ? html`<span style="font-size: 0.75rem; color: var(--scion-success-text, #166534);">Configured</span>`
            : html`<span style="font-size: 0.75rem; color: var(--scion-text-muted, #64748b);">Not configured</span>`}
        </div>
      </div>

      ${this.githubAppConfigured ? html`
        <div class="section">
          <h3 class="section-title">Installations</h3>
          <div style="display: flex; gap: 0.5rem; margin-bottom: 1rem;">
            <sl-button size="small" variant="default"
              ?loading=${this.githubAppDiscoverLoading}
              @click=${() => this.handleGitHubAppDiscover()}>
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              Discover from GitHub
            </sl-button>
            <sl-button size="small" variant="default"
              ?loading=${this.githubAppSyncLoading}
              @click=${() => this.handleGitHubAppSyncPermissions()}>
              <sl-icon slot="prefix" name="shield-check"></sl-icon>
              Sync Permissions
            </sl-button>
          </div>

          ${this.githubAppSyncResult ? html`
            <div class="status-message success">${this.githubAppSyncResult}</div>
          ` : ''}

          ${this.githubAppInstallationsLoading
            ? html`<div style="text-align: center; padding: 1rem;"><sl-spinner></sl-spinner></div>`
            : this.githubAppInstallations.length === 0
              ? html`<p style="color: var(--scion-text-muted);">No installations found. Click "Discover from GitHub" to sync.</p>`
              : html`
                  <div style="display: flex; flex-direction: column; gap: 0.5rem;">
                    ${this.githubAppInstallations.map(inst => html`
                      <div style="display: flex; align-items: center; gap: 0.75rem; padding: 0.75rem; background: var(--scion-bg-subtle, #f8fafc); border: 1px solid var(--scion-border, #e2e8f0); border-radius: var(--scion-radius, 0.5rem);">
                        <sl-icon name=${inst.account_type === 'Organization' ? 'building' : 'person'}></sl-icon>
                        <div style="flex: 1;">
                          <strong>${inst.account_login}</strong>
                          <div style="font-size: 0.75rem; color: var(--scion-text-muted);">
                            ${inst.account_type} · ${inst.repositories?.length || 0} repos · ID: ${inst.installation_id}
                          </div>
                        </div>
                        <span style="font-size: 0.6875rem; padding: 0.125rem 0.5rem; border-radius: 9999px; background: ${inst.status === 'active' ? '#dcfce7' : '#fef2f2'}; color: ${inst.status === 'active' ? '#166534' : '#991b1b'};">
                          ${inst.status}
                        </span>
                      </div>
                    `)}
                  </div>
                `}
        </div>
      ` : ''}
    `;
  }

  private async loadGitHubAppConfig(): Promise<void> {
    try {
      const res = await apiFetch('/api/v1/github-app');
      if (res.ok) {
        this.githubAppError = null;
        const data = (await res.json()) as GitHubAppConfigData;
        this.githubAppConfigured = data.configured;
        this.githubAppId = data.app_id;
        this.githubAppApiBaseUrl = data.api_base_url || '';
        this.githubAppWebhooksEnabled = data.webhooks_enabled;
        this.githubAppHasPrivateKey = data.has_private_key;
        this.githubAppHasWebhookSecret = data.has_webhook_secret;
        this.githubAppInstallationUrl = data.installation_url || '';
        this.githubAppRateLimit = data.rate_limit || null;
        // Keep rawConfig in sync so Save & Reload preserves these values
        if (this.rawConfig) {
          if (!this.rawConfig.server) {
            this.rawConfig.server = {};
          }
          const ghApp: V1GitHubAppConfig = { webhooks_enabled: data.webhooks_enabled };
          if (data.app_id) ghApp.app_id = data.app_id;
          if (data.api_base_url) ghApp.api_base_url = data.api_base_url;
          if (data.installation_url) ghApp.installation_url = data.installation_url;
          this.rawConfig.server.github_app = ghApp;
        }
        // Clear write-only fields after load
        this.githubAppPrivateKey = '';
        this.githubAppWebhookSecret = '';
      } else {
        this.githubAppError = await extractApiError(
          res,
          'Failed to load GitHub App configuration',
        );
      }
    } catch (e) {
      // Non-critical — tab just shows unconfigured state
    }
  }

  private async loadGitHubAppInstallations(): Promise<void> {
    this.githubAppInstallationsLoading = true;
    try {
      const res = await apiFetch('/api/v1/github-app/installations');
      if (res.ok) {
        const data = (await res.json()) as { installations: GitHubInstallationInfo[] };
        this.githubAppInstallations = data.installations || [];
      }
    } catch (e) {
      // Non-critical
    } finally {
      this.githubAppInstallationsLoading = false;
    }
  }

  private async handleGitHubAppDiscover(): Promise<void> {
    this.githubAppDiscoverLoading = true;
    this.githubAppSyncResult = null;
    try {
      const res = await apiFetch('/api/v1/github-app/installations/discover', { method: 'POST' });
      if (res.ok) {
        const data = (await res.json()) as { total: number };
        this.githubAppSyncResult = `Discovered ${data.total} installation(s) from GitHub.`;
        await this.loadGitHubAppInstallations();
      } else {
        const err = (await res.json().catch(() => ({}))) as { message?: string };
        this.githubAppSyncResult = `Discovery failed: ${err.message || res.statusText}`;
      }
    } catch (e) {
      this.githubAppSyncResult = 'Discovery failed: network error';
    } finally {
      this.githubAppDiscoverLoading = false;
    }
  }

  private async handleGitHubAppSyncPermissions(): Promise<void> {
    this.githubAppSyncLoading = true;
    this.githubAppSyncResult = null;
    try {
      const res = await apiFetch('/api/v1/github-app/sync-permissions', { method: 'POST' });
      if (res.ok) {
        const data = (await res.json()) as { affected_projects?: number; app_permissions?: Record<string, string> };
        const perms = data.app_permissions ? Object.entries(data.app_permissions).map(([k, v]) => `${k}:${v}`).join(', ') : 'none';
        this.githubAppSyncResult = `Permissions synced. App permissions: ${perms}. ${data.affected_projects || 0} project(s) affected.`;
      } else {
        const err = (await res.json().catch(() => ({}))) as { message?: string };
        this.githubAppSyncResult = `Sync failed: ${err.message || res.statusText}`;
      }
    } catch (e) {
      this.githubAppSyncResult = 'Sync failed: network error';
    } finally {
      this.githubAppSyncLoading = false;
    }
  }

  private async handleSaveGitHubApp(): Promise<void> {
    this.githubAppSaving = true;
    this.githubAppError = null;
    this.githubAppSuccess = null;

    try {
      const payload: Record<string, unknown> = {
        app_id: this.githubAppId || undefined,
        api_base_url: this.githubAppApiBaseUrl || undefined,
        webhooks_enabled: this.githubAppWebhooksEnabled,
        installation_url: this.githubAppInstallationUrl || undefined,
      };

      // Only send secrets if the user provided new values
      if (this.githubAppPrivateKey.trim()) {
        payload.private_key = this.githubAppPrivateKey.trim();
      }
      if (this.githubAppWebhookSecret.trim()) {
        payload.webhook_secret = this.githubAppWebhookSecret.trim();
      }

      const res = await apiFetch('/api/v1/github-app', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });

      if (!res.ok) {
        const err = await extractApiError(res, 'Failed to save GitHub App configuration');
        this.githubAppError = err;
        return;
      }

      this.githubAppSuccess = 'GitHub App configuration saved successfully.';
      // Update rawConfig so the main Save & Reload preserves current values
      if (this.rawConfig) {
        if (!this.rawConfig.server) {
          this.rawConfig.server = {};
        }
        const ghApp: V1GitHubAppConfig = { webhooks_enabled: this.githubAppWebhooksEnabled };
        if (this.githubAppId) ghApp.app_id = this.githubAppId;
        if (this.githubAppApiBaseUrl) ghApp.api_base_url = this.githubAppApiBaseUrl;
        if (this.githubAppInstallationUrl) ghApp.installation_url = this.githubAppInstallationUrl;
        this.rawConfig.server.github_app = ghApp;
      }
      // Reload to get fresh state (has_private_key, has_webhook_secret, configured)
      await this.loadGitHubAppConfig();
      // Reload installations if now configured
      if (this.githubAppConfigured) {
        await this.loadGitHubAppInstallations();
      }
    } catch {
      this.githubAppError = 'Failed to save GitHub App configuration';
    } finally {
      this.githubAppSaving = false;
    }
  }

  // ── Update check ──

  private async checkForUpdates(): Promise<void> {
    this.updateCheckLoading = true;
    this.updateCheckError = null;
    this.updateCheckResult = null;
    try {
      const res = await apiFetch('/api/v1/admin/maintenance/check-updates', {
        method: 'POST',
      });
      if (!res.ok) {
        this.updateCheckError = await extractApiError(res, 'Failed to check for updates');
        return;
      }
      this.updateCheckResult = (await res.json()) as UpdateCheckResult;
    } catch {
      this.updateCheckError = 'Failed to connect to server';
    } finally {
      this.updateCheckLoading = false;
    }
  }

  private renderUpdateConfirmDialog() {
    return html`
      <sl-dialog
        label="Update Server"
        open
        @sl-request-close=${() => (this.showUpdateConfirm = false)}
      >
        <div>
          <p>
            This will pull the latest code, rebuild the server, and
            <strong>restart the service</strong>. You will temporarily lose
            connectivity.
          </p>
          <p>
            Running agent containers are not affected and will continue
            working through the restart.
          </p>
          ${this.updateCheckResult
            ? html`<p>
                <strong>${this.updateCheckResult.commits_behind}</strong> new
                commit${this.updateCheckResult.commits_behind === 1 ? '' : 's'}
                will be applied.
              </p>`
            : nothing}
        </div>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => (this.showUpdateConfirm = false)}
          ?disabled=${this.updateRunning}
        >Cancel</sl-button>
        <sl-button
          slot="footer"
          variant="warning"
          ?loading=${this.updateRunning}
          @click=${() => this.triggerUpdate()}
        >
          <sl-icon slot="prefix" name="download"></sl-icon>
          Update &amp; Restart
        </sl-button>
      </sl-dialog>
    `;
  }

  private async triggerUpdate(): Promise<void> {
    this.updateRunning = true;
    try {
      const res = await apiFetch(
        '/api/v1/admin/maintenance/operations/rebuild-server/run',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ params: {} }),
        },
      );
      if (!res.ok) {
        const errMsg = await extractApiError(res, `HTTP ${res.status}`);
        this.updateCheckError = errMsg;
        return;
      }
      // Server will restart — the page will lose connectivity.
      this.showUpdateConfirm = false;
      this.updateCheckResult = null;
      this.successMessage = 'Update started. The server will restart shortly.';
    } catch {
      this.updateCheckError = 'Failed to trigger update';
    } finally {
      this.updateRunning = false;
    }
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-server-config': ScionPageAdminServerConfig;
  }
}
