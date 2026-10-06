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
 * agent-configure's Delete goes through the shared helper
 * (ptone/scion#2483 phase 2): its own dialog is the confirm, there is no
 * force fallback, 204 and 202 both go to /agents, and a failure is shown
 * inline.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

vi.mock('../../client/agent-delete.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/agent-delete.js')>();
  return { ...actual, runAgentDelete: vi.fn(actual.runAgentDelete) };
});
vi.mock('../../client/main.js', () => ({ navigateTo: vi.fn() }));

import { runAgentDelete } from '../../client/agent-delete.js';
import { navigateTo } from '../../client/main.js';
import './agent-configure.js';

type ConfigureInternals = HTMLElement & {
  agentId: string;
  error: string | null;
  handleDelete(): Promise<void>;
};

function page(): ConfigureInternals {
  // Not attached: the delete path needs no load or render.
  const el = document.createElement('scion-page-agent-configure') as ConfigureInternals;
  el.agentId = 'agent-1';
  return el;
}

const mutations: string[] = [];

beforeEach(() => {
  mutations.length = 0;
  vi.mocked(runAgentDelete).mockClear();
  vi.mocked(navigateTo).mockClear();
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubFetch(answer: () => Response): void {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request, init?: RequestInit) => {
      mutations.push(`${init?.method ?? 'GET'} ${String(input)}`);
      return Promise.resolve(answer());
    })
  );
}

describe('agent-configure delete', () => {
  it('delegates to the helper (no confirm, no force fallback) and goes to /agents on 204', async () => {
    stubFetch(() => new Response(null, { status: 204 }));
    const el = page();
    await el.handleDelete();
    expect(runAgentDelete).toHaveBeenCalledTimes(1);
    expect(runAgentDelete).toHaveBeenCalledWith({
      agentId: 'agent-1',
      confirm: false,
      forceFallback: false,
    });
    expect(mutations).toEqual(['DELETE /api/v1/agents/agent-1']);
    expect(navigateTo).toHaveBeenCalledWith('/agents');
  });

  it('goes to /agents on 202 too (the list shows Deleting…)', async () => {
    stubFetch(
      () =>
        new Response(
          JSON.stringify({
            agentId: 'agent-1',
            deletion: {
              state: 'deleting',
              soft: false,
              claim: 1,
              startedAt: '2026-10-04T12:00:00Z',
            },
          }),
          { status: 202, headers: { 'Content-Type': 'application/json' } }
        )
    );
    await page().handleDelete();
    expect(navigateTo).toHaveBeenCalledWith('/agents');
  });

  it('a 502 is shown inline, with no force offer and no navigation', async () => {
    stubFetch(
      () =>
        new Response(JSON.stringify({ error: { code: 'runtime_error', message: 'broker down' } }), {
          status: 502,
          headers: { 'Content-Type': 'application/json' },
        })
    );
    const el = page();
    await el.handleDelete();
    expect(el.error).toBe('broker down');
    expect(mutations).toEqual(['DELETE /api/v1/agents/agent-1']);
    expect(navigateTo).not.toHaveBeenCalled();
  });

  it('the page itself sends no DELETE: only the helper does', async () => {
    stubFetch(() => new Response(null, { status: 204 }));
    vi.mocked(runAgentDelete).mockResolvedValueOnce({ kind: 'deleted', forced: false });
    await page().handleDelete();
    expect(mutations).toEqual([]);
    expect(navigateTo).toHaveBeenCalledWith('/agents');
  });
});
