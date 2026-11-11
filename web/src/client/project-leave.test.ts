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
 * Project self-leave client (ptone/scion#7494): the reading of each answer
 * of POST …/leave, the capability gate, and the delete-first sequence.
 */

import { describe, it, expect, vi, afterEach } from 'vitest';

import {
  deleteLeaveAgents,
  leaveVisibility,
  readLeaveResponse,
  requestLeave,
  LEAVE_BLOCKED_GROUP_DERIVED,
  LEAVE_BLOCKED_LAST_OWNER,
  LEAVE_EXPERIMENT_OFF_MESSAGE,
  LEAVE_SESSION_REQUIRED_MESSAGE,
  LEAVE_UNSUPPORTED_MESSAGE,
  type LeaveAgentDeleter,
  type LeaveResult,
} from './project-leave.js';
import { requestBodyText, requestUrl } from './__fixtures__/request-url.js';
import type { Capabilities } from '../shared/types.js';

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const apiError = (status: number, code: string, message: string, details?: unknown): Response =>
  json(status, { error: { code, message, details } });

function agents(
  n: number,
  opts: { truncated?: boolean; count?: number } = {}
): LeaveResult['agents'] {
  return {
    count: opts.count ?? n,
    truncated: opts.truncated ?? false,
    items: Array.from({ length: n }, (_, i) => ({
      id: `a${i + 1}`,
      name: `agent-${i + 1}`,
      phase: 'running',
    })),
  };
}

const RESULT: LeaveResult = {
  projectId: 'p-1',
  dryRun: true,
  removedRoles: ['project-member'],
  remainingAccess: 'none',
  agents: agents(2),
  projectBoundTokens: { revoked: 1 },
  willBeHeld: true,
};

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('readLeaveResponse', () => {
  it('200 is the LeaveResult', async () => {
    expect(await readLeaveResponse(json(200, RESULT))).toEqual({ kind: 'ok', result: RESULT });
  });

  it('409 last_owner carries the guidance', async () => {
    const out = await readLeaveResponse(
      apiError(409, 'last_owner', 'Cannot remove the last owner', { guidance: 'Transfer it' })
    );
    expect(out).toEqual({
      kind: 'last_owner',
      message: 'Cannot remove the last owner',
      guidance: 'Transfer it',
    });
  });

  it('409 membership_group_derived lists only details.groups', async () => {
    const out = await readLeaveResponse(
      apiError(409, 'membership_group_derived', 'through a group', {
        groups: [{ id: 'g-1', name: 'Eng' }, { id: 'g-2' }, 'junk', null],
      })
    );
    expect(out).toEqual({
      kind: 'group_derived',
      message: 'through a group',
      groups: [
        { id: 'g-1', name: 'Eng' },
        { id: 'g-2', name: 'g-2' },
      ],
    });
  });

  it('403 with details.credential session_required is the login-session message', async () => {
    const out = await readLeaveResponse(
      apiError(403, 'forbidden', 'credential kind "uat" is not allowed', {
        reason: 'GOV_PENDING',
        credential: 'session_required',
      })
    );
    expect(out).toEqual({ kind: 'session_required', message: LEAVE_SESSION_REQUIRED_MESSAGE });
  });

  it('a 403 without session_required is a plain error', async () => {
    const out = await readLeaveResponse(apiError(403, 'forbidden', 'Forbidden'));
    expect(out).toEqual({ kind: 'error', status: 403, code: 'forbidden', message: 'Forbidden' });
  });

  it('404 is not a member', async () => {
    expect(await readLeaveResponse(apiError(404, 'not_found', 'Project not found.'))).toEqual({
      kind: 'not_member',
    });
  });

  it('404 "route not found" (the experiment off) is unavailable, not not-a-member', async () => {
    expect(await readLeaveResponse(apiError(404, 'not_found', 'route not found'))).toEqual({
      kind: 'unavailable',
      message: LEAVE_EXPERIMENT_OFF_MESSAGE,
    });
  });

  it('404 "Project route not found" (a hub without the route) is unavailable', async () => {
    expect(await readLeaveResponse(apiError(404, 'not_found', 'Project route not found'))).toEqual({
      kind: 'unavailable',
      message: LEAVE_UNSUPPORTED_MESSAGE,
    });
  });

  it('a 404 with no JSON body is not a member', async () => {
    expect(await readLeaveResponse(new Response('nope', { status: 404 }))).toEqual({
      kind: 'not_member',
    });
  });
});

describe('requestLeave', () => {
  it('posts {dryRun} to the leave route', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(json(200, RESULT)));
    vi.stubGlobal('fetch', fetchMock);
    await requestLeave('p 1', true);
    const [input, init] = fetchMock.mock.calls[0] as unknown as [RequestInfo, RequestInit];
    expect(requestUrl(input)).toBe('/api/v1/projects/p%201/leave');
    expect(init.method).toBe('POST');
    expect(JSON.parse(requestBodyText(init.body))).toEqual({ dryRun: true });
  });

  it('a network failure is an error outcome', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new Error('offline')))
    );
    expect(await requestLeave('p-1', false)).toEqual({
      kind: 'error',
      status: null,
      code: '',
      message: 'offline',
    });
  });
});

describe('leaveVisibility', () => {
  /** A project's capabilities, as the hub serves them. */
  const caps = (c: Capabilities): Capabilities => c;

  it('hidden when the capabilities carry no canLeave (experiment off)', () => {
    expect(leaveVisibility(caps({ actions: ['read'] }))).toEqual({ kind: 'hidden' });
    expect(leaveVisibility(undefined)).toEqual({ kind: 'hidden' });
    expect(leaveVisibility(null)).toEqual({ kind: 'hidden' });
  });

  it('enabled when canLeave is true', () => {
    expect(leaveVisibility(caps({ actions: [], canLeave: true }))).toEqual({ kind: 'enabled' });
  });

  it('blocked with guidance for last_owner and group_derived', () => {
    expect(
      leaveVisibility(caps({ actions: [], canLeave: false, leaveBlockedReason: 'last_owner' }))
    ).toEqual({ kind: 'blocked', reason: LEAVE_BLOCKED_LAST_OWNER });
    expect(
      leaveVisibility(caps({ actions: [], canLeave: false, leaveBlockedReason: 'group_derived' }))
    ).toEqual({ kind: 'blocked', reason: LEAVE_BLOCKED_GROUP_DERIVED });
  });

  it('hidden when canLeave is false with no known reason (a token credential)', () => {
    expect(leaveVisibility(caps({ actions: [], canLeave: false }))).toEqual({ kind: 'hidden' });
    expect(
      leaveVisibility(caps({ actions: [], canLeave: false, leaveBlockedReason: 'other' }))
    ).toEqual({ kind: 'hidden' });
  });
});

describe('deleteLeaveAgents', () => {
  it('deletes every agent in order when each is confirmed', async () => {
    const seen: string[] = [];
    const deleter: LeaveAgentDeleter = (id) => {
      seen.push(id);
      return Promise.resolve({ kind: 'confirmed' });
    };
    const out = await deleteLeaveAgents(agents(3), { deleter });
    expect(seen).toEqual(['a1', 'a2', 'a3']);
    expect(out).toMatchObject({ kind: 'done' });
    expect(out.kind === 'done' && out.deleted.map((a) => a.id)).toEqual(['a1', 'a2', 'a3']);
  });

  it('stops at the first unconfirmed delete and names it', async () => {
    const seen: string[] = [];
    const deleter: LeaveAgentDeleter = (id) => {
      seen.push(id);
      return Promise.resolve(
        id === 'a2'
          ? { kind: 'failed', reason: 'timed_out', message: 'not confirmed in time' }
          : { kind: 'confirmed' }
      );
    };
    const out = await deleteLeaveAgents(agents(3), { deleter });
    expect(seen).toEqual(['a1', 'a2']);
    expect(out).toEqual({
      kind: 'stopped',
      deleted: [{ id: 'a1', name: 'agent-1', phase: 'running' }],
      failed: { id: 'a2', name: 'agent-2', phase: 'running' },
      message: 'not confirmed in time',
    });
  });

  it('refuses a truncated list before any delete', async () => {
    const deleter = vi.fn<LeaveAgentDeleter>();
    expect(
      await deleteLeaveAgents(agents(50, { truncated: true, count: 51 }), { deleter })
    ).toEqual({ kind: 'truncated' });
    expect(await deleteLeaveAgents(agents(2, { count: 3 }), { deleter })).toEqual({
      kind: 'truncated',
    });
    expect(deleter).not.toHaveBeenCalled();
  });
});
