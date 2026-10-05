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
 * candidate shape (an `agent` target, no DM recency), and the load and
 * retain over the shared agent store's hub entry.
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
  retainTerminalPaletteAgents,
  selectTerminalPaletteCandidates,
} from './terminal-palette-data.js';
import { ChatPaletteDataController, type RawPaletteAgent } from './chat-palette-data.js';
import type { PaletteCandidate } from './chat-palette-types.js';
import type { Agent } from '../shared/types.js';
import { createHarness, settle, type Harness } from './__fixtures__/agent-store-harness.js';

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

let harness: Harness | null = null;

afterEach(() => {
  harness?.store.destroy();
  harness = null;
  apiFetchMock.mockReset();
  vi.unstubAllGlobals();
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

/** An agent-list row as the store holds it: attachable and running unless overridden. */
function row(id: string, extra: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p1',
    phase: 'running',
    _capabilities: { actions: ['attach'] },
    ...extra,
  } as Agent;
}

/** A store over an in-memory hub serving `agents`, under fake timers. */
function storeWith(agents: Agent[], pageSize?: number): Harness {
  vi.useFakeTimers();
  harness = createHarness(agents, pageSize ? { pageSize } : {});
  return harness;
}

function load(
  h: Harness,
  overrides: { onProgress?: (c: PaletteCandidate[]) => void; isCurrent?: () => boolean } = {}
): { controller: AbortController; promise: Promise<PaletteCandidate[]> } {
  const controller = new AbortController();
  const promise = loadTerminalPaletteAgents(
    {
      controller,
      isCurrent: overrides.isCurrent ?? ((): boolean => true),
      ...(overrides.onProgress ? { onProgress: overrides.onProgress } : {}),
    },
    h.store
  );
  return { controller, promise };
}

function labels(candidates: readonly PaletteCandidate[]): string[] {
  return candidates.map((c) => c.label);
}

describe('loadTerminalPaletteAgents', () => {
  it('walks the hub list (no project filter) and returns only viable candidates, with no DM fetch', async () => {
    const h = storeWith([row('a1', { name: 'Running' }), row('a2', { phase: 'stopped' })]);

    const { promise } = load(h);
    await h.connect();

    expect(labels(await promise)).toEqual(['Running']);
    expect(h.server.walks()).toBe(1);
    expect(h.server.requests.every((path) => path.startsWith('/api/v1/agents?'))).toBe(true);
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('publishes the viable candidates seen so far as the walk starts and after each page', async () => {
    const h = storeWith(
      [
        row('a1', { name: 'First' }),
        row('x1', { phase: 'stopped' }),
        row('x2', { _capabilities: { actions: ['lifecycle'] } }),
        row('a2', { name: 'Second' }),
      ],
      2
    );
    const progress: string[][] = [];

    const { promise } = load(h, { onProgress: (c) => progress.push(labels(c)) });
    await h.connect();

    expect(progress).toEqual([[], ['First'], ['First', 'Second']]);
    expect(labels(await promise)).toEqual(['First', 'Second']);
  });

  it('a reopen answers from the store with no request', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' })]);
    const first = load(h).promise;
    await h.connect();
    await first;
    const requests = h.server.requests.length;

    expect(labels(await load(h).promise)).toEqual(['Alpha']);
    expect(labels(await load(h).promise)).toEqual(['Alpha']);
    expect(h.server.requests.length).toBe(requests);
  });

  it('concurrent loads share one walk', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' })]);

    const loads = [load(h).promise, load(h).promise, load(h).promise];
    await h.connect();

    for (const candidates of await Promise.all(loads))
      expect(labels(candidates)).toEqual(['Alpha']);
    expect(h.server.walks()).toBe(1);
  });

  it('shares one walk with the chat palette', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' })]);
    apiFetchMock.mockResolvedValue(jsonResponse({ dms: [] }));
    const chat = new ChatPaletteDataController(h.store);

    const chatLoad = chat.loadAgentsGroup();
    const terminalLoad = load(h).promise;
    await h.connect();

    expect(labels(await chatLoad)).toEqual(['Alpha']);
    expect(labels(await terminalLoad)).toEqual(['Alpha']);
    expect(h.server.walks()).toBe(1);
  });

  it('a superseded load rejects as an AbortError without publishing progress, and the walk goes on for the others', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' })]);
    const progress: PaletteCandidate[][] = [];
    let current = true;
    const stale = load(h, { isCurrent: () => current, onProgress: (c) => progress.push(c) });
    const other = load(h).promise;
    current = false;
    stale.controller.abort();
    progress.length = 0;
    const rejection = expect(stale.promise).rejects.toMatchObject({ name: 'AbortError' });

    await h.connect();

    await rejection;
    expect(progress).toEqual([]);
    expect(labels(await other)).toEqual(['Alpha']);
    expect(h.server.walks()).toBe(1);
  });

  it('a load no longer current when the walk resolves rejects as an AbortError', async () => {
    const h = storeWith([row('a1')]);
    let current = true;
    const { promise } = load(h, { isCurrent: () => current });
    const rejection = expect(promise).rejects.toMatchObject({ name: 'AbortError' });
    current = false;

    await h.connect();

    await rejection;
  });

  it("rejects with the store's error when the walk fails, and a retry walks again", async () => {
    const h = storeWith([row('a1', { name: 'Alpha' })]);
    h.server.status = 500;
    const failed = load(h).promise;
    const rejection = expect(failed).rejects.toBeInstanceOf(Error);
    await h.connect();
    await rejection;
    await expect(failed).rejects.not.toMatchObject({ name: 'AbortError' });

    h.server.status = 200;
    const retry = load(h).promise;
    await settle();

    expect(labels(await retry)).toEqual(['Alpha']);
    expect(h.server.walks()).toBe(2);
  });
});

describe('selectTerminalPaletteCandidates', () => {
  it('returns the same candidates for a republished, unchanged row array', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' })]);
    const first = load(h).promise;
    await h.connect();
    await first;
    const snapshot = h.store.peek({ scope: 'hub' })!;

    const candidates = selectTerminalPaletteCandidates(snapshot);

    expect(selectTerminalPaletteCandidates({ ...snapshot, version: snapshot.version + 1 })).toBe(
      candidates
    );
    expect(selectTerminalPaletteCandidates({ ...snapshot, agents: [...snapshot.agents] })).not.toBe(
      candidates
    );
  });
});

describe('retainTerminalPaletteAgents', () => {
  it('an SSE status change reaches the retained listener with no request', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' }), row('a2', { name: 'Beta' })]);
    const heard: string[][] = [];
    const release = retainTerminalPaletteAgents((c) => heard.push(labels(c)), h.store);
    const first = load(h).promise;
    await h.connect();
    await first;
    const requests = h.server.requests.length;
    heard.length = 0;

    await h.emitAgent('status', { agentId: 'a2', phase: 'stopped' });

    expect(heard).toEqual([['Alpha']]);
    expect(h.server.requests.length).toBe(requests);
    release();
  });

  it('an SSE status change reaches the terminal and the chat palette consumers of one entry with no request', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' }), row('a2', { name: 'Beta' })]);
    apiFetchMock.mockResolvedValue(jsonResponse({ dms: [] }));
    const chat = new ChatPaletteDataController(h.store);
    const terminalHeard: string[][] = [];
    const chatHeard: string[][] = [];
    const releaseTerminal = retainTerminalPaletteAgents(
      (c) => terminalHeard.push(labels(c)),
      h.store
    );
    const releaseChat = h.store.retain({ scope: 'hub' }, (snapshot) => {
      const candidates = chat.deriveAgentCandidates(snapshot);
      if (snapshot.status === 'ready' && candidates) chatHeard.push(labels(candidates));
    });
    const loads = [chat.loadAgentsGroup(), load(h).promise];
    await h.connect();
    await Promise.all(loads);
    const requests = h.server.requests.length;
    terminalHeard.length = 0;
    chatHeard.length = 0;

    // Stopping removes Beta from the terminal palette (not attachable) and
    // keeps it in chat (a stopped agent can still be messaged).
    await h.emitAgent('status', { agentId: 'a2', phase: 'stopped' });
    await h.emitAgent('deleted', { agentId: 'a1' });

    expect(terminalHeard).toEqual([['Alpha'], []]);
    expect(chatHeard).toEqual([['Alpha', 'Beta'], ['Beta']]);
    expect(h.server.requests.length).toBe(requests);
    expect(h.server.walks()).toBe(1);
    releaseTerminal();
    releaseChat();
  });

  it('hears ready snapshots only, not the progress of a walk', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' }), row('a2', { name: 'Beta' })], 1);
    const heard: string[][] = [];
    const release = retainTerminalPaletteAgents((c) => heard.push(labels(c)), h.store);

    const first = load(h).promise;
    await h.connect();
    await first;

    expect(heard).toEqual([['Alpha', 'Beta']]);
    release();
  });

  it('does not fetch by itself', async () => {
    const h = storeWith([row('a1')]);
    const release = retainTerminalPaletteAgents(() => {}, h.store);
    await h.connect();

    expect(h.server.requests).toEqual([]);
    release();
  });

  it('stops hearing changes once released', async () => {
    const h = storeWith([row('a1', { name: 'Alpha' }), row('a2', { name: 'Beta' })]);
    const heard: string[][] = [];
    const releaseRetain = retainTerminalPaletteAgents((c) => heard.push(labels(c)), h.store);
    const keepAlive = h.store.retain({ scope: 'hub' }, () => {});
    const first = load(h).promise;
    await h.connect();
    await first;
    heard.length = 0;

    releaseRetain();
    await h.emitAgent('status', { agentId: 'a2', phase: 'stopped' });

    expect(heard).toEqual([]);
    keepAlive();
  });
});
