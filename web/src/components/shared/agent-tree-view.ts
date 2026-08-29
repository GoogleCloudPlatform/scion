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
 * Reusable inline agent tree/graph view component.
 *
 * Renders a list of agents as a parent/child lineage forest. The caller is
 * responsible for fetching and filtering agents; this component only handles
 * rendering, pan/zoom, hover-highlighting, and collapse/expand. Designed to
 * slot into the same content area as the grid/list views on the agents page
 * and project-detail page.
 *
 * Supports two flow directions via the `orientation` property/toolbar toggle:
 * 'vertical' (default, roots on top) and 'horizontal' (roots on the left,
 * depth increases to the right). Changing it dispatches an
 * `orientation-change` CustomEvent so host pages can persist the choice.
 *
 * Keyboard shortcuts (ignored while typing in inputs/dialogs/dropdowns or
 * when a modifier key is held): `t` transposes the orientation, `f` fits the graph
 * to the viewport, `+`/`=` zooms in, `-` zooms out.
 */

import { LitElement, html, css, svg, nothing } from 'lit';
import { customElement, property, state, query } from 'lit/decorators.js';
import type { Agent } from '../../shared/types.js';
import { getAgentDisplayStatus, can, isTerminalAvailable } from '../../shared/types.js';
import { getStateDisplay, type StatusVariant } from '../../shared/agent-state-display.js';
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
  type Orientation,
  type PositionedNode,
  type PositionedEdge,
  type PositionedUser,
} from '../../shared/lineage.js';
import type { StatusType } from './status-badge.js';
import './status-badge.js';
import { getMessageModeDisplay } from '../../shared/message-mode.js';
import './message-mode-badge.js';
import './quick-message-dialog.js';

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
 * Inline agent lineage graph component. Accepts an `agents` property (the
 * already-filtered list from the parent page) and renders it as an
 * interactive pan/zoom forest. All rendering state (hover, collapse,
 * pan/zoom, showUsers) is internal.
 */
@customElement('scion-agent-tree-view')
export class ScionAgentTreeView extends LitElement {
  /**
   * The agent list to render — filtering is the parent's responsibility.
   * When this changes the layout is recalculated but pan/zoom state is
   * preserved (so SSE updates don't reset your viewport).
   */
  @property({ attribute: false })
  agents: Agent[] = [];

  /**
   * Optional agent ID to center on when the view first renders with agents.
   * Used by the standalone /agents/graph page for deep linking.
   */
  @property({ type: String })
  focusId = '';

  /**
   * Flow direction of the lineage forest. Host pages may set it (e.g. from a
   * URL param); the toolbar toggle and the `t` shortcut update it in place
   * and dispatch `orientation-change` so hosts can stay in sync.
   */
  @property({ type: String })
  orientation: Orientation = 'vertical';

  @state() private showUsers = false;
  @state() private hoverId: string | null = null;
  @state() private collapsedIds: ReadonlySet<string> = new Set();
  @state() private scale = 1;
  @state() private panX = 0;
  @state() private panY = 0;
  @state() private quickMessageAgentId = '';
  @state() private quickMessageAgentName = '';
  @state() private quickMessageOpen = false;

  @query('.canvas') private canvasEl?: HTMLDivElement;

  private boundOnWheel = (e: WheelEvent) => this.onWheel(e);
  private boundOnKeyDown = (e: KeyboardEvent) => this.onKeyDown(e);
  /** Canvas-content size of the last computed layout (for keyboard "fit"). */
  private contentW = 0;
  private contentH = 0;
  private dragging = false;
  private dragStartX = 0;
  private dragStartY = 0;
  private dragPanX = 0;
  private dragPanY = 0;
  /** True once the initial fit-to-view / center-on-focus has fired. */
  private didAutoFit = false;

  static override styles = css`
    :host {
      display: block;
    }

    .toolbar {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      margin-bottom: 0.5rem;
      flex-wrap: wrap;
    }

    /* Segmented vertical/horizontal control, same visual language as
       <scion-view-toggle>. */
    .orientation-toggle {
      display: inline-flex;
      border: 1px solid var(--sl-color-neutral-300);
      border-radius: var(--sl-border-radius-medium, 6px);
      overflow: hidden;
    }

    .orientation-toggle button {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      width: 2rem;
      height: 2rem;
      border: none;
      padding: 0;
      background: var(--sl-color-neutral-0);
      color: var(--sl-color-neutral-600);
      cursor: pointer;
      transition: all 150ms ease;
    }

    .orientation-toggle button:not(:last-child) {
      border-right: 1px solid var(--sl-color-neutral-300);
    }

    .orientation-toggle button:hover:not(.active) {
      background: var(--sl-color-neutral-100);
    }

    .orientation-toggle button.active {
      background: var(--sl-color-primary-600);
      color: var(--sl-color-neutral-0);
    }

    .orientation-toggle sl-icon {
      font-size: 0.875rem;
    }

    /* Bootstrap Icons has no left→right tree glyph; rotate the top-down one
       so the root lands on the left. */
    .orientation-toggle sl-icon.rotated {
      transform: rotate(-90deg);
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
      user-select: none;
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
      transition:
        opacity 0.2s ease,
        stroke 0.2s ease;
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

    /*
     * Agent node wrapper — provides absolute positioning in the stage and a
     * stacking context for the collapse chip (which is a sibling of the <a>,
     * not a child, to satisfy the HTML spec: interactive content must not be
     * nested). Hover on the wrapper raises the whole card above its neighbours.
     */
    .node-wrapper {
      position: absolute;
    }

    .node-wrapper:hover {
      z-index: 2;
    }

    .node {
      position: relative; /* establishes stacking context for z-index */
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
    }

    .node.dim {
      opacity: 0.3;
    }

    /* Deep-linked node (?focus=<agent-id>): persistent ring so the agent the
       user jumped here for stands out. */
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

    /*
     * Collapse/expand chip — positioned relative to .node-wrapper (the nearest
     * positioned ancestor), not the <a> it used to live inside. The wrapper is
     * position:absolute, so bottom:-9px still hangs 9 px below the card.
     */
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

    /* Horizontal orientation: children grow to the right, so the chip hangs
       off the parent card's right-edge-center instead of bottom-center. */
    .collapse-chip.horizontal {
      bottom: auto;
      left: auto;
      right: -9px;
      top: 50%;
      transform: translateY(-50%);
    }

    /*
     * Terminal icon button — lower-right corner of the agent card.
     * Hidden by default; revealed on hover or keyboard focus-within so
     * the button is reachable without a pointer device.
     */
    .terminal-btn {
      position: absolute;
      bottom: 4px;
      right: 4px;
      z-index: 1;
      opacity: 0;
      transition: opacity 0.15s ease;
      font-size: 1rem;
      color: var(--sl-color-neutral-600);
    }

    .node-wrapper:hover .terminal-btn:not([disabled]),
    .node-wrapper:focus-within .terminal-btn:not([disabled]) {
      opacity: 1;
    }

    .node-wrapper:hover .terminal-btn[disabled],
    .node-wrapper:focus-within .terminal-btn[disabled] {
      opacity: 0.4;
    }

    .terminal-btn:hover {
      color: var(--sl-color-primary-600);
    }

    .terminal-btn[disabled] {
      pointer-events: none;
    }

    /*
     * Message icon button — lower-right corner, left of the terminal button.
     * Same hover-reveal pattern as .terminal-btn.
     */
    .message-btn {
      position: absolute;
      bottom: 4px;
      right: 28px;
      z-index: 1;
      opacity: 0;
      transition: opacity 0.15s ease;
      font-size: 1rem;
      color: var(--sl-color-neutral-600);
    }

    .node-wrapper:hover .message-btn,
    .node-wrapper:focus-within .message-btn {
      opacity: 1;
    }

    .message-btn:hover {
      color: var(--sl-color-primary-600);
    }

    /*
     * User (human) nodes are placed directly in the stage (no wrapper div),
     * so they keep position:absolute and use inline left/top for placement.
     */
    .node.user {
      position: absolute;
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

    .empty-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 0.75rem;
      padding: 3rem 1rem;
      color: var(--sl-color-neutral-600);
      text-align: center;
    }
  `;

  override connectedCallback(): void {
    super.connectedCallback();
    this.showUsers = localStorage.getItem('scion-graph-show-users') === 'true';
    // Wheel needs passive:false so we can preventDefault (prevent page zoom/scroll).
    this.addEventListener('wheel', this.boundOnWheel, { passive: false });
    window.addEventListener('keydown', this.boundOnKeyDown);
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    this.removeEventListener('wheel', this.boundOnWheel);
    window.removeEventListener('keydown', this.boundOnKeyDown);
  }

  override willUpdate(changedProperties: Map<PropertyKey, unknown>): void {
    super.willUpdate(changedProperties);
    // Flipping the flow direction swaps the canvas aspect ratio; re-fit so
    // the whole forest stays visible.
    if (
      changedProperties.has('orientation') &&
      changedProperties.get('orientation') !== undefined
    ) {
      this.didAutoFit = false;
    }
    if (changedProperties.has('agents')) {
      const oldAgents = changedProperties.get('agents') as Agent[] | undefined;
      // Re-fit when agents arrive for the first time or when the project
      // context switches (first agent's projectId changes). SSE status
      // updates on the same set don't reset the viewport.
      if (
        !oldAgents ||
        oldAgents.length === 0 ||
        this.agents.length === 0 ||
        oldAgents[0]?.projectId !== this.agents[0]?.projectId
      ) {
        this.didAutoFit = false;
      }
    }
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

  /** Centers the viewport on one node at 1:1 scale (for deep-link focus). */
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
    // Only pan from the background — keep node and control clicks working.
    for (const el of e.composedPath()) {
      if (el === this.canvasEl) break;
      const tag = (el as HTMLElement).tagName;
      if (tag === 'A' || tag === 'BUTTON' || tag === 'SL-BUTTON' || tag === 'SL-ICON-BUTTON')
        return;
    }
    e.preventDefault();
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
    // Toggling user nodes changes the canvas dimensions significantly; re-fit
    // so newly added user nodes (or freed space after hiding them) are visible.
    this.didAutoFit = false;
  }

  private setOrientation(orientation: Orientation): void {
    if (this.orientation === orientation) return;
    this.orientation = orientation;
    this.dispatchEvent(
      new CustomEvent('orientation-change', {
        detail: { orientation },
        bubbles: true,
        composed: true,
      })
    );
  }

  // --- Keyboard shortcuts ---------------------------------------------------

  /**
   * True when the keydown originated from a text-entry or overlay context
   * (inputs, textareas, selects, contenteditable, or anything inside a
   * Shoelace dialog/dropdown/input) — shortcuts must never fire while the
   * user is typing in the project filter or the quick-message dialog.
   */
  private isTextEntryTarget(e: KeyboardEvent): boolean {
    for (const el of e.composedPath()) {
      if (!(el instanceof HTMLElement)) continue;
      const tag = el.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || el.isContentEditable) {
        return true;
      }
      if (
        tag === 'SL-INPUT' ||
        tag === 'SL-TEXTAREA' ||
        tag === 'SL-SELECT' ||
        tag === 'SL-DIALOG' ||
        tag === 'SL-DROPDOWN'
      ) {
        return true;
      }
    }
    return false;
  }

  private onKeyDown(e: KeyboardEvent): void {
    if (!this.isConnected) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (this.isTextEntryTarget(e)) return;
    switch (e.key) {
      case 't':
      case 'T':
        this.setOrientation(this.orientation === 'vertical' ? 'horizontal' : 'vertical');
        break;
      case 'f':
      case 'F':
        if (this.contentW > 0 && this.contentH > 0) {
          this.fitToView(this.contentW, this.contentH);
        }
        break;
      case '+':
      case '=':
        this.zoomButtons(1.25);
        break;
      case '-':
        this.zoomButtons(1 / 1.25);
        break;
      default:
        return;
    }
    e.preventDefault();
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
      for (const a of agents) {
        if (rootUserOf(a) === uid) related.add(a.id);
      }
      return related;
    }

    const byId = new Map(agents.map((a) => [a.id, a]));
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
    // Walk down with BFS.
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

  private renderToolbar() {
    return html`
      <div class="toolbar">
        <sl-switch size="small" ?checked=${this.showUsers} @sl-change=${this.onShowUsersChange}
          >Show users</sl-switch
        >
        <div class="orientation-toggle" role="group" aria-label="Graph orientation">
          <button
            class=${this.orientation === 'vertical' ? 'active' : ''}
            title="Vertical layout (T)"
            @click=${() => this.setOrientation('vertical')}
          >
            <sl-icon name="diagram-3"></sl-icon>
          </button>
          <button
            class=${this.orientation === 'horizontal' ? 'active' : ''}
            title="Horizontal layout (T)"
            @click=${() => this.setOrientation('horizontal')}
          >
            <sl-icon name="diagram-3" class="rotated"></sl-icon>
          </button>
        </div>
      </div>
    `;
  }

  override render() {
    const agents = this.agents;
    if (agents.length === 0) {
      return html`
        ${this.renderToolbar()}
        <div class="empty-state">
          <sl-icon name="diagram-3"></sl-icon>
          <p>No agents to display.</p>
        </div>
      `;
    }

    const forest = buildLineageForest(agents);
    const hiddenCounts = descendantCounts(forest);
    pruneCollapsed(forest, this.collapsedIds);
    let layout = this.showUsers ? layoutForestWithUsers(forest) : layoutForest(forest);
    if (this.orientation === 'horizontal') {
      layout = transposeLayout(layout);
    }
    const { nodes, edges, users, width, height } = layout;
    this.contentW = width;
    this.contentH = height;
    const related = this.relatedIds(agents);

    // First render with content: center on the deep-linked agent if there is
    // one (and it survived filtering), otherwise fit the forest.
    // Only commit didAutoFit = true once the canvas has a non-zero size
    // (it can be 0 when the component is hidden or mid-CSS-transition), so
    // the fit retries on the next render rather than getting permanently
    // skipped.
    if (!this.didAutoFit) {
      const focus = this.focusId ? nodes.find((n) => n.agent.id === this.focusId) : undefined;
      const capturedW = width;
      const capturedH = height;
      requestAnimationFrame(() => {
        const canvas = this.canvasEl;
        if (canvas) {
          const rect = canvas.getBoundingClientRect();
          if (rect.width === 0 || rect.height === 0) {
            // Canvas not visible yet — leave didAutoFit = false so the next
            // render will retry.
            return;
          }
        }
        this.didAutoFit = true;
        if (focus) {
          this.centerOn(focus);
        } else {
          this.fitToView(capturedW, capturedH);
        }
      });
    }

    return html`
      ${this.renderToolbar()}
      <div
        class="canvas"
        @pointerdown=${this.onPointerDown}
        @pointermove=${this.onPointerMove}
        @pointerup=${this.onPointerUp}
        @pointercancel=${this.onPointerUp}
        @pointerleave=${() => (this.hoverId = null)}
      >
        <div
          class="stage"
          style="transform: translate(${this.panX}px, ${this.panY}px) scale(${this.scale})"
        >
          <svg width=${width} height=${height} aria-hidden="true">
            ${this.renderEdgeMarkers()} ${edges.map((e) => this.renderEdge(e, related))}
          </svg>
          ${users.map((u) => this.renderUserNode(u, agents, edges, related))}
          ${nodes.map((n) => this.renderNode(n, related, hiddenCounts))}
        </div>
        <div class="zoom-controls">
          <sl-button size="small" @click=${() => this.zoomButtons(1.25)} title="Zoom in (+)"
            >+</sl-button
          >
          <sl-button size="small" @click=${() => this.zoomButtons(1 / 1.25)} title="Zoom out (−)"
            >−</sl-button
          >
          <sl-button
            size="small"
            @click=${() => this.fitToView(width, height)}
            title="Fit to view (F)"
            >Fit</sl-button
          >
        </div>
      </div>

      <scion-quick-message-dialog
        agentId=${this.quickMessageAgentId}
        agentName=${this.quickMessageAgentName}
        ?open=${this.quickMessageOpen}
        @sl-request-close=${() => {
          this.quickMessageOpen = false;
        }}
      ></scion-quick-message-dialog>
    `;
  }

  /**
   * Arrowhead markers for the spawn direction (parent → child).
   */
  private renderEdgeMarkers() {
    const marker = (id: string, color: string) => svg`
      <marker
        id=${id}
        viewBox="0 0 8 8"
        markerWidth="8"
        markerHeight="8"
        refX="0.5"
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
    return svg`<path
      class="edge ${lit ? 'lit' : ''} ${dim ? 'dim' : ''}"
      d=${this.edgePath(e)}
    />`;
  }

  /**
   * Cubic sweep between the layout's edge anchors, finished with a short
   * straight run-in so the arrowhead always points along the flow axis.
   * Vertical: parent bottom-center → child top-center; horizontal: parent
   * right-edge-center → child left-edge-center. The stroke stops at the
   * arrowhead's BASE — the marker (refX near 0) extends past the path end so
   * the stroke can never poke out beyond the triangle's tip, which lands
   * exactly on the card's edge: touching, but never hidden under the card.
   */
  private edgePath(e: PositionedEdge): string {
    if (this.orientation === 'horizontal') {
      const midX = (e.x1 + e.x2) / 2;
      const xEnd = e.x2 - 6.5;
      const xApproach = xEnd - 4;
      return `M ${e.x1} ${e.y1} C ${midX} ${e.y1}, ${midX} ${e.y2}, ${xApproach} ${e.y2} L ${xEnd} ${e.y2}`;
    }
    const midY = (e.y1 + e.y2) / 2;
    const yEnd = e.y2 - 6.5;
    const yApproach = yEnd - 4;
    return `M ${e.x1} ${e.y1} C ${e.x1} ${midY}, ${e.x2} ${midY}, ${e.x2} ${yApproach} L ${e.x2} ${yEnd}`;
  }

  private toggleCollapse(agentId: string, e: Event): void {
    e.preventDefault();
    e.stopPropagation();
    const next = new Set(this.collapsedIds);
    if (!next.delete(agentId)) next.add(agentId);
    this.collapsedIds = next;
  }

  private renderNode(
    n: PositionedNode,
    related: Set<string> | null,
    hiddenCounts: Map<string, number>
  ) {
    const agent = n.agent;
    const status = getAgentDisplayStatus(agent);
    const color = VARIANT_COLOR[getStateDisplay(status).variant];
    const creator = agent.appliedConfig?.creatorName || agent.createdBy || '';
    const parentId = parentIdOf(agent);
    const isRoot = !parentId || !this.agents.some((a) => a.id === parentId);
    const dim = related !== null && !related.has(agent.id);
    const descendants = hiddenCounts.get(agent.id) ?? 0;
    const collapsed = this.collapsedIds.has(agent.id);
    // The <button> (collapse chip) must NOT be nested inside the <a> (node
    // link) — interactive content cannot be nested per the HTML spec and
    // causes accessibility and browser-behaviour problems. Instead, both are
    // children of a positioned wrapper div so the chip can still be
    // absolutely positioned relative to the card via the wrapper.
    return html`
      <div
        class="node-wrapper"
        style="left: ${n.px}px; top: ${n.py}px;"
        @pointerenter=${() => (this.hoverId = agent.id)}
        @pointerleave=${() => (this.hoverId = null)}
      >
        <a
          class="node ${dim ? 'dim' : ''} ${agent.id === this.focusId ? 'focus' : ''}"
          href="/agents/${agent.id}"
          style="border-left-color: ${color}"
          title=${`${agent.name}${agent.template ? ` — ${agent.template}` : ''}${isRoot && creator ? `\ncreated by ${creator}` : ''}`}
        >
          <span class="name">${agent.name}</span>
          <scion-status-badge
            status=${status as StatusType}
            label=${status}
            size="small"
          ></scion-status-badge>
          ${agent.template ? html`<span class="meta">${agent.template}</span>` : nothing}
          <span class="mode-icon" style="position: absolute; top: 4px; right: 6px; font-size: 14px; color: var(--sl-color-${getMessageModeDisplay(agent.messageMode).color}-600);">
            <sl-icon name=${getMessageModeDisplay(agent.messageMode).icon}></sl-icon>
          </span>
        </a>
        ${can(agent._capabilities, 'attach')
          ? html`
              <sl-icon-button
                class="message-btn"
                name="chat-dots"
                label="Message"
                @click=${(e: Event) => {
                  e.preventDefault();
                  e.stopPropagation();
                  this.quickMessageAgentId = agent.id;
                  this.quickMessageAgentName = agent.name;
                  this.quickMessageOpen = true;
                }}
              ></sl-icon-button>
            `
          : nothing}
        ${can(agent._capabilities, 'attach')
          ? html`
              <sl-icon-button
                class="terminal-btn"
                name="terminal"
                label="Terminal"
                href=${isTerminalAvailable(agent) ? `/agents/${agent.id}/terminal` : nothing}
                ?disabled=${!isTerminalAvailable(agent)}
              ></sl-icon-button>
            `
          : nothing}
        ${descendants > 0
          ? html`
              <button
                class="collapse-chip ${this.orientation === 'horizontal' ? 'horizontal' : ''}"
                title=${collapsed
                  ? `Expand ${descendants} hidden agent${descendants === 1 ? '' : 's'}`
                  : 'Collapse subtree'}
                @click=${(e: Event) => this.toggleCollapse(agent.id, e)}
              >
                ${collapsed ? `+${descendants}` : '−'}
              </button>
            `
          : nothing}
      </div>
    `;
  }

  private renderUserNode(
    u: PositionedUser,
    agents: Agent[],
    edges: PositionedEdge[],
    related: Set<string> | null
  ) {
    const key = userKey(u.id);
    let label = '';
    for (const a of agents) {
      if (a.ancestry?.length !== 1 || a.ancestry[0] !== u.id) continue;
      label = a.appliedConfig?.creatorName || a.createdBy || '';
      if (label) break;
    }
    if (!label) label = u.id.slice(0, 8);
    const started = edges.filter((e) => e.parentId === key).length;
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
    'scion-agent-tree-view': ScionAgentTreeView;
  }
}
