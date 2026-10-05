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
import {
  buildLineageForest,
  layoutForest,
  layoutForestWithUsers,
  NODE_W,
  NODE_H,
  type ForestLayout,
  type PositionedEdge,
} from '../../shared/lineage.js';

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
    el = document.createElement('scion-agent-tree-view');
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
    const scaleOf = (): number => (el as unknown as { scale: number }).scale;
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

/** Fails with a readable diff if any two node/user rectangles in the
 * component's current cached layout overlap. */
function assertNoNodeOverlap(el: ScionAgentTreeView): void {
  const layout = cachedLayout(el) as ForestLayout;
  const rects = [
    ...layout.nodes.map((n) => ({ id: n.agent.id, px: n.px, py: n.py })),
    ...layout.users.map((u) => ({ id: `user:${u.id}`, px: u.px, py: u.py })),
  ];
  for (let i = 0; i < rects.length; i++) {
    for (let j = i + 1; j < rects.length; j++) {
      const a = rects[i];
      const b = rects[j];
      const overlapsX = a.px < b.px + NODE_W && b.px < a.px + NODE_W;
      const overlapsY = a.py < b.py + NODE_H && b.py < a.py + NODE_H;
      if (overlapsX && overlapsY) {
        throw new Error(`${a.id} (${a.px},${a.py}) overlaps ${b.id} (${b.px},${b.py})`);
      }
    }
  }
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
    el = document.createElement('scion-agent-tree-view');
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

  /** The status-badge `label` for the node whose card links to /agents/<id>. */
  function statusLabel(agentId: string): string | null {
    return (
      el
        .shadowRoot!.querySelector(`a.node[href="/agents/${agentId}"] scion-status-badge`)
        ?.getAttribute('label') ?? null
    );
  }

  it('renders the current status on a cache hit, not the stale cached node object (#2388 review B1)', async () => {
    const before = cachedLayout(el);
    expect(statusLabel('k1')).toBe('running');

    // Same id/parentId/name for every agent (topology signature unchanged,
    // so this is a cache hit), but k1's own status changed. If renderNode
    // trusted the cached PositionedNode.agent instead of resolving the
    // current agent by ID, this would still read 'running'.
    el.agents = el.agents.map((a) => (a.id === 'k1' ? { ...a, phase: 'stopped' } : a));
    await el.updateComplete;

    expect(cachedLayout(el)).toBe(before); // confirms this really was a cache hit
    expect(statusLabel('k1')).toBe('stopped');
  });

  /** Edges whose title indicates non-messageable ("mismatch") styling. */
  function titledEdges(): Element[] {
    return Array.from(el.shadowRoot!.querySelectorAll('svg path.edge')).filter((p) =>
      p.querySelector('title')
    );
  }

  it('renders current edge styling on a cache hit, not the stale cached endpoints (#2388 review B1)', async () => {
    // A single-edge tree, so a messageMode change on one node affects exactly
    // one edge (baseAgents' r1-k1-g1 chain would make k1's messageMode
    // change affect both of k1's edges, muddying the assertion).
    el.agents = [agent('r1', 'root', ['user-1']), agent('k1', 'kid', ['user-1', 'r1'])];
    await el.updateComplete;
    const before = cachedLayout(el);
    // Both default to 'project' mode (agent() sets no messageMode): compatible.
    expect(titledEdges()).toHaveLength(0);

    // Same id/parentId/name (cache hit), but k1's messageMode now mismatches
    // root's. messageMode is not part of the topology signature by design
    // (edge styling reads live agents, same as node status), so this must
    // still show the mismatch on the very next render.
    el.agents = el.agents.map((a) => (a.id === 'k1' ? { ...a, messageMode: 'branch' } : a));
    await el.updateComplete;

    expect(cachedLayout(el)).toBe(before); // confirms this really was a cache hit
    const titled = titledEdges();
    expect(titled).toHaveLength(1);
    expect(titled[0].querySelector('title')!.textContent).toContain('root');
    expect(titled[0].querySelector('title')!.textContent).toContain('kid');
  });
});

describe('scion-agent-tree-view auto-fit scope detection (#2388 review N3)', () => {
  let el: ScionAgentTreeView;

  function didAutoFit(): boolean {
    return (el as unknown as { didAutoFit: boolean }).didAutoFit;
  }
  function setDidAutoFit(v: boolean): void {
    (el as unknown as { didAutoFit: boolean }).didAutoFit = v;
  }

  beforeEach(async () => {
    el = document.createElement('scion-agent-tree-view');
    el.agents = [
      { ...agent('r1', 'root', ['user-1']), projectId: 'p1' } as Agent,
      { ...agent('k1', 'kid', ['user-1', 'r1']), projectId: 'p1' } as Agent,
    ];
    document.body.appendChild(el);
    await el.updateComplete;
    // The canvas has zero size under happy-dom, so the render()-driven
    // rAF auto-fit never actually commits `didAutoFit = true`; set it
    // directly to simulate a completed fit and observe willUpdate's own
    // decision to keep or reset it.
    setDidAutoFit(true);
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  it('does not reset auto-fit when the same project scope reorders or gets a status-only update', async () => {
    el.agents = [{ ...el.agents[1], phase: 'stopped' }, { ...el.agents[0] }]; // reordered, one status changed, same projectId set {p1}
    await el.updateComplete;
    expect(didAutoFit()).toBe(true);
  });

  it('does not reset auto-fit when a multi-project scope reorders (#2388 review N-A)', async () => {
    // The whole point of comparing the projectId *set* instead of agents[0]
    // is a global view where a status sort can put a different project's
    // agent first without the scope (which projects are represented)
    // actually changing. An order-sensitive comparison — e.g. comparing
    // joined projectId arrays instead of sets — passes the single-project
    // reorder test above but fails this one, because reversing a
    // multi-project list changes the joined string even though the set of
    // projects is unchanged.
    el.agents = [
      { ...agent('r1', 'root-1', ['user-1']), projectId: 'p1' } as Agent,
      { ...agent('r2', 'root-2', ['user-2']), projectId: 'p2' } as Agent,
    ];
    await el.updateComplete;
    setDidAutoFit(true);

    el.agents = [
      { ...el.agents[1], phase: 'stopped' }, // p2's agent now first, status changed
      { ...el.agents[0] },
    ]; // reordered, same projectId set {p1, p2}
    await el.updateComplete;
    expect(didAutoFit()).toBe(true);
  });

  it('resets auto-fit when the project scope changes entirely', async () => {
    el.agents = [{ ...agent('r2', 'root-2', ['user-2']), projectId: 'p2' } as Agent];
    await el.updateComplete;
    expect(didAutoFit()).toBe(false);
  });

  it('resets auto-fit when a new project is added to the scope', async () => {
    el.agents = [...el.agents, { ...agent('r2', 'root-2', ['user-2']), projectId: 'p2' } as Agent];
    await el.updateComplete;
    expect(didAutoFit()).toBe(false);
  });

  it('does not reset auto-fit when a project drops out of the scope', async () => {
    // A project leaving scope (e.g. an agent delete emptied it out of a
    // cross-project graph) must not reset the viewport — that is the
    // cross-project variant of the same jarring reset a single-node delete
    // causes, just for the whole canvas instead of one node. Only a project
    // *entering* scope is worth re-fitting for.
    el.agents = [...el.agents, { ...agent('r2', 'root-2', ['user-2']), projectId: 'p2' } as Agent];
    await el.updateComplete;
    setDidAutoFit(true);
    el.agents = el.agents.filter((a) => a.projectId !== 'p2');
    await el.updateComplete;
    expect(didAutoFit()).toBe(true);
  });
});

describe('scion-agent-tree-view stable layout & keyed rendering on delete', () => {
  let el: ScionAgentTreeView;

  function baseAgents(): Agent[] {
    return [
      agent('r1', 'root-1', ['user-1']),
      agent('a1', 'child-a', ['user-1', 'r1']),
      agent('a2', 'child-b', ['user-1', 'r1']),
      agent('r2', 'root-2', ['user-2']),
      agent('b1', 'other-tree-child', ['user-2', 'r2']),
    ];
  }

  beforeEach(async () => {
    el = document.createElement('scion-agent-tree-view');
    el.agents = baseAgents();
    document.body.appendChild(el);
    await el.updateComplete;
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  /** Maps agent id -> its rendered .node-wrapper DOM element, for identity checks. */
  function wrappersById(): Map<string, Element> {
    const out = new Map<string, Element>();
    el.shadowRoot!.querySelectorAll('.node-wrapper').forEach((wrapper) => {
      const href = wrapper.querySelector('a.node')?.getAttribute('href') ?? '';
      out.set(href.replace('/agents/', ''), wrapper);
    });
    return out;
  }

  it('removing a leaf keeps every other node at its exact previous position', async () => {
    const before = nodePositions(el);

    el.agents = el.agents.filter((a) => a.id !== 'a2'); // a2 is a leaf
    await el.updateComplete;

    const after = nodePositions(el);
    expect(after['a2']).toBeUndefined();
    for (const id of ['r1', 'a1', 'r2', 'b1']) {
      expect(after[id]).toBe(before[id]);
    }
  });

  it('removing a parent with children keeps unrelated trees in place and does not overlap', async () => {
    // a1 has no children, so delete r1 instead: a1 and a2 both orphan and
    // get re-rooted, but the unrelated r2/b1 tree must not move or collide
    // with the orphans. Named so the surviving root-1 would *not* trivially
    // land back where it started under a naive global relayout.
    const before = nodePositions(el);

    el.agents = el.agents.filter((a) => a.id !== 'r1');
    await el.updateComplete;

    const after = nodePositions(el);
    expect(after['r1']).toBeUndefined();
    expect(after['r2']).toBe(before['r2']);
    expect(after['b1']).toBe(before['b1']);
    // a1/a2 are still rendered (promoted to roots), just not necessarily at
    // their old positions, and must not overlap r2/b1 or each other.
    expect(after['a1']).toBeTruthy();
    expect(after['a2']).toBeTruthy();
    assertNoNodeOverlap(el);
  });

  it('a widened, promoted tree that sorts after the anchored one still does not overlap it', async () => {
    // Orphans ("zzz-...") sort *after* the anchored tree ("root-1"/root-2" in
    // baseAgents), the opposite ordering from the test above — covering both
    // orderings of the same overlap risk.
    el.agents = [
      agent('anchor-root', 'anchor-root', ['user-1']),
      agent('anchor-child', 'anchor-child', ['user-1', 'anchor-root']),
      agent('victim', 'mmm-victim', ['user-2']),
      agent('zzz-orphan-1', 'zzz-orphan-1', ['user-2', 'victim']),
      agent('zzz-orphan-2', 'zzz-orphan-2', ['user-2', 'victim']),
    ];
    await el.updateComplete;
    const before = nodePositions(el);

    el.agents = el.agents.filter((a) => a.id !== 'victim');
    await el.updateComplete;

    const after = nodePositions(el);
    expect(after['anchor-root']).toBe(before['anchor-root']);
    expect(after['anchor-child']).toBe(before['anchor-child']);
    expect(after['zzz-orphan-1']).toBeTruthy();
    expect(after['zzz-orphan-2']).toBeTruthy();
    assertNoNodeOverlap(el);
  });

  it('keeps DOM element identity for surviving nodes across a deletion (keyed repeat())', async () => {
    const before = wrappersById();

    el.agents = el.agents.filter((a) => a.id !== 'a2');
    await el.updateComplete;

    const after = wrappersById();
    expect(after.has('a2')).toBe(false);
    for (const id of ['r1', 'a1', 'r2', 'b1']) {
      expect(after.get(id)).toBe(before.get(id));
    }
  });

  it('keeps the DOM edge element after a removed one at the same object identity (keyed repeat())', async () => {
    // Deleting a2 removes the *middle* edge (r1->a2) from [r1->a1, r1->a2,
    // r2->b1]. Holding a direct reference to the r2->b1 element (the one
    // *after* the removed edge, index 2) and checking it is still present by
    // object identity — not merely "an edge with these attributes exists" —
    // is what actually distinguishes keyed repeat() from plain .map():
    // unkeyed positional diffing reuses the DOM node at index 1 (originally
    // r1->a2) to display r2->b1's new data and drops the node that *was* at
    // index 2 (the array only has 2 slots left), so this reference would be
    // gone without the fix (confirmed by reverting repeat() to .map() here).
    const beforeEdges = Array.from(el.shadowRoot!.querySelectorAll('svg path.edge'));
    expect(beforeEdges).toHaveLength(3); // r1->a1, r1->a2, r2->b1
    const survivor = beforeEdges[2];

    el.agents = el.agents.filter((a) => a.id !== 'a2');
    await el.updateComplete;

    const afterEdges = Array.from(el.shadowRoot!.querySelectorAll('svg path.edge'));
    expect(afterEdges).toHaveLength(2);
    expect(afterEdges).toContain(survivor);
  });

  it('keeps DOM element identity for a surviving user group across a deletion (keyed repeat())', async () => {
    // user-1 is rendered before user-2 (sorted/grouped order); removing
    // user-1's only agent must not reuse its DOM slot for user-2 under
    // unkeyed rendering.
    // Named so user-1's root sorts (and therefore renders) before user-2's.
    (el as unknown as { showUsers: boolean }).showUsers = true;
    el.agents = [agent('solo', 'aaa-solo', ['user-1']), agent('r2', 'zzz-root-2', ['user-2'])];
    await el.updateComplete;

    const userEls = (): Element[] => Array.from(el.shadowRoot!.querySelectorAll('.node.user'));
    expect(userEls()).toHaveLength(2);
    const survivor = userEls()[1]; // user-2's card, rendered after user-1's

    el.agents = el.agents.filter((a) => a.id !== 'solo'); // removes user-1 entirely
    await el.updateComplete;

    const after = userEls();
    expect(after).toHaveLength(1);
    expect(after).toContain(survivor);
  });

  it('does not reset pan/zoom or auto-fit when a delete empties a second project out of scope', async () => {
    // A single-project leaf delete never reset auto-fit even before this
    // fix (scopeChanged/scopeExpanded only look at projectId sets). The
    // actual regression case is a *cross-project* graph where deleting the
    // last agent of one project used to reset the viewport.
    el.agents = [...el.agents, { ...agent('p2-root', 'p2-root', ['user-3']), projectId: 'p2' }];
    await el.updateComplete;

    const view = el as unknown as {
      panX: number;
      panY: number;
      scale: number;
      didAutoFit: boolean;
    };
    view.didAutoFit = true;
    view.panX = 42;
    view.panY = -17;
    view.scale = 1.4;

    el.agents = el.agents.filter((a) => a.id !== 'p2-root'); // empties project p2 out of scope
    await el.updateComplete;

    expect(view.didAutoFit).toBe(true);
    expect(view.panX).toBe(42);
    expect(view.panY).toBe(-17);
    expect(view.scale).toBe(1.4);
  });
});

describe('scion-agent-tree-view filterKey distinguishes a filter change from a delete', () => {
  let el: ScionAgentTreeView;

  function baseAgents(): Agent[] {
    return [
      agent('r1', 'root-1', ['user-1']),
      agent('a1', 'child-a', ['user-1', 'r1']),
      agent('a2', 'child-b', ['user-1', 'r1']),
    ];
  }

  beforeEach(async () => {
    el = document.createElement('scion-agent-tree-view');
    el.filterKey = 'running';
    el.agents = baseAgents();
    document.body.appendChild(el);
    await el.updateComplete;
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  it('a shrink with filterKey unchanged keeps survivors at their exact previous position (a delete)', async () => {
    const before = nodePositions(el);
    el.agents = el.agents.filter((a) => a.id !== 'a2');
    await el.updateComplete;
    const after = nodePositions(el);
    expect(after['r1']).toBe(before['r1']);
    expect(after['a1']).toBe(before['a1']);
  });

  it('a shrink alongside a filterKey change re-fits instead of staying stable (a filter change)', async () => {
    const view = el as unknown as { didAutoFit: boolean };
    view.didAutoFit = true;

    el.filterKey = 'stopped'; // e.g. the host's phase filter changed
    el.agents = el.agents.filter((a) => a.id !== 'a2');
    await el.updateComplete;

    expect(view.didAutoFit).toBe(false);
  });

  it('a filterKey change that hits the layout cache does not leave it stale for the next delete', async () => {
    // Change filterKey alone (e.g. "All" -> "Running" when every agent is
    // already running): the topology signature is unaffected by filterKey,
    // so with the agents array unchanged this is a cache *hit*, which must
    // still refresh the cached filterKey. Otherwise the next render (a real
    // delete, filterKey unchanged from here on) would see a stale
    // previous.filterKey mismatch and reflow the whole graph instead of
    // staying on the stable path.
    el.filterKey = 'stopped'; // beforeEach already set 'running'; this is the change
    await el.updateComplete;
    const before = nodePositions(el);

    el.agents = el.agents.filter((a) => a.id !== 'a2'); // a leaf delete, filterKey unchanged since
    await el.updateComplete;

    const after = nodePositions(el);
    expect(after['r1']).toBe(before['r1']);
    expect(after['a1']).toBe(before['a1']);
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
    el = document.createElement('scion-agent-tree-view');
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

describe('hover/relatedIds highlighting', () => {
  let el: ScionAgentTreeView;

  /** The `.node-wrapper` an agent's hover handlers are bound to. */
  function wrapperOf(agentId: string): Element {
    const link = el.shadowRoot!.querySelector(`a.node[href="/agents/${agentId}"]`);
    expect(link).not.toBeNull();
    const wrapper = link!.closest('.node-wrapper');
    expect(wrapper).not.toBeNull();
    return wrapper!;
  }

  /**
   * Dispatches the real pointerenter the node-wrapper's handler listens for
   * (agent-tree-view.ts, renderNode's `.node-wrapper`), instead of writing
   * the private `hoverId` state through a cast.
   *
   * No `bubbles`/`composed` init: pointerenter/pointerleave don't bubble
   * natively, and the dispatch target is already inside the shadow root, so
   * the component's own listener fires regardless. `.canvas` has its own
   * `pointerleave` handler that also clears `hoverId` — forcing `bubbles:
   * true` on a leave event would reach it too and mask a broken
   * node-wrapper handler (confirmed by mutation testing: it hid a no-op
   * node-wrapper pointerleave until this was fixed).
   */
  function hoverAgent(agentId: string): void {
    wrapperOf(agentId).dispatchEvent(new PointerEvent('pointerenter'));
  }

  /** Dispatches the real pointerleave the node-wrapper's handler listens for. */
  function leaveAgent(agentId: string): void {
    wrapperOf(agentId).dispatchEvent(new PointerEvent('pointerleave'));
  }

  /**
   * Asserts the node exists before reading its class: a renamed href/class,
   * or a node that silently failed to render, must fail loudly here instead
   * of reading back a vacuous "not dim".
   */
  function isDim(agentId: string): boolean {
    const link = el.shadowRoot!.querySelector(`a.node[href="/agents/${agentId}"]`);
    expect(link).not.toBeNull();
    return link!.classList.contains('dim');
  }

  /**
   * Identifies one specific edge's rendered <path>, rather than only
   * counting lit/dim edges in aggregate, by independently computing that
   * edge's endpoints with lineage.ts's own pure layout functions — the same
   * ones the component calls — and asking the component's own (private)
   * `edgePath` to turn them into the exact "d" string, rather than
   * duplicating that formula here: a cosmetic change to the curve shape
   * only breaks this helper if the rendered path's "d" actually differs.
   * Two edges sharing a parent share the same start point, so matching on
   * the *full* path — not just its "M x y" prefix — is what disambiguates
   * siblings. Valid only for the vertical, no-users, nothing-collapsed
   * fixtures used below: `layoutForest` doesn't account for showUsers,
   * collapse or horizontal orientation.
   */
  function edgeClasses(agents: Agent[], parentId: string, childId: string): string[] {
    const layout = layoutForest(buildLineageForest(agents));
    const edge = layout.edges.find((e) => e.parentId === parentId && e.childId === childId);
    expect(edge, `no computed edge ${parentId}->${childId}`).toBeTruthy();
    const d = (el as unknown as { edgePath(e: PositionedEdge): string }).edgePath(edge!);
    const path = Array.from(el.shadowRoot!.querySelectorAll('svg path.edge')).find(
      (p) => p.getAttribute('d') === d
    );
    expect(path, `no rendered edge ${parentId}->${childId} (d="${d}")`).toBeTruthy();
    return Array.from(path!.classList);
  }

  /**
   * Identifies one specific user node, rather than matching the incidental
   * `title` display text, by independently computing its exact rendered
   * position with lineage.ts's own pure layout functions and matching the
   * DOM node's `style` attribute to it — the same technique edgeClasses
   * uses for edges. Valid only for the vertical, nothing-collapsed fixtures
   * used below: `layoutForestWithUsers` doesn't account for collapse or
   * horizontal orientation. Call this again after any re-render rather than
   * holding on to the returned element, because Lit may replace it.
   */
  function findUserNode(agents: Agent[], userId: string): Element {
    const layout = layoutForestWithUsers(buildLineageForest(agents));
    const user = layout.users.find((u) => u.id === userId);
    expect(user, `no computed user node for ${userId}`).toBeTruthy();
    const node = Array.from(el.shadowRoot!.querySelectorAll('.node.user')).find((n) => {
      const htmlEl = n as HTMLElement;
      return htmlEl.style.left === `${user!.px}px` && htmlEl.style.top === `${user!.py}px`;
    });
    expect(node, `no rendered user node for ${userId}`).toBeTruthy();
    return node!;
  }

  afterEach(() => {
    vi.restoreAllMocks();
    el.remove();
    document.body.innerHTML = '';
  });

  describe('ancestors and descendants, multiple levels deep', () => {
    // a -> b -> c -> d, plus an unrelated root z. At least 3 levels, so a
    // one-step ancestor walk or a one-level BFS (instead of relatedIds'
    // actual multi-level walk/BFS) still gets caught — a 1-level fixture
    // can't tell the two apart.
    const fixture: Agent[] = [
      agent('a', 'a', ['user-1']),
      agent('b', 'b', ['user-1', 'a']),
      agent('c', 'c', ['user-1', 'a', 'b']),
      agent('d', 'd', ['user-1', 'a', 'b', 'c']),
      agent('z', 'z', ['user-2']),
    ];

    beforeEach(async () => {
      el = document.createElement('scion-agent-tree-view');
      el.agents = fixture;
      document.body.appendChild(el);
      await el.updateComplete;
    });

    it('hovering a middle node (b) reaches both of its descendant levels (c and d)', async () => {
      hoverAgent('b');
      await el.updateComplete;

      expect(isDim('a')).toBe(false); // ancestor
      expect(isDim('b')).toBe(false); // hovered
      expect(isDim('c')).toBe(false); // child
      expect(isDim('d')).toBe(false); // grandchild — a 1-level BFS would miss this
      expect(isDim('z')).toBe(true); // unrelated

      expect(edgeClasses(fixture, 'b', 'c')).toContain('lit');
      expect(edgeClasses(fixture, 'c', 'd')).toContain('lit');
    });

    it('hovering a deep node (c) reaches both of its ancestor levels (b and a)', async () => {
      hoverAgent('c');
      await el.updateComplete;

      expect(isDim('a')).toBe(false); // grandparent — a 1-step ancestor walk would miss this
      expect(isDim('b')).toBe(false); // parent
      expect(isDim('c')).toBe(false); // hovered
      expect(isDim('d')).toBe(false); // child
      expect(isDim('z')).toBe(true); // unrelated

      expect(edgeClasses(fixture, 'a', 'b')).toContain('lit');
      expect(edgeClasses(fixture, 'b', 'c')).toContain('lit');
    });
  });

  describe('cyclic ancestry does not infinite-loop the hover walk', () => {
    // x and y form a 2-cycle (x's parent is y, y's parent is x); k is also a
    // child of x; z is unrelated. buildLineageForest promotes x as the
    // cycle's root (lowest id; see lineage.test.ts's cyclic-ancestry tests),
    // so the rendered tree is x -> {k, y}. Hovering any cycle member must
    // terminate: relatedIds' ancestor walk breaks via
    // `related.has(parent.id)`, which a regression could drop, freezing the
    // tab on this same kind of malformed (but handled) input.
    const fixture: Agent[] = [
      agent('x', 'x', ['user-1', 'y']),
      agent('y', 'y', ['user-1', 'x']),
      agent('k', 'k', ['user-1', 'x']),
      agent('z', 'z', ['user-2']),
    ];

    beforeEach(async () => {
      el = document.createElement('scion-agent-tree-view');
      el.agents = fixture;
      document.body.appendChild(el);
      await el.updateComplete;
    });

    it('hovering a cycle member relates the whole cycle and terminates', async () => {
      // Bound relatedIds' ancestor walk instead of relying on a test
      // timeout: vitest's testTimeout cannot interrupt a *synchronous*
      // infinite loop (removing the cycle guard would freeze this worker,
      // not fail a test). Spy on getAgentById — relatedIds' own id lookup —
      // and return a copy of its real map whose `get` throws once a call
      // budget is exceeded, so a regressed guard fails this test cleanly
      // instead of hanging it.
      type AgentByIdHost = { getAgentById(a: Agent[]): Map<string, Agent> };
      const real = (Object.getPrototypeOf(el) as AgentByIdHost).getAgentById.bind(el);
      let calls = 0;
      vi.spyOn(el as unknown as AgentByIdHost, 'getAgentById').mockImplementation(function (
        this: unknown,
        a: Agent[]
      ) {
        const m = real(a);
        const bounded = new Map(m);
        bounded.get = (k: string): Agent | undefined => {
          if (++calls > 1000) throw new Error('relatedIds ancestor walk did not terminate');
          return m.get(k);
        };
        return bounded;
      });

      hoverAgent('x');
      await el.updateComplete;

      expect(isDim('x')).toBe(false);
      expect(isDim('y')).toBe(false);
      expect(isDim('k')).toBe(false);
      expect(isDim('z')).toBe(true);

      expect(edgeClasses(fixture, 'x', 'y')).toContain('lit');
      expect(edgeClasses(fixture, 'x', 'k')).toContain('lit');
    });
  });

  describe('single-level highlighting, sibling, missing parent, pointerleave, and no hover', () => {
    // r1 (root) -> m1 -> l1 (m1's descendant); s1 is m1's *sibling* (another
    // child of r1), not related to m1. orphan's parent id ('ghost') does not
    // exist in the set — the missing-parent case: the ancestor walk must
    // stop cleanly there instead of throwing or treating it as related.
    const fixture: Agent[] = [
      agent('r1', 'root', ['user-1']),
      agent('m1', 'mid', ['user-1', 'r1']),
      agent('l1', 'leaf', ['user-1', 'r1', 'm1']),
      agent('s1', 'sibling', ['user-1', 'r1']),
      agent('orphan', 'orphan', ['user-1', 'ghost']),
    ];

    beforeEach(async () => {
      el = document.createElement('scion-agent-tree-view');
      el.agents = fixture;
      document.body.appendChild(el);
      await el.updateComplete;
    });

    it('lights the hovered node, its ancestor and its descendant; dims the sibling and the unrelated orphan', async () => {
      hoverAgent('m1');
      await el.updateComplete;

      expect(isDim('m1')).toBe(false);
      expect(isDim('r1')).toBe(false);
      expect(isDim('l1')).toBe(false);
      expect(isDim('s1')).toBe(true); // sibling: neither ancestor nor descendant
      expect(isDim('orphan')).toBe(true); // unrelated

      expect(edgeClasses(fixture, 'r1', 'm1')).toContain('lit');
      expect(edgeClasses(fixture, 'm1', 'l1')).toContain('lit');
      expect(edgeClasses(fixture, 'r1', 's1')).toContain('dim'); // not just "not lit"
    });

    it('stops cleanly at a missing parent: hovering the orphan relates only itself', async () => {
      hoverAgent('orphan');
      await el.updateComplete;

      expect(isDim('orphan')).toBe(false); // the hovered node is never dimmed
      // The orphan's parent ('ghost') doesn't exist, so the ancestor walk
      // stops immediately; nothing points to the orphan as a parent, so the
      // descendant BFS finds nothing either. Everything else is unrelated.
      expect(isDim('r1')).toBe(true);
      expect(isDim('m1')).toBe(true);
      expect(isDim('l1')).toBe(true);
      expect(isDim('s1')).toBe(true);
      // Nothing but the orphan is related, so every edge among the other
      // nodes is dimmed.
      for (const [parentId, childId] of [
        ['r1', 'm1'],
        ['m1', 'l1'],
        ['r1', 's1'],
      ] as const) {
        expect(edgeClasses(fixture, parentId, childId)).toContain('dim');
      }
    });

    it('dims nothing when no node is hovered', () => {
      expect(isDim('r1')).toBe(false);
      expect(isDim('m1')).toBe(false);
      expect(isDim('orphan')).toBe(false);
      for (const [parentId, childId] of [
        ['r1', 'm1'],
        ['m1', 'l1'],
        ['r1', 's1'],
      ] as const) {
        const classes = edgeClasses(fixture, parentId, childId);
        expect(classes).not.toContain('lit');
        expect(classes).not.toContain('dim');
      }
    });

    it('undims everything again after the pointer leaves the hovered node', async () => {
      hoverAgent('m1');
      await el.updateComplete;
      expect(isDim('s1')).toBe(true); // sanity: the hover above took effect
      expect(isDim('orphan')).toBe(true);

      leaveAgent('m1');
      await el.updateComplete;

      expect(isDim('s1')).toBe(false);
      expect(isDim('m1')).toBe(false);
      expect(isDim('r1')).toBe(false);
      expect(isDim('l1')).toBe(false);
      expect(isDim('orphan')).toBe(false);
    });
  });

  describe('user-hover branch', () => {
    // Two separate root users, one root agent each, so there is exactly one
    // rendered user node per user.
    const fixture: Agent[] = [
      { ...agent('p1', 'p1', ['user-1']), createdBy: 'alice' } as Agent,
      { ...agent('p2', 'p2', ['user-2']), createdBy: 'bob' } as Agent,
    ];

    beforeEach(async () => {
      el = document.createElement('scion-agent-tree-view');
      el.agents = fixture;
      document.body.appendChild(el);
      await el.updateComplete;
      // showUsers defaults from localStorage (false here) in
      // connectedCallback, which already ran by the time the element is in
      // the DOM; set it after, as the layout-cache describe block above does.
      (el as unknown as { showUsers: boolean }).showUsers = true;
      await el.updateComplete;
    });

    it('hovering a user node lights every agent whose lineage starts with that user, dims the rest, and dims the other user node', async () => {
      expect(findUserNode(fixture, 'user-1').classList.contains('dim')).toBe(false); // no hover yet
      expect(findUserNode(fixture, 'user-2').classList.contains('dim')).toBe(false);

      findUserNode(fixture, 'user-1').dispatchEvent(new PointerEvent('pointerenter'));
      await el.updateComplete;

      expect(isDim('p1')).toBe(false); // alice's agent
      expect(isDim('p2')).toBe(true); // bob's agent
      // The user branch's most direct observable: the user nodes themselves.
      // Called again after the hover rather than reusing the elements found
      // above, because Lit may have replaced them across the re-render.
      expect(findUserNode(fixture, 'user-1').classList.contains('dim')).toBe(false);
      expect(findUserNode(fixture, 'user-2').classList.contains('dim')).toBe(true);
    });

    it('hovering an agent also lights its own root user node, leaving other users dim', async () => {
      // relatedIds adds userKey(rootUser) on an *agent* hover (not just the
      // user-hover branch above), so p1's root user node should light up too.
      hoverAgent('p1');
      await el.updateComplete;

      expect(findUserNode(fixture, 'user-1').classList.contains('dim')).toBe(false); // p1's root user
      expect(findUserNode(fixture, 'user-2').classList.contains('dim')).toBe(true);
    });

    it('leaving a hovered user node clears the highlight it set', async () => {
      findUserNode(fixture, 'user-1').dispatchEvent(new PointerEvent('pointerenter'));
      await el.updateComplete;
      expect(isDim('p2')).toBe(true); // sanity: the hover above took effect

      findUserNode(fixture, 'user-1').dispatchEvent(new PointerEvent('pointerleave'));
      await el.updateComplete;

      expect(isDim('p2')).toBe(false);
      expect(findUserNode(fixture, 'user-2').classList.contains('dim')).toBe(false);
    });
  });
});

describe('scion-agent-tree-view revealAgent and focusAgentNode', () => {
  let el: ScionAgentTreeView;

  interface Internals {
    scale: number;
    panX: number;
    panY: number;
    didAutoFit: boolean;
    collapsedIds: ReadonlySet<string>;
    pendingRevealId: string | null;
    highlightId: string | null;
    layoutCache: { layout: ForestLayout } | null;
  }
  const internals = (): Internals => el as unknown as Internals;

  const CANVAS_W = 800;
  const CANVAS_H = 600;
  let canvasSize = { width: CANVAS_W, height: CANVAS_H };

  function stubCanvas(): void {
    const canvas = el.shadowRoot!.querySelector<HTMLElement>('.canvas')!;
    canvas.getBoundingClientRect = (): DOMRect =>
      ({
        left: 0,
        top: 0,
        x: 0,
        y: 0,
        ...canvasSize,
        right: canvasSize.width,
        bottom: canvasSize.height,
      }) as DOMRect;
  }

  function nextFrame(): Promise<void> {
    return new Promise((resolve) => requestAnimationFrame(() => resolve()));
  }

  /** Lets the update and both frames it schedules (auto-fit, focus) run. */
  async function settle(): Promise<void> {
    await el.updateComplete;
    stubCanvas();
    await nextFrame();
    await nextFrame();
    await el.updateComplete;
  }

  function node(id: string): HTMLElement | null {
    return el.shadowRoot!.querySelector<HTMLElement>(`a.node[data-agent-id="${id}"]`);
  }

  function positioned(id: string): { px: number; py: number } {
    const n = internals().layoutCache!.layout.nodes.find((p) => p.agent.id === id);
    if (!n) throw new Error(`${id} is not laid out`);
    return n;
  }

  function expectCenteredOn(id: string, scale: number): void {
    const { px, py } = positioned(id);
    expect(internals().scale).toBe(scale);
    expect(internals().panX).toBeCloseTo(CANVAS_W / 2 - (px + NODE_W / 2) * scale);
    expect(internals().panY).toBeCloseTo(CANVAS_H / 2 - (py + NODE_H / 2) * scale);
    const stage = el.shadowRoot!.querySelector<HTMLElement>('.stage')!;
    expect(stage.getAttribute('style')).toContain(
      `translate(${internals().panX}px, ${internals().panY}px) scale(${scale})`
    );
  }

  beforeEach(async () => {
    canvasSize = { width: CANVAS_W, height: CANVAS_H };
    el = document.createElement('scion-agent-tree-view');
    el.agents = [
      agent('r1', 'root', ['user-1']),
      agent('k1', 'kid', ['user-1', 'r1']),
      agent('g1', 'grandkid', ['user-1', 'r1', 'k1']),
      agent('r2', 'other root', ['user-1']),
      agent('k2', 'other kid', ['user-1', 'r2']),
    ];
    document.body.appendChild(el);
    await settle();
  });

  afterEach(() => {
    vi.useRealTimers();
    el.remove();
    document.body.innerHTML = '';
  });

  it('returns false and changes nothing for an id that is not in the graph', async () => {
    internals().scale = 1.5;
    internals().panX = 12;
    internals().panY = 34;
    await el.updateComplete;

    expect(el.revealAgent('missing')).toBe(false);
    await settle();

    expect(internals().scale).toBe(1.5);
    expect(internals().panX).toBe(12);
    expect(internals().panY).toBe(34);
    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();
  });

  it('centers on the node at the current zoom instead of resetting it', async () => {
    internals().scale = 1.5;
    await el.updateComplete;

    expect(el.revealAgent('k2')).toBe(true);
    await settle();

    expectCenteredOn('k2', 1.5);
  });

  it('expands collapsed ancestors so the node is laid out, leaving other collapses alone', async () => {
    internals().collapsedIds = new Set(['r1', 'k1', 'r2']);
    await el.updateComplete;
    expect(node('g1')).toBeNull();

    expect(el.revealAgent('g1')).toBe(true);
    await settle();

    expect([...internals().collapsedIds]).toEqual(['r2']);
    expect(node('g1')).not.toBeNull();
    expectCenteredOn('g1', internals().scale);
  });

  it('highlights the node briefly and leaves keyboard focus alone', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    el.revealAgent('k1');
    await settle();

    expect(node('k1')!.classList.contains('jump-highlight')).toBe(true);
    expect(el.shadowRoot!.querySelectorAll('.jump-highlight')).toHaveLength(1);
    expect(el.shadowRoot!.activeElement).toBeNull();

    vi.advanceTimersByTime(2000);
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();
  });

  it('moves the highlight to the latest jump', async () => {
    el.revealAgent('k1');
    await settle();
    el.revealAgent('r2');
    await settle();

    expect(node('k1')!.classList.contains('jump-highlight')).toBe(false);
    expect(node('r2')!.classList.contains('jump-highlight')).toBe(true);
    expectCenteredOn('r2', internals().scale);
  });

  it('is not overridden by an initial fit that is still pending', async () => {
    internals().didAutoFit = false;
    el.revealAgent('k2');
    await settle();

    const scale = internals().scale;
    expectCenteredOn('k2', scale);

    // A later render must not run the initial fit over the centering either.
    internals().scale = 1.5;
    el.requestUpdate();
    await settle();
    expect(internals().scale).toBe(1.5);
  });

  it("leaves the target's own collapse alone", async () => {
    internals().collapsedIds = new Set(['k1']);
    await el.updateComplete;

    expect(el.revealAgent('k1')).toBe(true);
    await settle();

    expect([...internals().collapsedIds]).toEqual(['k1']);
    expect(node('g1')).toBeNull();
    expectCenteredOn('k1', internals().scale);
  });

  it('stops expanding at a cycle in the ancestry', async () => {
    el.agents = [
      agent('x2', 'loop a', ['user-1', 'x3']),
      agent('x3', 'loop b', ['user-1', 'x2']),
      agent('x1', 'leaf', ['user-1', 'x2']),
    ];
    internals().collapsedIds = new Set(['x2', 'x3']);
    await el.updateComplete;

    expect(el.revealAgent('x1')).toBe(true);
    expect(internals().collapsedIds.size).toBe(0);
  });

  it('restarts the highlight timer when the same agent is picked again', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    el.revealAgent('k1');
    await settle();
    vi.advanceTimersByTime(1500);

    el.revealAgent('k1');
    await settle();
    vi.advanceTimersByTime(1000);
    await el.updateComplete;
    expect(node('k1')!.classList.contains('jump-highlight')).toBe(true);

    vi.advanceTimersByTime(1000);
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();
  });

  it('focusAgentNode focuses the node without letting the browser scroll it into view', () => {
    const focus = vi.spyOn(HTMLElement.prototype, 'focus');

    expect(el.focusAgentNode('k1')).toBe(true);

    expect(el.shadowRoot!.activeElement).toBe(node('k1'));
    const call = focus.mock.contexts.findIndex((ctx) => ctx === node('k1'));
    expect(call).toBeGreaterThanOrEqual(0);
    expect(focus.mock.calls[call]).toEqual([{ preventScroll: true }]);
  });

  it('focusAgentNode returns false for a node that is not rendered', async () => {
    internals().collapsedIds = new Set(['k1']);
    await el.updateComplete;

    expect(el.focusAgentNode('g1')).toBe(false);
    expect(el.focusAgentNode('missing')).toBe(false);
    expect(el.shadowRoot!.activeElement).toBeNull();
  });

  it('drops a jump whose agent leaves the graph before it is applied', async () => {
    internals().scale = 1.25;
    internals().panX = 10;
    internals().panY = 20;
    await el.updateComplete;
    const agents = el.agents;

    el.revealAgent('k2');
    el.agents = agents.filter((a) => a.id !== 'k2');
    await settle();
    expect(internals().pendingRevealId).toBeNull();

    el.agents = agents;
    await settle();
    expect(internals().panX).toBe(10);
    expect(internals().panY).toBe(20);
  });

  it('on disconnect drops the pending jump and the highlight, and frees its frame and timer', async () => {
    vi.useFakeTimers({
      toFake: ['setTimeout', 'clearTimeout', 'requestAnimationFrame', 'cancelAnimationFrame'],
    });
    canvasSize = { width: 0, height: 0 };
    internals().scale = 1.25;
    internals().panX = 10;
    internals().panY = 20;
    el.revealAgent('k1');
    await el.updateComplete;
    expect(vi.getTimerCount()).toBeGreaterThan(0);

    el.remove();
    expect(vi.getTimerCount()).toBe(0);
    expect(internals().pendingRevealId).toBeNull();

    canvasSize = { width: CANVAS_W, height: CANVAS_H };
    document.body.appendChild(el);
    await el.updateComplete;
    stubCanvas();
    vi.advanceTimersByTime(100);
    await el.updateComplete;

    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();
    expect(internals().scale).toBe(1.25);
    expect(internals().panX).toBe(10);
    expect(internals().panY).toBe(20);
  });

  it('on disconnect clears a highlight that is showing, so a reconnect shows none', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    el.revealAgent('k1');
    await settle();
    expect(node('k1')!.classList.contains('jump-highlight')).toBe(true);

    el.remove();
    expect(internals().highlightId).toBeNull();
    expect(vi.getTimerCount()).toBe(0);

    document.body.appendChild(el);
    await settle();
    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();
  });

  it('waits for the canvas to have a size, then centers on the next render', async () => {
    canvasSize = { width: 0, height: 0 };
    internals().scale = 1.25;
    el.revealAgent('k2');
    await settle();
    expect(internals().pendingRevealId).toBe('k2');

    canvasSize = { width: CANVAS_W, height: CANVAS_H };
    el.requestUpdate();
    await settle();

    expect(internals().pendingRevealId).toBeNull();
    expectCenteredOn('k2', 1.25);
  });

  it('starts the highlight once the node is centered, not when the reveal is asked for', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    canvasSize = { width: 0, height: 0 };
    el.revealAgent('k2');
    await settle();
    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();

    vi.advanceTimersByTime(500);
    canvasSize = { width: CANVAS_W, height: CANVAS_H };
    el.requestUpdate();
    await settle();
    expectCenteredOn('k2', internals().scale);
    expect(node('k2')!.classList.contains('jump-highlight')).toBe(true);

    vi.advanceTimersByTime(1900);
    await el.updateComplete;
    expect(node('k2')!.classList.contains('jump-highlight')).toBe(true);
    vi.advanceTimersByTime(100);
    await el.updateComplete;
    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();
  });

  it('gives up on a reveal whose canvas has no size for a second', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    canvasSize = { width: 0, height: 0 };
    internals().scale = 1.25;
    internals().panX = 10;
    internals().panY = 20;
    el.revealAgent('k2');
    await settle();
    vi.advanceTimersByTime(999);
    expect(internals().pendingRevealId).toBe('k2');
    vi.advanceTimersByTime(1);
    expect(internals().pendingRevealId).toBeNull();

    canvasSize = { width: CANVAS_W, height: CANVAS_H };
    el.requestUpdate();
    await settle();
    expect(internals().scale).toBe(1.25);
    expect(internals().panX).toBe(10);
    expect(internals().panY).toBe(20);
    expect(el.shadowRoot!.querySelector('.jump-highlight')).toBeNull();
  });
});

// ptone/scion#765: dragging the canvas to pan must not start a text
// selection, while text stays selectable when no pan is in progress.
describe('scion-agent-tree-view drag-to-pan suppresses text selection', () => {
  let el: ScionAgentTreeView;

  beforeEach(async () => {
    el = document.createElement('scion-agent-tree-view');
    el.agents = [agent('r1', 'root', ['user-1']), agent('k1', 'kid', ['user-1', 'r1'])];
    document.body.appendChild(el);
    await el.updateComplete;
  });

  afterEach(() => {
    el.remove();
    document.body.innerHTML = '';
  });

  function canvas(): HTMLElement {
    return el.shadowRoot!.querySelector('.canvas')!;
  }

  function pointer(type: string, target: EventTarget = canvas()): void {
    target.dispatchEvent(
      new PointerEvent(type, { bubbles: true, composed: true, cancelable: true, pointerId: 1 })
    );
  }

  /** Dispatches a selectstart on `target` and returns whether it was prevented. */
  // Not composed, as browsers fire it: a selectstart on shadow-root text
  // never reaches document, so the component must listen on its render root.
  function selectStartPrevented(target: EventTarget): boolean {
    const ev = new Event('selectstart', { bubbles: true, composed: false, cancelable: true });
    target.dispatchEvent(ev);
    return ev.defaultPrevented;
  }

  /** Some text node inside the canvas (a node label), where a selection would begin. */
  function textTarget(): Node {
    const walker = document.createTreeWalker(canvas(), NodeFilter.SHOW_TEXT);
    let n: Node | null;
    while ((n = walker.nextNode())) {
      if (n.textContent?.trim()) return n;
    }
    return canvas();
  }

  it('prevents selectstart during an active pan', () => {
    pointer('pointerdown');
    expect(canvas().classList.contains('dragging')).toBe(true);
    expect(selectStartPrevented(textTarget())).toBe(true);
    // Also outside the canvas while the gesture is active.
    expect(selectStartPrevented(document.body)).toBe(true);
    pointer('pointerup');
  });

  it('does not prevent selectstart when no pan is active', () => {
    expect(selectStartPrevented(textTarget())).toBe(false);

    pointer('pointerdown');
    pointer('pointerup');
    expect(canvas().classList.contains('dragging')).toBe(false);
    expect(selectStartPrevented(textTarget())).toBe(false);
    expect(selectStartPrevented(document.body)).toBe(false);
  });

  it('stops suppressing selection after pointercancel and after disconnect mid-pan', () => {
    pointer('pointerdown');
    pointer('pointercancel');
    expect(selectStartPrevented(textTarget())).toBe(false);

    pointer('pointerdown');
    el.remove();
    expect(selectStartPrevented(document.body)).toBe(false);
  });

  it('clears a selection that started during the pan on pointerup', () => {
    // Mocked as Chromium reports a selection of shadow-root text: type
    // 'Range' but isCollapsed true (window.getSelection() is retargeted to
    // the host), so the check must key off type, not isCollapsed.
    const removeAllRanges = vi.fn();
    const sel: { type: string; isCollapsed: boolean; removeAllRanges: () => void } = {
      type: 'Caret',
      isCollapsed: true,
      removeAllRanges,
    };
    const spy = vi.spyOn(window, 'getSelection').mockReturnValue(sel as unknown as Selection);
    try {
      pointer('pointerdown');
      sel.type = 'Range'; // a selection slipped through mid-gesture
      pointer('pointerup');
      expect(removeAllRanges).toHaveBeenCalledTimes(1);
    } finally {
      spy.mockRestore();
    }
  });

  it('leaves a selection that existed before the pan, and one made without panning', () => {
    const removeAllRanges = vi.fn();
    const sel = { type: 'Range', isCollapsed: true, removeAllRanges };
    const spy = vi.spyOn(window, 'getSelection').mockReturnValue(sel as unknown as Selection);
    try {
      pointer('pointerdown');
      pointer('pointerup');
      // pointerup with no pan in progress (e.g. after a click on a link).
      pointer('pointerup');
      expect(removeAllRanges).not.toHaveBeenCalled();
    } finally {
      spy.mockRestore();
    }
  });

  it('removes the selectstart listeners from both the render root and document when the pan ends', () => {
    const rootRemove = vi.spyOn(el.renderRoot, 'removeEventListener');
    const docRemove = vi.spyOn(document, 'removeEventListener');
    try {
      pointer('pointerdown');
      pointer('pointerup');
      const removedSelectStart = (spy: typeof rootRemove): boolean =>
        spy.mock.calls.some(([type, , opts]) => type === 'selectstart' && opts === true);
      expect(removedSelectStart(rootRemove)).toBe(true);
      expect(removedSelectStart(docRemove)).toBe(true);
    } finally {
      rootRemove.mockRestore();
      docRemove.mockRestore();
    }
  });

  it('ends the pan and stops suppressing selection when pointer capture is lost', () => {
    pointer('pointerdown');
    expect(selectStartPrevented(document.body)).toBe(true);
    pointer('lostpointercapture');
    expect(canvas().classList.contains('dragging')).toBe(false);
    expect(selectStartPrevented(document.body)).toBe(false);
  });

  it('does not clear a selection on a pointerup with no pan in progress', () => {
    // Fresh element: no earlier pan has set hadSelectionAtPanStart, so only
    // the dragging guard in onPointerUp keeps the selection (e.g. a click on
    // a node link, whose pointerup still bubbles to the canvas).
    const removeAllRanges = vi.fn();
    const sel = { type: 'Range', isCollapsed: true, removeAllRanges };
    const spy = vi.spyOn(window, 'getSelection').mockReturnValue(sel as unknown as Selection);
    try {
      pointer('pointerup', el.shadowRoot!.querySelector('.canvas a')!);
      pointer('pointerup');
      expect(removeAllRanges).not.toHaveBeenCalled();
    } finally {
      spy.mockRestore();
    }
  });

  it('does not start a pan (or suppress selection) from a link or button', () => {
    // Node cards are links, so a pointerdown on a node label never pans and a
    // double-click there can still select the label text.
    const link = el.shadowRoot!.querySelector('.canvas a');
    expect(link).not.toBeNull();
    pointer('pointerdown', link!);
    expect(canvas().classList.contains('dragging')).toBe(false);
    expect(selectStartPrevented(textTarget())).toBe(false);
  });
});
