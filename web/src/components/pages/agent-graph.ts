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

import { LitElement, html, css, svg, nothing } from 'lit';
import { customElement, property, state, query } from 'lit/decorators.js';
import type { PageData, Agent } from '../../shared/types.js';
import { getAgentDisplayStatus } from '../../shared/types.js';
import { getStateDisplay, type StatusVariant } from '../../shared/agent-state-display.js';
import {
  buildLineageForest,
  descendantCounts,
  layoutForest,
  layoutForestWithUsers,
  parentIdOf,
  pruneCollapsed,
  rootUserOf,
  userKey,
  NODE_W,
  NODE_H,
  type PositionedNode,
  type PositionedEdge,
  type PositionedUser,
} from '../../shared/lineage.js';
import { apiFetch, extractApiError } from '../../client/api.js';
import { stateManager } from '../../client/state.js';
import type { StatusType } from '../shared/status-badge.js';
import type { ViewMode } from '../shared/view-toggle.js';
import '../shared/status-badge.js';
import '../shared/view-toggle.js';

const VARIANT_COLOR: Record<StatusVariant, string> = {
  success: 'var(--sl-color-success-600)',
  warning: 'var(--sl-color-warning-600)',
  danger: 'var(--sl-color-danger-600)',
  primary: 'var(--sl-color-primary-600)',
  neutral: 'var(--sl-color-neutral-400)',
};

const MIN_SCALE = 0.25;
const MAX_SCALE = 2.5;

/**
 * Agent lineage graph page: renders project agents as a parent/child forest
 * based on each agent's ancestry chain. Nodes are HTML cards (real anchors,
 * so client-side routing works); edges are drawn in an SVG underlay.
 * Supports pan (drag), zoom (wheel / buttons), and hover highlighting of the
 * hovered agent's lineage.
 */
@customElement('scion-page-agent-graph')
export class AgentGraphPage extends LitElement {
  @property({ type: Object }) pageData?: PageData;

  @state() private agents: Agent[] = [];
  @state() private loading = true;
  @state() private error: string | null = null;
  @state() private projectFilter = '';
  @state() private showUsers = false;
  @state() private hoverId: string | null = null;
  @state() private collapsedIds: ReadonlySet<string> = new Set();
  @state() private scale = 1;
  @state() private panX = 0;
  @state() private panY = 0;

  @query('.canvas') private canvasEl?: HTMLDivElement;

  private boundOnAgentsUpdated = () => this.onAgentsUpdated();
  private boundOnWheel = (e: WheelEvent) => this.onWheel(e);
  /** Agent to center and ring on load (?focus=<agent-id> deep link) */
  private focusId = '';
  private dragging = false;
  private dragStartX = 0;
  private dragStartY = 0;
  private dragPanX = 0;
  private dragPanY = 0;
  private didAutoFit = false;

  static override styles = css`
    :host {
      display: block;
      padding: var(--sl-spacing-large, 1.25rem);
    }

    .header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 1rem;
      margin-bottom: 1rem;
      flex-wrap: wrap;
    }

    .header h1 {
      margin: 0;
      font-size: 1.5rem;
    }

    .header-actions {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      flex-wrap: wrap;
    }

    .canvas {
      position: relative;
      overflow: hidden;
      border: 1px solid var(--sl-color-neutral-200);
      border-radius: var(--sl-border-radius-medium, 6px);
      background-color: var(--sl-color-neutral-0);
      background-image: radial-gradient(var(--sl-color-neutral-200) 1px, transparent 1px);
      background-size: 22px 22px;
      height: 70vh;
      min-height: 340px;
      cursor: grab;
      touch-action: none;
    }

    .canvas.dragging {
      cursor: grabbing;
    }

    .stage {
      position: absolute;
      top: 0;
      left: 0;
      transform-origin: 0 0;
    }

    .stage svg {
      position: absolute;
      top: 0;
      left: 0;
      overflow: visible;
      pointer-events: none;
    }

    .edge {
      stroke: var(--sl-color-neutral-400);
      stroke-width: 1.6;
      fill: none;
      transition: opacity 0.2s ease, stroke 0.2s ease;
      marker-end: url(#arrow-neutral);
    }

    .edge.dim {
      opacity: 0.15;
    }

    .edge.lit {
      stroke: var(--sl-color-primary-600);
      stroke-width: 2.2;
      marker-end: url(#arrow-lit);
    }

    .node {
      position: absolute;
      box-sizing: border-box;
      width: 180px;
      height: 76px;
      padding: 8px 10px;
      border: 1px solid var(--sl-color-neutral-300);
      border-left-width: 4px;
      border-radius: var(--sl-border-radius-medium, 6px);
      background: var(--sl-color-neutral-0);
      text-decoration: none;
      color: inherit;
      display: flex;
      flex-direction: column;
      justify-content: center;
      gap: 2px;
      transition:
        opacity 0.2s ease,
        box-shadow 0.15s ease,
        transform 0.15s ease;
      animation: node-in 0.35s ease both;
    }

    @keyframes node-in {
      from {
        opacity: 0;
        transform: translateY(-8px) scale(0.96);
      }
      to {
        opacity: 1;
        transform: translateY(0) scale(1);
      }
    }

    .node:hover {
      box-shadow: var(--sl-shadow-medium, 0 3px 10px rgba(0, 0, 0, 0.18));
      transform: translateY(-1px) scale(1.02);
      z-index: 2;
    }

    .node.dim {
      opacity: 0.3;
    }

    /* Deep-linked node (?focus=<agent-id>): persistent ring so the agent the
       user jumped here for stands out. The status color keeps the left edge
       (set inline), so only the remaining sides pick up the ring border. */
    .node.focus {
      border-color: var(--sl-color-primary-500);
      box-shadow: 0 0 0 3px var(--sl-color-primary-200);
    }

    .node .name {
      font-weight: 600;
      font-size: 0.95rem;
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    /* The whole card is a link to the agent detail page; underline the name
       on hover (same affordance as the list view) to make that visible. */
    .node:hover .name {
      text-decoration: underline;
    }

    .node .meta {
      font-size: 0.72rem;
      color: var(--sl-color-neutral-600);
      white-space: nowrap;
      overflow: hidden;
      text-overflow: ellipsis;
    }

    .node scion-status-badge {
      align-self: flex-start;
      max-width: 100%;
    }

    /* Collapse/expand toggle: small chip on the parent's bottom edge */
    .collapse-chip {
      position: absolute;
      bottom: -9px;
      left: 50%;
      transform: translateX(-50%);
      min-width: 18px;
      height: 18px;
      padding: 0 4px;
      box-sizing: border-box;
      border: 1px solid var(--sl-color-neutral-300);
      border-radius: 999px;
      background: var(--sl-color-neutral-0);
      color: var(--sl-color-neutral-600);
      font-size: 10px;
      line-height: 1;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      cursor: pointer;
      z-index: 1;
    }

    .collapse-chip:hover {
      border-color: var(--sl-color-primary-500);
      color: var(--sl-color-primary-600);
    }

    /* User (human) nodes: same footprint as agent nodes, distinct styling */
    .node.user {
      border-style: dashed;
      border-left-style: solid;
      border-left-color: var(--sl-color-neutral-400);
      background: var(--sl-color-neutral-50);
      cursor: default;
      justify-content: center;
    }

    .node.user .name {
      display: flex;
      align-items: center;
      gap: 6px;
    }

    .node.user .name sl-icon {
      font-size: 1rem;
      flex: none;
      color: var(--sl-color-neutral-600);
    }

    .zoom-controls {
      position: absolute;
      right: 10px;
      bottom: 10px;
      display: flex;
      gap: 4px;
      z-index: 3;
    }

    .empty-state,
    .loading-state,
    .error-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 0.75rem;
      padding: 3rem 1rem;
      color: var(--sl-color-neutral-600);
      text-align: center;
    }

    .error-state {
      color: var(--sl-color-danger-600);
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    stateManager.setScope({ type: 'dashboard' });

    const params = new URLSearchParams(window.location.search);
    this.projectFilter = params.get('project') || '';
    this.focusId = params.get('focus') || '';
    this.showUsers = localStorage.getItem('scion-graph-show-users') === 'true';

    const hydrated = stateManager.getAgents();
    if (hydrated.length > 0) {
      this.agents = hydrated;
      this.loading = false;
    } else {
      void this.loadAgents();
    }

    stateManager.addEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    // Wheel needs passive:false to preventDefault (browser page zoom/scroll).
    this.addEventListener('wheel', this.boundOnWheel, { passive: false });
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    stateManager.removeEventListener('agents-updated', this.boundOnAgentsUpdated as EventListener);
    this.removeEventListener('wheel', this.boundOnWheel);
    if (this.refetchTimer !== undefined) {
      window.clearTimeout(this.refetchTimer);
      this.refetchTimer = undefined;
    }
  }

  private refetchTimer: number | undefined;

  private onAgentsUpdated(): void {
    const updated = stateManager.getAgents();
    const prev = new Map(this.agents.map(a => [a.id, a]));
    let ancestryGap = false;
    // Every persisted agent has a non-empty ancestry (at minimum [userID]).
    // An empty chain means the update arrived via an event payload that
    // omitted it — keep the previously known chain, or quietly re-fetch.
    this.agents = updated.map(a => {
      if (a.ancestry && a.ancestry.length > 0) return a;
      const old = prev.get(a.id);
      if (old?.ancestry?.length) return { ...a, ancestry: old.ancestry };
      ancestryGap = true;
      return a;
    });
    if (ancestryGap && this.refetchTimer === undefined) {
      this.refetchTimer = window.setTimeout(() => {
        this.refetchTimer = undefined;
        void this.fetchAgents(true);
      }, 800);
    }
  }

  private async loadAgents(): Promise<void> {
    this.loading = true;
    this.error = null;
    try {
      await this.fetchAgents(false);
    } finally {
      this.loading = false;
    }
  }

  /** Fetches the agent list; quiet mode skips error state (background refresh). */
  private async fetchAgents(quiet: boolean): Promise<void> {
    try {
      const response = await apiFetch('/api/v1/agents');
      if (!response.ok) {
        throw new Error(await extractApiError(response, `HTTP ${response.status}: ${response.statusText}`));
      }
      const data = (await response.json()) as { agents?: Agent[] } | Agent[];
      this.agents = Array.isArray(data) ? data : data.agents || [];
      stateManager.seedAgents(this.agents);
    } catch (err) {
      console.error('Failed to load agents:', err);
      if (!quiet) {
        this.error = err instanceof Error ? err.message : 'Failed to load agents';
      }
    }
  }

  /** Agents after the project filter is applied */
  private get visibleAgents(): Agent[] {
    if (!this.projectFilter) return this.agents;
    return this.agents.filter(a => a.projectId === this.projectFilter);
  }

  private get projects(): Array<{ id: string; name: string }> {
    const seen = new Map<string, string>();
    for (const agent of this.agents) {
      if (agent.projectId && !seen.has(agent.projectId)) {
        seen.set(agent.projectId, agent.project || agent.projectId);
      }
    }
    return Array.from(seen, ([id, name]) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name));
  }

  private onProjectFilterChange(e: Event): void {
    const value = (e.target as HTMLSelectElement & { value: string }).value;
    this.projectFilter = value;
    this.didAutoFit = false;
    const url = new URL(window.location.href);
    if (value) {
      url.searchParams.set('project', value);
    } else {
      url.searchParams.delete('project');
    }
    window.history.replaceState({}, '', url);
  }

  // --- Pan & zoom -----------------------------------------------------------

  private onWheel(e: WheelEvent): void {
    const canvas = this.canvasEl;
    if (!canvas) return;
    const path = e.composedPath();
    if (!path.includes(canvas)) return;
    e.preventDefault();
    const factor = e.deltaY < 0 ? 1.12 : 1 / 1.12;
    this.zoomAround(e.clientX, e.clientY, factor);
  }

  private zoomAround(clientX: number, clientY: number, factor: number): void {
    const canvas = this.canvasEl;
    if (!canvas) return;
    const next = Math.min(MAX_SCALE, Math.max(MIN_SCALE, this.scale * factor));
    const applied = next / this.scale;
    if (applied === 1) return;
    const rect = canvas.getBoundingClientRect();
    const cx = clientX - rect.left;
    const cy = clientY - rect.top;
    this.panX = cx - applied * (cx - this.panX);
    this.panY = cy - applied * (cy - this.panY);
    this.scale = next;
  }

  private zoomButtons(factor: number): void {
    const canvas = this.canvasEl;
    if (!canvas) return;
    const rect = canvas.getBoundingClientRect();
    this.zoomAround(rect.left + rect.width / 2, rect.top + rect.height / 2, factor);
  }

  private fitToView(contentW: number, contentH: number): void {
    const canvas = this.canvasEl;
    if (!canvas) return;
    const rect = canvas.getBoundingClientRect();
    if (rect.width === 0 || rect.height === 0) return;
    const scale = Math.min(rect.width / contentW, rect.height / contentH, 1);
    this.scale = Math.max(MIN_SCALE, scale);
    this.panX = (rect.width - contentW * this.scale) / 2;
    this.panY = Math.max((rect.height - contentH * this.scale) / 2, 8);
  }

  /** Centers the viewport on one node at 1:1 scale (?focus= deep link). */
  private centerOn(n: PositionedNode): void {
    const canvas = this.canvasEl;
    if (!canvas) return;
    const rect = canvas.getBoundingClientRect();
    if (rect.width === 0 || rect.height === 0) return;
    this.scale = 1;
    this.panX = rect.width / 2 - (n.px + NODE_W / 2);
    this.panY = rect.height / 2 - (n.py + NODE_H / 2);
  }

  private onPointerDown(e: PointerEvent): void {
    // Only start panning from the background — keep node and control clicks
    // working. Walk the whole composed path instead of target.closest():
    // a click on sl-button lands inside its shadow root, where closest()
    // cannot see the host, and the pointer capture below would swallow the
    // button's click.
    for (const el of e.composedPath()) {
      if (el === this.canvasEl) break;
      const tag = (el as HTMLElement).tagName;
      if (tag === 'A' || tag === 'SL-BUTTON') return;
    }
    this.dragging = true;
    this.dragStartX = e.clientX;
    this.dragStartY = e.clientY;
    this.dragPanX = this.panX;
    this.dragPanY = this.panY;
    this.canvasEl?.setPointerCapture(e.pointerId);
    this.canvasEl?.classList.add('dragging');
  }

  private onPointerMove(e: PointerEvent): void {
    if (!this.dragging) return;
    this.panX = this.dragPanX + (e.clientX - this.dragStartX);
    this.panY = this.dragPanY + (e.clientY - this.dragStartY);
  }

  private onPointerUp(e: PointerEvent): void {
    this.dragging = false;
    this.canvasEl?.releasePointerCapture(e.pointerId);
    this.canvasEl?.classList.remove('dragging');
  }

  private onShowUsersChange(e: Event): void {
    this.showUsers = (e.target as HTMLInputElement & { checked: boolean }).checked;
    localStorage.setItem('scion-graph-show-users', String(this.showUsers));
    this.didAutoFit = false;
  }

  // --- Hover lineage highlight ---------------------------------------------

  /**
   * Keys related to the hovered node, or null when nothing is hovered.
   * Hovering an agent lights itself, its agent ancestors, its descendants,
   * and its originating user. Hovering a user lights every agent whose
   * lineage starts with that user. Everything else is dimmed.
   */
  private relatedIds(agents: Agent[]): Set<string> | null {
    if (!this.hoverId) return null;

    if (this.hoverId.startsWith('user:')) {
      const uid = this.hoverId.slice('user:'.length);
      const related = new Set<string>([this.hoverId]);
      // Every descendant inherits the chain's first entry, so a flat filter
      // is the full closure.
      for (const a of agents) {
        if (rootUserOf(a) === uid) related.add(a.id);
      }
      return related;
    }

    const byId = new Map(agents.map(a => [a.id, a]));
    const hovered = byId.get(this.hoverId);
    if (!hovered) return null;

    const related = new Set<string>([this.hoverId]);
    const rootUser = rootUserOf(hovered);
    if (rootUser) related.add(userKey(rootUser));
    // Walk up the ancestor chain.
    let cur: Agent | undefined = hovered;
    while (cur) {
      const pid = parentIdOf(cur);
      const parent = pid ? byId.get(pid) : undefined;
      if (!parent || related.has(parent.id)) break;
      related.add(parent.id);
      cur = parent;
    }
    // Walk down with BFS over a child adjacency map. Seeded from the
    // hovered agent only, so ancestors' other branches (the hovered
    // agent's siblings and cousins) stay dim.
    const childrenOf = new Map<string, string[]>();
    for (const a of agents) {
      const pid = parentIdOf(a);
      if (!pid) continue;
      const siblings = childrenOf.get(pid);
      if (siblings) {
        siblings.push(a.id);
      } else {
        childrenOf.set(pid, [a.id]);
      }
    }
    const queue = [hovered.id];
    for (let head = 0; head < queue.length; head++) {
      for (const childId of childrenOf.get(queue[head]) ?? []) {
        if (!related.has(childId)) {
          related.add(childId);
          queue.push(childId);
        }
      }
    }
    return related;
  }

  // --- Rendering ------------------------------------------------------------

  /** Grid/list picks from the toggle navigate back to the agents list. */
  private onViewChange(e: CustomEvent<{ view: ViewMode }>): void {
    const mode = e.detail.view;
    if (mode === 'graph') return;
    localStorage.setItem('scion-view-agents', mode);
    window.history.pushState({}, '', '/agents');
    window.dispatchEvent(new PopStateEvent('popstate'));
  }

  override render() {
    return html`
      <div class="header">
        <h1>Agents</h1>
        <div class="header-actions">
          <sl-select
            size="small"
            placeholder="All projects"
            clearable
            value=${this.projectFilter}
            @sl-change=${this.onProjectFilterChange}
            style="min-width: 180px"
          >
            ${this.projects.map(p => html`<sl-option value=${p.id}>${p.name}</sl-option>`)}
          </sl-select>
          <sl-switch
            size="small"
            ?checked=${this.showUsers}
            @sl-change=${this.onShowUsersChange}
          >Show users</sl-switch>
          <scion-view-toggle
            view="graph"
            @view-change=${this.onViewChange}
          ></scion-view-toggle>
        </div>
      </div>
      ${this.loading ? this.renderLoading() : this.error ? this.renderError() : this.renderGraph()}
    `;
  }

  private renderLoading() {
    return html`
      <div class="loading-state">
        <sl-spinner></sl-spinner>
        <p>Loading agents...</p>
      </div>
    `;
  }

  private renderError() {
    return html`
      <div class="error-state">
        <sl-icon name="exclamation-triangle"></sl-icon>
        <p>${this.error}</p>
        <sl-button size="small" @click=${() => this.loadAgents()}>Retry</sl-button>
      </div>
    `;
  }

  private renderGraph() {
    const agents = this.visibleAgents;
    if (agents.length === 0) {
      return html`
        <div class="empty-state">
          <sl-icon name="diagram-3"></sl-icon>
          <p>No agents to display${this.projectFilter ? ' in this project' : ''}.</p>
        </div>
      `;
    }

    const forest = buildLineageForest(agents);
    const hiddenCounts = descendantCounts(forest);
    pruneCollapsed(forest, this.collapsedIds);
    const { nodes, edges, users, width, height } = this.showUsers
      ? layoutForestWithUsers(forest)
      : layoutForest(forest);
    const related = this.relatedIds(agents);

    // First render with content: center on the deep-linked agent if there is
    // one (and it survived filtering), otherwise fit the forest.
    if (!this.didAutoFit) {
      this.didAutoFit = true;
      const focus = this.focusId ? nodes.find(n => n.agent.id === this.focusId) : undefined;
      requestAnimationFrame(() => (focus ? this.centerOn(focus) : this.fitToView(width, height)));
    }

    return html`
      <div
        class="canvas"
        @pointerdown=${this.onPointerDown}
        @pointermove=${this.onPointerMove}
        @pointerup=${this.onPointerUp}
        @pointercancel=${this.onPointerUp}
        @pointerleave=${() => (this.hoverId = null)}
      >
        <div class="stage" style="transform: translate(${this.panX}px, ${this.panY}px) scale(${this.scale})">
          <svg width=${width} height=${height} aria-hidden="true">
            ${this.renderEdgeMarkers()}
            ${edges.map(e => this.renderEdge(e, related))}
          </svg>
          ${users.map(u => this.renderUserNode(u, agents, edges, related))}
          ${nodes.map(n => this.renderNode(n, related, hiddenCounts))}
        </div>
        <div class="zoom-controls">
          <sl-button size="small" @click=${() => this.zoomButtons(1.25)} title="Zoom in">+</sl-button>
          <sl-button size="small" @click=${() => this.zoomButtons(1 / 1.25)} title="Zoom out">−</sl-button>
          <sl-button size="small" @click=${() => this.fitToView(width, height)} title="Fit to view">Fit</sl-button>
        </div>
      </div>
    `;
  }

  /**
   * Arrowhead markers for the spawn direction (parent → child). One marker
   * per edge style since marker fills don't inherit the path's stroke.
   * Rendered with svg`` — see the namespace note on renderEdge.
   */
  private renderEdgeMarkers() {
    const marker = (id: string, color: string) => svg`
      <marker
        id=${id}
        viewBox="0 0 8 8"
        markerWidth="7"
        markerHeight="7"
        refX="6.5"
        refY="4"
        orient="auto"
        markerUnits="userSpaceOnUse"
      >
        <path d="M 0 0.5 L 7 4 L 0 7.5 Z" fill=${color} />
      </marker>
    `;
    return svg`<defs>
      ${marker('arrow-neutral', 'var(--sl-color-neutral-400)')}
      ${marker('arrow-lit', 'var(--sl-color-primary-600)')}
    </defs>`;
  }

  private renderEdge(e: PositionedEdge, related: Set<string> | null) {
    const lit = related !== null && related.has(e.parentId) && related.has(e.childId);
    const dim = related !== null && !lit;
    const midY = (e.y1 + e.y2) / 2;
    const yEnd = e.y2 - 2; // leave room so the arrowhead tip meets the node edge
    // NOTE: svg`` (not html``) — nested fragments inside <svg> must be
    // created in the SVG namespace or the browser renders nothing.
    return svg`<path
      class="edge ${lit ? 'lit' : ''} ${dim ? 'dim' : ''}"
      d="M ${e.x1} ${e.y1} C ${e.x1} ${midY}, ${e.x2} ${midY}, ${e.x2} ${yEnd}"
    />`;
  }

  private toggleCollapse(agentId: string, e: Event): void {
    // The chip lives inside the node's <a>: keep the click from navigating.
    e.preventDefault();
    e.stopPropagation();
    const next = new Set(this.collapsedIds);
    if (!next.delete(agentId)) next.add(agentId);
    this.collapsedIds = next;
  }

  private renderNode(n: PositionedNode, related: Set<string> | null, hiddenCounts: Map<string, number>) {
    const agent = n.agent;
    const status = getAgentDisplayStatus(agent);
    const color = VARIANT_COLOR[getStateDisplay(status).variant];
    const creator = agent.appliedConfig?.creatorName || agent.createdBy || '';
    const parentId = parentIdOf(agent);
    const isRoot = !parentId || !this.visibleAgents.some(a => a.id === parentId);
    const dim = related !== null && !related.has(agent.id);
    const descendants = hiddenCounts.get(agent.id) ?? 0;
    const collapsed = this.collapsedIds.has(agent.id);
    return html`
      <a
        class="node ${dim ? 'dim' : ''} ${agent.id === this.focusId ? 'focus' : ''}"
        href="/agents/${agent.id}"
        style="left: ${n.px}px; top: ${n.py}px; border-left-color: ${color}"
        title=${`${agent.name}${agent.template ? ` — ${agent.template}` : ''}${isRoot && creator ? `\ncreated by ${creator}` : ''}`}
        @pointerenter=${() => (this.hoverId = agent.id)}
        @pointerleave=${() => (this.hoverId = null)}
      >
        <span class="name">${agent.name}</span>
        <scion-status-badge
          status=${status as StatusType}
          label=${status}
          size="small"
        ></scion-status-badge>
        ${agent.template ? html`<span class="meta">${agent.template}</span>` : nothing}
        ${descendants > 0 ? html`
          <button
            class="collapse-chip"
            title=${collapsed ? `Expand ${descendants} hidden agent${descendants === 1 ? '' : 's'}` : 'Collapse subtree'}
            @click=${(e: Event) => this.toggleCollapse(agent.id, e)}
          >${collapsed ? `+${descendants}` : '−'}</button>
        ` : nothing}
      </a>
    `;
  }

  private renderUserNode(
    u: PositionedUser,
    agents: Agent[],
    edges: PositionedEdge[],
    related: Set<string> | null
  ) {
    const key = userKey(u.id);
    // Display name: only agents the user created DIRECTLY (ancestry is just
    // [userID]) carry the human's name — on spawned descendants creatorName
    // is the spawning agent instead.
    let label = '';
    for (const a of agents) {
      if (a.ancestry?.length !== 1 || a.ancestry[0] !== u.id) continue;
      label = a.appliedConfig?.creatorName || a.createdBy || '';
      if (label) break;
    }
    if (!label) label = u.id.slice(0, 8);
    const started = edges.filter(e => e.parentId === key).length;
    const dim = related !== null && !related.has(key);
    return html`
      <div
        class="node user ${dim ? 'dim' : ''}"
        style="left: ${u.px}px; top: ${u.py}px"
        title=${`${label}\nstarted ${started} agent${started === 1 ? '' : 's'}`}
        @pointerenter=${() => (this.hoverId = key)}
        @pointerleave=${() => (this.hoverId = null)}
      >
        <span class="name"><sl-icon name="person-circle"></sl-icon> ${label}</span>
        <span class="meta">user</span>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-agent-graph': AgentGraphPage;
  }
}
