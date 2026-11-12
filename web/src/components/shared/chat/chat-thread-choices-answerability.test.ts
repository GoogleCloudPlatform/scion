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
 * Choices in the thread, phases 4 and 5 (ptone/scion#7673, design §4.5,
 * §4.6): one cached asker read per thread load, the read-only thread, a
 * card's 403 reaching the asker's other cards and the composer, and the
 * compact choice-response lines with their jump to the prompt.
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

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  stateManager,
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
  apiErrorMessageFromBody: (): undefined => undefined,
}));
vi.mock('../../../utils/toast.js', () => ({ showToast }));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;
type Card = HTMLElement & {
  updateComplete: Promise<boolean>;
  shadowRoot: ShadowRoot;
  answerBlocked: boolean;
};

const KEY = 'topic-choices';
const LINEAGE =
  'release-bot is in lineage mode and only accepts messages from users in its ancestry. You are not in its lineage.';

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

function decision(messageId: string, over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: `row-${messageId}`,
    messageId,
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

function prompt(id: string, minute: number) {
  return {
    id,
    projectId: 'proj-1',
    sender: 'agent:release-bot',
    senderId: 'a1',
    recipient: '',
    recipientId: '',
    msg: 'fallback',
    type: 'choice-prompt',
    agentId: 'a1',
    createdAt: `2026-11-12T15:${minute}:00Z`,
    threadId: KEY,
  };
}

function response(id: string, senderId: string, minute: number) {
  return {
    id,
    projectId: 'proj-1',
    sender: `user:${senderId}`,
    senderId,
    recipient: 'agent:release-bot',
    recipientId: 'a1',
    msg: 'alice chose "Yes" for D234 "Ship it?"',
    type: 'choice-response',
    agentId: 'a1',
    createdAt: `2026-11-12T15:${minute}:00Z`,
    threadId: KEY,
    dispatchState: 'failed',
  };
}

let history: Record<string, unknown>;
let agentRead: () => Response;

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
  await settle();
  await thread.updateComplete;
  return thread;
}

async function messageEl(thread: ScionChatThread, id: string): Promise<HTMLElement> {
  const el = thread.shadowRoot!.querySelector<HTMLElement & { updateComplete: Promise<boolean> }>(
    `#msg-${id}`
  )!;
  expect(el).not.toBeNull();
  await el.updateComplete;
  await settle();
  return el;
}

async function cardOf(thread: ScionChatThread, id: string): Promise<Card> {
  const el = await messageEl(thread, id);
  const card = el.shadowRoot!.querySelector<Card>('scion-chat-choices')!;
  expect(card).not.toBeNull();
  await card.updateComplete;
  return card;
}

const text = (n: Element | null | undefined): string =>
  n?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
const agentReads = (): number =>
  apiFetch.mock.calls.filter(([path]) => path.startsWith('/api/v1/agents/')).length;

beforeEach(() => {
  setFeatureFlag(CHAT_CHOICES_EXPERIMENT, true);
  history = {
    messages: [prompt('p1', 41), prompt('p2', 42)],
    messageDecisions: { p1: [decision('p1')], p2: [decision('p2')] },
    messageChoiceIntros: { p1: '', p2: '' },
    messageChoiceAnswerModes: { p1: 'single', p2: 'single' },
  };
  agentRead = () => json({ id: 'a1', name: 'release-bot', _messageability: { canMessage: true } });
  apiFetch.mockReset();
  showToast.mockReset();
  apiFetch.mockImplementation((path) => {
    if (path.includes('/messages?')) return Promise.resolve(json(history));
    if (path === '/api/v1/agents/a1') return Promise.resolve(agentRead());
    return Promise.resolve(json({ items: [] }));
  });
});

afterEach(() => {
  document.body.innerHTML = '';
  setFeatureFlag(CHAT_CHOICES_EXPERIMENT, false);
});

describe('asker answerability', () => {
  it('reads each asker once per thread load', async () => {
    const thread = await open();
    await cardOf(thread, 'p2');
    expect(agentReads()).toBe(1);
  });

  it('blocks every card of an asker that will not accept the viewer', async () => {
    agentRead = () =>
      json({
        id: 'a1',
        name: 'release-bot',
        _messageability: { canMessage: false, reason: 'mode_lineage_no_ancestry' },
      });
    const thread = await open();
    for (const id of ['p1', 'p2']) {
      const card = await cardOf(thread, id);
      expect(card.answerBlocked).toBe(true);
      expect(text(card.shadowRoot.querySelector('.reason'))).toBe(
        `You can't answer release-bot: ${LINEAGE}`
      );
    }
  });

  it('shows the gone note when the asker no longer exists', async () => {
    agentRead = () => json({ error: { code: 'not_found' } }, 404);
    const thread = await open();
    const card = await cardOf(thread, 'p1');
    expect(card.answerBlocked).toBe(false);
    expect(text(card.shadowRoot.querySelector('.note'))).toContain('is no longer running');
  });

  it('shows the read-only reason, without an asker read, in a thread the viewer cannot post in', async () => {
    history.canPost = false;
    const thread = await open();
    const card = await cardOf(thread, 'p1');
    expect(card.answerBlocked).toBe(true);
    expect(text(card.shadowRoot.querySelector('.reason'))).toBe(
      "You can read this thread but can't post in it, so you can't answer."
    );
    expect(agentReads()).toBe(0);
  });

  it('a 403 message_denied on one card blocks the asker’s other cards', async () => {
    const thread = await open();
    const first = await cardOf(thread, 'p1');
    apiFetch.mockImplementationOnce(() =>
      Promise.resolve(
        json(
          {
            error: {
              code: 'message_denied',
              message: 'Message delivery denied',
              details: { reason: 'mode_lineage_no_ancestry' },
            },
          },
          403
        )
      )
    );
    first.shadowRoot.querySelector<HTMLElement>('[data-choice="no"]')!.click();
    await settle();
    await thread.updateComplete;
    const second = await cardOf(thread, 'p2');
    expect(second.answerBlocked).toBe(true);
    expect(text(second.shadowRoot.querySelector('.reason'))).toContain(LINEAGE);
  });

  it('a 403 chat_post_denied makes the thread read-only', async () => {
    const thread = await open();
    expect(thread.shadowRoot!.querySelector('scion-chat-composer')).not.toBeNull();
    const first = await cardOf(thread, 'p1');
    apiFetch.mockImplementationOnce(() =>
      Promise.resolve(
        json(
          { error: { code: 'chat_post_denied', message: 'no', details: { reason: 'read_only' } } },
          403
        )
      )
    );
    first.shadowRoot.querySelector<HTMLElement>('[data-choice="no"]')!.click();
    await settle();
    await thread.updateComplete;
    expect(thread.shadowRoot!.querySelector('scion-chat-composer')).toBeNull();
    const second = await cardOf(thread, 'p2');
    expect(text(second.shadowRoot.querySelector('.reason'))).toContain("can't post in it");
  });
});

describe('choice-response lines', () => {
  beforeEach(() => {
    history.messages = [prompt('p1', 41), response('c1', 'u-me', 42), response('c2', 'u-bob', 43)];
    history.messageChoiceResponses = {
      c1: {
        rowId: 'row-p1',
        ref: 'D234',
        key: 'D234',
        version: 1,
        question: 'Ship it?',
        choiceId: 'yes',
        label: 'Yes',
        provenance: 'direct',
        promptMessageId: 'p1',
      },
    };
    history.messageChoiceResponseSets = {
      c2: {
        promptMessageId: 'p1',
        batchId: 'b1',
        answers: [
          {
            rowId: 'row-p1',
            ref: 'D234',
            key: 'D234',
            version: 1,
            question: 'Ship it?',
            choiceId: 'yes',
            label: 'Yes',
            provenance: 'direct',
            outcome: 'answer',
            matchedDefault: true,
          },
        ],
      },
    };
  });

  function lineOf(el: HTMLElement) {
    return el.shadowRoot!.querySelector<HTMLElement & { updateComplete: Promise<boolean> }>(
      'scion-chat-choice-response'
    );
  }

  it('renders a compact line in the answerer’s lane, no bubble', async () => {
    const thread = await open();
    const own = await messageEl(thread, 'c1');
    expect(own.shadowRoot!.querySelector('.bubble')).toBeNull();
    expect(own.shadowRoot!.querySelector('.choice-response-row.own')).not.toBeNull();
    const line = lineOf(own)!;
    await line.updateComplete;
    expect(text(line.shadowRoot!.querySelector('.line'))).toMatch(
      /^you chose Yes for D234 Ship it\? · \d\d:\d\d · not delivered \(agent gone\) · Jump$/
    );

    const other = await messageEl(thread, 'c2');
    expect(other.shadowRoot!.querySelector('.choice-response-row.other')).not.toBeNull();
    const setLine = lineOf(other)!;
    await setLine.updateComplete;
    expect(text(setLine.shadowRoot!.querySelector('summary'))).toContain('answered 1 decision');
  });

  it('jumps to the prompt', async () => {
    const thread = await open();
    const scroll = vi.spyOn(thread, 'scrollToMessageById').mockResolvedValue();
    const line = lineOf(await messageEl(thread, 'c1'))!;
    await line.updateComplete;
    line.shadowRoot!.querySelector<HTMLElement>('[data-action="jump"]')!.click();
    expect(scroll).toHaveBeenCalledWith('p1');
  });

  it('expands a set on a touch device without opening the message menu', async () => {
    vi.spyOn(window, 'matchMedia').mockImplementation(
      (query: string) =>
        ({
          matches: query === '(hover: none)',
          media: query,
          addEventListener: vi.fn(),
          removeEventListener: vi.fn(),
        }) as unknown as MediaQueryList
    );
    try {
      const thread = await open();
      const line = lineOf(await messageEl(thread, 'c2'))!;
      await line.updateComplete;
      const summary = line.shadowRoot!.querySelector('summary')!;
      summary.dispatchEvent(
        new MouseEvent('click', { bubbles: true, composed: true, clientX: 10, clientY: 20 })
      );
      await thread.updateComplete;
      expect(thread.shadowRoot!.querySelector('.context-menu')).toBeNull();
    } finally {
      vi.restoreAllMocks();
    }
  });

  it('keeps the bubble with the browser flag off', async () => {
    setFeatureFlag(CHAT_CHOICES_EXPERIMENT, false);
    const thread = await open();
    const el = await messageEl(thread, 'c1');
    expect(lineOf(el)).toBeNull();
    expect(text(el.shadowRoot!.querySelector('.bubble-content'))).toContain('alice chose');
  });
});
