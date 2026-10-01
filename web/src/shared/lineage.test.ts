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

import { describe, it, expect } from 'vitest';
import {
  buildLineageForest,
  computeStableLayout,
  descendantCounts,
  detectPureRemoval,
  edgeEndpoints,
  layoutForest,
  layoutForestWithUsers,
  parentIdOf,
  pruneCollapsed,
  rootUserOf,
  topologySignature,
  transposeLayout,
  userKey,
  NODE_W,
  NODE_H,
  GAP_X,
  GAP_Y,
  H_GAP_X,
  H_GAP_Y,
  PAD,
  type ForestLayout,
  type LineageNode,
  type Orientation,
} from './lineage.js';
import type { Agent } from './types.js';

/** Minimal agent factory for lineage tests */
function agent(id: string, name: string, ancestry?: string[]): Agent {
  return { id, name, ancestry, projectId: 'p1', template: 't', phase: 'running' } as Agent;
}

describe('parentIdOf', () => {
  it('returns the last ancestry entry', () => {
    expect(parentIdOf(agent('a', 'a', ['user-1', 'root-agent']))).toBe('root-agent');
  });

  it('returns undefined for empty or missing ancestry', () => {
    expect(parentIdOf(agent('a', 'a', []))).toBeUndefined();
    expect(parentIdOf(agent('a', 'a'))).toBeUndefined();
  });
});

describe('buildLineageForest', () => {
  it('builds a parent/child tree from ancestry chains', () => {
    const coordinator = agent('c1', 'coordinator', ['user-1']);
    const editor = agent('e1', 'editor', ['user-1', 'c1']);
    const techlead = agent('t1', 'techlead', ['user-1', 'c1']);
    const helper = agent('h1', 'helper', ['user-1', 'c1', 't1']);

    const roots = buildLineageForest([coordinator, editor, techlead, helper]);

    expect(roots).toHaveLength(1);
    expect(roots[0].agent.id).toBe('c1');
    expect(roots[0].children.map((c) => c.agent.id).sort()).toEqual(['e1', 't1']);
    const tl = roots[0].children.find((c) => c.agent.id === 't1')!;
    expect(tl.children.map((c) => c.agent.id)).toEqual(['h1']);
    expect(tl.children[0].depth).toBe(2);
  });

  it('treats user-created agents as roots', () => {
    const a = agent('a1', 'alpha', ['user-1']);
    const b = agent('b1', 'beta', ['user-2']);
    const roots = buildLineageForest([a, b]);
    expect(roots.map((r) => r.agent.id).sort()).toEqual(['a1', 'b1']);
    expect(roots.every((r) => r.depth === 0)).toBe(true);
  });

  it('promotes agents whose parent is outside the set to roots', () => {
    const orphan = agent('o1', 'orphan', ['user-1', 'deleted-agent']);
    const roots = buildLineageForest([orphan]);
    expect(roots).toHaveLength(1);
    expect(roots[0].agent.id).toBe('o1');
  });

  it('handles cyclic ancestry without hanging', () => {
    // a claims parent b; b claims parent a — malformed but must not loop.
    const a = agent('a1', 'alpha', ['user-1', 'b1']);
    const b = agent('b1', 'beta', ['user-1', 'a1']);
    const roots = buildLineageForest([a, b]);
    const layout = layoutForest(roots);
    expect(layout.nodes).toHaveLength(2);
    const ids = layout.nodes.map((n) => n.agent.id).sort();
    expect(ids).toEqual(['a1', 'b1']);
  });

  it('ignores self-referential ancestry', () => {
    const selfRef = agent('s1', 'self', ['user-1', 's1']);
    const roots = buildLineageForest([selfRef]);
    expect(roots).toHaveLength(1);
    expect(roots[0].children).toHaveLength(0);
  });
});

describe('layoutForest', () => {
  it('positions leaves in consecutive slots and centers parents', () => {
    const parent = agent('p1', 'parent', ['user-1']);
    const left = agent('l1', 'a-left', ['user-1', 'p1']);
    const right = agent('r1', 'b-right', ['user-1', 'p1']);

    const { nodes, edges } = layoutForest(buildLineageForest([parent, left, right]));

    const byId = new Map(nodes.map((n) => [n.agent.id, n]));
    const leftX = byId.get('l1')!.px;
    const rightX = byId.get('r1')!.px;
    const parentX = byId.get('p1')!.px;

    expect(leftX).toBe(PAD);
    expect(rightX).toBe(PAD + NODE_W + GAP_X);
    expect(parentX).toBe((leftX + rightX) / 2);
    expect(edges).toHaveLength(2);
    // Edges run from the parent's bottom-center to each child's top-center.
    for (const e of edges) {
      expect(e.x1).toBe(parentX + NODE_W / 2);
      expect(e.y2).toBeGreaterThan(e.y1);
    }
  });

  it('reports canvas size that bounds all nodes', () => {
    const a = agent('a1', 'alpha', ['user-1']);
    const b = agent('b1', 'beta', ['user-1']);
    const { nodes, width, height } = layoutForest(buildLineageForest([a, b]));
    for (const n of nodes) {
      expect(n.px + NODE_W).toBeLessThanOrEqual(width);
      expect(n.py).toBeLessThan(height);
    }
  });
});

describe('descendantCounts', () => {
  it('counts transitive descendants per node', () => {
    const root = agent('r1', 'root', ['user-1']);
    const mid = agent('m1', 'mid', ['user-1', 'r1']);
    const leafA = agent('la', 'leaf-a', ['user-1', 'r1', 'm1']);
    const leafB = agent('lb', 'leaf-b', ['user-1', 'r1', 'm1']);
    const counts = descendantCounts(buildLineageForest([root, mid, leafA, leafB]));
    expect(counts.get('r1')).toBe(3);
    expect(counts.get('m1')).toBe(2);
    expect(counts.get('la')).toBe(0);
  });
});

describe('pruneCollapsed', () => {
  it('hides the subtree of a collapsed node but keeps siblings', () => {
    const root = agent('r1', 'root', ['user-1']);
    const left = agent('l1', 'a-left', ['user-1', 'r1']);
    const leftKid = agent('lk', 'left-kid', ['user-1', 'r1', 'l1']);
    const right = agent('r2', 'b-right', ['user-1', 'r1']);
    const forest = pruneCollapsed(
      buildLineageForest([root, left, leftKid, right]),
      new Set(['l1'])
    );
    const { nodes, edges } = layoutForest(forest);
    const ids = nodes.map((n) => n.agent.id).sort();
    expect(ids).toEqual(['l1', 'r1', 'r2']);
    expect(edges.some((e) => e.childId === 'lk')).toBe(false);
    expect(edges.some((e) => e.childId === 'r2')).toBe(true);
  });

  it('collapsing the root leaves only the root visible', () => {
    const root = agent('r1', 'root', ['user-1']);
    const kid = agent('k1', 'kid', ['user-1', 'r1']);
    const forest = pruneCollapsed(buildLineageForest([root, kid]), new Set(['r1']));
    const { nodes, edges } = layoutForest(forest);
    expect(nodes.map((n) => n.agent.id)).toEqual(['r1']);
    expect(edges).toHaveLength(0);
  });
});

describe('rootUserOf', () => {
  it('returns the first ancestry entry for any depth', () => {
    expect(rootUserOf(agent('a', 'a', ['user-1']))).toBe('user-1');
    expect(rootUserOf(agent('b', 'b', ['user-1', 'c1', 'p1']))).toBe('user-1');
    expect(rootUserOf(agent('c', 'c'))).toBeUndefined();
  });
});

describe('layoutForestWithUsers', () => {
  it('adds a user row above the trees and shifts agents down', () => {
    const a = agent('a1', 'alpha', ['user-1']);
    const child = agent('c1', 'child', ['user-1', 'a1']);
    const { nodes, users } = layoutForestWithUsers(buildLineageForest([a, child]));

    expect(users).toHaveLength(1);
    expect(users[0].id).toBe('user-1');
    expect(users[0].py).toBe(PAD);
    const byId = new Map(nodes.map((n) => [n.agent.id, n]));
    expect(byId.get('a1')!.py).toBe(PAD + NODE_H + GAP_Y);
    expect(byId.get('c1')!.py).toBe(PAD + 2 * (NODE_H + GAP_Y));
  });

  it('groups roots from the same user under one centered user node', () => {
    const a = agent('a1', 'alpha', ['user-1']);
    const b = agent('b1', 'beta', ['user-1']);
    const c = agent('c1', 'gamma', ['user-2']);
    const { nodes, users, edges } = layoutForestWithUsers(buildLineageForest([a, b, c]));

    expect(users.map((u) => u.id).sort()).toEqual(['user-1', 'user-2']);
    const u1 = users.find((u) => u.id === 'user-1')!;
    const byId = new Map(nodes.map((n) => [n.agent.id, n]));
    expect(u1.px).toBe((byId.get('a1')!.px + byId.get('b1')!.px) / 2);

    const u1Edges = edges.filter((e) => e.parentId === userKey('user-1'));
    expect(u1Edges.map((e) => e.childId).sort()).toEqual(['a1', 'b1']);
    const u2Edges = edges.filter((e) => e.parentId === userKey('user-2'));
    expect(u2Edges.map((e) => e.childId)).toEqual(['c1']);
  });

  it('keeps agent-to-agent edges alongside user edges', () => {
    const root = agent('r1', 'root', ['user-1']);
    const child = agent('k1', 'kid', ['user-1', 'r1']);
    const { edges } = layoutForestWithUsers(buildLineageForest([root, child]));
    expect(edges.some((e) => e.parentId === 'r1' && e.childId === 'k1')).toBe(true);
    expect(edges.some((e) => e.parentId === userKey('user-1') && e.childId === 'r1')).toBe(true);
  });

  it('leaves roots without ancestry parentless but positioned', () => {
    const known = agent('a1', 'alpha', ['user-1']);
    const unknown = agent('u1', 'mystery');
    const { nodes, users, edges } = layoutForestWithUsers(buildLineageForest([known, unknown]));
    expect(users.map((u) => u.id)).toEqual(['user-1']);
    expect(nodes.map((n) => n.agent.id).sort()).toEqual(['a1', 'u1']);
    expect(edges.filter((e) => e.childId === 'u1')).toHaveLength(0);
  });
});

describe('transposeLayout', () => {
  it('maps depth to columns and leaf slots to rows', () => {
    const parent = agent('p1', 'parent', ['user-1']);
    const left = agent('l1', 'a-left', ['user-1', 'p1']);
    const right = agent('r1', 'b-right', ['user-1', 'p1']);

    const layout = transposeLayout(layoutForest(buildLineageForest([parent, left, right])));
    const byId = new Map(layout.nodes.map((n) => [n.agent.id, n]));

    // Depth → x: root in the leftmost column, children one column right.
    expect(byId.get('p1')!.px).toBe(PAD);
    expect(byId.get('l1')!.px).toBe(PAD + NODE_W + H_GAP_X);
    expect(byId.get('r1')!.px).toBe(PAD + NODE_W + H_GAP_X);

    // Leaf slot → y: siblings stack, parent vertically centered between them.
    expect(byId.get('l1')!.py).toBe(PAD);
    expect(byId.get('r1')!.py).toBe(PAD + NODE_H + H_GAP_Y);
    expect(byId.get('p1')!.py).toBe((byId.get('l1')!.py + byId.get('r1')!.py) / 2);
  });

  it('connects parent right-edge-center to child left-edge-center', () => {
    const parent = agent('p1', 'parent', ['user-1']);
    const left = agent('l1', 'a-left', ['user-1', 'p1']);
    const right = agent('r1', 'b-right', ['user-1', 'p1']);

    const layout = transposeLayout(layoutForest(buildLineageForest([parent, left, right])));
    const byId = new Map(layout.nodes.map((n) => [n.agent.id, n]));

    expect(layout.edges).toHaveLength(2);
    expect(layout.edges.map((e) => `${e.parentId}->${e.childId}`).sort()).toEqual([
      'p1->l1',
      'p1->r1',
    ]);
    for (const e of layout.edges) {
      const p = byId.get(e.parentId)!;
      const c = byId.get(e.childId)!;
      expect(e.x1).toBe(p.px + NODE_W);
      expect(e.y1).toBe(p.py + NODE_H / 2);
      expect(e.x2).toBe(c.px);
      expect(e.y2).toBe(c.py + NODE_H / 2);
      expect(e.x2).toBeGreaterThan(e.x1);
    }
  });

  it('reports canvas size that bounds all nodes', () => {
    const root = agent('r1', 'root', ['user-1']);
    const mid = agent('m1', 'mid', ['user-1', 'r1']);
    const leafA = agent('la', 'leaf-a', ['user-1', 'r1', 'm1']);
    const leafB = agent('lb', 'leaf-b', ['user-1', 'r1', 'm1']);
    const solo = agent('s1', 'solo', ['user-2']);

    const layout = transposeLayout(
      layoutForest(buildLineageForest([root, mid, leafA, leafB, solo]))
    );
    for (const n of layout.nodes) {
      expect(n.px).toBeGreaterThanOrEqual(PAD);
      expect(n.py).toBeGreaterThanOrEqual(PAD);
      expect(n.px + NODE_W).toBeLessThanOrEqual(layout.width);
      expect(n.py + NODE_H).toBeLessThanOrEqual(layout.height);
    }
    // Three depth levels → the deepest column ends exactly PAD short of width.
    expect(layout.width).toBe(PAD * 2 + 3 * NODE_W + 2 * H_GAP_X);
  });

  it('puts the user column on the left, centered across its roots', () => {
    const a = agent('a1', 'alpha', ['user-1']);
    const b = agent('b1', 'beta', ['user-1']);

    const layout = transposeLayout(layoutForestWithUsers(buildLineageForest([a, b])));
    const byId = new Map(layout.nodes.map((n) => [n.agent.id, n]));

    expect(layout.users).toHaveLength(1);
    const u = layout.users[0];
    expect(u.px).toBe(PAD);
    expect(byId.get('a1')!.px).toBe(PAD + NODE_W + H_GAP_X);
    expect(byId.get('b1')!.px).toBe(PAD + NODE_W + H_GAP_X);
    expect(u.py).toBe((byId.get('a1')!.py + byId.get('b1')!.py) / 2);

    // User → root edges leave the user card's right edge.
    const uEdges = layout.edges.filter((e) => e.parentId === userKey('user-1'));
    expect(uEdges.map((e) => e.childId).sort()).toEqual(['a1', 'b1']);
    for (const e of uEdges) {
      expect(e.x1).toBe(u.px + NODE_W);
      expect(e.y1).toBe(u.py + NODE_H / 2);
    }
  });
});

describe('topologySignature (#2388 layout cache key)', () => {
  const root = agent('r1', 'root', ['user-1']);
  const kid = agent('k1', 'kid', ['user-1', 'r1']);
  const noCollapse = new Set<string>();

  function sig(
    agents: Agent[],
    collapsed: ReadonlySet<string> = noCollapse,
    showUsers = false,
    orientation: 'vertical' | 'horizontal' = 'vertical'
  ): string {
    return topologySignature(agents, collapsed, showUsers, orientation);
  }

  it('is stable across agent-array reordering', () => {
    expect(sig([root, kid])).toBe(sig([kid, root]));
  });

  it('is unaffected by fields outside id/parentId/name (status-only updates)', () => {
    const busyRoot = { ...root, phase: 'stopped', activity: 'thinking' } as Agent;
    const busyKid = {
      ...kid,
      _capabilities: { attach: true },
      _messageability: { canMessage: false, reason: 'x' },
    } as unknown as Agent;
    expect(sig([root, kid])).toBe(sig([busyRoot, busyKid]));
  });

  it('is unaffected by object identity alone', () => {
    expect(sig([root, kid])).toBe(sig([{ ...root }, { ...kid }]));
  });

  it('changes when an agent is added', () => {
    const grandkid = agent('g1', 'grandkid', ['user-1', 'r1', 'k1']);
    expect(sig([root, kid])).not.toBe(sig([root, kid, grandkid]));
  });

  it('changes when an agent is removed', () => {
    expect(sig([root, kid])).not.toBe(sig([root]));
  });

  it('changes on reparent (ancestry change)', () => {
    const reparented = agent('k1', 'kid', ['user-1']); // now a root, not root's child
    expect(sig([root, kid])).not.toBe(sig([root, reparented]));
  });

  it('changes when the root user changes with the same direct parent (#2388 review B2)', () => {
    // layoutForestWithUsers groups roots by rootUserOf (ancestry[0]), a
    // separate input from parentIdOf (ancestry[last]). A signature keyed
    // only on the direct parent misses this: two lists below share every
    // child's direct parent ('gone', filtered out so both 'p' and 'c' are
    // roots) but disagree on which user 'c' is grouped under.
    const p = agent('p', 'p', ['u1']);
    const cUnderU1 = agent('c', 'c', ['u1', 'gone']);
    const cUnderU2 = agent('c', 'c', ['u2', 'gone']);
    expect(sig([p, cUnderU1], noCollapse, true)).not.toBe(sig([p, cUnderU2], noCollapse, true));
  });

  it('changes on rename', () => {
    const renamed = agent('k1', 'renamed-kid', ['user-1', 'r1']);
    expect(sig([root, kid])).not.toBe(sig([root, renamed]));
  });

  it('changes on collapse toggle', () => {
    expect(sig([root, kid])).not.toBe(sig([root, kid], new Set(['r1'])));
  });

  it('is unaffected by collapsedIds set insertion order', () => {
    const a = new Set(['r1', 'k1']);
    const b = new Set(['k1', 'r1']);
    expect(sig([root, kid], a)).toBe(sig([root, kid], b));
  });

  it('changes when showUsers toggles', () => {
    expect(sig([root, kid], noCollapse, false)).not.toBe(sig([root, kid], noCollapse, true));
  });

  it('changes when orientation toggles', () => {
    expect(sig([root, kid], noCollapse, false, 'vertical')).not.toBe(
      sig([root, kid], noCollapse, false, 'horizontal')
    );
  });
});

describe('name-tie ordering (#2388 review N1)', () => {
  it('breaks equal-name ties by ID, so layout position does not depend on input array order', () => {
    // topologySignature sorts by ID and is therefore order-independent. For
    // the cache to be sound, the layout it keys must be order-independent
    // too — otherwise a cache hit can draw whatever order was in effect at
    // the last invalidation instead of what a fresh compute would give.
    const a1 = agent('a1', 'worker', ['user-1']);
    const a2 = agent('a2', 'worker', ['user-1']);

    const forward = layoutForest(buildLineageForest([a1, a2]));
    const reversed = layoutForest(buildLineageForest([a2, a1]));

    const idsByX = (nodes: readonly { agent: Agent; px: number }[]) =>
      [...nodes].sort((x, y) => x.px - y.px).map((n) => n.agent.id);

    expect(idsByX(forward.nodes)).toEqual(idsByX(reversed.nodes));
    // Pin the actual order too, so this doesn't just prove "some" tie-break.
    expect(idsByX(forward.nodes)).toEqual(['a1', 'a2']);
  });
});

describe('cyclic-ancestry root promotion (#2388 review N-B)', () => {
  // Malformed cyclic ancestry: a's parent is b and b's parent is a, so
  // neither reaches a legitimate root. buildLineageForest must still
  // terminate and promote exactly one of them to a root — and which one
  // must not depend on the input array's order. topologySignature is
  // order-independent (sorted by id), so if promotion order depended on
  // input order, a cache hit could reuse a stale choice of root/layout for
  // input that produces the same signature.
  const a = agent('a', 'a', ['u', 'b']);
  const b = agent('b', 'b', ['u', 'a']);

  it('promotes the same node to root regardless of input array order', () => {
    const forwardRootIds = buildLineageForest([a, b]).map((n) => n.agent.id);
    const reversedRootIds = buildLineageForest([b, a]).map((n) => n.agent.id);

    expect(forwardRootIds).toEqual(reversedRootIds);
    expect(forwardRootIds).toEqual(['a']); // deterministic: lowest id wins
  });

  it('keeps layout identical for cyclic input regardless of array order (matches the order-independent signature)', () => {
    const noCollapse = new Set<string>();
    // The signature was already order-independent before this fix; this
    // pins that it stays true, so the two layout computations below are a
    // valid same-signature comparison.
    expect(topologySignature([a, b], noCollapse, false, 'vertical')).toBe(
      topologySignature([b, a], noCollapse, false, 'vertical')
    );

    const forward = layoutForest(buildLineageForest([a, b]));
    const reversed = layoutForest(buildLineageForest([b, a]));
    const posById = (layout: typeof forward) =>
      new Map(layout.nodes.map((n) => [n.agent.id, { px: n.px, py: n.py }]));

    expect(posById(forward)).toEqual(posById(reversed));
  });
});

describe('cycle with a non-cycle descendant (#2388 review round-3 F1)', () => {
  // x and y form a 2-cycle; child is a legitimate descendant of x, not
  // itself part of the cycle. child's id ('child') sorts before both cycle
  // members' ids ('x', 'y') — exactly the ordering that, before this fix,
  // promoted every unvisited node in plain id order rather than only actual
  // cycle members: child got promoted as its own isolated root first, and
  // the real x->child edge was silently dropped when x was promoted
  // afterward and `visit` filtered out the already-visited child.
  const x = agent('x', 'x', ['u', 'y']);
  const y = agent('y', 'y', ['u', 'x']);
  const child = agent('child', 'child', ['u', 'x']);

  function edgesOf(agents: Agent[]): string[] {
    const layout = layoutForest(buildLineageForest(agents));
    return layout.edges.map((e) => `${e.parentId}>${e.childId}`).sort();
  }

  it('keeps the real x->child edge for every input order', () => {
    const permutations = [
      [x, y, child],
      [x, child, y],
      [y, x, child],
      [y, child, x],
      [child, x, y],
      [child, y, x],
    ];
    for (const agents of permutations) {
      expect(edgesOf(agents)).toContain('x>child');

      const roots = buildLineageForest(agents);
      expect(roots).toHaveLength(1); // every agent reachable from one root
      expect(['x', 'y']).toContain(roots[0].agent.id); // never the descendant
    }
  });

  it('produces the same forest (root and edges) regardless of input order', () => {
    // child sorts first: the exact ordering that reproduced the bug pre-fix.
    const descendantFirst = edgesOf([child, x, y]);
    const cycleFirst = edgesOf([y, x, child]);

    expect(descendantFirst).toEqual(cycleFirst);
    expect(descendantFirst).toEqual(['x>child', 'x>y']); // x wins the id tie-break
  });
});

describe('order-independence across multiple cycles (#2388 review round-4 T1)', () => {
  // Two disjoint cycles with tails: b<->c (tail a->b), e<->f (tail d->f).
  // The unvisitedAscending sort fixes the order the two promoted roots are
  // appended in, and so their relative position in the layout — not which
  // member wins within each cycle (F1 already covers that).
  const a = agent('a', 'a', ['u', 'b']);
  const b = agent('b', 'b', ['u', 'c']);
  const c = agent('c', 'c', ['u', 'b']);
  const d = agent('d', 'd', ['u', 'f']);
  const e = agent('e', 'e', ['u', 'f']);
  const f = agent('f', 'f', ['u', 'e']);
  const all = [a, b, c, d, e, f];
  // Orders chosen so the tails (a, d) interleave with the cycles in
  // different relative sequences — in particular so 'd' precedes 'a' in at
  // least one order, which is what actually exercises the sort.
  const orders: Agent[][] = [all, [...all].reverse(), [d, e, f, a, b, c], [c, a, f, b, d, e]];

  it('promotes the same two roots — the lowest id in each cycle — for every input order', () => {
    for (const order of orders) {
      expect(buildLineageForest(order).map((n) => n.agent.id)).toEqual(['b', 'e']);
    }
  });

  it('produces the same layout for every input order (matches the order-independent signature)', () => {
    const noCollapse = new Set<string>();
    const signatures = orders.map((order) =>
      topologySignature(order, noCollapse, false, 'vertical')
    );
    expect(new Set(signatures).size).toBe(1); // sanity: still order-independent

    const layouts = orders.map((order) => layoutForest(buildLineageForest(order)));
    const posById = (layout: (typeof layouts)[number]) =>
      new Map(layout.nodes.map((n) => [n.agent.id, { px: n.px, py: n.py }]));

    const reference = posById(layouts[0]);
    for (const layout of layouts.slice(1)) {
      expect(posById(layout)).toEqual(reference);
    }
  });
});

describe('cycle promotion pins the lowest id, not merely the first member met while walking (#2388 review round-4 optional)', () => {
  // 3-cycle p->q->r->p with a tail attached to r (the highest, not lowest,
  // id). The tail's id sorts first, so its walk enters the cycle at r —
  // the first member *met*, but not the *lowest id*. This distinguishes
  // "promote the lowest id" from "promote cycle[0]", which the round-3
  // x/y/c test could not (that walk met the lowest-id member first).
  const tail = agent('a', 'tail', ['u', 'r']);
  const p = agent('p', 'p', ['u', 'q']);
  const q = agent('q', 'q', ['u', 'r']);
  const r = agent('r', 'r', ['u', 'p']);

  it('promotes p (the lowest id), not r (the cycle member the walk meets first)', () => {
    const roots = buildLineageForest([tail, p, q, r]).map((n) => n.agent.id);
    expect(roots).toEqual(['p']);
  });

  it('keeps every real edge', () => {
    const layout = layoutForest(buildLineageForest([tail, p, q, r]));
    const edges = layout.edges.map((e) => `${e.parentId}>${e.childId}`).sort();
    // tail's agent id is 'a', so its edge reads "r>a".
    expect(edges).toEqual(['p>r', 'r>a', 'r>q']);
  });
});

describe('detectPureRemoval', () => {
  const r1 = agent('r1', 'root-1', ['user-1']);
  const k1 = agent('k1', 'kid-1', ['user-1', 'r1']);
  const g1 = agent('g1', 'grandkid-1', ['user-1', 'r1', 'k1']);
  const r2 = agent('r2', 'root-2', ['user-2']);

  it('returns null when nothing changed', () => {
    expect(detectPureRemoval([r1, k1], [r1, k1])).toBeNull();
  });

  it('returns null when an agent was added', () => {
    expect(detectPureRemoval([r1], [r1, k1])).toBeNull();
  });

  it('returns null when a survivor was renamed', () => {
    expect(detectPureRemoval([r1, k1], [r1, { ...k1, name: 'renamed' }])).toBeNull();
  });

  it('returns null when a survivor was reparented', () => {
    const reparented = agent('g1', 'grandkid-1', ['user-1', 'r1']); // was under k1
    expect(detectPureRemoval([r1, k1, g1], [r1, k1, reparented])).toBeNull();
  });

  it('returns null when a survivor moved to a different root user', () => {
    const movedUser = agent('k1', 'kid-1', ['user-9', 'r1']);
    expect(detectPureRemoval([r1, k1], [r1, movedUser])).toBeNull();
  });

  it('reports a removed leaf with no orphans', () => {
    const result = detectPureRemoval([r1, k1, g1], [r1, k1]);
    expect(result).not.toBeNull();
    expect(result!.removedIds).toEqual(new Set(['g1']));
    expect(result!.orphanedIds).toEqual(new Set());
  });

  it('reports a removed parent and the children it orphans', () => {
    const result = detectPureRemoval([r1, k1, g1], [r1, g1]);
    expect(result).not.toBeNull();
    expect(result!.removedIds).toEqual(new Set(['k1']));
    expect(result!.orphanedIds).toEqual(new Set(['g1'])); // g1's parent (k1) was removed
  });

  it('reports multiple removed roots with no orphans', () => {
    const result = detectPureRemoval([r1, r2], []);
    expect(result).not.toBeNull();
    expect(result!.removedIds).toEqual(new Set(['r1', 'r2']));
    expect(result!.orphanedIds).toEqual(new Set());
  });
});

describe('computeStableLayout', () => {
  /** Node positions keyed by agent id, for comparing before/after a layout call. */
  function posById(layout: ForestLayout): Record<string, { px: number; py: number }> {
    const out: Record<string, { px: number; py: number }> = {};
    for (const n of layout.nodes) out[n.agent.id] = { px: n.px, py: n.py };
    return out;
  }

  const noCollapse = new Set<string>();

  /** Builds the `previous` argument, defaulting to the inputs the common
   * (vertical, no collapse, no users, no filter) tests use. */
  function prev(
    agents: readonly Agent[],
    layout: ForestLayout,
    overrides: {
      showUsers?: boolean;
      orientation?: 'vertical' | 'horizontal';
      filterKey?: string;
      collapsedIds?: ReadonlySet<string>;
    } = {}
  ) {
    return {
      agents,
      collapsedIds: overrides.collapsedIds ?? noCollapse,
      showUsers: overrides.showUsers ?? false,
      orientation: overrides.orientation ?? ('vertical' as const),
      filterKey: overrides.filterKey ?? '',
      layout,
    };
  }

  /**
   * Fails with a readable diff if any two node/user rectangles in `layout`
   * overlap. computeStableLayout's re-rooting branch isolates and places each
   * affected unit independently of the full-forest leaf-slot numbering: this
   * is the property that guarantees it actually avoids collisions, rather
   * than merely passing on one fixture.
   */
  function assertNoOverlap(layout: ForestLayout): void {
    const rects = [
      ...layout.nodes.map((n) => ({ id: n.agent.id, px: n.px, py: n.py })),
      ...layout.users.map((u) => ({ id: userKey(u.id), px: u.px, py: u.py })),
    ];
    for (let i = 0; i < rects.length; i++) {
      for (let j = i + 1; j < rects.length; j++) {
        const a = rects[i];
        const b = rects[j];
        const overlapsX = a.px < b.px + NODE_W && b.px < a.px + NODE_W;
        const overlapsY = a.py < b.py + NODE_H && b.py < a.py + NODE_H;
        if (overlapsX && overlapsY) {
          throw new Error(
            `${a.id} (${a.px},${a.py}) overlaps ${b.id} (${b.px},${b.py}) in layout ${JSON.stringify(layout)}`
          );
        }
      }
    }
  }

  /**
   * Fails with a readable diff unless `stable` is a structurally valid
   * layout of `agents`: unique node/user/edge keys, every edge's pixels
   * matching its own endpoints' actual positions, and the exact same visible
   * node set, user set and (parentId, childId) edge set as a full fresh
   * layout of the same inputs would produce — the stable path may reposition
   * things, but it must never show a different graph than the normal
   * pipeline would.
   */
  function assertStructurallyValid(
    agents: Agent[],
    collapsedIds: ReadonlySet<string>,
    showUsers: boolean,
    orientation: 'vertical' | 'horizontal',
    stable: ForestLayout
  ): void {
    const nodeIds = stable.nodes.map((n) => n.agent.id);
    expect(new Set(nodeIds).size).toBe(nodeIds.length);
    const userIds = stable.users.map((u) => u.id);
    expect(new Set(userIds).size).toBe(userIds.length);
    const edgeKeys = stable.edges.map((e) => `${e.parentId}>${e.childId}`);
    expect(new Set(edgeKeys).size).toBe(edgeKeys.length);

    const posByKey = new Map<string, { px: number; py: number }>();
    for (const n of stable.nodes) posByKey.set(n.agent.id, n);
    for (const u of stable.users) posByKey.set(userKey(u.id), u);
    for (const e of stable.edges) {
      const parent = posByKey.get(e.parentId);
      const child = posByKey.get(e.childId);
      expect(parent, `edge parent ${e.parentId} has no position`).toBeTruthy();
      expect(child, `edge child ${e.childId} has no position`).toBeTruthy();
      const expected = edgeEndpoints(orientation, parent!, child!);
      expect({ x1: e.x1, y1: e.y1, x2: e.x2, y2: e.y2 }).toEqual(expected);
    }

    let fresh = showUsers
      ? layoutForestWithUsers(pruneCollapsed(buildLineageForest(agents), collapsedIds))
      : layoutForest(pruneCollapsed(buildLineageForest(agents), collapsedIds));
    if (orientation === 'horizontal') fresh = transposeLayout(fresh);

    expect(new Set(nodeIds)).toEqual(new Set(fresh.nodes.map((n) => n.agent.id)));
    expect(new Set(userIds)).toEqual(new Set(fresh.users.map((u) => u.id)));
    expect(new Set(edgeKeys)).toEqual(
      new Set(fresh.edges.map((e) => `${e.parentId}>${e.childId}`))
    );
  }

  it('with no previous layout, matches a full fresh layout', () => {
    const agents = [agent('r1', 'root', ['user-1']), agent('k1', 'kid', ['user-1', 'r1'])];
    const stable = computeStableLayout(agents, noCollapse, false, 'vertical', '', null);
    const fresh = layoutForest(buildLineageForest(agents));
    expect(posById(stable)).toEqual(posById(fresh));
  });

  it('removing an unrelated leaf keeps every other node pixel-identical', () => {
    // Two independent trees: deleting a leaf from tree A must not move
    // anything in tree B, nor its own siblings/ancestors in tree A — a leaf
    // removal never needs to re-root anything, so the old layout is still
    // completely valid minus that one node.
    const before = [
      agent('r1', 'root-1', ['user-1']),
      agent('a1', 'child-a', ['user-1', 'r1']),
      agent('a2', 'child-b', ['user-1', 'r1']),
      agent('r2', 'root-2', ['user-2']),
      agent('b1', 'other-tree-child', ['user-2', 'r2']),
    ];
    const beforeLayout = layoutForest(buildLineageForest(before));
    const beforePos = posById(beforeLayout);

    const after = before.filter((a) => a.id !== 'a2'); // a2 is a leaf
    const stable = computeStableLayout(
      after,
      noCollapse,
      false,
      'vertical',
      '',
      prev(before, beforeLayout)
    );

    expect(stable.nodes.map((n) => n.agent.id).sort()).toEqual(['a1', 'b1', 'r1', 'r2']);
    for (const id of ['a1', 'r1', 'r2', 'b1']) {
      expect(posById(stable)[id]).toEqual(beforePos[id]);
    }
    // The removed node's edge is gone; nothing else is.
    expect(stable.edges.some((e) => e.childId === 'a2')).toBe(false);
    expect(stable.edges).toHaveLength(beforeLayout.edges.length - 1);
  });

  it('recenters a surviving user over its remaining roots after a leaf removal', () => {
    // Three roots under one user; deleting the last one must not leave the
    // user card hanging over the gap where it used to be.
    const before = [
      agent('r1', 'root-1', ['user-1']),
      agent('r2', 'root-2', ['user-1']),
      agent('r3', 'root-3', ['user-1']),
    ];
    const beforeLayout = layoutForestWithUsers(buildLineageForest(before));

    const after = before.filter((a) => a.id !== 'r3'); // r3 is a leaf (and a root)
    const stable = computeStableLayout(
      after,
      noCollapse,
      true,
      'vertical',
      '',
      prev(before, beforeLayout, { showUsers: true })
    );

    expect(stable.users).toHaveLength(1);
    const user = stable.users[0];
    const r1 = stable.nodes.find((n) => n.agent.id === 'r1')!;
    const r2 = stable.nodes.find((n) => n.agent.id === 'r2')!;
    expect(user.px).toBeCloseTo((r1.px + r2.px) / 2);
    // The user's edges must be rebuilt from the corrected position too.
    const userEdges = stable.edges.filter((e) => e.parentId === userKey('user-1'));
    expect(userEdges).toHaveLength(2);
    for (const e of userEdges) {
      expect(e.x1).toBeCloseTo(user.px + NODE_W / 2);
      expect(e.y1).toBeCloseTo(user.py + NODE_H);
    }
    assertNoOverlap(stable);
  });

  it('drops a user left with no surviving roots after a leaf removal', () => {
    const before = [agent('r1', 'root-1', ['user-1']), agent('r2', 'root-2', ['user-2'])];
    const beforeLayout = layoutForestWithUsers(buildLineageForest(before));
    const after = before.filter((a) => a.id !== 'r2');
    const stable = computeStableLayout(
      after,
      noCollapse,
      true,
      'vertical',
      '',
      prev(before, beforeLayout, { showUsers: true })
    );
    expect(stable.users.map((u) => u.id)).toEqual(['user-1']);
  });

  describe('re-rooting removal does not overlap anything', () => {
    // Deleting k1 orphans g1, which sorts before "root-1" and "root-2" by
    // name: a global fresh-layout-then-override approach would put g1 and r1
    // in whichever slots a full fresh layout assigns them (0 and 1), then
    // restore r2 to ITS old slot (1), landing r1 and r2 on top of each other.
    function fixture(): Agent[] {
      return [
        agent('r1', 'root-1', ['user-1']),
        agent('k1', 'kid-1', ['user-1', 'r1']),
        agent('g1', 'grandkid-1', ['user-1', 'r1', 'k1']),
        agent('r2', 'root-2', ['user-2']),
        agent('b1', 'other-tree-child', ['user-2', 'r2']),
      ];
    }

    it('vertical, no users', () => {
      const before = fixture();
      const beforeLayout = layoutForest(buildLineageForest(before));
      const after = before.filter((a) => a.id !== 'k1');
      const stable = computeStableLayout(
        after,
        noCollapse,
        false,
        'vertical',
        '',
        prev(before, beforeLayout)
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, false, 'vertical', stable);
      // r1's old tree widens from 1 slot to 2 (k1's only child g1 is promoted
      // alongside now-childless r1), so r2/b1 shift right by exactly one slot
      // to make room — with only one neighbour to shift, this matches a
      // fresh layout of `after` exactly.
      const fresh = layoutForest(buildLineageForest(after));
      expect(posById(stable)).toEqual(posById(fresh));
    });

    it('horizontal, no users', () => {
      const before = fixture();
      let beforeLayout = layoutForest(buildLineageForest(before));
      beforeLayout = transposeLayout(beforeLayout);
      const after = before.filter((a) => a.id !== 'k1');
      const stable = computeStableLayout(
        after,
        noCollapse,
        false,
        'horizontal',
        '',
        prev(before, beforeLayout, { orientation: 'horizontal' })
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, false, 'horizontal', stable);
      let fresh = layoutForest(buildLineageForest(after));
      fresh = transposeLayout(fresh);
      expect(posById(stable)).toEqual(posById(fresh));
    });

    it('vertical, with users', () => {
      // alpha (root) has two children zz1, zz2; mid is a second, independent
      // root under the same user. Deleting alpha orphans zz1/zz2. mid shares
      // alpha's user, so it is part of the same *unit* and is recomputed
      // along with it (not frozen) — what must hold is that the user group
      // stays correct: exactly one user card, and mid keeps its edge to it.
      const before = [
        agent('alpha', 'alpha', ['u']),
        agent('zz1', 'zz1', ['u', 'alpha']),
        agent('zz2', 'zz2', ['u', 'alpha']),
        agent('mid', 'mid', ['u']),
      ];
      const beforeLayout = layoutForestWithUsers(buildLineageForest(before));
      const after = before.filter((a) => a.id !== 'alpha');
      const stable = computeStableLayout(
        after,
        noCollapse,
        true,
        'vertical',
        '',
        prev(before, beforeLayout, { showUsers: true })
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, true, 'vertical', stable);
      expect(stable.users).toHaveLength(1); // exactly one "u" card, not duplicated
      expect(stable.edges.some((e) => e.parentId === userKey('u') && e.childId === 'mid')).toBe(
        true
      );
    });

    it('a middle tree that still fits stays in its old footprint', () => {
      // Three independent trees a(->a1->{x,y}), b(->b1), c(->c1). Deleting
      // 'a' leaves a1 (same width as the old a-tree: one leaf each at x,y)
      // sandwiched between the untouched b and c trees — it must land back
      // in a's old slot, not jump to the far right past c.
      const before = [
        agent('a', 'a', ['user-1']),
        agent('a1', 'a1', ['user-1', 'a']),
        agent('x', 'x', ['user-1', 'a', 'a1']),
        agent('y', 'y', ['user-1', 'a', 'a1']),
        agent('b', 'b', ['user-2']),
        agent('b1', 'b1', ['user-2', 'b']),
        agent('c', 'c', ['user-3']),
        agent('c1', 'c1', ['user-3', 'c']),
      ];
      const beforeLayout = layoutForest(buildLineageForest(before));
      const beforePos = posById(beforeLayout);
      const after = before.filter((a) => a.id !== 'a');
      const stable = computeStableLayout(
        after,
        noCollapse,
        false,
        'vertical',
        '',
        prev(before, beforeLayout)
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, false, 'vertical', stable);
      // b and c (untouched) stay exactly where they were...
      expect(posById(stable).b).toEqual(beforePos.b);
      expect(posById(stable).c).toEqual(beforePos.c);
      // ...and a1/x/y land back in a's old slot, not appended past c.
      expect(posById(stable).a1.px).toEqual(beforePos.a.px);
      expect(posById(stable).x.px).toBeLessThan(posById(stable).b.px);
    });

    it.each([
      ['vertical', false],
      ['vertical', true],
      ['horizontal', false],
      ['horizontal', true],
    ] as const)(
      'deleting the middle of a chain widens in place and shifts only later trees, in order, by exactly the overflow (orientation=%s, showUsers=%s)',
      (orientation, showUsers) => {
        // A tree (0L -> 0Lk) to the LEFT of everything else — "0L" sorts
        // before "a" — plus chain a->b->c (one leaf, c) and two independent
        // untouched trees s, t to the right. Deleting the middle agent b
        // turns a (now childless) and c (promoted) into two separate roots:
        // the tree widens from 1 slot to 2. 0L/0Lk (and, with users shown,
        // their user card) must stay completely untouched; a must stay at
        // its old pixel; c sits directly next to it; and s/t — the only
        // things with room to give — shift by exactly one slot, keeping
        // their order and spacing, not appended past the far end.
        const before = [
          agent('0L', '0L', ['z']),
          agent('0Lk', '0Lk', ['z', '0L']),
          agent('a', 'a', ['u']),
          agent('b', 'b', ['u', 'a']),
          agent('c', 'c', ['u', 'a', 'b']),
          agent('s', 's', ['v']),
          agent('t', 't', ['w']),
        ];
        let beforeLayout = showUsers
          ? layoutForestWithUsers(buildLineageForest(before))
          : layoutForest(buildLineageForest(before));
        if (orientation === 'horizontal') beforeLayout = transposeLayout(beforeLayout);
        const beforePos = posById(beforeLayout);
        const beforeUserPos = new Map(
          beforeLayout.users.map((u) => [u.id, { px: u.px, py: u.py }])
        );

        const after = before.filter((x) => x.id !== 'b');
        const stable = computeStableLayout(
          after,
          noCollapse,
          showUsers,
          orientation,
          '',
          prev(before, beforeLayout, { showUsers, orientation })
        );

        assertNoOverlap(stable);
        assertStructurallyValid(after, noCollapse, showUsers, orientation, stable);

        const pos = posById(stable);
        const axisOf = (p: { px: number; py: number }): number =>
          orientation === 'horizontal' ? p.py : p.px;
        const slotPitch = orientation === 'horizontal' ? NODE_H + H_GAP_Y : NODE_W + GAP_X;

        // Content to the left of everything touched is completely untouched.
        expect(pos['0L']).toEqual(beforePos['0L']);
        expect(pos['0Lk']).toEqual(beforePos['0Lk']);
        if (showUsers) {
          const stableUserPos = new Map(stable.users.map((u) => [u.id, { px: u.px, py: u.py }]));
          expect(stableUserPos.get('z')).toEqual(beforeUserPos.get('z'));
        }

        // a keeps its exact old pixel; no hole left behind.
        expect(pos.a).toEqual(beforePos.a);
        // c sits in the very next slot, directly adjacent to a.
        expect(axisOf(pos.c)).toBe(axisOf(pos.a) + slotPitch);
        // s and t each shift by exactly one slot pitch, keeping both their
        // relative order and their original spacing from each other.
        expect(axisOf(pos.s)).toBe(axisOf(beforePos.s) + slotPitch);
        expect(axisOf(pos.t)).toBe(axisOf(beforePos.t) + slotPitch);
        expect(axisOf(pos.t) - axisOf(pos.s)).toBe(axisOf(beforePos.t) - axisOf(beforePos.s));

        // Matches what a fresh layout of the same post-delete agents gives,
        // since there's nothing between the chain and the right-hand trees
        // that could be disturbed differently.
        let fresh = showUsers
          ? layoutForestWithUsers(buildLineageForest(after))
          : layoutForest(buildLineageForest(after));
        if (orientation === 'horizontal') fresh = transposeLayout(fresh);
        expect(posById(stable)).toEqual(posById(fresh));
      }
    );

    it('preserves collapse state in the re-rooting branch', () => {
      // r -> k -> {g1, g2}, k collapsed. Deleting r orphans k (still
      // collapsed), so g1/g2 must stay hidden — the isolated per-unit layout
      // has to run pruneCollapsed too, not just the full-forest fresh path.
      const before = [
        agent('r', 'r', ['user-1']),
        agent('k', 'k', ['user-1', 'r']),
        agent('g1', 'g1', ['user-1', 'r', 'k']),
        agent('g2', 'g2', ['user-1', 'r', 'k']),
      ];
      const collapsed = new Set(['k']);
      const forest = buildLineageForest(before);
      pruneCollapsed(forest, collapsed);
      const beforeLayout = layoutForest(forest);
      expect(beforeLayout.nodes.map((n) => n.agent.id).sort()).toEqual(['k', 'r']);

      const after = before.filter((a) => a.id !== 'r'); // orphans k (still collapsed)
      const stable = computeStableLayout(
        after,
        collapsed,
        false,
        'vertical',
        '',
        prev(before, beforeLayout, { collapsedIds: collapsed })
      );

      expect(stable.nodes.map((n) => n.agent.id).sort()).toEqual(['k']);
      assertNoOverlap(stable);
    });

    it('does not duplicate a user key when two removed agents affect the same user group', () => {
      // alpha and beta are both roots under user u, each with one child; both
      // get deleted in the same update, orphaning both children.
      const before = [
        agent('alpha', 'alpha', ['u']),
        agent('ak', 'ak', ['u', 'alpha']),
        agent('beta', 'beta', ['u']),
        agent('bk', 'bk', ['u', 'beta']),
      ];
      const beforeLayout = layoutForestWithUsers(buildLineageForest(before));
      const after = before.filter((a) => a.id !== 'alpha' && a.id !== 'beta');
      const stable = computeStableLayout(
        after,
        noCollapse,
        true,
        'vertical',
        '',
        prev(before, beforeLayout, { showUsers: true })
      );

      expect(stable.users).toHaveLength(1);
      expect(new Set(stable.users.map((u) => u.id)).size).toBe(stable.users.length);
      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, true, 'vertical', stable);
    });

    it('a non-root survivor stays with its actual root unit, not a group named after its own ancestry', () => {
      // p (no ancestry) has two children: q (survives, stays p's ordinary
      // child) and m (removed, orphaning n — n's ancestry names p, so n is
      // legitimately grouped under a fake "user:p" once promoted). q must
      // stay grouped with p itself; it was never promoted, so unlike n it has
      // no business forming its own "user:p" group just because its own
      // ancestry happens to name p too — a per-agent rootUserOf check finds
      // this indistinguishable from n's case, which is the bug.
      const before = [
        agent('p', 'p', []),
        agent('q', 'q', ['p']),
        agent('m', 'm', ['p']),
        agent('n', 'n', ['p', 'm']),
      ];
      const beforeLayout = layoutForestWithUsers(buildLineageForest(before));
      const after = before.filter((a) => a.id !== 'm');
      const stable = computeStableLayout(
        after,
        noCollapse,
        true,
        'vertical',
        '',
        prev(before, beforeLayout, { showUsers: true })
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, true, 'vertical', stable);
      // q is still p's direct child: a real edge between them, not a
      // "user:p" edge.
      expect(stable.edges.some((e) => e.parentId === 'p' && e.childId === 'q')).toBe(true);
      // n, promoted and genuinely naming p as its root user, gets its own
      // "user:p" card and edge — that part is correct and expected.
      expect(stable.edges.some((e) => e.parentId === userKey('p') && e.childId === 'n')).toBe(true);
    });

    it('drops a user whose sole root is removed, even while a separate tree is simultaneously re-rooted', () => {
      // x is u1's only agent. r (a different tree, under u2) loses its child
      // k's... no, r itself is removed, orphaning k. Both happen in the same
      // update: x's removal has nothing to promote (u1's group vanishes
      // entirely), while r's removal promotes k. The two must not interfere
      // with each other.
      const before = [
        agent('x', 'x', ['u1']),
        agent('r', 'r', ['u2']),
        agent('k', 'k', ['u2', 'r']),
      ];
      const beforeLayout = layoutForestWithUsers(buildLineageForest(before));
      const after = before.filter((a) => a.id !== 'x' && a.id !== 'r');
      const stable = computeStableLayout(
        after,
        noCollapse,
        true,
        'vertical',
        '',
        prev(before, beforeLayout, { showUsers: true })
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, true, 'vertical', stable);
      expect(stable.users.map((u) => u.id)).not.toContain('u1');
      expect(stable.users.map((u) => u.id)).toContain('u2');
    });

    it('packs both children of a deleted multi-child root together in its old footprint, without growing the canvas', () => {
      // r has two children c1, c2; s and t are untouched, independent trees
      // to the right. Deleting r must not send c2 past s and t — both
      // children belong to the same old tree and must be placed together.
      const before = [
        agent('r', 'r', ['user-1']),
        agent('c1', 'c1', ['user-1', 'r']),
        agent('c2', 'c2', ['user-1', 'r']),
        agent('s', 's', ['user-2']),
        agent('t', 't', ['user-3']),
      ];
      const beforeLayout = layoutForest(buildLineageForest(before));
      const beforePos = posById(beforeLayout);
      const after = before.filter((a) => a.id !== 'r');
      const stable = computeStableLayout(
        after,
        noCollapse,
        false,
        'vertical',
        '',
        prev(before, beforeLayout)
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, false, 'vertical', stable);
      const pos = posById(stable);
      // c1/c2 are promoted to roots, so they move up one row (depth 0 instead
      // of 1) — only their horizontal (packAxis) position must be unchanged.
      expect(pos.c1.px).toBe(beforePos.c1.px);
      expect(pos.c2.px).toBe(beforePos.c2.px);
      expect(pos.s).toEqual(beforePos.s);
      expect(pos.t).toEqual(beforePos.t);
      expect(stable.width).toBe(beforeLayout.width);
    });

    it('packs children of a deleted multi-child root with empty ancestry together, with users shown', () => {
      // r has no ancestry (the hub's no-identity convention); its children
      // inherit ancestry [r], making rootUserOf(c1) === rootUserOf(c2) ===
      // 'r' — both land in the same "user:r" unit once r is deleted, so they
      // pack together the same way the no-users case above does.
      const before = [
        agent('r', 'r', []),
        agent('c1', 'c1', ['r']),
        agent('c2', 'c2', ['r']),
        agent('s', 's', ['user-2']),
      ];
      const beforeLayout = layoutForestWithUsers(buildLineageForest(before));
      const beforePos = posById(beforeLayout);
      const after = before.filter((a) => a.id !== 'r');
      const stable = computeStableLayout(
        after,
        noCollapse,
        true,
        'vertical',
        '',
        prev(before, beforeLayout, { showUsers: true })
      );

      assertNoOverlap(stable);
      assertStructurallyValid(after, noCollapse, true, 'vertical', stable);
      expect(posById(stable).s).toEqual(beforePos.s);
      expect(stable.users.map((u) => u.id)).toContain('r');
    });

    it('recomputes the orphan-side user group when the deleted parent has empty ancestry, with no dangling edges', () => {
      // p has no ancestry; c (its child) inherits [p], so rootUserOf(c) ===
      // 'p' even though p itself was never grouped under any user (p was a
      // plain, ungrouped root). Deleting p must still produce a "p" user
      // card for the promoted c/g, not a dangling p->c edge.
      const before = [
        agent('p', 'p', []),
        agent('c', 'c', ['p']),
        agent('g', 'g', ['p', 'c']),
        agent('x', 'x', ['u1']),
      ];
      const beforeLayout = layoutForestWithUsers(buildLineageForest(before));
      const after = before.filter((a) => a.id !== 'p');
      const stable = computeStableLayout(
        after,
        noCollapse,
        true,
        'vertical',
        '',
        prev(before, beforeLayout, { showUsers: true })
      );

      assertStructurallyValid(after, noCollapse, true, 'vertical', stable);
      assertNoOverlap(stable);
      // No dangling edge to the deleted 'p'.
      expect(stable.edges.some((e) => e.parentId === 'p' || e.childId === 'p')).toBe(false);
      expect(stable.nodes.some((n) => n.agent.id === 'p')).toBe(false);
      // The orphaned c/g get a user card (c's rootUserOf is 'p').
      expect(stable.users.map((u) => u.id)).toContain('p');
      expect(stable.edges.some((e) => e.parentId === userKey('p') && e.childId === 'c')).toBe(true);
      // x (unrelated, under a real user) is untouched.
      expect(stable.users.map((u) => u.id)).toContain('u1');
    });
  });

  it('rebuilds re-rooting-branch edges with the correct endpoints', () => {
    const before = [
      agent('r1', 'root-1', ['user-1']),
      agent('k1', 'kid-1', ['user-1', 'r1']),
      agent('g1', 'grandkid-1', ['user-1', 'r1', 'k1']),
      agent('r2', 'root-2', ['user-2']),
      agent('b1', 'other-tree-child', ['user-2', 'r2']),
    ];
    const beforeLayout = layoutForest(buildLineageForest(before));
    const after = before.filter((a) => a.id !== 'k1');
    const stable = computeStableLayout(
      after,
      noCollapse,
      false,
      'vertical',
      '',
      prev(before, beforeLayout)
    );

    const pos = posById(stable);
    for (const e of stable.edges) {
      const parent = pos[e.parentId];
      const child = pos[e.childId];
      expect(parent).toBeTruthy();
      expect(child).toBeTruthy();
      expect(e.x1).toBeCloseTo(parent.px + NODE_W / 2);
      expect(e.y1).toBeCloseTo(parent.py + NODE_H);
      expect(e.x2).toBeCloseTo(child.px + NODE_W / 2); // not child.px: that would be the top-left corner
      expect(e.y2).toBeCloseTo(child.py);
    }
    // r2->b1 specifically, since it's the anchored edge most likely to point
    // at the wrong x if this regresses.
    const r2b1 = stable.edges.find((e) => e.parentId === 'r2' && e.childId === 'b1')!;
    expect(r2b1.x2).toBeCloseTo(pos.b1.px + NODE_W / 2);
  });

  it('falls back to a full fresh layout when an agent is added alongside a removal', () => {
    // Not a pure removal (net addition), so detectPureRemoval returns null
    // and the function must not try to reuse stale positions.
    const before = [agent('r1', 'root-1', ['user-1']), agent('k1', 'kid-1', ['user-1', 'r1'])];
    const beforeLayout = layoutForest(buildLineageForest(before));

    const after = [agent('r1', 'root-1', ['user-1']), agent('n1', 'newcomer', ['user-1', 'r1'])];
    const stable = computeStableLayout(
      after,
      noCollapse,
      false,
      'vertical',
      '',
      prev(before, beforeLayout)
    );
    const fresh = layoutForest(buildLineageForest(after));
    expect(posById(stable)).toEqual(posById(fresh));
  });

  it('falls back to a full fresh layout when orientation changed', () => {
    const before = [agent('r1', 'root-1', ['user-1']), agent('k1', 'kid-1', ['user-1', 'r1'])];
    const beforeLayout = layoutForest(buildLineageForest(before));
    const after = before.slice(0, 1); // a pure removal in agent terms...

    // ...but computeStableLayout is called with a changed orientation, which
    // has no "unaffected region" concept, so it must reflow fully in the new
    // orientation rather than mixing stale vertical pixels into a horizontal
    // layout.
    const stable = computeStableLayout(
      after,
      noCollapse,
      false,
      'horizontal',
      '',
      prev(before, beforeLayout, { orientation: 'vertical' })
    );
    let fresh = layoutForest(buildLineageForest(after));
    fresh = transposeLayout(fresh);
    expect(posById(stable)).toEqual(posById(fresh));
  });

  describe('structural invariants over randomized removals', () => {
    // Minimal seeded LCG: deterministic (reproducible on failure) without
    // relying on Math.random or a test dependency.
    function makeRng(seed: number): () => number {
      let state = seed >>> 0;
      return () => {
        state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
        return state / 0x1_0000_0000;
      };
    }

    /** Seeded Fisher–Yates: unlike `array.sort(() => rng() - 0.5)`, this
     * doesn't depend on the engine's sort algorithm for reproducibility. */
    function shuffle<T>(arr: readonly T[], rng: () => number): T[] {
      const out = [...arr];
      for (let i = out.length - 1; i > 0; i--) {
        const j = Math.floor(rng() * (i + 1));
        [out[i], out[j]] = [out[j], out[i]];
      }
      return out;
    }

    /**
     * A random forest: `size` agents, each either a root or a child of an
     * earlier agent. A child's ancestry is the parent's full chain plus the
     * parent's own id (real ancestry semantics), not just `[uid, parentId]`,
     * so multi-level chains are exercised. Roots get empty ancestry about
     * 30% of the time (the hub's no-identity convention) and a real user id
     * otherwise.
     */
    function randomForest(rng: () => number, size: number, userCount: number): Agent[] {
      const users = Array.from({ length: userCount }, (_, i) => `u${i}`);
      const agents: Agent[] = [];
      for (let i = 0; i < size; i++) {
        const id = `a${i}`;
        const parent = i > 0 && rng() < 0.65 ? agents[Math.floor(rng() * agents.length)] : null;
        if (parent) {
          agents.push(agent(id, id, [...(parent.ancestry ?? []), parent.id]));
        } else if (rng() < 0.3) {
          agents.push(agent(id, id, [])); // no-ancestry root
        } else {
          agents.push(agent(id, id, [users[Math.floor(rng() * users.length)]]));
        }
      }
      return agents;
    }

    /** About 20% of agents, for exercising collapse state in the re-rooting
     * branch alongside everything else. */
    function randomCollapsed(rng: () => number, agents: Agent[]): Set<string> {
      const collapsed = new Set<string>();
      for (const a of agents) {
        if (rng() < 0.2) collapsed.add(a.id);
      }
      return collapsed;
    }

    /**
     * Every surviving agent whose old root tree, and old user group when
     * `showUsers`, had no removed member is "unaffected" — checked
     * independently of `computeStableLayout`'s own notion of "affected" so a
     * bug in that notion can't hide from this assertion the way it would
     * hide from a no-overlap-only check. An unaffected agent is not
     * guaranteed to keep its *exact* pixel: a widened unit to its left can
     * push it along `packAxis` to make room. What must still hold is the
     * acceptance criterion in substance — this is a uniform, order-preserving
     * shift, not a reshuffle — so this checks that every unaffected agent's
     * `packAxis` position never *decreases* from its old one, and that the
     * relative order of unaffected agents (sorted by their old `packAxis`
     * position) is preserved in the new layout. Content positioned before
     * every touched tree/group must keep its exact pixel; everything else
     * must only move forward, in order. A hand-written test separately pins
     * the exact pixels for a specific widening shape.
     */
    function assertUnaffectedOrderPreserved(
      before: Agent[],
      beforeLayout: ForestLayout,
      removedIds: ReadonlySet<string>,
      showUsers: boolean,
      orientation: Orientation,
      stable: ForestLayout
    ): void {
      const oldTreeOf = new Map<string, string>();
      const walk = (node: LineageNode, rootId: string): void => {
        oldTreeOf.set(node.agent.id, rootId);
        for (const c of node.children) walk(c, rootId);
      };
      for (const root of buildLineageForest(before)) walk(root, root.agent.id);
      const oldById = new Map(before.map((a) => [a.id, a]));

      const touchedTrees = new Set<string>();
      const touchedUsers = new Set<string>();
      for (const id of removedIds) {
        const root = oldTreeOf.get(id);
        if (root !== undefined) touchedTrees.add(root);
        if (showUsers) {
          const uid = rootUserOf(oldById.get(id)!);
          if (uid) touchedUsers.add(uid);
        }
      }

      const axisOf = (p: { px: number; py: number }): number =>
        orientation === 'horizontal' ? p.py : p.px;
      const beforePos = new Map(
        beforeLayout.nodes.map((n) => [n.agent.id, { px: n.px, py: n.py }])
      );
      const stablePos = new Map(stable.nodes.map((n) => [n.agent.id, { px: n.px, py: n.py }]));

      const unaffectedByOldAxis = before
        .filter((a) => {
          if (removedIds.has(a.id)) return false;
          if (!beforePos.has(a.id)) return false; // hidden by collapse before; nothing to compare
          const uid = showUsers ? rootUserOf(a) : undefined;
          return (
            !touchedTrees.has(oldTreeOf.get(a.id)!) && !(uid !== undefined && touchedUsers.has(uid))
          );
        })
        .map((a) => ({ id: a.id, oldAxis: axisOf(beforePos.get(a.id)!) }))
        .sort((x, y) => x.oldAxis - y.oldAxis);

      // The strongest, independent check: anything positioned before the
      // leftmost edge of *any* touched old tree/group can never have
      // anything to make room for, so it must keep its *exact* previous
      // pixel — not just "moved forward, in order". The threshold is the
      // touched tree/group's own old span, not just the removed id's own
      // position within it: a removed id is not necessarily its tree's
      // leftmost member.
      const touchedOldAxes = before
        .filter((a) => {
          const uid = showUsers ? rootUserOf(a) : undefined;
          return (
            touchedTrees.has(oldTreeOf.get(a.id)!) || (uid !== undefined && touchedUsers.has(uid))
          );
        })
        .map((a) => beforePos.get(a.id))
        .filter((p): p is { px: number; py: number } => !!p)
        .map(axisOf);
      const minTouchedOldAxis = touchedOldAxes.length > 0 ? Math.min(...touchedOldAxes) : Infinity;

      let lastNewAxis = -Infinity;
      for (const { id, oldAxis } of unaffectedByOldAxis) {
        const newPos = stablePos.get(id);
        expect(newPos, `unaffected agent ${id} is missing from the stable layout`).toBeTruthy();
        if (oldAxis < minTouchedOldAxis) {
          expect(newPos, `agent ${id} left of all touched content moved`).toEqual(
            beforePos.get(id)
          );
        }
        const newAxis = axisOf(newPos!);
        expect(
          newAxis,
          `unaffected agent ${id} moved backward along packAxis`
        ).toBeGreaterThanOrEqual(oldAxis);
        expect(
          newAxis,
          `unaffected agent ${id} is out of order relative to other unaffected agents`
        ).toBeGreaterThanOrEqual(lastNewAxis);
        lastNewAxis = newAxis;
      }
    }

    it('no overlaps, valid structure and unaffected-order stability over many random removals, including sequential ones', () => {
      // ~1000 trial-starts (250 per showUsers x orientation combination), each
      // chaining 1-3 sequential removals that feed the previous stable layout
      // back in as `previous` — the realistic case, since a graph's `previous`
      // is almost always an earlier *stable* layout with its own holes and
      // shifted units, not a freshly-computed one. Runs in well under 1s in
      // isolation; the explicit timeout guards only against full-suite CPU
      // contention (confirmed necessary on a busy sandbox), not normal runtime.
      const rng = makeRng(0xc0ffee);
      let trials = 0;
      for (const showUsers of [false, true]) {
        for (const orientation of ['vertical', 'horizontal'] as const) {
          for (let i = 0; i < 250; i++) {
            let currentAgents = randomForest(rng, 4 + Math.floor(rng() * 8), 3);
            let currentCollapsed = randomCollapsed(rng, currentAgents);
            let currentLayout = showUsers
              ? layoutForestWithUsers(
                  pruneCollapsed(buildLineageForest(currentAgents), currentCollapsed)
                )
              : layoutForest(pruneCollapsed(buildLineageForest(currentAgents), currentCollapsed));
            if (orientation === 'horizontal') currentLayout = transposeLayout(currentLayout);

            const steps = 1 + Math.floor(rng() * 3); // 1-3 sequential removals
            for (let step = 0; step < steps && currentAgents.length > 1; step++) {
              const removeCount = Math.min(1 + Math.floor(rng() * 3), currentAgents.length - 1);
              const removedIds = new Set(
                shuffle(currentAgents, rng)
                  .slice(0, removeCount)
                  .map((a) => a.id)
              );
              const nextAgents = currentAgents.filter((a) => !removedIds.has(a.id));

              const stable = computeStableLayout(
                nextAgents,
                currentCollapsed,
                showUsers,
                orientation,
                '',
                prev(currentAgents, currentLayout, {
                  showUsers,
                  orientation,
                  collapsedIds: currentCollapsed,
                })
              );

              assertNoOverlap(stable);
              assertStructurallyValid(nextAgents, currentCollapsed, showUsers, orientation, stable);
              assertUnaffectedOrderPreserved(
                currentAgents,
                currentLayout,
                removedIds,
                showUsers,
                orientation,
                stable
              );

              currentAgents = nextAgents;
              currentLayout = stable;
              trials++;
            }
          }
        }
      }
      expect(trials).toBeGreaterThanOrEqual(1000);
    }, 20000);
  });

  describe('filterKey distinguishes a filter change from a delete', () => {
    const before = [
      agent('r1', 'root-1', ['user-1']),
      agent('a1', 'child-a', ['user-1', 'r1']),
      agent('a2', 'child-b', ['user-1', 'r1']),
    ];

    it('a shrink with an unchanged filterKey stays on the stable (delete) path', () => {
      const beforeLayout = layoutForest(buildLineageForest(before));
      const beforePos = posById(beforeLayout);
      const after = before.filter((a) => a.id !== 'a2');
      const stable = computeStableLayout(
        after,
        noCollapse,
        false,
        'vertical',
        'running', // same filterKey as `prev` below
        prev(before, beforeLayout, { filterKey: 'running' })
      );
      // Unchanged survivors keep their exact previous pixels — the delete path.
      expect(posById(stable).r1).toEqual(beforePos.r1);
      expect(posById(stable).a1).toEqual(beforePos.a1);
    });

    it('a shrink with a changed filterKey gets a full fresh (re-fit) layout', () => {
      const beforeLayout = layoutForest(buildLineageForest(before));
      const after = before.filter((a) => a.id !== 'a2');
      const stable = computeStableLayout(
        after,
        noCollapse,
        false,
        'vertical',
        'stopped', // different filterKey: this is a filter change, not a delete
        prev(before, beforeLayout, { filterKey: 'running' })
      );
      const fresh = layoutForest(buildLineageForest(after));
      expect(posById(stable)).toEqual(posById(fresh));
    });
  });
});
