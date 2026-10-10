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
 * Unified agent creation page component
 *
 * Single-surface form for creating and starting a new agent: an identity
 * section (name, project, template, harness config, broker, profile, task,
 * notify) and an "Additional Options" disclosure that embeds the shared
 * <scion-agent-config-form> (ptone/scion#3974). The form sends only the
 * fields the user set; everything else is resolved by the hub, and the
 * inherited values the page can read are shown as source-labelled
 * placeholders (resolveInheritedPlaceholders).
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { Project, RuntimeBroker, Template, GCPServiceAccount } from '../../shared/types.js';

interface HarnessConfigEntry {
  id: string;
  name: string;
  slug: string;
  displayName?: string;
  harness: string;
  scope: string;
}

import { isSharedWorkspace } from '../../shared/types.js';
import { isTargetKubernetesOnly } from '../../shared/runtime-kind.js';
import { KNOWN_HARNESS_NAMES, harnessDisplayName } from '../../shared/harness-utils.js';
import { defaultTriggersHint } from '../../shared/notification-triggers.js';
import {
  resolveInheritedPlaceholders,
  type AgentConfigPlaceholder,
  type HubPublicDefaults,
  type ProjectCreateDefaults,
} from '../../shared/agent-config-inherited.js';
import { GcpIdentityState } from '../../shared/gcp-identity-state.js';
import {
  assignStatusTarget,
  assignStatusTargetKey,
  projectServiceAccountListUrl,
  withoutAssignStatus,
  type AssignStatusTarget,
} from '../../shared/gcp-sa-assign-status.js';
import { apiFetch, apiFetchAllPages, parseApiError } from '../../client/api.js';
import { navigateTo } from '../../client/navigation.js';
import { showToast } from '../../utils/toast.js';
import type { AgentOtherInlineConfig, ScionAgentConfigForm } from '../shared/agent-config-form.js';
import '../shared/agent-config-form.js';
import '../shared/status-badge.js';

@customElement('scion-page-agent-create')
export class ScionPageAgentCreate extends LitElement {
  // ── Data from API ───────────────────────────────────────────────────
  @state() private projects: Project[] = [];
  @state() private brokers: RuntimeBroker[] = [];
  @state() private templates: Template[] = [];
  /** The template list failed to load; the rest of the form still loads. */
  @state() private templatesLoadFailed = false;
  @state() private harnessConfigs: HarnessConfigEntry[] = [];

  // ── UI State ────────────────────────────────────────────────────────
  @state() private loading = true;
  @state() private submitting = false;
  @state() private error: string | null = null;
  @state() private errorLinks: Array<{ label: string; href: string }> = [];
  @state() private advancedOpen = false;

  // ── Default Section Fields ──────────────────────────────────────────
  @state() private name = '';
  @state() private projectId = '';
  @state() private templateId = '';
  @state() private harness = 'antigravity';
  @state() private customHarness = '';
  @state() private brokerId = '';
  @state() private profile = '';
  @state() private task = '';
  @state() private notify = true;

  // ── Additional Options ──────────────────────────────────────────
  /** The hub's public settings, for the hub-tier placeholders. */
  @state() private hubSettings: HubPublicDefaults = {};
  /** The selected project's settings, for the project-tier placeholders. */
  @state() private projectSettings: ProjectCreateDefaults | null = null;

  /**
   * The GCP identity picker's state. Owned here, so submit can validate and
   * build gcp_identity from it, and rendered by the shared form.
   */
  readonly gcp = new GcpIdentityState();

  // ── Internal ────────────────────────────────────────────────────────

  /** The form data has loaded at least once. */
  private loadedOnce = false;

  /** Whether the projectId was explicitly passed via URL query param */
  private projectFromUrl = false;

  /** Cached project settings keyed by projectId */
  private projectSettingsCache: Map<string, ProjectCreateDefaults> = new Map();

  /** Profiles available on the currently selected broker */
  private get selectedBrokerProfiles(): import('../../shared/types.js').BrokerProfile[] {
    if (!this.brokerId) return [];
    const broker = this.brokers.find((b) => b.id === this.brokerId);
    return broker?.profiles?.filter((p) => p.available) ?? [];
  }

  /**
   * Whether the currently selected broker/profile combination is reliably
   * known to resolve to a Kubernetes runtime. Block is not offered in that
   * case. This deliberately does not guess: with no broker selected, no
   * matching profile, or (with no profile chosen) a broker whose available
   * profiles mix runtime types, this is false.
   */
  private get targetRuntimeIsKubernetesOnly(): boolean {
    if (!this.brokerId) return false;
    const broker = this.brokers.find((b) => b.id === this.brokerId);
    return isTargetKubernetesOnly(broker, this.profile);
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

  /** The selected template, if any. */
  private get selectedTemplate(): Template | undefined {
    return this.templates.find((t) => t.id === this.templateId);
  }

  /** Inherited values for the form's unset fields, from what this page fetched. */
  private get placeholders(): Record<string, AgentConfigPlaceholder> {
    return resolveInheritedPlaceholders({
      template: this.selectedTemplate,
      projectSettings: this.projectSettings,
      hubSettings: this.hubSettings,
    });
  }

  /** Template-supplied inline config the form does not edit, shown read-only. */
  private get otherInlineConfig(): AgentOtherInlineConfig | null {
    const kubernetes = this.selectedTemplate?.config?.kubernetes;
    return kubernetes ? { config: { kubernetes }, source: 'from the template' } : null;
  }

  private get form(): ScionAgentConfigForm | null {
    return this.shadowRoot?.querySelector('scion-agent-config-form') ?? null;
  }

  // ═══════════════════════════════════════════════════════════════════
  // Styles
  // ═══════════════════════════════════════════════════════════════════

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

    .page-header .project-subtitle {
      color: var(--scion-text-secondary, #475569);
      margin: 0.25rem 0 0 0;
      font-size: 0.875rem;
      font-weight: 500;
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

    .notify-field .source-label {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
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

    /* ── Additional Options Disclosure ───────────────────────────── */

    sl-details::part(base) {
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
    }

    sl-details::part(header) {
      font-size: 0.9375rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      padding: 0.875rem 1rem;
    }

    sl-details::part(content) {
      padding: 0 1rem 1rem 1rem;
    }
  `;

  // ═══════════════════════════════════════════════════════════════════
  // Lifecycle
  // ═══════════════════════════════════════════════════════════════════

  override connectedCallback(): void {
    super.connectedCallback();

    if (typeof window !== 'undefined') {
      const params = new URLSearchParams(window.location.search);

      const projectParam = params.get('projectId');
      if (projectParam) {
        this.projectId = projectParam;
        this.projectFromUrl = true;
      }

      if (params.get('advanced') === '1') {
        this.advancedOpen = true;
      }
    }

    void this.loadFormData();
  }

  override willUpdate(changedProperties: Map<string, unknown>): void {
    super.willUpdate(changedProperties);
    // Block is not offered for a Kubernetes runtime target. Hand the GCP
    // identity state the current target whenever the broker/profile
    // selection or the broker list changes; it corrects the displayed mode
    // (normalizeGcpModeForTarget) in the same update cycle. Whether an
    // explicit identity is sent is gated separately by gcpIdentityUserSet.
    if (
      changedProperties.has('brokerId') ||
      changedProperties.has('profile') ||
      changedProperties.has('brokers')
    ) {
      this.gcp.setTarget(this.targetRuntimeIsKubernetesOnly, this.profile);
      // The accounts' mapping state is for one target: ask again when it
      // changes (ptone/scion#4391). Not before the first load has started.
      if (
        this.gcpLoadSeq > 0 &&
        assignStatusTargetKey(this.gcpAssignTarget) !== this.gcpAssignTargetKey
      ) {
        void this.refreshGCPAssignStatus();
      }
    }
  }

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('error') && this.error) {
      this.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }

  /** Completes once this page and the embedded form have rendered. */
  protected override async getUpdateComplete(): Promise<boolean> {
    const done = await super.getUpdateComplete();
    await this.form?.updateComplete;
    return done;
  }

  // ═══════════════════════════════════════════════════════════════════
  // Data Loading
  // ═══════════════════════════════════════════════════════════════════

  private async loadFormData(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      // Build the templates URL — add scope filtering when a project is known
      // (e.g. from a URL query param) to reduce the result set.
      const tmplParams = new URLSearchParams({ status: 'active', limit: '100' });
      if (this.projectId) {
        tmplParams.set('projectId', this.projectId);
      }
      const tmplUrl = `/api/v1/templates?${tmplParams.toString()}`;

      const [projectsRes, brokersRes, templates, settingsRes, harnessConfigsRes] =
        await Promise.all([
          // Not `mine=true`: on the server that resolves to projects where the
          // caller holds the project-owner role specifically, but creating an
          // agent does not require ownership — project-member carries
          // agent.create, and POST /api/v1/agents authorizes against the target
          // project, not against ownership. Asking for owned projects hid every
          // project a member belongs to from this picker, so members could not
          // create an agent anywhere even though the API would have allowed it.
          // The unfiltered list is already scoped to what the caller may read.
          apiFetch('/api/v1/projects?limit=100'),
          fetch('/api/v1/runtime-brokers?limit=100', { credentials: 'include' }),
          // Caught on its own: a failed template page leaves the template
          // list empty with an inline error instead of failing the form.
          apiFetchAllPages<Template>(tmplUrl, 'templates').then(
            (list) => {
              this.templatesLoadFailed = false;
              return list;
            },
            (err: unknown) => {
              console.error('Failed to load templates:', err);
              this.templatesLoadFailed = true;
              return [] as Template[];
            }
          ),
          fetch('/api/v1/settings/public', { credentials: 'include' }),
          apiFetch('/api/v1/harness-configs?status=active&limit=100'),
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

      this.templates = templates;

      if (settingsRes.ok) {
        this.hubSettings = (await settingsRes.json()) as HubPublicDefaults;
      }

      if (harnessConfigsRes.ok) {
        const data = (await harnessConfigsRes.json()) as {
          harnessConfigs?: HarnessConfigEntry[];
        };
        this.harnessConfigs = (data.harnessConfigs || []).sort((a, b) =>
          (a.displayName || a.name).localeCompare(b.displayName || b.name)
        );
      }

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

      // Project settings, for the inherited placeholders
      if (this.projectId) {
        await this.loadProjectSettings();
      }

      // Reload harness configs scoped to the selected project
      if (this.projectId) {
        await this.loadHarnessConfigs();
      }
    } catch (err) {
      console.error('Failed to load form data:', err);
      this.error = 'Failed to load form data. Please try again.';
    } finally {
      this.loading = false;
      this.loadedOnce = true;
    }
  }

  /**
   * Loads the selected project's settings, shown as inherited placeholders.
   * The hub applies them itself when a field is left unset, so the form
   * never copies them into a field's value.
   */
  private async loadProjectSettings(): Promise<void> {
    const isStale = this.projectLoadGuard();
    this.projectSettings = null;
    const settings = await this.fetchProjectSettings(this.projectId);
    if (isStale()) return;
    this.projectSettings = settings;
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

    // Fallback: hub-level default broker
    const hubDefaultRuntimeBroker = this.hubSettings.defaultRuntimeBroker ?? '';
    if (hubDefaultRuntimeBroker) {
      const hubBroker = this.brokers.find(
        (b) =>
          b.id === hubDefaultRuntimeBroker ||
          (b.name && b.name.toLowerCase() === hubDefaultRuntimeBroker.toLowerCase()) ||
          (b.slug && b.slug.toLowerCase() === hubDefaultRuntimeBroker.toLowerCase())
      );
      if (hubBroker) {
        this.brokerId = hubBroker.id;
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
    const visible = this.projectId
      ? this.templates.filter(
          (t) =>
            t.scope === 'global' ||
            t.scope === 'user' ||
            (t.scope === 'project' && t.scopeId === this.projectId)
        )
      : this.templates;

    const byName = (a: Template, b: Template) =>
      (a.displayName || a.name).localeCompare(b.displayName || b.name);

    const user = visible.filter((t) => t.scope === 'user').sort(byName);
    const project = visible.filter((t) => t.scope === 'project').sort(byName);
    const global = visible.filter((t) => t.scope === 'global').sort(byName);
    const rest = visible
      .filter((t) => t.scope !== 'user' && t.scope !== 'project' && t.scope !== 'global')
      .sort(byName);
    return [...user, ...project, ...global, ...rest];
  }

  /**
   * Select the default template and harness config for the current project.
   */
  private async selectDefaultTemplate(): Promise<void> {
    const isStale = this.projectLoadGuard();
    const visible = this.filteredTemplates;

    const settings = this.projectId ? await this.fetchProjectSettings(this.projectId) : null;
    if (isStale()) return;
    const harnessDefault =
      settings?.defaultHarnessConfig || this.hubSettings.defaultHarnessConfig || 'claude';

    const harnessFor = (t: { defaultHarnessConfig?: string; harness?: string }) =>
      t.defaultHarnessConfig || t.harness || harnessDefault;

    let templateResolved = false;
    if (settings?.defaultTemplate) {
      const match = visible.find(
        (t) => t.name === settings.defaultTemplate || t.slug === settings.defaultTemplate
      );
      if (match) {
        this.templateId = match.id;
        this.setHarnessFromValue(harnessFor(match));
        templateResolved = true;
      }
    }

    // Hub-level default template fallback: try before the generic 'default' slug.
    const hubDefaultTemplate = this.hubSettings.defaultTemplate ?? '';
    if (!templateResolved && hubDefaultTemplate) {
      const hubMatch = visible.find(
        (t) => t.name === hubDefaultTemplate || t.slug === hubDefaultTemplate
      );
      if (hubMatch) {
        this.templateId = hubMatch.id;
        this.setHarnessFromValue(harnessFor(hubMatch));
        templateResolved = true;
      }
    }

    if (!templateResolved) {
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
  }

  private autoSelectProfile(): void {
    const profiles = this.selectedBrokerProfiles;
    if (profiles.length === 1) {
      this.profile = profiles[0].name;
    } else {
      this.profile = '';
    }
  }

  /**
   * Incremented on every project switch. Project-scoped loaders capture it
   * (with the project id) through projectLoadGuard and drop their results
   * when a switch happened while they were waiting, so a slow response for
   * the previous project cannot overwrite the current project's template,
   * limits or harness configs.
   */
  private projectLoadSeq = 0;

  /** Returns a check that is true once the project changed since the call. */
  private projectLoadGuard(): () => boolean {
    const seq = this.projectLoadSeq;
    const projectId = this.projectId;
    return () => seq !== this.projectLoadSeq || this.projectId !== projectId;
  }

  private async loadHarnessConfigs(): Promise<void> {
    const isStale = this.projectLoadGuard();
    try {
      const url = this.projectId
        ? `/api/v1/harness-configs?status=active&projectId=${encodeURIComponent(this.projectId)}&limit=100`
        : '/api/v1/harness-configs?status=active&limit=100';
      const res = await apiFetch(url);
      if (res.ok) {
        const data = (await res.json()) as { harnessConfigs?: HarnessConfigEntry[] };
        if (isStale()) return;
        this.harnessConfigs = (data.harnessConfigs || []).sort((a, b) =>
          (a.displayName || a.name).localeCompare(b.displayName || b.name)
        );
      }
    } catch (err) {
      console.error('Failed to load harness configs:', err);
    }
  }

  /**
   * Incremented at the start of every loadGCPServiceAccounts call. Each call
   * captures its own value and, after every await, drops its results if a
   * newer call has started since (ptone/scion#2548): project switches fire
   * the load unawaited, so a slow response for the previous project must not
   * overwrite the current project's accounts or default.
   */
  private gcpLoadSeq = 0;

  /**
   * Incremented by every fetch of the accounts (the project's load and each
   * mapping-state refresh), so only the newest fetch's mapping state is kept.
   */
  private gcpAssignSeq = 0;

  /** The key of the target the current accounts' mapping state was asked for. */
  private gcpAssignTargetKey = '';

  /** The Kubernetes broker and profile to ask the accounts' mapping state for, or null. */
  private get gcpAssignTarget(): AssignStatusTarget | null {
    return assignStatusTarget(this.targetRuntimeIsKubernetesOnly, this.brokerId, this.profile);
  }

  /** Fetches the project's accounts, with their mapping state on target when given. */
  private async fetchGCPServiceAccounts(
    projectId: string,
    target: AssignStatusTarget | null
  ): Promise<GCPServiceAccount[] | null> {
    try {
      const res = await apiFetch(projectServiceAccountListUrl(projectId, target));
      if (!res.ok) return null;
      const data = (await res.json()) as { items?: GCPServiceAccount[] } | GCPServiceAccount[];
      return Array.isArray(data) ? data : data.items || [];
    } catch {
      return null;
    }
  }

  /**
   * Re-asks the accounts' mapping state after the target changed, keeping
   * the mode, the chosen account and the defaults. With no target known any
   * more, or when the fetch fails, the previous target's labels are dropped
   * rather than shown against the new one.
   */
  private async refreshGCPAssignStatus(): Promise<void> {
    const projectId = this.projectId;
    if (!projectId) return;
    const loadSeq = this.gcpLoadSeq;
    const seq = ++this.gcpAssignSeq;
    const target = this.gcpAssignTarget;
    const key = assignStatusTargetKey(target);
    this.gcpAssignTargetKey = key;
    const isStale = (): boolean =>
      seq !== this.gcpAssignSeq || loadSeq !== this.gcpLoadSeq || this.projectId !== projectId;
    if (!target) {
      this.gcp.setAccounts(withoutAssignStatus(this.gcp.gcpServiceAccounts));
      return;
    }
    const accounts = await this.fetchGCPServiceAccounts(projectId, target);
    if (isStale()) return;
    this.gcp.setAccounts(accounts ?? withoutAssignStatus(this.gcp.gcpServiceAccounts));
  }

  private async loadGCPServiceAccounts(): Promise<void> {
    const seq = ++this.gcpLoadSeq;
    const assignSeq = ++this.gcpAssignSeq;
    const projectId = this.projectId;
    // True when a newer load has started or the project changed under this
    // one; a stale load must not touch any state after that point.
    const isStale = (): boolean => seq !== this.gcpLoadSeq || this.projectId !== projectId;

    // Recomputing defaults from scratch (initial load, or a project change).
    // This runs synchronously, before any await, so it is always performed
    // by the newest load.
    this.gcp.reset();

    if (!projectId) return;
    const target = this.gcpAssignTarget;
    this.gcpAssignTargetKey = assignStatusTargetKey(target);
    // Non-critical: a failed fetch leaves the picker without accounts.
    let accounts = (await this.fetchGCPServiceAccounts(projectId, target)) ?? [];
    if (isStale()) return;
    // The target changed while this load was in flight: this load's labels
    // are for the old target, so drop them and ask again for the current one.
    const targetChanged = assignSeq !== this.gcpAssignSeq;
    if (targetChanged) accounts = withoutAssignStatus(accounts);
    this.gcp.setAccounts(accounts);
    if (targetChanged) void this.refreshGCPAssignStatus();

    // The project's default identity: applied to the displayed value only
    // when the user has not already picked while the fetches were in flight.
    const settings = await this.fetchProjectSettings(projectId);
    if (isStale()) return;
    this.gcp.applyProjectDefaults(settings);
  }

  private async fetchProjectSettings(projectId: string): Promise<ProjectCreateDefaults | null> {
    if (!projectId) return null;

    const cached = this.projectSettingsCache.get(projectId);
    if (cached !== undefined) return cached;

    try {
      const res = await apiFetch(`/api/v1/projects/${projectId}/settings`);
      if (res.ok) {
        const data = (await res.json()) as ProjectCreateDefaults;
        this.projectSettingsCache.set(projectId, data);
        return data;
      }
    } catch {
      // Non-critical
    }
    return null;
  }

  // ═══════════════════════════════════════════════════════════════════
  // Form Helpers
  // ═══════════════════════════════════════════════════════════════════

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

  private setHarnessFromValue(value: string): void {
    const knownNames = this.harnessConfigs.map((hc) => hc.name);
    const available: readonly string[] = knownNames.length > 0 ? knownNames : KNOWN_HARNESS_NAMES;

    if (available.includes(value)) {
      this.harness = value;
      this.customHarness = '';
    } else {
      this.harness = '__other__';
      this.customHarness = value;
    }
  }

  /** Resolved harness config name for submission */
  private get resolvedHarness(): string {
    return this.harness === '__other__' ? this.customHarness : this.harness;
  }

  /** Hint text when the selected harness matches or differs from the template's default */
  private get templateHarnessHint(): string {
    const template = this.templates.find((t) => t.id === this.templateId);
    const configName = template?.defaultHarnessConfig || template?.harness;
    if (!configName) return '';

    if (configName === this.resolvedHarness) {
      return 'Matches selected template.';
    }
    return `Template suggests: ${configName}`;
  }

  // ═══════════════════════════════════════════════════════════════════
  // Submit
  // ═══════════════════════════════════════════════════════════════════

  /**
   * Create the agent and start it, then navigate to the agent detail page.
   */
  private async handleSubmit(_e: Event): Promise<void> {
    if (!this.name.trim()) {
      this.error = 'Agent name is required.';
      return;
    }
    if (!this.projectId) {
      this.error = 'Please select a project.';
      return;
    }

    // The submit-time guards read the live target, even if no update has
    // handed it to the identity state yet.
    this.gcp.targetKubernetesOnly = this.targetRuntimeIsKubernetesOnly;
    this.gcp.profile = this.profile;

    // The form's validation includes the GCP identity rules. The form is
    // rendered once the page has loaded; the identity state is checked
    // directly otherwise.
    const form = this.form;
    const errors = form ? form.validate() : [this.gcp.validate()].filter((e): e is string => !!e);
    if (errors.length > 0) {
      this.error = errors.join(' ');
      return;
    }

    this.submitting = true;
    this.error = null;
    this.errorLinks = [];

    try {
      const body: Record<string, unknown> = {
        name: this.slugify(this.name),
        projectId: this.projectId,
        harnessConfig: this.resolvedHarness,
        notify: this.notify,
      };

      if (this.templateId) body.template = this.templateId;
      if (this.brokerId) body.runtimeBrokerId = this.brokerId;
      if (this.profile) body.profile = this.profile;
      if (this.task.trim()) body.task = this.task.trim();

      // Additional Options: only what the user set. Everything else
      // (including gcp_identity with no explicit pick) is left to the hub's
      // own precedence instead of being pinned client-side.
      if (form) {
        Object.assign(body, form.collectTopLevel());
        const config = form.collectConfigPatch();
        if (Object.keys(config).length > 0) body.config = config;
      } else {
        const gcpIdentity = this.gcp.toRequest();
        if (gcpIdentity) body.gcp_identity = gcpIdentity;
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
            ...(this.projectId
              ? [{ label: 'Project Settings', href: `/projects/${this.projectId}/settings` }]
              : []),
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

      // Start the agent unless the create response shows it already started.
      const startedPhases = ['running', 'provisioning', 'cloning', 'starting'];
      const alreadyStarted = agent?.phase ? startedPhases.includes(agent.phase) : false;
      if (!alreadyStarted) {
        // The agent exists, so a failed start, including a rejected fetch,
        // is reported and its page still opens, where the user can start
        // it again without creating a second agent. The toast stack
        // outlives the navigation.
        let startError: string | null = null;
        try {
          const startResp = await fetch(`/api/v1/agents/${agentId}/start`, {
            method: 'POST',
            credentials: 'include',
          });
          if (!startResp.ok) {
            const fallback = `HTTP ${startResp.status}`;
            startError = (await parseApiError(startResp, fallback)).message;
          }
        } catch (startErr) {
          startError =
            startErr instanceof Error && startErr.message ? startErr.message : 'Request failed';
        }
        if (startError !== null) {
          console.warn('Agent created but failed to start:', startError);
          showToast(`Agent was created but did not start: ${startError}`, 'danger');
        }
      }

      // Navigate to agent detail page
      navigateTo(`/agents/${agentId}`);
    } catch (err) {
      console.error('Failed to create agent:', err);
      this.error = err instanceof Error ? err.message : 'Failed to create agent';
    } finally {
      this.submitting = false;
    }
  }

  // ═══════════════════════════════════════════════════════════════════
  // Render
  // ═══════════════════════════════════════════════════════════════════

  override render() {
    // After the first load the form stays mounted during a reload, so the
    // user's Additional Options edits (kept in the form) survive it.
    if (this.loading && !this.loadedOnce) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading...</p>
        </div>
      `;
    }

    const backHref = this.sourceProject ? `/projects/${this.sourceProject.id}` : '/agents';
    const backLabel = this.sourceProject ? `To ${this.sourceProject.name}` : 'Back to Agents';

    return html`
      <a href="${backHref}" class="back-link">
        <sl-icon name="arrow-left"></sl-icon>
        ${backLabel}
      </a>

      <div class="page-header">
        <h1>
          <sl-icon name="plus-circle"></sl-icon>
          Create Agent
        </h1>
        <p>Configure and start a new AI agent.</p>
        ${this.projectFromUrl && this.sourceProject
          ? html`<p class="project-subtitle">Project: ${this.sourceProject.name}</p>`
          : nothing}
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
                        (link, i) =>
                          html`${i > 0 ? html` or ` : nothing}<a href=${link.href}
                              >${link.label}</a
                            >`
                      )}</span
                    >`
                  : nothing}
              </div>
            `
          : ''}

        <!-- ═══════ Default Section ═══════ -->
        ${this.renderDefaultSection()}

        <!-- ═══════ Additional Options Disclosure ═══════ -->
        <sl-details
          summary="Additional Options"
          ?open=${this.advancedOpen}
          @sl-show=${(e: Event) => {
            if (e.target !== e.currentTarget) return;
            this.advancedOpen = true;
            // Show the General tab when the disclosure opens: a tab group
            // initialized while hidden inside sl-details may show none.
            requestAnimationFrame(() => this.form?.showTab('general'));
          }}
          @sl-hide=${(e: Event) => {
            if (e.target !== e.currentTarget) return;
            this.advancedOpen = false;
          }}
        >
          <scion-agent-config-form
            mode="create"
            .placeholders=${this.placeholders}
            .gcpIdentity=${this.gcp}
            .otherInlineConfig=${this.otherInlineConfig}
            ?branchAvailable=${!!this.selectedProject?.gitRemote &&
            !isSharedWorkspace(this.selectedProject)}
            ?disabled=${this.submitting}
          ></scion-agent-config-form>
        </sl-details>

        <!-- ═══════ Form Actions ═══════ -->
        <div class="form-actions">
          <sl-button
            variant="primary"
            ?loading=${this.submitting}
            ?disabled=${this.submitting}
            @click=${(e: Event) => this.handleSubmit(e)}
          >
            <sl-icon slot="prefix" name="play-circle"></sl-icon>
            Start
          </sl-button>
          <sl-button
            variant="text"
            ?disabled=${this.submitting}
            @click=${() => {
              const dest = this.sourceProject ? `/projects/${this.sourceProject.id}` : '/agents';
              navigateTo(dest);
            }}
          >
            Cancel
          </sl-button>
        </div>
      </div>
    `;
  }

  // ── Default Section ───────────────────────────────────────────────

  private renderDefaultSection() {
    return html`
      <!-- Agent Name -->
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

      <!-- Project (hidden when projectFromUrl) -->
      ${!this.projectFromUrl
        ? html`
            <div class="form-field">
              <label for="project">Project</label>
              <sl-select
                id="project"
                placeholder="Select a project..."
                .value=${this.projectId}
                @sl-change=${(e: Event) => {
                  this.projectId = (e.target as HTMLElement & { value: string }).value;
                  this.projectLoadSeq++;
                  this.selectBrokerForProject();
                  void this.selectDefaultTemplate();
                  void this.loadHarnessConfigs();
                  void this.loadGCPServiceAccounts();
                  void this.loadProjectSettings();
                }}
                required
              >
                ${this.projects.map((p) => html`<sl-option value=${p.id}>${p.name}</sl-option>`)}
              </sl-select>
              <div class="hint">The project workspace for this agent.</div>
            </div>
          `
        : nothing}

      <!-- Template -->
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
                  : t.scope === 'user'
                    ? ' (user)'
                    : t.scope === 'global'
                      ? ' (global)'
                      : ''}${t.description ? ` - ${t.description}` : ''}</sl-option
              >`
          )}
        </sl-select>
        ${this.templatesLoadFailed
          ? html`<div class="hint" style="color: var(--sl-color-danger-600);">
              Could not load templates. Reload the page to try again.
            </div>`
          : html`<div class="hint">Agent configuration template.</div>`}
      </div>

      <!-- Harness Config -->
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
            : KNOWN_HARNESS_NAMES.map(
                (name) => html` <sl-option value=${name}>${harnessDisplayName(name)}</sl-option> `
              )}
          <sl-option value="__other__">Other...</sl-option>
        </sl-select>
        <div class="hint">
          ${this.templateHarnessHint
            ? this.templateHarnessHint
            : 'The LLM harness configuration to use.'}
        </div>
      </div>

      <!-- Custom Harness Config Name (conditional) -->
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
        : nothing}

      <!-- Runtime Broker -->
      <div class="form-field">
        <label for="broker">Runtime Broker</label>
        <sl-select
          id="broker"
          placeholder="Select a broker..."
          .value=${this.brokerId}
          @sl-change=${(e: Event) => {
            this.brokerId = (e.target as HTMLElement & { value: string }).value;
            this.autoSelectProfile();
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

      <!-- Runtime Profile (conditional: broker has profiles) -->
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
        : nothing}

      <!-- Task -->
      <div class="form-field">
        <label for="task">Task</label>
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

      <!-- Notify -->
      <div class="notify-field">
        <sl-checkbox
          ?checked=${this.notify}
          @sl-change=${(e: Event) => {
            this.notify = (e.target as HTMLInputElement).checked;
          }}
        >
          Notify me on important agent state changes
        </sl-checkbox>
        <sl-tooltip content=${defaultTriggersHint()} hoist>
          <span class="help-badge">?</span>
        </sl-tooltip>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-create': ScionPageAgentCreate;
  }
}
