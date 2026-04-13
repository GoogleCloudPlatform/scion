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
 * Advanced agent configuration page.
 *
 * Presents a tabbed form for editing the full ScionConfig of an agent
 * that is in the 'created' phase (provisioned but not yet started).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import type { Agent, CapabilityField, GCPIdentityConfig, GCPServiceAccount, HarnessAdvancedCapabilities } from '../../shared/types.js';
import type { EnvEntry } from '../shared/env-editor.js';
import '../shared/env-editor.js';

interface ScionConfigPayload {
  image?: string;
  model?: string;
  user?: string;
  auth_selectedType?: string;
  task?: string;
  system_prompt?: string;
  agent_instructions?: string;
  branch?: string;
  max_turns?: number;
  max_model_calls?: number;
  max_duration?: string;
  resources?: {
    requests?: { cpu?: string; memory?: string };
    limits?: { cpu?: string; memory?: string };
    disk?: string;
  };
  env?: Record<string, string>;
  telemetry?: { enabled?: boolean };
}

interface AppliedConfig {
  image?: string;
  model?: string;
  harnessConfig?: string;
  harnessAuth?: string;
  task?: string;
  env?: Record<string, string>;
  gcpIdentity?: GCPIdentityConfig;
  inlineConfig?: ScionConfigPayload & {
    harness_config?: string;
  };
}

interface AgentWithConfig extends Agent {
  appliedConfig?: AppliedConfig;
}

@customElement('scion-page-agent-configure')
export class ScionPageAgentConfigure extends LitElement {
  @state() private agent: AgentWithConfig | null = null;
  @state() private loading = true;
  @state() private saving = false;
  @state() private starting = false;
  @state() private error: string | null = null;
  @state() private successMessage: string | null = null;
  @state() private showDeleteDialog = false;

  // Form fields — General
  @state() private model = '';
  @state() private image = '';
  @state() private branch = '';
  @state() private containerUser = '';
  @state() private authMethod = '';
  @state() private harnessConfig = '';
  @state() private telemetryEnabled = false;

  // Form fields — Task & Prompts
  @state() private task = '';
  @state() private systemPrompt = '';
  @state() private agentInstructions = '';

  // Form fields — Limits & Resources
  @state() private maxTurns = 0;
  @state() private maxModelCalls = 0;
  @state() private maxDuration = '';
  @state() private cpuRequest = '';
  @state() private memoryRequest = '';
  @state() private cpuLimit = '';
  @state() private memoryLimit = '';
  @state() private disk = '';

  // Form fields — Environment
  @state() private envEntries: EnvEntry[] = [];
  @state() private requiredEnvKeys: string[] = [];

  // Form fields — GCP Identity
  @state() private gcpMetadataMode: 'block' | 'passthrough' | 'assign' = 'block';
  @state() private gcpServiceAccountId = '';
  @state() private gcpServiceAccounts: GCPServiceAccount[] = [];

  private agentId = '';

  private get verifiedGCPServiceAccounts(): GCPServiceAccount[] {
    return this.gcpServiceAccounts.filter((sa) => sa.verified);
  }

  private async loadGCPServiceAccounts(groveId: string): Promise<void> {
    this.gcpServiceAccounts = [];
    try {
      const res = await apiFetch(`/api/v1/groves/${groveId}/gcp-service-accounts`);
      if (res.ok) {
        const data = (await res.json()) as { items?: GCPServiceAccount[] } | GCPServiceAccount[];
        this.gcpServiceAccounts = Array.isArray(data) ? data : data.items || [];
      }
    } catch {
      // Non-critical — just won't show assign option
    }
  }

  private get harnessCapabilities(): HarnessAdvancedCapabilities | null {
    return this.agent?.harnessCapabilities || null;
  }

  private supportReason(field?: CapabilityField): string {
    if (field?.reason) return field.reason;
    return 'Unsupported for the current harness.';
  }

  private isUnsupported(field?: CapabilityField): boolean {
    return field?.support === 'no';
  }

  private authFieldForMethod(method: string): CapabilityField | null {
    const authCaps = this.harnessCapabilities?.auth;
    if (!authCaps) return null;
    switch (method) {
      case 'api-key':
        return authCaps.api_key;
      case 'oauth-token':
        return authCaps.oauth_token;
      case 'auth-file':
        return authCaps.auth_file;
      case 'vertex-ai':
        return authCaps.vertex_ai;
      default:
        return null;
    }
  }

  private authMethodSupported(method: string): boolean {
    const field = this.authFieldForMethod(method);
    return field ? field.support !== 'no' : true;
  }

  static override styles = css`
    :host {
      display: block;
    }

    .back-link {
      display: inline-flex;
      align-items: center;
      gap: 0.5rem;
      color: var(--scion-text-muted, #64748b);
      text-decoration: none;
      font-size: 0.875rem;
      margin-bottom: 1rem;
    }

    .back-link:hover {
      color: var(--scion-primary, #3b82f6);
    }

    .page-header {
      margin-bottom: 1.5rem;
    }

    .page-header h1 {
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
      display: flex;
      align-items: center;
      gap: 0.75rem;
    }

    .page-header h1 sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.5rem;
    }

    .page-header .subtitle {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      font-size: 0.875rem;
    }

    .form-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      max-width: 720px;
    }

    .form-field {
      margin-bottom: 1.25rem;
    }

    .form-field label {
      display: block;
      font-size: 0.875rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin-bottom: 0.375rem;
    }

    .form-field .hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
    }

    .form-field sl-input,
    .form-field sl-select,
    .form-field sl-textarea {
      width: 100%;
    }

    .form-field sl-select::part(combobox) {
      cursor: pointer;
    }

    .form-field sl-select::part(expand-icon) {
      font-size: 1.25rem;
      color: var(--scion-text-secondary, #475569);
      border-left: 1px solid var(--scion-border, #e2e8f0);
      padding: 0 0.625rem;
      margin-left: 0.5rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      border-radius: 0 var(--scion-radius, 0.5rem) var(--scion-radius, 0.5rem) 0;
    }

    .notify-field {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      margin-bottom: 1.25rem;
    }

    .notify-field sl-checkbox::part(label) {
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .help-badge {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 18px;
      height: 18px;
      border-radius: 50%;
      background: var(--scion-text-muted, #64748b);
      color: var(--scion-surface, #ffffff);
      font-size: 0.6875rem;
      font-weight: 700;
      cursor: help;
      flex-shrink: 0;
    }

    .form-actions {
      display: flex;
      gap: 0.75rem;
      align-items: center;
      margin-top: 1.5rem;
      padding-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .form-actions .spacer {
      flex: 1;
    }

    .error-banner {
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-danger-700, #b91c1c);
      font-size: 0.875rem;
    }

    .error-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
    }

    .success-banner {
      background: var(--sl-color-success-50, #f0fdf4);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-success-700, #15803d);
      font-size: 0.875rem;
    }

    .success-banner sl-icon {
      flex-shrink: 0;
      margin-top: 0.125rem;
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

    .field-row {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
    }

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(body) {
      padding-top: 1.25rem;
    }
  `;

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('error') && this.error) {
      this.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }

  override connectedCallback(): void {
    super.connectedCallback();
    if (typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/agents\/([^/]+)\/configure/);
      if (match) {
        this.agentId = match[1];
      }
    }
    void this.loadAgent();
  }

  private globalTelemetryDefault = false;

  private async loadAgent(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [agentRes, settingsRes] = await Promise.all([
        apiFetch(`/api/v1/agents/${this.agentId}`),
        apiFetch('/api/v1/settings/public'),
      ]);

      if (settingsRes.ok) {
        const data = (await settingsRes.json()) as { telemetryEnabled?: boolean };
        this.globalTelemetryDefault = data.telemetryEnabled ?? false;
      }

      if (!agentRes.ok) {
        throw new Error(await extractApiError(agentRes, `HTTP ${agentRes.status}`));
      }

      this.agent = (await agentRes.json()) as AgentWithConfig;

      if (this.agent.phase !== 'created') {
        this.error = `This agent is in "${this.agent.phase}" phase and cannot be configured. Only agents in "created" phase can be edited.`;
        return;
      }

      void this.loadGCPServiceAccounts(this.agent.groveId);
      this.populateForm();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load agent';
    } finally {
      this.loading = false;
    }
  }

  private populateForm(): void {
    if (!this.agent) return;
    const ac = this.agent.appliedConfig;
    const ic = ac?.inlineConfig;

    // General
    this.model = ac?.model || ic?.model || '';
    this.image = ac?.image || ic?.image || '';
    this.branch = ic?.branch || '';
    this.containerUser = ic?.user || '';
    this.authMethod = ac?.harnessAuth || ic?.auth_selectedType || '';
    this.harnessConfig = ac?.harnessConfig || ic?.harness_config || '';
    this.telemetryEnabled = ic?.telemetry?.enabled ?? this.globalTelemetryDefault;

    // Task & Prompts
    this.task = ac?.task || ic?.task || '';
    this.systemPrompt = ic?.system_prompt || '';
    this.agentInstructions = ic?.agent_instructions || '';

    // Limits & Resources
    this.maxTurns = ic?.max_turns || 0;
    this.maxModelCalls = ic?.max_model_calls || 0;
    this.maxDuration = ic?.max_duration || '';
    this.cpuRequest = ic?.resources?.requests?.cpu || '';
    this.memoryRequest = ic?.resources?.requests?.memory || '';
    this.cpuLimit = ic?.resources?.limits?.cpu || '';
    this.memoryLimit = ic?.resources?.limits?.memory || '';
    this.disk = ic?.resources?.disk || '';

    // Environment
    const env = ac?.env || ic?.env || {};
    this.envEntries = Object.entries(env).map(([key, value]) => ({ key, value }));

    // Detect required keys that are empty (from env gathering)
    this.requiredEnvKeys = this.envEntries
      .filter((e) => e.key && !e.value)
      .map((e) => e.key);

    // GCP Identity
    const gcpId = ac?.gcpIdentity;
    this.gcpMetadataMode = (gcpId?.metadataMode as 'block' | 'passthrough' | 'assign') || 'block';
    this.gcpServiceAccountId = gcpId?.serviceAccountId || '';
  }

  private buildConfig(): ScionConfigPayload {
    const config: ScionConfigPayload = {};
    const caps = this.harnessCapabilities;

    if (this.model) config.model = this.model;
    if (this.image) config.image = this.image;
    if (this.branch) config.branch = this.branch;
    if (this.containerUser) config.user = this.containerUser;
    if (this.authMethod && this.authMethodSupported(this.authMethod)) config.auth_selectedType = this.authMethod;
    if (this.task) config.task = this.task;
    if (this.systemPrompt && !this.isUnsupported(caps?.prompts.system_prompt)) config.system_prompt = this.systemPrompt;
    if (this.agentInstructions) config.agent_instructions = this.agentInstructions;
    if (this.maxTurns && !this.isUnsupported(caps?.limits.max_turns)) config.max_turns = this.maxTurns;
    if (this.maxModelCalls && !this.isUnsupported(caps?.limits.max_model_calls)) config.max_model_calls = this.maxModelCalls;
    if (this.maxDuration && !this.isUnsupported(caps?.limits.max_duration)) config.max_duration = this.maxDuration;

    // Resources
    const hasResources =
      this.cpuRequest || this.memoryRequest || this.cpuLimit || this.memoryLimit || this.disk;
    if (hasResources) {
      config.resources = {};
      if (this.cpuRequest || this.memoryRequest) {
        config.resources.requests = {};
        if (this.cpuRequest) config.resources.requests.cpu = this.cpuRequest;
        if (this.memoryRequest) config.resources.requests.memory = this.memoryRequest;
      }
      if (this.cpuLimit || this.memoryLimit) {
        config.resources.limits = {};
        if (this.cpuLimit) config.resources.limits.cpu = this.cpuLimit;
        if (this.memoryLimit) config.resources.limits.memory = this.memoryLimit;
      }
      if (this.disk) config.resources.disk = this.disk;
    }

    // Env
    const env: Record<string, string> = {};
    for (const entry of this.envEntries) {
      if (entry.key) {
        env[entry.key] = entry.value;
      }
    }
    if (Object.keys(env).length > 0) {
      config.env = env;
    }

    // Telemetry
    if (!this.isUnsupported(caps?.telemetry.enabled)) {
      config.telemetry = { enabled: this.telemetryEnabled };
    }

    return config;
  }

  private validateRequiredEnv(): string[] {
    return this.requiredEnvKeys.filter((key) => {
      const entry = this.envEntries.find((e) => e.key === key);
      return !entry?.value;
    });
  }

  private buildGCPIdentityPayload(): Record<string, unknown> | null {
    if (this.gcpMetadataMode === 'assign') {
      if (!this.gcpServiceAccountId) return null;
      return { metadata_mode: 'assign', service_account_id: this.gcpServiceAccountId };
    }
    if (this.gcpMetadataMode === 'passthrough') {
      return { metadata_mode: 'passthrough' };
    }
    return { metadata_mode: 'block' };
  }

  private async handleSave(): Promise<void> {
    this.saving = true;
    this.error = null;
    this.successMessage = null;

    if (this.gcpMetadataMode === 'assign' && !this.gcpServiceAccountId) {
      this.error = 'Please select a service account for GCP identity assignment.';
      this.saving = false;
      return;
    }

    try {
      const config = this.buildConfig();
      const body: Record<string, unknown> = { config };
      const gcpIdentity = this.buildGCPIdentityPayload();
      if (gcpIdentity) body.gcp_identity = gcpIdentity;
      const res = await apiFetch(`/api/v1/agents/${this.agentId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      this.successMessage = 'Configuration saved successfully.';
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to save configuration';
    } finally {
      this.saving = false;
    }
  }

  private async handleStart(): Promise<void> {
    // Validate required env vars
    const missingKeys = this.validateRequiredEnv();
    if (missingKeys.length > 0) {
      this.error = `Missing required environment variables: ${missingKeys.join(', ')}. Please fill them in the Environment tab.`;
      // Activate the Environment tab
      const tabGroup = this.shadowRoot?.querySelector('sl-tab-group') as HTMLElement & {
        show?: (name: string) => void;
      };
      tabGroup?.show?.('environment');
      return;
    }

    this.starting = true;
    this.error = null;
    this.successMessage = null;

    if (this.gcpMetadataMode === 'assign' && !this.gcpServiceAccountId) {
      this.error = 'Please select a service account for GCP identity assignment.';
      this.starting = false;
      return;
    }

    try {
      // Save config first
      const config = this.buildConfig();
      const saveBody: Record<string, unknown> = { config };
      const gcpIdentity = this.buildGCPIdentityPayload();
      if (gcpIdentity) saveBody.gcp_identity = gcpIdentity;
      const saveRes = await apiFetch(`/api/v1/agents/${this.agentId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(saveBody),
      });

      if (!saveRes.ok) {
        throw new Error(await extractApiError(saveRes, `HTTP ${saveRes.status}`));
      }

      // Then start
      const startRes = await apiFetch(`/api/v1/agents/${this.agentId}/start`, {
        method: 'POST',
      });

      if (!startRes.ok) {
        throw new Error(await extractApiError(startRes, 'Failed to start agent'));
      }

      // Navigate to agent detail
      window.history.pushState({}, '', `/agents/${this.agentId}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to start agent';
    } finally {
      this.starting = false;
    }
  }

  private async handleDelete(): Promise<void> {
    this.showDeleteDialog = false;
    this.error = null;

    try {
      const res = await apiFetch(`/api/v1/agents/${this.agentId}`, {
        method: 'DELETE',
      });

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      window.history.pushState({}, '', '/agents');
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to delete agent';
    }
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading agent configuration...</p>
        </div>
      `;
    }

    if (!this.agent || (this.error && this.agent?.phase !== 'created')) {
      return html`
        <a href="/agents" class="back-link">
          <sl-icon name="arrow-left"></sl-icon>
          Back to Agents
        </a>
        <div class="form-card">
          <div class="error-banner">
            <sl-icon name="exclamation-triangle"></sl-icon>
            <span>${this.error || 'Agent not found'}</span>
          </div>
          <sl-button variant="default" @click=${() => {
            window.history.pushState({}, '', '/agents');
            window.dispatchEvent(new PopStateEvent('popstate'));
          }}>
            Back to Agents
          </sl-button>
        </div>
      `;
    }

    const isBusy = this.saving || this.starting;

    return html`
      <a href="/agents" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Agents
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="sliders"></sl-icon>
          Configure Agent: ${this.agent.name}
        </h1>
        <p class="subtitle">Status: Created (not started)</p>
      </div>

      <div class="form-card">
        ${this.error
          ? html`
              <div class="error-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.error}</span>
              </div>
            `
          : ''}
        ${this.successMessage
          ? html`
              <div class="success-banner">
                <sl-icon name="check-circle"></sl-icon>
                <span>${this.successMessage}</span>
              </div>
            `
          : ''}

        <sl-tab-group>
          <sl-tab slot="nav" panel="general">General</sl-tab>
          <sl-tab slot="nav" panel="task">Task &amp; Prompts</sl-tab>
          <sl-tab slot="nav" panel="limits">Limits &amp; Resources</sl-tab>
          <sl-tab slot="nav" panel="environment">Environment</sl-tab>

          <sl-tab-panel name="general">${this.renderGeneralTab()}</sl-tab-panel>
          <sl-tab-panel name="task">${this.renderTaskTab()}</sl-tab-panel>
          <sl-tab-panel name="limits">${this.renderLimitsTab()}</sl-tab-panel>
          <sl-tab-panel name="environment">${this.renderEnvironmentTab()}</sl-tab-panel>
        </sl-tab-group>

        <div class="form-actions">
          <sl-button
            variant="default"
            ?disabled=${isBusy}
            @click=${() => {
              const url = new URL('/agents/new', window.location.origin);
              url.searchParams.set('editingAgentId', this.agentId);
              window.history.pushState({}, '', url.toString());
              window.dispatchEvent(new PopStateEvent('popstate'));
            }}
          >
            <sl-icon slot="prefix" name="arrow-left"></sl-icon>
            Back
          </sl-button>
          <sl-button
            variant="default"
            ?loading=${this.saving}
            ?disabled=${isBusy}
            @click=${() => this.handleSave()}
          >
            Save
          </sl-button>
          <sl-button
            variant="primary"
            ?loading=${this.starting}
            ?disabled=${isBusy}
            @click=${() => this.handleStart()}
          >
            <sl-icon slot="prefix" name="play-circle"></sl-icon>
            Start
          </sl-button>
          <span class="spacer"></span>
          <sl-button
            variant="danger"
            outline
            ?disabled=${isBusy}
            @click=${() => { this.showDeleteDialog = true; }}
          >
            <sl-icon slot="prefix" name="trash"></sl-icon>
            Delete
          </sl-button>
        </div>
      </div>

      <sl-dialog
        label="Delete Agent"
        ?open=${this.showDeleteDialog}
        @sl-request-close=${() => { this.showDeleteDialog = false; }}
      >
        <p>Are you sure you want to delete agent <strong>${this.agent.name}</strong>? This action cannot be undone.</p>
        <sl-button slot="footer" variant="default" @click=${() => { this.showDeleteDialog = false; }}>
          Cancel
        </sl-button>
        <sl-button slot="footer" variant="danger" @click=${() => this.handleDelete()}>
          Delete
        </sl-button>
      </sl-dialog>
    `;
  }

  private renderGeneralTab() {
    const authFileCap = this.harnessCapabilities?.auth.auth_file;
    const oauthTokenCap = this.harnessCapabilities?.auth.oauth_token;
    const vertexCap = this.harnessCapabilities?.auth.vertex_ai;
    const telemetryCap = this.harnessCapabilities?.telemetry.enabled;
    const selectedAuthCap = this.authFieldForMethod(this.authMethod);

    return html`
      <div class="form-field">
        <label>Model</label>
        <sl-input
          placeholder="e.g. claude-opus-4-6, gemini-2.5-pro"
          .value=${this.model}
          @sl-input=${(e: Event) => { this.model = (e.target as HTMLElement & { value: string }).value; }}
        ></sl-input>
      </div>

      <div class="form-field">
        <label>Image</label>
        <sl-input
          placeholder="Container image override"
          .value=${this.image}
          @sl-input=${(e: Event) => { this.image = (e.target as HTMLElement & { value: string }).value; }}
        ></sl-input>
      </div>

      <div class="form-field">
        <label>Branch</label>
        <sl-input
          placeholder="Git branch for the agent"
          .value=${this.branch}
          @sl-input=${(e: Event) => { this.branch = (e.target as HTMLElement & { value: string }).value; }}
        ></sl-input>
      </div>

      <div class="form-field">
        <label>Container User</label>
        <sl-input
          placeholder="Unix user inside container"
          .value=${this.containerUser}
          @sl-input=${(e: Event) => { this.containerUser = (e.target as HTMLElement & { value: string }).value; }}
        ></sl-input>
      </div>

      <div class="form-field">
        <label>Auth Method</label>
        <sl-select
          placeholder="Select auth method..."
          .value=${this.authMethod}
          @sl-change=${(e: Event) => { this.authMethod = (e.target as HTMLElement & { value: string }).value; }}
        >
          <sl-option value="">Auto Detected</sl-option>
          <sl-option value="api-key">Provider API Key</sl-option>
          <sl-option value="oauth-token" ?disabled=${this.isUnsupported(oauthTokenCap)}>OAuth Token (env var)</sl-option>
          <sl-option value="vertex-ai" ?disabled=${this.isUnsupported(vertexCap)}>Vertex Model Garden</sl-option>
          <sl-option value="auth-file" ?disabled=${this.isUnsupported(authFileCap)}>Harness credential file</sl-option>
        </sl-select>
        ${this.authMethod && this.isUnsupported(selectedAuthCap || undefined)
          ? html`<div class="hint">${this.supportReason(selectedAuthCap || undefined)}</div>`
          : nothing}
      </div>

      ${this.harnessConfig
        ? html`
            <div class="form-field">
              <label>Harness Config</label>
              <sl-input .value=${this.harnessConfig} readonly></sl-input>
              <div class="hint">Set at creation time and cannot be changed.</div>
            </div>
          `
        : nothing}

      <div class="form-field">
        <label for="gcp-mode">GCP Identity</label>
        <sl-select
          id="gcp-mode"
          .value=${this.gcpMetadataMode}
          @sl-change=${(e: Event) => {
            this.gcpMetadataMode = (e.target as HTMLElement & { value: string }).value as
              | 'block'
              | 'passthrough'
              | 'assign';
            if (this.gcpMetadataMode !== 'assign') {
              this.gcpServiceAccountId = '';
            }
          }}
        >
          <sl-option value="block">Block</sl-option>
          ${this.gcpServiceAccounts.length > 0
            ? html`<sl-option value="assign">Assign Service Account</sl-option>`
            : nothing}
          <sl-option value="passthrough">Passthrough</sl-option>
        </sl-select>
        <div class="hint">
          ${this.gcpMetadataMode === 'block'
            ? 'Prevents the agent from accessing any GCP identity. Token requests are denied.'
            : this.gcpMetadataMode === 'assign'
              ? 'Assigns a registered GCP service account. GCP client libraries will authenticate automatically.'
              : 'No metadata interception. The agent inherits the broker\'s GCP identity. Requires broker ownership.'}
        </div>
      </div>

      ${this.gcpMetadataMode === 'assign'
        ? html`
            <div class="form-field">
              <label for="gcp-sa">Service Account</label>
              ${this.verifiedGCPServiceAccounts.length > 0
                ? html`
                    <sl-select
                      id="gcp-sa"
                      placeholder="Select a service account..."
                      .value=${this.gcpServiceAccountId}
                      @sl-change=${(e: Event) => {
                        this.gcpServiceAccountId = (e.target as HTMLElement & { value: string }).value;
                      }}
                    >
                      ${this.verifiedGCPServiceAccounts.map(
                        (sa) =>
                          html`<sl-option value=${sa.id}>
                            ${sa.email}${sa.displayName ? ` (${sa.displayName})` : ''}
                          </sl-option>`
                      )}
                    </sl-select>
                  `
                : html`
                    <div class="hint" style="margin-top: 0;">
                      No verified service accounts available. Register and verify service accounts in grove settings.
                    </div>
                  `}
            </div>
          `
        : nothing}

      <div class="notify-field">
        ${this.isUnsupported(telemetryCap)
          ? html`
              <sl-tooltip content=${this.supportReason(telemetryCap)} hoist>
                <sl-checkbox
                  ?checked=${this.telemetryEnabled}
                  ?disabled=${true}
                >
                  Enable Telemetry
                </sl-checkbox>
              </sl-tooltip>
            `
          : html`
              <sl-checkbox
                ?checked=${this.telemetryEnabled}
                @sl-change=${(e: Event) => { this.telemetryEnabled = (e.target as HTMLInputElement).checked; }}
              >
                Enable Telemetry
              </sl-checkbox>
            `}
        <sl-tooltip
          content="Collect telemetry data for this agent. The default reflects the global telemetry setting."
          hoist
        >
          <span class="help-badge">?</span>
        </sl-tooltip>
      </div>
    `;
  }

  private renderTaskTab() {
    const systemPromptCap = this.harnessCapabilities?.prompts.system_prompt;

    return html`
      <div class="form-field">
        <label>Task</label>
        <sl-textarea
          placeholder="Describe what this agent should work on..."
          .value=${this.task}
          @sl-input=${(e: Event) => { this.task = (e.target as HTMLElement & { value: string }).value; }}
          rows="8"
          resize="auto"
        ></sl-textarea>
        <div class="hint">The initial task or prompt for the agent.</div>
      </div>

      <div class="form-field">
        <label>System Prompt</label>
        ${this.isUnsupported(systemPromptCap)
          ? html`
              <sl-tooltip content=${this.supportReason(systemPromptCap)} hoist>
                <sl-textarea
                  placeholder="System prompt content or file:// URI..."
                  .value=${this.systemPrompt}
                  rows="8"
                  resize="auto"
                  ?disabled=${true}
                ></sl-textarea>
              </sl-tooltip>
            `
          : html`
              <sl-textarea
                placeholder="System prompt content or file:// URI..."
                .value=${this.systemPrompt}
                @sl-input=${(e: Event) => { this.systemPrompt = (e.target as HTMLElement & { value: string }).value; }}
                rows="8"
                resize="auto"
              ></sl-textarea>
            `}
        ${systemPromptCap?.support === 'partial'
          ? html`<div class="hint">${this.supportReason(systemPromptCap)}</div>`
          : nothing}
      </div>

      <div class="form-field">
        <label>Agent Instructions</label>
        <sl-textarea
          placeholder="Agent instructions content or file:// URI..."
          .value=${this.agentInstructions}
          @sl-input=${(e: Event) => { this.agentInstructions = (e.target as HTMLElement & { value: string }).value; }}
          rows="8"
          resize="auto"
        ></sl-textarea>
      </div>
    `;
  }

  private renderLimitsTab() {
    const maxTurnsCap = this.harnessCapabilities?.limits.max_turns;
    const maxModelCallsCap = this.harnessCapabilities?.limits.max_model_calls;
    const maxDurationCap = this.harnessCapabilities?.limits.max_duration;

    return html`
      <div class="field-row">
        <div class="form-field">
          <label>Max Turns</label>
          ${this.isUnsupported(maxTurnsCap)
            ? html`
                <sl-tooltip content=${this.supportReason(maxTurnsCap)} hoist>
                  <sl-input
                    type="number"
                    placeholder="0 = unlimited"
                    .value=${String(this.maxTurns || '')}
                    ?disabled=${true}
                  ></sl-input>
                </sl-tooltip>
              `
            : html`
                <sl-input
                  type="number"
                  placeholder="0 = unlimited"
                  .value=${String(this.maxTurns || '')}
                  @sl-input=${(e: Event) => { this.maxTurns = parseInt((e.target as HTMLElement & { value: string }).value) || 0; }}
                ></sl-input>
              `}
        </div>
        <div class="form-field">
          <label>Max Model Calls</label>
          ${this.isUnsupported(maxModelCallsCap)
            ? html`
                <sl-tooltip content=${this.supportReason(maxModelCallsCap)} hoist>
                  <sl-input
                    type="number"
                    placeholder="0 = unlimited"
                    .value=${String(this.maxModelCalls || '')}
                    ?disabled=${true}
                  ></sl-input>
                </sl-tooltip>
              `
            : html`
                <sl-input
                  type="number"
                  placeholder="0 = unlimited"
                  .value=${String(this.maxModelCalls || '')}
                  @sl-input=${(e: Event) => { this.maxModelCalls = parseInt((e.target as HTMLElement & { value: string }).value) || 0; }}
                ></sl-input>
              `}
        </div>
      </div>

      <div class="form-field">
        <label>Max Duration</label>
        ${this.isUnsupported(maxDurationCap)
          ? html`
              <sl-tooltip content=${this.supportReason(maxDurationCap)} hoist>
                <sl-input
                  placeholder="e.g. 30m, 2h"
                  .value=${this.maxDuration}
                  ?disabled=${true}
                ></sl-input>
              </sl-tooltip>
            `
          : html`
              <sl-input
                placeholder="e.g. 30m, 2h"
                .value=${this.maxDuration}
                @sl-input=${(e: Event) => { this.maxDuration = (e.target as HTMLElement & { value: string }).value; }}
              ></sl-input>
            `}
        <div class="hint">Go duration string. Empty means no limit.</div>
      </div>

      <div class="field-row">
        <div class="form-field">
          <label>CPU Request</label>
          <sl-input
            placeholder='e.g. "2", "500m"'
            .value=${this.cpuRequest}
            @sl-input=${(e: Event) => { this.cpuRequest = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>
        <div class="form-field">
          <label>Memory Request</label>
          <sl-input
            placeholder='e.g. "4Gi"'
            .value=${this.memoryRequest}
            @sl-input=${(e: Event) => { this.memoryRequest = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>
      </div>

      <div class="field-row">
        <div class="form-field">
          <label>CPU Limit</label>
          <sl-input
            placeholder='e.g. "4"'
            .value=${this.cpuLimit}
            @sl-input=${(e: Event) => { this.cpuLimit = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>
        <div class="form-field">
          <label>Memory Limit</label>
          <sl-input
            placeholder='e.g. "8Gi"'
            .value=${this.memoryLimit}
            @sl-input=${(e: Event) => { this.memoryLimit = (e.target as HTMLElement & { value: string }).value; }}
          ></sl-input>
        </div>
      </div>

      <div class="form-field">
        <label>Disk</label>
        <sl-input
          placeholder='e.g. "20Gi"'
          .value=${this.disk}
          @sl-input=${(e: Event) => { this.disk = (e.target as HTMLElement & { value: string }).value; }}
        ></sl-input>
      </div>
    `;
  }

  private renderEnvironmentTab() {
    return html`
      <scion-env-editor
        .entries=${this.envEntries}
        .requiredKeys=${this.requiredEnvKeys}
        @env-change=${(e: CustomEvent<{ entries: EnvEntry[] }>) => {
          this.envEntries = e.detail.entries;
        }}
      ></scion-env-editor>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-configure': ScionPageAgentConfigure;
  }
}
