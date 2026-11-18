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
 * Unit tests for `<scion-page-project-decisions>` (ptone/scion#7800, Mocks P3-2..P3-6).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import type { DecisionView, PageData } from '../../shared/types.js';
import type {
  ProjectDecisionLogEntryView,
  ProjectDecisionPromptView,
  ProjectMadeDecisionsResponse,
  ProjectPendingDecisionsResponse,
} from '../../client/project-decisions.js';
import { PAGE_TITLE_EVENT } from '../../client/page-title.js';
import type { PageTitleDetail } from '../../client/page-title.js';
import { stateManager } from '../../client/state.js';
import {
  DECISIONS_FLAG,
  FEATURE_FLAGS_CHANGED_EVENT,
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

await import('./project-decisions.js');
type ScionPageProjectDecisions = import('./project-decisions.js').ScionPageProjectDecisions;
type ScionChatDecisions = import('../shared/chat/chat-decisions.js').ScionChatDecisions;
type ScionDecisionsFilterPanel =
  import('../shared/decisions-filter-panel.js').ScionDecisionsFilterPanel;

function makeDecision(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r-1',
    messageId: 'm-1',
    key: 'D234',
    ref: 'D234',
    version: 1,
    status: 'pending',
    askerAgentId: 'a-rel',
    askerAgentName: 'release-bot',
    question: 'Ship v0.9 today?',
    defaultChoiceId: 'yes',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
    ],
    ...over,
  };
}

function makePrompt(over: Partial<ProjectDecisionPromptView> = {}): ProjectDecisionPromptView {
  return {
    promptMessageId: 'm-1',
    promptAvailable: true,
    chatKey: 'topic-release',
    conversationLabel: '#release',
    conversationAvailable: true,
    askerAgentId: 'a-rel',
    askerAgentName: 'release-bot',
    createdAt: '2026-11-13T15:30:00Z',
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
    answerId: 'ans-1',
    decision: {
      rowId: 'r-1',
      promptMessageId: 'm-1',
      promptAvailable: true,
      key: 'D9',
      ref: 'D9',
      version: 1,
      question: 'Cut the RC from main?',
      askerAgentId: 'a-rel',
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
    at: '2026-11-13T15:52:00Z',
    chatKey: 'topic-release',
    conversationLabel: '#release',
    conversationAvailable: true,
    sourceMessageAvailable: false,
    ...over,
  };
}

async function settle(el: ScionPageProjectDecisions, ms = 15): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
  await el.updateComplete;
  const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
    'scion-decisions-filter-panel'
  );
  await panel?.updateComplete;
  for (const card of Array.from(
    el.shadowRoot?.querySelectorAll<ScionChatDecisions>('scion-chat-decisions') ?? []
  )) {
    await card.updateComplete;
  }
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
  await panel?.updateComplete;
}

function panelRootOf(el: ScionPageProjectDecisions): ShadowRoot {
  const panel = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
    'scion-decisions-filter-panel'
  );
  if (!panel?.shadowRoot) throw new Error('expected <scion-decisions-filter-panel> shadowRoot');
  return panel.shadowRoot;
}

describe('<scion-page-project-decisions>', () => {
  let pendingResponses: ProjectPendingDecisionsResponse[];
  let madeResponses: ProjectMadeDecisionsResponse[];
  let projectHandler: ((url: URL, init?: RequestInit) => Promise<Response>) | null;
  let pendingHandler: ((url: URL, init?: RequestInit) => Promise<Response>) | null;
  let madeHandler: ((url: URL, init?: RequestInit) => Promise<Response>) | null;
  let agentHandler: ((url: URL, init?: RequestInit) => Promise<Response>) | null;
  let requests: { method: string; url: string; body?: unknown }[];
  let answerResponseFn: (url: URL, body: unknown) => Promise<Response>;
  let navEvents: string[];
  const onNavClick = (e: Event): void => {
    const path = (e as CustomEvent<{ path?: string }>).detail?.path;
    if (path) navEvents.push(path);
  };

  beforeAll(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  beforeEach(() => {
    delete window.__SCION_FEATURES__;
    window.localStorage.removeItem('scion:decisions:filters-open');
    resetServerFlagStateForTests();
    setPreferredTimeZone('UTC');
    window.history.replaceState({}, '', '/projects/p-1/decisions');
    requests = [];
    navEvents = [];
    showToast.mockReset();
    projectHandler = null;
    pendingHandler = null;
    madeHandler = null;
    agentHandler = null;
    document.addEventListener('nav-click', onNavClick);

    pendingResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt()],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];
    madeResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [makeLogEntry()],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [
          { id: 'u-me', name: 'Alice' },
          { id: 'u-bob', name: 'Bob' },
        ],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];
    answerResponseFn = (_url, body) => {
      const b = body as { choice_id?: string };
      const cid = b?.choice_id ?? 'yes';
      return jsonResponse({
        decision: makeDecision({
          status: 'decided',
          decision: {
            choiceId: cid,
            label: cid === 'yes' ? 'Yes' : 'No',
            answeredByUserId: 'u-me',
            answeredByName: 'Alice',
            provenance: 'direct',
            outcome: 'answer',
            surface: 'decisions_page',
            responseMessageId: 'resp-1',
            at: '2026-11-13T16:00:00Z',
          },
        }),
        responseMessageId: 'resp-1',
      });
    };

    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const raw =
          typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const url = new URL(raw, 'http://localhost');
        const method = (init?.method ?? 'GET').toUpperCase();
        const parsedBody =
          typeof init?.body === 'string' && init.body ? JSON.parse(init.body) : undefined;
        requests.push({ method, url: url.pathname + url.search, body: parsedBody });

        if (url.pathname === '/api/v1/projects/p-1') {
          if (projectHandler) return projectHandler(url, init);
          return jsonResponse({ id: 'p-1', name: 'scion', slug: 'scion' });
        }
        if (url.pathname === '/auth/me' || url.pathname === '/api/v1/auth/me') {
          return jsonResponse({ id: 'u-me', name: 'Alice' });
        }
        if (url.pathname === '/api/v1/projects/p-1/decisions') {
          const tab = url.searchParams.get('tab') ?? 'pending';
          if (tab === 'made') {
            if (madeHandler) return madeHandler(url, init);
            const next = madeResponses.length > 1 ? madeResponses.shift()! : madeResponses[0]!;
            return jsonResponse(next);
          }
          if (pendingHandler) return pendingHandler(url, init);
          const next =
            pendingResponses.length > 1 ? pendingResponses.shift()! : pendingResponses[0]!;
          return jsonResponse(next);
        }
        if (url.pathname.startsWith('/api/v1/agents/') && agentHandler) {
          return agentHandler(url, init);
        }
        if (
          url.pathname.startsWith('/api/v1/decisions/') ||
          url.pathname.startsWith('/api/v1/decision-prompts/')
        ) {
          return answerResponseFn(url, parsedBody);
        }
        return jsonResponse({}, 404);
      })
    );
  });

  afterEach(() => {
    document.removeEventListener('nav-click', onNavClick);
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    setPreferredTimeZone('');
    vi.unstubAllGlobals();
    vi.stubGlobal('EventSource', FakeEventSource);
  });

  function enableDecisionsFlag(): void {
    setFeatureFlag(DECISIONS_FLAG, true);
  }

  async function mount(pageData?: Partial<PageData>): Promise<ScionPageProjectDecisions> {
    const el = document.createElement('scion-page-project-decisions');
    el.pageData = {
      path: '/projects/p-1/decisions',
      title: 'Decisions',
      user: { id: 'u-me', email: 'alice@example.com', name: 'Alice', role: 'member' },
      ...pageData,
    };
    document.body.appendChild(el);
    await settle(el, 25);
    return el;
  }

  it('renders 404 and makes zero API calls when web.decisions is disabled', async () => {
    const el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(requests).toEqual([]);
  });

  it('renders Waiting on you live and purged cards, forwards surface="decisions_page", and renders collapsed Waiting on others', async () => {
    enableDecisionsFlag();
    const liveTopicPrompt = makePrompt({
      promptMessageId: 'm-topic',
      promptAvailable: true,
      chatKey: 'topic-rel',
      conversationLabel: '#release',
      conversationAvailable: true,
      decisions: [
        makeDecision({
          rowId: 'r-interp',
          messageId: 'm-topic',
          key: 'D234',
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-me',
            answeredByName: 'Alice',
            provenance: 'agent_interpreted',
            outcome: 'answer',
            surface: 'slack',
            sourceMessageId: 'msg-reply-9',
            responseMessageId: 'resp-i',
            at: '2026-11-13T15:44:00Z',
          },
        }),
      ],
    });
    const purgedDmPrompt = makePrompt({
      promptMessageId: 'm-dm-purged',
      promptAvailable: false,
      chatKey: 'dm:agent:a-nc:user:u-me',
      conversationLabel: 'DM with nc-x',
      conversationAvailable: true,
      askerAgentId: 'a-nc',
      askerAgentName: 'nc-x',
      decisions: [
        makeDecision({
          rowId: 'r-purged',
          messageId: 'm-dm-purged',
          key: 'migrate-db',
          askerAgentId: 'a-nc',
          askerAgentName: 'nc-x',
          question: 'Run backfill on staging first?',
        }),
      ],
    });
    const othersReadOnly = makePrompt({
      promptMessageId: 'm-ro',
      promptAvailable: true,
      chatKey: 'topic-ops',
      conversationLabel: '#ops',
      conversationAvailable: true,
      askerAgentId: 'a-docs',
      askerAgentName: 'docs-bot',
      waitingOnYou: false,
      canAnswer: false,
      waitingReasons: { 'r-ro': 'read_only' },
      decisions: [
        makeDecision({ rowId: 'r-ro', messageId: 'm-ro', key: 'D41', question: 'Rotate key?' }),
      ],
    });
    const othersConfirm = makePrompt({
      promptMessageId: 'm-bob',
      promptAvailable: false,
      chatKey: 'topic-rel',
      conversationLabel: '#release',
      conversationAvailable: true,
      waitingOnYou: false,
      canAnswer: false,
      waitingReasons: { 'r-bob': 'other_user_confirm' },
      decisions: [
        makeDecision({
          rowId: 'r-bob',
          messageId: 'm-bob',
          key: 'ship-now',
          question: 'Deploy after tagging?',
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-bob',
            answeredByName: 'Bob',
            provenance: 'agent_interpreted',
            outcome: 'answer',
            surface: 'native',
            responseMessageId: 'resp-bob',
            at: '2026-11-13T15:46:00Z',
          },
        }),
      ],
    });

    pendingResponses = [
      {
        counts: { waitingOnYou: 2, waitingOnOthers: 2 },
        prompts: [liveTopicPrompt, purgedDmPrompt, othersReadOnly, othersConfirm],
        agents: [],
        answerers: [],
      },
      // After answering r-purged:
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 2 },
        prompts: [liveTopicPrompt, othersReadOnly, othersConfirm],
        agents: [],
        answerers: [],
      },
    ];

    const el = await mount();
    const root = el.shadowRoot!;

    expect(root.querySelector('[data-waiting-badge]')?.textContent?.trim()).toBe('2');

    const cards = root.querySelectorAll('.pcard');
    expect(cards).toHaveLength(2);

    // Card 1: live topic prompt -> Open in chat permalink + View reply navigation
    const topicCard = root.querySelector('[data-prompt-card="m-topic"]')!;
    expect(topicCard.querySelector('[data-conv-link]')?.getAttribute('href')).toBe(
      '/chat/space/p-1/thread/topic-rel'
    );
    expect(topicCard.querySelector('[data-open-in-chat]')?.getAttribute('href')).toBe(
      '/chat/space/p-1/thread/topic-rel#msg-m-topic'
    );
    expect(topicCard.querySelector('[data-purged-note]')).toBeNull();

    const topicChoices = topicCard.querySelector<ScionChatDecisions>('scion-chat-decisions')!;
    expect(topicChoices.surface).toBe('decisions_page');
    topicChoices.shadowRoot?.querySelector<HTMLElement>('[data-action="view-reply"]')?.click();
    expect(navEvents).toContain('/chat/space/p-1/thread/topic-rel#msg-msg-reply-9');

    // Card 2: purged DM prompt -> "in your DM" link, purged note, NO Open in chat link, still answers!
    const dmCard = root.querySelector('[data-prompt-card="m-dm-purged"]')!;
    expect(dmCard.querySelector('[data-conv-link]')?.textContent?.trim()).toBe('your DM');
    expect(dmCard.querySelector('[data-conv-link]')?.getAttribute('href')).toBe(
      '/chat/dm/dm%3Aagent%3Aa-nc%3Auser%3Au-me'
    );
    expect(dmCard.querySelector('[data-open-in-chat]')).toBeNull();
    expect(dmCard.querySelector('[data-purged-note]')?.textContent).toContain(
      'Original message no longer available'
    );

    const dmChoices = dmCard.querySelector<ScionChatDecisions>('scion-chat-decisions')!;
    dmChoices.shadowRoot?.querySelector<HTMLElement>('[data-choice="yes"]')?.click();
    await settle(el, 90);

    const postCall = requests.find(
      (r) => r.method === 'POST' && r.url === '/api/v1/decisions/r-purged/answer'
    );
    expect(postCall?.body).toMatchObject({
      choice_id: 'yes',
      surface: 'decisions_page',
    });

    // Purged DM card removed from Waiting on you and badge decremented 2 -> 1
    expect(root.querySelector('[data-prompt-card="m-dm-purged"]')).toBeNull();
    expect(root.querySelector('[data-waiting-badge]')?.textContent?.trim()).toBe('1');

    // Collapsed Waiting on others (2)
    const othersDetails = root.querySelector<HTMLElement>('[data-waiting-others]')!;
    expect(othersDetails).not.toBeNull();
    expect(othersDetails.hasAttribute('open')).toBe(false);
    expect(othersDetails.getAttribute('summary')).toBe('Waiting on others (2)');
    expect(othersDetails.querySelector('[data-others-row="r-ro"]')?.textContent).toContain(
      'read-only for you'
    );
    expect(othersDetails.querySelector('[data-others-row="r-bob"]')?.textContent).toContain(
      'Bob to confirm'
    );
    expect(othersDetails.querySelector('[data-others-row="r-bob"]')?.textContent).toContain(
      'Original message no longer available'
    );
  });

  it('shows empty Waiting on you state and switches to Made when clicking "See decisions made"', async () => {
    enableDecisionsFlag();
    pendingResponses = [
      {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [],
        answerers: [],
      },
    ];
    madeResponses = [
      {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        entries: [],
        agents: [],
        answerers: [],
      },
    ];

    const el = await mount();
    const root = el.shadowRoot!;
    expect(root.querySelector('[data-waiting-badge]')).toBeNull();
    expect(root.querySelector('[data-empty="pending"]')?.textContent).toContain(
      'Nothing is waiting on you'
    );

    root.querySelector<HTMLElement>('[data-see-made]')!.click();
    await settle(el, 25);

    expect(window.location.search).toBe('?tab=made');
    expect(root.querySelector('sl-tab-panel[name="made"]')?.hasAttribute('active')).toBe(true);
    expect(root.querySelector('[data-empty="made"]')?.textContent).toContain(
      'No decisions recorded yet'
    );
  });

  it('renders Made log rows (deleted user, unavailable conversation, reply link gating, surfaces), filters, and Load more', async () => {
    enableDecisionsFlag();
    const rowOverride = makeLogEntry({
      answerId: 'ans-over',
      outcome: 'override',
      provenance: 'direct',
      previousChoiceLabel: 'Yes',
      surface: 'decisions_page',
      answeredByUserId: 'u-me',
      answeredByName: 'Alice',
    });
    const rowInterpretedLive = makeLogEntry({
      answerId: 'ans-interp-live',
      choiceId: 'yes',
      label: 'Yes',
      outcome: 'answer',
      provenance: 'agent_interpreted',
      surface: 'slack',
      sourceMessageId: 'msg-src-1',
      sourceMessageAvailable: true,
      conversationAvailable: true,
    });
    const rowInterpretedPurged = makeLogEntry({
      answerId: 'ans-interp-purged',
      choiceId: 'yes',
      label: 'Yes',
      outcome: 'answer',
      provenance: 'agent_interpreted',
      surface: 'discord',
      sourceMessageId: 'msg-src-purged',
      sourceMessageAvailable: false,
      conversationAvailable: true,
    });
    const rowDeletedUserDeadConv = makeLogEntry({
      answerId: 'ans-deleted',
      choiceId: 'yes',
      label: 'Yes',
      outcome: 'confirm',
      provenance: 'direct',
      surface: 'native',
      answeredByUserId: '',
      answeredByName: 'Charlie',
      chatKey: 'dm:agent:a-gone:user:u-charlie',
      conversationLabel: 'DM with nc-x',
      conversationAvailable: false,
    });
    const rowPage2 = makeLogEntry({
      answerId: 'ans-page2',
      choiceId: 'a',
      label: 'a · Extend input-needed',
      outcome: 'answer',
      provenance: 'direct',
      matchedDefault: true,
      surface: 'teams',
      answeredByUserId: 'u-bob',
      answeredByName: 'Bob',
    });

    madeResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [rowOverride, rowInterpretedLive, rowInterpretedPurged, rowDeletedUserDeadConv],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [
          { id: 'u-me', name: 'Alice' },
          { id: 'u-bob', name: 'Bob' },
        ],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
        nextCursor: 'cur-page-2',
      },
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [rowPage2],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [
          { id: 'u-me', name: 'Alice' },
          { id: 'u-bob', name: 'Bob' },
        ],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;

    // Row 1: override (.follow, "you", "changed from Yes", "Decisions page")
    const rOver = root.querySelector('[data-answer-row="ans-over"]')!;
    expect(rOver.classList.contains('follow')).toBe(true);
    expect(rOver.querySelector('[data-answered-by]')?.textContent?.trim()).toBe('you');
    expect(rOver.querySelector('[data-provenance]')?.textContent).toContain('changed');
    expect(rOver.querySelector('[data-provenance]')?.textContent).toContain('from Yes');
    expect(rOver.querySelector('[data-surface]')?.textContent?.trim()).toBe('Decisions page');

    // Row 2: interpreted with available source message -> has reply link
    const rInterpLive = root.querySelector('[data-answer-row="ans-interp-live"]')!;
    expect(rInterpLive.querySelector('[data-reply-link]')?.getAttribute('href')).toBe(
      '/chat/space/p-1/thread/topic-release#msg-msg-src-1'
    );
    expect(rInterpLive.querySelector('[data-surface]')?.textContent?.trim()).toBe('Slack');

    // Row 3: interpreted with purged source message -> NO reply link
    const rInterpPurged = root.querySelector('[data-answer-row="ans-interp-purged"]')!;
    expect(rInterpPurged.querySelector('[data-reply-link]')).toBeNull();
    expect(rInterpPurged.querySelector('[data-surface]')?.textContent?.trim()).toBe('Discord');

    // Row 4: deleted user ("Charlie (deleted user)", no link) + unavailable conversation ("your DM" muted, no link)
    const rDeleted = root.querySelector('[data-answer-row="ans-deleted"]')!;
    expect(rDeleted.classList.contains('follow')).toBe(true);
    const byCell = rDeleted.querySelector('[data-answered-by]')!;
    expect(byCell.textContent?.replace(/\s+/g, ' ').trim()).toBe('Charlie (deleted user)');
    expect(byCell.querySelector('a')).toBeNull();
    const whereCell = rDeleted.querySelector('[data-agent-where]')!;
    expect(whereCell.querySelector('a')).toBeNull();
    expect(whereCell.querySelector('[data-conv-unavailable]')?.textContent?.trim()).toBe('your DM');

    // Load more appends page 2 with cursor=cur-page-2
    root.querySelector<HTMLElement>('[data-load-more="made"]')!.click();
    await settle(el, 25);
    expect(
      requests.some((r) => r.url.includes('tab=made') && r.url.includes('cursor=cur-page-2'))
    ).toBe(true);
    const rP2 = root.querySelector('[data-answer-row="ans-page2"]')!;
    expect(rP2).not.toBeNull();
    expect(rP2.querySelector('[data-provenance]')?.textContent).toContain('recommended');
    expect(rP2.querySelector('[data-surface]')?.textContent?.trim()).toBe('Teams');
    expect(root.querySelector('[data-load-more="made"]')).toBeNull();

    // Filter changes combine & reset cursor
    const pRoot = panelRootOf(el);
    const agentCombobox = pRoot.querySelector<HTMLElement>('[data-filter="agent"]')!;
    agentCombobox.dispatchEvent(
      new CustomEvent('facet-change', {
        detail: { value: 'a-rel', label: 'release-bot' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 25);

    const provSelect = pRoot.querySelector<HTMLElement & { value?: string }>(
      '[data-filter="provenance"]'
    )!;
    provSelect.value = 'confirmed';
    provSelect.dispatchEvent(new CustomEvent('sl-change'));
    await settle(el, 25);

    const lastMadeUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(lastMadeUrl).toBe(
      '/api/v1/projects/p-1/decisions?tab=made&agent=a-rel&provenance=confirmed'
    );
    expect(root.querySelector('[data-no-match="made"]')?.textContent).toContain(
      'No decisions match these filters'
    );
  });

  it('removes a prompt settled in another tab live on decision-updated and updates counts without reload', async () => {
    enableDecisionsFlag();
    pendingResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [
          makePrompt({
            promptMessageId: 'm-dm-live',
            chatKey: 'dm:agent:a-rel:user:u-me',
            conversationLabel: 'DM with release-bot',
            decisions: [makeDecision({ rowId: 'r-dm-live', messageId: 'm-dm-live' })],
          }),
        ],
        agents: [],
        answerers: [],
      },
      // Server response on background live refresh after SSE:
      {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [],
        answerers: [],
      },
    ];

    const el = await mount();
    const root = el.shadowRoot!;
    expect(root.querySelector('[data-prompt-card="m-dm-live"]')).not.toBeNull();
    expect(root.querySelector('[data-waiting-badge]')?.textContent?.trim()).toBe('1');

    // Another project's event is ignored
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: {
          data: {
            projectId: 'other-project',
            messageId: 'm-dm-live',
            decision: makeDecision({
              rowId: 'r-dm-live',
              messageId: 'm-dm-live',
              status: 'decided',
              decision: {
                choiceId: 'yes',
                label: 'Yes',
                answeredByUserId: 'u-me',
                answeredByName: 'Alice',
                provenance: 'direct',
                outcome: 'answer',
                surface: 'decisions_page',
                responseMessageId: 'resp-sse',
                at: '2026-11-13T16:05:00Z',
              },
            }),
          },
        },
      })
    );
    await settle(el, 20);
    expect(root.querySelector('[data-prompt-card="m-dm-live"]')).not.toBeNull();

    // User-scoped DM decision-updated (no projectId) settles r-dm-live immediately and refreshes
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: {
          data: {
            messageId: 'm-dm-live',
            decision: makeDecision({
              rowId: 'r-dm-live',
              messageId: 'm-dm-live',
              status: 'decided',
              decision: {
                choiceId: 'yes',
                label: 'Yes',
                answeredByUserId: 'u-me',
                answeredByName: 'Alice',
                provenance: 'direct',
                outcome: 'answer',
                surface: 'decisions_page',
                responseMessageId: 'resp-sse',
                at: '2026-11-13T16:05:00Z',
              },
            }),
          },
        },
      })
    );
    await settle(el, 90);

    expect(root.querySelector('[data-prompt-card="m-dm-live"]')).toBeNull();
    expect(root.querySelector('[data-waiting-badge]')).toBeNull();
    expect(root.querySelector('[data-empty="pending"]')).not.toBeNull();
  });

  it('preserves Made Load more pages and cursor on live decision-updated, and never aborts an in-flight Load more', async () => {
    enableDecisionsFlag();
    const rowP1 = makeLogEntry({ answerId: 'ans-p1', label: 'Page 1 original' });
    const rowP2 = makeLogEntry({ answerId: 'ans-p2', label: 'Page 2 older' });
    const rowNew = makeLogEntry({ answerId: 'ans-new', label: 'Prepended live answer' });
    const rowP1Updated = makeLogEntry({ answerId: 'ans-p1', label: 'Page 1 updated' });

    let loadMoreAborted = false;
    let resolveLoadMore!: (res: Response) => void;
    let madeCall = 0;

    madeHandler = (url, init) => {
      madeCall++;
      const cursor = url.searchParams.get('cursor');
      if (madeCall === 1) {
        return jsonResponse({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          entries: [rowP1],
          agents: [{ id: 'a-rel', name: 'release-bot' }],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          nextCursor: 'cur-page-2',
        });
      }
      if (cursor === 'cur-page-2') {
        init?.signal?.addEventListener('abort', () => {
          loadMoreAborted = true;
        });
        return new Promise<Response>((resolve) => {
          resolveLoadMore = resolve;
        });
      }
      // Live refresh of page 1 (no cursor, limit=50)
      return jsonResponse({
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        entries: [rowNew, rowP1Updated],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        nextCursor: 'cur-stale-page-2',
      });
    };

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;
    expect(root.querySelector('[data-answer-row="ans-p1"]')).not.toBeNull();

    // Start Load more (stays in flight)
    root.querySelector<HTMLElement>('[data-load-more="made"]')!.click();
    await settle(el, 10);

    // Fire decision-updated while Load more is in flight; wait past 60ms debounce
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: { data: { projectId: 'p-1', messageId: 'm-1' } },
      })
    );
    await settle(el, 85);

    // In-flight Load more was NOT aborted, and live refresh deferred until Load more finishes
    expect(loadMoreAborted).toBe(false);
    expect(madeCall).toBe(2);

    // Resolve Load more with page 2 + cur-page-3 -> deferred live refresh runs automatically
    resolveLoadMore(
      new Response(
        JSON.stringify({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          entries: [rowP2],
          agents: [{ id: 'a-rel', name: 'release-bot' }],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          nextCursor: 'cur-page-3',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await settle(el, 40);

    expect(madeCall).toBe(3);
    const liveMadeUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(liveMadeUrl).toBe('/api/v1/projects/p-1/decisions?tab=made&limit=50');

    // New entry prepended, existing entry updated, page-2 entry and cur-page-3 preserved!
    const renderedIds = Array.from(root.querySelectorAll('[data-answer-row]')).map((tr) =>
      tr.getAttribute('data-answer-row')
    );
    expect(renderedIds).toEqual(['ans-new', 'ans-p1', 'ans-p2']);
    expect(root.querySelector('[data-answer-row="ans-p1"]')?.textContent).toContain(
      'Page 1 updated'
    );
    expect(root.querySelector('[data-load-more="made"]')).not.toBeNull();
  });

  it('does not fetch Made while Pending tab is active, then prepends when switching back to Made', async () => {
    enableDecisionsFlag();
    const rowInitial = makeLogEntry({ answerId: 'ans-initial', label: 'Initial' });
    const rowAfterSse = makeLogEntry({ answerId: 'ans-after-sse', label: 'Added while hidden' });

    madeResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [rowInitial],
        agents: [],
        answerers: [],
      },
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [rowAfterSse, rowInitial],
        agents: [],
        answerers: [],
      },
    ];

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;
    expect(root.querySelector('[data-answer-row="ans-initial"]')).not.toBeNull();

    const madeCountBefore = requests.filter((r) => r.url.includes('tab=made')).length;
    expect(madeCountBefore).toBe(1);

    // Switch to Pending tab
    root.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
    await settle(el, 20);

    // Fire decision-updated while Made is hidden
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: { data: { projectId: 'p-1', messageId: 'm-1' } },
      })
    );
    await settle(el, 90);

    // Zero Made fetches while hidden!
    expect(requests.filter((r) => r.url.includes('tab=made')).length).toBe(madeCountBefore);

    // Switch back to Made -> triggers live prepend refresh
    root.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
    await settle(el, 35);

    expect(requests.filter((r) => r.url.includes('tab=made')).length).toBe(madeCountBefore + 1);
    const renderedIds = Array.from(root.querySelectorAll('[data-answer-row]')).map((tr) =>
      tr.getAttribute('data-answer-row')
    );
    expect(renderedIds).toEqual(['ans-after-sse', 'ans-initial']);
  });

  it('preserves Pending prompts beyond page 1 (>50) across live decision-updated without aborting Load more', async () => {
    enableDecisionsFlag();
    const page1Prompts = Array.from({ length: 50 }, (_, i) =>
      makePrompt({
        promptMessageId: `m-p1-${i}`,
        decisions: [makeDecision({ rowId: `r-p1-${i}`, messageId: `m-p1-${i}` })],
      })
    );
    const page2Prompts = Array.from({ length: 10 }, (_, i) =>
      makePrompt({
        promptMessageId: `m-p2-${i}`,
        decisions: [makeDecision({ rowId: `r-p2-${i}`, messageId: `m-p2-${i}` })],
      })
    );

    let loadMoreAborted = false;
    let resolveLoadMore!: (res: Response) => void;
    let pendingCall = 0;

    pendingHandler = (url, init): Promise<Response> => {
      pendingCall++;
      const cursor = url.searchParams.get('cursor');
      if (pendingCall === 1) {
        return jsonResponse({
          counts: { waitingOnYou: 60, waitingOnOthers: 0 },
          prompts: page1Prompts,
          agents: [],
          answerers: [],
          nextCursor: 'cur-pending-2',
        });
      }
      if (cursor === 'cur-pending-2') {
        init?.signal?.addEventListener('abort', () => {
          loadMoreAborted = true;
        });
        return new Promise<Response>((resolve) => {
          resolveLoadMore = resolve;
        });
      }
      // Deferred live refresh requests limit=60 and keeps all 60 prompts
      return jsonResponse({
        counts: { waitingOnYou: 60, waitingOnOthers: 0 },
        prompts: [...page1Prompts, ...page2Prompts],
        agents: [],
        answerers: [],
        nextCursor: 'cur-pending-3',
      });
    };

    const el = await mount();
    const root = el.shadowRoot!;
    expect(root.querySelectorAll('.pcard')).toHaveLength(50);

    // Click Load more on Pending (in flight)
    root.querySelector<HTMLElement>('[data-load-more="pending"]')!.click();
    await settle(el, 10);

    // Fire decision-updated while Pending Load more is in flight
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: { data: { projectId: 'p-1', messageId: 'm-other' } },
      })
    );
    await settle(el, 85);

    expect(loadMoreAborted).toBe(false);
    expect(pendingCall).toBe(2);

    resolveLoadMore(
      new Response(
        JSON.stringify({
          counts: { waitingOnYou: 60, waitingOnOthers: 0 },
          prompts: page2Prompts,
          agents: [],
          answerers: [],
          nextCursor: 'cur-pending-3',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await settle(el, 45);

    expect(pendingCall).toBe(3);
    const livePendingUrl = [...requests].reverse().find((r) => r.url.includes('tab=pending'))!.url;
    expect(livePendingUrl).toBe('/api/v1/projects/p-1/decisions?tab=pending&limit=60');
    expect(root.querySelectorAll('.pcard')).toHaveLength(60);
    expect(root.querySelector('[data-prompt-card="m-p2-9"]')).not.toBeNull();
  });

  it('converts Made From/To dates in effectiveTimeZone() and blocks From > To with inline validation', async () => {
    enableDecisionsFlag();
    setPreferredTimeZone('America/Los_Angeles');

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;
    const pRoot = panelRootOf(el);

    const fromInput = pRoot.querySelector<HTMLElement & { value?: string }>(
      '[data-filter="from"]'
    )!;
    const toInput = pRoot.querySelector<HTMLElement & { value?: string }>('[data-filter="to"]')!;

    fromInput.value = '2026-11-13';
    fromInput.dispatchEvent(new CustomEvent('sl-change'));
    await settle(el, 20);

    toInput.value = '2026-11-13';
    toInput.dispatchEvent(new CustomEvent('sl-change'));
    await settle(el, 20);

    const tzMadeUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(tzMadeUrl).toContain('from=2026-11-13T08%3A00%3A00.000Z');
    expect(tzMadeUrl).toContain('to=2026-11-14T07%3A59%3A59.999Z');
    expect(root.querySelector('[data-date-range-error]')).toBeNull();

    // Invalid range: From (2026-11-15) > To (2026-11-13) -> inline error, zero new requests
    const countBeforeInvalid = requests.filter((r) => r.url.includes('tab=made')).length;
    fromInput.value = '2026-11-15';
    fromInput.dispatchEvent(new CustomEvent('sl-change'));
    await settle(el, 25);

    expect(requests.filter((r) => r.url.includes('tab=made')).length).toBe(countBeforeInvalid);
    const rangeErr = root.querySelector('[data-date-range-error]');
    expect(rangeErr).not.toBeNull();
    expect(rangeErr?.textContent).toContain('Start date must be on or before end date.');

    // Clear filters recovers cleanly
    pRoot.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(el, 25);

    expect(root.querySelector('[data-date-range-error]')).toBeNull();
    const recoveredUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(recoveredUrl).toBe('/api/v1/projects/p-1/decisions?tab=made');
  });

  it('drives all Made UI filter controls into a combined query and resets all on Clear filters', async () => {
    enableDecisionsFlag();
    setPreferredTimeZone('UTC');

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const pRoot = panelRootOf(el);

    const setSelectOrDate = async (selector: string, val: string): Promise<void> => {
      const ctrl = pRoot.querySelector<HTMLElement & { value?: string }>(selector)!;
      ctrl.value = val;
      ctrl.dispatchEvent(new CustomEvent('sl-change'));
      await settle(el, 15);
    };

    const setCombobox = async (selector: string, val: string, label: string): Promise<void> => {
      const ctrl = pRoot.querySelector<HTMLElement>(selector)!;
      ctrl.dispatchEvent(
        new CustomEvent('facet-change', {
          detail: { value: val, label },
          bubbles: true,
          composed: true,
        })
      );
      await settle(el, 15);
    };

    await setCombobox('[data-filter="conversation"]', 'topic-release', '#release');
    await setCombobox('[data-filter="agent"]', 'a-rel', 'release-bot');
    await setSelectOrDate('[data-filter="user"]', 'u-bob');
    await setSelectOrDate('[data-filter="provenance"]', 'interpreted');
    await setSelectOrDate('[data-filter="from"]', '2026-11-01');
    await setSelectOrDate('[data-filter="to"]', '2026-11-13');

    const combinedUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    const parsed = new URL(combinedUrl, 'http://localhost');
    expect(parsed.searchParams.get('tab')).toBe('made');
    expect(parsed.searchParams.get('conversation')).toBe('topic-release');
    expect(parsed.searchParams.get('agent')).toBe('a-rel');
    expect(parsed.searchParams.get('user')).toBe('u-bob');
    expect(parsed.searchParams.get('provenance')).toBe('interpreted');
    expect(parsed.searchParams.get('from')).toBe('2026-11-01T00:00:00.000Z');
    expect(parsed.searchParams.get('to')).toBe('2026-11-13T23:59:59.999Z');

    // Click Clear in panel summary and verify all params are cleared
    pRoot.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(el, 25);

    const resetUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(resetUrl).toBe('/api/v1/projects/p-1/decisions?tab=made');
    expect(pRoot.querySelector('[data-clear-filters]')).toBeNull();
  });

  it('preserves the exact <scion-chat-decisions> DOM element for surviving cards via keyed repeat()', async () => {
    enableDecisionsFlag();
    const card1 = makePrompt({
      promptMessageId: 'p-1',
      decisions: [makeDecision({ rowId: 'r-card-1', messageId: 'p-1', question: 'First?' })],
    });
    const card2 = makePrompt({
      promptMessageId: 'p-2',
      decisions: [makeDecision({ rowId: 'r-card-2', messageId: 'p-2', question: 'Second?' })],
    });

    pendingResponses = [
      {
        counts: { waitingOnYou: 2, waitingOnOthers: 0 },
        prompts: [card1, card2],
        agents: [],
        answerers: [],
      },
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [card2],
        agents: [],
        answerers: [],
      },
    ];

    const el = await mount();
    const root = el.shadowRoot!;

    const p1ChoicesBefore = root.querySelector<ScionChatDecisions>(
      '[data-prompt-card="p-1"] scion-chat-decisions'
    )!;
    const p2ChoicesBefore = root.querySelector<ScionChatDecisions>(
      '[data-prompt-card="p-2"] scion-chat-decisions'
    )!;
    expect(p1ChoicesBefore).not.toBeNull();
    expect(p2ChoicesBefore).not.toBeNull();
    expect(p1ChoicesBefore).not.toBe(p2ChoicesBefore);

    // Settle p-1 so it leaves Waiting on you
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: {
          data: {
            projectId: 'p-1',
            messageId: 'p-1',
            decision: makeDecision({
              rowId: 'r-card-1',
              messageId: 'p-1',
              status: 'decided',
              decision: {
                choiceId: 'yes',
                label: 'Yes',
                answeredByUserId: 'u-me',
                answeredByName: 'Alice',
                provenance: 'direct',
                outcome: 'answer',
                surface: 'native',
                responseMessageId: 'resp-card-1',
                at: '2026-11-13T16:10:00Z',
              },
            }),
          },
        },
      })
    );
    await settle(el, 90);

    expect(root.querySelector('[data-prompt-card="p-1"]')).toBeNull();
    expect(p1ChoicesBefore.isConnected).toBe(false);
    const p2ChoicesAfter = root.querySelector<ScionChatDecisions>(
      '[data-prompt-card="p-2"] scion-chat-decisions'
    )!;
    expect(p2ChoicesAfter).toBe(p2ChoicesBefore);
  });

  it('refreshes on chat-message-received for decision-prompt only and dynamically toggles on FEATURE_FLAGS_CHANGED_EVENT', async () => {
    // Start with flags OFF -> 404, zero requests
    const el = await mount();
    const root = el.shadowRoot!;
    expect(root.querySelector('scion-page-404')).not.toBeNull();
    expect(requests).toEqual([]);

    // Dynamically enable both flags -> boots and loads Pending
    pendingResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt({ promptMessageId: 'm-init' })],
        agents: [],
        answerers: [],
      },
      {
        counts: { waitingOnYou: 2, waitingOnOthers: 0 },
        prompts: [
          makePrompt({ promptMessageId: 'm-new' }),
          makePrompt({ promptMessageId: 'm-init' }),
        ],
        agents: [],
        answerers: [],
      },
    ];

    window.__SCION_FEATURES__ = {
      [DECISIONS_FLAG]: true,
    };
    window.dispatchEvent(new Event(FEATURE_FLAGS_CHANGED_EVENT));
    await settle(el, 35);

    expect(root.querySelector('scion-page-404')).toBeNull();
    expect(root.querySelector('[data-prompt-card="m-init"]')).not.toBeNull();

    const pendingCountBefore = requests.filter((r) => r.url.includes('tab=pending')).length;

    // Ignore other-project decision-prompt and same-project plain message
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { projectId: 'other-proj', type: 'decision-prompt' } },
      })
    );
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { projectId: 'p-1', type: 'message' } },
      })
    );
    await settle(el, 90);
    expect(requests.filter((r) => r.url.includes('tab=pending')).length).toBe(pendingCountBefore);

    // Same-project decision-prompt triggers debounced refresh
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { projectId: 'p-1', type: 'decision-prompt' } },
      })
    );
    await settle(el, 90);
    expect(requests.filter((r) => r.url.includes('tab=pending')).length).toBe(
      pendingCountBefore + 1
    );
    expect(root.querySelector('[data-prompt-card="m-new"]')).not.toBeNull();
    expect(root.querySelector('[data-waiting-badge]')?.textContent?.trim()).toBe('2');

    // Dynamically disable flag -> tears down and renders 404
    setFeatureFlag(DECISIONS_FLAG, false);
    await settle(el, 20);
    expect(root.querySelector('scion-page-404')).not.toBeNull();
  });

  it('renders error states on initial 500 for Pending and Made tabs and recovers on Retry', async () => {
    enableDecisionsFlag();
    let failPending = true;
    pendingHandler = () => {
      if (failPending) {
        return jsonResponse({ error: { message: 'database unavailable' } }, 500);
      }
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt({ promptMessageId: 'm-recovered' })],
        agents: [],
        answerers: [],
      });
    };

    let failMade = true;
    madeHandler = () => {
      if (failMade) {
        return jsonResponse({ error: { message: 'audit log timeout' } }, 500);
      }
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [makeLogEntry({ answerId: 'ans-recovered' })],
        agents: [],
        answerers: [],
      });
    };

    const el = await mount();
    const root = el.shadowRoot!;
    expect(root.querySelector('.error-state h2')?.textContent).toBe('Failed to Load Decisions');
    expect(root.querySelector('.error-details')?.textContent).toContain('database unavailable');

    // Click Retry on Pending error state
    failPending = false;
    root.querySelector<HTMLElement>('[data-retry-pending]')!.click();
    await settle(el, 35);
    expect(root.querySelector('[data-prompt-card="m-recovered"]')).not.toBeNull();

    // Switch to Made -> initial 500 -> Retry -> recovers
    root.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
    await settle(el, 30);
    const madePanel = root.querySelector('sl-tab-panel[name="made"]')!;
    expect(madePanel.querySelector('.error-state h2')?.textContent).toBe(
      'Failed to Load Decision Log'
    );
    expect(madePanel.querySelector('.error-details')?.textContent).toContain('audit log timeout');

    failMade = false;
    madePanel.querySelector<HTMLElement>('[data-retry-made]')!.click();
    await settle(el, 30);
    expect(madePanel.querySelector('[data-answer-row="ans-recovered"]')).not.toBeNull();
  });

  it('renders message_denied and safe fallback Waiting on others badges, handles malformed URLs, and sets ARIA attributes', async () => {
    enableDecisionsFlag();
    pendingResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 2 },
        prompts: [
          makePrompt({ promptMessageId: 'm-self' }),
          makePrompt({
            promptMessageId: 'm-denied',
            waitingOnYou: false,
            canAnswer: false,
            waitingReasons: {
              'r-msg-denied': 'message_denied',
              'r-custom': 'unexpected_backend_code',
            },
            decisions: [
              makeDecision({
                rowId: 'r-msg-denied',
                messageId: 'm-denied',
                question: 'Restricted?',
              }),
              makeDecision({ rowId: 'r-custom', messageId: 'm-denied', question: 'Unknown?' }),
            ],
          }),
        ],
        agents: [],
        answerers: [],
        conversations: [],
      },
    ];

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;
    const panel = panelRootOf(el);

    // Avatar decorative initial is aria-hidden="true"
    const avatar = root.querySelector('[data-prompt-card="m-self"] .avatar')!;
    expect(avatar.getAttribute('aria-hidden')).toBe('true');

    // Waiting on others badges: message_denied -> "messaging restricted", unknown -> fixed literal
    expect(root.querySelector('[data-others-row="r-msg-denied"]')?.textContent).toContain(
      'messaging restricted'
    );
    const customRowText = root.querySelector('[data-others-row="r-custom"]')?.textContent ?? '';
    expect(customRowText).toContain('not answerable by you');
    expect(customRowText).not.toContain('unexpected_backend_code');

    // Collapsible filter panel toggle button exposes aria-expanded and aria-controls="decisions-filter-body"
    const filtersToggle = panel.querySelector<HTMLElement>('[data-filter-panel-toggle]')!;
    expect(filtersToggle.getAttribute('aria-controls')).toBe('decisions-filter-body');
    expect(filtersToggle.getAttribute('aria-expanded')).toBe('false');
    expect(panel.querySelector('#decisions-filter-body')).not.toBeNull();
    filtersToggle.click();
    await settle(el, 10);
    expect(filtersToggle.getAttribute('aria-expanded')).toBe('true');

    el.remove();

    // Malformed percent-encoded projectId in URL renders error state without throwing
    const badEl = await mount({ path: '/projects/%E0%A4%A/decisions' });
    expect(badEl.shadowRoot?.querySelector('.error-details')?.textContent).toBe(
      'Invalid project ID in URL'
    );
  });

  it('defers Made live refresh while a filter reset is in flight so old-filter rows never leak into new-filter results', async () => {
    enableDecisionsFlag();
    const rowOld = makeLogEntry({ answerId: 'ans-old', surface: 'other', label: 'Unfiltered row' });
    const rowFiltered = makeLogEntry({ answerId: 'ans-filtered', label: 'Filtered agent row' });
    const rowLiveNew = makeLogEntry({ answerId: 'ans-live-new', label: 'New matching row' });

    let madeCall = 0;
    let filterResetAborted = false;
    let resolveFilterReset!: (res: Response) => void;

    madeHandler = (url, init) => {
      madeCall++;
      const agent = url.searchParams.get('agent');
      const limit = url.searchParams.get('limit');
      if (madeCall === 1) {
        return jsonResponse({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          entries: [rowOld],
          agents: [
            { id: 'a-rel', name: 'release-bot' },
            { id: 'a-x', name: 'agent-x' },
          ],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          conversations: [],
        });
      }
      if (agent === 'a-x' && !limit) {
        init?.signal?.addEventListener('abort', () => {
          filterResetAborted = true;
        });
        return new Promise<Response>((resolve) => {
          resolveFilterReset = resolve;
        });
      }
      // Deferred live refresh after filter reset settles (agent=a-x&limit=50)
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [rowLiveNew, rowFiltered],
        agents: [
          { id: 'a-rel', name: 'release-bot' },
          { id: 'a-x', name: 'agent-x' },
        ],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [],
      });
    };

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;
    const panel = panelRootOf(el);
    expect(root.querySelector('[data-answer-row="ans-old"]')).not.toBeNull();
    // P2-N3: surface="other" maps cleanly to capitalized "Other"
    expect(
      root.querySelector('[data-answer-row="ans-old"] [data-surface]')?.textContent?.trim()
    ).toBe('Other');

    // Change Agent filter to a-x -> holds reset fetch open while madeEntries still holds ans-old
    const agentCombobox = panel.querySelector<HTMLElement>('[data-filter="agent"]')!;
    agentCombobox.dispatchEvent(
      new CustomEvent('facet-change', {
        detail: { value: 'a-x' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 10);

    // Fire decision-updated while filter reset is in flight; wait past 60ms debounce
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: { data: { projectId: 'p-1', messageId: 'm-1' } },
      })
    );
    await settle(el, 85);

    expect(filterResetAborted).toBe(false);
    expect(madeCall).toBe(2);

    // Resolve filter reset with ['ans-filtered'] -> deferred live refresh replays cleanly
    resolveFilterReset(
      new Response(
        JSON.stringify({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          entries: [rowFiltered],
          agents: [
            { id: 'a-rel', name: 'release-bot' },
            { id: 'a-x', name: 'agent-x' },
          ],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          conversations: [],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await settle(el, 45);

    expect(madeCall).toBe(3);
    const renderedIds = Array.from(root.querySelectorAll('[data-answer-row]')).map((tr) =>
      tr.getAttribute('data-answer-row')
    );
    expect(renderedIds).toEqual(['ans-live-new', 'ans-filtered']);
    expect(renderedIds).not.toContain('ans-old');
  });

  it('defers Pending live refresh during initial boot reset so boot errors are never swallowed', async () => {
    enableDecisionsFlag();
    let pendingCall = 0;
    let bootResetAborted = false;
    let resolveBootPending!: (res: Response) => void;

    pendingHandler = (_url, init) => {
      pendingCall++;
      if (pendingCall === 1) {
        init?.signal?.addEventListener('abort', () => {
          bootResetAborted = true;
        });
        return new Promise<Response>((resolve) => {
          resolveBootPending = resolve;
        });
      }
      return jsonResponse({
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [],
        answerers: [],
      });
    };

    const el = await mount();
    const root = el.shadowRoot!;

    // Fire decision-updated while initial Pending reset is still in flight
    stateManager.dispatchEvent(
      new CustomEvent('decision-updated', {
        detail: { data: { projectId: 'p-1', messageId: 'm-boot' } },
      })
    );
    await settle(el, 85);

    expect(bootResetAborted).toBe(false);
    expect(pendingCall).toBe(1);

    // Fail the initial Pending reset with 500 -> error state renders (and deferred live does not run)
    resolveBootPending(
      new Response(JSON.stringify({ error: { message: 'boot pending error' } }), {
        status: 500,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await settle(el, 40);

    expect(pendingCall).toBe(1);
    expect(root.querySelector('.error-state h2')?.textContent).toBe('Failed to Load Decisions');
    expect(root.querySelector('.error-details')?.textContent).toContain('boot pending error');
    expect(root.querySelector('[data-empty="pending"]')).toBeNull();
  });

  it('falls back to projectId title without entering error state when project summary JSON is null', async () => {
    enableDecisionsFlag();
    projectHandler = () =>
      new Promise<Response>((resolve) => {
        setTimeout(() => void jsonResponse(null).then(resolve), 5);
      });
    const titleEvents: string[][] = [];
    const onTitle = (e: Event): void => {
      const segs = (e as CustomEvent<PageTitleDetail>).detail?.segments;
      if (segs) titleEvents.push(segs);
    };
    document.addEventListener(PAGE_TITLE_EVENT, onTitle);
    try {
      const el = await mount();
      const root = el.shadowRoot!;

      expect(root.querySelector('.error-state')).toBeNull();
      expect(root.querySelector('[data-prompt-card="m-1"]')).not.toBeNull();
      expect(root.querySelector('.crumbs a[href="/projects/p-1"]')?.textContent?.trim()).toBe(
        'p-1'
      );
      expect(titleEvents).toContainEqual(['Decisions', 'p-1']);
    } finally {
      document.removeEventListener(PAGE_TITLE_EVENT, onTitle);
    }
  });

  it('skips redundant history.replaceState on active-tab clicks and synchronous sl-tab-show re-entry', async () => {
    enableDecisionsFlag();
    const el = await mount();
    const root = el.shadowRoot!;

    const group = root.querySelector('sl-tab-group') as HTMLElement & {
      show?: (panel: string) => void;
    };
    group.show = (panel: string): void => {
      group.dispatchEvent(new CustomEvent('sl-tab-show', { detail: { name: panel } }));
    };

    const replaceSpy = vi.spyOn(window.history, 'replaceState');
    try {
      replaceSpy.mockClear();

      // (a) Clicking the already-active tab does not call history.replaceState
      root.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
      await settle(el, 15);
      expect(replaceSpy).toHaveBeenCalledTimes(0);

      // (b) Switching tabs when group.show synchronously fires sl-tab-show calls replaceState once and fetches Made once
      root.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
      await settle(el, 25);
      expect(replaceSpy).toHaveBeenCalledTimes(1);
      expect(window.location.search).toBe('?tab=made');
      expect(requests.filter((r) => r.url.includes('tab=made'))).toHaveLength(1);
    } finally {
      replaceSpy.mockRestore();
    }
  });

  it('round-trips valid URL filters on load and silently drops invalid parameters via replaceState without pushState', async () => {
    enableDecisionsFlag();
    const agentUuid = '3f9c0a11-2222-4333-8444-555555555555';
    madeResponses = [
      {
        counts: { waitingOnYou: 2, waitingOnOthers: 1 },
        entries: [makeLogEntry()],
        agents: [{ id: agentUuid, name: 'release-bot', hint: 'deleted · 3f9c' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];

    const pushSpy = vi.spyOn(window.history, 'pushState');
    const replaceSpy = vi.spyOn(window.history, 'replaceState');
    try {
      // Valid Made URL state
      const validEl = await mount({
        path: `/projects/p-1/decisions?tab=made&q=backfill&conversation=topic-release&agent=${agentUuid}&user=u-me&provenance=direct&from=2026-11-01&to=2026-11-14`,
      });
      expect(pushSpy).not.toHaveBeenCalled();

      const madeUrl = new URL(
        requests.find((r) => r.url.includes('tab=made'))!.url,
        'http://localhost'
      );
      expect(madeUrl.searchParams.get('q')).toBe('backfill');
      expect(madeUrl.searchParams.get('conversation')).toBe('topic-release');
      expect(madeUrl.searchParams.get('agent')).toBe(agentUuid);
      expect(madeUrl.searchParams.get('user')).toBe('u-me');
      expect(madeUrl.searchParams.get('provenance')).toBe('direct');
      expect(madeUrl.searchParams.get('from')).toBe('2026-11-01T00:00:00.000Z');
      expect(madeUrl.searchParams.get('to')).toBe('2026-11-14T23:59:59.999Z');

      const panel = panelRootOf(validEl);
      expect(panel.querySelector('[data-active-filter-count]')?.textContent?.trim()).toBe(
        '7 active'
      );
      expect(panel.querySelector('[data-filter-tag="q"]')?.textContent?.trim()).toBe('"backfill"');
      expect(panel.querySelector('[data-filter-tag="from"]')?.textContent?.trim()).toContain(
        '2026-11-01'
      );
      validEl.remove();

      // Invalid URL state: 1-char q, non-UUID agent, bogus provenance, inverted from > to, Made-only user on pending
      requests = [];
      replaceSpy.mockClear();
      const invalidEl = await mount({
        path: '/projects/p-1/decisions?tab=bogus&q=a&agent=not-a-uuid&user=u-me&provenance=bogus&from=2026-11-20&to=2026-11-10',
      });
      expect(pushSpy).not.toHaveBeenCalled();
      expect(window.location.search).toBe('');
      const pendingUrl = new URL(
        requests.find((r) => r.url.includes('tab=pending'))!.url,
        'http://localhost'
      );
      expect(pendingUrl.searchParams.get('q')).toBeNull();
      expect(pendingUrl.searchParams.get('agent')).toBeNull();
      expect(pendingUrl.searchParams.get('user')).toBeNull();
      expect(pendingUrl.searchParams.get('provenance')).toBeNull();
      expect(invalidEl.shadowRoot?.querySelector('.error-state')).toBeNull();
    } finally {
      pushSpy.mockRestore();
      replaceSpy.mockRestore();
    }
  });

  it('debounces search q by 300ms, ignores 1-char typing, and aborts superseded in-flight search requests', async () => {
    enableDecisionsFlag();
    let callNum = 0;
    let firstSearchAborted = false;
    let resolveFirstSearch!: (res: Response) => void;

    pendingHandler = (url, init): Promise<Response> => {
      callNum++;
      const q = url.searchParams.get('q');
      if (callNum === 1) {
        return jsonResponse({
          counts: { waitingOnYou: 2, waitingOnOthers: 0 },
          prompts: [makePrompt({ promptMessageId: 'm-boot' })],
          agents: [{ id: 'a-rel', name: 'release-bot' }],
          answerers: [],
          conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
        });
      }
      if (q === 'ba') {
        init?.signal?.addEventListener('abort', () => {
          firstSearchAborted = true;
        });
        return new Promise<Response>((resolve) => {
          resolveFirstSearch = resolve;
        });
      }
      return jsonResponse({
        counts: { waitingOnYou: 2, waitingOnOthers: 0 },
        filteredCounts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt({ promptMessageId: 'm-backfill' })],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });
    };

    const el = await mount();
    const root = el.shadowRoot!;
    const panel = panelRootOf(el);
    const searchInput = panel.querySelector<HTMLElement & { value: string }>('[data-filter="q"]')!;

    // 1-char input does not trigger an invalid q=b request even after 300ms
    searchInput.value = 'b';
    searchInput.dispatchEvent(new CustomEvent('sl-input'));
    await settle(el, 330);
    expect(callNum).toBe(1);

    // Type "ba" -> wait 320ms -> starts first search request (held in flight)
    searchInput.value = 'ba';
    searchInput.dispatchEvent(new CustomEvent('sl-input'));
    await settle(el, 100);
    expect(callNum).toBe(1); // Not yet fired before 300ms
    await settle(el, 230);
    expect(callNum).toBe(2);
    expect(firstSearchAborted).toBe(false);

    // Type "backfill" while "ba" is in flight -> after 300ms, aborts "ba" and fires "backfill"
    searchInput.value = 'backfill';
    searchInput.dispatchEvent(new CustomEvent('sl-input'));
    await settle(el, 330);
    expect(firstSearchAborted).toBe(true);
    expect(callNum).toBe(3);
    expect(window.location.search).toBe('?q=backfill');

    // Late resolution of superseded "ba" response is ignored
    resolveFirstSearch(
      new Response(
        JSON.stringify({
          counts: { waitingOnYou: 2, waitingOnOthers: 0 },
          prompts: [makePrompt({ promptMessageId: 'm-stale' })],
          agents: [],
          answerers: [],
          conversations: [],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );
    await settle(el, 25);

    expect(root.querySelector('[data-prompt-card="m-backfill"]')).not.toBeNull();
    expect(root.querySelector('[data-prompt-card="m-stale"]')).toBeNull();
  });

  it('preserves shared filters across Pending <-> Made tab switches, drops Made-only filters on Pending, and clears all on Clear', async () => {
    enableDecisionsFlag();
    const agentUuid = '3f9c0a11-2222-4333-8444-555555555555';
    pendingResponses = [
      {
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        filteredCounts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt({ promptMessageId: 'm-filtered' })],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
      {
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        filteredCounts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt({ promptMessageId: 'm-filtered' })],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
      {
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        prompts: [makePrompt({ promptMessageId: 'm-unfiltered' })],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];
    madeResponses = [
      {
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        entries: [makeLogEntry()],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
      {
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        entries: [makeLogEntry()],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];

    const el = await mount({
      path: `/projects/p-1/decisions?q=backfill&conversation=topic-release&agent=${agentUuid}`,
    });
    const root = el.shadowRoot!;

    // Filtered Waiting on you badge shows "1 of 3" and Waiting on others shows "(0 of 2)" (D6, F-2b)
    expect(root.querySelector('sl-tab[panel="pending"] sl-badge')?.textContent?.trim()).toBe(
      '1 of 3'
    );
    expect(root.querySelector('[data-waiting-others]')?.getAttribute('summary')).toBe(
      'Waiting on others (0 of 2)'
    );

    // Switch to Made -> shared filters carry over into Made URL & API request
    root.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
    await settle(el, 25);
    expect(window.location.search).toBe(
      `?tab=made&q=backfill&conversation=topic-release&agent=${agentUuid}`
    );
    const madeReq = new URL(
      requests.find((r) => r.url.includes('tab=made'))!.url,
      'http://localhost'
    );
    expect(madeReq.searchParams.get('q')).toBe('backfill');
    expect(madeReq.searchParams.get('conversation')).toBe('topic-release');
    expect(madeReq.searchParams.get('agent')).toBe(agentUuid);

    // Apply a Made-only filter (provenance=direct)
    const madePanel = panelRootOf(el);
    const provSelect = madePanel.querySelector<HTMLElement & { value: string }>(
      '[data-filter="provenance"]'
    )!;
    provSelect.value = 'direct';
    provSelect.dispatchEvent(new CustomEvent('sl-change'));
    await settle(el, 25);
    expect(window.location.search).toContain('provenance=direct');

    // Switch back to Pending -> shared filters remain, Made-only provenance is dropped from URL
    root.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
    await settle(el, 25);
    expect(window.location.search).toBe(
      `?q=backfill&conversation=topic-release&agent=${agentUuid}`
    );

    // Click summary Clear -> clears all active shared filters and restores unfiltered badge "3"
    panelRootOf(el).querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(el, 25);
    expect(window.location.search).toBe('');
    expect(root.querySelector('sl-tab[panel="pending"] sl-badge')?.textContent?.trim()).toBe('3');
  });

  it('renders filtered "n of m" counts, distinct F-5 no-match state, D9 SEARCH_TIMEOUT alert, capability check, "/" shortcut, and C4 Decisions copy', async () => {
    enableDecisionsFlag();
    const agentUuid = '3f9c0a11-2222-4333-8444-555555555555';

    // 1. Pre-P1 Hub without `conversations` facet keeps panel mounted with supportsSearch=false,
    //    hiding search/Conversation/pending-Agent and stripping unsupported URL params on Pending
    pendingResponses = [
      {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
      },
    ];
    const oldHubEl = await mount({
      path: `/projects/p-1/decisions?q=backfill&conversation=topic-release&agent=${agentUuid}`,
    });
    const oldPendingPanelEl = oldHubEl.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    expect(oldPendingPanelEl).not.toBeNull();
    expect(oldPendingPanelEl?.supportsSearch).toBe(false);
    const oldPendingPanel = panelRootOf(oldHubEl);
    expect(oldPendingPanel.querySelector('[data-filter="q"]')).toBeNull();
    expect(oldPendingPanel.querySelector('[data-filter="conversation"]')).toBeNull();
    expect(oldPendingPanel.querySelector('[data-filter="agent"]')).toBeNull();
    expect(window.location.search).toBe('');
    expect(oldHubEl.shadowRoot?.querySelector('[data-empty="pending"]')).not.toBeNull();
    expect(oldHubEl.shadowRoot?.querySelector('[data-no-match="pending"]')).toBeNull();
    // C4 naming: "Decisions" (never "Choices")
    expect(oldHubEl.shadowRoot?.querySelector('.sub')?.textContent).toContain(
      "Decisions agents asked in this project's conversations"
    );
    expect(oldHubEl.shadowRoot?.textContent?.toLowerCase()).not.toContain('choices');
    oldHubEl.remove();

    // 2. P1+ Hub with F-5 no-match state (`[data-no-match="pending"]`), "0 of 3" badge, and "Waiting on others (0 of 2)"
    pendingHandler = (url): Promise<Response> => {
      const q = url.searchParams.get('q');
      if (q === 'timeout-query') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              error: { code: 'SEARCH_TIMEOUT', message: 'search query timed out' },
            }),
            { status: 503, headers: { 'Content-Type': 'application/json' } }
          )
        );
      }
      if (q === 'nomatch') {
        return jsonResponse({
          counts: { waitingOnYou: 3, waitingOnOthers: 2 },
          filteredCounts: { waitingOnYou: 0, waitingOnOthers: 0 },
          prompts: [],
          agents: [{ id: agentUuid, name: 'release-bot' }],
          answerers: [],
          conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
        });
      }
      return jsonResponse({
        counts: { waitingOnYou: 3, waitingOnOthers: 2 },
        prompts: [
          makePrompt({ promptMessageId: 'm-1' }),
          makePrompt({
            promptMessageId: 'm-others',
            waitingOnYou: false,
            canAnswer: false,
            decisions: [makeDecision({ rowId: 'r-o1', messageId: 'm-others' })],
          }),
        ],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });
    };

    const el = await mount({ path: '/projects/p-1/decisions?q=nomatch' });
    const root = el.shadowRoot!;
    const panel = panelRootOf(el);

    expect(root.querySelector('sl-tab[panel="pending"] sl-badge')?.textContent?.trim()).toBe(
      '0 of 3'
    );
    expect(root.querySelector('[data-no-match="pending"]')?.textContent).toContain(
      'No decisions match these filters'
    );
    expect(root.querySelector('[data-empty="pending"]')).toBeNull();

    // Pressing "/" on the page expands the collapsed filter panel (and is ignored when focus is in an input)
    expect(panel.querySelector('[data-filter-panel-toggle]')?.getAttribute('aria-expanded')).toBe(
      'false'
    );
    const dummyInput = document.createElement('input');
    document.body.appendChild(dummyInput);
    dummyInput.focus();
    dummyInput.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true }));
    await settle(el, 10);
    expect(panel.querySelector('[data-filter-panel-toggle]')?.getAttribute('aria-expanded')).toBe(
      'false'
    );
    dummyInput.remove();

    window.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true }));
    await settle(el, 15);
    expect(panel.querySelector('[data-filter-panel-toggle]')?.getAttribute('aria-expanded')).toBe(
      'true'
    );

    // 3. D9: HTTP 503 SEARCH_TIMEOUT renders warning alert while keeping filter panel mounted
    const searchInput = panel.querySelector<HTMLElement & { value: string }>('[data-filter="q"]')!;
    searchInput.value = 'timeout-query';
    searchInput.dispatchEvent(new CustomEvent('sl-input'));
    await settle(el, 330);

    const timeoutAlert = root.querySelector('[data-search-timeout]');
    expect(timeoutAlert).not.toBeNull();
    expect(timeoutAlert?.textContent).toContain('Search took too long; narrow it');
    expect(root.querySelector('scion-decisions-filter-panel')).not.toBeNull();
    expect(root.querySelector('.error-state')).toBeNull();

    // Recover by clicking Clear on the filter panel -> unfiltered prompts return and "Waiting on others (2)" renders
    panel.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(el, 25);
    expect(root.querySelector('[data-search-timeout]')).toBeNull();
    expect(root.querySelector('[data-prompt-card="m-1"]')).not.toBeNull();
    expect(root.querySelector('[data-waiting-others]')?.getAttribute('summary')).toBe(
      'Waiting on others (2)'
    );
  });

  it('keeps Made Phase 3 filters, tags, and Clear working against an older Hub without conversations facet', async () => {
    enableDecisionsFlag();
    const agentUuid = '3f9c0a11-2222-4333-8444-555555555555';

    pendingHandler = (): Promise<Response> =>
      jsonResponse({
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
      });
    madeHandler = (url): Promise<Response> => {
      const prov = url.searchParams.get('provenance');
      return jsonResponse({
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        entries: prov === 'direct' ? [makeLogEntry({ provenance: 'direct' })] : [makeLogEntry()],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
      });
    };

    const el = await mount({
      path: `/projects/p-1/decisions?tab=made&q=backfill&conversation=topic-release&agent=${agentUuid}&user=u-me&provenance=direct&from=2026-11-01&to=2026-11-14`,
    });
    const panelEl = el.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    expect(panelEl).not.toBeNull();
    expect(panelEl?.supportsSearch).toBe(false);

    // Unsupported `q` and `conversation` are stripped, while Phase 3 Made filters stay applied & visible
    expect(window.location.search).toBe(
      `?tab=made&agent=${agentUuid}&user=u-me&provenance=direct&from=2026-11-01&to=2026-11-14`
    );

    const panel = panelRootOf(el);
    expect(panel.querySelector('[data-filter="q"]')).toBeNull();
    expect(panel.querySelector('[data-filter="conversation"]')).toBeNull();
    expect(panel.querySelector('[data-filter="agent"]')).not.toBeNull();
    expect(panel.querySelector('[data-filter="user"]')).not.toBeNull();
    expect(panel.querySelector('[data-filter="provenance"]')).not.toBeNull();
    expect(panel.querySelector('[data-filter="from"]')).not.toBeNull();
    expect(panel.querySelector('[data-filter="to"]')).not.toBeNull();

    const tags = Array.from(panel.querySelectorAll<HTMLElement>('sl-tag[data-filter-tag]'));
    expect(tags.map((t) => t.textContent?.trim())).toEqual([
      'Agent: release-bot',
      'Answered by: you',
      'Provenance: direct',
      'From: 2026-11-01',
      'To: 2026-11-14',
    ]);
    expect(panel.querySelector('[data-active-filter-count]')?.textContent).toBe('5 active');

    // Clear resets all Made filters and keeps ?tab=made clean
    panel.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(el, 25);
    expect(window.location.search).toBe('?tab=made');
    expect(panel.querySelectorAll('sl-tag[data-filter-tag]')).toHaveLength(0);
    expect(el.shadowRoot?.querySelector('[data-made-table]')).not.toBeNull();
  });

  it('renders filter panel, tags, Clear, and "/" shortcut on a first-load SEARCH_TIMEOUT on both Pending and Made tabs (D9, D10)', async () => {
    enableDecisionsFlag();
    const timeoutResponse = (): Promise<Response> =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            error: { code: 'SEARCH_TIMEOUT', message: 'search query timed out' },
          }),
          { status: 503, headers: { 'Content-Type': 'application/json' } }
        )
      );

    pendingHandler = (url): Promise<Response> => {
      if (url.searchParams.get('q') === 'slowsearch') {
        return timeoutResponse();
      }
      // Mirror Design 13.3: limit=0 returns conversations: [] so facet hydration must use limit=1
      if (url.searchParams.get('limit') === '0') {
        return jsonResponse({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          prompts: [],
          agents: [],
          answerers: [],
          conversations: [],
        });
      }
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt({ promptMessageId: 'm-recovered' })],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });
    };
    madeHandler = (url): Promise<Response> => {
      if (url.searchParams.get('q') === 'slowsearch') {
        return timeoutResponse();
      }
      if (url.searchParams.get('limit') === '0') {
        return jsonResponse({
          counts: { waitingOnYou: 1, waitingOnOthers: 0 },
          entries: [],
          agents: [],
          answerers: [],
          conversations: [],
        });
      }
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [makeLogEntry({ answerId: 'ans-recovered' })],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });
    };

    // 1. First-load SEARCH_TIMEOUT on Waiting on you (`/projects/p-1/decisions?q=slowsearch&conversation=topic-release`)
    const pendingEl = await mount({
      path: '/projects/p-1/decisions?q=slowsearch&conversation=topic-release',
    });
    const pendingRoot = pendingEl.shadowRoot!;
    const pendingPanelEl = pendingRoot.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    expect(pendingRoot.querySelector('[data-search-timeout]')).not.toBeNull();
    expect(pendingRoot.querySelector('.error-state')).toBeNull();
    expect(pendingPanelEl).not.toBeNull();
    expect(pendingPanelEl?.supportsSearch).toBe(true);

    // Fallback facet request ran with limit=1 and without q, populating Conversation/Agent pickers
    // and resolving the raw `topic-release` key tag to `#release` while keeping the timeout alert
    expect(
      requests.some(
        (r) => r.url.includes('tab=pending') && r.url.includes('limit=1') && !r.url.includes('q=')
      )
    ).toBe(true);

    const pendingPanel = panelRootOf(pendingEl);
    expect(pendingPanel.querySelector('sl-tag[data-filter-tag="q"]')?.textContent?.trim()).toBe(
      '"slowsearch"'
    );
    expect(
      pendingPanel.querySelector('sl-tag[data-filter-tag="conversation"]')?.textContent?.trim()
    ).toBe('Conversation: #release');
    expect(
      pendingPanel.querySelector<HTMLElement & { options?: unknown[] }>(
        '[data-filter="conversation"]'
      )?.options
    ).toEqual([{ value: 'topic-release', label: '#release' }]);
    expect(
      pendingPanel.querySelector<HTMLElement & { options?: unknown[] }>('[data-filter="agent"]')
        ?.options
    ).toEqual([{ value: 'a-rel', label: 'release-bot' }]);
    expect(pendingPanel.querySelector('[data-clear-filters]')).not.toBeNull();

    // "/" shortcut expands panel and exposes search input even on first-load timeout
    window.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true }));
    await settle(pendingEl, 15);
    expect(
      pendingPanel.querySelector('[data-filter-panel-toggle]')?.getAttribute('aria-expanded')
    ).toBe('true');
    expect(pendingPanel.querySelector('[data-filter="q"]')).not.toBeNull();

    // Clear recovers from first-load timeout on Pending
    pendingPanel.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(pendingEl, 25);
    expect(window.location.search).toBe('');
    expect(pendingRoot.querySelector('[data-search-timeout]')).toBeNull();
    expect(pendingRoot.querySelector('[data-prompt-card="m-recovered"]')).not.toBeNull();
    pendingEl.remove();
    window.localStorage.removeItem('scion:decisions:filters-open');

    // 2. First-load SEARCH_TIMEOUT on Made (`/projects/p-1/decisions?tab=made&q=slowsearch&conversation=topic-release`)
    requests = [];
    const madeEl = await mount({
      path: '/projects/p-1/decisions?tab=made&q=slowsearch&conversation=topic-release',
    });
    const madeRoot = madeEl.shadowRoot!;
    const madePanelEl = madeRoot.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    expect(
      madeRoot.querySelector('sl-tab-panel[name="made"] [data-search-timeout]')
    ).not.toBeNull();
    expect(madeRoot.querySelector('.error-state')).toBeNull();
    expect(madePanelEl).not.toBeNull();
    expect(madePanelEl?.supportsSearch).toBe(true);
    expect(
      requests.some(
        (r) => r.url.includes('tab=made') && r.url.includes('limit=1') && !r.url.includes('q=')
      )
    ).toBe(true);

    const madePanel = panelRootOf(madeEl);
    expect(madePanel.querySelector('sl-tag[data-filter-tag="q"]')?.textContent?.trim()).toBe(
      '"slowsearch"'
    );
    expect(
      madePanel.querySelector('sl-tag[data-filter-tag="conversation"]')?.textContent?.trim()
    ).toBe('Conversation: #release');
    expect(
      madePanel.querySelector<HTMLElement & { options?: unknown[] }>('[data-filter="conversation"]')
        ?.options
    ).toEqual([{ value: 'topic-release', label: '#release' }]);
    expect(
      madePanel.querySelector<HTMLElement & { options?: unknown[] }>('[data-filter="agent"]')
        ?.options
    ).toEqual([{ value: 'a-rel', label: 'release-bot' }]);
    expect(madePanel.querySelector('[data-clear-filters]')).not.toBeNull();

    // "/" shortcut works on Made first-load timeout too
    window.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true }));
    await settle(madeEl, 15);
    expect(
      madePanel.querySelector('[data-filter-panel-toggle]')?.getAttribute('aria-expanded')
    ).toBe('true');

    // Clear recovers from first-load timeout on Made (and clears stale timeout on inactive Pending tab)
    madePanel.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(madeEl, 25);
    expect(window.location.search).toBe('?tab=made');
    expect(madeRoot.querySelector('[data-search-timeout]')).toBeNull();
    expect(madeRoot.querySelector('[data-made-table]')).not.toBeNull();

    // Switching to Pending reloads Pending cleanly after the boot timeout
    madeRoot.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
    await settle(madeEl, 25);
    expect(madeRoot.querySelector('[data-prompt-card="m-recovered"]')).not.toBeNull();
  });

  it('aborts superseded in-flight Made and Pending loads across tab round-trips and re-issues fresh requests (D10, AC 4)', async () => {
    enableDecisionsFlag();
    const agentUuid = '3f9c0a11-2222-4333-8444-555555555555';

    // 1. Reviewer pass-2 sequence (plus shared-filter variant):
    //    Made request in flight -> switch to Pending -> change shared filter -> switch back to Made
    let releaseStaleProvMade: (() => void) | null = null;
    let staleProvAborted = false;
    let releaseStaleAgentMade: (() => void) | null = null;
    let staleAgentAborted = false;

    pendingHandler = (url): Promise<Response> =>
      jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        filteredCounts: url.searchParams.get('q')
          ? { waitingOnYou: 1, waitingOnOthers: 0 }
          : undefined,
        prompts: [makePrompt({ promptMessageId: 'm-pending' })],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });

    madeHandler = (url, init): Promise<Response> => {
      const prov = url.searchParams.get('provenance');
      const q = url.searchParams.get('q');
      const agent = url.searchParams.get('agent');

      if (prov === 'direct' && !q) {
        init?.signal?.addEventListener('abort', () => {
          staleProvAborted = true;
        });
        return new Promise<Response>((resolve) => {
          releaseStaleProvMade = (): void => {
            void jsonResponse({
              counts: { waitingOnYou: 1, waitingOnOthers: 0 },
              entries: [
                makeLogEntry({
                  answerId: 'ans-stale-prov',
                  decision: {
                    ...makeLogEntry().decision,
                    key: 'STALE_PROV',
                    ref: 'STALE_PROV',
                  },
                }),
              ],
              agents: [{ id: agentUuid, name: 'release-bot' }],
              answerers: [{ id: 'u-me', name: 'Alice' }],
              conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
            }).then(resolve);
          };
        });
      }

      if (agent === agentUuid && !q) {
        init?.signal?.addEventListener('abort', () => {
          staleAgentAborted = true;
        });
        return new Promise<Response>((resolve) => {
          releaseStaleAgentMade = (): void => {
            void jsonResponse({
              counts: { waitingOnYou: 1, waitingOnOthers: 0 },
              entries: [
                makeLogEntry({
                  answerId: 'ans-stale-agent',
                  decision: {
                    ...makeLogEntry().decision,
                    key: 'STALE_AGENT',
                    ref: 'STALE_AGENT',
                  },
                }),
              ],
              agents: [{ id: agentUuid, name: 'release-bot' }],
              answerers: [{ id: 'u-me', name: 'Alice' }],
              conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
            }).then(resolve);
          };
        });
      }

      const key = q === 'backfill' ? 'FRESH_BACKFILL' : 'INITIAL_MADE';
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [
          makeLogEntry({
            answerId: `ans-${key.toLowerCase()}`,
            decision: {
              ...makeLogEntry().decision,
              key,
              ref: key,
            },
          }),
        ],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });
    };

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;

    // 1a. Made-only filter (`provenance=direct`) in flight -> Pending -> `q=backfill` -> Made
    el.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { provenance: 'direct' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 5);
    expect(releaseStaleProvMade).not.toBeNull();

    root.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
    await settle(el, 10);
    expect(staleProvAborted).toBe(true);

    el.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { q: 'backfill' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 15);

    root.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
    releaseStaleProvMade!();
    await settle(el, 25);

    expect(window.location.search).toBe('?tab=made&q=backfill');
    expect(requests.some((r) => r.url.includes('tab=made') && r.url.includes('q=backfill'))).toBe(
      true
    );
    expect(root.querySelector('[data-made-table]')?.textContent).toContain('FRESH_BACKFILL');
    expect(root.querySelector('[data-made-table]')?.textContent).not.toContain('STALE_PROV');

    // 1b. Shared filter (`agent`) in flight on Made -> Pending -> change `q=backfill` -> Made
    el.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filters-clear', { bubbles: true, composed: true })
    );
    await settle(el, 20);

    el.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { agent: agentUuid },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 5);
    expect(releaseStaleAgentMade).not.toBeNull();
    expect(staleAgentAborted).toBe(false);

    // Switch to Pending (agent is shared so Made load stays in flight until shared filter changes on Pending)
    root.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
    await settle(el, 10);

    el.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { q: 'backfill' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 10);
    expect(staleAgentAborted).toBe(true);

    root.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
    releaseStaleAgentMade!();
    await settle(el, 25);

    expect(window.location.search).toBe(`?tab=made&q=backfill&agent=${agentUuid}`);
    expect(root.querySelector('[data-made-table]')?.textContent).toContain('FRESH_BACKFILL');
    expect(root.querySelector('[data-made-table]')?.textContent).not.toContain('STALE_AGENT');
    el.remove();

    // 2. Mirror sequence: Pending request in flight -> switch to Made -> change shared filter -> switch back to Pending
    let releaseStalePending: (() => void) | null = null;
    let stalePendingAborted = false;

    pendingHandler = (url, init): Promise<Response> => {
      const q = url.searchParams.get('q');
      if (q === 'slowfirst') {
        init?.signal?.addEventListener('abort', () => {
          stalePendingAborted = true;
        });
        return new Promise<Response>((resolve) => {
          releaseStalePending = (): void => {
            void jsonResponse({
              counts: { waitingOnYou: 1, waitingOnOthers: 0 },
              filteredCounts: { waitingOnYou: 1, waitingOnOthers: 0 },
              prompts: [makePrompt({ promptMessageId: 'm-stale-pending' })],
              agents: [{ id: agentUuid, name: 'release-bot' }],
              answerers: [{ id: 'u-me', name: 'Alice' }],
              conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
            }).then(resolve);
          };
        });
      }
      const promptId = q === 'freshsecond' ? 'm-fresh-pending' : 'm-initial-pending';
      return jsonResponse({
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        filteredCounts: q ? { waitingOnYou: 1, waitingOnOthers: 0 } : undefined,
        prompts: [makePrompt({ promptMessageId: promptId })],
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });
    };

    window.history.replaceState({}, '', '/projects/p-1/decisions');
    const el2 = await mount({ path: '/projects/p-1/decisions' });
    const root2 = el2.shadowRoot!;

    el2.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { q: 'slowfirst' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el2, 5);
    expect(releaseStalePending).not.toBeNull();
    expect(stalePendingAborted).toBe(false);

    // Switch to Made while Pending `q=slowfirst` is still in flight
    root2.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
    await settle(el2, 15);

    // Change shared filter `q=freshsecond` on Made -> aborts in-flight Pending load
    el2.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { q: 'freshsecond' },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el2, 15);
    expect(stalePendingAborted).toBe(true);

    // Switch back to Pending -> issues fresh `q=freshsecond` Pending load; stale response is ignored
    root2.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
    releaseStalePending!();
    await settle(el2, 25);

    expect(window.location.search).toBe('?q=freshsecond');
    expect(
      requests.some((r) => r.url.includes('tab=pending') && r.url.includes('q=freshsecond'))
    ).toBe(true);
    expect(root2.querySelector('[data-prompt-card="m-fresh-pending"]')).not.toBeNull();
    expect(root2.querySelector('[data-prompt-card="m-stale-pending"]')).toBeNull();
  });

  it('does not merge old-filter >200 Pending tail rows on live SSE refresh when pendingNeedsReset is set', async () => {
    enableDecisionsFlag();
    const agentUuid = '3f9c0a11-2222-4333-8444-555555555555';
    const page1Prompts = Array.from({ length: 200 }, (_, i) =>
      makePrompt({
        promptMessageId: `m-old-${i}`,
        decisions: [makeDecision({ rowId: `r-old-${i}`, messageId: `m-old-${i}` })],
      })
    );
    const page2Prompts = Array.from({ length: 5 }, (_, i) =>
      makePrompt({
        promptMessageId: `m-old-tail-${i}`,
        decisions: [makeDecision({ rowId: `r-old-tail-${i}`, messageId: `m-old-tail-${i}` })],
      })
    );

    pendingHandler = (url): Promise<Response> => {
      if (url.searchParams.get('agent') === agentUuid) {
        return jsonResponse({
          counts: { waitingOnYou: 205, waitingOnOthers: 0 },
          filteredCounts: { waitingOnYou: 1, waitingOnOthers: 0 },
          prompts: [
            makePrompt({
              promptMessageId: 'm-agent-only',
              askerAgentId: agentUuid,
              decisions: [makeDecision({ rowId: 'r-agent-only', messageId: 'm-agent-only' })],
            }),
          ],
          agents: [{ id: agentUuid, name: 'release-bot' }],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
        });
      }
      if (url.searchParams.get('cursor') === 'cur-p2') {
        return jsonResponse({
          counts: { waitingOnYou: 205, waitingOnOthers: 0 },
          prompts: page2Prompts,
          agents: [{ id: agentUuid, name: 'release-bot' }],
          answerers: [{ id: 'u-me', name: 'Alice' }],
          conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
        });
      }
      return jsonResponse({
        counts: { waitingOnYou: 205, waitingOnOthers: 0 },
        prompts: page1Prompts,
        nextCursor: 'cur-p2',
        agents: [{ id: agentUuid, name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      });
    };

    const el = await mount({ path: '/projects/p-1/decisions' });
    const root = el.shadowRoot!;

    // Load page 2 so >200 unfiltered prompts (including `m-old-tail-0`) are in memory
    root.querySelector<HTMLElement>('[data-load-more="pending"]')!.click();
    await settle(el, 25);
    expect(root.querySelector('[data-prompt-card="m-old-tail-0"]')).not.toBeNull();

    // Switch to Made and apply shared `agent` filter -> marks Pending as pendingNeedsReset
    root.querySelector<HTMLElement>('sl-tab[panel="made"]')!.click();
    await settle(el, 20);
    el.shadowRoot!.querySelector('scion-decisions-filter-panel')!.dispatchEvent(
      new CustomEvent('decisions-filter-change', {
        detail: { agent: agentUuid },
        bubbles: true,
        composed: true,
      })
    );
    await settle(el, 20);

    // Fire an SSE decision-prompt while on Made -> scheduleLiveRefresh must NOT run a `live` merge on Pending
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { projectId: 'p-1', type: 'decision-prompt' } },
      })
    );
    await settle(el, 90);

    // Switch back to Pending -> full reset runs for `agent=${agentUuid}`; old >200 tail row never leaks
    root.querySelector<HTMLElement>('sl-tab[panel="pending"]')!.click();
    await settle(el, 25);

    expect(root.querySelector('[data-prompt-card="m-agent-only"]')).not.toBeNull();
    expect(root.querySelector('[data-prompt-card="m-old-tail-0"]')).toBeNull();
  });

  it('disables Dismiss… with countdown tooltip before 5 minutes and unlocks at the boundary (ptone/scion#8213)', async () => {
    enableDecisionsFlag();
    const createdAt = new Date(Date.now() - (5 * 60_000 - 1200)).toISOString();
    pendingResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt({ createdAt })],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];

    const el = await mount({ path: '/projects/p-1/decisions' });
    const root = el.shadowRoot!;

    const disabledBtn = root.querySelector<HTMLElement>('[data-dismiss-prompt="m-1"]')!;
    expect(disabledBtn).not.toBeNull();
    expect(disabledBtn.hasAttribute('disabled')).toBe(true);
    const tip = root.querySelector<HTMLElement>('[data-dismiss-tooltip]')!;
    expect(tip?.getAttribute('content')).toBe('Dismiss available in 1 min');

    // Wait past the 1.2s remaining age so the controller's boundary timer fires automatically.
    await settle(el, 1350);

    const enabledBtn = root.querySelector<HTMLElement>('[data-dismiss-prompt="m-1"]')!;
    expect(enabledBtn.hasAttribute('disabled')).toBe(false);
    expect(root.querySelector('[data-dismiss-tooltip]')).toBeNull();
  });

  it('dismisses a multi-row set from the confirm dialog with reason and removes the card from Waiting on you (ptone/scion#8213)', async () => {
    enableDecisionsFlag();
    const d1 = makeDecision({ rowId: 'r-1', key: 'D234', ref: 'D234', question: 'Ship v0.9?' });
    const d2 = makeDecision({
      rowId: 'r-2',
      key: '17',
      ref: '17',
      question: 'Which approach?',
    });
    const setPrompt = makePrompt({
      promptMessageId: 'm-set',
      answerMode: 'set',
      createdAt: '2026-11-13T10:00:00Z',
      decisions: [d1, d2],
    });
    pendingResponses = [
      {
        counts: { waitingOnYou: 2, waitingOnOthers: 0 },
        prompts: [setPrompt],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
      {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];
    agentHandler = () => jsonResponse({ id: 'a-rel', name: 'release-bot' });
    answerResponseFn = (url) => {
      if (url.pathname === '/api/v1/decision-prompts/m-set/dismiss') {
        const dismissedMeta = {
          at: '2026-11-13T10:12:00Z',
          byUserId: 'u-me',
          byName: 'Alice',
          reason: 'Stale: already shipped.',
          surface: 'decisions_page',
          responseMessageId: 'resp-dsm',
        };
        return jsonResponse({
          dismissed: [
            { ...d1, status: 'dismissed', dismissed: dismissedMeta },
            { ...d2, status: 'dismissed', dismissed: dismissedMeta },
          ],
          alreadyDismissed: [],
          responseMessageId: 'resp-dsm',
          delivered: true,
        });
      }
      return jsonResponse({}, 404);
    };

    const el = await mount({ path: '/projects/p-1/decisions' });
    const root = el.shadowRoot!;
    expect(root.querySelector('[data-waiting-badge]')?.textContent?.trim()).toBe('2');

    root.querySelector<HTMLElement>('[data-dismiss-prompt="m-set"]')!.click();
    await settle(el, 20);

    const dialog = root.querySelector<HTMLElement>('sl-dialog[data-dismiss-dialog]')!;
    expect(dialog).not.toBeNull();
    expect(dialog.getAttribute('label')).toBe('Dismiss decisions');
    expect(dialog.querySelector('[data-dismiss-tell="live"]')?.textContent).toContain(
      'release-bot will be told in #release.'
    );
    const confirmBtn = dialog.querySelector<HTMLElement>('[data-dismiss-confirm]')!;
    expect(confirmBtn.textContent?.trim()).toBe('Dismiss 2 decisions');

    const reasonInput = dialog.querySelector<HTMLElement & { value?: string }>(
      '[data-dismiss-reason]'
    )!;
    reasonInput.value = '  Stale: already shipped.  ';
    reasonInput.dispatchEvent(new Event('sl-input', { bubbles: true, composed: true }));
    await settle(el, 10);

    confirmBtn.click();
    await settle(el, 90);

    const dismissReq = requests.find((r) => r.url === '/api/v1/decision-prompts/m-set/dismiss');
    expect(dismissReq).toEqual({
      method: 'POST',
      url: '/api/v1/decision-prompts/m-set/dismiss',
      body: { reason: 'Stale: already shipped.', surface: 'decisions_page' },
    });
    expect(root.querySelector('sl-dialog[data-dismiss-dialog]')).toBeNull();
    expect(root.querySelector('[data-prompt-card="m-set"]')).toBeNull();
    expect(root.querySelector('[data-waiting-badge]')).toBeNull();
    expect(root.querySelector('[data-empty="pending"]')).not.toBeNull();
    expect(showToast).toHaveBeenCalledWith('Dismissed 2 decisions.', 'success');
  });

  it('shows gone-agent notice in the dismiss dialog when the asking agent returns 404 (ptone/scion#8213)', async () => {
    enableDecisionsFlag();
    const d1 = makeDecision({
      rowId: 'r-gone',
      key: 'migrate',
      ref: 'migrate',
      askerAgentId: 'a-gone',
      askerAgentName: 'nc-x',
      question: 'Run the backfill on staging first?',
    });
    pendingResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [
          makePrompt({
            promptMessageId: 'm-gone',
            askerAgentId: 'a-gone',
            askerAgentName: 'nc-x',
            createdAt: '2026-11-13T10:00:00Z',
            decisions: [d1],
          }),
        ],
        agents: [{ id: 'a-gone', name: 'nc-x' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
      {
        counts: { waitingOnYou: 0, waitingOnOthers: 0 },
        prompts: [],
        agents: [{ id: 'a-gone', name: 'nc-x' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
        conversations: [{ key: 'topic-release', label: '#release', projectId: 'p-1' }],
      },
    ];
    agentHandler = () => jsonResponse({ error: { code: 'not_found' } }, 404);
    answerResponseFn = (url) => {
      if (url.pathname === '/api/v1/decision-prompts/m-gone/dismiss') {
        return jsonResponse({
          dismissed: [
            {
              ...d1,
              status: 'dismissed',
              dismissed: {
                at: '2026-11-13T10:15:00Z',
                byUserId: 'u-me',
                byName: 'Alice',
                surface: 'decisions_page',
                responseMessageId: 'resp-gone',
              },
            },
          ],
          alreadyDismissed: [],
          responseMessageId: 'resp-gone',
          delivered: false,
        });
      }
      return jsonResponse({}, 404);
    };

    const el = await mount({ path: '/projects/p-1/decisions' });
    const root = el.shadowRoot!;

    root.querySelector<HTMLElement>('[data-dismiss-prompt="m-gone"]')!.click();
    await settle(el, 25);

    const dialog = root.querySelector<HTMLElement>('sl-dialog[data-dismiss-dialog]')!;
    expect(dialog.querySelector('[data-dismiss-tell="gone"]')?.textContent).toContain(
      'nc-x is gone; nothing will be sent.'
    );

    dialog.querySelector<HTMLElement>('[data-dismiss-confirm]')!.click();
    await settle(el, 90);

    expect(root.querySelector('[data-prompt-card="m-gone"]')).toBeNull();
    expect(showToast).toHaveBeenCalledWith('nc-x is gone; nothing was sent.', 'neutral');
  });
});
