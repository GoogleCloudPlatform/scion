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

/** Body of the Reincarnate confirm dialog (ptone/scion#3707). */
export const REINCARNATE_CONFIRM_MESSAGE =
  'The agent will be stopped and re-provisioned with its current settings and template. ' +
  'Any work running in its current session is interrupted.';

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
