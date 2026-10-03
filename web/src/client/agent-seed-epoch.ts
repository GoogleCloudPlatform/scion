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
 *   index or server page can replay those changes into it.
 */
import type { Agent } from '../shared/types.js';
import type { SeedEpochToken, StateManager } from './state.js';
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
  /** A live create could not be decided (no `isMember` rule). */
  undecided: boolean;
}

/** One request's seed epoch. Every method is safe to call after {@link close}. */
export class AgentSeedEpoch {
  private readonly state: AgentSeedEpochState;
  private readonly token: SeedEpochToken;
  private readonly createdIds = new Set<string>();
  private readonly upsertedIds = new Set<string>();
  private closed = false;

  private readonly onCreated = (e: Event): void => {
    const id = (e as CustomEvent<{ data?: { agentId?: string } }>).detail?.data?.agentId;
    if (id) this.createdIds.add(id);
  };

  private readonly onChanged = (e: Event): void => {
    const upserted = (e as CustomEvent<{ data?: { upserted?: string[] } }>).detail?.data?.upserted;
    for (const id of upserted ?? []) this.upsertedIds.add(id);
  };

  /** Opens the epoch and starts recording. */
  constructor(state: AgentSeedEpochState = stateManager) {
    this.state = state;
    this.state.addEventListener('agent-created', this.onCreated);
    this.state.addEventListener('agents-changed', this.onChanged);
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
    for (const a of agents) {
      if (tombstones.has(a.id)) continue;
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
    return { agents: Array.from(members.values()), liveCreated, undecided };
  }

  /** Stop recording and close the store epoch. Idempotent. */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.state.removeEventListener('agent-created', this.onCreated);
    this.state.removeEventListener('agents-changed', this.onChanged);
    this.state.endSeedEpoch(this.token);
  }
}
