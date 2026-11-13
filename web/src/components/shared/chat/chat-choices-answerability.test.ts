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
 * `<scion-chat-choices>` answerability (ptone/scion#7673, design §4.3
 * "Pending, blocked" and "Asker gone", §4.5, §4.7): blocked before the
 * click with a visible reason, read-only, each 403 code on click, and the
 * gone asker's note.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';
import type { AskerAnswerability, ChoiceAnswerDeniedDetail } from '../../../client/chat-choices.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
const showToast = vi.hoisted(() => vi.fn());

vi.mock('../../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../../client/api.js')>()),
  apiFetch,
}));
vi.mock('../../../utils/toast.js', () => ({ showToast }));

const { deniedAnswerability, readOnlyAnswerability } =
  await import('../../../client/chat-choices.js');
await import('./chat-choices.js');
type ScionChatChoices = import('./chat-choices.js').ScionChatChoices;

const LINEAGE =
  'release-bot is in lineage mode and only accepts messages from users in its ancestry. You are not in its lineage.';

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
    ],
    ...over,
  };
}

function decided(): DecisionView {
  return view({
    status: 'decided',
    decision: {
      choiceId: 'no',
      label: 'No',
      answeredByUserId: 'u-bob',
      answeredByName: 'Bob',
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

async function mount(
  answerability?: AskerAnswerability,
  decisions: DecisionView[] = [view()]
): Promise<ScionChatChoices> {
  const el = document.createElement('scion-chat-choices');
  el.messageId = 'm1';
  el.currentUserId = 'u-me';
  el.decisions = decisions;
  if (answerability) el.answerability = answerability;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatChoices): ShadowRoot => el.shadowRoot!;
const button = (el: ScionChatChoices, id: string): HTMLElement =>
  root(el).querySelector<HTMLElement>(`[data-choice="${id}"]`)!;
const card = (el: ScionChatChoices): HTMLElement => root(el).querySelector<HTMLElement>('.card')!;
const reason = (el: ScionChatChoices): HTMLElement | null =>
  root(el).querySelector<HTMLElement>('.reason');
const text = (n: Element | null): string => n?.textContent?.replace(/\s+/g, ' ').trim() ?? '';

async function settle(el: ScionChatChoices): Promise<void> {
  for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

function expectBlocked(el: ScionChatChoices): void {
  expect(el.answerBlocked).toBe(true);
  for (const id of ['yes', 'no']) {
    const b = button(el, id);
    expect(b.getAttribute('aria-disabled')).toBe('true');
    expect(b.hasAttribute('disabled')).toBe(true);
  }
  expect(card(el).getAttribute('aria-disabled')).toBe('true');
  const r = reason(el)!;
  expect(r).not.toBeNull();
  expect(card(el).getAttribute('aria-describedby')).toBe(r.id);
  // No "Awaiting your answer" and no Enter hint while blocked.
  expect(root(el).querySelector('.hint')).toBeNull();
  expect(text(root(el).querySelector('.row'))).not.toContain('Awaiting your answer');
}

beforeEach(() => {
  apiFetch.mockReset();
  showToast.mockReset();
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('blocked before the click', () => {
  it('disables the choices and shows the asker rule with a lock icon', async () => {
    const el = await mount(deniedAnswerability('mode_lineage_no_ancestry', 'release-bot'));
    expectBlocked(el);
    const r = reason(el)!;
    expect(r.querySelector('sl-icon')!.getAttribute('name')).toBe('lock');
    expect(r.querySelector('b')!.textContent).toBe('release-bot');
    expect(text(r)).toBe(`You can't answer release-bot: ${LINEAGE}`);
  });

  it('keeps the recommended choice outlined while disabled', async () => {
    const el = await mount(deniedAnswerability('mode_none', 'release-bot'));
    expect(button(el, 'yes').hasAttribute('outline')).toBe(true);
  });

  it('sends nothing on click or Enter', async () => {
    const el = await mount(deniedAnswerability('mode_none', 'release-bot'));
    button(el, 'no').click();
    card(el).dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('shows the read-only reason with an eye icon', async () => {
    const el = await mount(readOnlyAnswerability());
    expectBlocked(el);
    const r = reason(el)!;
    expect(r.querySelector('sl-icon')!.getAttribute('name')).toBe('eye');
    expect(text(r)).toBe("You can read this thread but can't post in it, so you can't answer.");
  });

  it('blocks the stacked rows of choices with descriptions too', async () => {
    const el = await mount(deniedAnswerability('mode_none', 'release-bot'), [
      view({
        choices: [
          { id: 'yes', label: 'Yes', description: 'ship it' },
          { id: 'no', label: 'No' },
        ],
      }),
    ]);
    expect(button(el, 'yes').tagName).toBe('BUTTON');
    expect(button(el, 'yes').getAttribute('aria-disabled')).toBe('true');
    button(el, 'yes').click();
    await settle(el);
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('shows the answer, not the reason, on a decided card', async () => {
    const el = await mount(deniedAnswerability('mode_none', 'release-bot'), [decided()]);
    expect(reason(el)).toBeNull();
    expect(text(root(el).querySelector('.row-foot'))).toContain('Answered by Bob');
  });

  it('stays answerable when the lookup failed (unknown)', async () => {
    const el = await mount({ state: 'unknown' });
    expect(el.answerBlocked).toBe(false);
    expect(reason(el)).toBeNull();
    expect(button(el, 'yes').hasAttribute('disabled')).toBe(false);
  });
});

describe('403 on click (authoritative)', () => {
  function denied(code: string, details: Record<string, unknown> = {}): Response {
    return res(403, { error: { code, message: 'denied', details } });
  }

  async function clickDenied(
    response: Response
  ): Promise<{ el: ScionChatChoices; events: ChoiceAnswerDeniedDetail[] }> {
    const el = await mount();
    const events: ChoiceAnswerDeniedDetail[] = [];
    el.addEventListener('choice-answer-denied', (e) =>
      events.push((e as CustomEvent<ChoiceAnswerDeniedDetail>).detail)
    );
    apiFetch.mockResolvedValue(response);
    button(el, 'no').click();
    await settle(el);
    return { el, events };
  }

  it('message_denied blocks with details.reason, toasts and emits', async () => {
    const { el, events } = await clickDenied(
      denied('message_denied', { reason: 'mode_lineage_no_ancestry' })
    );
    expectBlocked(el);
    expect(text(reason(el))).toBe(`You can't answer release-bot: ${LINEAGE}`);
    expect(showToast).toHaveBeenCalledWith(`You can't answer release-bot: ${LINEAGE}`, 'warning');
    expect(events).toEqual([
      {
        messageId: 'm1',
        askerAgentId: 'a1',
        code: 'message_denied',
        reason: 'mode_lineage_no_ancestry',
        answerability: {
          state: 'blocked',
          askerName: 'release-bot',
          reason: LINEAGE,
        },
      },
    ]);
    // Nothing recorded: the row is still pending.
    expect(root(el).querySelector('.row')!.getAttribute('data-status')).toBe('pending');
  });

  it('chat_post_denied shows the read-only reason', async () => {
    const { el, events } = await clickDenied(denied('chat_post_denied', { reason: 'read_only' }));
    expectBlocked(el);
    expect(reason(el)!.querySelector('sl-icon')!.getAttribute('name')).toBe('eye');
    expect(text(reason(el))).toContain("can't post in it");
    expect(events[0]!.answerability.readOnly).toBe(true);
    expect(showToast).toHaveBeenCalledWith(
      "You can read this thread but can't post in it, so you can't answer.",
      'warning'
    );
  });

  it('any other 403 shows the generic refusal', async () => {
    const { el, events } = await clickDenied(denied('forbidden'));
    expectBlocked(el);
    expect(text(reason(el))).toBe("You can't answer this decision.");
    expect(reason(el)!.querySelector('sl-icon')!.getAttribute('name')).toBe('lock');
    expect(events[0]!.code).toBe('forbidden');
  });

  it('the 403 wins over a later ok from the thread', async () => {
    const { el } = await clickDenied(denied('forbidden'));
    el.answerability = { state: 'ok' };
    await el.updateComplete;
    expect(el.answerBlocked).toBe(true);
  });
});

describe('asker gone', () => {
  it('stays answerable with a muted note', async () => {
    const el = await mount({ state: 'gone' });
    expect(el.answerBlocked).toBe(false);
    expect(reason(el)).toBeNull();
    expect(button(el, 'yes').hasAttribute('disabled')).toBe(false);
    const note = root(el).querySelector('.note')!;
    expect(note.querySelector('b')!.textContent).toBe('release-bot');
    expect(text(note)).toBe(
      'release-bot is no longer running. Your answer will be recorded but not delivered.'
    );
    expect(note.querySelector('sl-icon')!.getAttribute('name')).toBe('info-circle');
    expect(text(root(el).querySelector('.row-foot'))).toContain('Awaiting your answer');
  });

  it('records the answer when clicked', async () => {
    const el = await mount({ state: 'gone' });
    apiFetch.mockResolvedValue(res(200, { decision: decided(), responseMessageId: 'resp-1' }));
    button(el, 'no').click();
    await settle(el);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(root(el).querySelector('.note')).toBeNull();
  });
});
