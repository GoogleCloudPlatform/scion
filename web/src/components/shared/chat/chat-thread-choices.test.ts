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
 * Choice prompts in the thread (ptone/scion#7673, design §4.1, §4.4): the
 * double gate (hub fields and the web.chat_choices browser flag), the
 * history side maps and live chat.message fields, and the version-guarded
 * patch from `choice-updated` (another tab's answer over SSE) and from the
 * card's own `choice-answered`.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { setFeatureFlag } from '../../../utils/feature-flags.js';
import { CHAT_CHOICES_EXPERIMENT } from '../../../client/chat-choices.js';
import type { DecisionView } from '../../../shared/types.js';

const stateManager = vi.hoisted(() =>
  Object.assign(new EventTarget(), {
    currentScope: null,
    isConnected: false,
    getAgent: (): undefined => undefined,
    seedAgents: (): void => {},
  })
);

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: unknown) => Promise<Response>>());
const showToast = vi.hoisted(() => vi.fn());

vi.mock('../../../utils/toast.js', () => ({ showToast }));

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  stateManager,
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
  apiErrorMessageFromBody: (): undefined => undefined,
}));

const { CHOICE_REPLACEMENT_ELSEWHERE_TOAST, MESSAGE_UNAVAILABLE_TOAST } =
  await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

const KEY = 'topic-choices';
const FALLBACK = 'Intro text\n\n**D234.** Ship it?\n  yes) Yes _(recommended)_\n  no) No';

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

function decision(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'row-1',
    messageId: 'p1',
    key: 'D234',
    ref: 'D234',
    version: 1,
    status: 'pending',
    askerAgentId: 'a1',
    askerAgentName: 'release-bot',
    question: 'Ship it?',
    defaultChoiceId: 'yes',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
    ],
    ...over,
  };
}

function decided(byName = 'Bob'): DecisionView {
  return decision({
    status: 'decided',
    decision: {
      choiceId: 'no',
      label: 'No',
      answeredByUserId: 'u-bob',
      answeredByName: byName,
      provenance: 'direct',
      surface: 'web',
      responseMessageId: 'resp-1',
      at: '2026-11-12T15:42:00Z',
    },
  });
}

function prompt(id = 'p1') {
  return {
    id,
    projectId: 'proj-1',
    sender: 'agent:release-bot',
    senderId: 'a1',
    recipient: '',
    recipientId: '',
    msg: FALLBACK,
    type: 'choice-prompt',
    agentId: 'a1',
    createdAt: '2026-11-12T15:41:00Z',
    threadId: KEY,
  };
}

let history: Record<string, unknown>;

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

async function open(): Promise<ScionChatThread> {
  const thread = document.createElement('scion-chat-thread');
  thread.conversationKey = KEY;
  thread.projectId = 'proj-1';
  thread.currentUserId = 'u-me';
  document.body.appendChild(thread);
  await thread.updateComplete;
  await vi.waitFor(() =>
    expect(apiFetch.mock.calls.some(([path]) => path.includes('/messages?'))).toBe(true)
  );
  await settle();
  await thread.updateComplete;
  return thread;
}

async function messageEl(thread: ScionChatThread, id = 'p1'): Promise<HTMLElement> {
  const el = thread.shadowRoot!.querySelector<HTMLElement & { updateComplete: Promise<boolean> }>(
    `#msg-${id}`
  )!;
  expect(el).not.toBeNull();
  await el.updateComplete;
  await settle();
  return el;
}

function cardOf(msgEl: HTMLElement) {
  return msgEl.shadowRoot!.querySelector<
    HTMLElement & { updateComplete: Promise<boolean>; shadowRoot: ShadowRoot }
  >('scion-chat-choices');
}

async function footText(msgEl: HTMLElement): Promise<string> {
  const card = cardOf(msgEl)!;
  await card.updateComplete;
  return card.shadowRoot.querySelector('.row-foot')?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

function bodyText(msgEl: HTMLElement): string {
  return msgEl.shadowRoot!.querySelector('.bubble-content')?.textContent ?? '';
}

function choiceUpdated(data: Record<string, unknown>): void {
  stateManager.dispatchEvent(new CustomEvent('choice-updated', { detail: { state: {}, data } }));
}

beforeEach(() => {
  setFeatureFlag(CHAT_CHOICES_EXPERIMENT, true);
  history = {
    messages: [prompt()],
    messageDecisions: { p1: [decision()] },
    messageChoiceIntros: { p1: 'Intro text' },
    messageChoiceAnswerModes: { p1: 'single' },
  };
  apiFetch.mockReset();
  apiFetch.mockImplementation((path) => {
    if (path.includes('/messages?')) return Promise.resolve(json(history));
    return Promise.resolve(json({ items: [] }));
  });
});

afterEach(() => {
  document.body.innerHTML = '';
  setFeatureFlag(CHAT_CHOICES_EXPERIMENT, false);
});

describe('choice prompt rendering', () => {
  it('renders the card from the history maps and shows only the intro', async () => {
    const thread = await open();
    const el = await messageEl(thread);

    expect(cardOf(el)).not.toBeNull();
    const text = bodyText(el);
    expect(text).toContain('Intro text');
    // The hub's markdown fallback is hidden.
    expect(text).not.toContain('recommended)_');
    expect(text).not.toContain('**D234.**');
    expect(el.shadowRoot!.querySelector('.bubble.choice')).not.toBeNull();
  });

  it('renders the fallback markdown as today with the browser flag off', async () => {
    setFeatureFlag(CHAT_CHOICES_EXPERIMENT, false);
    const thread = await open();
    const el = await messageEl(thread);

    expect(cardOf(el)).toBeNull();
    expect(bodyText(el)).toContain('Ship it?');
    expect(el.shadowRoot!.querySelector('.bubble.choice')).toBeNull();
  });

  it('renders the fallback when the hub sends no choice fields (experiment off)', async () => {
    history = { messages: [prompt()] };
    const thread = await open();
    const el = await messageEl(thread);

    expect(cardOf(el)).toBeNull();
    expect(bodyText(el)).toContain('Ship it?');
  });

  it('renders the card for a live chat.message with inline choice fields', async () => {
    history = { messages: [] };
    const thread = await open();
    stateManager.dispatchEvent(
      new CustomEvent('chat-message-received', {
        detail: {
          state: {},
          data: {
            ...prompt('p2'),
            conversationKey: KEY,
            decisions: [decision({ messageId: 'p2', rowId: 'row-2' })],
            choiceIntro: 'Live intro',
            answerMode: 'single',
          },
        },
      })
    );
    await settle();
    await thread.updateComplete;
    const el = await messageEl(thread, 'p2');

    expect(cardOf(el)).not.toBeNull();
    expect(bodyText(el)).toContain('Live intro');
    expect(bodyText(el)).not.toContain('**D234.**');
  });
});

describe('choice-updated', () => {
  it('patches the card when another tab answers (SSE)', async () => {
    const thread = await open();
    const el = await messageEl(thread);
    expect(await footText(el)).toBe('Awaiting your answer');

    choiceUpdated({ messageId: 'p1', threadId: KEY, decision: decided() });
    await thread.updateComplete;
    await settle();

    expect(await footText(el)).toMatch(/^Answered by Bob · /);
  });

  it('ignores an event for another conversation', async () => {
    const thread = await open();
    const el = await messageEl(thread);

    choiceUpdated({ messageId: 'p1', threadId: 'topic-other', decision: decided() });
    await thread.updateComplete;
    await settle();

    expect(await footText(el)).toBe('Awaiting your answer');
  });

  it('ignores a pending view arriving after the answer (out of order)', async () => {
    const thread = await open();
    const el = await messageEl(thread);

    choiceUpdated({ messageId: 'p1', threadId: KEY, decision: decided() });
    choiceUpdated({ messageId: 'p1', threadId: KEY, decision: decision() });
    await thread.updateComplete;
    await settle();

    expect(await footText(el)).toMatch(/^Answered by Bob/);
  });

  it('keeps the answer when a history page fetched earlier still says pending', async () => {
    const thread = await open();
    const el = await messageEl(thread);
    choiceUpdated({ messageId: 'p1', threadId: KEY, decision: decided() });
    await thread.updateComplete;

    // A reconnect refetch whose page predates the answer.
    const historyCalls = () =>
      apiFetch.mock.calls.filter(([path]) => path.includes('/messages?')).length;
    const before = historyCalls();
    // The first connect is the initial one; the second is a reconnect.
    stateManager.dispatchEvent(new Event('connected'));
    stateManager.dispatchEvent(new Event('connected'));
    await vi.waitFor(() => expect(historyCalls()).toBeGreaterThan(before));
    await settle();
    await thread.updateComplete;

    expect(await footText(el)).toMatch(/^Answered by Bob/);
  });

  it('holds the view of the card’s own answer (choice-answered)', async () => {
    const thread = await open();
    const el = await messageEl(thread);

    cardOf(el)!.dispatchEvent(
      new CustomEvent('choice-answered', {
        detail: { messageId: 'p1', decisions: [decided('Me')], responseMessageId: 'resp-1' },
        bubbles: true,
        composed: true,
      })
    );
    await thread.updateComplete;

    const internals = thread as unknown as {
      messageMap: Map<string, { decisions?: DecisionView[] }>;
    };
    expect(internals.messageMap.get('p1')?.decisions?.[0]?.status).toBe('decided');
  });
});

describe('set prompts and jump links', () => {
  it('renders a set prompt as one set card with only the intro', async () => {
    const { defaultChoiceId: _unused, ...noDefault } = decision({ rowId: 'row-2', key: '17' });
    history = {
      messages: [prompt()],
      messageDecisions: { p1: [decision(), noDefault] },
      messageChoiceIntros: { p1: 'Intro text' },
      messageChoiceAnswerModes: { p1: 'set' },
    };
    const thread = await open();
    const el = await messageEl(thread);

    const card = cardOf(el)!;
    expect(card).not.toBeNull();
    await card.updateComplete;
    expect(card.shadowRoot.querySelector('.card.set')).not.toBeNull();
    expect(card.shadowRoot.querySelectorAll('.row').length).toBe(2);
    expect(bodyText(el)).toContain('Intro text');
    expect(bodyText(el)).not.toContain('**D234.**');
  });

  it('a set row answered in another tab locks over SSE', async () => {
    history = {
      messages: [prompt()],
      messageDecisions: { p1: [decision(), decision({ rowId: 'row-2', key: '17' })] },
      messageChoiceAnswerModes: { p1: 'set' },
    };
    const thread = await open();
    const el = await messageEl(thread);

    choiceUpdated({ messageId: 'p1', threadId: KEY, decision: decided() });
    await thread.updateComplete;
    await settle();

    const card = cardOf(el)!;
    await card.updateComplete;
    const counter = card.shadowRoot.querySelector('.counter')?.textContent?.replace(/\s+/g, ' ');
    expect(counter?.trim()).toBe('0 remaining · 1 answered by Bob');
  });

  it('scrolls to the replacement when a card emits choice-jump', async () => {
    const thread = await open();
    const el = await messageEl(thread);
    const scroll = vi
      .spyOn(thread, 'scrollToMessageById')
      .mockImplementation(() => Promise.resolve());

    cardOf(el)!.dispatchEvent(
      new CustomEvent('choice-jump', {
        detail: { messageId: 'p2' },
        bubbles: true,
        composed: true,
      })
    );

    expect(scroll).toHaveBeenCalledWith('p2', true, false, CHOICE_REPLACEMENT_ELSEWHERE_TOAST);
  });

  it('says the new version is in another conversation when this one does not have it', async () => {
    const thread = await open();
    const el = await messageEl(thread);
    showToast.mockReset();
    apiFetch.mockImplementation((path) => {
      if (path.includes('around=')) {
        return Promise.resolve(
          json({ error: { code: 'message_unavailable', message: 'not found' } }, 404)
        );
      }
      if (path.includes('/messages?')) return Promise.resolve(json(history));
      return Promise.resolve(json({ items: [] }));
    });

    cardOf(el)!.dispatchEvent(
      new CustomEvent('choice-jump', {
        detail: { messageId: 'p-dm-v2' },
        bubbles: true,
        composed: true,
      })
    );

    await vi.waitFor(() =>
      expect(showToast).toHaveBeenCalledWith(CHOICE_REPLACEMENT_ELSEWHERE_TOAST, 'warning')
    );
    expect(showToast).not.toHaveBeenCalledWith(MESSAGE_UNAVAILABLE_TOAST, 'warning');
  });
});

describe('history choice maps on the other load paths', () => {
  it('renders a prompt that arrives through a lightweight-notification backfill as a card', async () => {
    history = { messages: [] };
    const thread = await open();
    history = {
      messages: [prompt('p2')],
      messageDecisions: { p2: [decision({ messageId: 'p2', rowId: 'row-2' })] },
      messageChoiceIntros: { p2: 'Backfilled intro' },
      messageChoiceAnswerModes: { p2: 'single' },
    };
    stateManager.dispatchEvent(new CustomEvent('chat-message-received', { detail: undefined }));
    await vi.waitFor(async () => {
      await thread.updateComplete;
      expect(thread.shadowRoot!.querySelector('#msg-p2')).not.toBeNull();
    });
    const el = await messageEl(thread, 'p2');
    expect(cardOf(el)).not.toBeNull();
    expect(bodyText(el)).toContain('Backfilled intro');
    expect(bodyText(el)).not.toContain('**D234.**');
  });

  it('renders the replacement as a card after Jump to v2 loads a window around it', async () => {
    const thread = await open();
    apiFetch.mockImplementation((path) => {
      if (path.includes('around=')) {
        return Promise.resolve(
          json({
            messages: [prompt('p9')],
            messageDecisions: { p9: [decision({ messageId: 'p9', rowId: 'row-9', version: 2 })] },
            messageChoiceIntros: { p9: 'Version two' },
            messageChoiceAnswerModes: { p9: 'single' },
          })
        );
      }
      if (path.includes('/messages?')) return Promise.resolve(json(history));
      return Promise.resolve(json({ items: [] }));
    });
    void thread.scrollToMessageById('p9');
    await vi.waitFor(async () => {
      await thread.updateComplete;
      expect(thread.shadowRoot!.querySelector('#msg-p9')).not.toBeNull();
    });
    const el = await messageEl(thread, 'p9');
    expect(cardOf(el)).not.toBeNull();
    expect(bodyText(el)).not.toBeNull();
    expect(bodyText(el)).not.toContain('**D234.**');
  });
});

describe('mobile touch on choice cards (ptone/scion#7839)', () => {
  let origMatchMedia: typeof window.matchMedia;

  beforeEach(() => {
    origMatchMedia = window.matchMedia;
    window.matchMedia = vi.fn().mockImplementation((query: string) => ({
      matches: query === '(hover: none)' || query.includes('max-width'),
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    }));
  });

  afterEach(() => {
    window.matchMedia = origMatchMedia;
  });

  function hasOpenMenu(thread: ScionChatThread): boolean {
    return (
      thread.shadowRoot!.querySelector('scion-action-sheet[open]') !== null ||
      thread.shadowRoot!.querySelector('.context-menu') !== null
    );
  }

  it('does not open the message context menu on mobile tap on sl-button, opt description, icon, or card gap', async () => {
    history = {
      messages: [prompt('p-plain'), prompt('p-desc')],
      messageDecisions: {
        'p-plain': [decision({ messageId: 'p-plain', rowId: 'row-plain' })],
        'p-desc': [
          decision({
            messageId: 'p-desc',
            rowId: 'row-desc',
            status: 'decided',
            decision: {
              choiceId: 'yes',
              label: 'Yes',
              answeredByUserId: 'u-me',
              answeredByName: 'Me',
              provenance: 'direct',
              surface: 'web',
              responseMessageId: 'resp-desc',
              at: '2026-11-12T15:42:00Z',
            },
            choices: [
              { id: 'yes', label: 'Yes', description: 'Deploy `v0.9` immediately.' },
              { id: 'no', label: 'No', description: 'Hold until Monday.' },
            ],
          }),
        ],
      },
      messageChoiceIntros: { 'p-plain': 'Plain', 'p-desc': 'With descriptions' },
      messageChoiceAnswerModes: { 'p-plain': 'single', 'p-desc': 'single' },
    };

    const thread = await open();
    apiFetch.mockImplementation(() =>
      Promise.resolve(json({ decision: decided('Me'), responseMessageId: 'resp-new' }))
    );

    const plainEl = await messageEl(thread, 'p-plain');
    const slBtn = cardOf(plainEl)!.shadowRoot.querySelector<HTMLElement>(
      'sl-button[data-choice="yes"]'
    )!;
    slBtn.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await thread.updateComplete;
    expect(hasOpenMenu(thread)).toBe(false);

    const descEl = await messageEl(thread, 'p-desc');
    const descCard = cardOf(descEl)!;
    const chosenOpt = descCard.shadowRoot.querySelector<HTMLElement>('button.opt.chosen')!;
    const icon = chosenOpt.querySelector<HTMLElement>('sl-icon')!;
    icon.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await thread.updateComplete;
    expect(hasOpenMenu(thread)).toBe(false);

    // Tapping the gap between stacked choice rows also stays out of the message menu.
    const choicesWrap = descCard.shadowRoot.querySelector<HTMLElement>('.choices')!;
    choicesWrap.dispatchEvent(new MouseEvent('click', { bubbles: true, composed: true }));
    await thread.updateComplete;
    expect(hasOpenMenu(thread)).toBe(false);

    // Touch long-press contextmenu on a choice row with description must not open the message menu.
    const descSpan = chosenOpt.querySelector<HTMLElement>('.desc')!;
    const touchCtx =
      typeof PointerEvent !== 'undefined'
        ? new PointerEvent('contextmenu', {
            bubbles: true,
            composed: true,
            cancelable: true,
            pointerType: 'touch',
            clientX: 40,
            clientY: 40,
          })
        : new MouseEvent('contextmenu', {
            bubbles: true,
            composed: true,
            cancelable: true,
            clientX: 40,
            clientY: 40,
          });
    descSpan.dispatchEvent(touchCtx);
    await thread.updateComplete;
    expect(hasOpenMenu(thread)).toBe(false);

    // Tapping the message intro outside <scion-chat-choices> still opens the message menu.
    const msgBody =
      descEl.shadowRoot!.querySelector<HTMLElement>('.message-content, .msg-body') ?? descEl;
    msgBody.dispatchEvent(
      new MouseEvent('click', { bubbles: true, composed: true, clientX: 20, clientY: 20 })
    );
    await thread.updateComplete;
    expect(hasOpenMenu(thread)).toBe(true);
  });
});
