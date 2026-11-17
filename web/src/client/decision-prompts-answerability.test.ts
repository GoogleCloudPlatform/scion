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
 * "Can I answer?" (ptone/scion#7673, design §4.5): the asker lookup through
 * the agent read, the 403 mapping, and the reason the answer endpoint
 * returns with a 403.
 */

import { describe, it, expect, vi, beforeEach } from 'vitest';

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: RequestInit) => Promise<Response>>());
vi.mock('./api.js', async (orig) => ({
  ...(await orig<typeof import('./api.js')>()),
  apiFetch,
}));

const {
  answerDecision,
  answerabilityFromDenial,
  fetchAskerAnswerability,
  GENERIC_ANSWER_DENIED_REASON,
  READ_ONLY_ANSWER_REASON,
} = await import('./chat-choices.js');

const LINEAGE =
  'release-bot is in lineage mode and only accepts messages from users in its ancestry. You are not in its lineage.';

function res(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

beforeEach(() => {
  apiFetch.mockReset();
});

describe('fetchAskerAnswerability', () => {
  it('reads the asker once through GET /api/v1/agents/{id}', async () => {
    apiFetch.mockResolvedValue(
      res(200, { id: 'a 1', name: 'release-bot', _messageability: { canMessage: true } })
    );
    expect(await fetchAskerAnswerability('a 1', 'release-bot')).toEqual({ state: 'ok' });
    expect(apiFetch).toHaveBeenCalledTimes(1);
    expect(apiFetch.mock.calls[0]![0]).toBe('/api/v1/agents/a%201');
  });

  it('is blocked with the denial copy when the asker will not accept the viewer', async () => {
    apiFetch.mockResolvedValue(
      res(200, {
        id: 'a1',
        name: 'release-bot',
        _messageability: { canMessage: false, reason: 'mode_lineage_no_ancestry' },
      })
    );
    expect(await fetchAskerAnswerability('a1', 'release-bot')).toEqual({
      state: 'blocked',
      askerName: 'release-bot',
      reason: LINEAGE,
    });
  });

  it('is ok when the read carries no messageability', async () => {
    apiFetch.mockResolvedValue(res(200, { id: 'a1', name: 'release-bot' }));
    expect(await fetchAskerAnswerability('a1', 'release-bot')).toEqual({ state: 'ok' });
  });

  it('is gone on 404', async () => {
    apiFetch.mockResolvedValue(res(404, { error: { code: 'not_found' } }));
    expect(await fetchAskerAnswerability('a1', 'release-bot')).toEqual({ state: 'gone' });
  });

  it('is unknown (still answerable) on a network error', async () => {
    apiFetch.mockImplementation(() => Promise.reject(new TypeError('offline')));
    expect(await fetchAskerAnswerability('a1', 'release-bot')).toEqual({ state: 'unknown' });
  });

  it('is unknown on another error status', async () => {
    apiFetch.mockResolvedValue(res(500, null));
    expect(await fetchAskerAnswerability('a1', 'release-bot')).toEqual({ state: 'unknown' });
  });
});

describe('answerabilityFromDenial', () => {
  it('maps message_denied to the asker rule', () => {
    expect(answerabilityFromDenial('message_denied', 'mode_none', 'release-bot')).toEqual({
      state: 'blocked',
      askerName: 'release-bot',
      reason: 'release-bot is sealed (mode: none) and cannot receive messages.',
    });
  });

  it('maps chat_post_denied to a read-only thread', () => {
    expect(answerabilityFromDenial('chat_post_denied', 'read_only', 'release-bot')).toEqual({
      state: 'blocked',
      readOnly: true,
      reason: READ_ONLY_ANSWER_REASON,
    });
  });

  it('maps any other 403 to the generic refusal', () => {
    expect(answerabilityFromDenial('forbidden', undefined, 'release-bot')).toEqual({
      state: 'blocked',
      reason: GENERIC_ANSWER_DENIED_REASON,
    });
  });
});

describe('answerDecision on 403', () => {
  it('returns the code and details.reason', async () => {
    apiFetch.mockResolvedValue(
      res(403, {
        error: {
          code: 'message_denied',
          message: 'Message delivery denied',
          details: { reason: 'mode_lineage_no_ancestry', senderMode: 'user' },
        },
      })
    );
    const out = await answerDecision('r1', 'y', 'k');
    expect(out).toMatchObject({
      ok: false,
      status: 403,
      code: 'message_denied',
      reason: 'mode_lineage_no_ancestry',
    });
  });
});
