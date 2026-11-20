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
 * Unit tests for `<scion-page-profile-decisions>` (ptone/scion#7996, Mocks R-1..R-4).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type { DecisionView, PageData } from '../../shared/types.js';
import type {
  ProjectDecisionLogEntryView,
  ProjectDecisionProjectRef,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import {
  PROFILE_DECISIONS_COUNT_CHANGED_EVENT,
  resetProfileDecisionProbeCacheForTests,
  type ProfileMadeByProjectDecisionsResponse,
  type ProfileMadeRecentDecisionsResponse,
  type ProfilePendingDecisionsResponse,
} from '../../client/profile-decisions.js';
import { stateManager } from '../../client/state.js';
import { avatarColor, avatarInitial } from '../shared/decisions-format.js';
import {
  DECISIONS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';

class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
    setTimeout(() => {
      if (this.readyState === FakeEventSource.CLOSED) return;
      this.readyState = FakeEventSource.OPEN;
      this.onopen?.(new Event('open'));
    }, 0);
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function jsonResponse(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

import '@shoelace-style/shoelace/dist/components/radio-group/radio-group.js';
import '@shoelace-style/shoelace/dist/components/radio-button/radio-button.js';
await import('./profile-decisions.js');
type ScionPageProfileDecisions = import('./profile-decisions.js').ScionPageProfileDecisions;
type ScionChatDecisions = import('../shared/chat/chat-decisions.js').ScionChatDecisions;
type ScionDecisionsFilterPanel =
  import('../shared/decisions-filter-panel.js').ScionDecisionsFilterPanel;
type ScionFacetCombobox = import('../shared/facet-combobox.js').ScionFacetCombobox;

const PROJ_SCION: ProjectDecisionProjectRef = { id: 'proj-1', name: 'scion', slug: 'scion' };
const PROJ_WEB: ProjectDecisionProjectRef = { id: 'proj-2', name: 'web-site', slug: 'web-site' };

const AGENT_REL = '11111111-2222-3333-4444-555555555555';
const AGENT_NCX = '22222222-3333-4444-5555-666666666666';
const AGENT_DES = '33333333-4444-5555-6666-777777777777';

function makeDecision(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r-1',
    messageId: 'm-1',
    key: 'ship-now',
    ref: 'ship-now',
    version: 1,
    status: 'pending',
    askerAgentId: AGENT_REL,
    askerAgentName: 'release-bot',
    question: 'Deploy to prod after tagging v0.9?',
    defaultChoiceId: 'yes',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'wait', label: 'Wait for QA' },
    ],
    ...over,
  };
}

function makePrompt(over: Partial<ProjectDecisionPromptView> = {}): ProjectDecisionPromptView {
  return {
    project: PROJ_SCION,
    promptMessageId: 'm-1',
    promptAvailable: true,
    chatKey: 'topic:release',
    conversationLabel: '#release',
    conversationAvailable: true,
    askerAgentId: AGENT_REL,
    askerAgentName: 'release-bot',
    createdAt: '2026-11-15T07:00:00Z',
    answerMode: 'single',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [makeDecision()],
    ...over,
  };
}

function makeLogEntry(
  over: Partial<ProjectDecisionLogEntryView> = {}
): ProjectDecisionLogEntryView {
  return {
    project: PROJ_SCION,
    answerId: 'ans-1',
    decision: {
      rowId: 'r-1',
      promptMessageId: 'm-1',
      promptAvailable: true,
      key: 'D9',
      ref: 'D9',
      version: 1,
      question: 'Cut the RC from main?',
      askerAgentId: AGENT_REL,
      askerAgentName: 'release-bot',
    },
    choiceId: 'no',
    label: 'No',
    matchedDefault: false,
    previousChoiceLabel: 'Yes',
    answeredByUserId: 'u-me',
    answeredByName: 'Alice',
    provenance: 'direct',
    outcome: 'override',
    surface: 'decisions_page',
    at: '2026-11-15T06:52:00Z',
    chatKey: 'topic:release',
    conversationLabel: '#release',
    conversationAvailable: true,
    sourceMessageAvailable: false,
    ...over,
  };
}

async function settle(el: ScionPageProfileDecisions, ms = 15): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
  await el.updateComplete;
  const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
    'scion-decisions-filter-panel'
  );
  await panel?.updateComplete;
  for (const cbx of Array.from(
    el.shadowRoot?.querySelectorAll<ScionFacetCombobox>('scion-facet-combobox') ?? []
  )) {
    await cbx.updateComplete;
  }
  for (const card of Array.from(
    el.shadowRoot?.querySelectorAll<ScionChatDecisions>('scion-chat-decisions') ?? []
  )) {
    await card.updateComplete;
  }
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('<scion-page-profile-decisions> (ptone/scion#7996)', () => {
  let requestedUrls: string[];
  let answerRequests: Array<{ url: string; body: Record<string, unknown> }>;
  let handleDecisionsFetch: (url: URL) => Promise<Response>;

  beforeAll(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  beforeEach(() => {
    requestedUrls = [];
    answerRequests = [];
    resetProfileDecisionProbeCacheForTests();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    window.history.replaceState(null, '', '/profile/decisions');

    const defaultPending: ProfilePendingDecisionsResponse = {
      counts: { waitingOnYou: 4, waitingOnOthers: 1 },
      prompts: [
        makePrompt({
          project: PROJ_SCION,
          promptMessageId: 'm-1',
          decisions: [makeDecision({ rowId: 'r-1', messageId: 'm-1', key: 'ship-now' })],
        }),
        makePrompt({
          project: PROJ_SCION,
          promptMessageId: 'm-2',
          chatKey: 'dm:ncx',
          conversationLabel: 'DM with nc-x',
          askerAgentId: AGENT_NCX,
          askerAgentName: 'nc-x',
          answerMode: 'set',
          decisions: [
            makeDecision({ rowId: 'r-2a', messageId: 'm-2', key: 'migrate' }),
            makeDecision({ rowId: 'r-2b', messageId: 'm-2', key: 'D12' }),
          ],
        }),
        makePrompt({
          project: PROJ_WEB,
          promptMessageId: 'm-3',
          chatKey: 'topic:landing',
          conversationLabel: '#landing',
          askerAgentId: AGENT_DES,
          askerAgentName: 'design-bot',
          decisions: [makeDecision({ rowId: 'r-3', messageId: 'm-3', key: 'hero-img' })],
        }),
      ],
      agents: [
        { id: AGENT_DES, name: 'design-bot', projectId: 'proj-2' },
        { id: AGENT_NCX, name: 'nc-x', projectId: 'proj-1' },
        { id: AGENT_REL, name: 'release-bot' },
      ],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [
        { key: 'topic:landing', label: '#landing', projectId: 'proj-2' },
        { key: 'topic:release', label: '#release', projectId: 'proj-1' },
      ],
      projects: [PROJ_SCION, PROJ_WEB],
      truncatedProjects: false,
    };

    const defaultMadeByProject: Omit<ProfileMadeByProjectDecisionsResponse, 'sort'> = {
      counts: { waitingOnYou: 4, waitingOnOthers: 1 },
      groups: [
        {
          project: PROJ_SCION,
          entries: [
            makeLogEntry({ answerId: 'ans-1', project: PROJ_SCION }),
            makeLogEntry({
              answerId: 'ans-2',
              project: PROJ_SCION,
              decision: {
                rowId: 'r-17',
                promptMessageId: 'm-17',
                promptAvailable: true,
                key: '17',
                ref: '17',
                version: 1,
                question: 'Which approach?',
                askerAgentId: AGENT_NCX,
                askerAgentName: 'nc-x',
              },
            }),
          ],
          more: true,
        },
        {
          project: PROJ_WEB,
          entries: [
            makeLogEntry({
              answerId: 'ans-3',
              project: PROJ_WEB,
              chatKey: 'topic:landing',
              conversationLabel: '#landing',
              decision: {
                rowId: 'r-font',
                promptMessageId: 'm-font',
                promptAvailable: true,
                key: 'font',
                ref: 'font',
                version: 1,
                question: 'Which body font?',
                askerAgentId: AGENT_DES,
                askerAgentName: 'design-bot',
              },
            }),
          ],
          more: false,
        },
      ],
      agents: defaultPending.agents,
      answerers: defaultPending.answerers,
      conversations: defaultPending.conversations ?? [],
      projects: defaultPending.projects,
      truncatedProjects: false,
    };

    const defaultMadeRecent: Omit<ProfileMadeRecentDecisionsResponse, 'sort'> = {
      counts: { waitingOnYou: 4, waitingOnOthers: 1 },
      entries: [
        makeLogEntry({ answerId: 'ans-1', project: PROJ_SCION }),
        makeLogEntry({ answerId: 'ans-3', project: PROJ_WEB, conversationLabel: '#landing' }),
      ],
      agents: defaultPending.agents,
      answerers: defaultPending.answerers,
      conversations: defaultPending.conversations ?? [],
      projects: defaultPending.projects,
      truncatedProjects: false,
      nextCursor: 'made-cur-2',
    };

    handleDecisionsFetch = (url: URL): Promise<Response> => {
      const tab = url.searchParams.get('tab') || 'pending';
      const sort = url.searchParams.get('sort') || (tab === 'made' ? 'recent' : 'project');
      const projectFilter = url.searchParams.get('project') || '';
      if (tab === 'pending') {
        if (projectFilter) {
          const filtered = defaultPending.prompts.filter((p) => p.project?.id === projectFilter);
          return jsonResponse({
            ...defaultPending,
            filteredCounts: {
              waitingOnYou: filtered.reduce((n, p) => n + p.decisions.length, 0),
              waitingOnOthers: 0,
            },
            prompts: filtered,
          });
        }
        return jsonResponse(defaultPending);
      }
      if (sort === 'project') {
        return jsonResponse(defaultMadeByProject);
      }
      const entries = projectFilter
        ? defaultMadeRecent.entries.filter((e) => e.project?.id === projectFilter)
        : defaultMadeRecent.entries;
      return jsonResponse({
        ...defaultMadeRecent,
        entries,
      });
    };

    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        if (url.pathname === '/api/v1/users/me/decisions') {
          requestedUrls.push(url.pathname + url.search);
          return handleDecisionsFetch(url);
        }
        if (/^\/api\/v1\/decisions\/([^/]+)\/answer$/.test(url.pathname)) {
          const body =
            typeof init?.body === 'string'
              ? (JSON.parse(init.body) as Record<string, unknown>)
              : {};
          answerRequests.push({ url: url.pathname, body });
          const rowId = url.pathname.split('/')[4] ?? 'r-1';
          const choiceId = typeof body.choice_id === 'string' ? body.choice_id : 'yes';
          const surface = typeof body.surface === 'string' ? body.surface : 'decisions_page';
          return jsonResponse({
            decision: makeDecision({
              rowId,
              status: 'decided',
              decision: {
                choiceId,
                label: 'Yes',
                answeredByUserId: 'u-me',
                answeredByName: 'Alice',
                provenance: 'direct',
                surface,
                responseMessageId: 'resp-1',
                at: '2026-11-15T07:05:00Z',
              },
            }),
          });
        }
        if (url.pathname === '/auth/me') {
          return jsonResponse({ id: 'u-me' });
        }
        return jsonResponse({}, 404);
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    resetProfileDecisionProbeCacheForTests();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    vi.unstubAllGlobals();
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  async function mount(path = '/profile/decisions'): Promise<ScionPageProfileDecisions> {
    window.history.replaceState(null, '', path);
    const el = document.createElement('scion-page-profile-decisions');
    el.pageData = {
      path,
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice' },
    } as PageData;
    document.body.appendChild(el);
    await settle(el, 25);
    return el;
  }

  it('renders 404 and sends zero /decisions requests when flags are off, or 404s when hub returns 404', async () => {
    let el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(requestedUrls).toEqual([]);
    el.remove();

    setFeatureFlag(DECISIONS_FLAG, true);
    handleDecisionsFetch = (): Promise<Response> =>
      jsonResponse({ error: { code: 'not_found' } }, 404);

    el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
  });

  it('renders Waiting on you grouped By project by default, syncs URL, switches to Most recent, and uses per-tab sort defaults (R-1, R-2, R6)', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const el = await mount('/profile/decisions');
    expect(window.location.search).toBe('?tab=pending&sort=project');
    expect(requestedUrls[0]).toBe('/api/v1/users/me/decisions?tab=pending&sort=project');

    // Two project section headers: scion (3 decisions across 2 prompts) and web-site (1 decision)
    const sortGroup = el.shadowRoot?.querySelector<HTMLElement>('sl-radio-group[data-sort-switch]');
    const innerRadiogroup =
      sortGroup?.shadowRoot?.querySelector<HTMLElement>('[role="radiogroup"]');
    const labelledById = innerRadiogroup?.getAttribute('aria-labelledby') ?? '';
    expect(labelledById).toBe('label');
    const innerSortLabel = sortGroup?.shadowRoot?.getElementById(labelledById);
    expect(innerSortLabel?.getAttribute('aria-hidden')).toBe('false');
    expect(innerSortLabel?.textContent?.trim()).toBe('Sort decisions');

    const groups = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLElement>('[data-pending-group]') ?? []
    );
    expect(groups.map((g) => g.getAttribute('data-pending-group'))).toEqual(['proj-1', 'proj-2']);
    expect(groups[0]?.querySelector('h3')?.textContent?.trim()).toBe('scion');
    expect(groups[0]?.querySelector('sl-badge')?.textContent?.trim()).toBe('3');
    expect(
      groups[0]
        ?.querySelector<HTMLAnchorElement>('[data-open-project-decisions]')
        ?.getAttribute('href')
    ).toBe('/projects/proj-1/decisions');
    expect(groups[1]?.querySelector('h3')?.textContent?.trim()).toBe('web-site');
    expect(groups[1]?.querySelector('sl-badge')?.textContent?.trim()).toBe('1');

    // In By project mode (R-1), project headers show the project name so card headers omit [data-card-project]
    expect(el.shadowRoot?.querySelectorAll('[data-card-project]')).toHaveLength(0);

    const convLinks = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLAnchorElement>('[data-conv-link]') ?? []
    ).map((a) => a.getAttribute('href'));
    expect(convLinks[0]).toContain('proj-1');
    expect(convLinks[2]).toContain('proj-2');

    // Switch sort to Most recent (R-2): project badge joins each card header
    el.shadowRoot?.querySelector<HTMLElement>('[data-sort-option="recent"]')?.click();
    await settle(el, 20);
    expect(window.location.search).toBe('?tab=pending&sort=recent');
    expect(requestedUrls).toContain('/api/v1/users/me/decisions?tab=pending&sort=recent');
    expect(el.shadowRoot?.querySelectorAll('[data-pending-group]')).toHaveLength(0);
    expect(el.shadowRoot?.querySelectorAll('[data-prompt-card]')).toHaveLength(3);
    const cardProjects = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLElement>('[data-card-project]') ?? []
    ).map((n) => n.textContent?.trim());
    expect(cardProjects).toEqual(['scion', 'scion', 'web-site']);

    // Switch to Made tab: defaults to sort=recent (R6)
    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="made"]')?.click();
    await settle(el, 20);
    expect(window.location.search).toBe('?tab=made&sort=recent');
    expect(requestedUrls).toContain('/api/v1/users/me/decisions?tab=made&sort=recent');
  });

  it('wires Project combobox, removable Project tag, filtered "n of m" badge, and D5 project labels/hints on Conversation and Agent options', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const el = await mount('/profile/decisions?tab=pending&sort=project');
    const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    expect(panel?.scope).toBe('profile');

    const projectCbx = el.shadowRoot?.querySelector<ScionFacetCombobox>(
      'scion-facet-combobox[data-filter="project"]'
    );
    expect(projectCbx).not.toBeNull();
    expect(projectCbx?.options).toEqual([
      { value: 'proj-1', label: 'scion' },
      { value: 'proj-2', label: 'web-site' },
    ]);

    // D5: Conversation options carry project hint; Agent options use agents[].projectId when present and fall back gracefully when absent (without inferring from loaded rows)
    expect(panel).not.toBeNull();
    panel?.setOpen(true);
    await settle(el, 15);
    const convCbx = panel?.shadowRoot?.querySelector<ScionFacetCombobox>(
      'scion-facet-combobox[data-filter="conversation"]'
    );
    expect(convCbx?.options).toEqual([
      { value: 'topic:landing', label: '#landing', hint: 'web-site' },
      { value: 'topic:release', label: '#release', hint: 'scion' },
    ]);

    const agentCbx = panel?.shadowRoot?.querySelector<ScionFacetCombobox>(
      'scion-facet-combobox[data-filter="agent"]'
    );
    expect(agentCbx?.options).toEqual([
      { value: AGENT_DES, label: 'design-bot', hint: 'web-site' },
      { value: AGENT_NCX, label: 'nc-x', hint: 'scion' },
      { value: AGENT_REL, label: 'release-bot' },
    ]);

    // Commit Project filter via combobox
    projectCbx?.dispatchEvent(
      new CustomEvent('facet-change', {
        detail: { value: 'proj-1', label: 'scion' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 20);

    expect(window.location.search).toBe('?tab=pending&sort=project&project=proj-1');
    expect(el.shadowRoot?.querySelector('[data-waiting-badge]')?.textContent?.trim()).toBe(
      '3 of 4'
    );

    // Collapsed filter panel shows removable "Project: scion" tag
    const tagTexts = Array.from(
      panel?.shadowRoot?.querySelectorAll<HTMLElement>('sl-tag[data-filter-tag]') ?? []
    ).map((t) => t.textContent?.trim());
    expect(tagTexts).toContain('Project: scion');

    // Removing the Project tag clears the project filter
    panel?.shadowRoot
      ?.querySelector<HTMLElement>('sl-tag[data-filter-tag="project"]')
      ?.dispatchEvent(new CustomEvent('sl-remove', { bubbles: true }));
    await settle(el, 20);
    expect(window.location.search).toBe('?tab=pending&sort=project');
  });

  it('renders Made By project overview groups and drills down via "See all in <project>" to sort=recent&project=<id> (R5, R-3)', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const el = await mount('/profile/decisions?tab=made&sort=project');
    expect(window.location.search).toBe('?tab=made&sort=project');
    expect(requestedUrls).toContain('/api/v1/users/me/decisions?tab=made&sort=project');

    const madeGroups = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLElement>('[data-made-group]') ?? []
    );
    expect(madeGroups.map((g) => g.getAttribute('data-made-group'))).toEqual(['proj-1', 'proj-2']);
    expect(el.shadowRoot?.querySelector('[data-load-more="made"]')).toBeNull();

    // Click "See all in scion" -> sets project=proj-1 and switches sort to recent
    const seeAll = el.shadowRoot?.querySelector<HTMLAnchorElement>(
      '[data-see-all-project="proj-1"]'
    );
    expect(seeAll?.textContent?.trim()).toBe('See all in scion');
    seeAll?.click();
    await settle(el, 25);

    expect(window.location.search).toBe('?tab=made&sort=recent&project=proj-1');
    expect(requestedUrls).toContain(
      '/api/v1/users/me/decisions?tab=made&sort=recent&project=proj-1'
    );

    // Recent table renders Project column and Load more button
    const headers = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLElement>('table.log th') ?? []
    ).map((th) => th.textContent?.trim());
    expect(headers).toContain('Project');
    expect(el.shadowRoot?.querySelector('[data-load-more="made"]')).not.toBeNull();
  });

  it('shows the R3 banner when truncatedProjects is true and hides it while a project filter is active', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);
    handleDecisionsFetch = (): Promise<Response> =>
      jsonResponse({
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [],
        answerers: [],
        conversations: [],
        projects: [PROJ_SCION],
        truncatedProjects: true,
      });

    const el = await mount('/profile/decisions');
    const banner = el.shadowRoot?.querySelector<HTMLElement>('[data-truncated-projects]');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain(
      'Showing your 200 most recently active projects; pick a project to see others'
    );

    // Selecting a project hides the truncated banner
    el.shadowRoot?.querySelector('scion-decisions-filter-panel')?.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { project: 'proj-1' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 25);
    expect(el.shadowRoot?.querySelector('[data-truncated-projects]')).toBeNull();

    // Clearing filters restores the truncated banner
    el.shadowRoot
      ?.querySelector('scion-decisions-filter-panel')
      ?.dispatchEvent(
        new CustomEvent('decisions-filters-clear', { bubbles: true, composed: true })
      );
    await settle(el, 25);
    expect(el.shadowRoot?.querySelector('[data-truncated-projects]')).not.toBeNull();
  });

  it('records surface=decisions_page on inline answer, updates nav badge event, and reloads By project from page 1 when paged (R7, Brief §Task)', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    let answeredR1 = false;
    handleDecisionsFetch = (url: URL): Promise<Response> => {
      const cursor = url.searchParams.get('cursor');
      if (cursor === 'page-2-cur') {
        return jsonResponse({
          counts: { waitingOnYou: 2, waitingOnOthers: 0 },
          prompts: [
            makePrompt({
              project: PROJ_WEB,
              promptMessageId: 'm-page2',
              askerAgentId: AGENT_DES,
              askerAgentName: 'design-bot',
              decisions: [makeDecision({ rowId: 'r-2', messageId: 'm-page2', key: 'hero-img' })],
            }),
          ],
          agents: [],
          answerers: [],
          conversations: [],
          projects: [PROJ_SCION, PROJ_WEB],
          truncatedProjects: false,
        });
      }
      if (!answeredR1) {
        return jsonResponse({
          counts: { waitingOnYou: 2, waitingOnOthers: 0 },
          prompts: [
            makePrompt({
              project: PROJ_SCION,
              promptMessageId: 'm-page1',
              decisions: [makeDecision({ rowId: 'r-1', messageId: 'm-page1', key: 'ship-now' })],
            }),
          ],
          agents: [],
          answerers: [],
          conversations: [],
          projects: [PROJ_SCION, PROJ_WEB],
          truncatedProjects: false,
          nextCursor: 'page-2-cur',
        });
      }
      // After r-1 is answered, only proj-2 remains on page 1 with a fresh empty cursor
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [
          makePrompt({
            project: PROJ_WEB,
            promptMessageId: 'm-page2',
            askerAgentId: AGENT_DES,
            askerAgentName: 'design-bot',
            decisions: [makeDecision({ rowId: 'r-2', messageId: 'm-page2', key: 'hero-img' })],
          }),
        ],
        agents: [],
        answerers: [],
        conversations: [],
        projects: [PROJ_SCION, PROJ_WEB],
        truncatedProjects: false,
      });
    };

    const navCounts: number[] = [];
    const onNavCount = (ev: Event): void => {
      navCounts.push((ev as CustomEvent<{ waitingOnYou: number }>).detail.waitingOnYou);
    };
    window.addEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, onNavCount);

    try {
      const el = await mount('/profile/decisions');
      expect(navCounts).toContain(2);

      // Load page 2 while in By project mode
      el.shadowRoot?.querySelector<HTMLElement>('[data-load-more="pending"]')?.click();
      await settle(el, 20);
      expect(requestedUrls).toContain(
        '/api/v1/users/me/decisions?tab=pending&sort=project&cursor=page-2-cur'
      );
      expect(el.shadowRoot?.querySelectorAll('[data-prompt-card]')).toHaveLength(2);

      // Answer r-1 inline via <scion-chat-decisions>
      answeredR1 = true;
      const firstCard = el.shadowRoot?.querySelector<ScionChatDecisions>(
        '[data-prompt-card="m-page1"] scion-chat-decisions'
      );
      expect(firstCard?.getAttribute('surface')).toBe('decisions_page');
      const yesBtn = firstCard?.shadowRoot?.querySelector<HTMLElement>('[data-choice="yes"]');
      expect(yesBtn).not.toBeNull();
      yesBtn?.click();

      await settle(el, 100);
      expect(answerRequests).toHaveLength(1);
      expect(answerRequests[0]?.body).toMatchObject({
        choice_id: 'yes',
        surface: 'decisions_page',
      });
      expect(navCounts.at(-1)).toBe(1);

      // Live reload after inline answer on paged By project resets from page 1 without stale cursor
      const postAnswerUrl = requestedUrls.at(-1) ?? '';
      expect(postAnswerUrl).toContain('/api/v1/users/me/decisions?tab=pending&sort=project');
      expect(postAnswerUrl).not.toContain('cursor=');
      expect(el.shadowRoot?.querySelectorAll('[data-prompt-card]')).toHaveLength(1);
      expect(el.shadowRoot?.querySelector('[data-pending-group="proj-2"]')).not.toBeNull();

      // Also test SSE decision-updated refresh
      stateManager.dispatchEvent(
        new CustomEvent('decision-updated', {
          detail: { data: { projectId: 'proj-2', messageId: 'm-page2' } },
        })
      );
      await settle(el, 100);
    } finally {
      window.removeEventListener(PROFILE_DECISIONS_COUNT_CHANGED_EVENT, onNavCount);
    }
  });

  it('handles SEARCH_TIMEOUT with warning alert + facet hydration, no-match Clear filters, and "/" shortcut', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    handleDecisionsFetch = (url: URL): Promise<Response> => {
      const q = url.searchParams.get('q') || '';
      if (q === 'slow') {
        return jsonResponse(
          { error: { code: 'SEARCH_TIMEOUT', message: 'search timed out' } },
          504
        );
      }
      if (q === 'nomatch') {
        return jsonResponse({
          counts: { waitingOnYou: 4, waitingOnOthers: 0 },
          filteredCounts: { waitingOnYou: 0, waitingOnOthers: 0 },
          prompts: [],
          agents: [{ id: AGENT_DES, name: 'design-bot', projectId: 'proj-2' }],
          answerers: [],
          conversations: [{ key: 'topic:landing', label: '#landing', projectId: 'proj-2' }],
          projects: [PROJ_SCION, PROJ_WEB],
          truncatedProjects: false,
        });
      }
      return jsonResponse({
        counts: { waitingOnYou: 4, waitingOnOthers: 0 },
        prompts: [makePrompt()],
        agents: [{ id: AGENT_DES, name: 'design-bot', projectId: 'proj-2' }],
        answerers: [],
        conversations: [{ key: 'topic:landing', label: '#landing', projectId: 'proj-2' }],
        projects: [PROJ_SCION, PROJ_WEB],
        truncatedProjects: false,
      });
    };

    // Mount directly on ?q=slow -> triggers SEARCH_TIMEOUT + non-q facet hydration
    const el = await mount('/profile/decisions?tab=pending&sort=project&q=slow');
    const timeoutAlert = el.shadowRoot?.querySelector('[data-search-timeout]');
    expect(timeoutAlert).not.toBeNull();
    expect(timeoutAlert?.textContent).toContain('Search took too long');
    expect(
      requestedUrls.some(
        (u) =>
          u.includes('/api/v1/users/me/decisions?tab=pending&sort=project&limit=1') &&
          !u.includes('q=')
      )
    ).toBe(true);

    // Clear filters from the filter panel -> loads prompts cleanly
    const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    expect(panel).not.toBeNull();
    if (!panel) return;
    panel.dispatchEvent(
      new CustomEvent('decisions-filters-clear', { bubbles: true, composed: true })
    );
    await settle(el, 25);
    expect(el.shadowRoot?.querySelector('[data-search-timeout]')).toBeNull();
    expect(el.shadowRoot?.querySelectorAll('[data-prompt-card]')).toHaveLength(1);

    // "/" shortcut opens the filter panel
    panel.setOpen(false);
    await settle(el, 10);
    expect(panel.shadowRoot?.querySelector('#decisions-filter-body')?.hasAttribute('hidden')).toBe(
      true
    );
    window.dispatchEvent(new KeyboardEvent('keydown', { key: '/', cancelable: true }));
    await settle(el, 15);
    expect(panel.shadowRoot?.querySelector('#decisions-filter-body')?.hasAttribute('hidden')).toBe(
      false
    );

    // Search with no matches -> shows no-match empty state and "Clear filters" button
    panel.dispatchEvent(
      new CustomEvent('decisions-search-input', {
        detail: { value: 'nomatch', immediate: true },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 25);
    expect(el.shadowRoot?.querySelector('[data-no-match="pending"]')?.textContent).toContain(
      'No decisions match these filters'
    );
    el.shadowRoot?.querySelector<HTMLElement>('[data-clear-filters]')?.click();
    await settle(el, 25);
    expect(window.location.search).toBe('?tab=pending&sort=project');
    expect(el.shadowRoot?.querySelectorAll('[data-prompt-card]')).toHaveLength(1);
  });

  it('piggybacks <scion-profile-nav> badge onto <scion-page-profile-decisions> fetch without issuing a ?limit=0 probe', async () => {
    await import('../profile/profile-nav.js');
    setFeatureFlag(DECISIONS_FLAG, true);

    window.history.replaceState(null, '', '/profile/decisions');
    const nav = document.createElement('scion-profile-nav') as HTMLElement & {
      updateComplete: Promise<boolean>;
      currentPath: string;
    };
    nav.currentPath = '/profile/decisions';
    const page = document.createElement('scion-page-profile-decisions');
    page.pageData = {
      path: '/profile/decisions',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice' },
    } as PageData;

    document.body.appendChild(nav);
    document.body.appendChild(page);
    await settle(page, 25);
    await nav.updateComplete;

    expect(requestedUrls.some((u) => u.includes('limit=0'))).toBe(false);
    expect(requestedUrls).toEqual(['/api/v1/users/me/decisions?tab=pending&sort=project']);
    const badge = nav.shadowRoot?.querySelector('[data-decisions-nav-badge]');
    expect(badge?.textContent?.trim()).toBe('4');
  });

  it('keeps pendingNeedsReset across an off-tab SSE refresh so Waiting on you reloads filtered prompts on tab switch', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const orig = handleDecisionsFetch;
    const el = await mount('/profile/decisions?tab=made&sort=recent');

    // Simulate a transient/timeout failure on any pending request carrying `project`
    handleDecisionsFetch = (url: URL): Promise<Response> =>
      (url.searchParams.get('tab') || 'pending') === 'pending' && url.searchParams.get('project')
        ? jsonResponse({ error: { code: 'internal', message: 'boom' } }, 500)
        : orig(url);

    // Apply a shared Project filter while on Made -> sets pendingNeedsReset
    const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    expect(panel).not.toBeNull();
    panel?.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { project: 'proj-2' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 25);

    // Fire an SSE decision-updated while still on Made -> must skip off-tab pending live load
    const urlsBeforeSse = requestedUrls.length;
    stateManager.dispatchEvent(new CustomEvent('decision-updated', { detail: {} }));
    await settle(el, 120);
    const sseUrls = requestedUrls.slice(urlsBeforeSse);
    expect(sseUrls.some((u) => u.includes('tab=pending'))).toBe(false);

    // Restore backend and switch back to Waiting on you -> must issue a reset load for project=proj-2
    handleDecisionsFetch = orig;
    const urlsBeforeSwitch = requestedUrls.length;
    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="pending"]')?.click();
    await settle(el, 40);

    const switchUrls = requestedUrls.slice(urlsBeforeSwitch);
    expect(switchUrls).toEqual([
      '/api/v1/users/me/decisions?tab=pending&sort=project&project=proj-2',
    ]);
    expect(window.location.search).toBe('?tab=pending&sort=project&project=proj-2');
    const cardIds = Array.from(
      el.shadowRoot?.querySelectorAll<HTMLElement>('[data-prompt-card]') ?? []
    ).map((c) => c.dataset.promptCard);
    expect(cardIds).toEqual(['m-3']);
  });

  it('never sends a stale cursor across sort, tab, or filter changes', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const orig = handleDecisionsFetch;
    handleDecisionsFetch = (url: URL): Promise<Response> => {
      const tab = url.searchParams.get('tab') || 'pending';
      if (tab === 'pending') {
        const cur = url.searchParams.get('cursor');
        const sort = url.searchParams.get('sort') || 'project';
        const nextCursor =
          cur === 'pending-cur-2'
            ? 'pending-cur-3'
            : cur
              ? undefined
              : sort === 'recent'
                ? 'pending-recent-cur-2'
                : 'pending-cur-2';
        return jsonResponse({
          counts: { waitingOnYou: 3, waitingOnOthers: 0 },
          prompts: [
            makePrompt({
              project: PROJ_SCION,
              promptMessageId: cur ? `m-${cur}` : `m-${sort}-1`,
              decisions: [
                makeDecision({
                  rowId: cur ? `r-${cur}` : `r-${sort}-1`,
                  messageId: cur ? `m-${cur}` : `m-${sort}-1`,
                }),
              ],
            }),
          ],
          agents: [{ id: AGENT_NCX, name: 'nc-x', projectId: 'proj-1' }],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          conversations: [{ key: 'topic:release', label: '#release', projectId: 'proj-1' }],
          projects: [PROJ_SCION, PROJ_WEB],
          truncatedProjects: false,
          ...(nextCursor ? { nextCursor } : {}),
        });
      }
      return orig(url);
    };

    const el = await mount('/profile/decisions?tab=pending&sort=project');
    expect(el.shadowRoot?.querySelector('[data-load-more="pending"]')).not.toBeNull();

    // Load page 2 on pending (uses cursor=pending-cur-2 once AND returns live cursor=pending-cur-3)
    el.shadowRoot?.querySelector<HTMLElement>('[data-load-more="pending"]')?.click();
    await settle(el, 25);
    expect(requestedUrls.filter((u) => u.includes('cursor=pending-cur-2'))).toHaveLength(1);
    expect(el.shadowRoot?.querySelector('[data-load-more="pending"]')).not.toBeNull();

    const recordStart = requestedUrls.length;

    // 1. Sort change on pending while holding live pending-cur-3 -> clears cursor immediately and sends no cursor
    el.shadowRoot?.querySelector<HTMLElement>('[data-sort-option="recent"]')?.click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-load-more="pending"]')).toBeNull();
    await settle(el, 25);
    // sort=recent returned live pending-recent-cur-2
    expect(el.shadowRoot?.querySelector('[data-load-more="pending"]')).not.toBeNull();

    // 2. Filter change on pending while holding live pending-recent-cur-2 -> sends no cursor
    el.shadowRoot?.querySelector('scion-decisions-filter-panel')?.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { project: 'proj-1' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 25);

    // 3. Switch to Made (default sort=recent returns live nextCursor=made-cur-2) -> no cursor on initial reset
    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="made"]')?.click();
    await settle(el, 25);
    expect(el.shadowRoot?.querySelector('[data-load-more="made"]')).not.toBeNull();

    // 4a. Sort change on Made (recent -> project) while holding live made-cur-2, immediately followed by
    // switching back to recent before sort=project settles -> made-cur-2 must already be cleared by setSort('project')
    // so sort=recent cannot leak cursor=made-cur-2!
    el.shadowRoot?.querySelector<HTMLElement>('[data-sort-option="project"]')?.click();
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('[data-load-more="made"]')).toBeNull();
    el.shadowRoot?.querySelector<HTMLElement>('[data-sort-option="recent"]')?.click();
    await settle(el, 25);
    expect(el.shadowRoot?.querySelector('[data-load-more="made"]')).not.toBeNull();

    // 4b. Sort change on Made (recent -> project) allowed to settle completely -> no cursor
    el.shadowRoot?.querySelector<HTMLElement>('[data-sort-option="project"]')?.click();
    await settle(el, 25);
    expect(el.shadowRoot?.querySelector('[data-load-more="made"]')).toBeNull();

    // 5. Switch back to Pending -> no cursor
    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="pending"]')?.click();
    await settle(el, 25);

    const subsequentUrls = requestedUrls.slice(recordStart);
    expect(subsequentUrls.length).toBeGreaterThanOrEqual(5);
    expect(subsequentUrls.some((u) => u.includes('cursor='))).toBe(false);
  });

  it('invalidates the opposite tab when setSort or drillDownMadeProject flushes a debounced search within 300ms', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const orig = handleDecisionsFetch;
    handleDecisionsFetch = (url: URL): Promise<Response> => {
      const tab = url.searchParams.get('tab') || 'pending';
      const sort = url.searchParams.get('sort') || (tab === 'made' ? 'recent' : 'project');
      const q = url.searchParams.get('q') || '';
      const cur = url.searchParams.get('cursor') || '';

      if (tab === 'made' && sort === 'recent' && q === 'foo') {
        if (cur === 'made-cur-2') {
          return jsonResponse(
            { error: { code: 'invalid_argument', message: 'cursor mismatch' } },
            400
          );
        }
        return jsonResponse({
          counts: { waitingOnYou: 4, waitingOnOthers: 1 },
          entries: [
            makeLogEntry({
              answerId: cur ? 'ans-foo-2' : 'ans-foo-1',
              project: PROJ_SCION,
            }),
          ],
          agents: [{ id: AGENT_NCX, name: 'nc-x', projectId: 'proj-1' }],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          conversations: [{ key: 'topic:release', label: '#release', projectId: 'proj-1' }],
          projects: [PROJ_SCION, PROJ_WEB],
          truncatedProjects: false,
          ...(cur ? {} : { nextCursor: 'made-cur-foo-2' }),
        });
      }
      return orig(url);
    };

    // 1. Reviewer reproducer: visit Made (caches entries + made-cur-2), return to Pending,
    // type 'foo' (debounced), click Most recent BEFORE 300ms, then switch back to Made and click Load more.
    const el = await mount('/profile/decisions?tab=pending&sort=project');
    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="made"]')?.click();
    await settle(el, 25);
    expect(requestedUrls).toContain('/api/v1/users/me/decisions?tab=made&sort=recent');

    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="pending"]')?.click();
    await settle(el, 10);

    const beforeFlushIdx = requestedUrls.length;
    el.shadowRoot?.querySelector('scion-decisions-filter-panel')?.dispatchEvent(
      new CustomEvent('decisions-search-input', {
        detail: { value: 'foo', immediate: false },
        bubbles: true,
        composed: true,
      })
    );
    // Immediately (< 300ms) click Most recent on Pending
    el.shadowRoot?.querySelector<HTMLElement>('[data-sort-option="recent"]')?.click();
    await settle(el, 25);
    expect(requestedUrls.slice(beforeFlushIdx)).toContain(
      '/api/v1/users/me/decisions?tab=pending&sort=recent&q=foo'
    );

    // Switch back to Made -> MUST issue a reset fetch with q=foo (no cursor!) and NOT keep stale made-cur-2
    const beforeMadeSwitchIdx = requestedUrls.length;
    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="made"]')?.click();
    await settle(el, 25);
    expect(requestedUrls.slice(beforeMadeSwitchIdx)).toEqual([
      '/api/v1/users/me/decisions?tab=made&sort=recent&q=foo',
    ]);

    // Load more on Made uses ONLY the fresh cursor (made-cur-foo-2), never made-cur-2
    el.shadowRoot?.querySelector<HTMLElement>('[data-load-more="made"]')?.click();
    await settle(el, 25);
    expect(requestedUrls[requestedUrls.length - 1]).toBe(
      '/api/v1/users/me/decisions?tab=made&sort=recent&cursor=made-cur-foo-2&q=foo'
    );

    // 2. drillDownMadeProject when project is ALREADY proj-1 and a debounced search is pending (< 300ms)
    // must still invalidate Pending so switching back refetches with the flushed search!
    el.shadowRoot?.querySelector('scion-decisions-filter-panel')?.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { project: 'proj-1' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 25);
    el.shadowRoot?.querySelector<HTMLElement>('[data-sort-option="project"]')?.click();
    await settle(el, 25);

    el.shadowRoot?.querySelector('scion-decisions-filter-panel')?.dispatchEvent(
      new CustomEvent('decisions-search-input', {
        detail: { value: 'baz', immediate: false },
        bubbles: true,
        composed: true,
      })
    );
    // Click "See all in scion" before 300ms while filterProject is ALREADY 'proj-1'
    el.shadowRoot?.querySelector<HTMLElement>('[data-see-all-project="proj-1"]')?.click();
    await settle(el, 25);
    expect(requestedUrls[requestedUrls.length - 1]).toBe(
      '/api/v1/users/me/decisions?tab=made&sort=recent&q=baz&project=proj-1'
    );

    const beforePendingReturnIdx = requestedUrls.length;
    el.shadowRoot?.querySelector<HTMLElement>('sl-tab[data-tab="pending"]')?.click();
    await settle(el, 25);
    expect(requestedUrls.slice(beforePendingReturnIdx)).toEqual([
      '/api/v1/users/me/decisions?tab=pending&sort=recent&q=baz&project=proj-1',
    ]);
  });

  it('subscribes to the profile SSE scope (including user.<id>.chat.>) so DM decision.updated events refresh Waiting on you', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const sseSources: FakeEventSource[] = [];
    class RecordingEventSource extends FakeEventSource {
      constructor(url: string) {
        super(url);
        sseSources.push(this);
      }
    }
    stateManager.setScope({ type: 'dashboard' });
    vi.stubGlobal('EventSource', RecordingEventSource);

    const el = await mount('/profile/decisions');
    const latestUrl = sseSources[sseSources.length - 1]?.url ?? '';
    expect(decodeURIComponent(latestUrl)).toContain('sub=project.>');
    expect(decodeURIComponent(latestUrl)).toContain('sub=user.u-me.chat.>');

    const beforeDmUpdateIdx = requestedUrls.length;
    (
      stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
    ).handleUpdate({
      subject: 'user.u-me.chat.decision.updated',
      data: {
        messageId: 'm-1',
        threadId: 'dm:agent:release-bot:user:u-me',
        decision: makeDecision({
          rowId: 'r-1',
          messageId: 'm-1',
          status: 'answered',
        }),
      },
    });

    await new Promise((r) => setTimeout(r, 90));
    await settle(el, 20);
    expect(requestedUrls.slice(beforeDmUpdateIdx)).toContain(
      '/api/v1/users/me/decisions?tab=pending&sort=project&limit=50'
    );
  });

  it('guards avatarColor and avatarInitial when askerAgentId and askerAgentName are omitted', async () => {
    expect(avatarColor(undefined)).toBe(avatarColor(''));
    expect(avatarColor(null)).toBe(avatarColor(''));
    expect(avatarInitial(undefined)).toBe('?');
    expect(avatarInitial(null)).toBe('?');
    expect(avatarInitial('   ')).toBe('?');

    setFeatureFlag(DECISIONS_FLAG, true);

    const promptWithoutAsker = makePrompt({
      project: PROJ_SCION,
      promptMessageId: 'm-missing-asker',
      decisions: [makeDecision({ rowId: 'r-missing-asker', messageId: 'm-missing-asker' })],
    }) as unknown as Record<string, unknown>;
    delete promptWithoutAsker.askerAgentId;
    delete promptWithoutAsker.askerAgentName;

    handleDecisionsFetch = (): Promise<Response> =>
      jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [promptWithoutAsker],
        agents: [],
        answerers: [],
        conversations: [],
        projects: [PROJ_SCION],
        truncatedProjects: false,
      });

    const el = await mount('/profile/decisions');
    const card = el.shadowRoot?.querySelector<HTMLElement>('[data-prompt-card="m-missing-asker"]');
    expect(card).not.toBeNull();
    expect(card?.querySelector('.avatar')?.textContent?.trim()).toBe('?');
  });

  it('renders withdrawn sibling rows on partially open profile prompts and drops fully withdrawn prompts live', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);

    const openRow = makeDecision({ rowId: 'r-open', messageId: 'm-prof-set', key: 'D234' });
    const wdRow = makeDecision({
      rowId: 'r-wd',
      messageId: 'm-prof-set',
      key: '17',
      status: 'withdrawn',
      withdrawn: {
        at: '2026-11-15T07:10:00Z',
        reason: 'Resolved offline.',
        byName: 'release-bot',
      },
    });
    const setPrompt = makePrompt({
      project: PROJ_SCION,
      promptMessageId: 'm-prof-set',
      answerMode: 'set',
      waitingOnYou: true,
      decisions: [openRow, wdRow],
    });

    let liveWithdrawn = false;
    handleDecisionsFetch = (): Promise<Response> =>
      jsonResponse({
        counts: { waitingOnYou: liveWithdrawn ? 0 : 1, waitingOnOthers: 0 },
        prompts: liveWithdrawn ? [] : [setPrompt],
        agents: [],
        answerers: [],
        conversations: [],
        projects: [PROJ_SCION],
        truncatedProjects: false,
      });

    const el = await mount('/profile/decisions');
    const root = el.shadowRoot!;
    const card = root.querySelector<ScionChatDecisions>(
      '[data-prompt-card="m-prof-set"] scion-chat-decisions'
    )!;
    expect(card).not.toBeNull();
    await card.updateComplete;

    const wdRowEl = card.shadowRoot!.querySelector('.row[data-row="r-wd"]')!;
    expect(wdRowEl.classList.contains('withdrawn')).toBe(true);
    expect(wdRowEl.textContent).toContain('No longer needed');
    expect(wdRowEl.textContent).toContain('Resolved offline.');

    liveWithdrawn = true;
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: {
          data: {
            projectId: PROJ_SCION.id,
            messageId: 'm-prof-set',
            decision: {
              ...openRow,
              status: 'withdrawn',
              withdrawn: { at: '2026-11-15T07:12:00Z', byName: 'release-bot' },
            },
          },
        },
      })
    );
    await el.updateComplete;

    expect(root.querySelector('[data-prompt-card="m-prof-set"]')).toBeNull();
    expect(root.querySelector('[data-waiting-others]')).toBeNull();
    expect(root.querySelector('[data-waiting-badge]')).toBeNull();
  });

  it('styles neutral group-count badges with Scion neutral badge tokens in both themes', () => {
    const ctor = customElements.get('scion-page-profile-decisions') as unknown as {
      styles: unknown;
    };
    const cssText = String(ctor.styles);
    expect(cssText).toMatch(
      /sl-badge\[variant=['"]neutral['"]\]::part\(base\)\s*\{[^}]*background:\s*var\(--scion-badge-neutral-bg/
    );
    expect(cssText).toMatch(
      /sl-badge\[variant=['"]neutral['"]\]::part\(base\)\s*\{[^}]*color:\s*var\(--scion-badge-neutral-text/
    );
  });
});
