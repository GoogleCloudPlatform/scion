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
  PROJECT_DECISIONS_FLAG,
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
    window.history.replaceState({}, '', '/projects/p-1/decisions');
    requests = [];
    navEvents = [];
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
            const next = madeResponses.length > 1 ? madeResponses.shift()! : madeResponses[0]!;
            return jsonResponse(next);
          }
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
});
