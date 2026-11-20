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
 * Unit tests for Decisions dismiss P5 (ptone/scion#8217):
 * - Made tab dismissal entries, Provenance = Dismissed filter, and plain-text reason sink
 * - Multi-select on Waiting on you (project and profile pages)
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../shared/types.js';
import type {
  ProjectDecisionLogEntryView,
  ProjectDecisionProjectRef,
  ProjectDecisionPromptView,
  ProjectMadeDecisionsResponse,
  ProjectPendingDecisionsResponse,
} from '../../client/project-decisions.js';
import {
  resetProfileDecisionProbeCacheForTests,
  type ProfileMadeByProjectDecisionsResponse,
  type ProfileMadeRecentDecisionsResponse,
  type ProfilePendingDecisionsResponse,
} from '../../client/profile-decisions.js';
import {
  DECISIONS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { setPreferredTimeZone } from '../../utils/time.js';

const showToast = vi.hoisted(() => vi.fn());
vi.mock('../../utils/toast.js', () => ({ showToast }));

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
await import('./project-decisions.js');
await import('./profile-decisions.js');
type ScionPageProjectDecisions = import('./project-decisions.js').ScionPageProjectDecisions;
type ScionPageProfileDecisions = import('./profile-decisions.js').ScionPageProfileDecisions;
type ScionDecisionsFilterPanel =
  import('../shared/decisions-filter-panel.js').ScionDecisionsFilterPanel;

const PROJ_SCION: ProjectDecisionProjectRef = { id: 'p-1', name: 'scion', slug: 'scion' };
const AGENT_REL = '11111111-2222-3333-4444-555555555555';

function makeDismissLogEntry(
  over: Partial<ProjectDecisionLogEntryView> = {}
): ProjectDecisionLogEntryView {
  return {
    project: PROJ_SCION,
    answerId: 'ans-dsm-1',
    decision: {
      rowId: 'r-dsm-1',
      promptMessageId: 'm-dsm-1',
      promptAvailable: true,
      key: 'D234',
      ref: 'D234',
      version: 1,
      question: 'Ship v0.9 today?',
      askerAgentId: AGENT_REL,
      askerAgentName: 'release-bot',
    },
    choiceId: '$dismissed',
    label: 'Dismissed',
    matchedDefault: false,
    answeredByUserId: 'u-me',
    answeredByName: 'Alice',
    provenance: 'direct',
    outcome: 'dismiss',
    dismissReason: 'Stale: the release already shipped.',
    surface: 'decisions_page',
    at: '2026-11-16T09:12:00Z',
    chatKey: 'topic-release',
    conversationLabel: '#release',
    conversationAvailable: true,
    sourceMessageAvailable: false,
    ...over,
  } as ProjectDecisionLogEntryView;
}

async function settle(
  el: HTMLElement & { updateComplete: Promise<boolean> },
  ms = 15
): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
  await el.updateComplete;
  const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
    'scion-decisions-filter-panel'
  );
  await panel?.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('Made tab dismissals and Provenance = Dismissed (ptone/scion#8217)', () => {
  let requests: { method: string; url: string; body?: unknown }[];
  let madeResponse: ProjectMadeDecisionsResponse;
  let profileMadeByProject: Omit<ProfileMadeByProjectDecisionsResponse, 'sort'>;
  let profileMadeRecent: Omit<ProfileMadeRecentDecisionsResponse, 'sort'>;

  beforeAll(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    window.localStorage.removeItem('scion:decisions:filters-open');
    resetServerFlagStateForTests();
    resetProfileDecisionProbeCacheForTests();
    setPreferredTimeZone('UTC');
    setFeatureFlag(DECISIONS_FLAG, true);
    showToast.mockReset();
    requests = [];

    const emptyPending: ProjectPendingDecisionsResponse = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      prompts: [],
      agents: [{ id: AGENT_REL, name: 'release-bot' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
    };
    const emptyProfilePending: ProfilePendingDecisionsResponse = {
      ...emptyPending,
      projects: [PROJ_SCION],
      truncatedProjects: false,
    };

    madeResponse = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      entries: [makeDismissLogEntry()],
      agents: [{ id: AGENT_REL, name: 'release-bot' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
    };
    profileMadeByProject = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      groups: [{ project: PROJ_SCION, entries: [makeDismissLogEntry()], more: false }],
      agents: [{ id: AGENT_REL, name: 'release-bot', projectId: 'p-1' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      projects: [PROJ_SCION],
      truncatedProjects: false,
    };
    profileMadeRecent = {
      counts: { waitingOnYou: 0, waitingOnOthers: 0 },
      entries: [makeDismissLogEntry()],
      agents: [{ id: AGENT_REL, name: 'release-bot', projectId: 'p-1' }],
      answerers: [{ id: 'u-me', name: 'Alice' }],
      conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      projects: [PROJ_SCION],
      truncatedProjects: false,
    };

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        const method = (init?.method ?? 'GET').toUpperCase();
        requests.push({ method, url: url.pathname + url.search });

        if (url.pathname === '/api/v1/projects/p-1') {
          return jsonResponse(PROJ_SCION);
        }
        if (url.pathname === '/auth/me' || url.pathname === '/api/v1/auth/me') {
          return jsonResponse({ id: 'u-me', name: 'Alice' });
        }
        if (url.pathname === '/api/v1/projects/p-1/decisions') {
          const tab = url.searchParams.get('tab') ?? 'pending';
          return tab === 'made' ? jsonResponse(madeResponse) : jsonResponse(emptyPending);
        }
        if (url.pathname === '/api/v1/users/me/decisions') {
          const tab = url.searchParams.get('tab') ?? 'pending';
          const sort = url.searchParams.get('sort') ?? 'project';
          if (tab !== 'made') return jsonResponse(emptyProfilePending);
          return sort === 'project'
            ? jsonResponse({ sort: 'project', ...profileMadeByProject })
            : jsonResponse({ sort: 'recent', ...profileMadeRecent });
        }
        return jsonResponse({}, 404);
      })
    );
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('renders a dismiss entry on project Made (desktop table and mobile list) with Dismissed, x-circle badge, plain-text reason, and no $dismissed pill (X6)', async () => {
    madeResponse.entries = [
      makeDismissLogEntry({
        answerId: 'ans-dsm-1',
        label: '', // even if label is omitted, never falls back to $dismissed
        dismissReason: 'Stale: the release already shipped.',
      }),
      makeDismissLogEntry({
        answerId: 'ans-dsm-2',
        decision: {
          rowId: 'r-dsm-2',
          promptMessageId: 'm-dsm-2',
          promptAvailable: true,
          key: 'migrate',
          ref: 'migrate',
          version: 1,
          question: 'Run the backfill on staging first?',
          askerAgentId: 'a-ncx',
          askerAgentName: 'nc-x',
        },
        dismissReason: undefined,
      }),
    ];
    window.history.replaceState({}, '', '/projects/p-1/decisions?tab=made&provenance=dismissed');
    const el = document.createElement('scion-page-project-decisions') as ScionPageProjectDecisions;
    el.pageData = {
      path: '/projects/p-1/decisions?tab=made&provenance=dismissed',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(el);
    await settle(el, 25);

    expect(
      requests.some((r) => r.url === '/api/v1/projects/p-1/decisions?tab=made&provenance=dismissed')
    ).toBe(true);

    const root = el.shadowRoot!;
    const row1 = root.querySelector<HTMLElement>('tr[data-answer-row="ans-dsm-1"]')!;
    expect(row1).not.toBeNull();
    expect(row1.textContent).toContain('D234');
    expect(row1.textContent).toContain('Ship v0.9 today?');
    expect(row1.textContent).toContain('Dismissed');
    expect(row1.textContent).toContain('Stale: the release already shipped.');
    expect(row1.textContent).toContain('release-bot');
    expect(row1.textContent).toContain('#release');
    expect(row1.textContent).not.toContain('$dismissed');

    const badge = row1.querySelector<HTMLElement>('[data-provenance] .badge.dec')!;
    expect(badge).not.toBeNull();
    expect(badge.textContent?.trim()).toBe('dismissed');
    expect(badge.querySelector('sl-icon')?.getAttribute('name')).toBe('x-circle');

    // Second row has no dismissReason -> no secondary reason element rendered
    const row2 = root.querySelector<HTMLElement>('tr[data-answer-row="ans-dsm-2"]')!;
    expect(row2.querySelector('[data-dismiss-reason]')).toBeNull();

    // Mobile stacked list item also renders Dismissed, reason, and x-circle badge without $dismissed
    const mobile1 = root.querySelector<HTMLElement>('[data-mobile-answer="ans-dsm-1"]')!;
    expect(mobile1.textContent).toContain('D234');
    expect(mobile1.textContent).toContain('Dismissed');
    expect(mobile1.textContent).toContain('Ship v0.9 today?');
    expect(mobile1.textContent).toContain('Stale: the release already shipped.');
    expect(mobile1.textContent).not.toContain('$dismissed');
    expect(mobile1.querySelector('.badge.dec sl-icon')?.getAttribute('name')).toBe('x-circle');
  });

  it('escapes HTML/Markdown payloads in dismissReason as inert plain text on project and profile Made (sink test)', async () => {
    const payload = '<img src=x onerror="window.__xss=1"> **not bold** <script>alert(1)</script>';
    madeResponse.entries = [makeDismissLogEntry({ dismissReason: payload })];
    profileMadeByProject.groups = [
      {
        project: PROJ_SCION,
        entries: [makeDismissLogEntry({ dismissReason: payload })],
        more: false,
      },
    ];

    window.history.replaceState({}, '', '/projects/p-1/decisions?tab=made');
    const projEl = document.createElement(
      'scion-page-project-decisions'
    ) as ScionPageProjectDecisions;
    projEl.pageData = {
      path: '/projects/p-1/decisions?tab=made',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(projEl);
    await settle(projEl, 25);

    const tableReason = projEl.shadowRoot!.querySelector<HTMLElement>(
      'tr[data-answer-row="ans-dsm-1"] [data-dismiss-reason]'
    )!;
    expect(tableReason).not.toBeNull();
    expect(tableReason.textContent).toBe(payload);
    expect(tableReason.querySelector('img, script, strong, b')).toBeNull();

    // Profile Made (sort=project grouped view)
    projEl.remove();
    window.history.replaceState(
      {},
      '',
      '/profile/decisions?tab=made&sort=project&provenance=dismissed'
    );
    const profEl = document.createElement(
      'scion-page-profile-decisions'
    ) as ScionPageProfileDecisions;
    profEl.pageData = {
      path: '/profile/decisions?tab=made&sort=project&provenance=dismissed',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(profEl);
    await settle(profEl, 25);

    expect(
      requests.some(
        (r) => r.url === '/api/v1/users/me/decisions?tab=made&sort=project&provenance=dismissed'
      )
    ).toBe(true);
    const profReason = profEl.shadowRoot!.querySelector<HTMLElement>(
      '[data-mobile-answer="ans-dsm-1"] [data-dismiss-reason]'
    )!;
    expect(profReason).not.toBeNull();
    expect(profReason.textContent).toBe(payload);
    expect(profReason.querySelector('img, script, strong, b')).toBeNull();
  });
});

function makeDecision(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r-1',
    messageId: 'm-1',
    key: 'D234',
    ref: 'D234',
    version: 1,
    status: 'pending',
    askerAgentId: AGENT_REL,
    askerAgentName: 'release-bot',
    question: 'Ship v0.9 today?',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
    ],
    ...over,
  };
}

function makePrompt(over: Partial<ProjectDecisionPromptView> = {}): ProjectDecisionPromptView {
  return {
    project: PROJ_SCION,
    promptMessageId: 'm-1',
    promptAvailable: true,
    chatKey: 'topic-release',
    conversationLabel: '#release',
    conversationAvailable: true,
    askerAgentId: AGENT_REL,
    askerAgentName: 'release-bot',
    createdAt: '2026-11-16T08:00:00Z',
    answerMode: 'single',
    waitingOnYou: true,
    canAnswer: true,
    decisions: [makeDecision()],
    ...over,
  };
}

function toggleCheckbox(box: HTMLElement & { checked?: boolean }, checked: boolean): void {
  box.checked = checked;
  box.dispatchEvent(new Event('sl-change', { bubbles: true }));
}

describe('Waiting on you multi-select and bulk dismiss (ptone/scion#8217)', () => {
  beforeAll(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    window.localStorage.removeItem('scion:decisions:filters-open');
    resetServerFlagStateForTests();
    resetProfileDecisionProbeCacheForTests();
    setPreferredTimeZone('UTC');
    setFeatureFlag(DECISIONS_FLAG, true);
    showToast.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
  });

  it('disables too-recent card checkboxes with countdown tooltip, skips them on Select all, keeps selection page-local, and clears selection (X5, X9)', async () => {
    const nowIso = new Date(Date.now() - 2 * 60_000).toISOString(); // 2 min old -> 3 min remaining
    const old1 = makePrompt({
      promptMessageId: 'm-old-1',
      askerAgentName: 'nc-x',
      chatKey: 'dm:ncx',
      conversationLabel: 'DM with nc-x',
      createdAt: '2026-11-10T08:00:00Z',
      decisions: [
        makeDecision({
          rowId: 'r-old-1',
          messageId: 'm-old-1',
          key: 'migrate',
          question: 'Run backfill?',
        }),
      ],
    });
    const old2 = makePrompt({
      promptMessageId: 'm-old-2',
      askerAgentName: 'release-bot',
      chatKey: 'topic-release',
      conversationLabel: '#release',
      createdAt: '2026-11-12T08:00:00Z',
      decisions: [
        makeDecision({
          rowId: 'r-old-2',
          messageId: 'm-old-2',
          key: 'D234',
          question: 'Ship v0.9?',
        }),
      ],
    });
    const recent = makePrompt({
      promptMessageId: 'm-recent',
      askerAgentName: 'docs-bot',
      chatKey: 'topic-docs',
      conversationLabel: '#docs',
      createdAt: nowIso,
      decisions: [
        makeDecision({
          rowId: 'r-rec',
          messageId: 'm-recent',
          key: 'toc',
          question: 'Split CLI ref?',
        }),
      ],
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        if (url.pathname === '/api/v1/projects/p-1') return jsonResponse(PROJ_SCION);
        if (url.pathname === '/auth/me') return jsonResponse({ id: 'u-me', name: 'Alice' });
        if (url.pathname === '/api/v1/projects/p-1/decisions') {
          return jsonResponse({
            counts: { waitingOnYou: 3, waitingOnOthers: 0 },
            prompts: [old1, old2, recent],
            agents: [{ id: AGENT_REL, name: 'release-bot' }],
            answerers: [{ id: 'u-me', name: 'Alice' }],
            conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
          });
        }
        return jsonResponse({}, 404);
      })
    );

    window.history.replaceState({}, '', '/projects/p-1/decisions');
    const el = document.createElement('scion-page-project-decisions') as ScionPageProjectDecisions;
    el.pageData = {
      path: '/projects/p-1/decisions',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(el);
    await settle(el, 25);

    const root = el.shadowRoot!;
    // Selection bar is hidden until at least 1 card is selected, while Select all is available
    expect(root.querySelector('[data-selection-bar]')).toBeNull();
    const selectAll = root.querySelector<
      HTMLElement & { checked?: boolean; indeterminate?: boolean }
    >('[data-select-all-prompts]')!;
    expect(selectAll).not.toBeNull();

    // Too-recent card checkbox is disabled with countdown tooltip
    const recentBox = root.querySelector<HTMLElement & { checked?: boolean }>(
      '[data-select-prompt="m-recent"]'
    )!;
    expect(recentBox.hasAttribute('disabled')).toBe(true);
    expect(
      root
        .querySelector<HTMLElement>('[data-select-prompt-tooltip="m-recent"]')
        ?.getAttribute('content')
    ).toMatch(/^Dismiss available in [34] min$/);

    // Attempting to check a too-recent card is ignored
    toggleCheckbox(recentBox, true);
    await settle(el, 10);
    expect(root.querySelector('[data-selection-bar]')).toBeNull();

    // Select one eligible card -> selection bar appears in indeterminate state
    const box1 = root.querySelector<HTMLElement & { checked?: boolean }>(
      '[data-select-prompt="m-old-1"]'
    )!;
    toggleCheckbox(box1, true);
    await settle(el, 10);

    const bar = root.querySelector<HTMLElement>('[data-selection-bar]')!;
    expect(bar).not.toBeNull();
    expect(bar.querySelector('[data-selected-count]')?.textContent).toBe('1 card selected');
    const barSelectAll = bar.querySelector<
      HTMLElement & { indeterminate?: boolean; checked?: boolean }
    >('[data-select-all-prompts]')!;
    expect(barSelectAll.indeterminate).toBe(true);
    expect(window.location.search).toBe(''); // page-local, never in URL

    // Select all on this page -> selects old1 & old2, skips recent -> 2 cards selected
    toggleCheckbox(barSelectAll, true);
    await settle(el, 10);
    expect(root.querySelector('[data-selected-count]')?.textContent).toBe('2 cards selected');
    expect(
      root.querySelector<HTMLElement>('[data-prompt-card="m-old-1"]')?.classList.contains('sel')
    ).toBe(true);
    expect(
      root.querySelector<HTMLElement>('[data-prompt-card="m-old-2"]')?.classList.contains('sel')
    ).toBe(true);
    expect(
      root.querySelector<HTMLElement>('[data-prompt-card="m-recent"]')?.classList.contains('sel')
    ).toBe(false);

    // Clear button resets selection and hides bar
    root.querySelector<HTMLElement>('[data-selection-clear]')!.click();
    await settle(el, 10);
    expect(root.querySelector('[data-selection-bar]')).toBeNull();
  });

  it('caps selection at 50 cards on Select all and individual toggles', async () => {
    const prompts = Array.from({ length: 55 }, (_, i) => {
      const id = `m-cap-${i + 1}`;
      return makePrompt({
        promptMessageId: id,
        createdAt: '2026-11-10T08:00:00Z',
        decisions: [makeDecision({ rowId: `r-cap-${i + 1}`, messageId: id, key: `K${i + 1}` })],
      });
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        if (url.pathname === '/api/v1/projects/p-1') return jsonResponse(PROJ_SCION);
        if (url.pathname === '/auth/me') return jsonResponse({ id: 'u-me', name: 'Alice' });
        if (url.pathname === '/api/v1/projects/p-1/decisions') {
          return jsonResponse({
            counts: { waitingOnYou: 55, waitingOnOthers: 0 },
            prompts,
            agents: [{ id: AGENT_REL, name: 'release-bot' }],
            answerers: [{ id: 'u-me', name: 'Alice' }],
            conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
          });
        }
        return jsonResponse({}, 404);
      })
    );

    window.history.replaceState({}, '', '/projects/p-1/decisions');
    const el = document.createElement('scion-page-project-decisions') as ScionPageProjectDecisions;
    el.pageData = {
      path: '/projects/p-1/decisions',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(el);
    await settle(el, 25);

    const root = el.shadowRoot!;
    const selectAll = root.querySelector<HTMLElement & { checked?: boolean }>(
      '[data-select-all-prompts]'
    )!;
    toggleCheckbox(selectAll, true);
    await settle(el, 15);

    expect(root.querySelector('[data-selected-count]')?.textContent).toBe('50 cards selected');
    const box51 = root.querySelector<HTMLElement & { checked?: boolean }>(
      '[data-select-prompt="m-cap-51"]'
    )!;
    expect(box51.hasAttribute('disabled')).toBe(true);
    toggleCheckbox(box51, true);
    await settle(el, 10);
    expect(root.querySelector('[data-selected-count]')?.textContent).toBe('50 cards selected');

    // Unchecking Select all clears all 50
    const barSelectAll = root.querySelector<HTMLElement & { checked?: boolean }>(
      '[data-select-all-prompts]'
    )!;
    toggleCheckbox(barSelectAll, false);
    await settle(el, 10);
    expect(root.querySelector('[data-selection-bar]')).toBeNull();
  });

  it('dismisses selected cards on Profile Decisions with max 4 in flight, viewer-waiting decisionIds + idempotencyKey, progress bar, partial-failure summary, and preserved changed state', async () => {
    const projWeb: ProjectDecisionProjectRef = { id: 'p-2', name: 'web-site', slug: 'web-site' };
    // Card 1 has: 1 pending row, 1 interpreted row awaiting viewer u-me (D6 -> included),
    // and 1 interpreted row awaiting bob (excluded from decisionIds!)
    const card1 = makePrompt({
      project: PROJ_SCION,
      promptMessageId: 'm-b1',
      askerAgentName: 'release-bot',
      chatKey: 'topic-release',
      conversationLabel: '#release',
      answerMode: 'set',
      waitingReasons: { 'r-b1-bob': 'other_user_confirm' },
      decisions: [
        makeDecision({
          rowId: 'r-b1-pend',
          messageId: 'm-b1',
          key: 'D234',
          question: 'Ship v0.9 today?',
        }),
        makeDecision({
          rowId: 'r-b1-me',
          messageId: 'm-b1',
          key: 'D235',
          question: 'Tag rc1?',
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-me',
            answeredByName: 'Alice',
            provenance: 'agent_interpreted',
            outcome: 'answer',
            surface: 'slack',
            responseMessageId: 'resp-i1',
            at: '2026-11-16T08:01:00Z',
          },
        }),
        makeDecision({
          rowId: 'r-b1-bob',
          messageId: 'm-b1',
          key: 'D236',
          question: 'Bob sign-off?',
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-bob',
            answeredByName: 'Bob',
            provenance: 'agent_interpreted',
            outcome: 'answer',
            surface: 'slack',
            responseMessageId: 'resp-i2',
            at: '2026-11-16T08:01:00Z',
          },
        }),
      ],
    });

    const otherCards = [2, 3, 4, 5, 6].map((n) =>
      makePrompt({
        project: n <= 3 ? PROJ_SCION : projWeb,
        promptMessageId: `m-b${n}`,
        askerAgentName: n === 6 ? 'docs-bot' : `bot-${n}`,
        chatKey: n === 6 ? 'topic-docs' : `topic-${n}`,
        conversationLabel: n === 6 ? '#docs' : `#chan-${n}`,
        decisions: [
          makeDecision({
            rowId: `r-b${n}`,
            messageId: `m-b${n}`,
            key: `K${n}`,
            question: `Question number ${n}?`,
          }),
        ],
      })
    );

    // Card 6 will fail with 409 dismiss_changed: row 1 was answered, row 2 is still open!
    otherCards[4] = makePrompt({
      project: projWeb,
      promptMessageId: 'm-b6',
      askerAgentName: 'docs-bot',
      chatKey: 'topic-docs',
      conversationLabel: '#docs',
      answerMode: 'set',
      decisions: [
        makeDecision({
          rowId: 'r-b6-a',
          messageId: 'm-b6',
          key: 'toc',
          question: 'Split CLI reference?',
        }),
        makeDecision({
          rowId: 'r-b6-b',
          messageId: 'm-b6',
          key: 'nav',
          question: 'Keep sidebar flat?',
        }),
      ],
    });

    let livePrompts: ProjectDecisionPromptView[] = [card1, ...otherCards];
    let inFlightDismiss = 0;
    let peakInFlightDismiss = 0;
    const dismissGates: Array<{ id: string; body: Record<string, unknown>; release: () => void }> =
      [];

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        if (url.pathname === '/auth/me') return jsonResponse({ id: 'u-me', name: 'Alice' });
        if (url.pathname === '/api/v1/users/me/decisions') {
          const waitingCount = livePrompts.filter((p) => p.waitingOnYou).length;
          return jsonResponse({
            counts: { waitingOnYou: waitingCount, waitingOnOthers: 0 },
            prompts: livePrompts,
            agents: [{ id: AGENT_REL, name: 'release-bot', projectId: 'p-1' }],
            answerers: [{ id: 'u-me', name: 'Alice' }],
            conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
            projects: [PROJ_SCION, projWeb],
            truncatedProjects: false,
          });
        }
        if (url.pathname.startsWith('/api/v1/agents/')) {
          return jsonResponse({ id: AGENT_REL, name: 'release-bot' });
        }
        const m = /^\/api\/v1\/decision-prompts\/([^/]+)\/dismiss$/.exec(url.pathname);
        if (m) {
          const msgId = decodeURIComponent(m[1]!);
          const body = JSON.parse((init?.body as string) || '{}') as Record<string, unknown>;
          inFlightDismiss++;
          peakInFlightDismiss = Math.max(peakInFlightDismiss, inFlightDismiss);
          return new Promise<Response>((resolve) => {
            dismissGates.push({
              id: msgId,
              body,
              release: () => {
                inFlightDismiss--;
                if (msgId === 'm-b6') {
                  const changedDecisions: DecisionView[] = [
                    makeDecision({
                      rowId: 'r-b6-a',
                      messageId: 'm-b6',
                      key: 'toc',
                      question: 'Split CLI reference?',
                      status: 'decided',
                      decision: {
                        choiceId: 'yes',
                        label: 'Yes',
                        answeredByUserId: 'u-bob',
                        answeredByName: 'Bob',
                        provenance: 'direct',
                        outcome: 'answer',
                        surface: 'native',
                        responseMessageId: 'resp-bob',
                        at: '2026-11-16T09:10:00Z',
                      },
                    }),
                    makeDecision({
                      rowId: 'r-b6-b',
                      messageId: 'm-b6',
                      key: 'nav',
                      question: 'Keep sidebar flat?',
                    }),
                  ];
                  livePrompts = livePrompts.map((p) =>
                    p.promptMessageId === 'm-b6' ? { ...p, decisions: changedDecisions } : p
                  );
                  resolve(
                    jsonResponse(
                      {
                        error: {
                          code: 'dismiss_changed',
                          message: 'the decisions to dismiss changed',
                          details: {
                            reasons: { 'r-b6-a': 'decided' },
                            decisions: changedDecisions,
                          },
                        },
                      },
                      409
                    )
                  );
                  return;
                }
                const target = livePrompts.find((p) => p.promptMessageId === msgId)!;
                const reqIds = new Set((body.decisionIds as string[]) ?? []);
                const dismissedViews = target.decisions
                  .filter((d) => reqIds.has(d.rowId))
                  .map((d) => ({
                    ...d,
                    status: 'dismissed' as const,
                    dismissed: {
                      at: '2026-11-16T09:12:00Z',
                      byUserId: 'u-me',
                      byName: 'Alice',
                      reason: typeof body.reason === 'string' ? body.reason : '',
                      surface: 'decisions_page',
                      responseMessageId: `resp-${msgId}`,
                    },
                  }));
                livePrompts = livePrompts.filter((p) => p.promptMessageId !== msgId);
                resolve(
                  jsonResponse({
                    dismissed: dismissedViews,
                    alreadyDismissed: [],
                    responseMessageId: `resp-${msgId}`,
                    delivered: true,
                    deliveryStatus: 'delivered',
                  })
                );
              },
            });
          });
        }
        return jsonResponse({}, 404);
      })
    );

    window.history.replaceState({}, '', '/profile/decisions');
    const el = document.createElement('scion-page-profile-decisions') as ScionPageProfileDecisions;
    el.pageData = {
      path: '/profile/decisions',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(el);
    await settle(el, 25);

    const root = el.shadowRoot!;
    // Select all 6 cards on Profile Waiting on you
    const selectAll = root.querySelector<HTMLElement & { checked?: boolean }>(
      '[data-select-all-prompts]'
    )!;
    toggleCheckbox(selectAll, true);
    await settle(el, 15);
    expect(root.querySelector('[data-selected-count]')?.textContent).toBe('6 cards selected');

    // Open bulk dismiss dialog
    root.querySelector<HTMLElement>('[data-selection-dismiss]')!.click();
    await settle(el, 15);

    const dialog = root.querySelector<HTMLElement>('sl-dialog[data-bulk-dismiss-dialog]')!;
    expect(dialog).not.toBeNull();
    // Lists agent, conversation, and first question for each selected card
    expect(dialog.textContent).toContain('release-bot');
    expect(dialog.textContent).toContain('#release');
    expect(dialog.textContent).toContain('Ship v0.9 today?');
    expect(dialog.textContent).toContain('docs-bot');
    expect(dialog.textContent).toContain('#docs');
    expect(dialog.textContent).toContain('Split CLI reference?');

    // Enter optional reason
    const textarea = dialog.querySelector<HTMLElement & { value?: string }>(
      '[data-bulk-dismiss-reason]'
    )!;
    textarea.value = '  Sprint closed; cleaning stale queue  ';
    textarea.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await settle(el, 5);

    // Confirm -> starts at most 4 calls in flight and shows progress
    dialog.querySelector<HTMLElement>('[data-bulk-dismiss-confirm]')!.click();
    await settle(el, 10);

    expect(inFlightDismiss).toBe(4);
    expect(dismissGates).toHaveLength(4);
    expect(dialog.querySelector('[data-bulk-dismiss-progress]')?.textContent).toContain('0 of 6');

    // Release first 2 -> 2 more start (still max 4 in flight), progress shows 2 of 6
    dismissGates[0]!.release();
    dismissGates[1]!.release();
    await settle(el, 10);
    expect(inFlightDismiss).toBe(4);
    expect(peakInFlightDismiss).toBe(4);
    expect(dismissGates).toHaveLength(6);
    expect(dialog.querySelector('[data-bulk-dismiss-progress]')?.textContent).toContain('2 of 6');

    // Release remaining 4
    for (let i = 2; i < 6; i++) {
      dismissGates[i]!.release();
    }
    await settle(el, 90);

    expect(peakInFlightDismiss).toBe(4);
    expect(root.querySelector('sl-dialog[data-bulk-dismiss-dialog]')).toBeNull();

    // Verify card 1 sent only viewer-waiting rows (pending + own interpreted, NOT bob's!) + idempotencyKey
    const card1Req = dismissGates.find((g) => g.id === 'm-b1')!;
    expect(card1Req.body.decisionIds).toEqual(['r-b1-pend', 'r-b1-me']);
    expect(card1Req.body.reason).toBe('Sprint closed; cleaning stale queue');
    expect(card1Req.body.surface).toBe('decisions_page');
    expect(typeof card1Req.body.idempotencyKey).toBe('string');
    expect((card1Req.body.idempotencyKey as string).length).toBeGreaterThan(8);

    // Unique idempotencyKey per card
    const keys = dismissGates.map((g) => g.body.idempotencyKey as string);
    expect(new Set(keys).size).toBe(6);

    // Summary toast reports partial success and names the changed card
    expect(showToast).toHaveBeenCalledTimes(1);
    const [toastMsg, toastVariant] = showToast.mock.calls[0]!;
    expect(toastVariant).toBe('warning');
    expect(toastMsg).toContain('Dismissed 5 of 6 cards; 1 had changed');
    expect(toastMsg).toContain('docs-bot · #docs');

    // Dismissed cards removed; changed card m-b6 remains with its updated state (row 1 decided by Bob)
    expect(root.querySelector('[data-prompt-card="m-b1"]')).toBeNull();
    const changedCard = root.querySelector<HTMLElement>('[data-prompt-card="m-b6"]')!;
    expect(changedCard).not.toBeNull();
    const innerDecisions = changedCard.querySelector('scion-chat-decisions') as HTMLElement & {
      decisions?: DecisionView[];
    };
    expect(innerDecisions.decisions?.[0]?.status).toBe('decided');
    expect(innerDecisions.decisions?.[1]?.status).toBe('pending');
  });

  it('distinguishes a deleted (404) asking agent from a suspended agent in the bulk dismiss dialog and Profile sort=recent (ptone/scion#8217)', async () => {
    const suspAgentId = '22222222-3333-4444-5555-666666666666';
    const goneAgentId = '33333333-4444-5555-6666-777777777777';
    const suspCard = makePrompt({
      promptMessageId: 'm-susp-1',
      askerAgentId: suspAgentId,
      askerAgentName: 'sleep-bot',
      chatKey: 'topic-ops',
      conversationLabel: '#ops',
      decisions: [
        makeDecision({
          rowId: 'r-s1',
          messageId: 'm-susp-1',
          key: 'S1',
          question: 'Restart worker?',
        }),
      ],
    });
    const goneCard = makePrompt({
      promptMessageId: 'm-gone-1',
      askerAgentId: goneAgentId,
      askerAgentName: 'nc-x',
      chatKey: 'dm:ncx',
      conversationLabel: 'DM with nc-x',
      decisions: [
        makeDecision({
          rowId: 'r-g1',
          messageId: 'm-gone-1',
          key: 'G1',
          question: 'Run migration?',
        }),
      ],
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        if (url.pathname === '/auth/me') return jsonResponse({ id: 'u-me', name: 'Alice' });
        if (url.pathname === '/api/v1/users/me/decisions') {
          return jsonResponse({
            counts: { waitingOnYou: 2, waitingOnOthers: 0 },
            prompts: [suspCard, goneCard],
            agents: [
              { id: suspAgentId, name: 'sleep-bot', projectId: 'p-1' },
              { id: goneAgentId, name: 'nc-x', projectId: 'p-1' },
            ],
            answerers: [{ id: 'u-me', name: 'Alice' }],
            conversations: [{ key: 'topic-ops', label: '#ops', projectId: 'p-1' }],
            projects: [PROJ_SCION],
            truncatedProjects: false,
          });
        }
        if (url.pathname === `/api/v1/agents/${suspAgentId}`) {
          return jsonResponse({ id: suspAgentId, name: 'sleep-bot', phase: 'suspended' });
        }
        if (url.pathname === `/api/v1/agents/${goneAgentId}`) {
          return jsonResponse({ error: { code: 'not_found' } }, 404);
        }
        return jsonResponse({}, 404);
      })
    );

    window.history.replaceState({}, '', '/profile/decisions?tab=pending&sort=recent');
    const el = document.createElement('scion-page-profile-decisions') as ScionPageProfileDecisions;
    el.pageData = {
      path: '/profile/decisions?tab=pending&sort=recent',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(el);
    await settle(el, 25);

    const root = el.shadowRoot!;
    // Select only the suspended-agent card -> dialog must NOT claim sleep-bot is gone
    toggleCheckbox(
      root.querySelector<HTMLElement & { checked?: boolean }>('[data-select-prompt="m-susp-1"]')!,
      true
    );
    await settle(el, 10);
    root.querySelector<HTMLElement>('[data-selection-dismiss]')!.click();
    await settle(el, 25);

    let dialog = root.querySelector<HTMLElement>('sl-dialog[data-bulk-dismiss-dialog]')!;
    expect(dialog.querySelector('[data-bulk-dismiss-tell="gone"]')).toBeNull();
    expect(dialog.textContent).not.toContain('sleep-bot is gone');
    dialog.querySelector<HTMLElement>('[data-bulk-dismiss-cancel]')!.click();
    await settle(el, 10);

    // Select only the deleted-agent card -> 404 probe marks nc-x as gone
    root.querySelector<HTMLElement>('[data-selection-clear]')!.click();
    await settle(el, 10);
    toggleCheckbox(
      root.querySelector<HTMLElement & { checked?: boolean }>('[data-select-prompt="m-gone-1"]')!,
      true
    );
    await settle(el, 10);
    root.querySelector<HTMLElement>('[data-selection-dismiss]')!.click();
    await settle(el, 25);

    dialog = root.querySelector<HTMLElement>('sl-dialog[data-bulk-dismiss-dialog]')!;
    expect(dialog.querySelector('[data-bulk-dismiss-tell="gone"]')?.textContent).toContain(
      'nc-x is gone; nothing will be sent.'
    );
  });

  it('renders (deleted) and dismiss available at <time> in Waiting on you headers and clamps 500-rune bulk reasons (ptone/scion#8217)', async () => {
    const goneAgentId = '44444444-5555-6666-7777-888888888888';
    const docsAgentId = '55555555-6666-7777-8888-999999999999';
    const recentCreatedAt = new Date(Date.now() - 2 * 60_000).toISOString();
    const gonePrompt = makePrompt({
      promptMessageId: 'm-hdr-gone',
      askerAgentId: goneAgentId,
      askerAgentName: 'nc-x',
      chatKey: 'dm:ncx',
      conversationLabel: 'DM with nc-x',
      decisions: [
        makeDecision({
          rowId: 'r-hg-1',
          messageId: 'm-hdr-gone',
          key: 'migrate',
          question: 'Run backfill?',
        }),
      ],
    });
    const recentPrompt = makePrompt({
      promptMessageId: 'm-hdr-recent',
      askerAgentId: docsAgentId,
      askerAgentName: 'docs-bot',
      chatKey: 'topic-docs',
      conversationLabel: '#docs',
      createdAt: recentCreatedAt,
      decisions: [
        makeDecision({
          rowId: 'r-hr-1',
          messageId: 'm-hdr-recent',
          key: 'toc',
          question: 'Split CLI reference?',
        }),
      ],
    });

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        if (url.pathname === '/auth/me') return jsonResponse({ id: 'u-me', name: 'Alice' });
        if (url.pathname === '/api/v1/users/me/decisions') {
          return jsonResponse({
            counts: { waitingOnYou: 2, waitingOnOthers: 0 },
            prompts: [gonePrompt, recentPrompt],
            agents: [
              { id: goneAgentId, name: 'nc-x', hint: '(deleted)', projectId: 'p-1' },
              { id: docsAgentId, name: 'docs-bot', projectId: 'p-1' },
            ],
            answerers: [{ id: 'u-me', name: 'Alice' }],
            conversations: [{ key: 'topic-docs', label: '#docs', projectId: 'p-1' }],
            projects: [PROJ_SCION],
            truncatedProjects: false,
          });
        }
        if (url.pathname === `/api/v1/agents/${goneAgentId}`) {
          return jsonResponse({ error: { code: 'not_found' } }, 404);
        }
        return jsonResponse({}, 404);
      })
    );

    window.history.replaceState({}, '', '/profile/decisions');
    const el = document.createElement('scion-page-profile-decisions') as ScionPageProfileDecisions;
    el.pageData = {
      path: '/profile/decisions',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
    };
    document.body.appendChild(el);
    await settle(el, 25);

    const root = el.shadowRoot!;
    expect(
      root.querySelector('[data-prompt-card="m-hdr-gone"] [data-asker-deleted]')?.textContent
    ).toBe('(deleted)');
    expect(root.querySelector('[data-prompt-card="m-hdr-recent"] [data-asker-deleted]')).toBeNull();
    expect(
      root.querySelector('[data-prompt-card="m-hdr-recent"] [data-dismiss-available-at]')
        ?.textContent
    ).toMatch(/· dismiss available at\s+\d{2}:\d{2}/);
    expect(
      root.querySelector('[data-prompt-card="m-hdr-gone"] [data-dismiss-available-at]')
    ).toBeNull();

    toggleCheckbox(
      root.querySelector<HTMLElement & { checked?: boolean }>('[data-select-prompt="m-hdr-gone"]')!,
      true
    );
    await settle(el, 10);
    root.querySelector<HTMLElement>('[data-selection-dismiss]')!.click();
    await settle(el, 15);

    const dialog = root.querySelector<HTMLElement>('sl-dialog[data-bulk-dismiss-dialog]')!;
    const textarea = dialog.querySelector<HTMLElement & { value?: string }>(
      '[data-bulk-dismiss-reason]'
    )!;
    expect(textarea.hasAttribute('maxlength')).toBe(false);
    textarea.value = '🚀'.repeat(505);
    textarea.dispatchEvent(new Event('sl-input', { bubbles: true }));
    await settle(el, 5);
    expect([...(textarea.value ?? '')]).toHaveLength(500);
    expect(dialog.querySelector('[data-bulk-dismiss-reason-count]')?.textContent).toBe('500/500');
  });
});
