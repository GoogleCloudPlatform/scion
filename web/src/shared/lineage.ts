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
 * Maps every node in `roots` to a "placement unit" id. When `showUsers` is
 * on, every root sharing a `rootUserOf` is one unit, keyed by `userKey` —
 * matching `layoutForestWithUsers`'s own grouping, so a unit's fresh layout
 * always has exactly one user node and every root that belongs with it.
 * Otherwise (or for a root with no recorded user) every root is its own
 * unit, keyed `root:<id>`. `computeStableLayout` uses this both ways: on the
 * current agents, to know which whole block to reposition together; on the
 * old agents (always with `showUsers: false`, since the old side only needs
 * "which old tree", not grouping by user) to look up where a unit used to be.
 */
function unitIdsOf(roots: LineageNode[], showUsers: boolean): Map<string, string> {
  const map = new Map<string, string>();
  for (const root of roots) {
    const uid = showUsers ? rootUserOf(root.agent) : undefined;
    const unit = uid ? userKey(uid) : `root:${root.agent.id}`;
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
 * elsewhere in the graph (#2481): `layoutForest`'s tidy-tree algorithm
 * assigns leaf x-slots with a single counter shared across every root in the
 * forest, so removing (or re-rooting) one node can renumber — and therefore
 * reposition — leaves in a completely unrelated tree laid out after it.
 *
 * Falls back to a full fresh layout (the pre-#2481 behavior) when there is no
 * previous layout, when `collapsedIds`/`showUsers`/`orientation`/`filterKey`
 * differ from what `previous.layout` was built with, or when
 * `detectPureRemoval` reports something other than a pure removal (see its
 * doc comment). `filterKey` lets a host distinguish "the data changed" (a
 * real delete — stays on the stable path) from "I'm looking at a different
 * subset of the same data" (a filter change — a fresh, re-fit layout, same as
 * before this function existed).
 *
 * Two removal shapes:
 * - Clean (no `orphanedIds`): every removed id was a leaf, so the remaining
 *   tree needs no re-rooting and the previous layout is still valid as-is —
 *   the removed nodes/edges are dropped in place and nothing else is
 *   recomputed, so nothing else can move. A user left with no roots is
 *   dropped; one that keeps some is recentred over them (its old midpoint can
 *   drift once a sibling root is gone).
 * - Re-rooting (`orphanedIds` non-empty): some old tree(s) must reflow. Every
 *   "placement unit" (`unitIdsOf`: a user's whole group when `showUsers`, else
 *   a single root tree) untouched by the removal is frozen at its exact
 *   previous pixels, same as the clean case. Each *affected* unit is laid out
 *   on its own — `buildLineageForest` + `pruneCollapsed` + `layoutForest[WithUsers]`
 *   on just its surviving members, the same pipeline `freshLayout` below uses
 *   for everything, so collapse state and user grouping can't drift — then
 *   placed in its old footprint if that doesn't overlap any already-placed
 *   unit, otherwise appended just past the rightmost one so far.
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

  // --- Clean removal: every removed id was a leaf ---------------------------
  if (removal.orphanedIds.size === 0) {
    const nodes = previous.layout.nodes.filter((n) => !removal.removedIds.has(n.agent.id));
    const nodeById = new Map(nodes.map((n) => [n.agent.id, n]));
    const liveEdges = previous.layout.edges.filter(
      (e) => !removal.removedIds.has(e.parentId) && !removal.removedIds.has(e.childId)
    );

    // Recenter each surviving user over its remaining root children: the old
    // midpoint can drift off the group once a sibling root is gone. Linear in
    // each root's packAxis position, so this matches what re-deriving from a
    // fresh layoutForestWithUsers call (then transposing) would give.
    const users = previous.layout.users.flatMap((u) => {
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

  // --- Re-rooting removal ----------------------------------------------------
  // A "unit" is a user's whole group when showUsers (so a removal never
  // splits one user's roots across two recomputed pieces, which was the
  // source of duplicate user keys and dropped user→root edges), else a
  // single root tree. A unit is "affected" if a removed agent belonged to it
  // (by old root for a tree, by rootUserOf for a user group — a user group's
  // *set* of members never changes shape from promotion, only from an
  // actual removal). Every other unit is frozen at its exact previous pixels.
  const oldGroupOf = unitIdsOf(buildLineageForest(previous.agents), false);
  const oldById = new Map(previous.agents.map((a) => [a.id, a]));

  const affectedGroups = new Set<string>();
  for (const id of removal.removedIds) {
    const group = oldGroupOf.get(id);
    if (group !== undefined) affectedGroups.add(group);
  }
  const affectedUserIds = new Set<string>();
  if (showUsers) {
    for (const id of removal.removedIds) {
      const uid = rootUserOf(oldById.get(id)!);
      if (uid) affectedUserIds.add(uid);
    }
  }
  const unitOf = unitIdsOf(buildLineageForest(agents), showUsers);
  const isAffectedUnit = (unit: string): boolean => {
    const uid = userIdFromKey(unit);
    if (uid !== null) return affectedUserIds.has(uid);
    const group = oldGroupOf.get(unit.slice('root:'.length));
    return group !== undefined && affectedGroups.has(group);
  };
  const isSurvivorAffected = (agentId: string): boolean => isAffectedUnit(unitOf.get(agentId)!);

  const placedNodes: PositionedNode[] = previous.layout.nodes.filter(
    (n) => !removal.removedIds.has(n.agent.id) && !isSurvivorAffected(n.agent.id)
  );
  const placedUsers: PositionedUser[] = previous.layout.users.filter(
    (u) => !affectedUserIds.has(u.id)
  );
  const placedEdges: PositionedEdge[] = previous.layout.edges.filter(
    (e) => !removal.removedIds.has(e.childId) && !isSurvivorAffected(e.childId)
  );

  // Already-placed rectangles, for real interval-overlap testing — not just
  // "is there room to the right of everything" — so a re-rooted unit can
  // land back in its old footprint even when it isn't the rightmost thing on
  // the canvas (an earlier version only ever matched the rightmost case).
  const occupied: { min: number; max: number }[] = [
    ...placedNodes.map((n) => packSpan(orientation, [n])!),
    ...placedUsers.map((u) => packSpan(orientation, [u])!),
  ];
  const overlapsOccupied = (span: { min: number; max: number }): boolean =>
    occupied.some((o) => span.min < o.max && o.min < span.max);
  let frontier = occupied.reduce((max, o) => Math.max(max, o.max), -Infinity);
  const gap = orientation === 'horizontal' ? H_GAP_Y : GAP_X;

  const affectedUnits = new Set<string>();
  for (const a of agents) {
    const unit = unitOf.get(a.id)!;
    if (isAffectedUnit(unit)) affectedUnits.add(unit);
  }

  for (const unit of [...affectedUnits].sort(compareIds)) {
    const unitAgents = agents.filter((a) => unitOf.get(a.id) === unit);
    if (unitAgents.length === 0) continue; // every agent that was in this unit is gone

    const unitForest = buildLineageForest(unitAgents);
    pruneCollapsed(unitForest, collapsedIds); // keep collapse state in sync with the normal pipeline
    let unitLayout = showUsers ? layoutForestWithUsers(unitForest) : layoutForest(unitForest);
    if (orientation === 'horizontal') unitLayout = transposeLayout(unitLayout);
    const unitSpan = packSpan(orientation, [...unitLayout.nodes, ...unitLayout.users])!;

    const uid = userIdFromKey(unit);
    const oldPositions =
      uid !== null
        ? previous.layout.nodes.filter((n) => rootUserOf(n.agent) === uid)
        : previous.layout.nodes.filter(
            (n) => oldGroupOf.get(n.agent.id) === oldGroupOf.get(unitAgents[0].id)
          );
    const oldUser = uid !== null ? previous.layout.users.find((u) => u.id === uid) : undefined;
    const oldSpan = packSpan(orientation, oldUser ? [...oldPositions, oldUser] : oldPositions);

    const width = unitSpan.max - unitSpan.min;
    const candidate = oldSpan ? { min: oldSpan.min, max: oldSpan.min + width } : null;
    const offset =
      candidate && !overlapsOccupied(candidate)
        ? candidate.min - unitSpan.min
        : (frontier === -Infinity ? 0 : frontier + gap) - unitSpan.min;

    for (const n of unitLayout.nodes) placedNodes.push(packShift(orientation, n, offset));
    for (const u of unitLayout.users) placedUsers.push(packShift(orientation, u, offset));
    for (const e of unitLayout.edges) placedEdges.push(packShiftEdge(orientation, e, offset));
    const finalSpan = { min: unitSpan.min + offset, max: unitSpan.max + offset };
    occupied.push(finalSpan);
    frontier = Math.max(frontier, finalSpan.max);
  }

  return {
    nodes: placedNodes,
    edges: placedEdges,
    users: placedUsers,
    ...layoutExtent(placedNodes, placedUsers),
  };
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
