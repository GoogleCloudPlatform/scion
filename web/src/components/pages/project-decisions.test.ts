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
import { stateManager } from '../../client/state.js';
import {
  CHAT_CHOICES_FLAG,
  FEATURE_FLAGS_CHANGED_EVENT,
  PROJECT_DECISIONS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { setPreferredTimeZone } from '../../utils/time.js';

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
type ScionChatChoices = import('../shared/chat/chat-choices.js').ScionChatChoices;

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
  for (const card of Array.from(
    el.shadowRoot?.querySelectorAll<ScionChatChoices>('scion-chat-choices') ?? []
  )) {
    await card.updateComplete;
  }
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

describe('<scion-page-project-decisions>', () => {
  let pendingResponses: ProjectPendingDecisionsResponse[];
  let madeResponses: ProjectMadeDecisionsResponse[];
  let pendingHandler: ((url: URL, init?: RequestInit) => Promise<Response>) | null;
  let madeHandler: ((url: URL, init?: RequestInit) => Promise<Response>) | null;
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
    resetServerFlagStateForTests();
    setPreferredTimeZone('UTC');
    window.history.replaceState({}, '', '/projects/p-1/decisions');
    requests = [];
    navEvents = [];
    pendingHandler = null;
    madeHandler = null;
    document.addEventListener('nav-click', onNavClick);

    pendingResponses = [
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        prompts: [makePrompt()],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
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
        if (
          url.pathname.startsWith('/api/v1/decisions/') ||
          url.pathname.startsWith('/api/v1/choice-prompts/')
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

  function enableBothFlags(): void {
    setFeatureFlag(PROJECT_DECISIONS_FLAG, true);
    setFeatureFlag(CHAT_CHOICES_FLAG, true);
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

  it('renders 404 and makes zero API calls unless both experiments are enabled', async () => {
    // Both off
    let el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(requests).toEqual([]);
    el.remove();

    // Only project_decisions on
    setFeatureFlag(PROJECT_DECISIONS_FLAG, true);
    setFeatureFlag(CHAT_CHOICES_FLAG, false);
    el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(requests).toEqual([]);
    el.remove();

    // Only chat_choices on
    setFeatureFlag(PROJECT_DECISIONS_FLAG, false);
    setFeatureFlag(CHAT_CHOICES_FLAG, true);
    el = await mount();
    expect(el.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
    expect(requests).toEqual([]);
  });

  it('renders Waiting on you live and purged cards, forwards surface="decisions_page", and renders collapsed Waiting on others', async () => {
    enableBothFlags();
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

    const topicChoices = topicCard.querySelector<ScionChatChoices>('scion-chat-choices')!;
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

    const dmChoices = dmCard.querySelector<ScionChatChoices>('scion-chat-choices')!;
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
    enableBothFlags();
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
    enableBothFlags();
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
      },
      {
        counts: { waitingOnYou: 1, waitingOnOthers: 0 },
        entries: [],
        agents: [{ id: 'a-rel', name: 'release-bot' }],
        answerers: [{ id: 'u-me', name: 'Alice' }],
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
    const agentSelect = root.querySelector<HTMLElement & { value?: string }>(
      '[data-filter="agent"]'
    )!;
    agentSelect.value = 'a-rel';
    agentSelect.dispatchEvent(new CustomEvent('sl-change'));
    await settle(el, 25);

    const provSelect = root.querySelector<HTMLElement & { value?: string }>(
      '[data-filter="provenance"]'
    )!;
    provSelect.value = 'confirmed';
    provSelect.dispatchEvent(new CustomEvent('sl-change'));
    await settle(el, 25);

    const lastMadeUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(lastMadeUrl).toBe(
      '/api/v1/projects/p-1/decisions?tab=made&agent=a-rel&provenance=confirmed'
    );
    expect(root.querySelector('[data-empty="made"]')?.textContent).toContain(
      'No decisions match the current filters'
    );
  });

  it('removes a prompt settled in another tab live on choice-updated and updates counts without reload', async () => {
    enableBothFlags();
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
      new CustomEvent('choice-updated', {
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

    // User-scoped DM choice-updated (no projectId) settles r-dm-live immediately and refreshes
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
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

  it('preserves Made Load more pages and cursor on live choice-updated, and never aborts an in-flight Load more', async () => {
    enableBothFlags();
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

    // Fire choice-updated while Load more is in flight; wait past 60ms debounce
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
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
    enableBothFlags();
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

    // Fire choice-updated while Made is hidden
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
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

  it('preserves Pending prompts beyond page 1 (>50) across live choice-updated without aborting Load more', async () => {
    enableBothFlags();
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

    pendingHandler = (url, init) => {
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

    // Fire choice-updated while Pending Load more is in flight
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
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
    enableBothFlags();
    setPreferredTimeZone('America/Los_Angeles');

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;

    const fromInput = root.querySelector<HTMLElement & { value?: string }>('[data-filter="from"]')!;
    const toInput = root.querySelector<HTMLElement & { value?: string }>('[data-filter="to"]')!;

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
    root.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(el, 25);

    expect(root.querySelector('[data-date-range-error]')).toBeNull();
    const recoveredUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(recoveredUrl).toBe('/api/v1/projects/p-1/decisions?tab=made');
  });

  it('drives all 5 Made UI filter controls into a combined query and resets all 5 on Clear filters', async () => {
    enableBothFlags();
    setPreferredTimeZone('UTC');

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;

    const setControl = async (selector: string, val: string): Promise<void> => {
      const ctrl = root.querySelector<HTMLElement & { value?: string }>(selector)!;
      ctrl.value = val;
      ctrl.dispatchEvent(new CustomEvent('sl-change'));
      await settle(el, 15);
    };

    await setControl('[data-filter="agent"]', 'a-rel');
    await setControl('[data-filter="user"]', 'u-bob');
    await setControl('[data-filter="provenance"]', 'interpreted');
    await setControl('[data-filter="from"]', '2026-11-01');
    await setControl('[data-filter="to"]', '2026-11-13');

    const combinedUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    const parsed = new URL(combinedUrl, 'http://localhost');
    expect(parsed.searchParams.get('tab')).toBe('made');
    expect(parsed.searchParams.get('agent')).toBe('a-rel');
    expect(parsed.searchParams.get('user')).toBe('u-bob');
    expect(parsed.searchParams.get('provenance')).toBe('interpreted');
    expect(parsed.searchParams.get('from')).toBe('2026-11-01T00:00:00.000Z');
    expect(parsed.searchParams.get('to')).toBe('2026-11-13T23:59:59.999Z');

    // Click Clear filters and verify all 5 params are cleared
    root.querySelector<HTMLElement>('[data-clear-filters]')!.click();
    await settle(el, 25);

    const resetUrl = [...requests].reverse().find((r) => r.url.includes('tab=made'))!.url;
    expect(resetUrl).toBe('/api/v1/projects/p-1/decisions?tab=made');
    expect(root.querySelector('[data-clear-filters]')).toBeNull();
  });

  it('preserves the exact <scion-chat-choices> DOM element for surviving cards via keyed repeat()', async () => {
    enableBothFlags();
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

    const p1ChoicesBefore = root.querySelector<ScionChatChoices>(
      '[data-prompt-card="p-1"] scion-chat-choices'
    )!;
    const p2ChoicesBefore = root.querySelector<ScionChatChoices>(
      '[data-prompt-card="p-2"] scion-chat-choices'
    )!;
    expect(p1ChoicesBefore).not.toBeNull();
    expect(p2ChoicesBefore).not.toBeNull();
    expect(p1ChoicesBefore).not.toBe(p2ChoicesBefore);

    // Settle p-1 so it leaves Waiting on you
    stateManager.dispatchEvent(
      new CustomEvent('choice-updated', {
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
    const p2ChoicesAfter = root.querySelector<ScionChatChoices>(
      '[data-prompt-card="p-2"] scion-chat-choices'
    )!;
    expect(p2ChoicesAfter).toBe(p2ChoicesBefore);
  });

  it('refreshes on chat-message-received for choice-prompt only and dynamically toggles on FEATURE_FLAGS_CHANGED_EVENT', async () => {
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
      [PROJECT_DECISIONS_FLAG]: true,
      [CHAT_CHOICES_FLAG]: true,
    };
    window.dispatchEvent(new Event(FEATURE_FLAGS_CHANGED_EVENT));
    await settle(el, 35);

    expect(root.querySelector('scion-page-404')).toBeNull();
    expect(root.querySelector('[data-prompt-card="m-init"]')).not.toBeNull();

    const pendingCountBefore = requests.filter((r) => r.url.includes('tab=pending')).length;

    // Ignore other-project choice-prompt and same-project plain message
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { projectId: 'other-proj', type: 'choice-prompt' } },
      })
    );
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { projectId: 'p-1', type: 'message' } },
      })
    );
    await settle(el, 90);
    expect(requests.filter((r) => r.url.includes('tab=pending')).length).toBe(pendingCountBefore);

    // Same-project choice-prompt triggers debounced refresh
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: { data: { projectId: 'p-1', type: 'choice-prompt' } },
      })
    );
    await settle(el, 90);
    expect(requests.filter((r) => r.url.includes('tab=pending')).length).toBe(
      pendingCountBefore + 1
    );
    expect(root.querySelector('[data-prompt-card="m-new"]')).not.toBeNull();
    expect(root.querySelector('[data-waiting-badge]')?.textContent?.trim()).toBe('2');

    // Dynamically disable flag -> tears down and renders 404
    setFeatureFlag(CHAT_CHOICES_FLAG, false);
    await settle(el, 20);
    expect(root.querySelector('scion-page-404')).not.toBeNull();
  });

  it('renders error states on initial 500 for Pending and Made tabs and recovers on Retry', async () => {
    enableBothFlags();
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
    enableBothFlags();
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
      },
    ];

    const el = await mount({ path: '/projects/p-1/decisions?tab=made' });
    const root = el.shadowRoot!;

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

    // Mobile Filters button exposes aria-expanded and aria-controls="made-filters-bar"
    const filtersToggle = root.querySelector<HTMLElement>('[data-mobile-filters-toggle]')!;
    expect(filtersToggle.getAttribute('aria-controls')).toBe('made-filters-bar');
    expect(filtersToggle.getAttribute('aria-expanded')).toBe('false');
    expect(root.querySelector('#made-filters-bar')).not.toBeNull();
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
});
