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
 * denied selector from a 403 scope_violation mint response; and for the hub
 * boundary, purpose and labels (ptone/scion#2125).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import type { ScionTokenList } from './token-list.js';
import type * as TokenListModule from './token-list.js';

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
    eligibility: {
      boundary: { kind: 'project', projectId },
      eligible: false,
      reason: 'no_relationship_candidacy',
    },
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
    const url = requestUrl(input);
    const method = init?.method ?? 'GET';
    if (url.startsWith('/api/v1/auth/scopes')) {
      return Promise.resolve(
        overrides.scopes ? overrides.scopes(url) : jsonResponse(CATALOG_RESPONSE)
      );
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
      return Promise.resolve(
        overrides.projects ? overrides.projects() : jsonResponse({ projects: [] })
      );
    }
    return Promise.resolve(jsonResponse({}));
  };
}

describe('scion-token-list — project eligibility (ptone/scion#2122)', () => {
  beforeAll(async () => {
    const mod = await import('./token-list.js');
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

    await (el as any).loadScopes({ kind: 'project', projectId });
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

    await (el as any).loadScopes({ kind: 'project', projectId });
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

    await (el as any).loadScopes({ kind: 'project', projectId });
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
    await (el as any).loadScopes({ kind: 'project', projectId });
    expect(scopeFetches).toBe(1);
    await (el as any).loadScopes({ kind: 'project', projectId });
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

    await (el as any).loadScopes({ kind: 'project', projectId: 'proj-a' });
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
    expect((el as any).scopesErrorKey).toBe('project:proj-b');
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

    await (el as any).loadScopes({ kind: 'project', projectId: 'proj-a' });
    await el.updateComplete;
    expect((el as any).availableScopes.length).toBeGreaterThan(0); // sanity: project A loaded

    await (el as any).loadScopes({ kind: 'project', projectId: 'proj-b' });
    await el.updateComplete;

    expect((el as any).availableScopes).toEqual([]);
    expect((el as any).scopesErrorKey).toBeNull();
  });

  it('an older, slower project response cannot overwrite a newer one that already resolved', async () => {
    const gateA = deferred<Response>();
    const el = await createComponent(
      baseFetch({
        scopes: (url) => {
          if (url.includes('projectId=proj-a')) return gateA.promise;
          if (url.includes('projectId=proj-b'))
            return jsonResponse(eligibilityResponseIneligible('proj-b'));
          return jsonResponse(CATALOG_RESPONSE);
        },
      })
    );

    // Start A (slow, gated) then switch to B (fast) before A resolves.
    const loadA = (el as any).loadScopes({ kind: 'project', projectId: 'proj-a' }) as Promise<void>;
    const loadB = (el as any).loadScopes({ kind: 'project', projectId: 'proj-b' }) as Promise<void>;
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

/** A hub-boundary eligibility response: broker:create allowed for hub only. */
const HUB_ELIGIBILITY_RESPONSE = {
  scopes: [
    {
      id: 'agent:read',
      resource: 'agent',
      action: 'read',
      description: 'Read agents',
      allowedBoundaries: ['project', 'hub'],
      eligibility: { boundary: { kind: 'hub' }, eligible: true },
    },
    {
      id: 'broker:create',
      resource: 'broker',
      action: 'create',
      description: 'Create brokers',
      allowedBoundaries: ['hub'],
      eligibility: { boundary: { kind: 'hub' }, eligible: false, reason: 'authority_required' },
    },
  ],
  aliases: [],
};

/** A catalog whose entries carry allowedBoundaries (no eligibility). */
const CATALOG_WITH_BOUNDARIES = {
  scopes: [
    {
      id: 'agent:read',
      resource: 'agent',
      action: 'read',
      description: 'Read agents',
      allowedBoundaries: ['project', 'hub'],
    },
    {
      id: 'agent:delete',
      resource: 'agent',
      action: 'delete',
      description: 'Delete agents',
      allowedBoundaries: ['project'],
    },
    {
      id: 'broker:create',
      resource: 'broker',
      action: 'create',
      description: 'Create brokers',
      allowedBoundaries: ['hub'],
    },
  ],
  aliases: [
    {
      id: 'agent:manage',
      description: 'All agent management operations',
      expands_to: ['agent:delete', 'agent:read'],
    },
  ],
};

function postedBody(fetchFn: ReturnType<typeof vi.fn>): Record<string, unknown> {
  const call = fetchFn.mock.calls.find(
    ([input, init]) =>
      requestUrl(input as string) === '/api/v1/auth/tokens' &&
      (init as RequestInit | undefined)?.method === 'POST'
  );
  expect(call).toBeDefined();
  return JSON.parse((call![1] as RequestInit).body as string) as Record<string, unknown>;
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent?.replace(/\s+/g, ' ') ?? '';
}

describe('scion-token-list — hub boundary, purpose and labels (ptone/scion#2125)', () => {
  let tokenBoundaryKind: typeof TokenListModule.tokenBoundaryKind;
  let buildTokenLabels: typeof TokenListModule.buildTokenLabels;

  beforeAll(async () => {
    const mod = await import('./token-list.js');
    formatEligibilityReason = mod.formatEligibilityReason;
    tokenBoundaryKind = mod.tokenBoundaryKind;
    buildTokenLabels = mod.buildTokenLabels;
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('hub boundary hides the project select and fetches ?boundary=hub', async () => {
    const urls: string[] = [];
    const el = await createComponent(
      baseFetch({
        projects: () => jsonResponse({ projects: [{ id: 'p1', name: 'P1' }, { id: 'p2' }] }),
        scopes: (url) => {
          urls.push(url);
          return url.includes('boundary=hub')
            ? jsonResponse(HUB_ELIGIBILITY_RESPONSE)
            : jsonResponse(CATALOG_RESPONSE);
        },
      })
    );

    (el as any).openCreateDialog();
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('sl-select[label="Project"]')).not.toBeNull();

    (el as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('sl-select[label="Project"]')).toBeNull();
    expect(urls).toContain('/api/v1/auth/scopes?boundary=hub');
    expect(urls.some((u) => u.includes('boundary=hub') && u.includes('projectId'))).toBe(false);
    const broker = (el as any).availableScopes.find((s: any) => s.value === 'broker:create');
    expect(broker.eligible).toBe(false);
    expect(broker.eligibilityReason).toBe('authority_required');
  });

  it('switching boundary clears selected scopes and uses a separate cache entry', async () => {
    let hubFetches = 0;
    let projectFetches = 0;
    const el = await createComponent(
      baseFetch({
        scopes: (url) => {
          if (url.includes('boundary=hub')) {
            hubFetches++;
            return jsonResponse(HUB_ELIGIBILITY_RESPONSE);
          }
          if (url.includes('projectId=p1')) {
            projectFetches++;
            return jsonResponse(eligibilityResponse('p1'));
          }
          return jsonResponse(CATALOG_RESPONSE);
        },
      })
    );

    (el as any).createProjectId = 'p1';
    await (el as any).loadScopes({ kind: 'project', projectId: 'p1' });
    (el as any).createScopes = new Set(['agent:read']);

    (el as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    expect((el as any).createScopes.size).toBe(0);
    expect((el as any).scopesCache.has('hub')).toBe(true);
    expect((el as any).scopesCache.has('project:p1')).toBe(true);
    expect((el as any).availableScopes.some((s: any) => s.value === 'broker:create')).toBe(true);

    (el as any).createScopes = new Set(['agent:read']);
    (el as any).handleBoundaryChange('project');
    await new Promise((r) => setTimeout(r, 10));
    expect((el as any).createScopes.size).toBe(0);
    // Back to the project's own cached eligibility, not the hub list.
    const del = (el as any).availableScopes.find((s: any) => s.value === 'agent:delete');
    expect(del.eligible).toBe(false);

    (el as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    expect(hubFetches).toBe(1);
    expect(projectFetches).toBe(1);
  });

  it('hub create posts boundary {kind: hub} with no projectId', async () => {
    const el = await createComponent(
      baseFetch({ projects: () => jsonResponse({ projects: [{ id: 'p1', name: 'P1' }] }) })
    );
    (el as any).openCreateDialog();
    (el as any).handleBoundaryChange('hub');
    (el as any).createName = 'ci-hub';
    (el as any).createScopes = new Set(['agent:read']);

    await (el as any).handleCreate(new Event('submit'));

    const body = postedBody(globalThis.fetch as unknown as ReturnType<typeof vi.fn>);
    expect(body.boundary).toEqual({ kind: 'hub' });
    expect('projectId' in body).toBe(false);
    expect((el as any).createError).toBeNull();
    expect((el as any).revealDialogOpen).toBe(true);
  });

  it('project create posts an explicit project boundary', async () => {
    const el = await createComponent(
      baseFetch({ projects: () => jsonResponse({ projects: [{ id: 'p1', name: 'P1' }] }) })
    );
    (el as any).openCreateDialog();
    (el as any).createName = 'ci-project';
    (el as any).createScopes = new Set(['agent:read']);

    await (el as any).handleCreate(new Event('submit'));

    const body = postedBody(globalThis.fetch as unknown as ReturnType<typeof vi.fn>);
    expect(body.boundary).toEqual({ kind: 'project', projectId: 'p1' });
    expect('projectId' in body).toBe(false);
  });

  it('a project create with no project selected is refused before submit', async () => {
    const el = await createComponent(baseFetch());
    (el as any).openCreateDialog();
    (el as any).createName = 'x';
    (el as any).createScopes = new Set(['agent:read']);

    await (el as any).handleCreate(new Event('submit'));

    expect((el as any).createError).toBe('Project is required');
  });

  it('purpose and labels are sent when filled and omitted when empty', async () => {
    const el = await createComponent(baseFetch());
    (el as any).openCreateDialog();
    (el as any).handleBoundaryChange('hub');
    (el as any).createName = 'empty-meta';
    (el as any).createScopes = new Set(['agent:read']);
    (el as any).createPurpose = '   ';
    (el as any).createLabels = [{ key: '', value: '' }];

    await (el as any).handleCreate(new Event('submit'));
    let body = postedBody(globalThis.fetch as unknown as ReturnType<typeof vi.fn>);
    expect('purpose' in body).toBe(false);
    expect('labels' in body).toBe(false);

    document.body.innerHTML = '';
    const el2 = await createComponent(baseFetch());
    (el2 as any).openCreateDialog();
    (el2 as any).handleBoundaryChange('hub');
    (el2 as any).createName = 'with-meta';
    (el2 as any).createScopes = new Set(['agent:read']);
    (el2 as any).createPurpose = 'nightly deploy';
    (el2 as any).createLabels = [
      { key: 'team', value: 'infra' },
      { key: '', value: '' },
      { key: 'env', value: 'prod' },
    ];

    await (el2 as any).handleCreate(new Event('submit'));
    body = postedBody(globalThis.fetch as unknown as ReturnType<typeof vi.fn>);
    expect(body.purpose).toBe('nightly deploy');
    expect(body.labels).toEqual({ team: 'infra', env: 'prod' });
  });

  it('duplicate label keys are blocked before submit', async () => {
    const el = await createComponent(baseFetch());
    (el as any).openCreateDialog();
    (el as any).handleBoundaryChange('hub');
    (el as any).createName = 'dup';
    (el as any).createScopes = new Set(['agent:read']);
    (el as any).createLabels = [
      { key: 'team', value: 'a' },
      { key: 'team', value: 'b' },
    ];

    await (el as any).handleCreate(new Event('submit'));

    expect((el as any).createError).toBe('Duplicate label key "team"');
    const fetchFn = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    expect(
      fetchFn.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === 'POST')
    ).toBe(false);
  });

  it('caps the labels editor rows and builds labels from rows', () => {
    expect(buildTokenLabels([{ key: ' a ', value: '1' }])).toEqual({ labels: { a: '1' } });
    expect(buildTokenLabels([{ key: '', value: 'orphan' }]).error).toBe('Each label needs a key');
    expect(buildTokenLabels([]).labels).toBeUndefined();
  });

  it('keeps a __proto__ label key as a key and refuses it twice', () => {
    const { labels, error } = buildTokenLabels([{ key: '__proto__', value: 'x' }]);
    expect(error).toBeUndefined();
    expect(Object.prototype.hasOwnProperty.call(labels, '__proto__')).toBe(true);
    expect(JSON.stringify(labels)).toBe('{"__proto__":"x"}');
    expect(
      buildTokenLabels([
        { key: '__proto__', value: 'a' },
        { key: '__proto__', value: 'b' },
      ]).error
    ).toBe('Duplicate label key "__proto__"');
  });

  it('the labels editor stops adding rows at the limit', async () => {
    const el = await createComponent(baseFetch());
    (el as any).openCreateDialog();
    for (let i = 0; i < 12; i++) (el as any).addLabelRow();
    expect((el as any).createLabels.length).toBe(8);
  });

  it('refuses a purpose longer than 128 bytes before submit', async () => {
    const el = await createComponent(baseFetch());
    (el as any).openCreateDialog();
    (el as any).handleBoundaryChange('hub');
    (el as any).createName = 'long';
    (el as any).createScopes = new Set(['agent:read']);
    (el as any).createPurpose = 'é'.repeat(65); // 130 bytes, 65 characters

    await (el as any).handleCreate(new Event('submit'));

    expect((el as any).createError).toContain('128 bytes');
  });

  it('a hub token row shows a Hub badge, never a blank boundary cell', async () => {
    const el = await createComponent(
      baseFetch({
        projects: () => jsonResponse({ projects: [{ id: 'p1', name: 'Project One' }] }),
        tokens: () =>
          jsonResponse({
            items: [
              {
                id: 't1',
                name: 'hub-token',
                prefix: 'scion_pat_aaaa',
                boundary: { kind: 'hub' },
                scopes: ['agent:read'],
                revoked: false,
                created: '2026-11-01T00:00:00Z',
              },
              {
                id: 't2',
                name: 'project-token',
                prefix: 'scion_pat_bbbb',
                boundary: { kind: 'project', projectId: 'p1' },
                projectId: 'p1',
                scopes: ['agent:read'],
                revoked: false,
                created: '2026-11-01T00:00:00Z',
              },
              {
                id: 't3',
                name: 'odd-token',
                prefix: 'scion_pat_cccc',
                scopes: ['agent:read'],
                revoked: false,
                created: '2026-11-01T00:00:00Z',
              },
            ],
          }),
      })
    );

    const header = Array.from(el.shadowRoot!.querySelectorAll('th')).map((th) =>
      th.textContent?.trim()
    );
    expect(header).toContain('Boundary');
    expect(header).not.toContain('Project');

    const rows = Array.from(el.shadowRoot!.querySelectorAll('tbody tr'));
    expect(rows).toHaveLength(3);
    const boundaryCells = rows.map((row) => row.querySelectorAll('td')[2]);
    const hubBadge = boundaryCells[0].querySelector('.boundary-hub');
    expect(hubBadge?.textContent?.trim()).toBe('Hub');
    expect(boundaryCells[0].querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      'Any project you can currently access'
    );
    expect(boundaryCells[1].textContent?.trim()).toBe('Project One');
    expect(boundaryCells[2].textContent?.trim()).toBe('Unknown');
    for (const cell of boundaryCells) {
      expect(cell.textContent?.trim()).not.toBe('');
    }
  });

  it('purpose and labels render as text (markup shown literally)', async () => {
    const markup = '<img src=x onerror="alert(1)">';
    const el = await createComponent(
      baseFetch({
        tokens: () =>
          jsonResponse({
            items: [
              {
                id: 't1',
                name: 'meta-token',
                prefix: 'scion_pat_aaaa',
                boundary: { kind: 'hub' },
                purpose: markup,
                labels: { team: '<b>infra</b>', agent: 'agent:x' },
                scopes: ['agent:read'],
                revoked: false,
                created: '2026-11-01T00:00:00Z',
              },
            ],
          }),
      })
    );

    const row = el.shadowRoot!.querySelector('tbody tr')!;
    expect(row.querySelector('img')).toBeNull();
    expect(row.querySelector('b')).toBeNull();
    expect(row.querySelector('.token-purpose')?.textContent).toBe(markup);
    const chips = Array.from(row.querySelectorAll('.label-chip')).map((c) => c.textContent);
    expect(chips).toEqual(['agent=agent:x', 'team=<b>infra</b>']);
  });

  it('authority_required and boundary-aware boundary_not_allowed labels', () => {
    expect(formatEligibilityReason('authority_required', 'hub')).toBe(
      'your account has no current access that includes this permission'
    );
    expect(formatEligibilityReason('boundary_not_allowed', 'hub')).toBe(
      'not selectable for a hub token'
    );
    expect(formatEligibilityReason('boundary_not_allowed', 'project')).toBe(
      'not selectable for a project token'
    );
    expect(formatEligibilityReason('boundary_not_allowed')).toBe(
      'not selectable for a project token'
    );
    expect(formatEligibilityReason('project_access_required', 'project')).toBe(
      'you do not currently have authority for this permission in this project'
    );
  });

  it('unsupported_boundary falls back to allowedBoundaries filtering with a notice', async () => {
    let hubFetches = 0;
    const el = await createComponent(
      baseFetch({
        scopes: (url) => {
          if (!url.includes('boundary=hub')) return jsonResponse(CATALOG_WITH_BOUNDARIES);
          hubFetches++;
          return jsonResponse(
            {
              error: {
                code: 'unsupported_boundary',
                message: 'hub boundary eligibility is not available yet',
              },
            },
            400
          );
        },
      })
    );

    (el as any).openCreateDialog();
    (el as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const values = (el as any).availableScopes.map((s: any) => s.value);
    expect(values).toContain('agent:read');
    expect(values).toContain('broker:create');
    expect(values).not.toContain('agent:delete'); // project-only
    expect(values).not.toContain('agent:manage'); // alias with a project-only member
    expect((el as any).scopesErrorKey).toBeNull();
    expect((el as any).scopesUnsupportedKey).toBe('hub');
    expect(shadowText(el)).toContain(
      'Eligibility is not available on this hub; the server checks scopes when you create the token.'
    );
    // Not cached: a hub that gains hub eligibility is asked again.
    expect((el as any).scopesCache.has('hub')).toBe(false);
    expect(hubFetches).toBe(1);

    // Back on the project boundary the notice is gone.
    (el as any).handleBoundaryChange('project');
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;
    expect(shadowText(el)).not.toContain('Eligibility is not available on this hub');

    // Switching to Hub again sends a second ?boundary=hub request.
    (el as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    expect(hubFetches).toBe(2);
    expect((el as any).scopesUnsupportedKey).toBe('hub');
  });

  it('a failed hub eligibility request still leaves out scopes a hub token may not select', async () => {
    const el = await createComponent(
      baseFetch({
        scopes: (url) =>
          url.includes('boundary=hub')
            ? jsonResponse({ error: { code: 'forbidden', message: 'forbidden' } }, 403)
            : jsonResponse(CATALOG_WITH_BOUNDARIES),
      })
    );

    (el as any).openCreateDialog();
    (el as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const values = (el as any).availableScopes.map((s: any) => s.value);
    expect(values).toContain('agent:read');
    expect(values).toContain('broker:create');
    expect(values).not.toContain('agent:delete'); // project-only
    expect(values).not.toContain('agent:manage'); // alias with a project-only member
    expect((el as any).scopesErrorKey).toBe('hub');
    expect((el as any).scopesUnsupportedKey).toBeNull();
    expect(shadowText(el)).toContain(
      'Could not check which scopes you can select for this hub token'
    );

    // A network error on the hub request filters the same way.
    document.body.innerHTML = '';
    const el2 = await createComponent(
      baseFetch({
        scopes: (url) =>
          url.includes('boundary=hub')
            ? Promise.reject(new TypeError('network down'))
            : jsonResponse(CATALOG_WITH_BOUNDARIES),
      })
    );
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    (el2 as any).openCreateDialog();
    (el2 as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    const values2 = (el2 as any).availableScopes.map((s: any) => s.value);
    expect(values2).not.toContain('agent:delete');
    expect(values2).toContain('broker:create');
    expect((el2 as any).scopesErrorKey).toBe('hub');
  });

  it('a failed project eligibility request leaves out hub-only scopes for a project token', async () => {
    const el = await createComponent(
      baseFetch({
        projects: () => jsonResponse({ projects: [{ id: 'p1', name: 'P1' }, { id: 'p2' }] }),
        scopes: (url) =>
          url.includes('projectId=p1') || url.includes('boundary=hub')
            ? jsonResponse({ error: { code: 'forbidden', message: 'forbidden' } }, 403)
            : jsonResponse(CATALOG_WITH_BOUNDARIES),
      })
    );

    (el as any).openCreateDialog();
    (el as any).handleProjectChange('p1');
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const values = (el as any).availableScopes.map((s: any) => s.value);
    expect(values).not.toContain('broker:create'); // hub-only
    expect(values).toContain('agent:delete'); // project-only
    expect(values).toContain('agent:read');
    expect(values).toContain('agent:manage'); // every member allows project
    expect((el as any).scopesErrorKey).toBe('project:p1');
    // The cached plain catalog stays unfiltered.
    const cached = (el as any).scopesCache.get('').map((s: any) => s.value);
    expect(cached).toContain('broker:create');

    // A failed hub request then filters that same catalog for hub: the
    // hub-only scope is offered again and the project-only one is not.
    (el as any).handleBoundaryChange('hub');
    await new Promise((r) => setTimeout(r, 10));
    const hubValues = (el as any).availableScopes.map((s: any) => s.value);
    expect(hubValues).toContain('broker:create');
    expect(hubValues).not.toContain('agent:delete');
    expect((el as any).scopesErrorKey).toBe('hub');

    // A network error on the project request filters the same way.
    document.body.innerHTML = '';
    const el2 = await createComponent(
      baseFetch({
        scopes: (url) =>
          url.includes('projectId=p1')
            ? Promise.reject(new TypeError('network down'))
            : jsonResponse(CATALOG_WITH_BOUNDARIES),
      })
    );
    vi.spyOn(console, 'warn').mockImplementation(() => {});
    (el2 as any).openCreateDialog();
    (el2 as any).handleProjectChange('p1');
    await new Promise((r) => setTimeout(r, 10));
    const values2 = (el2 as any).availableScopes.map((s: any) => s.value);
    expect(values2).not.toContain('broker:create');
    expect(values2).toContain('agent:delete');
    expect((el2 as any).scopesErrorKey).toBe('project:p1');
  });

  it('an older hub response without boundary is treated as project when projectId is set', () => {
    expect(tokenBoundaryKind({ projectId: 'p1' })).toBe('project');
    expect(tokenBoundaryKind({ boundary: { kind: 'hub' } })).toBe('hub');
    expect(
      tokenBoundaryKind({ boundary: { kind: 'project', projectId: 'p1' }, projectId: 'p1' })
    ).toBe('project');
    expect(tokenBoundaryKind({})).toBe('');
  });

  it('the token value is dropped once the reveal dialog has closed', async () => {
    const el = await createComponent(baseFetch());
    (el as any).openCreateDialog();
    (el as any).handleBoundaryChange('hub');
    (el as any).createName = 'reveal';
    (el as any).createScopes = new Set(['agent:read']);
    await (el as any).handleCreate(new Event('submit'));
    await el.updateComplete;
    expect((el as any).revealToken).toBe('scion_pat_x');

    const dialog = el.shadowRoot!.querySelector('sl-dialog[label="Token Created"]')!;
    dialog.dispatchEvent(new CustomEvent('sl-after-hide', { bubbles: true, composed: true }));
    expect((el as any).revealToken).toBe('');
  });
});
