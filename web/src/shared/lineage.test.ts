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
