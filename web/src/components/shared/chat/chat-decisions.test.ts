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
 * `<scion-chat-choices>` in single mode (ptone/scion#7673, design §4.2,
 * §4.3, §4.7): pending with the recommended choice outlined, submitting,
 * answered, the 409 and error toasts, and Enter on the focused card.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';
import type { ChoiceAnsweredDetail } from '../../../client/chat-choices.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
const showToast = vi.hoisted(() => vi.fn());

vi.mock('../../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../../client/api.js')>()),
  apiFetch,
}));
vi.mock('../../../utils/toast.js', () => ({ showToast }));

await import('./chat-choices.js');
type ScionChatChoices = import('./chat-choices.js').ScionChatChoices;

function view(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r1',
    messageId: 'm1',
    key: 'D234',
    ref: 'D234',
    version: 1,
    status: 'pending',
    askerAgentId: 'a1',
    askerAgentName: 'release-bot',
    question: 'Ship `v0.9` today?',
    defaultChoiceId: 'yes',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
      { id: 'wait', label: 'Wait for #812' },
    ],
    ...over,
  };
}

function noDefault(): DecisionView {
  const { defaultChoiceId: _unused, ...rest } = view();
  return rest;
}

function answered(choiceId: string, by: { id: string; name: string }): DecisionView {
  return view({
    status: 'decided',
    decision: {
      choiceId,
      label: choiceId,
      answeredByUserId: by.id,
      answeredByName: by.name,
      provenance: 'direct',
      surface: 'web',
      responseMessageId: 'resp-1',
      at: '2026-11-12T15:42:00Z',
    },
  });
}

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
  el.currentUserId = 'u-me';
  el.decisions = decisions;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatChoices): ShadowRoot => el.shadowRoot!;
const button = (el: ScionChatChoices, id: string): HTMLElement =>
  root(el).querySelector<HTMLElement>(`[data-choice="${id}"]`)!;
const foot = (el: ScionChatChoices): string =>
  root(el).querySelector('.row-foot')?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
const card = (el: ScionChatChoices): HTMLElement => root(el).querySelector<HTMLElement>('.card')!;

async function settle(el: ScionChatChoices): Promise<void> {
  await getMarkdownRenderer();
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

beforeEach(() => {
  apiFetch.mockReset();
  showToast.mockReset();
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('pending', () => {
  it('shows the key, the question and the choices with the recommended one outlined', async () => {
    const el = await mount([view()]);
    await settle(el);

    expect(root(el).querySelector('.key')?.textContent).toBe('D234');
    // Question markdown is rendered (inline code), not shown raw.
    expect(root(el).querySelector('.q code')?.textContent).toBe('v0.9');
    const yes = button(el, 'yes');
    expect(yes.getAttribute('variant')).toBe('primary');
    expect(yes.hasAttribute('outline')).toBe(true);
    expect(yes.textContent).toContain('(recommended)');
    const no = button(el, 'no');
    expect(no.getAttribute('variant')).toBe('default');
    expect(no.hasAttribute('outline')).toBe(false);
    expect(no.hasAttribute('disabled')).toBe(false);
    expect(foot(el)).toBe('Awaiting your answer');
  });

  it('labels the card group and describes the Enter shortcut', async () => {
    const el = await mount([view()]);
    const c = card(el);
    expect(c.getAttribute('role')).toBe('group');
    expect(c.getAttribute('tabindex')).toBe('0');
    expect(c.getAttribute('aria-label')).toBe('Ship v0.9 today?');
    const hintId = c.getAttribute('aria-describedby');
    expect(hintId).toBeTruthy();
    expect(root(el).getElementById(hintId!)?.textContent).toContain(
      'Press Enter to choose the recommended answer (outlined).'
    );
  });

  it('has no Enter hint without a recommended choice', async () => {
    const el = await mount([noDefault()]);
    expect(card(el).hasAttribute('aria-describedby')).toBe(false);
    expect(root(el).querySelector('.hint')).toBeNull();
  });

  it('stacks choices as rows with descriptions when any choice has one', async () => {
    const el = await mount([
      view({
        defaultChoiceId: 'a',
        choices: [
          { id: 'a', label: 'Extend input-needed', description: 'Add a `timeout` field.' },
          { id: 'b', label: 'New waiting state' },
        ],
      }),
    ]);
    await settle(el);
    expect(root(el).querySelector('.choices.stack')).not.toBeNull();
    const a = button(el, 'a');
    expect(a.tagName).toBe('BUTTON');
    expect(a.classList.contains('recd')).toBe(true);
    expect(a.querySelector('.desc code')?.textContent).toBe('timeout');
    expect(button(el, 'b').classList.contains('recd')).toBe(false);
  });
});

describe('answering', () => {
  it('shows submitting, then the 200 view, and emits choice-answered', async () => {
    let resolve!: (r: Response) => void;
    apiFetch.mockImplementation(() => new Promise<Response>((r) => (resolve = r)));
    const el = await mount([view()]);
    const events: ChoiceAnsweredDetail[] = [];
    el.addEventListener('choice-answered', (e) =>
      events.push((e as CustomEvent<ChoiceAnsweredDetail>).detail)
    );

    button(el, 'no').click();
    await el.updateComplete;

    // Submitting: the clicked button spins, the others are disabled.
    expect(button(el, 'no').hasAttribute('loading')).toBe(true);
    expect(button(el, 'yes').hasAttribute('disabled')).toBe(true);
    expect(button(el, 'wait').hasAttribute('disabled')).toBe(true);
    expect(foot(el)).toBe('Sending…');
    const [path, init] = apiFetch.mock.calls[0]!;
    expect(path).toBe('/api/v1/decisions/r1/answer');
    const body = JSON.parse(init?.body as string) as { choice_id: string; idempotency_key: string };
    expect(body.choice_id).toBe('no');
    expect(body.idempotency_key).toBeTruthy();

    resolve(
      res(200, {
        decision: answered('no', { id: 'u-me', name: 'Me' }),
        responseMessageId: 'resp-1',
      })
    );
    await settle(el);

    const no = button(el, 'no');
    expect(no.getAttribute('variant')).toBe('primary');
    expect(no.hasAttribute('outline')).toBe(false);
    expect(no.querySelector('sl-icon[name="check-circle-fill"]')).not.toBeNull();
    expect(button(el, 'yes').hasAttribute('disabled')).toBe(true);
    // The recommended outline is a pending-only treatment.
    expect(button(el, 'yes').hasAttribute('outline')).toBe(false);
    expect(foot(el)).toMatch(/^Answered by you · \d{2}:\d{2}$/);
    expect(events).toEqual([
      {
        messageId: 'm1',
        decisions: [answered('no', { id: 'u-me', name: 'Me' })],
        responseMessageId: 'resp-1',
      },
    ]);
    expect(showToast).not.toHaveBeenCalled();
  });

  it('names another answerer', async () => {
    const el = await mount([answered('yes', { id: 'u-bob', name: 'Bob' })]);
    expect(foot(el)).toMatch(/^Answered by Bob · /);
    expect(button(el, 'no').hasAttribute('disabled')).toBe(true);
  });

  it('applies the winner and toasts on 409 already_answered', async () => {
    const winner = answered('yes', { id: 'u-bob', name: 'Bob' });
    apiFetch.mockResolvedValue(
      res(409, {
        error: {
          code: 'already_answered',
          message: 'already answered',
          details: { decision: winner },
        },
      })
    );
    const el = await mount([view()]);
    button(el, 'no').click();
    await settle(el);

    expect(showToast).toHaveBeenCalledWith('Already answered by Bob.', 'warning');
    expect(foot(el)).toMatch(/^Answered by Bob · /);
    expect(button(el, 'yes').getAttribute('variant')).toBe('primary');
  });

  it('toasts the replacement on 409 replaced', async () => {
    apiFetch.mockResolvedValue(
      res(409, {
        error: {
          code: 'replaced',
          message: 'replaced',
          details: {
            decision: view({ status: 'replaced' }),
            replacedBy: view({ rowId: 'r2', version: 2 }),
          },
        },
      })
    );
    const el = await mount([view()]);
    button(el, 'yes').click();
    await settle(el);

    expect(showToast).toHaveBeenCalledWith('Replaced by v2.', 'warning');
    expect(button(el, 'yes').hasAttribute('disabled')).toBe(true);
  });

  it('re-enables the card and toasts on an error', async () => {
    apiFetch.mockResolvedValue(
      res(500, { error: { code: 'internal', message: 'something broke' } })
    );
    const el = await mount([view()]);
    const events: unknown[] = [];
    el.addEventListener('choice-answered', (e) => events.push(e));
    button(el, 'no').click();
    await settle(el);

    expect(showToast).toHaveBeenCalledWith('something broke', 'danger');
    expect(button(el, 'no').hasAttribute('loading')).toBe(false);
    expect(button(el, 'no').hasAttribute('disabled')).toBe(false);
    expect(foot(el)).toBe('Awaiting your answer');
    expect(events).toHaveLength(0);
  });

  it('ignores a pending view that arrives after the answer (out of order)', async () => {
    apiFetch.mockResolvedValue(
      res(200, { decision: answered('no', { id: 'u-me', name: 'Me' }), responseMessageId: 'r' })
    );
    const el = await mount([view()]);
    button(el, 'no').click();
    await settle(el);
    el.decisions = [view()];
    await el.updateComplete;
    expect(foot(el)).toMatch(/^Answered by you/);
  });
});

describe('Enter on the focused card', () => {
  it('answers with the recommended choice', async () => {
    apiFetch.mockResolvedValue(
      res(200, { decision: answered('yes', { id: 'u-me', name: 'Me' }), responseMessageId: 'r' })
    );
    const el = await mount([view()]);
    const c = card(el);
    c.focus();
    c.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, composed: true }));
    await settle(el);

    expect(apiFetch).toHaveBeenCalledTimes(1);
    const body = JSON.parse(apiFetch.mock.calls[0]![1]?.body as string) as { choice_id: string };
    expect(body.choice_id).toBe('yes');
  });

  it('does nothing without a recommended choice', async () => {
    const el = await mount([noDefault()]);
    card(el).dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('leaves Enter on a button to the button', async () => {
    const el = await mount([view()]);
    button(el, 'no').dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, composed: true })
    );
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('does nothing once answered', async () => {
    const el = await mount([answered('yes', { id: 'u-me', name: 'Me' })]);
    card(el).dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });
});

describe('agent-written markdown in the card', () => {
  async function rendered(el: ScionChatChoices): Promise<void> {
    await vi.waitFor(
      () => {
        if (!root(el).querySelector('.desc strong')) throw new Error('markdown not rendered yet');
      },
      { timeout: 2000, interval: 10 }
    );
  }

  it('renders no link or image inside a choice button, so its text cannot open anything', async () => {
    const el = await mount([
      view({
        question: 'Ship it? See [the plan](https://example.com/plan).',
        choices: [
          {
            id: 'yes',
            label: 'Yes',
            description:
              '**Now.** See [the runbook](https://example.com/runbook) ![diagram](https://example.com/d.png)',
          },
          { id: 'no', label: 'No', description: 'Wait.' },
        ],
      }),
    ]);
    await rendered(el);

    const opt = button(el, 'yes');
    expect(opt.tagName).toBe('BUTTON');
    expect(opt.querySelector('a, img')).toBeNull();
    const desc = opt.querySelector('.desc')!;
    expect(desc.textContent).toContain('See the runbook diagram');
    // The question is not inside a control: its link stays.
    expect(root(el).querySelector('.q a')?.getAttribute('href')).toBe('https://example.com/plan');
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('drops images from elsewhere in the question', async () => {
    const el = await mount([
      view({
        question: 'Ship it? ![pixel](https://tracker.example/p.gif)',
        choices: [
          { id: 'yes', label: 'Yes', description: '**Now.**' },
          { id: 'no', label: 'No' },
        ],
      }),
    ]);
    await rendered(el);

    expect(root(el).querySelector('.q')?.textContent).toContain('Ship it?');
    expect(root(el).querySelector('img[src^="https://tracker.example"]')).toBeNull();
  });
});

describe('own answer racing a replacement', () => {
  it('keeps replaced when the own POST returns decided after the replaced event', async () => {
    let release: (r: Response) => void = () => {};
    apiFetch.mockReturnValue(new Promise<Response>((resolve) => (release = resolve)));
    const el = await mount([view()]);
    button(el, 'yes').click();
    await settle(el);

    // The --update lands first (SSE through the thread)...
    el.decisions = [
      view({
        status: 'replaced',
        replacedBy: { rowId: 'r1-v2', messageId: 'm2', version: 2, at: '2026-11-12T15:43:00Z' },
      }),
    ];
    await el.updateComplete;
    // ...then the POST that was answered just before it returns decided.
    release(
      res(200, {
        decision: answered('yes', { id: 'u-me', name: 'Me' }),
        responseMessageId: 'resp-1',
      })
    );
    await settle(el);

    expect(foot(el)).toMatch(/^Replaced by v2/);
    expect(root(el).querySelector('.row')?.getAttribute('data-status')).toBe('replaced');
  });
});

describe('Phase 2: single card interpreted, confirm, and change (Mocks P2-1, P2-2, P2-3, P2-6)', () => {
  function interpretedSingle(
    by: { id: string; name: string },
    over: Partial<DecisionView> = {}
  ): DecisionView {
    return view({
      status: 'decided',
      defaultChoiceId: 'no',
      decision: {
        choiceId: 'yes',
        label: 'Yes',
        answeredByUserId: by.id,
        answeredByName: by.name,
        provenance: 'agent_interpreted',
        outcome: 'answer',
        interpretedByAgentId: 'a1',
        sourceMessageId: 'msg-reply-1',
        surface: 'web',
        responseMessageId: 'resp-interp',
        at: '2026-11-12T15:44:00Z',
      },
      ...over,
    });
  }

  it('P2-1: recorded answerer sees interpreted badge, View reply, Confirm button, and Enter is a no-op', async () => {
    const el = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    await settle(el);

    expect(root(el).querySelector('.card-head .badge.interp')?.textContent?.trim()).toBe(
      'interpreted'
    );
    const yes = button(el, 'yes');
    expect(yes.getAttribute('variant')).toBe('primary');
    expect(yes.hasAttribute('outline')).toBe(false);
    expect(yes.hasAttribute('disabled')).toBe(false);
    expect(yes.querySelector('sl-icon[slot="prefix"]')?.getAttribute('name')).toBe('robot');

    // defaultChoiceId ("no") is NOT outlined on an interpreted card; all buttons stay enabled.
    const no = button(el, 'no');
    expect(no.getAttribute('variant')).toBe('default');
    expect(no.hasAttribute('outline')).toBe(false);
    expect(no.hasAttribute('disabled')).toBe(false);

    expect(foot(el)).toMatch(
      /^Interpreted by release-bot from your reply · \d\d:\d\d · View reply$/
    );
    const viewReply = root(el).querySelector<HTMLAnchorElement>('[data-action="view-reply"]')!;
    const scrolled: string[] = [];
    document.body.addEventListener('scroll-to-message', (e) =>
      scrolled.push((e as CustomEvent<{ messageId: string }>).detail.messageId)
    );
    const clickEvt = new MouseEvent('click', { bubbles: true, cancelable: true, composed: true });
    viewReply.dispatchEvent(clickEvt);
    expect(clickEvt.defaultPrevented).toBe(true);
    expect(scrolled).toEqual(['msg-reply-1']);

    const confirmBtn = root(el).querySelector<HTMLElement>('.confirm-row [data-action="confirm"]')!;
    expect(confirmBtn.textContent?.replace(/\s+/g, ' ').trim()).toBe('Confirm “Yes”');
    expect(root(el).querySelector('.hint')).toBeNull();

    // Enter on the focused card group is a no-op when status !== 'pending'.
    card(el).dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('P2-1 -> P2-3: clicking Confirm posts choice_id and transitions to confirmed badge & footer', async () => {
    const confirmedView: DecisionView = view({
      status: 'decided',
      decision: {
        choiceId: 'yes',
        label: 'Yes',
        answeredByUserId: 'u-me',
        answeredByName: 'Me',
        provenance: 'direct',
        outcome: 'confirm',
        surface: 'web',
        responseMessageId: 'resp-conf',
        at: '2026-11-12T15:52:00Z',
      },
    });
    apiFetch.mockResolvedValueOnce(
      res(200, { decision: confirmedView, responseMessageId: 'resp-conf' })
    );

    const el = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    const confirmBtn = root(el).querySelector<HTMLElement>('[data-action="confirm"]')!;
    confirmBtn.click();
    await settle(el);

    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(JSON.parse(apiFetch.mock.calls[0]![1]!.body as string).choice_id).toBe('yes');
    expect(root(el).querySelector('.card-head .badge.conf')?.textContent?.trim()).toBe('confirmed');
    expect(root(el).querySelector('.confirm-row')).toBeNull();
    expect(foot(el)).toMatch(
      /^Answered by you \(confirmed\) · \d\d:\d\d · first interpreted by release-bot from your reply$/
    );
  });

  it('P2-6 -> P2-3: picking a different choice opens inline change bar; Cancel restores; Change answer posts override (and survives reload)', async () => {
    const overriddenView: DecisionView = view({
      status: 'decided',
      decision: {
        choiceId: 'no',
        label: 'No',
        previousChoiceLabel: 'Yes',
        answeredByUserId: 'u-me',
        answeredByName: 'Me',
        provenance: 'direct',
        outcome: 'override',
        surface: 'web',
        responseMessageId: 'resp-over',
        at: '2026-11-12T15:53:00Z',
      },
    });
    apiFetch.mockResolvedValueOnce(
      res(200, { decision: overriddenView, responseMessageId: 'resp-over' })
    );

    const el = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    button(el, 'no').click();
    await settle(el);

    // Did not POST yet; inline change bar is open.
    expect(apiFetch).not.toHaveBeenCalled();
    expect(root(el).querySelector('.confirm-row')).toBeNull();
    const bar = root(el).querySelector('.confirmbar')!;
    expect(bar.textContent?.replace(/\s+/g, ' ').trim()).toContain(
      'Change your answer from Yes to No? release-bot will be told.'
    );
    expect(button(el, 'yes').hasAttribute('outline')).toBe(true);
    expect(button(el, 'yes').querySelector('sl-icon')?.getAttribute('name')).toBe('robot');
    expect(button(el, 'no').getAttribute('variant')).toBe('primary');
    expect(button(el, 'no').getAttribute('aria-pressed')).toBe('true');
    expect(button(el, 'no').querySelector('sl-icon')?.getAttribute('name')).toBe('check-lg');

    // Cancel restores P2-1 without sending.
    bar.querySelector<HTMLElement>('[data-action="cancel-change"]')!.click();
    await settle(el);
    expect(root(el).querySelector('.confirmbar')).toBeNull();
    expect(root(el).querySelector('.confirm-row')).not.toBeNull();
    expect(apiFetch).not.toHaveBeenCalled();

    // Open again and click Change answer.
    button(el, 'no').click();
    await settle(el);
    root(el).querySelector<HTMLElement>('[data-action="confirm-change"]')!.click();
    await settle(el);

    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(JSON.parse(apiFetch.mock.calls[0]![1]!.body as string).choice_id).toBe('no');
    expect(root(el).querySelector('.card-head .badge.over')?.textContent?.trim()).toBe('changed');
    expect(foot(el)).toMatch(
      /^Answered by you · \d\d:\d\d · changed from the interpreted answer Yes$/
    );

    // Reload parity: mounting fresh from history view with previousChoiceLabel renders identically;
    // mounting without previousChoiceLabel omits the trailing label cleanly.
    const reloaded = await mount([overriddenView]);
    expect(foot(reloaded)).toMatch(
      /^Answered by you · \d\d:\d\d · changed from the interpreted answer Yes$/
    );
    const { previousChoiceLabel: _drop, ...withoutPrev } = overriddenView.decision!;
    const fallback = await mount([{ ...overriddenView, decision: withoutPrev }]);
    expect(foot(fallback)).toMatch(
      /^Answered by you · \d\d:\d\d · changed from the interpreted answer$/
    );
  });

  it('P2-2: another viewer sees disabled choices with owner tooltip and no confirm controls', async () => {
    const el = await mount([interpretedSingle({ id: 'u-bob', name: 'Bob' })]);
    await settle(el);

    expect(root(el).querySelector('.card-head .badge.interp')?.textContent?.trim()).toBe(
      'interpreted'
    );
    for (const id of ['yes', 'no', 'wait']) {
      expect(button(el, id).hasAttribute('disabled')).toBe(true);
    }
    const tip = root(el).querySelector('sl-tooltip');
    expect(tip?.getAttribute('content')).toBe('Only Bob can confirm or change this answer');
    expect(root(el).querySelector('.confirm-row')).toBeNull();
    expect(root(el).querySelector('.confirmbar')).toBeNull();
    expect(foot(el)).toMatch(
      /^Interpreted by release-bot from Bob's reply · \d\d:\d\d · View reply$/
    );
  });

  it('clicking the already-chosen interpreted choice is a no-op when confirmbar is closed and cancels confirmbar when open', async () => {
    const el = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    await settle(el);

    // Closed `.confirmbar`: clicking the filled robot-marked choice must NOT send confirm.
    button(el, 'yes').click();
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(root(el).querySelector('.confirmbar')).toBeNull();
    expect(root(el).querySelector('.confirm-row')).not.toBeNull();

    // Open `.confirmbar` by selecting 'no', verify polite status announcement, then click 'yes' to cancel.
    button(el, 'no').click();
    await settle(el);
    const bar = root(el).querySelector('.confirmbar');
    expect(bar).not.toBeNull();
    const statusSpan = bar?.querySelector('span[role="status"]');
    expect(statusSpan?.getAttribute('aria-live')).toBe('polite');

    button(el, 'yes').click();
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(root(el).querySelector('.confirmbar')).toBeNull();
    expect(root(el).querySelector('.confirm-row')).not.toBeNull();
  });

  it('Hub-shaped 403 not_recorded_answerer (with details.decision) toasts warning and refreshes card to read-only without blocking', async () => {
    const bobInterp = interpretedSingle({ id: 'u-bob', name: 'Bob' });
    apiFetch.mockResolvedValueOnce(
      res(403, {
        error: {
          code: 'not_recorded_answerer',
          message: 'only the recorded answerer may confirm or change an interpreted answer',
          details: { decision: bobInterp },
        },
      })
    );

    const el = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    root(el).querySelector<HTMLElement>('[data-action="confirm"]')!.click();
    await settle(el);

    expect(showToast).toHaveBeenCalledWith(
      'only the recorded answerer may confirm or change an interpreted answer',
      'warning'
    );
    expect(root(el).querySelector('sl-tooltip')?.getAttribute('content')).toBe(
      'Only Bob can confirm or change this answer'
    );
    for (const id of ['yes', 'no', 'wait']) {
      expect(button(el, id).hasAttribute('disabled')).toBe(true);
    }
    expect(root(el).querySelector('.confirm-row')).toBeNull();
    expect(root(el).querySelector('[data-reason]')).toBeNull();
    expect(root(el).querySelector('[data-note]')).toBeNull();
  });

  it('stacked (described-choice) interpreted single cards disable non-owner buttons natively and open confirmbar with aria-pressed for owner', async () => {
    const stackedChoices = [
      { id: 'yes', label: 'Yes', description: 'Deploy right away.' },
      { id: 'no', label: 'No', description: 'Hold until Monday.' },
    ];

    const elBob = await mount([
      interpretedSingle({ id: 'u-bob', name: 'Bob' }, { choices: stackedChoices }),
    ]);
    await settle(elBob);
    for (const id of ['yes', 'no']) {
      const opt = button(elBob, id);
      expect(opt.tagName.toLowerCase()).toBe('button');
      expect(opt.hasAttribute('disabled')).toBe(true);
      expect(opt.getAttribute('title')).toBe('Only Bob can confirm or change this answer');
    }
    button(elBob, 'no').click();
    await settle(elBob);
    expect(apiFetch).not.toHaveBeenCalled();
    expect(root(elBob).querySelector('.confirmbar')).toBeNull();

    const elMe = await mount([
      interpretedSingle({ id: 'u-me', name: 'Me' }, { choices: stackedChoices }),
    ]);
    await settle(elMe);
    expect(button(elMe, 'yes').hasAttribute('disabled')).toBe(false);
    button(elMe, 'yes').click();
    await settle(elMe);
    expect(apiFetch).not.toHaveBeenCalled();

    button(elMe, 'no').click();
    await settle(elMe);
    expect(root(elMe).querySelector('.confirmbar')).not.toBeNull();
    expect(button(elMe, 'no').getAttribute('aria-pressed')).toBe('true');

    button(elMe, 'yes').click();
    await settle(elMe);
    expect(root(elMe).querySelector('.confirmbar')).toBeNull();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('closes an open confirmbar when SSE updates the row to direct-confirmed', async () => {
    const el = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    button(el, 'no').click();
    await settle(el);
    expect(root(el).querySelector('.confirmbar')).not.toBeNull();

    el.decisions = [
      view({
        status: 'decided',
        decision: {
          choiceId: 'yes',
          label: 'Yes',
          answeredByUserId: 'u-me',
          answeredByName: 'Me',
          provenance: 'direct',
          outcome: 'confirm',
          surface: 'web',
          responseMessageId: 'resp-sse',
          at: '2026-11-12T15:55:00Z',
        },
      }),
    ];
    await settle(el);

    expect(root(el).querySelector('.confirmbar')).toBeNull();
    expect(root(el).querySelector('.confirm-row')).toBeNull();
    expect(root(el).querySelector('.card-head .badge.conf')?.textContent?.trim()).toBe('confirmed');
  });

  it('omits data-readonly on sl-button while an interpreted confirmation is loading', async () => {
    let resolvePost!: (r: Response) => void;
    apiFetch.mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          resolvePost = resolve;
        })
    );

    const el = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    await settle(el);

    // Before clicking Confirm, the already-interpreted choice button is non-clickable -> data-readonly.
    expect(button(el, 'yes').hasAttribute('data-readonly')).toBe(true);

    const confirmBtn = root(el).querySelector<HTMLElement>('.confirm-row [data-action="confirm"]')!;
    confirmBtn.click();
    await el.updateComplete;

    expect(button(el, 'yes').hasAttribute('loading')).toBe(true);
    expect(button(el, 'yes').hasAttribute('data-readonly')).toBe(false);

    resolvePost(
      res(200, {
        decision: {
          ...interpretedSingle({ id: 'u-me', name: 'Me' }),
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-me',
            answeredByName: 'Me',
            provenance: 'direct',
            outcome: 'confirm',
            surface: 'web',
            responseMessageId: 'resp-ok',
            at: '2026-11-12T15:56:00Z',
          },
        },
        responseMessageId: 'resp-ok',
      })
    );
    await settle(el);
    expect(button(el, 'yes').hasAttribute('loading')).toBe(false);
    expect(button(el, 'yes').hasAttribute('data-readonly')).toBe(true);
  });

  it('handles contextmenu safely when window.matchMedia is undefined', async () => {
    const el = await mount([view()]);
    await settle(el);
    const origMatchMedia = window.matchMedia;
    Object.defineProperty(window, 'matchMedia', {
      value: undefined,
      configurable: true,
      writable: true,
    });
    try {
      expect(() => {
        button(el, 'yes').dispatchEvent(
          new MouseEvent('contextmenu', {
            bubbles: true,
            composed: true,
            cancelable: true,
            clientX: 40,
            clientY: 40,
          })
        );
      }).not.toThrow();
    } finally {
      Object.defineProperty(window, 'matchMedia', {
        value: origMatchMedia,
        configurable: true,
        writable: true,
      });
    }
  });

  it('forwards surface="decisions_page" on single answer, confirm, and override', async () => {
    const elPending = await mount([view()]);
    elPending.surface = 'decisions_page';
    await settle(elPending);
    apiFetch.mockResolvedValueOnce(
      res(200, { decision: answered('yes', { id: 'u-me', name: 'Me' }), responseMessageId: 'r-p' })
    );
    button(elPending, 'yes').click();
    await settle(elPending);
    expect(JSON.parse(apiFetch.mock.calls[0]![1]?.body as string)).toMatchObject({
      choice_id: 'yes',
      surface: 'decisions_page',
    });

    const elConfirm = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    elConfirm.surface = 'decisions_page';
    await settle(elConfirm);
    apiFetch.mockResolvedValueOnce(
      res(200, {
        decision: view({
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-me',
            answeredByName: 'Me',
            provenance: 'direct',
            outcome: 'confirm',
            surface: 'decisions_page',
            responseMessageId: 'r-c',
            at: '2026-11-12T15:56:00Z',
          },
        }),
        responseMessageId: 'r-c',
      })
    );
    root(elConfirm).querySelector<HTMLElement>('[data-action="confirm"]')!.click();
    await settle(elConfirm);
    expect(JSON.parse(apiFetch.mock.calls[1]![1]?.body as string)).toMatchObject({
      choice_id: 'yes',
      surface: 'decisions_page',
    });

    const elOverride = await mount([interpretedSingle({ id: 'u-me', name: 'Me' })]);
    elOverride.surface = 'decisions_page';
    await settle(elOverride);
    button(elOverride, 'no').click();
    await settle(elOverride);
    apiFetch.mockResolvedValueOnce(
      res(200, {
        decision: view({
          status: 'decided',
          decision: {
            choiceId: 'no',
            label: 'No',
            previousChoiceLabel: 'Yes',
            answeredByUserId: 'u-me',
            answeredByName: 'Me',
            provenance: 'direct',
            outcome: 'override',
            surface: 'decisions_page',
            responseMessageId: 'r-o',
            at: '2026-11-12T15:57:00Z',
          },
        }),
        responseMessageId: 'r-o',
      })
    );
    root(elOverride).querySelector<HTMLElement>('[data-action="confirm-change"]')!.click();
    await settle(elOverride);
    expect(JSON.parse(apiFetch.mock.calls[2]![1]?.body as string)).toMatchObject({
      choice_id: 'no',
      surface: 'decisions_page',
    });
  });
});
