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

import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Agent } from '../../shared/types.js';
import {
  FILTERS_QUERY_DIALECT_FLAG,
  getViewSchema,
  knownKeysFor,
  parse,
  type Query,
  type ViewSchema,
} from '../../shared/filter-query/index.js';
import {
  agentToGraphFilterRow,
  extractSingleProjectScope,
  filterGraphAgents,
  makeGraphValueSuggestions,
  migrateProjectParamToQuery,
  resolveProjectLoadTarget,
} from './agent-graph-filter.js';
import type { SeededDrainResult } from '../../client/agent-drain.js';
import { setFeatureFlag, resetServerFlagStateForTests } from '../../utils/feature-flags.js';
import { stateManager } from '../../client/state.js';
import type { ScionAgentTreeView } from '../shared/agent-tree-view.js';
import type { AgentGraphPage } from './agent-graph.js';

const GRAPH_SCHEMA = getViewSchema('graph') as ViewSchema;

function parseGraph(input: string): Query {
  const res = parse(input, knownKeysFor(GRAPH_SCHEMA));
  if (res.error || !res.query) {
    throw new Error(`Unexpected parse error for ${input}: ${res.error?.message}`);
  }
  return res.query;
}

function mkAgent(overrides: Partial<Agent> & { id: string }): Agent {
  return {
    name: `Agent ${overrides.id}`,
    slug: overrides.id,
    projectId: 'p-1',
    project: 'Alpha',
    projectSlug: 'alpha',
    template: 'developer',
    phase: 'running',
    messageMode: 'project',
    ...overrides,
  };
}

describe('agent-graph-filter pure helpers', () => {
  const root = mkAgent({
    id: 'root',
    name: 'Coordinator',
    slug: 'coord',
    phase: 'stopped',
    activity: 'completed',
    harnessConfig: 'claude',
    harnessVendor: 'anthropic',
    template: 'coordinator',
    labels: { team: 'core' },
    messageMode: 'direct',
    _messageability: { canMessage: true, canReachViewer: true },
  });
  const mid = mkAgent({
    id: 'mid',
    name: 'Lead',
    slug: 'lead',
    phase: 'suspended',
    harnessConfig: 'gemini-cli',
    harnessVendor: 'google',
    ancestry: ['user:u1', 'root'],
    labels: { team: 'ui', env: 'staging' },
    messageMode: 'owner',
    _messageability: { canMessage: false, canReachViewer: false },
  });
  const leaf = mkAgent({
    id: 'leaf',
    name: 'Worker',
    slug: 'worker-1',
    phase: 'running',
    activity: 'working',
    harnessConfig: 'antigravity',
    harnessVendor: 'google',
    ancestry: ['user:u1', 'root', 'mid'],
    labels: { team: 'ui' },
  });
  const other = mkAgent({
    id: 'other',
    name: 'Other Branch',
    slug: 'other-1',
    projectId: 'p-2',
    project: 'Beta',
    projectSlug: 'beta',
    phase: 'stopped',
    harnessConfig: 'gemini-cli',
    harnessVendor: 'google',
    ancestry: ['user:u1', 'root'],
  });
  const unrelated = mkAgent({
    id: 'unrelated',
    name: 'Standalone',
    slug: 'standalone',
    projectId: 'p-2',
    project: 'Beta',
    projectSlug: 'beta',
    phase: 'error',
    harnessConfig: 'codex',
    harnessVendor: 'openai',
  });

  const all = [root, mid, leaf, other, unrelated];

  it('matches phase:running with dimmed ancestor chain and hides non-matching non-ancestors', () => {
    const res = filterGraphAgents(all, parseGraph('phase:running'));
    expect(res.hasActiveFilter).toBe(true);
    expect(res.agents.map((a) => a.id)).toEqual(['root', 'mid', 'leaf']);
    expect(Array.from(res.contextIds).sort()).toEqual(['mid', 'root']);
  });

  it('matches harness:antigravity using harnessConfig and never harnessVendor', () => {
    const res = filterGraphAgents(all, parseGraph('harness:antigravity'));
    expect(res.agents.map((a) => a.id)).toEqual(['root', 'mid', 'leaf']);
    expect(Array.from(res.contextIds).sort()).toEqual(['mid', 'root']);

    // harnessVendor ("google") must not match harness:
    const vendorRes = filterGraphAgents(all, parseGraph('harness:google'));
    expect(vendorRes.agents).toEqual([]);
    expect(vendorRes.contextIds.size).toBe(0);
    expect(vendorRes.hasActiveFilter).toBe(true);
  });

  it('does not put matching ancestors into contextIds', () => {
    const res = filterGraphAgents(all, parseGraph('label:team=ui'));
    // mid and leaf match; root is non-matching ancestor
    expect(res.agents.map((a) => a.id)).toEqual(['root', 'mid', 'leaf']);
    expect(Array.from(res.contextIds)).toEqual(['root']);
  });

  it('supports activity, template, project (slug/id/name), mode, and text', () => {
    expect(filterGraphAgents(all, parseGraph('activity:working')).agents.map((a) => a.id)).toEqual([
      'root',
      'mid',
      'leaf',
    ]);
    expect(
      filterGraphAgents(all, parseGraph('template:coordinator')).agents.map((a) => a.id)
    ).toEqual(['root']);
    expect(filterGraphAgents(all, parseGraph('project:beta')).agents.map((a) => a.id)).toEqual([
      'root',
      'other',
      'unrelated',
    ]);
    expect(filterGraphAgents(all, parseGraph('project:p-2')).agents.map((a) => a.id)).toEqual([
      'root',
      'other',
      'unrelated',
    ]);
    expect(filterGraphAgents(all, parseGraph('mode:can_message')).agents.map((a) => a.id)).toEqual([
      'root',
    ]);
    expect(
      filterGraphAgents(all, parseGraph('mode:cannot_message')).agents.map((a) => a.id)
    ).toEqual(['root', 'mid']);
    expect(filterGraphAgents(all, parseGraph('Standalone')).agents.map((a) => a.id)).toEqual([
      'unrelated',
    ]);
  });

  it('returns all agents with no contextIds when query is empty', () => {
    const res = filterGraphAgents(all, null);
    expect(res.hasActiveFilter).toBe(false);
    expect(res.agents.map((a) => a.id)).toEqual(all.map((a) => a.id));
    expect(res.contextIds.size).toBe(0);
  });

  it('provides dynamic value suggestions from loaded agents', () => {
    const suggest = makeGraphValueSuggestions(() => all);
    expect(suggest('harness', '')).toEqual(['antigravity', 'claude', 'codex', 'gemini-cli']);
    expect(suggest('project', '')).toEqual(['alpha', 'beta']);
    expect(suggest('template', '')).toEqual(['coordinator', 'developer']);
    expect(suggest('label', '')).toContain('team=ui');
    expect(suggest('label', '')).toContain('team');
  });

  it('migrates ?project= to ?q=project:... and extracts single project scope', () => {
    expect(migrateProjectParamToQuery(new URLSearchParams('project=p-1'))).toEqual({
      queryText: 'project:p-1',
      migrated: true,
      initialProject: 'p-1',
    });
    expect(migrateProjectParamToQuery(new URLSearchParams('project=my+proj'))).toEqual({
      queryText: 'project:"my proj"',
      migrated: true,
      initialProject: 'my proj',
    });
    expect(migrateProjectParamToQuery(new URLSearchParams('project=p-1&q=phase:running'))).toEqual({
      queryText: 'project:p-1 phase:running',
      migrated: true,
      initialProject: 'p-1',
    });
    expect(extractSingleProjectScope(parseGraph('project:p-1 phase:running'))).toBe('p-1');
    expect(extractSingleProjectScope(parseGraph('project:p-1,p-2'))).toBeNull();
    expect(extractSingleProjectScope(parseGraph('-project:p-1'))).toBeNull();
    expect(agentToGraphFilterRow(leaf).text()).toEqual(['Worker', 'worker-1']);
  });
});

describe('scion-agent-tree-view context dimming & filtered empty state', () => {
  let tree: ScionAgentTreeView | null = null;

  beforeAll(async () => {
    await import('../shared/agent-tree-view.js');
  });

  afterEach(() => {
    tree?.remove();
    tree = null;
  });

  it('dims context nodes with aria-description while keeping them focusable and clickable', async () => {
    const parent = mkAgent({ id: 'p', name: 'Parent', phase: 'stopped' });
    const child = mkAgent({
      id: 'c',
      name: 'Child',
      phase: 'running',
      ancestry: ['user:u1', 'p'],
    });
    tree = document.createElement('scion-agent-tree-view') as ScionAgentTreeView;
    tree.agents = [parent, child];
    tree.contextIds = new Set(['p']);
    document.body.appendChild(tree);
    await tree.updateComplete;

    const parentLink = tree.shadowRoot!.querySelector<HTMLAnchorElement>(
      'a.node[data-agent-id="p"]'
    );
    const childLink = tree.shadowRoot!.querySelector<HTMLAnchorElement>(
      'a.node[data-agent-id="c"]'
    );
    expect(parentLink).not.toBeNull();
    expect(parentLink!.classList.contains('context')).toBe(true);
    expect(parentLink!.getAttribute('aria-description')).toBe(
      'shown as context; does not match filter'
    );
    expect(parentLink!.getAttribute('href')).toBe('/agents/p');

    expect(childLink).not.toBeNull();
    expect(childLink!.classList.contains('context')).toBe(false);
    expect(childLink!.hasAttribute('aria-description')).toBe(false);
  });

  it('renders filtered empty state with Clear filter button that dispatches clear-filter', async () => {
    tree = document.createElement('scion-agent-tree-view') as ScionAgentTreeView;
    tree.agents = [];
    tree.filteredEmpty = true;
    const onClear = vi.fn();
    tree.addEventListener('clear-filter', onClear);
    document.body.appendChild(tree);
    await tree.updateComplete;

    const empty = tree.shadowRoot!.querySelector('.empty-state');
    expect(empty?.textContent).toContain('No agents match the current filter.');
    const btn = empty?.querySelector('sl-button') as HTMLElement | null;
    expect(btn).not.toBeNull();
    btn!.click();
    expect(onClear).toHaveBeenCalledTimes(1);
  });
});

describe('scion-page-agent-graph filter dialect integration', () => {
  let page: AgentGraphPage | null = null;

  beforeAll(async () => {
    await import('./agent-graph.js');
  });

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.spyOn(stateManager, 'setScope').mockImplementation(() => {});
    vi.spyOn(stateManager, 'sseConnected').mockResolvedValue(undefined);
    vi.spyOn(stateManager, 'isAgentSetComplete').mockReturnValue(true);
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({}), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
  });

  afterEach(() => {
    page?.remove();
    page = null;
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    document.body.replaceChildren();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  const parent = mkAgent({
    id: 'a-parent',
    name: 'Parent',
    phase: 'stopped',
    harnessConfig: 'claude',
  });
  const child = mkAgent({
    id: 'a-child',
    name: 'Child',
    phase: 'running',
    harnessConfig: 'antigravity',
    ancestry: ['user:u1', 'a-parent'],
  });
  const other = mkAgent({
    id: 'a-other',
    name: 'Other',
    phase: 'stopped',
    harnessConfig: 'codex',
  });

  it('keeps legacy select, URL, and palette selection unchanged when filters.query_dialect is off', async () => {
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([parent, child, other]);
    window.history.replaceState({}, '', '/agents/graph');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    document.body.appendChild(page);
    await page.updateComplete;

    expect(page.shadowRoot!.querySelector('scion-filter-box')).toBeNull();
    expect(page.shadowRoot!.querySelector('sl-select')).not.toBeNull();

    // With experiment off, selecting a real candidate whose revealAgent returns
    // false must not fall through to onSelectHidden or call focusAgentNode.
    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    const revealSpy = vi.spyOn(tree, 'revealAgent').mockReturnValue(false);
    const focusSpy = vi.spyOn(tree, 'focusAgentNode');
    page.graphPalette.open();
    const palette = await vi.waitFor(() => {
      const p = document.querySelector('scion-quick-palette') as
        | (HTMLElement & { open?: boolean; groups?: { agents?: { status?: string } } })
        | null;
      if (!p?.open || p.groups?.agents?.status !== 'ready') throw new Error('palette not ready');
      return p;
    });
    palette.dispatchEvent(
      new CustomEvent('palette-select', {
        detail: { target: { kind: 'agent', agentId: 'a-other', displayName: 'Other' } },
      })
    );
    expect(revealSpy).toHaveBeenCalledWith('a-other');
    palette
      .shadowRoot!.querySelector('sl-dialog')!
      .dispatchEvent(new Event('sl-after-hide', { bubbles: true, composed: true }));
    await new Promise((r) => setTimeout(r, 0));
    expect(focusSpy).not.toHaveBeenCalled();
  });

  it('migrates ?project= to ?q=project:... and filters with dimmed context when dialect is on', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([parent, child, other]);
    window.history.replaceState({}, '', '/agents/graph?project=p-1');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    document.body.appendChild(page);
    await page.updateComplete;

    expect(window.location.search).toBe('?q=project%3Aalpha');
    const box = page.shadowRoot!.querySelector('scion-filter-box');
    expect(box).not.toBeNull();
    expect(page.shadowRoot!.querySelector('sl-select')).toBeNull();

    // Typing invalid or un-committed text does not churn filterKey.
    const keyBeforeInput = (
      page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView
    ).filterKey;
    box!.dispatchEvent(
      new CustomEvent('filter-input', {
        detail: {
          value: 'project:alpha phase:',
          query: parseGraph('project:alpha'),
          valid: false,
        },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;
    expect(
      (page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView).filterKey
    ).toBe(keyBeforeInput);

    // Filter by harness:antigravity -> child matched, parent dimmed context, other hidden
    box!.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: {
          value: 'harness:antigravity',
          query: parseGraph('harness:antigravity'),
          serverQ: '',
        },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;

    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    expect(tree.agents.map((a) => a.id)).toEqual(['a-parent', 'a-child']);
    expect(Array.from(tree.contextIds ?? [])).toEqual(['a-parent']);
    expect(tree.filterKey).toBe('harness:antigravity');

    // Quick-jump palette keeps searching all loaded agents even while filtered
    expect(page.graphPalette.isAvailable).toBe(true);
    page.graphPalette.open();
    const palette = await vi.waitFor(
      () => {
        const p = document.querySelector('scion-quick-palette') as
          | (HTMLElement & {
              open?: boolean;
              groups?: {
                agents?: { status?: string; candidates: Array<{ target: { agentId: string } }> };
              };
            })
          | null;
        if (!p?.open || p.groups?.agents?.status !== 'ready') throw new Error('palette not ready');
        return p;
      },
      { timeout: 5000 }
    );
    const candidateIds = (palette.groups?.agents?.candidates ?? [])
      .map((c) => c.target.agentId)
      .sort();
    expect(candidateIds).toEqual(['a-child', 'a-other', 'a-parent']);
  });

  it('re-flows layout deliberately on narrowing filter-input while keeping stable positions on live delete', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    const r = mkAgent({ id: 'r', name: 'Root', phase: 'running', labels: { keep: '1' } });
    const c1 = mkAgent({
      id: 'c1',
      name: 'Child 1',
      phase: 'running',
      ancestry: ['user:u1', 'r'],
      labels: { keep: '1' },
    });
    const c2 = mkAgent({
      id: 'c2',
      name: 'Child 2',
      phase: 'stopped',
      ancestry: ['user:u1', 'r'],
      labels: { drop: '1' },
    });
    const c3 = mkAgent({
      id: 'c3',
      name: 'Child 3',
      phase: 'running',
      ancestry: ['user:u1', 'r'],
      labels: { keep: '1' },
    });

    const nodePositions = (treeEl: ScionAgentTreeView): Record<string, [number, number]> => {
      const out: Record<string, [number, number]> = {};
      for (const wrapper of treeEl.shadowRoot!.querySelectorAll<HTMLElement>('.node-wrapper')) {
        const id = wrapper.querySelector<HTMLElement>('a.node')?.dataset.agentId;
        if (id) out[id] = [parseFloat(wrapper.style.left), parseFloat(wrapper.style.top)];
      }
      return out;
    };

    // Baseline: fresh render of the narrowed subset [r, c1, c3].
    const freshTree = document.createElement('scion-agent-tree-view') as ScionAgentTreeView;
    freshTree.agents = [r, c1, c3];
    document.body.appendChild(freshTree);
    await freshTree.updateComplete;
    const expectedCompacted = nodePositions(freshTree);
    freshTree.remove();

    // Mount full graph [r, c1, c2, c3], then narrow via filter-input + debounced filter-commit.
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([r, c1, c2, c3]);
    vi.spyOn(stateManager, 'getDeletedAgentIds').mockReturnValue(new Set());
    window.history.replaceState({}, '', '/agents/graph');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    document.body.appendChild(page);
    await page.updateComplete;

    const box = page.shadowRoot!.querySelector('scion-filter-box')!;
    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    const narrowedQuery = parseGraph('phase:running');

    // Narrowing filter-input updates filterKey in the same render as agents -> compacted layout.
    box.dispatchEvent(
      new CustomEvent('filter-input', {
        detail: { value: 'phase:running', query: narrowedQuery, valid: true },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;
    await tree.updateComplete;
    expect(nodePositions(tree)).toEqual(expectedCompacted);

    // Subsequent debounced filter-commit keeps the same compacted layout.
    box.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: { value: 'phase:running', query: narrowedQuery, serverQ: '' },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;
    await tree.updateComplete;
    expect(nodePositions(tree)).toEqual(expectedCompacted);

    // Invalid keystroke leaves filterKey (and layout) unchanged.
    const keyBeforeInvalid = tree.filterKey;
    box.dispatchEvent(
      new CustomEvent('filter-input', {
        detail: { value: 'phase:running phase:', query: narrowedQuery, valid: false },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;
    expect(tree.filterKey).toBe(keyBeforeInvalid);

    // Live delete of c1 under unchanged filter takes stable delete path (c3 stays at its x/y).
    const c3PosBeforeDelete = nodePositions(tree)['c3'];
    stateManager.dispatchEvent(
      new CustomEvent('agents-changed', { detail: { data: { upserted: [], deleted: ['c1'] } } })
    );
    await page.updateComplete;
    await tree.updateComplete;
    expect(nodePositions(tree)['c3']).toEqual(c3PosBeforeDelete);
  });

  it('clears the filter and reveals/focuses the picked agent when selecting a hidden agent in unscoped mode', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([parent, child, other]);
    window.history.replaceState({}, '', '/agents/graph?q=harness:antigravity');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    document.body.appendChild(page);
    await page.updateComplete;

    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    expect(tree.agents.map((a) => a.id)).toEqual(['a-parent', 'a-child']);
    let nodeRenderedAtReveal = false;
    const origReveal = tree.revealAgent.bind(tree);
    const revealSpy = vi.spyOn(tree, 'revealAgent').mockImplementation((id: string) => {
      nodeRenderedAtReveal = Boolean(
        tree.shadowRoot?.querySelector(`a.node[data-agent-id="${id}"]`)
      );
      return origReveal(id);
    });
    const focusSpy = vi.spyOn(tree, 'focusAgentNode');

    page.graphPalette.open();
    const palette = await vi.waitFor(() => {
      const p = document.querySelector('scion-quick-palette') as
        | (HTMLElement & { open?: boolean; groups?: { agents?: { status?: string } } })
        | null;
      if (!p?.open || p.groups?.agents?.status !== 'ready') throw new Error('palette not ready');
      return p;
    });

    // Pick 'a-other', which is currently hidden by harness:antigravity.
    // Call 1 (from GraphPaletteController) returns false; Call 2 (from onSelectHidden)
    // must run only after the child tree view finishes rendering the revealed node.
    palette.dispatchEvent(
      new CustomEvent('palette-select', {
        detail: { target: { kind: 'agent', agentId: 'a-other', displayName: 'Other' } },
      })
    );
    await vi.waitFor(() => expect(revealSpy).toHaveBeenCalledTimes(2));

    expect(window.location.search).toBe('');
    expect(tree.agents.map((a) => a.id)).toEqual(['a-parent', 'a-child', 'a-other']);
    expect(revealSpy).toHaveBeenLastCalledWith('a-other');
    expect(nodeRenderedAtReveal).toBe(true);

    palette
      .shadowRoot!.querySelector('sl-dialog')!
      .dispatchEvent(new Event('sl-after-hide', { bubbles: true, composed: true }));
    await vi.waitFor(() => expect(focusSpy).toHaveBeenCalledWith('a-other'));
  });
});

describe('scion-page-agent-graph R1 load scope with non-held drains', () => {
  let page: AgentGraphPage | null = null;

  beforeAll(async () => {
    await import('./agent-graph.js');
  });

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    vi.spyOn(stateManager, 'setScope').mockImplementation(() => {});
    vi.spyOn(stateManager, 'sseConnected').mockResolvedValue(undefined);
    vi.spyOn(stateManager, 'isAgentSetComplete').mockReturnValue(false);
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([]);
    vi.spyOn(stateManager, 'getProjects').mockReturnValue([]);
    vi.spyOn(stateManager, 'getDeletedAgentIds').mockReturnValue(new Set());
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({}), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
  });

  afterEach(() => {
    page?.remove();
    page = null;
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    document.body.replaceChildren();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  const alphaAgent = mkAgent({
    id: 'a-1',
    name: 'Alpha One',
    slug: 'alpha-1',
    projectId: 'proj-alpha-uuid',
    project: 'Alpha Project',
    projectSlug: 'alpha',
    phase: 'running',
  });
  const betaAgent = mkAgent({
    id: 'b-1',
    name: 'Beta One',
    slug: 'beta-1',
    projectId: 'proj-beta-uuid',
    project: 'Beta Project',
    projectSlug: 'beta',
    phase: 'stopped',
  });

  function drainResult(
    agents: Agent[],
    overrides: Partial<SeededDrainResult> = {}
  ): SeededDrainResult {
    return {
      agents,
      complete: !overrides.capped,
      capped: false,
      stale: false,
      requests: 1,
      error: null,
      firstPageFailed: false,
      ...overrides,
    };
  }

  function stubProbeNotComplete(): void {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify({ agents: [alphaAgent], complete: false }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );
  }

  it('issues a project drain when narrowing a capped unscoped drain with project:<id>', async () => {
    const drainCalls: string[] = [];
    window.history.replaceState({}, '', '/agents/graph');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    const runner = (
      page as unknown as {
        drainRunner: { run: (opts: { url: string }) => Promise<SeededDrainResult | null> };
      }
    ).drainRunner;
    vi.spyOn(runner, 'run').mockImplementation((opts) => {
      drainCalls.push(opts.url);
      if (opts.url.includes('projectId=')) {
        return Promise.resolve(drainResult([alphaAgent]));
      }
      return Promise.resolve(drainResult([alphaAgent, betaAgent], { capped: true }));
    });

    document.body.appendChild(page);
    await vi.waitFor(() => expect(drainCalls).toEqual(['/api/v1/agents']));
    await page.updateComplete;

    const box = page.shadowRoot!.querySelector('scion-filter-box')!;
    box.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: {
          value: 'project:proj-alpha-uuid',
          query: parseGraph('project:proj-alpha-uuid'),
          serverQ: '',
        },
        bubbles: true,
        composed: true,
      })
    );
    await vi.waitFor(() =>
      expect(drainCalls).toEqual(['/api/v1/agents', '/api/v1/agents?projectId=proj-alpha-uuid'])
    );
  });

  it('normalises ?q=project:<slug> to projectId so live-created agents join and follow-up commits do not re-drain', async () => {
    stubProbeNotComplete();
    const drainCalls: string[] = [];
    window.history.replaceState({}, '', '/agents/graph?q=project:alpha');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    const runner = (
      page as unknown as {
        drainRunner: { run: (opts: { url: string }) => Promise<SeededDrainResult | null> };
      }
    ).drainRunner;
    vi.spyOn(runner, 'run').mockImplementation((opts) => {
      drainCalls.push(opts.url);
      return Promise.resolve(drainResult([alphaAgent]));
    });

    document.body.appendChild(page);
    await vi.waitFor(() => expect(drainCalls).toEqual(['/api/v1/agents?projectId=alpha']));
    await page.updateComplete;

    // Live-created agent in the same project must join (memberScope normalised to proj-alpha-uuid).
    const liveAgent = mkAgent({
      id: 'a-live',
      name: 'Live Agent',
      slug: 'a-live',
      projectId: 'proj-alpha-uuid',
      project: 'Alpha Project',
      projectSlug: 'alpha',
      phase: 'running',
    });
    vi.spyOn(stateManager, 'getAgent').mockImplementation((id) =>
      id === 'a-live' ? liveAgent : undefined
    );
    stateManager.dispatchEvent(
      new CustomEvent('agent-created', { detail: { data: { agentId: 'a-live' } } })
    );
    stateManager.dispatchEvent(
      new CustomEvent('agents-changed', { detail: { data: { upserted: ['a-live'], deleted: [] } } })
    );
    await page.updateComplete;

    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    expect(tree.agents.map((a) => a.id).sort()).toEqual(['a-1', 'a-live']);

    // Adding phase:running to project:alpha must NOT issue a second drain.
    const box = page.shadowRoot!.querySelector('scion-filter-box')!;
    box.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: {
          value: 'project:alpha phase:running',
          query: parseGraph('project:alpha phase:running'),
          serverQ: '',
        },
        bubbles: true,
        composed: true,
      })
    );
    await page.updateComplete;
    expect(drainCalls).toEqual(['/api/v1/agents?projectId=alpha']);
  });

  it('drains unscoped for project:<name> on an unprobed hub so matching agents are shown (not empty)', async () => {
    const drainCalls: string[] = [];
    window.history.replaceState({}, '', '/agents/graph?q=project%3A%22Alpha%20Project%22');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    const runner = (
      page as unknown as {
        drainRunner: { run: (opts: { url: string }) => Promise<SeededDrainResult | null> };
      }
    ).drainRunner;
    vi.spyOn(runner, 'run').mockImplementation((opts) => {
      drainCalls.push(opts.url);
      return Promise.resolve(drainResult([alphaAgent, betaAgent]));
    });

    document.body.appendChild(page);
    await vi.waitFor(() => {
      expect(drainCalls).toEqual(['/api/v1/agents']);
      expect(page!.shadowRoot!.querySelector('scion-agent-tree-view')).not.toBeNull();
    });

    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    expect(tree.agents.map((a) => a.id)).toEqual(['a-1']);
    expect(tree.filteredEmpty).toBe(false);
  });

  it('aborts an in-flight initial project drain and loads unscoped when filter is cleared', async () => {
    stubProbeNotComplete();
    const drainCalls: string[] = [];
    let releaseFirstDrain!: (res: SeededDrainResult | null) => void;
    window.history.replaceState({}, '', '/agents/graph?q=project:alpha');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    const runner = (
      page as unknown as {
        drainRunner: {
          run: (opts: { url: string }) => Promise<SeededDrainResult | null>;
          abort: () => void;
        };
      }
    ).drainRunner;
    const abortSpy = vi.spyOn(runner, 'abort');
    vi.spyOn(runner, 'run').mockImplementation((opts) => {
      drainCalls.push(opts.url);
      if (drainCalls.length === 1) {
        return new Promise((resolve) => {
          releaseFirstDrain = resolve;
        });
      }
      return Promise.resolve(drainResult([alphaAgent, betaAgent]));
    });

    document.body.appendChild(page);
    await vi.waitFor(() => expect(drainCalls).toEqual(['/api/v1/agents?projectId=alpha']));

    // Clear while the initial project drain is still in flight (memberScope === null).
    const box = page.shadowRoot!.querySelector('scion-filter-box')!;
    box.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: {
          value: '',
          query: { mode: 'text', text: '' },
          serverQ: '',
        },
        bubbles: true,
        composed: true,
      })
    );
    releaseFirstDrain(null);

    await vi.waitFor(() => {
      expect(drainCalls).toEqual(['/api/v1/agents?projectId=alpha', '/api/v1/agents']);
      expect(page!.shadowRoot!.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    expect(abortSpy).toHaveBeenCalled();
    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    expect(tree.agents.map((a) => a.id).sort()).toEqual(['a-1', 'b-1']);
  });

  it('wires tree-view clear-filter to onClearFilter and keeps filteredEmpty false when unfiltered and empty', async () => {
    window.history.replaceState({}, '', '/agents/graph');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    const runner = (
      page as unknown as {
        drainRunner: { run: (opts: { url: string }) => Promise<SeededDrainResult | null> };
      }
    ).drainRunner;
    vi.spyOn(runner, 'run').mockResolvedValue(drainResult([]));

    document.body.appendChild(page);
    await vi.waitFor(() =>
      expect(page!.shadowRoot!.querySelector('scion-agent-tree-view')).not.toBeNull()
    );
    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    expect(tree.filteredEmpty).toBe(false);

    // Confirm resolveProjectLoadTarget handles slug/name/id normalisation directly.
    expect(resolveProjectLoadTarget(parseGraph('project:alpha'), [alphaAgent], [])).toBe(
      'proj-alpha-uuid'
    );
    expect(resolveProjectLoadTarget(parseGraph('project:"Alpha Project"'), [alphaAgent], [])).toBe(
      'proj-alpha-uuid'
    );
    expect(resolveProjectLoadTarget(parseGraph('project:"Alpha Project"'), [], [])).toBe('');
  });

  it('shows filtered empty state with Clear filter button when ?q=project:nosuchslug drains empty', async () => {
    stubProbeNotComplete();
    const drainCalls: string[] = [];
    window.history.replaceState({}, '', '/agents/graph?q=project:nosuchslug');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    const runner = (
      page as unknown as {
        drainRunner: { run: (opts: { url: string }) => Promise<SeededDrainResult | null> };
      }
    ).drainRunner;
    vi.spyOn(runner, 'run').mockImplementation((opts) => {
      drainCalls.push(opts.url);
      if (opts.url.includes('projectId=nosuchslug')) {
        return Promise.resolve(drainResult([]));
      }
      return Promise.resolve(drainResult([alphaAgent, betaAgent]));
    });

    document.body.appendChild(page);
    await vi.waitFor(() => {
      expect(drainCalls).toEqual(['/api/v1/agents?projectId=nosuchslug']);
      expect(page!.shadowRoot!.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    await tree.updateComplete;
    expect(tree.filteredEmpty).toBe(true);
    const clearBtn = tree.shadowRoot!.querySelector('.empty-state sl-button') as HTMLElement | null;
    expect(clearBtn).not.toBeNull();

    // Clicking Clear filter clears ?q= and loads unscoped agents.
    clearBtn!.click();
    await vi.waitFor(() =>
      expect(drainCalls).toEqual(['/api/v1/agents?projectId=nosuchslug', '/api/v1/agents'])
    );
    expect(window.location.search).toBe('');
  });

  it('keeps project:<slug> without re-draining when picking a hidden same-project agent from palette', async () => {
    stubProbeNotComplete();
    const alphaStopped = mkAgent({
      id: 'a-2',
      name: 'Alpha Stopped',
      slug: 'alpha-2',
      projectId: 'proj-alpha-uuid',
      project: 'Alpha Project',
      projectSlug: 'alpha',
      phase: 'stopped',
    });
    const drainCalls: string[] = [];
    window.history.replaceState({}, '', '/agents/graph?q=project:alpha%20phase:running');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    const runner = (
      page as unknown as {
        drainRunner: { run: (opts: { url: string }) => Promise<SeededDrainResult | null> };
      }
    ).drainRunner;
    vi.spyOn(runner, 'run').mockImplementation((opts) => {
      drainCalls.push(opts.url);
      return Promise.resolve(drainResult([alphaAgent, alphaStopped]));
    });

    document.body.appendChild(page);
    await vi.waitFor(() => {
      expect(drainCalls).toEqual(['/api/v1/agents?projectId=alpha']);
      expect(page!.shadowRoot!.querySelector('scion-agent-tree-view')).not.toBeNull();
    });

    const tree = page.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView;
    expect(tree.agents.map((a) => a.id)).toEqual(['a-1']);
    const revealSpy = vi.spyOn(tree, 'revealAgent');
    const focusSpy = vi.spyOn(tree, 'focusAgentNode');

    page.graphPalette.open();
    const palette = await vi.waitFor(() => {
      const p = document.querySelector('scion-quick-palette') as
        | (HTMLElement & { open?: boolean; groups?: { agents?: { status?: string } } })
        | null;
      if (!p?.open || p.groups?.agents?.status !== 'ready') throw new Error('palette not ready');
      return p;
    });

    // Pick 'a-2', hidden by phase:running, in the same loaded project.
    palette.dispatchEvent(
      new CustomEvent('palette-select', {
        detail: { target: { kind: 'agent', agentId: 'a-2', displayName: 'Alpha Stopped' } },
      })
    );
    await vi.waitFor(() => expect(revealSpy).toHaveBeenCalledWith('a-2'));

    // Keeps project:alpha, issues no second drain, reveals and focuses a-2.
    expect(window.location.search).toBe('?q=project%3Aalpha');
    expect(drainCalls).toEqual(['/api/v1/agents?projectId=alpha']);
    expect(tree.agents.map((a) => a.id)).toEqual(['a-1', 'a-2']);

    palette
      .shadowRoot!.querySelector('sl-dialog')!
      .dispatchEvent(new Event('sl-after-hide', { bubbles: true, composed: true }));
    await vi.waitFor(() => expect(focusSpy).toHaveBeenCalledWith('a-2'));
  });
});
