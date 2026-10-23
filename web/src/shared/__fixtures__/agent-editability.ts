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
 * Test fixture: the editability the hub reports for an agent the caller may
 * update, for the keys the agent form renders. It follows the rule in
 * pkg/hub/agent_config_mutability.go (buildAgentEditability): T0 is
 * immediate; T1 is now with no container, locked ("not available yet")
 * with a live one, and the task is locked on resume; the timezone is now in
 * any phase; T2 and T3 need a reincarnation, except the GCP identity of a
 * created agent (now) and its role (locked); TX is locked.
 */

import type { AgentEditability, AgentEditTier, AgentFieldEditState } from '../types.js';

/** The phases the Edit page is reachable in. */
export const EDIT_PHASES = ['created', 'stopped', 'error', 'suspended', 'running'] as const;

const TIERS: Record<string, AgentEditTier> = {
  name: 'T0',
  labels: 'T0',
  messageMode: 'T0',
  'config.task': 'T1',
  'config.model': 'T1',
  'config.thinking_level': 'T1',
  'config.image': 'T1',
  'config.user': 'T1',
  'config.telemetry': 'T1',
  'config.auth_selectedType': 'T1',
  'config.max_turns': 'T1',
  'config.max_model_calls': 'T1',
  'config.max_duration': 'T1',
  'config.resources': 'T1',
  'config.env': 'T1',
  explicitTimezone: 'T1',
  'config.system_prompt': 'T2',
  'config.agent_instructions': 'T2',
  agentRole: 'T3',
  gcp_identity: 'T3',
  projectId: 'TX',
  template: 'TX',
  harnessConfig: 'TX',
  runtimeBrokerId: 'TX',
  profile: 'TX',
  branch: 'TX',
};

const SESSION = new Set(['config.model', 'config.thinking_level', 'config.auth_selectedType']);
const LIVE = new Set(['provisioning', 'cloning', 'starting', 'running', 'stopping']);

export const RUNNING_REASON = 'Editing a running agent is not available yet.';
export const NOT_ON_RESUME_REASON = 'The task is not sent again when a suspended agent resumes.';
export const FIXED_REASON = 'Fixed when the agent was created; no transition applies a change.';
export const ROLE_CREATED_REASON =
  'The role cannot change before the first start. Delete and recreate the agent, or reincarnate it after it has started.';

function stateFor(key: string, tier: AgentEditTier, phase: string): AgentFieldEditState {
  const live = LIVE.has(phase);
  const config = key.startsWith('config.');
  const clear = tier === 'T1' && config ? { clearNeedsReincarnate: true } : {};
  const session = SESSION.has(key) ? { sessionSensitive: true } : {};
  const locked = (reason: string): AgentFieldEditState => ({
    tier,
    disposition: 'locked',
    reason,
    ...clear,
    ...session,
  });
  if (phase === 'created' && key === 'agentRole') return locked(ROLE_CREATED_REASON);
  if (phase === 'created' && key === 'gcp_identity') return { tier, disposition: 'now' };
  switch (tier) {
    case 'T0':
      return { tier, disposition: 'immediate' };
    case 'T1':
      if (key === 'explicitTimezone') return { tier, disposition: 'now' };
      if (phase === 'suspended' && key === 'config.task') return locked(NOT_ON_RESUME_REASON);
      if (live) return locked(RUNNING_REASON);
      return {
        tier,
        disposition: 'now',
        ...clear,
        ...session,
        ...(phase === 'suspended' && SESSION.has(key) ? { note: 'session' } : {}),
      };
    case 'T2':
    case 'T3':
      if (live && config) return locked(RUNNING_REASON);
      return { tier, disposition: 'reincarnate' };
    default:
      return locked(FIXED_REASON);
  }
}

/** The editability the hub reports for an agent in phase, with optional overrides. */
export function hubEditability(
  phase: string,
  overrides: Record<string, AgentFieldEditState> = {}
): AgentEditability {
  const fields: Record<string, AgentFieldEditState> = {};
  for (const [key, tier] of Object.entries(TIERS)) fields[key] = stateFor(key, tier, phase);
  return { phase, fields: { ...fields, ...overrides } };
}
