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
 * The seed-epoch protocol for one agents request that seeds the state
 * store (a sorted first request, a paged page request, a legacy list).
 *
 * Open an {@link AgentSeedEpoch} before the request is sent, call
 * {@link AgentSeedEpoch.seed} with the response, and {@link
 * AgentSeedEpoch.close} in a `finally`. While open it:
 * - holds a state-store seed epoch, so a live delta that arrives while the
 *   request is in flight is re-applied over the (older) response rows
 *   instead of being overwritten by them;
 * - records the IDs created live, so a create that the response predates
 *   is still part of the membership a page renders;
 * - records every ID upserted live, so a page that adopts a fresh member
 *   index or server page can replay those changes into it;
 * - records the merged delta of every ID changed live while not in the
 *   store, so a page whose counts or member index come from the response
 *   can replay it (see {@link AgentSeedEpoch.unknownChanges});
 * - records the IDs deleted live, so a page whose member index or counts
 *   come from the response can replay those deletes (see
 *   {@link AgentSeedEpoch.deletedChanges});
 * - records whether any live change landed at all, for a page that holds
 *   counts it cannot adjust (see {@link AgentSeedEpoch.sawChanges});
 * - records whether the live connection resynced (`agents-resync`), after
 *   which the response may predate changes the connection missed (see
 *   {@link AgentSeedEpoch.sawResync}).
 */
import type { Agent } from '../shared/types.js';
import type {
  AgentsChangedDetail,
  SeedEpochToken,
  StateManager,
  UnknownAgentDelta,
} from './state.js';
import { stateManager } from './state.js';

/** The state-store surface the protocol needs (injectable for tests). */
export type AgentSeedEpochState = Pick<
  StateManager,
  | 'beginSeedEpoch'
  | 'seedAgents'
  | 'endSeedEpoch'
  | 'getAgent'
  | 'getDeletedAgentIds'
  | 'addEventListener'
  | 'removeEventListener'
>;

/** Options for {@link AgentSeedEpoch.seed}. */
export interface AgentSeedOptions {
  /** Merge into existing state objects instead of replacing them (the compact view or a partial page). */
  partial: boolean;
  /**
   * The page's membership rule for an agent created live while the request
   * was in flight. When omitted, such a create cannot be decided on the
   * client: it is not added, and the result is marked `undecided`.
   */
  isMember?: (agent: Agent) => boolean;
}

/** The membership after seeding. */
export interface AgentSeedResult {
  /**
   * The response rows minus live deletes, each as the state store's current
   * object, followed by every decidable live create the response did not
   * contain.
   */
  agents: Agent[];
  /** Live creates the response did not contain, as state objects (a subset of `agents`). */
  liveCreated: Agent[];
  /**
   * IDs of response rows left out because the agent was deleted live while
   * the request was in flight (its delete arrived after the epoch opened)
   * and the store does not hold it. A page that renders the response as
   * one server page is short by at least this many rows, and a refresh
   * clears the shortfall.
   *
   * Every other tombstoned row is left out of `agents` without being
   * listed here, because a refresh would leave it out the same way:
   * - a row whose tombstone predates the request (for example a deleted
   *   agent the server lists again; the store ignores a live `created` for
   *   a tombstoned ID, so it never re-enters the store);
   * - a row whose agent the store still holds.
   */
  dropped: string[];
  /** A live create could not be decided (no `isMember` rule). */
  undecided: boolean;
}

/** One request's seed epoch. Every method is safe to call after {@link close}. */
export class AgentSeedEpoch {
  private readonly state: AgentSeedEpochState;
  private readonly token: SeedEpochToken;
  private readonly createdIds = new Set<string>();
  private readonly upsertedIds = new Set<string>();
  private readonly deletedIds = new Set<string>();
  private readonly unknownDeltas = new Map<string, UnknownAgentDelta>();
  private resynced = false;
  private closed = false;

  private readonly onResync = (): void => {
    this.resynced = true;
  };

  private readonly onCreated = (e: Event): void => {
    const id = (e as CustomEvent<{ data?: { agentId?: string } }>).detail?.data?.agentId;
    if (id) this.createdIds.add(id);
  };

  private readonly onChanged = (e: Event): void => {
    const data = (e as CustomEvent<{ data?: Partial<AgentsChangedDetail> }>).detail?.data;
    for (const id of data?.deleted ?? []) this.deletedIds.add(id);
    for (const id of data?.upserted ?? []) {
      this.upsertedIds.add(id);
      // The store now holds the agent, with every earlier unknown delta
      // applied, so its object supersedes the recorded one.
      this.unknownDeltas.delete(id);
    }
    for (const [id, delta] of data?.unknown ?? []) {
      // Per field, the last value wins (the same merge as the store's flush).
      const next: UnknownAgentDelta = { ...this.unknownDeltas.get(id) };
      if (delta.phase !== undefined) next.phase = delta.phase;
      if (delta.activity !== undefined) next.activity = delta.activity;
      if (delta.lastActivityEvent !== undefined) next.lastActivityEvent = delta.lastActivityEvent;
      this.unknownDeltas.set(id, next);
    }
  };

  /** Opens the epoch and starts recording. */
  constructor(state: AgentSeedEpochState = stateManager) {
    this.state = state;
    this.state.addEventListener('agent-created', this.onCreated);
    this.state.addEventListener('agents-changed', this.onChanged);
    this.state.addEventListener('agents-resync', this.onResync);
    this.token = this.state.beginSeedEpoch();
  }

  /**
   * IDs upserted live (creates included) since the epoch opened, minus
   * those deleted since. Recording stops at {@link close}.
   */
  get changedIds(): string[] {
    const tombstones = this.state.getDeletedAgentIds();
    return Array.from(this.upsertedIds).filter((id) => !tombstones.has(id));
  }

  /**
   * The merged live delta of each ID that changed while not in the store
   * (an unknown-ID delta) since the epoch opened, in the shape of
   * {@link AgentsChangedDetail.unknown}: per field, the last value wins.
   * An ID upserted live after its unknown delta is left out (its store
   * object is newer), as is an ID deleted since. Recording stops at
   * {@link close}.
   */
  get unknownChanges(): Map<string, UnknownAgentDelta> {
    const tombstones = this.state.getDeletedAgentIds();
    const out = new Map<string, UnknownAgentDelta>();
    for (const [id, delta] of this.unknownDeltas) {
      if (!tombstones.has(id)) out.set(id, { ...delta });
    }
    return out;
  }

  /**
   * IDs deleted live since the epoch opened, in the shape of
   * {@link AgentsChangedDetail.deleted}. Replaying them is always safe: a
   * delete is idempotent. Recording stops at {@link close}.
   */
  get deletedChanges(): string[] {
    return Array.from(this.deletedIds);
  }

  /**
   * Whether the live connection resynced since the epoch opened. Changes
   * the connection missed may postdate the response, so a page adopting
   * it shows its stale banner or refresh chip. Recording stops at
   * {@link close}.
   */
  get sawResync(): boolean {
    return this.resynced;
  }

  /**
   * Whether any live change landed since the epoch opened: an upsert, a
   * create, a delete or an unknown-ID delta. A page holding counts it
   * cannot adjust (a count-only snapshot) uses it to offer a refresh.
   * Recording stops at {@link close}.
   */
  get sawChanges(): boolean {
    return (
      this.upsertedIds.size > 0 ||
      this.createdIds.size > 0 ||
      this.deletedIds.size > 0 ||
      this.unknownDeltas.size > 0
    );
  }

  /**
   * Seed `agents` under this epoch (a no-op for the store if the epoch was
   * invalidated by a scope change) and build the membership. Seeding ends
   * the store epoch; live IDs are still recorded until {@link close}.
   * Callers still call {@link close} after seeding; it is idempotent, so a
   * close on every exit path is safe whether or not a seed happened.
   */
  seed(agents: Agent[], options: AgentSeedOptions): AgentSeedResult {
    this.state.seedAgents(agents, { token: this.token, partial: options.partial });

    const tombstones = this.state.getDeletedAgentIds();
    const members = new Map<string, Agent>();
    const dropped: string[] = [];
    for (const a of agents) {
      if (tombstones.has(a.id)) {
        // Only a delete that arrived during this request makes the page
        // short; an older tombstone hides the row on every refresh.
        if (this.deletedIds.has(a.id) && !this.state.getAgent(a.id)) dropped.push(a.id);
        continue;
      }
      members.set(a.id, this.state.getAgent(a.id) ?? a);
    }
    const liveCreated: Agent[] = [];
    let undecided = false;
    for (const id of this.createdIds) {
      if (members.has(id) || tombstones.has(id)) continue;
      const agent = this.state.getAgent(id);
      if (!agent) continue;
      if (!options.isMember) {
        undecided = true;
        continue;
      }
      if (options.isMember(agent)) {
        members.set(id, agent);
        liveCreated.push(agent);
      }
    }
    return { agents: Array.from(members.values()), liveCreated, dropped, undecided };
  }

  /** Stop recording and close the store epoch. Idempotent. */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.state.removeEventListener('agent-created', this.onCreated);
    this.state.removeEventListener('agents-changed', this.onChanged);
    this.state.removeEventListener('agents-resync', this.onResync);
    this.state.endSeedEpoch(this.token);
  }
}
