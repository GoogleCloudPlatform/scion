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
 * Single-surface form for creating and starting a new agent. All settings
 * (previously split between agent-create and agent-configure) are available
 * in a default section + an "Additional Options" disclosure with tabbed
 * advanced settings.
 */

import { LitElement, html, css, nothing } from 'lit';
import { customElement, state } from 'lit/decorators.js';

import type { Project, RuntimeBroker, Template, GCPServiceAccount, MessageMode } from '../../shared/types.js';

interface HarnessConfigEntry {
  id: string;
  name: string;
  slug: string;
  displayName?: string;
  harness: string;
  scope: string;
}

import { isSharedWorkspace } from '../../shared/types.js';
import { KNOWN_HARNESS_NAMES, harnessDisplayName } from '../../shared/harness-utils.js';
import { normalizeModelAlias } from '../../shared/model-utils.js';
import { MESSAGE_MODE_DISPLAY } from '../../shared/message-mode.js';
import { apiFetch, parseApiError } from '../../client/api.js';
import { navigateTo } from '../../client/main.js';
import type { EnvEntry } from '../shared/env-editor.js';
import '../shared/env-editor.js';
import '../shared/status-badge.js';

@customElement('scion-page-agent-create')
export class ScionPageAgentCreate extends LitElement {
  // ── Data from API ───────────────────────────────────────────────────
  @state() private projects: Project[] = [];
  @state() private brokers: RuntimeBroker[] = [];
  @state() private templates: Template[] = [];
  @state() private harnessConfigs: HarnessConfigEntry[] = [];
  @state() private gcpServiceAccounts: GCPServiceAccount[] = [];

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
  @state() private harness = 'gemini-cli';
  @state() private customHarness = '';
  @state() private brokerId = '';
  @state() private profile = '';
  @state() private task = '';
  @state() private notify = true;

  // ── Additional Options > General Tab ────────────────────────────────
  @state() private branch = '';
  @state() private modelSelection: '' | 'small' | 'medium' | 'large' | 'extra-large' | 'other' = '';
  @state() private customModelId = '';
  @state() private thinkingLevel: number | null = null;
  @state() private image = '';
  @state() private containerUser = '';
  @state() private telemetryEnabled = false;
  @state() private autoExposePortsEnabled = false;
  @state() private autoExposePortsMode = 'allowlist';
  @state() private autoExposePortsList = '';
  @state() private autoExposePortsInterval = '3s';

  // ── Additional Options > Auth & Security Tab ────────────────────────
  @state() private agentRole = '';
  @state() private messageMode = '';
  @state() private harnessAuth = '';
  @state() private gcpMetadataMode: 'block' | 'passthrough' | 'assign' = 'block';
  @state() private gcpServiceAccountId = '';

  // ── Additional Options > Prompts Tab ────────────────────────────────
  @state() private systemPrompt = '';
  @state() private agentInstructions = '';

  // ── Additional Options > Limits & Resources Tab ─────────────────────
  @state() private maxTurns = 0;
  @state() private maxModelCalls = 0;
  @state() private maxDuration = '';
  @state() private cpuRequest = '';
  @state() private memoryRequest = '';
  @state() private cpuLimit = '';
  @state() private memoryLimit = '';
  @state() private disk = '';

  // ── Additional Options > Environment & Labels Tab ───────────────────
  @state() private envEntries: EnvEntry[] = [];
  @state() private labelEntries: Array<{ key: string; value: string }> = [];

  // ── Internal ────────────────────────────────────────────────────────

  /** Whether the projectId was explicitly passed via URL query param */
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
      defaultModel?: string;
    }
  > = new Map();

  /** Profiles available on the currently selected broker */
  private get selectedBrokerProfiles(): import('../../shared/types.js').BrokerProfile[] {
    if (!this.brokerId) return [];
    const broker = this.brokers.find((b) => b.id === this.brokerId);
    return broker?.profiles?.filter((p) => p.available) ?? [];
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

  /** Verified GCP service accounts for the assign dropdown */
  private get verifiedGCPServiceAccounts(): GCPServiceAccount[] {
    return this.gcpServiceAccounts.filter((sa) => sa.verified);
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

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(body) {
      padding-top: 1.25rem;
    }

    .field-row {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 1rem;
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

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('error') && this.error) {
      this.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }

  // ═══════════════════════════════════════════════════════════════════
  // Data Loading
  // ═══════════════════════════════════════════════════════════════════

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

      if (templatesRes.ok) {
        const data = (await templatesRes.json()) as { templates?: Template[] } | Template[];
        this.templates = Array.isArray(data) ? data : data.templates || [];
      }

      if (settingsRes.ok) {
        const data = (await settingsRes.json()) as {
          telemetryEnabled?: boolean;
          autoExposePortsEnabled?: boolean;
        };
        this.telemetryEnabled = data.telemetryEnabled ?? false;
        this.autoExposePortsEnabled = data.autoExposePortsEnabled ?? false;
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

      // Apply project-settings defaults to advanced fields
      if (this.projectId) {
        await this.applyProjectDefaults();
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
    }
  }

  /** Apply project-settings defaults to advanced fields.
   *  Resets all project-defaultable fields first so that switching projects
   *  does not leak the previous project's defaults into the new one. */
  private async applyProjectDefaults(): Promise<void> {
    // Reset to base defaults before applying new project settings
    this.maxTurns = 0;
    this.maxModelCalls = 0;
    this.maxDuration = '';
    this.modelSelection = '';
    this.customModelId = '';

    const settings = await this.fetchProjectSettings(this.projectId);
    if (!settings) return;

    if (settings.defaultMaxTurns) this.maxTurns = settings.defaultMaxTurns;
    if (settings.defaultMaxModelCalls) this.maxModelCalls = settings.defaultMaxModelCalls;
    if (settings.defaultMaxDuration) this.maxDuration = settings.defaultMaxDuration;
    if (settings.defaultModel) {
      const derived = this.deriveModelSelection(settings.defaultModel);
      this.modelSelection = derived.selection;
      this.customModelId = derived.customId;
    }
  }

  private deriveModelSelection(model: string): {
    selection: '' | 'small' | 'medium' | 'large' | 'extra-large' | 'other';
    customId: string;
  } {
    if (!model) return { selection: '', customId: '' };
    const normalized = normalizeModelAlias(model);
    if (['small', 'medium', 'large', 'extra-large'].includes(normalized)) {
      return {
        selection: normalized as 'small' | 'medium' | 'large' | 'extra-large',
        customId: '',
      };
    }
    return { selection: 'other', customId: model };
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
    const rest = visible.filter((t) => t.scope !== 'user' && t.scope !== 'project' && t.scope !== 'global').sort(byName);
    return [...user, ...project, ...global, ...rest];
  }

  /**
   * Select the default template and harness config for the current project.
   */
  private async selectDefaultTemplate(): Promise<void> {
    const visible = this.filteredTemplates;

    const settings = this.projectId ? await this.fetchProjectSettings(this.projectId) : null;
    const harnessDefault = settings?.defaultHarnessConfig || 'gemini-cli';

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

  private async loadHarnessConfigs(): Promise<void> {
    try {
      const url = this.projectId
        ? `/api/v1/harness-configs?status=active&projectId=${encodeURIComponent(this.projectId)}&limit=100`
        : '/api/v1/harness-configs?status=active&limit=100';
      const res = await apiFetch(url);
      if (res.ok) {
        const data = (await res.json()) as { harnessConfigs?: HarnessConfigEntry[] };
        this.harnessConfigs = (data.harnessConfigs || []).sort((a, b) =>
          (a.displayName || a.name).localeCompare(b.displayName || b.name)
        );
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
        const data = (await res.json()) as { items?: GCPServiceAccount[] } | GCPServiceAccount[];
        this.gcpServiceAccounts = Array.isArray(data) ? data : data.items || [];
      }
    } catch {
      // Non-critical
    }

    // Apply project default GCP identity if configured
    const settings = await this.fetchProjectSettings(this.projectId);
    if (settings?.defaultGCPIdentityMode) {
      const mode = settings.defaultGCPIdentityMode as 'block' | 'passthrough' | 'assign';
      if (mode === 'assign' && settings.defaultGCPIdentityServiceAccountID) {
        const verified = this.verifiedGCPServiceAccounts;
        const match = verified.find((sa) => sa.id === settings.defaultGCPIdentityServiceAccountID);
        if (match) {
          this.gcpMetadataMode = 'assign';
          this.gcpServiceAccountId = match.id;
        }
      } else if (mode === 'passthrough' || mode === 'block') {
        this.gcpMetadataMode = mode;
      }
    }
  }

  private async fetchProjectSettings(projectId: string): Promise<{
    defaultTemplate?: string;
    defaultHarnessConfig?: string;
    defaultMaxTurns?: number;
    defaultMaxModelCalls?: number;
    defaultMaxDuration?: string;
    defaultGCPIdentityMode?: string;
    defaultGCPIdentityServiceAccountID?: string;
    defaultModel?: string;
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
          defaultModel?: string;
        };
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

  private buildLabels(): Record<string, string> | undefined {
    const valid = this.labelEntries.filter((l) => l.key.trim());
    if (valid.length === 0) return undefined;
    const labels: Record<string, string> = {};
    for (const l of valid) {
      labels[l.key.trim()] = l.value.trim();
    }
    return labels;
  }

  /**
   * Build the config payload for advanced fields (mirrors agent-configure.ts buildConfig).
   */
  private buildConfig(): Record<string, unknown> {
    const config: Record<string, unknown> = {};

    // Model
    const model = this.modelSelection === 'other' ? this.customModelId : this.modelSelection;
    if (model) config.model = model;

    // Thinking level
    config.thinking_level = this.thinkingLevel;

    // Container
    if (this.image) config.image = this.image;
    if (this.containerUser) config.user = this.containerUser;

    // Auth
    if (this.harnessAuth) config.auth_selectedType = this.harnessAuth;

    // Prompts
    if (this.systemPrompt) config.system_prompt = this.systemPrompt;
    if (this.agentInstructions) config.agent_instructions = this.agentInstructions;

    // Limits
    if (this.maxTurns) config.max_turns = this.maxTurns;
    if (this.maxModelCalls) config.max_model_calls = this.maxModelCalls;
    if (this.maxDuration) config.max_duration = this.maxDuration;

    // Resources
    const hasResources =
      this.cpuRequest || this.memoryRequest || this.cpuLimit || this.memoryLimit || this.disk;
    if (hasResources) {
      const resources: Record<string, unknown> = {};
      if (this.cpuRequest || this.memoryRequest) {
        const requests: Record<string, string> = {};
        if (this.cpuRequest) requests.cpu = this.cpuRequest;
        if (this.memoryRequest) requests.memory = this.memoryRequest;
        resources.requests = requests;
      }
      if (this.cpuLimit || this.memoryLimit) {
        const limits: Record<string, string> = {};
        if (this.cpuLimit) limits.cpu = this.cpuLimit;
        if (this.memoryLimit) limits.memory = this.memoryLimit;
        resources.limits = limits;
      }
      if (this.disk) resources.disk = this.disk;
      config.resources = resources;
    }

    // Environment variables
    const env: Record<string, string> = {};
    for (const entry of this.envEntries) {
      if (entry.key) {
        env[entry.key] = entry.value;
      }
    }

    // Telemetry (use structured config property, matching agent-configure.ts)
    config.telemetry = { enabled: this.telemetryEnabled };

    // Auto-expose ports
    env.SCION_AUTO_EXPOSE_PORTS = this.autoExposePortsEnabled ? 'true' : 'false';
    if (this.autoExposePortsEnabled) {
      env.SCION_AUTO_EXPOSE_MODE = this.autoExposePortsMode;
      if (this.autoExposePortsList) {
        env.SCION_AUTO_EXPOSE_PORTS_LIST = this.autoExposePortsList;
      }
      env.SCION_AUTO_EXPOSE_INTERVAL = this.autoExposePortsInterval || '3s';
    }

    if (Object.keys(env).length > 0) {
      config.env = env;
    }

    return config;
  }

  // ═══════════════════════════════════════════════════════════════════
  // Submit
  // ═══════════════════════════════════════════════════════════════════

  /**
   * Create the agent without starting it. Navigates to the agent detail page.
   */
  private async handleCreateOnly(_e: Event): Promise<void> {
    return this.handleSubmit(_e, true);
  }

  private async handleSubmit(_e: Event, provisionOnly = false): Promise<void> {
    if (!this.name.trim()) {
      this.error = 'Agent name is required.';
      return;
    }
    if (!this.projectId) {
      this.error = 'Please select a project.';
      return;
    }

    // Validate GCP assign mode
    if (this.gcpMetadataMode === 'assign' && !this.gcpServiceAccountId) {
      this.error = 'Please select a service account for GCP identity assignment.';
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

      if (this.branch.trim()) body.branch = this.branch.trim();
      if (this.templateId) body.template = this.templateId;
      if (this.brokerId) body.runtimeBrokerId = this.brokerId;
      if (this.profile) body.profile = this.profile;
      if (this.task.trim()) body.task = this.task.trim();
      if (this.agentRole) body.agentRole = this.agentRole;
      if (this.messageMode) body.messageMode = this.messageMode;
      if (provisionOnly) body.provisionOnly = true;

      const builtLabels = this.buildLabels();
      if (builtLabels) body.labels = builtLabels;

      // GCP identity
      if (this.gcpMetadataMode === 'assign' && this.gcpServiceAccountId) {
        body.gcp_identity = {
          metadata_mode: 'assign',
          service_account_id: this.gcpServiceAccountId,
        };
      } else if (this.gcpMetadataMode === 'passthrough') {
        body.gcp_identity = { metadata_mode: 'passthrough' };
      } else if (this.gcpMetadataMode === 'block') {
        body.gcp_identity = { metadata_mode: 'block' };
      }

      // Advanced config
      body.config = this.buildConfig();

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

      // Start the agent unless provisionOnly was requested
      if (!provisionOnly) {
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
    if (this.loading) {
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
            // Force the tab-group to show the General tab when disclosure opens.
            // sl-tab-group may not initialize correctly when hidden inside sl-details.
            requestAnimationFrame(() => {
              const tabGroup = this.shadowRoot?.querySelector('sl-tab-group');
              if (tabGroup) {
                (tabGroup as any).show?.('general');
              }
            });
          }}
          @sl-hide=${(e: Event) => {
            if (e.target !== e.currentTarget) return;
            this.advancedOpen = false;
          }}
        >
          <sl-tab-group>
            <sl-tab slot="nav" panel="general" active>General</sl-tab>
            <sl-tab slot="nav" panel="auth-security">Auth &amp; Security</sl-tab>
            <sl-tab slot="nav" panel="env-labels">Environment &amp; Labels</sl-tab>
            <sl-tab slot="nav" panel="limits">Limits &amp; Resources</sl-tab>
            <sl-tab slot="nav" panel="prompts">Prompts</sl-tab>

            <sl-tab-panel name="general">${this.renderGeneralTab()}</sl-tab-panel>
            <sl-tab-panel name="auth-security">${this.renderAuthSecurityTab()}</sl-tab-panel>
            <sl-tab-panel name="env-labels">${this.renderEnvironmentTab()}</sl-tab-panel>
            <sl-tab-panel name="limits">${this.renderLimitsTab()}</sl-tab-panel>
            <sl-tab-panel name="prompts">${this.renderPromptsTab()}</sl-tab-panel>
          </sl-tab-group>
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
            variant="default"
            ?disabled=${this.submitting}
            @click=${(e: Event) => this.handleCreateOnly(e)}
          >
            Create
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
                  this.selectBrokerForProject();
                  void this.selectDefaultTemplate();
                  void this.loadHarnessConfigs();
                  void this.loadGCPServiceAccounts();
                  void this.applyProjectDefaults();
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
        <div class="hint">Agent configuration template.</div>
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
        <sl-tooltip
          content="You will be notified when this agent reaches: Completed, Waiting for Input, or Limits Exceeded."
          hoist
        >
          <span class="help-badge">?</span>
        </sl-tooltip>
      </div>
    `;
  }

  // ── Additional Options > General Tab ──────────────────────────────

  private renderGeneralTab() {
    return html`
      <!-- Branch (conditional: project has gitRemote and is not shared workspace) -->
      ${this.selectedProject?.gitRemote && !isSharedWorkspace(this.selectedProject)
        ? html`
            <div class="form-field">
              <label>Branch</label>
              <sl-input
                placeholder="defaults to agent name"
                .value=${this.branch}
                @sl-input=${(e: Event) => {
                  this.branch = (e.target as HTMLElement & { value: string }).value;
                }}
              ></sl-input>
              <div class="hint">Git branch for this agent's workspace.</div>
            </div>
          `
        : nothing}

      <!-- Model -->
      <div class="form-field">
        <label>Model</label>
        <sl-select
          placeholder="Use harness default"
          .value=${this.modelSelection}
          clearable
          @sl-change=${(e: Event) => {
            this.modelSelection = (e.target as HTMLElement & { value: string })
              .value as typeof this.modelSelection;
            if (this.modelSelection !== 'other') this.customModelId = '';
          }}
        >
          <sl-option value="small">Small</sl-option>
          <sl-option value="medium">Medium</sl-option>
          <sl-option value="large">Large</sl-option>
          <sl-option value="extra-large">Extra Large</sl-option>
          <sl-option value="other">Other (specify)</sl-option>
        </sl-select>
      </div>

      <!-- Custom Model ID (conditional) -->
      ${this.modelSelection === 'other'
        ? html`
            <div class="form-field">
              <label>Custom Model ID</label>
              <sl-input
                placeholder="e.g. claude-opus-4-8"
                .value=${this.customModelId}
                @sl-input=${(e: Event) => {
                  this.customModelId = (e.target as HTMLElement & { value: string }).value;
                }}
              ></sl-input>
            </div>
          `
        : nothing}

      <!-- Thinking Level -->
      <div class="form-field">
        <label>
          Thinking
          Level${this.thinkingLevel !== null
            ? html` <span style="font-weight:normal;color:var(--sl-color-neutral-500)"
                >(${this.thinkingLevel})</span
              >`
            : nothing}
        </label>
        <div style="display:flex;align-items:center;gap:0.75rem">
          <sl-range
            min="0"
            max="100"
            step="1"
            .value=${this.thinkingLevel ?? 50}
            ?disabled=${this.thinkingLevel === null}
            style="flex:1"
            @sl-input=${(e: Event) => {
              this.thinkingLevel = (e.target as HTMLElement & { value: number }).value;
            }}
          ></sl-range>
          <sl-checkbox
            ?checked=${this.thinkingLevel !== null}
            @sl-change=${(e: Event) => {
              this.thinkingLevel = (e.target as HTMLInputElement).checked ? 50 : null;
            }}
          >
            Set
          </sl-checkbox>
        </div>
        <div class="hint" style="display:flex;justify-content:space-between;margin-top:0.25rem">
          <span>0 = minimal reasoning</span>
          <span>${this.thinkingLevel === null ? 'Using harness default' : ''}</span>
          <span>100 = maximum reasoning</span>
        </div>
      </div>

      <!-- Container Image -->
      <div class="form-field">
        <label>Container Image</label>
        <sl-input
          placeholder="Container image override"
          .value=${this.image}
          @sl-input=${(e: Event) => {
            this.image = (e.target as HTMLElement & { value: string }).value;
          }}
        ></sl-input>
        <div class="hint">Override the default container image.</div>
      </div>

      <!-- Container User -->
      <div class="form-field">
        <label>Container User</label>
        <sl-input
          placeholder="Unix user inside container"
          .value=${this.containerUser}
          @sl-input=${(e: Event) => {
            this.containerUser = (e.target as HTMLElement & { value: string }).value;
          }}
        ></sl-input>
      </div>

      <!-- Telemetry -->
      <div class="notify-field">
        <sl-checkbox
          ?checked=${this.telemetryEnabled}
          @sl-change=${(e: Event) => {
            this.telemetryEnabled = (e.target as HTMLInputElement).checked;
          }}
        >
          Enable Telemetry
        </sl-checkbox>
        <sl-tooltip content="Collect telemetry data for this agent." hoist>
          <span class="help-badge">?</span>
        </sl-tooltip>
      </div>

      <!-- Auto-Expose Ports -->
      <div class="notify-field">
        <sl-checkbox
          ?checked=${this.autoExposePortsEnabled}
          @sl-change=${(e: Event) => {
            this.autoExposePortsEnabled = (e.target as HTMLInputElement).checked;
          }}
        >
          Enable Auto-Expose Ports
        </sl-checkbox>
        <sl-tooltip
          content="Automatically detect and expose TCP listening ports from this agent's container."
          hoist
        >
          <span class="help-badge">?</span>
        </sl-tooltip>
      </div>

      <!-- Auto-Expose Sub-fields (conditional) -->
      ${this.autoExposePortsEnabled
        ? html`
            <div class="form-field">
              <label>Port Filter Mode</label>
              <sl-select
                .value=${this.autoExposePortsMode}
                @sl-change=${(e: Event) => {
                  this.autoExposePortsMode = (e.target as HTMLElement & { value: string }).value;
                }}
              >
                <sl-option value="allowlist">Allowlist</sl-option>
                <sl-option value="denylist">Denylist</sl-option>
              </sl-select>
              <div class="hint">
                ${this.autoExposePortsMode === 'allowlist'
                  ? 'Only expose ports in the filter list below.'
                  : 'Expose all ports except those in the filter list below.'}
              </div>
            </div>
            <div class="form-field">
              <label>Port Filter List</label>
              <sl-input
                placeholder="e.g. 3000,5173,8080"
                .value=${this.autoExposePortsList}
                @sl-input=${(e: Event) => {
                  this.autoExposePortsList = (e.target as HTMLElement & { value: string }).value;
                }}
              ></sl-input>
              <div class="hint">
                Comma-separated list of ports to
                ${this.autoExposePortsMode === 'allowlist' ? 'allow' : 'deny'}.
              </div>
            </div>
            <div class="form-field">
              <label>Scan Interval</label>
              <sl-input
                placeholder="3s"
                .value=${this.autoExposePortsInterval}
                @sl-input=${(e: Event) => {
                  this.autoExposePortsInterval = (
                    e.target as HTMLElement & { value: string }
                  ).value;
                }}
              ></sl-input>
              <div class="hint">How often to scan for new listening ports (e.g. 3s, 5s).</div>
            </div>
          `
        : nothing}
    `;
  }

  // ── Additional Options > Auth & Security Tab ──────────────────────

  private renderAuthSecurityTab() {
    return html`
      <!-- Agent Role -->
      <div class="form-field">
        <label>Agent Role</label>
        <sl-select
          placeholder="Select a role..."
          .value=${this.agentRole}
          @sl-change=${(e: Event) => {
            this.agentRole = (e.target as HTMLElement & { value: string }).value;
          }}
        >
          <sl-option value="">Default (determined by project settings)</sl-option>
          <sl-option value="none">None (no hub access)</sl-option>
          <sl-option value="readonly">Read-only</sl-option>
          <sl-option value="baseline">Baseline (standard)</sl-option>
          <sl-option value="full">Full (requires admin)</sl-option>
        </sl-select>
        <div class="hint">Authorization role for hub API access.</div>
      </div>

      <!-- Message Mode -->
      <div class="form-field">
        <label>Message Mode</label>
        <sl-select
          placeholder="Select a message mode..."
          .value=${this.messageMode}
          @sl-change=${(e: Event) => {
            this.messageMode = (e.target as HTMLElement & { value: string }).value;
          }}
        >
          <sl-option value="">Default (inherit from parent)</sl-option>
          ${(Object.entries(MESSAGE_MODE_DISPLAY) as [MessageMode, typeof MESSAGE_MODE_DISPLAY[MessageMode]][]).map(
            ([mode, display]) => html`
              <sl-option value=${mode}>
                <sl-icon slot="prefix" name=${display.icon}></sl-icon>
                ${display.label} — ${display.description}
              </sl-option>
            `
          )}
        </sl-select>
        ${this.messageMode === 'none'
          ? html`<div class="hint" style="color: var(--sl-color-danger-600);">
              This agent will be created in sealed mode. It will not be able to send or receive messages.
            </div>`
          : html`<div class="hint">Message authorization scope. Default inherits from the parent agent's mode.</div>`}
      </div>

      <!-- Harness Authentication -->
      <div class="form-field">
        <label>Harness Authentication</label>
        <sl-select
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
          <sl-option value="none">No Authentication</sl-option>
        </sl-select>
        <div class="hint">Override the authentication method for the harness.</div>
      </div>

      <!-- GCP Identity -->
      <div class="form-field">
        <label>GCP Identity</label>
        <sl-select
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
              : "No metadata interception. The agent inherits the broker's GCP identity. Requires broker ownership."}
        </div>
      </div>

      <!-- GCP Service Account (conditional) -->
      ${this.gcpMetadataMode === 'assign'
        ? html`
            <div class="form-field">
              <label>Service Account</label>
              ${this.verifiedGCPServiceAccounts.length > 0
                ? html`
                    <sl-select
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
                      No verified service accounts available. Register and verify service accounts
                      in project settings.
                    </div>
                  `}
            </div>
          `
        : nothing}
    `;
  }

  // ── Additional Options > Prompts Tab ──────────────────────────────

  private renderPromptsTab() {
    return html`
      <div class="form-field">
        <label>System Prompt</label>
        <sl-textarea
          placeholder="System prompt content or file:// URI..."
          .value=${this.systemPrompt}
          @sl-input=${(e: Event) => {
            this.systemPrompt = (e.target as HTMLElement & { value: string }).value;
          }}
          rows="6"
          resize="auto"
        ></sl-textarea>
        <div class="hint">
          Custom system prompt for the agent. Can be inline text or a file:// URI.
        </div>
      </div>

      <div class="form-field">
        <label>Agent Instructions</label>
        <sl-textarea
          placeholder="Agent instructions content or file:// URI..."
          .value=${this.agentInstructions}
          @sl-input=${(e: Event) => {
            this.agentInstructions = (e.target as HTMLElement & { value: string }).value;
          }}
          rows="6"
          resize="auto"
        ></sl-textarea>
        <div class="hint">
          Additional instructions for the agent. Can be inline text or a file:// URI.
        </div>
      </div>
    `;
  }

  // ── Additional Options > Limits & Resources Tab ───────────────────

  private renderLimitsTab() {
    return html`
      <div class="field-row">
        <div class="form-field">
          <label>Max Turns</label>
          <sl-input
            type="number"
            placeholder="0 = unlimited"
            .value=${String(this.maxTurns || '')}
            @sl-input=${(e: Event) => {
              this.maxTurns = parseInt((e.target as HTMLElement & { value: string }).value) || 0;
            }}
          ></sl-input>
        </div>
        <div class="form-field">
          <label>Max Model Calls</label>
          <sl-input
            type="number"
            placeholder="0 = unlimited"
            .value=${String(this.maxModelCalls || '')}
            @sl-input=${(e: Event) => {
              this.maxModelCalls =
                parseInt((e.target as HTMLElement & { value: string }).value) || 0;
            }}
          ></sl-input>
        </div>
      </div>

      <div class="form-field">
        <label>Max Duration</label>
        <sl-input
          placeholder="e.g. 30m, 2h"
          .value=${this.maxDuration}
          @sl-input=${(e: Event) => {
            this.maxDuration = (e.target as HTMLElement & { value: string }).value;
          }}
        ></sl-input>
        <div class="hint">Go duration string. Empty means no limit.</div>
      </div>

      <div class="field-row">
        <div class="form-field">
          <label>CPU Request</label>
          <sl-input
            placeholder='e.g. "2", "500m"'
            .value=${this.cpuRequest}
            @sl-input=${(e: Event) => {
              this.cpuRequest = (e.target as HTMLElement & { value: string }).value;
            }}
          ></sl-input>
        </div>
        <div class="form-field">
          <label>Memory Request</label>
          <sl-input
            placeholder='e.g. "4Gi"'
            .value=${this.memoryRequest}
            @sl-input=${(e: Event) => {
              this.memoryRequest = (e.target as HTMLElement & { value: string }).value;
            }}
          ></sl-input>
        </div>
      </div>

      <div class="field-row">
        <div class="form-field">
          <label>CPU Limit</label>
          <sl-input
            placeholder='e.g. "4"'
            .value=${this.cpuLimit}
            @sl-input=${(e: Event) => {
              this.cpuLimit = (e.target as HTMLElement & { value: string }).value;
            }}
          ></sl-input>
        </div>
        <div class="form-field">
          <label>Memory Limit</label>
          <sl-input
            placeholder='e.g. "8Gi"'
            .value=${this.memoryLimit}
            @sl-input=${(e: Event) => {
              this.memoryLimit = (e.target as HTMLElement & { value: string }).value;
            }}
          ></sl-input>
        </div>
      </div>

      <div class="form-field">
        <label>Disk</label>
        <sl-input
          placeholder='e.g. "20Gi"'
          .value=${this.disk}
          @sl-input=${(e: Event) => {
            this.disk = (e.target as HTMLElement & { value: string }).value;
          }}
        ></sl-input>
      </div>
    `;
  }

  // ── Additional Options > Environment & Labels Tab ─────────────────

  private renderEnvironmentTab() {
    return html`
      <!-- Environment Variables -->
      <div class="form-field">
        <label>Environment Variables</label>
        <scion-env-editor
          .entries=${this.envEntries}
          @env-change=${(e: CustomEvent<{ entries: EnvEntry[] }>) => {
            this.envEntries = e.detail.entries;
          }}
        ></scion-env-editor>
      </div>

      <!-- Labels -->
      <div class="form-field" style="margin-top: 1.5rem;">
        <label>Labels</label>
        ${this.labelEntries.map(
          (entry, i) => html`
            <div style="display: flex; gap: 0.5em; margin-bottom: 0.5em; align-items: center;">
              <sl-input
                size="small"
                placeholder="key"
                .value=${entry.key}
                @sl-input=${(e: Event) => {
                  const updated = [...this.labelEntries];
                  updated[i] = {
                    ...updated[i],
                    key: (e.target as HTMLElement & { value: string }).value,
                  };
                  this.labelEntries = updated;
                }}
                style="flex: 1;"
              ></sl-input>
              <sl-input
                size="small"
                placeholder="value"
                .value=${entry.value}
                @sl-input=${(e: Event) => {
                  const updated = [...this.labelEntries];
                  updated[i] = {
                    ...updated[i],
                    value: (e.target as HTMLElement & { value: string }).value,
                  };
                  this.labelEntries = updated;
                }}
                style="flex: 1;"
              ></sl-input>
              <sl-icon-button
                name="x-lg"
                label="Remove"
                @click=${() => {
                  this.labelEntries = this.labelEntries.filter((_, idx) => idx !== i);
                }}
              ></sl-icon-button>
            </div>
          `
        )}
        ${this.labelEntries.length < 16
          ? html`<sl-button
              size="small"
              variant="text"
              @click=${() => {
                this.labelEntries = [...this.labelEntries, { key: '', value: '' }];
              }}
            >
              <sl-icon slot="prefix" name="plus-lg"></sl-icon>
              Add label
            </sl-button>`
          : nothing}
        <div class="hint">Optional key-value labels to organize agents (max 16).</div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-create': ScionPageAgentCreate;
  }
}
