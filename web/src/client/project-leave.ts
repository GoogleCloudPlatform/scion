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
 * Project self-leave (ptone/scion#7494): POST /api/v1/projects/{id}/leave,
 * the reading of each answer the hub gives, and the delete-first step that
 * deletes the caller's agents before the leave.
 */

import { apiFetch } from './api.js';
import {
  deleteAgentAndWait,
  type AgentDeleteWaitOutcome,
  type AgentDeletionWaitOptions,
} from './agent-delete-wait.js';
import type { Capabilities, MembershipCapabilities } from '../shared/types.js';

/** One agent of a leave result: exactly the three fields the hub sends. */
export interface LeaveAgentItem {
  id: string;
  name: string;
  phase: string;
}

/** The `LeaveResult` of POST …/leave, for a dry run and a real leave alike. */
export interface LeaveResult {
  projectId: string;
  dryRun: boolean;
  removedRoles: string[];
  /** `none`, `group` or `hub`. */
  remainingAccess: string;
  agents: { count: number; truncated: boolean; items: LeaveAgentItem[] };
  projectBoundTokens: { revoked: number };
  willBeHeld: boolean;
}

/** A group named in a group-derived refusal (only groups the caller may read). */
export interface LeaveGroupRef {
  id: string;
  name: string;
}

/** What a leave request came back with. */
export type LeaveOutcome =
  | { kind: 'ok'; result: LeaveResult }
  /** 409 last_owner: the caller is the last usable direct owner. */
  | { kind: 'last_owner'; message: string; guidance: string }
  /** 409 membership_group_derived: the caller's only membership is a group's. */
  | { kind: 'group_derived'; message: string; groups: LeaveGroupRef[] }
  /** 403 with details.credential session_required: not an interactive session. */
  | { kind: 'session_required'; message: string }
  /** 404: the caller has no binding in the project (or it does not exist). */
  | { kind: 'not_member' }
  /** Any other refusal or a network failure. */
  | { kind: 'error'; status: number | null; code: string; message: string };

export const LEAVE_SESSION_REQUIRED_MESSAGE =
  'Leaving a project needs a signed-in browser session. Sign in again and retry.';
export const LEAVE_NOT_MEMBER_MESSAGE =
  "You're no longer a member of this project, or it doesn't exist.";

interface ErrorBody {
  error?: {
    code?: string;
    message?: string;
    details?: { guidance?: unknown; groups?: unknown; credential?: unknown };
  };
}

function readGroups(value: unknown): LeaveGroupRef[] {
  if (!Array.isArray(value)) return [];
  return (value as unknown[])
    .filter(
      (g): g is LeaveGroupRef =>
        !!g && typeof g === 'object' && typeof (g as LeaveGroupRef).id === 'string'
    )
    .map((g) => ({ id: g.id, name: typeof g.name === 'string' && g.name ? g.name : g.id }));
}

/** Read a leave response into a {@link LeaveOutcome}. */
export async function readLeaveResponse(res: Response): Promise<LeaveOutcome> {
  if (res.ok) {
    return { kind: 'ok', result: (await res.json()) as LeaveResult };
  }
  if (res.status === 404) return { kind: 'not_member' };
  let body: ErrorBody | null = null;
  try {
    body = (await res.json()) as ErrorBody;
  } catch {
    // Not JSON.
  }
  const code = body?.error?.code ?? '';
  const message = body?.error?.message || `HTTP ${res.status}`;
  const details = body?.error?.details;
  if (res.status === 409 && code === 'last_owner') {
    const guidance = typeof details?.guidance === 'string' ? details.guidance : '';
    return { kind: 'last_owner', message, guidance };
  }
  if (res.status === 409 && code === 'membership_group_derived') {
    return { kind: 'group_derived', message, groups: readGroups(details?.groups) };
  }
  if (res.status === 403 && details?.credential === 'session_required') {
    return { kind: 'session_required', message: LEAVE_SESSION_REQUIRED_MESSAGE };
  }
  return { kind: 'error', status: res.status, code, message };
}

/** POST …/leave. Never throws: a network failure is an `error` outcome. */
export async function requestLeave(projectId: string, dryRun: boolean): Promise<LeaveOutcome> {
  try {
    const res = await apiFetch(`/api/v1/projects/${encodeURIComponent(projectId)}/leave`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ dryRun }),
      suppressAccessDeniedToast: true,
    });
    return await readLeaveResponse(res);
  } catch (err) {
    return {
      kind: 'error',
      status: null,
      code: '',
      message: err instanceof Error ? err.message : 'Failed to reach the Hub',
    };
  }
}

/** How a project's or members list's capabilities present the Leave action. */
export type LeaveVisibility =
  | { kind: 'hidden' }
  | { kind: 'enabled' }
  | { kind: 'blocked'; reason: string };

export const LEAVE_BLOCKED_LAST_OWNER =
  "You're the last owner of this project. Transfer ownership to another member, or delete the project, before you leave.";
export const LEAVE_BLOCKED_GROUP_DERIVED =
  "You're a member through a group. Leave the group, or ask a group admin to remove you.";

/**
 * Leave is offered only when the server's capabilities say so: `canLeave`
 * true enables it; `canLeave` false with a known reason shows it disabled
 * with guidance; anything else (the experiment off, no direct binding, a
 * token credential) hides it. The dry run stays the final authority.
 */
export function leaveVisibility(
  caps:
    | Pick<Capabilities | MembershipCapabilities, 'canLeave' | 'leaveBlockedReason'>
    | null
    | undefined
): LeaveVisibility {
  if (!caps || typeof caps.canLeave !== 'boolean') return { kind: 'hidden' };
  if (caps.canLeave) return { kind: 'enabled' };
  if (caps.leaveBlockedReason === 'last_owner') {
    return { kind: 'blocked', reason: LEAVE_BLOCKED_LAST_OWNER };
  }
  if (caps.leaveBlockedReason === 'group_derived') {
    return { kind: 'blocked', reason: LEAVE_BLOCKED_GROUP_DERIVED };
  }
  return { kind: 'hidden' };
}

/** The result of {@link deleteLeaveAgents}. */
export type LeaveAgentsDeleteResult =
  | { kind: 'done'; deleted: LeaveAgentItem[] }
  /** Stopped at `failed`; `deleted` were confirmed before it. */
  | { kind: 'stopped'; deleted: LeaveAgentItem[]; failed: LeaveAgentItem; message: string }
  /** The list was cut short, so it cannot be deleted in full; nothing was sent. */
  | { kind: 'truncated' };

/** The delete step, replaceable in tests. */
export type LeaveAgentDeleter = (
  agentId: string,
  agentName: string,
  opts?: AgentDeletionWaitOptions
) => Promise<AgentDeleteWaitOutcome>;

/**
 * Delete each agent of a dry run's list in order, waiting for the hub to
 * confirm each one, and stop at the first that is not confirmed. A list the
 * hub cut short is refused before any delete.
 */
export async function deleteLeaveAgents(
  agents: LeaveResult['agents'],
  opts: {
    deleter?: LeaveAgentDeleter | undefined;
    wait?: AgentDeletionWaitOptions | undefined;
    onProgress?: ((agent: LeaveAgentItem, index: number) => void) | undefined;
  } = {}
): Promise<LeaveAgentsDeleteResult> {
  if (agents.truncated || agents.items.length < agents.count) return { kind: 'truncated' };
  const deleter = opts.deleter ?? deleteAgentAndWait;
  const deleted: LeaveAgentItem[] = [];
  for (const [index, agent] of agents.items.entries()) {
    opts.onProgress?.(agent, index);
    const outcome = await deleter(agent.id, agent.name || agent.id, opts.wait);
    if (outcome.kind !== 'confirmed') {
      return { kind: 'stopped', deleted, failed: agent, message: outcome.message };
    }
    deleted.push(agent);
  }
  return { kind: 'done', deleted };
}

/** Heading of a project view whose project answered 404 (left, removed or gone). */
export const PROJECT_NO_ACCESS_TITLE = 'You no longer have access';
/** Text of that view. The hub answers a non-member and a missing project alike. */
export const PROJECT_NO_ACCESS_MESSAGE =
  "This project doesn't exist, or you no longer have access to it.";
