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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { resetServerFlagStateForTests } from '../../utils/feature-flags.js';
import { stateManager } from '../../client/state.js';
import type { AgentListWindow } from '../../client/agent-list-window.js';
import type { Agent } from '../../shared/types.js';
import type { ScionAgentTreeView } from '../shared/agent-tree-view.js';
import './agents.js';
import type { ScionPageAgents } from './agents.js';
import './project-detail.js';
import type { ScionPageProjectDetail } from './project-detail.js';

describe('<scion-page-agents> unified filter dialect (P1)', () => {
  let fetchUrls: string[];

  beforeEach(() => {
    fetchUrls = [];
    localStorage.clear();
    window.history.replaceState(null, '', '/agents');
    resetServerFlagStateForTests();
    vi.stubGlobal(
      'EventSource',
      class {
        onopen: (() => void) | null = null;
        onerror: (() => void) | null = null;
        constructor() {
          queueMicrotask(() => this.onopen?.());
        }
        addEventListener(type: string, cb: () => void): void {
          if (type === 'open') queueMicrotask(cb);
        }
        removeEventListener(): void {}
        close(): void {}
      }
    );
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        fetchUrls.push(url);
        return Promise.resolve(
          new Response(JSON.stringify({ agents: [], complete: true, totalCount: 0 }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.unstubAllGlobals();
    localStorage.clear();
    window.history.replaceState(null, '', '/agents');
    resetServerFlagStateForTests();
  });

  it('hydrates ?q=activity%3Awaiting_for_input in <scion-filter-box> (ptone/scion#9300)', async () => {
    const waitingAgent: Agent = {
      id: 'a-wait',
      name: 'Waiting Bot',
      slug: 'waiting-bot',
      projectId: 'p1',
      template: 'developer',
      phase: 'running',
      activity: 'waiting_for_input',
    };
    const thinkingAgent: Agent = {
      id: 'a-think',
      name: 'Thinking Bot',
      slug: 'thinking-bot',
      projectId: 'p1',
      template: 'developer',
      phase: 'running',
      activity: 'thinking',
    };
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        fetchUrls.push(url);
        return Promise.resolve(
          new Response(
            JSON.stringify({
              agents: [waitingAgent, thinkingAgent],
              complete: true,
              totalCount: 2,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      })
    );

    window.history.replaceState(null, '', '/agents?q=activity%3Awaiting_for_input');
    const elOn = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(elOn);
    await new Promise((r) => setTimeout(r, 15));
    await elOn.updateComplete;

    const box = elOn.shadowRoot?.querySelector('scion-filter-box');
    expect(box?.value).toBe('activity:waiting_for_input');
    expect(fetchUrls.some((u) => u.includes('q=activity%3Awaiting_for_input'))).toBe(true);
    expect(elOn.shadowRoot?.textContent).toContain('Waiting Bot');
    expect(elOn.shadowRoot?.textContent).not.toContain('Thinking Bot');
    elOn.remove();
  });

  it('migrates localStorage phase once to ?q= and renders <scion-filter-box>', async () => {
    localStorage.setItem('scion-filter-agents-phase', 'running');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 10));

    expect(localStorage.getItem('scion-filter-agents-phase')).toBeNull();
    expect(new URLSearchParams(window.location.search).get('q')).toBe('phase:running');

    const box = el.shadowRoot?.querySelector('scion-filter-box');
    expect(box).not.toBeNull();
    expect(box?.value).toBe('phase:running');
    expect(fetchUrls.some((u) => u.includes('q=phase%3Arunning'))).toBe(true);
  });

  it('edits query text when clicking phase chips and updates ?q= in URL', async () => {
    window.history.replaceState(null, '', '/agents?q=label%3Ateam%3Dui');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const buttons = Array.from(
      el.shadowRoot?.querySelectorAll('.filter-bar .scope-toggle button') ?? []
    );
    const errorBtn = buttons.find((b) => b.textContent?.trim() === 'Error') as HTMLButtonElement;
    expect(errorBtn).toBeDefined();
    errorBtn.click();
    await el.updateComplete;

    expect(new URLSearchParams(window.location.search).get('q')).toBe('phase:error label:team=ui');

    const allBtn = buttons.find((b) => b.textContent?.trim() === 'All') as HTMLButtonElement;
    allBtn.click();
    await el.updateComplete;
    expect(new URLSearchParams(window.location.search).get('q')).toBe('label:team=ui');
  });

  it('shows "No Matching Agents" instead of "No Agents Found" when a filter query matches zero rows', async () => {
    window.history.replaceState(null, '', '/agents?q=phase%3Aerror');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const heading = el.shadowRoot?.querySelector('.empty-state h2')?.textContent?.trim();
    expect(heading).toBe('No Matching Agents');
  });

  it('retires old mode dropdown and migrates scope/mode from localStorage', async () => {
    localStorage.setItem('scion-scope-agents', 'mine');
    localStorage.setItem('scion-filter-agents-mode', 'owner');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    el.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u1', email: 'u1@example.com', name: 'U1', role: 'admin' },
    };
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    expect(localStorage.getItem('scion-scope-agents')).toBeNull();
    expect(localStorage.getItem('scion-filter-agents-mode')).toBeNull();
    expect(new URLSearchParams(window.location.search).get('q')).toBe('is:mine mode:owner');

    // Mode dropdown is retired; only is:mine is sent to hub (mode: stripped).
    expect(el.shadowRoot?.querySelector('sl-menu-item[value="owner"]')).toBeNull();
    const agentFetches = fetchUrls.filter((u) => u.includes('/api/v1/agents'));
    expect(agentFetches.length).toBeGreaterThan(0);
    expect(agentFetches.some((u) => u.includes('q=is%3Amine') && !u.includes('mode'))).toBe(true);
  });

  it('edits is:mine / is:shared via scope chips and highlights no phase chip for multi-value phase', async () => {
    window.history.replaceState(null, '', '/agents?q=phase%3Arunning%2Cerror');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    el.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u1', email: 'u1@example.com', name: 'U1', role: 'admin' },
    };
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    // No phase chip (not even "All") is active for phase:running,error.
    const activePhaseBtns = el.shadowRoot?.querySelectorAll(
      '.filter-bar .scope-toggle button.active'
    );
    expect(activePhaseBtns?.length ?? 0).toBe(0);

    // Clicking Mine in the header scope toggle writes is:mine into ?q=.
    const scopeBtns = Array.from(
      el.shadowRoot?.querySelectorAll('.header-actions .scope-toggle button') ?? []
    );
    const mineBtn = scopeBtns.find((b) => b.textContent?.includes('Mine')) as HTMLButtonElement;
    expect(mineBtn).toBeDefined();
    mineBtn.click();
    await el.updateComplete;
    expect(new URLSearchParams(window.location.search).get('q')).toBe(
      'is:mine phase:running,error'
    );
  });

  it('renders <scion-filter-box view="project_agents"> on project detail when dialect is on', async () => {
    window.history.replaceState(null, '', '/projects/proj-123?q=harness%3Aclaude');

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        fetchUrls.push(url);
        if (url.includes('/api/v1/projects/proj-123/agents')) {
          return Promise.resolve(
            new Response(JSON.stringify({ agents: [], complete: true, totalCount: 0 }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(
          new Response(
            JSON.stringify({
              id: 'proj-123',
              name: 'Proj 123',
              slug: 'proj-123',
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      })
    );

    const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    el.projectId = 'proj-123';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;

    const box = el.shadowRoot?.querySelector('scion-filter-box');
    expect(box).not.toBeNull();
    expect(box?.view).toBe('project_agents');
    expect(box?.value).toBe('harness:claude');
    expect(
      fetchUrls.some(
        (u) => u.includes('/api/v1/projects/proj-123/agents') && u.includes('q=harness%3Aclaude')
      )
    ).toBe(true);
  });

  it('refuses live-add of SSE-created agents on global page (paged) under server-only owner:<email> and is:mine, while admitting owner:me and self email', async () => {
    const makeAgent = (id: string, ownerId: string): Agent => ({
      id,
      name: `agent-${id}`,
      slug: `agent-${id}`,
      projectId: 'proj-1',
      template: 'developer',
      phase: 'running',
      ownerId,
    });
    const pagedResponse = {
      agents: [makeAgent('a1', 'bob-id')],
      complete: false,
      totalCount: 2,
      nextCursor: 'cursor-1',
      stats: { total: 2, running: 2, agents: [['a1', 'running']] },
    };
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(JSON.stringify(pagedResponse), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        )
      )
    );

    const mountWithQ = async (q: string): Promise<ScionPageAgents> => {
      window.history.replaceState(null, '', `/agents?q=${encodeURIComponent(q)}`);
      const el = document.createElement('scion-page-agents') as ScionPageAgents;
      el.pageData = {
        path: '/agents',
        title: 'Agents',
        user: { id: 'me-id', email: 'me@example.com', name: 'Me', role: 'admin' },
      };
      document.body.appendChild(el);
      await new Promise((r) => setTimeout(r, 15));
      await el.updateComplete;
      return el;
    };

    const carolAgent = makeAgent('live-carol', 'carol-id');
    const meAgent = makeAgent('live-me', 'me-id');

    // External email: live-created carol agent must NOT be added to paged member index.
    const elEmail = await mountWithQ('owner:bob@example.com');
    const winEmail = (elEmail as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(winEmail.state).toBe('paged');
    const beforeEmailTotal = winEmail.stats.total;
    stateManager.seedAgents([carolAgent]);
    winEmail.applyChanges({
      upserted: [carolAgent.id],
      deleted: [],
      unknown: new Map(),
      generation: stateManager.scopeGeneration,
    });
    expect(winEmail.stats.total).toBe(beforeEmailTotal);
    elEmail.remove();

    // is:mine: live-created agent must NOT be added to paged member index.
    const elScope = await mountWithQ('is:mine');
    const winScope = (elScope as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(winScope.state).toBe('paged');
    const beforeScopeTotal = winScope.stats.total;
    stateManager.seedAgents([meAgent]);
    winScope.applyChanges({
      upserted: [meAgent.id],
      deleted: [],
      unknown: new Map(),
      generation: stateManager.scopeGeneration,
    });
    expect(winScope.stats.total).toBe(beforeScopeTotal);
    elScope.remove();

    // owner:me and self email: matching live-created agent IS added.
    for (const q of ['owner:me', 'owner:me@example.com']) {
      const elSelf = await mountWithQ(q);
      const winSelf = (elSelf as unknown as { agentWindow: AgentListWindow }).agentWindow;
      expect(winSelf.state).toBe('paged');
      const beforeTotal = winSelf.stats.total;
      const freshMe = makeAgent(`live-me-${q}`, 'me-id');
      stateManager.seedAgents([freshMe]);
      winSelf.applyChanges({
        upserted: [freshMe.id],
        deleted: [],
        unknown: new Map(),
        generation: stateManager.scopeGeneration,
      });
      expect(winSelf.stats.total).toBe(beforeTotal + 1);
      elSelf.remove();
    }
  });

  it('refuses live-add on project detail under server-only owner:<email> during drain and paged state, while admitting owner:me', async () => {
    const makeAgent = (id: string, ownerId: string): Agent => ({
      id,
      name: `proj-agent-${id}`,
      slug: `proj-agent-${id}`,
      projectId: 'proj-123',
      template: 'developer',
      phase: 'running',
      ownerId,
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (url.includes('/api/v1/projects/proj-123/agents')) {
          return Promise.resolve(
            new Response(JSON.stringify({ agents: [makeAgent('p1', 'bob-id')], complete: true }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'proj-123', name: 'Proj 123', slug: 'proj-123' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    const mountProjectWithQ = async (q: string): Promise<ScionPageProjectDetail> => {
      window.history.replaceState(null, '', `/projects/proj-123?q=${encodeURIComponent(q)}`);
      const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
      el.projectId = 'proj-123';
      el.pageData = {
        path: '/projects/proj-123',
        title: 'Project',
        user: { id: 'me-id', email: 'me@example.com', name: 'Me', role: 'admin' },
      };
      document.body.appendChild(el);
      await new Promise((r) => setTimeout(r, 20));
      await el.updateComplete;
      return el;
    };

    const carolAgent = makeAgent('live-carol-proj', 'carol-id');
    const meAgent = makeAgent('live-me-proj', 'me-id');

    const elEmail = await mountProjectWithQ('owner:bob@example.com');
    const isMemberEmail = (
      elEmail as unknown as { isProjectMember: (a: Agent, l: string) => boolean }
    ).isProjectMember(carolAgent, '');
    expect(isMemberEmail).toBe(false);
    elEmail.remove();

    const elMe = await mountProjectWithQ('owner:me');
    const isMemberMe = (
      elMe as unknown as { isProjectMember: (a: Agent, l: string) => boolean }
    ).isProjectMember(meAgent, '');
    const isMemberOther = (
      elMe as unknown as { isProjectMember: (a: Agent, l: string) => boolean }
    ).isProjectMember(carolAgent, '');
    expect(isMemberMe).toBe(true);
    expect(isMemberOther).toBe(false);
    elMe.remove();
  });

  it('does not overwrite in-progress invalid text or URL when a deferred commit fires on global and project pages', async () => {
    vi.useFakeTimers();
    try {
      // 1) Global agents page
      window.history.replaceState(null, '', '/agents');
      const agentsEl = document.createElement('scion-page-agents') as ScionPageAgents;
      document.body.appendChild(agentsEl);
      await vi.advanceTimersByTimeAsync(20);
      await agentsEl.updateComplete;

      const box1 = agentsEl.shadowRoot?.querySelector('scion-filter-box');
      expect(box1).not.toBeNull();
      const input1 = box1?.shadowRoot?.querySelector('input') as HTMLInputElement;

      fetchUrls = [];
      input1.value = 'phase:running';
      input1.dispatchEvent(new Event('input'));
      await agentsEl.updateComplete;

      await vi.advanceTimersByTimeAsync(100);
      input1.value = 'phase:running label:';
      input1.dispatchEvent(new Event('input'));
      await agentsEl.updateComplete;

      await vi.advanceTimersByTimeAsync(250);
      await agentsEl.updateComplete;

      const box1After = agentsEl.shadowRoot?.querySelector('scion-filter-box');
      expect(box1After?.value).toBe('phase:running label:');
      expect(new URLSearchParams(window.location.search).get('q')).toBeNull();
      expect(fetchUrls.some((u) => u.includes('q=phase%3Arunning'))).toBe(true);
      agentsEl.remove();

      // 2) Project detail page
      window.history.replaceState(null, '', '/projects/proj-123');
      vi.stubGlobal(
        'fetch',
        vi.fn((input: RequestInfo | URL) => {
          const url =
            typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
          fetchUrls.push(url);
          if (url.includes('/api/v1/projects/proj-123/agents')) {
            return Promise.resolve(
              new Response(
                JSON.stringify({
                  agents: [
                    {
                      id: 'a1',
                      name: 'a1',
                      slug: 'a1',
                      projectId: 'proj-123',
                      template: 'dev',
                      phase: 'running',
                    },
                  ],
                  complete: true,
                  totalCount: 1,
                }),
                { status: 200, headers: { 'Content-Type': 'application/json' } }
              )
            );
          }
          return Promise.resolve(
            new Response(JSON.stringify({ id: 'proj-123', name: 'Proj 123', slug: 'proj-123' }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        })
      );

      const projEl = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
      projEl.projectId = 'proj-123';
      document.body.appendChild(projEl);
      await vi.advanceTimersByTimeAsync(20);
      await projEl.updateComplete;

      const box2 = projEl.shadowRoot?.querySelector('scion-filter-box');
      expect(box2).not.toBeNull();
      const input2 = box2?.shadowRoot?.querySelector('input') as HTMLInputElement;

      fetchUrls = [];
      input2.value = 'phase:running';
      input2.dispatchEvent(new Event('input'));
      await projEl.updateComplete;

      await vi.advanceTimersByTimeAsync(100);
      input2.value = 'phase:running label:';
      input2.dispatchEvent(new Event('input'));
      await projEl.updateComplete;

      await vi.advanceTimersByTimeAsync(250);
      await projEl.updateComplete;

      const box2After = projEl.shadowRoot?.querySelector('scion-filter-box');
      expect(box2After?.value).toBe('phase:running label:');
      expect(new URLSearchParams(window.location.search).get('q')).toBeNull();
      expect(
        fetchUrls.some(
          (u) => u.includes('/api/v1/projects/proj-123/agents') && u.includes('q=phase%3Arunning')
        )
      ).toBe(true);
      projEl.remove();
    } finally {
      vi.useRealTimers();
    }
  });

  it('pins all remaining live-add/membership sites (shouldAdd, loadFitAgents, drainGlobalAgents, project-detail isAddable/shouldAdd/drainProjectAgents)', async () => {
    const makeAgent = (id: string, projectId: string, ownerId: string): Agent => ({
      id,
      name: `a-${id}`,
      slug: `a-${id}`,
      projectId,
      template: 'developer',
      phase: 'running',
      ownerId,
      messageMode: 'owner',
    });

    const user = { id: 'me-id', email: 'me@example.com', name: 'Me', role: 'admin' as const };
    const dispatchEpochCreate = (agent: Agent): void => {
      stateManager.seedAgents([agent]);
      stateManager.dispatchEvent(
        new CustomEvent('agent-created', {
          detail: { data: agent },
        })
      );
    };
    const dispatchSseCreate = (agent: Agent): void => {
      stateManager.seedAgents([agent]);
      stateManager.dispatchEvent(
        new CustomEvent('agents-changed', {
          detail: {
            data: {
              upserted: [agent.id],
              deleted: [],
              unknown: new Map(),
              generation: stateManager.scopeGeneration,
            },
          },
        })
      );
    };

    // (A) agents.ts shouldAdd (small/local state)
    window.history.replaceState(null, '', '/agents?q=owner%3Abob%40example.com');
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({ agents: [makeAgent('a1', 'proj-1', 'bob-id')], complete: true }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        )
      )
    );
    const elSmall = document.createElement('scion-page-agents') as ScionPageAgents;
    elSmall.pageData = { path: '/agents', title: 'Agents', user };
    document.body.appendChild(elSmall);
    await new Promise((r) => setTimeout(r, 15));
    await elSmall.updateComplete;
    dispatchSseCreate(makeAgent('live-carol-small', 'proj-1', 'carol-id'));
    expect((elSmall as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toEqual(['a1']);
    elSmall.remove();

    // (B) agents.ts loadFitAgents epoch.seed isMember (SSE create during in-flight fit request)
    let resolveFit!: (r: Response) => void;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        () =>
          new Promise<Response>((res) => {
            resolveFit = res;
          })
      )
    );
    const elFit = document.createElement('scion-page-agents') as ScionPageAgents;
    elFit.pageData = { path: '/agents', title: 'Agents', user };
    document.body.appendChild(elFit);
    await new Promise((r) => setTimeout(r, 5));
    dispatchEpochCreate(makeAgent('live-carol-fit', 'proj-1', 'carol-id'));
    resolveFit(
      new Response(
        JSON.stringify({ agents: [makeAgent('a1', 'proj-1', 'bob-id')], complete: true }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await new Promise((r) => setTimeout(r, 15));
    expect((elFit as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toEqual(['a1']);
    elFit.remove();

    // (C) agents.ts drainGlobalAgents isMember (SSE create during in-flight drain)
    window.history.replaceState(null, '', '/agents?q=mode%3Aowner%20owner%3Abob%40example.com');
    let resolveDrain!: (r: Response) => void;
    vi.stubGlobal(
      'fetch',
      vi.fn(
        () =>
          new Promise<Response>((res) => {
            resolveDrain = res;
          })
      )
    );
    const elDrain = document.createElement('scion-page-agents') as ScionPageAgents;
    elDrain.pageData = { path: '/agents', title: 'Agents', user };
    document.body.appendChild(elDrain);
    await new Promise((r) => setTimeout(r, 10));
    dispatchEpochCreate(makeAgent('live-carol-drain', 'proj-1', 'carol-id'));
    resolveDrain(
      new Response(JSON.stringify({ agents: [makeAgent('a1', 'proj-1', 'bob-id')] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await new Promise((r) => setTimeout(r, 15));
    expect((elDrain as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toEqual(['a1']);
    elDrain.remove();

    // (D) project-detail.ts isAddable (paged state) + shouldAdd (small state)
    const makeProjectFetch = (agentsPayload: unknown) =>
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (url.includes('/api/v1/projects/proj-123/agents')) {
          return Promise.resolve(
            new Response(JSON.stringify(agentsPayload), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'proj-123', name: 'Proj 123', slug: 'proj-123' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      });

    window.history.replaceState(null, '', '/projects/proj-123?q=owner%3Abob%40example.com');
    vi.stubGlobal(
      'fetch',
      makeProjectFetch({
        agents: [makeAgent('p1', 'proj-123', 'bob-id')],
        complete: false,
        totalCount: 2,
        nextCursor: 'c1',
        stats: { total: 2, running: 2, agents: [['p1', 'running']] },
      })
    );
    const projPaged = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projPaged.projectId = 'proj-123';
    projPaged.pageData = { path: '/projects/proj-123', title: 'Project', user };
    document.body.appendChild(projPaged);
    await new Promise((r) => setTimeout(r, 20));
    const projWin = (projPaged as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(projWin.state).toBe('paged');
    const beforeProjTotal = projWin.stats.total;
    const carolProjPaged = makeAgent('live-carol-proj-paged', 'proj-123', 'carol-id');
    stateManager.seedAgents([carolProjPaged]);
    projWin.applyChanges({
      upserted: [carolProjPaged.id],
      deleted: [],
      unknown: new Map(),
      generation: stateManager.scopeGeneration,
    });
    expect(projWin.stats.total).toBe(beforeProjTotal);
    projPaged.remove();

    // project-detail.ts shouldAdd (small state)
    vi.stubGlobal(
      'fetch',
      makeProjectFetch({ agents: [makeAgent('p1', 'proj-123', 'bob-id')], complete: true })
    );
    const projSmall = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projSmall.projectId = 'proj-123';
    projSmall.pageData = { path: '/projects/proj-123', title: 'Project', user };
    document.body.appendChild(projSmall);
    await new Promise((r) => setTimeout(r, 20));
    dispatchSseCreate(makeAgent('live-carol-proj-small', 'proj-123', 'carol-id'));
    expect((projSmall as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toEqual(['p1']);
    projSmall.remove();

    // (E) project-detail.ts drainProjectAgents isMember (SSE create during in-flight drain)
    window.history.replaceState(
      null,
      '',
      '/projects/proj-123?q=mode%3Aowner%20owner%3Abob%40example.com'
    );
    let resolveProjDrain!: (r: Response) => void;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const url =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (url.includes('/api/v1/projects/proj-123/agents')) {
          return new Promise<Response>((res) => {
            resolveProjDrain = res;
          });
        }
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'proj-123', name: 'Proj 123', slug: 'proj-123' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );
    const projDrain = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projDrain.projectId = 'proj-123';
    projDrain.pageData = { path: '/projects/proj-123', title: 'Project', user };
    document.body.appendChild(projDrain);
    await new Promise((r) => setTimeout(r, 15));
    dispatchEpochCreate(makeAgent('live-carol-proj-drain', 'proj-123', 'carol-id'));
    resolveProjDrain(
      new Response(JSON.stringify({ agents: [makeAgent('p1', 'proj-123', 'bob-id')] }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await new Promise((r) => setTimeout(r, 20));
    expect((projDrain as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toEqual(['p1']);
    projDrain.remove();
  });

  it('updates committed predicates without refetching when only a client-only mode: value changes', async () => {
    const makeModeAgent = (
      id: string,
      projectId: string,
      messageMode: 'owner' | 'direct'
    ): Agent => ({
      id,
      name: `agent-${id}`,
      slug: `agent-${id}`,
      projectId,
      template: 'developer',
      phase: 'running',
      messageMode,
    });

    const dispatchSseCreate = (agent: Agent): void => {
      stateManager.seedAgents([agent]);
      stateManager.dispatchEvent(
        new CustomEvent('agents-changed', {
          detail: {
            data: {
              upserted: [agent.id],
              deleted: [],
              unknown: new Map(),
              generation: stateManager.scopeGeneration,
            },
          },
        })
      );
    };

    // 1. Global agents page: ?q=mode:owner -> commit mode:direct
    window.history.replaceState(null, '', '/agents?q=mode%3Aowner');
    const globalFetch = vi.fn(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            agents: [makeModeAgent('a-owner-0', 'proj-1', 'owner')],
            complete: true,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
    );
    vi.stubGlobal('fetch', globalFetch);

    const agentsEl = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(agentsEl);
    await new Promise((r) => setTimeout(r, 20));
    await agentsEl.updateComplete;

    const fetchCountBefore = globalFetch.mock.calls.length;
    const box1 = agentsEl.shadowRoot?.querySelector('scion-filter-box');
    const input1 = box1?.shadowRoot?.querySelector('input') as HTMLInputElement;
    input1.value = 'mode:direct';
    input1.dispatchEvent(new Event('input'));
    input1.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await agentsEl.updateComplete;

    // Server subset is unchanged ('' -> ''), so no extra fetch should fire.
    expect(globalFetch.mock.calls.length).toBe(fetchCountBefore);

    const globalWin = (agentsEl as unknown as { agentWindow: AgentListWindow }).agentWindow;
    const liveDirect1 = makeModeAgent('live-direct-1', 'proj-1', 'direct');
    const liveOwner1 = makeModeAgent('live-owner-1', 'proj-1', 'owner');
    expect(globalWin.passesCommittedQuery(liveDirect1)).toBe(true);
    expect(globalWin.passesCommittedQuery(liveOwner1)).toBe(false);

    dispatchSseCreate(liveDirect1);
    dispatchSseCreate(liveOwner1);
    // Display reflects committed mode:direct; held set stays complete when loadedServerQ === ''.
    expect(globalWin.display.map((a) => a.id)).toContain('live-direct-1');
    expect(globalWin.display.map((a) => a.id)).not.toContain('live-owner-1');
    agentsEl.remove();

    // 2. Project detail page: ?q=mode:owner -> commit mode:direct
    window.history.replaceState(null, '', '/projects/proj-123?q=mode%3Aowner');
    const projFetch = vi.fn((input: RequestInfo | URL) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      if (url.includes('/api/v1/projects/proj-123/agents')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              agents: [makeModeAgent('p-owner-0', 'proj-123', 'owner')],
              complete: true,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ id: 'proj-123', name: 'Proj 123', slug: 'proj-123' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    });
    vi.stubGlobal('fetch', projFetch);

    const projEl = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projEl.projectId = 'proj-123';
    document.body.appendChild(projEl);
    await new Promise((r) => setTimeout(r, 20));
    await projEl.updateComplete;

    const projFetchBefore = projFetch.mock.calls.length;
    const box2 = projEl.shadowRoot?.querySelector('scion-filter-box');
    const input2 = box2?.shadowRoot?.querySelector('input') as HTMLInputElement;
    input2.value = 'mode:direct';
    input2.dispatchEvent(new Event('input'));
    input2.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await projEl.updateComplete;

    expect(projFetch.mock.calls.length).toBe(projFetchBefore);

    const projWin = (projEl as unknown as { agentWindow: AgentListWindow }).agentWindow;
    const liveDirect2 = makeModeAgent('live-proj-direct-1', 'proj-123', 'direct');
    const liveOwner2 = makeModeAgent('live-proj-owner-1', 'proj-123', 'owner');
    expect(projWin.passesCommittedQuery(liveDirect2)).toBe(true);
    expect(projWin.passesCommittedQuery(liveOwner2)).toBe(false);

    dispatchSseCreate(liveDirect2);
    dispatchSseCreate(liveOwner2);
    expect(projWin.display.map((a) => a.id)).toContain('live-proj-direct-1');
    expect(projWin.display.map((a) => a.id)).not.toContain('live-proj-owner-1');
    projEl.remove();
  });

  it('keeps SSE-created agents in non-matching phase visible after clearing a phase chip without refetch (global and project)', async () => {
    const dispatchSseCreate = (agent: Agent): void => {
      stateManager.seedAgents([agent]);
      stateManager.dispatchEvent(
        new CustomEvent('agents-changed', {
          detail: {
            data: {
              upserted: [agent.id],
              deleted: [],
              unknown: new Map(),
              generation: stateManager.scopeGeneration,
            },
          },
        })
      );
    };

    // 1. Global agents page: complete unfiltered set -> phase:running -> SSE create in provisioning -> clear phase chip.
    const globalFetch = vi.fn(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            agents: [
              {
                id: 'g-run',
                name: 'g-run',
                slug: 'g-run',
                projectId: 'proj-1',
                template: 'developer',
                phase: 'running',
              },
            ],
            complete: true,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } }
        )
      )
    );
    vi.stubGlobal('fetch', globalFetch);
    const agentsEl = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(agentsEl);
    await new Promise((r) => setTimeout(r, 20));
    await agentsEl.updateComplete;

    const globalFetchesBefore = globalFetch.mock.calls.length;
    const runningBtn = Array.from(
      agentsEl.shadowRoot?.querySelectorAll('.scope-toggle button') ?? []
    ).find((b) => b.textContent?.trim() === 'Running') as HTMLButtonElement;
    runningBtn.click();
    await agentsEl.updateComplete;
    expect(globalFetch.mock.calls.length).toBe(globalFetchesBefore);

    const newGlobalAgent: Agent = {
      id: 'g-new-prov',
      name: 'g-new-prov',
      slug: 'g-new-prov',
      projectId: 'proj-1',
      template: 'developer',
      phase: 'provisioning',
    };
    dispatchSseCreate(newGlobalAgent);
    const globalWin = (agentsEl as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(globalWin.display.map((a) => a.id)).toEqual(['g-run']);

    const allPhaseBtn = Array.from(
      agentsEl.shadowRoot?.querySelectorAll('.scope-toggle button') ?? []
    ).find((b) => b.textContent?.trim() === 'All') as HTMLButtonElement;
    allPhaseBtn.click();
    await agentsEl.updateComplete;
    expect(globalFetch.mock.calls.length).toBe(globalFetchesBefore);
    expect(globalWin.display.map((a) => a.id)).toContain('g-new-prov');
    agentsEl.remove();

    // 2. Project detail page: complete unfiltered set -> phase:running -> SSE create in provisioning -> clear phase chip.
    window.history.replaceState(null, '', '/projects/proj-123');
    const projFetch = vi.fn((input: RequestInfo | URL) => {
      const url =
        typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
      if (url.includes('/api/v1/projects/proj-123/agents')) {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              agents: [
                {
                  id: 'p-run',
                  name: 'p-run',
                  slug: 'p-run',
                  projectId: 'proj-123',
                  template: 'developer',
                  phase: 'running',
                },
              ],
              complete: true,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ id: 'proj-123', name: 'Proj 123', slug: 'proj-123' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        })
      );
    });
    vi.stubGlobal('fetch', projFetch);
    const projEl = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projEl.projectId = 'proj-123';
    document.body.appendChild(projEl);
    await new Promise((r) => setTimeout(r, 20));
    await projEl.updateComplete;

    const projFetchesBefore = projFetch.mock.calls.length;
    const projRunBtn = Array.from(
      projEl.shadowRoot?.querySelectorAll('.scope-toggle button') ?? []
    ).find((b) => b.textContent?.trim() === 'Running') as HTMLButtonElement;
    projRunBtn.click();
    await projEl.updateComplete;
    expect(projFetch.mock.calls.length).toBe(projFetchesBefore);

    const newProjAgent: Agent = {
      id: 'p-new-prov',
      name: 'p-new-prov',
      slug: 'p-new-prov',
      projectId: 'proj-123',
      template: 'developer',
      phase: 'provisioning',
    };
    dispatchSseCreate(newProjAgent);
    const projWin = (projEl as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(projWin.display.map((a) => a.id)).toEqual(['p-run']);

    const projAllBtn = Array.from(
      projEl.shadowRoot?.querySelectorAll('.scope-toggle button') ?? []
    ).find((b) => b.textContent?.trim() === 'All') as HTMLButtonElement;
    projAllBtn.click();
    await projEl.updateComplete;
    expect(projFetch.mock.calls.length).toBe(projFetchesBefore);
    expect(projWin.display.map((a) => a.id)).toContain('p-new-prov');
    projEl.remove();
  });

  it('shows a feedback notice when clearing a chip removes unrepresentable text (phase:running -test and is:mine "phase:running")', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              agents: [
                {
                  id: 'a-1',
                  name: 'a-1',
                  slug: 'a-1',
                  projectId: 'proj-1',
                  template: 'developer',
                  phase: 'running',
                },
              ],
              complete: true,
            }),
            {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            }
          )
        )
      )
    );

    // 1. phase:running -test -> click All phase chip -> clears to '' with notice.
    window.history.replaceState(null, '', '/agents?q=' + encodeURIComponent('phase:running -test'));
    const el1 = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el1);
    await new Promise((r) => setTimeout(r, 20));
    await el1.updateComplete;

    const allPhaseBtn = Array.from(
      el1.shadowRoot?.querySelectorAll('.scope-toggle button') ?? []
    ).find((b) => b.textContent?.trim() === 'All') as HTMLButtonElement;
    allPhaseBtn.click();
    await new Promise((r) => setTimeout(r, 20));
    await el1.updateComplete;

    const box1 = el1.shadowRoot?.querySelector('scion-filter-box') as HTMLElement & {
      value: string;
      notice: string;
      updateComplete: Promise<boolean>;
    };
    await box1.updateComplete;
    expect(box1.value).toBe('');
    expect(box1.shadowRoot?.querySelector('.feedback')?.textContent?.trim()).toContain(
      'Cleared remaining text'
    );
    el1.remove();

    // 2. is:mine "phase:running" -> click All scope chip -> clears to '' with notice.
    window.history.replaceState(
      null,
      '',
      '/agents?q=' + encodeURIComponent('is:mine "phase:running"')
    );
    const el2 = document.createElement('scion-page-agents') as ScionPageAgents;
    el2.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u-1', email: 'u@example.com', name: 'U', role: 'member' },
    };
    document.body.appendChild(el2);
    await new Promise((r) => setTimeout(r, 20));
    await el2.updateComplete;

    const scopeAllBtn = el2.shadowRoot?.querySelector(
      '.header-actions .scope-toggle button'
    ) as HTMLButtonElement;
    scopeAllBtn.click();
    await new Promise((r) => setTimeout(r, 20));
    await el2.updateComplete;

    const box2 = el2.shadowRoot?.querySelector('scion-filter-box') as HTMLElement & {
      value: string;
      notice: string;
      updateComplete: Promise<boolean>;
    };
    await box2.updateComplete;
    expect(box2.value).toBe('');
    expect(box2.shadowRoot?.querySelector('.feedback')?.textContent?.trim()).toContain(
      'Cleared remaining text'
    );
    el2.remove();
  });

  it('keeps phase-independent header counts across fit, drain, and live provisioning create on both global and project pages', async () => {
    const seeded: Agent[] = [
      {
        id: 'a-run',
        name: 'alpha-run',
        slug: 'alpha-run',
        projectId: 'proj-123',
        template: 'developer',
        phase: 'running',
      },
      {
        id: 'a-stop',
        name: 'beta-stop',
        slug: 'beta-stop',
        projectId: 'proj-123',
        template: 'developer',
        phase: 'stopped',
      },
    ];

    const seenUrls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const rawUrl =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        seenUrls.push(rawUrl);
        const parsed = new URL(rawUrl, 'http://localhost');
        const isAgentsList =
          parsed.pathname === '/api/v1/agents' ||
          parsed.pathname === '/api/v1/projects/proj-123/agents';
        if (isAgentsList) {
          const isSorted = parsed.searchParams.has('sort');
          const q = parsed.searchParams.get('q') ?? '';
          // Realistic hub behavior:
          // - sorted fit probe is phase-independent (returns all phases when complete=true)
          // - unsorted drain endpoint applies full `q` (so sending phase:running would hide a-stop)
          const rows =
            !isSorted && q.includes('phase:running')
              ? seeded.filter((a) => a.phase === 'running')
              : seeded;
          return Promise.resolve(
            new Response(
              JSON.stringify(
                isSorted
                  ? {
                      agents: rows,
                      totalCount: rows.length,
                      complete: true,
                      stats: {
                        total: rows.length,
                        running: rows.filter((a) => a.phase === 'running').length,
                        agents: rows.map((a) => [a.id, a.phase]),
                      },
                    }
                  : { agents: rows }
              ),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'proj-123', name: 'Proj 123', slug: 'proj-123' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    const provisioningAgent: Agent = {
      id: 'a-prov',
      name: 'gamma-prov',
      slug: 'gamma-prov',
      projectId: 'proj-123',
      template: 'developer',
      phase: 'provisioning',
    };

    // 1. Global agents page: fit (updated sort) vs drain (name sort) + live provisioning create
    window.history.replaceState(null, '', '/agents?q=' + encodeURIComponent('phase:running'));
    localStorage.setItem('scion-sort-agents', JSON.stringify({ field: 'updated', dir: 'desc' }));
    const globalFit = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(globalFit);
    await new Promise((r) => setTimeout(r, 25));
    await globalFit.updateComplete;
    const globalFitWin = (globalFit as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(globalFitWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(globalFitWin.items.map((a) => a.id)).toEqual(['a-run']);
    globalFit.remove();

    seenUrls.length = 0;
    localStorage.setItem('scion-sort-agents', JSON.stringify({ field: 'name', dir: 'asc' }));
    const globalDrain = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(globalDrain);
    await new Promise((r) => setTimeout(r, 25));
    await globalDrain.updateComplete;
    const globalDrainReq = seenUrls.find((u) => u.includes('/api/v1/agents'));
    expect(globalDrainReq).toBeDefined();
    expect(globalDrainReq).not.toContain('phase%3Arunning');
    const globalDrainWin = (globalDrain as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(globalDrainWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(globalDrainWin.items.map((a) => a.id)).toEqual(['a-run']);

    stateManager.seedAgents([...seeded, provisioningAgent]);
    stateManager.dispatchEvent(
      new CustomEvent('agents-changed', {
        detail: {
          data: {
            upserted: [provisioningAgent.id],
            deleted: [],
            unknown: new Map(),
            generation: stateManager.scopeGeneration,
          },
        },
      })
    );
    await globalDrain.updateComplete;
    expect(globalDrainWin.stats).toEqual({ total: 3, running: 1, incomplete: false });
    expect(globalDrainWin.items.map((a) => a.id)).toEqual(['a-run']);
    globalDrain.remove();

    // 2. Project detail page: fit (updated sort) vs drain (name sort) + live provisioning create
    window.history.replaceState(
      null,
      '',
      '/projects/proj-123?q=' + encodeURIComponent('phase:running')
    );
    localStorage.setItem(
      'scion-sort-project-agents-proj-123',
      JSON.stringify({ field: 'updated', dir: 'desc' })
    );
    const projFit = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projFit.projectId = 'proj-123';
    document.body.appendChild(projFit);
    await new Promise((r) => setTimeout(r, 25));
    await projFit.updateComplete;
    const projFitWin = (projFit as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(projFitWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(projFitWin.items.map((a) => a.id)).toEqual(['a-run']);
    projFit.remove();

    seenUrls.length = 0;
    localStorage.setItem(
      'scion-sort-project-agents-proj-123',
      JSON.stringify({ field: 'name', dir: 'asc' })
    );
    const projDrain = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projDrain.projectId = 'proj-123';
    document.body.appendChild(projDrain);
    await new Promise((r) => setTimeout(r, 25));
    await projDrain.updateComplete;
    const projDrainReq = seenUrls.find((u) => u.includes('/api/v1/projects/proj-123/agents'));
    expect(projDrainReq).toBeDefined();
    expect(projDrainReq).not.toContain('phase%3Arunning');
    const projDrainWin = (projDrain as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(projDrainWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(projDrainWin.items.map((a) => a.id)).toEqual(['a-run']);

    stateManager.seedAgents([...seeded, provisioningAgent]);
    stateManager.dispatchEvent(
      new CustomEvent('agents-changed', {
        detail: {
          data: {
            upserted: [provisioningAgent.id],
            deleted: [],
            unknown: new Map(),
            generation: stateManager.scopeGeneration,
          },
        },
      })
    );
    await projDrain.updateComplete;
    expect(projDrainWin.stats).toEqual({ total: 3, running: 1, incomplete: false });
    expect(projDrainWin.items.map((a) => a.id)).toEqual(['a-run']);
    projDrain.remove();
  });

  it('422-refused project drains unphased and does not retry fit when switching phase chips', async () => {
    const seeded: Agent[] = [
      {
        id: 'p-run',
        name: 'p-run',
        slug: 'p-run',
        projectId: 'proj-422',
        template: 'developer',
        phase: 'running',
        labels: { env: 'x' },
      },
      {
        id: 'p-stop',
        name: 'p-stop',
        slug: 'p-stop',
        projectId: 'proj-422',
        template: 'developer',
        phase: 'stopped',
        labels: { env: 'x' },
      },
    ];

    const fitRequests: string[] = [];
    const drainRequests: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const rawUrl =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        const parsed = new URL(rawUrl, 'http://localhost');
        if (parsed.pathname === '/api/v1/projects/proj-422/agents') {
          if (parsed.searchParams.has('sort')) {
            fitRequests.push(rawUrl);
            return Promise.resolve(
              new Response(JSON.stringify({ error: { code: 'sorted_view_unavailable' } }), {
                status: 422,
                headers: { 'Content-Type': 'application/json' },
              })
            );
          }
          drainRequests.push(rawUrl);
          return Promise.resolve(
            new Response(JSON.stringify({ agents: seeded }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'proj-422', name: 'Proj 422', slug: 'proj-422' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    window.history.replaceState(
      null,
      '',
      '/projects/proj-422?q=' + encodeURIComponent('label:env=x phase:stopped')
    );
    localStorage.setItem(
      'scion-sort-project-agents-proj-422',
      JSON.stringify({ field: 'updated', dir: 'desc' })
    );
    const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    el.projectId = 'proj-422';
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 30));
    await el.updateComplete;

    expect(fitRequests).toHaveLength(1);
    expect(drainRequests).toHaveLength(1);
    expect(drainRequests[0]).not.toContain('phase%3A');
    const win = (el as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(win.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(win.items.map((a) => a.id)).toEqual(['p-stop']);

    // Click Running then Stopped phase chips: no additional fit request after the first 422.
    const buttons = Array.from(el.shadowRoot?.querySelectorAll('.scope-toggle button') ?? []);
    const runningBtn = buttons.find(
      (b) => b.textContent?.trim() === 'Running'
    ) as HTMLButtonElement;
    const stoppedBtn = buttons.find(
      (b) => b.textContent?.trim() === 'Stopped'
    ) as HTMLButtonElement;
    runningBtn.click();
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;
    expect(fitRequests).toHaveLength(1);
    expect(win.items.map((a) => a.id)).toEqual(['p-run']);
    expect(win.stats).toEqual({ total: 2, running: 1, incomplete: false });

    stoppedBtn.click();
    await new Promise((r) => setTimeout(r, 20));
    await el.updateComplete;
    expect(fitRequests).toHaveLength(1);
    expect(win.items.map((a) => a.id)).toEqual(['p-stop']);
    expect(win.stats).toEqual({ total: 2, running: 1, incomplete: false });
    el.remove();
  });

  it('admits non-matching-phase live creates matching non-phase terms under mixed ?q=phase:running label:env=x in both complete and paged states (global and project)', async () => {
    const dispatchCreate = (agent: Agent): void => {
      stateManager.seedAgents([agent]);
      stateManager.dispatchEvent(
        new CustomEvent('agents-changed', {
          detail: {
            data: {
              upserted: [agent.id],
              deleted: [],
              unknown: new Map(),
              generation: stateManager.scopeGeneration,
            },
          },
        })
      );
    };

    const runRun = (id: string, projectId: string): Agent => ({
      id,
      name: id,
      slug: id,
      projectId,
      template: 'developer',
      phase: 'running',
      labels: { env: 'x' },
      updated: '2026-02-01T00:00:00Z',
    });
    const stopX = (id: string, projectId: string): Agent => ({
      id,
      name: id,
      slug: id,
      projectId,
      template: 'developer',
      phase: 'stopped',
      labels: { env: 'x' },
      updated: '2026-02-02T00:00:00Z',
    });
    const stopY = (id: string, projectId: string): Agent => ({
      id,
      name: id,
      slug: id,
      projectId,
      template: 'developer',
      phase: 'stopped',
      labels: { env: 'y' },
      updated: '2026-02-02T00:00:00Z',
    });
    const runXNew = (id: string, projectId: string): Agent => ({
      id,
      name: id,
      slug: id,
      projectId,
      template: 'developer',
      phase: 'running',
      labels: { env: 'x' },
      updated: '2026-02-03T00:00:00Z',
    });

    const mixedQ = encodeURIComponent('phase:running label:env=x');

    // --- 1. Complete (small) state on both pages ---
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const rawUrl =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (rawUrl.includes('/agents')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                agents: [runRun('seed-run', 'proj-mix')],
                totalCount: 1,
                complete: true,
                stats: { total: 1, running: 1, agents: [['seed-run', 'running']] },
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'proj-mix', name: 'Proj Mix', slug: 'proj-mix' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    // 1a. Global page (complete)
    window.history.replaceState(null, '', `/agents?q=${mixedQ}`);
    localStorage.setItem('scion-sort-agents', JSON.stringify({ field: 'updated', dir: 'desc' }));
    const gSmall = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(gSmall);
    await new Promise((r) => setTimeout(r, 25));
    await gSmall.updateComplete;
    const gSmallWin = (gSmall as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(gSmallWin.state).toBe('small');
    expect(gSmallWin.stats).toEqual({ total: 1, running: 1, incomplete: false });
    expect(gSmallWin.items.map((a) => a.id)).toEqual(['seed-run']);

    dispatchCreate(stopX('g-stop-x', 'proj-mix'));
    dispatchCreate(stopY('g-stop-y', 'proj-mix'));
    await gSmall.updateComplete;
    expect(gSmallWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(gSmallWin.items.map((a) => a.id)).toEqual(['seed-run']);
    gSmall.remove();

    // 1b. Project page (complete)
    window.history.replaceState(null, '', `/projects/proj-mix?q=${mixedQ}`);
    localStorage.setItem(
      'scion-sort-project-agents-proj-mix',
      JSON.stringify({ field: 'updated', dir: 'desc' })
    );
    const pSmall = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    pSmall.projectId = 'proj-mix';
    document.body.appendChild(pSmall);
    await new Promise((r) => setTimeout(r, 25));
    await pSmall.updateComplete;
    const pSmallWin = (pSmall as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(pSmallWin.state).toBe('small');
    expect(pSmallWin.stats).toEqual({ total: 1, running: 1, incomplete: false });

    dispatchCreate(stopX('p-stop-x', 'proj-mix'));
    dispatchCreate(stopY('p-stop-y', 'proj-mix'));
    await pSmall.updateComplete;
    expect(pSmallWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(pSmallWin.items.map((a) => a.id)).toEqual(['seed-run']);
    pSmall.remove();

    // --- 2. Paged state on both pages ---
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const rawUrl =
          typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
        if (rawUrl.includes('/agents')) {
          return Promise.resolve(
            new Response(
              JSON.stringify({
                agents: [runRun('pg-run', 'proj-mix')],
                totalCount: 1,
                complete: false,
                nextCursor: 'cur-2',
                stats: { total: 1, running: 1, agents: [['pg-run', 'running']] },
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        }
        return Promise.resolve(
          new Response(JSON.stringify({ id: 'proj-mix', name: 'Proj Mix', slug: 'proj-mix' }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      })
    );

    // 2a. Global page (paged)
    window.history.replaceState(null, '', `/agents?q=${mixedQ}`);
    const gPaged = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(gPaged);
    await new Promise((r) => setTimeout(r, 25));
    await gPaged.updateComplete;
    const gPagedWin = (gPaged as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(gPagedWin.state).toBe('paged');
    expect(gPagedWin.stats).toEqual({ total: 1, running: 1, incomplete: false });

    dispatchCreate(stopX('gp-stop-x', 'proj-mix'));
    dispatchCreate(stopY('gp-stop-y', 'proj-mix'));
    await gPaged.updateComplete;
    expect(gPagedWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(gPagedWin.updatesAvailable).toBe(false);

    dispatchCreate(runXNew('gp-run-x', 'proj-mix'));
    await gPaged.updateComplete;
    expect(gPagedWin.stats).toEqual({ total: 3, running: 2, incomplete: false });
    expect(gPagedWin.updatesAvailable).toBe(true);
    gPaged.remove();

    // 2b. Project page (paged)
    window.history.replaceState(null, '', `/projects/proj-mix?q=${mixedQ}`);
    const pPaged = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    pPaged.projectId = 'proj-mix';
    document.body.appendChild(pPaged);
    await new Promise((r) => setTimeout(r, 25));
    await pPaged.updateComplete;
    const pPagedWin = (pPaged as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(pPagedWin.state).toBe('paged');
    expect(pPagedWin.stats).toEqual({ total: 1, running: 1, incomplete: false });

    dispatchCreate(stopX('pp-stop-x', 'proj-mix'));
    dispatchCreate(stopY('pp-stop-y', 'proj-mix'));
    await pPaged.updateComplete;
    expect(pPagedWin.stats).toEqual({ total: 2, running: 1, incomplete: false });
    expect(pPagedWin.updatesAvailable).toBe(false);

    dispatchCreate(runXNew('pp-run-x', 'proj-mix'));
    await pPaged.updateComplete;
    expect(pPagedWin.stats).toEqual({ total: 3, running: 2, incomplete: false });
    expect(pPagedWin.updatesAvailable).toBe(true);
    pPaged.remove();
  });

  it('R2: keeps path-equal counts, live-create filtering, and distinct 422 refusal keys for text/exclusion-only remainders', async () => {
    const mk = (id: string, name: string, phase: Agent['phase']): Agent => ({
      id,
      name,
      slug: name,
      projectId: 'proj-r2',
      phase,
      updated: '2026-06-01T00:00:00Z',
    });
    const allAgents = [
      mk('a-api-run', 'api server one', 'running'),
      mk('a-api-stop', 'api server two', 'stopped'),
      mk('a-api-test', 'api server test', 'running'),
      mk('a-labelx-run', 'label:x-run', 'running'),
      mk('a-test-run', 'test-runner', 'running'),
    ];
    const dispatchCreate = (agent: Agent): void => {
      stateManager.seedAgents([agent], { partial: true });
      stateManager.dispatchEvent(
        new CustomEvent('agents-changed', {
          detail: {
            state: {},
            data: {
              upserted: [agent.id],
              deleted: [],
              resync: false,
              projectId: agent.projectId,
            },
          },
        })
      );
    };

    const filterForQuery = (q: string): Agent[] => {
      if (q.includes('"api server"') && q.includes('-test')) {
        return allAgents.filter(
          (a) =>
            a.name.toLowerCase().includes('api server') && !a.name.toLowerCase().includes('test')
        );
      }
      if (q.includes('"label:x"')) {
        return allAgents.filter((a) => a.name.toLowerCase().includes('label:x'));
      }
      if (q.includes('-test')) {
        return allAgents.filter((a) => !a.name.toLowerCase().includes('test'));
      }
      return allAgents;
    };

    const installFetch = (opts: { paged?: boolean; refuseQueries?: Set<string> } = {}) => {
      const requestedFitQs: string[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: RequestInfo | URL) => {
          const rawUrl =
            typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
          const u = new URL(rawUrl, 'http://localhost');
          if (u.pathname.endsWith('/agents')) {
            const q = u.searchParams.get('q') ?? '';
            const hasFit = u.searchParams.has('fit');
            if (hasFit) requestedFitQs.push(q);
            if (hasFit && opts.refuseQueries?.has(q)) {
              return Promise.resolve(
                new Response(JSON.stringify({ error: { code: 'too_many_candidates' } }), {
                  status: 422,
                  headers: { 'Content-Type': 'application/json' },
                })
              );
            }
            const matched = filterForQuery(q);
            if (hasFit && opts.paged) {
              const running = matched.filter((a) => a.phase === 'running');
              return Promise.resolve(
                new Response(
                  JSON.stringify({
                    agents: running.slice(0, 1),
                    totalCount: running.length,
                    complete: false,
                    nextCursor: 'cur-next',
                    stats: {
                      total: matched.length,
                      running: running.length,
                      agents: matched.map((a) => [a.id, a.phase]),
                    },
                  }),
                  { status: 200, headers: { 'Content-Type': 'application/json' } }
                )
              );
            }
            return Promise.resolve(
              new Response(
                JSON.stringify({
                  agents: matched,
                  totalCount: matched.length,
                  complete: true,
                }),
                { status: 200, headers: { 'Content-Type': 'application/json' } }
              )
            );
          }
          return Promise.resolve(
            new Response(JSON.stringify({ id: 'proj-r2', name: 'Proj R2', slug: 'proj-r2' }), {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            })
          );
        })
      );
      return requestedFitQs;
    };

    const queries = [
      { raw: 'phase:running -test', want: { total: 3, running: 2, incomplete: false } },
      { raw: 'phase:running "label:x"', want: { total: 1, running: 1, incomplete: false } },
      { raw: 'phase:running api server -test', want: { total: 2, running: 1, incomplete: false } },
    ];

    for (const { raw, want } of queries) {
      const encoded = encodeURIComponent(raw);

      // (ii) & (iii) Global page: sort=updated (fit), sort=name (drain), tree (drain), paged -> identical stats
      for (const mode of ['fit', 'sort-name', 'tree', 'paged'] as const) {
        installFetch({ paged: mode === 'paged' });
        localStorage.clear();
        if (mode === 'sort-name') {
          localStorage.setItem('scion-sort-agents', JSON.stringify({ field: 'name', dir: 'asc' }));
        } else if (mode === 'tree') {
          localStorage.setItem('scion-view-agents', 'tree');
        } else {
          localStorage.setItem(
            'scion-sort-agents',
            JSON.stringify({ field: 'updated', dir: 'desc' })
          );
        }
        window.history.replaceState(null, '', `/agents?q=${encoded}`);
        const el = document.createElement('scion-page-agents') as ScionPageAgents;
        document.body.appendChild(el);
        await new Promise((r) => setTimeout(r, 25));
        await el.updateComplete;
        const win = (el as unknown as { agentWindow: AgentListWindow }).agentWindow;
        expect(win.stats, `global ${mode} ${raw}`).toEqual(want);

        // (i) SSE create of excluded stopped test-bot must not change counts
        if (mode === 'fit') {
          dispatchCreate(mk(`sse-excl-g-${raw}`, 'test-bot', 'stopped'));
          await el.updateComplete;
          expect(win.stats, `global fit SSE excluded ${raw}`).toEqual(want);
        }
        el.remove();
      }

      // (ii) & (iii) Project page: sort=updated (fit), sort=name (drain), tree (drain), paged -> identical stats
      for (const mode of ['fit', 'sort-name', 'tree', 'paged'] as const) {
        installFetch({ paged: mode === 'paged' });
        localStorage.clear();
        if (mode === 'sort-name') {
          localStorage.setItem(
            'scion-sort-project-agents-proj-r2',
            JSON.stringify({ field: 'name', dir: 'asc' })
          );
        } else if (mode === 'tree') {
          localStorage.setItem('scion-view-project-agents-proj-r2', 'tree');
        } else {
          localStorage.setItem(
            'scion-sort-project-agents-proj-r2',
            JSON.stringify({ field: 'updated', dir: 'desc' })
          );
        }
        window.history.replaceState(null, '', `/projects/proj-r2?q=${encoded}`);
        const el = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
        el.projectId = 'proj-r2';
        document.body.appendChild(el);
        await new Promise((r) => setTimeout(r, 25));
        await el.updateComplete;
        const win = (el as unknown as { agentWindow: AgentListWindow }).agentWindow;
        expect(win.stats, `project ${mode} ${raw}`).toEqual(want);

        // (i) SSE create of excluded stopped test-bot must not change counts
        if (mode === 'fit') {
          dispatchCreate(mk(`sse-excl-p-${raw}`, 'test-bot', 'stopped'));
          await el.updateComplete;
          expect(win.stats, `project fit SSE excluded ${raw}`).toEqual(want);
        }
        el.remove();
      }
    }

    // (iv) Project page 422 refusal key: refusal for `phase:running -test` must not suppress
    // the sorted fit attempt for unfiltered `""`, and vice versa.
    localStorage.clear();
    localStorage.setItem(
      'scion-sort-project-agents-proj-r2',
      JSON.stringify({ field: 'updated', dir: 'desc' })
    );
    const fitQs1 = installFetch({ refuseQueries: new Set(['phase:running -test']) });
    window.history.replaceState(
      null,
      '',
      `/projects/proj-r2?q=${encodeURIComponent('phase:running -test')}`
    );
    const pRef1 = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    pRef1.projectId = 'proj-r2';
    document.body.appendChild(pRef1);
    await new Promise((r) => setTimeout(r, 25));
    await pRef1.updateComplete;
    expect(fitQs1).toContain('phase:running -test');
    const winRef1 = (pRef1 as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(winRef1.isSortedRefused('-test')).toBe(true);
    expect(winRef1.isSortedRefused('')).toBe(false);
    pRef1.remove();

    const fitQs2 = installFetch({ refuseQueries: new Set(['']) });
    window.history.replaceState(null, '', '/projects/proj-r2');
    const pRef2 = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    pRef2.projectId = 'proj-r2';
    document.body.appendChild(pRef2);
    await new Promise((r) => setTimeout(r, 25));
    await pRef2.updateComplete;
    expect(fitQs2).toContain('');
    const winRef2 = (pRef2 as unknown as { agentWindow: AgentListWindow }).agentWindow;
    expect(winRef2.isSortedRefused('')).toBe(true);
    expect(winRef2.isSortedRefused('-test')).toBe(false);
    pRef2.remove();
  });

  it('inline Graph views on /agents and /projects/:id render non-matching ancestors dimmed as context (ptone/scion#9301)', async () => {
    const parentAgent: Agent = {
      id: 'a-parent',
      name: 'Parent Coord',
      slug: 'parent-coord',
      projectId: 'proj-g',
      projectSlug: 'alpha',
      template: 'coordinator',
      phase: 'stopped',
      harnessConfig: 'claude',
      ancestry: ['user:u1'],
    };
    const childAgent: Agent = {
      id: 'a-child',
      name: 'Child Dev',
      slug: 'child-dev',
      projectId: 'proj-g',
      projectSlug: 'alpha',
      template: 'developer',
      phase: 'running',
      harnessConfig: 'antigravity',
      ancestry: ['user:u1', 'a-parent'],
    };
    const unrelatedAgent: Agent = {
      id: 'a-unrelated',
      name: 'Unrelated',
      slug: 'unrelated',
      projectId: 'proj-g',
      projectSlug: 'alpha',
      template: 'developer',
      phase: 'stopped',
      harnessConfig: 'codex',
      ancestry: ['user:u1'],
    };
    const allAgents = [parentAgent, childAgent, unrelatedAgent];

    const installGraphFetch = (): string[] => {
      const urls: string[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: RequestInfo | URL) => {
          const url =
            typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
          urls.push(url);
          if (url.includes('/agents')) {
            const parsed = new URL(url, 'http://localhost');
            const idsParam = parsed.searchParams.get('ids');
            const q = parsed.searchParams.get('q') ?? '';
            let rows = allAgents;
            if (idsParam) {
              const wanted = new Set(idsParam.split(',').filter(Boolean));
              rows = allAgents.filter((a) => wanted.has(a.id));
            } else if (q.includes('harness:antigravity')) {
              // Simulate server-side ?q= filtering (which strips non-matching ancestors).
              rows = [childAgent];
            }
            return Promise.resolve(
              new Response(
                JSON.stringify({ agents: rows, complete: true, totalCount: rows.length }),
                { status: 200, headers: { 'Content-Type': 'application/json' } }
              )
            );
          }
          return Promise.resolve(
            new Response(
              JSON.stringify({
                id: 'proj-g',
                name: 'Alpha',
                slug: 'alpha',
                _capabilities: { actions: ['read'] },
              }),
              { status: 200, headers: { 'Content-Type': 'application/json' } }
            )
          );
        })
      );
      return urls;
    };

    const resetStore = (): void => {
      // Force a scope transition so stateManager clears all cached agents.
      stateManager.setScope({ type: 'project', projectId: `reset-${Math.random()}` });
    };

    const assertTreeContext = async (tree: ScionAgentTreeView): Promise<void> => {
      await vi.waitFor(async () => {
        await tree.updateComplete;
        expect(tree.agents.map((a) => a.id).sort()).toEqual(['a-child', 'a-parent']);
      });
      expect(Array.from(tree.contextIds ?? [])).toEqual(['a-parent']);
      const parentLink = tree.shadowRoot!.querySelector<HTMLAnchorElement>(
        'a.node[data-agent-id="a-parent"]'
      );
      const childLink = tree.shadowRoot!.querySelector<HTMLAnchorElement>(
        'a.node[data-agent-id="a-child"]'
      );
      expect(parentLink).not.toBeNull();
      expect(parentLink!.classList.contains('context')).toBe(true);
      expect(parentLink!.getAttribute('aria-description')).toBe(
        'shown as context; does not match filter'
      );
      expect(childLink).not.toBeNull();
      expect(childLink!.classList.contains('context')).toBe(false);
      expect(tree.shadowRoot!.querySelector('a.node[data-agent-id="a-unrelated"]')).toBeNull();
    };

    // 1. Cold load of /agents inline Graph view with server-narrowed ?q=harness:antigravity
    resetStore();
    const globalColdUrls = installGraphFetch();
    localStorage.clear();
    localStorage.setItem('scion-view-agents', 'graph');
    window.history.replaceState(null, '', '/agents?q=harness%3Aantigravity');
    const agentsCold = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(agentsCold);
    await vi.waitFor(() => {
      expect(agentsCold.shadowRoot?.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    const coldGlobalTree = agentsCold.shadowRoot!.querySelector(
      'scion-agent-tree-view'
    ) as ScionAgentTreeView;
    await assertTreeContext(coldGlobalTree);
    expect(globalColdUrls.some((u) => u.includes('ids=a-parent') && !u.includes('q='))).toBe(true);

    // Live SSE deletion of a store-resolved context ancestor invalidates the graph cache.
    stateManager.seedAgents([parentAgent], { partial: true });
    const deleteDetail = {
      upserted: [] as string[],
      deleted: ['a-parent'],
      unknown: new Map(),
      generation: 1,
    };
    (stateManager as unknown as { state: { agents: Map<string, Agent> } }).state.agents.delete(
      'a-parent'
    );
    stateManager.dispatchEvent(
      new CustomEvent('agents-changed', { detail: { state: {}, data: deleteDetail } })
    );
    await vi.waitFor(async () => {
      await agentsCold.updateComplete;
      await coldGlobalTree.updateComplete;
      expect(coldGlobalTree.agents.map((a) => a.id)).toEqual(['a-child']);
    });
    agentsCold.remove();

    // 2. Cold load of /agents inline Graph view with ?q=phase:running (ancestors in held set)
    resetStore();
    const globalPhaseUrls = installGraphFetch();
    localStorage.clear();
    localStorage.setItem('scion-view-agents', 'graph');
    window.history.replaceState(null, '', '/agents?q=phase%3Arunning');
    const agentsPage = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(agentsPage);
    await vi.waitFor(() => {
      expect(agentsPage.shadowRoot?.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    await agentsPage.updateComplete;
    await assertTreeContext(
      agentsPage.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView
    );
    expect(globalPhaseUrls.some((u) => u.includes('ids='))).toBe(false);
    agentsPage.remove();

    // 3. Global /agents inline Graph view narrowing from unfiltered to server-filtered harness:antigravity
    resetStore();
    installGraphFetch();
    localStorage.clear();
    localStorage.setItem('scion-view-agents', 'graph');
    window.history.replaceState(null, '', '/agents');
    const agentsNarrow = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(agentsNarrow);
    await vi.waitFor(() => {
      expect(agentsNarrow.shadowRoot?.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    const agentsBox = agentsNarrow.shadowRoot!.querySelector('scion-filter-box')!;
    agentsBox.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: {
          value: 'harness:antigravity',
          query: {
            mode: 'structured',
            terms: [{ key: 'harness', values: ['antigravity'], negate: false }],
          },
          serverQ: 'harness:antigravity',
        },
        bubbles: true,
        composed: true,
      })
    );
    await new Promise((r) => setTimeout(r, 25));
    await agentsNarrow.updateComplete;
    await assertTreeContext(
      agentsNarrow.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView
    );
    agentsNarrow.remove();

    // 4. Cold load of /projects/:id inline Graph view with server-narrowed ?q=harness:antigravity
    resetStore();
    const projColdUrls = installGraphFetch();
    localStorage.clear();
    localStorage.setItem('scion-view-project-agents', 'graph');
    window.history.replaceState(null, '', '/projects/proj-g?q=harness%3Aantigravity');
    const projCold = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projCold.projectId = 'proj-g';
    document.body.appendChild(projCold);
    await vi.waitFor(() => {
      expect(projCold.shadowRoot?.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    await assertTreeContext(
      projCold.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView
    );
    expect(
      projColdUrls.some(
        (u) =>
          u.includes('/api/v1/projects/proj-g/agents') &&
          u.includes('ids=a-parent') &&
          !u.includes('q=')
      )
    ).toBe(true);
    projCold.remove();

    // 5. Cold load of /projects/:id inline Graph view with ?q=phase:running (ancestors in held set)
    resetStore();
    const projPhaseUrls = installGraphFetch();
    localStorage.clear();
    localStorage.setItem('scion-view-project-agents', 'graph');
    window.history.replaceState(null, '', '/projects/proj-g?q=phase%3Arunning');
    const projPage = document.createElement('scion-page-project-detail') as ScionPageProjectDetail;
    projPage.projectId = 'proj-g';
    document.body.appendChild(projPage);
    await vi.waitFor(() => {
      expect(projPage.shadowRoot?.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    await projPage.updateComplete;
    await assertTreeContext(
      projPage.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView
    );
    expect(projPhaseUrls.some((u) => u.includes('ids='))).toBe(false);
    projPage.remove();

    // 6. Project /projects/:id inline Graph view narrowing from unfiltered to server-filtered harness:antigravity
    resetStore();
    installGraphFetch();
    localStorage.clear();
    localStorage.setItem('scion-view-project-agents', 'graph');
    window.history.replaceState(null, '', '/projects/proj-g');
    const projNarrow = document.createElement(
      'scion-page-project-detail'
    ) as ScionPageProjectDetail;
    projNarrow.projectId = 'proj-g';
    document.body.appendChild(projNarrow);
    await vi.waitFor(() => {
      expect(projNarrow.shadowRoot?.querySelector('scion-agent-tree-view')).not.toBeNull();
    });
    const projBox = projNarrow.shadowRoot!.querySelector('scion-filter-box')!;
    projBox.dispatchEvent(
      new CustomEvent('filter-commit', {
        detail: {
          value: 'harness:antigravity',
          query: {
            mode: 'structured',
            terms: [{ key: 'harness', values: ['antigravity'], negate: false }],
          },
          serverQ: 'harness:antigravity',
        },
        bubbles: true,
        composed: true,
      })
    );
    await new Promise((r) => setTimeout(r, 25));
    await projNarrow.updateComplete;
    await assertTreeContext(
      projNarrow.shadowRoot!.querySelector('scion-agent-tree-view') as ScionAgentTreeView
    );
    projNarrow.remove();
  });
});
