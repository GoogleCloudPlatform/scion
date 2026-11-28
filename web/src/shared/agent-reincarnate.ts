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
 * Reincarnation text and held-edit helpers shared by the agent detail and
 * edit pages.
 */

import type { Agent } from './types.js';
import { canMessageAgent, isAgentLaunching } from './types.js';
import { getDenialMessage } from './message-mode.js';

/** Body of the Reincarnate confirm dialog (ptone/scion#3707). */
export const REINCARNATE_CONFIRM_MESSAGE =
  'The agent will be stopped and re-provisioned with its current settings and template. ' +
  'Any work running in its current session is interrupted.';

/**
 * Message sent to an agent when the user chooses "Ask the agent to prepare"
 * from the agent detail Reincarnate dialog (ptone/scion#9845).
 * Steps 1-5 mirror `reincarnateFiveLineContract` in cmd/reincarnate.go.
 */
export const REINCARNATE_PREPARE_MESSAGE = [
  '**Reincarnation requested.** A user asked you, from the agent detail page, to hand off your work and reincarnate yourself. Reach a safe stopping point in your current task, then:',
  '1. Commit and push your branch.',
  '2. Write a handoff file (`scion reincarnate --handoff-template` prints the template).',
  '3. Run `scion reincarnate --dry-run` to see what changes.',
  '4. Run `scion reincarnate --handoff-file <file>`.',
  '5. Do nothing after that call; your container will be stopped.',
  '',
  'If you cannot reincarnate now, reply to this message explaining why.',
].join('\n');

export interface ReincarnatePrepareAvailability {
  available: boolean;
  reason: string;
}

/**
 * Whether "Ask the agent to prepare" is available for `agent`, and if not,
 * the user-facing reason shown in the Reincarnate dialog.
 */
export function reincarnatePrepareAvailability(
  agent: Agent | null | undefined
): ReincarnatePrepareAvailability {
  if (!agent) {
    return { available: false, reason: "This agent can't receive messages." };
  }
  const harnessConfig = agent.harnessConfig || agent.appliedConfig?.harnessConfig;
  if (harnessConfig === 'shell') {
    return { available: false, reason: "Shell agents can't receive messages." };
  }
  if (agent.messageMode === 'none') {
    return { available: false, reason: "This agent can't receive messages." };
  }
  if (agent._messageability?.canMessage === false) {
    const reason = agent._messageability.reason
      ? getDenialMessage(agent._messageability.reason, agent.name)
      : "This agent can't receive messages.";
    return { available: false, reason };
  }
  if (!canMessageAgent(agent._capabilities)) {
    return { available: false, reason: "This agent can't receive messages." };
  }
  if (isAgentLaunching(agent)) {
    return {
      available: false,
      reason: 'The agent must be running to prepare its own handoff. Use Reincarnate now.',
    };
  }
  if (agent.phase === 'running' || agent.phase === 'suspended') {
    return { available: true, reason: '' };
  }
  return {
    available: false,
    reason: 'The agent must be running to prepare its own handoff. Use Reincarnate now.',
  };
}

/** The top-level keys a held edit can carry besides config. */
const HELD_TOP_LEVEL_KEYS = ['agentRole', 'gcp_identity'] as const;

/**
 * The wire keys of the agent's held edits that only a reincarnation applies
 * (prompts, role, GCP identity: tiers T2 and T3 in the agent's
 * editability), sorted. Empty when nothing is held.
 */
export function heldReincarnateKeys(agent: Agent | null | undefined): string[] {
  const pending = agent?.pendingConfig;
  if (!pending) return [];
  const keys = [
    ...Object.keys(pending.config ?? {}).map((k) => `config.${k}`),
    ...HELD_TOP_LEVEL_KEYS.filter((k) => pending[k] !== undefined),
  ];
  const fields = agent?.editability?.fields ?? {};
  return keys
    .filter((k) => {
      const tier = fields[k]?.tier;
      return tier === 'T2' || tier === 'T3';
    })
    .sort();
}
