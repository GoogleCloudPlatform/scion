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
 * Pure lineage-forest construction and layout for the agent graph view.
 * Kept free of Lit/DOM dependencies so it can be unit-tested directly.
 */

import type { Agent } from './types.js';

/** A node in the lineage forest (each agent has at most one parent). */
export interface LineageNode {
  agent: Agent;
  children: LineageNode[];
  /** Horizontal position in leaf units (assigned by layout) */
  x: number;
  /** Tree depth: 0 for roots */
  depth: number;
}

export interface PositionedNode {
  agent: Agent;
  /** Pixel coordinates of the node's top-left corner */
  px: number;
  py: number;
}

export interface PositionedEdge {
  /** Pixel coordinates: parent bottom-center -> child top-center */
  x1: number;
  y1: number;
  x2: number;
  y2: number;
  parentId: string;
  childId: string;
}

/** A user (human) node shown above the trees they originated. */
export interface PositionedUser {
  /** User ID: the first ancestry entry shared by every agent in the tree */
  id: string;
  px: number;
  py: number;
}

export interface ForestLayout {
  nodes: PositionedNode[];
  edges: PositionedEdge[];
  /** Present only in layouts produced by layoutForestWithUsers */
  users: PositionedUser[];
  width: number;
  height: number;
}

export const NODE_W = 180;
export const NODE_H = 76;
export const GAP_X = 24;
export const GAP_Y = 52;
export const PAD = 24;

/** Graph flow direction: vertical = top→down (default), horizontal = left→right. */
export type Orientation = 'vertical' | 'horizontal';

/**
 * Horizontal-orientation spacing. Cards keep their 180×76 size, so the
 * along-depth gap between columns must clear the wide edge curves (cards are
 * wider than tall), while sibling rows can pack tighter.
 */
export const H_GAP_X = 64;
export const H_GAP_Y = 24;

/** Edge/hover key for a user node, distinct from any agent ID. */
export function userKey(userId: string): string {
  return `user:${userId}`;
}

/**
 * Direct parent ID from the agent's ancestry chain ([root, ..., parent]).
 * The parent may be another agent or a user; callers decide by lookup.
 */
export function parentIdOf(agent: Agent): string | undefined {
  const chain = agent.ancestry;
  return chain && chain.length > 0 ? chain[chain.length - 1] : undefined;
}

/**
 * The user (human) at the origin of the agent's lineage chain. Ancestry
 * always starts with the user who created the root agent, so this is stable
 * across the whole tree.
 */
export function rootUserOf(agent: Agent): string | undefined {
  const chain = agent.ancestry;
  return chain && chain.length > 0 ? chain[0] : undefined;
}

/**
 * Builds the lineage forest. An agent is attached under its parent only when
 * the parent is another agent in the given set; otherwise it becomes a root
 * (its parent is a user, filtered out, or deleted). A visited guard keeps
 * malformed cyclic ancestry from hanging the layout: any agent not reachable
 * from a root is promoted to a root.
 */
export function buildLineageForest(agents: Agent[]): LineageNode[] {
  const byId = new Map<string, LineageNode>();
  for (const agent of agents) {
    byId.set(agent.id, { agent, children: [], x: 0, depth: 0 });
  }

  const roots: LineageNode[] = [];
  for (const node of byId.values()) {
    const parentId = parentIdOf(node.agent);
    const parent = parentId ? byId.get(parentId) : undefined;
    if (parent && parent !== node) {
      parent.children.push(node);
    } else {
      roots.push(node);
    }
  }

  const byName = (a: LineageNode, b: LineageNode) => a.agent.name.localeCompare(b.agent.name);
  for (const node of byId.values()) {
    node.children.sort(byName);
  }
  roots.sort(byName);

  // Walk the forest, assigning depths. Dropping already-visited children as
  // we go turns any malformed cyclic ancestry into plain tree edges instead
  // of infinite recursion; nodes unreachable from a root (cycle members) are
  // then promoted to roots.
  const visited = new Set<string>();
  const visit = (node: LineageNode, depth: number) => {
    if (visited.has(node.agent.id)) return;
    visited.add(node.agent.id);
    node.depth = depth;
    node.children = node.children.filter((c) => !visited.has(c.agent.id));
    for (const child of node.children) visit(child, depth + 1);
  };
  for (const root of roots) visit(root, 0);
  for (const node of byId.values()) {
    if (!visited.has(node.agent.id)) {
      roots.push(node);
      visit(node, 0);
    }
  }

  return roots;
}

/**
 * Number of transitive descendants for every node in the forest, keyed by
 * agent ID. Compute this BEFORE pruneCollapsed — pruning removes the very
 * subtrees being counted.
 */
export function descendantCounts(roots: LineageNode[]): Map<string, number> {
  const counts = new Map<string, number>();
  const count = (node: LineageNode): number => {
    let total = 0;
    for (const child of node.children) {
      total += 1 + count(child);
    }
    counts.set(node.agent.id, total);
    return total;
  };
  for (const root of roots) count(root);
  return counts;
}

/**
 * Drops the children of every collapsed node so the layout skips their
 * subtrees. Mutates the given forest (buildLineageForest returns a fresh
 * one per call) and returns it for chaining.
 */
export function pruneCollapsed(
  roots: LineageNode[],
  collapsed: ReadonlySet<string>
): LineageNode[] {
  const walk = (node: LineageNode): void => {
    if (collapsed.has(node.agent.id)) {
      node.children = [];
      return;
    }
    for (const child of node.children) walk(child);
  };
  for (const root of roots) walk(root);
  return roots;
}

/**
 * Tidy-ish tree layout: leaves take consecutive horizontal slots, parents
 * center over their children. Returns positioned nodes/edges plus the canvas
 * size in pixels.
 */
export function layoutForest(roots: LineageNode[]): ForestLayout {
  let nextLeaf = 0;
  let maxDepth = 0;

  const assign = (node: LineageNode) => {
    maxDepth = Math.max(maxDepth, node.depth);
    if (node.children.length === 0) {
      node.x = nextLeaf++;
      return;
    }
    for (const child of node.children) assign(child);
    const first = node.children[0].x;
    const last = node.children[node.children.length - 1].x;
    node.x = (first + last) / 2;
  };
  for (const root of roots) assign(root);

  const px = (n: LineageNode) => PAD + n.x * (NODE_W + GAP_X);
  const py = (n: LineageNode) => PAD + n.depth * (NODE_H + GAP_Y);

  const nodes: PositionedNode[] = [];
  const edges: PositionedEdge[] = [];
  const walk = (node: LineageNode) => {
    nodes.push({ agent: node.agent, px: px(node), py: py(node) });
    for (const child of node.children) {
      edges.push({
        x1: px(node) + NODE_W / 2,
        y1: py(node) + NODE_H,
        x2: px(child) + NODE_W / 2,
        y2: py(child),
        parentId: node.agent.id,
        childId: child.agent.id,
      });
      walk(child);
    }
  };
  for (const root of roots) walk(root);

  return {
    nodes,
    edges,
    users: [],
    width: PAD * 2 + Math.max(nextLeaf, 1) * (NODE_W + GAP_X) - GAP_X,
    height: PAD * 2 + (maxDepth + 1) * (NODE_H + GAP_Y) - GAP_Y,
  };
}

/**
 * Like layoutForest, but inserts a row of user (human) nodes above the trees,
 * grouping each root agent under the user at the origin of its lineage chain
 * (ancestry[0]). Roots sharing a user are laid out adjacently under a single
 * user node with an edge to each. Roots with no recorded ancestry keep their
 * position but get no user parent.
 */
export function layoutForestWithUsers(roots: LineageNode[]): ForestLayout {
  // Group roots by originating user, preserving the sorted root order.
  const groups = new Map<string, LineageNode[]>();
  const ungrouped: LineageNode[] = [];
  for (const root of roots) {
    const uid = rootUserOf(root.agent);
    if (!uid) {
      ungrouped.push(root);
      continue;
    }
    const group = groups.get(uid);
    if (group) {
      group.push(root);
    } else {
      groups.set(uid, [root]);
    }
  }
  const ordered = [...groups.values()].flat().concat(ungrouped);

  // Shift every agent down one row to make room for the user row. Children
  // were already de-cycled by buildLineageForest, so plain recursion is safe.
  const bump = (node: LineageNode, depth: number): void => {
    node.depth = depth;
    for (const child of node.children) bump(child, depth + 1);
  };
  for (const root of ordered) bump(root, 1);

  const base = layoutForest(ordered);
  const nodeById = new Map(base.nodes.map((n) => [n.agent.id, n]));

  const users: PositionedUser[] = [];
  const edges = [...base.edges];
  for (const [uid, groupRoots] of groups) {
    const xs = groupRoots.map((r) => nodeById.get(r.agent.id)!.px);
    const px = (Math.min(...xs) + Math.max(...xs)) / 2;
    const py = PAD;
    users.push({ id: uid, px, py });
    for (const root of groupRoots) {
      const target = nodeById.get(root.agent.id)!;
      edges.push({
        x1: px + NODE_W / 2,
        y1: py + NODE_H,
        x2: target.px + NODE_W / 2,
        y2: target.py,
        parentId: userKey(uid),
        childId: root.agent.id,
      });
    }
  }

  return { ...base, edges, users };
}

/**
 * Transposes a vertical layout (from layoutForest / layoutForestWithUsers)
 * into a horizontal one: depth maps to the x axis (roots on the left, depth
 * increases to the right) and leaf slots stack vertically. Cards keep their
 * NODE_W×NODE_H size — only positions change — so the column pitch uses
 * NODE_W + H_GAP_X and the row pitch NODE_H + H_GAP_Y. Edges are recomputed
 * from the transposed node positions: parent right-edge-center → child
 * left-edge-center. User nodes end up in the leftmost column, vertically
 * centered across their group's roots.
 *
 * The vertical layout is the single source of truth for the tidy-tree
 * algorithm; this helper only recovers the abstract (slot, depth) coordinates
 * from the vertical pixel grid and re-projects them.
 */
export function transposeLayout(layout: ForestLayout): ForestLayout {
  const slotOf = (px: number) => (px - PAD) / (NODE_W + GAP_X);
  const depthOf = (py: number) => (py - PAD) / (NODE_H + GAP_Y);
  const hx = (depth: number) => PAD + depth * (NODE_W + H_GAP_X);
  const hy = (slot: number) => PAD + slot * (NODE_H + H_GAP_Y);

  const nodes: PositionedNode[] = layout.nodes.map((n) => ({
    agent: n.agent,
    px: hx(depthOf(n.py)),
    py: hy(slotOf(n.px)),
  }));
  const users: PositionedUser[] = layout.users.map((u) => ({
    id: u.id,
    px: hx(depthOf(u.py)),
    py: hy(slotOf(u.px)),
  }));

  const posByKey = new Map<string, { px: number; py: number }>();
  for (const n of nodes) posByKey.set(n.agent.id, n);
  for (const u of users) posByKey.set(userKey(u.id), u);

  const edges: PositionedEdge[] = [];
  for (const e of layout.edges) {
    const parent = posByKey.get(e.parentId);
    const child = posByKey.get(e.childId);
    if (!parent || !child) continue;
    edges.push({
      x1: parent.px + NODE_W,
      y1: parent.py + NODE_H / 2,
      x2: child.px,
      y2: child.py + NODE_H / 2,
      parentId: e.parentId,
      childId: e.childId,
    });
  }

  let maxX = 0;
  let maxY = 0;
  for (const p of posByKey.values()) {
    maxX = Math.max(maxX, p.px + NODE_W);
    maxY = Math.max(maxY, p.py + NODE_H);
  }

  return {
    nodes,
    edges,
    users,
    width: (posByKey.size > 0 ? maxX : PAD + NODE_W) + PAD,
    height: (posByKey.size > 0 ? maxY : PAD + NODE_H) + PAD,
  };
}
