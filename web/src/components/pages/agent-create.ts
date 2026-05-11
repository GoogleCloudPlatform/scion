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
 * Agent creation page component
 *
 * Form for creating and starting a new agent
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type {
  Agent,
  Project,
  RuntimeBroker,
  Template,
  GCPServiceAccount,
} from '../../shared/types.js';

interface HarnessConfigEntry {
  id: string;
  name: string;
  slug: string;
  displayName?: string;
  harness: string;
  scope: string;
}
import { isSharedWorkspace } from '../../shared/types.js';
import { apiFetch, parseApiError } from '../../client/api.js';
import '../shared/status-badge.js';

@customElement('scion-page-agent-create')
export class ScionPageAgentCreate extends LitElement {
  @state()
  private projects: Project[] = [];

  @state()
  private brokers: RuntimeBroker[] = [];

  @state()
  private templates: Template[] = [];

  @state()
  private loading = true;

  @state()
  private submitting = false;

  @state()
  private submittingEdit = false;

  @state()
  private error: string | null = null;

  @state()
  private errorLinks: Array<{ label: string; href: string }> = [];

  /** Form field values */
  @state()
  private name = '';

  @state()
  private projectId = '';

  @state()
  private templateId = '';

  @state()
  private harness = 'gemini';

  @state()
  private customHarness = '';

  @state()
  private harnessAuth = '';

  @state()
  private brokerId = '';

  @state()
  private profile = '';

  @state()
  private branch = '';

  @state()
  private task = '';

  @state()
  private notify = true;

  @state()
  private telemetryEnabled = false;

  @state()
  private gcpMetadataMode: 'block' | 'passthrough' | 'assign' = 'block';

  @state()
  private gcpServiceAccountId = '';

  @state()
  private gcpServiceAccounts: GCPServiceAccount[] = [];

  @state()
  private harnessConfigs: HarnessConfigEntry[] = [];

  /** ID of an existing agent we're editing (came back from configure page) */
  private editingAgentId: string | null = null;

  /** Whether the projectId was explicitly passed via URL query param (user navigated from project page) */
  private projectFromUrl = false;

  /** Cached project settings keyed by projectId */
  private projectSettingsCache: Map<
    string,
    {
      defaultTemplate?: string;
      defaultHarnessConfig?: string;
      defaultMaxTurns?: number;
      defaultMaxModelCalls?: number;
      defaultMaxDuration?: string;
      defaultGCPIdentityMode?: string;
      defaultGCPIdentityServiceAccountID?: string;
    }
  > = new Map();

  /** Profiles available on the currently selected broker */
  private get selectedBrokerProfiles(): import('../../shared/types.js').BrokerProfile[] {
    if (!this.brokerId) return [];
    const broker = this.brokers.find((b) => b.id === this.brokerId);
    return broker?.profiles?.filter((p) => p.available) ?? [];
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

    .page-header p {
      color: var(--scion-text-muted, #64748b);
      margin: 0;
      font-size: 0.875rem;
    }

    .form-card {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      max-width: 640px;
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

    .broker-option {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .broker-dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      flex-shrink: 0;
    }

    .broker-dot.online {
      background: var(--sl-color-success-500, #22c55e);
    }

    .broker-dot.offline {
      background: var(--sl-color-neutral-400, #9ca3af);
    }

    .broker-dot.degraded {
      background: var(--sl-color-warning-500, #f59e0b);
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
      margin-top: 1.5rem;
      padding-top: 1.5rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
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

    .error-links a {
      color: inherit;
      font-weight: 600;
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
      const params = new URLSearchParams(window.location.search);

      // Pre-select projectId from URL query param if present
      const projectParam = params.get('projectId');
      if (projectParam) {
        this.projectId = projectParam;
        this.projectFromUrl = true;
      }

      // Check if returning from configure page with an existing agent
      const editingParam = params.get('editingAgentId');
      if (editingParam) {
        this.editingAgentId = editingParam;
      }
    }

    void this.loadFormData();
  }

  /** The currently selected project */
  private get selectedProject(): Project | undefined {
    return this.projects.find((p) => p.id === this.projectId);
  }

  /** The project matching the URL-provided projectId, used for back-navigation */
  private get sourceProject(): Project | undefined {
    if (!this.projectFromUrl) return undefined;
    return this.projects.find((p) => p.id === this.projectId);
  }

  private async loadFormData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [projectsRes, brokersRes, templatesRes, settingsRes, harnessConfigsRes] =
        await Promise.all([
          fetch('/api/v1/projects?mine=true&limit=200', { credentials: 'include' }),
          fetch('/api/v1/runtime-brokers?limit=200', { credentials: 'include' }),
          fetch('/api/v1/templates?status=active&limit=200', { credentials: 'include' }),
          fetch('/api/v1/settings/public', { credentials: 'include' }),
          fetch('/api/v1/harness-configs?status=active&limit=100', { credentials: 'include' }),
        ]);

      if (projectsRes.ok) {
        const data = (await projectsRes.json()) as { projects?: Project[] } | Project[];
        const projects = Array.isArray(data) ? data : data.projects || [];
        this.projects = projects.sort((a, b) => (a.name || '').localeCompare(b.name || ''));
      }

      if (brokersRes.ok) {
        const data = (await brokersRes.json()) as { brokers?: RuntimeBroker[] } | RuntimeBroker[];
        this.brokers = Array.isArray(data) ? data : data.brokers || [];
      }

      if (templatesRes.ok) {
        const data = (await templatesRes.json()) as { templates?: Template[] } | Template[];
        this.templates = Array.isArray(data) ? data : data.templates || [];
      }

      if (settingsRes.ok) {
        const data = (await settingsRes.json()) as { telemetryEnabled?: boolean };
        this.telemetryEnabled = data.telemetryEnabled ?? false;
      }

      if (harnessConfigsRes.ok) {
        const data = (await harnessConfigsRes.json()) as {
          harnessConfigs?: HarnessConfigEntry[];
        };
        this.harnessConfigs = data.harnessConfigs || [];
      }

      // If returning from configure page, populate form from existing agent
      if (this.editingAgentId) {
        await this.populateFromAgent(this.editingAgentId);
      } else {
        // Auto-select first project if none selected
        if (!this.projectId && this.projects.length > 0) {
          this.projectId = this.projects[0].id;
        }

        // Auto-select broker based on project's default
        this.selectBrokerForProject();

        // Auto-select template based on project settings, then fallback
        if (!this.templateId) {
          await this.selectDefaultTemplate();
        }

        // Load GCP service accounts for selected project
        if (this.projectId) {
          await this.loadGCPServiceAccounts();
        }
      }

      // Reload harness configs scoped to the selected project (the initial
      // fetch above has no projectId filter and returns configs from all
      // projects, which causes duplicates in the dropdown).
      if (this.projectId) {
        await this.loadHarnessConfigs();
      }
    } catch (err) {
      console.error('Failed to load form data:', err);
      this.error = 'Failed to load form data. Please try again.';
    } finally {
      this.loading = false;
    }
  }

  private async handleSubmit(_e: Event): Promise<void> {
    // Validate required fields
    if (!this.name.trim()) {
      this.error = 'Agent name is required.';
      return;
    }
    if (!this.projectId) {
      this.error = 'Please select a project.';
      return;
    }

    this.submitting = true;
    this.error = null;
    this.errorLinks = [];

    try {
      // If returning from configure, delete the old agent first
      if (this.editingAgentId) {
        await this.deleteEditingAgent();
        this.editingAgentId = null;
      }

      const body: Record<string, unknown> = {
        name: this.slugify(this.name),
        projectId: this.projectId,
        harnessConfig: this.resolvedHarness,
        notify: this.notify,
      };

      if (this.branch.trim()) {
        body.branch = this.branch.trim();
      }
      if (this.harnessAuth) {
        body.harnessAuth = this.harnessAuth;
      }
      if (this.templateId) {
        body.template = this.templateId;
      }
      if (this.brokerId) {
        body.runtimeBrokerId = this.brokerId;
      }
      if (this.profile) {
        body.profile = this.profile;
      }
      if (this.task.trim()) {
        body.task = this.task.trim();
      }

      // GCP identity assignment
      if (this.gcpMetadataMode === 'assign' && this.gcpServiceAccountId) {
        body.gcp_identity = {
          metadata_mode: 'assign',
          service_account_id: this.gcpServiceAccountId,
        };
      } else if (this.gcpMetadataMode === 'passthrough') {
        body.gcp_identity = {
          metadata_mode: 'passthrough',
        };
      } else if (this.gcpMetadataMode === 'block') {
        body.gcp_identity = {
          metadata_mode: 'block',
        };
      }

      // Pass config options
      const config: Record<string, unknown> = {
        env: {
          SCION_TELEMETRY_ENABLED: this.telemetryEnabled ? 'true' : 'false',
        },
      };
      body.config = config;

      // Validate GCP assign mode
      if (this.gcpMetadataMode === 'assign' && !this.gcpServiceAccountId) {
        this.error = 'Please select a service account for GCP identity assignment.';
        this.submitting = false;
        return;
      }

      const response = await fetch('/api/v1/agents', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        const apiErr = await parseApiError(response, `HTTP ${response.status}`);
        if (apiErr.code === 'missing_env_vars') {
          this.errorLinks = [
            ...(this.projectId ? [{ label: 'Project Settings', href: `/projects/${this.projectId}/settings` }] : []),
            { label: 'Profile Secrets', href: '/profile/secrets' },
          ];
        }
        throw new Error(apiErr.message);
      }

      const result = (await response.json()) as {
        agent?: { id: string; status?: string; phase?: string };
        id?: string;
      };
      const agent = result.agent;
      const agentId = agent?.id || result.id;

      if (!agentId) {
        throw new Error('No agent ID in response');
      }

      // If the backend didn't already start the agent, explicitly start it.
      const startedPhases = ['running', 'provisioning', 'cloning', 'starting'];
      const alreadyStarted = agent?.phase ? startedPhases.includes(agent.phase) : false;
      if (!alreadyStarted) {
        const startResp = await fetch(`/api/v1/agents/${agentId}/start`, {
          method: 'POST',
          credentials: 'include',
        });
        if (!startResp.ok) {
          console.warn('Agent created but failed to start:', startResp.status);
        }
      }

      // Navigate to agent detail page
      window.history.pushState({}, '', `/agents/${agentId}`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      console.error('Failed to create agent:', err);
      this.error = err instanceof Error ? err.message : 'Failed to create agent';
    } finally {
      this.submitting = false;
    }
  }

  private async handleCreateAndEdit(_e: Event): Promise<void> {
    if (!this.name.trim()) {
      this.error = 'Agent name is required.';
      return;
    }
    if (!this.projectId) {
      this.error = 'Please select a project.';
      return;
    }

    this.submittingEdit = true;
    this.error = null;
    this.errorLinks = [];

    try {
      // If returning from configure, delete the old agent first
      if (this.editingAgentId) {
        await this.deleteEditingAgent();
        this.editingAgentId = null;
      }

      const body: Record<string, unknown> = {
        name: this.slugify(this.name),
        projectId: this.projectId,
        harnessConfig: this.resolvedHarness,
        notify: this.notify,
        provisionOnly: true,
      };

      if (this.branch.trim()) {
        body.branch = this.branch.trim();
      }
      if (this.harnessAuth) {
        body.harnessAuth = this.harnessAuth;
      }
      if (this.templateId) {
        body.template = this.templateId;
      }
      if (this.brokerId) {
        body.runtimeBrokerId = this.brokerId;
      }
      if (this.profile) {
        body.profile = this.profile;
      }
      if (this.task.trim()) {
        body.task = this.task.trim();
      }

      // GCP identity assignment
      if (this.gcpMetadataMode === 'assign' && this.gcpServiceAccountId) {
        body.gcp_identity = {
          metadata_mode: 'assign',
          service_account_id: this.gcpServiceAccountId,
        };
      } else if (this.gcpMetadataMode === 'passthrough') {
        body.gcp_identity = {
          metadata_mode: 'passthrough',
        };
      } else if (this.gcpMetadataMode === 'block') {
        body.gcp_identity = {
          metadata_mode: 'block',
        };
      }

      // Validate GCP assign mode
      if (this.gcpMetadataMode === 'assign' && !this.gcpServiceAccountId) {
        this.error = 'Please select a service account for GCP identity assignment.';
        this.submittingEdit = false;
        return;
      }

      const response = await fetch('/api/v1/agents', {
        method: 'POST',
        credentials: 'include',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        const apiErr = await parseApiError(response, `HTTP ${response.status}`);
        if (apiErr.code === 'missing_env_vars') {
          this.errorLinks = [
            ...(this.projectId ? [{ label: 'Project Settings', href: `/projects/${this.projectId}/settings` }] : []),
            { label: 'Profile Secrets', href: '/profile/secrets' },
          ];
        }
        throw new Error(apiErr.message);
      }

      const result = (await response.json()) as {
        agent?: { id: string };
        id?: string;
      };
      const agentId = result.agent?.id || result.id;

      if (!agentId) {
        throw new Error('No agent ID in response');
      }

      // Navigate to the advanced configure page
      window.history.pushState({}, '', `/agents/${agentId}/configure`);
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      console.error('Failed to create agent:', err);
      this.error = err instanceof Error ? err.message : 'Failed to create agent';
    } finally {
      this.submittingEdit = false;
    }
  }
/**
 * Select the best broker for the currently selected project.
 * Prefers the project's default broker; falls back to first online broker.
 */
private selectBrokerForProject(): void {
  const project = this.projects.find((p) => p.id === this.projectId);
  if (project?.defaultRuntimeBrokerId) {
    const defaultBroker = this.brokers.find((b) => b.id === project.defaultRuntimeBrokerId);
    if (defaultBroker) {
      this.brokerId = defaultBroker.id;
      this.autoSelectProfile();
      return;
    }
  }

  // Fallback: first online broker, then first broker
    const onlineBroker = this.brokers.find((b) => b.status === 'online');
    if (onlineBroker) {
      this.brokerId = onlineBroker.id;
    } else if (this.brokers.length > 0) {
      this.brokerId = this.brokers[0].id;
    }
    this.autoSelectProfile();
    }

  /**
   * Returns templates visible to the selected project: project-scoped templates
   * for the current project plus global templates.
   */
  private get filteredTemplates(): Template[] {
    if (!this.projectId) return this.templates;
    return this.templates.filter(
      (t) =>
        t.scope === 'global' ||
        t.scope === 'user' ||
        (t.scope === 'project' && t.scopeId === this.projectId)
    );
  }

  /**
   * Select the default template and harness config for the current project using project settings.
   * Falls back to a template named "default", then the first available template.
   * The harness config is determined by: template defaultHarnessConfig > template harness > project default > 'gemini'.
   */
  private async selectDefaultTemplate(): Promise<void> {
    const visible = this.filteredTemplates;

    // Fetch project settings (used for both template and harness defaults)
    const settings = this.projectId ? await this.fetchProjectSettings(this.projectId) : null;
    const harnessDefault = settings?.defaultHarnessConfig || 'gemini';

    const harnessFor = (t: { defaultHarnessConfig?: string; harness?: string }) =>
      t.defaultHarnessConfig || t.harness || harnessDefault;

    // Try project settings default template first
    if (settings?.defaultTemplate) {
      const match = visible.find(
        (t) => t.name === settings.defaultTemplate || t.slug === settings.defaultTemplate
      );
      if (match) {
        this.templateId = match.id;
        this.setHarnessFromValue(harnessFor(match));
        return;
      }
    }

    // Fallback: template named "default"
    const fallback = visible.find((t) => t.slug === 'default' || t.name === 'default');
    if (fallback) {
      this.templateId = fallback.id;
      this.setHarnessFromValue(harnessFor(fallback));
    } else if (visible.length > 0) {
      this.templateId = visible[0].id;
      this.setHarnessFromValue(harnessFor(visible[0]));
    } else {
      this.templateId = '';
      this.setHarnessFromValue(harnessDefault);
    }
  }

  /**
   * Handle broker selection change: reset and auto-select profile.
   */
  private onBrokerChange(): void {
    this.autoSelectProfile();
  }

  /**
   * Auto-select the profile for the current broker.
   * If only one available profile exists, select it; otherwise clear selection.
   */
  private autoSelectProfile(): void {
    const profiles = this.selectedBrokerProfiles;
    if (profiles.length === 1) {
      this.profile = profiles[0].name;
    } else {
      this.profile = '';
    }
  }

  /**
   * Populate form fields from an existing agent (when returning from configure page).
   */
  private async populateFromAgent(agentId: string): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/agents/${agentId}`);
      if (!res.ok) {
        // Agent may have been deleted; clear editing state and fall back to defaults
        this.editingAgentId = null;
        return;
      }
      const data = (await res.json()) as { agent?: Agent } | Agent;
      const agent: Agent = 'agent' in data && data.agent ? data.agent : (data as Agent);

      this.name = agent.name || '';
      this.projectId = agent.projectId || '';
      if (agent.harnessConfig) this.setHarnessFromValue(agent.harnessConfig);
      if (agent.harnessAuth) this.harnessAuth = agent.harnessAuth;
      if (agent.template) this.templateId = agent.template;
      if (agent.runtimeBrokerId) this.brokerId = agent.runtimeBrokerId;
      if (agent.appliedConfig?.profile) this.profile = agent.appliedConfig.profile;
    } catch {
      // If fetch fails, clear editing state
      this.editingAgentId = null;
    }
  }

  /**
   * Delete the agent being edited (used when cancelling after returning from configure).
   */
  private async deleteEditingAgent(): Promise<void> {
    if (!this.editingAgentId) return;
    try {
      await apiFetch(`/api/v1/agents/${this.editingAgentId}`, { method: 'DELETE' });
    } catch {
      // Best-effort deletion; navigate away regardless
    }
  }

  private async loadHarnessConfigs(): Promise<void> {
    try {
      const url = this.projectId
        ? `/api/v1/harness-configs?status=active&projectId=${encodeURIComponent(this.projectId)}&limit=100`
        : '/api/v1/harness-configs?status=active&limit=100';
      const res = await apiFetch(url);
      if (res.ok) {
        const data = (await res.json()) as { harnessConfigs?: HarnessConfigEntry[] };
        this.harnessConfigs = data.harnessConfigs || [];
      }
    } catch (err) {
      console.error('Failed to load harness configs:', err);
    }
  }

  private async loadGCPServiceAccounts(): Promise<void> {
    this.gcpServiceAccounts = [];
    this.gcpServiceAccountId = '';
    this.gcpMetadataMode = 'block';

    if (!this.projectId) return;

    try {
      const res = await apiFetch(`/api/v1/projects/${this.projectId}/gcp-service-accounts`);
      if (res.ok) {
        const data = (await res.json()) as
          | {
              items?: GCPServiceAccount[];
            }
          | GCPServiceAccount[];
        this.gcpServiceAccounts = Array.isArray(data) ? data : data.items || [];
      }
    } catch {
      // Non-critical — just won't show GCP identity section
    }

    // Apply project default GCP identity if configured
    const settings = await this.fetchProjectSettings(this.projectId);
    if (settings?.defaultGCPIdentityMode) {
      const mode = settings.defaultGCPIdentityMode as 'block' | 'passthrough' | 'assign';
      if (mode === 'assign' && settings.defaultGCPIdentityServiceAccountID) {
        const verified = this.verifiedGCPServiceAccounts;
        const match = verified.find(
          (sa) => sa.id === settings.defaultGCPIdentityServiceAccountID
        );
        if (match) {
          this.gcpMetadataMode = 'assign';
          this.gcpServiceAccountId = match.id;
        }
        // If the configured SA isn't found/verified, fall through to block
      } else if (mode === 'passthrough' || mode === 'block') {
        this.gcpMetadataMode = mode;
      }
    }
  }

  private get verifiedGCPServiceAccounts(): GCPServiceAccount[] {
    return this.gcpServiceAccounts.filter((sa) => sa.verified);
  }

  /**
   * Fetch project settings and return the defaultTemplate value (if any).
   * Results are cached per projectId to avoid redundant requests.
   */
  private async fetchProjectSettings(
    projectId: string
  ): Promise<{
    defaultTemplate?: string;
    defaultHarnessConfig?: string;
    defaultMaxTurns?: number;
    defaultMaxModelCalls?: number;
    defaultMaxDuration?: string;
    defaultGCPIdentityMode?: string;
    defaultGCPIdentityServiceAccountID?: string;
  } | null> {
    if (!projectId) return null;

    const cached = this.projectSettingsCache.get(projectId);
    if (cached !== undefined) return cached;

    try {
      const res = await apiFetch(`/api/v1/projects/${projectId}/settings`);
      if (res.ok) {
        const data = (await res.json()) as {
          defaultTemplate?: string;
          defaultHarnessConfig?: string;
          defaultMaxTurns?: number;
          defaultMaxModelCalls?: number;
          defaultMaxDuration?: string;
          defaultGCPIdentityMode?: string;
          defaultGCPIdentityServiceAccountID?: string;
        };
        this.projectSettingsCache.set(projectId, data);
        return data;
      }
    } catch {
      // Non-critical — fall back to generic default
    }
    return null;
  }

  private slugify(text: string): string {
    return text
      .toLowerCase()
      .trim()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '');
  }

  private onTemplateChange(e: Event): void {
    const select = e.target as HTMLElement & { value: string };
    this.templateId = select.value;

    const template = this.templates.find((t) => t.id === this.templateId);
    const configName = template?.defaultHarnessConfig || template?.harness;
    if (configName) {
      this.setHarnessFromValue(configName);
    }
  }

  /**
   * Set harness from a value, falling back to "Other" if the value doesn't
   * match any known harness config in the dropdown.
   */
  private setHarnessFromValue(value: string): void {
    const knownNames = this.harnessConfigs.map((hc) => hc.name);
    const fallbackNames = ['gemini', 'claude', 'opencode', 'codex'];
    const available = knownNames.length > 0 ? knownNames : fallbackNames;

    if (available.includes(value)) {
      this.harness = value;
      this.customHarness = '';
    } else {
      this.harness = '__other__';
      this.customHarness = value;
    }
  }

  /** Resolved harness config name for submission. */
  private get resolvedHarness(): string {
    return this.harness === '__other__' ? this.customHarness : this.harness;
  }

  /** Hint text showing when the selected harness config matches the template's default. */
  private get templateHarnessHint(): string {
    const template = this.templates.find((t) => t.id === this.templateId);
    const configName = template?.defaultHarnessConfig || template?.harness;
    if (!configName) return '';

    if (configName === this.resolvedHarness) {
      return 'Matches selected template.';
    }
    return `Template suggests: ${configName}`;
  }

  override render() {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading...</p>
        </div>
      `;
    }

    return html`
      <a
        href="${this.sourceProject ? `/projects/${this.sourceProject.id}` : '/agents'}"
        class="back-link"
      >
        <sl-icon name="arrow-left"></sl-icon>
        ${this.sourceProject ? `To ${this.sourceProject.name}` : 'Back to Agents'}
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="plus-circle"></sl-icon>
          Create Agent
        </h1>
        <p>Configure and start a new AI agent.</p>
      </div>

      <div class="form-card">
        ${this.error
          ? html`
              <div class="error-banner">
                <sl-icon name="exclamation-triangle"></sl-icon>
                <span>${this.error}</span>
                ${this.errorLinks.length > 0
                  ? html`<span class="error-links"
                      >&nbsp;&mdash;
                      ${this.errorLinks.map(
                        (link, i) => html`${i > 0 ? html` or ` : nothing}<a href=${link.href}>${link.label}</a>`
                      )}</span
                    >`
                  : nothing}
              </div>
            `
          : ''}

        <div>
          <div class="form-field">
            <label for="name">Agent Name</label>
            <sl-input
              id="name"
              placeholder="my-agent"
              .value=${this.name}
              @sl-input=${(e: Event) => {
                this.name = (e.target as HTMLElement & { value: string }).value;
              }}
              required
            ></sl-input>
          </div>

          ${this.selectedProject?.gitRemote && !isSharedWorkspace(this.selectedProject)
            ? html`
                <div class="form-field">
                  <label for="branch">Branch</label>
                  <sl-input
                    id="branch"
                    placeholder="defaults to agent name"
                    .value=${this.branch}
                    @sl-input=${(e: Event) => {
                      this.branch = (e.target as HTMLElement & { value: string }).value;
                    }}
                  ></sl-input>
                  <div class="hint">Git branch for this agent's workspace.</div>
                </div>
              `
            : ''}

          <div class="form-field">
            <label for="project">Project</label>
            <sl-select
              id="project"
              placeholder="Select a project..."
              .value=${this.projectId}
              @sl-change=${(e: Event) => {
                this.projectId = (e.target as HTMLElement & { value: string }).value;
                this.selectBrokerForProject();
                void this.selectDefaultTemplate();
                void this.loadHarnessConfigs();
                void this.loadGCPServiceAccounts();
                // Clear branch if new project is not git-based
                if (!this.selectedProject?.gitRemote) {
                  this.branch = '';
                }
              }}
              required
            >
              ${this.projects.map((p) => html`<sl-option value=${p.id}>${p.name}</sl-option>`)}
            </sl-select>
            <div class="hint">The project workspace for this agent.</div>
          </div>

          <div class="form-field">
            <label for="template">Template</label>
            <sl-select
              id="template"
              placeholder="Select a template..."
              .value=${this.templateId}
              @sl-change=${(e: Event) => this.onTemplateChange(e)}
            >
              ${this.filteredTemplates.map(
                (t) =>
                  html`<sl-option value=${t.id}
                    >${t.displayName || t.name}${t.scope === 'project'
                      ? ' (project)'
                      : ''}${t.description ? ` - ${t.description}` : ''}</sl-option
                  >`
              )}
            </sl-select>
            <div class="hint">Agent configuration template.</div>
          </div>

          <div class="form-field">
            <label for="harness">Harness Config</label>
            <sl-select
              id="harness"
              placeholder="Select a harness..."
              .value=${this.harness}
              @sl-change=${(e: Event) => {
                this.harness = (e.target as HTMLElement & { value: string }).value;
                if (this.harness !== '__other__') {
                  this.customHarness = '';
                }
              }}
            >
              ${this.harnessConfigs.length > 0
                ? this.harnessConfigs.map(
                    (hc) => html`
                      <sl-option value=${hc.name}>
                        ${hc.displayName || hc.name}
                        ${hc.harness ? html` <small>(${hc.harness})</small>` : ''}
                      </sl-option>
                    `
                  )
                : html`
                    <sl-option value="gemini">Gemini</sl-option>
                    <sl-option value="claude">Claude</sl-option>
                    <sl-option value="opencode">OpenCode</sl-option>
                    <sl-option value="codex">Codex</sl-option>
                  `}
              <sl-option value="__other__">Other...</sl-option>
            </sl-select>
            <div class="hint">
              ${this.templateHarnessHint
                ? this.templateHarnessHint
                : 'The LLM harness configuration to use.'}
            </div>
          </div>
          ${this.harness === '__other__'
            ? html`
                <div class="form-field">
                  <label for="custom-harness">Custom Harness Config Name</label>
                  <sl-input
                    id="custom-harness"
                    placeholder="e.g. my-custom-harness"
                    .value=${this.customHarness}
                    @sl-input=${(e: Event) => {
                      this.customHarness = (e.target as HTMLElement & { value: string }).value;
                    }}
                    required
                  ></sl-input>
                  <div class="hint">
                    Name of the harness config directory (from .scion/harness-configs/).
                  </div>
                </div>
              `
            : ''}

          <div class="form-field">
            <label for="harness-auth">Harness Authentication</label>
            <sl-select
              id="harness-auth"
              placeholder="Select auth method..."
              .value=${this.harnessAuth}
              @sl-change=${(e: Event) => {
                this.harnessAuth = (e.target as HTMLElement & { value: string }).value;
              }}
            >
              <sl-option value="">Auto Detected</sl-option>
              <sl-option value="api-key">Provider API Key</sl-option>
              <sl-option value="oauth-token">OAuth Token (env var)</sl-option>
              <sl-option value="vertex-ai">Vertex Model Garden</sl-option>
              <sl-option value="auth-file">Harness credential file</sl-option>
            </sl-select>
            <div class="hint">Override the authentication method for the harness.</div>
          </div>

          <div class="form-field">
            <label for="broker">Runtime Broker</label>
            <sl-select
              id="broker"
              placeholder="Select a broker..."
              .value=${this.brokerId}
              @sl-change=${(e: Event) => {
                this.brokerId = (e.target as HTMLElement & { value: string }).value;
                this.onBrokerChange();
              }}
            >
              ${this.brokers.map(
                (b) =>
                  html`<sl-option value=${b.id} ?disabled=${b.status === 'offline'}>
                    ${b.name} (${b.status})
                  </sl-option>`
              )}
            </sl-select>
            <div class="hint">The compute node that will run this agent.</div>
          </div>

          ${this.selectedBrokerProfiles.length > 0
            ? html`
                <div class="form-field">
                  <label for="profile">Runtime Profile</label>
                  <sl-select
                    id="profile"
                    .value=${this.profile}
                    @sl-change=${(e: Event) => {
                      this.profile = (e.target as HTMLElement & { value: string }).value;
                    }}
                  >
                    <sl-option value="">Use broker default</sl-option>
                    ${this.selectedBrokerProfiles.map(
                      (p) => html`<sl-option value=${p.name}>${p.name} (${p.type})</sl-option>`
                    )}
                  </sl-select>
                  <div class="hint">The runtime profile on the selected broker.</div>
                </div>
              `
            : ''}

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
                : ''}
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
                            this.gcpServiceAccountId = (
                              e.target as HTMLElement & { value: string }
                            ).value;
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
                          No verified service accounts available. Register and verify service
                          accounts in project settings.
                        </div>
                      `}
                </div>
              `
            : ''}

          <div class="form-field">
            <label for="task">Initial Task</label>
            <sl-textarea
              id="task"
              placeholder="Describe what this agent should work on..."
              .value=${this.task}
              @sl-input=${(e: Event) => {
                this.task = (e.target as HTMLElement & { value: string }).value;
              }}
              rows="4"
              resize="auto"
            ></sl-textarea>
            <div class="hint">The task or prompt to start the agent with.</div>
          </div>

          <div class="notify-field">
            <sl-checkbox
              ?checked=${this.notify}
              @sl-change=${(e: Event) => {
                this.notify = (e.target as HTMLInputElement).checked;
              }}
            >
              Notify me on important agent state changes
            </sl-checkbox>
            <sl-tooltip
              content="You will be notified when this agent reaches: Completed, Waiting for Input, or Limits Exceeded."
              hoist
            >
              <span class="help-badge">?</span>
            </sl-tooltip>
          </div>

          <div class="form-actions">
            <sl-button
              variant="default"
              ?loading=${this.submittingEdit}
              ?disabled=${this.submitting || this.submittingEdit}
              @click=${(e: Event) => this.handleCreateAndEdit(e)}
            >
              <sl-icon slot="prefix" name="sliders"></sl-icon>
              Create &amp; Edit
            </sl-button>
            <sl-button
              variant="primary"
              ?loading=${this.submitting}
              ?disabled=${this.submitting || this.submittingEdit}
              @click=${(e: Event) => this.handleSubmit(e)}
            >
              <sl-icon slot="prefix" name="play-circle"></sl-icon>
              Create &amp; Start Agent
            </sl-button>
            <sl-button
              variant="default"
              ?disabled=${this.submitting || this.submittingEdit}
              @click=${async () => {
                if (this.editingAgentId) {
                  await this.deleteEditingAgent();
                }
                const dest = this.sourceProject ? `/projects/${this.sourceProject.id}` : '/agents';
                window.history.pushState({}, '', dest);
                window.dispatchEvent(new PopStateEvent('popstate'));
              }}
            >
              Cancel
            </sl-button>
          </div>
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-create': ScionPageAgentCreate;
  }
}
