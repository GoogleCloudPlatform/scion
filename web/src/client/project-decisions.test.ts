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
import {
  DECISIONS_FLAG,
  decisionsEnabled,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../utils/feature-flags.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
vi.mock('./api.js', async (orig) => ({
  ...(await orig<typeof import('./api.js')>()),
  apiFetch,
}));

const {
  DecisionsApiError,
  buildDecisionsUrlSearch,
  buildProjectDecisionsQuery,
  fetchProjectDecisionCounts,
  fetchProjectMadeDecisions,
  fetchProjectPendingDecisions,
  hasDecisionsSearchCapability,
  isDecisionsSearchTimeoutError,
  parseDecisionsUrlState,
} = await import('./project-decisions.js');

function res(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 404 ? 'Not Found' : 'OK',
    json: () => Promise.resolve(body),
    text: () => Promise.resolve(JSON.stringify(body)),
  } as unknown as Response;
}

describe('decisionsEnabled (single flag gate)', () => {
  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('follows web.decisions alone', () => {
    expect(decisionsEnabled()).toBe(false);

    setFeatureFlag(DECISIONS_FLAG, true);
    expect(decisionsEnabled()).toBe(true);

    setFeatureFlag(DECISIONS_FLAG, false);
    expect(decisionsEnabled()).toBe(false);
  });
});

describe('project-decisions API client', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  it('builds clean query strings for pending and made tabs', () => {
    expect(buildProjectDecisionsQuery()).toBe('?tab=pending');
    expect(buildProjectDecisionsQuery({ tab: 'pending', limit: 0 })).toBe('?tab=pending&limit=0');
    expect(buildProjectDecisionsQuery({ tab: 'pending', cursor: 'cur-1', limit: 25 })).toBe(
      '?tab=pending&limit=25&cursor=cur-1'
    );
    expect(
      buildProjectDecisionsQuery({
        tab: 'made',
        cursor: 'cur-2',
        filter: {
          agent: 'a-1',
          user: 'u-1',
          provenance: 'confirmed',
          from: '2026-11-01',
          to: '2026-11-13',
        },
      })
    ).toBe(
      '?tab=made&cursor=cur-2&agent=a-1&user=u-1&provenance=confirmed&from=2026-11-01&to=2026-11-13'
    );
    expect(
      buildProjectDecisionsQuery({
        tab: 'made',
        filter: { agent: 'all', user: '', provenance: '', from: ' ', to: '' },
      })
    ).toBe('?tab=made');
  });

  it('fetches pending decisions and normalizes optional arrays', async () => {
    const ac = new AbortController();
    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 2, waitingOnOthers: 1 },
        prompts: [{ promptMessageId: 'm1' }],
        agents: [{ id: 'a1', name: 'bot' }],
        answerers: [{ id: 'u1', name: 'Alice' }],
        nextCursor: 'next-p',
      })
    );
    const out = await fetchProjectPendingDecisions('proj 1', {
      cursor: 'c0',
      limit: 10,
      signal: ac.signal,
    });
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/projects/proj%201/decisions?tab=pending&limit=10&cursor=c0',
      { signal: ac.signal }
    );
    expect(out.counts).toEqual({ waitingOnYou: 2, waitingOnOthers: 1 });
    expect(out.prompts).toHaveLength(1);
    expect(out.nextCursor).toBe('next-p');
  });

  it('fetches made decisions with filters and cursor', async () => {
    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        entries: [{ answerId: 'ans-1' }],
        agents: [],
        answerers: [],
      })
    );
    const out = await fetchProjectMadeDecisions(
      'p1',
      { agent: 'a1', provenance: 'overridden' },
      { cursor: 'c1' }
    );
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/projects/p1/decisions?tab=made&cursor=c1&agent=a1&provenance=overridden',
      undefined
    );
    expect(out.entries).toHaveLength(1);
    expect(out.nextCursor).toBeUndefined();
  });

  it('fetches counts with tab=pending&limit=0', async () => {
    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 4, waitingOnOthers: 2 },
        prompts: [],
        agents: [],
        answerers: [],
      })
    );
    const counts = await fetchProjectDecisionCounts('p1');
    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/projects/p1/decisions?tab=pending&limit=0',
      undefined
    );
    expect(counts).toEqual({ waitingOnYou: 4, waitingOnOthers: 2 });
  });

  it('throws extracted API error on non-2xx and identifies SEARCH_TIMEOUT', async () => {
    apiFetch.mockResolvedValueOnce(
      res(403, { error: { code: 'forbidden', message: 'agents cannot read project decisions' } })
    );
    await expect(fetchProjectPendingDecisions('p1')).rejects.toThrow(
      'agents cannot read project decisions'
    );

    apiFetch.mockResolvedValueOnce(
      res(503, {
        error: {
          code: 'SEARCH_TIMEOUT',
          message: 'Search took too long. Try a longer phrase or search one space.',
        },
      })
    );
    let caught: unknown;
    try {
      await fetchProjectMadeDecisions('p1', { q: 'backfill' });
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(DecisionsApiError);
    expect((caught as InstanceType<typeof DecisionsApiError>).status).toBe(503);
    expect((caught as InstanceType<typeof DecisionsApiError>).code).toBe('SEARCH_TIMEOUT');
    expect(isDecisionsSearchTimeoutError(caught)).toBe(true);
    expect(isDecisionsSearchTimeoutError(new Error('network'))).toBe(false);
  });

  it('includes shared filters (q, conversation, agent) on both tabs and omits out-of-bounds q', () => {
    expect(
      buildProjectDecisionsQuery({
        tab: 'pending',
        filter: {
          q: '  backfill  ',
          conversation: 'topic-release',
          agent: '00000000-0000-4000-8000-000000000001',
        },
      })
    ).toBe(
      '?tab=pending&q=backfill&conversation=topic-release&agent=00000000-0000-4000-8000-000000000001'
    );

    // 1-rune q and >256-rune q are omitted so client never triggers HTTP 400
    expect(buildProjectDecisionsQuery({ tab: 'pending', filter: { q: 'x' } })).toBe('?tab=pending');
    expect(buildProjectDecisionsQuery({ tab: 'made', filter: { q: 'a'.repeat(257) } })).toBe(
      '?tab=made'
    );
    // 2 multi-byte emoji runes count as 2 Unicode code points (valid)
    expect(buildProjectDecisionsQuery({ tab: 'pending', filter: { q: '🚀🎉' } })).toBe(
      '?tab=pending&q=%F0%9F%9A%80%F0%9F%8E%89'
    );
    // Made-only filters are not sent on tab=pending
    expect(
      buildProjectDecisionsQuery({
        tab: 'pending',
        filter: {
          q: 'ship',
          user: 'u-1',
          provenance: 'direct',
          from: '2026-11-01',
          to: '2026-11-12',
        },
      })
    ).toBe('?tab=pending&q=ship');
  });

  it('preserves conversations facet presence for capability check and normalizes filteredCounts', async () => {
    // Older pre-P1 hub omits `conversations`
    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        prompts: [],
        agents: [],
        answerers: [],
      })
    );
    const legacy = await fetchProjectPendingDecisions('p1');
    expect(legacy.conversations).toBeUndefined();
    expect(hasDecisionsSearchCapability(legacy)).toBe(false);

    // P1+ hub sends `conversations` (even empty) and `filteredCounts` when filtered
    apiFetch.mockResolvedValueOnce(
      res(200, {
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        filteredCounts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [],
        agents: [{ id: '00000000-0000-4000-8000-000000000001', name: 'release-bot', hint: '3f9c' }],
        answerers: [],
        conversations: [{ key: 't-91', label: '#release', projectId: 'p1' }],
      })
    );
    const modern = await fetchProjectPendingDecisions('p1', {
      filter: { q: 'ship', conversation: 't-91' },
    });
    expect(apiFetch).toHaveBeenLastCalledWith(
      '/api/v1/projects/p1/decisions?tab=pending&q=ship&conversation=t-91',
      undefined
    );
    expect(hasDecisionsSearchCapability(modern)).toBe(true);
    expect(modern.filteredCounts).toEqual({ waitingOnYou: 1, waitingOnOthers: 0 });
    expect(modern.conversations).toEqual([{ key: 't-91', label: '#release', projectId: 'p1' }]);
    expect(modern.agents[0]?.hint).toBe('3f9c');
  });
});

describe('Decisions URL state round-trip & validation (D10, D11)', () => {
  const AGENT_UUID = '00000000-0000-4000-8000-000000000042';

  it('round-trips valid pending and made URL parameters cleanly', () => {
    const pendingSearch = buildDecisionsUrlSearch({
      tab: 'pending',
      q: 'backfill',
      conversation: 't-91',
      agent: AGENT_UUID,
    });
    expect(pendingSearch).toBe(`q=backfill&conversation=t-91&agent=${AGENT_UUID}`);
    expect(parseDecisionsUrlState(pendingSearch)).toEqual({
      tab: 'pending',
      q: 'backfill',
      conversation: 't-91',
      agent: AGENT_UUID,
      user: '',
      provenance: '',
      from: '',
      to: '',
    });

    const madeSearch = buildDecisionsUrlSearch({
      tab: 'made',
      q: 'backfill',
      conversation: 't-91',
      agent: AGENT_UUID,
      user: 'u-alice',
      provenance: 'confirmed',
      from: '2026-11-01',
      to: '2026-11-14',
    });
    expect(madeSearch).toBe(
      `tab=made&q=backfill&conversation=t-91&agent=${AGENT_UUID}&user=u-alice&provenance=confirmed&from=2026-11-01&to=2026-11-14`
    );
    expect(parseDecisionsUrlState(`?${madeSearch}`)).toEqual({
      tab: 'made',
      q: 'backfill',
      conversation: 't-91',
      agent: AGENT_UUID,
      user: 'u-alice',
      provenance: 'confirmed',
      from: '2026-11-01',
      to: '2026-11-14',
    });

    const dismissedSearch = buildDecisionsUrlSearch({
      tab: 'made',
      provenance: 'dismissed',
    });
    expect(dismissedSearch).toBe('tab=made&provenance=dismissed');
    expect(parseDecisionsUrlState(`?${dismissedSearch}`).provenance).toBe('dismissed');
    expect(buildProjectDecisionsQuery({ tab: 'made', filter: { provenance: 'dismissed' } })).toBe(
      '?tab=made&provenance=dismissed'
    );
  });

  it('silently drops invalid URL values on load', () => {
    // Unknown tab falls back to pending; Made-only params dropped on pending;
    // 1-char q, >256-byte conversation, and non-UUID agent dropped
    expect(
      parseDecisionsUrlState(
        `?tab=bogus&q=a&conversation=${'x'.repeat(257)}&agent=not-a-uuid&user=u-1&provenance=direct&from=2026-11-01`
      )
    ).toEqual({
      tab: 'pending',
      q: '',
      conversation: '',
      agent: '',
      user: '',
      provenance: '',
      from: '',
      to: '',
    });

    // Invalid provenance, impossible calendar dates, and >256-char q dropped on tab=made
    expect(
      parseDecisionsUrlState(
        `?tab=made&q=${'z'.repeat(257)}&provenance=bogus&from=2026-02-29&to=not-a-date&user=all`
      )
    ).toEqual({
      tab: 'made',
      q: '',
      conversation: '',
      agent: '',
      user: '',
      provenance: '',
      from: '',
      to: '',
    });

    // Inverted date range (from > to) is dropped on load; agent_interpreted normalizes to interpreted
    expect(
      parseDecisionsUrlState('?tab=made&provenance=agent_interpreted&from=2026-11-15&to=2026-11-01')
    ).toEqual({
      tab: 'made',
      q: '',
      conversation: '',
      agent: '',
      user: '',
      provenance: 'interpreted',
      from: '',
      to: '',
    });

    // U+0085 (NEL, trimmed by Go strings.TrimSpace), U+FEFF (BOM, trimmed by JS), and embedded control chars
    // never slip a 1-rune or control-char query past client normalization to trigger a Hub 400
    expect(parseDecisionsUrlState('?q=a%C2%85').q).toBe('');
    expect(parseDecisionsUrlState('?q=a%EF%BB%BF').q).toBe('');
    expect(parseDecisionsUrlState('?q=ab%00cd').q).toBe('');
    expect(parseDecisionsUrlState('?q=a%C2%85b').q).toBe('');
    expect(parseDecisionsUrlState('?q=%EF%BB%BFship%C2%85').q).toBe('ship');
  });
});
