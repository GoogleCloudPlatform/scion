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
 * Inherited values for the create form's unset fields (ptone/scion#3974).
 *
 * resolveInheritedPlaceholders is the one place the web works out what an
 * agent created with a field left unset inherits. It is a thin client-side
 * overlay: it reads only what the page already fetches from existing APIs
 * (the chosen template, the project's settings and the hub's public
 * settings), follows the order the hub applies them at create, and labels
 * each value with where it comes from. It never shows a guessed or built-in
 * value: when the inherited value is not in those sources, the field gets no
 * placeholder and the form shows just "Inherited".
 *
 * Full effective-value resolution (every rung, including the harness config,
 * the hub's operational defaults and the broker's own settings) is the hub's
 * job and is tracked in ptone/scion#3914. When that resolver exists, this
 * function is what it replaces.
 *
 * pkg/hub/testdata/agent-create-inherited-golden.json pins the overlay to
 * what the hub dispatches, one case per tier (template, project, hub).
 */

import type { AgentResourceSpec, TemplateAgentConfig } from './types.js';

/** An inherited value shown as a placeholder, with where it comes from. */
export interface AgentConfigPlaceholder {
  /** The inherited value, when known. For a toggle, "true" or "false". */
  value?: string;
  /** Where the value comes from, e.g. "inherited from project settings". */
  source: string;
  /**
   * The value is an override, not a fallback: it applies even when the field
   * is set here (the project telemetry setting).
   */
  override?: boolean;
}

/** The project settings fields the create form reads (GET /api/v1/projects/{id}/settings). */
export interface ProjectCreateDefaults {
  defaultTemplate?: string;
  defaultHarnessConfig?: string;
  defaultHarnessAuth?: string;
  defaultModel?: string;
  defaultThinkingLevel?: number | null;
  /** null or absent: no project value; 0: Unlimited; n: a limit. */
  defaultMaxTurns?: number | null;
  defaultMaxModelCalls?: number | null;
  defaultMaxDuration?: string;
  defaultResources?: AgentResourceSpec | null;
  telemetryEnabled?: boolean | null;
  autoExposePortsEnabled?: boolean | null;
  defaultAgentRole?: string;
  maxAgentRole?: string;
  defaultGCPIdentityMode?: string;
  defaultGCPIdentityServiceAccountID?: string;
  defaultGCPIdentityServiceAccountIDByProfile?: Record<string, string>;
  defaultGCPIdentityServiceAccountIDByBroker?: Record<string, string>;
}

/** The hub public settings fields the create form reads (GET /api/v1/settings/public). */
export interface HubPublicDefaults {
  telemetryEnabled?: boolean;
  autoExposePortsEnabled?: boolean;
  defaultRuntimeBroker?: string;
  defaultHarnessConfig?: string;
  defaultTemplate?: string;
  defaultModel?: string;
}

/** Selected Runtime Broker context for inherited GCP identity resolution. */
export interface InheritedGCPBrokerTarget {
  id: string;
  name?: string | undefined;
  flat?: boolean | undefined;
}

/** A service account summary used to format inherited GCP identity labels. */
export interface InheritedGCPServiceAccount {
  id: string;
  email: string;
  displayName?: string | undefined;
}

/** What the overlay reads: only values the page fetched. */
export interface InheritedSources {
  /** The chosen template, as the template list returns it. */
  template?:
    | { agentConfig?: TemplateAgentConfig | undefined; defaultHarnessConfig?: string | undefined }
    | null
    | undefined;
  projectSettings?: ProjectCreateDefaults | null | undefined;
  hubSettings?: HubPublicDefaults | null | undefined;
  /** Selected Runtime Broker on the create form, if any. */
  broker?: InheritedGCPBrokerTarget | null | undefined;
  /** Selected runtime profile on a legacy broker, if any. */
  profile?: string | undefined;
  /** Whether hub.flat_runtime_brokers is enabled in the browser session. */
  flatBrokersEnabled?: boolean | undefined;
  /** Project/hub GCP service accounts loaded on the form, to format SA labels. */
  gcpServiceAccounts?: readonly InheritedGCPServiceAccount[] | undefined;
  /** Resolved hub-wide GCP identity default, when known. */
  hubGCPIdentity?:
    | { mode?: string | undefined; serviceAccountId?: string | undefined }
    | null
    | undefined;
}

export const FROM_TEMPLATE = 'inherited from the template';
export const FROM_PROJECT = 'inherited from project settings';
export const FROM_HUB = 'inherited from hub defaults';
export const PROJECT_OVERRIDE = 'project setting; overrides any value set here';

export function fromBrokerDefault(brokerLabel: string): string {
  return `inherited from project default for Runtime Broker "${brokerLabel}"`;
}

export function fromProfileDefault(profile: string): string {
  return `inherited from project default for profile "${profile}"`;
}

export type InheritedGCPIdentityRung = 'broker' | 'profile' | 'project' | 'hub';

export interface InheritedGCPIdentity {
  rung: InheritedGCPIdentityRung;
  mode: 'block' | 'passthrough' | 'assign';
  serviceAccountId?: string | undefined;
  value: string;
  source: string;
}

function formatServiceAccountLabel(
  id: string,
  accounts: readonly InheritedGCPServiceAccount[] | undefined
): string {
  const sa = accounts?.find((a) => a.id === id);
  if (!sa) return id;
  return sa.displayName ? `${sa.displayName} (${sa.email})` : sa.email;
}

function formatTierIdentity(
  rung: 'project' | 'hub',
  mode: string | undefined,
  serviceAccountId: string | undefined,
  source: string,
  accounts: readonly InheritedGCPServiceAccount[] | undefined
): InheritedGCPIdentity | undefined {
  if (!mode) return undefined;
  if (mode === 'assign') {
    if (!serviceAccountId) {
      return { rung, mode: 'block', value: 'Block', source };
    }
    return {
      rung,
      mode: 'assign',
      serviceAccountId,
      value: `Assign ${formatServiceAccountLabel(serviceAccountId, accounts)}`,
      source,
    };
  }
  if (mode === 'passthrough') {
    return { rung, mode: 'passthrough', value: 'Passthrough', source };
  }
  if (mode === 'block') {
    return { rung, mode: 'block', value: 'Block', source };
  }
  return undefined;
}

/**
 * Resolves the inherited GCP identity ladder for an untouched create form
 * (ptone/scion#8185, pkg/hub/handlers_agents_core.go):
 * 1. Per-broker project default (only when hub.flat_runtime_brokers is on and
 *    the selected Runtime Broker is flat).
 * 2. Per-profile project default (legacy profile-based brokers only).
 * 3. Project-wide default (defaultGCPIdentityMode / defaultGCPIdentityServiceAccountID).
 * 4. Hub default (when known).
 */
export function resolveInheritedGCPIdentity(
  sources: InheritedSources
): InheritedGCPIdentity | undefined {
  const ps = sources.projectSettings ?? {};
  const accounts = sources.gcpServiceAccounts;

  if (sources.flatBrokersEnabled && sources.broker?.flat && sources.broker.id) {
    const brokerSAId = ps.defaultGCPIdentityServiceAccountIDByBroker?.[sources.broker.id];
    if (brokerSAId) {
      return {
        rung: 'broker',
        mode: 'assign',
        serviceAccountId: brokerSAId,
        value: `Assign ${formatServiceAccountLabel(brokerSAId, accounts)}`,
        source: fromBrokerDefault(sources.broker.name || sources.broker.id),
      };
    }
  }

  if (!sources.broker?.flat && sources.profile) {
    const profileSAId = ps.defaultGCPIdentityServiceAccountIDByProfile?.[sources.profile];
    if (profileSAId) {
      return {
        rung: 'profile',
        mode: 'assign',
        serviceAccountId: profileSAId,
        value: `Assign ${formatServiceAccountLabel(profileSAId, accounts)}`,
        source: fromProfileDefault(sources.profile),
      };
    }
  }

  const projectTier = formatTierIdentity(
    'project',
    ps.defaultGCPIdentityMode,
    ps.defaultGCPIdentityServiceAccountID,
    FROM_PROJECT,
    accounts
  );
  if (projectTier) return projectTier;

  return formatTierIdentity(
    'hub',
    sources.hubGCPIdentity?.mode,
    sources.hubGCPIdentity?.serviceAccountId,
    FROM_HUB,
    accounts
  );
}

const RESOURCE_KEYS: ReadonlyArray<{
  key: string;
  get: (r: AgentResourceSpec) => string | undefined;
}> = [
  { key: 'config.resources.requests.cpu', get: (r) => r.requests?.cpu },
  { key: 'config.resources.requests.memory', get: (r) => r.requests?.memory },
  { key: 'config.resources.limits.cpu', get: (r) => r.limits?.cpu },
  { key: 'config.resources.limits.memory', get: (r) => r.limits?.memory },
  { key: 'config.resources.disk', get: (r) => r.disk },
];

/**
 * The inherited value of each field the sources determine, by the form's
 * field key. A field the sources do not determine has no entry.
 *
 * The order per field follows the hub's create path
 * (pkg/hub/handlers_agent_create_helpers.go, project_settings_handlers.go,
 * hub_agent_defaults.go):
 * - harness config: project, then the template's declared
 *   default_harness_config (never its harness type, ptone/scion#601), then
 *   hub (ptone/scion#3914). With none of these the Runtime Broker resolves it
 *   from its own settings, which the page cannot read, so nothing is shown.
 * - model: project, then template, then hub (the hub's SCION_MODEL). The
 *   template's model is its agentConfig snapshot's (ptone/scion#4222).
 * - thinking level, harness auth, limits, resources: project. The
 *   template's and the hub's values for these are not readable here.
 * - agent role: the project default, capped by the project maximum, as the
 *   hub caps it (see inheritedAgentRole).
 * - message mode: the template's agentConfig message_mode. The row's
 *   config is not read: the hub backfills the snapshot for every template.
 * - telemetry: the project setting is an override, applied even over a value
 *   set here. A template sets no telemetry (ptone/scion#4524), and the
 *   hub-wide public telemetry setting is not the hub's dispatched telemetry
 *   config, so neither is shown.
 * - auto-expose ports: project, then the hub default (a harness config's
 *   env, not readable here, sits between them). A template's
 *   SCION_AUTO_EXPOSE_PORTS is a reserved key and is ignored.
 * - image: not shown. A template image is an environment key the agent
 *   ignores; the broker resolves the image from settings and the harness
 *   config.
 */
/** A count limit as hint text: 0 is "Unlimited". */
function countLimitText(n: number): string {
  return n === 0 ? 'Unlimited' : String(n);
}

export function resolveInheritedPlaceholders(
  sources: InheritedSources
): Record<string, AgentConfigPlaceholder> {
  const out: Record<string, AgentConfigPlaceholder> = {};
  const tc = sources.template?.agentConfig;
  const ps = sources.projectSettings ?? {};
  const hub = sources.hubSettings ?? {};

  const templateHarnessConfig = sources.template?.defaultHarnessConfig;
  if (ps.defaultHarnessConfig) {
    out.harnessConfig = { value: ps.defaultHarnessConfig, source: FROM_PROJECT };
  } else if (templateHarnessConfig) {
    out.harnessConfig = { value: templateHarnessConfig, source: FROM_TEMPLATE };
  } else if (hub.defaultHarnessConfig) {
    out.harnessConfig = { value: hub.defaultHarnessConfig, source: FROM_HUB };
  }

  if (ps.defaultModel) out['config.model'] = { value: ps.defaultModel, source: FROM_PROJECT };
  else if (tc?.model) out['config.model'] = { value: tc.model, source: FROM_TEMPLATE };
  else if (hub.defaultModel) out['config.model'] = { value: hub.defaultModel, source: FROM_HUB };

  if (typeof ps.defaultThinkingLevel === 'number') {
    out['config.thinking_level'] = { value: String(ps.defaultThinkingLevel), source: FROM_PROJECT };
  }
  if (ps.defaultHarnessAuth) {
    out['config.auth_selectedType'] = { value: ps.defaultHarnessAuth, source: FROM_PROJECT };
  }
  // A project count limit of 0 is Unlimited, not unset (ptone/scion#3925).
  if (typeof ps.defaultMaxTurns === 'number') {
    out['config.max_turns'] = { value: countLimitText(ps.defaultMaxTurns), source: FROM_PROJECT };
  }
  if (typeof ps.defaultMaxModelCalls === 'number') {
    out['config.max_model_calls'] = {
      value: countLimitText(ps.defaultMaxModelCalls),
      source: FROM_PROJECT,
    };
  }
  if (ps.defaultMaxDuration) {
    out['config.max_duration'] = { value: ps.defaultMaxDuration, source: FROM_PROJECT };
  }
  if (ps.defaultResources) {
    for (const r of RESOURCE_KEYS) {
      const v = r.get(ps.defaultResources);
      if (v) out[r.key] = { value: v, source: FROM_PROJECT };
    }
  }
  const role = inheritedAgentRole(ps.defaultAgentRole, ps.maxAgentRole);
  if (role) out.agentRole = { value: role, source: FROM_PROJECT };

  if (tc?.message_mode) out.messageMode = { value: tc.message_mode, source: FROM_TEMPLATE };

  if (typeof ps.telemetryEnabled === 'boolean') {
    out['config.telemetry'] = {
      value: String(ps.telemetryEnabled),
      source: PROJECT_OVERRIDE,
      override: true,
    };
  }

  if (typeof ps.autoExposePortsEnabled === 'boolean') {
    out.autoExpose = { value: String(ps.autoExposePortsEnabled), source: FROM_PROJECT };
  } else if (typeof hub.autoExposePortsEnabled === 'boolean') {
    out.autoExpose = {
      value: String(hub.autoExposePortsEnabled),
      source: `${FROM_HUB}, unless the harness config sets it`,
    };
  }

  if (sources.flatBrokersEnabled && sources.broker?.flat) {
    const gcp = resolveInheritedGCPIdentity(sources);
    if (gcp?.rung === 'broker') {
      out.gcp_identity = { value: gcp.value, source: gcp.source };
    }
  }

  return out;
}

/** Agent roles from least to most privileged, as the hub orders them. */
const AGENT_ROLE_ORDER = ['none', 'readonly', 'baseline', 'full'];

/**
 * The role a user-created agent inherits: the hub takes the project default
 * (else the hub default, not readable here, else full) and caps it at the
 * project maximum (pkg/hub/handlers_agents_core.go). Known only when the
 * project sets a default, or when its maximum is none, which caps
 * everything.
 */
export function inheritedAgentRole(
  projectDefault: string | undefined,
  projectMax: string | undefined
): string | undefined {
  const rank = (r: string | undefined): number => (r ? AGENT_ROLE_ORDER.indexOf(r) : -1);
  const max = rank(projectMax) >= 0 ? projectMax : undefined;
  if (max === 'none') return 'none';
  if (rank(projectDefault) < 0) return undefined;
  if (max && rank(max) < rank(projectDefault)) return max;
  return projectDefault;
}
