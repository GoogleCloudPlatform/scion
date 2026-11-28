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
 * Hub admin page for System One provider instances (`/admin/system-one`, ptone/scion#8779).
 *
 * Gated on `hub.system_one`: renders `<scion-page-404>` and makes zero API calls
 * while the web flag is off. All authorization, endpoint pinning, secret-name
 * allowlists, and credential rules are enforced server-side by the Hub.
 *
 * Opening, listing, inspecting, or creating instances never contacts a provider.
 * Provider requests occur only via the explicit user-initiated Test dialog.
 */

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';

import type { PageData } from '../../shared/types.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import {
  FEATURE_FLAGS_CHANGED_EVENT,
  SYSTEM_ONE_FLAG,
  isFeatureEnabled,
} from '../../utils/feature-flags.js';
import { formatInstantWithZone } from '../../utils/time.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { srOnlyStyles } from '../shared/styles.js';
import './not-found.js';

const ADAPTERS_PATH = '/api/v1/admin/system-one/adapters';
const INSTANCES_PATH = '/api/v1/admin/system-one/instances';
const PROJECTS_PATH = '/api/v1/projects';

const DEFAULT_PROBE_SPEC = JSON.stringify(
  {
    version: 1,
    questions: [
      {
        id: 'q1',
        type: 'binary',
        question: 'Is this synthetic message a routine connectivity check?',
      },
    ],
    output: {
      question: 'q1',
    },
  },
  null,
  2
);

const DEFAULT_PROBE_TEXT = 'Synthetic connectivity check from Scion System One admin UI.';

export interface SystemOneAdapterAuthMode {
  mode: string;
  authKind: string;
  transportScheme?: string;
  customEndpointAllowed: boolean;
  scopesRequired?: boolean;
}

export interface SystemOneAdapterCapability {
  id: string;
  version?: string;
  displayName?: string;
  providerRetention?: string;
  defaultEndpoint?: string;
  modelRequired: boolean;
  supportsModelListing: boolean;
  authModes: SystemOneAdapterAuthMode[];
  allowedSecretNames?: string[];
  defaultSecretName?: string;
  allowedSecretScopes?: string[];
  defaultSecretScope?: string;
}

export interface SystemOneSecretRef {
  name: string;
  scope?: string;
}

export interface SystemOneInstanceAuth {
  kind: string;
  secretName?: string;
  secretRef?: SystemOneSecretRef;
  headerName?: string;
  scopes?: string[];
  audience?: string;
  quotaProject?: string;
  credentialType?: string;
  serviceAccountEmail?: string;
}

export interface SystemOneInstanceLimits {
  maxInFlight?: number;
  ratePerSec?: number;
  maxInputTokens?: number;
  callTimeout?: string;
}

export interface SystemOneInstance {
  id: string;
  name: string;
  adapter: string;
  model?: string;
  endpoint: string;
  location?: string;
  auth: SystemOneInstanceAuth;
  options?: Record<string, string>;
  limits?: SystemOneInstanceLimits;
  allowPrivateNetwork?: boolean;
  enabled: boolean;
  revision: number;
  createdBy?: string;
  updatedBy?: string;
  createdAt: string;
  updatedAt: string;
}

export interface ProbeSecretRefProvenance {
  name: string;
  scope?: string;
  scope_used?: string;
}

export interface ProbeAuthProvenance {
  auth_kind?: string;
  secret_ref?: ProbeSecretRefProvenance;
  secret_version?: number;
  scopes?: string[];
  quota_project?: string;
  credential_type?: string;
  service_account_email?: string;
  reason_hint?: string;
}

export interface ProbeUsage {
  input_tokens: number;
  output_tokens: number;
  truncated?: boolean;
}

export interface ProbeEvaluation {
  eval_id?: string;
  project_id?: string;
  configured_model?: string;
  resolved_model?: string;
  auth?: ProbeAuthProvenance;
  class: string;
  reason?: string;
  usage?: ProbeUsage;
  notes?: string[];
  elapsed?: number;
}

export interface ProbeDecision {
  kind: string;
  bool?: boolean;
  string?: string;
}

export interface ProbeSystemOneResponse {
  class: string;
  reason?: string;
  models?: string[];
  decision?: ProbeDecision;
  evaluation?: ProbeEvaluation;
}

interface ProjectOption {
  id: string;
  name: string;
  slug?: string;
}

/** Format Go time.Duration (nanoseconds) into a human-readable latency string. */
export function formatElapsedNs(ns?: number): string {
  if (ns === undefined || ns === null || !Number.isFinite(ns) || ns <= 0) {
    return 'Unavailable';
  }
  const ms = ns / 1_000_000;
  if (ms < 1) {
    return '<1 ms';
  }
  if (ms < 1000) {
    return `${Math.round(ms)} ms`;
  }
  return `${(ms / 1000).toFixed(2)} s`;
}

/** Human-readable label for an adapter auth mode. */
export function authModeLabel(mode: string): string {
  switch (mode) {
    case 'owned_api_key':
      return 'Instance-owned API key';
    case 'secret_ref':
      return 'Named secret reference';
    case 'google_access_token':
      return 'ADC (Hub process identity)';
    default:
      return mode;
  }
}

/** Human-readable credential status for a persisted instance (never includes secret values). */
export function formatCredentialStatus(inst: SystemOneInstance): {
  modeLabel: string;
  statusText: string;
} {
  if (inst.auth.kind === 'google_access_token') {
    const credType = inst.auth.credentialType || 'unresolved';
    return {
      modeLabel: 'ADC (Hub process identity)',
      statusText: `ADC (${credType})`,
    };
  }
  if (inst.auth.secretRef) {
    const scope = inst.auth.secretRef.scope || 'project_or_hub';
    return {
      modeLabel: 'Named secret',
      statusText: `Named: ${inst.auth.secretRef.name} (${scope})`,
    };
  }
  return {
    modeLabel: 'Owned API key',
    statusText: 'Instance key set',
  };
}

/** Whether an instance's named secret is no longer in the adapter's effective allowlist. */
export function hasInvalidSecretBinding(
  inst: SystemOneInstance,
  adaptersById: Map<string, SystemOneAdapterCapability>
): boolean {
  if (!inst.auth.secretRef) {
    return false;
  }
  const cap = adaptersById.get(inst.adapter);
  if (!cap) {
    return false;
  }
  const allowed = cap.allowedSecretNames ?? [];
  return !allowed.includes(inst.auth.secretRef.name);
}

/** Whether probing this instance can consult a project-scoped named secret. */
export function instanceNeedsProjectSelectorForTest(inst: SystemOneInstance | null): boolean {
  if (!inst || !inst.auth.secretRef) {
    return false;
  }
  const scope = inst.auth.secretRef.scope || 'project_or_hub';
  return scope === 'project' || scope === 'project_or_hub';
}

@customElement('scion-page-admin-system-one')
export class ScionPageAdminSystemOne extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private flagEnabled = isFeatureEnabled(SYSTEM_ONE_FLAG);
  @state() private loading = true;
  @state() private hubUnavailable = false;
  @state() private forbidden = false;
  @state() private error: string | null = null;

  @state() private adapters: SystemOneAdapterCapability[] = [];
  @state() private instances: SystemOneInstance[] = [];
  @state() private hasMoreInstances = false;

  // Inspect dialog state
  @state() private inspectDialogOpen = false;
  @state() private inspectLoading = false;
  @state() private inspectError: string | null = null;
  @state() private inspectInstance: SystemOneInstance | null = null;

  // Create dialog state
  @state() private createDialogOpen = false;
  @state() private createName = '';
  @state() private createAdapterId = '';
  @state() private createModel = '';
  @state() private createAuthMode = '';
  @state() private createApiKey = '';
  @state() private createSecretName = '';
  @state() private createSecretScope = 'project_or_hub';
  @state() private createScopesText = '';
  @state() private createQuotaProject = '';
  @state() private createEndpoint = '';
  @state() private createEnabled = true;
  @state() private createSubmitting = false;
  @state() private createError: string | null = null;

  // Test (probe) dialog state
  @state() private testDialogOpen = false;
  @state() private testInstance: SystemOneInstance | null = null;
  @state() private testSpecText = DEFAULT_PROBE_SPEC;
  @state() private testProbeText = DEFAULT_PROBE_TEXT;
  @state() private testProjectId = '';
  @state() private testProjects: ProjectOption[] = [];
  @state() private testProjectsLoading = false;
  @state() private testProjectsError: string | null = null;
  @state() private testRunning = false;
  @state() private testError: string | null = null;
  @state() private testResult: ProbeSystemOneResponse | null = null;
  private testRequestSeq = 0;
  private testAbortController: AbortController | null = null;

  // Delete dialog state
  @state() private deleteDialogOpen = false;
  @state() private deleteInstance: SystemOneInstance | null = null;
  @state() private deleteSubmitting = false;
  @state() private deleteError: string | null = null;

  private readonly onFeatureFlagsChanged = (): void => {
    const next = isFeatureEnabled(SYSTEM_ONE_FLAG);
    if (next !== this.flagEnabled) {
      this.flagEnabled = next;
      if (next) {
        void this.loadData();
      }
    }
  };

  static override styles = [
    srOnlyStyles,
    css`
      :host {
        display: block;
      }

      .header {
        display: flex;
        align-items: flex-start;
        justify-content: space-between;
        gap: 1rem;
        margin-bottom: 1.5rem;
        flex-wrap: wrap;
      }

      .header h1 {
        font-size: 1.5rem;
        font-weight: 700;
        color: var(--scion-text, #1e293b);
        margin: 0 0 0.25rem 0;
      }

      .header-subtitle {
        font-size: 0.875rem;
        color: var(--scion-text-muted, #64748b);
        margin: 0;
        max-width: 48rem;
        line-height: 1.5;
      }

      .header-actions {
        display: flex;
        gap: 0.5rem;
        align-items: center;
      }

      .table-container {
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        overflow-x: auto;
      }

      table {
        width: 100%;
        table-layout: fixed;
        border-collapse: collapse;
      }

      th {
        text-align: left;
        padding: 0.625rem 0.5rem;
        font-size: 0.7rem;
        font-weight: 600;
        text-transform: uppercase;
        letter-spacing: 0.04em;
        color: var(--scion-text-muted, #64748b);
        background: var(--scion-bg-subtle, #f1f5f9);
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        white-space: normal;
        line-height: 1.3;
      }

      td {
        padding: 0.625rem 0.5rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        color: var(--scion-text, #1e293b);
        font-size: 0.8125rem;
        vertical-align: top;
        overflow-wrap: break-word;
      }

      tr:last-child td {
        border-bottom: none;
      }

      tr:hover td {
        background: var(--scion-bg-subtle, #f8fafc);
      }

      .instance-name-btn {
        background: none;
        border: none;
        padding: 0;
        font: inherit;
        font-weight: 600;
        color: var(--scion-primary, #3b82f6);
        cursor: pointer;
        text-align: left;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
        max-width: 100%;
      }

      .instance-name-btn:hover {
        text-decoration: underline;
      }

      .mono {
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.75rem;
      }

      .endpoint-cell sl-tooltip {
        display: block;
        max-width: 100%;
      }

      .endpoint-text {
        display: block;
        width: 100%;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }

      .actions-cell {
        white-space: nowrap;
      }

      .muted {
        color: var(--scion-text-muted, #64748b);
        font-size: 0.75rem;
      }

      .cell-stack {
        display: flex;
        flex-direction: column;
        gap: 0.2rem;
        min-width: 0;
      }

      .row-actions {
        display: flex;
        gap: 0.25rem;
        flex-wrap: nowrap;
        align-items: center;
      }

      .row-actions sl-button::part(base) {
        padding: 0;
        font-size: 0.75rem;
      }

      .row-actions sl-button::part(label) {
        padding: 0 0.45rem;
      }

      .loading-state,
      .empty-state,
      .error-state {
        display: flex;
        flex-direction: column;
        align-items: center;
        justify-content: center;
        padding: 3rem 1.5rem;
        text-align: center;
        background: var(--scion-surface, #ffffff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: var(--scion-radius-lg, 0.75rem);
        color: var(--scion-text-muted, #64748b);
      }

      .empty-state > sl-icon,
      .error-state > sl-icon {
        font-size: 2.5rem;
        margin-bottom: 0.75rem;
        opacity: 0.6;
      }

      .empty-state h2,
      .error-state h2 {
        font-size: 1.125rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
        margin: 0 0 0.5rem 0;
      }

      .empty-state p,
      .error-state p {
        margin: 0 0 1.25rem 0;
        max-width: 34rem;
        line-height: 1.5;
      }

      .form-fields {
        display: flex;
        flex-direction: column;
        gap: 1rem;
      }

      .field-note {
        font-size: 0.8125rem;
        color: var(--scion-text-muted, #64748b);
        line-height: 1.45;
        margin: 0.25rem 0 0 0;
      }

      .notice-box {
        padding: 0.75rem 1rem;
        border-radius: var(--scion-radius, 0.5rem);
        border: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-bg-subtle, #f8fafc);
        font-size: 0.8125rem;
        color: var(--scion-text, #1e293b);
        line-height: 1.5;
      }

      .notice-box.warning {
        border-color: var(--sl-color-warning-300, #fcd34d);
        background: var(--sl-color-warning-50, #fffbeb);
      }

      .notice-box.danger {
        border-color: var(--sl-color-danger-300, #fca5a5);
        background: var(--sl-color-danger-50, #fef2f2);
        color: var(--sl-color-danger-800, #991b1b);
      }

      .detail-grid {
        display: grid;
        grid-template-columns: 11rem 1fr;
        gap: 0.625rem 1rem;
        font-size: 0.875rem;
      }

      .detail-label {
        color: var(--scion-text-muted, #64748b);
        font-weight: 500;
      }

      .detail-value {
        color: var(--scion-text, #1e293b);
        word-break: break-word;
      }

      .scope-list {
        margin: 0;
        padding-left: 1.125rem;
        font-family: var(--scion-font-mono, monospace);
        font-size: 0.8125rem;
      }

      .probe-result {
        margin-top: 1rem;
        padding: 1rem;
        border-radius: var(--scion-radius, 0.5rem);
        border: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-bg-subtle, #f8fafc);
      }

      .probe-result-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        gap: 0.5rem;
        margin-bottom: 0.75rem;
      }

      .probe-result-header h3 {
        margin: 0;
        font-size: 0.9375rem;
        font-weight: 600;
        color: var(--scion-text, #1e293b);
      }

      .badge-row {
        display: flex;
        gap: 0.375rem;
        flex-wrap: wrap;
        align-items: center;
      }

      .dialog-footer {
        display: flex;
        justify-content: flex-end;
        gap: 0.5rem;
      }

      sl-dialog::part(panel) {
        max-width: 42rem;
        width: 100%;
      }
    `,
  ];

  override connectedCallback(): void {
    super.connectedCallback();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFeatureFlagsChanged);
    this.flagEnabled = isFeatureEnabled(SYSTEM_ONE_FLAG);
    if (this.flagEnabled) {
      void this.loadData();
    }
  }

  override disconnectedCallback(): void {
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.onFeatureFlagsChanged);
    this.testAbortController?.abort();
    this.testAbortController = null;
    this.testRequestSeq++;
    super.disconnectedCallback();
  }

  private get adaptersById(): Map<string, SystemOneAdapterCapability> {
    return new Map(this.adapters.map((a) => [a.id, a]));
  }

  private get selectedCreateAdapter(): SystemOneAdapterCapability | undefined {
    return this.adapters.find((a) => a.id === this.createAdapterId);
  }

  private get selectedCreateAuthMode(): SystemOneAdapterAuthMode | undefined {
    return this.selectedCreateAdapter?.authModes.find((m) => m.mode === this.createAuthMode);
  }

  private async loadData(): Promise<void> {
    this.loading = true;
    this.error = null;
    this.hubUnavailable = false;
    this.forbidden = false;
    this.hasMoreInstances = false;

    try {
      const [adaptersRes, firstInstancesRes] = await Promise.all([
        apiFetch(ADAPTERS_PATH),
        apiFetch(INSTANCES_PATH),
      ]);

      if (adaptersRes.status === 404 || firstInstancesRes.status === 404) {
        this.hubUnavailable = true;
        return;
      }
      if (adaptersRes.status === 403 || firstInstancesRes.status === 403) {
        this.forbidden = true;
        return;
      }
      if (!adaptersRes.ok) {
        throw new Error(
          await extractApiError(adaptersRes, `Failed to load adapters (HTTP ${adaptersRes.status})`)
        );
      }
      if (!firstInstancesRes.ok) {
        throw new Error(
          await extractApiError(
            firstInstancesRes,
            `Failed to load instances (HTTP ${firstInstancesRes.status})`
          )
        );
      }

      const adaptersBody = (await adaptersRes.json()) as {
        adapters?: SystemOneAdapterCapability[];
      };
      const firstBody = (await firstInstancesRes.json()) as {
        instances?: SystemOneInstance[];
        nextCursor?: string;
      };

      const collected: SystemOneInstance[] = [...(firstBody.instances ?? [])];
      let cursor = firstBody.nextCursor ?? '';
      const seenCursors = new Set<string>();
      const maxPages = 20;
      let pages = 1;

      while (cursor && !seenCursors.has(cursor) && pages < maxPages) {
        seenCursors.add(cursor);
        const pageRes = await apiFetch(
          `${INSTANCES_PATH}?cursor=${encodeURIComponent(cursor)}&limit=200`
        );
        if (!pageRes.ok) {
          throw new Error(
            await extractApiError(pageRes, `Failed to load instances (HTTP ${pageRes.status})`)
          );
        }
        const pageBody = (await pageRes.json()) as {
          instances?: SystemOneInstance[];
          nextCursor?: string;
        };
        if (pageBody.instances) {
          collected.push(...pageBody.instances);
        }
        cursor = pageBody.nextCursor ?? '';
        pages++;
      }

      this.adapters = adaptersBody.adapters ?? [];
      this.instances = collected;
      this.hasMoreInstances = Boolean(cursor);
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load System One providers';
    } finally {
      this.loading = false;
    }
  }

  private async openInspect(inst: SystemOneInstance): Promise<void> {
    this.inspectDialogOpen = true;
    this.inspectLoading = true;
    this.inspectError = null;
    this.inspectInstance = inst;

    try {
      const res = await apiFetch(`${INSTANCES_PATH}/${encodeURIComponent(inst.name)}`);
      if (!res.ok) {
        throw new Error(
          await extractApiError(res, `Failed to load instance details (HTTP ${res.status})`)
        );
      }
      this.inspectInstance = (await res.json()) as SystemOneInstance;
    } catch (err) {
      this.inspectError = err instanceof Error ? err.message : 'Failed to inspect instance';
    } finally {
      this.inspectLoading = false;
    }
  }

  private closeInspect(): void {
    this.inspectDialogOpen = false;
    this.inspectInstance = null;
    this.inspectError = null;
  }

  private openCreate(): void {
    this.createName = '';
    this.createModel = '';
    this.createApiKey = '';
    this.createScopesText = '';
    this.createQuotaProject = '';
    this.createEnabled = true;
    this.createError = null;
    this.createSubmitting = false;

    const firstAdapter = this.adapters[0];
    if (firstAdapter) {
      this.applyCreateAdapter(firstAdapter.id);
    } else {
      this.createAdapterId = '';
      this.createAuthMode = '';
      this.createEndpoint = '';
      this.createSecretName = '';
      this.createSecretScope = 'project_or_hub';
    }

    this.createDialogOpen = true;
  }

  private closeCreate(): void {
    this.createDialogOpen = false;
    // Clear write-only credential immediately when closing.
    this.createApiKey = '';
    this.createError = null;
  }

  private applyCreateAdapter(adapterId: string): void {
    this.createAdapterId = adapterId;
    const cap = this.adapters.find((a) => a.id === adapterId);
    if (!cap) {
      this.createAuthMode = '';
      this.createEndpoint = '';
      this.createSecretName = '';
      this.createSecretScope = 'project_or_hub';
      return;
    }
    const firstMode = cap.authModes[0]?.mode ?? '';
    this.createAuthMode = firstMode;
    this.createEndpoint = cap.defaultEndpoint ?? '';
    this.createSecretName = cap.defaultSecretName ?? cap.allowedSecretNames?.[0] ?? '';
    this.createSecretScope = cap.defaultSecretScope ?? 'project_or_hub';
    this.createApiKey = '';
    this.createScopesText = '';
    this.createQuotaProject = '';
  }

  private handleCreateAuthModeChange(mode: string): void {
    this.createAuthMode = mode;
    const cap = this.selectedCreateAdapter;
    const modeCap = cap?.authModes.find((m) => m.mode === mode);
    if (modeCap && !modeCap.customEndpointAllowed) {
      this.createEndpoint = cap?.defaultEndpoint ?? '';
    }
    if (mode !== 'owned_api_key') {
      this.createApiKey = '';
    }
  }

  private async submitCreate(e: Event): Promise<void> {
    e.preventDefault();
    if (this.createSubmitting) return;

    const cap = this.selectedCreateAdapter;
    const modeCap = this.selectedCreateAuthMode;
    if (!cap || !modeCap) {
      this.createError = 'Select a registered adapter and authentication mode.';
      return;
    }

    const name = this.createName.trim();
    if (!name) {
      this.createError = 'Instance name is required.';
      return;
    }

    const model = this.createModel.trim();
    if (cap.modelRequired && !model) {
      this.createError = `Model is required for adapter "${cap.id}".`;
      return;
    }

    const body: Record<string, unknown> = {
      name,
      adapter: cap.id,
      enabled: this.createEnabled,
    };
    if (model) {
      body.model = model;
    }

    const endpoint = this.createEndpoint.trim();
    if (modeCap.customEndpointAllowed && endpoint) {
      body.endpoint = endpoint;
    } else if (cap.defaultEndpoint) {
      body.endpoint = cap.defaultEndpoint;
    }

    if (modeCap.mode === 'owned_api_key') {
      const apiKey = this.createApiKey;
      if (!apiKey.trim()) {
        this.createError = 'API key is required for instance-owned API key authentication.';
        return;
      }
      body.auth = {
        kind: 'api_key',
        apiKey,
      };
    } else if (modeCap.mode === 'secret_ref') {
      if (!this.createSecretName) {
        this.createError = 'Select an allowed secret name.';
        return;
      }
      body.auth = {
        kind: 'api_key',
        secretRef: {
          name: this.createSecretName,
          scope: this.createSecretScope || cap.defaultSecretScope || 'project_or_hub',
        },
      };
    } else if (modeCap.mode === 'google_access_token') {
      const scopes = this.createScopesText
        .split(/[\s,]+/)
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
      if (scopes.length === 0) {
        this.createError = 'At least one OAuth2 scope is required for ADC authentication.';
        return;
      }
      body.auth = {
        kind: 'google_access_token',
        scopes,
      };
      const qp = this.createQuotaProject.trim();
      if (qp) {
        body.options = { quota_project: qp };
      }
    }

    this.createSubmitting = true;
    this.createError = null;
    // Clear write-only credential from component state immediately on submit.
    this.createApiKey = '';

    try {
      const res = await apiFetch(INSTANCES_PATH, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        throw new Error(
          await extractApiError(res, `Failed to create instance (HTTP ${res.status})`)
        );
      }
      this.createDialogOpen = false;
      await this.loadData();
    } catch (err) {
      this.createError = err instanceof Error ? err.message : 'Failed to create instance';
    } finally {
      this.createSubmitting = false;
    }
  }

  private openTest(inst: SystemOneInstance): void {
    this.testAbortController?.abort();
    this.testAbortController = null;
    this.testRequestSeq++;
    this.testInstance = inst;
    this.testSpecText = DEFAULT_PROBE_SPEC;
    this.testProbeText = DEFAULT_PROBE_TEXT;
    this.testProjectId = '';
    this.testProjects = [];
    this.testProjectsError = null;
    this.testError = null;
    this.testResult = null;
    this.testRunning = false;
    this.testDialogOpen = true;

    if (instanceNeedsProjectSelectorForTest(inst)) {
      void this.loadVisibleProjects();
    }
  }

  private closeTest(): void {
    this.testAbortController?.abort();
    this.testAbortController = null;
    this.testRequestSeq++;
    this.testDialogOpen = false;
    this.testInstance = null;
    this.testRunning = false;
    this.testResult = null;
    this.testError = null;
  }

  private async loadVisibleProjects(): Promise<void> {
    this.testProjectsLoading = true;
    this.testProjectsError = null;
    try {
      const res = await apiFetch(PROJECTS_PATH);
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Failed to load projects (HTTP ${res.status})`));
      }
      const data = (await res.json()) as {
        projects?: ProjectOption[];
      };
      this.testProjects = data.projects ?? [];
    } catch (err) {
      this.testProjectsError = err instanceof Error ? err.message : 'Failed to load projects';
    } finally {
      this.testProjectsLoading = false;
    }
  }

  private async runTest(): Promise<void> {
    const inst = this.testInstance;
    if (!inst || this.testRunning) return;

    let parsedSpec: unknown;
    try {
      parsedSpec = JSON.parse(this.testSpecText);
    } catch {
      this.testError = 'Synthetic spec must be valid JSON.';
      return;
    }

    if (instanceNeedsProjectSelectorForTest(inst) && !this.testProjectId) {
      this.testError = 'Select a project to test a project-scoped named secret.';
      return;
    }

    const payload: Record<string, unknown> = {
      spec: parsedSpec,
      probe: { text: this.testProbeText },
    };
    if (this.testProjectId) {
      payload.project = this.testProjectId;
    }

    this.testAbortController?.abort();
    const controller = new AbortController();
    this.testAbortController = controller;
    const seq = ++this.testRequestSeq;
    const targetName = inst.name;

    this.testRunning = true;
    this.testError = null;
    this.testResult = null;

    try {
      const res = await apiFetch(`${INSTANCES_PATH}/${encodeURIComponent(targetName)}/probe`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
        signal: controller.signal,
      });
      if (
        seq !== this.testRequestSeq ||
        !this.testDialogOpen ||
        this.testInstance?.name !== targetName
      ) {
        return;
      }
      if (!res.ok) {
        throw new Error(await extractApiError(res, `Test request failed (HTTP ${res.status})`));
      }
      const body = (await res.json()) as ProbeSystemOneResponse;
      if (
        seq !== this.testRequestSeq ||
        !this.testDialogOpen ||
        this.testInstance?.name !== targetName
      ) {
        return;
      }
      this.testResult = body;
    } catch (err) {
      if (
        seq !== this.testRequestSeq ||
        !this.testDialogOpen ||
        this.testInstance?.name !== targetName ||
        (err instanceof DOMException && err.name === 'AbortError')
      ) {
        return;
      }
      this.testError = err instanceof Error ? err.message : 'Test request failed';
    } finally {
      if (seq === this.testRequestSeq) {
        this.testRunning = false;
        this.testAbortController = null;
      }
    }
  }

  private openDelete(inst: SystemOneInstance): void {
    this.deleteInstance = inst;
    this.deleteError = null;
    this.deleteSubmitting = false;
    this.deleteDialogOpen = true;
  }

  private closeDelete(): void {
    this.deleteDialogOpen = false;
    this.deleteInstance = null;
    this.deleteError = null;
  }

  private async confirmDelete(): Promise<void> {
    const inst = this.deleteInstance;
    if (!inst || this.deleteSubmitting) return;

    this.deleteSubmitting = true;
    this.deleteError = null;
    try {
      const res = await apiFetch(`${INSTANCES_PATH}/${encodeURIComponent(inst.name)}`, {
        method: 'DELETE',
      });
      if (!res.ok) {
        throw new Error(
          await extractApiError(res, `Failed to delete instance (HTTP ${res.status})`)
        );
      }
      this.deleteDialogOpen = false;
      this.deleteInstance = null;
      await this.loadData();
    } catch (err) {
      this.deleteError = err instanceof Error ? err.message : 'Failed to delete instance';
    } finally {
      this.deleteSubmitting = false;
    }
  }

  override render(): TemplateResult {
    if (!this.flagEnabled || this.hubUnavailable) {
      return html`<scion-page-404 .pageData=${this.pageData}></scion-page-404>`;
    }

    if (this.forbidden) {
      return html`
        <div class="error-state" role="alert">
          <sl-icon name="shield-x"></sl-icon>
          <h2>Access Denied</h2>
          <p>
            You do not have permission to administer System One provider instances (<code
              >system_one_instance.read</code
            >
            required).
          </p>
        </div>
      `;
    }

    return html`
      <div class="header">
        <div>
          <h1>System One Providers</h1>
          <p class="header-subtitle">
            Hub-managed provider instances for System One evaluations. Listing or inspecting
            instances never contacts a provider or reveals credential values; use the explicit
            <strong>Test</strong> action to send a single synthetic request.
          </p>
        </div>
        <div class="header-actions">
          <sl-button
            variant="default"
            size="small"
            ?disabled=${this.loading}
            @click=${() => void this.loadData()}
          >
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Refresh
          </sl-button>
          <sl-button
            variant="primary"
            size="small"
            ?disabled=${this.loading || this.adapters.length === 0}
            @click=${() => this.openCreate()}
          >
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add Provider Instance
          </sl-button>
        </div>
      </div>

      ${this.renderBody()} ${this.renderInspectDialog()} ${this.renderCreateDialog()}
      ${this.renderTestDialog()} ${this.renderDeleteDialog()}
    `;
  }

  private renderBody(): TemplateResult {
    if (this.loading) {
      return html`
        <div class="loading-state">
          <sl-spinner style="font-size: 2rem; margin-bottom: 0.75rem;"></sl-spinner>
          <div>Loading System One provider instances…</div>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state" role="alert">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Unable to load providers</h2>
          <p>${this.error}</p>
          <sl-button variant="primary" size="small" @click=${() => void this.loadData()}>
            Retry
          </sl-button>
        </div>
      `;
    }

    if (this.instances.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="robot"></sl-icon>
          <h2>No provider instances configured</h2>
          <p>
            Configure a System One provider instance to make an adapter and model available for
            explicit admin tests. CEL-only rules continue to work without any provider configured.
          </p>
          <sl-button
            variant="primary"
            size="small"
            ?disabled=${this.adapters.length === 0}
            @click=${() => this.openCreate()}
          >
            <sl-icon slot="prefix" name="plus-lg"></sl-icon>
            Add Provider Instance
          </sl-button>
        </div>
      `;
    }

    const byId = this.adaptersById;

    return html`
      ${this.hasMoreInstances
        ? html`
            <div class="notice-box" role="status" style="margin-bottom: 0.75rem;">
              Showing the first ${this.instances.length} provider instances; additional instances
              exist on the Hub.
            </div>
          `
        : nothing}
      <div class="table-container">
        <table aria-label="System One provider instances">
          <caption class="sr-only">
            System One provider instances
          </caption>
          <colgroup>
            <col style="width: 13%;" />
            <col style="width: 12%;" />
            <col style="width: 13%;" />
            <col style="width: 18%;" />
            <col style="width: 21%;" />
            <col style="width: 23%;" />
          </colgroup>
          <thead>
            <tr>
              <th>Name</th>
              <th>Adapter</th>
              <th>Configured Model</th>
              <th>Endpoint</th>
              <th>Auth &amp; Credential Status</th>
              <th>Actions</th>
            </tr>
          </thead>
          <tbody>
            ${this.instances.map((inst) => {
              const cred = formatCredentialStatus(inst);
              const invalidBinding = hasInvalidSecretBinding(inst, byId);
              const adapterCap = byId.get(inst.adapter);
              return html`
                <tr data-instance-name=${inst.name}>
                  <td>
                    <div class="cell-stack">
                      <button
                        type="button"
                        class="instance-name-btn"
                        title=${inst.name}
                        @click=${() => void this.openInspect(inst)}
                      >
                        ${inst.name}
                      </button>
                      <div class="badge-row">
                        <sl-badge variant=${inst.enabled ? 'neutral' : 'warning'} pill>
                          ${inst.enabled ? 'Enabled' : 'Disabled'}
                        </sl-badge>
                      </div>
                    </div>
                  </td>
                  <td>
                    <div class="cell-stack">
                      <span>${adapterCap?.displayName || inst.adapter}</span>
                      <span class="mono muted">${inst.adapter}</span>
                    </div>
                  </td>
                  <td>
                    <span class="mono">${inst.model || '—'}</span>
                  </td>
                  <td class="endpoint-cell">
                    <sl-tooltip content=${inst.endpoint} hoist>
                      <span class="mono endpoint-text" title=${inst.endpoint}>
                        ${inst.endpoint}
                      </span>
                    </sl-tooltip>
                  </td>
                  <td>
                    <div class="cell-stack">
                      <span>${cred.modeLabel}</span>
                      <span class="muted">${cred.statusText}</span>
                      ${invalidBinding
                        ? html`
                            <div class="badge-row">
                              <sl-badge variant="danger" pill>Invalid binding</sl-badge>
                              <span class="muted">
                                Secret name is no longer permitted by Hub allowlist
                              </span>
                            </div>
                          `
                        : nothing}
                    </div>
                  </td>
                  <td class="actions-cell">
                    <div class="row-actions">
                      <sl-button
                        size="small"
                        variant="default"
                        @click=${() => void this.openInspect(inst)}
                      >
                        Inspect<span class="sr-only"> ${inst.name}</span>
                      </sl-button>
                      <sl-button size="small" variant="default" @click=${() => this.openTest(inst)}>
                        Test<span class="sr-only"> ${inst.name}</span>
                      </sl-button>
                      <sl-button
                        size="small"
                        variant="danger"
                        outline
                        @click=${() => this.openDelete(inst)}
                      >
                        Delete<span class="sr-only"> ${inst.name}</span>
                      </sl-button>
                    </div>
                  </td>
                </tr>
              `;
            })}
          </tbody>
        </table>
      </div>
    `;
  }

  private renderInspectDialog(): TemplateResult {
    const inst = this.inspectInstance;
    const invalidBinding = inst ? hasInvalidSecretBinding(inst, this.adaptersById) : false;

    return html`
      <sl-dialog
        label=${inst ? `Inspect Provider Instance: ${inst.name}` : 'Inspect Provider Instance'}
        ?open=${this.inspectDialogOpen}
        @sl-after-hide=${(e: Event) => {
          if (e.target === e.currentTarget) this.closeInspect();
        }}
      >
        ${this.inspectLoading
          ? html`
              <div style="display:flex;align-items:center;gap:0.75rem;padding:1rem 0;">
                <sl-spinner></sl-spinner>
                <span>Loading instance details…</span>
              </div>
            `
          : nothing}
        ${this.inspectError
          ? html`<div class="notice-box danger" role="alert">${this.inspectError}</div>`
          : nothing}
        ${inst
          ? html`
              <div class="detail-grid">
                <div class="detail-label">Name</div>
                <div class="detail-value mono">${inst.name}</div>

                <div class="detail-label">ID</div>
                <div class="detail-value mono">${inst.id}</div>

                <div class="detail-label">Adapter</div>
                <div class="detail-value mono">${inst.adapter}</div>

                <div class="detail-label">Configured Model</div>
                <div class="detail-value mono">${inst.model || '—'}</div>

                <div class="detail-label">Endpoint</div>
                <div class="detail-value mono">${inst.endpoint}</div>

                <div class="detail-label">Status</div>
                <div class="detail-value">
                  ${inst.enabled ? 'Enabled' : 'Disabled'} (revision ${inst.revision})
                </div>

                <div class="detail-label">Auth Mode</div>
                <div class="detail-value">${formatCredentialStatus(inst).modeLabel}</div>

                ${this.renderInspectAuthFields(inst, invalidBinding)}

                <div class="detail-label">Created</div>
                <div class="detail-value">
                  ${inst.createdAt ? formatInstantWithZone(inst.createdAt) : '—'}
                </div>

                <div class="detail-label">Updated</div>
                <div class="detail-value">
                  ${inst.updatedAt ? formatInstantWithZone(inst.updatedAt) : '—'}
                </div>
              </div>
            `
          : nothing}
        <div slot="footer" class="dialog-footer">
          <sl-button variant="default" @click=${() => this.closeInspect()}>Close</sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderInspectAuthFields(
    inst: SystemOneInstance,
    invalidBinding: boolean
  ): TemplateResult {
    if (inst.auth.kind === 'google_access_token') {
      return html`
        <div class="detail-label">Identity</div>
        <div class="detail-value">
          Hub process identity (not browser user or calling agent identity)
        </div>

        <div class="detail-label">Credential Type</div>
        <div class="detail-value mono">${inst.auth.credentialType || 'unresolved'}</div>

        ${inst.auth.serviceAccountEmail
          ? html`
              <div class="detail-label">Service Account</div>
              <div class="detail-value mono">${inst.auth.serviceAccountEmail}</div>
            `
          : nothing}

        <div class="detail-label">Quota Project</div>
        <div class="detail-value mono">
          ${inst.auth.quotaProject || 'implicit (service account association)'}
        </div>

        <div class="detail-label">OAuth2 Scopes</div>
        <div class="detail-value">
          ${inst.auth.scopes && inst.auth.scopes.length > 0
            ? html`
                <ul class="scope-list">
                  ${inst.auth.scopes.map((s) => html`<li>${s}</li>`)}
                </ul>
              `
            : '—'}
        </div>
      `;
    }

    if (inst.auth.secretRef) {
      return html`
        <div class="detail-label">Named Secret</div>
        <div class="detail-value">
          <span class="mono">${inst.auth.secretRef.name}</span>
          ${invalidBinding
            ? html`
                <div class="notice-box danger" style="margin-top:0.375rem;">
                  Invalid binding: <code>${inst.auth.secretRef.name}</code> is no longer in the
                  Hub's effective allowlist for <code>${inst.adapter}</code>. Evaluations fail
                  closed without substituting another secret.
                </div>
              `
            : nothing}
        </div>

        <div class="detail-label">Secret Scope</div>
        <div class="detail-value mono">${inst.auth.secretRef.scope || 'project_or_hub'}</div>
      `;
    }

    return html`
      <div class="detail-label">Credential Status</div>
      <div class="detail-value">
        Instance key set
        ${inst.auth.secretName
          ? html`<span class="muted">
              (internal ref: <span class="mono">${inst.auth.secretName}</span>)</span
            >`
          : nothing}
      </div>
    `;
  }

  private renderCreateDialog(): TemplateResult {
    const cap = this.selectedCreateAdapter;
    const modeCap = this.selectedCreateAuthMode;
    const endpointLocked = modeCap ? !modeCap.customEndpointAllowed : false;

    return html`
      <sl-dialog
        label="Add System One Provider Instance"
        ?open=${this.createDialogOpen}
        @sl-after-hide=${(e: Event) => {
          if (e.target === e.currentTarget) this.closeCreate();
        }}
      >
        <form
          id="create-system-one-form"
          class="form-fields"
          @submit=${(e: Event) => void this.submitCreate(e)}
        >
          ${this.createError
            ? html`<div class="notice-box danger" role="alert">${this.createError}</div>`
            : nothing}

          <sl-input
            label="Instance Name"
            name="name"
            required
            placeholder="e.g. gemini-primary"
            help-text="Lowercase alphanumeric slug (1–63 characters, hyphens allowed)."
            .value=${this.createName}
            @sl-input=${(e: Event) => {
              this.createName = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          <sl-select
            label="Adapter"
            name="adapter"
            hoist
            required
            .value=${this.createAdapterId}
            @sl-change=${(e: Event) => {
              this.applyCreateAdapter((e.target as HTMLSelectElement).value);
            }}
          >
            ${this.adapters.map(
              (a) => html`
                <sl-option value=${a.id}>
                  ${a.displayName ? `${a.displayName} (${a.id})` : a.id}
                </sl-option>
              `
            )}
          </sl-select>

          ${cap?.providerRetention
            ? html`<p class="field-note">
                <strong>Provider retention:</strong> ${cap.providerRetention}
              </p>`
            : nothing}

          <sl-input
            label=${cap?.modelRequired ? 'Configured Model (Required)' : 'Configured Model'}
            name="model"
            ?required=${Boolean(cap?.modelRequired)}
            placeholder=${cap?.id === 'gemini_decisions'
              ? 'e.g. gemini-decision-1'
              : 'e.g. jev-1.14.0'}
            .value=${this.createModel}
            @sl-input=${(e: Event) => {
              this.createModel = (e.target as HTMLInputElement).value;
            }}
          ></sl-input>

          ${cap && cap.authModes.length > 0
            ? html`
                <sl-radio-group
                  label="Authentication Mode"
                  name="authMode"
                  .value=${this.createAuthMode}
                  @sl-change=${(e: Event) => {
                    this.handleCreateAuthModeChange((e.target as HTMLInputElement).value);
                  }}
                >
                  ${cap.authModes.map(
                    (m) => html`
                      <sl-radio value=${m.mode}>
                        ${authModeLabel(m.mode)}
                        ${m.transportScheme
                          ? html` <span class="muted">(${m.transportScheme})</span>`
                          : nothing}
                      </sl-radio>
                    `
                  )}
                </sl-radio-group>
              `
            : nothing}
          ${this.renderCreateAuthFields(cap, modeCap)}

          <div>
            <sl-input
              label="Endpoint"
              name="endpoint"
              type="url"
              ?readonly=${endpointLocked}
              ?disabled=${endpointLocked}
              placeholder=${cap?.defaultEndpoint || 'https://...'}
              .value=${this.createEndpoint}
              @sl-input=${(e: Event) => {
                this.createEndpoint = (e.target as HTMLInputElement).value;
              }}
            ></sl-input>
            ${endpointLocked
              ? html`
                  <p class="field-note">
                    ${modeCap?.mode === 'google_access_token'
                      ? 'ADC instances are pinned to the adapter’s canonical default endpoint to protect Hub-host credentials. Custom endpoints require an instance-owned API key. Enforced by the Hub.'
                      : 'Named-secret instances are pinned to the adapter’s canonical default endpoint to protect shared Hub/project secrets. Custom endpoints require an instance-owned API key. Enforced by the Hub.'}
                  </p>
                `
              : html`
                  <p class="field-note">
                    Defaults to the adapter’s canonical HTTPS endpoint. Custom HTTPS endpoints are
                    permitted only with an instance-owned API key and remain subject to Hub egress
                    policy.
                  </p>
                `}
          </div>

          <sl-switch
            ?checked=${this.createEnabled}
            @sl-change=${(e: Event) => {
              this.createEnabled = (e.target as HTMLInputElement).checked;
            }}
          >
            Enabled
          </sl-switch>

          <div class="notice-box">
            <strong>Configuration check only:</strong> Creating an instance validates static
            configuration and (for <code>hub</code> or <code>project_or_hub</code> named refs) any
            existing Hub secret type, but
            <strong>does not validate project secrets or call the provider</strong>. All
            restrictions are enforced by the Hub.
          </div>
        </form>

        <div slot="footer" class="dialog-footer">
          <sl-button variant="default" @click=${() => this.closeCreate()}>Cancel</sl-button>
          <sl-button
            variant="primary"
            type="submit"
            form="create-system-one-form"
            ?loading=${this.createSubmitting}
            ?disabled=${this.createSubmitting || !cap || !modeCap}
          >
            Create Instance
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderCreateAuthFields(
    cap: SystemOneAdapterCapability | undefined,
    modeCap: SystemOneAdapterAuthMode | undefined
  ): TemplateResult | typeof nothing {
    if (!cap || !modeCap) return nothing;

    if (modeCap.mode === 'owned_api_key') {
      return html`
        <sl-input
          label="API Key (Write-Only)"
          name="apiKey"
          type="password"
          password-toggle
          autocomplete="new-password"
          required
          placeholder="Enter provider API key"
          help-text="Stored as a Hub-scoped internal secret owned by this instance. Never echoed, returned, or stored in browser storage."
          .value=${this.createApiKey}
          @sl-input=${(e: Event) => {
            this.createApiKey = (e.target as HTMLInputElement).value;
          }}
        ></sl-input>
      `;
    }

    if (modeCap.mode === 'secret_ref') {
      const allowedNames = cap.allowedSecretNames ?? [];
      const allowedScopes = cap.allowedSecretScopes ?? ['hub', 'project', 'project_or_hub'];
      return html`
        <sl-select
          label="Allowed Secret Name"
          name="secretName"
          hoist
          required
          help-text="Restricted to Hub-approved secret names for this adapter (configured by the host operator). Only environment and on_demand secrets are permitted; internal and file secrets are refused."
          .value=${this.createSecretName}
          @sl-change=${(e: Event) => {
            this.createSecretName = (e.target as HTMLSelectElement).value;
          }}
        >
          ${allowedNames.map((n) => html`<sl-option value=${n}>${n}</sl-option>`)}
        </sl-select>

        <sl-select
          label="Secret Resolution Scope"
          name="secretScope"
          hoist
          required
          help-text="Default is project_or_hub: resolves the evaluating project’s secret first and falls back to Hub only when definitively absent."
          .value=${this.createSecretScope}
          @sl-change=${(e: Event) => {
            this.createSecretScope = (e.target as HTMLSelectElement).value;
          }}
        >
          ${allowedScopes.map((sc) => html`<sl-option value=${sc}>${sc}</sl-option>`)}
        </sl-select>
      `;
    }

    if (modeCap.mode === 'google_access_token') {
      return html`
        <div class="notice-box">
          <strong>Hub process identity:</strong> Keyless Application Default Credentials (ADC) use
          the Hub process identity — not your browser user or calling agent identity. Creating an
          instance does not discover or verify ADC credentials.
        </div>

        <sl-textarea
          label="OAuth2 Scopes (Required)"
          name="scopes"
          required
          rows="2"
          placeholder="https://www.googleapis.com/auth/..."
          help-text="One or more https://www.googleapis.com/auth/ scopes (no default). The scope required by Gemini Decisions isn't yet confirmed; cloud-platform alone was observed to be insufficient."
          .value=${this.createScopesText}
          @sl-input=${(e: Event) => {
            this.createScopesText = (e.target as HTMLTextAreaElement).value;
          }}
        ></sl-textarea>

        <sl-input
          label="Quota Project (Optional)"
          name="quotaProject"
          placeholder="e.g. my-gcp-project-123"
          help-text="Explicit GCP billing/quota project ID (required when Hub ADC uses authorized_user credentials; leave blank for implicit service-account association)."
          .value=${this.createQuotaProject}
          @sl-input=${(e: Event) => {
            this.createQuotaProject = (e.target as HTMLInputElement).value;
          }}
        ></sl-input>
      `;
    }

    return nothing;
  }

  private renderTestDialog(): TemplateResult {
    const inst = this.testInstance;
    const needsProject = instanceNeedsProjectSelectorForTest(inst);
    const runDisabled = this.testRunning || !inst || (needsProject && !this.testProjectId);

    return html`
      <sl-dialog
        label=${inst ? `Test Provider Instance: ${inst.name}` : 'Test Provider Instance'}
        ?open=${this.testDialogOpen}
        @sl-after-hide=${(e: Event) => {
          if (e.target === e.currentTarget) this.closeTest();
        }}
      >
        ${inst
          ? html`
              <div class="form-fields" style="gap: 0.75rem;">
                <div class="notice-box warning" role="note">
                  <strong>Provider request notice:</strong> This sends one request to the provider
                  and may incur charges.
                </div>

                <div class="detail-grid">
                  <div class="detail-label">Instance</div>
                  <div class="detail-value mono">${inst.name}</div>
                  <div class="detail-label">Adapter</div>
                  <div class="detail-value mono">${inst.adapter}</div>
                  <div class="detail-label">Configured Model</div>
                  <div class="detail-value mono">${inst.model || '—'}</div>
                  <div class="detail-label">Auth</div>
                  <div class="detail-value">${formatCredentialStatus(inst).statusText}</div>
                </div>

                ${needsProject
                  ? html`
                      <div>
                        <sl-select
                          label="Project (required)"
                          name="testProject"
                          hoist
                          clearable
                          required
                          ?disabled=${this.testProjectsLoading}
                          placeholder=${this.testProjectsLoading
                            ? 'Loading visible projects…'
                            : 'Select a project'}
                          .value=${this.testProjectId}
                          @sl-change=${(e: Event) => {
                            this.testProjectId = (e.target as HTMLSelectElement).value;
                          }}
                        >
                          ${this.testProjects.map(
                            (p) => html`
                              <sl-option value=${p.id}>
                                ${p.name}${p.slug ? ` (${p.slug})` : ''}
                              </sl-option>
                            `
                          )}
                        </sl-select>
                        <p class="field-note">
                          Only projects visible to your account are listed. Testing a
                          <code>project</code> or <code>project_or_hub</code> named secret requires
                          a selected project and both <code>project.read</code> and
                          <code>project.secret_read</code> permission before credential access or
                          provider I/O.
                        </p>
                        ${this.testProjectsError
                          ? html`<div class="notice-box danger" role="alert">
                              ${this.testProjectsError}
                            </div>`
                          : nothing}
                      </div>
                    `
                  : nothing}

                <sl-textarea
                  label="Synthetic Probe Input Text"
                  name="probeText"
                  rows="1"
                  .value=${this.testProbeText}
                  @sl-input=${(e: Event) => {
                    this.testProbeText = (e.target as HTMLTextAreaElement).value;
                  }}
                ></sl-textarea>

                <sl-textarea
                  label="Synthetic Spec (JSON)"
                  name="probeSpec"
                  rows="3"
                  .value=${this.testSpecText}
                  @sl-input=${(e: Event) => {
                    this.testSpecText = (e.target as HTMLTextAreaElement).value;
                  }}
                ></sl-textarea>

                ${this.testError
                  ? html`<div class="notice-box danger" role="alert">${this.testError}</div>`
                  : nothing}
                ${this.testResult ? this.renderProbeResult(inst, this.testResult) : nothing}
              </div>
            `
          : nothing}

        <div slot="footer" class="dialog-footer">
          <sl-button variant="default" @click=${() => this.closeTest()}>Close</sl-button>
          <sl-button
            variant="primary"
            ?loading=${this.testRunning}
            ?disabled=${runDisabled}
            @click=${() => void this.runTest()}
          >
            <sl-icon slot="prefix" name="play-circle"></sl-icon>
            Run Test
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }

  private renderProbeResult(
    inst: SystemOneInstance,
    result: ProbeSystemOneResponse
  ): TemplateResult {
    const ev = result.evaluation;
    const notes = ev?.notes ?? [];
    const modelUnreported = notes.includes('model_unreported') || !ev?.resolved_model;
    const modelAliasReported = notes.includes('model_alias_reported');
    const usageUnreported = notes.includes('usage_unreported') || !ev?.usage;

    const badgeVariant =
      result.class === 'success' ? 'success' : result.class === 'uncertain' ? 'warning' : 'danger';

    return html`
      <div class="probe-result" data-testid="probe-result">
        <div class="probe-result-header">
          <h3>Test Result</h3>
          <sl-badge variant=${badgeVariant} pill>${result.class}</sl-badge>
        </div>

        <div class="detail-grid">
          <div class="detail-label">Outcome / Class</div>
          <div class="detail-value mono">
            ${result.class}${result.reason ? ` (${result.reason})` : ''}
          </div>

          ${result.decision
            ? html`
                <div class="detail-label">Decision</div>
                <div class="detail-value mono">
                  ${result.decision.bool !== undefined
                    ? String(result.decision.bool)
                    : (result.decision.string ?? result.decision.kind)}
                </div>
              `
            : nothing}

          <div class="detail-label">Latency</div>
          <div class="detail-value mono">${formatElapsedNs(ev?.elapsed)}</div>

          <div class="detail-label">Configured Model</div>
          <div class="detail-value mono">${ev?.configured_model || inst.model || '—'}</div>

          <div class="detail-label">Reported Model</div>
          <div class="detail-value mono">
            ${modelUnreported
              ? 'Model unreported'
              : `${ev?.resolved_model}${modelAliasReported ? ' (alias)' : ''}`}
          </div>

          <div class="detail-label">Token Usage</div>
          <div class="detail-value mono">
            ${usageUnreported
              ? 'Usage unavailable'
              : `input: ${ev?.usage?.input_tokens}, output: ${ev?.usage?.output_tokens}${
                  ev?.usage?.truncated ? ' (truncated)' : ''
                }`}
          </div>

          ${ev?.auth?.secret_ref
            ? html`
                <div class="detail-label">Secret Provenance</div>
                <div class="detail-value mono">
                  ${ev.auth.secret_ref.name}
                  ${ev.auth.secret_ref.scope_used
                    ? ` (resolved: ${ev.auth.secret_ref.scope_used}${
                        ev.auth.secret_version ? ` v${ev.auth.secret_version}` : ''
                      })`
                    : ''}
                </div>
              `
            : nothing}
          ${ev?.auth?.credential_type
            ? html`
                <div class="detail-label">ADC Provenance</div>
                <div class="detail-value mono">
                  ${ev.auth.credential_type}
                  ${ev.auth.quota_project ? ` — quota: ${ev.auth.quota_project}` : ''}
                </div>
              `
            : nothing}
        </div>
      </div>
    `;
  }

  private renderDeleteDialog(): TemplateResult {
    const inst = this.deleteInstance;
    const isNamedRef = Boolean(inst?.auth.secretRef);

    return html`
      <sl-dialog
        label="Delete Provider Instance"
        ?open=${this.deleteDialogOpen}
        @sl-after-hide=${(e: Event) => {
          if (e.target === e.currentTarget) this.closeDelete();
        }}
      >
        ${this.deleteError
          ? html`<div class="notice-box danger" role="alert" style="margin-bottom:0.75rem;">
              ${this.deleteError}
            </div>`
          : nothing}
        ${inst
          ? html`
              <p style="margin-top:0;">
                Delete provider instance <strong class="mono">${inst.name}</strong>?
              </p>
              <p class="field-note">
                ${isNamedRef
                  ? 'This deletes the provider instance record only. The referenced Hub or project secret is not deleted.'
                  : inst.auth.kind === 'google_access_token'
                    ? 'This deletes the provider instance record. Hub process ADC credentials are unaffected.'
                    : 'This deletes the provider instance record and its managed internal API-key secret.'}
              </p>
            `
          : nothing}
        <div slot="footer" class="dialog-footer">
          <sl-button variant="default" @click=${() => this.closeDelete()}>Cancel</sl-button>
          <sl-button
            variant="danger"
            ?loading=${this.deleteSubmitting}
            ?disabled=${this.deleteSubmitting}
            @click=${() => void this.confirmDelete()}
          >
            Delete Instance
          </sl-button>
        </div>
      </sl-dialog>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-admin-system-one': ScionPageAdminSystemOne;
  }
}
