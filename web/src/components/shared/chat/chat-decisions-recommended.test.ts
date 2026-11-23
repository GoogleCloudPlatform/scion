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
 * `<scion-chat-decisions>` keeps marking the recommended choice after a
 * decision leaves pending (ptone/scion#8477, design for ptone/scion#8443
 * section 4.3, acceptance 5): a muted visible marker, the title suffix and
 * the screen-reader "(recommended)", never the pending outline, so the picked
 * state stays dominant.
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import type { AnswerView, DecisionView } from '../../../shared/types.js';

vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./chat-decisions.js');
type ScionChatDecisions = import('./chat-decisions.js').ScionChatDecisions;

const ME = { id: 'u-me', name: 'Me' };

function answer(choiceId: string, over: Partial<AnswerView> = {}): AnswerView {
  return {
    choiceId,
    label: choiceId,
    answeredByUserId: ME.id,
    answeredByName: ME.name,
    provenance: 'direct',
    outcome: 'answer',
    surface: 'web',
    responseMessageId: 'resp-1',
    at: '2026-11-20T10:00:00Z',
    ...over,
  };
}

function view(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r1',
    messageId: 'm1',
    key: 'D1',
    ref: 'D1',
    version: 1,
    status: 'pending',
    askerAgentId: 'a1',
    askerAgentName: 'agent-x',
    question: 'Ship it?',
    defaultChoiceId: 'yes',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
    ],
    ...over,
  };
}

const STACKED = [
  { id: 'yes', label: 'Yes', description: 'Deploy right away.' },
  { id: 'no', label: 'No', description: 'Hold until Monday.' },
];

/** Every terminal state from acceptance 5, plus the interpreted (decided) card. */
const TERMINAL: Array<[string, Partial<DecisionView>]> = [
  ['decided, recommendation not picked', { status: 'decided', decision: answer('no') }],
  ['decided, recommendation picked', { status: 'decided', decision: answer('yes') }],
  [
    'decided by an interpreted answer',
    {
      status: 'decided',
      decision: answer('no', { provenance: 'agent_interpreted', interpretedByAgentId: 'a1' }),
    },
  ],
  [
    'replaced',
    {
      status: 'replaced',
      replacedBy: { rowId: 'r2', messageId: 'm2', version: 2, at: '2026-11-20T11:00:00Z' },
    },
  ],
  [
    'withdrawn',
    { status: 'withdrawn', withdrawn: { at: '2026-11-20T11:00:00Z', byName: 'agent-x' } },
  ],
  [
    'dismissed',
    {
      status: 'dismissed',
      dismissed: {
        at: '2026-11-20T11:00:00Z',
        byUserId: ME.id,
        byName: ME.name,
        reason: 'Not needed',
        surface: 'web',
        responseMessageId: 'resp-d',
      },
    },
  ],
];

async function mount(decisions: DecisionView[], set = false): Promise<ScionChatDecisions> {
  const el = document.createElement('scion-chat-decisions');
  el.messageId = 'm1';
  el.currentUserId = ME.id;
  if (set) el.answerMode = 'set';
  el.decisions = decisions;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const text = (n: Element | null | undefined): string =>
  n?.textContent?.replace(/\s+/g, ' ').trim() ?? '';

function choice(el: ScionChatDecisions, id: string, rowId?: string): HTMLElement {
  const scope = rowId
    ? el.shadowRoot!.querySelector<HTMLElement>(`.row[data-row="${rowId}"]`)!
    : el.shadowRoot!;
  return scope.querySelector<HTMLElement>(`[data-choice="${id}"]`)!;
}

function expectMuted(c: HTMLElement): void {
  const mark = c.querySelector('[data-rec-mark]');
  expect(text(mark)).toBe('recommended');
  expect(mark?.getAttribute('aria-hidden')).toBe('true');
  expect(text(c.querySelector('.sr-only'))).toBe('(recommended)');
  expect(c.getAttribute('title')).toContain('(recommended)');
}

function expectUnmarked(c: HTMLElement): void {
  expect(c.querySelector('[data-rec-mark]')).toBeNull();
  expect(c.querySelector('.sr-only')).toBeNull();
}

afterEach(() => {
  document.body.innerHTML = '';
});

describe('single card buttons', () => {
  for (const [name, over] of TERMINAL) {
    it(`${name}: muted marker on the recommended choice, no outline`, async () => {
      const el = await mount([view(over)]);
      const yes = choice(el, 'yes');
      expectMuted(yes);
      expect(yes.hasAttribute('outline')).toBe(false);
      expectUnmarked(choice(el, 'no'));
    });
  }

  it('keeps the picked choice filled with its check when it is not the recommendation', async () => {
    const el = await mount([view({ status: 'decided', decision: answer('no') })]);
    const no = choice(el, 'no');
    expect(no.getAttribute('variant')).toBe('primary');
    expect(no.querySelector('sl-icon[name="check-circle-fill"]')).not.toBeNull();
    expect(choice(el, 'yes').getAttribute('variant')).toBe('default');
  });

  it('marks a picked recommendation filled and muted-marked', async () => {
    const el = await mount([view({ status: 'decided', decision: answer('yes') })]);
    const yes = choice(el, 'yes');
    expect(yes.getAttribute('variant')).toBe('primary');
    expect(yes.querySelector('sl-icon[name="check-circle-fill"]')).not.toBeNull();
    expectMuted(yes);
  });

  it('pending keeps the outline and no visible muted marker', async () => {
    const el = await mount([view()]);
    const yes = choice(el, 'yes');
    expect(yes.hasAttribute('outline')).toBe(true);
    expect(yes.querySelector('[data-rec-mark]')).toBeNull();
    expect(text(yes.querySelector('.sr-only'))).toBe('(recommended)');
  });

  it('marks nothing without a recommendation', async () => {
    const el = await mount([
      view({ defaultChoiceId: '', status: 'decided', decision: answer('no') }),
    ]);
    expectUnmarked(choice(el, 'yes'));
    expectUnmarked(choice(el, 'no'));
  });
});

describe('single card stacked rows (choices with descriptions)', () => {
  for (const [name, over] of TERMINAL) {
    it(`${name}: muted marker, not the pending .recd outline`, async () => {
      const el = await mount([view({ choices: STACKED, ...over })]);
      const yes = choice(el, 'yes');
      expect(yes.classList.contains('opt')).toBe(true);
      expectMuted(yes);
      expect(yes.classList.contains('recd')).toBe(false);
      expectUnmarked(choice(el, 'no'));
    });
  }
});

describe('set card', () => {
  function row(rowId: string, key: string, over: Partial<DecisionView> = {}): DecisionView {
    return view({ rowId, key, ref: key, ...over });
  }

  for (const [name, over] of TERMINAL) {
    it(`${name} row: muted marker, never outlined`, async () => {
      const el = await mount([row('r1', 'D1', over), row('r2', 'D2', over)], true);
      const yes = choice(el, 'yes', 'r1');
      expectMuted(yes);
      expect(yes.getAttribute('data-look')).not.toBe('outline');
      expectUnmarked(choice(el, 'no', 'r1'));
    });
  }

  it('stacked set rows show the muted marker without .recd', async () => {
    const decided = { status: 'decided', decision: answer('no'), choices: STACKED } as const;
    const el = await mount([row('r1', 'D1', decided), row('r2', 'D2', decided)], true);
    const yes = choice(el, 'yes', 'r1');
    expect(yes.classList.contains('opt')).toBe(true);
    expectMuted(yes);
    expect(yes.classList.contains('recd')).toBe(false);
  });

  it('pending set rows keep the outline without the muted marker', async () => {
    const el = await mount(
      [row('r1', 'D1'), row('r2', 'D2', { status: 'decided', decision: answer('no') })],
      true
    );
    const pendingYes = choice(el, 'yes', 'r1');
    expect(pendingYes.querySelector('[data-rec-mark]')).toBeNull();
    expectMuted(choice(el, 'yes', 'r2'));
  });
});
