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

import { LitElement, html, css, nothing, type TemplateResult } from 'lit';
import { customElement, state } from 'lit/decorators.js';
import { keyed } from 'lit/directives/keyed.js';

import { apiFetch, extractApiError } from '../../client/api.js';
import { navigateTo } from '../../client/main.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import type {
  Agent,
  CapabilityField,
  GCPIdentityConfig,
  GCPServiceAccount,
  HarnessAdvancedCapabilities,
  MessageMode,
  RuntimeBroker,
} from '../../shared/types.js';
import { isTargetKubernetesOnly } from '../../shared/runtime-kind.js';
import { normalizeModelAlias } from '../../shared/model-utils.js';
import { MESSAGE_MODE_DISPLAY } from '../../shared/message-mode.js';
import { isValidTimeZone } from '../../utils/time.js';
import type { EnvEntry } from '../shared/env-editor.js';
import type { TimezoneChangeDetail } from '../shared/timezone-picker.js';
import '../shared/env-editor.js';
import '../shared/timezone-picker.js';
import '../shared/message-mode-badge.js';

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
  thinking_level?: number | null;
  env?: Record<string, string>;
  telemetry?: { enabled?: boolean };
}

/**
 * Env var names the dedicated auto-expose UI controls own, rather than the
 * generic env-row editor. Shared between populateForm (which filters them
 * out of envEntries and snapshots their loaded values) and buildConfig
 * (which re-synthesizes or re-sends them).
 */
const AUTO_EXPOSE_ENV_KEYS = [
  'SCION_AUTO_EXPOSE_PORTS',
  'SCION_AUTO_EXPOSE_MODE',
  'SCION_AUTO_EXPOSE_PORTS_LIST',
  'SCION_AUTO_EXPOSE_INTERVAL',
] as const;
const AUTO_EXPOSE_ENV_KEYS_SET: ReadonlySet<string> = new Set(AUTO_EXPOSE_ENV_KEYS);

/**
 * The agent container timezone is not an env record: the hub keeps it in
 * AppliedConfig.ExplicitTimezone, written only by the PATCH's top-level
 * explicitTimezone field (the Timezone row below), and ignores config.env.TZ.
 * The env table therefore never shows, sends or deletes this key.
 */
const TZ_ENV_KEY = 'TZ';

/**
 * The warning the hub's agent PATCH returns when an explicitTimezone edit
 * changes the zone of an agent whose container is live
 * (explicitTimezoneNextStartWarning in pkg/hub/agent_tz_writers.go).
 */
const TZ_NEXT_START_WARNING = "explicitTimezone applies at the agent's next start";

/**
 * Short labels for the hub's timezoneSource values (the rung of the agent
 * timezone chain that supplied resolvedTimezone; see pkg/hub/agent_tz.go).
 */
const TIMEZONE_SOURCE_LABELS: Readonly<Record<string, string>> = {
  explicit: 'Pinned on this agent',
  legacy: 'Pinned (kept from an earlier TZ setting)',
  user: 'Your TZ environment variable',
  project: 'Project TZ environment variable',
  hub: 'Hub TZ environment variable',
  broker: 'Broker TZ environment variable',
  progeny: 'Inherited TZ environment variable',
  'hub-default': 'Hub default timezone',
  none: 'Not set (container default)',
};

/** Human label for a timezoneSource value; unknown values are shown as-is. */
function timezoneSourceLabel(source: string): string {
  return TIMEZONE_SOURCE_LABELS[source] ?? source;
}

/** The agent PATCH response: the agent plus its resolved container timezone. */
interface AgentPatchResponse {
  appliedConfig?: AppliedConfig;
  resolvedTimezone?: string;
  timezoneSource?: string;
  warnings?: string[];
}

/** True when both env-keyed maps have exactly the same keys and values. */
function envMapsEqual(a: Record<string, string>, b: Record<string, string>): boolean {
  const aKeys = Object.keys(a);
  const bKeys = Object.keys(b);
  if (aKeys.length !== bKeys.length) return false;
  return aKeys.every((k) => a[k] === b[k]);
}

interface AppliedConfig {
  image?: string;
  model?: string;
  thinkingLevel?: number | null;
  harnessConfig?: string;
  harnessAuth?: string;
  task?: string;
  env?: Record<string, string>;
  gcpIdentity?: GCPIdentityConfig;
  inlineConfig?: ScionConfigPayload & {
    harness_config?: string;
  };
  agentRole?: string;
  /** The runtime profile this agent was dispatched with, if any (api/types.go RunConfig.Profile). */
  profile?: string;
  /** The agent's pinned container timezone (IANA name), if any. */
  explicitTimezone?: string;
  /** True when explicitTimezone was adopted from a TZ an older hub saved in env. */
  explicitTimezoneLegacy?: boolean;
}

interface AgentWithConfig extends Omit<Agent, 'appliedConfig'> {
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
  @state() private modelSelection: '' | 'small' | 'medium' | 'large' | 'extra-large' | 'other' = '';
  @state() private customModelId = '';
  @state() private thinkingLevel: number | null = null;
  @state() private image = '';
  @state() private branch = '';
  @state() private containerUser = '';
  @state() private authMethod = '';
  @state() private harnessConfig = '';
  @state() private telemetryEnabled = false;
  @state() private autoExposePortsEnabled = false;
  @state() private autoExposePortsMode = 'allowlist';
  @state() private autoExposePortsList = '';
  @state() private autoExposePortsInterval = '3s';

  // Snapshots of the values above as populateForm last loaded them (from the
  // live, derived config), so buildConfig can tell "the user changed this
  // control" from "this is just what was already there". Both telemetryEnabled
  // and the auto-expose fields are synthesized from global defaults when the
  // live config doesn't set them (see populateForm), so they are almost never
  // literally absent -- sending them unconditionally on every Save/Start
  // would record an edit that never happened (ptone/scion#2493 R1-1).
  private loadedTelemetryEnabled = false;
  private loadedAutoExposePortsEnabled = false;
  private loadedAutoExposePortsMode = 'allowlist';
  private loadedAutoExposePortsList = '';
  private loadedAutoExposePortsInterval = '3s';
  // Exactly which SCION_AUTO_EXPOSE_* keys were present in the loaded env,
  // and their raw values -- see populateForm. Lets buildConfig re-send only
  // what was really there (ptone/scion#2493 R2-1 facet (a)) instead of
  // synthesizing a key that was never live just because some OTHER env row
  // changed.
  private loadedAutoExposeEnvKeys: Record<string, string> = {};
  // Snapshot of this.envEntries as populateForm last loaded it (shallow
  // copies, so later edits to this.envEntries can't retroactively change
  // what "loaded" means). Lets buildConfig tell whether the user edited the
  // custom env rows at all -- see the Env section of buildConfig.
  private loadedEnvEntries: EnvEntry[] = [];

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

  // Timezone row. Pin and Unpin write explicitTimezone with their own PATCH,
  // separate from Save, so they work in any phase.
  /** The stored pin (appliedConfig.explicitTimezone); '' when unpinned. */
  @state() private tzPinned = '';
  /**
   * The zone the agent resolves to at its next start, and the rung that
   * supplies it. The agent GET does not report these, so on load they are
   * known only for a pinned agent (the pin itself); any PATCH response
   * carries both.
   */
  @state() private tzResolved: string | null = null;
  @state() private tzSource: string | null = null;
  @state() private tzPicking = false;
  @state() private tzDraft = '';
  @state() private tzSaving = false;
  @state() private tzError: string | null = null;
  /** True when the last PATCH reported that the change applies on next start. */
  @state() private tzNextStartWarned = false;
  /** Bumped to remount the picker with a fresh value when Pin… opens. */
  @state() private tzPickerRevision = 0;

  // Form fields — Message Mode
  @state() private messageMode = '';

  // Form fields — GCP Identity
  @state() private gcpMetadataMode: 'block' | 'passthrough' | 'assign' = 'block';
  @state() private gcpServiceAccountId = '';
  @state() private gcpServiceAccounts: GCPServiceAccount[] = [];
  /**
   * Whether gcpMetadataMode came from a real stored decision
   * (appliedConfig.gcpIdentity.metadataMode) rather than this page's own
   * "nothing configured" placeholder default. A stored "block" must display
   * exactly as stored and must never be auto-corrected away — stored values
   * are not migrated, the same as a project default (ptone/scion#2328
   * Phase 2).
   */
  @state() private gcpMetadataModeFromStorage = false;
  /**
   * True once the user has explicitly interacted with the GCP Identity
   * picker (either select) this session. Gates whether gcp_identity is sent
   * at all on Save/Start: unless the user changed something, the request
   * omits gcp_identity entirely. For PATCH this is a true no-op — a nil
   * gcp_identity never touches the agent's stored config
   * (handlers_agents_core.go applyAgentUpdate) — which is exactly what must
   * happen both for a resave of an unrelated field and for a known-Kubernetes
   * target with nothing explicitly chosen (which would otherwise route an
   * explicit "passthrough" through the Hub's passthrough ownership gate).
   */
  @state() private gcpIdentityUserSet = false;

  /** Explanation text shared by the help-text slot and the disabled option's tooltip. */
  private static readonly gcpIdentityK8sHintText =
    'Block is not supported on the Kubernetes runtime: this agent targets a Kubernetes ' +
    'broker/profile. Choose Passthrough or Assign Service Account instead.';

  /** The agent's own runtime broker, loaded to determine its runtime kind for the GCP Identity picker. */
  @state() private targetBroker: RuntimeBroker | null = null;

  private agentId = '';

  private get verifiedGCPServiceAccounts(): GCPServiceAccount[] {
    return this.gcpServiceAccounts.filter((sa) => sa.verified);
  }

  /**
   * Whether this agent's runtime broker/profile is reliably known to be
   * Kubernetes. A NEW selection of Block is disabled in that case
   * (ptone/scion#2328 Phase 2) — an already-stored Block stays selectable as
   * the displayed value, see render(). Unknown until targetBroker has loaded,
   * which reads as false — the same "do not guess" default as
   * agent-create.ts.
   */
  private get targetRuntimeIsKubernetesOnly(): boolean {
    return isTargetKubernetesOnly(
      this.targetBroker ?? undefined,
      this.agent?.appliedConfig?.profile ?? ''
    );
  }

  /**
   * Short explanation rendered into the GCP identity select's `help-text`
   * slot when this agent's target is reliably known to be Kubernetes: block
   * is disabled for a NEW selection in that case.
   *
   * Rendered as a slotted child of the `<sl-select>` (not a sibling
   * `aria-describedby` reference) because the element that receives focus is
   * the `role="combobox"` input inside Shoelace's shadow root, which an
   * attribute on the host cannot reach across the shadow boundary. Shoelace
   * wires its own `help-text` slot to that combobox's `aria-describedby`
   * internally (mirrors project-settings.ts's renderKubernetesBlockHint).
   */
  private renderKubernetesBlockHint(): TemplateResult | typeof nothing {
    if (!this.targetRuntimeIsKubernetesOnly) return nothing;
    if (this.gcpIdentityUserSet) {
      return html`<div slot="help-text">${ScionPageAgentConfigure.gcpIdentityK8sHintText}</div>`;
    }
    // Untouched: name the actual effective identity rather than overclaiming
    // the broker's own default applies — that is only true when this agent
    // genuinely has nothing configured.
    if (this.gcpMetadataModeFromStorage) {
      const modeLabel =
        this.gcpMetadataMode === 'assign'
          ? 'Assign Service Account'
          : this.gcpMetadataMode === 'passthrough'
            ? 'Passthrough'
            : 'Block';
      return html`<div slot="help-text">
        ${ScionPageAgentConfigure.gcpIdentityK8sHintText} This agent's current identity is
        "${modeLabel}", as previously configured; it stays in effect until you change it here.
      </div>`;
    }
    return html`<div slot="help-text">
      ${ScionPageAgentConfigure.gcpIdentityK8sHintText} No explicit identity is configured for this
      agent, so the broker's own Kubernetes default applies automatically; choosing Passthrough or
      Assign here sends that choice explicitly instead.
    </div>`;
  }

  private async loadGCPServiceAccounts(projectId: string): Promise<void> {
    this.gcpServiceAccounts = [];
    try {
      const res = await apiFetch(
        `/api/v1/projects/${projectId}/gcp-service-accounts?includeHubScoped=true`
      );
      if (res.ok) {
        const data = (await res.json()) as { items?: GCPServiceAccount[] } | GCPServiceAccount[];
        this.gcpServiceAccounts = Array.isArray(data) ? data : data.items || [];
      }
    } catch {
      // Non-critical — just won't show assign option
    }
  }

  /** Loads this agent's own runtime broker, to classify its runtime kind for the GCP Identity picker. */
  private async loadTargetBroker(brokerId: string): Promise<void> {
    try {
      const res = await apiFetch(`/api/v1/runtime-brokers/${brokerId}`);
      if (res.ok) {
        this.targetBroker = (await res.json()) as RuntimeBroker;
      }
    } catch {
      // Non-critical — an unknown broker just leaves the target unknown,
      // which is the same "do not guess" default as no broker at all.
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

    .phase-notice {
      background: var(--sl-color-neutral-50, #f8fafc);
      border: 1px solid var(--sl-color-neutral-200, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.75rem 1rem;
      margin-bottom: 1.25rem;
      display: flex;
      align-items: flex-start;
      gap: 0.5rem;
      color: var(--sl-color-neutral-700, #334155);
      font-size: 0.875rem;
    }

    .phase-notice sl-icon {
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

    .timezone-current {
      display: flex;
      align-items: center;
      flex-wrap: wrap;
      gap: 0.5rem;
      font-size: 0.875rem;
      color: var(--scion-text, #1e293b);
    }

    .timezone-current .spacer {
      flex: 1;
    }

    .timezone-value {
      font-weight: 600;
    }

    .timezone-source {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.8125rem;
    }

    .timezone-picker-row {
      margin-top: 0.75rem;
    }

    .timezone-picker-actions {
      display: flex;
      gap: 0.5rem;
      margin-top: 0.5rem;
    }

    .timezone-error {
      margin-top: 0.375rem;
      font-size: 0.8125rem;
      color: var(--sl-color-danger-700, #b91c1c);
    }

    .env-tz-hint {
      font-size: 0.75rem;
      color: var(--scion-text-muted, #64748b);
      margin-bottom: 0.75rem;
    }

    sl-tab-group {
      --indicator-color: var(--scion-primary, #3b82f6);
    }

    sl-tab-group::part(body) {
      padding-top: 1.25rem;
    }
  `;

  override willUpdate(changedProperties: Map<string, unknown>): void {
    super.willUpdate(changedProperties);
    // Re-check whenever the target broker (loaded asynchronously, after
    // populateForm has already read the agent's stored mode) or the mode
    // itself changes. Unlike agent-create.ts, this does NOT correct away a
    // value that came from storage (gcpMetadataModeFromStorage) — a stored
    // "block" is not migrated, same as a project default. The Block option is
    // always rendered here (merely disabled on a known-Kubernetes target, see
    // render()), so there is no blank-select case to fix for a stored value;
    // normalisation exists only to stop this page's own "nothing configured"
    // placeholder default from looking and acting like an explicit choice.
    if (changedProperties.has('targetBroker') || changedProperties.has('gcpMetadataMode')) {
      this.normalizeGcpModeForTarget();
    }
  }

  override updated(changedProperties: Map<string, unknown>): void {
    super.updated(changedProperties);
    if (changedProperties.has('error') && this.error) {
      this.scrollIntoView({ behavior: 'smooth', block: 'start' });
    }
  }

  /**
   * Corrects the *displayed* gcpMetadataMode away from "block" when this
   * agent's target is reliably known to be Kubernetes (see
   * targetRuntimeIsKubernetesOnly) — but only when "block" is this page's own
   * placeholder default (gcpMetadataModeFromStorage is false), never when it
   * reflects a real stored decision.
   *
   * Also clears gcpIdentityUserSet when it rewrites the mode, for the same
   * reason as agent-create.ts's normalizeGcpModeForTarget: the target broker
   * loads asynchronously, so a user could explicitly pick "Block" while it is
   * still unknown (Block is enabled until targetRuntimeIsKubernetesOnly is
   * confirmed true) and then have the broker resolve as Kubernetes-only out
   * from under that choice. Without clearing the flag, Save/Start would send
   * the auto-substituted "passthrough" as if the user had picked it for this
   * target, through the Hub's passthrough ownership gate.
   */
  private normalizeGcpModeForTarget(): void {
    if (
      this.gcpMetadataMode === 'block' &&
      !this.gcpMetadataModeFromStorage &&
      this.targetRuntimeIsKubernetesOnly
    ) {
      this.gcpMetadataMode = 'passthrough';
      this.gcpIdentityUserSet = false;
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
  private globalAutoExposePortsDefault = false;

  private async loadAgent(): Promise<void> {
    this.loading = true;
    this.error = null;

    try {
      const [agentRes, settingsRes] = await Promise.all([
        apiFetch(`/api/v1/agents/${this.agentId}`),
        apiFetch('/api/v1/settings/public'),
      ]);

      if (settingsRes.ok) {
        const data = (await settingsRes.json()) as {
          telemetryEnabled?: boolean;
          autoExposePortsEnabled?: boolean;
        };
        this.globalTelemetryDefault = data.telemetryEnabled ?? false;
        this.globalAutoExposePortsDefault = data.autoExposePortsEnabled ?? false;
      }

      if (!agentRes.ok) {
        throw new Error(await extractApiError(agentRes, `HTTP ${agentRes.status}`));
      }

      this.agent = (await agentRes.json()) as AgentWithConfig;
      dispatchPageTitle(this, 'Configure', this.agent.name || this.agentId);

      // The timezone pin is accepted in any phase, so the Timezone row is
      // loaded (and rendered) even when the rest of the form is not.
      this.populateTimezone();

      // Outside "created" only the Timezone row is editable; render()
      // shows a phase notice instead of the form.
      if (this.agent.phase !== 'created') {
        return;
      }

      void this.loadGCPServiceAccounts(this.agent.projectId);
      if (this.agent.runtimeBrokerId) {
        void this.loadTargetBroker(this.agent.runtimeBrokerId);
      }
      this.populateForm();
    } catch (err) {
      this.error = err instanceof Error ? err.message : 'Failed to load agent';
    } finally {
      this.loading = false;
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

  private populateForm(): void {
    if (!this.agent) return;
    const ac = this.agent.appliedConfig;
    const ic = ac?.inlineConfig;

    // The live env for the custom env rows: ac.env wins outright when it is
    // non-empty, matching how the hub treats AppliedConfig.Env as the
    // authoritative live map.
    const env = ac?.env || ic?.env || {};

    // The auto-expose controls need a DIFFERENT merge: per-key, with ic.env
    // taking precedence over ac.env for each of the four keys individually
    // (ptone/scion#2493 R4-2), not an all-or-nothing choice between the two
    // maps. resolveDerivedConfig's hub/project auto-expose stamp
    // (handlers_agent_create_helpers.go) writes only into
    // InlineConfig.Env -- it is never aliased into AppliedConfig.Env when
    // the create request had no explicit env of its own, and a template's
    // own env (merged into AppliedConfig.Env separately) can otherwise make
    // `ac.env` non-empty and win outright under the plain `||` merge above,
    // hiding the stamp the control is supposed to show. The hub's R4-1
    // carve-out (applyAgentUpdate) keeps InlineConfig.Env populated with the
    // live auto-expose keys after an untouched Save/Start specifically so
    // this per-key read keeps seeing them.
    const autoExposeEnv: Record<string, string> = { ...(ac?.env ?? {}), ...(ic?.env ?? {}) };

    // General
    this.model = ac?.model || ic?.model || '';
    const derived = this.deriveModelSelection(this.model);
    this.modelSelection = derived.selection;
    this.customModelId = derived.customId;
    this.thinkingLevel = ac?.thinkingLevel ?? ic?.thinking_level ?? null;
    this.image = ac?.image || ic?.image || '';
    this.branch = ic?.branch || '';
    this.containerUser = ic?.user || '';
    this.authMethod = ac?.harnessAuth || ic?.auth_selectedType || '';
    this.harnessConfig = ac?.harnessConfig || ic?.harness_config || '';
    this.telemetryEnabled = ic?.telemetry?.enabled ?? this.globalTelemetryDefault;
    this.autoExposePortsEnabled =
      autoExposeEnv.SCION_AUTO_EXPOSE_PORTS === 'true'
        ? true
        : autoExposeEnv.SCION_AUTO_EXPOSE_PORTS === 'false'
          ? false
          : this.globalAutoExposePortsDefault;
    this.autoExposePortsMode = autoExposeEnv.SCION_AUTO_EXPOSE_MODE || 'allowlist';
    this.autoExposePortsList = autoExposeEnv.SCION_AUTO_EXPOSE_PORTS_LIST || '';
    this.autoExposePortsInterval = autoExposeEnv.SCION_AUTO_EXPOSE_INTERVAL || '3s';

    // Snapshot what was just loaded, so buildConfig can later tell an actual
    // edit to these controls apart from their synthesized starting value
    // (ptone/scion#2493 R1-1). loadedAutoExposeEnvKeys additionally records
    // exactly which of these keys were PRESENT in the loaded (per-key
    // merged) env and their raw values (as opposed to the derived
    // booleans/strings above, which can't tell "present and false" from
    // "absent, defaulted to false") -- buildConfig needs that to re-send
    // only what was really there when the auto-expose controls themselves
    // weren't touched (R2-1 facet (a)).
    this.loadedTelemetryEnabled = this.telemetryEnabled;
    this.loadedAutoExposePortsEnabled = this.autoExposePortsEnabled;
    this.loadedAutoExposePortsMode = this.autoExposePortsMode;
    this.loadedAutoExposePortsList = this.autoExposePortsList;
    this.loadedAutoExposePortsInterval = this.autoExposePortsInterval;
    this.loadedAutoExposeEnvKeys = {};
    for (const key of AUTO_EXPOSE_ENV_KEYS) {
      if (autoExposeEnv[key] !== undefined) {
        this.loadedAutoExposeEnvKeys[key] = autoExposeEnv[key];
      }
    }

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

    // Environment — filter out auto-expose env vars managed by dedicated UI
    // controls, and TZ, which the Timezone row owns (an empty TZ left over
    // from env gathering must not show up as a "required" row either).
    this.envEntries = Object.entries(env)
      .filter(([key]) => !AUTO_EXPOSE_ENV_KEYS_SET.has(key) && key !== TZ_ENV_KEY)
      .map(([key, value]) => ({ key, value }));
    this.loadedEnvEntries = this.envEntries.map((e) => ({ ...e }));

    // Detect required keys that are empty (from env gathering)
    this.requiredEnvKeys = this.envEntries.filter((e) => e.key && !e.value).map((e) => e.key);

    // Message Mode
    this.messageMode = this.agent.messageMode || '';

    // GCP Identity
    const gcpId = ac?.gcpIdentity;
    this.gcpMetadataModeFromStorage = gcpId?.metadataMode != null;
    this.gcpMetadataMode = (gcpId?.metadataMode as 'block' | 'passthrough' | 'assign') || 'block';
    this.gcpServiceAccountId = gcpId?.serviceAccountId || '';
    // Fresh load: nothing has been touched yet, regardless of what the
    // stored/placeholder mode displays.
    this.gcpIdentityUserSet = false;
  }

  /** Loads the Timezone row's state from the agent as fetched. */
  private populateTimezone(): void {
    const ac = this.agent?.appliedConfig;
    this.tzPinned = ac?.explicitTimezone ?? '';
    if (this.tzPinned) {
      this.tzResolved = this.tzPinned;
      this.tzSource = ac?.explicitTimezoneLegacy ? 'legacy' : 'explicit';
    } else {
      this.tzResolved = null;
      this.tzSource = null;
    }
    this.tzPicking = false;
    this.tzDraft = '';
    this.tzError = null;
    this.tzNextStartWarned = false;
  }

  /** Updates the Timezone row from an agent PATCH response. */
  private applyTimezoneFromResponse(data: AgentPatchResponse): void {
    if (data.appliedConfig) {
      this.tzPinned = data.appliedConfig.explicitTimezone ?? '';
    }
    if (typeof data.resolvedTimezone === 'string' && data.timezoneSource) {
      this.tzResolved = data.resolvedTimezone;
      this.tzSource = data.timezoneSource;
    }
  }

  /**
   * Writes explicitTimezone: a zone name pins it, '' unpins it. Sent on its
   * own, never together with config, so it neither depends on nor changes
   * the rest of the form.
   */
  private async patchExplicitTimezone(value: string): Promise<void> {
    this.tzSaving = true;
    this.tzError = null;
    try {
      const res = await apiFetch(`/api/v1/agents/${this.agentId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ explicitTimezone: value }),
      });
      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }
      const data = (await res.json()) as AgentPatchResponse;
      // The hub's PATCH response reports the pin and the resolved zone. If a
      // response lacks either part (an older hub, or a proxy), fall back to
      // what was just written, so the row never shows the previous pin.
      this.tzPinned = value;
      if (typeof data.resolvedTimezone !== 'string' || !data.timezoneSource) {
        this.tzResolved = value || null;
        this.tzSource = value ? 'explicit' : null;
      }
      this.applyTimezoneFromResponse(data);
      this.tzNextStartWarned = (data.warnings ?? []).includes(TZ_NEXT_START_WARNING);
      this.tzPicking = false;
    } catch (err) {
      this.tzError = err instanceof Error ? err.message : 'Failed to update timezone';
    } finally {
      this.tzSaving = false;
    }
  }

  private async handleTimezonePin(): Promise<void> {
    const zone = this.tzDraft.trim();
    if (!zone || !isValidTimeZone(zone)) {
      this.tzError = zone
        ? `"${zone}" is not a valid IANA timezone name.`
        : 'Choose a timezone to pin.';
      return;
    }
    await this.patchExplicitTimezone(zone);
  }

  private async handleTimezoneUnpin(): Promise<void> {
    await this.patchExplicitTimezone('');
  }

  private buildConfig(): ScionConfigPayload {
    const config: ScionConfigPayload = {};
    const caps = this.harnessCapabilities;

    // Fields below are either dual-purpose on the hub side (empty means
    // "unchanged", not "clear" — model, image, auth_selectedType, task: see
    // applyAgentUpdate) or not rendered by this page at all (e.g. volumes,
    // skills, mcp_servers), so an omitted key is always the right way to say
    // "I didn't touch this". They keep the truthy-only guard below.
    const model = this.modelSelection === 'other' ? this.customModelId : this.modelSelection;
    if (model) config.model = model;
    config.thinking_level = this.thinkingLevel;
    if (this.image) config.image = this.image;
    if (this.authMethod && this.authMethodSupported(this.authMethod))
      config.auth_selectedType = this.authMethod;
    if (this.task) config.task = this.task;

    // Fields below are plain, single-value fields this page owns outright
    // (it is the only place that edits them, once a harness supports them)
    // and clearing one back to empty is a meaningful, intentional edit — not
    // "I never looked at this field". They must be sent even when empty, so
    // the hub's recordExplicitEdits (ptone/scion#2493) can tell "present and
    // cleared" apart from "absent", and record the clear as an explicit
    // CreateInputs edit instead of silently leaving a stale value in place
    // for `scion reincarnate` to restore. A harness-unsupported field is
    // still omitted entirely, since this page gives the user no way to view
    // or edit it in that case.
    config.branch = this.branch;
    config.user = this.containerUser;
    config.agent_instructions = this.agentInstructions;
    if (!this.isUnsupported(caps?.prompts.system_prompt)) config.system_prompt = this.systemPrompt;
    if (!this.isUnsupported(caps?.limits.max_turns)) config.max_turns = this.maxTurns;
    if (!this.isUnsupported(caps?.limits.max_model_calls))
      config.max_model_calls = this.maxModelCalls;
    if (!this.isUnsupported(caps?.limits.max_duration)) config.max_duration = this.maxDuration;

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

    // Env. Built in two parts: the user-editable rows (env), and the
    // auto-expose controls, which are written as plain env vars (matching
    // agent-create) but owned by dedicated UI controls rather than the
    // generic env-row editor.
    //
    // TZ is skipped on both sides: a TZ row typed into the table is never
    // sent (the hub would ignore it anyway; the Timezone row writes the
    // pin), and its presence alone never counts as an env edit.
    const env: Record<string, string> = {};
    for (const entry of this.envEntries) {
      if (entry.key && entry.key !== TZ_ENV_KEY) {
        env[entry.key] = entry.value;
      }
    }
    const loadedEnvMap: Record<string, string> = {};
    for (const entry of this.loadedEnvEntries) {
      if (entry.key && entry.key !== TZ_ENV_KEY) loadedEnvMap[entry.key] = entry.value;
    }
    const customEnvChanged = !envMapsEqual(env, loadedEnvMap);

    // Both the auto-expose toggle and its sub-fields are synthesized from a
    // global default whenever the live config doesn't set them explicitly,
    // so whether to send `config.env` AT ALL is gated on whether the user
    // changed a custom row OR one of these controls (ptone/scion#2493
    // R1-1) -- sending it unconditionally would record a change that never
    // happened: it would both freeze the auto-expose defaults into
    // CreateInputs as if the user had typed them, and -- because
    // InlineConfig.Env is replaced wholesale -- reach the live Env too on a
    // mere Start, without a Save ever happening.
    const autoExposeChanged =
      this.autoExposePortsEnabled !== this.loadedAutoExposePortsEnabled ||
      (this.autoExposePortsEnabled &&
        (this.autoExposePortsMode !== this.loadedAutoExposePortsMode ||
          this.autoExposePortsList !== this.loadedAutoExposePortsList ||
          this.autoExposePortsInterval !== this.loadedAutoExposePortsInterval));

    if (autoExposeChanged) {
      // The user actually touched one of these controls: synthesize the
      // full new set from their current values.
      env.SCION_AUTO_EXPOSE_PORTS = this.autoExposePortsEnabled ? 'true' : 'false';
      if (this.autoExposePortsEnabled) {
        env.SCION_AUTO_EXPOSE_MODE = this.autoExposePortsMode;
        if (this.autoExposePortsList) {
          env.SCION_AUTO_EXPOSE_PORTS_LIST = this.autoExposePortsList;
        }
        env.SCION_AUTO_EXPOSE_INTERVAL = this.autoExposePortsInterval || '3s';
      }
    } else if (customEnvChanged) {
      // Only a custom row changed, not these controls. `env` is still going
      // to be sent because of that row, and the hub's per-key env diff
      // (recordExplicitEdits) treats a key present in the live env but
      // absent from the request as the user having removed it -- so any
      // auto-expose key that really is live must still be re-sent verbatim,
      // or it would be wiped from CreateInputs as an unintended side effect
      // of the unrelated row edit. The critical difference from the
      // (reverted) earlier fix: re-send ONLY the keys loadedAutoExposeEnvKeys
      // says were actually present live -- never synthesize a key that
      // wasn't there just because the toggle's current (possibly
      // global-default) value happens to be computable. Synthesizing here
      // was ptone/scion#2493 R2-1 facet (a): an agent with no live
      // auto-expose keys at all (created via CLI/API, or a template that
      // never set them) would otherwise gain them the first time ANY
      // unrelated env row was edited.
      Object.assign(env, this.loadedAutoExposeEnvKeys);
    }

    if (customEnvChanged || autoExposeChanged) {
      config.env = env;
    }

    // Telemetry — same reasoning as auto-expose above: telemetryEnabled is
    // synthesized from a global default when the live config has no
    // explicit telemetry, so only send it when the user actually toggled
    // it. Sending {enabled: X} unconditionally would overwrite the live
    // hub-stamped telemetry config (Cloud/Hub/filters) with a bare
    // {enabled} object at reincarnate time.
    if (
      !this.isUnsupported(caps?.telemetry.enabled) &&
      this.telemetryEnabled !== this.loadedTelemetryEnabled
    ) {
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

  /**
   * Returns null when nothing should be sent at all: the caller then omits
   * gcp_identity from the PATCH body, which is a true no-op on the server
   * (handlers_agents_core.go applyAgentUpdate only touches
   * AppliedConfig.GCPIdentity when the field is present) — so the agent's
   * stored identity, whatever it is, is left exactly as it was.
   *
   * This omission is scoped to a known-Kubernetes target with no explicit
   * user choice — the same scope as agent-create.ts, not every runtime.
   * Sending an explicit "passthrough" there would hit the Hub's passthrough
   * ownership gate for a request that never asked for passthrough, and a
   * resave of an untouched stored value must not rewrite it — Kubernetes is
   * also where the Block option is disabled for a NEW selection, so an
   * untouched value there is reliably either "nothing configured" or
   * "stored", never a fresh pick. On every other runtime this method keeps
   * the pre-existing behavior of always sending the current mode/service
   * account explicitly: there is no passthrough-gate or block-migration
   * concern to avoid there, and the request shape for non-Kubernetes agents
   * must not change.
   */
  private buildGCPIdentityPayload(): Record<string, unknown> | null {
    if (this.targetRuntimeIsKubernetesOnly && !this.gcpIdentityUserSet) return null;
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

    // Transition-only: a resave of an already-stored "block" (gcpIdentityUserSet
    // false) must succeed — it is refused only when the user actively set
    // "block" themselves, which the UI itself already prevents (the Block
    // option is disabled for new selection on a known-Kubernetes target, see
    // render()). This guard is defense-in-depth, not the primary control.
    if (
      this.gcpIdentityUserSet &&
      this.gcpMetadataMode === 'block' &&
      this.targetRuntimeIsKubernetesOnly
    ) {
      this.error =
        'Block is not available for a Kubernetes runtime target. Choose Passthrough or Assign Service Account.';
      this.saving = false;
      return;
    }

    try {
      const config = this.buildConfig();
      const body: Record<string, unknown> = { config };
      const gcpIdentity = this.buildGCPIdentityPayload();
      if (gcpIdentity) body.gcp_identity = gcpIdentity;
      // Always include messageMode for created-phase agents so "Default" can
      // clear a previously set mode. Use null to signal "unset" to the backend.
      if (this.agent?.phase === 'created') {
        body.messageMode = this.messageMode || null;
      }
      const res = await apiFetch(`/api/v1/agents/${this.agentId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });

      if (!res.ok) {
        throw new Error(await extractApiError(res, `HTTP ${res.status}`));
      }

      try {
        this.applyTimezoneFromResponse((await res.json()) as AgentPatchResponse);
      } catch {
        // A body that is not the agent PATCH response leaves the row as is.
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

    // Transition-only — see the identical guard in handleSave for why.
    if (
      this.gcpIdentityUserSet &&
      this.gcpMetadataMode === 'block' &&
      this.targetRuntimeIsKubernetesOnly
    ) {
      this.error =
        'Block is not available for a Kubernetes runtime target. Choose Passthrough or Assign Service Account.';
      this.starting = false;
      return;
    }

    try {
      // Save config first
      const config = this.buildConfig();
      const saveBody: Record<string, unknown> = { config };
      const gcpIdentity = this.buildGCPIdentityPayload();
      if (gcpIdentity) saveBody.gcp_identity = gcpIdentity;
      // Always include messageMode for created-phase agents so "Default" can
      // clear a previously set mode. Use null to signal "unset" to the backend.
      if (this.agent?.phase === 'created') {
        saveBody.messageMode = this.messageMode || null;
      }
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
      navigateTo(`/agents/${this.agentId}`);
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

      navigateTo('/agents');
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
          <sl-button
            variant="default"
            @click=${() => {
              navigateTo('/agents');
            }}
          >
            Back to Agents
          </sl-button>
        </div>
      `;
    }

    if (this.agent.phase !== 'created') {
      // Reached from the agent-detail Configure button in any phase. Only the
      // timezone pin can change after the agent has started, so the other
      // settings are not rendered here.
      return html`
        <a href="/agents/${this.agent.id || this.agentId}" class="back-link">
          <sl-icon name="arrow-left"></sl-icon>
          Back to Agent
        </a>

        <div class="page-header">
          <h1>
            <sl-icon name="sliders"></sl-icon>
            Configure Agent: ${this.agent.name}
          </h1>
          <p class="subtitle">Status: ${this.agent.phase}</p>
        </div>

        <div class="form-card">
          <div class="phase-notice" data-testid="phase-notice">
            <sl-icon name="info-circle"></sl-icon>
            <span
              >This agent is in "${this.agent.phase}" phase, so only its timezone can be changed
              here. This page edits other settings only while an agent is in "created" phase.</span
            >
          </div>
          ${this.renderTimezoneRow()}
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
              navigateTo(`/agents/${this.agentId}`);
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
            @click=${() => {
              this.showDeleteDialog = true;
            }}
          >
            <sl-icon slot="prefix" name="trash"></sl-icon>
            Delete
          </sl-button>
        </div>
      </div>

      <sl-dialog
        label="Delete Agent"
        ?open=${this.showDeleteDialog}
        @sl-request-close=${() => {
          this.showDeleteDialog = false;
        }}
      >
        <p>
          Are you sure you want to delete agent <strong>${this.agent.name}</strong>? This action
          cannot be undone.
        </p>
        <sl-button
          slot="footer"
          variant="default"
          @click=${() => {
            this.showDeleteDialog = false;
          }}
        >
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
        <sl-select
          label="Model"
          placeholder="use harness default"
          .value=${this.modelSelection}
          clearable
          @sl-change=${(e: any) => {
            this.modelSelection = e.target.value;
            if (e.target.value !== 'other') this.customModelId = '';
          }}
        >
          <sl-option value="small">Small</sl-option>
          <sl-option value="medium">Medium</sl-option>
          <sl-option value="large">Large</sl-option>
          <sl-option value="extra-large">Extra Large</sl-option>
          <sl-option value="other">Other (specify)</sl-option>
        </sl-select>

        ${this.modelSelection === 'other'
          ? html`
              <sl-input
                label="Model ID"
                placeholder="e.g. claude-opus-4-8"
                .value=${this.customModelId}
                @sl-input=${(e: any) => {
                  this.customModelId = e.target.value;
                }}
                style="margin-top: 0.75rem"
              >
              </sl-input>
            `
          : ''}
      </div>

      <div class="form-field">
        <label
          >Thinking
          Level${this.thinkingLevel !== null
            ? html` <span style="font-weight:normal;color:var(--sl-color-neutral-500)"
                >(${this.thinkingLevel})</span
              >`
            : ''}</label
        >
        <div style="display:flex;align-items:center;gap:0.75rem">
          <sl-range
            min="0"
            max="100"
            step="1"
            .value=${this.thinkingLevel ?? 50}
            ?disabled=${this.thinkingLevel === null}
            style="flex:1"
            @sl-input=${(e: any) => {
              this.thinkingLevel = e.target.value;
            }}
          ></sl-range>
          <sl-checkbox
            ?checked=${this.thinkingLevel !== null}
            @sl-change=${(e: any) => {
              this.thinkingLevel = e.target.checked ? 50 : null;
            }}
            >Set</sl-checkbox
          >
        </div>
        <div class="hint" style="display:flex;justify-content:space-between;margin-top:0.25rem">
          <span>0 = minimal reasoning</span>
          <span>${this.thinkingLevel === null ? 'Using harness default' : ''}</span>
          <span>100 = maximum reasoning</span>
        </div>
      </div>

      <div class="form-field">
        <label>Image</label>
        <sl-input
          placeholder="Container image override"
          .value=${this.image}
          @sl-input=${(e: Event) => {
            this.image = (e.target as HTMLElement & { value: string }).value;
          }}
        ></sl-input>
      </div>

      ${this.renderTimezoneRow()}

      <div class="form-field">
        <label>Branch</label>
        <sl-input
          placeholder="Git branch for the agent"
          .value=${this.branch}
          @sl-input=${(e: Event) => {
            this.branch = (e.target as HTMLElement & { value: string }).value;
          }}
        ></sl-input>
      </div>

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

      <div class="form-field">
        <label>Auth Method</label>
        <sl-select
          placeholder="Select auth method..."
          .value=${this.authMethod}
          @sl-change=${(e: Event) => {
            this.authMethod = (e.target as HTMLElement & { value: string }).value;
          }}
        >
          <sl-option value="">Auto Detected</sl-option>
          <sl-option value="api-key">Provider API Key</sl-option>
          <sl-option value="oauth-token" ?disabled=${this.isUnsupported(oauthTokenCap)}
            >OAuth Token (env var)</sl-option
          >
          <sl-option value="vertex-ai" ?disabled=${this.isUnsupported(vertexCap)}
            >Vertex Model Garden</sl-option
          >
          <sl-option value="auth-file" ?disabled=${this.isUnsupported(authFileCap)}
            >Harness credential file</sl-option
          >
          <sl-option value="none">No Authentication</sl-option>
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
      ${this.agent?.appliedConfig?.agentRole
        ? html`
            <div class="form-field">
              <label>Agent Role</label>
              <sl-select value=${this.agent.appliedConfig.agentRole} disabled>
                <sl-option value="none">None — No hub access</sl-option>
                <sl-option value="readonly">Read-only — Read-only access</sl-option>
                <sl-option value="baseline">Baseline — Standard access</sl-option>
                <sl-option value="full">Full — Full access</sl-option>
              </sl-select>
              <div class="hint">
                Authorization role set at creation time. Determines hub API access level.
              </div>
            </div>
          `
        : nothing}

      <!-- Message Mode -->
      ${this.agent?.phase === 'created'
        ? html`
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
                ${(
                  Object.entries(MESSAGE_MODE_DISPLAY) as [
                    MessageMode,
                    (typeof MESSAGE_MODE_DISPLAY)[MessageMode],
                  ][]
                ).map(
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
                    This agent is configured in sealed mode. It will not be able to send or receive
                    messages.
                  </div>`
                : this.messageMode === 'hub'
                  ? html`<div class="hint">
                      Hub mode enables messaging with permitted agents in other projects on this
                      Hub, in addition to all agents and users in this project. External reach
                      requires the Hub cross-project switch to be enabled.
                    </div>`
                  : html`<div class="hint">
                      Message authorization scope. Default inherits from the parent agent's mode.
                    </div>`}
            </div>
          `
        : this.agent?.messageMode
          ? html`
              <div class="form-field">
                <label>Message Mode</label>
                <div style="padding: 0.25rem 0;">
                  <scion-message-mode-badge
                    mode=${this.agent.messageMode}
                    size="medium"
                  ></scion-message-mode-badge>
                </div>
                <div class="hint">
                  Message mode is read-only for started agents. Use the agent detail page to change
                  it.
                </div>
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
            this.gcpIdentityUserSet = true;
            if (this.gcpMetadataMode !== 'assign') {
              this.gcpServiceAccountId = '';
            }
          }}
        >
          <sl-option
            value="block"
            ?disabled=${this.targetRuntimeIsKubernetesOnly}
            title=${this.targetRuntimeIsKubernetesOnly
              ? ScionPageAgentConfigure.gcpIdentityK8sHintText
              : nothing}
            >Block</sl-option
          >
          ${this.gcpServiceAccounts.length > 0
            ? html`<sl-option value="assign">Assign Service Account</sl-option>`
            : nothing}
          <sl-option value="passthrough">Passthrough</sl-option>
          ${this.renderKubernetesBlockHint()}
        </sl-select>
        <div class="hint">
          ${this.gcpMetadataMode === 'block'
            ? 'Prevents the agent from accessing any GCP identity. Token requests are denied.'
            : this.gcpMetadataMode === 'assign'
              ? 'Assigns a registered GCP service account. GCP client libraries will authenticate automatically.'
              : "No metadata interception. The agent inherits the broker's GCP identity. Requires broker ownership."}
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
                        this.gcpIdentityUserSet = true;
                      }}
                    >
                      ${this.verifiedGCPServiceAccounts.map(
                        (sa) =>
                          html`<sl-option value=${sa.id}>
                            ${sa.email}${sa.displayName ? ` (${sa.displayName})` : ''}${sa.scope ===
                            'hub'
                              ? ' (Hub)'
                              : ''}
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

      <div class="notify-field">
        ${this.isUnsupported(telemetryCap)
          ? html`
              <sl-tooltip content=${this.supportReason(telemetryCap)} hoist>
                <sl-checkbox ?checked=${this.telemetryEnabled} ?disabled=${true}>
                  Enable Telemetry
                </sl-checkbox>
              </sl-tooltip>
            `
          : html`
              <sl-checkbox
                ?checked=${this.telemetryEnabled}
                @sl-change=${(e: Event) => {
                  this.telemetryEnabled = (e.target as HTMLInputElement).checked;
                }}
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
          content="Automatically detect and expose TCP listening ports from this agent's container. The default reflects the global auto-expose setting."
          hoist
        >
          <span class="help-badge">?</span>
        </sl-tooltip>
      </div>

      ${this.autoExposePortsEnabled
        ? html`
            <div class="form-field">
              <label for="auto-expose-mode">Port Filter Mode</label>
              <sl-select
                id="auto-expose-mode"
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
              <label for="auto-expose-ports-list">Port Filter List</label>
              <sl-input
                id="auto-expose-ports-list"
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
              <label for="auto-expose-interval">Scan Interval</label>
              <sl-input
                id="auto-expose-interval"
                placeholder="3s"
                .value=${this.autoExposePortsInterval}
                @sl-input=${(e: Event) => {
                  this.autoExposePortsInterval = (
                    e.target as HTMLElement & { value: string }
                  ).value;
                }}
              ></sl-input>
              <div class="hint">
                How often to scan for new listening ports (e.g. 3s, 5s). Minimum 1s.
              </div>
            </div>
          `
        : nothing}
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
          @sl-input=${(e: Event) => {
            this.task = (e.target as HTMLElement & { value: string }).value;
          }}
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
                @sl-input=${(e: Event) => {
                  this.systemPrompt = (e.target as HTMLElement & { value: string }).value;
                }}
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
          @sl-input=${(e: Event) => {
            this.agentInstructions = (e.target as HTMLElement & { value: string }).value;
          }}
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
                  @sl-input=${(e: Event) => {
                    this.maxTurns =
                      parseInt((e.target as HTMLElement & { value: string }).value) || 0;
                  }}
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
                  @sl-input=${(e: Event) => {
                    this.maxModelCalls =
                      parseInt((e.target as HTMLElement & { value: string }).value) || 0;
                  }}
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
                @sl-input=${(e: Event) => {
                  this.maxDuration = (e.target as HTMLElement & { value: string }).value;
                }}
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

  /**
   * The Timezone row: the agent's container timezone and where it comes
   * from, with Pin… (the shared zone picker, no "Auto" entry) and Unpin.
   */
  private renderTimezoneRow(): TemplateResult {
    const known = this.tzSource !== null;
    // resolvedTimezone "" (source "none") means no TZ is sent, so the
    // container runs its image default, UTC.
    const value = known ? this.tzResolved || 'UTC' : 'Not pinned';
    const sourceText = known
      ? timezoneSourceLabel(this.tzSource ?? '')
      : 'Resolved at start: a TZ environment variable (user, project, hub or broker scope), then the hub default timezone, then UTC.';
    // Outside the created phase the agent has been started before, so a
    // change reaches its container only at the next start (the hub also
    // warns when the container is live).
    const showNextStart = this.agent?.phase !== 'created' || this.tzNextStartWarned;

    return html`
      <div class="form-field timezone-row" data-testid="timezone-row">
        <label>Timezone</label>
        <div class="timezone-current">
          <sl-icon name="globe"></sl-icon>
          <span class="timezone-value" data-testid="timezone-value">${value}</span>
          <span class="timezone-source" data-testid="timezone-source">${sourceText}</span>
          <span class="spacer"></span>
          ${this.tzPicking
            ? nothing
            : html`
                <sl-button
                  size="small"
                  variant="default"
                  data-testid="timezone-pin-open"
                  ?disabled=${this.tzSaving}
                  @click=${() => {
                    this.tzDraft = this.tzPinned;
                    this.tzError = null;
                    this.tzPickerRevision++;
                    this.tzPicking = true;
                  }}
                  >Pin…</sl-button
                >
                ${this.tzPinned
                  ? html`
                      <sl-button
                        size="small"
                        variant="default"
                        data-testid="timezone-unpin"
                        ?loading=${this.tzSaving}
                        ?disabled=${this.tzSaving}
                        @click=${() => this.handleTimezoneUnpin()}
                        >Unpin</sl-button
                      >
                    `
                  : nothing}
              `}
        </div>
        ${this.tzPicking
          ? html`
              <div class="timezone-picker-row">
                ${keyed(
                  this.tzPickerRevision,
                  html`
                    <scion-timezone-picker
                      label="Pin timezone"
                      .value=${this.tzDraft}
                      ?disabled=${this.tzSaving}
                      @timezone-change=${(e: CustomEvent<TimezoneChangeDetail>) => {
                        this.tzDraft = e.detail.timezone;
                        this.tzError = null;
                      }}
                    ></scion-timezone-picker>
                  `
                )}
                <div class="timezone-picker-actions">
                  <sl-button
                    size="small"
                    variant="primary"
                    data-testid="timezone-pin-confirm"
                    ?loading=${this.tzSaving}
                    ?disabled=${this.tzSaving}
                    @click=${() => this.handleTimezonePin()}
                    >Pin</sl-button
                  >
                  <sl-button
                    size="small"
                    variant="default"
                    data-testid="timezone-pin-cancel"
                    ?disabled=${this.tzSaving}
                    @click=${() => {
                      this.tzPicking = false;
                      this.tzError = null;
                    }}
                    >Cancel</sl-button
                  >
                </div>
              </div>
            `
          : nothing}
        ${showNextStart
          ? html`<div class="hint" data-testid="timezone-next-start">
              A timezone change applies on the agent's next start.
            </div>`
          : nothing}
        ${this.tzError
          ? html`<div class="timezone-error" role="alert" data-testid="timezone-error">
              ${this.tzError}
            </div>`
          : nothing}
      </div>
    `;
  }

  private renderEnvironmentTab() {
    return html`
      <div class="hint env-tz-hint">
        TZ is not set here: the agent's timezone is managed by the Timezone row on the General tab.
      </div>
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
