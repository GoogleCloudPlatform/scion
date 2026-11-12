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

import { describe, it, expect, vi, beforeEach } from 'vitest';
import type { DecisionView, Message } from '../shared/types.js';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
vi.mock('./api.js', async (orig) => ({
  ...(await orig<typeof import('./api.js')>()),
  apiFetch,
}));

const {
  answerDecision,
  applyChoiceHistoryMaps,
  carryChoiceFields,
  newerDecisionView,
  patchDecisions,
} = await import('./chat-choices.js');

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
    type: 'choice-prompt',
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

  it('lets a terminal view replace another terminal view', () => {
    const replaced = view({ status: 'replaced' });
    expect(newerDecisionView(decided(), replaced)).toBe(replaced);
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

describe('applyChoiceHistoryMaps', () => {
  it('copies the five side maps onto their messages', () => {
    const prompt = msg();
    const resp = msg({ id: 'm2', type: 'choice-response' });
    const respSet = msg({ id: 'm3', type: 'choice-response' });
    applyChoiceHistoryMaps([prompt, resp, respSet], {
      messageDecisions: { m1: [view()] },
      messageChoiceIntros: { m1: 'Intro' },
      messageChoiceAnswerModes: { m1: 'single' },
      messageChoiceResponses: {
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
      messageChoiceResponseSets: { m3: { promptMessageId: 'm1', batchId: 'b', answers: [] } },
    });
    expect(prompt.decisions?.[0]?.rowId).toBe('r1');
    expect(prompt.choiceIntro).toBe('Intro');
    expect(prompt.answerMode).toBe('single');
    expect(resp.choiceResponse?.label).toBe('Yes');
    expect(respSet.choiceResponseSet?.batchId).toBe('b');
  });

  it('leaves messages untouched without maps (experiment off)', () => {
    const m = msg();
    applyChoiceHistoryMaps([m], {});
    expect(m.decisions).toBeUndefined();
    expect(m.choiceIntro).toBeUndefined();
  });
});

describe('carryChoiceFields', () => {
  it('keeps an answered row when an older copy of the prompt arrives', () => {
    const existing = msg({ decisions: [decided()] });
    const incoming = msg({ decisions: [view()] });
    carryChoiceFields(existing, incoming);
    expect(incoming.decisions?.[0]?.status).toBe('decided');
  });

  it('takes newer rows', () => {
    const existing = msg({ decisions: [view()] });
    const incoming = msg({ decisions: [decided()] });
    carryChoiceFields(existing, incoming);
    expect(incoming.decisions?.[0]?.status).toBe('decided');
  });

  it('keeps the held choice fields when the incoming copy has none', () => {
    const existing = msg({ decisions: [view()], choiceIntro: 'Intro', answerMode: 'single' });
    const incoming = msg();
    carryChoiceFields(existing, incoming);
    expect(incoming.decisions).toBe(existing.decisions);
    expect(incoming.choiceIntro).toBe('Intro');
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
