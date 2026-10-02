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
 * The "Jump to agent" palette for a page that shows agents as a graph
 * (`<scion-agent-tree-view>`): picking an agent brings its node into view
 * in place, instead of navigating anywhere.
 *
 * - Candidates are exactly the agents the rendered tree view shows (its
 *   `agents`, which the page has already filtered), kept current while the
 *   palette is open. Nothing is fetched and no filter is changed.
 * - The palette is available only while the page renders a tree view with
 *   at least one agent, outside any hidden subtree. A page that is loading,
 *   failed, empty, or showing another view does not offer it, and becoming
 *   unavailable closes it.
 * - Cmd+K / Ctrl+K toggles it, and the header's palette button opens it,
 *   unless another modal is open.
 * - A pick brings the agent's node into view at once (the tree view's
 *   `revealAgent`), and moves keyboard focus to it once the palette's close
 *   has settled, so the node, not the dialog's trigger, ends up focused.
 * - The palette element is mounted on `document.body`, outside the page's
 *   render root, so the page replacing its own template (e.g. loading to
 *   loaded) cannot remove it.
 */

import type { ReactiveController, ReactiveControllerHost } from 'lit';
import type { Agent } from '../../../shared/types.js';
import type { PaletteCandidate } from '../../../client/chat-palette-types.js';
import { buildAgentCandidate } from '../../../client/agent-palette-candidate.js';
import {
  GRAPH_PALETTE_OPEN_REQUEST_EVENT,
  setGraphPaletteAvailable,
} from '../../../client/graph-palette-events.js';
import type { ScionAgentTreeView } from '../agent-tree-view.js';
import { QuickPaletteHost, isQuickPaletteShortcut } from './quick-palette-host.js';

export interface GraphPaletteControllerOptions {
  /** The page's tree view, if it is rendered. */
  treeView: () => ScionAgentTreeView | null;
}

/** One Agents-group row per agent, in the given order. */
export function buildGraphAgentCandidates(agents: readonly Agent[]): PaletteCandidate[] {
  return agents.filter((agent) => agent.id).map((agent) => buildAgentCandidate(agent));
}

type Host = ReactiveControllerHost & HTMLElement;

/**
 * Whether two agent lists hold the same agent objects in the same order. A
 * page's filtered list is a new array on every render, so identity alone
 * would rebuild the candidates on each one.
 */
function sameAgents(a: readonly Agent[], b: readonly Agent[] | null): boolean {
  return b !== null && a.length === b.length && a.every((agent, i) => agent === b[i]);
}

export class GraphPaletteController implements ReactiveController {
  private readonly host: Host;
  private readonly options: GraphPaletteControllerOptions;
  /** Created on connect, once the host's render root exists; disposed on disconnect. */
  private palette: QuickPaletteHost | null = null;
  private available = false;
  private pickedAgentId: string | null = null;
  /** The tree view's agents the open palette's candidates were built from. */
  private candidateAgents: readonly Agent[] | null = null;
  /** Watches for an ancestor being hidden (e.g. the router hiding the page). */
  private hiddenObserver: MutationObserver | null = null;

  constructor(host: Host, options: GraphPaletteControllerOptions) {
    this.host = host;
    this.options = options;
    host.addController(this);
  }

  /** Whether the palette is open or opening. */
  get isOpen(): boolean {
    return this.palette?.isOpen ?? false;
  }

  /** Whether the palette can be opened right now. */
  get isAvailable(): boolean {
    return this.available;
  }

  hostConnected(): void {
    this.palette = new QuickPaletteHost({
      mount: document.body,
      label: 'Jump to agent',
      placeholder: 'Search agents…',
      load: (): Promise<PaletteCandidate[]> => {
        this.candidateAgents = this.shownAgents();
        return Promise.resolve(buildGraphAgentCandidates(this.candidateAgents));
      },
      onSelect: (target): void => {
        const revealed = this.options.treeView()?.revealAgent(target.agentId) ?? false;
        this.pickedAgentId = revealed ? target.agentId : null;
      },
      onSelectionSettled: (): void => {
        const id = this.pickedAgentId;
        this.pickedAgentId = null;
        if (id) this.options.treeView()?.focusAgentNode(id);
      },
    });
    document.addEventListener('keydown', this.handleKeydown);
    document.addEventListener(GRAPH_PALETTE_OPEN_REQUEST_EVENT, this.handleOpenRequest);
    this.hiddenObserver = new MutationObserver(() => this.sync());
    this.hiddenObserver.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ['hidden'],
      subtree: true,
    });
    this.sync();
  }

  hostUpdated(): void {
    this.sync();
  }

  hostDisconnected(): void {
    document.removeEventListener('keydown', this.handleKeydown);
    document.removeEventListener(GRAPH_PALETTE_OPEN_REQUEST_EVENT, this.handleOpenRequest);
    this.hiddenObserver?.disconnect();
    this.hiddenObserver = null;
    this.palette?.dispose();
    this.palette = null;
    this.pickedAgentId = null;
    this.candidateAgents = null;
    this.setAvailable(false);
  }

  /** Opens the palette if it is available and closed. */
  open(): void {
    if (!this.available || !this.palette || this.palette.isOpen) return;
    this.pickedAgentId = null;
    this.palette.open();
  }

  /** The agents the rendered tree view shows; empty while none is rendered. */
  private shownAgents(): readonly Agent[] {
    const tree = this.options.treeView();
    if (!tree?.isConnected || tree.closest('[hidden]')) return [];
    return tree.agents;
  }

  private sync(): void {
    const agents = this.shownAgents();
    const available = this.palette !== null && agents.length > 0 && !this.host.closest('[hidden]');
    if (!available) {
      this.palette?.hide();
    } else if (this.palette?.isOpen && !sameAgents(agents, this.candidateAgents)) {
      this.candidateAgents = agents;
      this.palette.setCandidates(buildGraphAgentCandidates(agents));
    }
    this.setAvailable(available);
  }

  private setAvailable(available: boolean): void {
    if (available === this.available) return;
    this.available = available;
    setGraphPaletteAvailable(this, available);
  }

  private readonly handleKeydown = (e: KeyboardEvent): void => {
    if (!this.available || !this.palette) return;
    if (!isQuickPaletteShortcut(e)) return;
    if (this.palette.isOpen) {
      e.preventDefault();
      this.palette.close();
      return;
    }
    if (this.palette.hasUnrelatedModalOpen()) return;
    e.preventDefault();
    this.open();
  };

  private readonly handleOpenRequest = (): void => {
    if (this.palette?.hasUnrelatedModalOpen()) return;
    this.open();
  };
}
