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
 * Tests for the <scion-agent-tree-view> keyboard shortcuts:
 *   o     — toggle orientation vertical ⇄ horizontal
 *   f     — fit to view
 *   + / = — zoom in
 *   -     — zoom out
 * Shortcuts must be ignored when a modifier key is held or when the event
 * originates from a text-entry/overlay context (inputs, sl-dialog, etc.).
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
import './agent-tree-view.js';
import type { ScionAgentTreeView } from './agent-tree-view.js';
import type { Agent } from '../../shared/types.js';

// happy-dom's localStorage is not functional in this setup; the component
// reads/writes the show-users preference from it, so provide a minimal stub.
beforeAll(() => {
  const store = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
});

function agent(id: string, name: string, ancestry?: string[]): Agent {
  return { id, name, ancestry, projectId: 'p1', template: 't', phase: 'running' } as Agent;
}

function pressKey(key: string, init: KeyboardEventInit = {}, target: EventTarget = window): void {
  target.dispatchEvent(
    new KeyboardEvent('keydown', { key, bubbles: true, composed: true, ...init })
  );
}

describe('scion-agent-tree-view keyboard shortcuts', () => {
  let el: ScionAgentTreeView;

  beforeEach(async () => {
    el = document.createElement('scion-agent-tree-view') as ScionAgentTreeView;
    el.agents = [agent('r1', 'root', ['user-1']), agent('k1', 'kid', ['user-1', 'r1'])];
    document.body.appendChild(el);
    await el.updateComplete;
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  it('toggles orientation on "t" and dispatches orientation-change', () => {
    let eventOrientation: string | null = null;
    el.addEventListener('orientation-change', (e) => {
      eventOrientation = (e as CustomEvent<{ orientation: string }>).detail.orientation;
    });

    expect(el.orientation).toBe('vertical');
    pressKey('t');
    expect(el.orientation).toBe('horizontal');
    expect(eventOrientation).toBe('horizontal');
    pressKey('T');
    expect(el.orientation).toBe('vertical');
    expect(eventOrientation).toBe('vertical');
  });

  it('ignores shortcuts when a modifier key is held', () => {
    pressKey('t', { ctrlKey: true });
    pressKey('t', { metaKey: true });
    pressKey('t', { altKey: true });
    expect(el.orientation).toBe('vertical');
  });

  it('ignores shortcuts originating from text-entry targets', () => {
    const input = document.createElement('input');
    document.body.appendChild(input);
    pressKey('t', {}, input);
    expect(el.orientation).toBe('vertical');

    const dialog = document.createElement('sl-dialog');
    const inner = document.createElement('button');
    dialog.appendChild(inner);
    document.body.appendChild(dialog);
    pressKey('t', {}, inner);
    expect(el.orientation).toBe('vertical');

    // Sanity check: the same key from the background still works.
    pressKey('t');
    expect(el.orientation).toBe('horizontal');
  });

  it('zooms in on "+"/"=" and out on "-"', () => {
    // scale is internal state; reach in for assertion purposes only.
    const scaleOf = () => (el as unknown as { scale: number }).scale;
    const initial = scaleOf();
    pressKey('+');
    expect(scaleOf()).toBeCloseTo(initial * 1.25);
    pressKey('=');
    expect(scaleOf()).toBeCloseTo(initial * 1.25 * 1.25);
    pressKey('-');
    expect(scaleOf()).toBeCloseTo(initial * 1.25);
  });

  it('does not throw on "f" (fit) even when the canvas has no size', () => {
    expect(() => pressKey('f')).not.toThrow();
  });

  it('stops handling keys after removal from the DOM', () => {
    el.remove();
    pressKey('t');
    expect(el.orientation).toBe('vertical');
  });
});

/** Reads the private layout-cache entry's layout object, for identity checks only. */
function cachedLayout(el: ScionAgentTreeView): unknown {
  return (el as unknown as { layoutCache: { layout: unknown } | null }).layoutCache?.layout;
}

/** node-wrapper positions keyed by agent id, read from the rendered shadow DOM. */
function nodePositions(el: ScionAgentTreeView): Record<string, string | null> {
  const positions: Record<string, string | null> = {};
  const wrappers = el.shadowRoot!.querySelectorAll('.node-wrapper');
  wrappers.forEach((wrapper) => {
    const link = wrapper.querySelector('a.node');
    const href = link?.getAttribute('href') ?? '';
    const id = href.replace('/agents/', '');
    positions[id] = wrapper.getAttribute('style');
  });
  return positions;
}

describe('scion-agent-tree-view layout cache (#2388)', () => {
  let el: ScionAgentTreeView;

  function baseAgents(): Agent[] {
    return [
      agent('r1', 'root', ['user-1']),
      agent('k1', 'kid', ['user-1', 'r1']),
      agent('g1', 'grandkid', ['user-1', 'r1', 'k1']),
    ];
  }

  beforeEach(async () => {
    el = document.createElement('scion-agent-tree-view') as ScionAgentTreeView;
    el.agents = baseAgents();
    document.body.appendChild(el);
    await el.updateComplete;
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  it('computes a layout on first render', () => {
    expect(cachedLayout(el)).toBeTruthy();
  });

  it('reuses the cached layout object on a status-only agent update', async () => {
    const before = cachedLayout(el);
    // New object identities (as a real SSE status delta would produce), same
    // id/parentId/name for every agent: the topology signature is unchanged.
    el.agents = el.agents.map((a) => ({ ...a, phase: 'stopped', activity: 'thinking' }));
    await el.updateComplete;
    expect(cachedLayout(el)).toBe(before);
  });

  it('reuses the cached layout object across pan, zoom and hover', async () => {
    const before = cachedLayout(el);
    (el as unknown as { panX: number; panY: number; scale: number }).panX = 37;
    (el as unknown as { panY: number }).panY = -12;
    (el as unknown as { scale: number }).scale = 1.6;
    (el as unknown as { hoverId: string | null }).hoverId = 'r1';
    await el.updateComplete;
    expect(cachedLayout(el)).toBe(before);
  });

  it('invalidates the cache when an agent is added', async () => {
    const before = cachedLayout(el);
    el.agents = [...el.agents, agent('n1', 'newcomer', ['user-1', 'r1'])];
    await el.updateComplete;
    expect(cachedLayout(el)).not.toBe(before);
  });

  it('invalidates the cache when an agent is removed', async () => {
    const before = cachedLayout(el);
    el.agents = el.agents.filter((a) => a.id !== 'g1');
    await el.updateComplete;
    expect(cachedLayout(el)).not.toBe(before);
  });

  it('invalidates the cache on reparent (ancestry change)', async () => {
    const before = cachedLayout(el);
    el.agents = el.agents.map((a) => (a.id === 'g1' ? agent('g1', 'grandkid', ['user-1']) : a));
    await el.updateComplete;
    expect(cachedLayout(el)).not.toBe(before);
  });

  it('invalidates the cache on rename', async () => {
    const before = cachedLayout(el);
    el.agents = el.agents.map((a) => (a.id === 'k1' ? { ...a, name: 'renamed-kid' } : a));
    await el.updateComplete;
    expect(cachedLayout(el)).not.toBe(before);
  });

  it('invalidates the cache on collapse toggle', async () => {
    const before = cachedLayout(el);
    (el as unknown as { collapsedIds: ReadonlySet<string> }).collapsedIds = new Set(['r1']);
    await el.updateComplete;
    expect(cachedLayout(el)).not.toBe(before);
  });

  it('invalidates the cache when showUsers toggles', async () => {
    const before = cachedLayout(el);
    (el as unknown as { showUsers: boolean }).showUsers = true;
    await el.updateComplete;
    expect(cachedLayout(el)).not.toBe(before);
  });

  it('invalidates the cache on orientation change', async () => {
    const before = cachedLayout(el);
    el.orientation = 'horizontal';
    await el.updateComplete;
    expect(cachedLayout(el)).not.toBe(before);
  });

  it('produces node positions identical to a forced-fresh recomputation', async () => {
    const viaCache = nodePositions(el);

    // Force a cache miss with a structurally-identical-but-different-identity
    // agents array, guaranteeing a real recomputation from scratch.
    (el as unknown as { layoutCache: unknown }).layoutCache = null;
    el.agents = el.agents.map((a) => ({ ...a }));
    await el.updateComplete;
    const viaFreshRecompute = nodePositions(el);

    expect(viaCache).toEqual(viaFreshRecompute);
  });
});

describe('scion-agent-tree-view edge endpoint lookup via id map (#2388)', () => {
  let el: ScionAgentTreeView;

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  it('styles each edge from its own parent/child pair, not a mismatched lookup', async () => {
    // Two independent trees sharing no name; if edge endpoint resolution ever
    // mixed up IDs across nodes, one of these edges would pick up the wrong
    // agent's messageMode and either gain or lose the mismatch styling.
    const agents: Agent[] = [
      { ...agent('r1', 'root-1', ['user-1']), messageMode: 'project' } as Agent,
      { ...agent('k1', 'kid-1', ['user-1', 'r1']), messageMode: 'branch' } as Agent,
      { ...agent('r2', 'root-2', ['user-2']), messageMode: 'project' } as Agent,
      { ...agent('k2', 'kid-2', ['user-2', 'r2']), messageMode: 'project' } as Agent,
    ];
    el = document.createElement('scion-agent-tree-view') as ScionAgentTreeView;
    el.agents = agents;
    document.body.appendChild(el);
    await el.updateComplete;

    const edgePaths = Array.from(el.shadowRoot!.querySelectorAll('svg path.edge'));
    expect(edgePaths).toHaveLength(2);

    // r1 (project) -> k1 (branch) is a mode mismatch: dashed, non-messageable.
    const mismatchEdge = edgePaths.find((p) => p.querySelector('title'));
    expect(mismatchEdge).toBeTruthy();
    expect(mismatchEdge!.querySelector('title')!.textContent).toContain('root-1');
    expect(mismatchEdge!.querySelector('title')!.textContent).toContain('kid-1');
    expect(mismatchEdge!.getAttribute('stroke-dasharray')).toBe('5,5');

    // r2 (project) -> k2 (project) is compatible: no mismatch tooltip.
    const okEdge = edgePaths.find((p) => !p.querySelector('title'));
    expect(okEdge).toBeTruthy();
  });
});
