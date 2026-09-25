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
 * Tests for <scion-chat-thread> v2 wiring against the server contract.
 *
 * Two invariants are load-bearing and were previously broken:
 *  1. The read watermark POST body must use `messageId` — the field
 *     `handleConversationRead` decodes. Any other name leaves the watermark
 *     empty server-side and unread state never advances.
 *  2. SSE `chat-message-received` events belong to every conversation the user
 *     can see; the thread must only refetch for its own conversation, and
 *     concurrent refetches must collapse into a single in-flight request.
 *  3. The scroll to the newest message must happen after the loaded messages
 *     have rendered, and must not override a deliberate user scroll.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

/** Stand-in for the global stateManager: only the EventTarget surface is used. */
class FakeStateManager extends EventTarget {
  currentScope: { type: string; userId: string } | null = null;
}
const fakeStateManager = new FakeStateManager();

const apiFetch = vi.fn();

vi.mock('../../../client/main.js', () => ({
  get stateManager() {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args) as unknown,
  extractApiError: () => Promise.resolve('error'),
}));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;
type ChatSendDetail = import('./chat-composer.js').ChatSendDetail;
type Message = import('../../../shared/types.js').Message;

const CONVERSATION_KEY = 'topic-1';

/** An empty history response, the shape fetchHistoryV2/backfillV2 expect. */
function emptyHistory(): Response {
  return {
    ok: true,
    status: 200,
    json: () => Promise.resolve({ items: [] }),
  } as unknown as Response;
}

/** Mount a v2 thread with its initial history load already settled. */
async function mount(): Promise<ScionChatThread> {
  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = CONVERSATION_KEY;
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
  apiFetch.mockClear();
  return el;
}

/** Emit a chat-message-received event in the envelope stateManager uses. */
function emitChatMessage(data: Record<string, unknown>): void {
  fakeStateManager.dispatchEvent(
    new CustomEvent('chat-message-received', { detail: { state: {}, data } })
  );
}

/** How many history refetches were issued? */
function historyCalls(): number {
  return apiFetch.mock.calls.filter((c) => String(c[0]).includes('/messages?')).length;
}

describe('scion-chat-thread route-to-agent indicator', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  /**
   * Routing uses per-message recipient data (set at send time), not the
   * current default-agent UI state. Only messages whose `recipient` field
   * was populated at send time show the routing header.
   */
  it('marks only messages with a recipient as routed, not all human messages', async () => {
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'm1',
              sender: 'me@example.com',
              senderId: 'user-me',
              recipient: 'agent:coder',
              msg: 'mine',
              createdAt: '2026-01-01T00:00:00Z',
            },
            {
              id: 'm2',
              sender: 'them@example.com',
              senderId: 'user-them',
              msg: 'theirs (sent before default agent set)',
              createdAt: '2026-01-01T00:01:00Z',
            },
            {
              id: 'm3',
              sender: 'agent:coder',
              senderId: 'agent-1',
              msg: 'reply',
              createdAt: '2026-01-01T00:02:00Z',
            },
          ],
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.currentUserId = 'user-me';
    el.defaultAgent = 'coder';
    el.members = [
      { id: 'user-me', kind: 'user', name: 'Me', email: 'me@example.com' },
      { id: 'user-them', kind: 'user', name: 'Them', email: 'them@example.com' },
      { id: 'agent-1', kind: 'agent', name: 'Coder', email: 'agent:coder' },
    ];
    document.body.appendChild(el);
    await el.updateComplete;

    await vi.waitFor(() => {
      const rendered = el.shadowRoot?.querySelectorAll('scion-chat-message');
      expect(rendered?.length).toBe(3);
    });

    const routed = Array.from(el.shadowRoot?.querySelectorAll('scion-chat-message') ?? []).map(
      (m) => m.getAttribute('routedTo')
    );
    // m1 has recipient=agent:coder → shows "coder"; m2 has no recipient → empty; m3 is agent → empty
    expect(routed).toEqual(['coder', '', '']);
  });
});

describe('scion-chat-thread agent recipient reconciliation', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('preserves the optimistic agent recipient when the SSE message uses thread routing', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      _pendingIdempotencyKeys: Set<string>;
    };
    internals.messageMap.set('pending-1', {
      id: 'pending-1',
      projectId: '',
      sender: '',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      msg: 'Please help',
      type: 'chat',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
      dispatchState: 'pending',
    });
    internals._pendingIdempotencyKeys.add('pending-1');

    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'server-1',
      msg: 'Please help',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: `thread:${CONVERSATION_KEY}`,
      recipientId: CONVERSATION_KEY,
      type: 'chat',
      createdAt: '2026-01-01T00:00:00Z',
    });

    await vi.waitFor(() => expect(internals.messageMap.has('server-1')).toBe(true));
    expect(internals.messageMap.get('server-1')?.recipient).toBe('agent:coder');
    expect(internals.messageMap.get('server-1')?.recipientId).toBe('coder');
    expect(internals.messageMap.has('pending-1')).toBe(false);
  });

  it('preserves the optimistic agent recipient when the POST response finds an SSE version', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    el.defaultAgent = 'coder';
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    let resolveSend!: (response: Response) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveSend = resolve;
        })
    );
    apiFetch.mockResolvedValue(emptyHistory());

    const sendPromise = internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'Please help',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );
    const optimistic = Array.from(internals.messageMap.values()).find(
      (message) => message.dispatchState === 'pending'
    );
    expect(optimistic?.recipient).toBe('agent:coder');

    internals.messageMap.set('server-2', {
      id: 'server-2',
      projectId: '',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: `thread:${CONVERSATION_KEY}`,
      recipientId: CONVERSATION_KEY,
      msg: 'Please help',
      type: 'chat',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
    });
    resolveSend({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ id: 'server-2' }),
    } as unknown as Response);

    await sendPromise;
    expect(internals.messageMap.get('server-2')?.recipient).toBe('agent:coder');
    expect(internals.messageMap.get('server-2')?.recipientId).toBe('coder');
  });

  it('preserves an existing agent recipient when backfill uses thread routing', async () => {
    const el = await mount();
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      mergeMessages(messages: Message[]): void;
    };
    const existing: Message = {
      id: 'server-3',
      projectId: '',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      msg: 'Please help',
      type: 'chat',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
    };
    const backfilled: Message = {
      ...existing,
      recipient: `thread:${CONVERSATION_KEY}`,
      recipientId: CONVERSATION_KEY,
    };

    internals.mergeMessages([existing]);
    internals.mergeMessages([backfilled]);

    expect(internals.messageMap.get('server-3')?.recipient).toBe('agent:coder');
    expect(internals.messageMap.get('server-3')?.recipientId).toBe('coder');
  });
});

// nc-delivery-unreachable: the send response now reports the real dispatch
// outcome instead of the frontend hard-coding "dispatched" on any HTTP 2xx.
describe('scion-chat-thread dispatch state from send response', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('uses resData.dispatchState instead of hard-coding "dispatched"', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 201,
        json: () =>
          Promise.resolve({
            id: 'server-failed',
            dispatchState: 'failed',
            dispatchFailureReason: 'Agent unreachable (suspended)',
            dispatchFailureCode: 'agent_unreachable',
          }),
      } as unknown as Response)
    );

    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hello',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    const msg = internals.messageMap.get('server-failed');
    expect(msg?.dispatchState).toBe('failed');
    expect(msg?.dispatchFailureReason).toBe('Agent unreachable (suspended)');
    expect(msg?.dispatchFailureCode).toBe('agent_unreachable');
  });

  it('falls back to "dispatched" when the response omits dispatchState', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 201,
        json: () => Promise.resolve({ id: 'server-ok' }),
      } as unknown as Response)
    );

    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hello',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    const msg = internals.messageMap.get('server-ok');
    expect(msg?.dispatchState).toBe('dispatched');
  });

  // Review R2/nit 2: when the SSE echo lands before the HTTP response (the
  // opposite ordering from the tests above), handleChatSendV2 must mutate the
  // already-merged SSE message in place (the "sseVersion" branch) rather than
  // let a later Map.set from mergeMessages wipe the failure reason/code.
  it('keeps the failed state and reason when HTTP resolves after the SSE echo (sseVersion branch)', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    let resolveSend!: (response: Response) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveSend = resolve;
        })
    );
    apiFetch.mockResolvedValue(emptyHistory());

    const sendPromise = internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hello',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    // The SSE echo already landed under the real ID, carrying the real
    // outcome (nc-delivery-unreachable review R2).
    internals.messageMap.set('server-sse-failed', {
      id: 'server-sse-failed',
      projectId: '',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      msg: 'hello',
      type: 'instruction',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
      dispatchState: 'failed',
      dispatchFailureReason: 'Agent unreachable (suspended)',
      dispatchFailureCode: 'agent_unreachable',
    });

    resolveSend({
      ok: true,
      status: 201,
      json: () =>
        Promise.resolve({
          id: 'server-sse-failed',
          dispatchState: 'failed',
          dispatchFailureReason: 'Agent unreachable (suspended)',
          dispatchFailureCode: 'agent_unreachable',
        }),
    } as unknown as Response);

    await sendPromise;

    const msg = internals.messageMap.get('server-sse-failed');
    expect(msg?.dispatchState).toBe('failed');
    expect(msg?.dispatchFailureReason).toBe('Agent unreachable (suspended)');
    expect(msg?.dispatchFailureCode).toBe('agent_unreachable');
  });

  // Review round 3, FYI 1: `failed` is terminal for a given message ID. If
  // the HTTP response resolves first and persists `failed` (the sync
  // dispatch_error path, whose SSE echo is published optimistically as
  // "dispatched" before the dispatch attempt runs), a later SSE event for
  // the same ID reporting "dispatched" must not downgrade the entry back to
  // "Delivered". mergeMessages must keep the failed state and its reason/code.
  it('never downgrades a failed message when SSE dispatched arrives after HTTP failed', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 201,
        json: () =>
          Promise.resolve({
            id: 'server-http-first-failed',
            dispatchState: 'failed',
            dispatchFailureReason: 'dispatch failed: connection refused',
            dispatchFailureCode: 'dispatch_error',
          }),
      } as unknown as Response)
    );

    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hello',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    expect(internals.messageMap.get('server-http-first-failed')?.dispatchState).toBe('failed');

    // The SSE echo lands afterward, carrying the pre-dispatch optimistic
    // "dispatched" state (events.go publishes it before the synchronous
    // dispatch attempt for this path). It also carries a changed `msg` text
    // and a later `createdAt` — non-dispatch fields that should still merge
    // in from the incoming entry even though the dispatch fields are pinned
    // (round 4 Nit 3).
    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'server-http-first-failed',
      msg: 'hello (edited)',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      type: 'instruction',
      createdAt: '2026-01-01T00:00:01Z',
      dispatchState: 'dispatched',
    });

    const msg = internals.messageMap.get('server-http-first-failed');
    expect(msg?.dispatchState).toBe('failed');
    expect(msg?.dispatchFailureReason).toBe('dispatch failed: connection refused');
    expect(msg?.dispatchFailureCode).toBe('dispatch_error');
    // Other fields from the incoming entry still merge in.
    expect(msg?.msg).toBe('hello (edited)');
    expect(msg?.createdAt).toBe('2026-01-01T00:00:01Z');
  });

  // Round 4, Optional 1: the guard only checked incoming dispatchState against
  // "dispatched"/"pending". An incoming entry that omits dispatchState
  // entirely (e.g. a backfill/history row without dispatch info) fell through
  // the guard and wiped an existing `failed` state to undefined. `failed` is
  // terminal, so a missing dispatchState must not clear it either.
  it('never clears a failed message when an incoming entry has no dispatchState at all', async () => {
    const el = await mount();
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      mergeMessages(messages: Message[]): void;
    };

    const failed: Message = {
      id: 'server-no-dispatch-state',
      projectId: '',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      msg: 'hello',
      type: 'instruction',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
      dispatchState: 'failed',
      dispatchFailureReason: 'Agent unreachable (suspended)',
      dispatchFailureCode: 'agent_unreachable',
    };
    // Incoming entry for the same ID with no dispatchState field at all.
    const noDispatchState: Message = {
      id: 'server-no-dispatch-state',
      projectId: '',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      msg: 'hello (edited)',
      type: 'instruction',
      agentId: '',
      createdAt: '2026-01-01T00:00:01Z',
    };

    internals.mergeMessages([failed]);
    internals.mergeMessages([noDispatchState]);

    const msg = internals.messageMap.get('server-no-dispatch-state');
    expect(msg?.dispatchState).toBe('failed');
    expect(msg?.dispatchFailureReason).toBe('Agent unreachable (suspended)');
    expect(msg?.dispatchFailureCode).toBe('agent_unreachable');
    // Other fields from the incoming entry still merge in.
    expect(msg?.msg).toBe('hello (edited)');
    expect(msg?.createdAt).toBe('2026-01-01T00:00:01Z');
  });

  // Round 4, item 2: the sseVersion branch of handleChatSendV2 (the SSE echo
  // lands before the HTTP response) has the same never-downgrade guard as
  // mergeMessages, but no test exercised an actual downgrade attempt there —
  // the existing sseVersion-branch test above sends `failed` on both sides,
  // which passes even without the guard. Send a genuine "dispatched" HTTP
  // response after an SSE-delivered `failed` to prove the guard holds.
  it('never downgrades a failed message when HTTP resolves as dispatched after the SSE echo (sseVersion branch)', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as {
      messageMap: Map<string, Message>;
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    let resolveSend!: (response: Response) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveSend = resolve;
        })
    );
    apiFetch.mockResolvedValue(emptyHistory());

    const sendPromise = internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hello',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    // The SSE echo already landed under the real ID, carrying `failed`.
    internals.messageMap.set('server-sse-first-failed', {
      id: 'server-sse-first-failed',
      projectId: '',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      msg: 'hello',
      type: 'instruction',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
      dispatchState: 'failed',
      dispatchFailureReason: 'Agent unreachable (suspended)',
      dispatchFailureCode: 'agent_unreachable',
    });

    // The HTTP response resolves afterward, genuinely reporting "dispatched"
    // (unlike the existing sseVersion test, which resolves "failed" on both
    // sides and would pass even without the guard).
    resolveSend({
      ok: true,
      status: 201,
      json: () =>
        Promise.resolve({
          id: 'server-sse-first-failed',
          dispatchState: 'dispatched',
        }),
    } as unknown as Response);

    await sendPromise;

    const msg = internals.messageMap.get('server-sse-first-failed');
    expect(msg?.dispatchState).toBe('failed');
    expect(msg?.dispatchFailureReason).toBe('Agent unreachable (suspended)');
    expect(msg?.dispatchFailureCode).toBe('agent_unreachable');
  });

  // Review R2: PublishUserMessage now carries dispatchFailureReason/Code on
  // the SSE event for a failed row, so a live viewer in another tab (which
  // only ever sees the SSE path, never the send response) also renders
  // "Agent unreachable" instead of a bare "Failed".
  it('carries dispatchFailureReason and dispatchFailureCode from the SSE event onto the merged message', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as { messageMap: Map<string, Message> };

    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'sse-failed-1',
      msg: 'hi',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'coder',
      type: 'instruction',
      createdAt: '2026-01-01T00:00:00Z',
      dispatchState: 'failed',
      dispatchFailureReason: 'Agent unreachable (suspended)',
      dispatchFailureCode: 'agent_unreachable',
    });

    await vi.waitFor(() => expect(internals.messageMap.has('sse-failed-1')).toBe(true));
    const msg = internals.messageMap.get('sse-failed-1');
    expect(msg?.dispatchState).toBe('failed');
    expect(msg?.dispatchFailureReason).toBe('Agent unreachable (suspended)');
    expect(msg?.dispatchFailureCode).toBe('agent_unreachable');
  });
});

describe('scion-chat-thread read watermark', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('posts the message id under the server-side field name', async () => {
    const el = await mount();

    await (
      el as unknown as { advanceReadWatermark(id: string): Promise<void> }
    ).advanceReadWatermark('msg-7');

    const readCall = apiFetch.mock.calls.find(
      (c) => String(c[0]).endsWith('/read') && (c[1] as RequestInit | undefined)?.method === 'POST'
    );
    expect(readCall).toBeDefined();
    const init = readCall![1] as RequestInit;
    expect(init.method).toBe('POST');
    expect(JSON.parse(String(init.body))).toEqual({ messageId: 'msg-7' });
  });

  it('warns when the server rejects the watermark update', async () => {
    const el = await mount();
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    apiFetch.mockResolvedValue({ ok: false, status: 400 } as unknown as Response);

    await (
      el as unknown as { advanceReadWatermark(id: string): Promise<void> }
    ).advanceReadWatermark('msg-7');

    expect(warn).toHaveBeenCalled();
    warn.mockRestore();
  });

  /**
   * The POST outlives the conversation it was issued for. Announcing its
   * completion afterwards moves the unread badge of a thread the user already
   * left, so a response that lands after a switch must be dropped.
   */
  /**
   * Regression: when a DM is opened for the first time, showUnreadDivider is
   * false (no prior read state exists). The initial-load path must still
   * advance the watermark after the 500ms render-settle delay so the blue
   * dot clears.
   */
  it('advances watermark on initial load even when showUnreadDivider is false', async () => {
    const MESSAGES = [
      { id: 'm1', sender: 'them@example.com', msg: 'hello', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'm2', sender: 'them@example.com', msg: 'world', createdAt: '2026-01-01T00:01:00Z' },
    ];

    const messagesHistory = (): Response =>
      ({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ items: MESSAGES }),
      }) as unknown as Response;

    vi.useFakeTimers();

    apiFetch.mockReset();
    apiFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (init?.method === 'POST' && String(url).endsWith('/read')) {
        return Promise.resolve({ ok: true, status: 200 } as unknown as Response);
      }
      return Promise.resolve(messagesHistory());
    });

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    // Do NOT set showUnreadDivider — simulates first-time DM open.
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);

    // Let the history response settle and the component render.
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(MESSAGES.length)
    );
    await el.updateComplete;

    // Advance past the 500ms render-settle delay.
    vi.advanceTimersByTime(600);
    // Flush the microtask queue so the awaited POST resolves.
    await vi.waitFor(() => {
      const readCall = apiFetch.mock.calls.find(
        (c) =>
          String(c[0]).endsWith('/read') && (c[1] as RequestInit | undefined)?.method === 'POST'
      );
      expect(readCall).toBeDefined();
    });

    const readCall = apiFetch.mock.calls.find(
      (c) => String(c[0]).endsWith('/read') && (c[1] as RequestInit | undefined)?.method === 'POST'
    );
    const body = JSON.parse(String((readCall![1] as RequestInit).body));
    expect(body).toEqual({ messageId: 'm2' });

    vi.useRealTimers();
  });

  it.each([
    ['2026-09-19T00:00:00Z', '2026-09-19T00:00:00Z', 'b', 'a'],
    ['2026-09-19T00:00:00.100000001Z', '2026-09-19T00:00:00.1Z', 'a', 'b'],
    ['2026-09-19T00:00:00.000002Z', '2026-09-19T00:00:00.000001Z', 'a', 'b'],
    ['2026-09-19T01:00:00+01:00', '2026-09-19T00:00:00Z', 'b', 'a'],
  ])(
    'acknowledges the server tail for tied millisecond timestamps (%s / %s)',
    async (newer, older, tailID, oldID) => {
      vi.useFakeTimers();
      try {
        apiFetch.mockImplementation((url: string) =>
          Promise.resolve({
            ok: true,
            json: () =>
              Promise.resolve(
                String(url).includes('/messages?')
                  ? {
                      items: [
                        {
                          id: tailID,
                          sender: 'user:them',
                          msg: 'newest',
                          type: 'chat',
                          createdAt: newer,
                        },
                        {
                          id: oldID,
                          sender: 'user:them',
                          msg: 'older',
                          type: 'chat',
                          createdAt: older,
                        },
                      ],
                    }
                  : {}
              ),
          } as Response)
        );
        const el = await mount();
        await vi.advanceTimersByTimeAsync(600);
        const readCall = apiFetch.mock.calls.find(
          (c) =>
            String(c[0]).endsWith('/read') && (c[1] as RequestInit | undefined)?.method === 'POST'
        );
        expect(readCall).toBeDefined();
        expect(JSON.parse(String((readCall![1] as RequestInit).body))).toEqual({
          messageId: tailID,
        });
        const bubbles = el.shadowRoot!.querySelectorAll('scion-chat-message');
        expect(bubbles[bubbles.length - 1].id).toBe(`msg-${tailID}`);
      } finally {
        vi.useRealTimers();
      }
    }
  );

  it('drops a watermark response that lands after a conversation switch', async () => {
    const el = await mount();

    let settleRead!: (res: Response) => void;
    apiFetch.mockImplementation((url: string) =>
      String(url).endsWith('/read')
        ? new Promise<Response>((resolve) => {
            settleRead = resolve;
          })
        : Promise.resolve(emptyHistory())
    );

    const updated = vi.fn();
    el.addEventListener('read-state-updated', updated);

    const pending = (
      el as unknown as { advanceReadWatermark(id: string): Promise<void> }
    ).advanceReadWatermark('msg-7');

    // Switch away while the POST is in flight.
    el.conversationKey = 'topic-2';
    await el.updateComplete;

    settleRead({ ok: true, status: 200 } as unknown as Response);
    await pending;

    expect(updated).not.toHaveBeenCalled();
  });
});

describe('scion-chat-thread receipt expiry', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  it('hides Seen at the exact timer deadline without another UI event', async () => {
    const el = await mount();
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-09-19T00:00:00Z'));
    el.currentUserId = 'user-me';
    const internals = el as unknown as {
      mergeMessages(messages: Message[]): void;
      applyPeerReadState(id: string, readAt: string): void;
      seenExpired: boolean;
    };
    internals.mergeMessages([
      {
        id: 'receipt-1',
        projectId: '',
        sender: 'user:me@example.com',
        senderId: 'user-me',
        recipient: '',
        msg: 'hello',
        type: 'chat',
        agentId: '',
        dispatchState: 'dispatched',
        createdAt: new Date().toISOString(),
      },
    ]);
    internals.applyPeerReadState('receipt-1', new Date().toISOString());
    await el.updateComplete;
    const bubble = () => el.shadowRoot!.querySelector('scion-chat-message')!;
    expect(bubble().getAttribute('dispatchState')).toBe('dispatched');
    expect(bubble().hasAttribute('seen')).toBe(true);
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000 - 1);
    expect(internals.seenExpired).toBe(false);
    expect(bubble().getAttribute('dispatchState')).toBe('dispatched');
    await vi.advanceTimersByTimeAsync(1);
    await el.updateComplete;
    expect(internals.seenExpired).toBe(true);
    expect(bubble().getAttribute('dispatchState')).toBe('');
  });

  it('rearms expiry when a newer receipt arrives and cancels it on teardown', async () => {
    const el = await mount();
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-09-19T00:00:00Z'));
    const internals = el as unknown as {
      applyPeerReadState(id: string, readAt: string): void;
      seenExpired: boolean;
      _seenExpiryTimer: ReturnType<typeof setTimeout> | null;
    };
    internals.applyPeerReadState('first', new Date().toISOString());
    await vi.advanceTimersByTimeAsync(1000);
    internals.applyPeerReadState('second', new Date().toISOString());
    await vi.advanceTimersByTimeAsync(5 * 60 * 1000 - 1000);
    expect(internals.seenExpired).toBe(false);
    await vi.advanceTimersByTimeAsync(1000);
    expect(internals.seenExpired).toBe(true);
    internals.applyPeerReadState('third', new Date().toISOString());
    el.remove();
    expect(internals._seenExpiryTimer).toBeNull();
  });
});

describe('scion-chat-thread SSE message filtering', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('ignores events for other conversations', async () => {
    await mount();

    emitChatMessage({ threadId: 'some-other-topic', id: 'm1' });
    await Promise.resolve();

    expect(historyCalls()).toBe(0);
  });

  it('refetches history for its own conversation', async () => {
    await mount();

    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm1' });
    await vi.waitFor(() => expect(historyCalls()).toBe(1));
  });

  /**
   * The indicator is otherwise held for TYPING_EXPIRY_MS after the last typing
   * event, so it lingers for seconds after the message it announced arrives.
   */
  it('clears the sender typing indicator when their message arrives', async () => {
    const el = await mount();
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-typing-received', {
        detail: { data: { threadId: CONVERSATION_KEY, userId: 'user-them', displayName: 'Them' } },
      })
    );
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.typing-indicator')).not.toBeNull();

    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm1', senderId: 'user-them' });
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.typing-indicator')).toBeNull();
  });

  it('leaves other users typing indicators alone', async () => {
    const el = await mount();
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-typing-received', {
        detail: { data: { threadId: CONVERSATION_KEY, userId: 'user-them', displayName: 'Them' } },
      })
    );
    await el.updateComplete;

    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm1', senderId: 'user-other' });
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.typing-indicator')).not.toBeNull();
  });

  it('collapses a burst of events into one trailing refetch', async () => {
    await mount();

    // Hold the first refetch open so the following events arrive mid-flight.
    let release: () => void = () => {};
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    apiFetch.mockImplementationOnce(() => gate.then(() => emptyHistory()));

    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm1' });
    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm2' });
    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm3' });
    expect(historyCalls()).toBe(1);

    release();
    // The three events yield the in-flight fetch plus a single trailing one.
    await vi.waitFor(() => expect(historyCalls()).toBe(2));
  });
});

/**
 * A DM mounted from a cold load subscribes before the space rail has
 * configured the chat scope, so the scope is not where the thread can learn
 * who it is — and the user was shown their own "X is typing…".
 */
describe('scion-chat-thread typing self-filter', () => {
  /** Mount a v2 thread, optionally with the user ID the page passes down. */
  async function mountAs(currentUserId: string): Promise<ScionChatThread> {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.currentUserId = currentUserId;
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    return el;
  }

  function emitTyping(userId: string): void {
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-typing-received', {
        detail: { data: { threadId: CONVERSATION_KEY, userId, displayName: 'Me' } },
      })
    );
  }

  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
    fakeStateManager.currentScope = null;
  });

  afterEach(() => {
    fakeStateManager.currentScope = null;
    document.body.innerHTML = '';
  });

  it('falls back to the page-supplied user ID when no scope exists', async () => {
    const el = await mountAs('user-me');

    emitTyping('user-me');
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.typing-indicator')).toBeNull();
  });

  it('picks up the scope user ID when the scope lands after mount', async () => {
    const el = await mountAs('');
    fakeStateManager.currentScope = { type: 'chat', userId: 'user-me' };

    emitTyping('user-me');
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.typing-indicator')).toBeNull();
  });

  it('still shows the peer typing', async () => {
    const el = await mountAs('user-me');

    emitTyping('user-them');
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.typing-indicator')).not.toBeNull();
  });
});

/**
 * Opening a thread landed the user on the oldest message: the scroll ran in a
 * `finally` block without awaiting `updateComplete`, so `scrollToBottom` read
 * the DOM before the loaded messages had rendered — while the thread still
 * showed the loading spinner the scroll container did not even exist, and the
 * scroll was a silent no-op.
 */
describe('scion-chat-thread initial scroll position', () => {
  // happy-dom performs no layout: every element reports zero size, so the
  // geometry the component reads has to be supplied by the test.
  const SCROLL_HEIGHT = 1000;
  const CLIENT_HEIGHT = 300;

  /** Each write to the scroll container's scrollTop, with the DOM it saw. */
  let scrollWrites: { top: number; messagesRendered: number; renderPending: boolean }[] = [];
  let scrollTops: WeakMap<HTMLElement, number>;

  /** The thread element owning a node inside its shadow root. */
  function hostOf(node: HTMLElement): (ScionChatThread & { isUpdatePending: boolean }) | null {
    const root = node.getRootNode();
    const host = (root as ShadowRoot).host as unknown;
    return (host ?? null) as (ScionChatThread & { isUpdatePending: boolean }) | null;
  }
  const originalScrollTop = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollTop');
  const originalScrollHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'scrollHeight'
  );
  const originalClientHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'clientHeight'
  );

  const HISTORY = [
    { id: 'm1', sender: 'them@example.com', msg: 'oldest', createdAt: '2026-01-01T00:00:00Z' },
    { id: 'm2', sender: 'them@example.com', msg: 'middle', createdAt: '2026-01-01T00:01:00Z' },
    { id: 'm3', sender: 'them@example.com', msg: 'newest', createdAt: '2026-01-01T00:02:00Z' },
  ];

  function history(): Response {
    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve({ items: HISTORY }),
    } as unknown as Response;
  }

  /** Mount a v2 thread and wait for its history to render. */
  async function mountWithHistory(): Promise<ScionChatThread> {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(HISTORY.length)
    );
    await el.updateComplete;
    // Let the deferred (post-render) scroll run.
    await Promise.resolve();
    return el;
  }

  /**
   * Let a background history refetch run to completion, including the scroll
   * it defers behind updateComplete. The macrotask flushes are what make the
   * difference: the refetch chain resolves over several microtask turns, so
   * awaiting updateComplete alone looks before anything could have happened.
   */
  async function flushRefetch(el: ScionChatThread): Promise<void> {
    await new Promise((resolve) => setTimeout(resolve, 0));
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 0));
  }

  function scrollContainer(el: ScionChatThread): HTMLElement {
    const node = el.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (!node) throw new Error('scroll container not rendered');
    return node;
  }

  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockImplementation(() => Promise.resolve(history()));
    scrollWrites = [];
    scrollTops = new WeakMap();

    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get: () => SCROLL_HEIGHT,
    });
    Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
      configurable: true,
      get: () => CLIENT_HEIGHT,
    });
    Object.defineProperty(HTMLElement.prototype, 'scrollTop', {
      configurable: true,
      get(this: HTMLElement) {
        return scrollTops.get(this) ?? 0;
      },
      set(this: HTMLElement, value: number) {
        scrollTops.set(this, value);
        // isConnected keeps a detached element's late scroll out of the shared
        // recorder: the setter lives on the prototype, so an element torn down
        // by a previous test can still write here if its refetch chain lands
        // afterwards, and the positive control would count it as its own.
        if (this.classList.contains('messages-scroll') && this.isConnected) {
          scrollWrites.push({
            top: value,
            messagesRendered: this.querySelectorAll('scion-chat-message').length,
            renderPending: hostOf(this)?.isUpdatePending ?? false,
          });
        }
      },
    });
  });

  afterEach(() => {
    for (const [prop, descriptor] of [
      ['scrollTop', originalScrollTop],
      ['scrollHeight', originalScrollHeight],
      ['clientHeight', originalClientHeight],
    ] as const) {
      if (descriptor) {
        Object.defineProperty(HTMLElement.prototype, prop, descriptor);
      } else {
        delete (HTMLElement.prototype as unknown as Record<string, unknown>)[prop];
      }
    }
    document.body.innerHTML = '';
  });

  it('scrolls to the newest message only after the loaded messages have rendered', async () => {
    await mountWithHistory();

    const last = scrollWrites.at(-1);
    expect(last, 'expected a scroll to the bottom after the initial load').toBeDefined();
    expect(last?.top).toBe(SCROLL_HEIGHT);
    // The scroll must see the populated list, not the loading placeholder.
    expect(last?.messagesRendered).toBe(HISTORY.length);
    // And it must not run while a render is still queued — that is the read of
    // stale geometry the bug was made of.
    expect(scrollWrites.filter((w) => w.renderPending)).toEqual([]);
  });

  it('does not yank back a user who scrolled away while a load was in flight', async () => {
    const el = await mountWithHistory();
    const container = scrollContainer(el);

    // The user scrolls up to read older messages.
    container.scrollTop = 0;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    scrollWrites = [];

    // A message arrives on the SSE stream and triggers a background refetch.
    // Wait for the *next* history call: the mount already made one, so waiting
    // for any call at all would be satisfied before the emit is even handled.
    const before = historyCalls();
    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm4' });
    await vi.waitFor(() => expect(historyCalls()).toBeGreaterThan(before));
    // Drain the whole refetch chain — apiFetch promise, .json(), the merge and
    // the deferred updateComplete.then() — before looking. A single microtask
    // is not enough: the assertion would run before a scroll could have
    // happened and would pass with the pinnedToBottom guard deleted.
    await flushRefetch(el);

    expect(scrollWrites.filter((w) => w.top === SCROLL_HEIGHT)).toEqual([]);
  });

  // Positive control for the test above. It has to run on its own element:
  // sharing one with the negative phase lets that phase's still-in-flight
  // refetch land inside this window, so the control passes on someone else's
  // scroll and stops noticing whether flushRefetch is long enough. Keep the
  // flush sequence identical to the negative case — that is the whole point.
  it('positive control: a pinned user IS scrolled by the same refetch', async () => {
    const el = await mountWithHistory();
    scrollWrites = [];

    const before = historyCalls();
    emitChatMessage({ threadId: CONVERSATION_KEY, id: 'm5' });
    await vi.waitFor(() => expect(historyCalls()).toBeGreaterThan(before));
    await flushRefetch(el);

    expect(
      scrollWrites.filter((w) => w.top === SCROLL_HEIGHT),
      'the flush must be long enough for a scroll to land when the user is pinned'
    ).not.toEqual([]);
  });

  it('scrolls back to the bottom when the user asks to jump to latest', async () => {
    const el = await mountWithHistory();
    const container = scrollContainer(el);

    container.scrollTop = 0;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    scrollWrites = [];

    const jump = el.shadowRoot?.querySelector('.jump-btn') as HTMLElement | null;
    expect(jump, 'jump-to-latest pill should be shown once scrolled away').not.toBeNull();
    jump?.click();
    await el.updateComplete;
    await Promise.resolve();

    expect(scrollWrites.at(-1)?.top).toBe(SCROLL_HEIGHT);
  });
});

describe('scion-chat-thread search result navigation', () => {
  const TARGET = {
    id: 'target-message',
    sender: 'them@example.com',
    msg: 'search target',
    createdAt: '2026-01-01T00:01:00Z',
  };
  let scrollIntoView: ReturnType<typeof vi.fn>;
  const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;

  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          messages: [
            {
              ...TARGET,
              id: 'currently-loaded-message',
              msg: 'currently loaded',
            },
          ],
        }),
    } as unknown as Response);
    scrollIntoView = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: scrollIntoView,
    });
  });

  afterEach(() => {
    if (originalScrollIntoView) {
      Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
        configurable: true,
        value: originalScrollIntoView,
      });
    } else {
      delete (HTMLElement.prototype as unknown as Record<string, unknown>).scrollIntoView;
    }
    document.body.innerHTML = '';
  });

  it('fetches and replaces the message window when the target is not loaded', async () => {
    const el = await mount();
    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          messages: [
            { ...TARGET, id: 'before-message', msg: 'before' },
            TARGET,
            { ...TARGET, id: 'after-message', msg: 'after' },
          ],
          nextCursor: 'older-cursor',
        }),
    } as unknown as Response);

    await el.scrollToMessageById(TARGET.id);
    await el.updateComplete;

    expect(apiFetch).toHaveBeenCalledWith(
      expect.stringContaining(
        `/api/v1/chat/conversations/${CONVERSATION_KEY}/messages?around=${TARGET.id}`
      )
    );
    const target = el.shadowRoot?.querySelector(`#msg-${TARGET.id}`);
    expect(target).not.toBeNull();
    expect(el.shadowRoot?.querySelector('#msg-currently-loaded-message')).toBeNull();
    expect(target?.classList.contains('permalink-highlight')).toBe(true);
    expect(scrollIntoView).toHaveBeenCalledWith({ behavior: 'smooth', block: 'center' });
    expect(el.shadowRoot?.querySelector('.jump-btn')).not.toBeNull();
  });

  it('refetches the newest window after jumping to an older search result', async () => {
    const el = await mount();
    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ messages: [TARGET], nextCursor: 'older-cursor' }),
    } as unknown as Response);
    await el.scrollToMessageById(TARGET.id);
    await el.updateComplete;

    const latest = { ...TARGET, id: 'latest-message', msg: 'latest' };
    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ messages: [latest] }),
    } as unknown as Response);

    const jump = el.shadowRoot?.querySelector('.jump-btn') as HTMLElement | null;
    jump?.click();
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelector('#msg-latest-message')).not.toBeNull()
    );

    expect(el.shadowRoot?.querySelector(`#msg-${TARGET.id}`)).toBeNull();
    expect(apiFetch.mock.calls.at(-1)?.[0]).not.toContain('around=');
  });

  it('keeps live tail messages out of a detached around window', async () => {
    const el = await mount();
    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ messages: [TARGET], nextCursor: 'older-cursor' }),
    } as unknown as Response);
    await el.scrollToMessageById(TARGET.id);

    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'live-tail-message',
      msg: 'newest tail',
      sender: 'them@example.com',
      createdAt: '2026-01-01T01:00:00Z',
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('#msg-live-tail-message')).toBeNull();
    expect(el.shadowRoot?.querySelector('.jump-btn')).not.toBeNull();
  });
});

/**
 * SSE-delivered messages with attachments must render the attachment previews
 * immediately — not only after the next user-triggered re-render. The bug was
 * that v2AttachmentMap was populated AFTER mergeMessages(), so the Lit render
 * triggered by the messages array reassignment saw an empty attachment map.
 */
describe('scion-chat-thread SSE attachment preview', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('renders attachment refs on the first render after an SSE message arrives', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    el.members = [
      { id: 'user-me', kind: 'user' as const, name: 'Me', email: 'me@example.com' },
      { id: 'agent-1', kind: 'agent' as const, name: 'Bot', email: 'agent:bot' },
    ];
    await el.updateComplete;

    // Simulate an SSE message from an agent with an attachment.
    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'msg-attach-1',
      msg: 'Here is a file',
      sender: 'agent:bot',
      senderId: 'agent-1',
      type: 'assistant-reply',
      createdAt: new Date().toISOString(),
      attachments: [{ id: 'att-1', name: 'report.pdf', mime: 'application/pdf', size: 1024 }],
    });

    // Wait for the message to render.
    await vi.waitFor(() => {
      const msgs = el.shadowRoot?.querySelectorAll('scion-chat-message');
      expect(msgs?.length).toBe(1);
    });
    await el.updateComplete;

    // The attachment refs should be populated on the first render.
    const chatMsg = el.shadowRoot?.querySelector('scion-chat-message') as
      | import('./chat-message.js').ScionChatMessage
      | null;
    expect(chatMsg).not.toBeNull();
    expect(chatMsg!.attachmentRefs).toHaveLength(1);
    expect(chatMsg!.attachmentRefs[0].name).toBe('report.pdf');
  });
});

describe('scion-chat-thread catch-up after SSE reconnect', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('refetches the latest page when the stream reconnects', async () => {
    await mount();

    // The first connection is not a gap: history was just loaded.
    fakeStateManager.dispatchEvent(new CustomEvent('connected'));
    expect(historyCalls()).toBe(0);

    // A second connection means the stream died and came back; the hub keeps
    // no event history, so anything sent meanwhile was never delivered.
    fakeStateManager.dispatchEvent(new CustomEvent('connected'));
    await vi.waitFor(() => expect(historyCalls()).toBe(1));
  });

  it('stops refetching once unmounted', async () => {
    const el = await mount();
    fakeStateManager.dispatchEvent(new CustomEvent('connected'));
    el.remove();

    fakeStateManager.dispatchEvent(new CustomEvent('connected'));
    await new Promise((r) => setTimeout(r, 10));
    expect(historyCalls()).toBe(0);
  });
});

/**
 * Mention fan-out messages (type:"mention") are created for agent dispatch
 * tracking. They duplicate the content of the primary instruction message and
 * must not appear in the rendered chat. The filter lives in mergeMessages() and
 * must:
 *  1. Exclude mention messages from this.messages (the display array).
 *  2. Allow non-mention types through unchanged.
 *  3. Keep mention messages in messageMap for ID-based dedup tracking.
 */
describe('scion-chat-thread mention message filtering', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('excludes messages with type "mention" from the rendered message list', async () => {
    const el = await mount();

    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'msg-mention-1',
      msg: '@coder please help',
      sender: 'me@example.com',
      senderId: 'user-me',
      type: 'mention',
      createdAt: '2026-01-01T00:00:00Z',
    });

    // Give the SSE handler time to process and merge.
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    // The mention message should NOT appear in the rendered output.
    const rendered = el.shadowRoot?.querySelectorAll('scion-chat-message');
    expect(rendered?.length).toBe(0);
  });

  it('allows messages with other types through the filter', async () => {
    const el = await mount();
    const now = new Date();

    const messages = [
      {
        threadId: CONVERSATION_KEY,
        id: 'msg-instruction',
        msg: 'instruction msg',
        sender: 'me@example.com',
        senderId: 'user-me',
        type: 'instruction',
        createdAt: new Date(now.getTime()).toISOString(),
      },
      {
        threadId: CONVERSATION_KEY,
        id: 'msg-chat',
        msg: 'chat msg',
        sender: 'them@example.com',
        senderId: 'user-them',
        type: 'chat',
        createdAt: new Date(now.getTime() + 1000).toISOString(),
      },
      {
        threadId: CONVERSATION_KEY,
        id: 'msg-empty-type',
        msg: 'empty type msg',
        sender: 'them@example.com',
        senderId: 'user-them',
        type: '',
        createdAt: new Date(now.getTime() + 2000).toISOString(),
      },
    ];

    for (const m of messages) {
      emitChatMessage(m);
    }

    await vi.waitFor(() => {
      const rendered = el.shadowRoot?.querySelectorAll('scion-chat-message');
      expect(rendered?.length).toBe(3);
    });
  });

  it('keeps mention messages in messageMap for dedup tracking', async () => {
    const el = await mount();

    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'msg-mention-dedup',
      msg: '@coder check this',
      sender: 'me@example.com',
      senderId: 'user-me',
      type: 'mention',
      createdAt: '2026-01-01T00:00:00Z',
    });

    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 0));
    await el.updateComplete;

    // Not rendered.
    const rendered = el.shadowRoot?.querySelectorAll('scion-chat-message');
    expect(rendered?.length).toBe(0);

    // But present in messageMap for dedup. messageMap is private — access via
    // type escape so the test can verify the internal invariant.
    const messageMap = (el as unknown as { messageMap: Map<string, unknown> }).messageMap;
    expect(messageMap.has('msg-mention-dedup')).toBe(true);
  });

  it('filters mention messages mixed with displayable messages', async () => {
    const el = await mount();
    const now = new Date();

    // Send a mix: one instruction, one mention, one assistant-reply.
    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'msg-instr',
      msg: 'Help me',
      sender: 'me@example.com',
      senderId: 'user-me',
      type: 'instruction',
      createdAt: new Date(now.getTime()).toISOString(),
    });
    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'msg-mention-mixed',
      msg: 'Help me',
      sender: 'me@example.com',
      senderId: 'user-me',
      type: 'mention',
      createdAt: new Date(now.getTime() + 1000).toISOString(),
    });
    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'msg-reply',
      msg: 'Sure!',
      sender: 'agent:coder',
      senderId: 'agent-1',
      type: 'assistant-reply',
      createdAt: new Date(now.getTime() + 2000).toISOString(),
    });

    // Wait for the two displayable messages to render.
    await vi.waitFor(() => {
      const rendered = el.shadowRoot?.querySelectorAll('scion-chat-message');
      expect(rendered?.length).toBe(2);
    });

    // Verify the mention is in messageMap but not displayed.
    const messageMap = (el as unknown as { messageMap: Map<string, unknown> }).messageMap;
    expect(messageMap.has('msg-mention-mixed')).toBe(true);
    expect(messageMap.has('msg-instr')).toBe(true);
    expect(messageMap.has('msg-reply')).toBe(true);
  });
});

/**
 * Touch devices have no `:hover` state to reveal message actions, and a
 * long-press (which would fire `contextmenu`) is consumed by iOS's native
 * text-selection gesture instead. A tap on the message must open the same
 * context menu a desktop right-click does — but only on touch, and never
 * when the tap actually landed on a link/button inside the message.
 */
describe('scion-chat-thread touch tap-to-open context menu', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  /** Stub `matchMedia('(hover: none)')` to report a touch or hover-capable device. */
  function mockHoverCapability(hoverNone: boolean): void {
    vi.spyOn(window, 'matchMedia').mockImplementation(
      (query: string) =>
        ({
          matches: query === '(hover: none)' ? hoverNone : false,
          media: query,
          addEventListener: vi.fn(),
          removeEventListener: vi.fn(),
        }) as unknown as MediaQueryList
    );
  }

  /** Mount a thread with one rendered message bubble. */
  async function mountWithMessage(): Promise<{ el: ScionChatThread; bubble: HTMLElement }> {
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'm1',
              sender: 'them@example.com',
              senderId: 'user-them',
              msg: 'hello there',
              type: 'chat',
              createdAt: '2026-01-01T00:00:00Z',
            },
          ],
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(1)
    );
    const bubble = el.shadowRoot!.querySelector('scion-chat-message') as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    await bubble.updateComplete;
    return { el, bubble };
  }

  function tap(target: Element): void {
    target.dispatchEvent(
      new MouseEvent('click', { bubbles: true, composed: true, clientX: 10, clientY: 20 })
    );
  }

  it('opens the context menu on tap when the device cannot hover', async () => {
    mockHoverCapability(true);
    const { el, bubble } = await mountWithMessage();

    tap(bubble);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.context-menu')).not.toBeNull();
  });

  it('does not open the context menu on tap on a hover-capable (desktop) device', async () => {
    mockHoverCapability(false);
    const { el, bubble } = await mountWithMessage();

    tap(bubble);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.context-menu')).toBeNull();
  });

  it('ignores a tap that lands on a link inside the message', async () => {
    mockHoverCapability(true);
    const { el, bubble } = await mountWithMessage();

    // e.target is retargeted to the <scion-chat-message> host once the click
    // crosses its shadow boundary, so the handler must consult
    // composedPath()[0] to see the real element that was tapped.
    const anchor = document.createElement('a');
    anchor.setAttribute('class', 'entity-link');
    anchor.href = '#';
    bubble.shadowRoot!.querySelector('.bubble')!.appendChild(anchor);

    tap(anchor);
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.context-menu')).toBeNull();
  });

  it('shows actions for a newly tapped message and hides the previous one', async () => {
    mockHoverCapability(true);
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'm1',
              sender: 'them@example.com',
              senderId: 'user-them',
              msg: 'first',
              type: 'chat',
              createdAt: '2026-01-01T00:00:00Z',
            },
            {
              id: 'm2',
              sender: 'them@example.com',
              senderId: 'user-them',
              msg: 'second',
              type: 'chat',
              createdAt: '2026-01-01T00:01:00Z',
            },
          ],
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(2)
    );
    const [first, second] = Array.from(el.shadowRoot!.querySelectorAll('scion-chat-message'));

    tap(first);
    await el.updateComplete;
    expect(
      (el as unknown as { contextMenuMessage: { id: string } | null }).contextMenuMessage?.id
    ).toBe('m1');

    tap(second);
    await el.updateComplete;
    expect(
      (el as unknown as { contextMenuMessage: { id: string } | null }).contextMenuMessage?.id
    ).toBe('m2');
  });

  it('dismisses an open context menu when the thread scrolls', async () => {
    mockHoverCapability(true);
    const { el, bubble } = await mountWithMessage();

    tap(bubble);
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.context-menu')).not.toBeNull();

    const scrollEl = el.shadowRoot!.querySelector('.messages-scroll')!;
    scrollEl.dispatchEvent(new Event('scroll'));
    await el.updateComplete;

    expect(el.shadowRoot?.querySelector('.context-menu')).toBeNull();
  });
});

describe('scion-chat-thread inter-agent day-split markers', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  function makeIaMessage(overrides: Partial<Message> = {}): Message {
    return {
      id: 'ia-1',
      projectId: '',
      sender: 'agent:alpha',
      senderId: 'agent-alpha-id',
      recipient: 'agent:beta',
      recipientId: 'agent-beta-id',
      msg: 'hello',
      type: 'agent-message',
      agentId: '',
      createdAt: new Date(2026, 0, 15, 9, 0).toISOString(),
      ...overrides,
    };
  }

  /**
   * Mount an agent-DM thread whose history and inter-agent endpoints are both
   * under test control. V2 mode's real-time transport is the mocked
   * `stateManager` EventTarget, not a network EventSource, so this needs no
   * further mocking beyond `apiFetch`.
   */
  async function mountAgentDM(opts: {
    history?: Array<Record<string, unknown>>;
    interagent?: Message[];
  }): Promise<ScionChatThread> {
    apiFetch.mockImplementation((url: unknown) => {
      const u = String(url);
      if (u.includes('/interagent?')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ messages: opts.interagent ?? [] }),
        } as unknown as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ items: opts.history ?? [] }),
      } as unknown as Response);
    });

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = 'dm:agent:coder';
    el.isDM = true;
    document.body.appendChild(el);
    await vi.waitFor(() => {
      const internals = el as unknown as { interagentMessages: Message[] };
      expect(internals.interagentMessages.length).toBe((opts.interagent ?? []).length);
    });
    await el.updateComplete;
    return el;
  }

  it('splits a run of inter-agent messages across 3 days into 3 markers with the main separator between them', async () => {
    const iaMessages = [
      makeIaMessage({ id: 'ia-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-2', createdAt: new Date(2026, 0, 16, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-3', createdAt: new Date(2026, 0, 17, 9, 0).toISOString() }),
    ];
    const el = await mountAgentDM({ interagent: iaMessages });

    const rows = Array.from(el.shadowRoot!.querySelector('.messages-list')!.children);
    const tags = rows.map((r) => r.tagName.toLowerCase());
    expect(tags).toEqual([
      'div',
      'scion-chat-interagent-marker',
      'div',
      'scion-chat-interagent-marker',
      'div',
      'scion-chat-interagent-marker',
    ]);

    const dividers = el.shadowRoot!.querySelectorAll('.date-divider');
    expect(dividers.length).toBe(3);
    expect(dividers[0].textContent).toContain('Jan 15');
    expect(dividers[1].textContent).toContain('Jan 16');
    expect(dividers[2].textContent).toContain('Jan 17');

    const markers = el.shadowRoot!.querySelectorAll('scion-chat-interagent-marker');
    expect(markers.length).toBe(3);
    for (const marker of markers) {
      expect((marker as unknown as { messageCount: number }).messageCount).toBe(1);
    }

    // #1871's per-marker internal divider is gone — the main separator is the
    // only date UI now. That guard belongs on an *expanded* marker (a
    // collapsed marker renders only the pill, so checking for the absence of
    // a divider there proves nothing); see the expanded-marker test below.
  });

  it('does not duplicate the date separator inside an expanded marker', async () => {
    // Regression test for R1: expanding every marker must not render a
    // second, identical divider directly under the one the main timeline
    // already rendered for that day. Each marker here holds a single day
    // (one message), so a correct implementation shows zero internal
    // dividers; the pre-fix code showed one per marker.
    const iaMessages = [
      makeIaMessage({ id: 'ia-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-2', createdAt: new Date(2026, 0, 16, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-3', createdAt: new Date(2026, 0, 17, 9, 0).toISOString() }),
    ];
    const el = await mountAgentDM({ interagent: iaMessages });

    (el as unknown as { interagentExpandAll: boolean }).interagentExpandAll = true;
    await el.updateComplete;

    const markers = Array.from(el.shadowRoot!.querySelectorAll('scion-chat-interagent-marker'));
    expect(markers.length).toBe(3);
    for (const marker of markers) {
      await (marker as unknown as { updateComplete: Promise<boolean> }).updateComplete;
      expect(marker.shadowRoot?.querySelectorAll('.date-divider').length).toBe(0);
    }
  });

  it('renders no orphan date separators when inter-agent messages are hidden', async () => {
    // Regression test for R2: hiding the inter-agent toggle must not leave
    // stacked empty separators for days that contain only hidden markers.
    const iaMessages = [
      makeIaMessage({ id: 'ia-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-2', createdAt: new Date(2026, 0, 16, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-3', createdAt: new Date(2026, 0, 17, 9, 0).toISOString() }),
    ];
    const el = await mountAgentDM({ interagent: iaMessages });

    (el as unknown as { interagentVisible: boolean }).interagentVisible = false;
    await el.updateComplete;

    expect(el.shadowRoot!.querySelectorAll('.date-divider').length).toBe(0);
    const markers = el.shadowRoot!.querySelectorAll('scion-chat-interagent-marker');
    expect(markers.length).toBe(3);
    for (const marker of markers) {
      expect((marker as unknown as { hidden: boolean }).hidden).toBe(true);
    }
  });

  it('preserves a marker element (and its expanded state) across a hide/show toggle', async () => {
    // Regression test for R4: the R2 fix omits the divider row for a day
    // whose only content is a hidden marker, which changes how many rows
    // precede every later row. Rendered without stable keys, Lit's
    // positional array diffing tears down and rebuilds every
    // scion-chat-interagent-marker (and scion-chat-message) after that
    // point, so an expanded marker comes back collapsed. This must fail
    // against 5bd970ca5, which renders `rows` as a plain array.
    const iaMessages = [
      makeIaMessage({ id: 'ia-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-2', createdAt: new Date(2026, 0, 16, 9, 0).toISOString() }),
      makeIaMessage({ id: 'ia-3', createdAt: new Date(2026, 0, 17, 9, 0).toISOString() }),
    ];
    const el = await mountAgentDM({ interagent: iaMessages });

    type MarkerEl = Element & {
      messages: Message[];
      expanded: boolean;
      updateComplete: Promise<boolean>;
    };
    const findMarker = (): MarkerEl =>
      Array.from(el.shadowRoot!.querySelectorAll('scion-chat-interagent-marker')).find(
        (m) => (m as unknown as MarkerEl).messages[0]?.id === 'ia-2'
      ) as unknown as MarkerEl;

    const markerBefore = findMarker();
    markerBefore.expanded = true;
    await markerBefore.updateComplete;
    expect(markerBefore.expanded).toBe(true);

    (el as unknown as { interagentVisible: boolean }).interagentVisible = false;
    await el.updateComplete;
    (el as unknown as { interagentVisible: boolean }).interagentVisible = true;
    await el.updateComplete;

    const markerAfter = findMarker();
    expect(markerAfter).toBe(markerBefore);
    expect(markerAfter.expanded).toBe(true);
  });

  it('does not duplicate the date separator for a human message on the same day as an inter-agent run', async () => {
    const el = await mountAgentDM({
      history: [
        {
          id: 'm1',
          sender: 'them@example.com',
          senderId: 'user-them',
          recipient: 'agent:coder',
          msg: 'before',
          type: 'chat',
          createdAt: new Date(2026, 0, 15, 8, 0).toISOString(),
        },
        {
          id: 'm2',
          sender: 'them@example.com',
          senderId: 'user-them',
          recipient: 'agent:coder',
          msg: 'after',
          type: 'chat',
          createdAt: new Date(2026, 0, 15, 11, 0).toISOString(),
        },
      ],
      interagent: [
        makeIaMessage({ id: 'ia-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
        makeIaMessage({ id: 'ia-2', createdAt: new Date(2026, 0, 15, 9, 30).toISOString() }),
      ],
    });

    const rows = Array.from(el.shadowRoot!.querySelector('.messages-list')!.children);
    const tags = rows.map((r) => r.tagName.toLowerCase());
    expect(tags).toEqual([
      'div', // single date separator
      'scion-chat-message', // m1
      'scion-chat-interagent-marker', // ia-1, ia-2 grouped
      'scion-chat-message', // m2
    ]);

    const dividers = el.shadowRoot!.querySelectorAll('.date-divider');
    expect(dividers.length).toBe(1);
    expect(dividers[0].textContent).toContain('Jan 15');

    const marker = el.shadowRoot!.querySelector('scion-chat-interagent-marker');
    expect((marker as unknown as { messageCount: number }).messageCount).toBe(2);
  });

  it('gives a human message its own divider the day after an inter-agent run', async () => {
    const el = await mountAgentDM({
      history: [
        {
          id: 'm1',
          sender: 'them@example.com',
          senderId: 'user-them',
          recipient: 'agent:coder',
          msg: 'next day',
          type: 'chat',
          createdAt: new Date(2026, 0, 16, 8, 0).toISOString(),
        },
      ],
      interagent: [
        makeIaMessage({ id: 'ia-1', createdAt: new Date(2026, 0, 15, 9, 0).toISOString() }),
        makeIaMessage({ id: 'ia-2', createdAt: new Date(2026, 0, 15, 9, 30).toISOString() }),
      ],
    });

    const rows = Array.from(el.shadowRoot!.querySelector('.messages-list')!.children);
    const tags = rows.map((r) => r.tagName.toLowerCase());
    expect(tags).toEqual([
      'div', // Jan 15 separator
      'scion-chat-interagent-marker', // ia-1, ia-2 grouped
      'div', // Jan 16 separator
      'scion-chat-message', // m1
    ]);

    const dividers = el.shadowRoot!.querySelectorAll('.date-divider');
    expect(dividers.length).toBe(2);
    expect(dividers[0].textContent).toContain('Jan 15');
    expect(dividers[1].textContent).toContain('Jan 16');
  });

  it('splits an inter-agent run across the midnight boundary into two markers', async () => {
    const el = await mountAgentDM({
      interagent: [
        makeIaMessage({ id: 'ia-1', createdAt: new Date(2026, 0, 15, 23, 59, 59).toISOString() }),
        makeIaMessage({ id: 'ia-2', createdAt: new Date(2026, 0, 16, 0, 0, 0).toISOString() }),
      ],
    });

    const rows = Array.from(el.shadowRoot!.querySelector('.messages-list')!.children);
    const tags = rows.map((r) => r.tagName.toLowerCase());
    expect(tags).toEqual([
      'div', // Jan 15 separator
      'scion-chat-interagent-marker', // ia-1
      'div', // Jan 16 separator
      'scion-chat-interagent-marker', // ia-2
    ]);

    const markers = el.shadowRoot!.querySelectorAll('scion-chat-interagent-marker');
    expect(markers.length).toBe(2);
    for (const marker of markers) {
      expect((marker as unknown as { messageCount: number }).messageCount).toBe(1);
    }
  });
});
