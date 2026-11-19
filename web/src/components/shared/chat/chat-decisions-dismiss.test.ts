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
 * Dismissed state, card menu, and dismiss dialog in `<scion-chat-decisions>`
 * (ptone/scion#8213, ptone/scion#8216, design 8194 §4.4a, §4.7, Mocks X1–X4b, X8, X9):
 * single and set row rendering, whole-card footer collapse, Jump to vN link,
 * plain-text sink safety on `dismissed.reason`, `⋯` menu visibility and 5-minute
 * countdown, full row-picker dialog, and 409 (`dismissed`, `dismiss_changed`,
 * `too_recent`) handling.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import type { DecisionView } from '../../../shared/types.js';
import type { AskerAnswerability } from '../../../client/decision-prompts.js';
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
  opts: {
    currentUserId?: string;
    answerMode?: 'single' | 'set';
    answerability?: AskerAnswerability;
    surface?: 'native' | 'decisions_page';
    createdAt?: string;
    conversationLabel?: string;
    permalinkHref?: string;
    waitingReasons?: Record<string, string>;
    askerGone?: boolean;
  } = {}
): Promise<ScionChatDecisions> {
  const el = document.createElement('scion-chat-decisions');
  el.messageId = 'm1';
  el.currentUserId = opts.currentUserId ?? 'u-me';
  el.answerMode = opts.answerMode;
  if (opts.answerability) el.answerability = opts.answerability;
  if (opts.surface) el.surface = opts.surface;
  if (opts.createdAt) el.createdAt = opts.createdAt;
  if (opts.conversationLabel) el.conversationLabel = opts.conversationLabel;
  if (opts.permalinkHref) el.permalinkHref = opts.permalinkHref;
  if (opts.waitingReasons) el.waitingReasons = opts.waitingReasons;
  if (opts.askerGone !== undefined) el.askerGone = opts.askerGone;
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

    // Rows dismissed in separate calls keep per-row footers and show "2 dismissed" counter.
    const elSeparate = await mount(
      [
        dismissedView({ rowId: 'r1', key: 'D234', ref: 'D234' }),
        dismissedView({
          rowId: 'r2',
          key: '17',
          ref: '17',
          dismissed: {
            at: '2026-11-16T09:20:00Z',
            byUserId: 'u-bob',
            byName: 'bob',
            reason: 'Later call.',
            surface: 'native',
            responseMessageId: 'resp-d2',
          },
        }),
      ],
      { answerMode: 'set' }
    );
    expect(elSeparate.shadowRoot!.querySelector('.card.dismissed-all')).toBeNull();
    expect(elSeparate.shadowRoot!.querySelectorAll('.row .wd')).toHaveLength(2);
    const counterSeparate = elSeparate.shadowRoot!.querySelector('.card-foot .counter')!;
    expect(counterSeparate.textContent?.trim()).toBe('2 dismissed');
    expect(elSeparate.shadowRoot!.querySelector('.card-foot sl-button.submit')).toBeNull();
  });

  it('collapses a whole set dismissed in one call into a single card footer with no Submit or menu (Mock X3)', async () => {
    const elAll = await mount(
      [
        dismissedView({ rowId: 'r1', key: 'D234', ref: 'D234' }),
        dismissedView({ rowId: 'r2', key: '17', ref: '17', question: 'Which approach?' }),
        dismissedView({
          rowId: 'r3',
          key: 'ship-now',
          ref: 'ship-now',
          question: 'Deploy to prod after tagging?',
        }),
      ],
      { answerMode: 'set' }
    );
    const card = elAll.shadowRoot!.querySelector('.card.set.dismissed-all')!;
    expect(card).not.toBeNull();
    expect(card.querySelector('.card-head .tag-wd')?.textContent).toBe('Dismissed');
    expect(card.querySelector('[data-card-menu]')).toBeNull();
    expect(card.querySelectorAll('.row .wd')).toHaveLength(0);
    const foot = card.querySelector('.card-foot.card-foot-wd')!;
    expect(foot).not.toBeNull();
    expect(foot.querySelector('.wd')?.textContent).toContain('Dismissed by alice');
    expect(foot.querySelector('.wd-reason')?.textContent).toBe('Stale: v0.9 shipped last week.');
    expect(card.querySelector('sl-button.submit')).toBeNull();
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

  it('shows the ⋯ menu only when at least one row waits on the viewer, and disables Dismiss… with countdown before 5 minutes (Mocks X2, X4, X9)', async () => {
    // Pending single -> menu shown
    const elOpen = await mount([view()]);
    expect(elOpen.shadowRoot!.querySelector('[data-card-menu-trigger]')).not.toBeNull();
    const openItem = elOpen.shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')!;
    expect(openItem.hasAttribute('disabled')).toBe(false);

    // Interpreted awaiting viewer confirm (D6) -> menu shown
    const elOwnInterp = await mount(
      [
        view({
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-me',
            answeredByName: 'Me',
            provenance: 'agent_interpreted',
            surface: 'web',
            responseMessageId: 'resp-i',
            at: '2026-11-16T09:01:00Z',
          },
        }),
      ],
      { currentUserId: 'u-me' }
    );
    expect(elOwnInterp.shadowRoot!.querySelector('[data-card-menu-trigger]')).not.toBeNull();

    // Interpreted awaiting bob -> menu hidden
    const elBobInterp = await mount(
      [
        view({
          status: 'decided',
          decision: {
            choiceId: 'yes',
            label: 'Yes',
            answeredByUserId: 'u-bob',
            answeredByName: 'bob',
            provenance: 'agent_interpreted',
            surface: 'web',
            responseMessageId: 'resp-i',
            at: '2026-11-16T09:01:00Z',
          },
        }),
      ],
      { currentUserId: 'u-me' }
    );
    expect(elBobInterp.shadowRoot!.querySelector('[data-card-menu-trigger]')).toBeNull();

    // Blocked viewer (read-only thread) -> menu hidden
    const elBlocked = await mount([view()], {
      answerability: { state: 'blocked', readOnly: true, reason: 'Read-only thread' },
    });
    expect(elBlocked.shadowRoot!.querySelector('[data-card-menu-trigger]')).toBeNull();

    // Too recent (< 5 min): menu shown, Dismiss… disabled with countdown tooltip, unlocks at boundary
    const createdAt = new Date(Date.now() - (5 * 60_000 - 250)).toISOString();
    const elRecent = await mount([view()], { createdAt });
    const recentItem = elRecent.shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')!;
    expect(recentItem.hasAttribute('disabled')).toBe(true);
    expect(
      elRecent.shadowRoot!.querySelector('[data-dismiss-tooltip]')?.getAttribute('content')
    ).toBe('Dismiss available in 1 min');

    // Clicking while disabled does not open dialog
    recentItem.click();
    await elRecent.updateComplete;
    expect(elRecent.shadowRoot!.querySelector('sl-dialog[data-dismiss-dialog]')).toBeNull();

    // Wait past the 250ms remaining boundary
    await new Promise((r) => setTimeout(r, 320));
    await elRecent.updateComplete;
    expect(
      elRecent
        .shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')
        ?.hasAttribute('disabled')
    ).toBe(false);
    expect(elRecent.shadowRoot!.querySelector('[data-dismiss-tooltip]')).toBeNull();
  });

  it('opens the set row-picker dialog, disables rows waiting on others, enforces 500-char reason counter, and sends only checked decisionIds (Mock X4)', async () => {
    const r1 = view({ rowId: 'r1', key: 'D234', ref: 'D234', question: 'Ship `v0.9` today?' });
    const r2 = view({ rowId: 'r2', key: '17', ref: '17', question: 'Which approach?' });
    const rBob = view({
      rowId: 'r3',
      key: 'ship-now',
      ref: 'ship-now',
      question: 'Deploy to prod after tagging?',
      status: 'decided',
      decision: {
        choiceId: 'yes',
        label: 'Yes',
        answeredByUserId: 'u-bob',
        answeredByName: 'bob',
        provenance: 'agent_interpreted',
        surface: 'web',
        responseMessageId: 'resp-bob',
        at: '2026-11-16T09:01:00Z',
      },
    });
    const el = await mount([r1, r2, rBob], {
      currentUserId: 'u-me',
      answerMode: 'set',
      conversationLabel: '#release',
    });

    el.shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')!.click();
    await el.updateComplete;

    const dialog = el.shadowRoot!.querySelector<HTMLElement>('sl-dialog[data-dismiss-dialog]')!;
    expect(dialog).not.toBeNull();
    expect(dialog.getAttribute('label')).toBe('Dismiss decisions');
    expect(dialog.querySelector('[data-dismiss-tell="live"]')?.textContent).toContain(
      'release-bot will be told in #release.'
    );

    const check1 = dialog.querySelector<HTMLElement & { checked: boolean }>(
      '[data-dismiss-check="r1"]'
    )!;
    const check2 = dialog.querySelector<HTMLElement & { checked: boolean }>(
      '[data-dismiss-check="r2"]'
    )!;
    const disabledBob = dialog.querySelector<HTMLElement>('[data-dismiss-row="r3"] sl-checkbox')!;
    expect(check1.hasAttribute('checked')).toBe(true);
    expect(check2.hasAttribute('checked')).toBe(true);
    expect(disabledBob.hasAttribute('disabled')).toBe(true);
    expect(dialog.querySelector('[data-dismiss-row="r3"]')?.textContent).toContain(
      "Waiting on bob to confirm release-bot's reading"
    );

    const confirmBtn = dialog.querySelector<HTMLElement>('[data-dismiss-confirm]')!;
    expect(confirmBtn.textContent?.trim()).toBe('Dismiss 2 decisions');

    // Uncheck r2 -> button updates to "Dismiss 1 decision"
    check2.checked = false;
    check2.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(confirmBtn.textContent?.trim()).toBe('Dismiss 1 decision');

    // Uncheck r1 -> 0 checked -> button disabled
    check1.checked = false;
    check1.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(confirmBtn.hasAttribute('disabled')).toBe(true);

    // Re-check r1
    check1.checked = true;
    check1.dispatchEvent(new Event('sl-change'));
    await el.updateComplete;
    expect(confirmBtn.hasAttribute('disabled')).toBe(false);

    // Reason counter & 500-rune limit clamping
    const reasonField = dialog.querySelector<HTMLElement & { value: string }>(
      '[data-dismiss-reason]'
    )!;
    expect(dialog.querySelector('[data-dismiss-reason-count]')?.textContent).toBe('0/500');
    reasonField.value = 'x'.repeat(520);
    reasonField.dispatchEvent(new Event('sl-input', { bubbles: true, composed: true }));
    await el.updateComplete;
    expect(dialog.querySelector('[data-dismiss-reason-count]')?.textContent).toBe('500/500');

    reasonField.value = '  Stale: v0.9 shipped.  ';
    reasonField.dispatchEvent(new Event('sl-input', { bubbles: true, composed: true }));
    await el.updateComplete;
    expect(dialog.querySelector('[data-dismiss-reason-count]')?.textContent).toBe('24/500');

    const r1Dismissed = dismissedView({
      rowId: 'r1',
      key: 'D234',
      ref: 'D234',
      dismissed: {
        at: '2026-11-16T09:15:00Z',
        byUserId: 'u-me',
        byName: 'Me',
        reason: 'Stale: v0.9 shipped.',
        surface: 'native',
        responseMessageId: 'resp-new',
      },
    });
    apiFetch.mockResolvedValueOnce(
      res(200, {
        dismissed: [r1Dismissed],
        alreadyDismissed: [],
        responseMessageId: 'resp-new',
        delivered: true,
        deliveryStatus: 'delivered',
      })
    );

    const answeredEvents: unknown[] = [];
    el.addEventListener('decision-answered', (e) => answeredEvents.push((e as CustomEvent).detail));

    confirmBtn.click();
    await new Promise((r) => setTimeout(r, 15));
    await el.updateComplete;

    expect(apiFetch).toHaveBeenCalledTimes(1);
    const [url, init] = apiFetch.mock.calls[0]!;
    expect(url).toBe('/api/v1/decision-prompts/m1/dismiss');
    const sent = JSON.parse(init?.body as string) as Record<string, unknown>;
    expect(sent.decisionIds).toEqual(['r1']);
    expect(sent.reason).toBe('Stale: v0.9 shipped.');
    expect(sent.surface).toBe('native');
    expect(typeof sent.idempotencyKey).toBe('string');
    expect((sent.idempotencyKey as string).length).toBeGreaterThan(0);

    expect(el.shadowRoot!.querySelector('sl-dialog[data-dismiss-dialog]')).toBeNull();
    expect(el.shadowRoot!.querySelector('[data-row="r1"].dismissed')).not.toBeNull();
    expect(answeredEvents).toHaveLength(1);
    expect(showToast).toHaveBeenCalledWith('Decision dismissed.', 'success');
  });

  it('shows gone-agent notice in single dialog when asker is gone, and handles 409 dismiss_changed and too_recent (Mocks X4b, X9)', async () => {
    const goneEl = await mount(
      [
        view({
          rowId: 'r-mig',
          key: 'migrate',
          ref: 'migrate',
          askerAgentId: 'a-gone',
          askerAgentName: 'nc-x',
          question: 'Run the backfill on staging first?',
        }),
      ],
      { answerability: { state: 'gone' } }
    );
    goneEl.shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')!.click();
    await goneEl.updateComplete;

    const goneDialog = goneEl.shadowRoot!.querySelector<HTMLElement>(
      'sl-dialog[data-dismiss-dialog]'
    )!;
    expect(goneDialog.getAttribute('label')).toBe('Dismiss decision');
    expect(goneDialog.querySelector('[data-dismiss-tell="gone"]')?.textContent).toContain(
      'nc-x is gone; nothing will be sent.'
    );
    expect(goneDialog.querySelector('[data-dismiss-confirm]')?.textContent?.trim()).toBe('Dismiss');

    // 409 dismiss_changed refreshes the card and toasts
    const decidedFresh = view({
      rowId: 'r-mig',
      key: 'migrate',
      ref: 'migrate',
      status: 'decided',
      decision: {
        choiceId: 'yes',
        label: 'Yes',
        answeredByUserId: 'u-bob',
        answeredByName: 'bob',
        provenance: 'direct',
        surface: 'web',
        responseMessageId: 'resp-b',
        at: '2026-11-16T09:16:00Z',
      },
    });
    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'dismiss_changed',
          message: 'the decisions changed',
          details: { decisions: [decidedFresh], reasons: { 'r-mig': 'decided' } },
        },
      })
    );
    goneDialog.querySelector<HTMLElement>('[data-dismiss-confirm]')!.click();
    await new Promise((r) => setTimeout(r, 15));
    await goneEl.updateComplete;

    expect(goneEl.shadowRoot!.querySelector('sl-dialog[data-dismiss-dialog]')).toBeNull();
    expect(showToast).toHaveBeenCalledWith('Nothing dismissed: this card changed.', 'warning');
    expect(
      goneEl.shadowRoot!.querySelector('[data-row="r-mig"]')?.getAttribute('data-status')
    ).toBe('decided');

    // 409 too_recent re-arms countdown lock on the menu item
    const skewEl = await mount([view({ rowId: 'r-skew' })]);
    skewEl.shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')!.click();
    await skewEl.updateComplete;
    const availAt = new Date(Date.now() + 2 * 60_000 + 30_000).toISOString();
    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'too_recent',
          message: 'decisions must be at least 5 minutes old before they can be dismissed',
          details: { availableAt: availAt },
        },
      })
    );
    const tooRecentEvents: string[] = [];
    skewEl.addEventListener('decision-dismiss-too-recent', (e) => {
      tooRecentEvents.push((e as CustomEvent<{ availableAt: string }>).detail.availableAt);
    });
    skewEl
      .shadowRoot!.querySelector<HTMLElement>(
        'sl-dialog[data-dismiss-dialog] [data-dismiss-confirm]'
      )!
      .click();
    await new Promise((r) => setTimeout(r, 15));
    await skewEl.updateComplete;

    expect(skewEl.shadowRoot!.querySelector('sl-dialog[data-dismiss-dialog]')).toBeNull();
    expect(tooRecentEvents).toEqual([availAt]);
    expect(
      skewEl
        .shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')
        ?.hasAttribute('disabled')
    ).toBe(true);
    expect(
      skewEl.shadowRoot!.querySelector('[data-dismiss-tooltip]')?.getAttribute('content')
    ).toBe('Dismiss available in 3 min');
  });

  it('copies permalink from Copy link, reconciles open dialog on live updates, and keeps suspended agents as live (not gone)', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    const origClip = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    try {
      const elChat = await mount([view()]);
      elChat.shadowRoot!.querySelector<HTMLElement>('[data-copy-link-menu-item]')!.click();
      await new Promise((r) => setTimeout(r, 10));
      expect(writeText).toHaveBeenCalledWith(
        `${window.location.origin}${window.location.pathname}#msg-m1`
      );

      const elPurgedPage = await mount([view()], { surface: 'decisions_page', permalinkHref: '' });
      expect(elPurgedPage.shadowRoot!.querySelector('[data-copy-link-menu-item]')).toBeNull();
    } finally {
      if (origClip) Object.defineProperty(navigator, 'clipboard', origClip);
      else delete (navigator as unknown as Record<string, unknown>).clipboard;
    }

    // Suspended agent on decisions_page returns 200 -> dialog tell stays "live", never "gone"
    apiFetch.mockResolvedValueOnce(res(200, { id: 'a1', name: 'release-bot', phase: 'suspended' }));
    const elSusp = await mount([view()], {
      surface: 'decisions_page',
      conversationLabel: '#release',
    });
    elSusp.shadowRoot!.querySelector<HTMLElement>('[data-dismiss-menu-item]')!.click();
    await new Promise((r) => setTimeout(r, 15));
    await elSusp.updateComplete;
    expect(elSusp.shadowRoot!.querySelector('[data-dismiss-tell="gone"]')).toBeNull();
    expect(elSusp.shadowRoot!.querySelector('[data-dismiss-tell="live"]')?.textContent).toContain(
      'release-bot will be told in #release.'
    );

    // Live update while dialog is open closes dialog when all rows become non-actionable
    elSusp.decisions = [dismissedView()];
    await elSusp.updateComplete;
    expect(elSusp.shadowRoot!.querySelector('sl-dialog[data-dismiss-dialog]')).toBeNull();
  });
});
