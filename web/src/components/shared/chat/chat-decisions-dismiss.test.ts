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
 * Dismissed row state in `<scion-chat-decisions>` (ptone/scion#8213, design 8194 §4.7):
 * single and set rendering, Jump to vN link, plain-text sink safety on `dismissed.reason`,
 * and 409 `dismissed` handling on single and set submissions.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';
import { setPreselectRecommendedChoices } from '../../../client/decision-preferences.js';

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
    question: 'Ship v0.9 today?',
    defaultChoiceId: 'yes',
    choices: [
      { id: 'yes', label: 'Yes' },
      { id: 'no', label: 'No' },
    ],
    ...over,
  };
}

function dismissedView(over: Partial<DecisionView> = {}): DecisionView {
  return view({
    status: 'dismissed',
    dismissed: {
      at: '2026-11-16T09:12:00Z',
      byUserId: 'u-alice',
      byName: 'alice',
      reason: 'Stale: v0.9 shipped last week.',
      surface: 'decisions_page',
      responseMessageId: 'resp-d1',
    },
    ...over,
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
  decisions: DecisionView[],
  opts: { currentUserId?: string; answerMode?: 'single' | 'set' } = {}
): Promise<ScionChatDecisions> {
  const el = document.createElement('scion-chat-decisions');
  el.messageId = 'm1';
  el.currentUserId = opts.currentUserId ?? 'u-me';
  el.answerMode = opts.answerMode;
  el.decisions = decisions;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

describe('<scion-chat-decisions> dismissed state (ptone/scion#8213)', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    showToast.mockReset();
    setPreselectRecommendedChoices(false);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    setPreselectRecommendedChoices(false);
  });

  it('renders single dismissed row with disabled choices, actor ("you" vs name), reason, and Jump to v2 link', async () => {
    const elOther = await mount([
      dismissedView({
        replacedBy: { rowId: 'r2', messageId: 'm2', version: 2, at: '2026-11-16T10:00:00Z' },
      }),
    ]);
    const rowOther = elOther.shadowRoot!.querySelector('.row.dismissed')!;
    expect(rowOther).not.toBeNull();
    expect(rowOther.getAttribute('data-status')).toBe('dismissed');
    for (const btn of Array.from(rowOther.querySelectorAll('sl-button'))) {
      expect(btn.hasAttribute('disabled')).toBe(true);
    }
    const wdOther = rowOther.querySelector('.wd')!;
    expect(wdOther.textContent).toContain('Dismissed by alice');
    expect(wdOther.querySelector('sl-icon.dc')?.getAttribute('name')).toBe('x-circle');
    expect(rowOther.querySelector('.wd-reason')?.textContent).toBe(
      'Stale: v0.9 shipped last week.'
    );

    const jumpEvents: string[] = [];
    elOther.addEventListener('decision-jump', (e) => {
      jumpEvents.push((e as CustomEvent<{ messageId: string }>).detail.messageId);
    });
    const jumpLink = wdOther.querySelector<HTMLAnchorElement>('a.jump')!;
    expect(jumpLink.textContent).toContain('Jump to v2');
    jumpLink.click();
    expect(jumpEvents).toEqual(['m2']);

    // When dismissed by the viewer, shows "Dismissed by you".
    const elSelf = await mount([dismissedView()], { currentUserId: 'u-alice' });
    expect(elSelf.shadowRoot!.querySelector('.wd')?.textContent).toContain('Dismissed by you');
  });

  it('renders dismissed.reason strictly as plain text (no HTML or Markdown execution)', async () => {
    const payload = '<img src="x" onerror="alert(1)"> [click](https://evil.example) **raw**';
    const el = await mount([
      dismissedView({
        dismissed: {
          at: '2026-11-16T09:12:00Z',
          byUserId: 'u-alice',
          byName: 'alice',
          reason: payload,
          surface: 'decisions_page',
          responseMessageId: 'resp-d1',
        },
      }),
    ]);
    await new Promise((r) => setTimeout(r, 15));
    await el.updateComplete;

    const reasonEl = el.shadowRoot!.querySelector('.wd-reason')!;
    expect(reasonEl.textContent).toBe(payload);
    expect(reasonEl.querySelector('img')).toBeNull();
    expect(reasonEl.querySelector('a')).toBeNull();
    expect(reasonEl.querySelector('strong')).toBeNull();
  });

  it('excludes dismissed rows from set counters and hides Submit when all rows are dismissed', async () => {
    const elPartial = await mount(
      [
        view({ rowId: 'r1', key: 'D234', ref: 'D234' }),
        dismissedView({
          rowId: 'r2',
          key: '17',
          ref: '17',
          question: 'Which approach?',
        }),
      ],
      { answerMode: 'set' }
    );
    const counterPartial = elPartial.shadowRoot!.querySelector('.card-foot .counter')!;
    expect(counterPartial.textContent).toContain('1 remaining');
    expect(counterPartial.textContent).toContain('1 dismissed');
    expect(elPartial.shadowRoot!.querySelector('.card-foot sl-button.submit')).not.toBeNull();

    const elAll = await mount(
      [
        dismissedView({ rowId: 'r1', key: 'D234', ref: 'D234' }),
        dismissedView({ rowId: 'r2', key: '17', ref: '17' }),
      ],
      { answerMode: 'set' }
    );
    const counterAll = elAll.shadowRoot!.querySelector('.card-foot .counter')!;
    expect(counterAll.textContent?.trim()).toBe('2 dismissed');
    expect(elAll.shadowRoot!.querySelector('.card-foot sl-button.submit')).toBeNull();
  });

  it('applies 409 dismissed on single answer and set submit', async () => {
    const elSingle = await mount([view()]);
    const freshDismissed = dismissedView();
    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'dismissed',
          message: 'decision D234 v1 was dismissed',
          details: { decision: freshDismissed },
        },
      })
    );
    elSingle.shadowRoot!.querySelector<HTMLElement>('sl-button[data-choice="yes"]')!.click();
    await new Promise((r) => setTimeout(r, 15));
    await elSingle.updateComplete;

    expect(showToast).toHaveBeenCalledWith('alice dismissed this decision.', 'warning');
    expect(elSingle.shadowRoot!.querySelector('.row.dismissed')).not.toBeNull();

    // Set 409 answer_set_changed with reason 'dismissed' -> "was dismissed".
    const r1 = view({ rowId: 'r1', key: 'D234', ref: 'D234' });
    const r2 = view({ rowId: 'r2', key: '17', ref: '17', question: 'Which approach?' });
    const elSet = await mount([r1, r2], { answerMode: 'set' });
    elSet.shadowRoot!.querySelector<HTMLElement>('[data-row="r1"] [data-choice="yes"]')!.click();
    elSet.shadowRoot!.querySelector<HTMLElement>('[data-row="r2"] [data-choice="no"]')!.click();
    await elSet.updateComplete;

    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'answer_set_changed',
          message: 'the decisions to answer changed; refresh and submit again',
          details: {
            decisions: [r1, dismissedView({ rowId: 'r2', key: '17', ref: '17' })],
            reasons: { r2: 'dismissed' },
          },
        },
      })
    );
    elSet.shadowRoot!.querySelector<HTMLElement>('sl-button.submit')!.click();
    await new Promise((r) => setTimeout(r, 15));
    await elSet.updateComplete;

    const alertText = elSet.shadowRoot!.querySelector('sl-alert.changed')?.textContent ?? '';
    expect(alertText).toContain('17 was dismissed');
  });
});
