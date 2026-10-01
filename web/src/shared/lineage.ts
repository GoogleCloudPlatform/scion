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

/** Deterministic string ordering, used everywhere an id needs a stable tie-break. */
function compareIds(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/**
 * Pixel endpoints for the edge between a positioned parent and child, given
 * the flow direction. Vertical: parent bottom-center → child top-center.
 * Horizontal: parent right-edge-center → child left-edge-center. The single
 * source of truth for edge geometry — `layoutForest`, `layoutForestWithUsers`
 * and `transposeLayout` all call this instead of inlining the math, and so
 * does `computeStableLayout` when it rebuilds an edge from a node whose
 * position it chose to preserve rather than recompute.
 */
export function edgeEndpoints(
  orientation: Orientation,
  parent: { px: number; py: number },
  child: { px: number; py: number }
): { x1: number; y1: number; x2: number; y2: number } {
  if (orientation === 'horizontal') {
    return {
      x1: parent.px + NODE_W,
      y1: parent.py + NODE_H / 2,
      x2: child.px,
      y2: child.py + NODE_H / 2,
    };
  }
  return {
    x1: parent.px + NODE_W / 2,
    y1: parent.py + NODE_H,
    x2: child.px + NODE_W / 2,
    y2: child.py,
  };
}

/**
 * A signature that changes iff a layout input the forest/layout functions
 * actually read would change: membership (add/remove), direct-parent
 * structure (reparent, via ancestry's last entry), the root user a tree is
 * grouped under in layoutForestWithUsers (ancestry's first entry — a
 * separate input from the direct parent, and read only when `showUsers` is
 * on, but included unconditionally so toggling `showUsers` after an
 * ancestry-only change still invalidates correctly), name (sort order and
 * label), collapse state, the show-users toggle, or orientation. Two agent
 * lists that differ only in object identity or in fields outside this set
 * (status, capabilities, messageability, etc.) produce the same signature.
 * Callers use this to cache layout across status-only renders, pans, zooms
 * and hovers, and to invalidate it exactly on the changes that affect
 * topology or geometry.
 *
 * Sorted by ID before hashing so the signature is independent of the input
 * array's order (e.g. after an SSE-triggered re-sort with no real change).
 * Ties in `buildLineageForest`'s name sort break on ID (see `byName` below),
 * so ID + parent + root user + name fully determines layout: nothing the
 * layout depends on varies while producing the same signature.
 */
export function topologySignature(
  agents: readonly Agent[],
  collapsedIds: ReadonlySet<string>,
  showUsers: boolean,
  orientation: Orientation
): string {
  const rows = agents
    .map((a) => [a.id, parentIdOf(a) ?? '', rootUserOf(a) ?? '', a.name] as const)
    .sort((a, b) => compareIds(a[0], b[0]));
  const collapsed = [...collapsedIds].sort();
  return JSON.stringify({ rows, collapsed, showUsers, orientation });
}

/**
 * Builds the lineage forest. An agent is attached under its parent only when
 * the parent is another agent in the given set; otherwise it becomes a root
 * (its parent is a user, filtered out, or deleted). A visited guard keeps
 * malformed cyclic ancestry from hanging the layout: for each cycle among
 * agents unreachable from any legitimate root, exactly one member (the
 * lowest id) is promoted to a root, and the rest of that cycle — plus any
 * ordinary descendants hanging off it — attach beneath it as usual.
 */
export function buildLineageForest(agents: readonly Agent[]): LineageNode[] {
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

  // ID tie-break makes ordering a pure function of (id, name) — not of input
  // array order — so equal-named siblings/roots always land in the same
  // position regardless of history. This matters for the layout cache
  // (topologySignature): the signature is order-independent, so the layout
  // it keys must be too, or a cache hit can draw a stale ordering for ties.
  const byName = (a: LineageNode, b: LineageNode) =>
    a.agent.name.localeCompare(b.agent.name) || compareIds(a.agent.id, b.agent.id);
  for (const node of byId.values()) {
    node.children.sort(byName);
  }
  roots.sort(byName);

  // Walk the forest, assigning depths. Dropping already-visited children as
  // we go turns any malformed cyclic ancestry into plain tree edges instead
  // of infinite recursion; nodes still unreachable afterward are handled
  // below by promoting one member per cycle.
  const visited = new Set<string>();
  const visit = (node: LineageNode, depth: number) => {
    if (visited.has(node.agent.id)) return;
    visited.add(node.agent.id);
    node.depth = depth;
    node.children = node.children.filter((c) => !visited.has(c.agent.id));
    for (const child of node.children) visit(child, depth + 1);
  };
  for (const root of roots) visit(root, 0);
  // Every unvisited node's parent exists and is itself unvisited (otherwise
  // the node would already be visited above), so walking up from it must
  // eventually repeat — that repeat is the cycle it hangs off, which may be
  // itself or an ancestor further up a tail. Promote only that cycle's
  // lowest-id member and let `visit` walk back down through it: this reaches
  // every real descendant via its existing `children` entry, dropping no
  // edge except the one into the promoted member. Starting points are
  // processed in id order (not input order) so promoted roots are appended
  // in a deterministic order, keeping the layout a pure function of
  // topologySignature's inputs.
  const unvisitedAscending = [...byId.values()]
    .filter((n) => !visited.has(n.agent.id))
    .sort((a, b) => compareIds(a.agent.id, b.agent.id));
  for (const node of unvisitedAscending) {
    if (visited.has(node.agent.id)) continue; // reached by an earlier promotion in this loop
    const path: LineageNode[] = [];
    const pathIndexById = new Map<string, number>();
    let cur = node;
    while (!pathIndexById.has(cur.agent.id)) {
      pathIndexById.set(cur.agent.id, path.length);
      path.push(cur);
      cur = byId.get(parentIdOf(cur.agent)!)!; // guaranteed to exist and be unvisited; see above
    }
    // typed local: slice() accepts undefined, so a bare "!" is a lint no-op
    const cycleStartIndex: number = pathIndexById.get(cur.agent.id)!;
    const cycle = path.slice(cycleStartIndex);
    const cycleRoot = cycle.reduce((min, n) =>
      compareIds(n.agent.id, min.agent.id) < 0 ? n : min
    );
    if (!visited.has(cycleRoot.agent.id)) {
      roots.push(cycleRoot);
      visit(cycleRoot, 0);
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
        ...edgeEndpoints(
          'vertical',
          { px: px(node), py: py(node) },
          { px: px(child), py: py(child) }
        ),
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
        ...edgeEndpoints('vertical', { px, py }, { px: target.px, py: target.py }),
        parentId: userKey(uid),
        childId: root.agent.id,
      });
    }
  }

  return { ...base, edges, users };
}

/**
 * Maps every node in `roots` to the agent id of the root of its tree, keyed
 * `root:<id>`. `computeStableLayout` uses this on the *old* agents to tell
 * which old tree a given (possibly since-removed) agent belonged to —
 * everything descended from one old root gets the same key regardless of
 * what the removal does to the tree's shape, which is what lets pieces
 * promoted from the same old tree be laid out and placed together instead of
 * competing as independent units (see the re-rooting branch).
 */
function oldTreeKeysOf(roots: LineageNode[]): Map<string, string> {
  const map = new Map<string, string>();
  for (const root of roots) {
    const unit = `root:${root.agent.id}`;
    const walk = (node: LineageNode): void => {
      map.set(node.agent.id, unit);
      for (const child of node.children) walk(child);
    };
    walk(root);
  }
  return map;
}

/**
 * Maps every node in `roots` (a *current* forest) to its placement unit:
 * every root's own unit — `userKey(rootUserOf(root))` when `showUsers` and
 * the root has one, else `oldTreeOf.get(root.id)` (so roots with no user,
 * or when users aren't shown, pack with whatever else came from the same old
 * tree) — propagated down to every descendant via the tree, not computed
 * independently per agent. That propagation matters: a non-root descendant
 * must always share its actual current root's unit, never its own
 * `rootUserOf`, or a node whose ancestry happens to name a different "user"
 * than the tree it's actually (still) attached to gets split into its own
 * spurious one-node group.
 */
function currentUnitsOf(
  roots: LineageNode[],
  showUsers: boolean,
  oldTreeOf: ReadonlyMap<string, string>
): Map<string, string> {
  const map = new Map<string, string>();
  for (const root of roots) {
    const uid = showUsers ? rootUserOf(root.agent) : undefined;
    const unit = uid ? userKey(uid) : oldTreeOf.get(root.agent.id)!;
    const walk = (node: LineageNode): void => {
      map.set(node.agent.id, unit);
      for (const child of node.children) walk(child);
    };
    walk(root);
  }
  return map;
}

/** Inverse of `userKey`: the user id from an edge/hover/unit key, or null if
 * `key` isn't one (i.e. it's a plain agent id). */
export function userIdFromKey(key: string): string | null {
  return key.startsWith('user:') ? key.slice('user:'.length) : null;
}

/** The canvas pixel size needed to contain every node/user, with padding. */
function layoutExtent(
  nodes: PositionedNode[],
  users: PositionedUser[]
): { width: number; height: number } {
  let width = PAD + NODE_W + PAD;
  let height = PAD + NODE_H + PAD;
  for (const n of nodes) {
    width = Math.max(width, n.px + NODE_W + PAD);
    height = Math.max(height, n.py + NODE_H + PAD);
  }
  for (const u of users) {
    width = Math.max(width, u.px + NODE_W + PAD);
    height = Math.max(height, u.py + NODE_H + PAD);
  }
  return { width, height };
}

/** What changed between two agent lists for `computeStableLayout` to key off. */
export interface PureRemoval {
  /** IDs present in the old list but not the new one. */
  removedIds: Set<string>;
  /** Surviving IDs whose direct parent is one of `removedIds` — these are
   * promoted to new roots by `buildLineageForest` and need a fresh position. */
  orphanedIds: Set<string>;
}

/**
 * Detects a "pure removal": `newAgents` is missing one or more IDs from
 * `oldAgents`, and every surviving agent has the same direct parent
 * (`parentIdOf`), root user (`rootUserOf`) and name as before — i.e. the only
 * topology input that changed is which IDs are present, not how the
 * survivors relate to each other. Returns null for any other kind of change:
 * an addition, a rename, or a reparent among survivors fail the per-agent
 * loop below; no change at all (identical lists) fails the length guard
 * first. None of those have a well-defined "unaffected" region worth
 * preserving, and are rare enough (or, for "no change", already handled by
 * the caller's own layout cache) that a full reflow is fine.
 */
export function detectPureRemoval(
  oldAgents: readonly Agent[],
  newAgents: readonly Agent[]
): PureRemoval | null {
  if (newAgents.length >= oldAgents.length) return null;
  const oldById = new Map(oldAgents.map((a) => [a.id, a]));
  const newIds = new Set(newAgents.map((a) => a.id));
  for (const a of newAgents) {
    const old = oldById.get(a.id);
    if (
      !old ||
      parentIdOf(old) !== parentIdOf(a) ||
      rootUserOf(old) !== rootUserOf(a) ||
      old.name !== a.name
    ) {
      return null;
    }
  }
  const removedIds = new Set<string>();
  for (const a of oldAgents) {
    if (!newIds.has(a.id)) removedIds.add(a.id);
  }
  const orphanedIds = new Set<string>();
  for (const a of newAgents) {
    const pid = parentIdOf(a);
    if (pid && removedIds.has(pid)) orphanedIds.add(a.id);
  }
  return { removedIds, orphanedIds };
}

/** The axis `computeStableLayout`'s placement step packs trees along: the one
 * the tidy-tree algorithm's shared leaf counter writes to — `px` before a
 * horizontal transpose, `py` after (see `transposeLayout`). */
function packAxis(orientation: Orientation, p: { px: number; py: number }): number {
  return orientation === 'horizontal' ? p.py : p.px;
}

/** The [min, max) extent a set of positioned rectangles spans along
 * `packAxis`, or null for an empty set. */
function packSpan(
  orientation: Orientation,
  positions: { px: number; py: number }[]
): { min: number; max: number } | null {
  if (positions.length === 0) return null;
  const size = orientation === 'horizontal' ? NODE_H : NODE_W;
  let min = Infinity;
  let max = -Infinity;
  for (const p of positions) {
    const a = packAxis(orientation, p);
    min = Math.min(min, a);
    max = Math.max(max, a + size);
  }
  return { min, max };
}

/** Shifts a positioned rectangle along `packAxis`. */
function packShift<T extends { px: number; py: number }>(
  orientation: Orientation,
  p: T,
  offset: number
): T {
  return orientation === 'horizontal' ? { ...p, py: p.py + offset } : { ...p, px: p.px + offset };
}

/** Shifts an edge's endpoints along `packAxis` (see `packShift`). */
function packShiftEdge(
  orientation: Orientation,
  e: PositionedEdge,
  offset: number
): PositionedEdge {
  return orientation === 'horizontal'
    ? { ...e, y1: e.y1 + offset, y2: e.y2 + offset }
    : { ...e, x1: e.x1 + offset, x2: e.x2 + offset };
}

/**
 * Builds the layout for `agents`, reusing `previous`'s pixel positions for
 * every node/user unaffected by what changed since `previous.agents`. This is
 * what keeps unrelated nodes from visibly jumping when an agent is deleted
 * elsewhere in the graph: `layoutForest`'s tidy-tree algorithm assigns leaf
 * x-slots with a single counter shared across every root in the forest, so
 * removing (or re-rooting) one node can renumber — and therefore reposition —
 * leaves in a completely unrelated tree laid out after it.
 *
 * Falls back to a full fresh, re-fit layout when there is no previous
 * layout, when `collapsedIds`/`showUsers`/`orientation`/`filterKey`
 * differ from what `previous.layout` was built with, or when
 * `detectPureRemoval` reports something other than a pure removal (see its
 * doc comment). `filterKey` lets a host distinguish "the data changed" (a
 * real delete — stays on the stable path) from "I'm looking at a different
 * subset of the same data" (a filter change — a fresh, re-fit layout, same as
 * before this function existed).
 *
 * Two removal shapes, dispatched below on `removal.orphanedIds.size`:
 * - Clean (no `orphanedIds`): every removed id was a leaf — see
 *   `stableCleanRemoval`.
 * - Re-rooting (`orphanedIds` non-empty): some old tree(s) must reflow — see
 *   `stableReRootingRemoval`.
 */
export function computeStableLayout(
  agents: Agent[],
  collapsedIds: ReadonlySet<string>,
  showUsers: boolean,
  orientation: Orientation,
  filterKey: string,
  previous: {
    agents: readonly Agent[];
    collapsedIds: ReadonlySet<string>;
    showUsers: boolean;
    orientation: Orientation;
    filterKey: string;
    layout: ForestLayout;
  } | null
): ForestLayout {
  const freshLayout = (): ForestLayout => {
    const forest = buildLineageForest(agents);
    pruneCollapsed(forest, collapsedIds);
    let layout = showUsers ? layoutForestWithUsers(forest) : layoutForest(forest);
    if (orientation === 'horizontal') layout = transposeLayout(layout);
    return layout;
  };

  if (!previous) return freshLayout();
  if (
    previous.collapsedIds !== collapsedIds ||
    previous.showUsers !== showUsers ||
    previous.orientation !== orientation ||
    previous.filterKey !== filterKey
  ) {
    return freshLayout();
  }
  const removal = detectPureRemoval(previous.agents, agents);
  if (!removal) return freshLayout();

  if (removal.orphanedIds.size === 0) {
    return stableCleanRemoval(previous.layout, removal.removedIds, orientation);
  }
  return stableReRootingRemoval(agents, collapsedIds, showUsers, orientation, previous, removal);
}

/**
 * The clean-removal path: every removed id was a leaf, so the remaining tree
 * needs no re-rooting and `previousLayout` is still valid as-is — the
 * removed nodes/edges are dropped in place and nothing else is recomputed, so
 * nothing else can move. A user left with no roots is dropped; one that
 * keeps some is recentred over them (its old midpoint can drift once a
 * sibling root is gone).
 */
function stableCleanRemoval(
  previousLayout: ForestLayout,
  removedIds: ReadonlySet<string>,
  orientation: Orientation
): ForestLayout {
  const nodes = previousLayout.nodes.filter((n) => !removedIds.has(n.agent.id));
  const nodeById = new Map(nodes.map((n) => [n.agent.id, n]));
  const liveEdges = previousLayout.edges.filter(
    (e) => !removedIds.has(e.parentId) && !removedIds.has(e.childId)
  );

  // Recenter each surviving user over its remaining root children: the old
  // midpoint can drift off the group once a sibling root is gone. Linear in
  // each root's packAxis position, so this matches what re-deriving from a
  // fresh layoutForestWithUsers call (then transposing) would give.
  const users = previousLayout.users.flatMap((u) => {
    const rootIds = liveEdges.filter((e) => e.parentId === userKey(u.id)).map((e) => e.childId);
    if (rootIds.length === 0) return []; // every root under this user is gone
    const roots = rootIds.map((id) => nodeById.get(id)!); // every id is a live edge's childId, always a surviving node
    const span = packSpan(orientation, roots)!; // roots is non-empty
    const center = (span.min + span.max - (orientation === 'horizontal' ? NODE_H : NODE_W)) / 2;
    return [orientation === 'horizontal' ? { ...u, py: center } : { ...u, px: center }];
  });
  const userById = new Map(users.map((u) => [u.id, u]));

  const edges = liveEdges.map((e) => {
    const uid = userIdFromKey(e.parentId);
    if (uid === null) return e;
    const user = userById.get(uid);
    const child = nodeById.get(e.childId);
    if (!user || !child) return e;
    return { ...e, ...edgeEndpoints(orientation, user, child) };
  });

  return { nodes, edges, users, ...layoutExtent(nodes, users) };
}

/**
 * The re-rooting path: some old tree(s) must reflow because a removed agent
 * had surviving children. A "unit" is a user's whole group when `showUsers`
 * (keyed by `userKey`), else everything descended from one *old* root tree
 * (keyed by `oldTreeKeysOf`, so pieces promoted from the same old tree are
 * placed together instead of competing for the same spot). Every unit
 * untouched by the removal is frozen at its exact previous pixels, same as
 * the clean case. Each *affected* unit is laid out on its own —
 * `buildLineageForest` + `pruneCollapsed` + `layoutForest[WithUsers]` on just
 * its surviving members, the same pipeline used for a full fresh layout, so
 * collapse state and user grouping can't drift — then anchored at its old
 * footprint. If it no longer fits there (it widened), it stays anchored
 * anyway, and everything from that footprint's old right edge onward —
 * frozen content, and any later affected unit — shifts right by exactly the
 * overflow: a uniform, order-preserving translation, the same thing a fresh
 * layout does when inserting one more slot, not a reshuffle or a relocation
 * to the far end. A unit with no old footprint at all (every member was
 * hidden by collapse before) is the one case with nothing to anchor to, and
 * is appended past whatever has been placed so far.
 */
function stableReRootingRemoval(
  agents: Agent[],
  collapsedIds: ReadonlySet<string>,
  showUsers: boolean,
  orientation: Orientation,
  previous: { agents: readonly Agent[]; layout: ForestLayout },
  removal: PureRemoval
): ForestLayout {
  // Grouping by the *old* tree (not the new root an agent ends up under) is
  // what keeps multiple pieces promoted from the same tree together as one
  // placement decision instead of competing for the same old footprint and
  // shoving each other off-screen.
  const oldForest = buildLineageForest(previous.agents);
  const oldTreeOf = oldTreeKeysOf(oldForest);
  const unitOf = currentUnitsOf(buildLineageForest(agents), showUsers, oldTreeOf);
  // Old-side units via the same propagation rule as the current side (not a
  // per-node `rootUserOf` check): an old unit's membership must be read off
  // the old tree the same way a current one is, or the two can disagree for
  // a non-root node whose own ancestry names a different "user" than the
  // tree it's attached to.
  const oldUnitOf = currentUnitsOf(oldForest, showUsers, oldTreeOf);

  // A unit is affected if any of its *current* members descended from an old
  // tree the removal touched — propagated via old-tree membership, not an
  // agent's own removal status. Two cases this catches that a removed
  // agent's own status wouldn't: a user's untouched roots are re-laid out
  // together with the rest of its group rather than dropped from it, and an
  // orphan whose removed parent had no ancestry of its own still gets its
  // (new) group recomputed instead of leaving a dangling edge.
  const touchedOldTrees = new Set<string>();
  for (const id of removal.removedIds) {
    const tree = oldTreeOf.get(id);
    if (tree !== undefined) touchedOldTrees.add(tree);
  }
  const affectedUnits = new Set<string>();
  for (const a of agents) {
    if (touchedOldTrees.has(oldTreeOf.get(a.id)!)) affectedUnits.add(unitOf.get(a.id)!);
  }
  // A removed old root's own user group can end up with no surviving member
  // at all (its whole subtree is gone, nothing promoted) — the loop above
  // never marks that case, since it only propagates from current survivors,
  // but the group's stale user card still needs to be dropped. Scoped to
  // `id` being an old root itself (`oldTreeOf.get(id) === "root:" + id`, not
  // just any removed descendant) so a removed non-root doesn't pull in an
  // unrelated group that merely shares its own ancestry-derived rootUserOf.
  if (showUsers) {
    const oldById = new Map(previous.agents.map((a) => [a.id, a]));
    for (const id of removal.removedIds) {
      if (oldTreeOf.get(id) === `root:${id}`) {
        const uid = rootUserOf(oldById.get(id)!);
        if (uid) affectedUnits.add(userKey(uid));
      }
    }
  }
  const isSurvivorAffected = (agentId: string): boolean => affectedUnits.has(unitOf.get(agentId)!);

  // Frozen (unaffected) content keeps its exact old position unless an
  // affected unit to its left widens past its old footprint, in which case
  // it — and everything else from that point on — shifts right by exactly
  // the overflow. Kept separate from `placedUnit*` below until the very end:
  // the cumulative shift a piece of frozen content needs depends on *every*
  // affected unit's overflow to its left, not just the nearest one, so it's
  // computed once from the final list of overflow boundaries rather than by
  // mutating these arrays once per affected unit.
  let frozenNodes = previous.layout.nodes.filter(
    (n) => !removal.removedIds.has(n.agent.id) && !isSurvivorAffected(n.agent.id)
  );
  let frozenUsers = previous.layout.users.filter((u) => !affectedUnits.has(userKey(u.id)));
  let frozenEdges = previous.layout.edges.filter(
    (e) =>
      !removal.removedIds.has(e.parentId) &&
      !removal.removedIds.has(e.childId) &&
      !isSurvivorAffected(e.childId)
  );
  const gap = orientation === 'horizontal' ? H_GAP_Y : GAP_X;

  const placedUnitNodes: PositionedNode[] = [];
  const placedUnitUsers: PositionedUser[] = [];
  const placedUnitEdges: PositionedEdge[] = [];

  // Affected units are processed in old-footprint order (left to right).
  // `pendingShift` is the total overflow accumulated from earlier units in
  // this pass: a unit's own anchor must account for it too, or two adjacent
  // affected units could both anchor at their (now stale) old positions and
  // overlap each other. `shiftBoundaries` records where each unit's own
  // overflow starts applying, so frozen content's final shift — computed
  // once, after this loop — is the sum of every boundary at or before its
  // own old position, not just the nearest one.
  const unitsInOldOrder = [...affectedUnits]
    .map((unit) => {
      const oldPositions = previous.layout.nodes.filter((n) => oldUnitOf.get(n.agent.id) === unit);
      const oldUser = previous.layout.users.find((u) => userKey(u.id) === unit);
      const oldSpan = packSpan(orientation, oldUser ? [...oldPositions, oldUser] : oldPositions);
      return { unit, oldSpan };
    })
    .sort(
      (a, b) =>
        (a.oldSpan?.min ?? Infinity) - (b.oldSpan?.min ?? Infinity) || compareIds(a.unit, b.unit)
    );

  let pendingShift = 0;
  const shiftBoundaries: { atOrAfter: number; amount: number }[] = [];

  for (const { unit, oldSpan } of unitsInOldOrder) {
    const unitAgents = agents.filter((a) => unitOf.get(a.id) === unit);
    // A unit can be "affected" with no current members at all: a removed old
    // root whose entire subtree is gone, nothing promoted (see the
    // user-group fallback above). Nothing to lay out or place — it just
    // disappears, which the frozen-content filters above already did.
    if (unitAgents.length === 0) continue;

    const unitForest = buildLineageForest(unitAgents);
    pruneCollapsed(unitForest, collapsedIds); // keep collapse state in sync with the normal pipeline
    let unitLayout = showUsers ? layoutForestWithUsers(unitForest) : layoutForest(unitForest);
    if (orientation === 'horizontal') unitLayout = transposeLayout(unitLayout);
    const unitSpan = packSpan(orientation, [...unitLayout.nodes, ...unitLayout.users])!;
    const width = unitSpan.max - unitSpan.min;

    let offset: number;
    if (oldSpan) {
      // Anchor at the old footprint, shifted right by whatever earlier
      // affected units in this same pass already pushed into this unit's old
      // territory — never append elsewhere while an old footprint exists.
      const anchor = oldSpan.min + pendingShift;
      offset = anchor - unitSpan.min;
      const overflow = width - (oldSpan.max - oldSpan.min);
      if (overflow > 0) {
        shiftBoundaries.push({ atOrAfter: oldSpan.max, amount: overflow });
        pendingShift += overflow;
      }
    } else {
      // No old position at all (every member was hidden by collapse before):
      // nothing to anchor to, so append past whatever is placed so far,
      // including frozen content's eventual shift.
      const placedSoFarSpan = packSpan(orientation, [...placedUnitNodes, ...placedUnitUsers]);
      const frozenSpan = packSpan(orientation, [...frozenNodes, ...frozenUsers]);
      const frontier = Math.max(
        placedSoFarSpan?.max ?? -Infinity,
        (frozenSpan?.max ?? -Infinity) + pendingShift
      );
      offset = (frontier === -Infinity ? 0 : frontier + gap) - unitSpan.min;
    }

    for (const n of unitLayout.nodes) placedUnitNodes.push(packShift(orientation, n, offset));
    for (const u of unitLayout.users) placedUnitUsers.push(packShift(orientation, u, offset));
    for (const e of unitLayout.edges) placedUnitEdges.push(packShiftEdge(orientation, e, offset));
  }

  const cumulativeShiftFor = (axisPos: number): number =>
    shiftBoundaries.reduce((total, b) => (axisPos >= b.atOrAfter ? total + b.amount : total), 0);
  frozenNodes = frozenNodes.map((n) => {
    const amount = cumulativeShiftFor(packAxis(orientation, n));
    return amount > 0 ? packShift(orientation, n, amount) : n;
  });
  frozenUsers = frozenUsers.map((u) => {
    const amount = cumulativeShiftFor(packAxis(orientation, u));
    return amount > 0 ? packShift(orientation, u, amount) : u;
  });
  frozenEdges = frozenEdges.map((e) => {
    const amount = cumulativeShiftFor(orientation === 'horizontal' ? e.y2 : e.x2);
    return amount > 0 ? packShiftEdge(orientation, e, amount) : e;
  });

  const nodes = [...frozenNodes, ...placedUnitNodes];
  const users = [...frozenUsers, ...placedUnitUsers];
  const edges = [...frozenEdges, ...placedUnitEdges];
  return { nodes, edges, users, ...layoutExtent(nodes, users) };
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
      ...edgeEndpoints('horizontal', parent, child),
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
