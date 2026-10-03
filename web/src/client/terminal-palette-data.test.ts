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
 * Tests for the terminal view's agents-only candidate source: viability
 * (attach + running/stopping, not chat's lifecycle-or-attach messageability)
 * candidate shape (an `agent` target, no DM recency), and the bounded,
 * progressive load.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, afterEach } from 'vitest';

vi.mock('./api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(),
  };
});

import { apiFetch } from './api.js';
import {
  isTerminalPaletteAgentViable,
  buildTerminalAgentCandidates,
  loadTerminalPaletteAgents,
} from './terminal-palette-data.js';
import { PaletteLoadError, type RawPaletteAgent } from './chat-palette-data.js';
import type { PaletteCandidate } from './chat-palette-types.js';

const apiFetchMock = vi.mocked(apiFetch);

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status });
}

/** An attachable running agent, with `overrides` applied and the `omit` keys removed. */
function agent(
  overrides: Partial<RawPaletteAgent> = {},
  omit: ReadonlyArray<Exclude<keyof RawPaletteAgent, 'id'>> = []
): RawPaletteAgent {
  const result: RawPaletteAgent = {
    id: 'a1',
    name: 'Agent One',
    phase: 'running',
    _capabilities: { actions: ['attach'] },
    ...overrides,
  };
  for (const key of omit) delete result[key];
  return result;
}

afterEach(() => {
  apiFetchMock.mockReset();
  vi.useRealTimers();
});

describe('isTerminalPaletteAgentViable', () => {
  it('is viable when running and attach-capable', () => {
    expect(isTerminalPaletteAgentViable(agent())).toBe(true);
  });

  it('is viable when stopping and attach-capable', () => {
    expect(isTerminalPaletteAgentViable(agent({ phase: 'stopping' }))).toBe(true);
  });

  it('excludes a stopped agent, even if attach-capable', () => {
    expect(isTerminalPaletteAgentViable(agent({ phase: 'stopped' }))).toBe(false);
  });

  it('excludes every other phase (created, provisioning, cloning, starting, suspended, error)', () => {
    for (const phase of [
      'created',
      'provisioning',
      'cloning',
      'starting',
      'suspended',
      'error',
    ] as const) {
      expect(isTerminalPaletteAgentViable(agent({ phase }))).toBe(false);
    }
  });

  it('excludes a running agent reported offline', () => {
    expect(isTerminalPaletteAgentViable(agent({ activity: 'offline' }))).toBe(false);
  });

  it("excludes an agent the viewer can only message via `lifecycle`, not `attach` — unlike chat's messageability", () => {
    expect(isTerminalPaletteAgentViable(agent({ _capabilities: { actions: ['lifecycle'] } }))).toBe(
      false
    );
  });

  it('excludes an agent with no capabilities at all (fail closed)', () => {
    expect(isTerminalPaletteAgentViable(agent({}, ['_capabilities']))).toBe(false);
  });

  it('is viable with both lifecycle and attach', () => {
    expect(
      isTerminalPaletteAgentViable(agent({ _capabilities: { actions: ['lifecycle', 'attach'] } }))
    ).toBe(true);
  });
});

describe('buildTerminalAgentCandidates', () => {
  it('builds an agent-target candidate for each viable agent, skipping non-viable ones', () => {
    const candidates = buildTerminalAgentCandidates([
      agent({ id: 'a1', name: 'Alice-bot' }),
      agent({ id: 'a2', name: 'Stopped-bot', phase: 'stopped' }),
      agent({ id: 'a3', name: 'No-attach-bot', _capabilities: { actions: ['lifecycle'] } }),
    ]);

    expect(candidates).toHaveLength(1);
    expect(candidates[0]).toMatchObject({
      group: 'agents',
      label: 'Alice-bot',
      activityMs: 0,
      target: { kind: 'agent', agentId: 'a1', displayName: 'Alice-bot' },
    });
  });

  it('uses slug, then id, as the label fallback when name is absent', () => {
    const [bySlug] = buildTerminalAgentCandidates([agent({ slug: 'alice-slug' }, ['name'])]);
    expect(bySlug.label).toBe('alice-slug');

    const [byId] = buildTerminalAgentCandidates([agent({}, ['name', 'slug'])]);
    expect(byId.label).toBe('a1');
  });

  it('uses the project name as the secondary label, falling back to slug', () => {
    const [withProject] = buildTerminalAgentCandidates([
      agent({ project: 'My Project', slug: 'alice-slug' }),
    ]);
    expect(withProject.secondaryLabel).toBe('My Project');

    const [withoutProject] = buildTerminalAgentCandidates([
      agent({ slug: 'alice-slug' }, ['project']),
    ]);
    expect(withoutProject.secondaryLabel).toBe('alice-slug');
  });

  it('does not repeat the slug as the secondary label when the slug is already the label', () => {
    const [candidate] = buildTerminalAgentCandidates([
      agent({ slug: 'alice-slug' }, ['name', 'project']),
    ]);
    expect(candidate.label).toBe('alice-slug');
    expect(candidate.secondaryLabel).toBe('');
  });

  it('makes the name, slug and project all searchable, without duplicates', () => {
    const [candidate] = buildTerminalAgentCandidates([
      agent({ name: 'Alice-bot', slug: 'alice-slug', project: 'My Project' }),
    ]);
    expect(candidate.searchFields).toEqual(['Alice-bot', 'alice-slug', 'My Project']);

    const [slugOnly] = buildTerminalAgentCandidates([
      agent({ slug: 'alice-slug' }, ['name', 'project']),
    ]);
    expect(slugOnly.searchFields).toEqual(['alice-slug']);
  });

  it('skips an agent with no id', () => {
    const candidates = buildTerminalAgentCandidates([agent({ id: '' })]);
    expect(candidates).toEqual([]);
  });

  it('builds a stable agent candidate ID (agentCandidateId)', () => {
    const [candidate] = buildTerminalAgentCandidates([agent({ id: 'a1' })]);
    expect(candidate.id).toBe(JSON.stringify(['agent', 'a1']));
  });
});

function load(
  overrides: { onProgress?: (c: PaletteCandidate[]) => void; isCurrent?: () => boolean } = {}
): { controller: AbortController; promise: Promise<PaletteCandidate[]> } {
  const controller = new AbortController();
  const promise = loadTerminalPaletteAgents({
    controller,
    isCurrent: overrides.isCurrent ?? ((): boolean => true),
    ...(overrides.onProgress ? { onProgress: overrides.onProgress } : {}),
  });
  return { controller, promise };
}

function hangUntilAborted(): (
  url: string,
  options?: { signal?: AbortSignal | null }
) => Promise<Response> {
  return (_url, options) =>
    new Promise((_resolve, reject) => {
      options?.signal?.addEventListener('abort', () => {
        reject(new DOMException('aborted', 'AbortError'));
      });
    });
}

describe('loadTerminalPaletteAgents', () => {
  it('fetches /api/v1/agents (no project filter) and returns only viable candidates, with no DM fetch', async () => {
    apiFetchMock.mockResolvedValueOnce(
      jsonResponse({
        agents: [
          agent({ id: 'a1', name: 'Running' }),
          agent({ id: 'a2', name: 'Stopped', phase: 'stopped' }),
        ],
      })
    );

    const candidates = await load().promise;

    expect(apiFetchMock).toHaveBeenCalledTimes(1);
    expect(apiFetchMock.mock.calls[0][0]).toBe('/api/v1/agents?limit=100');
    expect(candidates.map((c) => c.label)).toEqual(['Running']);
  });

  it("passes the controller's signal through to the underlying fetch", async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ agents: [] }));

    const { controller, promise } = load();
    await promise;

    expect(apiFetchMock.mock.calls[0][1]).toEqual({ signal: controller.signal });
  });

  it('publishes the viable candidates seen so far after each page, before the walk finishes', async () => {
    apiFetchMock
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [
            agent({ id: 'a1', name: 'First' }),
            agent({ id: 'x1', name: 'Stopped', phase: 'stopped' }),
          ],
          nextCursor: 'c1',
        })
      )
      .mockResolvedValueOnce(
        jsonResponse({
          agents: [
            agent({ id: 'x2', name: 'No-attach', _capabilities: { actions: ['lifecycle'] } }),
            agent({ id: 'a2', name: 'Second' }),
          ],
        })
      );
    const progress: string[][] = [];

    const candidates = await load({
      onProgress: (c) => progress.push(c.map((x) => x.label)),
    }).promise;

    expect(progress).toEqual([['First'], ['First', 'Second']]);
    expect(candidates.map((c) => c.label)).toEqual(['First', 'Second']);
  });

  it('does not publish progress for a superseded load, and rejects it as an AbortError', async () => {
    apiFetchMock.mockResolvedValueOnce(jsonResponse({ agents: [agent()] }));
    const progress: PaletteCandidate[][] = [];

    const { promise } = load({ isCurrent: () => false, onProgress: (c) => progress.push(c) });

    await expect(promise).rejects.toMatchObject({ name: 'AbortError' });
    expect(progress).toEqual([]);
  });

  it('a request that never settles is aborted after the idle bound and rejects with a retryable PaletteLoadError', async () => {
    vi.useFakeTimers();
    let aborted = false;
    apiFetchMock.mockImplementationOnce((url, options) => {
      options?.signal?.addEventListener('abort', () => {
        aborted = true;
      });
      return hangUntilAborted()(url, options);
    });

    const { promise } = load();
    promise.catch(() => {});

    await vi.advanceTimersByTimeAsync(89_999);
    expect(aborted).toBe(false);

    const expectation = expect(promise).rejects.toBeInstanceOf(PaletteLoadError);
    await vi.advanceTimersByTimeAsync(2);
    expect(aborted).toBe(true);
    await expectation;
  });
});
