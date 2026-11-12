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
 * Answerability on a set card (ptone/scion#7673, design §4.3, §4.5): a
 * blocked viewer gets every row and Submit disabled with the reason shown
 * once under the rows; a gone asker's set stays answerable with the note;
 * a 403 on the set submit blocks the card like a single click does.
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

await import('./chat-choices.js');
type ScionChatChoices = import('./chat-choices.js').ScionChatChoices;

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
    defaultChoiceId: 'yes',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
    ],
    ...over,
  };
}

/** Two rows, both recommended: Submit is ready unless blocked. */
function set(): DecisionView[] {
  return [row('r1', 'D234'), row('r2', '17')];
}

const BLOCKED: AskerAnswerability = {
  state: 'blocked',
  askerName: 'release-bot',
  reason: 'release-bot only takes messages from its creator.',
};

function res(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

async function mount(
  decisions: DecisionView[],
  answerability?: AskerAnswerability
): Promise<ScionChatChoices> {
  const el = document.createElement('scion-chat-choices');
  el.messageId = 'm1';
  el.currentUserId = 'u-me';
  el.answerMode = 'set';
  el.decisions = decisions;
  if (answerability) el.answerability = answerability;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatChoices): ShadowRoot => el.shadowRoot!;
const submit = (el: ScionChatChoices): HTMLElement =>
  root(el).querySelector<HTMLElement>('sl-button.submit')!;
const submitOrNull = (el: ScionChatChoices): HTMLElement | null =>
  root(el).querySelector<HTMLElement>('sl-button.submit');

/**
 * A blocked set (mock s10): nothing selected, the recommended choice
 * outlined, and no counter or Submit; the reason line is the footer.
 */
function expectBlockedLook(el: ScionChatChoices): void {
  for (const r of ['r1', 'r2']) {
    const yes = choice(el, r, 'yes');
    expect(yes.getAttribute('aria-pressed'), `${r}/yes`).toBe('false');
    expect(yes.getAttribute('variant')).toBe('primary');
    expect(yes.hasAttribute('outline')).toBe(true);
    expect(yes.querySelector('sl-icon')).toBeNull();
  }
  expect(submitOrNull(el)).toBeNull();
  expect(root(el).querySelector('.card-foot')).toBeNull();
  expect(root(el).querySelector('.counter')).toBeNull();
}
const choice = (el: ScionChatChoices, rowId: string, id: string): HTMLElement =>
  root(el).querySelector<HTMLElement>(`[data-row="${rowId}"] [data-choice="${id}"]`)!;

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

describe('blocked set card', () => {
  it('disables every row and Submit and shows the reason once', async () => {
    const el = await mount(set(), BLOCKED);

    for (const r of ['r1', 'r2']) {
      for (const c of ['yes', 'no']) {
        expect(choice(el, r, c).hasAttribute('disabled'), `${r}/${c}`).toBe(true);
        expect(choice(el, r, c).getAttribute('aria-disabled')).toBe('true');
      }
    }
    expectBlockedLook(el);
    const reasons = root(el).querySelectorAll('.reason');
    expect(reasons).toHaveLength(1);
    expect(reasons[0]!.textContent).toContain("You can't answer release-bot:");
    // The reason is the card's last element: it stands in for the footer.
    expect(root(el).querySelector('.card')!.lastElementChild).toBe(reasons[0]);
    const card = root(el).querySelector('.card')!;
    expect(card.getAttribute('aria-disabled')).toBe('true');
    expect(card.getAttribute('aria-describedby')).toBe(reasons[0]!.id);
  });

  it('ignores clicks and never posts', async () => {
    const el = await mount(set(), BLOCKED);
    choice(el, 'r1', 'no').click();
    await settle(el);

    expect(choice(el, 'r1', 'no').getAttribute('aria-pressed')).toBe('false');
    expect(submitOrNull(el)).toBeNull();
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('shows the read-only reason on a set', async () => {
    const el = await mount(set(), {
      state: 'blocked',
      readOnly: true,
      reason: "You can read this thread but can't post in it, so you can't answer.",
    });
    const reason = root(el).querySelector('.reason')!;
    expect(reason.getAttribute('data-reason')).toBe('read-only');
    expectBlockedLook(el);
  });

  it('returns to the normal set look, defaults selected, once unblocked', async () => {
    const el = await mount(set(), BLOCKED);
    expectBlockedLook(el);

    el.answerability = { state: 'ok' };
    await el.updateComplete;

    expect(choice(el, 'r1', 'yes').getAttribute('aria-pressed')).toBe('true');
    expect(choice(el, 'r1', 'yes').hasAttribute('outline')).toBe(false);
    expect(root(el).querySelector('.counter')?.textContent).toContain('0 remaining');
    expect(submit(el).hasAttribute('disabled')).toBe(false);
    expect(root(el).querySelector('.reason')).toBeNull();
  });
});

describe('gone asker on a set card', () => {
  it('stays answerable with the not-delivered note', async () => {
    apiFetch.mockResolvedValue(
      res(200, {
        responseMessageId: 'resp',
        batchId: 'b',
        decisions: set().map((d) => ({
          ...d,
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-me',
            provenance: 'direct',
            surface: 'web',
            responseMessageId: 'resp',
            batchId: 'b',
            at: '2026-11-12T15:44:00Z',
          },
        })),
      })
    );
    const el = await mount(set(), { state: 'gone' });

    const note = root(el).querySelector('[data-note="asker-gone"]');
    expect(note?.textContent).toContain('is no longer running');
    expect(submit(el).hasAttribute('disabled')).toBe(false);
    submit(el).click();
    await settle(el);
    expect(apiFetch).toHaveBeenCalledTimes(1);
    // Nothing pending any more: the note goes away.
    expect(root(el).querySelector('[data-note="asker-gone"]')).toBeNull();
  });
});

describe('403 on the set submit', () => {
  it('message_denied blocks the card, toasts and emits choice-answer-denied', async () => {
    apiFetch.mockResolvedValue(
      res(403, {
        error: {
          code: 'message_denied',
          message: 'denied',
          details: { reason: 'mode_none', senderMode: 'user', recipientMode: 'none' },
        },
      })
    );
    const el = await mount(set());
    const events: ChoiceAnswerDeniedDetail[] = [];
    el.addEventListener('choice-answer-denied', (e) =>
      events.push((e as CustomEvent<ChoiceAnswerDeniedDetail>).detail)
    );

    submit(el).click();
    await settle(el);

    expect(apiFetch.mock.calls[0]![0]).toBe('/api/v1/choice-prompts/m1/answers');
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({
      messageId: 'm1',
      askerAgentId: 'a1',
      code: 'message_denied',
      reason: 'mode_none',
    });
    expect(events[0]!.answerability.state).toBe('blocked');
    expect(showToast).toHaveBeenCalledWith(
      expect.stringContaining("You can't answer release-bot"),
      'warning'
    );
    expectBlockedLook(el);
    expect(choice(el, 'r1', 'no').hasAttribute('disabled')).toBe(true);
    expect(root(el).querySelectorAll('.reason')).toHaveLength(1);
  });

  it('chat_post_denied shows the read-only reason', async () => {
    apiFetch.mockResolvedValue(
      res(403, { error: { code: 'chat_post_denied', message: 'read only' } })
    );
    const el = await mount(set());
    submit(el).click();
    await settle(el);

    expect(root(el).querySelector('.reason')?.getAttribute('data-reason')).toBe('read-only');
    expectBlockedLook(el);
  });
});
