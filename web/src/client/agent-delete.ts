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
 * The one agent-delete flow shared by every page that deletes an agent
 * (agents, agent-detail, project-detail, agent-configure), for the
 * backend-driven delete lifecycle (ptone/scion#2483 phase 2, §2.4).
 *
 * It owns the confirm, the DELETE, the reading of every answer the hub can
 * give (204, 202 `{agentId, deletion}`, 502/503 with the force fallback,
 * 409 and other errors, a network failure), and the force variant. On 202
 * it records the hub's `deletion` view in the state manager, so the page
 * shows "Deleting…" at once and the SSE `deleted` removes the agent later.
 * It returns a typed {@link AgentDeleteOutcome}; each page keeps only its
 * presentation (row removal, redirect, toast, inline error).
 */

import { apiFetch } from './api.js';
import { stateManager } from './state.js';
import { showConfirm } from '../components/shared/confirm-dialog.js';
import { readAcceptedDeletion, START_BLOCKED_BY_DELETE_MESSAGE } from '../shared/agent-deletion.js';
import type { DeletionInfo } from '../shared/types.js';

/** What happened to a delete request, for the page to present. */
export type AgentDeleteOutcome =
  /** The user declined the confirm; nothing was sent. */
  | { kind: 'cancelled' }
  /** 204: the agent is gone. */
  | { kind: 'deleted'; forced: boolean }
  /**
   * 202: the hub is still deleting. `deletion` (if the body carried one)
   * has already been applied to the state manager.
   */
  | { kind: 'accepted'; forced: boolean; deletion: DeletionInfo | null }
  /**
   * The delete did not happen: an HTTP error (`status` set; `code` is the
   * hub's error code when it sent one) or a network failure (`status`
   * null). Also returned when the user declines the 502/503 force offer,
   * carrying the original error.
   */
  | { kind: 'failed'; forced: boolean; status: number | null; code: string; message: string };

export interface AgentDeleteOptions {
  agentId: string;
  /** Name for the confirm dialogs; defaults to "this agent". */
  agentName?: string | undefined;
  /**
   * Ask "Are you sure…?" before a normal delete (default true). The click
   * event's Alt key skips it, as before. Ignored when `force` is set, which
   * always asks its own confirm.
   */
  confirm?: boolean | undefined;
  /** The triggering click, for the Alt-key bypass. */
  event?: MouseEvent | undefined;
  /**
   * Send `?force=true` directly (the failure banner's Force), after a force
   * confirm. The hub then completes the delete despite broker errors.
   */
  force?: boolean | undefined;
  /**
   * When a normal DELETE answers 502 or 503, offer a force delete (default
   * true). agent-configure turns it off to keep its own behaviour.
   */
  forceFallback?: boolean | undefined;
  /**
   * Called with `true` once the user has confirmed and the request starts,
   * and with `false` when it settles (pages show a spinner in between).
   */
  onBusy?: ((busy: boolean) => void) | undefined;
}

/** The per-click part of {@link AgentDeleteOptions} a page passes through. */
export type AgentDeleteRequest = Pick<AgentDeleteOptions, 'event' | 'confirm' | 'force'>;

/** Confirm for the 502/503 force fallback (unchanged wording). */
export const FORCE_FALLBACK_CONFIRM_MESSAGE =
  'Delete failed — the broker may be unreachable. Force delete this agent? This will remove the hub record without notifying the broker.';

/** Confirm for the failure banner's Force button. */
export function forceDeleteConfirmMessage(agentName: string): string {
  return (
    `Force delete agent "${agentName}"? The hub finishes the delete even if the broker ` +
    'cannot be reached or has not confirmed the teardown, so a container may be left ' +
    'behind on the broker.'
  );
}

const FORCE_CONFIRM_OPTIONS = {
  title: 'Force Delete',
  confirmText: 'Force Delete',
  variant: 'danger',
} as const;

/** Error code and message from a failed response (the `extractApiError` text). */
async function readError(
  res: Response,
  fallback: string
): Promise<{ code: string; message: string }> {
  try {
    const data = (await res.json()) as {
      error?: { code?: string; message?: string; details?: { guidance?: string } } | string;
      message?: string;
    } | null;
    if (data && typeof data.error === 'object' && data.error) {
      const code = data.error.code ?? '';
      if (data.error.message) {
        let message = data.error.message;
        if (data.error.details?.guidance) message += ` — ${data.error.details.guidance}`;
        return { code, message };
      }
      return { code, message: fallback };
    }
    if (data && typeof data.message === 'string') return { code: '', message: data.message };
    if (data && typeof data.error === 'string') return { code: '', message: data.error };
  } catch {
    // Not JSON.
  }
  return { code: '', message: fallback };
}

async function sendDelete(agentId: string, forced: boolean): Promise<Response> {
  return apiFetch(`/api/v1/agents/${agentId}${forced ? '?force=true' : ''}`, {
    method: 'DELETE',
  });
}

/** Read a successful answer: 202 records the accepted view, else deleted. */
async function successOutcome(
  agentId: string,
  response: Response,
  forced: boolean
): Promise<AgentDeleteOutcome> {
  if (response.status === 202) {
    const deletion = await readAcceptedDeletion(response);
    stateManager.applyDeleteAccepted(agentId, deletion);
    return { kind: 'accepted', forced, deletion };
  }
  return { kind: 'deleted', forced };
}

/**
 * Run one agent delete (see the module doc). Never throws: every failure,
 * including a network error, is a `failed` outcome.
 */
export async function runAgentDelete(opts: AgentDeleteOptions): Promise<AgentDeleteOutcome> {
  const { agentId } = opts;
  const name = opts.agentName || 'this agent';
  const forced = opts.force === true;

  if (forced) {
    if (!(await showConfirm(forceDeleteConfirmMessage(name), FORCE_CONFIRM_OPTIONS))) {
      return { kind: 'cancelled' };
    }
  } else if (
    opts.confirm !== false &&
    !opts.event?.altKey &&
    !(await showConfirm(`Are you sure you want to delete agent "${name}"?`))
  ) {
    return { kind: 'cancelled' };
  }

  opts.onBusy?.(true);
  let forcing = forced;
  try {
    const response = await sendDelete(agentId, forced);
    if (response.ok) return await successOutcome(agentId, response, forced);

    if (
      !forced &&
      opts.forceFallback !== false &&
      (response.status === 502 || response.status === 503) &&
      (await showConfirm(FORCE_FALLBACK_CONFIRM_MESSAGE, FORCE_CONFIRM_OPTIONS))
    ) {
      forcing = true;
      const forceResponse = await sendDelete(agentId, true);
      if (forceResponse.ok) return await successOutcome(agentId, forceResponse, true);
      const err = await readError(forceResponse, 'Failed to force delete agent');
      return { kind: 'failed', forced: true, status: forceResponse.status, ...err };
    }

    const err = await readError(
      response,
      forced ? 'Failed to force delete agent' : 'Failed to delete agent'
    );
    return { kind: 'failed', forced, status: response.status, ...err };
  } catch (err) {
    console.error('Failed to delete agent:', err);
    return {
      kind: 'failed',
      forced: forcing,
      status: null,
      code: '',
      message:
        err instanceof Error
          ? err.message
          : forcing
            ? 'Failed to force delete agent'
            : 'Failed to delete agent',
    };
  } finally {
    opts.onBusy?.(false);
  }
}

/**
 * The toast text for a failed lifecycle action (start, resume, restart,
 * ...). A 409 `delete_in_progress` (design §2.1: an unfinished delete
 * blocks start) gets an explanation instead of the hub's generic text.
 */
export async function lifecycleActionErrorMessage(
  response: Response,
  fallback: string
): Promise<string> {
  const err = await readError(response, fallback);
  if (response.status === 409 && err.code === 'delete_in_progress') {
    return START_BLOCKED_BY_DELETE_MESSAGE;
  }
  return err.message;
}
