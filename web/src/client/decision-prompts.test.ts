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

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import type { AnswerView, DecisionView, Message } from '../shared/types.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
vi.mock('./api.js', async (orig) => ({
  ...(await orig<typeof import('./api.js')>()),
  apiFetch,
}));

const {
  DISMISS_MIN_AGE_MS,
  actionableReason,
  answerDecision,
  answerDecisionSet,
  applyDecisionHistoryMaps,
  carryDecisionFields,
  dismissAvailableAtMs,
  dismissDecisionPrompt,
  dismissRemainingMinutes,
  isClosedUnanswered,
  isOpenDecision,
  isTerminalDecision,
  newerDecisionView,
  patchDecisions,
} = await import('./decision-prompts.js');

function view(over: Partial<DecisionView> = {}): DecisionView {
  return {
    rowId: 'r1',
    messageId: 'm1',
    key: 'D1',
    ref: 'D1',
    version: 1,
    status: 'pending',
    askerAgentId: 'a1',
    askerAgentName: 'bot',
    question: 'Ship?',
    choices: [
      { id: 'y', label: 'Yes' },
      { id: 'n', label: 'No' },
    ],
    ...over,
  };
}

const decided = (over: Partial<DecisionView> = {}): DecisionView =>
  view({
    status: 'decided',
    decision: {
      choiceId: 'y',
      label: 'Yes',
      provenance: 'direct',
      surface: 'web',
      responseMessageId: 'resp',
      at: '2026-11-12T10:00:00Z',
    },
    ...over,
  });

function msg(over: Partial<Message> = {}): Message {
  return {
    id: 'm1',
    projectId: 'p',
    sender: 'agent:bot',
    senderId: 'a1',
    recipient: 'user:u',
    recipientId: 'u',
    msg: 'fallback',
    type: 'decision-prompt',
    agentId: 'a1',
    createdAt: '2026-11-12T09:00:00Z',
    ...over,
  };
}

describe('newerDecisionView', () => {
  it('takes the incoming view', () => {
    const d = decided();
    expect(newerDecisionView(view(), d)).toBe(d);
  });

  it('keeps a terminal view over a pending one (out of order)', () => {
    const held = decided();
    expect(newerDecisionView(held, view())).toBe(held);
    const replaced = view({ status: 'replaced' });
    expect(newerDecisionView(replaced, view())).toBe(replaced);
  });

  it('moves decided to replaced, and keeps replaced over a later decided', () => {
    const replaced = view({ status: 'replaced' });
    expect(newerDecisionView(decided(), replaced)).toBe(replaced);
    // An own POST answered just before the --update lands after the
    // replaced event: replaced is final.
    expect(newerDecisionView(replaced, decided())).toBe(replaced);
  });

  it('takes a newer decided view over a decided one', () => {
    const later = decided({ decision: { ...decided().decision!, choiceId: 'n', label: 'No' } });
    expect(newerDecisionView(decided(), later)).toBe(later);
  });

  it('upgrades agent_interpreted to direct and refuses a late agent_interpreted downgrade (even with identical at)', () => {
    const sameAt = '2026-11-12T10:00:00Z';
    const interpreted = decided({
      decision: {
        choiceId: 'y',
        label: 'Yes',
        provenance: 'agent_interpreted',
        outcome: 'answer',
        answeredByUserId: 'u1',
        surface: 'native',
        responseMessageId: 'resp-interp',
        at: sameAt,
      },
    });
    const confirmed = decided({
      decision: {
        choiceId: 'y',
        label: 'Yes',
        provenance: 'direct',
        outcome: 'confirm',
        answeredByUserId: 'u1',
        surface: 'web',
        responseMessageId: 'resp-conf',
        at: sameAt,
      },
    });
    const overridden = decided({
      decision: {
        choiceId: 'n',
        label: 'No',
        provenance: 'direct',
        outcome: 'override',
        previousChoiceLabel: 'Yes',
        answeredByUserId: 'u1',
        surface: 'web',
        responseMessageId: 'resp-over',
        at: sameAt,
      },
    });
    expect(newerDecisionView(interpreted, confirmed)).toBe(confirmed);
    expect(newerDecisionView(interpreted, overridden)).toBe(overridden);
    expect(newerDecisionView(confirmed, interpreted)).toBe(confirmed);
    expect(newerDecisionView(overridden, interpreted)).toBe(overridden);
    expect(patchDecisions([overridden], [interpreted])).toEqual([overridden]);
  });
});

describe('actionableReason (golden parity with pkg/store/testdata/decision_actionable_cases.json)', () => {
  interface GoldenCase {
    name: string;
    userId: string;
    status: DecisionView['status'];
    decision: {
      provenance: string;
      outcome: string;
      answeredByUserId: string;
    } | null;
    want: string;
  }

  const casesPath = join(__dirname, '../../../pkg/store/testdata/decision_actionable_cases.json');
  const cases = JSON.parse(readFileSync(casesPath, 'utf8')) as GoldenCase[];

  for (const tc of cases) {
    it(`matches Go store on: ${tc.name}`, () => {
      const decision: AnswerView | undefined = tc.decision
        ? {
            choiceId: 'y',
            label: 'Yes',
            provenance: tc.decision.provenance,
            outcome: tc.decision.outcome,
            ...(tc.decision.answeredByUserId
              ? { answeredByUserId: tc.decision.answeredByUserId }
              : {}),
            surface: 'web',
            responseMessageId: 'resp',
            at: '2026-11-12T10:00:00Z',
          }
        : undefined;
      const d = view({
        status: tc.status,
        ...(decision ? { decision } : {}),
      });
      expect(actionableReason(d, tc.userId)).toBe(tc.want);
    });
  }

  it('treats undefined userId identically to empty userId', () => {
    expect(actionableReason(view(), undefined)).toBe('');
    const ownInterp = decided({
      decision: {
        ...decided().decision!,
        provenance: 'agent_interpreted',
        answeredByUserId: 'u1',
      },
    });
    expect(actionableReason(ownInterp, undefined)).toBe('not_recorded_answerer');
  });

  it('fails closed (not_recorded_answerer) when both userId and answeredByUserId are empty', () => {
    const deletedOwnerInterp = decided({
      decision: {
        ...decided().decision!,
        provenance: 'agent_interpreted',
        answeredByUserId: '',
      },
    });
    expect(actionableReason(deletedOwnerInterp, '')).toBe('not_recorded_answerer');
    expect(actionableReason(deletedOwnerInterp, undefined)).toBe('not_recorded_answerer');
  });
});

describe('patchDecisions', () => {
  it('replaces the named row and returns a new array', () => {
    const list = [view(), view({ rowId: 'r2' })];
    const d = decided();
    const next = patchDecisions(list, [d]);
    expect(next).not.toBe(list);
    expect(next[0]).toBe(d);
    expect(next[1]).toBe(list[1]);
  });

  it('returns the same array when nothing changes', () => {
    const list = [decided()];
    expect(patchDecisions(list, [view()])).toBe(list);
    expect(patchDecisions(list, [view({ rowId: 'other' })])).toBe(list);
  });
});

describe('applyDecisionHistoryMaps', () => {
  it('copies the five side maps onto their messages', () => {
    const prompt = msg();
    const resp = msg({ id: 'm2', type: 'decision-response' });
    const respSet = msg({ id: 'm3', type: 'decision-response' });
    applyDecisionHistoryMaps([prompt, resp, respSet], {
      messageDecisions: { m1: [view()] },
      messageDecisionIntros: { m1: 'Intro' },
      messageDecisionAnswerModes: { m1: 'single' },
      messageDecisionResponses: {
        m2: {
          rowId: 'r1',
          ref: 'D1',
          key: 'D1',
          version: 1,
          question: 'Ship?',
          choiceId: 'y',
          label: 'Yes',
          provenance: 'direct',
          promptMessageId: 'm1',
        },
      },
      messageDecisionResponseSets: { m3: { promptMessageId: 'm1', batchId: 'b', answers: [] } },
    });
    expect(prompt.decisions?.[0]?.rowId).toBe('r1');
    expect(prompt.decisionIntro).toBe('Intro');
    expect(prompt.answerMode).toBe('single');
    expect(resp.decisionResponse?.label).toBe('Yes');
    expect(respSet.decisionResponseSet?.batchId).toBe('b');
  });

  it('leaves messages untouched without maps (experiment off)', () => {
    const m = msg();
    applyDecisionHistoryMaps([m], {});
    expect(m.decisions).toBeUndefined();
    expect(m.decisionIntro).toBeUndefined();
  });
});

describe('carryDecisionFields', () => {
  it('keeps an answered row when an older copy of the prompt arrives', () => {
    const existing = msg({ decisions: [decided()] });
    const incoming = msg({ decisions: [view()] });
    carryDecisionFields(existing, incoming);
    expect(incoming.decisions?.[0]?.status).toBe('decided');
  });

  it('takes newer rows', () => {
    const existing = msg({ decisions: [view()] });
    const incoming = msg({ decisions: [decided()] });
    carryDecisionFields(existing, incoming);
    expect(incoming.decisions?.[0]?.status).toBe('decided');
  });

  it('keeps the held choice fields when the incoming copy has none', () => {
    const existing = msg({ decisions: [view()], decisionIntro: 'Intro', answerMode: 'single' });
    const incoming = msg();
    carryDecisionFields(existing, incoming);
    expect(incoming.decisions).toBe(existing.decisions);
    expect(incoming.decisionIntro).toBe('Intro');
    expect(incoming.answerMode).toBe('single');
  });
});

describe('answerDecision', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  const res = (status: number, body: unknown): Response =>
    ({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(body),
    }) as unknown as Response;

  it('posts the choice id and idempotency key', async () => {
    apiFetch.mockResolvedValue(res(200, { decision: decided(), responseMessageId: 'resp' }));
    const out = await answerDecision('r 1', 'y', 'key-1');
    expect(out).toEqual({ ok: true, decision: decided(), responseMessageId: 'resp' });
    const [path, init] = apiFetch.mock.calls[0]!;
    expect(path).toBe('/api/v1/decisions/r%201/answer');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(init?.body as string)).toEqual({ choice_id: 'y', idempotency_key: 'key-1' });
  });

  it('includes surface when surface is decisions_page and omits it on native', async () => {
    apiFetch.mockResolvedValue(res(200, { decision: decided(), responseMessageId: 'resp' }));
    await answerDecision('r1', 'y', 'key-page', 'decisions_page');
    expect(JSON.parse(apiFetch.mock.calls[0]![1]?.body as string)).toEqual({
      choice_id: 'y',
      idempotency_key: 'key-page',
      surface: 'decisions_page',
    });

    await answerDecision('r1', 'y', 'key-native', 'native');
    expect(JSON.parse(apiFetch.mock.calls[1]![1]?.body as string)).toEqual({
      choice_id: 'y',
      idempotency_key: 'key-native',
    });
  });

  it('returns the current view and code on 409', async () => {
    apiFetch.mockResolvedValue(
      res(409, {
        error: {
          code: 'already_answered',
          message: 'decision D1 v1 is already answered',
          details: { decision: decided() },
        },
      })
    );
    const out = await answerDecision('r1', 'n', 'k');
    expect(out.ok).toBe(false);
    if (out.ok) return;
    expect(out.status).toBe(409);
    expect(out.code).toBe('already_answered');
    expect(out.decision?.status).toBe('decided');
    expect(out.message).toBe('decision D1 v1 is already answered');
  });

  it('carries the newer version on 409 replaced', async () => {
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
    const out = await answerDecision('r1', 'y', 'k');
    expect(!out.ok && out.replacedBy?.version).toBe(2);
  });

  it('resolves a network failure as status 0', async () => {
    apiFetch.mockImplementation(() => Promise.reject(new TypeError('offline')));
    const out = await answerDecision('r1', 'y', 'k');
    expect(out).toMatchObject({ ok: false, status: 0 });
  });
});

describe('answerDecisionSet', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  const res = (status: number, body: unknown): Response =>
    ({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(body),
    }) as unknown as Response;

  it('posts the answers and reads the camelCase 200 (B0)', async () => {
    apiFetch.mockResolvedValue(
      res(200, { responseMessageId: 'resp', batchId: 'b1', decisions: [decided()] })
    );
    const out = await answerDecisionSet('m 1', [{ rowId: 'r1', choiceId: 'y' }], 'key-1');
    expect(out).toEqual({
      ok: true,
      decisions: [decided()],
      responseMessageId: 'resp',
      batchId: 'b1',
    });
    const [path, init] = apiFetch.mock.calls[0]!;
    expect(path).toBe('/api/v1/decision-prompts/m%201/answers');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(init?.body as string)).toEqual({
      idempotency_key: 'key-1',
      answers: [{ decision_row_id: 'r1', choice_id: 'y' }],
    });
  });

  it('includes surface when surface is decisions_page and omits it on native', async () => {
    apiFetch.mockResolvedValue(
      res(200, { responseMessageId: 'resp', batchId: 'b1', decisions: [decided()] })
    );
    await answerDecisionSet('m1', [{ rowId: 'r1', choiceId: 'y' }], 'key-page', 'decisions_page');
    expect(JSON.parse(apiFetch.mock.calls[0]![1]?.body as string)).toEqual({
      idempotency_key: 'key-page',
      answers: [{ decision_row_id: 'r1', choice_id: 'y' }],
      surface: 'decisions_page',
    });

    await answerDecisionSet('m1', [{ rowId: 'r1', choiceId: 'y' }], 'key-native', 'native');
    expect(JSON.parse(apiFetch.mock.calls[1]![1]?.body as string)).toEqual({
      idempotency_key: 'key-native',
      answers: [{ decision_row_id: 'r1', choice_id: 'y' }],
    });
  });

  it('returns the fresh views and reasons on 409 answer_set_changed', async () => {
    apiFetch.mockResolvedValue(
      res(409, {
        error: {
          code: 'answer_set_changed',
          message: 'the decisions to answer changed; refresh and submit again',
          details: { decisions: [view({ status: 'replaced' })], reasons: { r1: 'replaced', x: 3 } },
        },
      })
    );
    const out = await answerDecisionSet('m1', [{ rowId: 'r1', choiceId: 'y' }], 'k');
    expect(out.ok).toBe(false);
    if (out.ok) return;
    expect(out.code).toBe('answer_set_changed');
    expect(out.decisions?.[0]?.status).toBe('replaced');
    expect(out.reasons).toEqual({ r1: 'replaced' });
  });

  it('treats a 200 without decisions as an error', async () => {
    apiFetch.mockResolvedValue(res(200, { responseMessageId: 'resp' }));
    const out = await answerDecisionSet('m1', [{ rowId: 'r1', choiceId: 'y' }], 'k');
    expect(out.ok).toBe(false);
  });

  it('resolves a network failure as status 0', async () => {
    apiFetch.mockImplementation(() => Promise.reject(new TypeError('offline')));
    const out = await answerDecisionSet('m1', [{ rowId: 'r1', choiceId: 'y' }], 'k');
    expect(out).toMatchObject({ ok: false, status: 0 });
  });
});

describe('isOpenDecision, isClosedUnanswered, and isTerminalDecision', () => {
  it('classifies pending, interpreted, direct, replaced, withdrawn, dismissed, and unknown statuses', () => {
    const pendingView = view();
    const interpView = decided({
      decision: {
        ...decided().decision!,
        provenance: 'agent_interpreted',
        answeredByUserId: 'u1',
      },
    });
    const directView = decided();
    const replacedView = view({ status: 'replaced' });
    const withdrawnView = view({ status: 'withdrawn' });
    const dismissedView = view({
      status: 'dismissed',
      dismissed: {
        at: '2026-11-16T09:12:00Z',
        byUserId: 'u1',
        byName: 'alice',
        reason: 'Stale',
        surface: 'decisions_page',
        responseMessageId: 'resp-dsm',
      },
    });
    const unknownFutureView = view({ status: 'expired_v99' });

    expect(isOpenDecision(pendingView)).toBe(true);
    expect(isOpenDecision(interpView)).toBe(true);
    expect(isOpenDecision(directView)).toBe(false);
    expect(isOpenDecision(replacedView)).toBe(false);
    expect(isOpenDecision(withdrawnView)).toBe(false);
    expect(isOpenDecision(dismissedView)).toBe(false);
    expect(isOpenDecision(unknownFutureView)).toBe(false);

    expect(isClosedUnanswered(pendingView)).toBe(false);
    expect(isClosedUnanswered(interpView)).toBe(false);
    expect(isClosedUnanswered(directView)).toBe(false);
    expect(isClosedUnanswered(replacedView)).toBe(true);
    expect(isClosedUnanswered(withdrawnView)).toBe(true);
    expect(isClosedUnanswered(dismissedView)).toBe(true);
    expect(isClosedUnanswered(unknownFutureView)).toBe(true);

    expect(isTerminalDecision(pendingView)).toBe(false);
    expect(isTerminalDecision(directView)).toBe(true);
    expect(isTerminalDecision(replacedView)).toBe(true);
    expect(isTerminalDecision(withdrawnView)).toBe(true);
    expect(isTerminalDecision(dismissedView)).toBe(true);

    // Dismissed is terminal (rank 3) and never reverts to pending or decided.
    expect(newerDecisionView(pendingView, dismissedView)).toBe(dismissedView);
    expect(newerDecisionView(directView, dismissedView)).toBe(dismissedView);
    expect(newerDecisionView(dismissedView, pendingView)).toBe(dismissedView);
    expect(newerDecisionView(dismissedView, directView)).toBe(dismissedView);
  });

  it('keeps actionableReason as withdrawn even when replacedBy links to a re-asked v2 (D7)', () => {
    const withdrawnReasked = view({
      status: 'withdrawn',
      withdrawn: { at: '2026-11-16T09:12:00Z', byName: 'bot' },
      replacedBy: { rowId: 'r2', messageId: 'm2', version: 2, at: '2026-11-16T09:15:00Z' },
    });
    expect(actionableReason(withdrawnReasked, 'u1')).toBe('withdrawn');
  });

  it('keeps actionableReason as dismissed even when replacedBy links to a re-asked v2', () => {
    const dismissedReasked = view({
      status: 'dismissed',
      dismissed: {
        at: '2026-11-16T09:12:00Z',
        byUserId: 'u1',
        byName: 'alice',
        surface: 'decisions_page',
        responseMessageId: 'resp-dsm',
      },
      replacedBy: { rowId: 'r2', messageId: 'm2', version: 2, at: '2026-11-16T09:15:00Z' },
    });
    expect(actionableReason(dismissedReasked, 'u1')).toBe('dismissed');
  });
});

describe('dismissRemainingMinutes and dismissDecisionPrompt', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  const res = (status: number, body: unknown): Response =>
    ({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(body),
    }) as unknown as Response;

  it('computes 5-minute dismissal threshold and rounds remaining minutes up', () => {
    const created = '2026-11-16T09:00:00Z';
    const createdMs = Date.parse(created);
    expect(dismissAvailableAtMs(created)).toBe(createdMs + DISMISS_MIN_AGE_MS);
    expect(dismissAvailableAtMs('not-a-date')).toBe(0);

    expect(dismissRemainingMinutes(created, createdMs)).toBe(5);
    expect(dismissRemainingMinutes(created, createdMs + 4 * 60_000 + 59_000)).toBe(1);
    expect(dismissRemainingMinutes(created, createdMs + 5 * 60_000)).toBe(0);
    expect(dismissRemainingMinutes('not-a-date', createdMs)).toBe(0);
  });

  it('posts trimmed reason and surface to /dismiss and reads 200 response', async () => {
    const d1 = view({
      status: 'dismissed',
      dismissed: {
        at: '2026-11-16T09:12:00Z',
        byUserId: 'u1',
        byName: 'alice',
        reason: 'Stale release',
        surface: 'decisions_page',
        responseMessageId: 'resp-d1',
      },
    });
    apiFetch.mockResolvedValue(
      res(200, {
        dismissed: [d1],
        alreadyDismissed: [],
        responseMessageId: 'resp-d1',
        delivered: true,
        deliveryStatus: 'delivered',
      })
    );

    const out = await dismissDecisionPrompt('m 1', {
      reason: '  Stale release  ',
      surface: 'decisions_page',
    });
    expect(out).toEqual({
      ok: true,
      dismissed: [d1],
      alreadyDismissed: [],
      responseMessageId: 'resp-d1',
      delivered: true,
      deliveryStatus: 'delivered',
    });
    const [path, init] = apiFetch.mock.calls[0]!;
    expect(path).toBe('/api/v1/decision-prompts/m%201/dismiss');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(init?.body as string)).toEqual({
      reason: 'Stale release',
      surface: 'decisions_page',
    });
  });

  it('parses 409 too_recent and 409 dismiss_changed error details', async () => {
    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'too_recent',
          message: 'decisions may be dismissed 5 minutes after they are asked',
          details: { availableAt: '2026-11-16T09:05:00Z' },
        },
      })
    );
    const early = await dismissDecisionPrompt('m1');
    expect(early).toMatchObject({
      ok: false,
      status: 409,
      code: 'too_recent',
      availableAt: '2026-11-16T09:05:00Z',
    });

    apiFetch.mockResolvedValueOnce(
      res(409, {
        error: {
          code: 'dismiss_changed',
          message: 'the decisions changed; refresh and try again',
          details: {
            decisions: [decided()],
            reasons: { r1: 'decided' },
          },
        },
      })
    );
    const changed = await dismissDecisionPrompt('m1');
    expect(changed).toMatchObject({
      ok: false,
      status: 409,
      code: 'dismiss_changed',
      reasons: { r1: 'decided' },
    });
  });

  it('reads deliveryStatus, and claims no delivery for a missing or unknown value', async () => {
    const d1 = view({ status: 'dismissed' });
    apiFetch.mockResolvedValueOnce(
      res(200, {
        dismissed: [d1],
        alreadyDismissed: [],
        responseMessageId: 'resp',
        delivered: false,
        deliveryStatus: 'unreachable',
      })
    );
    expect(await dismissDecisionPrompt('m1')).toMatchObject({ deliveryStatus: 'unreachable' });

    apiFetch.mockResolvedValueOnce(
      res(200, {
        dismissed: [d1],
        alreadyDismissed: [],
        delivered: true,
        deliveryStatus: 'teleported',
      })
    );
    expect(await dismissDecisionPrompt('m1')).toMatchObject({ deliveryStatus: 'failed' });

    apiFetch.mockResolvedValueOnce(res(200, { dismissed: [], alreadyDismissed: [d1] }));
    expect(await dismissDecisionPrompt('m1')).toMatchObject({ deliveryStatus: 'none' });
  });

  it('resolves a network failure as status 0', async () => {
    apiFetch.mockImplementation(() => Promise.reject(new TypeError('offline')));
    const out = await dismissDecisionPrompt('m1');
    expect(out).toMatchObject({ ok: false, status: 0 });
  });
});
