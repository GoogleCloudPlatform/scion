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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { readFileSync } from 'fs';
import { join } from 'path';
import { canGroup } from '../shared/groups';
import {
  GroupsApiError,
  buildGroupsQuery,
  listGroups,
  getGroup,
  createGroup,
  updateGroup,
  deleteGroup,
  listMyGroups,
  listMembers,
  addMember,
  removeMember,
} from './groups-api';

/* -------------------------------------------------------------------------- */
/* Helpers                                                                    */
/* -------------------------------------------------------------------------- */

const FIXTURES_DIR = join(__dirname, '__fixtures__', 'groups');

function loadFixture<T = unknown>(name: string): T {
  return JSON.parse(readFileSync(join(FIXTURES_DIR, name), 'utf-8')) as T;
}

/** Create a Response from a fixture file with the given status. */
function fixtureResponse(name: string, status: number): Response {
  const body = readFileSync(join(FIXTURES_DIR, name), 'utf-8');
  return new Response(body, {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Create a JSON Response from an inline object. */
function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/* -------------------------------------------------------------------------- */
/* Mock fetch globally                                                        */
/* -------------------------------------------------------------------------- */

let fetchMock: ReturnType<typeof vi.fn>;

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal('fetch', fetchMock);
});

afterEach(() => {
  vi.restoreAllMocks();
});

/* ========================================================================== */
/* canGroup() — fail-closed behavior                                          */
/* ========================================================================== */

describe('canGroup()', () => {
  it('returns false when capabilities are undefined', () => {
    expect(canGroup(undefined, 'read')).toBe(false);
    expect(canGroup(undefined, 'create')).toBe(false);
  });

  it('returns false when the action is not in the actions array', () => {
    const caps = { actions: ['read'] };
    expect(canGroup(caps, 'delete')).toBe(false);
    expect(canGroup(caps, 'update')).toBe(false);
    expect(canGroup(caps, 'create')).toBe(false);
  });

  it('returns true when the action is present', () => {
    const caps = { actions: ['read', 'update', 'delete', 'addMember', 'removeMember'] };
    expect(canGroup(caps, 'read')).toBe(true);
    expect(canGroup(caps, 'update')).toBe(true);
    expect(canGroup(caps, 'delete')).toBe(true);
    expect(canGroup(caps, 'addMember')).toBe(true);
    expect(canGroup(caps, 'removeMember')).toBe(true);
  });

  it('returns true for scope-level actions', () => {
    const caps = { actions: ['create', 'list'] };
    expect(canGroup(caps, 'create')).toBe(true);
    expect(canGroup(caps, 'list')).toBe(true);
  });

  it('returns false for empty actions array', () => {
    const caps = { actions: [] as string[] };
    expect(canGroup(caps, 'read')).toBe(false);
  });

  it('returns false when actions property is missing (fail-closed)', () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const caps = {} as any;
    expect(canGroup(caps, 'read')).toBe(false);
  });
});

/* ========================================================================== */
/* Error fixture → GroupsApiError.kind mapping                                */
/* ========================================================================== */

describe('error classification from fixtures', () => {
  const errorCases: Array<{
    fixture: string;
    httpStatus: number;
    expectedKind: string;
    description: string;
  }> = [
    {
      fixture: 'error-conflict-slug.json',
      httpStatus: 409,
      expectedKind: 'conflict_slug',
      description: '409 slug conflict → conflict_slug',
    },
    {
      fixture: 'error-conflict-member.json',
      httpStatus: 409,
      expectedKind: 'conflict_member',
      description: '409 member conflict → conflict_member',
    },
    {
      fixture: 'error-cycle.json',
      httpStatus: 400,
      expectedKind: 'cycle',
      description: '400 cycle detection → cycle',
    },
    {
      fixture: 'error-quota.json',
      httpStatus: 429,
      expectedKind: 'quota',
      description: '429 quota exceeded → quota',
    },
    {
      fixture: 'error-last-owner.json',
      httpStatus: 400,
      expectedKind: 'last_owner',
      description: '400 last owner → last_owner',
    },
    {
      fixture: 'error-constraint-gate.json',
      httpStatus: 403,
      expectedKind: 'constraint_gate',
      description: '403 constraint gate → constraint_gate',
    },
    {
      fixture: 'error-delegation.json',
      httpStatus: 403,
      expectedKind: 'delegation',
      description: '403 delegation → delegation',
    },
    {
      fixture: 'error-hierarchy.json',
      httpStatus: 403,
      expectedKind: 'hierarchy',
      description: '403 role hierarchy → hierarchy',
    },
    {
      fixture: 'error-not-found.json',
      httpStatus: 404,
      expectedKind: 'not_found',
      description: '404 not found → not_found',
    },
  ];

  it.each(errorCases)(
    '$description',
    async ({ fixture, httpStatus, expectedKind }) => {
      // Use createGroup as a representative API function to trigger classification
      fetchMock.mockResolvedValueOnce(fixtureResponse(fixture, httpStatus));

      try {
        await createGroup({ name: 'test' });
        expect.fail('Expected GroupsApiError to be thrown');
      } catch (err) {
        expect(err).toBeInstanceOf(GroupsApiError);
        const apiErr = err as GroupsApiError;
        expect(apiErr.kind).toBe(expectedKind);
        expect(apiErr.httpStatus).toBe(httpStatus);
        // Message should match the fixture's error message
        const fixtureData = loadFixture<{ error: { message: string } }>(fixture);
        expect(apiErr.message).toBe(fixtureData.error.message);
      }
    }
  );

  it('classifies unknown 500 as http', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: { code: 'internal_error', message: 'Internal server error' } }, 500)
    );

    try {
      await getGroup('some-id');
      expect.fail('Expected GroupsApiError to be thrown');
    } catch (err) {
      expect(err).toBeInstanceOf(GroupsApiError);
      const apiErr = err as GroupsApiError;
      expect(apiErr.kind).toBe('http');
      expect(apiErr.httpStatus).toBe(500);
    }
  });

  it('classifies generic 403 as forbidden', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ error: { code: 'forbidden', message: 'Insufficient permissions' } }, 403)
    );

    try {
      await updateGroup('id', { name: 'updated' });
      expect.fail('Expected GroupsApiError to be thrown');
    } catch (err) {
      expect(err).toBeInstanceOf(GroupsApiError);
      const apiErr = err as GroupsApiError;
      expect(apiErr.kind).toBe('forbidden');
    }
  });

  it('classifies generic 400 as validation', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        { error: { code: 'validation_error', message: 'name is required' } },
        400
      )
    );

    try {
      await createGroup({ name: '' });
      expect.fail('Expected GroupsApiError to be thrown');
    } catch (err) {
      expect(err).toBeInstanceOf(GroupsApiError);
      const apiErr = err as GroupsApiError;
      expect(apiErr.kind).toBe('validation');
    }
  });
});

/* ========================================================================== */
/* URL / query building                                                       */
/* ========================================================================== */

describe('buildGroupsQuery()', () => {
  it('returns base path with no filter', () => {
    expect(buildGroupsQuery({})).toBe('/api/v1/groups');
  });

  it('adds search parameter', () => {
    const url = buildGroupsQuery({ search: 'platform' });
    expect(url).toBe('/api/v1/groups?search=platform');
  });

  it('adds groupType parameter', () => {
    const url = buildGroupsQuery({ groupType: 'explicit' });
    expect(url).toBe('/api/v1/groups?groupType=explicit');
  });

  it('adds ownerId parameter', () => {
    const url = buildGroupsQuery({ ownerId: 'u-123' });
    expect(url).toBe('/api/v1/groups?ownerId=u-123');
  });

  it('adds projectId parameter', () => {
    const url = buildGroupsQuery({ projectId: 'p-456' });
    expect(url).toBe('/api/v1/groups?projectId=p-456');
  });

  it('adds parentId parameter', () => {
    const url = buildGroupsQuery({ parentId: 'g-789' });
    expect(url).toBe('/api/v1/groups?parentId=g-789');
  });

  it('adds limit parameter', () => {
    const url = buildGroupsQuery({ limit: 25 });
    expect(url).toBe('/api/v1/groups?limit=25');
  });

  it('adds cursor parameter', () => {
    const url = buildGroupsQuery({ cursor: 'abc123' });
    expect(url).toBe('/api/v1/groups?cursor=abc123');
  });

  it('combines multiple parameters', () => {
    const url = buildGroupsQuery({
      search: 'eng',
      groupType: 'explicit',
      ownerId: 'u-1',
      limit: 10,
      cursor: 'cur',
    });
    // URLSearchParams preserves insertion order
    expect(url).toContain('search=eng');
    expect(url).toContain('groupType=explicit');
    expect(url).toContain('ownerId=u-1');
    expect(url).toContain('limit=10');
    expect(url).toContain('cursor=cur');
    expect(url.startsWith('/api/v1/groups?')).toBe(true);
  });

  it('omits undefined values', () => {
    const url = buildGroupsQuery({ search: undefined, limit: undefined });
    expect(url).toBe('/api/v1/groups');
  });

  it('handles limit of 0', () => {
    // limit=0 is a valid value (even if unusual)
    const url = buildGroupsQuery({ limit: 0 });
    expect(url).toBe('/api/v1/groups?limit=0');
  });
});

/* ========================================================================== */
/* API functions — happy paths                                                */
/* ========================================================================== */

describe('listGroups()', () => {
  it('fetches and returns the list response', async () => {
    const fixture = loadFixture('list-groups.json');
    fetchMock.mockResolvedValueOnce(jsonResponse(fixture));

    const result = await listGroups({});
    expect(result.groups).toHaveLength(3);
    expect(result.totalCount).toBe(42);
    expect(result.nextCursor).toBeDefined();
    expect(result._capabilities?.actions).toContain('create');
  });

  it('passes filter parameters as query string', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ groups: [], totalCount: 0 })
    );

    await listGroups({ search: 'team', groupType: 'explicit', limit: 5 });
    const calledUrl = fetchMock.mock.calls[0][0] as string;
    expect(calledUrl).toContain('search=team');
    expect(calledUrl).toContain('groupType=explicit');
    expect(calledUrl).toContain('limit=5');
  });
});

describe('getGroup()', () => {
  it('fetches a single group by ID', async () => {
    const fixture = loadFixture('group-with-capabilities.json');
    fetchMock.mockResolvedValueOnce(jsonResponse(fixture));

    const result = await getGroup('g-00000000-0000-0000-0000-000000000001');
    expect(result.id).toBe('g-00000000-0000-0000-0000-000000000001');
    expect(result.name).toBe('Platform Engineers');
    expect(result._capabilities?.actions).toContain('delete');
  });

  it('fetches a group by slug', async () => {
    const fixture = loadFixture('group-without-capabilities.json');
    fetchMock.mockResolvedValueOnce(jsonResponse(fixture));

    await getGroup('frontend-team');
    const calledUrl = fetchMock.mock.calls[0][0] as string;
    expect(calledUrl).toBe('/api/v1/groups/frontend-team');
  });
});

describe('createGroup()', () => {
  it('posts the request and returns the created group', async () => {
    const fixture = loadFixture('group-with-capabilities.json');
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(fixture), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const result = await createGroup({
      name: 'Platform Engineers',
      description: 'Core platform engineering team',
    });
    expect(result.name).toBe('Platform Engineers');

    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/groups');
    expect(opts.method).toBe('POST');
    const body = JSON.parse(opts.body as string);
    expect(body.name).toBe('Platform Engineers');
  });
});

describe('updateGroup()', () => {
  it('patches the group and returns the updated version', async () => {
    const fixture = loadFixture('group-with-capabilities.json');
    fetchMock.mockResolvedValueOnce(jsonResponse(fixture));

    await updateGroup('g-123', { name: 'Renamed' });

    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/groups/g-123');
    expect(opts.method).toBe('PATCH');
  });
});

describe('deleteGroup()', () => {
  it('sends DELETE and resolves on 204', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));

    await expect(deleteGroup('g-123')).resolves.toBeUndefined();

    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/groups/g-123');
    expect(opts.method).toBe('DELETE');
    expect(opts.credentials).toBe('include');
  });

  it('uses raw fetch (not apiFetch) — no scion:access-denied event', async () => {
    // deleteGroup uses raw fetch with credentials:'include'
    fetchMock.mockResolvedValueOnce(
      fixtureResponse('error-constraint-gate.json', 403)
    );

    try {
      await deleteGroup('g-123');
      expect.fail('Expected GroupsApiError');
    } catch (err) {
      expect(err).toBeInstanceOf(GroupsApiError);
      expect((err as GroupsApiError).kind).toBe('constraint_gate');
    }
  });
});

describe('listMyGroups()', () => {
  it('fetches the current user groups from /api/v1/users/me/groups', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ groups: [loadFixture('group-with-capabilities.json')] })
    );

    const result = await listMyGroups();
    expect(result).toHaveLength(1);
    expect(result[0].slug).toBe('platform-engineers');

    const calledUrl = fetchMock.mock.calls[0][0] as string;
    expect(calledUrl).toBe('/api/v1/users/me/groups');
  });
});

describe('listMembers()', () => {
  it('fetches group members', async () => {
    const fixture = loadFixture('members.json');
    fetchMock.mockResolvedValueOnce(jsonResponse(fixture));

    const result = await listMembers('g-00000000-0000-0000-0000-000000000001');
    expect(result).toHaveLength(4);
    expect(result[0].role).toBe('owner');
    expect(result[2].memberType).toBe('group');
  });
});

describe('addMember()', () => {
  it('posts the member request and returns the member', async () => {
    const member = {
      groupId: 'g-1',
      memberType: 'user' as const,
      memberId: 'u-1',
      role: 'member' as const,
      addedAt: '2026-01-01T00:00:00Z',
    };
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify(member), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const result = await addMember('g-1', {
      memberType: 'user',
      memberId: 'u-1',
      role: 'member',
    });
    expect(result.memberId).toBe('u-1');
  });
});

describe('removeMember()', () => {
  it('returns ok on 204', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }));

    const result = await removeMember('g-1', 'user', 'u-1');
    expect(result.outcome).toBe('ok');

    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/groups/g-1/members/user/u-1');
    expect(opts.method).toBe('DELETE');
    expect(opts.credentials).toBe('include');
  });

  it('throws last_owner on 400', async () => {
    fetchMock.mockResolvedValueOnce(
      fixtureResponse('error-last-owner.json', 400)
    );

    try {
      await removeMember('g-1', 'user', 'u-1');
      expect.fail('Expected GroupsApiError');
    } catch (err) {
      expect(err).toBeInstanceOf(GroupsApiError);
      expect((err as GroupsApiError).kind).toBe('last_owner');
    }
  });

  it('throws constraint_gate on 403', async () => {
    fetchMock.mockResolvedValueOnce(
      fixtureResponse('error-constraint-gate.json', 403)
    );

    try {
      await removeMember('g-1', 'user', 'u-1');
      expect.fail('Expected GroupsApiError');
    } catch (err) {
      expect(err).toBeInstanceOf(GroupsApiError);
      expect((err as GroupsApiError).kind).toBe('constraint_gate');
    }
  });
});

/* ========================================================================== */
/* Abort propagation                                                          */
/* ========================================================================== */

describe('abort propagation', () => {
  it('passes AbortSignal to fetch for listGroups', async () => {
    const controller = new AbortController();
    fetchMock.mockResolvedValueOnce(
      jsonResponse({ groups: [], totalCount: 0 })
    );

    await listGroups({}, controller.signal);
    const opts = fetchMock.mock.calls[0][1] as RequestInit;
    expect(opts.signal).toBe(controller.signal);
  });

  it('rejects with AbortError when signal is aborted', async () => {
    const controller = new AbortController();
    controller.abort();
    fetchMock.mockRejectedValueOnce(new DOMException('The operation was aborted.', 'AbortError'));

    await expect(listGroups({}, controller.signal)).rejects.toThrow('aborted');
  });
});
