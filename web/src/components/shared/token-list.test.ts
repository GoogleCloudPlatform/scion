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
 * Tests for token-list.ts's project-scoped mint eligibility (ptone/scion#2122):
 * fetching GET /api/v1/auth/scopes?projectId= per selected project, disabling
 * ineligible scopes with a visible reason, alias eligibility requiring every
 * expanded member to be eligible, per-project caching, and surfacing the
 * denied selector from a 403 scope_violation mint response.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let ScionTokenList: any;
let formatEligibilityReason: (reason?: string) => string;
let relationshipBadgeText: (scope: string) => string;

function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** The unchanged catalog shape: no eligibility on any entry. */
const CATALOG_RESPONSE = {
  scopes: [
    { id: 'agent:read', resource: 'agent', action: 'read', description: 'Read agents' },
    {
      id: 'agent:attach',
      resource: 'agent',
      action: 'attach',
      description: 'Attach to agent sessions',
    },
    { id: 'agent:delete', resource: 'agent', action: 'delete', description: 'Delete agents' },
  ],
  aliases: [
    {
      id: 'agent:manage',
      description: 'All agent management operations',
      expands_to: ['agent:delete', 'agent:read'],
    },
  ],
};

/** A project-boundary eligibility response: attach eligible, delete is not. */
function eligibilityResponse(projectId: string) {
  return {
    scopes: [
      {
        id: 'agent:attach',
        resource: 'agent',
        action: 'attach',
        description: 'Attach to agent sessions',
        eligibilityKind: 'relationship',
        eligibility: { boundary: { kind: 'project', projectId }, eligible: true },
      },
      {
        id: 'agent:delete',
        resource: 'agent',
        action: 'delete',
        description: 'Delete agents',
        eligibilityKind: 'flat_role',
        eligibility: {
          boundary: { kind: 'project', projectId },
          eligible: false,
          reason: 'flat_role_insufficient',
        },
      },
      {
        id: 'agent:read',
        resource: 'agent',
        action: 'read',
        description: 'Read agents',
        eligibilityKind: 'flat_role',
        eligibility: { boundary: { kind: 'project', projectId }, eligible: true },
      },
    ],
    aliases: [
      {
        id: 'agent:manage',
        description: 'All agent management operations',
        expands_to: ['agent:delete', 'agent:read'],
        eligibility: {
          boundary: { kind: 'project', projectId },
          eligible: false,
          ineligibleMembers: ['agent:delete'],
        },
      },
    ],
  };
}

/** Like eligibilityResponse, but agent:attach is ineligible -- used to tell
 * two different projects' responses apart in race-condition tests. */
function eligibilityResponseIneligible(projectId: string) {
  const resp = eligibilityResponse(projectId);
  resp.scopes[0] = {
    ...resp.scopes[0],
    eligibility: { boundary: { kind: 'project', projectId }, eligible: false, reason: 'no_relationship_candidacy' },
  };
  return resp;
}

async function createComponent(
  fetchMock: (input: string | URL | Request, init?: RequestInit) => Promise<Response>
) {
  vi.stubGlobal('fetch', vi.fn(fetchMock));
  const el = document.createElement('scion-token-list') as InstanceType<typeof ScionTokenList>;
  document.body.appendChild(el);
  await el.updateComplete;
  // Let the async connectedCallback fetches (loadScopes/loadData) settle.
  await new Promise((r) => setTimeout(r, 20));
  await el.updateComplete;
  return el;
}

/** Default fetch router: empty tokens/projects, plain catalog for scopes. */
function baseFetch(
  overrides: Partial<{
    scopes: (url: string) => Response | Promise<Response>;
    projects: () => Response;
    tokens: () => Response;
    create: (init?: RequestInit) => Response;
  }> = {}
) {
  return (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const url = typeof input === 'string' ? input : input.toString();
    const method = init?.method ?? 'GET';
    if (url.startsWith('/api/v1/auth/scopes')) {
      return Promise.resolve(overrides.scopes ? overrides.scopes(url) : jsonResponse(CATALOG_RESPONSE));
    }
    if (url === '/api/v1/auth/tokens' && method === 'POST') {
      return Promise.resolve(
        overrides.create ? overrides.create(init) : jsonResponse({ token: 'scion_pat_x' })
      );
    }
    if (url.startsWith('/api/v1/auth/tokens')) {
      return Promise.resolve(overrides.tokens ? overrides.tokens() : jsonResponse({ items: [] }));
    }
    if (url.startsWith('/api/v1/projects')) {
      return Promise.resolve(overrides.projects ? overrides.projects() : jsonResponse({ projects: [] }));
    }
    return Promise.resolve(jsonResponse({}));
  };
}

describe('scion-token-list — project eligibility (ptone/scion#2122)', () => {
  beforeAll(async () => {
    const mod = await import('./token-list.js');
    ScionTokenList = mod.ScionTokenList;
    formatEligibilityReason = mod.formatEligibilityReason;
    relationshipBadgeText = mod.relationshipBadgeText;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('loads the plain catalog with no eligibility before any project is selected', async () => {
    const el = await createComponent(baseFetch());

    const scopes = (el as any).availableScopes;
    const attach = scopes.find((s: any) => s.value === 'agent:attach');
    expect(attach).toBeDefined();
    expect(attach.eligible).toBeUndefined();
    expect(attach.eligibilityReason).toBeUndefined();
  });

  it('fetches ?projectId= and disables an ineligible scope with its reason', async () => {
    const projectId = 'proj-1';
    const el = await createComponent(
      baseFetch({
        scopes: (url) =>
          url.includes(`projectId=${projectId}`)
            ? jsonResponse(eligibilityResponse(projectId))
            : jsonResponse(CATALOG_RESPONSE),
      })
    );

    await (el as any).loadScopes(projectId);
    await el.updateComplete;

    const scopes = (el as any).availableScopes;
    const attach = scopes.find((s: any) => s.value === 'agent:attach');
    const del = scopes.find((s: any) => s.value === 'agent:delete');

    expect(attach.eligible).toBe(true);
    expect(attach.eligibilityKind).toBe('relationship');
    expect(del.eligible).toBe(false);
    expect(del.eligibilityReason).toBe('flat_role_insufficient');
  });

  it('formats flat_role_insufficient with a human-readable label, and falls back to the raw code for an unmapped reason', () => {
    expect(formatEligibilityReason('flat_role_insufficient')).toBe(
      'your project role does not include this permission'
    );
    expect(formatEligibilityReason('some_future_reason_code')).toBe('some_future_reason_code');
    expect(formatEligibilityReason(undefined)).toBe('not currently selectable');
  });

  it('describes port_access reach through roles that grant it in its relationship badge', () => {
    expect(relationshipBadgeText('agent:attach')).toBe(
      'Own agents & descendants — checked per agent'
    );
    expect(relationshipBadgeText('agent:port_access')).toBe(
      'Own agents & descendants, or any agent in the project if your role grants port access — checked per agent'
    );
  });

  it('refuses to select an ineligible scope even if toggled directly', async () => {
    const projectId = 'proj-1';
    const el = await createComponent(
      baseFetch({
        scopes: (url) =>
          url.includes(`projectId=${projectId}`)
            ? jsonResponse(eligibilityResponse(projectId))
            : jsonResponse(CATALOG_RESPONSE),
      })
    );

    await (el as any).loadScopes(projectId);
    await el.updateComplete;

    (el as any).toggleScope('agent:delete');
    expect((el as any).createScopes.has('agent:delete')).toBe(false);

    (el as any).toggleScope('agent:attach');
    expect((el as any).createScopes.has('agent:attach')).toBe(true);
  });

  it('marks an alias ineligible when any expanded member is ineligible, and names the members', async () => {
    const projectId = 'proj-2';
    const el = await createComponent(
      baseFetch({
        scopes: (url) =>
          url.includes(`projectId=${projectId}`)
            ? jsonResponse(eligibilityResponse(projectId))
            : jsonResponse(CATALOG_RESPONSE),
      })
    );

    await (el as any).loadScopes(projectId);
    await el.updateComplete;

    const alias = (el as any).availableScopes.find((s: any) => s.value === 'agent:manage');
    expect(alias.eligible).toBe(false);
    expect(alias.ineligibleMembers).toEqual(['agent:delete']);
  });

  it('caches eligibility per project and does not refetch on repeat selection', async () => {
    const projectId = 'proj-3';
    let scopeFetches = 0;
    const el = await createComponent(
      baseFetch({
        scopes: (url) => {
          if (url.includes(`projectId=${projectId}`)) scopeFetches++;
          return url.includes(`projectId=${projectId}`)
            ? jsonResponse(eligibilityResponse(projectId))
            : jsonResponse(CATALOG_RESPONSE);
        },
      })
    );

    expect(scopeFetches).toBe(0);
    await (el as any).loadScopes(projectId);
    expect(scopeFetches).toBe(1);
    await (el as any).loadScopes(projectId);
    expect(scopeFetches).toBe(1);
  });

  it('handleProjectChange clears previously selected scopes and refetches eligibility', async () => {
    const projectId = 'proj-4';
    const el = await createComponent(
      baseFetch({
        scopes: (url) =>
          url.includes(`projectId=${projectId}`)
            ? jsonResponse(eligibilityResponse(projectId))
            : jsonResponse(CATALOG_RESPONSE),
      })
    );

    (el as any).createScopes = new Set(['agent:read']);
    (el as any).handleProjectChange(projectId);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 10));

    expect((el as any).createProjectId).toBe(projectId);
    expect((el as any).createScopes.size).toBe(0);
    const attach = (el as any).availableScopes.find((s: any) => s.value === 'agent:attach');
    expect(attach.eligible).toBe(true);
  });

  it('surfaces the denied selector in createError for a 403 scope_violation', async () => {
    const el = await createComponent(
      baseFetch({
        projects: () => jsonResponse({ projects: [{ id: 'p1', name: 'P1' }] }),
        create: () =>
          new Response(
            JSON.stringify({
              error: {
                code: 'scope_violation',
                message:
                  'requested scopes exceed issuer authority: selector "agent:delete" denied (flat_role_insufficient)',
                details: { selector: 'agent:delete', reason: 'flat_role_insufficient' },
              },
            }),
            { status: 403, headers: { 'Content-Type': 'application/json' } }
          ),
      })
    );

    (el as any).createName = 'my-token';
    (el as any).createProjectId = 'p1';
    (el as any).createScopes = new Set(['agent:delete']);

    await (el as any).handleCreate(new Event('submit'));

    expect((el as any).createError).toContain('agent:delete');
  });

  it('a 403 on project switch does not keep the previous project eligibility on screen', async () => {
    const el = await createComponent(
      baseFetch({
        scopes: (url) => {
          if (url.includes('projectId=proj-a')) return jsonResponse(eligibilityResponse('proj-a'));
          if (url.includes('projectId=proj-b')) {
            return jsonResponse({ error: { code: 'forbidden', message: 'forbidden' } }, 403);
          }
          return jsonResponse(CATALOG_RESPONSE);
        },
      })
    );

    await (el as any).loadScopes('proj-a');
    await el.updateComplete;
    let del = (el as any).availableScopes.find((s: any) => s.value === 'agent:delete');
    expect(del.eligible).toBe(false); // sanity: project A's eligibility loaded first

    (el as any).handleProjectChange('proj-b');
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    // Project B's fetch failed: must show the plain catalog, not A's
    // eligibility, and must record which project failed.
    del = (el as any).availableScopes.find((s: any) => s.value === 'agent:delete');
    expect(del.eligible).toBeUndefined();
    expect((el as any).scopesErrorProjectId).toBe('proj-b');
  });

  it('an empty eligibility list for a newly selected project replaces the previous project list', async () => {
    const el = await createComponent(
      baseFetch({
        scopes: (url) => {
          if (url.includes('projectId=proj-a')) return jsonResponse(eligibilityResponse('proj-a'));
          if (url.includes('projectId=proj-b')) return jsonResponse({ scopes: [], aliases: [] });
          return jsonResponse(CATALOG_RESPONSE);
        },
      })
    );

    await (el as any).loadScopes('proj-a');
    await el.updateComplete;
    expect((el as any).availableScopes.length).toBeGreaterThan(0); // sanity: project A loaded

    await (el as any).loadScopes('proj-b');
    await el.updateComplete;

    expect((el as any).availableScopes).toEqual([]);
    expect((el as any).scopesErrorProjectId).toBeNull();
  });

  it('an older, slower project response cannot overwrite a newer one that already resolved', async () => {
    const gateA = deferred<Response>();
    const el = await createComponent(
      baseFetch({
        scopes: (url) => {
          if (url.includes('projectId=proj-a')) return gateA.promise;
          if (url.includes('projectId=proj-b')) return jsonResponse(eligibilityResponseIneligible('proj-b'));
          return jsonResponse(CATALOG_RESPONSE);
        },
      })
    );

    // Start A (slow, gated) then switch to B (fast) before A resolves.
    const loadA = (el as any).loadScopes('proj-a') as Promise<void>;
    const loadB = (el as any).loadScopes('proj-b') as Promise<void>;
    await loadB;
    await el.updateComplete;

    let attach = (el as any).availableScopes.find((s: any) => s.value === 'agent:attach');
    expect(attach.eligible).toBe(false); // B's (ineligible) fixture, confirming B applied

    // Now let A's slow response land after B's.
    gateA.resolve(jsonResponse(eligibilityResponse('proj-a')));
    await loadA;
    await el.updateComplete;

    // A must not have overwritten B's already-applied, newer state.
    attach = (el as any).availableScopes.find((s: any) => s.value === 'agent:attach');
    expect(attach.eligible).toBe(false);
  });
});
