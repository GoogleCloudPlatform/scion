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
 * Project settings page component
 *
 * Displays project-scoped templates, environment variables, secrets, and danger-zone actions (delete).
 */

import { LitElement, html, css } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData, Project, Template, AdminGroup, GitHubAppProjectStatus, GitHubTokenPermissions, RuntimeBroker, BrokerProfile, GCPServiceAccount } from '../../shared/types.js';
import { can, canAny } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import '../shared/env-var-list.js';
import '../shared/secret-list.js';
import '../shared/shared-dir-list.js';
import '../shared/group-member-editor.js';
import '../shared/gcp-service-account-list.js';
import '../shared/scheduled-event-list.js';
import '../shared/subscription-manager.js';
import '../shared/schedule-list.js';


interface ProjectResourceSpec {
  requests?: { cpu?: string | undefined; memory?: string | undefined };
  limits?: { cpu?: string | undefined; memory?: string | undefined };
  disk?: string;
}

interface ProjectSettings {
  defaultTemplate?: string | undefined;
  defaultHarnessConfig?: string | undefined;
  telemetryEnabled?: boolean | null | undefined;
  activeProfile?: string | undefined;
  defaultMaxTurns?: number | undefined;
  defaultMaxModelCalls?: number | undefined;
  defaultMaxDuration?: string | undefined;
  defaultResources?: ProjectResourceSpec | undefined;
  defaultGCPIdentityMode?: string | undefined;
  defaultGCPIdentityServiceAccountID?: string | undefined;
}

interface HarnessConfigEntry {
  id: string;
  name: string;
  slug: string;
  displayName?: string;
  harness: string;
  scope: string;
}

interface RuntimeBrokerWithProvider extends RuntimeBroker {
  localPath?: string;
}

@customElement('scion-page-project-settings')
export class ScionPageProjectSettings extends LitElement {
  @property({ type: Object })
  pageData: PageData | null = null;

  @property({ type: String })
  projectId = '';

  @state()
  private loading = true;

  @state()
  private project: Project | null = null;

  @state()
  private error: string | null = null;

  @state()
  private deleteLoading = false;


  @state()
  private templates: Template[] = [];

  @state()
  private templatesLoading = true;

  @state()
  private syncLoading = false;

  @state()
  private syncError: string | null = null;

  @state()
  private syncSuccess: string | null = null;

  @state()
  private syncRepoUrl = '';

  @state()
  private importMode: 'url' | 'workspace' = 'url';

  @state()
  private membersGroup: AdminGroup | null = null;

  @state()
  private settings: ProjectSettings = {};

  @state()
  private settingsLoading = true;

  @state()
  private settingsSaving = false;

  @state()
  private settingsError: string | null = null;

  @state()
  private settingsSuccess: string | null = null;

  @state()
  private activeConfigTab = 'general';

  @state()
  private activeResourcesTab = 'env-vars';

  @state()
  private activeSchedulesTab = 'events';

  @state()
  private dropdownTemplates: Template[] = [];

  @state()
  private harnessConfigs: HarnessConfigEntry[] = [];

  @state()
  private configDefaultTemplate = '';

  @state()
  private configDefaultHarnessConfig = '';

  @state()
  private configTelemetryEnabled: boolean | null = null;

  @state()
  private hubTelemetryDefault: boolean | null = null;

  // Default agent limits
  @state()
  private configDefaultMaxTurns = 0;

  @state()
  private configDefaultMaxModelCalls = 0;

  @state()
  private configDefaultMaxDuration = '';

  // Default resources
  @state()
  private configDefaultResCpuReq = '';

  @state()
  private configDefaultResMemReq = '';

  @state()
  private configDefaultResCpuLim = '';

  @state()
  private configDefaultResMemLim = '';

  @state()
  private configDefaultResDisk = '';

  // Default GCP identity
  @state()
  private configDefaultGCPIdentityMode = '';

  @state()
  private configDefaultGCPIdentitySAID = '';

  @state()
  private gcpServiceAccounts: GCPServiceAccount[] = [];

  // GitHub App integration
  @state()
  private githubAppStatus: GitHubAppProjectStatus | null = null;

  @state()
  private githubAppInstallationId: number | null = null;

  @state()
  private githubAppPermissions: GitHubTokenPermissions | null = null;

  @state()
  private githubAppError: string | null = null;

  @state()
  private githubAppConfigured = false;

  @state()
  private githubAppInstallationUrl = '';

  @state()
  private githubAppLoading = false;

  // Runtime Brokers (providers)
  @state()
  private brokers: RuntimeBrokerWithProvider[] = [];

  @state()
  private brokersLoading = false;

  @state()
  private brokersError: string | null = null;

  private brokerRelativeTimeInterval: ReturnType<typeof setInterval> | null = null;


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

    .header {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 2rem;
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

    .section {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      padding: 1.5rem;
      margin-bottom: 1.5rem;
    }

    .section h2 {
      font-size: 1.125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .section p {
      font-size: 0.875rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0 0 1rem 0;
    }

    .section-header {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1rem;
    }

    .section-header-text {
      flex: 1;
    }

    .template-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .template-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .template-item sl-icon {
      color: var(--scion-primary, #3b82f6);
      font-size: 1.125rem;
      flex-shrink: 0;
    }

    .template-info {
      flex: 1;
      min-width: 0;
    }

    .template-name {
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .template-meta {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.125rem;
    }

    .template-badge {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
    }

    .empty-templates {
      text-align: center;
      padding: 2rem 1rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .empty-templates sl-icon {
      font-size: 2rem;
      margin-bottom: 0.5rem;
      display: block;
    }

    .sync-status {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.75rem 1rem;
      border-radius: var(--scion-radius, 0.5rem);
      font-size: 0.8125rem;
      margin-bottom: 1rem;
    }

    .sync-status.error {
      background: var(--sl-color-danger-50, #fef2f2);
      color: var(--sl-color-danger-700, #b91c1c);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
    }

    .sync-status.success {
      background: var(--sl-color-success-50, #f0fdf4);
      color: var(--sl-color-success-700, #15803d);
      border: 1px solid var(--sl-color-success-200, #bbf7d0);
    }

    .sync-status.syncing {
      background: var(--sl-color-primary-50, #eff6ff);
      color: var(--sl-color-primary-700, #1d4ed8);
      border: 1px solid var(--sl-color-primary-200, #bfdbfe);
    }

    .danger-section {
      border-color: var(--sl-color-danger-200, #fecaca);
    }

    .danger-section h2 {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .delete-area {
      display: flex;
      align-items: flex-start;
      justify-content: space-between;
      gap: 1.5rem;
      padding-top: 1rem;
      border-top: 1px solid var(--scion-border, #e2e8f0);
    }

    .delete-info {
      flex: 1;
    }

    .delete-info h3 {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.25rem 0;
    }

    .delete-info p {
      font-size: 0.8125rem;
      color: var(--scion-text-muted, #64748b);
      margin: 0;
    }

    .delete-actions {
      flex-shrink: 0;
      display: flex;
      flex-direction: column;
      align-items: flex-end;
      gap: 0.75rem;
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

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(base) {
      background: transparent;
    }

    sl-tab-panel::part(base) {
      padding: 1.5rem 0 0 0;
    }

    .config-form {
      display: flex;
      flex-direction: column;
      gap: 1rem;
    }

    .config-field {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .config-field label {
      font-size: 0.8125rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .config-field .field-help {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
    }

    .config-actions {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      justify-content: flex-end;
      padding-top: 0.5rem;
    }

    .config-status {
      font-size: 0.8125rem;
    }

    .config-status.error {
      color: var(--sl-color-danger-600, #dc2626);
    }

    .config-status.success {
      color: var(--sl-color-success-600, #16a34a);
    }

    .done-footer {
      display: flex;
      justify-content: flex-start;
      margin-top: 1rem;
    }

    /* GitHub App section styles */
    .github-no-install {
      text-align: center;
      padding: 1.5rem;
    }

    .github-no-install p {
      margin: 0.5rem 0;
    }

    .github-status-row {
      display: flex;
      flex-wrap: wrap;
      gap: 1.5rem;
      margin-bottom: 1rem;
    }

    .github-status-item {
      display: flex;
      flex-direction: column;
      gap: 0.25rem;
    }

    .github-status-value {
      display: flex;
      align-items: center;
      gap: 0.5rem;
    }

    .github-status-dot {
      display: inline-block;
      width: 10px;
      height: 10px;
      border-radius: 50%;
    }

    .github-status-dot.ok { background: #16a34a; }
    .github-status-dot.degraded { background: #eab308; }
    .github-status-dot.error { background: #dc2626; }
    .github-status-dot.unchecked { background: #94a3b8; }

    .github-permissions {
      margin-top: 1rem;
    }

    .github-perm-grid {
      display: flex;
      flex-wrap: wrap;
      gap: 0.375rem;
      margin-top: 0.375rem;
    }

    .github-perm-badge {
      font-size: 0.75rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      border: 1px solid var(--scion-border, #e2e8f0);
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
    }

    .github-perm-badge.write {
      background: #dbeafe;
      border-color: #93c5fd;
      color: #1e40af;
    }

    .github-perm-badge.read {
      background: #f0fdf4;
      border-color: #86efac;
      color: #166534;
    }

    .status-message.warning {
      background: #fef3c7;
      color: #92400e;
      border: 1px solid #fcd34d;
    }

    /* Runtime Brokers tab */
    .broker-list {
      display: flex;
      flex-direction: column;
      gap: 0.5rem;
    }

    .broker-item {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      padding: 0.75rem 1rem;
      background: var(--scion-bg-subtle, #f8fafc);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .broker-status-dot {
      width: 10px;
      height: 10px;
      border-radius: 50%;
      flex-shrink: 0;
    }

    .broker-status-dot.online { background: #16a34a; }
    .broker-status-dot.offline { background: #94a3b8; }
    .broker-status-dot.degraded { background: #eab308; }

    .broker-info {
      flex: 1;
      min-width: 0;
    }

    .broker-name-row {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .broker-name {
      font-weight: 600;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .broker-default-badge {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      background: var(--scion-primary, #3b82f6);
      color: #fff;
      white-space: nowrap;
    }

    .broker-meta-row {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-top: 0.25rem;
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem 0.75rem;
    }

    .broker-profiles {
      display: flex;
      flex-wrap: wrap;
      gap: 0.25rem;
      margin-top: 0.375rem;
    }

    .broker-profile-badge {
      font-size: 0.6875rem;
      padding: 0.125rem 0.5rem;
      border-radius: 9999px;
      background: var(--scion-bg-subtle, #f1f5f9);
      color: var(--scion-text-muted, #64748b);
      border: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
    }

    .broker-profile-badge.available {
      background: var(--sl-color-success-50, #f0fdf4);
      border-color: var(--sl-color-success-200, #86efac);
      color: var(--sl-color-success-700, #166534);
    }

    .broker-actions {
      display: flex;
      align-items: center;
      gap: 0.25rem;
      flex-shrink: 0;
    }

    .empty-brokers {
      text-align: center;
      padding: 2rem 1rem;
      color: var(--scion-text-muted, #64748b);
      font-size: 0.875rem;
    }

    .empty-brokers sl-icon {
      font-size: 2rem;
      margin-bottom: 0.5rem;
      display: block;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    if (!this.projectId && typeof window !== 'undefined') {
      const match = window.location.pathname.match(/\/projects\/([^/]+)/);
      if (match) {
        this.projectId = match[1];
      }
    }
    void this.loadProject().then(() => this.loadMembersGroup());
    void this.loadTemplates();
    void this.loadDropdownTemplates();
    void this.loadSettings();
    void this.loadHubTelemetryDefault();
    void this.loadHarnessConfigs();
    void this.loadBrokers();
    void this.loadGCPServiceAccounts();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    if (this.brokerRelativeTimeInterval) {
      clearInterval(this.brokerRelativeTimeInterval);
      this.brokerRelativeTimeInterval = null;
    }
  }

  private async loadProject(skipGitHubCheck = false): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}`);

      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }

      this.project = (await response.json()) as Project;
      dispatchPageTitle(this, 'Settings', this.project.name || this.projectId);
      // Pre-populate template import URL with the project's git remote when set
      if (this.project.gitRemote && !this.syncRepoUrl) {
        this.syncRepoUrl = this.project.gitRemote;
      }
      // Populate GitHub App fields from project data
      this.githubAppInstallationId = this.project.githubInstallationId ?? null;
      this.githubAppStatus = this.project.githubAppStatus ?? null;
      this.githubAppPermissions = this.project.githubPermissions ?? null;

      // Check if the hub has a GitHub App configured (only on initial load)
      if (!skipGitHubCheck && this.project.gitRemote) {
        void this.checkGitHubAppConfigured();
      }
    } catch (err) {
      console.error('Failed to load project:', err);
      this.error = err instanceof Error ? err.message : 'Failed to load project';
    } finally {
      this.loading = false;
    }
  }

  private async checkGitHubAppConfigured(): Promise<void> {
    this.githubAppLoading = true;
    try {
      const res = await apiFetch('/api/v1/github-app');
      if (res.ok) {
        const data = (await res.json()) as { configured: boolean; installation_url?: string };
        this.githubAppConfigured = data.configured;
        this.githubAppInstallationUrl = data.installation_url || '';

        // Auto-discover if configured and project has no installation yet
        if (data.configured && this.githubAppInstallationId == null && this.project?.gitRemote) {
          await this.discoverGitHubInstallation();
          return; // discoverGitHubInstallation handles githubAppLoading
        }
      }
    } catch {
      // Non-critical — just don't show the section
    } finally {
      this.githubAppLoading = false;
    }
  }

  private async loadTemplates(): Promise<void> {
    this.templatesLoading = true;
    try {
      const response = await apiFetch(
        `/api/v1/templates?scope=project&projectId=${encodeURIComponent(this.projectId)}&status=active`
      );
      if (response.ok) {
        const data = (await response.json()) as { templates?: Template[] } | Template[];
        this.templates = Array.isArray(data) ? data : data.templates || [];
      }
    } catch (err) {
      console.error('Failed to load templates:', err);
    } finally {
      this.templatesLoading = false;
    }
  }

  private async loadDropdownTemplates(): Promise<void> {
    try {
      const response = await apiFetch(
        `/api/v1/templates?projectId=${encodeURIComponent(this.projectId)}&status=active`
      );
      if (response.ok) {
        const data = (await response.json()) as { templates?: Template[] } | Template[];
        this.dropdownTemplates = Array.isArray(data) ? data : data.templates || [];
      }
    } catch (err) {
      console.error('Failed to load dropdown templates:', err);
    }
  }

  private async loadMembersGroup(): Promise<void> {
    if (!this.project) {
      console.warn('[project-settings] loadMembersGroup: project not loaded yet, skipping');
      return;
    }
    const projectUUID = this.project.id;
    try {
      const url = `/api/v1/groups?projectId=${encodeURIComponent(projectUUID)}&groupType=explicit&limit=10`;
      console.debug('[project-settings] loadMembersGroup:', url);
      const response = await apiFetch(url);
      if (response.ok) {
        const data = (await response.json()) as { groups?: AdminGroup[] } | AdminGroup[];
        const groups = Array.isArray(data) ? data : data.groups || [];
        console.debug(
          '[project-settings] groups for project:',
          groups.length,
          groups.map((g) => g.slug)
        );
        // Find the members group (slug pattern: project:<slug>:members)
        this.membersGroup = groups.find((g) => g.slug?.endsWith(':members')) || null;
        if (!this.membersGroup) {
          console.warn('[project-settings] no :members group found for project', projectUUID);
        }
      } else {
        console.warn('[project-settings] loadMembersGroup response not ok:', response.status);
      }
    } catch (err) {
      console.error('[project-settings] Failed to load project members group:', err);
    }
  }

  private async loadSettings(): Promise<void> {
    this.settingsLoading = true;
    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}/settings`);
      if (response.ok) {
        this.settings = (await response.json()) as ProjectSettings;
        this.configDefaultTemplate = this.settings.defaultTemplate || '';
        this.configDefaultHarnessConfig = this.settings.defaultHarnessConfig || '';
        this.configTelemetryEnabled = this.settings.telemetryEnabled ?? null;
        this.configDefaultMaxTurns = this.settings.defaultMaxTurns || 0;
        this.configDefaultMaxModelCalls = this.settings.defaultMaxModelCalls || 0;
        this.configDefaultMaxDuration = this.settings.defaultMaxDuration || '';
        const res = this.settings.defaultResources;
        this.configDefaultResCpuReq = res?.requests?.cpu || '';
        this.configDefaultResMemReq = res?.requests?.memory || '';
        this.configDefaultResCpuLim = res?.limits?.cpu || '';
        this.configDefaultResMemLim = res?.limits?.memory || '';
        this.configDefaultResDisk = res?.disk || '';
        this.configDefaultGCPIdentityMode = this.settings.defaultGCPIdentityMode || '';
        this.configDefaultGCPIdentitySAID = this.settings.defaultGCPIdentityServiceAccountID || '';
      }
    } catch (err) {
      console.error('Failed to load project settings:', err);
    } finally {
      this.settingsLoading = false;
    }
  }

  private async loadHubTelemetryDefault(): Promise<void> {
    try {
      const response = await apiFetch('/api/v1/settings/public');
      if (response.ok) {
        const data = (await response.json()) as { telemetryEnabled?: boolean };
        this.hubTelemetryDefault = data.telemetryEnabled ?? false;
      }
    } catch (err) {
      console.error('Failed to load hub telemetry default:', err);
    }
  }

  private async loadHarnessConfigs(): Promise<void> {
    try {
      const response = await apiFetch(
        `/api/v1/harness-configs?status=active&projectId=${encodeURIComponent(this.projectId)}&limit=100`
      );
      if (response.ok) {
        const data = (await response.json()) as { harnessConfigs?: HarnessConfigEntry[] };
        this.harnessConfigs = data.harnessConfigs || [];
      }
    } catch (err) {
      console.error('Failed to load harness configs:', err);
    }
  }

  private async loadGCPServiceAccounts(): Promise<void> {
    try {
      const response = await apiFetch(
        `/api/v1/projects/${this.projectId}/gcp-service-accounts`
      );
      if (response.ok) {
        const data = (await response.json()) as { items?: GCPServiceAccount[] };
        this.gcpServiceAccounts = (data.items || []).filter((sa) => sa.verified);
      }
    } catch (err) {
      console.error('Failed to load GCP service accounts:', err);
    }
  }

  private async handleSaveConfig(): Promise<void> {
    this.settingsSaving = true;
    this.settingsError = null;
    this.settingsSuccess = null;

    try {
      // Build default resources if any field is set
      let defaultResources: ProjectResourceSpec | undefined;
      if (
        this.configDefaultResCpuReq ||
        this.configDefaultResMemReq ||
        this.configDefaultResCpuLim ||
        this.configDefaultResMemLim ||
        this.configDefaultResDisk
      ) {
        defaultResources = {};
        if (this.configDefaultResCpuReq || this.configDefaultResMemReq) {
          defaultResources.requests = {
            cpu: this.configDefaultResCpuReq || undefined,
            memory: this.configDefaultResMemReq || undefined,
          };
        }
        if (this.configDefaultResCpuLim || this.configDefaultResMemLim) {
          defaultResources.limits = {
            cpu: this.configDefaultResCpuLim || undefined,
            memory: this.configDefaultResMemLim || undefined,
          };
        }
        if (this.configDefaultResDisk) {
          defaultResources.disk = this.configDefaultResDisk;
        }
      }

      const body: ProjectSettings = {
        defaultTemplate: this.configDefaultTemplate || undefined,
        defaultHarnessConfig: this.configDefaultHarnessConfig || undefined,
        telemetryEnabled: this.configTelemetryEnabled,
        defaultMaxTurns: this.configDefaultMaxTurns || undefined,
        defaultMaxModelCalls: this.configDefaultMaxModelCalls || undefined,
        defaultMaxDuration: this.configDefaultMaxDuration || undefined,
        defaultResources,
        defaultGCPIdentityMode: this.configDefaultGCPIdentityMode || undefined,
        defaultGCPIdentityServiceAccountID:
          this.configDefaultGCPIdentityMode === 'assign'
            ? this.configDefaultGCPIdentitySAID || undefined
            : undefined,
      };

      const response = await apiFetch(`/api/v1/projects/${this.projectId}/settings`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `Failed to save: HTTP ${response.status}`));
      }

      this.settings = (await response.json()) as ProjectSettings;
      this.settingsSuccess = 'Configuration saved.';
    } catch (err) {
      console.error('Failed to save project settings:', err);
      this.settingsError = err instanceof Error ? err.message : 'Failed to save settings';
    } finally {
      this.settingsSaving = false;
    }
  }

  private async handleSyncTemplates(): Promise<void> {
    this.syncLoading = true;
    this.syncError = null;
    this.syncSuccess = null;

    try {
      const body =
        this.importMode === 'workspace'
          ? { workspacePath: this.syncRepoUrl || '/.scion/templates' }
          : { sourceUrl: this.syncRepoUrl };
      const response = await apiFetch(`/api/v1/projects/${this.projectId}/import-templates`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!response.ok) {
        throw new Error(await extractApiError(response, `Failed to import templates: HTTP ${response.status}`));
      }

      const data = (await response.json()) as { templates: string[]; count: number };
      await Promise.all([this.loadTemplates(), this.loadDropdownTemplates()]);
      this.syncSuccess = `${data.count} template${data.count !== 1 ? 's' : ''} imported successfully.`;
    } catch (err) {
      console.error('Failed to import templates:', err);
      this.syncError = err instanceof Error ? err.message : 'Failed to import templates';
    } finally {
      this.syncLoading = false;
    }
  }

  private async handleDeleteProject(event?: MouseEvent): Promise<void> {
    const projectName = this.project?.name || this.projectId;

    if (
      !event?.altKey &&
      !confirm(
        `Are you sure you want to delete "${projectName}"?\n\nAll agents in this project will be stopped and deleted.\n\nThis action cannot be undone.`
      )
    ) {
      return;
    }

    this.deleteLoading = true;

    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}`, {
        method: 'DELETE',
      });

      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, `Failed to delete project: HTTP ${response.status}`));
      }

      // Navigate back to projects list
      window.history.pushState({}, '', '/projects');
      window.dispatchEvent(new PopStateEvent('popstate'));
    } catch (err) {
      console.error('Failed to delete project:', err);
      alert(err instanceof Error ? err.message : 'Failed to delete project');
    } finally {
      this.deleteLoading = false;
    }
  }

  override render() {
    if (this.loading) {
      return this.renderLoading();
    }

    if (this.error || !this.project) {
      return this.renderError();
    }

    return html`
      <a href="/projects/${this.projectId}" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Project
      </a>

      <div class="header">
        <sl-icon name="gear"></sl-icon>
        <h1>${this.project.name} Settings</h1>
      </div>

      ${this.renderConfigSection()}

      ${this.renderGitHubAppSection()}

      ${this.membersGroup
        ? html`
            <scion-group-member-editor
              groupId=${this.membersGroup.id}
              ?readOnly=${!canAny(this.project._capabilities, 'update', 'manage')}
              compact
              sectionTitle="Members"
              sectionDescription="Users and groups who can create and manage agents in this project."
            ></scion-group-member-editor>
          `
        : ''}

      ${this.renderResourcesSection()}

      ${this.pageData?.user
        ? html`
            <scion-subscription-manager
              .projectId=${this.project.id}
              compact
            ></scion-subscription-manager>
          `
        : ''}

      ${this.renderSchedulesSection()}

      ${can(this.project._capabilities, 'delete')
        ? html`
            <div class="section danger-section">
              <h2>Danger Zone</h2>
              <p>Irreversible actions that affect this project and its resources.</p>

              <div class="delete-area">
                <div class="delete-info">
                  <h3>Delete this project</h3>
                  <p>
                    Permanently remove this project, its configuration, and all agents. All running
                    agents will be stopped and deleted. This action cannot be undone.
                  </p>
                </div>
                <div class="delete-actions">
                  <sl-button
                    variant="danger"
                    size="small"
                    ?loading=${this.deleteLoading}
                    ?disabled=${this.deleteLoading}
                    @click=${(e: MouseEvent) => this.handleDeleteProject(e)}
                  >
                    <sl-icon slot="prefix" name="trash"></sl-icon>
                    Delete Project
                  </sl-button>
                </div>
              </div>
            </div>
          `
        : html`
            <div class="section">
              <h2>Permissions</h2>
              <p>You don't have permission to modify this project.</p>
            </div>
          `}

      <div class="done-footer">
        <sl-button variant="default" href="/projects/${this.projectId}">
          <sl-icon slot="prefix" name="arrow-left"></sl-icon>
          Back to ${this.project.name}
        </sl-button>
      </div>
    `;
  }

  private isGitHubRemote(): boolean {
    const remote = this.project?.gitRemote || '';
    return /github\.com[/:]/.test(remote);
  }

  private renderGitHubAppSection() {
    if (!this.project?.gitRemote) return '';
    if (!this.isGitHubRemote()) return '';
    if (!this.githubAppLoading && !this.githubAppConfigured) return '';

    const status = this.githubAppStatus;
    const hasInstallation = this.githubAppInstallationId != null;

    const stateIcon = (s: string | undefined) => {
      switch (s) {
        case 'ok': return html`<span class="github-status-dot ok"></span>`;
        case 'degraded': return html`<span class="github-status-dot degraded"></span>`;
        case 'error': return html`<span class="github-status-dot error"></span>`;
        case 'unchecked': return html`<span class="github-status-dot unchecked"></span>`;
        default: return '';
      }
    };

    const stateLabel = (s: string | undefined) => {
      switch (s) {
        case 'ok': return 'Active';
        case 'degraded': return 'Degraded';
        case 'error': return 'Error';
        case 'unchecked': return 'Unchecked';
        default: return 'Not configured';
      }
    };

    return html`
      <div class="section">
        <h2>GitHub App Integration</h2>
        <p>Automatic token management for GitHub operations via GitHub App installation tokens.</p>

        ${this.githubAppError ? html`
          <div class="status-message error">${this.githubAppError}</div>
        ` : ''}

        ${!hasInstallation ? html`
          <div class="github-no-install">
            <sl-icon name="github" style="font-size: 2rem; color: var(--scion-text-muted, #64748b);"></sl-icon>
            ${this.githubAppLoading ? html`
              <p>Checking for GitHub App installation…</p>
            ` : !this.githubAppConfigured ? html`
              <p>No GitHub App has been configured on this Hub.</p>
              <p class="field-help">Ask your Hub admin to configure the GitHub App integration, then install it on your organization or account.</p>
            ` : html`
              <p>No GitHub App installation found for this project's repository.</p>

              ${this.githubAppInstallationUrl ? html`
                <p class="field-help">
                  <a href=${this.githubAppInstallationUrl} target="_blank" rel="noopener noreferrer">
                    Install the GitHub App
                  </a> on your organization or account, then click Discover.
                </p>
              ` : html`
                <p class="field-help">Install the GitHub App on your organization or account, then click Discover.</p>
              `}
              <sl-button variant="default" size="small" @click=${() => this.discoverGitHubInstallation()}>
                <sl-icon slot="prefix" name="search"></sl-icon>
                Discover Installation
              </sl-button>
            `}
          </div>
        ` : html`
          <div class="github-status-row">
            <div class="github-status-item">
              <span class="field-help">Status</span>
              <div class="github-status-value">
                ${stateIcon(status?.state)}
                <strong>${stateLabel(status?.state)}</strong>
              </div>
            </div>
            <div class="github-status-item">
              <span class="field-help">Installation ID</span>
              <code>${this.githubAppInstallationId}</code>
            </div>
            ${status?.last_token_mint ? html`
              <div class="github-status-item">
                <span class="field-help">Last Token Mint</span>
                <span>${new Date(status.last_token_mint).toLocaleString()}</span>
              </div>
            ` : ''}
          </div>

          ${status?.state === 'error' || status?.state === 'degraded' ? html`
            <div class="status-message ${status.state === 'error' ? 'error' : 'warning'}">
              <strong>${this.formatGitHubErrorCode(status.error_code)}:</strong> ${status.error_message}
              ${status.state === 'error' && this.project?.gitRemote ? html`
                <br><small>Agents will use PAT fallback if available.</small>
              ` : ''}
            </div>
          ` : ''}

          <div class="github-permissions">
            <span class="field-help">Token Permissions</span>
            <div class="github-perm-grid">
              ${this.renderPermBadge('Contents', this.githubAppPermissions?.contents)}
              ${this.renderPermBadge('Pull Requests', this.githubAppPermissions?.pull_requests)}
              ${this.renderPermBadge('Issues', this.githubAppPermissions?.issues)}
              ${this.renderPermBadge('Metadata', this.githubAppPermissions?.metadata)}
              ${this.renderPermBadge('Checks', this.githubAppPermissions?.checks)}
              ${this.renderPermBadge('Actions', this.githubAppPermissions?.actions)}
            </div>
          </div>

          <div style="margin-top: 1rem; display: flex; align-items: center; gap: 0.5rem;">
            <sl-button variant="default" size="small" ?loading=${this.githubAppLoading} ?disabled=${this.githubAppLoading} @click=${() => this.checkGitHubStatus()}>
              <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
              ${status?.state === 'unchecked' ? 'Check Status' : 'Recheck Status'}
            </sl-button>
            <a href=${`https://github.com/settings/installations/${this.githubAppInstallationId}`}
               target="_blank" rel="noopener noreferrer" style="text-decoration: none;">
              <sl-button variant="text" size="small">
                <sl-icon slot="prefix" name="gear"></sl-icon>
                Configure Installation
              </sl-button>
            </a>
            <sl-button variant="text" size="small" @click=${() => this.removeGitHubInstallation()}>
              <sl-icon slot="prefix" name="x-circle"></sl-icon>
              Remove
            </sl-button>
          </div>
        `}
      </div>
    `;
  }

  private renderPermBadge(label: string, value: string | undefined) {
    if (!value) return '';
    return html`
      <span class="github-perm-badge ${value}">
        ${label}: ${value}
      </span>
    `;
  }

  private formatGitHubErrorCode(code?: string): string {
    switch (code) {
      case 'private_key_invalid': return 'Private Key Invalid';
      case 'app_not_found': return 'App Not Found';
      case 'installation_revoked': return 'Installation Revoked';
      case 'installation_suspended': return 'Installation Suspended';
      case 'repo_not_accessible': return 'Repository Not Accessible';
      case 'permission_denied': return 'Permission Denied';
      case 'token_mint_failed': return 'Token Mint Failed';
      default: return code ?? 'Error';
    }
  }

  private async checkGitHubStatus(): Promise<void> {
    this.githubAppError = null;
    this.githubAppLoading = true;
    try {
      // Actively verify the installation by minting a test token
      const res = await apiFetch(`/api/v1/projects/${this.projectId}/github-status`, { method: 'POST' });
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Check failed (${res.status})`);
      }
      const result = (await res.json()) as {
        status?: GitHubAppProjectStatus;
        permissions?: GitHubTokenPermissions;
        installation_id?: number;
      };
      // Update local state from the check response
      this.githubAppStatus = result.status ?? null;
      this.githubAppPermissions = result.permissions ?? null;
      this.githubAppInstallationId = result.installation_id ?? this.githubAppInstallationId;
      if (this.project) {
        this.project = {
          ...this.project,
          githubAppStatus: result.status,
          githubPermissions: result.permissions,
        };
      }
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Check failed';
    } finally {
      this.githubAppLoading = false;
    }
  }

  private async discoverGitHubInstallation(): Promise<void> {
    this.githubAppLoading = true;
    this.githubAppError = null;
    try {
      const res = await apiFetch('/api/v1/github-app/installations/discover', { method: 'POST' });
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Failed to discover installations (${res.status})`);
      }
      // Refresh just the project's GitHub App state without a full reload
      await this.refreshGitHubAppState();
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Discovery failed';
    } finally {
      this.githubAppLoading = false;
    }
  }

  private async refreshGitHubAppState(): Promise<void> {
    const res = await apiFetch(`/api/v1/projects/${this.projectId}`);
    if (res.ok) {
      const project = (await res.json()) as Project;
      this.githubAppInstallationId = project.githubInstallationId ?? null;
      this.githubAppStatus = project.githubAppStatus ?? null;
      this.githubAppPermissions = project.githubPermissions ?? null;
      // Update the project object in place so renderProjectIcon and other parts reflect the change
      if (this.project) {
        this.project = { ...this.project, githubInstallationId: project.githubInstallationId, githubAppStatus: project.githubAppStatus, githubPermissions: project.githubPermissions };
      }
    }
  }

  private async removeGitHubInstallation(): Promise<void> {
    if (!confirm('Remove GitHub App installation from this project? Agents will fall back to PAT authentication.')) return;
    this.githubAppLoading = true;
    this.githubAppError = null;
    try {
      const res = await apiFetch(`/api/v1/projects/${this.projectId}/github-installation`, { method: 'DELETE' });
      if (!res.ok) {
        const data = (await res.json().catch(() => ({}))) as { message?: string };
        throw new Error(data.message || `Failed to remove installation (${res.status})`);
      }
      this.githubAppInstallationId = null;
      this.githubAppStatus = null;
      this.githubAppPermissions = null;
      if (this.project) {
        this.project = { ...this.project, githubInstallationId: undefined, githubAppStatus: undefined, githubPermissions: undefined };
      }
    } catch (err) {
      this.githubAppError = err instanceof Error ? err.message : 'Remove failed';
    } finally {
      this.githubAppLoading = false;
    }
  }

  private renderConfigSection() {
    const canEdit = canAny(this.project!._capabilities, 'update', 'manage');

    if (this.settingsLoading) {
      return html`
        <div class="section">
          <h2>Configuration</h2>
          <p>Project configuration and agent defaults.</p>
          <div style="text-align: center; padding: 1rem;"><sl-spinner></sl-spinner></div>
        </div>
      `;
    }

    return html`
      <div class="section">
        <h2>Configuration</h2>
        <p>Project configuration and agent defaults.</p>

        <sl-tab-group
          @sl-tab-show=${(e: CustomEvent) => {
            this.activeConfigTab = (e.detail as { name: string }).name;
          }}
        >
          <sl-tab slot="nav" panel="general" ?active=${this.activeConfigTab === 'general'}
            >General</sl-tab
          >
          <sl-tab slot="nav" panel="limits" ?active=${this.activeConfigTab === 'limits'}
            >Limits</sl-tab
          >
          <sl-tab slot="nav" panel="resources" ?active=${this.activeConfigTab === 'resources'}
            >Resources</sl-tab
          >
          <sl-tab slot="nav" panel="brokers" ?active=${this.activeConfigTab === 'brokers'}
            >Runtime Brokers</sl-tab
          >

          <sl-tab-panel name="general">
            <div class="config-form">
              <div class="config-field">
                <label>Default Template</label>
                <sl-select
                  placeholder="None (use server default)"
                  clearable
                  value=${this.configDefaultTemplate}
                  ?disabled=${!canEdit}
                  @sl-change=${(e: Event) => {
                    this.configDefaultTemplate = (e.target as HTMLSelectElement).value;
                  }}
                >
                  ${this.dropdownTemplates.map(
                    (t) => html` <sl-option value=${t.name}>${t.displayName || t.name}</sl-option> `
                  )}
                </sl-select>
                <span class="field-help"
                  >Template used when creating agents without specifying one.</span
                >
              </div>

              <div class="config-field">
                <label>Default Harness Config</label>
                <sl-select
                  placeholder="None (use server default)"
                  clearable
                  value=${this.configDefaultHarnessConfig}
                  ?disabled=${!canEdit}
                  @sl-change=${(e: Event) => {
                    this.configDefaultHarnessConfig = (e.target as HTMLSelectElement).value;
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
                </sl-select>
                <span class="field-help"
                  >Harness configuration used by default for new agents.</span
                >
              </div>

              <div class="config-field">
                <label>Telemetry</label>
                <sl-select
                  value=${this.configTelemetryEnabled === true
                    ? 'enabled'
                    : this.configTelemetryEnabled === false
                      ? 'disabled'
                      : 'inherit'}
                  ?disabled=${!canEdit}
                  @sl-change=${(e: Event) => {
                    const val = (e.target as HTMLSelectElement).value;
                    this.configTelemetryEnabled =
                      val === 'enabled' ? true : val === 'disabled' ? false : null;
                  }}
                >
                  <sl-option value="inherit"
                    >Use hub default (${this.hubTelemetryDefault === null ? '…' : this.hubTelemetryDefault ? 'enabled' : 'disabled'})</sl-option
                  >
                  <sl-option value="enabled">Enabled</sl-option>
                  <sl-option value="disabled">Disabled</sl-option>
                </sl-select>
                <span class="field-help"
                  >Controls telemetry for agents in this project. "Use hub default" inherits the server-level setting.</span
                >
              </div>

              <div class="config-field">
                <label>Default Service Account</label>
                <sl-select
                  value=${this.configDefaultGCPIdentityMode || 'inherit'}
                  ?disabled=${!canEdit}
                  @sl-change=${(e: Event) => {
                    const val = (e.target as HTMLSelectElement).value;
                    this.configDefaultGCPIdentityMode = val === 'inherit' ? '' : val;
                    if (val !== 'assign') {
                      this.configDefaultGCPIdentitySAID = '';
                    }
                  }}
                >
                  <sl-option value="inherit">None (default to block)</sl-option>
                  <sl-option value="block">Block</sl-option>
                  <sl-option value="passthrough">Passthrough</sl-option>
                  <sl-option value="assign">Assign Service Account</sl-option>
                </sl-select>
                <span class="field-help"
                  >Controls GCP metadata server access for new agents. "Block" prevents access, "Passthrough" allows host identity, "Assign" binds a specific service account.</span
                >
              </div>

              ${this.configDefaultGCPIdentityMode === 'assign'
                ? html`
                    <div class="config-field">
                      <label>Service Account</label>
                      <sl-select
                        placeholder="Select a verified service account"
                        clearable
                        value=${this.configDefaultGCPIdentitySAID}
                        ?disabled=${!canEdit}
                        @sl-change=${(e: Event) => {
                          this.configDefaultGCPIdentitySAID = (e.target as HTMLSelectElement).value;
                        }}
                      >
                        ${this.gcpServiceAccounts.length > 0
                          ? this.gcpServiceAccounts.map(
                              (sa) => html`
                                <sl-option value=${sa.id}>
                                  ${sa.displayName || sa.email}
                                  <small>(${sa.email})</small>
                                </sl-option>
                              `
                            )
                          : html`<sl-option value="" disabled
                              >No verified service accounts available</sl-option
                            >`}
                      </sl-select>
                      <span class="field-help"
                        >The GCP service account to assign to new agents by default. Only verified accounts are shown.</span
                      >
                    </div>
                  `
                : ''}
            </div>
          </sl-tab-panel>

          <sl-tab-panel name="limits">
            <div class="config-form">
              <span class="field-help" style="display: block; margin-bottom: 0.25rem;"
                >Applied to new agents unless overridden by template or agent config.</span
              >

              <div class="config-field">
                <label>Default Max Turns</label>
                <sl-input
                  type="number"
                  placeholder="No limit"
                  .value=${this.configDefaultMaxTurns ? String(this.configDefaultMaxTurns) : ''}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultMaxTurns =
                      parseInt((e.target as HTMLInputElement).value) || 0;
                  }}
                ></sl-input>
                <span class="field-help">Maximum conversation turns per agent.</span>
              </div>

              <div class="config-field">
                <label>Default Max Model Calls</label>
                <sl-input
                  type="number"
                  placeholder="No limit"
                  .value=${this.configDefaultMaxModelCalls
                    ? String(this.configDefaultMaxModelCalls)
                    : ''}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultMaxModelCalls =
                      parseInt((e.target as HTMLInputElement).value) || 0;
                  }}
                ></sl-input>
                <span class="field-help">Maximum LLM API calls per agent.</span>
              </div>

              <div class="config-field">
                <label>Default Max Duration</label>
                <sl-input
                  type="text"
                  placeholder="e.g. 2h, 30m"
                  .value=${this.configDefaultMaxDuration}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultMaxDuration = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
                <span class="field-help">Maximum execution time (Go duration format).</span>
              </div>
            </div>
          </sl-tab-panel>

          <sl-tab-panel name="resources">
            <div class="config-form">
              <span class="field-help" style="display: block; margin-bottom: 0.25rem;"
                >Default resource requests and limits for new agents.</span
              >

              <div class="config-field">
                <label>CPU Request</label>
                <sl-input
                  type="text"
                  placeholder="e.g. 500m, 1"
                  .value=${this.configDefaultResCpuReq}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultResCpuReq = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              </div>

              <div class="config-field">
                <label>Memory Request</label>
                <sl-input
                  type="text"
                  placeholder="e.g. 512Mi, 1Gi"
                  .value=${this.configDefaultResMemReq}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultResMemReq = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              </div>

              <div class="config-field">
                <label>CPU Limit</label>
                <sl-input
                  type="text"
                  placeholder="e.g. 1, 2"
                  .value=${this.configDefaultResCpuLim}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultResCpuLim = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              </div>

              <div class="config-field">
                <label>Memory Limit</label>
                <sl-input
                  type="text"
                  placeholder="e.g. 1Gi, 2Gi"
                  .value=${this.configDefaultResMemLim}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultResMemLim = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              </div>

              <div class="config-field">
                <label>Disk</label>
                <sl-input
                  type="text"
                  placeholder="e.g. 10Gi"
                  .value=${this.configDefaultResDisk}
                  ?disabled=${!canEdit}
                  @sl-input=${(e: Event) => {
                    this.configDefaultResDisk = (e.target as HTMLInputElement).value;
                  }}
                ></sl-input>
              </div>
            </div>
          </sl-tab-panel>

          <sl-tab-panel name="brokers">
            ${this.renderBrokersContent()}
          </sl-tab-panel>
        </sl-tab-group>

        ${canEdit && this.activeConfigTab !== 'brokers'
          ? html`
              <div class="config-actions">
                ${this.settingsError
                  ? html`<span class="config-status error">${this.settingsError}</span>`
                  : ''}
                ${this.settingsSuccess
                  ? html`<span class="config-status success">${this.settingsSuccess}</span>`
                  : ''}
                <sl-button
                  variant="primary"
                  size="small"
                  ?loading=${this.settingsSaving}
                  ?disabled=${this.settingsSaving}
                  @click=${() => this.handleSaveConfig()}
                >
                  Save Configuration
                </sl-button>
              </div>
            `
          : ''}
      </div>
    `;
  }

  private renderResourcesSection() {
    const canEdit = canAny(this.project!._capabilities, 'update', 'manage');
    if (!canEdit) return '';

    return html`
      <div class="section">
        <h2>Resources</h2>
        <p>Project-scoped resources available to agents.</p>

        <sl-tab-group
          @sl-tab-show=${(e: CustomEvent) => {
            this.activeResourcesTab = (e.detail as { name: string }).name;
          }}
        >
          <sl-tab slot="nav" panel="env-vars" ?active=${this.activeResourcesTab === 'env-vars'}>Environment Variables</sl-tab>
          <sl-tab slot="nav" panel="secrets" ?active=${this.activeResourcesTab === 'secrets'}>Secrets</sl-tab>
          <sl-tab slot="nav" panel="shared-dirs" ?active=${this.activeResourcesTab === 'shared-dirs'}>Shared Directories</sl-tab>
          <sl-tab slot="nav" panel="templates" ?active=${this.activeResourcesTab === 'templates'}>Templates</sl-tab>
          <sl-tab slot="nav" panel="gcp-sa" ?active=${this.activeResourcesTab === 'gcp-sa'}>GCP Service Accounts</sl-tab>

          <sl-tab-panel name="env-vars">
            <scion-env-var-list
              scope="project"
              scopeId=${this.projectId}
              apiBasePath="/api/v1/projects/${this.projectId}"
            ></scion-env-var-list>
          </sl-tab-panel>

          <sl-tab-panel name="secrets">
            <scion-secret-list
              scope="project"
              scopeId=${this.projectId}
              apiBasePath="/api/v1/projects/${this.projectId}"
            ></scion-secret-list>
          </sl-tab-panel>

          <sl-tab-panel name="shared-dirs">
            <scion-shared-dir-list
              projectId=${this.projectId}
              apiBasePath="/api/v1/projects/${this.projectId}"
            ></scion-shared-dir-list>
          </sl-tab-panel>

          <sl-tab-panel name="templates">
            ${this.renderTemplatesContent()}
          </sl-tab-panel>

          <sl-tab-panel name="gcp-sa">
            <scion-gcp-service-account-list
              projectId=${this.projectId}
            ></scion-gcp-service-account-list>
          </sl-tab-panel>
        </sl-tab-group>
      </div>
    `;
  }

  private renderTemplatesContent() {
    const canSync = canAny(this.project!._capabilities, 'update', 'manage');
    return html`
      <div class="section-header" style="margin-bottom: 1rem;">
        <div class="section-header-text">
          <p style="margin: 0;">Project-scoped agent templates imported into the Hub.</p>
        </div>
        ${canSync
          ? html`
              <sl-button
                size="small"
                variant="default"
                ?loading=${this.syncLoading}
                ?disabled=${this.syncLoading || (this.importMode === 'url' && !this.syncRepoUrl)}
                @click=${() => this.handleSyncTemplates()}
              >
                <sl-icon slot="prefix" name="download"></sl-icon>
                Import Templates
              </sl-button>
            `
          : ''}
      </div>
      ${canSync
        ? html`
            <div style="margin-bottom: 1rem;">
              <sl-radio-group
                size="small"
                value=${this.importMode}
                style="margin-bottom: 0.5rem;"
                @sl-change=${(e: Event) => {
                  this.importMode = (e.target as HTMLInputElement).value as 'url' | 'workspace';
                  this.syncRepoUrl = '';
                  this.syncError = null;
                  this.syncSuccess = null;
                  if (this.importMode === 'url' && this.project?.gitRemote) {
                    this.syncRepoUrl = this.project.gitRemote;
                  }
                }}
              >
                <sl-radio-button value="url">Import from URL</sl-radio-button>
                <sl-radio-button value="workspace">Import from workspace</sl-radio-button>
              </sl-radio-group>
              <sl-input
                placeholder=${this.importMode === 'workspace'
                  ? '/.scion/templates'
                  : 'https://github.com/org/repo/tree/main/.scion/templates'}
                size="small"
                clearable
                .value=${this.syncRepoUrl}
                ?disabled=${this.syncLoading}
                @sl-input=${(e: Event) => {
                  this.syncRepoUrl = (e.target as HTMLInputElement).value;
                }}
                @sl-clear=${() => {
                  this.syncRepoUrl = '';
                }}
              >
                <sl-icon slot="prefix" name=${this.importMode === 'workspace' ? 'folder' : 'github'}></sl-icon>
              </sl-input>
              <div style="margin-top: 0.25rem; font-size: 0.75rem; color: var(--sl-color-neutral-500);">
                ${this.importMode === 'workspace'
                  ? 'Path within the project workspace — the default will be used if no path is provided'
                  : 'GitHub URL to a template or templates directory — supports arbitrary deep paths'}
              </div>
            </div>
          `
        : ''}

      ${this.syncLoading
        ? html`
            <div class="sync-status syncing">
              <sl-spinner style="font-size: 0.875rem;"></sl-spinner>
              ${this.importMode === 'workspace'
                ? `Importing templates from workspace ${this.syncRepoUrl || '/.scion/templates'}...`
                : `Importing templates from ${this.syncRepoUrl}...`}
            </div>
          `
        : ''}
      ${this.syncError
        ? html`
            <div class="sync-status error">
              <sl-icon name="exclamation-triangle"></sl-icon>
              ${this.syncError}
            </div>
          `
        : ''}
      ${this.syncSuccess
        ? html`
            <div class="sync-status success">
              <sl-icon name="check-circle"></sl-icon>
              ${this.syncSuccess}
            </div>
          `
        : ''}
      ${this.templatesLoading && !this.syncLoading
        ? html`<div class="empty-templates"><sl-spinner></sl-spinner></div>`
        : this.templates.length > 0
          ? html`
              <div class="template-list">
                ${this.templates.map(
                  (t) => html`
                    <a href="/projects/${this.projectId}/templates/${t.id}" class="template-item" style="text-decoration: none; color: inherit; cursor: pointer;">
                      <sl-icon name="file-earmark-code"></sl-icon>
                      <div class="template-info">
                        <div class="template-name">${t.displayName || t.name}</div>
                        ${t.description
                          ? html`<div class="template-meta">${t.description}</div>`
                          : ''}
                      </div>
                      ${t.harness ? html`<span class="template-badge">${t.harness}</span>` : ''}
                      <sl-icon name="chevron-right" style="color: var(--sl-color-neutral-400); font-size: 0.875rem;"></sl-icon>
                    </a>
                  `
                )}
              </div>
            `
          : html`
              <div class="empty-templates">
                <sl-icon name="file-earmark"></sl-icon>
                <p>No project templates imported yet.</p>
                ${canSync
                  ? html`<p>Enter a source above and click "Import Templates" to import.</p>`
                  : ''}
              </div>
            `}
    `;
  }

  private renderSchedulesSection() {
    return html`
      <div class="section">
        <h2>Schedules</h2>
        <p>Manage scheduled and recurring events for this project.</p>

        <sl-tab-group
          @sl-tab-show=${(e: CustomEvent) => {
            this.activeSchedulesTab = (e.detail as { name: string }).name;
          }}
        >
          <sl-tab slot="nav" panel="events" ?active=${this.activeSchedulesTab === 'events'}>Events</sl-tab>
          <sl-tab slot="nav" panel="recurring" ?active=${this.activeSchedulesTab === 'recurring'}>Recurring</sl-tab>

          <sl-tab-panel name="events">
            <scion-scheduled-event-list
              .projectId=${this.project!.id}
            ></scion-scheduled-event-list>
          </sl-tab-panel>

          <sl-tab-panel name="recurring">
            <scion-schedule-list
              .projectId=${this.project!.id}
            ></scion-schedule-list>
          </sl-tab-panel>
        </sl-tab-group>
      </div>
    `;
  }

  private async loadBrokers(): Promise<void> {
    this.brokersLoading = true;
    this.brokersError = null;
    try {
      const response = await apiFetch(`/api/v1/runtime-brokers?projectId=${this.projectId}`);
      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to load brokers'));
      }
      const data = await response.json() as { brokers: RuntimeBrokerWithProvider[] };
      this.brokers = data.brokers || [];

      // Start relative time refresh if we have brokers
      if (this.brokers.length > 0 && !this.brokerRelativeTimeInterval) {
        this.brokerRelativeTimeInterval = setInterval(() => this.requestUpdate(), 15_000);
      }
    } catch (err) {
      console.error('Failed to load brokers:', err);
      this.brokersError = err instanceof Error ? err.message : 'Failed to load brokers';
    } finally {
      this.brokersLoading = false;
    }
  }

  private async handleSetDefaultBroker(brokerId: string): Promise<void> {
    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ defaultRuntimeBrokerId: brokerId }),
      });
      if (!response.ok) {
        throw new Error(await extractApiError(response, 'Failed to set default broker'));
      }
      const updated = (await response.json()) as Project;
      // Preserve _capabilities from the current project since PATCH responses may omit them
      const caps = this.project?._capabilities || updated._capabilities;
      this.project = caps ? { ...updated, _capabilities: caps } : updated;
    } catch (err) {
      console.error('Failed to set default broker:', err);
    }
  }

  private async handleRemoveBroker(brokerId: string, brokerName: string): Promise<void> {
    const confirmed = confirm(`Remove broker "${brokerName}" from this project?`);
    if (!confirmed) return;

    try {
      const response = await apiFetch(`/api/v1/projects/${this.projectId}/providers/${brokerId}`, {
        method: 'DELETE',
      });
      if (!response.ok && response.status !== 204) {
        throw new Error(await extractApiError(response, 'Failed to remove broker'));
      }
      // If we removed the default broker, clear it from the project
      if (this.project?.defaultRuntimeBrokerId === brokerId) {
        this.project = { ...this.project, defaultRuntimeBrokerId: '' };
      }
      await this.loadBrokers();
    } catch (err) {
      console.error('Failed to remove broker:', err);
    }
  }

  private formatRelativeTime(dateString: string): string {
    try {
      const date = new Date(dateString);
      if (isNaN(date.getTime())) return '\u2014';
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

  private renderBrokersContent() {
    if (this.brokersLoading) {
      return html`<div class="empty-brokers"><sl-spinner></sl-spinner></div>`;
    }

    if (this.brokersError) {
      return html`
        <div class="sync-status error">
          <sl-icon name="exclamation-triangle"></sl-icon>
          ${this.brokersError}
        </div>
      `;
    }

    return html`
      <p style="margin: 0 0 1rem 0; font-size: 0.8125rem; color: var(--scion-text-muted, #64748b);">
        Runtime Brokers provide access to container runtime environments.
      </p>
      ${this.brokers.length === 0
        ? html`
            <div class="empty-brokers">
              <sl-icon name="hdd-rack"></sl-icon>
              <p>No runtime brokers are registered for this project.</p>
            </div>
          `
        : html`
            <div class="broker-list">
              ${this.brokers.map((broker) => this.renderBrokerItem(broker))}
            </div>
          `}
    `;
  }

  private renderBrokerItem(broker: RuntimeBrokerWithProvider) {
    const isDefault = this.project?.defaultRuntimeBrokerId === broker.id;
    const canEdit = canAny(this.project!._capabilities, 'update', 'manage');

    return html`
      <div class="broker-item">
        <span class="broker-status-dot ${broker.status}"></span>
        <div class="broker-info">
          <div class="broker-name-row">
            <span class="broker-name">${broker.name}</span>
            ${isDefault ? html`<span class="broker-default-badge">Default</span>` : ''}
          </div>
          <div class="broker-meta-row">
            <span>${broker.status}</span>
            <span>Last seen: ${this.formatRelativeTime(broker.lastHeartbeat)}</span>
            ${broker.version ? html`<span>v${broker.version}</span>` : ''}
          </div>
          ${broker.profiles && broker.profiles.length > 0
            ? html`
                <div class="broker-profiles">
                  ${broker.profiles.map(
                    (p: BrokerProfile) => html`
                      <span class="broker-profile-badge ${p.available ? 'available' : ''}">${p.name} (${p.type})</span>
                    `
                  )}
                </div>
              `
            : ''}
        </div>
        ${canEdit
          ? html`
              <div class="broker-actions">
                ${!isDefault
                  ? html`
                      <sl-tooltip content="Set as default">
                        <sl-icon-button
                          name="star"
                          label="Set as default"
                          @click=${() => this.handleSetDefaultBroker(broker.id)}
                        ></sl-icon-button>
                      </sl-tooltip>
                    `
                  : html`
                      <sl-tooltip content="Default broker">
                        <sl-icon-button
                          name="star-fill"
                          label="Default broker"
                          style="color: var(--scion-primary, #3b82f6);"
                          disabled
                        ></sl-icon-button>
                      </sl-tooltip>
                    `}
                <sl-tooltip content="Remove broker">
                  <sl-icon-button
                    name="trash"
                    label="Remove"
                    style="color: var(--sl-color-danger-600, #dc2626);"
                    @click=${() => this.handleRemoveBroker(broker.id, broker.name)}
                  ></sl-icon-button>
                </sl-tooltip>
              </div>
            `
          : ''}
      </div>
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading settings...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <a href="/projects/${this.projectId}" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        Back to Project
      </a>

      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <h2>Failed to Load Settings</h2>
        <p>There was a problem loading this project.</p>
        <div class="error-details">${this.error || 'Project not found'}</div>
        <sl-button variant="primary" @click=${() => this.loadProject()}>
          <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
          Retry
        </sl-button>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-project-settings': ScionPageProjectSettings;
  }
}
