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
import { resetServerFlagStateForTests, setFeatureFlag } from '../../utils/feature-flags.js';
import { FILTERS_QUERY_DIALECT_FLAG } from '../../shared/filter-query/index.js';
import { stateManager } from '../../client/state.js';
import type { AgentListWindow } from '../../client/agent-list-window.js';
import type { Agent } from '../../shared/types.js';
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

  it('renders legacy label input when filters.query_dialect is off', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, false);
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('scion-filter-box')).toBeNull();
    expect(el.shadowRoot?.querySelector('sl-input[placeholder*="label"]')).not.toBeNull();
  });

  it('hydrates ?q=activity%3Awaiting_for_input in both dialect-on and dialect-off modes (ptone/scion#9300)', async () => {
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

    // 1. Dialect ON: hydrates <scion-filter-box> and sends q=activity%3Awaiting_for_input.
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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

    // 2. Dialect OFF: hydrates activityFilter, ignores persisted scope/phase/mode
    // for this load, renders removable tag, and filters client-side.
    fetchUrls = [];
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, false);
    localStorage.setItem('scion-scope-agents', 'mine');
    localStorage.setItem('scion-filter-agents-phase', 'error');
    localStorage.setItem('scion-filter-agents-mode', 'lineage');
    window.history.replaceState(null, '', '/agents?q=activity%3Awaiting_for_input');
    const elOff = document.createElement('scion-page-agents') as ScionPageAgents;
    elOff.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u1', email: 'u1@example.com', name: 'U1', role: 'admin' },
    };
    document.body.appendChild(elOff);
    await new Promise((r) => setTimeout(r, 15));
    await elOff.updateComplete;

    const tag = elOff.shadowRoot?.querySelector('[data-activity-filter-tag]');
    expect(tag?.textContent?.trim()).toBe('activity:waiting_for_input');
    expect(fetchUrls.some((u) => u.includes('scope=mine') || u.includes('phase=error'))).toBe(
      false
    );
    expect(elOff.shadowRoot?.textContent).toContain('Waiting Bot');
    expect(elOff.shadowRoot?.textContent).not.toContain('Thinking Bot');

    // Removing the tag clears the filter and strips ?q= from the URL.
    tag?.dispatchEvent(new CustomEvent('sl-remove', { bubbles: true }));
    await elOff.updateComplete;
    expect(new URLSearchParams(window.location.search).get('q')).toBeNull();
    expect(elOff.shadowRoot?.querySelector('[data-activity-filter-tag]')).toBeNull();
    expect(elOff.shadowRoot?.textContent).toContain('Thinking Bot');
    elOff.remove();
  });

  it('composes Phase and key-only label filters with the dialect-off activity tag', async () => {
    const runningWaiting: Agent = {
      id: 'a-run-wait',
      name: 'Running Waiting',
      slug: 'running-waiting',
      projectId: 'p1',
      template: 'developer',
      phase: 'running',
      activity: 'waiting_for_input',
      labels: { team: 'ui' },
    };
    const stoppedWaiting: Agent = {
      id: 'a-stop-wait',
      name: 'Stopped Waiting',
      slug: 'stopped-waiting',
      projectId: 'p1',
      template: 'developer',
      phase: 'stopped',
      activity: 'waiting_for_input',
      labels: { env: 'prod' },
    };
    const runningThinking: Agent = {
      id: 'a-run-think',
      name: 'Running Thinking',
      slug: 'running-thinking',
      projectId: 'p1',
      template: 'developer',
      phase: 'running',
      activity: 'thinking',
      labels: { team: 'ui' },
    };
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              agents: [runningWaiting, stoppedWaiting, runningThinking],
              complete: true,
              totalCount: 3,
            }),
            { status: 200, headers: { 'Content-Type': 'application/json' } }
          )
        )
      )
    );

    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, false);
    window.history.replaceState(null, '', '/agents?q=activity%3Awaiting_for_input');
    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 15));
    await el.updateComplete;

    expect(el.shadowRoot?.textContent).toContain('Running Waiting');
    expect(el.shadowRoot?.textContent).toContain('Stopped Waiting');
    expect(el.shadowRoot?.textContent).not.toContain('Running Thinking');

    // Choosing Phase=stopped composes with activity:waiting_for_input.
    const phaseButtons = Array.from(
      el.shadowRoot?.querySelectorAll('.filter-bar .scope-toggle button') ?? []
    );
    const stoppedBtn = phaseButtons.find((b) =>
      b.textContent?.trim().startsWith('Stopped')
    ) as HTMLButtonElement;
    expect(stoppedBtn).toBeDefined();
    stoppedBtn.click();
    await el.updateComplete;

    expect(el.shadowRoot?.textContent).toContain('Stopped Waiting');
    expect(el.shadowRoot?.textContent).not.toContain('Running Waiting');
    expect(el.shadowRoot?.textContent).not.toContain('Running Thinking');

    // Reset Phase to All, then type a key-only label filter ("team").
    const allBtn = phaseButtons.find((b) =>
      b.textContent?.trim().startsWith('All')
    ) as HTMLButtonElement;
    expect(allBtn).toBeDefined();
    allBtn.click();
    await el.updateComplete;

    const labelInput = el.shadowRoot?.querySelector('sl-input[placeholder*="label"]') as
      | (HTMLElement & { value: string })
      | null;
    expect(labelInput).not.toBeNull();
    labelInput!.value = 'team';
    labelInput!.dispatchEvent(new CustomEvent('sl-input', { bubbles: true }));
    await el.updateComplete;

    expect(el.shadowRoot?.textContent).toContain('Running Waiting');
    expect(el.shadowRoot?.textContent).not.toContain('Stopped Waiting');
    expect(el.shadowRoot?.textContent).not.toContain('Running Thinking');
    el.remove();
  });

  it('migrates localStorage phase once to ?q= and renders <scion-filter-box> when on', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
    window.history.replaceState(null, '', '/agents?q=phase%3Aerror');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    const heading = el.shadowRoot?.querySelector('.empty-state h2')?.textContent?.trim();
    expect(heading).toBe('No Matching Agents');
  });

  it('keeps legacy empty-state behaviour unchanged when filters.query_dialect is off', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, false);
    localStorage.setItem('scion-filter-agents-phase', 'error');

    const el = document.createElement('scion-page-agents') as ScionPageAgents;
    document.body.appendChild(el);
    await new Promise((r) => setTimeout(r, 10));
    await el.updateComplete;

    // With experiment off and zero total agents returned, legacy renders "No Agents Found".
    const heading = el.shadowRoot?.querySelector('.empty-state h2')?.textContent?.trim();
    expect(heading).toBe('No Agents Found');
  });

  it('retires old mode dropdown and migrates scope/mode from localStorage when dialect is on', async () => {
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    setFeatureFlag(FILTERS_QUERY_DIALECT_FLAG, true);
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
    expect(globalWin.passesCommittedLabel(liveDirect1)).toBe(true);
    expect(globalWin.passesCommittedLabel(liveOwner1)).toBe(false);

    dispatchSseCreate(liveDirect1);
    dispatchSseCreate(liveOwner1);
    expect((agentsEl as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toContain(
      'live-direct-1'
    );
    expect((agentsEl as unknown as { agents: Agent[] }).agents.map((a) => a.id)).not.toContain(
      'live-owner-1'
    );
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
    expect(projWin.passesCommittedLabel(liveDirect2)).toBe(true);
    expect(projWin.passesCommittedLabel(liveOwner2)).toBe(false);

    dispatchSseCreate(liveDirect2);
    dispatchSseCreate(liveOwner2);
    expect((projEl as unknown as { agents: Agent[] }).agents.map((a) => a.id)).toContain(
      'live-proj-direct-1'
    );
    expect((projEl as unknown as { agents: Agent[] }).agents.map((a) => a.id)).not.toContain(
      'live-proj-owner-1'
    );
    projEl.remove();
  });
});
