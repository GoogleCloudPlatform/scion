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
 * `<scion-chat-choice-response>` and `<scion-chat-message>` choice-response
 * attribution (ptone/scion#7673, ptone/scion#7775, design §4.6 & Mock P2-7):
 * single and set lines, `answeredByUserId` attribution, interpreted
 * provenance, confirmed, overridden/changed, not delivered, View reply,
 * and Jump.
 */

import { describe, it, expect, afterEach } from 'vitest';
import { setFeatureFlag } from '../../../utils/feature-flags.js';
import { CHAT_CHOICES_EXPERIMENT } from '../../../client/chat-choices.js';
import type {
  ChoiceResponseSetAnswer,
  ChoiceResponseSetView,
  ChoiceResponseView,
} from '../../../shared/types.js';

await import('./chat-choice-response.js');
await import('./chat-message.js');
type ScionChatChoiceResponse = import('./chat-choice-response.js').ScionChatChoiceResponse;
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

function single(over: Partial<ChoiceResponseView> = {}): ChoiceResponseView {
  return {
    rowId: 'r1',
    ref: 'D234',
    key: 'D234',
    version: 1,
    question: 'Ship v0.9 today?',
    choiceId: 'yes',
    label: 'Yes',
    provenance: 'direct',
    promptMessageId: 'p1',
    ...over,
  };
}

function setAnswer(over: Partial<ChoiceResponseSetAnswer> = {}): ChoiceResponseSetAnswer {
  const key = over.key ?? 'D234';
  const label = over.label ?? 'Yes';
  return {
    rowId: 'r1',
    ref: key,
    key,
    version: 1,
    question: `${key}?`,
    choiceId: label,
    label,
    provenance: 'direct',
    outcome: 'answer',
    matchedDefault: false,
    ...over,
  };
}

function set(over: Partial<ChoiceResponseSetView> = {}): ChoiceResponseSetView {
  return {
    promptMessageId: 'p2',
    batchId: 'b1',
    answers: [
      setAnswer({ rowId: 'r1', key: 'D234', label: 'Yes', matchedDefault: true }),
      setAnswer({ rowId: 'r2', key: '17', label: 'a · Extend input-needed' }),
      setAnswer({ rowId: 'r3', key: 'ship-now', label: 'Yes' }),
    ],
    ...over,
  };
}

async function mount(
  props: Partial<
    Pick<
      ScionChatChoiceResponse,
      | 'response'
      | 'responseSet'
      | 'answererName'
      | 'isOwnAnswerer'
      | 'agentName'
      | 'dispatchState'
      | 'timestamp'
    >
  >
): Promise<ScionChatChoiceResponse> {
  const el = document.createElement('scion-chat-choice-response');
  el.answererName = 'alice';
  el.timestamp = '2026-11-12T15:42:00Z';
  Object.assign(el, props);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatChoiceResponse): ShadowRoot => el.shadowRoot!;
const text = (n: Element | null): string => n?.textContent?.replace(/\s+/g, ' ').trim() ?? '';

function jumps(el: HTMLElement): string[] {
  const out: string[] = [];
  el.addEventListener('scroll-to-message', (e) =>
    out.push((e as CustomEvent<{ messageId: string }>).detail.messageId)
  );
  return out;
}

afterEach(() => {
  document.body.innerHTML = '';
  setFeatureFlag(CHAT_CHOICES_EXPERIMENT, false);
});

describe('single', () => {
  it('names who chose what for which decision, with time and Jump', async () => {
    const el = await mount({ response: single() });
    const line = root(el).querySelector('[data-kind="single"]')!;
    expect(text(line)).toMatch(/^alice chose Yes for D234 Ship v0\.9 today\? · \d\d:\d\d · Jump$/);
    expect(line.querySelector('sl-icon')!.getAttribute('name')).toBe('check-circle-fill');
    expect(line.querySelector('.key')!.textContent).toBe('D234');
    expect(line.querySelector('.q')!.textContent).toBe('Ship v0.9 today?');
    expect([...line.querySelectorAll('b')].map((b) => b.textContent)).toEqual(['alice', 'Yes']);
    expect(line.querySelector('time')!.getAttribute('datetime')).toBe('2026-11-12T15:42:00Z');
  });

  it('jumps to the prompt', async () => {
    const el = await mount({ response: single() });
    const got = jumps(el);
    root(el).querySelector<HTMLButtonElement>('[data-action="jump"]')!.click();
    expect(got).toEqual(['p1']);
  });

  it('renders an interpreted answer for another user vs you, with View reply and Jump', async () => {
    const other = await mount({
      response: single({ provenance: 'agent_interpreted', sourceMessageId: 'src-1' }),
      agentName: 'release-bot',
      answererName: 'Bob',
    });
    const otherLine = root(other).querySelector('[data-kind="single"]')!;
    expect(otherLine.querySelector('sl-icon')!.getAttribute('name')).toBe('robot');
    expect(text(otherLine)).toMatch(
      /^release-bot recorded Bob's answer Yes for D234 from their reply · \d\d:\d\d · View reply · Jump$/
    );
    const otherJumps = jumps(other);
    otherLine.querySelector<HTMLButtonElement>('[data-action="view-reply"]')!.click();
    otherLine.querySelector<HTMLButtonElement>('[data-action="jump"]')!.click();
    expect(otherJumps).toEqual(['src-1', 'p1']);

    const own = await mount({
      response: single({ provenance: 'agent_interpreted' }),
      agentName: 'release-bot',
      answererName: 'you',
      isOwnAnswerer: true,
    });
    const ownLine = root(own).querySelector('[data-kind="single"]')!;
    expect(text(ownLine)).toMatch(
      /^release-bot recorded your answer Yes for D234 from your reply · \d\d:\d\d · Jump$/
    );
    expect(ownLine.querySelector('[data-action="view-reply"]')).toBeNull();
  });

  it('renders a confirmed single answer with patch-check and not sent to the agent', async () => {
    const el = await mount({
      response: single({ outcome: 'confirm' }),
      answererName: 'you',
      isOwnAnswerer: true,
    });
    const line = root(el).querySelector('[data-kind="single"]')!;
    expect(line.querySelector('sl-icon')!.getAttribute('name')).toBe('patch-check');
    expect(text(line.querySelector('[data-state="not-sent"]'))).toBe('not sent to the agent');
    expect(text(line)).toMatch(
      /^you confirmed Yes for D234 · \d\d:\d\d · not sent to the agent · Jump$/
    );
  });

  it('renders an overridden single answer with arrow-left-right and previousChoiceLabel', async () => {
    const el = await mount({
      response: single({
        choiceId: 'no',
        label: 'No',
        outcome: 'override',
        previousChoiceLabel: 'Yes',
      }),
      answererName: 'you',
      isOwnAnswerer: true,
    });
    const line = root(el).querySelector('[data-kind="single"]')!;
    const icon = line.querySelector('sl-icon')!;
    expect(icon.getAttribute('name')).toBe('arrow-left-right');
    expect(icon.classList.contains('change')).toBe(true);
    expect(text(line)).toMatch(
      /^you changed D234 to No \(was interpreted as Yes\) · \d\d:\d\d · Jump$/
    );

    const noPrev = await mount({
      response: single({ choiceId: 'no', label: 'No', outcome: 'override' }),
      answererName: 'Bob',
    });
    expect(text(root(noPrev).querySelector('[data-kind="single"]'))).toMatch(
      /^Bob changed D234 to No · \d\d:\d\d · Jump$/
    );
  });

  it('says not delivered when dispatch failed', async () => {
    const el = await mount({ response: single(), dispatchState: 'failed' });
    expect(text(root(el).querySelector('[data-state="not-delivered"]'))).toBe(
      'not delivered (agent gone)'
    );
    expect(text(root(el).querySelector('.line'))).toMatch(/· not delivered \(agent gone\) · Jump$/);
  });

  it('says nothing about delivery otherwise', async () => {
    const el = await mount({ response: single(), dispatchState: 'dispatched' });
    expect(root(el).querySelector('[data-state="not-delivered"]')).toBeNull();
  });
});

describe('set', () => {
  it('summarises collapsed, then lists the answers in position order', async () => {
    const el = await mount({ responseSet: set(), answererName: 'you' });
    const details = root(el).querySelector<HTMLDetailsElement>('details[data-kind="set"]')!;
    expect(details.open).toBe(false);
    const summary = details.querySelector('summary')!;
    expect(text(summary)).toMatch(/^you answered 3 decisions · \d\d:\d\d · Show answers ▸$/);

    details.open = true;
    details.dispatchEvent(new Event('toggle'));
    await el.updateComplete;
    expect(text(summary)).toContain('Hide answers ▾');
    const rows = [...details.querySelectorAll('li[data-row]')].map((li) => text(li));
    expect(rows).toEqual(['D234 Yes (recommended)', '17 a · Extend input-needed', 'ship-now Yes']);
  });

  it('renders multi-prompt interpreted set with View reply, (other question), and per-item Jump', async () => {
    const bobSet = await mount({
      responseSet: {
        batchId: 'b-bob',
        answers: [
          setAnswer({
            rowId: 'r1',
            key: 'ship-now',
            label: 'Yes',
            provenance: 'agent_interpreted',
            sourceMessageId: 'src-bob',
            promptMessageId: 'prompt-a',
          }),
          setAnswer({
            rowId: 'r2',
            key: 'D9',
            label: 'Later',
            provenance: 'agent_interpreted',
            sourceMessageId: 'src-bob',
            promptMessageId: 'prompt-b',
          }),
        ],
      },
      agentName: 'release-bot',
      answererName: 'Bob',
    });
    const summary = root(bobSet).querySelector('summary')!;
    expect(summary.querySelector('sl-icon')!.getAttribute('name')).toBe('robot');
    expect(text(summary)).toMatch(
      /^release-bot recorded Bob's answers to 2 decisions from their reply · \d\d:\d\d · Show answers ▸$/
    );
    const rows = [...root(bobSet).querySelectorAll('li[data-row]')].map((li) => text(li));
    expect(rows).toEqual(['ship-now Yes · Jump', 'D9 Later (other question) · Jump']);
    const footer = root(bobSet).querySelector('ul.answers > li:not([data-row])')!;
    expect(text(footer)).toBe('View reply');
    expect(footer.querySelector('[data-action="jump"]')).toBeNull();

    const got = jumps(bobSet);
    root(bobSet).querySelector<HTMLButtonElement>('[data-action="view-reply"]')!.click();
    const itemJumps = root(bobSet).querySelectorAll<HTMLButtonElement>(
      'li[data-row] [data-action="jump"]'
    );
    expect(itemJumps.length).toBe(2);
    itemJumps[0].click();
    itemJumps[1].click();
    expect(got).toEqual(['src-bob', 'prompt-a', 'prompt-b']);

    const ownSingleSet = await mount({
      responseSet: set({
        answers: [
          setAnswer({
            rowId: 'r1',
            key: 'D234',
            label: 'Yes',
            provenance: 'agent_interpreted',
            promptMessageId: 'p2',
          }),
        ],
      }),
      agentName: 'release-bot',
      answererName: 'you',
      isOwnAnswerer: true,
    });
    expect(text(root(ownSingleSet).querySelector('summary'))).toMatch(
      /^release-bot recorded your answer from your reply · \d\d:\d\d · Show answers ▸$/
    );
    expect(root(ownSingleSet).querySelector('li[data-row] [data-action="jump"]')).toBeNull();
    expect(text(root(ownSingleSet).querySelector('[data-action="jump"]'))).toBe('Jump to question');
  });

  it('renders confirmed and overridden items in sets, and marks all-confirm sets not sent', async () => {
    const mixed = await mount({
      responseSet: set({
        answers: [
          setAnswer({
            rowId: 'r1',
            key: 'D234',
            label: 'Yes',
            outcome: 'confirm',
            matchedDefault: true,
          }),
          setAnswer({
            rowId: 'r2',
            key: '17',
            label: 'New type',
            outcome: 'override',
            previousChoiceLabel: 'Extend',
          }),
          setAnswer({ rowId: 'r3', key: 'ship-now', label: 'No', outcome: 'override' }),
        ],
      }),
      answererName: 'you',
      isOwnAnswerer: true,
    });
    expect(root(mixed).querySelector('[data-state="not-sent"]')).toBeNull();
    const rows = [...root(mixed).querySelectorAll('li[data-row]')].map((li) => text(li));
    expect(rows).toEqual([
      'D234 Yes (confirmed)',
      '17 New type (changed from Extend)',
      'ship-now No (changed)',
    ]);

    const allConfirm = await mount({
      responseSet: set({
        answers: [
          setAnswer({ rowId: 'r1', key: 'D234', label: 'Yes', outcome: 'confirm' }),
          setAnswer({ rowId: 'r2', key: '17', label: 'Extend', outcome: 'confirm' }),
        ],
      }),
      answererName: 'you',
      isOwnAnswerer: true,
    });
    expect(root(allConfirm).querySelector('summary sl-icon')!.getAttribute('name')).toBe(
      'patch-check'
    );
    expect(text(root(allConfirm).querySelector('[data-state="not-sent"]'))).toBe(
      'not sent to the agent'
    );
  });

  it('jumps to the question', async () => {
    const el = await mount({ responseSet: set() });
    const got = jumps(el);
    const jump = root(el).querySelector<HTMLButtonElement>('[data-action="jump"]')!;
    expect(text(jump)).toBe('Jump to question');
    jump.click();
    expect(got).toEqual(['p2']);
  });

  it('says not delivered when dispatch failed', async () => {
    const el = await mount({ responseSet: set(), dispatchState: 'failed' });
    expect(text(root(el).querySelector('summary'))).toContain('not delivered (agent gone)');
  });
});

describe('scion-chat-message attribution by answeredByUserId', () => {
  async function mountMsg(props: Partial<ScionChatMessage>): Promise<ScionChatMessage> {
    setFeatureFlag(CHAT_CHOICES_EXPERIMENT, true);
    const msg = document.createElement('scion-chat-message');
    msg.messageId = 'm1';
    msg.messageType = 'choice-response';
    msg.timestamp = '2026-11-12T15:42:00Z';
    Object.assign(msg, props);
    document.body.appendChild(msg);
    await msg.updateComplete;
    const inner = msg.shadowRoot?.querySelector<ScionChatChoiceResponse>(
      'scion-chat-choice-response'
    );
    if (inner) await inner.updateComplete;
    return msg;
  }

  it('attributes own vs other user strictly by answeredByUserId === currentUserId, never !fromAgent', async () => {
    const ownMsg = await mountMsg({
      currentUserId: 'u-alice',
      fromAgent: true,
      senderName: 'Alice',
      routedTo: 'release-bot',
      choiceResponse: single({ answeredByUserId: 'u-alice', answeredByName: 'Alice' }),
    });
    expect(ownMsg.shadowRoot!.querySelector('.choice-response-row.own')).not.toBeNull();
    const ownLine = ownMsg.shadowRoot!.querySelector('scion-chat-choice-response')!;
    expect(text(ownLine.shadowRoot!.querySelector('.line'))).toContain('you chose Yes for D234');

    const bobMsg = await mountMsg({
      currentUserId: 'u-alice',
      fromAgent: false,
      senderName: 'Alice',
      routedTo: 'release-bot',
      choiceResponse: single({ answeredByUserId: 'u-bob', answeredByName: 'Bob' }),
    });
    expect(bobMsg.shadowRoot!.querySelector('.choice-response-row.other')).not.toBeNull();
    const bobLine = bobMsg.shadowRoot!.querySelector('scion-chat-choice-response')!;
    expect(text(bobLine.shadowRoot!.querySelector('.line'))).toContain('Bob chose Yes for D234');
  });

  it('never attributes "you" when currentUserId is empty', async () => {
    const msg = await mountMsg({
      currentUserId: '',
      fromAgent: false,
      senderName: 'Alice',
      choiceResponse: single({ answeredByUserId: 'u-alice', answeredByName: 'Alice' }),
    });
    expect(msg.shadowRoot!.querySelector('.choice-response-row.other')).not.toBeNull();
    const line = msg.shadowRoot!.querySelector('scion-chat-choice-response')!;
    expect(text(line.shadowRoot!.querySelector('.line'))).toContain('Alice chose Yes for D234');
    expect(text(line.shadowRoot!.querySelector('.line'))).not.toContain('you chose');
  });

  it('attributes deleted answerer (answeredByUserId omitted or empty) to answeredByName in other lane for both empty and non-empty currentUserId', async () => {
    for (const currentUserId of ['', 'u-alice']) {
      for (const response of [
        single({ answeredByName: 'Old User' }),
        single({ answeredByUserId: '', answeredByName: 'Old User' }),
      ]) {
        const msg = await mountMsg({
          currentUserId,
          fromAgent: false,
          senderName: 'Alice',
          choiceResponse: response,
        });
        expect(msg.shadowRoot!.querySelector('.choice-response-row.other')).not.toBeNull();
        expect(msg.shadowRoot!.querySelector('.choice-response-row.own')).toBeNull();
        const line = msg.shadowRoot!.querySelector('scion-chat-choice-response')!;
        expect(text(line.shadowRoot!.querySelector('.line'))).toContain(
          'Old User chose Yes for D234'
        );
        expect(text(line.shadowRoot!.querySelector('.line'))).not.toContain('you chose');
      }
    }
  });

  it('places agent-sent interpreted single and set responses in the left lane with agentName and viewer-aware wording', async () => {
    const ownInterp = await mountMsg({
      currentUserId: 'u-alice',
      fromAgent: true,
      senderIsAgent: true,
      sender: 'agent:release-bot',
      senderName: 'release-bot',
      routedTo: '',
      choiceResponse: single({
        provenance: 'agent_interpreted',
        answeredByUserId: 'u-alice',
        answeredByName: 'Alice',
        sourceMessageId: 'reply-42',
      }),
    });
    expect(ownInterp.shadowRoot!.querySelector('.choice-response-row.other')).not.toBeNull();
    const ownLine = ownInterp.shadowRoot!.querySelector('scion-chat-choice-response')!;
    expect(ownLine.getAttribute('lane')).toBe('other');
    expect(text(ownLine.shadowRoot!.querySelector('.line'))).toMatch(
      /^release-bot recorded your answer Yes for D234 from your reply · \d\d:\d\d · View reply · Jump$/
    );
    const got = jumps(ownInterp);
    ownLine.shadowRoot!.querySelector<HTMLButtonElement>('[data-action="view-reply"]')!.click();
    expect(got).toEqual(['reply-42']);

    const bobInterpSet = await mountMsg({
      currentUserId: 'u-alice',
      fromAgent: true,
      senderIsAgent: true,
      sender: 'agent:release-bot',
      senderName: '',
      routedTo: '',
      choiceResponseSet: set({
        answers: [
          setAnswer({
            rowId: 'r1',
            key: 'D234',
            label: 'Yes',
            provenance: 'agent_interpreted',
            answeredByUserId: 'u-bob',
            answeredByName: 'Bob',
          }),
          setAnswer({
            rowId: 'r2',
            key: '17',
            label: 'Extend',
            provenance: 'agent_interpreted',
            answeredByUserId: 'u-bob',
            answeredByName: 'Bob',
          }),
        ],
      }),
    });
    expect(bobInterpSet.shadowRoot!.querySelector('.choice-response-row.other')).not.toBeNull();
    const bobSummary = bobInterpSet
      .shadowRoot!.querySelector('scion-chat-choice-response')!
      .shadowRoot!.querySelector('summary')!;
    expect(text(bobSummary)).toMatch(
      /^release-bot recorded Bob's answers to 2 decisions from their reply · \d\d:\d\d · Show answers ▸$/
    );
  });
});

it('renders nothing without a view', async () => {
  const el = await mount({});
  expect(root(el).querySelector('.line, details')).toBeNull();
});
