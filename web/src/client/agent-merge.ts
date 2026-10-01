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
 * Merge a coalesced `agents-changed` payload into a held agent array
 * (design §6.2, §7): the small/held/capped states' `H`, kept live with no
 * full rebuild.
 *
 * This is the one place the per-page `onAgentsUpdated` full-rebuild used to
 * live: project-detail.ts and agents.ts both now call `mergeChanged` from
 * their `agents-changed` listener instead of re-deriving the whole list
 * from `stateManager.getAgents()` on every event.
 *
 * Identity is preserved on two levels:
 * - every agent object in `held` that `change` did not touch is carried
 *   over by reference (`===`) into the result, never copied or rebuilt;
 * - the returned array is `held` itself, by reference, when nothing in
 *   `change` actually altered membership or content — never a fresh array
 *   just because a flush happened.
 */

import type { Agent, Capabilities } from '../shared/types.js';
import type { AgentsChangedDetail } from './state.js';

export interface MergeChangedOptions {
  /**
   * Full `Agent` lookup for an upserted ID, e.g. `stateManager.getAgent`.
   * `agents-changed` carries only IDs; the merged, up-to-date object lives
   * in the state manager's map. An ID with no entry (a delta that raced a
   * delete, or a stale generation) is skipped.
   */
  getAgent: (id: string) => Agent | undefined;
  /**
   * The page's add rule (design §6.2): whether a *new* ID — one not
   * already in `held` — should be added at all. Never called for an ID
   * already in `held`; an existing member is always updated in place
   * regardless of this rule, exactly as today's per-page merges do (an
   * agent that moved out of scope still gets its last known state until an
   * explicit `deleted` removes it). Omitting this accepts every new ID
   * (the project page's rule: any agent in `agents-changed` is already
   * scoped to the page's SSE subscription).
   */
  shouldAdd?: (agent: Agent) => boolean;
  /**
   * Scope-level capabilities to inherit onto a brand-new agent when its own
   * object carries none (design §7: "scope-capability inheritance for
   * SSE-created agents … applies to new IDs only"). An agent already in
   * `held` keeps whatever capabilities it already resolved, even if this
   * update's object is itself missing them — `stateManager`'s own merge
   * already preserves a prior truthy `_capabilities`, so this only ever
   * fires for an ID the held array has never seen before.
   */
  scopeCapabilities?: Capabilities | undefined;
}

/** Fold `scopeCapabilities` onto `agent` when it carries no `_capabilities` of its own. */
function withInheritedCapabilities(agent: Agent, scopeCapabilities?: Capabilities): Agent {
  if (agent._capabilities || !scopeCapabilities) return agent;
  return { ...agent, _capabilities: scopeCapabilities };
}

/**
 * Apply one coalesced `agents-changed` flush to `held`. Returns `held`
 * itself when nothing changed, and otherwise a new array with every
 * untouched element carried over by reference (design §7, §10 A10).
 *
 * `change.unknown` is not consulted here: an "unknown" entry is, by
 * definition, for an ID `stateManager` has no full `Agent` object for yet
 * (§7) — there is nothing for the small/held state to adopt until a later
 * flush reports it as a real upsert. (The paged state's member index
 * handles `unknown` deltas separately, through `AgentListWindow.applyChanges`.)
 */
export function mergeChanged(
  held: readonly Agent[],
  change: AgentsChangedDetail,
  options: MergeChangedOptions
): Agent[] {
  if (change.upserted.length === 0 && change.deleted.length === 0) {
    return held as Agent[];
  }

  const byId = new Map(held.map((a) => [a.id, a]));
  let changed = false;

  for (const id of change.deleted) {
    if (byId.delete(id)) {
      changed = true;
    }
  }

  for (const id of change.upserted) {
    const agent = options.getAgent(id);
    if (!agent) continue; // raced a delete, or belongs to a stale generation.

    const existing = byId.get(id);
    if (existing) {
      if (existing === agent) continue;
      byId.set(id, agent);
      changed = true;
      continue;
    }

    if (options.shouldAdd && !options.shouldAdd(agent)) continue;
    byId.set(id, withInheritedCapabilities(agent, options.scopeCapabilities));
    changed = true;
  }

  if (!changed) return held as Agent[];
  return Array.from(byId.values());
}
