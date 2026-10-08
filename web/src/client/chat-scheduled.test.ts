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

const apiFetch = vi.fn();
vi.mock('./api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args),
  extractApiError: async (res: Response, fallback: string) => {
    try {
      const body = (await res.json()) as { error?: { message?: string } };
      return body.error?.message ?? fallback;
    } catch {
      return fallback;
    }
  },
}));

import {
  applyScheduledUpdate,
  cancelScheduledMessage,
  conversationSupportsScheduledSend,
  createScheduledMessage,
  listScheduledMessages,
  scheduledFailureText,
  sortScheduled,
  type ScheduledMessage,
} from './chat-scheduled.js';

function sm(
  id: string,
  fireAt: string,
  status: ScheduledMessage['status'] = 'pending'
): ScheduledMessage {
  return {
    id,
    conversationKey: 't1',
    content: `content ${id}`,
    fireAt,
    status,
    createdAt: '2026-10-07T10:00:00Z',
    updatedAt: '2026-10-07T10:00:00Z',
  };
}

beforeEach(() => {
  apiFetch.mockReset();
});

describe('conversationSupportsScheduledSend', () => {
  it('allows topics only', () => {
    expect(conversationSupportsScheduledSend('3f2b9c1e-0000-0000-0000-000000000000')).toBe(true);
    expect(conversationSupportsScheduledSend('dm:agent:a:user:u')).toBe(false);
    expect(conversationSupportsScheduledSend('')).toBe(false);
  });
});

describe('applyScheduledUpdate', () => {
  it('adds and orders by fire time, updates in place, removes sent and cancelled', () => {
    let list = applyScheduledUpdate([], sm('b', '2026-10-08T09:00:00Z'));
    list = applyScheduledUpdate(list, sm('a', '2026-10-08T08:00:00Z'));
    expect(list.map((m) => m.id)).toEqual(['a', 'b']);

    list = applyScheduledUpdate(list, sm('a', '2026-10-08T08:00:00Z', 'failed'));
    expect(list.map((m) => [m.id, m.status])).toEqual([
      ['a', 'failed'],
      ['b', 'pending'],
    ]);

    list = applyScheduledUpdate(list, sm('b', '2026-10-08T09:00:00Z', 'sent'));
    list = applyScheduledUpdate(list, sm('a', '2026-10-08T08:00:00Z', 'cancelled'));
    expect(list).toEqual([]);
  });
});

describe('sortScheduled', () => {
  it('orders by instant, not by text (the hub trims fractional zeros)', () => {
    const later = sm('later', '2026-10-08T08:00:00.5Z');
    const earlier = sm('earlier', '2026-10-08T08:00:00Z');
    expect(sortScheduled([later, earlier]).map((m) => m.id)).toEqual(['earlier', 'later']);
  });
});

describe('API helpers', () => {
  it('creates with fire_at, idempotency key and reply-to', async () => {
    apiFetch.mockResolvedValue(
      new Response(JSON.stringify(sm('x', '2026-10-08T07:00:00Z')), { status: 201 })
    );
    const created = await createScheduledMessage('t1', {
      content: 'hi',
      fireAt: '2026-10-08T07:00:00.000Z',
      idempotencyKey: 'k1',
      replyToId: 'm1',
    });
    expect(created.id).toBe('x');
    const [path, init] = apiFetch.mock.calls[0] as [string, RequestInit];
    expect(path).toBe('/api/v1/chat/conversations/t1/scheduled');
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body as string)).toEqual({
      content: 'hi',
      fire_at: '2026-10-08T07:00:00.000Z',
      idempotency_key: 'k1',
      reply_to_id: 'm1',
    });
  });

  it('surfaces the hub error message', async () => {
    apiFetch.mockResolvedValue(
      new Response(JSON.stringify({ error: { message: 'fire_at must be in the future' } }), {
        status: 400,
      })
    );
    await expect(
      createScheduledMessage('t1', { content: 'hi', fireAt: 'x', idempotencyKey: 'k' })
    ).rejects.toThrow('fire_at must be in the future');
  });

  it('lists and cancels', async () => {
    apiFetch.mockResolvedValueOnce(
      new Response(JSON.stringify({ scheduledMessages: [sm('a', '2026-10-08T08:00:00Z')] }), {
        status: 200,
      })
    );
    expect((await listScheduledMessages('t1')).map((m) => m.id)).toEqual(['a']);

    apiFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await cancelScheduledMessage('t1', 'a');
    const [path, init] = apiFetch.mock.calls[1] as [string, RequestInit];
    expect(path).toBe('/api/v1/chat/conversations/t1/scheduled/a');
    expect(init.method).toBe('DELETE');

    apiFetch.mockResolvedValueOnce(new Response('{}', { status: 409 }));
    await expect(cancelScheduledMessage('t1', 'a')).rejects.toThrow('already being sent');
  });
});

describe('scheduledFailureText', () => {
  it('has a message for every reason and a default', () => {
    for (const r of [
      'no_access',
      'conversation_gone',
      'sender_inactive',
      'recipient_gone',
      'missed',
      'interrupted',
    ]) {
      expect(scheduledFailureText(r)).not.toBe(scheduledFailureText(undefined));
    }
  });
});
