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
 * Delete an agent and wait until the hub confirms the delete.
 *
 * For a flow that must not go on until an agent is really gone (deleting a
 * member's agents before a project leave, ptone/scion#7494). The DELETE is
 * the shared {@link runAgentDelete}, without its confirm or force fallback.
 * A 204 is confirmed at once. A 202 is polled with GET /api/v1/agents/{id}
 * until the hub confirms the delete (404, or a set `deletedAt`), reports it
 * failed, shows no delete running (`not_taken`), refuses the read
 * (`unobservable`), or the wait runs out (`timed_out`). The rules match the
 * CLI's WaitForAgentDeletion (pkg/hubclient/agent_delete.go). Only
 * `confirmed` counts as deleted.
 */

import { apiFetch } from './api.js';
import { runAgentDelete } from './agent-delete.js';
import type { DeletionInfo } from '../shared/types.js';

/** Why a delete did not reach confirmation. */
export type AgentDeleteWaitFailure =
  /** The DELETE itself was refused or did not reach the hub. */
  | 'delete_failed'
  /** The hub reports `deletion.state` failed. */
  | 'deletion_failed'
  /** After the 202 the agent is live with no deletion marker. */
  | 'not_taken'
  /** The poll was refused (401, 403 or another non-retryable 4xx). */
  | 'unobservable'
  /** The wait ended before an outcome was seen. */
  | 'timed_out';

/** The outcome of {@link deleteAgentAndWait}. */
export type AgentDeleteWaitOutcome =
  | { kind: 'confirmed' }
  | { kind: 'failed'; reason: AgentDeleteWaitFailure; message: string };

/** Tuning for the poll after a 202. Zero or missing values take the defaults. */
export interface AgentDeletionWaitOptions {
  /** Delay between polls (default 2000 ms). */
  intervalMs?: number;
  /** Total wait budget (default 180000 ms). */
  timeoutMs?: number;
  /** Consecutive live polls without a deletion marker before `not_taken` (default 2). */
  notTakenPolls?: number;
  /** Clock, for tests. */
  now?: () => number;
  /** Delay, for tests. */
  sleep?: (ms: number) => Promise<void>;
}

export const DEFAULT_DELETION_POLL_INTERVAL_MS = 2000;
export const DEFAULT_DELETION_POLL_TIMEOUT_MS = 180_000;
export const DEFAULT_DELETION_NOT_TAKEN_POLLS = 2;

/** Whether a serialized `deletedAt` is set (the hub may send Go's zero time). */
function deletedAtSet(value: unknown): boolean {
  if (typeof value !== 'string' || !value) return false;
  const ms = Date.parse(value);
  return Number.isFinite(ms) && ms > 0;
}

/** A poll status that retrying cannot change (404 is handled before this). */
function nonRetryable4xx(status: number): boolean {
  return status >= 400 && status < 500 && status !== 404 && status !== 408 && status !== 429;
}

const defaultSleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Poll an agent the hub accepted a delete for (202) until an outcome is seen.
 * Network errors, 5xx, 408 and 429 keep the poll going until the budget runs
 * out. Never throws.
 */
export async function waitForAgentDeletion(
  agentId: string,
  opts: AgentDeletionWaitOptions = {}
): Promise<AgentDeleteWaitOutcome> {
  const interval =
    opts.intervalMs && opts.intervalMs > 0 ? opts.intervalMs : DEFAULT_DELETION_POLL_INTERVAL_MS;
  const timeout =
    opts.timeoutMs && opts.timeoutMs > 0 ? opts.timeoutMs : DEFAULT_DELETION_POLL_TIMEOUT_MS;
  const notTakenPolls =
    opts.notTakenPolls && opts.notTakenPolls > 0
      ? opts.notTakenPolls
      : DEFAULT_DELETION_NOT_TAKEN_POLLS;
  const now = opts.now ?? Date.now;
  const sleep = opts.sleep ?? defaultSleep;

  const start = now();
  let liveWithoutMarker = 0;
  for (;;) {
    // The error of this poll, if it failed; reported if the budget runs out.
    let lastError = '';
    try {
      const res = await apiFetch(`/api/v1/agents/${encodeURIComponent(agentId)}`, {
        authErrors: 'caller',
      });
      if (res.status === 404) return { kind: 'confirmed' };
      if (nonRetryable4xx(res.status)) {
        return {
          kind: 'failed',
          reason: 'unobservable',
          message: `the Hub refused to show the delete's progress (HTTP ${res.status})`,
        };
      }
      if (!res.ok) {
        lastError = `HTTP ${res.status}`;
      } else {
        const agent = (await res.json()) as {
          deletedAt?: unknown;
          deletion?: DeletionInfo | null;
        } | null;
        if (agent && deletedAtSet(agent.deletedAt)) return { kind: 'confirmed' };
        const deletion = agent?.deletion ?? null;
        if (!deletion) {
          liveWithoutMarker++;
          if (liveWithoutMarker >= notTakenPolls) {
            return {
              kind: 'failed',
              reason: 'not_taken',
              message: 'the Hub is not deleting the agent',
            };
          }
        } else if (deletion.state === 'failed') {
          return {
            kind: 'failed',
            reason: 'deletion_failed',
            message: deletion.error || 'the Hub reports the delete failed',
          };
        } else {
          liveWithoutMarker = 0;
        }
      }
    } catch (err) {
      lastError = err instanceof Error ? err.message : 'network error';
    }

    if (now() - start >= timeout) {
      return {
        kind: 'failed',
        reason: 'timed_out',
        message: lastError
          ? `the delete was not confirmed in time (${lastError})`
          : 'the delete was not confirmed in time',
      };
    }
    await sleep(interval);
  }
}

/**
 * Delete one agent (no confirm, no force fallback) and wait for the hub to
 * confirm it. A 404 on the DELETE is an agent already gone. Never throws.
 */
export async function deleteAgentAndWait(
  agentId: string,
  agentName: string,
  opts: AgentDeletionWaitOptions = {}
): Promise<AgentDeleteWaitOutcome> {
  const outcome = await runAgentDelete({
    agentId,
    agentName,
    confirm: false,
    forceFallback: false,
  });
  switch (outcome.kind) {
    case 'deleted':
      return { kind: 'confirmed' };
    case 'accepted':
      return waitForAgentDeletion(agentId, opts);
    case 'failed':
      if (outcome.status === 404) return { kind: 'confirmed' };
      return { kind: 'failed', reason: 'delete_failed', message: outcome.message };
    default:
      return { kind: 'failed', reason: 'delete_failed', message: 'the delete was not sent' };
  }
}
