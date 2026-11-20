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
 * `<scion-chat-decisions>` withdrawn states (ptone/scion#8193, design §4.6,
 * mocks W1–W5, W7): single and set withdrawn rows, plain-text reason sink,
 * D7 Jump to v2, set counter and excluded summary, whole-set withdrawn card
 * footer, live patch selection drop and terminal ordering, 409 single toast
 * and set alert, and unknown future closed statuses.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';
import type { DecisionAnsweredDetail } from '../../../client/decision-prompts.js';
import { patchDecisions } from '../../../client/decision-prompts.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
const showToast = vi.hoisted(() => vi.fn());

vi.mock('../../../client/api.js', async (orig) => ({
  ...(await orig<typeof import('../../../client/api.js')>()),
  apiFetch,
}));
vi.mock('../../../utils/toast.js', () => ({ showToast }));

await import('./chat-decisions.js');
type ScionChatDecisions = import('./chat-decisions.js').ScionChatDecisions;

const WD_AT = '2026-11-16T15:52:00Z';

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

function withdrawn(
  d: DecisionView,
  opts: { reason?: string; at?: string; byName?: string } = {}
): DecisionView {
  return {
    ...d,
    status: 'withdrawn',
    withdrawn: {
      at: opts.at ?? WD_AT,
      ...(opts.reason !== undefined ? { reason: opts.reason } : {}),
      byName: opts.byName ?? d.askerAgentName,
    },
  };
}

function set(): DecisionView[] {
  return [
    row('r1', 'D234', { defaultChoiceId: 'yes', question: 'Ship `v0.9` today?' }),
    row('r2', '17', {
      question: 'Which approach?',
      choices: [
        { id: 'a', label: 'a · Extend input-needed' },
        { id: 'b', label: 'b · New waiting state' },
      ],
    }),
    row('r3', 'ship-now', { defaultChoiceId: 'no', question: 'Deploy to prod after tagging?' }),
  ];
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
  answerMode?: 'single' | 'set'
): Promise<ScionChatDecisions> {
  const el = document.createElement('scion-chat-decisions');
  el.messageId = 'm1';
  el.currentUserId = 'u-me';
  if (answerMode) el.answerMode = answerMode;
  el.decisions = decisions;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

const root = (el: ScionChatDecisions): ShadowRoot => el.shadowRoot!;
const text = (n: Element | null | undefined): string =>
  n?.textContent?.replace(/\s+/g, ' ').trim() ?? '';

beforeEach(() => {
  apiFetch.mockReset();
  showToast.mockReset();
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('single withdrawn decision (W1, D7, plain-text sink)', () => {
  it('fades and disables choices, hides Enter hint, and renders footer + optional reason (W1)', async () => {
    const el = await mount([
      withdrawn(row('r1', 'D234', { defaultChoiceId: 'yes', question: 'Ship `v0.9` today?' }), {
        reason: 'CI found a regression; the release moved to Monday.',
      }),
    ]);

    expect(root(el).querySelector('.row.withdrawn')).not.toBeNull();
    expect(root(el).querySelector('.card-head .version')).toBeNull();
    expect(root(el).querySelector('.hint')).toBeNull();

    for (const id of ['yes', 'no']) {
      const btn = root(el).querySelector<HTMLElement>(`[data-choice="${id}"]`)!;
      expect(btn.hasAttribute('disabled')).toBe(true);
      expect(btn.hasAttribute('outline')).toBe(false);
    }

    const wd = root(el).querySelector('.wd')!;
    expect(wd).not.toBeNull();
    expect(wd.querySelector('sl-icon')?.getAttribute('name')).toBe('slash-circle');
    expect(text(wd)).toMatch(/^No longer needed · withdrawn by release-bot · \d\d:\d\d$/);

    const reasonEl = root(el).querySelector('.wd-reason')!;
    expect(text(reasonEl)).toBe('CI found a regression; the release moved to Monday.');

    // Enter on the card does not post.
    root(el)
      .querySelector('.card')!
      .dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    expect(apiFetch).not.toHaveBeenCalled();
  });

  it('omits .wd-reason when reason is empty or absent', async () => {
    const el = await mount([withdrawn(row('r1', 'D234'))]);
    expect(root(el).querySelector('.wd')).not.toBeNull();
    expect(root(el).querySelector('.wd-reason')).toBeNull();
  });

  it('renders the reason strictly as plain text (sink test: no HTML or Markdown execution)', async () => {
    const raw =
      '<img src=x onerror="window.__xss=1"><script>alert(1)</script> [evil](https://evil.example) **bold** `code`';
    const el = await mount([withdrawn(row('r1', 'D234'), { reason: raw })]);

    const reasonEl = root(el).querySelector('.wd-reason')!;
    expect(reasonEl.textContent).toBe(raw);
    expect(reasonEl.children.length).toBe(0);
    expect(reasonEl.querySelector('img, script, a, strong, b, code')).toBeNull();
  });

  it('appends Jump to v2 and shows v1 in card header when re-asked with --update (D7)', async () => {
    const d = {
      ...withdrawn(row('r1', 'D234'), { reason: 'Superseded' }),
      replacedBy: { rowId: 'r1-v2', messageId: 'm2', version: 2, at: '2026-11-16T15:55:00Z' },
    };
    const el = await mount([d]);

    expect(text(root(el).querySelector('.card-head .version'))).toBe('v1');
    const wd = root(el).querySelector('.wd')!;
    expect(text(wd)).toMatch(
      /^No longer needed · withdrawn by release-bot · \d\d:\d\d · Jump to v2$/
    );

    const link = wd.querySelector<HTMLAnchorElement>('a.jump')!;
    expect(link.getAttribute('href')).toBe('#msg-m2');
    const onJump = vi.fn();
    document.body.addEventListener('decision-jump', onJump);
    const click = new MouseEvent('click', { bubbles: true, cancelable: true, composed: true });
    link.dispatchEvent(click);
    document.body.removeEventListener('decision-jump', onJump);
    expect(click.defaultPrevented).toBe(true);
    expect((onJump.mock.calls[0]![0] as CustomEvent).detail).toEqual({ messageId: 'm2' });
  });
});

describe('set with withdrawn rows and whole-set withdrawal (W2, W3, W4, W7)', () => {
  it('excludes a withdrawn row from the set counter and submits only the remaining rows (W2, W7)', async () => {
    const d = set();
    d[1] = withdrawn(d[1]!, { reason: 'Settled in review.' });
    const el = await mount(d, 'set');

    const r2 = root(el).querySelector('.row[data-row="r2"]')!;
    expect(r2.classList.contains('withdrawn')).toBe(true);
    expect(r2.querySelector('[data-choice="a"]')!.hasAttribute('disabled')).toBe(true);
    expect(text(r2.querySelector('.wd'))).toMatch(
      /^No longer needed · withdrawn by release-bot · \d\d:\d\d$/
    );
    expect(text(r2.querySelector('.wd-reason'))).toBe('Settled in review.');

    expect(text(root(el).querySelector('.counter'))).toBe('2 remaining · 1 no longer needed');

    // Select D234 -> 1 remaining · 1 no longer needed (W2)
    root(el).querySelector<HTMLElement>('.row[data-row="r1"] [data-choice="yes"]')!.click();
    await el.updateComplete;
    expect(text(root(el).querySelector('.counter'))).toBe('1 remaining · 1 no longer needed');
    expect(root(el).querySelector('sl-button.submit')!.hasAttribute('disabled')).toBe(true);

    // Select ship-now -> 0 remaining · 1 no longer needed (W7)
    root(el).querySelector<HTMLElement>('.row[data-row="r3"] [data-choice="no"]')!.click();
    await el.updateComplete;
    expect(text(root(el).querySelector('.counter'))).toBe('0 remaining · 1 no longer needed');
    expect(root(el).querySelector('sl-button.submit')!.hasAttribute('disabled')).toBe(false);

    // Submit posts only r1 and r3.
    const decidedAfter = [
      {
        ...d[0]!,
        status: 'decided' as const,
        decision: {
          choiceId: 'yes',
          label: 'Yes',
          answeredByUserId: 'u-me',
          answeredByName: 'alice',
          provenance: 'direct',
          surface: 'native',
          responseMessageId: 'resp-1',
          batchId: 'b-1',
          at: WD_AT,
        },
      },
      d[1]!,
      {
        ...d[2]!,
        status: 'decided' as const,
        decision: {
          choiceId: 'no',
          label: 'No',
          answeredByUserId: 'u-me',
          answeredByName: 'alice',
          provenance: 'direct',
          surface: 'native',
          responseMessageId: 'resp-1',
          batchId: 'b-1',
          at: WD_AT,
        },
      },
    ];
    apiFetch.mockResolvedValue(
      res(200, { decisions: decidedAfter, responseMessageId: 'resp-1', batchId: 'b-1' })
    );
    root(el).querySelector<HTMLElement>('sl-button.submit')!.click();
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalledTimes(1));
    const posted = JSON.parse(apiFetch.mock.calls[0]![1]?.body as string) as {
      answers: { decision_row_id: string; choice_id: string }[];
    };
    expect(posted.answers).toEqual([
      { decision_row_id: 'r1', choice_id: 'yes' },
      { decision_row_id: 'r3', choice_id: 'no' },
    ]);
  });

  it('renders partly answered + withdrawn set summary without collapsing row footers (W4)', async () => {
    const d = set();
    d[0] = {
      ...d[0]!,
      status: 'decided',
      decision: {
        choiceId: 'yes',
        label: 'Yes',
        answeredByUserId: 'u-alice',
        answeredByName: 'alice',
        provenance: 'direct',
        surface: 'native',
        responseMessageId: 'resp-1',
        at: '2026-11-16T15:44:00Z',
      },
    };
    d[1] = withdrawn(d[1]!);
    d[2] = withdrawn(d[2]!);
    const el = await mount(d, 'set');

    expect(root(el).querySelector('.card.withdrawn-all')).toBeNull();
    expect(root(el).querySelectorAll('.row .wd').length).toBe(2);
    expect(text(root(el).querySelector('.counter'))).toBe(
      '1 answered by alice · 2 no longer needed'
    );
    expect(root(el).querySelector('sl-button.submit')).toBeNull();
  });

  it('collapses whole withdrawn set into one card-footer line, omits Submit, and drops local selections (W3)', async () => {
    const pendingSet = set();
    const el = await mount(pendingSet, 'set');

    // User selects choices on r1 and r2 before withdrawal arrives.
    root(el).querySelector<HTMLElement>('.row[data-row="r1"] [data-choice="yes"]')!.click();
    root(el).querySelector<HTMLElement>('.row[data-row="r2"] [data-choice="a"]')!.click();
    await el.updateComplete;
    expect(
      root(el)
        .querySelector('.row[data-row="r1"] [data-choice="yes"]')!
        .getAttribute('aria-pressed')
    ).toBe('true');

    // Live update withdraws all three rows in the same call.
    const wholeWd = pendingSet.map((item) =>
      withdrawn(item, { reason: "Release cancelled; I'll ask again for the next one." })
    );
    el.decisions = wholeWd;
    await el.updateComplete;

    expect(root(el).querySelector('.card.withdrawn-all')).not.toBeNull();
    expect(root(el).querySelectorAll('.row.withdrawn').length).toBe(3);
    // Per-row footers collapsed; one .wd in .card-foot instead.
    expect(root(el).querySelectorAll('.row .wd').length).toBe(0);
    const cardFoot = root(el).querySelector('.card-foot')!;
    expect(text(cardFoot.querySelector('.wd'))).toMatch(
      /^No longer needed · withdrawn by release-bot · \d\d:\d\d$/
    );
    expect(text(cardFoot.querySelector('.wd-reason'))).toBe(
      "Release cancelled; I'll ask again for the next one."
    );
    expect(root(el).querySelector('sl-button.submit')).toBeNull();
    expect(root(el).querySelector('.note')).toBeNull();

    // Local selections were dropped and buttons are not selected/pressed.
    for (const btn of Array.from(root(el).querySelectorAll('button.sb'))) {
      expect(btn.classList.contains('sb-filled')).toBe(false);
      expect(btn.hasAttribute('disabled')).toBe(true);
    }
  });

  it('keeps per-row footers when rows were withdrawn in separate calls with different reasons', async () => {
    const d = [
      withdrawn(row('r1', 'D1'), { at: '2026-11-16T15:50:00Z', reason: 'First reason' }),
      withdrawn(row('r2', 'D2'), { at: '2026-11-16T15:52:00Z', reason: 'Second reason' }),
    ];
    const el = await mount(d, 'set');

    expect(root(el).querySelector('.card.withdrawn-all')).toBeNull();
    expect(root(el).querySelectorAll('.row .wd').length).toBe(2);
    expect(text(root(el).querySelector('.counter'))).toBe('2 no longer needed');
  });
});

describe('live patch ordering and 409 handling (W5)', () => {
  it('updates an open set card live when one selected row is withdrawn, and ignores stale pending patches', async () => {
    const d = set();
    const el = await mount(d, 'set');

    root(el).querySelector<HTMLElement>('.row[data-row="r1"] [data-choice="yes"]')!.click();
    root(el).querySelector<HTMLElement>('.row[data-row="r2"] [data-choice="b"]')!.click();
    await el.updateComplete;
    expect(text(root(el).querySelector('.counter'))).toBe('1 remaining');

    const wdRow2 = withdrawn(d[1]!, { reason: 'No longer applicable' });
    const afterWd = patchDecisions(el.decisions, [wdRow2]);
    el.decisions = afterWd;
    await el.updateComplete;

    // r2 is now withdrawn, r1 selection remains, r3 still needs an answer -> 1 remaining · 1 no longer needed.
    expect(text(root(el).querySelector('.counter'))).toBe('1 remaining · 1 no longer needed');
    expect(
      root(el)
        .querySelector('.row[data-row="r1"] [data-choice="yes"]')!
        .getAttribute('aria-pressed')
    ).toBe('true');

    // Stale pending patch for r2 must NOT revert withdrawn back to pending.
    const afterStale = patchDecisions(el.decisions, [d[1]!]);
    expect(afterStale).toBe(el.decisions);
    el.decisions = afterStale;
    await el.updateComplete;
    expect(root(el).querySelector('.row[data-row="r2"]')!.classList.contains('withdrawn')).toBe(
      true
    );
  });

  it('applies details.decision and toasts "No longer needed: <name> withdrew this decision" on single 409 withdrawn', async () => {
    const pending = row('r1', 'D234', { defaultChoiceId: 'yes' });
    const el = await mount([pending]);

    const wd = withdrawn(pending, { reason: 'Moved to Monday' });
    apiFetch.mockResolvedValue(
      res(409, {
        error: {
          code: 'withdrawn',
          message: 'decision D234 v1 was withdrawn',
          details: { decision: wd },
        },
      })
    );

    const answeredEvents: DecisionAnsweredDetail[] = [];
    el.addEventListener('decision-answered', (e) => {
      answeredEvents.push((e as CustomEvent<DecisionAnsweredDetail>).detail);
    });

    root(el).querySelector<HTMLElement>('[data-choice="yes"]')!.click();
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledTimes(1));
    await el.updateComplete;

    expect(showToast).toHaveBeenCalledWith(
      'No longer needed: release-bot withdrew this decision',
      'warning'
    );
    expect(root(el).querySelector('.row.withdrawn')).not.toBeNull();
    expect(text(root(el).querySelector('.wd-reason'))).toBe('Moved to Monday');
    expect(answeredEvents).toHaveLength(1);
    expect(answeredEvents[0]?.decisions[0]?.status).toBe('withdrawn');
  });

  it('shows "is no longer needed" inline alert and keeps other selections on set 409 answer_set_changed (W5)', async () => {
    const d = set();
    const el = await mount(d, 'set');

    root(el).querySelector<HTMLElement>('.row[data-row="r1"] [data-choice="yes"]')!.click();
    root(el).querySelector<HTMLElement>('.row[data-row="r2"] [data-choice="a"]')!.click();
    root(el).querySelector<HTMLElement>('.row[data-row="r3"] [data-choice="no"]')!.click();
    await el.updateComplete;

    const fresh = [d[0]!, withdrawn(d[1]!), d[2]!];
    apiFetch.mockResolvedValue(
      res(409, {
        error: {
          code: 'answer_set_changed',
          message: 'the answer set changed',
          details: {
            decisions: fresh,
            reasons: { r2: 'withdrawn' },
          },
        },
      })
    );

    root(el).querySelector<HTMLElement>('sl-button.submit')!.click();
    await vi.waitFor(() => expect(root(el).querySelector('sl-alert.changed')).not.toBeNull());
    await el.updateComplete;

    const alertText = text(root(el).querySelector('sl-alert.changed'));
    expect(alertText).toContain('This changed while you were choosing: 17 is no longer needed.');
    expect(alertText).toContain('Your other selections are kept');
    expect(root(el).querySelector('.row[data-row="r2"]')!.classList.contains('withdrawn')).toBe(
      true
    );
    expect(text(root(el).querySelector('.counter'))).toBe('0 remaining · 1 no longer needed');
    expect(root(el).querySelector('sl-button.submit')!.hasAttribute('disabled')).toBe(false);
  });
});

describe('unknown future statuses render closed', () => {
  it('renders a single card with an unknown status as closed and non-interactive', async () => {
    const el = await mount([row('r1', 'D234', { status: 'expired_v99', defaultChoiceId: 'yes' })]);

    expect(root(el).querySelector('.row.closed')).not.toBeNull();
    expect(root(el).querySelector('.hint')).toBeNull();
    expect(text(root(el).querySelector('.card'))).not.toContain('Awaiting your answer');
    for (const id of ['yes', 'no']) {
      expect(root(el).querySelector(`[data-choice="${id}"]`)!.hasAttribute('disabled')).toBe(true);
    }
  });

  it('renders an unknown status row in a set as closed and excludes it from the counter', async () => {
    const d = set();
    d[1] = { ...d[1]!, status: 'expired_v99' };
    const el = await mount(d, 'set');

    expect(root(el).querySelector('.row[data-row="r2"]')!.classList.contains('closed')).toBe(true);
    expect(
      root(el).querySelector('.row[data-row="r2"] [data-choice="a"]')!.hasAttribute('disabled')
    ).toBe(true);
    expect(text(root(el).querySelector('.counter'))).toBe('2 remaining · 1 closed');
  });
});
