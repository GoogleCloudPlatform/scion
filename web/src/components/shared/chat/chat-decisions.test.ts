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
 * `<scion-chat-decisions>` in single mode (ptone/scion#7673, design §4.2,
 * §4.3, §4.7): pending with the recommended choice outlined, submitting,
 * answered, the 409 and error toasts, and Enter on the focused card.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';
import type { DecisionAnsweredDetail } from '../../../client/decision-prompts.js';
import { getMarkdownRenderer } from '../../../utils/markdown.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
const showToast = vi.hoisted(() => vi.fn());

vi.mock('../../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../../client/api.js')>()),
  apiFetch,
}));
vi.mock('../../../utils/toast.js', () => ({ showToast }));

await import('./chat-decisions.js');
type ScionChatDecisions = import('./chat-decisions.js').ScionChatDecisions;

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

async function mount(decisions: DecisionView[]): Promise<ScionChatDecisions> {
  const el = document.createElement('scion-chat-decisions');
  el.messageId = 'm1';
  el.currentUserId = 'u-me';
  el.decisions = decisions;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatDecisions): ShadowRoot => el.shadowRoot!;
const button = (el: ScionChatDecisions, id: string): HTMLElement =>
  root(el).querySelector<HTMLElement>(`[data-choice="${id}"]`)!;
const foot = (el: ScionChatDecisions): string =>
  root(el).querySelector('.row-foot')?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
const card = (el: ScionChatDecisions): HTMLElement => root(el).querySelector<HTMLElement>('.card')!;

async function settle(el: ScionChatDecisions): Promise<void> {
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
  it('shows submitting, then the 200 view, and emits decision-answered', async () => {
    let resolve!: (r: Response) => void;
    apiFetch.mockImplementation(() => new Promise<Response>((r) => (resolve = r)));
    const el = await mount([view()]);
    const events: DecisionAnsweredDetail[] = [];
    el.addEventListener('decision-answered', (e) =>
      events.push((e as CustomEvent<DecisionAnsweredDetail>).detail)
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
    el.addEventListener('decision-answered', (e) => events.push(e));
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
  async function rendered(el: ScionChatDecisions): Promise<void> {
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

describe('single Other free-form option (ptone/scion#8152)', () => {
  function viewWithOther(over: Partial<DecisionView> = {}): DecisionView {
    return view({
      key: 'region',
      ref: 'region',
      question: 'Which `region` for the canary?',
      defaultChoiceId: 'us-central1',
      choices: [
        { id: 'us-central1', label: 'us-central1' },
        { id: 'europe-west1', label: 'europe-west1' },
      ],
      other: {
        label: 'Another region',
        placeholder: 'e.g. europe-west4',
      },
      ...over,
    });
  }

  function typeIntoOther(el: ScionChatDecisions, val: string): HTMLElement {
    const input = root(el).querySelector<HTMLElement & { value: string }>('.editor sl-input')!;
    input.value = val;
    input.dispatchEvent(new Event('sl-input', { bubbles: true, composed: true }));
    return input;
  }

  it('renders Other last with pencil and ellipsis, opens the inline editor without posting, and moves focus', async () => {
    const focusMoves: Event[] = [];
    const onMove = (e: Event): void => {
      focusMoves.push(e);
    };
    window.addEventListener('scion-focus-moved', onMove);
    try {
      const el = await mount([viewWithOther()]);
      await settle(el);

      const allButtons = Array.from(root(el).querySelectorAll('.choices [data-choice]'));
      expect(allButtons.map((b) => b.getAttribute('data-choice'))).toEqual([
        'us-central1',
        'europe-west1',
        '$other',
      ]);
      const otherBtn = button(el, '$other');
      expect(otherBtn.classList.contains('other-btn')).toBe(true);
      expect(otherBtn.querySelector('sl-icon')?.getAttribute('name')).toBe('pencil');
      expect(otherBtn.textContent?.trim()).toBe('Another region…');
      expect(root(el).querySelector('.editor')).toBeNull();

      otherBtn.click();
      await settle(el);
      expect(apiFetch).not.toHaveBeenCalled();

      const editor = root(el).querySelector('.editor');
      expect(editor).not.toBeNull();
      expect(editor?.querySelector('.lab')?.textContent).toBe('Another region');
      const input = editor?.querySelector('sl-input');
      expect(input?.hasAttribute('maxlength')).toBe(false);
      expect(input?.getAttribute('placeholder')).toBe('e.g. europe-west4');
      expect(input?.getAttribute('label')).toBe('Which region for the canary?');
      expect(focusMoves.length).toBeGreaterThanOrEqual(1);

      // Send starts disabled while empty or whitespace-only.
      const sendBtn = editor!.querySelector<HTMLElement>('[data-action="send-other"]')!;
      expect(sendBtn.hasAttribute('disabled')).toBe(true);
      typeIntoOther(el, '   ');
      await el.updateComplete;
      expect(sendBtn.hasAttribute('disabled')).toBe(true);
    } finally {
      window.removeEventListener('scion-focus-moved', onMove);
    }
  });

  it('counts trimmed runes (including astral emoji), warns from 180, and disables Send above 200', async () => {
    const el = await mount([viewWithOther({ other: { label: 'Other' } })]);
    button(el, '$other').click();
    await settle(el);

    const input = root(el).querySelector('.editor sl-input')!;
    const sendBtn = root(el).querySelector<HTMLElement>('[data-action="send-other"]')!;
    expect(input.getAttribute('placeholder')).toBe('Type your answer');
    expect(input.hasAttribute('maxlength')).toBe(false);

    // Surrounding whitespace is excluded from the counter (158 trimmed runes < 160).
    typeIntoOther(el, `   ${'a'.repeat(158)}   `);
    await el.updateComplete;
    expect(root(el).querySelector('.editor .cnt')).toBeNull();

    // Astral emoji use 2 UTF-16 code units per rune: 160 emoji = 320 UTF-16 units, 160 runes.
    typeIntoOther(el, '🚀'.repeat(160));
    await el.updateComplete;
    const cnt160 = root(el).querySelector('.editor .cnt');
    expect(cnt160?.textContent).toBe('160 / 200');
    expect(cnt160?.classList.contains('warn')).toBe(false);
    expect(cnt160?.classList.contains('over')).toBe(false);
    expect(cnt160?.hasAttribute('aria-live')).toBe(false);
    expect(sendBtn.hasAttribute('disabled')).toBe(false);

    typeIntoOther(el, '🚀'.repeat(180));
    await el.updateComplete;
    const cnt180 = root(el).querySelector('.editor .cnt');
    expect(cnt180?.textContent).toBe('180 / 200');
    expect(cnt180?.classList.contains('warn')).toBe(true);
    expect(cnt180?.getAttribute('aria-live')).toBe('polite');
    expect(sendBtn.hasAttribute('disabled')).toBe(false);

    typeIntoOther(el, `  ${'🚀'.repeat(200)}  `);
    await el.updateComplete;
    expect(root(el).querySelector('.editor .cnt')?.textContent).toBe('200 / 200');
    expect(sendBtn.hasAttribute('disabled')).toBe(false);

    typeIntoOther(el, '🚀'.repeat(201));
    await el.updateComplete;
    const cnt201 = root(el).querySelector('.editor .cnt');
    expect(cnt201?.textContent).toBe('201 / 200');
    expect(cnt201?.classList.contains('over')).toBe(true);
    expect(cnt201?.getAttribute('aria-live')).toBe('polite');
    expect(sendBtn.hasAttribute('disabled')).toBe(true);
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('sends trimmed text on Enter, renders quoted literal plain text when answered, and ignores card Enter while open', async () => {
    const el = await mount([viewWithOther()]);
    const events: DecisionAnsweredDetail[] = [];
    el.addEventListener('decision-answered', (e) =>
      events.push((e as CustomEvent<DecisionAnsweredDetail>).detail)
    );

    button(el, '$other').click();
    await settle(el);

    // Card-level Enter does not pick defaultChoiceId while the Other editor is open.
    card(el).dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(apiFetch).not.toHaveBeenCalled();

    const rawText = '  europe-west4 **bold** <script>window.__pwned=1</script> @bot  ';
    const trimmed = rawText.trim();
    const decidedOther = viewWithOther({
      status: 'decided',
      decision: {
        choiceId: '$other',
        label: 'Another region',
        otherText: trimmed,
        answeredByUserId: 'u-me',
        answeredByName: 'Me',
        provenance: 'direct',
        surface: 'native',
        responseMessageId: 'resp-other',
        at: '2026-11-12T15:44:00Z',
      },
    });
    apiFetch.mockResolvedValueOnce(
      res(200, { decision: decidedOther, responseMessageId: 'resp-other' })
    );

    const input = typeIntoOther(el, rawText);
    await el.updateComplete;
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);

    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(JSON.parse(apiFetch.mock.calls[0]![1]?.body as string)).toMatchObject({
      choice_id: '$other',
      other_text: trimmed,
    });
    expect(events).toHaveLength(1);
    expect(root(el).querySelector('.editor')).toBeNull();

    // Answered state: Other button filled without ellipsis, quoted text plain (no HTML/Markdown).
    const chosenBtn = button(el, '$other');
    expect(chosenBtn.getAttribute('variant')).toBe('primary');
    expect(chosenBtn.textContent?.trim()).toBe('Another region');
    const otext = root(el).querySelector('.otext');
    expect(otext?.textContent).toBe(`“${trimmed}”`);
    expect(otext?.children.length).toBe(0);
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
    expect(foot(el)).toContain('Answered by you');
  });

  it('cancels on Escape or Cancel, restores focus to Other, and drops the draft when a fixed choice is clicked or the row changes', async () => {
    const el = await mount([viewWithOther()]);
    button(el, '$other').click();
    await settle(el);

    const input = typeIntoOther(el, 'draft-1');
    await el.updateComplete;

    let focusMoved = false;
    window.addEventListener(
      'scion-focus-moved',
      () => {
        focusMoved = true;
      },
      { once: true }
    );
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settle(el);
    expect(root(el).querySelector('.editor')).toBeNull();
    expect(focusMoved).toBe(true);

    // Re-opening starts empty; clicking a fixed choice posts that choice and discards the draft.
    button(el, '$other').click();
    await settle(el);
    expect(root(el).querySelector<HTMLElement & { value: string }>('.editor sl-input')!.value).toBe(
      ''
    );
    typeIntoOther(el, 'draft-2');
    await el.updateComplete;

    apiFetch.mockResolvedValueOnce(
      res(200, {
        decision: viewWithOther({
          status: 'decided',
          decision: {
            choiceId: 'europe-west1',
            label: 'europe-west1',
            answeredByUserId: 'u-me',
            answeredByName: 'Me',
            provenance: 'direct',
            surface: 'native',
            responseMessageId: 'resp-fixed',
            at: '2026-11-12T15:45:00Z',
          },
        }),
        responseMessageId: 'resp-fixed',
      })
    );
    button(el, 'europe-west1').click();
    await settle(el);
    expect(JSON.parse(apiFetch.mock.calls[0]![1]?.body as string)).toEqual({
      choice_id: 'europe-west1',
      idempotency_key: expect.any(String),
    });
    expect(root(el).querySelector('.editor')).toBeNull();

    // External update to replaced/withdrawn/dismissed/decided drops open editor & draft.
    const el2 = await mount([viewWithOther()]);
    button(el2, '$other').click();
    await settle(el2);
    typeIntoOther(el2, 'draft-3');
    el2.decisions = [viewWithOther({ status: 'withdrawn' })];
    await settle(el2);
    expect(root(el2).querySelector('.editor')).toBeNull();
    expect(button(el2, '$other').hasAttribute('disabled')).toBe(true);
    // Reopening on an open row proves the draft itself was cleared, not just hidden.
    el2.decisions = [viewWithOther({ status: 'pending' })];
    await settle(el2);
    button(el2, '$other').click();
    await settle(el2);
    expect(
      root(el2).querySelector<HTMLElement & { value: string }>('.editor sl-input')?.value
    ).toBe('');

    const el3 = await mount([viewWithOther()]);
    button(el3, '$other').click();
    await settle(el3);
    typeIntoOther(el3, 'draft-4');
    el3.decisions = [viewWithOther({ status: 'dismissed' })];
    await settle(el3);
    expect(root(el3).querySelector('.editor')).toBeNull();
    expect(button(el3, '$other').hasAttribute('disabled')).toBe(true);
    el3.decisions = [viewWithOther({ status: 'pending' })];
    await settle(el3);
    button(el3, '$other').click();
    await settle(el3);
    expect(
      root(el3).querySelector<HTMLElement & { value: string }>('.editor sl-input')?.value
    ).toBe('');
  });

  it('renders stacked layout (O4) with 1 fixed choice and Other inline description + inset editor', async () => {
    const el = await mount([
      view({
        key: 'proceed',
        ref: 'proceed',
        question: 'Tag `v0.9` now?',
        defaultChoiceId: '1',
        choices: [{ id: '1', label: 'Yes, tag now', description: 'Tags and drafts notes' }],
        other: {
          label: 'Something else',
          description: 'Tell me `what` to do instead',
        },
      }),
    ]);
    await settle(el);

    expect(root(el).querySelector('.choices')?.classList.contains('stack')).toBe(true);
    const otherRow = button(el, '$other');
    expect(otherRow.classList.contains('other-opt')).toBe(true);
    expect(otherRow.querySelector('.desc code')?.textContent).toBe('what');

    otherRow.click();
    await settle(el);
    const sel = root(el).querySelector('.opt.sel');
    expect(sel).not.toBeNull();
    expect(sel?.querySelector('.editor.inset')).not.toBeNull();
    expect(sel?.querySelector('.editor.inset .lab')).toBeNull();
  });

  it('keeps the draft open on 400 validation_error and closes to the winner on 409 already_answered', async () => {
    const el = await mount([viewWithOther()]);
    button(el, '$other').click();
    await settle(el);

    const input = typeIntoOther(el, 'keep-this-draft');
    await el.updateComplete;

    // 400 validation_error: danger toast, editor stays open with draft preserved.
    apiFetch.mockResolvedValueOnce(
      res(400, {
        error: {
          code: 'validation_error',
          message: 'other_text cannot contain control characters',
        },
      })
    );
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);
    expect(showToast).toHaveBeenCalledWith(
      'other_text cannot contain control characters',
      'danger'
    );
    expect(root(el).querySelector('.editor')).not.toBeNull();
    expect(root(el).querySelector<HTMLElement & { value: string }>('.editor sl-input')?.value).toBe(
      'keep-this-draft'
    );

    // 409 already_answered while editor is open: warning toast, editor closes, winner applied.
    showToast.mockClear();
    const winner = viewWithOther({
      status: 'decided',
      decision: {
        choiceId: 'europe-west1',
        label: 'europe-west1',
        answeredByUserId: 'u-bob',
        answeredByName: 'Bob',
        provenance: 'direct',
        surface: 'native',
        responseMessageId: 'resp-bob',
        at: '2026-11-12T15:50:00Z',
      },
    });
    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'already_answered',
          message: 'already answered',
          details: { decision: winner },
        },
      })
    );
    root(el).querySelector<HTMLElement>('[data-action="send-other"]')!.click();
    await settle(el);
    expect(showToast).toHaveBeenCalledWith('Already answered by Bob.', 'warning');
    expect(root(el).querySelector('.editor')).toBeNull();
    expect(button(el, 'europe-west1').getAttribute('variant')).toBe('primary');
    expect(foot(el)).toMatch(/^Answered by Bob · /);
  });

  it('supports interpreted Other confirm bar, text edit override, fixed override, and non-owner read-only (O9)', async () => {
    const interpretedOther = viewWithOther({
      status: 'decided',
      decision: {
        choiceId: '$other',
        label: 'Another region',
        otherText: 'europe-west4',
        answeredByUserId: 'u-me',
        answeredByName: 'Alice',
        provenance: 'agent_interpreted',
        outcome: 'answer',
        sourceMessageId: 'src-1',
        surface: 'slack',
        responseMessageId: 'resp-interp-1',
        at: '2026-11-20T03:00:00Z',
      },
    });

    const el = await mount([interpretedOther]);
    const otherBtn = button(el, '$other');
    expect(otherBtn.getAttribute('variant')).toBe('primary');
    expect(otherBtn.hasAttribute('outline')).toBe(true);
    const input = root(el).querySelector<HTMLElement & { value: string }>('.editor sl-input')!;
    expect(input).not.toBeNull();
    expect(input.value).toBe('europe-west4');
    expect(
      root(el).querySelector('[data-action="confirm"]')?.textContent?.replace(/\s+/g, ' ').trim()
    ).toBe('Confirm “Another region”');

    // Editing the text switches from Confirm to Change answer bar.
    typeIntoOther(el, 'europe-west4 after freeze');
    await settle(el);
    const bar = root(el).querySelector('.confirmbar');
    expect(bar).not.toBeNull();
    expect(bar?.textContent).toContain(
      'Change your answer from Another region “europe-west4” to Another region “europe-west4 after freeze”?'
    );

    // Cancel reverts draft back to recorded text and restores Confirm.
    root(el).querySelector<HTMLElement>('[data-action="cancel-change"]')!.click();
    await settle(el);
    expect(input.value).toBe('europe-west4');
    expect(root(el).querySelector('.confirmbar')).toBeNull();
    expect(root(el).querySelector('[data-action="confirm"]')).not.toBeNull();

    // Clicking a fixed choice stages override from Other to fixed.
    button(el, 'europe-west1').click();
    await settle(el);
    expect(root(el).querySelector('.confirmbar')?.textContent).toContain(
      'Change your answer from Another region “europe-west4” to europe-west1?'
    );
    expect(root(el).querySelector('.editor')).toBeNull();
    expect(root(el).querySelector('.otext')?.textContent).toBe('“europe-west4”');

    // Clicking Other returns to editor; edit and submit override.
    button(el, '$other').click();
    await settle(el);
    typeIntoOther(el, 'asia-east1');
    await settle(el);
    apiFetch.mockResolvedValueOnce(
      res(200, {
        decision: viewWithOther({
          status: 'decided',
          decision: {
            choiceId: '$other',
            label: 'Another region',
            otherText: 'asia-east1',
            previousChoiceLabel: 'Another region',
            previousOtherText: 'europe-west4',
            answeredByUserId: 'u-me',
            answeredByName: 'Alice',
            provenance: 'direct',
            outcome: 'override',
            surface: 'native',
            responseMessageId: 'resp-over-1',
            at: '2026-11-20T03:05:00Z',
          },
        }),
        responseMessageId: 'resp-over-1',
      })
    );
    root(el).querySelector<HTMLElement>('[data-action="confirm-change"]')!.click();
    await settle(el);
    expect(JSON.parse(apiFetch.mock.calls[0]![1]?.body as string)).toEqual({
      choice_id: '$other',
      other_text: 'asia-east1',
      idempotency_key: expect.any(String),
    });
    expect(foot(el)).toContain('changed from the interpreted answer Another region “europe-west4”');

    // Non-owner sees disabled robot button and static .otext, no editor.
    const elOtherUser = await mount([interpretedOther]);
    elOtherUser.currentUserId = 'u-other';
    await settle(elOtherUser);
    expect(root(elOtherUser).querySelector('.editor')).toBeNull();
    expect(root(elOtherUser).querySelector('.otext')?.textContent).toBe('“europe-west4”');
    expect(button(elOtherUser, '$other').hasAttribute('disabled')).toBe(true);
  });

  it('allows changing an interpreted fixed choice to Other from the single card', async () => {
    const interpretedFixed = viewWithOther({
      status: 'decided',
      decision: {
        choiceId: 'us-central1',
        label: 'us-central1',
        answeredByUserId: 'u-me',
        answeredByName: 'Alice',
        provenance: 'agent_interpreted',
        outcome: 'answer',
        sourceMessageId: 'src-2',
        surface: 'slack',
        responseMessageId: 'resp-interp-2',
        at: '2026-11-20T03:00:00Z',
      },
    });

    const el = await mount([interpretedFixed]);
    const otherBtn = button(el, '$other');
    expect(otherBtn.hasAttribute('disabled')).toBe(false);
    otherBtn.click();
    await settle(el);

    expect(root(el).querySelector('.editor')).not.toBeNull();
    const changeBtn = root(el).querySelector<HTMLElement>('[data-action="confirm-change"]')!;
    expect(changeBtn.hasAttribute('disabled')).toBe(true);

    typeIntoOther(el, 'me-central2');
    await settle(el);
    expect(changeBtn.hasAttribute('disabled')).toBe(false);
    expect(root(el).querySelector('.confirmbar')?.textContent).toContain(
      'Change your answer from us-central1 to Another region “me-central2”?'
    );

    apiFetch.mockResolvedValueOnce(
      res(200, {
        decision: viewWithOther({
          status: 'decided',
          decision: {
            choiceId: '$other',
            label: 'Another region',
            otherText: 'me-central2',
            previousChoiceLabel: 'us-central1',
            answeredByUserId: 'u-me',
            answeredByName: 'Alice',
            provenance: 'direct',
            outcome: 'override',
            surface: 'native',
            responseMessageId: 'resp-over-2',
            at: '2026-11-20T03:06:00Z',
          },
        }),
        responseMessageId: 'resp-over-2',
      })
    );
    changeBtn.click();
    await settle(el);
    expect(JSON.parse(apiFetch.mock.calls[0]![1]?.body as string)).toEqual({
      choice_id: '$other',
      other_text: 'me-central2',
      idempotency_key: expect.any(String),
    });
    expect(root(el).querySelector('.otext')?.textContent).toBe('“me-central2”');
  });
});

describe('set card Other free-form answers (ptone/scion#8153, O5–O7)', () => {
  function setWithOther(): DecisionView[] {
    return [
      view({
        rowId: 'r1',
        key: 'region',
        ref: 'region',
        question: 'Which region for the canary?',
        defaultChoiceId: 'us-central1',
        choices: [
          { id: 'us-central1', label: 'us-central1' },
          { id: 'europe-west1', label: 'europe-west1' },
        ],
        other: { label: 'Another region', placeholder: 'e.g. europe-west4' },
      }),
      view({
        rowId: 'r2',
        key: 'notify',
        ref: 'notify',
        question: 'Notify the release channel?',
        choices: [
          { id: 'yes', label: 'Yes' },
          { id: 'no', label: 'No' },
        ],
      }),
      view({
        rowId: 'r3',
        key: 'proceed',
        ref: 'proceed',
        question: 'Tag `v0.9` now?',
        choices: [{ id: 'yes', label: 'Yes' }],
        other: { label: 'Something else' },
      }),
    ];
  }

  function rowEl(el: ScionChatDecisions, rowId: string): HTMLElement {
    const row = root(el).querySelector<HTMLElement>(`.row[data-row="${rowId}"]`);
    if (!row) throw new Error(`no row ${rowId}`);
    return row;
  }

  function rowChoice(el: ScionChatDecisions, rowId: string, choiceId: string): HTMLElement {
    const btn = rowEl(el, rowId).querySelector<HTMLElement>(`[data-choice="${choiceId}"]`);
    if (!btn) throw new Error(`no choice ${choiceId} in row ${rowId}`);
    return btn;
  }

  function typeIntoSetOther(el: ScionChatDecisions, rowId: string, value: string): HTMLElement {
    const input = rowEl(el, rowId).querySelector<HTMLElement & { value: string }>(
      '.editor sl-input'
    );
    if (!input) throw new Error(`no set other sl-input in row ${rowId}`);
    input.value = value;
    input.dispatchEvent(new CustomEvent('sl-input', { bubbles: true, composed: true }));
    return input;
  }

  function submitBtn(el: ScionChatDecisions): HTMLElement {
    const btn = root(el).querySelector<HTMLElement>('.card-foot sl-button.submit');
    if (!btn) throw new Error('no submit button');
    return btn;
  }

  function submitHint(el: ScionChatDecisions): string {
    return root(el).querySelector('.card-foot sl-tooltip')?.getAttribute('content') ?? '';
  }

  it('O5–O7: per-row Other editor without Send/Cancel, remaining gating, draft preservation across choice switch, submit and literal display', async () => {
    const el = await mount(setWithOther());

    // Row r2 has no Other button; r1 and r3 do.
    expect(rowEl(el, 'r1').querySelector('[data-choice="$other"]')).not.toBeNull();
    expect(rowEl(el, 'r2').querySelector('[data-choice="$other"]')).toBeNull();
    expect(rowEl(el, 'r3').querySelector('[data-choice="$other"]')).not.toBeNull();

    // Pick fixed answers on r2 and r3, and Other on r1.
    rowChoice(el, 'r2', 'yes').click();
    rowChoice(el, 'r3', 'yes').click();
    rowChoice(el, 'r1', '$other').click();
    await settle(el);

    // Editor opens on r1 with no per-row Send/Cancel buttons.
    const r1Editor = rowEl(el, 'r1').querySelector('.editor');
    expect(r1Editor).not.toBeNull();
    expect(r1Editor?.querySelector('[data-action="send-other"]')).toBeNull();
    expect(r1Editor?.querySelector('[data-action="cancel-other"]')).toBeNull();
    expect(r1Editor?.querySelector('.set-other-help')?.textContent?.trim()).toBe(
      'Type your answer to include it in the submit.'
    );
    expect(rowChoice(el, 'r1', '$other').getAttribute('aria-pressed')).toBe('true');

    // Empty Other counts as 1 remaining, Submit disabled, hint names region.
    expect(root(el).querySelector('.counter')?.textContent?.trim()).toBe('1 remaining');
    expect(submitBtn(el).hasAttribute('disabled')).toBe(true);
    expect(submitHint(el)).toBe('Choose or type an answer for region first');
    const srHintId = submitBtn(el).getAttribute('aria-describedby');
    expect(srHintId).toBeTruthy();
    expect(root(el).querySelector(`#${srHintId}`)?.textContent?.trim()).toBe(
      'Choose or type an answer for region first'
    );

    // Whitespace-only still counts as remaining.
    typeIntoSetOther(el, 'r1', '   ');
    await el.updateComplete;
    expect(submitBtn(el).hasAttribute('disabled')).toBe(true);
    expect(root(el).querySelector('.counter')?.textContent?.trim()).toBe('1 remaining');

    // Typing text makes 0 remaining and enables Submit.
    const literal = 'eu-"west4" *bold* <b>hi</b> @release-bot';
    typeIntoSetOther(el, 'r1', `  ${literal}  `);
    await el.updateComplete;
    expect(root(el).querySelector('.counter')?.textContent?.trim()).toBe('0 remaining');
    expect(submitBtn(el).hasAttribute('disabled')).toBe(false);
    expect(rowEl(el, 'r1').querySelector('.set-other-help')).toBeNull();

    // Switching r1 to a fixed choice hides the editor but keeps the draft in memory.
    rowChoice(el, 'r1', 'europe-west1').click();
    await settle(el);
    expect(rowEl(el, 'r1').querySelector('.editor')).toBeNull();
    expect(rowChoice(el, 'r1', '$other').getAttribute('aria-pressed')).toBe('false');

    // Switching back to Other restores the draft.
    rowChoice(el, 'r1', '$other').click();
    await settle(el);
    expect(
      rowEl(el, 'r1').querySelector<HTMLElement & { value: string }>('.editor sl-input')?.value
    ).toBe(`  ${literal}  `);

    // Also test counter at 160, 180, and >200 runes (astral emoji = 1 rune) on r3.
    rowChoice(el, 'r3', '$other').click();
    await settle(el);
    typeIntoSetOther(el, 'r3', '🚀'.repeat(160));
    await el.updateComplete;
    expect(rowEl(el, 'r3').querySelector('.cnt')?.textContent?.trim()).toBe('160 / 200');
    expect(submitBtn(el).hasAttribute('disabled')).toBe(false);

    typeIntoSetOther(el, 'r3', '🚀'.repeat(201));
    await el.updateComplete;
    expect(rowEl(el, 'r3').querySelector('.cnt.over')?.textContent?.trim()).toBe('201 / 200');
    expect(submitBtn(el).hasAttribute('disabled')).toBe(true);
    expect(submitHint(el)).toBe('Shorten the answer for proceed');
    const overHintId = submitBtn(el).getAttribute('aria-describedby');
    expect(overHintId).toBeTruthy();
    expect(root(el).querySelector(`#${overHintId}`)?.textContent?.trim()).toBe(
      'Shorten the answer for proceed'
    );

    typeIntoSetOther(el, 'r3', 'Wait until Monday');
    await el.updateComplete;
    expect(submitBtn(el).hasAttribute('disabled')).toBe(false);

    // Submit posts trimmed other_text only on $other rows.
    const decided = setWithOther().map((d, idx) => ({
      ...d,
      status: 'decided',
      decision:
        idx === 0
          ? {
              choiceId: '$other',
              label: 'Another region',
              otherText: literal,
              answeredByUserId: 'u-alice',
              answeredByName: 'Alice',
              provenance: 'direct',
              surface: 'native',
              responseMessageId: 'resp-set-1',
              batchId: 'batch-1',
              at: '2026-11-20T01:00:00Z',
            }
          : idx === 1
            ? {
                choiceId: 'yes',
                label: 'Yes',
                answeredByUserId: 'u-alice',
                answeredByName: 'Alice',
                provenance: 'direct',
                surface: 'native',
                responseMessageId: 'resp-set-1',
                batchId: 'batch-1',
                at: '2026-11-20T01:00:00Z',
              }
            : {
                choiceId: '$other',
                label: 'Something else',
                otherText: 'Wait until Monday',
                answeredByUserId: 'u-alice',
                answeredByName: 'Alice',
                provenance: 'direct',
                surface: 'native',
                responseMessageId: 'resp-set-1',
                batchId: 'batch-1',
                at: '2026-11-20T01:00:00Z',
              },
    }));
    apiFetch.mockResolvedValueOnce(
      res(200, { decisions: decided, responseMessageId: 'resp-set-1', batchId: 'batch-1' })
    );
    submitBtn(el).click();
    await settle(el);

    expect(apiFetch).toHaveBeenCalledTimes(1);
    const [url, init] = apiFetch.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/decision-prompts/m1/answers');
    expect(JSON.parse(init.body as string)).toMatchObject({
      answers: [
        { decision_row_id: 'r1', choice_id: '$other', other_text: literal },
        { decision_row_id: 'r2', choice_id: 'yes' },
        { decision_row_id: 'r3', choice_id: '$other', other_text: 'Wait until Monday' },
      ],
    });

    // O7: chosen Other button has no ellipsis, .otext renders literal text without HTML parsing, drafts cleared.
    expect(rowChoice(el, 'r1', '$other').textContent?.trim()).toBe('Another region');
    const otext = rowEl(el, 'r1').querySelector('.otext');
    expect(otext?.textContent).toBe(`“${literal}”`);
    expect(otext?.children.length).toBe(0);
    expect((el as unknown as { setOtherDrafts: Map<string, string> }).setOtherDrafts.size).toBe(0);
  });

  it('preselectRecommendedChoices never selects Other', async () => {
    const { setPreselectRecommendedChoices } =
      await import('../../../client/decision-preferences.js');
    setPreselectRecommendedChoices(true);
    try {
      const ds = setWithOther();
      // Even if a malformed payload carried defaultChoiceId: '$other', never pick Other.
      ds[2] = { ...ds[2]!, defaultChoiceId: '$other' };
      const el = await mount(ds);

      expect(rowChoice(el, 'r1', 'us-central1').getAttribute('aria-pressed')).toBe('true');
      expect(rowChoice(el, 'r1', '$other').getAttribute('aria-pressed')).toBe('false');
      expect(rowChoice(el, 'r3', '$other').getAttribute('aria-pressed')).toBe('false');
      expect(rowEl(el, 'r3').querySelector('.editor')).toBeNull();
    } finally {
      setPreselectRecommendedChoices(false);
    }
  });

  it('409 answer_set_changed keeps drafts of still-open rows and clears drafts of closed rows (not just hides)', async () => {
    const el = await mount(setWithOther());

    // Type drafts on both r1 and r3, and pick fixed choice on r2.
    rowChoice(el, 'r1', '$other').click();
    rowChoice(el, 'r2', 'yes').click();
    rowChoice(el, 'r3', '$other').click();
    await settle(el);
    typeIntoSetOther(el, 'r1', 'draft-for-r1');
    typeIntoSetOther(el, 'r3', 'draft-for-r3');
    await el.updateComplete;

    // 409 answer_set_changed: r3 was replaced (no longer open), r1 and r2 are still open.
    const fresh = setWithOther();
    fresh[2] = {
      ...fresh[2]!,
      status: 'replaced',
      replacedBy: { rowId: 'r3-v2', messageId: 'm2', version: 2, at: '2026-11-20T01:05:00Z' },
    };
    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'answer_set_changed',
          message: 'the decisions to answer changed',
          details: { decisions: fresh, reasons: { r3: 'replaced' } },
        },
      })
    );

    submitBtn(el).click();
    await settle(el);

    // Real assertion on internal draft map: r1 kept, r3 deleted (cleared, not just hidden).
    const drafts = (el as unknown as { setOtherDrafts: Map<string, string> }).setOtherDrafts;
    expect(drafts.get('r1')).toBe('draft-for-r1');
    expect(drafts.has('r3')).toBe(false);
    expect(
      rowEl(el, 'r1').querySelector<HTMLElement & { value: string }>('.editor sl-input')?.value
    ).toBe('draft-for-r1');
    expect(rowEl(el, 'r3').querySelector('.editor')).toBeNull();

    // Also verify SSE/prop update withdrawing r1 clears r1's draft too.
    el.decisions = fresh.map((d) =>
      d.rowId === 'r1'
        ? { ...d, status: 'withdrawn', withdrawn: { at: '2026-11-20T01:06:00Z', byName: 'bot' } }
        : d
    );
    await settle(el);
    expect(
      (el as unknown as { setOtherDrafts: Map<string, string> }).setOtherDrafts.has('r1')
    ).toBe(false);
  });

  it('renders stacked layout with Other description and inset editor on a set row', async () => {
    const ds = setWithOther();
    ds[0] = {
      ...ds[0]!,
      other: {
        label: 'Another region',
        description: 'Any `GCP` region code',
        placeholder: 'e.g. europe-west4',
      },
    };
    const el = await mount(ds);
    await settle(el);

    expect(rowEl(el, 'r1').querySelector('.choices')?.classList.contains('stack')).toBe(true);
    const otherOpt = rowChoice(el, 'r1', '$other');
    expect(otherOpt.classList.contains('other-opt')).toBe(true);
    expect(otherOpt.querySelector('.desc code')?.textContent).toBe('GCP');

    otherOpt.click();
    await settle(el);
    expect(rowEl(el, 'r1').querySelector('.opt.sel .editor.inset sl-input')).not.toBeNull();
    expect(rowEl(el, 'r1').querySelector('[data-action="send-other"]')).toBeNull();
  });
});
