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
 * `<scion-chat-choice-response>` (ptone/scion#7673, design §4.6): the
 * single and set lines, interpreted provenance, not delivered, and Jump.
 */

import { describe, it, expect, afterEach } from 'vitest';
import type { ChoiceResponseSetView, ChoiceResponseView } from '../../../shared/types.js';

await import('./chat-choice-response.js');
type ScionChatChoiceResponse = import('./chat-choice-response.js').ScionChatChoiceResponse;

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

function set(): ChoiceResponseSetView {
  const a = (rowId: string, key: string, label: string, matchedDefault: boolean) => ({
    rowId,
    ref: key,
    key,
    version: 1,
    question: `${key}?`,
    choiceId: label,
    label,
    provenance: 'direct',
    outcome: 'answer',
    matchedDefault,
  });
  return {
    promptMessageId: 'p2',
    batchId: 'b1',
    answers: [
      a('r1', 'D234', 'Yes', true),
      a('r2', '17', 'a · Extend input-needed', false),
      a('r3', 'ship-now', 'Yes', false),
    ],
  };
}

async function mount(
  props: Partial<
    Pick<
      ScionChatChoiceResponse,
      'response' | 'responseSet' | 'answererName' | 'agentName' | 'dispatchState' | 'timestamp'
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

function jumps(el: ScionChatChoiceResponse): string[] {
  const out: string[] = [];
  el.addEventListener('scroll-to-message', (e) =>
    out.push((e as CustomEvent<{ messageId: string }>).detail.messageId)
  );
  return out;
}

afterEach(() => {
  document.body.innerHTML = '';
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

  it('marks an interpreted answer with the robot and who recorded it', async () => {
    const el = await mount({
      response: single({ provenance: 'agent_interpreted' }),
      agentName: 'agent-x',
    });
    const line = root(el).querySelector('[data-kind="single"]')!;
    expect(line.querySelector('sl-icon')!.getAttribute('name')).toBe('robot');
    expect(text(line.querySelector('[data-state="interpreted"]'))).toBe(
      "recorded from alice's reply by agent-x"
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

it('renders nothing without a view', async () => {
  const el = await mount({});
  expect(root(el).querySelector('.line, details')).toBeNull();
});
