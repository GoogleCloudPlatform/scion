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
 * Tests for scope-capability caching (#98).
 *
 * Agent-scope and project-scope capabilities are different vocabularies, and
 * the Hub computes them with separate calls. They used to share one slot, so
 * the Projects page's answer could be read back by the Agents page — which
 * decides whether "New Agent" renders. `setScope` clears the cache, but both
 * pages declare the same `dashboard` scope, so it early-returns between them
 * and the stale value survived.
 *
 * These tests pin the property that actually matters: a read for one resource
 * never returns what was written for another.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import type { Capabilities } from '../shared/types.js';

/**
 * Stand-in for EventSource, which happy-dom does not implement.
 *
 * setScope opens an SSE subscription for the new scope, so the tests that
 * exercise a navigation need this even though they assert nothing about SSE.
 */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;

  constructor(readonly url: string) {
    super();
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

/** Capabilities as the Hub returns them for a scope. */
const caps = (...actions: string[]): Capabilities => ({ actions });

describe('scope capabilities are keyed by resource', () => {
  it('does not hand one resource the other resource capabilities', () => {
    const sm = new StateManager();

    // Projects page loads first and caches what it may do with projects.
    sm.seedScopeCapabilities('project', caps('create', 'delete'));

    // The Agents page must not read that back, even though nothing cleared it.
    expect(sm.getScopeCapabilities('agent')).toBeUndefined();
    expect(sm.getScopeCapabilities('project')).toEqual(caps('create', 'delete'));
  });

  it('keeps both resources independently', () => {
    const sm = new StateManager();

    sm.seedScopeCapabilities('agent', caps('create'));
    sm.seedScopeCapabilities('project', caps('read'));

    expect(sm.getScopeCapabilities('agent')).toEqual(caps('create'));
    expect(sm.getScopeCapabilities('project')).toEqual(caps('read'));
  });

  it('a later seed for one resource does not overwrite the other', () => {
    const sm = new StateManager();

    sm.seedScopeCapabilities('agent', caps('create'));
    sm.seedScopeCapabilities('project', caps('read'));
    // Projects refetches and comes back with fewer actions.
    sm.seedScopeCapabilities('project', caps());

    expect(sm.getScopeCapabilities('agent')).toEqual(caps('create'));
    expect(sm.getScopeCapabilities('project')).toEqual(caps());
  });

  it(
    'survives the navigation that caused the bug: agents -> projects -> agents ' +
      'leaves agent capabilities intact and never yields project ones',
    () => {
      const sm = new StateManager();

      // Visit Agents: scope becomes dashboard, agent caps cached.
      sm.setScope({ type: 'dashboard' });
      sm.seedScopeCapabilities('agent', caps('create'));

      // Visit Projects: same scope, so setScope early-returns and clears nothing.
      sm.setScope({ type: 'dashboard' });
      sm.seedScopeCapabilities('project', caps());

      // Back to Agents: the answer must still be the agent one.
      sm.setScope({ type: 'dashboard' });
      expect(sm.getScopeCapabilities('agent')).toEqual(caps('create'));
    }
  );

  it('clears every resource when the scope actually changes', () => {
    const sm = new StateManager();

    sm.setScope({ type: 'dashboard' });
    sm.seedScopeCapabilities('agent', caps('create'));
    sm.seedScopeCapabilities('project', caps('create'));

    // A real scope change must drop cached capabilities with the rest of state.
    sm.setScope({ type: 'brokers-list' });

    expect(sm.getScopeCapabilities('agent')).toBeUndefined();
    expect(sm.getScopeCapabilities('project')).toBeUndefined();
  });
});

describe('SSR hydrate attributes capabilities to the prefetched list', () => {
  it('attributes them to agents when the payload carries agents', () => {
    const sm = new StateManager();

    sm.hydrate({ agents: [] }, caps('create'));

    expect(sm.getScopeCapabilities('agent')).toEqual(caps('create'));
    expect(sm.getScopeCapabilities('project')).toBeUndefined();
  });

  it('attributes them to projects when the payload carries projects', () => {
    const sm = new StateManager();

    sm.hydrate({ projects: [] }, caps('create'));

    expect(sm.getScopeCapabilities('project')).toEqual(caps('create'));
    expect(sm.getScopeCapabilities('agent')).toBeUndefined();
  });

  it('drops them when the payload carries both lists, rather than guessing', () => {
    const sm = new StateManager();

    // One capability set cannot describe both resources. Attributing it to
    // both would reintroduce the defect, so neither is seeded and each page
    // fetches its own.
    sm.hydrate({ agents: [], projects: [] }, caps('create'));

    expect(sm.getScopeCapabilities('agent')).toBeUndefined();
    expect(sm.getScopeCapabilities('project')).toBeUndefined();
  });

  it('seeds nothing when the payload has no capabilities', () => {
    const sm = new StateManager();

    sm.hydrate({ agents: [] });

    expect(sm.getScopeCapabilities('agent')).toBeUndefined();
  });
});
