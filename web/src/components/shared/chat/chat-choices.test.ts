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
