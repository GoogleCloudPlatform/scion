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

import { describe, it, expect, vi, beforeEach } from 'vitest';

const apiFetch = vi.hoisted(() =>
  vi.fn<(path: string, init?: RequestInit & { authErrors?: string }) => Promise<Response>>()
);
vi.mock('./api.js', async (orig) => ({
  ...(await orig<typeof import('./api.js')>()),
  apiFetch,
}));

const {
  PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
  PROFILE_DECISION_PROBE_TTL_MS,
  buildProfileDecisionsQuery,
  buildProfileDecisionsUrlSearch,
  defaultProfileDecisionsSort,
  dispatchProfileDecisionsCountChanged,
  dispatchProfileDecisionsNotFound,
  fetchProfileDecisionProbe,
  fetchProfileMadeDecisions,
  fetchProfilePendingDecisions,
  getCachedProfileDecisionProbe,
  getLastKnownProfileDecisionAvailability,
  isChoicePromptMessageEvent,
  parseProfileDecisionsUrlState,
  resetProfileDecisionProbeCacheForTests,
  scheduleSharedProfileDecisionProbeRefresh,
} = await import('./profile-decisions.js');
const { stateManager } = await import('./state.js');

function res(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 404 ? 'Not Found' : 'OK',
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  } as unknown as Response;
}

describe('profile-decisions URL state (D10, R6)', () => {
  it('applies per-tab default sort: project on pending and recent on made', () => {
    expect(defaultProfileDecisionsSort('pending')).toBe('project');
    expect(defaultProfileDecisionsSort('made')).toBe('recent');

    expect(parseProfileDecisionsUrlState('')).toEqual({
      tab: 'pending',
      sort: 'project',
      project: '',
      q: '',
      conversation: '',
      agent: '',
      user: '',
      provenance: '',
      from: '',
      to: '',
    });

    expect(parseProfileDecisionsUrlState('?tab=made')).toEqual({
      tab: 'made',
      sort: 'recent',
      project: '',
      q: '',
      conversation: '',
      agent: '',
      user: '',
      provenance: '',
      from: '',
      to: '',
    });
  });

  it('round-trips tab, sort, project, shared filters, and made filters in the URL', () => {
    const pendingSearch = buildProfileDecisionsUrlSearch({
      tab: 'pending',
      sort: 'recent',
      project: 'proj-1',
      q: 'backfill',
      conversation: 'topic:release',
      agent: '11111111-2222-3333-4444-555555555555',
      user: 'ignored-on-pending',
    });
    expect(pendingSearch).toBe(
      'tab=pending&sort=recent&project=proj-1&q=backfill&conversation=topic%3Arelease&agent=11111111-2222-3333-4444-555555555555'
    );
    expect(parseProfileDecisionsUrlState(pendingSearch)).toEqual({
      tab: 'pending',
      sort: 'recent',
      project: 'proj-1',
      q: 'backfill',
      conversation: 'topic:release',
      agent: '11111111-2222-3333-4444-555555555555',
      user: '',
      provenance: '',
      from: '',
      to: '',
    });

    const madeSearch = buildProfileDecisionsUrlSearch({
      tab: 'made',
      sort: 'project',
      project: 'proj-2',
      q: 'font',
      conversation: 'topic:landing',
      agent: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
      user: 'u-1',
      provenance: 'confirmed',
      from: '2026-11-01',
      to: '2026-11-14',
    });
    expect(parseProfileDecisionsUrlState(madeSearch)).toEqual({
      tab: 'made',
      sort: 'project',
      project: 'proj-2',
      q: 'font',
      conversation: 'topic:landing',
      agent: 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee',
      user: 'u-1',
      provenance: 'confirmed',
      from: '2026-11-01',
      to: '2026-11-14',
    });
  });

  it('always writes tab and sort in the URL including defaults (R6, R-1)', () => {
    expect(buildProfileDecisionsUrlSearch({ tab: 'pending' })).toBe('tab=pending&sort=project');
    expect(buildProfileDecisionsUrlSearch({ tab: 'made' })).toBe('tab=made&sort=recent');
  });

  it('silently drops invalid sort, overlong/control-char project, 1-rune q, non-UUID agent, and inverted dates', () => {
    const overlongProject = 'p'.repeat(129);
    const parsed = parseProfileDecisionsUrlState(
      `?tab=made&sort=bogus&project=${overlongProject}&q=x&agent=not-uuid&from=2026-11-15&to=2026-11-01`
    );
    expect(parsed).toEqual({
      tab: 'made',
      sort: 'recent',
      project: '',
      q: '',
      conversation: '',
      agent: '',
      user: '',
      provenance: '',
      from: '',
      to: '',
    });

    const ctrlProject = parseProfileDecisionsUrlState('?tab=pending&sort=alpha&project=bad%00id');
    expect(ctrlProject.sort).toBe('project');
    expect(ctrlProject.project).toBe('');
  });
});

describe('profile-decisions API client (R1, R3, R4, R5, §7)', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    resetProfileDecisionProbeCacheForTests();
  });

  it('builds profile query strings and omits cursor on made+project overview', () => {
    expect(buildProfileDecisionsQuery()).toBe('?tab=pending&sort=project');
    expect(
      buildProfileDecisionsQuery({
        tab: 'pending',
        sort: 'recent',
        limit: 25,
        cursor: 'cur-p',
        filter: { project: 'proj-1', q: 'deploy' },
      })
    ).toBe('?tab=pending&sort=recent&limit=25&cursor=cur-p&q=deploy&project=proj-1');

    expect(
      buildProfileDecisionsQuery({
        tab: 'made',
        sort: 'project',
        cursor: 'should-be-ignored',
        filter: { project: 'proj-1', provenance: 'direct' },
      })
    ).toBe('?tab=made&sort=project&project=proj-1&provenance=direct');
  });

  it('fetches pending profile decisions including projects, truncatedProjects, and filteredCounts', async () => {
    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 4, waitingOnOthers: 2 },
        filteredCounts: { waitingOnYou: 3, waitingOnOthers: 1 },
        prompts: [{ promptMessageId: 'm-1' }],
        agents: [{ id: 'a-1', name: 'release-bot' }],
        answerers: [],
        conversations: [{ key: 'topic:rel', label: '#release', projectId: 'proj-1' }],
        projects: [{ id: 'proj-1', name: 'scion', slug: 'scion' }],
        truncatedProjects: true,
        nextCursor: 'next-1',
      })
    );

    const out = await fetchProfilePendingDecisions({
      sort: 'project',
      filter: { project: 'proj-1' },
    });
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/users/me/decisions?tab=pending&sort=project&project=proj-1',
      undefined
    );
    expect(out.counts).toEqual({ waitingOnYou: 4, waitingOnOthers: 2 });
    expect(out.filteredCounts).toEqual({ waitingOnYou: 3, waitingOnOthers: 1 });
    expect(out.projects).toEqual([{ id: 'proj-1', name: 'scion', slug: 'scion' }]);
    expect(out.truncatedProjects).toBe(true);
    expect(out.nextCursor).toBe('next-1');
  });

  it('fetches made decisions in both sort=project (groups) and sort=recent (entries + nextCursor)', async () => {
    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        groups: [
          {
            project: { id: 'proj-1', name: 'scion', slug: 'scion' },
            entries: [{ answerId: 'ans-1' }],
            more: true,
          },
        ],
        agents: [],
        answerers: [],
        conversations: [],
        projects: [{ id: 'proj-1', name: 'scion', slug: 'scion' }],
        truncatedProjects: false,
      })
    );

    const grouped = await fetchProfileMadeDecisions({}, { sort: 'project' });
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/users/me/decisions?tab=made&sort=project',
      undefined
    );
    expect(grouped.sort).toBe('project');
    if (grouped.sort === 'project') {
      expect(grouped.groups).toHaveLength(1);
      expect(grouped.groups[0]?.more).toBe(true);
    }

    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [{ answerId: 'ans-1' }],
        agents: [],
        answerers: [],
        conversations: [],
        projects: [{ id: 'proj-1', name: 'scion', slug: 'scion' }],
        truncatedProjects: false,
        nextCursor: 'made-cur-2',
      })
    );

    const recent = await fetchProfileMadeDecisions(
      { project: 'proj-1' },
      { sort: 'recent', cursor: 'made-cur-1' }
    );
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/users/me/decisions?tab=made&sort=recent&cursor=made-cur-1&project=proj-1',
      undefined
    );
    expect(recent.sort).toBe('recent');
    if (recent.sort === 'recent') {
      expect(recent.entries).toHaveLength(1);
      expect(recent.nextCursor).toBe('made-cur-2');
    }
  });

  it('probes GET /api/v1/users/me/decisions?limit=0 with authErrors=caller, caches 200/404, deduplicates concurrent calls, and retries transient errors', async () => {
    let resolveFirst!: (r: Response) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveFirst = resolve;
        })
    );
    const p1 = fetchProfileDecisionProbe();
    const p2 = fetchProfileDecisionProbe();
    expect(apiFetch).toHaveBeenCalledTimes(1);
    resolveFirst(res(200, { counts: { waitingOnYou: 4, waitingOnOthers: 5 } }));

    await expect(Promise.all([p1, p2])).resolves.toEqual([
      { available: true, waitingOnYou: 4 },
      { available: true, waitingOnYou: 4 },
    ]);
    expect(apiFetch).toHaveBeenLastCalledWith('/api/v1/users/me/decisions?limit=0', {
      authErrors: 'caller',
    });

    // Cached reuse without issuing another HTTP request
    await expect(fetchProfileDecisionProbe()).resolves.toEqual({
      available: true,
      waitingOnYou: 4,
    });
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(getCachedProfileDecisionProbe()).toEqual({ available: true, waitingOnYou: 4 });

    // Force refresh updates cached value
    apiFetch.mockResolvedValueOnce(res(200, { counts: { waitingOnYou: 2, waitingOnOthers: 5 } }));
    await expect(fetchProfileDecisionProbe({ forceRefresh: true })).resolves.toEqual({
      available: true,
      waitingOnYou: 2,
    });
    expect(apiFetch).toHaveBeenCalledTimes(2);

    // 404 is cached as unavailable
    resetProfileDecisionProbeCacheForTests();
    apiFetch.mockResolvedValueOnce(res(404, { error: { code: 'not_found' } }));
    await expect(fetchProfileDecisionProbe()).resolves.toEqual({
      available: false,
      waitingOnYou: 0,
    });
    await expect(fetchProfileDecisionProbe()).resolves.toEqual({
      available: false,
      waitingOnYou: 0,
    });
    expect(apiFetch).toHaveBeenCalledTimes(3);

    // Transient 503 returns null and is NOT cached
    resetProfileDecisionProbeCacheForTests();
    apiFetch.mockResolvedValueOnce(res(503, { error: { code: 'unavailable' } }));
    await expect(fetchProfileDecisionProbe()).resolves.toBeNull();
    expect(getCachedProfileDecisionProbe()).toBeNull();
  });

  it('dispatches count-changed and not-found events for profile nav badge synchronization', () => {
    const received: Array<{ available: boolean; waitingOnYou: number }> = [];
    const listener = (ev: Event): void => {
      received.push((ev as CustomEvent<{ available: boolean; waitingOnYou: number }>).detail);
    };
    window.addEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, listener);
    try {
      dispatchProfileDecisionsCountChanged(3);
      dispatchProfileDecisionsCountChanged(-2);
      dispatchProfileDecisionsNotFound();
      expect(received).toEqual([
        { available: true, waitingOnYou: 3 },
        { available: true, waitingOnYou: 0 },
        { available: false, waitingOnYou: 0 },
      ]);
    } finally {
      window.removeEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, listener);
    }
  });

  it('prevents a slow forced ?limit=0 probe from overwriting a fresher page-derived count', async () => {
    let resolveSlowProbe!: (r: Response) => void;
    apiFetch
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveSlowProbe = resolve;
          })
      )
      .mockResolvedValueOnce(
        res(200, {
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          prompts: [],
          agents: [],
          answerers: [],
          conversations: [],
          projects: [],
          truncatedProjects: false,
        })
      );

    const received: Array<{ available: boolean; waitingOnYou: number }> = [];
    const listener = (ev: Event): void => {
      received.push((ev as CustomEvent<{ available: boolean; waitingOnYou: number }>).detail);
    };
    window.addEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, listener);
    try {
      scheduleSharedProfileDecisionProbeRefresh();
      await new Promise((r) => setTimeout(r, 120));
      expect(apiFetch).toHaveBeenCalledTimes(1);

      // User navigates to /profile/decisions while the forced probe is still in flight;
      // the page fetch resolves fresher count = 1 and broadcasts it.
      const pageRes = await fetchProfilePendingDecisions({ sort: 'project' });
      dispatchProfileDecisionsCountChanged(pageRes.counts.waitingOnYou);
      expect(getCachedProfileDecisionProbe()).toEqual({ available: true, waitingOnYou: 1 });

      // Now the older ?limit=0 probe settles with stale count = 5 -> must NOT overwrite or broadcast!
      resolveSlowProbe(res(200, { counts: { waitingOnYou: 5, waitingOnOthers: 0 } }));
      await new Promise((r) => setTimeout(r, 20));

      expect(getCachedProfileDecisionProbe()).toEqual({ available: true, waitingOnYou: 1 });
      expect(received).toEqual([{ available: true, waitingOnYou: 1 }]);
    } finally {
      window.removeEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, listener);
    }
  });

  it('matches real Hub decision-prompt chat events and ignores plain messages, legacy choice-prompt, or dead message.choices shapes', () => {
    expect(
      isChoicePromptMessageEvent(
        new CustomEvent('chat-message-received', {
          detail: { data: { id: 'm1', type: 'decision-prompt' } },
        })
      )
    ).toBe(true);
    expect(
      isChoicePromptMessageEvent(
        new CustomEvent('chat-message-received', {
          detail: { data: { id: 'm2', decisions: [{ rowId: 'r1' }] } },
        })
      )
    ).toBe(true);
    expect(
      isChoicePromptMessageEvent(
        new CustomEvent('chat-message-received', {
          detail: { data: { id: 'm-legacy', type: 'choice-prompt' } },
        })
      )
    ).toBe(false);
    expect(
      isChoicePromptMessageEvent(
        new CustomEvent('chat-message-received', {
          detail: { data: { id: 'm3', type: 'message', content: 'hello' } },
        })
      )
    ).toBe(false);
    expect(
      isChoicePromptMessageEvent(
        new CustomEvent('chat-message-received', {
          detail: { data: { message: { choices: [{ rowId: 'r1' }] } } },
        })
      )
    ).toBe(false);
  });

  it('invalidates the probe cache on module-level SSE events (even with no nav mounted) and after PROFILE_DECISION_PROBE_TTL_MS', async () => {
    let now = 1_000_000;
    const nowSpy = vi.spyOn(Date, 'now').mockImplementation(() => now);
    try {
      expect(getLastKnownProfileDecisionAvailability()).toBeNull();
      apiFetch.mockResolvedValueOnce(res(200, { counts: { waitingOnYou: 3, waitingOnOthers: 0 } }));
      await expect(fetchProfileDecisionProbe()).resolves.toEqual({
        available: true,
        waitingOnYou: 3,
      });
      expect(getCachedProfileDecisionProbe()).toEqual({ available: true, waitingOnYou: 3 });
      expect(getLastKnownProfileDecisionAvailability()).toBe(true);

      // Unmounted decision-updated marks cache dirty without issuing a background fetch
      stateManager.dispatchEvent(new CustomEvent('decision-updated', { detail: {} }));
      expect(apiFetch).toHaveBeenCalledTimes(1);
      expect(getCachedProfileDecisionProbe()).toBeNull();
      expect(getLastKnownProfileDecisionAvailability()).toBe(true);

      // Next fetchProfileDecisionProbe re-probes and clears dirty state
      apiFetch.mockResolvedValueOnce(res(200, { counts: { waitingOnYou: 1, waitingOnOthers: 0 } }));
      await expect(fetchProfileDecisionProbe()).resolves.toEqual({
        available: true,
        waitingOnYou: 1,
      });
      expect(apiFetch).toHaveBeenCalledTimes(2);
      expect(getCachedProfileDecisionProbe()).toEqual({ available: true, waitingOnYou: 1 });

      // Decision-prompt chat message also marks cache dirty; plain chat message does not
      stateManager.dispatchEvent(
        new CustomEvent('chat-message-received', {
          detail: { data: { id: 'm-plain', type: 'message' } },
        })
      );
      expect(getCachedProfileDecisionProbe()).toEqual({ available: true, waitingOnYou: 1 });

      stateManager.dispatchEvent(
        new CustomEvent('chat-message-received', {
          detail: {
            data: { id: 'm-choice', type: 'decision-prompt', decisions: [{ rowId: 'r1' }] },
          },
        })
      );
      expect(getCachedProfileDecisionProbe()).toBeNull();

      apiFetch.mockResolvedValueOnce(res(200, { counts: { waitingOnYou: 2, waitingOnOthers: 0 } }));
      await expect(fetchProfileDecisionProbe()).resolves.toEqual({
        available: true,
        waitingOnYou: 2,
      });
      expect(apiFetch).toHaveBeenCalledTimes(3);

      // Within TTL: fresh, no extra probe
      now += PROFILE_DECISION_PROBE_TTL_MS - 1;
      expect(getCachedProfileDecisionProbe()).toEqual({ available: true, waitingOnYou: 2 });
      await expect(fetchProfileDecisionProbe()).resolves.toEqual({
        available: true,
        waitingOnYou: 2,
      });
      expect(apiFetch).toHaveBeenCalledTimes(3);

      // At/past TTL: stale, next call re-probes
      now += 2;
      expect(getCachedProfileDecisionProbe()).toBeNull();
      expect(getLastKnownProfileDecisionAvailability()).toBe(true);
      apiFetch.mockResolvedValueOnce(res(200, { counts: { waitingOnYou: 0, waitingOnOthers: 0 } }));
      await expect(fetchProfileDecisionProbe()).resolves.toEqual({
        available: true,
        waitingOnYou: 0,
      });
      expect(apiFetch).toHaveBeenCalledTimes(4);
    } finally {
      nowSpy.mockRestore();
    }
  });
});
