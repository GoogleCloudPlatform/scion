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
 * Merge a coalesced `agents-changed` payload into a held agent array:
 * the small/held/capped states' `H`, kept live with no
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
   * The page's add rule: whether a *new* ID — one not
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
   * object carries none. Not consulted for an ID
   * already in `held` — that case instead carries the *held* object's own
   * `_capabilities` forward when the incoming update lacks them (see
   * `mergeChanged`'s existing-member branch). `stateManager`'s own object
   * for an ID only preserves a prior truthy `_capabilities` that *it* had
   * already stored; it has no way to know about capabilities a page added
   * on top of its own copy via this option, so `mergeChanged` must carry
   * those forward itself rather than relying on `getAgent` to have done so.
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
 * untouched element carried over by reference.
 *
 * `change.unknown` is not consulted here: an "unknown" entry is, by
 * definition, for an ID `stateManager` has no full `Agent` object for yet, so
 * there is nothing for the small/held state to adopt until a later
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
      // `stateManager`'s own object for this ID only preserves a prior
      // truthy `_capabilities` that *it* had already stored (state.ts's
      // own merge). It has no way to know about capabilities this page
      // added on top via `withInheritedCapabilities` below, for an ID
      // created while already held by a *different* page/view with no
      // scope caps of its own — so without this, an incoming update with
      // no `_capabilities` of its own would silently drop them, hiding
      // action buttons the user could use a moment ago. Only the one
      // changed object is copied; everything else keeps its reference.
      const next =
        !agent._capabilities && existing._capabilities
          ? ({ ...agent, _capabilities: existing._capabilities } as Agent)
          : agent;
      byId.set(id, next);
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

/**
 * Drop any agent already tombstoned by an SSE `deleted` event.
 *
 * A REST response can race an SSE `deleted` that was already processed
 * before the response arrives: `stateManager.seedAgents` already skips a
 * tombstoned ID when populating its own map (state.ts), but a caller that
 * also assigns the raw REST array into its *own* page-level state must
 * apply the same rule itself, or the stale agent sits in the UI
 * indefinitely — unlike a live `agents-changed` flush (handled by
 * `mergeChanged` above), nothing will ever name that already-resolved ID
 * again to remove it later.
 *
 * Returns `agents` itself, by reference, when nothing needs dropping —
 * the common case, and the only one that matters for array identity here,
 * since this runs once per REST response rather than per live delta.
 */
export function dropTombstoned(agents: readonly Agent[], deletedIds: ReadonlySet<string>): Agent[] {
  if (deletedIds.size === 0) return agents as Agent[];
  let anyTombstoned = false;
  for (const a of agents) {
    if (deletedIds.has(a.id)) {
      anyTombstoned = true;
      break;
    }
  }
  if (!anyTombstoned) return agents as Agent[];
  return agents.filter((a) => !deletedIds.has(a.id));
}

/**
 * Same reasoning as `dropTombstoned`, applied to a `[id, phase]` pairs list
 * instead of full `Agent` objects — the shape a paged response's
 * `stats.agents` carries, seeded into `AgentMemberIndex`. Without this, a
 * REST page response racing an SSE `deleted` would re-seed the member
 * index with an ID the client already knows is gone, inflating the paged
 * total/running counts and Stop-all visibility, with no later event ever
 * naming that ID again to correct it.
 */
export function dropTombstonedPairs(
  pairs: ReadonlyArray<readonly [string, string]>,
  deletedIds: ReadonlySet<string>
): Array<[string, string]> {
  if (deletedIds.size === 0) return pairs as Array<[string, string]>;
  let anyTombstoned = false;
  for (const [id] of pairs) {
    if (deletedIds.has(id)) {
      anyTombstoned = true;
      break;
    }
  }
  if (!anyTombstoned) return pairs as Array<[string, string]>;
  return pairs.filter(([id]) => !deletedIds.has(id)) as Array<[string, string]>;
}
