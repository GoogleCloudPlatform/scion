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
  descendantCounts,
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
