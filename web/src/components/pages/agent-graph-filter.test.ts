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
} from './agent-graph-filter.js';
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
    messageMode: 'branch',
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
    messageMode: 'lineage',
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
  });

  afterEach(() => {
    page?.remove();
    page = null;
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    document.body.replaceChildren();
    vi.restoreAllMocks();
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

  it('keeps legacy select and URL unchanged when filters.query_dialect is off', async () => {
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([parent, child, other]);
    window.history.replaceState({}, '', '/agents/graph');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    document.body.appendChild(page);
    await page.updateComplete;

    expect(page.shadowRoot!.querySelector('scion-filter-box')).toBeNull();
    expect(page.shadowRoot!.querySelector('sl-select')).not.toBeNull();
  });

  it('migrates ?project= to ?q=project:... and filters with dimmed context when dialect is on', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    vi.spyOn(stateManager, 'getAgents').mockReturnValue([parent, child, other]);
    window.history.replaceState({}, '', '/agents/graph?project=p-1');
    page = document.createElement('scion-page-agent-graph') as AgentGraphPage;
    document.body.appendChild(page);
    await page.updateComplete;

    expect(window.location.search).toBe('?q=project%3Ap-1');
    const box = page.shadowRoot!.querySelector('scion-filter-box');
    expect(box).not.toBeNull();
    expect(page.shadowRoot!.querySelector('sl-select')).toBeNull();

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
});
