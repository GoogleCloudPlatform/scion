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
 * `<scion-chat-choices>` in set mode (ptone/scion#7673, design §4.2, §4.3,
 * §4.7): defaults pre-selected, local selection, the "N remaining" counter,
 * Submit gating and its missing-keys hint, the set POST, the submitted
 * footer, 409 answer_set_changed, rows answered elsewhere, and replaced
 * rows left out of a smaller set.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';
import type { ChoiceAnsweredDetail } from '../../../client/chat-choices.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
const showToast = vi.hoisted(() => vi.fn());

vi.mock('../../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../../client/api.js')>()),
  apiFetch,
}));
vi.mock('../../../utils/toast.js', () => ({ showToast }));

await import('./chat-choices.js');
type ScionChatChoices = import('./chat-choices.js').ScionChatChoices;

const AT = '2026-11-12T15:44:00Z';

function row(rowId: string, key: string, over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId,
    messageId: 'm1',
    key,
    ref: key,
    version: 1,
    status: 'pending',
    askerAgentId: 'a1',
    askerAgentName: 'release-bot',
    question: `Question ${key}?`,
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
    ],
    ...over,
  };
}

/** The mock's set: D234 and ship-now recommend a choice, 17 does not. */
function set(): DecisionView[] {
  return [
    row('r1', 'D234', { defaultChoiceId: 'yes' }),
    row('r2', '17', {
      choices: [
        { id: 'a', label: 'a · Extend input-needed' },
        { id: 'b', label: 'b · New waiting state' },
      ],
    }),
    row('r3', 'ship-now', { defaultChoiceId: 'no' }),
  ];
}

function decidedBy(
  d: DecisionView,
  choiceId: string,
  by: { id: string; name: string },
  batchId = 'batch-1'
): DecisionView {
  return {
    ...d,
    status: 'decided',
    decision: {
      choiceId,
      label: choiceId,
      answeredByUserId: by.id,
      answeredByName: by.name,
      provenance: 'direct',
      surface: 'web',
      responseMessageId: 'resp-1',
      batchId,
      at: AT,
    },
  };
}

function replaced(d: DecisionView): DecisionView {
  return {
    ...d,
    status: 'replaced',
    replacedBy: { rowId: `${d.rowId}-v2`, messageId: 'm2', version: 2, at: '2026-11-12T15:43:00Z' },
  };
}

const ME = { id: 'u-me', name: 'alice' };
const BOB = { id: 'u-bob', name: 'Bob' };

function res(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

async function mount(decisions: DecisionView[]): Promise<ScionChatChoices> {
  const el = document.createElement('scion-chat-choices');
  el.messageId = 'm1';
  el.currentUserId = ME.id;
  el.answerMode = 'set';
  el.decisions = decisions;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatChoices): ShadowRoot => el.shadowRoot!;
const text = (n: Element | null | undefined): string =>
  n?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
const rowEl = (el: ScionChatChoices, rowId: string): HTMLElement =>
  root(el).querySelector<HTMLElement>(`.row[data-row="${rowId}"]`)!;
const choice = (el: ScionChatChoices, rowId: string, id: string): HTMLElement =>
  rowEl(el, rowId).querySelector<HTMLElement>(`[data-choice="${id}"]`)!;
const submit = (el: ScionChatChoices): HTMLElement =>
  root(el).querySelector<HTMLElement>('sl-button.submit')!;
const counter = (el: ScionChatChoices): string => text(root(el).querySelector('.counter'));

async function settle(el: ScionChatChoices): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

async function click(el: ScionChatChoices, node: HTMLElement): Promise<void> {
  node.click();
  await settle(el);
}

beforeEach(() => {
  apiFetch.mockReset();
  showToast.mockReset();
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('set card, pending', () => {
  it('renders the header with the count and every key badge', async () => {
    const el = await mount(set());
    const head = root(el).querySelector('.card-head')!;
    expect(text(head.querySelector('.title'))).toBe('3 decisions');
    expect(Array.from(head.querySelectorAll('.key')).map((k) => k.textContent)).toEqual([
      'D234',
      '17',
      'ship-now',
    ]);
    expect(head.querySelector('sl-icon')?.getAttribute('name')).toBe('list-check');
    // Each row carries its own key badge.
    expect(text(rowEl(el, 'r2').querySelector('.row-top .key'))).toBe('17');
  });

  it('treats 2+ decisions as a set even without answerMode', async () => {
    const el = document.createElement('scion-chat-choices');
    el.decisions = set();
    document.body.appendChild(el);
    await el.updateComplete;
    expect(root(el).querySelector('.card.set')).not.toBeNull();
  });

  it('pre-selects recommended choices: filled with check-lg, aria-pressed', async () => {
    const el = await mount(set());
    const yes = choice(el, 'r1', 'yes');
    // A native toggle button, so aria-pressed reaches assistive technology
    // (sl-button keeps it on its host, where it is not exposed).
    expect(yes.tagName).toBe('BUTTON');
    expect((yes as HTMLButtonElement).type).toBe('button');
    expect(yes.getAttribute('aria-pressed')).toBe('true');
    expect(yes.getAttribute('data-look')).toBe('filled');
    expect(yes.querySelector('sl-icon')?.getAttribute('name')).toBe('check-lg');
    const no = choice(el, 'r1', 'no');
    expect(no.tagName).toBe('BUTTON');
    expect(no.getAttribute('data-look')).toBe('default');
    expect(no.getAttribute('aria-pressed')).toBe('false');
  });

  it('exposes the selection on the buttons themselves, not on a wrapper', async () => {
    const el = await mount(set());
    const pressed = Array.from(
      root(el).querySelectorAll<HTMLElement>('.row [aria-pressed="true"]')
    ).map((b) => `${b.tagName}:${b.closest('.row')!.getAttribute('data-row')}:${b.dataset.choice}`);
    expect(pressed).toEqual(['BUTTON:r1:yes', 'BUTTON:r3:no']);
    expect(root(el).querySelector('sl-button[aria-pressed]')).toBeNull();
  });

  it('counts rows without a selection in warning colour and disables Submit', async () => {
    const el = await mount(set());
    expect(counter(el)).toBe('1 remaining');
    expect(root(el).querySelector('.counter')!.classList.contains('missing')).toBe(true);
    expect(submit(el).hasAttribute('disabled')).toBe(true);
  });

  it('names the missing keys in the Submit tooltip and its aria-describedby hint', async () => {
    const el = await mount(set());
    const tooltip = root(el).querySelector('sl-tooltip')!;
    expect(tooltip.getAttribute('content')).toBe('Choose an answer for 17 first');
    const hintId = submit(el).getAttribute('aria-describedby')!;
    expect(text(root(el).getElementById(hintId))).toBe('Choose an answer for 17 first');
    // The counter is count only (ptone 7b355253).
    expect(counter(el)).not.toContain('17');
  });

  it('a click only selects: nothing is sent, the counter drops, Submit enables', async () => {
    const el = await mount(set());
    await click(el, choice(el, 'r2', 'a'));
    expect(apiFetch).not.toHaveBeenCalled();
    expect(choice(el, 'r2', 'a').getAttribute('aria-pressed')).toBe('true');
    expect(counter(el)).toBe('0 remaining');
    expect(root(el).querySelector('.counter')!.classList.contains('missing')).toBe(false);
    expect(submit(el).hasAttribute('disabled')).toBe(false);
    expect(root(el).querySelector('sl-tooltip')).toBeNull();
    expect(submit(el).hasAttribute('aria-describedby')).toBe(false);
  });

  it('a recommended choice returns to outlined when another is picked', async () => {
    const el = await mount(set());
    await click(el, choice(el, 'r3', 'yes'));
    const no = choice(el, 'r3', 'no');
    expect(no.getAttribute('data-look')).toBe('outline');
    expect(no.getAttribute('aria-pressed')).toBe('false');
    expect(choice(el, 'r3', 'yes').getAttribute('aria-pressed')).toBe('true');
  });

  it('labels the set and each row as groups, with no card-level Enter', async () => {
    const el = await mount(set());
    const card = root(el).querySelector<HTMLElement>('.card')!;
    expect(card.getAttribute('role')).toBe('group');
    expect(card.getAttribute('aria-label')).toBe('3 decisions from release-bot');
    expect(card.hasAttribute('tabindex')).toBe(false);
    expect(rowEl(el, 'r1').getAttribute('role')).toBe('group');
    expect(rowEl(el, 'r1').getAttribute('aria-label')).toBe('Question D234?');

    card.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('stacks choices with descriptions and selects them the same way', async () => {
    const d = set();
    d[1] = row('r2', '17', {
      choices: [
        { id: 'a', label: 'Extend', description: 'Add a `timeout` field.' },
        { id: 'b', label: 'New state', description: 'Touches web and hub.' },
      ],
    });
    const el = await mount(d);
    expect(rowEl(el, 'r2').querySelector('.choices.stack')).not.toBeNull();
    await click(el, choice(el, 'r2', 'b'));
    const b = choice(el, 'r2', 'b');
    expect(b.getAttribute('aria-pressed')).toBe('true');
    expect(b.classList.contains('chosen')).toBe(true);
    expect(b.querySelector('sl-icon')?.getAttribute('name')).toBe('check-lg');
    expect(counter(el)).toBe('0 remaining');
  });
});

describe('set card, submit', () => {
  it('POSTs every actionable selection, then shows each row answered and the submitted footer', async () => {
    const answeredViews = set().map((d, i) => decidedBy(d, ['yes', 'a', 'no'][i]!, ME));
    let release: (r: Response) => void = () => {};
    apiFetch.mockReturnValueOnce(new Promise((r) => (release = r)));
    const el = await mount(set());
    const events: ChoiceAnsweredDetail[] = [];
    el.addEventListener('choice-answered', (e) =>
      events.push((e as CustomEvent<ChoiceAnsweredDetail>).detail)
    );
    await click(el, choice(el, 'r2', 'a'));
    await click(el, submit(el));

    const [path, init] = apiFetch.mock.calls[0]!;
    expect(path).toBe('/api/v1/choice-prompts/m1/answers');
    expect(init?.method).toBe('POST');
    const body = JSON.parse(init!.body as string);
    expect(body.answers).toEqual([
      { decision_row_id: 'r1', choice_id: 'yes' },
      { decision_row_id: 'r2', choice_id: 'a' },
      { decision_row_id: 'r3', choice_id: 'no' },
    ]);
    expect(typeof body.idempotency_key).toBe('string');

    // While in flight: Submit loading, choices disabled.
    expect(submit(el).hasAttribute('loading')).toBe(true);
    expect(choice(el, 'r1', 'no').hasAttribute('disabled')).toBe(true);

    release(
      res(200, { responseMessageId: 'resp-1', batchId: 'batch-1', decisions: answeredViews })
    );
    await settle(el);

    for (const id of ['r1', 'r2', 'r3']) {
      expect(text(rowEl(el, id).querySelector('.row-foot'))).toMatch(
        /^Answered by you · \d\d:\d\d$/
      );
    }
    expect(choice(el, 'r2', 'a').querySelector('sl-icon')?.getAttribute('name')).toBe(
      'check-circle-fill'
    );
    expect(choice(el, 'r2', 'b').hasAttribute('disabled')).toBe(true);
    expect(counter(el)).toMatch(/^Submitted by you · \d\d:\d\d$/);
    expect(submit(el)).toBeNull();
    expect(events).toEqual([
      { messageId: 'm1', decisions: answeredViews, responseMessageId: 'resp-1' },
    ]);
  });

  it('shows another user’s submit as "Submitted by <name>"', async () => {
    const el = await mount(set().map((d) => decidedBy(d, 'yes', BOB)));
    expect(counter(el)).toMatch(/^Submitted by Bob · \d\d:\d\d$/);
    // Loaded already answered: no concurrent-answer note.
    expect(root(el).querySelector('.note')).toBeNull();
  });

  it('re-enables the card with a toast on other errors', async () => {
    apiFetch.mockResolvedValueOnce(res(500, { error: { code: 'internal', message: 'boom' } }));
    const el = await mount(set());
    await click(el, choice(el, 'r2', 'b'));
    await click(el, submit(el));
    expect(showToast).toHaveBeenCalledWith(expect.any(String), 'danger');
    expect(submit(el).hasAttribute('disabled')).toBe(false);
    expect(submit(el).hasAttribute('loading')).toBe(false);
    expect(choice(el, 'r2', 'b').getAttribute('aria-pressed')).toBe('true');
  });
});

describe('set card, 409 answer_set_changed', () => {
  function conflict(decisions: DecisionView[], reasons: Record<string, string>): Response {
    return res(409, {
      error: {
        code: 'answer_set_changed',
        message: 'the decisions to answer changed; refresh and submit again',
        details: { decisions, reasons },
      },
    });
  }

  it('takes the fresh views, keeps still-valid selections and names changed keys inline', async () => {
    const fresh = set();
    fresh[1] = decidedBy(fresh[1]!, 'b', BOB, 'batch-bob');
    apiFetch.mockResolvedValueOnce(conflict(fresh, { r2: 'decided' }));
    const el = await mount(set());
    await click(el, choice(el, 'r1', 'no'));
    await click(el, choice(el, 'r2', 'a'));
    await click(el, submit(el));

    const alert = root(el).querySelector('sl-alert')!;
    expect(alert.getAttribute('variant')).toBe('warning');
    expect(text(alert)).toBe(
      'This changed while you were choosing: 17 was answered by Bob. Your other selections are kept — review and submit again.'
    );
    expect(text(alert.querySelector('b'))).toBe('17');
    // Kept: the viewer's pick on D234; dropped: the pick on 17.
    expect(choice(el, 'r1', 'no').getAttribute('aria-pressed')).toBe('true');
    expect(text(rowEl(el, 'r2').querySelector('.row-foot'))).toMatch(/^Answered by Bob/);
    expect(counter(el)).toBe('0 remaining · 1 answered by Bob');
    expect(submit(el).hasAttribute('disabled')).toBe(false);
    expect(showToast).not.toHaveBeenCalled();
  });

  it('keeps the alert until the next submit, which sends the smaller set', async () => {
    const fresh = set();
    fresh[1] = replaced(fresh[1]!);
    apiFetch.mockResolvedValueOnce(conflict(fresh, { r2: 'replaced' }));
    const el = await mount(set());
    await click(el, choice(el, 'r2', 'a'));
    await click(el, submit(el));
    expect(text(root(el).querySelector('sl-alert'))).toContain('17 was replaced');

    // A selection change does not clear it.
    await click(el, choice(el, 'r1', 'no'));
    expect(root(el).querySelector('sl-alert')).not.toBeNull();

    apiFetch.mockResolvedValueOnce(
      res(200, {
        responseMessageId: 'resp-2',
        batchId: 'batch-2',
        decisions: [
          decidedBy(fresh[0]!, 'no', ME, 'b2'),
          fresh[1],
          decidedBy(fresh[2]!, 'no', ME, 'b2'),
        ],
      })
    );
    await click(el, submit(el));
    const body = JSON.parse(apiFetch.mock.calls[1]![1]!.body as string);
    expect(body.answers.map((a: { decision_row_id: string }) => a.decision_row_id)).toEqual([
      'r1',
      'r3',
    ]);
    expect(root(el).querySelector('sl-alert')).toBeNull();
  });

  it('maps every hub reason to a phrase', async () => {
    const fresh = [
      replaced(row('r1', 'A')),
      decidedBy(row('r2', 'B'), 'yes', BOB),
      row('r3', 'C', { status: 'decided' }),
      row('r4', 'D', { defaultChoiceId: 'yes' }),
    ];
    apiFetch.mockResolvedValueOnce(
      conflict(fresh, {
        r1: 'replaced',
        r2: 'not_recorded_answerer',
        r3: 'not_actionable',
        r4: 'missing',
      })
    );
    const el = await mount([
      row('r1', 'A', { defaultChoiceId: 'yes' }),
      row('r2', 'B', { defaultChoiceId: 'yes' }),
      row('r3', 'C', { defaultChoiceId: 'yes' }),
      row('r4', 'D', { defaultChoiceId: 'yes' }),
    ]);
    await click(el, submit(el));
    expect(text(root(el).querySelector('sl-alert'))).toBe(
      'This changed while you were choosing: A was replaced, B was answered by Bob, C is no longer needed, D still needs an answer. Your other selections are kept — review and submit again.'
    );
  });

  it('says nothing was sent when no row remains actionable', async () => {
    const fresh = set().map((d) => decidedBy(d, 'yes', BOB, 'batch-bob'));
    apiFetch.mockResolvedValueOnce(
      conflict(fresh, { r1: 'decided', r2: 'decided', r3: 'decided' })
    );
    const el = await mount(set());
    await click(el, choice(el, 'r2', 'a'));
    await click(el, submit(el));
    expect(text(root(el).querySelector('sl-alert'))).toMatch(/Your selections were not sent\.$/);
    expect(submit(el)).toBeNull();
  });
});

describe('set card, answered elsewhere while choosing', () => {
  it('locks a row answered by someone else and shrinks the set', async () => {
    const el = await mount(set());
    const next = set();
    next[1] = decidedBy(next[1]!, 'b', BOB, 'batch-bob');
    el.decisions = next;
    await settle(el);
    expect(choice(el, 'r2', 'a').hasAttribute('disabled')).toBe(true);
    expect(counter(el)).toBe('0 remaining · 1 answered by Bob');
    expect(submit(el).hasAttribute('disabled')).toBe(false);
    expect(root(el).querySelector('.note')).toBeNull();
  });

  it('shows the not-sent note when no actionable row remains', async () => {
    const el = await mount(set());
    await click(el, choice(el, 'r2', 'a'));
    el.decisions = set().map((d) => decidedBy(d, 'no', BOB, 'batch-bob'));
    await settle(el);
    expect(text(root(el).querySelector('.note'))).toBe(
      'Bob answered while you were choosing; your selections were not sent.'
    );
    expect(counter(el)).toMatch(/^Submitted by Bob/);
    expect(submit(el)).toBeNull();
  });

  it('shows no note when the viewer answered in another tab', async () => {
    const el = await mount(set());
    el.decisions = set().map((d) => decidedBy(d, 'no', ME));
    await settle(el);
    expect(root(el).querySelector('.note')).toBeNull();
    expect(counter(el)).toMatch(/^Submitted by you/);
  });
});

describe('set card, replaced rows', () => {
  it('disables and fades a replaced row, links its new version, and leaves it out', async () => {
    const d = set();
    d[1] = replaced(d[1]!);
    const el = await mount(d);
    const r2 = rowEl(el, 'r2');
    expect(r2.classList.contains('replaced')).toBe(true);
    expect(choice(el, 'r2', 'a').hasAttribute('disabled')).toBe(true);
    expect(text(r2.querySelector('.row-foot'))).toMatch(
      /^Replaced by v2 · \d\d:\d\d · Jump to v2$/
    );
    expect(counter(el)).toBe('0 remaining · 1 replaced');
    expect(submit(el).hasAttribute('disabled')).toBe(false);

    const jumps: string[] = [];
    el.addEventListener('choice-jump', (e) =>
      jumps.push((e as CustomEvent<{ messageId: string }>).detail.messageId)
    );
    await click(el, r2.querySelector<HTMLElement>('a.jump')!);
    expect(jumps).toEqual(['m2']);
  });

  it('submits the remaining rows as a smaller set', async () => {
    const d = set();
    d[1] = replaced(d[1]!);
    apiFetch.mockResolvedValueOnce(
      res(200, { responseMessageId: 'r', batchId: 'b', decisions: d })
    );
    const el = await mount(d);
    await click(el, submit(el));
    const body = JSON.parse(apiFetch.mock.calls[0]![1]!.body as string);
    expect(body.answers).toEqual([
      { decision_row_id: 'r1', choice_id: 'yes' },
      { decision_row_id: 'r3', choice_id: 'no' },
    ]);
  });

  it('shows what happened when every row is replaced', async () => {
    const el = await mount(set().map(replaced));
    expect(counter(el)).toBe('3 replaced');
    expect(submit(el)).toBeNull();
  });
});

describe('Phase 2: set card interpreted pre-selection, chips, and multi-user exclusion (Mocks P2-4, P2-5)', () => {
  function interpretedRow(
    d: DecisionView,
    choiceId: string,
    by: { id: string; name: string },
    sourceMessageId = 'msg-src-1'
  ): DecisionView {
    const opt = d.choices.find((c) => c.id === choiceId);
    return {
      ...d,
      status: 'decided',
      decision: {
        choiceId,
        label: opt?.label ?? choiceId,
        answeredByUserId: by.id,
        answeredByName: by.name,
        provenance: 'agent_interpreted',
        outcome: 'answer',
        interpretedByAgentId: 'a1',
        sourceMessageId,
        surface: 'web',
        responseMessageId: 'resp-interp',
        at: AT,
      },
    };
  }

  it('P2-4: pre-selects viewer interpreted choice (beating defaultChoiceId), renders native aria-pressed button + will confirm chip, and counts 2 of 2 ready · 1 confirm', async () => {
    // Row 1 recommends "no", but agent interpreted viewer's reply as "yes" -> "yes" wins!
    const d1 = interpretedRow(row('r1', 'D234', { defaultChoiceId: 'no' }), 'yes', ME);
    const d2 = row('r2', '17', {
      defaultChoiceId: 'a',
      choices: [
        { id: 'a', label: 'a · Extend input-needed' },
        { id: 'b', label: 'b · New waiting state' },
      ],
    });
    const el = await mount([d1, d2]);

    const r1 = rowEl(el, 'r1');
    expect(text(r1.querySelector('.chip.confirm'))).toBe('will confirm');
    const yesBtn = choice(el, 'r1', 'yes');
    const noBtn = choice(el, 'r1', 'no');
    expect(yesBtn.tagName).toBe('BUTTON');
    expect(yesBtn.getAttribute('aria-pressed')).toBe('true');
    expect(yesBtn.getAttribute('data-look')).toBe('filled');
    expect(yesBtn.querySelector('sl-icon')?.getAttribute('name')).toBe('robot');
    expect(noBtn.getAttribute('aria-pressed')).toBe('false');
    expect(noBtn.getAttribute('data-look')).toBe('default');
    expect(text(r1.querySelector('.row-foot'))).toMatch(
      /^Interpreted by release-bot from your reply · \d\d:\d\d · View reply$/
    );

    expect(counter(el)).toBe('2 of 2 ready · 1 confirm');
    expect(submit(el).hasAttribute('disabled')).toBe(false);

    // Submit mixed set (1 confirm + 1 pending answer).
    apiFetch.mockResolvedValueOnce(
      res(200, {
        responseMessageId: 'resp-set',
        batchId: 'b-set',
        decisions: [
          {
            ...d1,
            decision: {
              ...d1.decision!,
              provenance: 'direct',
              outcome: 'confirm',
              batchId: 'b-set',
            },
          },
          decidedBy(d2, 'a', ME, 'b-set'),
        ],
      })
    );
    await click(el, submit(el));
    const body = JSON.parse(apiFetch.mock.calls[0]![1]!.body as string);
    expect(body.answers).toEqual([
      { decision_row_id: 'r1', choice_id: 'yes' },
      { decision_row_id: 'r2', choice_id: 'a' },
    ]);
  });

  it('P2-5: changing viewer interpreted row shows will change + subtle robot outline, while excluding another user interpreted row from counter and POST body', async () => {
    const d1 = interpretedRow(row('r1', 'D234'), 'yes', ME);
    const d2 = row('r2', '17', {
      defaultChoiceId: 'a',
      choices: [
        { id: 'a', label: 'a · Extend input-needed' },
        { id: 'b', label: 'b · New waiting state' },
      ],
    });
    const d3 = interpretedRow(row('r3', 'ship-now'), 'yes', BOB);
    const el = await mount([d1, d2, d3]);

    // Bob's row is read-only, has tooltip & chip, and is excluded from counter.
    const r3 = rowEl(el, 'r3');
    expect(r3.classList.contains('readonly')).toBe(true);
    expect(text(r3.querySelector('.chip.interp'))).toBe("Bob's answer");
    expect(r3.querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      'Only Bob can confirm or change this answer'
    );
    expect(choice(el, 'r3', 'yes').hasAttribute('disabled')).toBe(true);
    expect(choice(el, 'r3', 'yes').hasAttribute('aria-pressed')).toBe(false);
    expect(choice(el, 'r3', 'no').hasAttribute('disabled')).toBe(true);
    expect(counter(el)).toBe('2 of 2 ready · 1 confirm');

    // Viewer changes D234 from Yes -> No.
    await click(el, choice(el, 'r1', 'no'));
    const r1 = rowEl(el, 'r1');
    expect(text(r1.querySelector('.chip.change'))).toBe('will change');
    expect(choice(el, 'r1', 'yes').getAttribute('data-look')).toBe('subtle');
    expect(choice(el, 'r1', 'yes').getAttribute('aria-pressed')).toBe('false');
    expect(choice(el, 'r1', 'yes').querySelector('sl-icon')?.getAttribute('name')).toBe('robot');
    expect(choice(el, 'r1', 'no').getAttribute('data-look')).toBe('filled');
    expect(choice(el, 'r1', 'no').getAttribute('aria-pressed')).toBe('true');
    expect(choice(el, 'r1', 'no').querySelector('sl-icon')?.getAttribute('name')).toBe('check-lg');
    expect(text(r1.querySelector('.row-foot'))).toBe(
      'Interpreted as Yes by release-bot from your reply · View reply'
    );
    expect(counter(el)).toBe('2 of 2 ready · 1 change');

    // Unrelated SSE update on r3 preserves viewer's manual pick on r1.
    el.decisions = [d1, d2, decidedBy(d3, 'yes', BOB, 'b-bob')];
    await settle(el);
    expect(choice(el, 'r1', 'no').getAttribute('aria-pressed')).toBe('true');

    // Submit sends ONLY r1 and r2 (excludes Bob's r3).
    apiFetch.mockResolvedValueOnce(
      res(200, {
        responseMessageId: 'resp-mix',
        batchId: 'b-mix',
        decisions: [
          {
            ...d1,
            decision: {
              ...d1.decision!,
              choiceId: 'no',
              label: 'No',
              previousChoiceLabel: 'Yes',
              provenance: 'direct',
              outcome: 'override',
              batchId: 'b-mix',
            },
          },
          decidedBy(d2, 'a', ME, 'b-mix'),
          decidedBy(d3, 'yes', BOB, 'b-bob'),
        ],
      })
    );
    await click(el, submit(el));
    const body = JSON.parse(apiFetch.mock.calls[0]![1]!.body as string);
    expect(body.answers).toEqual([
      { decision_row_id: 'r1', choice_id: 'no' },
      { decision_row_id: 'r2', choice_id: 'a' },
    ]);
  });

  it('supports all-confirm set submit and preserves picks on actionable interpreted rows across 409 answer_set_changed', async () => {
    const d1 = interpretedRow(row('r1', 'D234'), 'yes', ME);
    const d2 = interpretedRow(row('r2', '17'), 'no', ME);
    const d3 = row('r3', 'ship-now', { defaultChoiceId: 'yes' });
    const el = await mount([d1, d2]);
    expect(counter(el)).toBe('2 of 2 ready · 2 confirms');

    // Now test 409 answer_set_changed where r3 became Bob's interpreted row while viewer changed r1 -> no.
    const elRace = await mount([d1, d2, d3]);
    await click(elRace, choice(elRace, 'r1', 'no'));
    expect(counter(elRace)).toBe('3 of 3 ready · 1 confirm · 1 change');

    const freshR3 = interpretedRow(d3, 'yes', BOB);
    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'answer_set_changed',
          message: 'the decisions to answer changed; refresh and submit again',
          details: {
            decisions: [d1, d2, freshR3],
            reasons: { r3: 'not_recorded_answerer' },
          },
        },
      })
    );
    await click(elRace, submit(elRace));

    expect(text(root(elRace).querySelector('sl-alert.changed'))).toContain(
      'ship-now was answered by Bob. Your other selections are kept — review and submit again.'
    );
    expect(choice(elRace, 'r1', 'no').getAttribute('aria-pressed')).toBe('true');
    expect(counter(elRace)).toBe('2 of 2 ready · 1 confirm · 1 change');
  });

  it('keeps replaced and other-user direct decided counts in the "M of M ready" counter while omitting not_recorded_answerer rows', async () => {
    const d1 = interpretedRow(row('r1', 'D234'), 'yes', ME);
    const d2 = decidedBy(row('r2', 'D-direct'), 'no', BOB, 'b-bob');
    const d3 = interpretedRow(row('r3', 'D-carol'), 'yes', { id: 'u-carol', name: 'Carol' });
    const d4 = replaced(row('r4', 'D-repl'));
    const el = await mount([d1, d2, d3, d4]);

    expect(counter(el)).toBe('1 of 1 ready · 1 confirm · 1 answered by Bob · 1 replaced');
  });

  it('stacked (described-choice) interpreted set rows disable non-owner buttons natively and toggle aria-pressed for owner', async () => {
    const stackedChoices = [
      { id: 'yes', label: 'Yes', description: 'Deploy right away.' },
      { id: 'no', label: 'No', description: 'Hold until Monday.' },
    ];
    const d1 = interpretedRow(row('r1', 'D-me', { choices: stackedChoices }), 'yes', ME);
    const d2 = interpretedRow(row('r2', 'D-bob', { choices: stackedChoices }), 'yes', BOB);
    const el = await mount([d1, d2]);

    for (const id of ['yes', 'no']) {
      const opt = choice(el, 'r2', id);
      expect(opt.classList.contains('opt')).toBe(true);
      expect(opt.hasAttribute('disabled')).toBe(true);
      expect(opt.getAttribute('title')).toBe('Only Bob can confirm or change this answer');
    }
    await click(el, choice(el, 'r2', 'no'));
    expect(choice(el, 'r2', 'no').hasAttribute('aria-pressed')).toBe(false);
    expect(text(rowEl(el, 'r2').querySelector('.chip.interp'))).toBe("Bob's answer");

    expect(choice(el, 'r1', 'yes').hasAttribute('disabled')).toBe(false);
    expect(choice(el, 'r1', 'yes').getAttribute('aria-pressed')).toBe('true');
    await click(el, choice(el, 'r1', 'no'));
    expect(choice(el, 'r1', 'no').getAttribute('aria-pressed')).toBe('true');
    expect(choice(el, 'r1', 'yes').getAttribute('aria-pressed')).toBe('false');
    expect(text(rowEl(el, 'r1').querySelector('.chip.change'))).toBe('will change');
    expect(counter(el)).toBe('1 of 1 ready · 1 change');
  });

  it('forwards surface="decisions_page" on set submit', async () => {
    const [d1, d2] = [row('r1', 'D234', { defaultChoiceId: 'yes' }), row('r2', '17', { defaultChoiceId: 'no' })];
    const el = await mount([d1, d2]);
    el.surface = 'decisions_page';
    await el.updateComplete;
    apiFetch.mockResolvedValueOnce(
      res(200, {
        responseMessageId: 'resp-set',
        batchId: 'b-set',
        decisions: [decidedBy(d1, 'yes', ME, 'b-set'), decidedBy(d2, 'no', ME, 'b-set')],
      })
    );
    await click(el, submit(el));
    const body = JSON.parse(apiFetch.mock.calls[0]![1]?.body as string);
    expect(body).toMatchObject({
      surface: 'decisions_page',
      answers: [
        { decision_row_id: 'r1', choice_id: 'yes' },
        { decision_row_id: 'r2', choice_id: 'no' },
      ],
    });
  });
});
