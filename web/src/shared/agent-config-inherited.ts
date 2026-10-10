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

import type { AgentResourceSpec, TemplateConfig } from './types.js';

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
  defaultMaxTurns?: number;
  defaultMaxModelCalls?: number;
  defaultMaxDuration?: string;
  defaultResources?: AgentResourceSpec | null;
  telemetryEnabled?: boolean | null;
  autoExposePortsEnabled?: boolean | null;
  defaultAgentRole?: string;
  defaultGCPIdentityMode?: string;
  defaultGCPIdentityServiceAccountID?: string;
  defaultGCPIdentityServiceAccountIDByProfile?: Record<string, string>;
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

/** What the overlay reads: only values the page fetched. */
export interface InheritedSources {
  /** The chosen template, as the template list returns it. */
  template?: { image?: string | undefined; config?: TemplateConfig | undefined } | null | undefined;
  projectSettings?: ProjectCreateDefaults | null | undefined;
  hubSettings?: HubPublicDefaults | null | undefined;
}

export const FROM_TEMPLATE = 'inherited from the template';
export const FROM_PROJECT = 'inherited from project settings';
export const FROM_HUB = 'inherited from hub defaults';
export const PROJECT_OVERRIDE = 'project setting; overrides any value set here';

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
 * - model: project, then hub, then template (the hub's SCION_MODEL).
 * - thinking level, harness auth, limits, resources, agent role: project.
 *   The template's and the hub's values for these are not readable here.
 * - image, message mode: template (the image is resolved by the broker, with
 *   the template ahead of the harness config).
 * - telemetry: the project setting is an override, applied even over a value
 *   set here; otherwise the template's. The hub-wide public telemetry
 *   setting is not applied at create, so it is not shown.
 * - auto-expose ports: project, then the template's env, then the hub
 *   default (a harness config's env, not readable here, sits between the
 *   template and the hub).
 */
export function resolveInheritedPlaceholders(
  sources: InheritedSources
): Record<string, AgentConfigPlaceholder> {
  const out: Record<string, AgentConfigPlaceholder> = {};
  const tc = sources.template?.config;
  const ps = sources.projectSettings ?? {};
  const hub = sources.hubSettings ?? {};

  if (ps.defaultModel) out['config.model'] = { value: ps.defaultModel, source: FROM_PROJECT };
  else if (hub.defaultModel) out['config.model'] = { value: hub.defaultModel, source: FROM_HUB };
  else if (tc?.model) out['config.model'] = { value: tc.model, source: FROM_TEMPLATE };

  if (typeof ps.defaultThinkingLevel === 'number') {
    out['config.thinking_level'] = { value: String(ps.defaultThinkingLevel), source: FROM_PROJECT };
  }
  if (ps.defaultHarnessAuth) {
    out['config.auth_selectedType'] = { value: ps.defaultHarnessAuth, source: FROM_PROJECT };
  }
  if (ps.defaultMaxTurns) {
    out['config.max_turns'] = { value: String(ps.defaultMaxTurns), source: FROM_PROJECT };
  }
  if (ps.defaultMaxModelCalls) {
    out['config.max_model_calls'] = {
      value: String(ps.defaultMaxModelCalls),
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
  if (ps.defaultAgentRole) out.agentRole = { value: ps.defaultAgentRole, source: FROM_PROJECT };

  const image = tc?.image || sources.template?.image;
  if (image) out['config.image'] = { value: image, source: FROM_TEMPLATE };
  if (tc?.messageMode) out.messageMode = { value: tc.messageMode, source: FROM_TEMPLATE };

  if (typeof ps.telemetryEnabled === 'boolean') {
    out['config.telemetry'] = {
      value: String(ps.telemetryEnabled),
      source: PROJECT_OVERRIDE,
      override: true,
    };
  } else if (typeof tc?.telemetry?.enabled === 'boolean') {
    out['config.telemetry'] = { value: String(tc.telemetry.enabled), source: FROM_TEMPLATE };
  }

  const templateAutoExpose = tc?.env?.SCION_AUTO_EXPOSE_PORTS;
  if (typeof ps.autoExposePortsEnabled === 'boolean') {
    out.autoExpose = { value: String(ps.autoExposePortsEnabled), source: FROM_PROJECT };
  } else if (templateAutoExpose === 'true' || templateAutoExpose === 'false') {
    out.autoExpose = { value: templateAutoExpose, source: FROM_TEMPLATE };
  } else if (typeof hub.autoExposePortsEnabled === 'boolean') {
    out.autoExpose = {
      value: String(hub.autoExposePortsEnabled),
      source: `${FROM_HUB}, unless the harness config sets it`,
    };
  }

  return out;
}
