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
 * The graph pages' "Jump to agent" palette: each offers exactly the agents
 * its graph shows (after the page's own filters), only while the graph is
 * showing (not while the page is loading, failed or empty), and a pick
 * focuses the agent in the page's tree view.
 */

// @vitest-environment happy-dom

import {
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  it,
  vi,
  type MockInstance,
} from 'vitest';
import type { LitElement } from 'lit';
import type { Agent, PageData } from '../../shared/types.js';
import type { GraphPaletteController } from '../shared/palette/graph-palette-controller.js';
import type { ScionQuickPalette } from '../shared/palette/quick-palette.js';
import { stateManager } from '../../client/state.js';
import type { AgentListWindow } from '../../client/agent-list-window.js';

/** happy-dom has no EventSource; setScope opens one. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

type GraphPage = LitElement & {
  graphPalette: GraphPaletteController;
  pageData: PageData | null;
};

const PROJECT_ID = 'p1';

function agent(id: string, phase: Agent['phase'], projectId = PROJECT_ID): Agent {
  return {
    id,
    name: `Agent ${id}`,
    slug: id,
    projectId,
    project: `Project ${projectId}`,
    template: 't',
    phase,
  } as Agent;
}

const AGENTS = [
  agent('a1', 'running'),
  agent('a2', 'stopped'),
  agent('a3', 'running'),
  agent('b1', 'running', 'p2'),
];

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

let store: Map<string, string>;
let page: GraphPage | null = null;
let getAgents: MockInstance<typeof stateManager.getAgents>;

beforeAll(async () => {
  await Promise.all([
    import('./agents.js'),
    import('./agent-graph.js'),
    import('./project-detail.js'),
  ]);
}, 60_000);

beforeEach(() => {
  store = new Map();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
  vi.stubGlobal('EventSource', FakeEventSource);
  vi.spyOn(stateManager, 'setScope').mockImplementation(() => {});
  // With setScope stubbed no stream opens; a whole-set load waits for one.
  vi.spyOn(stateManager, 'sseConnected').mockResolvedValue(undefined);
  getAgents = vi.spyOn(stateManager, 'getAgents').mockReturnValue([]);
  vi.spyOn(stateManager, 'seedAgents').mockImplementation(() => {});
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (url.includes(`/api/v1/projects/${PROJECT_ID}/agents`)) {
        return Promise.resolve(jsonResponse({ agents: [], _capabilities: { actions: [] } }));
      }
      if (url.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
        return Promise.resolve(
          jsonResponse({
            id: PROJECT_ID,
            name: 'Project p1',
            slug: 'p1',
            _capabilities: { actions: ['read'] },
          })
        );
      }
      if (url.includes('/api/v1/agents')) return Promise.resolve(jsonResponse({ agents: [] }));
      return Promise.resolve(jsonResponse({}, 404));
    })
  );
});

afterEach(() => {
  page?.remove();
  page = null;
  document.body.replaceChildren();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

async function mount(tag: string, setup: (el: GraphPage) => void = () => {}): Promise<GraphPage> {
  const el = document.createElement(tag) as GraphPage;
  el.pageData = {
    path: '/',
    title: 'Agents',
    user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
  };
  setup(el);
  document.body.appendChild(el);
  page = el;
  await el.updateComplete;
  // Let the page's own initial loads settle before the test sets its state.
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

/**
 * Sets a page's private state, then waits for it to render. A page with an
 * agent window filters through the window's view state, so a phase filter
 * also goes there, as the page's own phase handler does.
 */
async function setState(el: GraphPage, state: Record<string, unknown>): Promise<void> {
  Object.assign(el, state);
  const win = (el as GraphPage & { agentWindow?: AgentListWindow }).agentWindow;
  if (win && 'phaseFilter' in state) {
    win.setViewState({ phaseFilter: state.phaseFilter as string });
  }
  el.requestUpdate();
  await el.updateComplete;
}

/** The palette element, wherever it is mounted. */
/** Fires `type` from the palette's own dialog, composed, as Shoelace does. */
function fireFromDialog(palette: ScionQuickPalette, type: 'sl-hide' | 'sl-after-hide'): void {
  palette
    .shadowRoot!.querySelector('sl-dialog')!
    .dispatchEvent(new Event(type, { bubbles: true, composed: true }));
}

function paletteEl(): ScionQuickPalette | null {
  return (
    document.querySelector<ScionQuickPalette>('scion-quick-palette') ??
    page?.renderRoot.querySelector<ScionQuickPalette>('scion-quick-palette') ??
    null
  );
}

/** Whether the page offers the palette; when it does not, the shortcut opens nothing. */
async function expectUnavailable(el: GraphPage): Promise<void> {
  expect(el.graphPalette.isAvailable).toBe(false);
  expect(el.renderRoot.querySelector('scion-agent-tree-view')).toBeNull();
  el.graphPalette.open();
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(el.graphPalette.isOpen).toBe(false);
}

async function openCandidateIds(el: GraphPage): Promise<string[]> {
  el.graphPalette.open();
  const palette = await vi.waitFor(
    () => {
      const p = paletteEl();
      if (!p?.open || p.groups.agents?.status !== 'ready') throw new Error('palette not ready');
      return p;
    },
    { timeout: 5000 }
  );
  return (palette.groups.agents?.candidates ?? []).map((c) =>
    c.target.kind === 'agent' ? c.target.agentId : ''
  );
}

function treeAgentIds(el: GraphPage): string[] {
  const tree = el.renderRoot.querySelector('scion-agent-tree-view');
  return (tree?.agents ?? []).map((a) => a.id);
}

async function expectPickFocusesTree(el: GraphPage, agentId: string): Promise<void> {
  const tree = el.renderRoot.querySelector('scion-agent-tree-view')!;
  const revealAgent = vi.spyOn(tree, 'revealAgent');
  const focusAgentNode = vi.spyOn(tree, 'focusAgentNode');
  if (!el.graphPalette.isOpen) await openCandidateIds(el);
  const palette = paletteEl()!;
  palette.dispatchEvent(
    new CustomEvent('palette-select', {
      detail: { target: { kind: 'agent', agentId, displayName: agentId } },
    })
  );
  expect(revealAgent).toHaveBeenCalledWith(agentId);
  fireFromDialog(palette, 'sl-after-hide');
  await vi.waitFor(() => expect(focusAgentNode).toHaveBeenCalledWith(agentId));
}

describe('/agents', () => {
  it('in graph mode offers only the agents the filtered graph shows', async () => {
    store.set('scion-view-agents', 'graph');
    const el = await mount('scion-page-agents');
    await setState(el, { agents: AGENTS, phaseFilter: 'running' });

    expect(el.graphPalette.isAvailable).toBe(true);
    const ids = await openCandidateIds(el);
    expect(ids.sort()).toEqual(['a1', 'a3', 'b1']);
    expect(ids.sort()).toEqual(treeAgentIds(el).sort());
    await expectPickFocusesTree(el, 'a3');
  });

  it('is not offered outside graph mode, and leaving graph mode closes it', async () => {
    store.set('scion-view-agents', 'graph');
    const el = await mount('scion-page-agents');
    await setState(el, { agents: AGENTS });
    await openCandidateIds(el);

    await setState(el, { viewMode: 'grid' });
    expect(el.graphPalette.isOpen).toBe(false);
    expect(el.graphPalette.isAvailable).toBe(false);

    await setState(el, { viewMode: 'list' });
    expect(el.graphPalette.isAvailable).toBe(false);
  });

  it('is not offered while loading, failed or empty, even with agents held', async () => {
    store.set('scion-view-agents', 'graph');
    const el = await mount('scion-page-agents');
    await setState(el, { agents: AGENTS, loading: true });
    await expectUnavailable(el);

    await setState(el, { loading: false, error: 'boom' });
    await expectUnavailable(el);

    await setState(el, { error: null, phaseFilter: 'error' });
    await expectUnavailable(el);

    await setState(el, { agents: [], phaseFilter: '' });
    await expectUnavailable(el);

    await setState(el, { agents: AGENTS });
    expect(el.graphPalette.isAvailable).toBe(true);
  });

  it('keeps the open palette in step with the graph', async () => {
    store.set('scion-view-agents', 'graph');
    const el = await mount('scion-page-agents');
    await setState(el, { agents: AGENTS });
    await openCandidateIds(el);

    await setState(el, { agents: AGENTS.filter((a) => a.id !== 'a2') });
    const ids = (paletteEl()!.groups.agents?.candidates ?? []).map((c) =>
      c.target.kind === 'agent' ? c.target.agentId : ''
    );
    expect(ids.sort()).toEqual(['a1', 'a3', 'b1']);
  });
});

describe('/agents/graph', () => {
  it('offers only the agents of the project filter', async () => {
    getAgents.mockReturnValue(AGENTS);
    const el = await mount('scion-page-agent-graph');
    await setState(el, { projectFilter: 'p2' });

    expect(el.graphPalette.isAvailable).toBe(true);
    expect(await openCandidateIds(el)).toEqual(['b1']);
    expect(treeAgentIds(el)).toEqual(['b1']);
    await expectPickFocusesTree(el, 'b1');
  });

  it('is not offered while loading, failed or empty', async () => {
    getAgents.mockReturnValue(AGENTS);
    const el = await mount('scion-page-agent-graph');
    await setState(el, { loading: true });
    await expectUnavailable(el);

    await setState(el, { loading: false, error: 'boom' });
    await expectUnavailable(el);

    await setState(el, { error: null });
    expect(el.graphPalette.isAvailable).toBe(true);

    await setState(el, { projectFilter: 'nonexistent' });
    expect(el.graphPalette.isAvailable).toBe(false);
  });
});

describe('project page', () => {
  async function mountProject(): Promise<GraphPage> {
    return mount('scion-page-project-detail', (el) => {
      (el as GraphPage & { projectId: string }).projectId = PROJECT_ID;
    });
  }

  it('in graph mode offers only the agents the filtered graph shows', async () => {
    store.set('scion-view-project-agents', 'graph');
    const el = await mountProject();
    const projectAgents = AGENTS.filter((a) => a.projectId === PROJECT_ID);
    await setState(el, { agents: projectAgents, phaseFilter: 'stopped' });

    expect(el.graphPalette.isAvailable).toBe(true);
    expect(await openCandidateIds(el)).toEqual(['a2']);
    expect(treeAgentIds(el)).toEqual(['a2']);
    await expectPickFocusesTree(el, 'a2');
  });

  it('is not offered outside graph mode, and leaving graph mode closes it', async () => {
    store.set('scion-view-project-agents', 'graph');
    const el = await mountProject();
    await setState(el, { agents: AGENTS.filter((a) => a.projectId === PROJECT_ID) });
    await openCandidateIds(el);

    await setState(el, { viewMode: 'grid' });
    expect(el.graphPalette.isOpen).toBe(false);
    expect(el.graphPalette.isAvailable).toBe(false);
  });

  it('is not offered while loading, failed or empty, even with agents held', async () => {
    store.set('scion-view-project-agents', 'graph');
    const el = await mountProject();
    const projectAgents = AGENTS.filter((a) => a.projectId === PROJECT_ID);
    await setState(el, { agents: projectAgents, loading: true });
    await expectUnavailable(el);

    await setState(el, { loading: false, error: 'boom' });
    await expectUnavailable(el);

    await setState(el, { error: null, phaseFilter: 'error' });
    await expectUnavailable(el);

    await setState(el, { agents: [], phaseFilter: '' });
    await expectUnavailable(el);

    await setState(el, { agents: projectAgents });
    expect(el.graphPalette.isAvailable).toBe(true);
  });

  it('opens again after the page swaps to its loading or error template and back', async () => {
    store.set('scion-view-project-agents', 'graph');
    const el = await mountProject();
    const projectAgents = AGENTS.filter((a) => a.projectId === PROJECT_ID);
    await setState(el, { agents: projectAgents });
    await openCandidateIds(el);
    el.graphPalette.open();

    // Loading -> main page, with the palette dismissed first.
    paletteEl()!.dispatchEvent(
      new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } })
    );
    await setState(el, { loading: true });
    await setState(el, { loading: false });
    expect(await openCandidateIds(el)).toEqual(['a1', 'a2', 'a3']);
    expect(paletteEl()!.isConnected).toBe(true);

    // Open over the main page, then error -> Retry -> main page.
    await setState(el, { error: 'boom' });
    expect(el.graphPalette.isOpen).toBe(false);
    await setState(el, { error: null });
    expect(await openCandidateIds(el)).toEqual(['a1', 'a2', 'a3']);
    await expectPickFocusesTree(el, 'a3');
  });
});

describe('tree view hosts with a complete set', () => {
  it('/agents and the project page leave the ancestor-not-loaded marker off', async () => {
    store.set('scion-view-agents', 'graph');
    const agentsPage = await mount('scion-page-agents');
    await setState(agentsPage, { agents: AGENTS });
    const agentsTree = agentsPage.renderRoot.querySelector('scion-agent-tree-view');
    expect(agentsTree).not.toBeNull();
    expect(agentsTree?.markMissingAncestors).toBe(false);
    agentsPage.remove();

    store.set('scion-view-project-agents', 'graph');
    const projectPage = await mount('scion-page-project-detail', (el) => {
      (el as GraphPage & { projectId: string }).projectId = PROJECT_ID;
    });
    await setState(projectPage, { agents: AGENTS.filter((a) => a.projectId === PROJECT_ID) });
    const projectTree = projectPage.renderRoot.querySelector('scion-agent-tree-view');
    expect(projectTree).not.toBeNull();
    expect(projectTree?.markMissingAncestors).toBe(false);
  });
});
