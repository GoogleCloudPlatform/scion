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
 * Tests for scion-page-agents' `loadedScope` tracking: the graph view's
 * filterKey must reflect the scope the currently-loaded agent list was
 * actually fetched for, not `agentScope` (which changes synchronously on
 * click, before the new list arrives).
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import './agents.js';
import type { ScionPageAgents } from './agents.js';
import { stateManager } from '../../client/state.js';
import type { Agent } from '../../shared/types.js';

beforeAll(() => {
  const store = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
});

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function agent(id: string): Agent {
  return { id, name: id, projectId: 'p1', template: 't', phase: 'running' } as Agent;
}

describe('scion-page-agents loadedScope tracking', () => {
  let el: ScionPageAgents;
  let resolveScoped: ((r: Response) => void) | null = null;

  beforeEach(async () => {
    vi.spyOn(stateManager, 'setScope').mockImplementation(() => {});
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([]);
    vi.spyOn(stateManager, 'getScopeCapabilities').mockReturnValue(undefined);
    vi.spyOn(stateManager, 'getDeletedAgentIds').mockReturnValue(new Set());
    vi.spyOn(stateManager, 'seedAgents').mockImplementation(() => {});
    vi.spyOn(stateManager, 'seedScopeCapabilities').mockImplementation(() => {});

    resolveScoped = null;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const url = typeof input === 'string' ? input : input.toString();
        if (url.includes('scope=mine')) {
          // Held open until the test explicitly resolves it, so the
          // in-flight window (agentScope already 'mine', loadedScope not yet
          // updated) can be observed.
          return new Promise<Response>((resolve) => {
            resolveScoped = resolve;
          });
        }
        return Promise.resolve(jsonResponse({ agents: [agent('a1')] }));
      })
    );

    el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await el.updateComplete;
    await Promise.resolve(); // flush the initial (unscoped) fetchAndMergeAgents
    await el.updateComplete;
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function loadedScope(): string {
    return (el as unknown as { loadedScope: string }).loadedScope;
  }

  it('does not update loadedScope until the newly-scoped list actually lands', async () => {
    expect(loadedScope()).toBe('all');

    (el as unknown as { setScope: (s: 'all' | 'mine' | 'shared') => void }).setScope('mine');
    // setScope flips agentScope synchronously and fires loadAgents() without
    // awaiting it; immediately after the call returns, the request for
    // scope=mine has only just started (apiFetch's first await hasn't
    // resolved), so loadedScope must still read the previously-loaded scope.
    expect(loadedScope()).toBe('all');

    resolveScoped!(jsonResponse({ agents: [agent('a2')] }));
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    expect(loadedScope()).toBe('mine');
    expect((el as unknown as { error: string | null }).error).toBeNull();
    expect((el as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toEqual(['a2']);
  });
});
