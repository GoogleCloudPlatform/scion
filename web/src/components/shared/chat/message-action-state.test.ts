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
 * Unit tests for `MessageActionStateController` and its helpers
 * (ptone/scion#8839, design §4.10.1–§4.10.2).
 */

import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, type ReactiveController, type TemplateResult } from 'lit';
import { StateManager } from '../../../client/state.js';
import {
  MAX_ACTION_DISMISS_REASON_RUNES,
  MessageActionStateController,
  clampActionDismissReason,
  isWholeSetDismissed,
  isWholeSetWithdrawn,
  messageActionRowStatusClass,
  subscribeMessageActionUpdated,
  type MessageActionHost,
  type MessageActionUpdatedPayload,
} from './message-action-state.js';

class MockHost extends EventTarget implements MessageActionHost {
  controllers: ReactiveController[] = [];
  updateCount = 0;

  addController(controller: ReactiveController): void {
    this.controllers.push(controller);
  }

  removeController(controller: ReactiveController): void {
    this.controllers = this.controllers.filter((c) => c !== controller);
  }

  requestUpdate(): void {
    this.updateCount++;
  }

  get updateComplete(): Promise<boolean> {
    return Promise.resolve(true);
  }

  connect(): void {
    for (const c of this.controllers) c.hostConnected?.();
  }

  disconnect(): void {
    for (const c of this.controllers) c.hostDisconnected?.();
  }
}

function renderTemplate(tpl: TemplateResult): HTMLDivElement {
  const container = document.createElement('div');
  document.body.appendChild(container);
  render(tpl, container);
  return container;
}

function text(el: Element | null | undefined): string {
  return el?.textContent?.replace(/\s+/g, ' ').trim() ?? '';
}

function emitSSE(sm: StateManager, subject: string, data: unknown): void {
  const internal = sm as unknown as {
    handleUpdate: (ev: { subject: string; data: unknown }) => void;
  };
  internal.handleUpdate({ subject, data });
}

afterEach(() => {
  document.body.innerHTML = '';
  vi.useRealTimers();
});

describe('MessageActionStateController: terminal-state banners', () => {
  it('renders replaced banner with vN and jump link that dispatches the configured jump event', () => {
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, {
      kind: 'secret_request',
      jumpEventName: 'secret-request-jump',
    });

    const container = renderTemplate(
      ctrl.renderReplacedBanner({
        messageId: 'm-next',
        version: 2,
        at: '2026-11-16T09:12:00Z',
      })
    );
    const foot = container.querySelector('.row-foot')!;
    expect(foot.querySelector('sl-icon')?.getAttribute('name')).toBe('arrow-repeat');
    expect(text(foot)).toMatch(/^Replaced by v2 · \d\d:\d\d · Jump to v2$/);

    const link = foot.querySelector<HTMLAnchorElement>('a.jump')!;
    expect(link.getAttribute('href')).toBe('#msg-m-next');

    const jumps: string[] = [];
    host.addEventListener('secret-request-jump', (e) => {
      jumps.push((e as CustomEvent<{ messageId: string }>).detail.messageId);
    });
    const click = new MouseEvent('click', { bubbles: true, cancelable: true });
    link.dispatchEvent(click);
    expect(click.defaultPrevented).toBe(true);
    expect(jumps).toEqual(['m-next']);
  });

  it('renders replaced banner without version or messageId gracefully', () => {
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, { kind: 'secret_request' });

    const container = renderTemplate(ctrl.renderReplacedBanner(undefined));
    expect(text(container.querySelector('.row-foot'))).toBe('Replaced');
    expect(container.querySelector('a.jump')).toBeNull();
  });

  it('renders withdrawn banner with plain-text reason, jump link, and card-wd class', () => {
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, { kind: 'secret_request' });
    const rawReason = '<img src=x onerror="alert(1)"> **no longer needed**';

    const container = renderTemplate(
      ctrl.renderWithdrawnBanner(
        {
          by: 'deploy-bot',
          at: '2026-11-16T09:15:00Z',
          reason: rawReason,
          replacedBy: { messageId: 'm-v2', version: 2 },
        },
        { inCardFoot: true }
      )
    );

    const wd = container.querySelector('.wd.card-wd')!;
    expect(wd).not.toBeNull();
    expect(wd.querySelector('sl-icon')?.getAttribute('name')).toBe('slash-circle');
    expect(text(wd)).toMatch(
      /^No longer needed · withdrawn by deploy-bot · \d\d:\d\d · Jump to v2$/
    );

    const reasonEl = container.querySelector('.wd-reason')!;
    expect(reasonEl.textContent).toBe(rawReason);
    expect(reasonEl.querySelector('img')).toBeNull();
  });

  it('renders dismissed banner with actor, plain-text reason, and omits .wd-reason when empty', () => {
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, { kind: 'secret_request' });

    const withReason = renderTemplate(
      ctrl.renderDismissedBanner({
        by: 'alice',
        at: '2026-11-16T09:20:00Z',
        reason: 'Use the shared bot token',
      })
    );
    const wd = withReason.querySelector('.wd')!;
    expect(wd.querySelector('sl-icon.dc')?.getAttribute('name')).toBe('x-circle');
    expect(text(wd)).toMatch(/^Dismissed by alice · \d\d:\d\d$/);
    expect(withReason.querySelector('.wd-reason')?.textContent).toBe('Use the shared bot token');

    const withoutReason = renderTemplate(
      ctrl.renderDismissedBanner({
        by: 'you',
      })
    );
    expect(text(withoutReason.querySelector('.wd'))).toBe('Dismissed by you');
    expect(withoutReason.querySelector('.wd-reason')).toBeNull();
  });

  it('renders expired, submitting, and closed banners', () => {
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, { kind: 'secret_request' });

    const expired = renderTemplate(
      ctrl.renderExpiredBanner({
        at: '2026-11-23T09:20:00Z',
        replacedBy: { messageId: 'm-new', version: 3 },
      })
    );
    expect(expired.querySelector('.wd sl-icon')?.getAttribute('name')).toBe('clock');
    expect(text(expired.querySelector('.wd'))).toMatch(/^Expired · \d\d:\d\d · Jump to v3$/);

    const submittingDefault = renderTemplate(ctrl.renderSubmittingBanner());
    expect(text(submittingDefault.querySelector('.row-foot'))).toBe('Sending…');

    const submittingCustom = renderTemplate(ctrl.renderSubmittingBanner('Submitting…'));
    expect(text(submittingCustom.querySelector('.row-foot'))).toBe('Submitting…');

    const closed = renderTemplate(ctrl.renderClosedBanner());
    expect(text(closed.querySelector('.row-foot'))).toBe('Closed');
  });
});

describe('MessageActionStateController: live chat.<kind>.updated subscription', () => {
  it('routes user and project chat.<kind>.updated through StateManager without emitting chat-message-received', () => {
    const sm = new StateManager();
    const onSecretUpdate = vi.fn();
    const onMessage = vi.fn();
    sm.addEventListener('chat-message-received', onMessage);

    const host = new MockHost();
    let activeMsgId = 'm-sec-1';
    new MessageActionStateController<MessageActionUpdatedPayload>(host, {
      kind: 'secret_request',
      eventSource: sm,
      getMessageId: (): string => activeMsgId,
      onUpdated: onSecretUpdate,
    });
    host.connect();

    emitSSE(sm, 'user.u1.chat.secret_request.updated', {
      messageId: 'm-sec-1',
      threadId: 'dm:agent:a1:user:u1',
      secretRequest: { rowId: 'sr-1', status: 'fulfilled' },
    });
    expect(onMessage).not.toHaveBeenCalled();
    expect(onSecretUpdate).toHaveBeenCalledTimes(1);
    expect(onSecretUpdate.mock.calls[0]?.[0]).toMatchObject({
      kind: 'secret_request',
      messageId: 'm-sec-1',
    });

    // Different messageId is ignored.
    emitSSE(sm, 'project.p1.chat.secret_request.updated', {
      messageId: 'm-sec-other',
      threadId: 'topic-1',
    });
    expect(onSecretUpdate).toHaveBeenCalledTimes(1);

    // Different kind is ignored.
    emitSSE(sm, 'project.p1.chat.exec_approval.updated', {
      messageId: 'm-sec-1',
      threadId: 'topic-1',
    });
    expect(onSecretUpdate).toHaveBeenCalledTimes(1);

    // Project-scoped matching event includes projectId.
    activeMsgId = 'm-sec-2';
    emitSSE(sm, 'project.p1.chat.secret_request.updated', {
      messageId: 'm-sec-2',
      threadId: 'topic-1',
    });
    expect(onSecretUpdate).toHaveBeenCalledTimes(2);
    expect(onSecretUpdate.mock.calls[1]?.[0]).toMatchObject({
      kind: 'secret_request',
      projectId: 'p1',
      messageId: 'm-sec-2',
    });

    // Disconnecting unsubscribes.
    host.disconnect();
    emitSSE(sm, 'project.p1.chat.secret_request.updated', {
      messageId: 'm-sec-2',
      threadId: 'topic-1',
    });
    expect(onSecretUpdate).toHaveBeenCalledTimes(2);
  });

  it('subscribes to decision-updated for kind="decision" and filters by threadId', () => {
    const sm = new StateManager();
    const received: MessageActionUpdatedPayload[] = [];
    const unsubscribe = subscribeMessageActionUpdated('decision', (data) => received.push(data), {
      eventSource: sm,
      threadId: () => 'topic-1',
    });

    emitSSE(sm, 'project.p1.chat.decision.updated', {
      messageId: 'm1',
      threadId: 'topic-other',
      decision: { rowId: 'r1' },
    });
    expect(received).toHaveLength(0);

    emitSSE(sm, 'project.p1.chat.decision.updated', {
      messageId: 'm1',
      threadId: 'topic-1',
      decision: { rowId: 'r1' },
    });
    expect(received).toHaveLength(1);
    expect(received[0]?.messageId).toBe('m1');

    unsubscribe();
    emitSSE(sm, 'project.p1.chat.decision.updated', {
      messageId: 'm1',
      threadId: 'topic-1',
      decision: { rowId: 'r2' },
    });
    expect(received).toHaveLength(1);
  });
});

describe('MessageActionStateController: dismiss-with-reason affordance', () => {
  it('allows immediate dismiss when dismissMinAgeMs is 0 (e.g. secret requests)', () => {
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, {
      kind: 'secret_request',
      dismissMinAgeMs: 0,
      getCreatedAt: () => new Date().toISOString(),
      getDismissableRowIds: () => ['sr-1', 'sr-2'],
    });
    host.connect();

    expect(ctrl.dismissRemainingMin).toBe(0);
    expect(ctrl.openDismissDialog()).toBe(true);
    expect(ctrl.dismissDialogOpen).toBe(true);
    expect([...ctrl.dismissCheckedIds]).toEqual(['sr-1', 'sr-2']);
  });

  it('enforces minimum age countdown, unlocks at boundary, and cleans up timer on disconnect', () => {
    vi.useFakeTimers();
    const startMs = Date.parse('2026-11-16T10:00:00Z');
    let now = startMs + 4 * 60_000; // 4m elapsed -> 1m remaining
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, {
      kind: 'decision',
      dismissMinAgeMs: 5 * 60_000,
      getCreatedAt: () => new Date(startMs).toISOString(),
      clockNow: () => now,
      hasDismissableRows: () => true,
      getDismissableRowIds: () => ['r1'],
    });
    host.connect();

    expect(ctrl.dismissRemainingMin).toBe(1);
    expect(ctrl.openDismissDialog()).toBe(false);
    expect(ctrl.dismissDialogOpen).toBe(false);

    const updatesBefore = host.updateCount;
    now = startMs + 5 * 60_000 + 25;
    vi.advanceTimersByTime(60_025);
    expect(host.updateCount).toBeGreaterThan(updatesBefore);
    expect(ctrl.dismissRemainingMin).toBe(0);
    expect(ctrl.openDismissDialog()).toBe(true);

    // Server 409 too_recent override re-arms countdown.
    ctrl.setDismissAvailableAtOverride(now + 2 * 60_000 + 10_000);
    expect(ctrl.dismissRemainingMin).toBe(3);

    // Disconnect clears timer cleanly without firing further updates.
    const updatesAtDisconnect = host.updateCount;
    host.disconnect();
    vi.advanceTimersByTime(5 * 60_000);
    expect(host.updateCount).toBe(updatesAtDisconnect);
  });

  it('toggles rows, clamps reason to 500 runes, blocks close while submitting, and reconciles open dialog', () => {
    const host = new MockHost();
    let dismissable = ['r1', 'r2'];
    const ctrl = new MessageActionStateController(host, {
      kind: 'secret_request',
      getDismissableRowIds: () => dismissable,
    });
    host.connect();

    expect(ctrl.openDismissDialog()).toBe(true);
    ctrl.toggleDismissRow('r2', false);
    expect([...ctrl.dismissCheckedIds]).toEqual(['r1']);

    // Multi-code-unit emoji clamping to 500 runes.
    const fakeInput = { value: '🔑'.repeat(505) };
    ctrl.onDismissReasonInput({ target: fakeInput } as unknown as Event);
    expect([...ctrl.dismissReasonDraft]).toHaveLength(MAX_ACTION_DISMISS_REASON_RUNES);
    expect([...fakeInput.value]).toHaveLength(MAX_ACTION_DISMISS_REASON_RUNES);

    // Submitting blocks closeDismissDialog and reconciliation.
    ctrl.setDismissSubmitting(true);
    ctrl.closeDismissDialog();
    expect(ctrl.dismissDialogOpen).toBe(true);

    dismissable = [];
    ctrl.reconcileOpenDismissDialog();
    expect(ctrl.dismissDialogOpen).toBe(true);

    // Once not submitting, reconciliation drops invalid ids and closes when empty.
    ctrl.setDismissSubmitting(false);
    dismissable = ['r2'];
    ctrl.toggleDismissRow('r2', true);
    ctrl.reconcileOpenDismissDialog();
    expect([...ctrl.dismissCheckedIds]).toEqual(['r2']);

    dismissable = [];
    ctrl.reconcileOpenDismissDialog();
    expect(ctrl.dismissDialogOpen).toBe(false);
    expect(ctrl.dismissReasonDraft).toBe('');
  });

  it('cancels stale probeAskerGone results on close or disconnect', async () => {
    const host = new MockHost();
    const ctrl = new MessageActionStateController(host, {
      kind: 'decision',
      getDismissableRowIds: () => ['r1'],
    });
    host.connect();
    ctrl.openDismissDialog();

    let resolveProbe!: (v: boolean) => void;
    ctrl.probeAskerGone(
      () =>
        new Promise<boolean>((r) => {
          resolveProbe = r;
        })
    );
    ctrl.closeDismissDialog();
    resolveProbe(true);
    await Promise.resolve();
    expect(ctrl.dismissProbedGone).toBe(false);

    // Fresh open + live probe sets dismissProbedGone.
    ctrl.openDismissDialog();
    ctrl.probeAskerGone(() => Promise.resolve(true));
    await Promise.resolve();
    expect(ctrl.dismissProbedGone).toBe(true);
  });
});

describe('message-action-state pure helpers', () => {
  it('clamps dismiss reasons by Unicode rune count', () => {
    expect(clampActionDismissReason('short')).toBe('short');
    expect([...clampActionDismissReason('a'.repeat(600))]).toHaveLength(500);
  });

  it('maps row statuses to CSS modifier classes', () => {
    expect(messageActionRowStatusClass('replaced')).toBe(' replaced');
    expect(messageActionRowStatusClass('withdrawn')).toBe(' withdrawn');
    expect(messageActionRowStatusClass('dismissed')).toBe(' dismissed');
    expect(messageActionRowStatusClass('expired')).toBe(' expired');
    expect(messageActionRowStatusClass('pending')).toBe('');
    expect(messageActionRowStatusClass('unknown_closed', true)).toBe(' closed');
  });

  it('detects whole-set withdrawn and whole-set dismissed cards', () => {
    const w1 = { status: 'withdrawn', withdrawn: { at: 't1', reason: 'done' }, by: 'bot' };
    const w2 = { status: 'withdrawn', withdrawn: { at: 't1', reason: 'done' }, by: 'bot' };
    const w3 = { status: 'withdrawn', withdrawn: { at: 't2', reason: 'done' }, by: 'bot' };
    expect(isWholeSetWithdrawn([w1, w2], (r) => r.by)).toBe(true);
    expect(isWholeSetWithdrawn([w1, w3], (r) => r.by)).toBe(false);

    const d1 = {
      status: 'dismissed',
      dismissed: { responseMessageId: 'rm1', at: 't1', reason: 'stale' },
      by: 'alice',
    };
    const d2 = {
      status: 'dismissed',
      dismissed: { responseMessageId: 'rm1', at: 't1', reason: 'stale' },
      by: 'alice',
    };
    const d3 = {
      status: 'dismissed',
      dismissed: { responseMessageId: 'rm2', at: 't1', reason: 'stale' },
      by: 'alice',
    };
    expect(isWholeSetDismissed([d1, d2], (r) => r.by)).toBe(true);
    expect(isWholeSetDismissed([d1, d3], (r) => r.by)).toBe(false);
  });
});
