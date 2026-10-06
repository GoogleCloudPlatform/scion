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

/**
 * How long a failed view stays visible after its base time; mirrors
 * `store.DeletionDisplayTTL` (pkg/store/deletion_view.go).
 */
export const DELETION_DISPLAY_TTL_MS = 15 * 60_000;

function parseMs(iso: string | undefined): number | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  return Number.isNaN(t) ? null : t;
}

function leaseExpiryMs(d: DeletionInfo): number | null {
  return parseMs(d.leaseExpiresAt);
}

/**
 * The deletion view to render at `nowMs`, computed the way the hub would on
 * its next read (pkg/store/deletion_view.go):
 *
 * - a `deleting` view whose lease has passed reads as `failed`/`abandoned`,
 *   expiring `DELETION_DISPLAY_TTL_MS` after the lease. A `finalizing`
 *   stage row keeps the generic `abandoned` code but gets no `expiresAt`:
 *   the hub never expires it, and it blocks start until a retry or force
 *   (design note D4);
 * - a `failed` view whose `expiresAt` has passed reads as `null`. Views
 *   with no `expiresAt` (in_doubt, finalizing) never expire.
 *
 * Returns `null` when no delete is active or visible.
 */
export function effectiveDeletion(
  deletion: DeletionInfo | null | undefined,
  nowMs: number
): DeletionInfo | null {
  if (!deletion) return null;
  let view = deletion;
  if (deletion.state === 'deleting') {
    const lease = leaseExpiryMs(deletion);
    if (lease === null || nowMs < lease) return deletion;
    const flipped: DeletionInfo = {
      ...deletion,
      state: 'failed',
      code: deletion.code || 'abandoned',
    };
    if (deletion.stage === 'finalizing') delete flipped.expiresAt;
    else flipped.expiresAt = new Date(lease + DELETION_DISPLAY_TTL_MS).toISOString();
    view = flipped;
  }
  const expires = parseMs(view.expiresAt);
  if (expires !== null && nowMs >= expires) return null;
  return view;
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

/** Codes the hub stores only while a row is finalizing (design §2.3). */
const FINALIZING_CODES = new Set(['revoke_failed', 'finalize_failed']);

/**
 * Whether a failed view blocks starting the agent (design §2.1
 * `deleteBlocksStart`): an `in_doubt` failure (a cross-node teardown may
 * still run) or a finalizing row (teardown has already run). Start then
 * returns 409 `delete_in_progress`, and Force is the way out.
 */
export function deletionBlocksStart(d: DeletionInfo | null | undefined): boolean {
  if (!d || d.state !== 'failed') return false;
  return d.code === 'in_doubt' || d.stage === 'finalizing' || FINALIZING_CODES.has(d.code ?? '');
}

/** Short hint for a start-blocking failure, for dense rows. */
export const DELETION_START_BLOCKED_HINT = 'Start is blocked; Force delete finishes it.';

/**
 * Explanation shown when Start (or resume/restart) returns 409
 * `delete_in_progress`, instead of the generic error toast.
 */
export const START_BLOCKED_BY_DELETE_MESSAGE =
  "This agent can't be started because its delete hasn't finished. " +
  'Retry the delete, or use Force delete to finish removing it.';

/**
 * The failure banner's text for a failed view: `title` is the badge label
 * (`Delete failed: <error>`, `Delete interrupted`, ...), `detail` explains
 * what happened and what Retry and Force do. For an `in_doubt` or
 * finalizing row the detail says that starting is blocked and that Force
 * is the way out (design §8 phase 2, D3).
 */
export function deletionBannerText(d: DeletionInfo): { title: string; detail: string } {
  const title = deletionBadgeLabel(d);
  let detail: string;
  if (d.code === 'in_doubt') {
    detail =
      "The broker hasn't confirmed that the agent's container was removed. " +
      'Starting this agent is blocked until it does. Force delete is the way out: ' +
      'it finishes removing the agent without that confirmation.';
  } else if (deletionBlocksStart(d)) {
    detail =
      "The agent's container was already torn down, so starting this agent is blocked. " +
      'Retry the delete, or use Force delete to finish removing it.';
  } else if (d.code === 'abandoned') {
    detail =
      'The hub stopped working on this delete before it finished. Retry it, or force delete.';
  } else {
    detail = 'Retry the delete, or force delete to remove the agent anyway.';
  }
  return { title, detail };
}

/**
 * The next instant at which some agent's effective deletion view changes
 * on its own, or `null` if none will: a `deleting` view's lease (it flips
 * to abandoned) or a `failed` view's `expiresAt` (it disappears), whichever
 * is earliest and still in the future. A view controller arms one timer for
 * this instant and recomputes after it fires, so an abandoned flip is
 * followed by a second timer for that view's expiry.
 */
export function nextDeletionDeadline(
  agents: Iterable<Pick<Agent, 'deletion'>>,
  nowMs: number
): number | null {
  let earliest: number | null = null;
  for (const a of agents) {
    const view = effectiveDeletion(a.deletion, nowMs);
    if (!view) continue;
    const at = view.state === 'deleting' ? leaseExpiryMs(view) : parseMs(view.expiresAt);
    if (at === null || at <= nowMs) continue;
    if (earliest === null || at < earliest) earliest = at;
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
