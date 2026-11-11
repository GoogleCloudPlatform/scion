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
 * Shared agent sort comparator.
 *
 * Moves project-detail.ts's `displayAgents` sort block into one shared,
 * unchanged function. `agents.ts` keeps its own inline copy until it is
 * migrated later; the two blocks are identical in every
 * practical case today, since `createdAt`/`updatedAt` are legacy fallback
 * fields that current API responses never populate (see the comment on
 * `Agent` in `./types.ts`).
 */

import type { Agent } from './types.js';
import { getAgentDisplayStatus } from './types.js';

export type AgentSortField = 'name' | 'status' | 'created' | 'updated';
export type SortDir = 'asc' | 'desc';

/**
 * The `updated` sort key: `lastActivityEvent` when it is set and is not the
 * zero-time sentinel (`0001-...`), otherwise `updated` (falling back to the
 * legacy `updatedAt` field, as project-detail.ts does today).
 */
export function updatedKey(a: Agent): string {
  return a.lastActivityEvent && !a.lastActivityEvent.startsWith('0001')
    ? a.lastActivityEvent
    : a.updated || a.updatedAt || '';
}

function createdKey(a: Agent): string {
  return a.created || a.createdAt || '';
}

/** Today's two-agent comparator, moved unchanged. */
export function agentCompare(a: Agent, b: Agent, field: AgentSortField, dir: SortDir): number {
  let cmp = 0;
  switch (field) {
    case 'name':
      cmp = (a.name || '').localeCompare(b.name || '');
      break;
    case 'status':
      cmp = getAgentDisplayStatus(a).localeCompare(getAgentDisplayStatus(b));
      break;
    case 'created':
      cmp = createdKey(a).localeCompare(createdKey(b));
      break;
    case 'updated':
      cmp = updatedKey(a).localeCompare(updatedKey(b));
      break;
  }
  return dir === 'asc' ? cmp : -cmp;
}

/**
 * Today's `displayAgents` sort: a stable sort by `agentCompare`. Stable over
 * the REST order (`created DESC, id DESC`), so ties show in created-desc
 * order in both directions, exactly as today.
 */
export function sortAgents(agents: readonly Agent[], field: AgentSortField, dir: SortDir): Agent[] {
  const sorted = [...agents];
  sorted.sort((a, b) => agentCompare(a, b, field, dir));
  return sorted;
}

/**
 * The server's sort key K for a sort: `updatedKey` for `updated`, the
 * created time for `created`.
 */
export function serverSortKey(a: Agent, sort: 'updated' | 'created'): string {
  return sort === 'created' ? createdKey(a) : updatedKey(a);
}

/**
 * The server's total order: `(K dir, created DESC, id DESC)`, where K is
 * `serverSortKey(a, sort)` (`updated` by default). For `created`, K is the
 * created time, so this is `(created dir, id DESC)`. Used to reinsert an
 * updated row at the correct
 * position within an already server-sorted page,
 * where `agentCompare`'s stability over the REST order cannot be relied on
 * because the page did not arrive in that order.
 *
 * This is a string comparison of the same ISO timestamps the server keys
 * on, not the server's true-time comparison — a local re-sort can drift from the server's order by the
 * same sub-second edge case today's client-side order already has. The view is corrected on the next fetch.
 */
export function serverOrderCompare(
  a: Agent,
  b: Agent,
  dir: SortDir,
  sort: 'updated' | 'created' = 'updated'
): number {
  const ak = serverSortKey(a, sort);
  const bk = serverSortKey(b, sort);
  if (ak !== bk) {
    const cmp = ak < bk ? -1 : 1;
    return dir === 'asc' ? cmp : -cmp;
  }
  const ac = createdKey(a);
  const bc = createdKey(b);
  if (ac !== bc) {
    // created DESC, regardless of dir.
    return ac < bc ? 1 : -1;
  }
  if (a.id === b.id) return 0; // reflexive: same id is never "less than" itself.
  // id DESC, regardless of dir.
  return a.id < b.id ? 1 : -1;
}
