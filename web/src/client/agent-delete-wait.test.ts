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
 * deleteAgentAndWait / waitForAgentDeletion: only a delete the hub confirms
 * (204, or a 202 followed by a 404 or a set deletedAt) counts as deleted.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DeletionInfo } from '../shared/types.js';

vi.mock('../components/shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

const holder = vi.hoisted(() => ({ sm: null as unknown }));
vi.mock('./state.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./state.js')>();
  return {
    ...actual,
    get stateManager(): unknown {
      return holder.sm ?? actual.stateManager;
    },
  };
});

import { deleteAgentAndWait, waitForAgentDeletion } from './agent-delete-wait.js';
import { showConfirm } from '../components/shared/confirm-dialog.js';
import { requestUrl } from './__fixtures__/request-url.js';

const DELETING: DeletionInfo = {
  state: 'deleting',
  soft: false,
  startedAt: '2026-11-09T12:00:00Z',
};
const FAILED: DeletionInfo = {
  state: 'failed',
  soft: false,
  startedAt: '2026-11-09T12:00:00Z',
  error: 'broker refused',
};

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Stub fetch with one queued answer per call; records each URL and method. */
function stubFetch(...answers: Array<Response | Error>): Array<{ url: string; method: string }> {
  const calls: Array<{ url: string; method: string }> = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: requestUrl(input), method: init?.method ?? 'GET' });
      const next = answers.shift();
      if (!next) throw new Error('unexpected fetch');
      return next instanceof Error ? Promise.reject(next) : Promise.resolve(next);
    })
  );
  return calls;
}

/** A fake clock: sleep advances it, so the budget is reached deterministically. */
function fakeClock(): { now: () => number; sleep: (ms: number) => Promise<void> } {
  let t = 0;
  return {
    now: (): number => t,
    sleep: (ms: number): Promise<void> => {
      t += ms;
      return Promise.resolve();
    },
  };
}

beforeEach(() => {
  holder.sm = { applyDeleteAccepted: vi.fn(() => true) };
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

afterEach(() => {
  holder.sm = null;
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('deleteAgentAndWait', () => {
  it('204 is confirmed at once, without a confirm dialog', async () => {
    const calls = stubFetch(new Response(null, { status: 204 }));
    const out = await deleteAgentAndWait('a1', 'alpha');
    expect(out).toEqual({ kind: 'confirmed' });
    expect(calls).toEqual([{ url: '/api/v1/agents/a1', method: 'DELETE' }]);
    expect(showConfirm).not.toHaveBeenCalled();
  });

  it('a 404 on the DELETE is an agent already gone', async () => {
    stubFetch(json(404, { error: { code: 'not_found', message: 'Agent not found' } }));
    expect(await deleteAgentAndWait('a1', 'alpha')).toEqual({ kind: 'confirmed' });
  });

  it('a refused DELETE fails without a force fallback', async () => {
    const calls = stubFetch(json(503, { error: { code: 'unavailable', message: 'broker down' } }));
    const out = await deleteAgentAndWait('a1', 'alpha');
    expect(out).toEqual({ kind: 'failed', reason: 'delete_failed', message: 'broker down' });
    expect(calls).toHaveLength(1);
    expect(showConfirm).not.toHaveBeenCalled();
  });

  it('202 then a poll 404 is confirmed', async () => {
    const clock = fakeClock();
    const calls = stubFetch(
      json(202, { agentId: 'a1', deletion: DELETING }),
      json(200, { id: 'a1', deletion: DELETING }),
      json(404, { error: { code: 'not_found' } })
    );
    const out = await deleteAgentAndWait('a1', 'alpha', clock);
    expect(out).toEqual({ kind: 'confirmed' });
    expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual([
      'DELETE /api/v1/agents/a1',
      'GET /api/v1/agents/a1',
      'GET /api/v1/agents/a1',
    ]);
  });

  it('202 then a set deletedAt is confirmed', async () => {
    stubFetch(
      json(202, { agentId: 'a1', deletion: DELETING }),
      json(200, { id: 'a1', deletedAt: '2026-11-09T12:00:05Z' })
    );
    expect(await deleteAgentAndWait('a1', 'alpha', fakeClock())).toEqual({ kind: 'confirmed' });
  });

  it("Go's zero deletedAt is not a confirmation", async () => {
    stubFetch(
      json(202, { agentId: 'a1', deletion: DELETING }),
      json(200, { id: 'a1', deletedAt: '0001-01-01T00:00:00Z', deletion: null }),
      json(200, { id: 'a1', deletedAt: '0001-01-01T00:00:00Z', deletion: null })
    );
    const out = await deleteAgentAndWait('a1', 'alpha', fakeClock());
    expect(out).toMatchObject({ kind: 'failed', reason: 'not_taken' });
  });
});

describe('waitForAgentDeletion', () => {
  it('a failed deletion state stops with the hub message', async () => {
    stubFetch(json(200, { id: 'a1', deletion: FAILED }));
    expect(await waitForAgentDeletion('a1', fakeClock())).toEqual({
      kind: 'failed',
      reason: 'deletion_failed',
      message: 'broker refused',
    });
  });

  it('a 403 poll is unobservable, not confirmed', async () => {
    stubFetch(json(403, { error: { code: 'forbidden' } }));
    expect(await waitForAgentDeletion('a1', fakeClock())).toMatchObject({
      kind: 'failed',
      reason: 'unobservable',
    });
  });

  it('5xx and network errors keep polling until the budget runs out', async () => {
    const clock = fakeClock();
    const answers: Array<Response | Error> = [];
    for (let i = 0; i < 10; i++) {
      answers.push(i % 2 ? new Error('offline') : json(502, {}));
    }
    stubFetch(...answers);
    const out = await waitForAgentDeletion('a1', { ...clock, intervalMs: 1000, timeoutMs: 3000 });
    expect(out).toMatchObject({ kind: 'failed', reason: 'timed_out' });
  });

  it('a delete still running at the deadline times out', async () => {
    const clock = fakeClock();
    stubFetch(
      json(200, { id: 'a1', deletion: DELETING }),
      json(200, { id: 'a1', deletion: DELETING }),
      json(200, { id: 'a1', deletion: DELETING })
    );
    const out = await waitForAgentDeletion('a1', { ...clock, intervalMs: 1000, timeoutMs: 2000 });
    expect(out).toMatchObject({ kind: 'failed', reason: 'timed_out' });
  });

  it('one live poll without a marker is not yet not_taken (read-after-write lag)', async () => {
    stubFetch(
      json(200, { id: 'a1', deletion: null }),
      json(200, { id: 'a1', deletion: DELETING }),
      json(404, {})
    );
    expect(await waitForAgentDeletion('a1', fakeClock())).toEqual({ kind: 'confirmed' });
  });
});
