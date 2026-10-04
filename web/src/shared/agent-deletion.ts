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
 * Pure view logic for the backend-driven delete lifecycle
 * (ptone/scion#2483 §2.1, §2.2, phase 1b).
 *
 * The hub publishes `agent.deletion` as `null`, `deleting` (with a lease the
 * live engine renews about every 20s) or `failed`. A `deleting` view whose
 * lease passes with no renewal means the engine died; the hub reports it as
 * `failed`/`abandoned` on the next read, and the client must flip it
 * itself in between (N4). Every function here takes `nowMs` explicitly so
 * the flip is testable with any clock.
 */

import type { Agent, DeletionInfo } from './types.js';

function leaseExpiryMs(d: DeletionInfo): number | null {
  if (!d.leaseExpiresAt) return null;
  const t = Date.parse(d.leaseExpiresAt);
  return Number.isNaN(t) ? null : t;
}

/**
 * The deletion view to render at `nowMs`: `deletion` itself, except that a
 * `deleting` view whose lease has passed reads as `failed`/`abandoned`,
 * exactly as the hub would compute it on the next read. Returns `null`
 * when no delete is active.
 */
export function effectiveDeletion(
  deletion: DeletionInfo | null | undefined,
  nowMs: number
): DeletionInfo | null {
  if (!deletion) return null;
  if (deletion.state !== 'deleting') return deletion;
  const lease = leaseExpiryMs(deletion);
  if (lease === null || nowMs < lease) return deletion;
  return { ...deletion, state: 'failed', code: deletion.code || 'abandoned' };
}

/**
 * True while a delete is live for `agent` at `nowMs`. Pages hide Start,
 * Stop, Suspend, Resume and Delete while this holds; a failed (or
 * client-flipped abandoned) delete re-enables them.
 */
export function isDeletionActive(agent: Pick<Agent, 'deletion'>, nowMs: number): boolean {
  return effectiveDeletion(agent.deletion, nowMs)?.state === 'deleting';
}

const FAILURE_TEXT: Record<string, string> = {
  runtime_error: 'runtime error',
  runtime_unavailable: 'runtime unavailable',
  conflict: 'conflict',
  in_doubt: 'outcome unknown',
  revoke_failed: 'could not revoke credentials',
  finalize_failed: 'could not finalize',
};

/**
 * Badge text for an effective deletion view: `Deleting…`, `Delete
 * interrupted` for `abandoned`, or `Delete failed: <error or code text>`.
 */
export function deletionBadgeLabel(d: DeletionInfo): string {
  if (d.state === 'deleting') return 'Deleting…';
  if (d.code === 'abandoned') return 'Delete interrupted';
  const detail = d.error || (d.code ? (FAILURE_TEXT[d.code] ?? d.code.replace(/_/g, ' ')) : '');
  return detail ? `Delete failed: ${detail}` : 'Delete failed';
}

/**
 * Earliest lease expiry still in the future among `agents` that are
 * `deleting`, or `null` if there is none. A view controller arms one timer
 * for this instant; leases already past need no timer, since they render
 * as abandoned already.
 */
export function earliestLeaseExpiry(
  agents: Iterable<Pick<Agent, 'deletion'>>,
  nowMs: number
): number | null {
  let earliest: number | null = null;
  for (const a of agents) {
    const d = a.deletion;
    if (!d || d.state !== 'deleting') continue;
    const lease = leaseExpiryMs(d);
    if (lease === null || lease <= nowMs) continue;
    if (earliest === null || lease < earliest) earliest = lease;
  }
  return earliest;
}

/**
 * Whether a DELETE 202 body's `deletion` should replace the client's
 * current view. The 202 arrives about 20s after the claim, so its SSE
 * deltas have usually landed already. A delta for the same (or a newer)
 * claim, such as a renewal or a failure that beat the response, must not
 * be overwritten by the older "deleting" snapshot.
 */
export function shouldApplyAcceptedDeletion(
  current: DeletionInfo | null | undefined,
  accepted: DeletionInfo
): boolean {
  // Same claim: the SSE copy is at least as fresh (a renewal or the
  // failure itself), so keep it.
  return !current || accepted.claim > current.claim;
}

/**
 * Read the `deletion` view from a DELETE 202 body (`{agentId, deletion}`).
 * Returns `null` for a body that is missing, malformed or carries no
 * deletion; the caller then simply waits for the SSE deltas.
 */
export async function readAcceptedDeletion(response: Response): Promise<DeletionInfo | null> {
  try {
    const body = (await response.json()) as { deletion?: DeletionInfo | null } | null;
    const d = body?.deletion;
    return d && typeof d === 'object' && typeof d.state === 'string' ? d : null;
  } catch {
    return null;
  }
}
