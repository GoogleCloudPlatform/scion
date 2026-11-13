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
  CHAT_CHOICES_FLAG,
  PROJECT_DECISIONS_FLAG,
  projectDecisionsEnabled,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../utils/feature-flags.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
vi.mock('./api.js', async (orig) => ({
  ...(await orig<typeof import('./api.js')>()),
  apiFetch,
}));

const {
  buildProjectDecisionsQuery,
  fetchProjectDecisionCounts,
  fetchProjectMadeDecisions,
  fetchProjectPendingDecisions,
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

describe('projectDecisionsEnabled (dual flag gate)', () => {
  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('requires both web.project_decisions and web.chat_choices to be enabled', () => {
    expect(projectDecisionsEnabled()).toBe(false);

    setFeatureFlag(PROJECT_DECISIONS_FLAG, true);
    setFeatureFlag(CHAT_CHOICES_FLAG, false);
    expect(projectDecisionsEnabled()).toBe(false);

    setFeatureFlag(PROJECT_DECISIONS_FLAG, false);
    setFeatureFlag(CHAT_CHOICES_FLAG, true);
    expect(projectDecisionsEnabled()).toBe(false);

    setFeatureFlag(PROJECT_DECISIONS_FLAG, true);
    setFeatureFlag(CHAT_CHOICES_FLAG, true);
    expect(projectDecisionsEnabled()).toBe(true);
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

  it('throws extracted API error on non-2xx', async () => {
    apiFetch.mockResolvedValueOnce(
      res(403, { error: { code: 'forbidden', message: 'agents cannot read project decisions' } })
    );
    await expect(fetchProjectPendingDecisions('p1')).rejects.toThrow(
      'agents cannot read project decisions'
    );
  });
});
