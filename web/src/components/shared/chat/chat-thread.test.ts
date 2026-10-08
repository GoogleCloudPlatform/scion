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
  private agentsById = new Map<string, { id: string; projectId: string }>();
  /** Seed an agent record for `getAgent` lookups (peer-agent project fallback). */
  setAgent(id: string, projectId: string): void {
    this.agentsById.set(id, { id, projectId });
  }
  getAgent(id: string): { id: string; projectId: string } | undefined {
    return this.agentsById.get(id);
  }
  /** Clear all seeded agent records (mirrors `setScope()` clearing `state.agents`). */
  clearAgents(): void {
    this.agentsById.clear();
  }
}
const fakeStateManager = new FakeStateManager();

const apiFetch = vi.fn();

const navigateToMock = vi.fn();

const extractApiErrorMock = vi.fn((_res: unknown, _fallback: string) => Promise.resolve('error'));

vi.mock('../../../client/main.js', () => ({
  get navigateTo() {
    return navigateToMock;
  },
  get stateManager() {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args) as unknown,
  extractApiError: (res: unknown, fallback: string) => extractApiErrorMock(res, fallback),
}));

const showConfirmMock = vi.fn<(message: string, options?: unknown) => Promise<boolean>>();
const showToastMock = vi.fn();
vi.mock('../../../utils/toast.js', () => ({
  showToast: (...args: unknown[]) => showToastMock(...args) as unknown,
}));

vi.mock('../confirm-dialog.js', () => ({
  showConfirm: (message: string, options?: unknown) => showConfirmMock(message, options),
}));

const { pinnedAfterScroll } = await import('./chat-thread.js');
// Registers <sl-textarea> so the composer's shadow root actually contains it
// (and its own shadow root) instead of an unupgraded, shadow-less stand-in —
// needed for the reply-focus tests below to walk into the native <textarea>.
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';
type ScionChatThread = import('./chat-thread.js').ScionChatThread;
type ChatSendDetail = import('./chat-composer.js').ChatSendDetail;
type Message = import('../../../shared/types.js').Message;
type ChatAgentMember = import('./chat-members.js').ChatAgentMember;

import { chatRecentFiles } from '../../../client/chat-recent-files.js';
import { wakeConfirmBudgetMs } from './chat-wake.js';
import { agentGraphHref, terminalHref } from '../../../client/open-terminal.js';
import { setPreferredTimeZone } from '../../../utils/time.js';

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

// nc-reply-recipient: the reply's primary recipient is resolved server-side
// from reply_to_id (the replied-to message's actual sender), not from a
// client-supplied agent slug. The client sends only reply_to_id; it must
// never send a routing hint the server would have to trust.
describe('scion-chat-thread reply send payload (nc-reply-recipient)', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('sends reply_to_id and never a client-supplied reply_to_agent', async () => {
    const el = await mount();
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 201,
      json: () => Promise.resolve({ id: 'reply-1' }),
    } as unknown as Response);

    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'thanks!',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
          replyToId: 'orig-msg-1',
          replyToContent: 'original message from the agent',
        },
      })
    );

    const sendCall = apiFetch.mock.calls.find(
      (c) =>
        String(c[0]).endsWith('/messages') && (c[1] as RequestInit | undefined)?.method === 'POST'
    );
    expect(sendCall).toBeDefined();
    const body = JSON.parse(String((sendCall![1] as RequestInit).body));
    expect(body.reply_to_id).toBe('orig-msg-1');
    expect(body).not.toHaveProperty('reply_to_agent');
  });
});

// "Send with interruption": the composer's interrupt flag must reach the v2
// send body, and only when requested.
describe('scion-chat-thread interrupt send payload', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  async function sendAndGetBody(interrupt: boolean): Promise<Record<string, unknown>> {
    const el = await mount();
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 201,
      json: () => Promise.resolve({ id: 'sent-1' }),
    } as unknown as Response);

    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'stop and look at this',
          plain: false,
          interrupt,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    const sendCall = apiFetch.mock.calls.find(
      (c) =>
        String(c[0]).endsWith('/messages') && (c[1] as RequestInit | undefined)?.method === 'POST'
    );
    expect(sendCall).toBeDefined();
    return JSON.parse(String((sendCall![1] as RequestInit).body)) as Record<string, unknown>;
  }

  it('sends interrupt: true when the composer requests interruption', async () => {
    const body = await sendAndGetBody(true);
    expect(body.interrupt).toBe(true);
  });

  it('omits interrupt on an ordinary send', async () => {
    const body = await sendAndGetBody(false);
    expect(body).not.toHaveProperty('interrupt');
  });
});

// Wake-on-send: a send to a suspended agent the user may wake gets a wake
// offer (409 canWake) and a dialog; "Wake and send" resends with wake,
// Cancel hands the draft back to the composer.
describe('scion-chat-thread wake on send', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
    showConfirmMock.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  type Internals = {
    messageMap: Map<string, Message>;
    sendError: string | null;
    handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
  };

  function wakeOfferResponse(): Response {
    return {
      ok: false,
      status: 409,
      json: () =>
        Promise.resolve({
          error: {
            code: 'agent_not_running',
            message: 'Agent "sleepy" is suspended',
            details: { agentId: 'a-1', agentSlug: 'sleepy', phase: 'suspended', canWake: true },
          },
        }),
    } as unknown as Response;
  }

  function createdResponse(body: Record<string, unknown>): Response {
    return {
      ok: true,
      status: 201,
      json: () => Promise.resolve(body),
    } as unknown as Response;
  }

  function sendBodies(): Array<Record<string, unknown>> {
    return apiFetch.mock.calls
      .filter(
        (c) =>
          String(c[0]).endsWith('/messages') && (c[1] as RequestInit | undefined)?.method === 'POST'
      )
      .map((c) => JSON.parse(String((c[1] as RequestInit).body)) as Record<string, unknown>);
  }

  function send(
    internals: Internals,
    callbacks: { onSuccess: () => void; onError: (msg: string) => void }
  ): Promise<void> {
    return internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'please pick this up',
          plain: false,
          interrupt: false,
          mentions: [],
          attachmentIds: [],
          ...callbacks,
        },
      })
    );
  }

  it('asks the hub to offer a wake on an ordinary send', async () => {
    const el = await mount();
    apiFetch.mockResolvedValueOnce(createdResponse({ id: 'ok-1' }));
    await send(el as unknown as Internals, { onSuccess: vi.fn(), onError: vi.fn() });
    const [body] = sendBodies();
    expect(body.offer_wake).toBe(true);
    expect(body).not.toHaveProperty('wake');
    expect(showConfirmMock).not.toHaveBeenCalled();
  });

  it('wakes and delivers when the user confirms', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);

    let resolveWake!: (r: Response) => void;
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveWake = resolve;
        })
    );

    const onSuccess = vi.fn();
    const onError = vi.fn();
    const done = send(internals, { onSuccess, onError });

    // While the wake request is in flight the bubble says it is waking.
    await vi.waitFor(() => expect(sendBodies()).toHaveLength(2));
    const waking = [...internals.messageMap.values()].find((m) => m.msg === 'please pick this up');
    expect(waking?.dispatchState).toBe('waking');

    resolveWake(createdResponse({ id: 'woken-1', dispatchState: 'dispatched' }));
    await done;

    expect(showConfirmMock).toHaveBeenCalledTimes(1);
    expect(String(showConfirmMock.mock.calls[0]![0])).toContain('@sleepy is suspended');
    const bodies = sendBodies();
    expect(bodies[0]!.offer_wake).toBe(true);
    expect(bodies[1]!.wake).toBe(true);
    expect(bodies[1]).not.toHaveProperty('offer_wake');
    expect(bodies[1]!.content).toBe('please pick this up');
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(onError).not.toHaveBeenCalled();
    expect(internals.messageMap.get('woken-1')?.dispatchState).toBe('dispatched');
  });

  it('keeps the draft and sends nothing more when the user cancels', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(false);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());

    const onSuccess = vi.fn();
    const onError = vi.fn();
    await send(internals, { onSuccess, onError });

    expect(sendBodies()).toHaveLength(1);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(onSuccess).not.toHaveBeenCalled();
    expect(internals.sendError).toBeNull();
    expect([...internals.messageMap.values()]).toHaveLength(0);
    // Cancel is not an error: nothing error-like is rendered.
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.send-error')).toBeNull();
  });

  it('offers no wake for an agent that is not resumable', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    apiFetch.mockResolvedValueOnce(
      createdResponse({
        id: 'stopped-1',
        dispatchState: 'failed',
        dispatchFailureReason: 'Agent unreachable (stopped)',
        dispatchFailureCode: 'agent_unreachable',
      })
    );
    await send(internals, { onSuccess: vi.fn(), onError: vi.fn() });

    expect(showConfirmMock).not.toHaveBeenCalled();
    const msg = internals.messageMap.get('stopped-1');
    expect(msg?.dispatchState).toBe('failed');
    expect(msg?.dispatchFailureReason).toBe('Agent unreachable (stopped)');
  });

  it('offers no wake for a conflict without canWake', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    apiFetch.mockResolvedValueOnce({
      ok: false,
      status: 409,
      json: () =>
        Promise.resolve({
          error: {
            code: 'agent_not_running',
            message: 'conversation resolution failed',
            details: { canWake: false },
          },
        }),
    } as unknown as Response);
    const onError = vi.fn();
    await send(internals, { onSuccess: vi.fn(), onError });

    expect(showConfirmMock).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledTimes(1);
    expect(internals.sendError).toBe('conversation resolution failed');
  });

  function composerDisabled(el: ScionChatThread): boolean {
    const composer = el.shadowRoot?.querySelector('scion-chat-composer') as
      | (HTMLElement & { disabled: boolean })
      | null;
    return composer?.disabled ?? false;
  }

  it('blocks the composer while waking and restores the draft only after', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    let resolveWake!: (r: Response) => void;
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveWake = resolve;
        })
    );
    const onError = vi.fn(() => {
      // The draft comes back while the composer is still blocked, so no
      // text typed during the wait can be overwritten.
      expect(composerDisabled(el)).toBe(true);
    });
    const done = send(internals, { onSuccess: vi.fn(), onError });

    await vi.waitFor(() => expect(sendBodies()).toHaveLength(2));
    await el.updateComplete;
    expect(composerDisabled(el)).toBe(true);

    resolveWake({
      ok: false,
      status: 502,
      json: () =>
        Promise.resolve({
          error: { code: 'runtime_error', message: 'Failed to wake agent: boom' },
        }),
    } as unknown as Response);
    await done;
    await el.updateComplete;

    expect(onError).toHaveBeenCalledTimes(1);
    expect(composerDisabled(el)).toBe(false);
  });

  /** A thread whose wake-send retries run without real delays. */
  async function mountFastRetry(): Promise<ScionChatThread> {
    const el = await mount();
    el.currentUserId = 'user-me';
    const t = el as unknown as { wakeRetryDelayMs: number; wakeConfirmBudgetOverrideMs: number };
    t.wakeRetryDelayMs = 1;
    t.wakeConfirmBudgetOverrideMs = 200;
    return el;
  }

  function inProgressResponse(): Response {
    return {
      ok: false,
      status: 409,
      json: () =>
        Promise.resolve({ error: { code: 'send_in_progress', message: 'still running' } }),
    } as unknown as Response;
  }

  it('confirms a dropped wake send by retrying with the same idempotency key', async () => {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
    // The hub is still waking: it says so, then returns the original send.
    apiFetch.mockResolvedValueOnce(inProgressResponse());
    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ id: 'landed-1', dispatchState: 'dispatched' }),
    } as unknown as Response);

    const onSuccess = vi.fn();
    const onError = vi.fn();
    await send(internals, { onSuccess, onError });

    const bodies = sendBodies();
    expect(bodies).toHaveLength(4);
    // Every wake attempt is the identical request: same key, wake set.
    const wakeBodies = bodies.slice(1);
    expect(new Set(wakeBodies.map((b) => b.idempotency_key)).size).toBe(1);
    expect(wakeBodies.every((b) => b.wake === true)).toBe(true);
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(onError).not.toHaveBeenCalled();
    expect(internals.sendError).toBeNull();
    expect(internals.messageMap.get('landed-1')?.dispatchState).toBe('dispatched');
  });

  it('restores the draft when a dropped wake send cannot be confirmed', async () => {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    // Every attempt after the offer fails on the network.
    apiFetch.mockImplementation((url: string, init?: RequestInit) =>
      init?.method === 'POST'
        ? Promise.reject(new TypeError('Failed to fetch'))
        : Promise.resolve(emptyHistory())
    );

    const onSuccess = vi.fn();
    const onError = vi.fn();
    await send(internals, { onSuccess, onError });

    expect(sendBodies().length).toBeGreaterThan(2);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(onSuccess).not.toHaveBeenCalled();
    expect(internals.sendError).toContain('Could not confirm whether the message was delivered');
  });

  it('confirms a dropped wake send against its own conversation after a switch', async () => {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showToastMock.mockReset();
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    let dropWake!: (err: Error) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((_, reject) => {
          dropWake = reject;
        })
    );
    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ id: 'landed-2', dispatchState: 'dispatched' }),
    } as unknown as Response);

    const onSuccess = vi.fn();
    const onError = vi.fn();
    const done = send(internals, { onSuccess, onError });
    await vi.waitFor(() => expect(sendBodies()).toHaveLength(2));
    el.conversationKey = 'topic-2';
    await el.updateComplete;
    dropWake(new TypeError('Failed to fetch'));
    await done;

    const posts = apiFetch.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST'
    );
    // The confirmation goes to the conversation the message was sent in.
    expect(String(posts[2]![0])).toContain(`/conversations/${CONVERSATION_KEY}/messages`);
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(onError).not.toHaveBeenCalled();
    // Delivered: no draft is saved and the open conversation is untouched.
    expect(localStorage.getItem(`scion-chat-draft-${CONVERSATION_KEY}`)).toBeNull();
    expect(showToastMock).not.toHaveBeenCalled();
    expect(internals.messageMap.has('landed-2')).toBe(false);
  });

  /** Lets a pending POST be answered later. */
  function holdNextPost(): { answer: (r: Response) => void; drop: (e: Error) => void } {
    const ctl = {} as { answer: (r: Response) => void; drop: (e: Error) => void };
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve, reject) => {
          ctl.answer = resolve;
          ctl.drop = reject;
        })
    );
    return ctl;
  }

  async function switchConversation(el: ScionChatThread): Promise<void> {
    el.conversationKey = 'topic-2';
    await el.updateComplete;
  }

  it('retries a gateway 502 without a hub error and confirms the send', async () => {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockResolvedValueOnce({
      ok: false,
      status: 502,
      json: () => Promise.reject(new SyntaxError('not json')),
    } as unknown as Response);
    apiFetch.mockResolvedValueOnce({
      ok: false,
      status: 504,
      json: () => Promise.resolve({ message: 'gateway timeout' }),
    } as unknown as Response);
    apiFetch.mockResolvedValueOnce(createdResponse({ id: 'gw-1', dispatchState: 'dispatched' }));

    const onSuccess = vi.fn();
    const onError = vi.fn();
    await send(internals, { onSuccess, onError });

    expect(sendBodies()).toHaveLength(4);
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(onError).not.toHaveBeenCalled();
  });

  it("does not retry the hub's own 502 answer", async () => {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockResolvedValueOnce({
      ok: false,
      status: 502,
      json: () =>
        Promise.resolve({ error: { code: 'runtime_error', message: 'Failed to wake agent: x' } }),
    } as unknown as Response);

    const onError = vi.fn();
    await send(internals, { onSuccess: vi.fn(), onError });

    expect(sendBodies()).toHaveLength(2);
    expect(onError).toHaveBeenCalledTimes(1);
  });

  it('shows the maintenance answer instead of retrying it', async () => {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: () => Promise.resolve({ error: 'system_maintenance', message: 'Down for maintenance' }),
    } as unknown as Response);

    const onError = vi.fn();
    await send(internals, { onSuccess: vi.fn(), onError });

    expect(sendBodies()).toHaveLength(2);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(internals.sendError).not.toContain('Could not confirm');
    // extractApiError (mocked to 'error') read the maintenance answer.
    expect(internals.sendError).toBe('error');
  });

  /**
   * Wake send whose first attempt ends without an outcome (`first`: a
   * dropped connection by default, or a response such as send_in_progress
   * or a gateway drop), then gets `answer` on retry.
   */
  async function dropThenAnswer(
    answer: Response,
    first: 'drop' | Response = 'drop'
  ): Promise<{
    internals: Internals;
    onError: ReturnType<typeof vi.fn>;
    onSuccess: ReturnType<typeof vi.fn>;
  }> {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    if (first === 'drop') {
      apiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
    } else {
      apiFetch.mockResolvedValueOnce(first);
    }
    apiFetch.mockResolvedValueOnce(answer);
    const onError = vi.fn();
    const onSuccess = vi.fn();
    await send(internals, { onSuccess, onError });
    return { internals, onError, onSuccess };
  }

  it('reports an unknown outcome for maintenance after a dropped attempt', async () => {
    const { internals, onError, onSuccess } = await dropThenAnswer({
      ok: false,
      status: 503,
      json: () => Promise.resolve({ error: 'system_maintenance', message: 'Down for maintenance' }),
    } as unknown as Response);
    expect(sendBodies()).toHaveLength(3);
    expect(onSuccess).not.toHaveBeenCalled();
    expect(onError).toHaveBeenCalledTimes(1);
    expect(internals.sendError).toContain('Could not confirm whether the message was delivered');
  });

  function maintenance(): Response {
    return {
      ok: false,
      status: 503,
      json: () => Promise.resolve({ error: 'system_maintenance', message: 'Down for maintenance' }),
    } as unknown as Response;
  }

  it('reports an unknown outcome for maintenance after send_in_progress', async () => {
    const { internals, onError } = await dropThenAnswer(maintenance(), inProgressResponse());
    expect(sendBodies()).toHaveLength(3);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(internals.sendError).toContain('Could not confirm whether the message was delivered');
  });

  it('reports an unknown outcome for maintenance after a gateway drop', async () => {
    const { internals, onError } = await dropThenAnswer(maintenance(), {
      ok: false,
      status: 502,
      json: () => Promise.reject(new SyntaxError('not json')),
    } as unknown as Response);
    expect(sendBodies()).toHaveLength(3);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(internals.sendError).toContain('Could not confirm whether the message was delivered');
  });

  it('reports an unknown outcome for a rate limit after a dropped attempt', async () => {
    const { internals } = await dropThenAnswer({
      ok: false,
      status: 429,
      json: () => Promise.resolve({ error: { code: 'rate_limited', message: 'slow down' } }),
    } as unknown as Response);
    expect(internals.sendError).toContain('Could not confirm whether the message was delivered');
  });

  it("keeps the hub's answer about this send after a dropped attempt", async () => {
    const { internals, onError } = await dropThenAnswer({
      ok: false,
      status: 502,
      json: () =>
        Promise.resolve({ error: { code: 'runtime_error', message: 'Failed to wake agent: x' } }),
    } as unknown as Response);
    expect(onError).toHaveBeenCalledTimes(1);
    // extractApiError is mocked to 'error' in this file: the answer was used.
    expect(internals.sendError).toBe('error');
  });

  it('sizes the confirmation budget from the recipient count', async () => {
    const el = await mount();
    const internals = el as unknown as Internals & {
      postChatSend(
        url: string,
        init: RequestInit,
        wake: boolean,
        budgetMs: number
      ): Promise<Response>;
    };
    const spy = vi.spyOn(internals, 'postChatSend');
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockResolvedValueOnce(createdResponse({ id: 'b-1' }));
    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hi @a @b',
          plain: false,
          interrupt: false,
          mentions: ['a', 'b', 'a'],
          attachmentIds: [],
          onSuccess: vi.fn(),
          onError: vi.fn(),
        },
      })
    );
    const wakeCall = spy.mock.calls.find((c) => c[2] === true);
    expect(wakeCall?.[3]).toBe(wakeConfirmBudgetMs(3));
  });

  it('keeps the could-not-confirm wording when the user switched away', async () => {
    const el = await mountFastRetry();
    const internals = el as unknown as Internals;
    showToastMock.mockReset();
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    const wake = holdNextPost();
    const onError = vi.fn();
    const done = send(internals, { onSuccess: vi.fn(), onError });
    await vi.waitFor(() => expect(sendBodies()).toHaveLength(2));
    await switchConversation(el);
    // Every retry fails on the network too: the outcome stays unknown.
    apiFetch.mockImplementation((_url: string, init?: RequestInit) =>
      init?.method === 'POST'
        ? Promise.reject(new TypeError('Failed to fetch'))
        : Promise.resolve(emptyHistory())
    );
    wake.drop(new TypeError('Failed to fetch'));
    await done;

    expect(onError).not.toHaveBeenCalled();
    expect(internals.sendError).toBeNull();
    expect(localStorage.getItem(`scion-chat-draft-${CONVERSATION_KEY}`)).toBe(
      'please pick this up'
    );
    expect(showToastMock).toHaveBeenCalledTimes(1);
    const toast = String(showToastMock.mock.calls[0]![0]);
    expect(toast).toContain('Could not confirm whether the message was delivered');
    expect(toast).not.toContain('switched conversations');
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
  });

  it('returns a failed send to its own conversation after a switch', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    showToastMock.mockReset();
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    const wake = holdNextPost();
    const onError = vi.fn();
    const done = send(internals, { onSuccess: vi.fn(), onError });
    await vi.waitFor(() => expect(sendBodies()).toHaveLength(2));
    await switchConversation(el);
    wake.answer({
      ok: false,
      status: 403,
      json: () => Promise.resolve({ error: { code: 'forbidden', message: 'no' } }),
    } as unknown as Response);
    await done;

    expect(onError).not.toHaveBeenCalled();
    expect(internals.sendError).toBeNull();
    expect(localStorage.getItem(`scion-chat-draft-${CONVERSATION_KEY}`)).toBe(
      'please pick this up'
    );
    expect(String(showToastMock.mock.calls[0]![0])).toContain('switched conversations');
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
  });

  it('shows no dialog when the wake offer arrives after a switch', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    showToastMock.mockReset();
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
    const first = holdNextPost();
    const onError = vi.fn();
    const done = send(internals, { onSuccess: vi.fn(), onError });
    await vi.waitFor(() => expect(sendBodies()).toHaveLength(1));
    await switchConversation(el);
    first.answer(wakeOfferResponse());
    await done;

    expect(showConfirmMock).not.toHaveBeenCalled();
    expect(onError).not.toHaveBeenCalled();
    expect(sendBodies()).toHaveLength(1);
    expect(localStorage.getItem(`scion-chat-draft-${CONVERSATION_KEY}`)).toBe(
      'please pick this up'
    );
    expect(showToastMock).toHaveBeenCalledTimes(1);
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
  });

  describe('reply bar after a failed send', () => {
    type ReplyInternals = Internals & {
      composerReplyTo: { messageId: string; senderName: string; content: string } | null;
    };
    const reply = { messageId: 'm-9', senderName: 'Coder', content: 'earlier' };

    function sendReply(internals: ReplyInternals, onError: () => void): Promise<void> {
      internals.composerReplyTo = reply;
      return internals.handleChatSendV2(
        new CustomEvent<ChatSendDetail>('chat-send', {
          detail: {
            text: 'replying',
            plain: false,
            interrupt: false,
            mentions: [],
            attachmentIds: [],
            replyToId: reply.messageId,
            onSuccess: vi.fn(),
            onError,
          },
        })
      );
    }

    function failure(): Response {
      return {
        ok: false,
        status: 500,
        json: () => Promise.resolve({ error: { code: 'internal', message: 'boom' } }),
      } as unknown as Response;
    }

    it('is restored when the send fails in the same conversation', async () => {
      const el = await mount();
      const internals = el as unknown as ReplyInternals;
      apiFetch.mockResolvedValueOnce(failure());
      await sendReply(internals, vi.fn());
      expect(internals.composerReplyTo).toEqual(reply);
    });

    it('is not restored into a conversation the user switched to', async () => {
      const el = await mount();
      const internals = el as unknown as ReplyInternals;
      showToastMock.mockReset();
      const pending = holdNextPost();
      const onError = vi.fn();
      const done = sendReply(internals, onError);
      await vi.waitFor(() => expect(sendBodies()).toHaveLength(1));
      await switchConversation(el);
      pending.answer(failure());
      await done;
      expect(internals.composerReplyTo).toBeNull();
      // The composer's onError would restore its own reply bar: not called.
      expect(onError).not.toHaveBeenCalled();
      localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
    });

    it('is not restored after a network error once switched away', async () => {
      const el = await mount();
      const internals = el as unknown as ReplyInternals;
      showToastMock.mockReset();
      const pending = holdNextPost();
      const onError = vi.fn();
      const done = sendReply(internals, onError);
      await vi.waitFor(() => expect(sendBodies()).toHaveLength(1));
      await switchConversation(el);
      pending.drop(new TypeError('Failed to fetch'));
      await done;
      expect(internals.composerReplyTo).toBeNull();
      expect(onError).not.toHaveBeenCalled();
      localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
    });
  });

  it('sends in another conversation while a wake is in flight', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    let resolveWake!: (r: Response) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveWake = resolve;
        })
    );
    const wakeDone = send(internals, { onSuccess: vi.fn(), onError: vi.fn() });
    await vi.waitFor(() => expect(sendBodies()).toHaveLength(2));

    el.conversationKey = 'topic-2';
    await el.updateComplete;
    expect(composerDisabled(el)).toBe(false);
    apiFetch.mockResolvedValueOnce(createdResponse({ id: 'other-1' }));
    const onSuccess = vi.fn();
    const onError = vi.fn();
    await send(internals, { onSuccess, onError });

    const posts = apiFetch.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST'
    );
    expect(String(posts[2]![0])).toContain('/conversations/topic-2/messages');
    expect(onSuccess).toHaveBeenCalledTimes(1);
    expect(onError).not.toHaveBeenCalled();

    resolveWake(createdResponse({ id: 'woken-2', dispatchState: 'dispatched' }));
    await wakeDone;
    // The wake's outcome does not leak into the conversation now open.
    expect(internals.messageMap.has('woken-2')).toBe(false);
  });

  it('hands the draft back when the same conversation is still sending', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    let resolveFirst!: (r: Response) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveFirst = resolve;
        })
    );
    const first = send(internals, { onSuccess: vi.fn(), onError: vi.fn() });
    await vi.waitFor(() => expect(sendBodies()).toHaveLength(1));

    const onSuccess = vi.fn();
    const onError = vi.fn();
    await send(internals, { onSuccess, onError });
    expect(sendBodies()).toHaveLength(1);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(onSuccess).not.toHaveBeenCalled();

    resolveFirst(createdResponse({ id: 'first-1' }));
    await first;
  });

  it('keeps the draft with its conversation when the user switches during the dialog', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    showToastMock.mockReset();
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
    let answer!: (v: boolean) => void;
    showConfirmMock.mockImplementationOnce(
      () =>
        new Promise<boolean>((resolve) => {
          answer = resolve;
        })
    );
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());

    const onError = vi.fn();
    const done = send(internals, { onSuccess: vi.fn(), onError });
    await vi.waitFor(() => expect(showConfirmMock).toHaveBeenCalled());

    el.conversationKey = 'topic-2';
    await el.updateComplete;
    answer(true);
    await done;

    expect(sendBodies()).toHaveLength(1);
    expect(onError).not.toHaveBeenCalled();
    expect(localStorage.getItem(`scion-chat-draft-${CONVERSATION_KEY}`)).toBe(
      'please pick this up'
    );
    expect(showToastMock).toHaveBeenCalledTimes(1);
    expect(String(showToastMock.mock.calls[0]![0])).toContain('kept as a draft');
    localStorage.removeItem(`scion-chat-draft-${CONVERSATION_KEY}`);
  });

  it('keeps the draft and shows the error when the wake is refused', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    showConfirmMock.mockResolvedValueOnce(true);
    apiFetch.mockResolvedValueOnce(wakeOfferResponse());
    apiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: () =>
        Promise.resolve({
          error: { code: 'forbidden', message: 'You do not have permission to wake this agent' },
        }),
    } as unknown as Response);

    const onSuccess = vi.fn();
    const onError = vi.fn();
    await send(internals, { onSuccess, onError });

    expect(sendBodies()).toHaveLength(2);
    expect(onError).toHaveBeenCalledTimes(1);
    expect(onSuccess).not.toHaveBeenCalled();
    // extractApiError is mocked to 'error' in this file.
    expect(internals.sendError).toBe('error');
    expect([...internals.messageMap.values()]).toHaveLength(0);
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

  // A replayed send response without dispatchState (an idempotency hit
  // returns only id/content/sender) defaults to dispatched; it must not
  // overwrite an SSE-delivered terminal no_recipient.
  it('never downgrades an SSE-delivered no_recipient when the HTTP response omits dispatchState', async () => {
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
          text: 'thanks',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    internals.messageMap.set('server-sse-no-recipient', {
      id: 'server-sse-no-recipient',
      projectId: '',
      sender: 'me@example.com',
      senderId: 'user-me',
      recipient: 'thread:t1',
      recipientId: 't1',
      msg: 'thanks',
      type: 'chat',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
      dispatchState: 'no_recipient',
    });

    resolveSend({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ id: 'server-sse-no-recipient', content: 'thanks' }),
    } as unknown as Response);

    await sendPromise;

    expect(internals.messageMap.get('server-sse-no-recipient')?.dispatchState).toBe('no_recipient');
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

describe('scion-chat-thread stale send and the sending state', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('frees the new conversation to send and keeps a stale send off it', async () => {
    const el = await mount();
    const internals = el as unknown as {
      _sendingConversations: Set<string>;
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    const pending: Array<(value: unknown) => void> = [];
    const send = (text: string) =>
      internals.handleChatSendV2(
        new CustomEvent<ChatSendDetail>('chat-send', {
          detail: {
            text,
            plain: false,
            interrupt: false,
            onSuccess: vi.fn(),
            onError: vi.fn(),
            mentions: [],
            attachmentIds: [],
          },
        })
      );
    const holdNextPost = () =>
      apiFetch.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            pending.push(resolve);
          })
      );

    const inFlight = internals._sendingConversations;
    holdNextPost();
    const staleSend = send('first');
    expect(inFlight.has(CONVERSATION_KEY)).toBe(true);

    // Switch conversations while the first POST is still in flight: the
    // composer must not stay stuck in sending on the new conversation.
    el.conversationKey = 'other-thread';
    await el.updateComplete;
    expect(inFlight.has('other-thread')).toBe(false);

    // Start a send on the new conversation, then let the stale one finish.
    holdNextPost();
    const freshSend = send('second');
    expect(inFlight.has('other-thread')).toBe(true);
    await vi.waitFor(() => expect(pending).toHaveLength(2));

    pending[0]({ ok: false, status: 500, text: () => Promise.resolve('') });
    await staleSend;
    expect(inFlight.has(CONVERSATION_KEY)).toBe(false);
    expect(inFlight.has('other-thread')).toBe(true);

    pending[1]({
      ok: true,
      status: 201,
      json: () => Promise.resolve({ id: 'server-fresh', attachments: [] }),
    });
    await freshSend;
    expect(inFlight.size).toBe(0);
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

  /**
   * An optimistic send's temporary idempotency-key ID must never be POSTed
   * as the read watermark. On a slow send (sendAgentRouted can wait up to
   * 30s per recipient on dispatchWithBrokerRetry) with no SSE echo yet, the
   * optimistic message is the only, and therefore "last", message in the
   * thread when the 1s maybeAdvanceReadWatermark debounce fires. The server
   * would reject that ID (handleConversationRead), but the client must not
   * even try.
   */
  it('POSTs the last real message as the read watermark, not the optimistic send id, when no SSE echo has arrived', async () => {
    vi.useFakeTimers();
    try {
      // mount() resolves with empty history, so no message exists yet and
      // the initial-open watermark timer is never scheduled (messages.length
      // is 0 in initialLoadV2's finally block) — isolating this test to the
      // maybeAdvanceReadWatermark debounce path under test, below.
      const el = await mount();
      // mount() only waits for the initial history GET to have been *called*,
      // not for initialLoadV2's post-fetch tail (mergeMessages,
      // scrollToBottomAfterRender, the messages.length check that decides
      // whether to schedule the initial-open watermark timer) to have
      // *settled*. Flush that tail now, before seeding the real message
      // below — otherwise that tail can observe the real message this test
      // adds next and schedule its own (correctly-implemented) initial-open
      // advance, which would mask a regression in the debounce path this
      // test exists to catch.
      await vi.advanceTimersByTimeAsync(0);

      el.currentUserId = 'user-me';
      const internals = el as unknown as {
        messageMap: Map<string, Message>;
        mergeMessages(messages: Message[]): void;
        handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
        maybeAdvanceReadWatermark(): void;
      };

      // Seed one real, already-persisted message — a legitimate watermark
      // candidate distinct from the optimistic send below, so the test can
      // tell "skipped the pending id and posted the real one" apart from
      // "posted nothing".
      internals.mergeMessages([
        {
          id: 'real-msg-1',
          projectId: '',
          sender: 'them@example.com',
          senderId: 'user-them',
          recipient: '',
          msg: 'hi',
          type: 'chat',
          agentId: '',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ]);

      // The send's own POST never resolves within this test — models a slow
      // sendAgentRouted dispatch, or a send that simply outlives the 1s
      // debounce. No SSE echo is emitted either.
      apiFetch.mockImplementation((url: string, init?: RequestInit) => {
        if (String(url).endsWith('/messages') && init?.method === 'POST') {
          return new Promise<Response>(() => {});
        }
        if (init?.method === 'POST' && String(url).endsWith('/read')) {
          return Promise.resolve({ ok: true, status: 200 } as unknown as Response);
        }
        return Promise.resolve(emptyHistory());
      });

      void internals.handleChatSendV2(
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

      const optimistic = Array.from(internals.messageMap.values()).find(
        (m) => m.dispatchState === 'pending'
      );
      expect(optimistic).toBeDefined();

      // Models the scroll event scrollToBottomAfterRender triggers in a real
      // browser: the only trigger that would otherwise start the 1s debounce
      // this early.
      internals.maybeAdvanceReadWatermark();
      await vi.advanceTimersByTimeAsync(1001);

      const readCalls = apiFetch.mock.calls.filter(
        (c) =>
          String(c[0]).endsWith('/read') && (c[1] as RequestInit | undefined)?.method === 'POST'
      );
      expect(readCalls).toHaveLength(1);
      expect(JSON.parse(String((readCalls[0][1] as RequestInit).body))).toEqual({
        messageId: 'real-msg-1',
      });
    } finally {
      vi.useRealTimers();
    }
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

/**
 * Mark-unread sets the caller's own watermark backwards. If this conversation
 * is open when that happens, the normal auto-advance (viewing = read) must
 * not immediately undo it — Slack-like behaviour — until the user navigates
 * away and back, or sends a message here. The signal for "this just
 * happened" is a self-targeted read-state event: the same SSE event a DM
 * peer's "seen" tick uses, just addressed to the reader instead.
 */
describe('scion-chat-thread mark-unread auto-advance suppression', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
  });

  type Internals = {
    mergeMessages(messages: Message[]): void;
    maybeAdvanceReadWatermark(): void;
    handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    _autoAdvanceSuppressed: boolean;
    peerReadMessageId: string;
  };

  function aMessage(id: string): Message {
    return {
      id,
      projectId: '',
      sender: 'them@example.com',
      senderId: 'user-them',
      recipient: '',
      msg: 'hi',
      type: 'chat',
      agentId: '',
      dispatchState: 'dispatched',
      createdAt: new Date().toISOString(),
    };
  }

  /** Was a POST to /read issued? */
  function sawReadPost(): boolean {
    return apiFetch.mock.calls.some(
      (c) => String(c[0]).endsWith('/read') && (c[1] as RequestInit | undefined)?.method === 'POST'
    );
  }

  it("suppresses auto-advance once this tab's own watermark moves backward via SSE with unread:true", async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as Internals;
    internals.mergeMessages([aMessage('m1')]);

    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-read-state-updated', {
        detail: {
          data: {
            conversationKey: CONVERSATION_KEY,
            userId: 'user-me',
            messageId: '',
            unread: true,
          },
        },
      })
    );

    expect(internals._autoAdvanceSuppressed).toBe(true);

    vi.useFakeTimers();
    internals.maybeAdvanceReadWatermark();
    await vi.advanceTimersByTimeAsync(1500);

    expect(sawReadPost()).toBe(false);
  });

  /**
   * The discriminator for "this is mark-unread" is the event's `unread`
   * field, not merely a self-targeted userId. A self event lacking it — e.g.
   * a hypothetical future self-notifying /read — must not suppress, and must
   * not be misapplied as a peer's seen tick either.
   */
  it('does not suppress a self-targeted event without unread:true', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as Internals;
    internals.mergeMessages([aMessage('m1')]);

    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-read-state-updated', {
        detail: { data: { conversationKey: CONVERSATION_KEY, userId: 'user-me', messageId: 'm1' } },
      })
    );

    expect(internals._autoAdvanceSuppressed).toBe(false);
    expect(internals.peerReadMessageId).toBe('');

    vi.useFakeTimers();
    internals.maybeAdvanceReadWatermark();
    await vi.advanceTimersByTimeAsync(1500);

    expect(sawReadPost()).toBe(true);
  });

  /**
   * The debounced callback re-checks suppression when it *fires*, not only
   * when maybeAdvanceReadWatermark schedules it. Exercised directly here
   * (flip the flag after scheduling, without going through
   * suppressAutoAdvance's own clearTimeout) so this covers the guard even if
   * some future suppression path ever sets the flag without also clearing
   * the timer.
   */
  it('re-checks suppression when the debounced callback fires, not only when it was scheduled', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as Internals;
    internals.mergeMessages([aMessage('m1')]);

    vi.useFakeTimers();
    internals.maybeAdvanceReadWatermark();
    internals._autoAdvanceSuppressed = true;
    await vi.advanceTimersByTimeAsync(1500);

    expect(sawReadPost()).toBe(false);
  });

  it("still applies a DM peer's seen tick for a different user id", async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as Internals;

    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-read-state-updated', {
        detail: {
          data: {
            conversationKey: CONVERSATION_KEY,
            userId: 'user-them',
            messageId: 'm-99',
            readAt: new Date().toISOString(),
          },
        },
      })
    );

    expect(internals.peerReadMessageId).toBe('m-99');
    expect(internals._autoAdvanceSuppressed).toBe(false);
  });

  it('ignores a self-targeted read-state event for a different conversation', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as Internals;

    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-read-state-updated', {
        detail: { data: { conversationKey: 'some-other-topic', userId: 'user-me', messageId: '' } },
      })
    );

    expect(internals._autoAdvanceSuppressed).toBe(false);
  });

  it('lifts the suppression when the user sends a message', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';
    const internals = el as unknown as Internals;
    internals._autoAdvanceSuppressed = true;
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ id: 'sent-1' }),
    } as unknown as Response);

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

    expect(internals._autoAdvanceSuppressed).toBe(false);
  });

  it('lifts the suppression when the conversation is switched away from and back', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals._autoAdvanceSuppressed = true;

    el.conversationKey = 'some-other-topic';
    await el.updateComplete;
    expect(internals._autoAdvanceSuppressed).toBe(false);
  });

  /**
   * A dedicated test for the initial-load watermark timer's suppression
   * guard, independent of mount()'s "first apiFetch call" resolution
   * heuristic: mount() returns while loadHistory() may still be in flight,
   * so relying on that resolution order to imply the timer is already armed
   * would be fragile. This test instead waits for `loading` to go false —
   * set in the same synchronous finally block, immediately before the timer
   * is armed — so the timer is armed (on the real clock, since fake timers
   * are never installed here) before the self event is dispatched and before
   * the real-time wait past
   * its delay.
   */
  it('suppresses the initial-load watermark timer directly, independent of mount() timing', async () => {
    apiFetch.mockReset();
    apiFetch.mockImplementation((url: unknown) => {
      if (String(url).includes('/messages?')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: [aMessage('m1')] }),
        } as unknown as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({}),
      } as unknown as Response);
    });

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.currentUserId = 'user-me';
    document.body.appendChild(el);
    await el.updateComplete;

    const internals = el as unknown as Internals & { loading: boolean };
    await vi.waitFor(() => expect(internals.loading).toBe(false));

    apiFetch.mockClear();
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-read-state-updated', {
        detail: {
          data: {
            conversationKey: CONVERSATION_KEY,
            userId: 'user-me',
            messageId: '',
            unread: true,
          },
        },
      })
    );

    // Real-time wait past the initial timer's delay (500ms with no prior
    // read state to show a divider for). Deliberately not vi.useFakeTimers()
    // — the timer was armed on the real clock before this test could have
    // installed a fake one.
    await new Promise((resolve) => setTimeout(resolve, 900));

    expect(sawReadPost()).toBe(false);
  }, 8000);
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

  it('stays at the bottom when the list grows under a pinned reader', async () => {
    const el = await mountWithHistory();
    scrollWrites = [];

    // An image further up finishes loading and the list grows.
    el.keepPinnedToBottom();

    expect(scrollWrites.at(-1)?.top).toBe(SCROLL_HEIGHT);
  });

  it('leaves a reader who scrolled away where they are when the list grows', async () => {
    const el = await mountWithHistory();
    const container = scrollContainer(el);
    container.scrollTop = 0;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    scrollWrites = [];

    el.keepPinnedToBottom();

    expect(scrollWrites).toEqual([]);
  });

  it('keeps the pin when a scroll event trails the list growing beneath the reader', async () => {
    const el = await mountWithHistory();
    const container = scrollContainer(el);
    // At the bottom: 1000 - 700 - 300 = 0.
    container.scrollTop = SCROLL_HEIGHT - CLIENT_HEIGHT;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;

    // An image box renders and the list grows by 484px before the scroll
    // event queued by the last pin write is dispatched. The position did
    // not move up, so this is not the reader scrolling away.
    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get: () => SCROLL_HEIGHT + 484,
    });
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    scrollWrites = [];

    el.keepPinnedToBottom();

    expect(scrollWrites.at(-1)?.top).toBe(SCROLL_HEIGHT + 484);
  });

  it('keeps the pin when the list shrinks and then grows before the scroll event', async () => {
    const el = await mountWithHistory();
    const container = scrollContainer(el);
    container.scrollTop = SCROLL_HEIGHT - CLIENT_HEIGHT;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;

    // Clamp scrollTop to the scroll range, as a browser does.
    let height = SCROLL_HEIGHT;
    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get: () => height,
    });
    Object.defineProperty(HTMLElement.prototype, 'scrollTop', {
      configurable: true,
      get(this: HTMLElement) {
        return Math.min(scrollTops.get(this) ?? 0, Math.max(0, height - CLIENT_HEIGHT));
      },
      set(this: HTMLElement, value: number) {
        scrollTops.set(this, Math.min(value, Math.max(0, height - CLIENT_HEIGHT)));
        if (this.classList.contains('messages-scroll') && this.isConnected) {
          scrollWrites.push({ top: value, messagesRendered: 0, renderPending: false });
        }
      },
    });

    // A reserved image box is taller than the image: the list shrinks, the
    // offset is pulled up, and the resize catch-up runs.
    height = SCROLL_HEIGHT - 171;
    el.keepPinnedToBottom();
    // Then the list grows before the clamp's scroll event is dispatched.
    height = SCROLL_HEIGHT + 101;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    scrollWrites = [];

    el.keepPinnedToBottom();

    expect(scrollWrites.at(-1)?.top).toBe(SCROLL_HEIGHT + 101);
  });

  it('drops the pin when the reader scrolls up past the threshold', async () => {
    const el = await mountWithHistory();
    const container = scrollContainer(el);
    container.scrollTop = SCROLL_HEIGHT - CLIENT_HEIGHT;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;

    container.scrollTop = SCROLL_HEIGHT - CLIENT_HEIGHT - 200;
    container.dispatchEvent(new Event('scroll'));
    await el.updateComplete;
    scrollWrites = [];

    el.keepPinnedToBottom();

    expect(scrollWrites).toEqual([]);
  });

  for (const step of [0.5, 0.9]) {
    it(`drops the pin when the reader drags up slowly, ${step}px at a time`, async () => {
      const el = await mountWithHistory();
      const container = scrollContainer(el);
      container.scrollTop = SCROLL_HEIGHT - CLIENT_HEIGHT;
      container.dispatchEvent(new Event('scroll'));
      await el.updateComplete;

      // Each frame moves less than a pixel, but together they carry the
      // reader well past the threshold.
      let top = SCROLL_HEIGHT - CLIENT_HEIGHT;
      while (top > SCROLL_HEIGHT - CLIENT_HEIGHT - 200) {
        top -= step;
        container.scrollTop = top;
        container.dispatchEvent(new Event('scroll'));
      }
      await el.updateComplete;
      scrollWrites = [];

      el.keepPinnedToBottom();

      expect(scrollWrites).toEqual([]);
    });
  }

  describe('steps aside for the other scroll owners', () => {
    const owners: Array<[string, (el: Record<string, unknown>) => void]> = [
      ['the unread anchor', (el) => (el._unreadAnchorActive = true)],
      ['a jump to a message', (el) => (el._jumpScrollCleanup = () => {})],
      ['a view around an older message', (el) => (el.viewingAroundMessage = true)],
    ];
    for (const [owner, claim] of owners) {
      it(`writes no scroll while ${owner} is steering`, async () => {
        const el = await mountWithHistory();
        const internals = el as unknown as Record<string, unknown>;
        expect(internals.pinnedToBottom, 'the reader starts at the bottom').toBe(true);
        claim(internals);
        scrollWrites = [];

        el.keepPinnedToBottom();

        expect(scrollWrites).toEqual([]);
      });
    }
  });

  it('watches the message list for size changes while open', async () => {
    const observed: Element[] = [];
    const callbacks: ResizeObserverCallback[] = [];
    const original = globalThis.ResizeObserver;
    globalThis.ResizeObserver = class {
      constructor(cb: ResizeObserverCallback) {
        callbacks.push(cb);
      }
      observe(target: Element): void {
        observed.push(target);
      }
      unobserve(): void {}
      disconnect(): void {}
    } as unknown as typeof ResizeObserver;
    try {
      const el = await mountWithHistory();
      expect(observed.some((t) => t.classList.contains('messages-list'))).toBe(true);
      scrollWrites = [];
      for (const cb of callbacks) cb([], {} as ResizeObserver);
      expect(scrollWrites.at(-1)?.top).toBe(SCROLL_HEIGHT);
    } finally {
      globalThis.ResizeObserver = original;
    }
  });

  it('stops watching the message list once removed', async () => {
    const instances: { targets: Element[]; disconnects: number }[] = [];
    const original = globalThis.ResizeObserver;
    globalThis.ResizeObserver = class {
      private readonly record = { targets: [] as Element[], disconnects: 0 };
      constructor() {
        instances.push(this.record);
      }
      observe(target: Element): void {
        this.record.targets.push(target);
      }
      unobserve(): void {}
      disconnect(): void {
        this.record.disconnects++;
      }
    } as unknown as typeof ResizeObserver;
    try {
      const el = await mountWithHistory();
      const watch = instances.find((r) =>
        r.targets.some((t) => t.classList.contains('messages-list'))
      );
      expect(watch, 'the list is watched while open').toBeDefined();
      const before = watch!.disconnects;

      el.remove();

      expect(watch!.disconnects).toBe(before + 1);
      expect((el as unknown as Record<string, unknown>)._bottomPinTarget).toBeNull();
    } finally {
      globalThis.ResizeObserver = original;
    }
  });
});

describe('pinnedAfterScroll', () => {
  it('pins near the bottom whatever came before', () => {
    expect(pinnedAfterScroll(false, 0, true, true)).toBe(true);
    expect(pinnedAfterScroll(false, 79, false, false)).toBe(true);
  });

  it('keeps a pin when the list grew without the reader scrolling up', () => {
    expect(pinnedAfterScroll(true, 484, false, false)).toBe(true);
  });

  it('drops the pin when the reader scrolled up', () => {
    expect(pinnedAfterScroll(true, 200, true, false)).toBe(false);
  });

  it('drops the pin while another scroll owner is steering', () => {
    expect(pinnedAfterScroll(true, 200, false, true)).toBe(false);
  });

  it('never pins a reader away from the bottom who was not pinned', () => {
    expect(pinnedAfterScroll(false, 200, false, false)).toBe(false);
  });
});

/**
 * Opening a thread with unread messages should land the "New messages"
 * divider near the top of the viewport (nc-open-at-unread), not centered and
 * not at the bottom — the opposite of "Jump to latest", which the user
 * confirmed is correct and unchanged. A search result or a `#msg-` deep link
 * takes precedence, and the anchor only ever applies once, at open time: a
 * message arriving over SSE afterwards must not re-anchor to the divider.
 */
describe('scion-chat-thread unread-divider open anchor', () => {
  // happy-dom performs no layout: every element reports zero size and zero
  // offsetTop, so the geometry the component reads has to be supplied by the
  // test — same approach as the "initial scroll position" describe above.
  const SCROLL_HEIGHT = 1000;
  const CLIENT_HEIGHT = 300;
  const DIVIDER_OFFSET_TOP = 500;
  const ANCHOR_MARGIN_PX = 16;

  let scrollTops: WeakMap<HTMLElement, number>;
  let dividerOffsetTop: number;

  /**
   * Captures the ResizeObserver callback so a test can fire a "resize" by
   * hand: happy-dom exposes the `ResizeObserver` constructor but never
   * actually observes real layout changes (nothing in these tests has real
   * layout at all — see the offsetTop/scrollHeight stubs below).
   */
  class StubResizeObserver {
    static instances: StubResizeObserver[] = [];
    disconnected = false;
    private readonly callback: ResizeObserverCallback;

    constructor(callback: ResizeObserverCallback) {
      this.callback = callback;
      StubResizeObserver.instances.push(this);
    }

    observe(): void {}
    unobserve(): void {}
    disconnect(): void {
      this.disconnected = true;
    }
    /** Simulate the browser telling us `.messages-list` changed height. */
    trigger(): void {
      this.callback([], this as unknown as ResizeObserver);
    }
  }
  const originalResizeObserver = globalThis.ResizeObserver;

  const originalScrollTop = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollTop');
  const originalScrollHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'scrollHeight'
  );
  const originalClientHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'clientHeight'
  );
  const originalOffsetTop = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetTop');

  const MESSAGES = [
    {
      id: 'm1',
      sender: 'them@example.com',
      msg: 'already read',
      createdAt: '2026-01-01T00:00:00Z',
    },
    { id: 'm2', sender: 'them@example.com', msg: 'unread one', createdAt: '2026-01-01T00:01:00Z' },
    { id: 'm3', sender: 'them@example.com', msg: 'unread two', createdAt: '2026-01-01T00:02:00Z' },
  ];

  function messagesHistory(): Response {
    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve({ items: MESSAGES }),
    } as unknown as Response;
  }

  /** Route apiFetch the way the real read-state (GET) / watermark (POST) /
   *  history endpoints are actually split, so fetchOwnReadState sees a
   *  distinct response from advanceReadWatermark's POST to the same path. */
  function mockEndpoints(lastReadMessageId: string | null): void {
    apiFetch.mockImplementation((url: string, init?: RequestInit) => {
      const u = String(url);
      if (u.endsWith('/read')) {
        if (init?.method === 'POST') {
          return Promise.resolve({ ok: true, status: 200 } as unknown as Response);
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve(lastReadMessageId ? { lastReadMessageId } : {}),
        } as unknown as Response);
      }
      if (u.includes('/messages?')) {
        return Promise.resolve(messagesHistory());
      }
      return Promise.resolve(emptyHistory());
    });
  }

  function scrollContainer(el: ScionChatThread): HTMLElement {
    const node = el.shadowRoot?.querySelector('.messages-scroll') as HTMLElement | null;
    if (!node) throw new Error('scroll container not rendered');
    return node;
  }

  function mountThread(): ScionChatThread {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    return el;
  }

  /**
   * Installs a fake requestAnimationFrame/cancelAnimationFrame pair backed by
   * a handle->callback map, mirroring a real browser: only callbacks that
   * weren't canceled are left for a test to fire. `restore` puts the
   * originals back and must be called (e.g. from a `finally`) once the test
   * is done driving the fake queue.
   */
  function installRafStub(): {
    callbacks: Map<number, FrameRequestCallback>;
    cancelSpy: ReturnType<typeof vi.fn<(handle: number) => void>>;
    restore: () => void;
  } {
    const callbacks = new Map<number, FrameRequestCallback>();
    let nextHandle = 0;
    const originalRaf = globalThis.requestAnimationFrame;
    const originalCancelRaf = globalThis.cancelAnimationFrame;
    const cancelSpy = vi.fn((handle: number) => {
      callbacks.delete(handle);
    });
    globalThis.requestAnimationFrame = ((cb: FrameRequestCallback) => {
      const handle = ++nextHandle;
      callbacks.set(handle, cb);
      return handle;
    }) as typeof requestAnimationFrame;
    globalThis.cancelAnimationFrame = cancelSpy as typeof cancelAnimationFrame;
    return {
      callbacks,
      cancelSpy,
      restore: () => {
        globalThis.requestAnimationFrame = originalRaf;
        globalThis.cancelAnimationFrame = originalCancelRaf;
      },
    };
  }

  beforeEach(() => {
    apiFetch.mockReset();
    scrollTops = new WeakMap();
    dividerOffsetTop = DIVIDER_OFFSET_TOP;
    window.location.hash = '';
    StubResizeObserver.instances = [];

    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get: () => SCROLL_HEIGHT,
    });
    Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
      configurable: true,
      get: () => CLIENT_HEIGHT,
    });
    Object.defineProperty(HTMLElement.prototype, 'offsetTop', {
      configurable: true,
      get(this: HTMLElement) {
        return this.classList.contains('unread-divider') ? dividerOffsetTop : 0;
      },
    });
    Object.defineProperty(HTMLElement.prototype, 'scrollTop', {
      configurable: true,
      get(this: HTMLElement) {
        return scrollTops.get(this) ?? 0;
      },
      set(this: HTMLElement, value: number) {
        scrollTops.set(this, value);
      },
    });
  });

  afterEach(() => {
    for (const [prop, descriptor] of [
      ['scrollTop', originalScrollTop],
      ['scrollHeight', originalScrollHeight],
      ['clientHeight', originalClientHeight],
      ['offsetTop', originalOffsetTop],
    ] as const) {
      if (descriptor) {
        Object.defineProperty(HTMLElement.prototype, prop, descriptor);
      } else {
        delete (HTMLElement.prototype as unknown as Record<string, unknown>)[prop];
      }
    }
    globalThis.ResizeObserver = originalResizeObserver;
    window.location.hash = '';
    document.body.innerHTML = '';
  });

  it('scrolls the unread divider near the top of the viewport on open', async () => {
    mockEndpoints('m1');
    const el = mountThread();

    await vi.waitFor(() => expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull());
    const container = scrollContainer(el);
    await vi.waitFor(() => expect(container.scrollTop).toBeGreaterThan(0));

    // Anchored just above the divider, with a margin — not centered (the old
    // behavior) and not at the bottom.
    expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX);
    expect(container.scrollTop).toBeLessThan(SCROLL_HEIGHT - CLIENT_HEIGHT);
  });

  it('scrolls to the bottom when there are no unread messages', async () => {
    mockEndpoints(null);
    const el = mountThread();

    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(MESSAGES.length)
    );
    const container = scrollContainer(el);
    await vi.waitFor(() => expect(container.scrollTop).toBe(SCROLL_HEIGHT));
    expect(el.shadowRoot?.querySelector('.unread-divider')).toBeNull();
  });

  it('clamps to the bottom when the unread content is shorter than the viewport', async () => {
    mockEndpoints('m1');
    // The divider sits close enough to the tail that anchoring it to the top
    // with a margin would overscroll past the container's max scroll.
    dividerOffsetTop = SCROLL_HEIGHT - 10;
    const el = mountThread();

    await vi.waitFor(() => expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull());
    const container = scrollContainer(el);
    const maxScroll = SCROLL_HEIGHT - CLIENT_HEIGHT;
    await vi.waitFor(() => expect(container.scrollTop).toBe(maxScroll));
  });

  it('a message deep link overrides the unread anchor', async () => {
    mockEndpoints('m1');
    window.location.hash = '#msg-m2';
    const scrollIntoView = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: scrollIntoView,
    });

    try {
      const el = mountThread();

      await vi.waitFor(() => expect(scrollIntoView).toHaveBeenCalled());

      // The unread-divider anchor write never happened: jumping straight to
      // the linked message took precedence, and the container was never
      // otherwise scrolled.
      const container = scrollContainer(el);
      expect(container.scrollTop).toBe(0);
    } finally {
      delete (HTMLElement.prototype as unknown as Record<string, unknown>).scrollIntoView;
    }
  });

  it('does not re-anchor to the divider when a new SSE message arrives', async () => {
    mockEndpoints('m1');
    const el = mountThread();

    await vi.waitFor(() => expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull());
    const container = scrollContainer(el);
    await vi.waitFor(() => expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX));
    const anchoredTop = container.scrollTop;

    emitChatMessage({
      threadId: CONVERSATION_KEY,
      id: 'm4',
      msg: 'new incoming',
      sender: 'them@example.com',
      createdAt: '2026-01-01T00:03:00Z',
    });
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(4)
    );
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 0));

    // Still parked at the unread anchor — a live message keeps the ordinary
    // stick-to-bottom / "Jump to latest" behavior, which here means "don't
    // move", since the anchor already left pinnedToBottom false.
    expect(container.scrollTop).toBe(anchoredTop);
    expect(el.shadowRoot?.querySelector('.jump-btn')).not.toBeNull();
  });

  /**
   * R1 (review of nc-open-at-unread): `chat.ts:handleSearchNavigate` routes a
   * same-thread search result straight to `scrollToMessageById`, with no
   * `#msg-` hash, so `initialLoadV2`'s hash-vs-divider precedence never runs.
   * Reproduced in Chromium: opening with unread, jumping to a loaded message
   * at 400ms, then growing a row one frame later left the target at -1808px
   * because the ResizeObserver's re-anchor overrode the search-jump.
   */
  it('deactivates the unread anchor on a same-thread search-jump, so a later resize does not override it (R1)', async () => {
    mockEndpoints('m1');
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
    const scrollIntoView = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: scrollIntoView,
    });

    try {
      const el = mountThread();
      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      const container = scrollContainer(el);
      await vi.waitFor(() =>
        expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX)
      );
      const ro = StubResizeObserver.instances.at(-1);
      expect(ro).toBeDefined();

      // Search-jump to an already-loaded message, within the 2s anchor window.
      await el.scrollToMessageById('m3');
      expect(scrollIntoView).toHaveBeenCalled();

      // scrollIntoView is stubbed above (no real layout in happy-dom), so
      // simulate where the smooth scroll left the viewport.
      container.scrollTop = 42;

      // One frame later, a row above the divider resizes (image/attachment
      // load), the way it did in the Chromium repro.
      ro?.trigger();
      await el.updateComplete;

      // The anchor must not have re-applied and pulled the view back to the
      // divider — that was the R1 bug (reproduced as -1808px in Chromium).
      expect(container.scrollTop).toBe(42);
    } finally {
      delete (HTMLElement.prototype as unknown as Record<string, unknown>).scrollIntoView;
    }
  });

  it('a reply-jump also deactivates the unread anchor', async () => {
    mockEndpoints('m1');
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
    const scrollIntoView = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: scrollIntoView,
    });

    try {
      const el = mountThread();
      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      const container = scrollContainer(el);
      await vi.waitFor(() =>
        expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX)
      );
      const ro = StubResizeObserver.instances.at(-1);

      // Reply-preview click routes through the same `scroll-to-message` event
      // as the reply-jump path (see `handleScrollToMessage`).
      const anyMessage = el.shadowRoot?.querySelector('scion-chat-message');
      expect(anyMessage).not.toBeNull();
      anyMessage?.dispatchEvent(
        new CustomEvent('scroll-to-message', {
          detail: { messageId: 'm3' },
          bubbles: true,
          composed: true,
        })
      );
      await el.updateComplete;

      container.scrollTop = 77;
      ro?.trigger();

      expect(container.scrollTop).toBe(77);
    } finally {
      delete (HTMLElement.prototype as unknown as Record<string, unknown>).scrollIntoView;
    }
  });

  it('"Jump to latest" also deactivates the unread anchor, for symmetry', async () => {
    mockEndpoints('m1');
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
    const el = mountThread();

    await vi.waitFor(() => expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull());
    const container = scrollContainer(el);
    await vi.waitFor(() => expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX));
    const ro = StubResizeObserver.instances.at(-1);

    const jump = el.shadowRoot?.querySelector('.jump-btn') as HTMLElement | null;
    expect(jump).not.toBeNull();
    jump?.click();
    await el.updateComplete;
    await new Promise((resolve) => setTimeout(resolve, 0));

    expect(container.scrollTop).toBe(SCROLL_HEIGHT);

    // A later resize must not pull the view back to the divider.
    container.scrollTop = 123;
    ro?.trigger();
    expect(container.scrollTop).toBe(123);
  });

  describe('rule-6 machinery (resize re-anchor, manual-scroll stop, teardown)', () => {
    it('re-applies the anchor when content above the divider resizes', async () => {
      mockEndpoints('m1');
      globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
      const el = mountThread();

      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      const container = scrollContainer(el);
      await vi.waitFor(() =>
        expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX)
      );
      const ro = StubResizeObserver.instances.at(-1);

      // The divider moves down as content above it grows.
      dividerOffsetTop = DIVIDER_OFFSET_TOP + 50;
      ro?.trigger();

      expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP + 50 - ANCHOR_MARGIN_PX);
    });

    it('stops re-anchoring once the user scrolls manually', async () => {
      mockEndpoints('m1');
      globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
      const el = mountThread();

      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      const container = scrollContainer(el);
      await vi.waitFor(() =>
        expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX)
      );
      const ro = StubResizeObserver.instances.at(-1);

      // A real user scroll: the browser sets scrollTop and fires 'scroll' —
      // not through applyUnreadAnchor's own write.
      container.scrollTop = 10;
      container.dispatchEvent(new Event('scroll'));

      dividerOffsetTop = DIVIDER_OFFSET_TOP + 50;
      ro?.trigger();

      // No re-apply: the manual scroll already deactivated the anchor.
      expect(container.scrollTop).toBe(10);
    });

    /**
     * O2 (review round 2 of nc-open-at-unread): `resetV2State` and
     * `disconnectedCallback` also `clearTimeout` unrelated timers
     * (`_initialWatermarkTimer`, etc.), so a bare
     * `expect(clearTimeoutSpy).toHaveBeenCalled()` passed even with the
     * anchor's own `clearTimeout(this._unreadAnchorTimer)` deleted from
     * `deactivateUnreadAnchor` (review finding O2). Reading `_unreadAnchorTimer`
     * directly captures the exact id the anchor's own `setTimeout` returned,
     * so the assertion can require `clearTimeout` be called with that
     * specific id rather than with any id at all.
     */
    it('tears down the resize observer and timer on thread switch', async () => {
      mockEndpoints('m1');
      globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
      const clearTimeoutSpy = vi.spyOn(globalThis, 'clearTimeout');
      const el = mountThread();

      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      await vi.waitFor(() =>
        expect(scrollContainer(el).scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX)
      );
      const ro = StubResizeObserver.instances.at(-1);
      expect(ro?.disconnected).toBe(false);
      const anchorTimerId = (
        el as unknown as { _unreadAnchorTimer: ReturnType<typeof setTimeout> | null }
      )._unreadAnchorTimer;
      expect(anchorTimerId).not.toBeNull();
      clearTimeoutSpy.mockClear();

      el.conversationKey = 'topic-2';
      await el.updateComplete;

      expect(ro?.disconnected).toBe(true);
      expect(clearTimeoutSpy).toHaveBeenCalledWith(anchorTimerId);
      clearTimeoutSpy.mockRestore();
    });

    it('tears down the resize observer and timer on disconnect', async () => {
      mockEndpoints('m1');
      globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
      const clearTimeoutSpy = vi.spyOn(globalThis, 'clearTimeout');
      const el = mountThread();

      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      await vi.waitFor(() =>
        expect(scrollContainer(el).scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX)
      );
      const ro = StubResizeObserver.instances.at(-1);
      expect(ro?.disconnected).toBe(false);
      const anchorTimerId = (
        el as unknown as { _unreadAnchorTimer: ReturnType<typeof setTimeout> | null }
      )._unreadAnchorTimer;
      expect(anchorTimerId).not.toBeNull();
      clearTimeoutSpy.mockClear();

      el.remove();

      expect(ro?.disconnected).toBe(true);
      expect(clearTimeoutSpy).toHaveBeenCalledWith(anchorTimerId);
      clearTimeoutSpy.mockRestore();
    });
  });

  /**
   * O1 (review round 2 of nc-open-at-unread): the anchor state was a shared
   * boolean, not a per-open token, so a deferred rAF callback (or a
   * ResizeObserver notification) scheduled by a superseded open (thread A)
   * could in principle apply to the thread now open (thread B) once B also
   * activates its own anchor. Both callbacks now capture `fetchId` as a local
   * `openToken` at schedule time, so a stale callback compares against a
   * fixed value instead of the shared field the next open's activation would
   * otherwise have overwritten.
   *
   * The round-2 version of this test asserted only that B's scrollTop was
   * unchanged after firing A's stale callback — but `applyUnreadAnchor`
   * always reads the *current* DOM, so an ungated stale call just recomputes
   * and rewrites the value B's own callback had already written, making the
   * assertion pass even with both token checks deleted (review finding O1).
   * To make an ungated call produce an observably different result, this
   * restores A's own divider geometry — a position distinguishable from B's —
   * immediately before firing each stale callback, so an ungated
   * `applyUnreadAnchor` would actually move the scroll position to A's
   * target. It also checks the ResizeObserver side effects an ungated call
   * would have (tearing down and replacing B's real observer), since B's
   * `.messages-list` never resizes in this test to trigger the RO through the
   * scroll-position check alone.
   */
  it('a stale deferred rAF and a stale ResizeObserver callback from a superseded open cannot apply to the thread now open', async () => {
    mockEndpoints('m1');
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
    const rafQueue: FrameRequestCallback[] = [];
    const originalRaf = globalThis.requestAnimationFrame;
    globalThis.requestAnimationFrame = ((cb: FrameRequestCallback) => {
      rafQueue.push(cb);
      return rafQueue.length;
    }) as typeof requestAnimationFrame;

    const A_DIVIDER_OFFSET = DIVIDER_OFFSET_TOP;
    const B_DIVIDER_OFFSET = DIVIDER_OFFSET_TOP + 200;
    const aAnchorTarget = A_DIVIDER_OFFSET - ANCHOR_MARGIN_PX;
    const bAnchorTarget = B_DIVIDER_OFFSET - ANCHOR_MARGIN_PX;

    try {
      const el = mountThread();
      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      // Thread A's activation rAF is queued, not yet run.
      await vi.waitFor(() => expect(rafQueue.length).toBe(1));
      const staleCallbackFromA = rafQueue.shift()!;

      // Switch to thread B before A's rAF fires. B also has unread content,
      // at a divider position distinguishable from A's.
      dividerOffsetTop = B_DIVIDER_OFFSET;
      el.conversationKey = 'topic-2';
      await el.updateComplete;
      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      await vi.waitFor(() => expect(rafQueue.length).toBe(1));
      const callbackFromB = rafQueue.shift()!;

      // B's own activation applies first, as it would in practice.
      callbackFromB(0);
      const container = scrollContainer(el);
      expect(container.scrollTop).toBe(bAnchorTarget);
      const roFromB = StubResizeObserver.instances.at(-1);
      expect(roFromB).toBeDefined();
      expect(roFromB?.disconnected).toBe(false);
      const instanceCountAfterB = StubResizeObserver.instances.length;

      // Restore A's own divider geometry right before firing the stale rAF
      // callback the browser still had queued for A: if the token didn't
      // gate `applyUnreadAnchor`'s current-DOM read, this would move B's
      // scroll position to A's target — a value distinguishable from B's —
      // and would also disconnect B's real ResizeObserver and replace it
      // with one bound to A's stale token.
      dividerOffsetTop = A_DIVIDER_OFFSET;
      staleCallbackFromA(0);
      expect(container.scrollTop).toBe(bAnchorTarget);
      expect(container.scrollTop).not.toBe(aAnchorTarget);
      expect(roFromB?.disconnected).toBe(false);
      expect(StubResizeObserver.instances.length).toBe(instanceCountAfterB);

      // A genuine resize for B still re-anchors correctly afterwards — the
      // token checks didn't leave B's own machinery broken.
      dividerOffsetTop = B_DIVIDER_OFFSET - 30;
      roFromB?.trigger();
      expect(container.scrollTop).toBe(B_DIVIDER_OFFSET - 30 - ANCHOR_MARGIN_PX);
    } finally {
      globalThis.requestAnimationFrame = originalRaf;
    }
  });

  /**
   * Nit (review of nc-open-at-unread): a genuine user scroll landing in the
   * same frame as `applyUnreadAnchor`'s own scrollTop write was swallowed by
   * the `_applyingUnreadAnchor` guard, since the guard only clears on the
   * next rAF. Comparing the observed scrollTop against the value the anchor
   * itself just wrote recognizes this case too.
   */
  it('recognizes a genuine user scroll landing in the same frame as the anchor write', async () => {
    mockEndpoints('m1');
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;
    const el = mountThread();

    await vi.waitFor(() => expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull());
    const container = scrollContainer(el);
    await vi.waitFor(() => expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX));
    const ro = StubResizeObserver.instances.at(-1);

    // Simulate a real user scroll landing in the same frame as a re-apply:
    // the write itself sets `_applyingUnreadAnchor`, but the scrollTop the
    // 'scroll' event reports disagrees with what was just written.
    dividerOffsetTop = DIVIDER_OFFSET_TOP + 50;
    ro?.trigger();
    container.scrollTop = 5;
    container.dispatchEvent(new Event('scroll'));

    // The anchor must now be inactive: a further resize does not re-apply.
    dividerOffsetTop = DIVIDER_OFFSET_TOP + 100;
    ro?.trigger();
    expect(container.scrollTop).toBe(5);
  });

  /**
   * Gemini review of PR #1932 (round 4, comment 2): `applyUnreadAnchor`
   * scheduled a guard-clearing rAF on every call without canceling a
   * previous one still pending. Two rapid layout updates (e.g. ResizeObserver
   * firing twice in one frame window) could then leave an earlier rAF alive
   * to clear `_applyingUnreadAnchor` while a later write's own 'scroll'
   * event was still pending — misreading that programmatic scroll as the
   * user taking over and wrongly deactivating the anchor.
   */
  it('two rapid anchor applications: the earlier rAF does not clear the guard early, so a programmatic scroll from the second is not treated as a user scroll', async () => {
    mockEndpoints('m1');
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;

    const { callbacks: rafCallbacks, restore } = installRafStub();

    try {
      const el = mountThread();
      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      // Settle the initial open (the deferred initial-anchor rAF, then its
      // own guard rAF) before driving the rapid resizes under test.
      while (rafCallbacks.size) {
        const [handle, cb] = [...rafCallbacks.entries()][0];
        rafCallbacks.delete(handle);
        cb(0);
      }
      const container = scrollContainer(el);
      expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP - ANCHOR_MARGIN_PX);
      const ro = StubResizeObserver.instances.at(-1);
      expect(ro).toBeDefined();

      // Write A: schedules a guard-clearing rAF, not yet fired.
      dividerOffsetTop = DIVIDER_OFFSET_TOP + 10;
      ro?.trigger();
      expect(rafCallbacks.size).toBe(1);
      const [handleA] = [...rafCallbacks.keys()];

      // Write B lands before write A's guard rAF has fired — ResizeObserver
      // firing twice in one frame window.
      dividerOffsetTop = DIVIDER_OFFSET_TOP + 20;
      ro?.trigger();
      const writtenB = container.scrollTop;
      expect(writtenB).toBe(DIVIDER_OFFSET_TOP + 20 - ANCHOR_MARGIN_PX);

      // Exactly one guard rAF must survive — write A's must have been
      // canceled, not left pending alongside write B's.
      expect(rafCallbacks.size).toBe(1);
      const [handleB] = [...rafCallbacks.keys()];
      expect(handleB).not.toBe(handleA);

      // Simulate the browser: it would only fire what wasn't canceled. If A's
      // rAF is still (wrongly) in the map, firing it here reproduces exactly
      // the race Gemini flagged.
      if (rafCallbacks.has(handleA)) {
        rafCallbacks.get(handleA)!(0);
        rafCallbacks.delete(handleA);
      }

      // The 'scroll' event resulting from write B lands now, before write
      // B's own guard rAF has fired.
      container.dispatchEvent(new Event('scroll'));

      // If A's stale rAF had wrongly cleared the guard, this would have been
      // misread as a user scroll and deactivated the anchor. Confirm it is
      // still active: a further resize still re-anchors.
      dividerOffsetTop = DIVIDER_OFFSET_TOP + 30;
      ro?.trigger();
      expect(container.scrollTop).toBe(DIVIDER_OFFSET_TOP + 30 - ANCHOR_MARGIN_PX);
    } finally {
      restore();
    }
  });

  /**
   * Gemini review of PR #1932 (round 4, comment 3): `deactivateUnreadAnchor`
   * tore down the resize observer and timer but left both the deferred
   * initial-anchor rAF (`scrollToUnreadDivider`) and the guard-clearing rAF
   * (`applyUnreadAnchor`) pending, and never reset `_applyingUnreadAnchor`.
   * A still-pending rAF could fire later against a torn-down or superseded
   * anchor, and a stuck `true` guard would leak into the next open.
   */
  it('deactivateUnreadAnchor cancels pending rAFs and resets the guard: no callback runs afterwards, and the guard is false', async () => {
    mockEndpoints('m1');
    globalThis.ResizeObserver = StubResizeObserver as unknown as typeof ResizeObserver;

    const { callbacks: rafCallbacks, cancelSpy, restore } = installRafStub();

    type Internal = {
      _applyingUnreadAnchor: boolean;
      applyUnreadAnchor(): void;
      deactivateUnreadAnchor(): void;
    };

    try {
      const el = mountThread();
      await vi.waitFor(() =>
        expect(el.shadowRoot?.querySelector('.unread-divider')).not.toBeNull()
      );
      const internal = el as unknown as Internal;

      // The deferred initial-anchor rAF is pending — deliberately never
      // allowed to fire.
      expect(rafCallbacks.size).toBe(1);
      const [initialRafHandle] = [...rafCallbacks.keys()];

      // Tearing down the anchor before that first frame lands must cancel
      // it, not let it fire later against the torn-down anchor.
      internal.deactivateUnreadAnchor();

      expect(cancelSpy).toHaveBeenCalledWith(initialRafHandle);
      expect(rafCallbacks.has(initialRafHandle)).toBe(false);

      cancelSpy.mockClear();

      // Now exercise the guard-clearing rAF directly: a fresh programmatic
      // write leaves the guard true and a rAF pending to clear it.
      internal.applyUnreadAnchor();
      expect(internal._applyingUnreadAnchor).toBe(true);
      expect(rafCallbacks.size).toBe(1);
      const [guardRafHandle] = [...rafCallbacks.keys()];

      // Deactivating again while that guard rAF is still pending.
      internal.deactivateUnreadAnchor();

      expect(cancelSpy).toHaveBeenCalledWith(guardRafHandle);
      // Nothing is left pending to run afterwards.
      expect(rafCallbacks.size).toBe(0);
      // Reset immediately — not left waiting on the now-canceled rAF.
      expect(internal._applyingUnreadAnchor).toBe(false);
    } finally {
      restore();
    }
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
 * Jump-to-message paths (search-jump, reply-jump, deep link) all funnel
 * through scrollToMessageById()'s single smooth `scrollIntoView`. A layout
 * shift after the scroll starts — an image loading, a late markdown render,
 * an attachment's height resolving — can leave the target off screen, since
 * Chromium's smooth scroll targets the position computed when it started
 * (pre-existing, found in review of #1749). The fix re-checks once the
 * scroll settles (via `scrollend`, or a poll fallback where that event is
 * unsupported) and corrects if needed, capped so it can never loop, and
 * cancellable by a manual scroll, another jump, or a thread switch.
 */
describe('scion-chat-thread jump-to-message scrollend re-check', () => {
  const TARGET = {
    id: 'jump-target',
    sender: 'them@example.com',
    msg: 'jump target',
    createdAt: '2026-01-01T00:01:00Z',
  };

  // happy-dom performs no layout: scrollHeight/clientHeight report zero
  // unless overridden, which would make every scroll position look "clamped"
  // (see isJumpScrollClamped). Fix them so scrollTop alone determines
  // top/bottom clamping in the tests that care about it.
  const SCROLL_HEIGHT = 1000;
  const CLIENT_HEIGHT = 300;
  const originalScrollHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'scrollHeight'
  );
  const originalClientHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'clientHeight'
  );

  let scrollIntoViewCalls: Array<{ el: Element; opts: ScrollIntoViewOptions }>;
  const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;

  let rects: WeakMap<Element, DOMRect>;
  const originalGetBoundingClientRect = HTMLElement.prototype.getBoundingClientRect;

  /** Give an element a fixed bounding rect for the "in view" check. happy-dom
   *  performs no layout, so every element reports an all-zero rect unless the
   *  test supplies one. */
  function setRect(el: Element, rect: { top: number; bottom: number }): void {
    rects.set(el, {
      top: rect.top,
      bottom: rect.bottom,
      left: 0,
      right: 0,
      width: 0,
      height: rect.bottom - rect.top,
      x: 0,
      y: rect.top,
      toJSON: () => ({}),
    } as DOMRect);
  }

  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ items: [TARGET] }),
    } as unknown as Response);

    scrollIntoViewCalls = [];
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: function (this: Element, opts: ScrollIntoViewOptions) {
        scrollIntoViewCalls.push({ el: this, opts });
      },
    });

    rects = new WeakMap();
    Object.defineProperty(HTMLElement.prototype, 'getBoundingClientRect', {
      configurable: true,
      value: function (this: Element) {
        return rects.get(this) ?? originalGetBoundingClientRect.call(this);
      },
    });

    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get: () => SCROLL_HEIGHT,
    });
    Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
      configurable: true,
      get: () => CLIENT_HEIGHT,
    });
  });

  afterEach(() => {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: originalScrollIntoView,
    });
    Object.defineProperty(HTMLElement.prototype, 'getBoundingClientRect', {
      configurable: true,
      value: originalGetBoundingClientRect,
    });
    for (const [prop, descriptor] of [
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
    vi.useRealTimers();
  });

  /**
   * Mount (TARGET is already in the seeded history, so no `around` fetch is
   * needed) and position the target far above the viewport with the
   * container mid-thread (`scrollTop` away from either scroll extreme), so
   * the pre-scroll check in `scrollToMessageById` sees a real, unclamped
   * scroll and arms the watch — the common case most of these tests exercise.
   * Then simulate the scroll having landed the target in view, and clear the
   * jump's own `scrollIntoView` call so each test only sees calls made by the
   * re-check itself.
   */
  async function mountAndJump(): Promise<{
    el: ScionChatThread;
    scrollEl: HTMLElement;
    targetEl: HTMLElement;
  }> {
    const el = await mount();
    expect(
      el.shadowRoot?.getElementById(`msg-${TARGET.id}`),
      'TARGET must already be loaded from the seeded history'
    ).not.toBeNull();

    const scrollEl = el.shadowRoot!.querySelector('.messages-scroll') as HTMLElement;
    const targetEl = el.shadowRoot!.getElementById(`msg-${TARGET.id}`) as HTMLElement;
    setRect(scrollEl, { top: 0, bottom: 300 });
    setRect(targetEl, { top: -900, bottom: -850 }); // far above the viewport
    scrollEl.scrollTop = 500; // mid-thread: not clamped at either end

    await el.scrollToMessageById(TARGET.id);
    await el.updateComplete;

    setRect(targetEl, { top: 100, bottom: 150 }); // settled — fully inside, "in view"
    scrollIntoViewCalls = [];
    return { el, scrollEl, targetEl };
  }

  it('re-checks on scrollend and corrects when a layout shift moved the target out of view', async () => {
    const { scrollEl, targetEl } = await mountAndJump();

    // A large layout shift (e.g. an image finishing load above the target)
    // has pushed it below the visible area by the time the scroll settles.
    setRect(targetEl, { top: 500, bottom: 550 });

    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([
      { el: targetEl, opts: { behavior: 'auto', block: 'center' } },
    ]);
  });

  it('does not re-scroll when the target is already in view on scrollend', async () => {
    const { scrollEl } = await mountAndJump();

    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
  });

  it('treats a target taller than the viewport as in view once it spans the midpoint, avoiding a wasted correction (R2)', async () => {
    const { scrollEl, targetEl } = await mountAndJump();

    // A long agent reply: taller than the 300px container, so it can never
    // be fully contained, but `block: 'center'` has it straddling the
    // container's midpoint (150) — exactly where centering aims for.
    setRect(targetEl, { top: -200, bottom: 400 });

    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
  });

  it('cancels the pending re-check when the user scrolls manually (wheel)', async () => {
    const { scrollEl, targetEl } = await mountAndJump();
    setRect(targetEl, { top: 500, bottom: 550 });

    // A manual wheel scroll arrives before the browser reports settle.
    scrollEl.dispatchEvent(new Event('wheel'));
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
  });

  it('cancels the pending re-check on a scrollbar-drag/middle-click pointerdown (R1)', async () => {
    const { scrollEl, targetEl } = await mountAndJump();
    setRect(targetEl, { top: 500, bottom: 550 });

    // Scrollbar drags and middle-click autoscroll dispatch no wheel, touch,
    // or key events — only pointerdown.
    scrollEl.dispatchEvent(new Event('pointerdown'));
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
  });

  it('cancels the pending re-check on a document-level keydown even when focus is outside the scroll container (R1)', async () => {
    const { scrollEl, targetEl } = await mountAndJump();
    setRect(targetEl, { top: 500, bottom: 550 });

    // The common state right after clicking a message: activeElement is
    // <body>, so the keydown never reaches scrollEl, yet Chromium still
    // scrolls the last-clicked scroller on PageUp.
    document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'PageUp', bubbles: true }));
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
  });

  it('does not cancel the pending re-check for keydowns unrelated to scrolling', async () => {
    const { scrollEl, targetEl } = await mountAndJump();
    setRect(targetEl, { top: 500, bottom: 550 });

    // Typing (e.g. in a reply box) must not be treated as a manual scroll.
    document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'a', bubbles: true }));
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([
      { el: targetEl, opts: { behavior: 'auto', block: 'center' } },
    ]);
  });

  it('falls back to a settle poll when scrollend is unsupported', async () => {
    const { el, scrollEl, targetEl } = await mountAndJump();
    vi.spyOn(
      el as unknown as { supportsScrollEndEvent: () => boolean },
      'supportsScrollEndEvent'
    ).mockReturnValue(false);

    // Put the target back off-screen so the re-issued jump below sees a real
    // scroll and arms a fresh watch via the now-stubbed fallback path.
    setRect(targetEl, { top: -900, bottom: -850 });
    scrollEl.scrollTop = 500;

    vi.useFakeTimers();
    try {
      // Re-issue the jump so the newly stubbed fallback path is picked up.
      await el.scrollToMessageById(TARGET.id);
      setRect(targetEl, { top: 500, bottom: 550 });
      scrollIntoViewCalls = [];

      // Poll interval is 50ms; settle requires JUMP_SCROLL_SETTLE_STABLE_MS
      // (150ms) of no scrollTop movement, which happy-dom's static scrollTop
      // satisfies immediately. Advance just past the first settle (150ms) but
      // well short of the second poll cycle's ~150ms-later correction (that
      // repeated-correction behavior is covered by the recheck-cap test).
      await vi.advanceTimersByTimeAsync(200);

      expect(scrollIntoViewCalls).toEqual([
        { el: targetEl, opts: { behavior: 'auto', block: 'center' } },
      ]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('caps corrective re-scrolls so a target that never settles cannot loop forever', async () => {
    const { scrollEl, targetEl } = await mountAndJump();
    // Never let the target land in view — every scrollend still sees it off screen.
    setRect(targetEl, { top: 500, bottom: 550 });

    scrollEl.dispatchEvent(new Event('scrollend'));
    scrollEl.dispatchEvent(new Event('scrollend'));
    scrollEl.dispatchEvent(new Event('scrollend'));
    scrollEl.dispatchEvent(new Event('scrollend'));

    // Capped at 2 re-checks, however many scrollends fire afterward.
    expect(scrollIntoViewCalls.length).toBe(2);
  });

  it('cleans up after the idle timeout when there is no scroll activity and scrollend never fires (R1)', async () => {
    const { el, scrollEl, targetEl } = await mountAndJump();
    // Re-arm from a clean, off-screen state so the idle timeout below is
    // timed from this jump, not from mountAndJump's initial one.
    setRect(targetEl, { top: -900, bottom: -850 });
    scrollEl.scrollTop = 500;

    vi.useFakeTimers();
    try {
      await el.scrollToMessageById(TARGET.id);
      setRect(targetEl, { top: 500, bottom: 550 }); // stays off-screen throughout
      scrollIntoViewCalls = [];

      // No `scroll` event and no `scrollend` arrive: the idle timeout
      // (300ms) fires with no activity at all to have restarted it.
      await vi.advanceTimersByTimeAsync(300);

      // A stray scrollend afterward — e.g. from the user's own subsequent
      // scroll — must find nothing armed.
      scrollEl.dispatchEvent(new Event('scrollend'));

      expect(scrollIntoViewCalls).toEqual([]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('still corrects a settle that takes longer than the old fixed 1500ms deadline, as long as scroll events keep arriving (R4)', async () => {
    const { el, scrollEl, targetEl } = await mountAndJump();
    // Re-arm from a clean, off-screen state so this jump's own watch is what
    // gets timed, not mountAndJump's initial one.
    setRect(targetEl, { top: -900, bottom: -850 });
    scrollEl.scrollTop = 500;

    vi.useFakeTimers();
    try {
      await el.scrollToMessageById(TARGET.id);
      scrollIntoViewCalls = [];

      // A long smooth scroll fires `scroll` every frame right up until
      // `scrollend`. This runs past 1500ms — the fixed deadline from arm
      // time that review R4 found Chromium's smooth scroll can exceed on
      // jumps beyond ~8000px (measured ~1520ms) — each `scroll` restarting
      // the idle timeout so the watch is still armed when it settles.
      for (let elapsed = 0; elapsed < 1600; elapsed += 100) {
        scrollEl.dispatchEvent(new Event('scroll'));
        await vi.advanceTimersByTimeAsync(100);
      }

      // The layout shift and settle land after the old fixed deadline would
      // already have torn the watch down.
      setRect(targetEl, { top: 500, bottom: 550 });
      scrollEl.dispatchEvent(new Event('scrollend'));

      expect(scrollIntoViewCalls).toEqual([
        { el: targetEl, opts: { behavior: 'auto', block: 'center' } },
      ]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('tears the watch down at the hard cap even while scroll events keep arriving continuously (O2)', async () => {
    const { el, scrollEl, targetEl } = await mountAndJump();
    // Re-arm from a clean, off-screen state so this jump's own watch is what
    // gets timed against the hard cap, not mountAndJump's initial one.
    setRect(targetEl, { top: -900, bottom: -850 });
    scrollEl.scrollTop = 500;

    const jumpScrollCleanup = (): (() => void) | null =>
      (el as unknown as { _jumpScrollCleanup: (() => void) | null })._jumpScrollCleanup;

    vi.useFakeTimers();
    try {
      await el.scrollToMessageById(TARGET.id);
      setRect(targetEl, { top: 500, bottom: 550 }); // never settles
      scrollIntoViewCalls = [];

      // Continuous `scroll` activity restarts the 300ms idle timeout on its
      // own forever — a scroller that never goes idle would otherwise stay
      // watched indefinitely — but the hard cap (JUMP_SCROLL_HARD_CAP_MS,
      // 5000ms) is not reset by `scroll` events, so it must still fire.
      for (let elapsed = 0; elapsed < 5100; elapsed += 100) {
        scrollEl.dispatchEvent(new Event('scroll'));
        await vi.advanceTimersByTimeAsync(100);
      }

      expect(jumpScrollCleanup()).toBeNull();

      // A stray scrollend after the cap has fired must find nothing armed.
      scrollEl.dispatchEvent(new Event('scrollend'));

      expect(scrollIntoViewCalls).toEqual([]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not cancel the pending re-check for a scroll-shaped key typed into an editable field (N3)', async () => {
    const { scrollEl, targetEl } = await mountAndJump();
    setRect(targetEl, { top: 500, bottom: 550 });

    // Space and the arrow keys are ordinary typing in the composer (or any
    // other editable field) — even though they're in JUMP_SCROLL_CANCEL_KEYS
    // for the document-wide case that catches real scroll input outside the
    // scroll container. In real Chromium, Shoelace's `<sl-textarea>` wraps its
    // native `<textarea>` in shadow DOM, so a document-level listener's
    // `e.target` is retargeted to the `<sl-textarea>` host — a non-editable
    // element — while `e.composedPath()[0]` is still the real textarea
    // (review N3). happy-dom does not implement that retargeting: `.target`
    // is fixed to whichever node `dispatchEvent()` was called on, and
    // `composedPath()` is derived from that same un-retargeted `.target`
    // (see Event.js), so even a real shadow-DOM textarea reports the same
    // element for both — a light-DOM textarea can't tell them apart either,
    // for the same underlying reason. Faking `composedPath()` on the event is
    // the only way to reproduce the divergence in this test environment, so
    // the test still exercises which one the handler actually consults.
    const textarea = document.createElement('textarea'); // never attached — target is faked below
    const event = new KeyboardEvent('keydown', { key: ' ', bubbles: true });
    Object.defineProperty(event, 'composedPath', {
      value: () => [textarea, document.body, document],
    });
    // Dispatched from document.body, so `e.target` is BODY — non-editable,
    // standing in for the retargeted `<sl-textarea>` host — while the faked
    // `composedPath()[0]` above is the real, editable textarea.
    document.body.dispatchEvent(event);
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([
      { el: targetEl, opts: { behavior: 'auto', block: 'center' } },
    ]);
  });

  it('does not arm a re-check when the target is already centred before the jump (R1)', async () => {
    const el = await mount();
    const scrollEl = el.shadowRoot!.querySelector('.messages-scroll') as HTMLElement;
    const targetEl = el.shadowRoot!.getElementById(`msg-${TARGET.id}`) as HTMLElement;
    setRect(scrollEl, { top: 0, bottom: 300 });
    setRect(targetEl, { top: 100, bottom: 150 }); // already centred
    scrollEl.scrollTop = 500;

    await el.scrollToMessageById(TARGET.id);
    await el.updateComplete;
    scrollIntoViewCalls = [];

    // If the watcher had been armed anyway, this later shift-and-scrollend
    // would "correct" a jump that never needed one.
    setRect(targetEl, { top: 500, bottom: 550 });
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
  });

  it('does not arm a re-check when centering is clamped at the end of the thread (R1)', async () => {
    const el = await mount();
    const scrollEl = el.shadowRoot!.querySelector('.messages-scroll') as HTMLElement;
    const targetEl = el.shadowRoot!.getElementById(`msg-${TARGET.id}`) as HTMLElement;
    setRect(scrollEl, { top: 0, bottom: 300 });
    setRect(targetEl, { top: 320, bottom: 370 }); // just below the viewport
    // Already pinned to the bottom (SCROLL_HEIGHT - CLIENT_HEIGHT): centering
    // this target would need to scroll further down, which is clamped — the
    // common reply-jump-while-pinned-to-bottom case (review R1).
    scrollEl.scrollTop = SCROLL_HEIGHT - CLIENT_HEIGHT;

    await el.scrollToMessageById(TARGET.id);
    await el.updateComplete;
    scrollIntoViewCalls = [];

    setRect(targetEl, { top: 500, bottom: 550 });
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
  });

  it("never undoes the user's first manual scroll after a jump that produced no scroll (reviewer repro, R1)", async () => {
    const el = await mount();
    const scrollEl = el.shadowRoot!.querySelector('.messages-scroll') as HTMLElement;
    const targetEl = el.shadowRoot!.getElementById(`msg-${TARGET.id}`) as HTMLElement;
    setRect(scrollEl, { top: 0, bottom: 300 });
    setRect(targetEl, { top: 320, bottom: 370 }); // off screen, but clamped — see above
    scrollEl.scrollTop = SCROLL_HEIGHT - CLIENT_HEIGHT;

    await el.scrollToMessageById(TARGET.id);
    await el.updateComplete;
    scrollIntoViewCalls = [];

    // The user's next scroll is real and deliberate — a scrollbar drag or a
    // PageUp with focus elsewhere — and must never be undone.
    scrollEl.scrollTop = 200;
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
    expect(scrollEl.scrollTop).toBe(200);
  });

  it('tears down the pending re-check on thread switch (R3)', async () => {
    const { el, scrollEl, targetEl } = await mountAndJump();

    const jumpScrollCleanup = (): (() => void) | null =>
      (el as unknown as { _jumpScrollCleanup: (() => void) | null })._jumpScrollCleanup;
    expect(
      jumpScrollCleanup(),
      'the mounted jump must have armed a pending watch for this test to be meaningful'
    ).not.toBeNull();

    // Switching conversations runs resetV2State() (see `updated()`'s
    // conversationKey handling), which must cancel the pending watch — it
    // belongs to the thread being left, and a late correction must not fire
    // against the thread being switched to. Called directly (rather than by
    // assigning conversationKey and awaiting the reload) to isolate this
    // teardown from `scrollToBottom()`'s own, separate cancel on the new
    // thread's initial-load scroll — dispatching scrollend after a real
    // thread switch made the previous version of this test pass even with
    // resetV2State's cancel removed, because that second cancel path also
    // runs and masked the mutation (review R3 was about exactly this kind of
    // false pass, from a different cause).
    (el as unknown as { resetV2State: () => void }).resetV2State();

    expect(jumpScrollCleanup()).toBeNull();

    // And the cancellation must actually be effective: a scrollend that
    // arrives afterward must not trigger a correction.
    setRect(targetEl, { top: 500, bottom: 550 });
    scrollIntoViewCalls = [];
    scrollEl.dispatchEvent(new Event('scrollend'));

    expect(scrollIntoViewCalls).toEqual([]);
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

  // Review round 3, R3-3: the thread's DisplayZoneController re-renders the
  // date divider when the preference changes after mount — pin it, since
  // deleting the controller left every other test in this suite green.
  it('re-renders the date divider zone label after a mounted thread outlives a preference change', async () => {
    try {
      const el = await mountAgentDM({
        interagent: [makeIaMessage({ id: 'ia-1', createdAt: '2026-09-23T15:00:00Z' })],
      });

      const dividerBefore = el.shadowRoot!.querySelector('.date-divider');
      expect(dividerBefore?.textContent).toContain('UTC'); // Auto, pinned ambient zone

      setPreferredTimeZone('Asia/Tokyo');
      await el.updateComplete;

      const dividerAfter = el.shadowRoot!.querySelector('.date-divider');
      expect(dividerAfter?.textContent).toContain('Asia/Tokyo');
      expect(dividerAfter?.textContent).not.toContain('UTC');
    } finally {
      setPreferredTimeZone('');
    }
  });
});

describe('scion-chat-thread path-link project context fallback', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    fakeStateManager.clearAgents();
  });

  /** Minimal Message fixture; override fields per-test. */
  function makeMessage(overrides: Partial<Message> = {}): Message {
    return {
      id: 'm1',
      projectId: '',
      sender: 'agent:fix-filebrowser-symlink-lead',
      senderId: 'agent-1',
      recipient: '',
      recipientId: '',
      msg: 'Note: /scion-volumes/scratchpad/projects/visibility-removal/scoping.md',
      type: 'chat',
      agentId: '',
      createdAt: '2026-01-01T00:00:00Z',
      ...overrides,
    };
  }

  type Internals = {
    sendError: string | null;
    // Loading/error/downloadUrl now live inside the reusable
    // <scion-chat-file-preview> (see chat-file-preview.test.ts); this
    // component only resolves which path to preview.
    filePreview: { containerPath: string; projectId: string } | null;
    handlePathLinkClick(e: CustomEvent<{ path: string }>, msg?: Message): void;
  };

  it('shows a clear error for a cold-load DM with no thread projectId and no message fallback', async () => {
    const el = await mount();
    // A cold load straight into a DM has no inherited project to fall back to.
    el.isDM = true;
    el.projectId = '';
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md' },
      }),
      makeMessage({ projectId: '' })
    );

    expect(internals.sendError).toBe(
      'Cannot open file: could not determine which project this file belongs to'
    );
    expect(internals.filePreview).toBeNull();
  });

  it('a DM with an inherited unrelated project uses the message project, not the thread project', async () => {
    const el = await mount();
    el.isDM = true;
    // The thread project here is only `inheritedProjectId()` — whatever
    // project the user was last viewing before opening this DM, unrelated
    // to the DM itself.
    el.projectId = 'proj-inherited';
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'scoping notes', size: 14 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md' },
      }),
      makeMessage({ senderProjectId: 'proj-visibility-removal' })
    );

    expect(internals.sendError).toBeNull();
    expect(internals.filePreview?.projectId).toBe('proj-visibility-removal');
    expect(internals.filePreview?.containerPath).toBe(
      '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md'
    );
  });

  it('a DM with an inherited unrelated project uses the message projectId when senderProjectId is absent', async () => {
    const el = await mount();
    el.isDM = true;
    // Same inherited-project setup as above, but the message only carries
    // `projectId` (no `senderProjectId`), as on the agent->user outbound
    // path: handleAgentOutboundMessage stamps ProjectID but never sets
    // SenderProjectID.
    el.projectId = 'proj-inherited';
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'scoping notes', size: 14 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md' },
      }),
      makeMessage({ projectId: 'proj-visibility-removal' })
    );

    expect(internals.sendError).toBeNull();
    expect(internals.filePreview?.projectId).toBe('proj-visibility-removal');
    expect(internals.filePreview?.containerPath).toBe(
      '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md'
    );
  });

  it('a cold-load DM with an empty project uses the message senderProjectId', async () => {
    const el = await mount();
    el.isDM = true;
    el.projectId = '';
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'scoping notes', size: 14 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md' },
      }),
      makeMessage({ senderProjectId: 'proj-visibility-removal' })
    );

    expect(internals.sendError).toBeNull();
    expect(internals.filePreview?.projectId).toBe('proj-visibility-removal');
    expect(internals.filePreview?.containerPath).toBe(
      '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md'
    );
  });

  it('the agent-to-user outbound path (only msg.projectId, no senderProjectId) resolves', async () => {
    const el = await mount();
    el.isDM = true;
    el.projectId = '';
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'notes', size: 5 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    // handleAgentOutboundMessage stamps ProjectID on the message but never
    // sets SenderProjectID, so this is the real shape of an agent DM.
    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/notes.md' },
      }),
      makeMessage({ projectId: 'proj-fallback' })
    );

    expect(internals.sendError).toBeNull();
    expect(internals.filePreview?.projectId).toBe('proj-fallback');
    expect(internals.filePreview?.containerPath).toBe('/scion-volumes/scratchpad/notes.md');
  });

  it('prefers the thread-level projectId for a project-scoped (non-DM) thread', async () => {
    const el = await mount();
    el.isDM = false;
    el.projectId = 'proj-thread';
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'notes', size: 5 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/notes.md' },
      }),
      makeMessage({ senderProjectId: 'proj-sender' })
    );

    expect(internals.filePreview?.projectId).toBe('proj-thread');
    expect(internals.filePreview?.containerPath).toBe('/scion-volumes/scratchpad/notes.md');
  });

  it('keeps the "unrecognized path format" behavior for a resolvable project', async () => {
    const el = await mount();
    el.isDM = true;
    el.projectId = '';
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', { detail: { path: 'not-a-path' } }),
      makeMessage({ senderProjectId: 'proj-visibility-removal' })
    );

    expect(internals.sendError).toBe('Cannot open file: unrecognized path format');
  });

  it('a DM with an inherited unrelated project and no message project shows the error, not the inherited project', async () => {
    const el = await mount();
    el.isDM = true;
    // The thread project is only `inheritedProjectId()` — whatever project
    // the user was last viewing before opening this DM — and this is a
    // human-to-human DM (conversationKey does not start with "dm:agent:"),
    // so there is no peer-agent fallback either.
    el.projectId = 'proj-inherited';
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'scoping notes', size: 14 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/projects/visibility-removal/scoping.md' },
      }),
      makeMessage({ projectId: '' })
    );

    expect(internals.sendError).toBe(
      'Cannot open file: could not determine which project this file belongs to'
    );
    expect(internals.filePreview).toBeNull();
    // Must never have resolved (or fetched) against the unrelated inherited project.
    expect(apiFetch.mock.calls.some((call) => String(call[0]).includes('/proj-inherited/'))).toBe(
      false
    );
  });

  it('falls back to the DM peer agent project when the message carries no project of its own', async () => {
    // Simulates clicking a path link on the client's own just-sent optimistic
    // message (chat-thread sets `optimisticMsg.projectId = ''` until the
    // server-echoed version replaces it), in an agent DM.
    fakeStateManager.setAgent('coder', 'proj-peer-agent');
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    apiFetch.mockImplementation(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ items: [] }),
      } as unknown as Response)
    );
    el.conversationKey = 'dm:agent:coder:user:u1';
    el.isDM = true;
    // An unrelated inherited project must not win, nor be needed.
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'scoping notes', size: 14 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/notes.md' },
      }),
      makeMessage({ projectId: '' })
    );

    expect(internals.sendError).toBeNull();
    expect(internals.filePreview?.projectId).toBe('proj-peer-agent');
    expect(internals.filePreview?.containerPath).toBe('/scion-volumes/scratchpad/notes.md');
  });

  it('uses the message project over the cached peer-agent project when both are available', async () => {
    // Precedence: the message's own project always wins over the DM peer's
    // cached project (`resolvePathLinkProjectId`: `fromMsg || peer`, never the
    // other order). A mutant that checks the peer first would pass this test
    // with 'proj-peer', not 'proj-msg'.
    fakeStateManager.setAgent('coder', 'proj-peer');
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    apiFetch.mockImplementation(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ items: [] }),
      } as unknown as Response)
    );
    el.conversationKey = 'dm:agent:coder:user:u1';
    el.isDM = true;
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ content: 'scoping notes', size: 14 }),
    } as unknown as Response);
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/notes.md' },
      }),
      makeMessage({ senderProjectId: 'proj-msg' })
    );

    expect(internals.sendError).toBeNull();
    expect(internals.filePreview?.projectId).toBe('proj-msg');
    expect(internals.filePreview?.containerPath).toBe('/scion-volumes/scratchpad/notes.md');
  });

  it('does not use the peer-agent fallback when the peer agent is not in the local cache', async () => {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    apiFetch.mockImplementation(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ items: [] }),
      } as unknown as Response)
    );
    el.conversationKey = 'dm:agent:unknown-agent:user:u1';
    el.isDM = true;
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    const internals = el as unknown as Internals;

    await internals.handlePathLinkClick(
      new CustomEvent('path-link-click', {
        detail: { path: '/scion-volumes/scratchpad/notes.md' },
      }),
      makeMessage({ projectId: '' })
    );

    expect(internals.sendError).toBe(
      'Cannot open file: could not determine which project this file belongs to'
    );
    expect(internals.filePreview).toBeNull();
  });
});

// O1 (p2a-r3 review, A25.3): deliveryStateFor's "stays visible on every
// message, like failed" rule was added for 'deferred' (design
// agent-reincarnate §3.7, F5) but had no test — mutation W1 (dropping the
// deferred clause) left all 298 chat tests green. Pins both 'deferred' and
// 'failed' directly against the private deliveryStateFor method, since it
// is otherwise pure given seenExpired=false (isMessageSeen is never
// consulted in that branch).
describe('scion-chat-thread deliveryStateFor visibility (O1, p2a-r3 review)', () => {
  afterEach(() => {
    document.body.innerHTML = '';
  });

  type DeliveryStateInternals = {
    deliveryStateFor(msg: Message, lastOwnMessageId: string, seenExpired: boolean): string;
  };

  function makeDeliveryMessage(dispatchState: string): Message {
    return {
      id: 'older-message',
      projectId: '',
      sender: 'user:me@example.com',
      senderId: 'user-me',
      recipient: 'agent:coder',
      recipientId: 'agent-1',
      msg: 'an earlier message',
      type: 'chat',
      agentId: 'agent-1',
      createdAt: '2026-01-01T00:00:00Z',
      dispatchState,
    };
  }

  it.each(['deferred', 'failed'])(
    'keeps dispatchState=%s visible even when it is not the last own message',
    async (dispatchState) => {
      const el = document.createElement('scion-chat-thread') as ScionChatThread;
      document.body.appendChild(el);
      await el.updateComplete;
      const internals = el as unknown as DeliveryStateInternals;

      const older = makeDeliveryMessage(dispatchState);

      // Not the last own message, and seen-expiry has already passed:
      // every other dispatchState value would be hidden here (the whole
      // point of deferred/failed being an exception to "only show on the
      // most recent own message").
      expect(internals.deliveryStateFor(older, 'a-newer-message-id', true)).toBe(dispatchState);
      // Also visible before seen-expiry, and regardless of lastOwnMessageId.
      expect(internals.deliveryStateFor(older, 'a-newer-message-id', false)).toBe(dispatchState);
    }
  );

  // no_recipient is shown like dispatched: only on the newest own message.
  it('shows no_recipient only on the newest own message', () => {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    document.body.appendChild(el);
    const internals = el as unknown as DeliveryStateInternals;
    const msg = makeDeliveryMessage('no_recipient');
    expect(internals.deliveryStateFor(msg, msg.id, false)).toBe('no_recipient');
    expect(internals.deliveryStateFor(msg, 'a-newer-message-id', false)).toBe('');
    expect(internals.deliveryStateFor(msg, 'a-newer-message-id', true)).toBe('');
  });

  it('hides an ordinary dispatched state once it is no longer the last own message', () => {
    // Control: proves the test above is actually exercising the
    // deferred/failed exception, not a bug that shows every dispatchState
    // unconditionally.
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    document.body.appendChild(el);
    const internals = el as unknown as DeliveryStateInternals;
    const older = makeDeliveryMessage('dispatched');
    expect(internals.deliveryStateFor(older, 'a-newer-message-id', false)).toBe('');
  });
});

// ---------------------------------------------------------------------------
// Recent-files capture hooks
// ---------------------------------------------------------------------------

describe('scion-chat-thread recent-files capture', () => {
  let ingestSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
    ingestSpy = vi.spyOn(chatRecentFiles, 'ingest').mockImplementation(() => {});
  });

  afterEach(() => {
    ingestSpy.mockRestore();
    document.body.innerHTML = '';
  });

  it('captures an accepted own-send provisionally, keyed by the server id, never the optimistic id', async () => {
    const el = await mount();
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 201,
        json: () =>
          Promise.resolve({
            id: 'server-1',
            attachments: [{ id: 'att-1', name: 'a.txt', mime: 'text/plain', size: 5 }],
          }),
      } as unknown as Response)
    );

    const beforeSend = Date.now();
    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'see /workspace/a.md',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: ['att-1'],
        },
      })
    );
    const afterSend = Date.now();

    expect(ingestSpy).toHaveBeenCalledTimes(1);
    const [message, refs, context, opts] = ingestSpy.mock.calls[0];
    expect((message as { id: string }).id).toBe('server-1');
    expect(refs).toEqual([{ id: 'att-1', name: 'a.txt', mime: 'text/plain', size: 5 }]);
    expect((context as { provisional?: boolean }).provisional).toBe(true);
    expect((opts as { scopeGeneration?: number }).scopeGeneration).toBe(
      chatRecentFiles.scopeGeneration
    );
    // The message text (which is what buildCandidates scans for inline
    // container paths, as opposed to attachment refs) must actually reach
    // ingest, not just the id.
    expect((message as { text?: string }).text).toBe('see /workspace/a.md');
    // The provisional record's timestamp is the client's own optimistic
    // send time (the response never carries the server's createdAt) — it
    // must be a real timestamp captured around the send, not an empty or
    // fabricated value.
    const sentAtMs = new Date((message as { sentAt: string }).sentAt).getTime();
    expect(sentAtMs).toBeGreaterThanOrEqual(beforeSend);
    expect(sentAtMs).toBeLessThanOrEqual(afterSend);
  });

  it('attributes an accepted send to the conversation/project it was sent in, even if the thread switches conversations before the response resolves', async () => {
    const el = await mount();
    el.projectId = 'proj-A';
    await el.updateComplete;
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    let resolveSend!: (value: unknown) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveSend = resolve;
        })
    );

    const sendPromise = internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'see /workspace/a.md',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    // The thread is reused for a different conversation/project while the
    // POST above is still in flight.
    el.conversationKey = 'other-thread';
    el.projectId = 'proj-B';
    await el.updateComplete;

    resolveSend({
      ok: true,
      status: 201,
      json: () => Promise.resolve({ id: 'server-race', attachments: [] }),
    });
    await sendPromise;

    expect(ingestSpy).toHaveBeenCalledTimes(1);
    const [message, , context] = ingestSpy.mock.calls[0];
    expect((message as { conversationKey?: string }).conversationKey).toBe(CONVERSATION_KEY);
    expect((context as { projectId?: string }).projectId).toBe('proj-A');
  });

  it('does not re-resolve the project against a new conversation when none was resolvable at send time', async () => {
    // `recordRecentFiles`'s `opts.projectId` snapshot must be trusted even
    // when it's the empty string ("no project was resolvable at send
    // time") — a truthy check on that snapshot would treat '' the same as
    // "not provided" and wrongly fall back to re-resolving against whatever
    // project the thread has since moved on to.
    const el = await mount();
    el.projectId = '';
    await el.updateComplete;
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    let resolveSend!: (value: unknown) => void;
    apiFetch.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveSend = resolve;
        })
    );

    const sendPromise = internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'see /workspace/a.md',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    // Switch to a project-backed conversation while the POST is in flight.
    el.conversationKey = 'other-thread';
    el.projectId = 'proj-B';
    await el.updateComplete;

    resolveSend({
      ok: true,
      status: 201,
      json: () => Promise.resolve({ id: 'server-race-no-project', attachments: [] }),
    });
    await sendPromise;

    expect(ingestSpy).toHaveBeenCalledTimes(1);
    const [, , context] = ingestSpy.mock.calls[0];
    // No project was resolvable at send time, so the context must omit
    // projectId entirely — never fall back to the new conversation's project.
    expect((context as { projectId?: string }).projectId).toBeUndefined();
  });

  it('does not capture a send whose response is missing an id', async () => {
    const el = await mount();
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 201,
        json: () => Promise.resolve({ attachments: [] }), // no id
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

    expect(ingestSpy).not.toHaveBeenCalled();
  });

  it('does not capture a failed send', async () => {
    const el = await mount();
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: false,
        status: 500,
        text: () => Promise.resolve(''),
      } as unknown as Response)
    );

    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hello',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          onError: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    expect(ingestSpy).not.toHaveBeenCalled();
  });

  it('does not capture when the send throws (network failure)', async () => {
    const el = await mount();
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    };

    apiFetch.mockImplementationOnce(() => Promise.reject(new Error('offline')));

    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hello',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          onError: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );

    expect(ingestSpy).not.toHaveBeenCalled();
  });

  it('captures a live SSE message from another sender, non-provisionally', async () => {
    const el = await mount();
    el.currentUserId = 'user-me';

    emitChatMessage({
      id: 'srv-2',
      threadId: CONVERSATION_KEY,
      senderId: 'agent-1',
      sender: 'agent:coder',
      msg: 'here: /workspace/report.md',
      type: 'chat',
      createdAt: '2026-01-01T00:00:00Z',
    });
    await el.updateComplete;

    expect(ingestSpy).toHaveBeenCalledTimes(1);
    const [message, , context] = ingestSpy.mock.calls[0];
    expect((message as { id: string }).id).toBe('srv-2');
    expect((context as { provisional?: boolean }).provisional).toBeUndefined();
  });

  it('does not capture a live message whose SSE payload has no createdAt', async () => {
    // A fabricated (viewing-time) timestamp must never be recorded, and must
    // never "correct" a pending provisional record for the same server id.
    const el = await mount();
    el.currentUserId = 'user-me';

    emitChatMessage({
      id: 'srv-no-time',
      threadId: CONVERSATION_KEY,
      senderId: 'agent-1',
      sender: 'agent:coder',
      msg: 'here: /workspace/report.md',
      type: 'chat',
      // createdAt intentionally omitted
    });
    await el.updateComplete;

    expect(ingestSpy).not.toHaveBeenCalled();
  });

  it('captures messages admitted through the initial history load', async () => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'hist-1',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'note /workspace/hist.md',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
            },
          ],
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.projectId = 'proj-thread';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());

    const [message, , context, opts] = ingestSpy.mock.calls[0];
    expect((message as { id: string }).id).toBe('hist-1');
    expect((context as { projectId?: string }).projectId).toBe('proj-thread');
    expect((opts as { scopeGeneration?: number }).scopeGeneration).toBe(
      chatRecentFiles.scopeGeneration
    );
  });

  it('drops an initial history page once the thread has switched conversations', async () => {
    // The race is specifically in the gap between the fetch resolving (the
    // existing fetchId check at that point still passes) and `res.json()`
    // resolving — not the earlier gap during the fetch itself, which the
    // existing check already covers.
    apiFetch.mockReset();
    let resolveJson!: (value: unknown) => void;
    apiFetch.mockImplementationOnce(
      () =>
        Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            new Promise((resolve) => {
              resolveJson = resolve;
            }),
        }) as unknown as Promise<Response>
    );
    // The new conversation's own load stays pending, so whatever the
    // pagination state reads afterwards was left by the stale page alone.
    apiFetch.mockImplementation(() => new Promise(() => {}));

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.projectId = 'proj-A';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(resolveJson).toBeDefined());

    // Switch conversations while `res.json()` is still pending.
    el.conversationKey = 'other-thread';
    el.projectId = 'proj-B';
    await el.updateComplete;

    resolveJson({
      items: [
        {
          id: 'hist-stale',
          projectId: '',
          sender: 'agent:coder',
          senderId: 'agent-1',
          recipient: '',
          recipientId: '',
          msg: 'note /workspace/hist.md',
          type: 'chat',
          agentId: '',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
      nextCursor: 'stale-cursor',
      messageAttachments: {
        'hist-stale': [{ id: 'att-stale', name: 'a.md', mime: 'text/markdown', size: 1 }],
      },
    });

    for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));

    // The page belongs to the conversation the thread has left: it is
    // neither shown in the new one nor recorded as its files.
    const internals = el as unknown as {
      messageMap: Map<string, unknown>;
      v2AttachmentMap: Map<string, unknown>;
      nextCursor: string | null;
      hasOlderMessages: boolean;
    };
    expect(internals.messageMap.has('hist-stale')).toBe(false);
    expect(internals.v2AttachmentMap.has('hist-stale')).toBe(false);
    // A one-item page would mark the history exhausted and set a cursor.
    expect(internals.nextCursor).toBeNull();
    expect(internals.hasOlderMessages).toBe(true);
    expect(ingestSpy).not.toHaveBeenCalled();
  });

  it('forwards a historical message’s legacy (wave-1) attachment paths to ingest', async () => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'hist-legacy',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'no mention in text',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
              attachments: ['/workspace/legacy.md'],
            },
          ],
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.projectId = 'proj-thread';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());

    const [message] = ingestSpy.mock.calls[0];
    expect((message as { legacyAttachmentPaths?: string[] }).legacyAttachmentPaths).toEqual([
      '/workspace/legacy.md',
    ]);
  });

  it('forwards a historical message’s modern (W7) attachment refs from messageAttachments to ingest', async () => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'hist-refs',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'see attached',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
            },
          ],
          messageAttachments: {
            'hist-refs': [
              { id: 'att-hist-1', name: 'report.pdf', mime: 'application/pdf', size: 10 },
            ],
          },
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.projectId = 'proj-thread';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());

    const [, refs] = ingestSpy.mock.calls[0];
    expect(refs).toEqual([
      { id: 'att-hist-1', name: 'report.pdf', mime: 'application/pdf', size: 10 },
    ]);
  });

  it('captures messages admitted through a reconnect backfill', async () => {
    const el = await mount();

    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'backfill-1',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'note /workspace/backfill.md',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
            },
          ],
        }),
    } as unknown as Response);

    // A lightweight SSE notification (no full payload) falls back to backfill.
    emitChatMessage({});
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());

    const [message, , , opts] = ingestSpy.mock.calls[0];
    expect((message as { id: string }).id).toBe('backfill-1');
    expect((opts as { scopeGeneration?: number }).scopeGeneration).toBe(
      chatRecentFiles.scopeGeneration
    );
    void el;
  });

  it('drops a reconnect-backfill page once the thread has switched conversations', async () => {
    // Same race as the initial-history-load test above, but for
    // runBackfillV2's post-json re-check: the gap is between
    // the fetch resolving (its earlier fetchId check still passes) and
    // `res.json()` resolving.
    const el = await mount();
    el.projectId = 'proj-A';
    await el.updateComplete;

    let resolveJson!: (value: unknown) => void;
    apiFetch.mockImplementationOnce(
      () =>
        Promise.resolve({
          ok: true,
          status: 200,
          json: () =>
            new Promise((resolve) => {
              resolveJson = resolve;
            }),
        }) as unknown as Promise<Response>
    );

    emitChatMessage({}); // a lightweight SSE notification triggers backfillV2
    await vi.waitFor(() => expect(resolveJson).toBeDefined());

    // Switch conversations while `res.json()` is still pending.
    el.conversationKey = 'other-thread';
    el.projectId = 'proj-B';
    await el.updateComplete;

    resolveJson({
      items: [
        {
          id: 'backfill-stale',
          projectId: '',
          sender: 'agent:coder',
          senderId: 'agent-1',
          recipient: '',
          recipientId: '',
          msg: 'note /workspace/backfill.md',
          type: 'chat',
          agentId: '',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
      messageAttachments: {
        'backfill-stale': [{ id: 'att-stale', name: 'b.md', mime: 'text/markdown', size: 1 }],
      },
      messageExtensions: { 'backfill-stale': { messageId: 'backfill-stale', replyToId: 'x' } },
      replyPreviews: { 'backfill-stale': { messageId: 'x', senderName: 'A', content: 'c' } },
    });

    for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));

    // The page belongs to the conversation the thread has left: it is
    // neither shown in the new one nor recorded as its files.
    const internals = el as unknown as {
      messageMap: Map<string, unknown>;
      v2AttachmentMap: Map<string, unknown>;
      v2MessageExtMap: Map<string, unknown>;
      v2ReplyPreviewMap: Map<string, unknown>;
    };
    expect(internals.messageMap.has('backfill-stale')).toBe(false);
    expect(internals.v2AttachmentMap.has('backfill-stale')).toBe(false);
    expect(internals.v2MessageExtMap.has('backfill-stale')).toBe(false);
    expect(internals.v2ReplyPreviewMap.has('backfill-stale')).toBe(false);
    expect(ingestSpy).not.toHaveBeenCalled();
  });

  it('captures messages admitted through an around-message fetch', async () => {
    // scrollToMessageById only attempts fetchAroundMessage when the target id
    // isn't already rendered, and the thread's "messages-scroll" container
    // (which the scroll math needs) only renders once at least one message
    // exists — seed the initial history with an unrelated message first.
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'seed-1',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'seed message',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
            },
          ],
        }),
    } as unknown as Response);
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());
    ingestSpy.mockClear();

    apiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'around-1',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'note /workspace/around.md',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
            },
          ],
        }),
    } as unknown as Response);

    await el.scrollToMessageById('missing-message-id');
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());

    const [message, , , opts] = ingestSpy.mock.calls[0];
    expect((message as { id: string }).id).toBe('around-1');
    expect((opts as { scopeGeneration?: number }).scopeGeneration).toBe(
      chatRecentFiles.scopeGeneration
    );
  });

  it('captures nothing from mounting/rendering alone, with no load, merge, or send', async () => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await el.updateComplete;
    await el.updateComplete;

    expect(ingestSpy).not.toHaveBeenCalled();
  });

  it('does not capture a mention fan-out copy admitted through history', async () => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'mention-1',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'note /workspace/mention.md',
              type: 'mention',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
            },
            {
              id: 'real-1',
              projectId: '',
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'note /workspace/real.md',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:01Z',
            },
          ],
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.projectId = 'proj-thread';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());

    expect(ingestSpy).toHaveBeenCalledTimes(1);
    const [message] = ingestSpy.mock.calls[0];
    expect((message as { id: string }).id).toBe('real-1');
  });

  it('resolves the DM sender project over the thread-level inherited project (live SSE, messageProjectId)', async () => {
    // The SSE payload's ChatEventData has no senderProjectId field (only
    // projectId flows through live messages today), so this exercises the
    // messageProjectId fallback half of the precedence at thread level.
    const el = await mount();
    el.isDM = true;
    el.projectId = 'proj-inherited';

    emitChatMessage({
      id: 'srv-3',
      threadId: CONVERSATION_KEY,
      senderId: 'agent-1',
      sender: 'agent:coder',
      msg: 'note /workspace/dm.md',
      type: 'chat',
      projectId: 'proj-sender',
      createdAt: '2026-01-01T00:00:00Z',
    });
    await el.updateComplete;

    const [, , context] = ingestSpy.mock.calls[0];
    expect((context as { projectId?: string }).projectId).toBe('proj-sender');
  });

  it('resolves the DM senderProjectId over both messageProjectId and the thread-level inherited project (history load)', async () => {
    // Unlike the live-SSE path above, a full history Message does carry
    // senderProjectId — this proves the sender-field half of the precedence
    // (resolveMessageProjectId: senderProjectId before messageProjectId)
    // actually wins at thread level, not just in the pure-helper unit tests.
    apiFetch.mockReset();
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: [
            {
              id: 'hist-sender',
              projectId: 'proj-message', // messageProjectId — must lose to senderProjectId
              sender: 'agent:coder',
              senderId: 'agent-1',
              recipient: '',
              recipientId: '',
              msg: 'note /workspace/dm.md',
              type: 'chat',
              agentId: '',
              createdAt: '2026-01-01T00:00:00Z',
              senderProjectId: 'proj-sender',
            },
          ],
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.isDM = true;
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(ingestSpy).toHaveBeenCalled());

    const [, , context] = ingestSpy.mock.calls[0];
    expect((context as { projectId?: string }).projectId).toBe('proj-sender');
  });

  it('omits the path candidate (but the spy call still happens) when no project resolves', async () => {
    const el = await mount();
    el.isDM = true;
    el.projectId = 'proj-inherited';

    emitChatMessage({
      id: 'srv-4',
      threadId: CONVERSATION_KEY,
      senderId: 'agent-1',
      sender: 'agent:coder',
      msg: 'note /workspace/dm.md',
      type: 'chat',
      createdAt: '2026-01-01T00:00:00Z',
    });
    await el.updateComplete;

    const [, , context] = ingestSpy.mock.calls[0];
    expect((context as { projectId?: string }).projectId).toBeUndefined();
  });
});

/**
 * "Open terminal" / "Open in graph" on the message right-click context menu
 * (nc-msg-agent-actions). These reuse the exact icons/labels/actions the
 * toolbar (`renderAgentToolbarButtons`, pages/chat.ts) and members sidebar
 * (`renderAgent`, chat-members.ts) already use, but act on the message's
 * author agent rather than the thread's default agent or DM peer.
 */
describe('scion-chat-thread agent message context-menu actions (nc-msg-agent-actions)', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    navigateToMock.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  function rightClick(target: Element): void {
    target.dispatchEvent(
      new MouseEvent('contextmenu', {
        bubbles: true,
        composed: true,
        cancelable: true,
        clientX: 10,
        clientY: 20,
      })
    );
  }

  /** Mount a thread with the given history items and agent roster. */
  async function mountWithMessages(
    items: Record<string, unknown>[],
    agentMembers: ChatAgentMember[] = []
  ): Promise<{ el: ScionChatThread; bubbles: HTMLElement[] }> {
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ items }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.agentMembers = agentMembers;
    document.body.appendChild(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(items.length)
    );
    const bubbles = Array.from(el.shadowRoot!.querySelectorAll('scion-chat-message')) as Array<
      HTMLElement & { updateComplete: Promise<boolean> }
    >;
    for (const b of bubbles) await b.updateComplete;
    return { el, bubbles };
  }

  /** Text of every open context-menu item, trimmed. */
  function menuItemLabels(el: ScionChatThread): string[] {
    return Array.from(el.shadowRoot!.querySelectorAll('.context-menu-item')).map(
      (n) => n.textContent?.replace(/\s+/g, ' ').trim() ?? ''
    );
  }

  function findMenuItem(el: ScionChatThread, label: string): HTMLElement | undefined {
    return Array.from(el.shadowRoot!.querySelectorAll<HTMLElement>('.context-menu-item')).find(
      (n) => n.textContent?.includes(label)
    );
  }

  const AGENT_MSG = {
    id: 'm1',
    sender: 'agent:coder',
    senderId: 'agent-1',
    msg: 'agent says hi',
    type: 'chat',
    createdAt: '2026-01-01T00:00:00Z',
  };
  const USER_MSG = {
    id: 'm2',
    sender: 'them@example.com',
    senderId: 'user-them',
    msg: 'user says hi',
    type: 'chat',
    createdAt: '2026-01-01T00:01:00Z',
  };

  /**
   * A second roster entry, listed *before* the author in every fixture below,
   * with `canAttach`/`projectId` deliberately opposite the author's. A lookup
   * that resolved `agentMembers[0]` instead of matching `senderId` would gate
   * "Open terminal" on this agent's `canAttach` and build the graph link from
   * this agent's `projectId` instead of the author's — every assertion below
   * is written so that substitution produces a visibly wrong result.
   */
  const OTHER_AGENT: ChatAgentMember = {
    id: 'agent-other',
    kind: 'agent',
    displayName: 'Other',
    canAttach: false,
    projectId: 'proj-other',
  };

  it('shows both items on an agent message and hides them on a user message', async () => {
    const { el, bubbles } = await mountWithMessages(
      [AGENT_MSG, USER_MSG],
      [
        OTHER_AGENT,
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: true,
          projectId: 'proj-1',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    let labels = menuItemLabels(el);
    expect(labels.some((l) => l.includes('Open terminal'))).toBe(true);
    expect(labels.some((l) => l.includes('Open in graph'))).toBe(true);

    rightClick(bubbles[1]);
    await el.updateComplete;
    labels = menuItemLabels(el);
    expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
    expect(labels.some((l) => l.includes('Open in graph'))).toBe(false);
  });

  it('resolves the lookup by the message author id, not roster position (roster-ordering regression)', async () => {
    // The reviewer's exact repro (nc-msg-agent-actions-review.md, R1): the
    // author is second in the roster, and the first entry has the opposite
    // canAttach and a different projectId. `agentMembers[0]` would show
    // terminal (the other agent can attach) and point the graph link at
    // `proj-other` — both wrong for this message's actual author.
    const { el, bubbles } = await mountWithMessages(
      [AGENT_MSG],
      [
        {
          id: 'agent-other',
          kind: 'agent',
          displayName: 'Other',
          canAttach: true,
          projectId: 'proj-other',
        },
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: false,
          projectId: 'proj-author',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    expect(menuItemLabels(el).some((l) => l.includes('Open terminal'))).toBe(false);

    findMenuItem(el, 'Open in graph')!.click();
    expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-author', 'agent-1'));
  });

  it('clicking "Open terminal" invokes the same nav-click action the toolbar button uses, for the message author agent', async () => {
    const { el, bubbles } = await mountWithMessages(
      [AGENT_MSG],
      [
        OTHER_AGENT,
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: true,
          projectId: 'proj-1',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;

    const navClick = vi.fn();
    document.addEventListener('nav-click', navClick);
    try {
      findMenuItem(el, 'Open terminal')!.click();
    } finally {
      document.removeEventListener('nav-click', navClick);
    }

    expect(navClick).toHaveBeenCalledTimes(1);
    const detail = (navClick.mock.calls[0][0] as CustomEvent<{ path: string }>).detail;
    expect(detail.path).toBe(terminalHref('agent-1'));
    // The context menu closes after acting, same as every other item.
    await el.updateComplete;
    expect(el.shadowRoot?.querySelector('.context-menu')).toBeNull();
  });

  it('clicking "Open in graph" invokes the same navigation the toolbar button uses, for the message author agent', async () => {
    const { el, bubbles } = await mountWithMessages(
      [AGENT_MSG],
      [
        OTHER_AGENT,
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: true,
          projectId: 'proj-1',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    findMenuItem(el, 'Open in graph')!.click();
    await el.updateComplete;

    expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-1', 'agent-1'));
    expect(el.shadowRoot?.querySelector('.context-menu')).toBeNull();
  });

  it('prefers the message senderProjectId over the roster projectId for the graph link (agents from another project, #1913)', async () => {
    const { el, bubbles } = await mountWithMessages(
      [{ ...AGENT_MSG, senderProjectId: 'proj-sender' }],
      [
        OTHER_AGENT,
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: true,
          projectId: 'proj-1',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    findMenuItem(el, 'Open in graph')!.click();

    expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-sender', 'agent-1'));
  });

  it("prefers the message senderProjectId over the message's own projectId for the graph link", async () => {
    const { el, bubbles } = await mountWithMessages(
      [{ ...AGENT_MSG, senderProjectId: 'proj-sender', projectId: 'proj-msg' }],
      [
        OTHER_AGENT,
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: true,
          projectId: 'proj-1',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    findMenuItem(el, 'Open in graph')!.click();

    expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-sender', 'agent-1'));
  });

  it("prefers the roster projectId over the message's own projectId for the graph link", async () => {
    // No senderProjectId. The rostered author's projectId ('proj-roster')
    // must win over the message's own projectId ('proj-msg') — the roster
    // is checked first in the chain.
    const { el, bubbles } = await mountWithMessages(
      [{ ...AGENT_MSG, projectId: 'proj-msg' }],
      [
        OTHER_AGENT,
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: true,
          projectId: 'proj-roster',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    findMenuItem(el, 'Open in graph')!.click();

    expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-roster', 'agent-1'));
  });

  it('hides "Open terminal" (fail closed) when canAttach is not explicitly true, but keeps "Open in graph"', async () => {
    const { el, bubbles } = await mountWithMessages(
      [AGENT_MSG],
      [OTHER_AGENT, { id: 'agent-1', kind: 'agent', displayName: 'Coder', projectId: 'proj-1' }]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    const labels = menuItemLabels(el);
    expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
    expect(labels.some((l) => l.includes('Open in graph'))).toBe(true);
  });

  it('hides "Open in graph" when no project can be resolved, but keeps "Open terminal"', async () => {
    const { el, bubbles } = await mountWithMessages(
      [AGENT_MSG],
      [OTHER_AGENT, { id: 'agent-1', kind: 'agent', displayName: 'Coder', canAttach: true }]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    const labels = menuItemLabels(el);
    expect(labels.some((l) => l.includes('Open terminal'))).toBe(true);
    expect(labels.some((l) => l.includes('Open in graph'))).toBe(false);
  });

  it('hides both items when the message has no senderId, even if classified as agent-authored by type', async () => {
    // isSenderAgent can return true by `msg.type` alone (assistant-reply /
    // mention-reply) with no matching roster member. Without an id there is
    // no agent to open a terminal on or focus the graph on.
    const { el, bubbles } = await mountWithMessages(
      [
        {
          id: 'm3',
          sender: 'unknown',
          senderId: '',
          senderProjectId: 'proj-sender',
          msg: 'no sender id',
          type: 'assistant-reply',
          createdAt: '2026-01-01T00:02:00Z',
        },
      ],
      [
        OTHER_AGENT,
        {
          id: 'agent-1',
          kind: 'agent',
          displayName: 'Coder',
          canAttach: true,
          projectId: 'proj-1',
        },
      ]
    );

    rightClick(bubbles[0]);
    await el.updateComplete;
    const labels = menuItemLabels(el);
    expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
    expect(labels.some((l) => l.includes('Open in graph'))).toBe(false);
  });

  describe('a departed author (no longer in the roster)', () => {
    it('shows "Open in graph" using senderProjectId, but hides "Open terminal" (product decision: graph can still show the project/history)', async () => {
      const { el, bubbles } = await mountWithMessages(
        [{ ...AGENT_MSG, senderProjectId: 'proj-sender' }],
        [OTHER_AGENT]
      );

      rightClick(bubbles[0]);
      await el.updateComplete;
      const labels = menuItemLabels(el);
      expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
      expect(labels.some((l) => l.includes('Open in graph'))).toBe(true);

      findMenuItem(el, 'Open in graph')!.click();
      expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-sender', 'agent-1'));
    });

    it('hides both items when no project can be resolved', async () => {
      const { el, bubbles } = await mountWithMessages([AGENT_MSG], [OTHER_AGENT]);

      rightClick(bubbles[0]);
      await el.updateComplete;
      const labels = menuItemLabels(el);
      expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
      expect(labels.some((l) => l.includes('Open in graph'))).toBe(false);
    });

    it("prefers the message's own projectId over the thread's, for a cross-project departed author (agent-to-user rows never set senderProjectId)", async () => {
      // No senderProjectId and no roster entry, but the message carries its
      // own projectId (the author's project, as agent-to-user rows do)
      // which differs from the thread's project. The author's project must
      // win — falling back to the thread's would point the graph at the
      // wrong project.
      const { el, bubbles } = await mountWithMessages(
        [{ ...AGENT_MSG, projectId: 'proj-author' }],
        [OTHER_AGENT]
      );
      el.projectId = 'proj-thread';

      rightClick(bubbles[0]);
      await el.updateComplete;
      const labels = menuItemLabels(el);
      expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
      expect(labels.some((l) => l.includes('Open in graph'))).toBe(true);

      findMenuItem(el, 'Open in graph')!.click();
      expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-author', 'agent-1'));
    });

    it("prefers the message's own projectId over the thread's in a DM too", async () => {
      const { el, bubbles } = await mountWithMessages(
        [{ ...AGENT_MSG, projectId: 'proj-author' }],
        [OTHER_AGENT]
      );
      el.isDM = true;
      el.projectId = 'proj-inherited';

      rightClick(bubbles[0]);
      await el.updateComplete;
      findMenuItem(el, 'Open in graph')!.click();
      expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-author', 'agent-1'));
    });

    it('falls back to the thread\'s own project id for "Open in graph" in a project-scoped (non-DM) thread, when the message carries no project of its own', async () => {
      // Neither senderProjectId, a roster entry, nor the message's own
      // projectId is available, but this is a project-scoped thread, so its
      // own projectId is a correct, safe last resort — unlike a DM's
      // projectId (see the next test).
      const { el, bubbles } = await mountWithMessages([AGENT_MSG], [OTHER_AGENT]);
      el.projectId = 'proj-thread';

      rightClick(bubbles[0]);
      await el.updateComplete;
      const labels = menuItemLabels(el);
      expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
      expect(labels.some((l) => l.includes('Open in graph'))).toBe(true);

      findMenuItem(el, 'Open in graph')!.click();
      expect(navigateToMock).toHaveBeenCalledWith(agentGraphHref('proj-thread', 'agent-1'));
    });

    it('does not fall back to the thread projectId in a DM — it is only the inherited, unrelated project', async () => {
      const { el, bubbles } = await mountWithMessages([AGENT_MSG], [OTHER_AGENT]);
      el.isDM = true;
      el.projectId = 'proj-inherited';

      rightClick(bubbles[0]);
      await el.updateComplete;
      const labels = menuItemLabels(el);
      expect(labels.some((l) => l.includes('Open terminal'))).toBe(false);
      expect(labels.some((l) => l.includes('Open in graph'))).toBe(false);
    });
  });
});

/**
 * Choosing Reply from the message context menu must move keyboard focus into
 * the composer so the user can start typing the reply immediately. The menu
 * itself must be gone and the reply-preview chip rendered before focus lands
 * — otherwise the menu's own teardown could still be mid-flight and steal
 * focus back to the message bubble.
 */
describe('scion-chat-thread reply focuses the composer', () => {
  beforeEach(() => {
    apiFetch.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  /** Mount a v2 thread with one rendered message bubble per given item, in order. */
  async function mountWithMessages(
    items: Array<{ id: string; msg: string; createdAt: string }>
  ): Promise<{ el: ScionChatThread; bubbles: HTMLElement[] }> {
    apiFetch.mockResolvedValue({
      ok: true,
      status: 200,
      json: () =>
        Promise.resolve({
          items: items.map((item) => ({
            sender: 'them@example.com',
            senderId: 'user-them',
            type: 'chat',
            ...item,
          })),
        }),
    } as unknown as Response);

    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await vi.waitFor(() =>
      expect(el.shadowRoot?.querySelectorAll('scion-chat-message').length).toBe(items.length)
    );
    const bubbles = Array.from(
      el.shadowRoot!.querySelectorAll('scion-chat-message')
    ) as (HTMLElement & {
      updateComplete: Promise<boolean>;
    })[];
    await Promise.all(bubbles.map((b) => b.updateComplete));
    return { el, bubbles };
  }

  /** Right-click a message bubble, then click the "Reply" item in the menu that opens. */
  async function chooseReplyFromContextMenu(
    el: ScionChatThread,
    bubble: HTMLElement
  ): Promise<void> {
    bubble.dispatchEvent(
      new MouseEvent('contextmenu', { bubbles: true, composed: true, clientX: 5, clientY: 5 })
    );
    await el.updateComplete;

    const replyItem = Array.from(el.shadowRoot!.querySelectorAll('.context-menu-item')).find(
      (item) => item.textContent?.includes('Reply')
    ) as HTMLElement | undefined;
    expect(replyItem).toBeTruthy();
    replyItem!.click();
    await el.updateComplete;
  }

  it('makes the composer textarea the active element after choosing Reply', async () => {
    const { el, bubbles } = await mountWithMessages([
      { id: 'm1', msg: 'hello there', createdAt: '2026-01-01T00:00:00Z' },
    ]);

    await chooseReplyFromContextMenu(el, bubbles[0]);

    // Menu is gone and the reply-preview chip is up before focus is asserted.
    expect(el.shadowRoot?.querySelector('.context-menu')).toBeNull();
    const composer = el.shadowRoot!.querySelector('scion-chat-composer') as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    await composer.updateComplete;
    expect(composer.shadowRoot?.querySelector('.reply-bar')).not.toBeNull();

    await vi.waitFor(() => {
      const slTextarea = composer.shadowRoot?.querySelector('sl-textarea') as
        | (HTMLElement & { shadowRoot: ShadowRoot | null })
        | null;
      expect(slTextarea).not.toBeNull();
      const textarea = slTextarea!.shadowRoot?.querySelector('textarea') ?? null;
      expect(composer.shadowRoot?.activeElement).toBe(slTextarea);
      expect(slTextarea!.shadowRoot?.activeElement).toBe(textarea);
    });
  });

  it('places the caret at the end of the existing draft, not at its start', async () => {
    const { el, bubbles } = await mountWithMessages([
      { id: 'm1', msg: 'hello there', createdAt: '2026-01-01T00:00:00Z' },
    ]);
    const composer = el.shadowRoot!.querySelector('scion-chat-composer') as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    await composer.updateComplete;

    const slTextarea = composer.shadowRoot!.querySelector('sl-textarea') as HTMLElement & {
      shadowRoot: ShadowRoot | null;
      updateComplete: Promise<boolean>;
    };
    await slTextarea.updateComplete;
    const textarea = slTextarea.shadowRoot!.querySelector('textarea') as HTMLTextAreaElement;

    // Simulate an in-progress draft the user had already typed.
    textarea.value = 'existing draft text';
    textarea.selectionStart = textarea.selectionEnd = 0;
    textarea.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    await composer.updateComplete;

    await chooseReplyFromContextMenu(el, bubbles[0]);

    await vi.waitFor(() => {
      expect(document.activeElement).toBe(el);
      expect(textarea.selectionStart).toBe('existing draft text'.length);
      expect(textarea.selectionEnd).toBe('existing draft text'.length);
    });
  });

  it('re-focuses the composer when the reply target switches from one message to another', async () => {
    const { el, bubbles } = await mountWithMessages([
      { id: 'm1', msg: 'hello there', createdAt: '2026-01-01T00:00:00Z' },
      { id: 'm2', msg: 'second message', createdAt: '2026-01-01T00:01:00Z' },
    ]);

    await chooseReplyFromContextMenu(el, bubbles[0]);

    const composer = el.shadowRoot!.querySelector('scion-chat-composer') as HTMLElement & {
      updateComplete: Promise<boolean>;
    };
    await composer.updateComplete;
    const slTextarea = composer.shadowRoot!.querySelector('sl-textarea') as HTMLElement & {
      shadowRoot: ShadowRoot | null;
    };
    let textarea: HTMLTextAreaElement | null = null;
    await vi.waitFor(() => {
      textarea = slTextarea.shadowRoot?.querySelector('textarea') ?? null;
      expect(textarea).not.toBeNull();
      expect(slTextarea.shadowRoot?.activeElement).toBe(textarea);
    });

    // Move focus away — a subsequent reply-target change must reclaim it
    // rather than leaving focus wherever it drifted to in between.
    textarea!.blur();
    expect(slTextarea.shadowRoot?.activeElement).not.toBe(textarea);

    await chooseReplyFromContextMenu(el, bubbles[1]);
    await composer.updateComplete;

    // Confirms the target actually changed, not just a re-fire on message 1.
    expect(composer.shadowRoot?.querySelector('.reply-bar .reply-content')?.textContent).toContain(
      'second message'
    );
    await vi.waitFor(() => {
      expect(composer.shadowRoot?.activeElement).toBe(slTextarea);
      expect(slTextarea.shadowRoot?.activeElement).toBe(textarea);
    });
  });
});

describe('scion-chat-thread /stop slash command', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
    fakeStateManager.clearAgents();
  });

  /**
   * Regression test for ptone/scion#2482: `/stop <agent>` must stop the
   * agent, not delete it. It must hit the project-scoped stop endpoint the
   * hub actually resolves slugs against (handleProjectAgentAction,
   * pkg/hub/handlers_projects_core.go), not the unscoped
   * `/api/v1/agents/{id}/stop` route, which only resolves UUIDs and always
   * 404s for a slug.
   */
  it('sends POST to the project-scoped stop endpoint, not DELETE', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    const internals = el as unknown as {
      handleSlashStop(args: string): Promise<void>;
    };

    apiFetch.mockResolvedValueOnce({ ok: true, status: 200, json: () => Promise.resolve({}) });

    await internals.handleSlashStop('my-agent');

    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/projects/proj-1/agents/my-agent/stop',
      expect.objectContaining({ method: 'POST' })
    );
    expect(apiFetch).not.toHaveBeenCalledWith(
      expect.stringMatching(/^\/api\/v1\/agents\/my-agent$/),
      expect.objectContaining({ method: 'DELETE' })
    );
  });

  it('shows "Failed to stop agent" on a non-2xx response', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    const internals = el as unknown as {
      handleSlashStop(args: string): Promise<void>;
    };

    apiFetch.mockResolvedValueOnce({
      ok: false,
      status: 404,
      json: () =>
        Promise.resolve({
          error: { code: 'agent_not_found', message: 'Agent "my-agent" not found in project' },
        }),
    });

    await internals.handleSlashStop('my-agent');

    await vi.waitFor(() => {
      const lines = Array.from(el.shadowRoot?.querySelectorAll('scion-chat-system-line') ?? []);
      const messages = lines.map((l) => l.getAttribute('message'));
      expect(messages.some((m) => m?.includes('Failed to stop agent'))).toBe(true);
    });
  });

  it('shows a local message and makes no request when there is no project context', async () => {
    const el = await mount();
    el.projectId = '';
    const internals = el as unknown as {
      handleSlashStop(args: string): Promise<void>;
    };

    await internals.handleSlashStop('my-agent');

    // Scoped to POST/DELETE rather than just the no-request case, so this
    // would also catch a DELETE regression (the on-mount mark-as-read fetch
    // is a POST too, but it's debounced 1s behind a setTimeout — see
    // maybeAdvanceReadWatermark — so it never fires within this synchronous
    // assertion window).
    expect(apiFetch).not.toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({ method: expect.stringMatching(/^(POST|DELETE)$/) })
    );
    await vi.waitFor(() => {
      const lines = Array.from(el.shadowRoot?.querySelectorAll('scion-chat-system-line') ?? []);
      const messages = lines.map((l) => l.getAttribute('message'));
      expect(messages).toContain('No project context available.');
    });
  });

  /**
   * In a chat-page DM, `projectId` is only `inheritedProjectId()` — the
   * previously viewed project, not one the DM belongs to (see
   * `resolvePathLinkProjectId`). `/stop <slug>` must resolve against the DM
   * peer agent's own project instead, the same fallback
   * `resolvePathLinkProjectId` already uses for path links.
   */
  it('in a DM, targets the peer agent project, not the inherited thread projectId', async () => {
    fakeStateManager.setAgent('coder', 'proj-peer');
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = 'dm:agent:coder:user:u1';
    el.isDM = true;
    // The previously viewed project — must never be used for a DM's /stop.
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValueOnce({ ok: true, status: 200, json: () => Promise.resolve({}) });
    const internals = el as unknown as {
      handleSlashStop(args: string): Promise<void>;
    };

    await internals.handleSlashStop('my-agent');

    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/projects/proj-peer/agents/my-agent/stop',
      expect.objectContaining({ method: 'POST' })
    );
    expect(apiFetch).not.toHaveBeenCalledWith(
      expect.stringContaining('proj-inherited'),
      expect.anything()
    );
  });

  it('in a DM with no peer project, sends no stop request and shows the local message', async () => {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = 'dm:agent:unknown-agent:user:u1';
    el.isDM = true;
    // Non-empty, to prove this is never used as a fallback in a DM.
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
    const internals = el as unknown as {
      handleSlashStop(args: string): Promise<void>;
    };

    await internals.handleSlashStop('my-agent');

    expect(apiFetch).not.toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({ method: expect.stringMatching(/^(POST|DELETE)$/) })
    );
    await vi.waitFor(() => {
      const lines = Array.from(el.shadowRoot?.querySelectorAll('scion-chat-system-line') ?? []);
      const messages = lines.map((l) => l.getAttribute('message'));
      expect(messages).toContain('No project context available.');
    });
  });
});

describe('scion-chat-thread /status slash command', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
    fakeStateManager.clearAgents();
  });

  type StatusInternals = { handleSlashStatus(): Promise<void> };

  function agentsPage(agents: Array<{ slug: string; phase: string }>, nextCursor?: string) {
    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve(nextCursor ? { agents, nextCursor } : { agents }),
    };
  }

  function systemMessages(el: ScionChatThread): Array<string | null> {
    return Array.from(el.shadowRoot?.querySelectorAll('scion-chat-system-line') ?? []).map((l) =>
      l.getAttribute('message')
    );
  }

  function agentListCalls(): string[] {
    return apiFetch.mock.calls
      .map((c) => String(c[0]))
      .filter((u) => u.startsWith('/api/v1/agents?'));
  }

  /**
   * The GET /api/v1/agents list handler filters on `projectId` and returns
   * `{ agents, nextCursor }`. A `project=` param is ignored, and a client
   * reading `items` never sees any agents.
   */
  it('queries by projectId and renders the agents from the response', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce(
      agentsPage([
        { slug: 'coder', phase: 'running' },
        { slug: 'reviewer', phase: 'stopped' },
      ])
    );

    await (el as unknown as StatusInternals).handleSlashStatus();

    expect(agentListCalls()).toEqual(['/api/v1/agents?projectId=proj-1']);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain(
        'Project agents:\n  coder: running\n  reviewer: stopped'
      );
    });
  });

  /**
   * The server caps each page (500 at the store) and returns `nextCursor`
   * when more remain. The cursor is bound to the request filter, so each
   * follow-up page must repeat the same `projectId`.
   */
  it('follows nextCursor across pages, repeating the projectId filter', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch
      .mockResolvedValueOnce(agentsPage([{ slug: 'a1', phase: 'running' }], 'c1'))
      .mockResolvedValueOnce(agentsPage([], 'c2'))
      .mockResolvedValueOnce(agentsPage([{ slug: 'a2', phase: 'stopped' }]));

    await (el as unknown as StatusInternals).handleSlashStatus();

    expect(agentListCalls()).toEqual([
      '/api/v1/agents?projectId=proj-1',
      '/api/v1/agents?projectId=proj-1&cursor=c1',
      '/api/v1/agents?projectId=proj-1&cursor=c2',
    ]);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('Project agents:\n  a1: running\n  a2: stopped');
    });
  });

  it('stops with a failure message when the server repeats a cursor', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch
      .mockResolvedValueOnce(agentsPage([{ slug: 'a1', phase: 'running' }], 'c1'))
      .mockResolvedValueOnce(agentsPage([{ slug: 'a2', phase: 'running' }], 'c1'));

    await (el as unknown as StatusInternals).handleSlashStatus();

    expect(agentListCalls()).toHaveLength(2);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('Failed to fetch project status.');
    });
  });

  /**
   * Mirrors the component's MAX_STATUS_AGENT_PAGES bound in chat-thread.ts
   * (the server has no page limit). A server that keeps minting new cursors
   * must not be followed forever: the walk stops at the bound and the
   * listing ends with a truncation note.
   */
  const MAX_STATUS_AGENT_PAGES = 20;

  it('stops at the page bound and notes truncation when cursors never end', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    let page = 0;
    apiFetch.mockImplementation((url: string) => {
      if (!String(url).startsWith('/api/v1/agents?')) return Promise.resolve(emptyHistory());
      page++;
      // Stop minting cursors well past the bound so an unbounded walk
      // fails the call-count assertion instead of hanging the test.
      const next = page < MAX_STATUS_AGENT_PAGES + 5 ? `c${page}` : undefined;
      return Promise.resolve(agentsPage([{ slug: `a${page}`, phase: 'running' }], next));
    });

    await (el as unknown as StatusInternals).handleSlashStatus();

    expect(agentListCalls()).toHaveLength(MAX_STATUS_AGENT_PAGES);
    const expected = Array.from(
      { length: MAX_STATUS_AGENT_PAGES },
      (_, i) => `  a${i + 1}: running`
    );
    expected.push('  … (list truncated)');
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain(`Project agents:\n${expected.join('\n')}`);
    });
  });

  it('does not note truncation when the last page has no nextCursor', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch
      .mockResolvedValueOnce(agentsPage([{ slug: 'a1', phase: 'running' }], 'c1'))
      .mockResolvedValueOnce(agentsPage([{ slug: 'a2', phase: 'running' }]));

    await (el as unknown as StatusInternals).handleSlashStatus();

    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('Project agents:\n  a1: running\n  a2: running');
    });
    expect(systemMessages(el).some((m) => m?.includes('list truncated'))).toBe(false);
  });

  it('fails without showing earlier pages when a follow-up page errors', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch
      .mockResolvedValueOnce(agentsPage([{ slug: 'a1', phase: 'running' }], 'c1'))
      .mockResolvedValueOnce({ ok: false, status: 500, json: () => Promise.resolve({}) })
      .mockResolvedValueOnce(agentsPage([{ slug: 'a2', phase: 'running' }]));

    await (el as unknown as StatusInternals).handleSlashStatus();

    expect(agentListCalls()).toEqual([
      '/api/v1/agents?projectId=proj-1',
      '/api/v1/agents?projectId=proj-1&cursor=c1',
    ]);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('Failed to fetch project status.');
    });
    expect(systemMessages(el).some((m) => m?.startsWith('Project agents:'))).toBe(false);
  });

  /**
   * In a chat-page DM, `projectId` is only the inherited project (whatever
   * the user viewed before opening the DM). Like /stop, /status must list
   * the DM peer agent's own project instead.
   */
  it('in a DM, lists the peer agent project, not the inherited projectId', async () => {
    fakeStateManager.setAgent('coder', 'proj-peer');
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = 'dm:agent:coder:user:u1';
    el.isDM = true;
    // The previously viewed project; a DM's /status must never use it.
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValueOnce(agentsPage([{ slug: 'coder', phase: 'running' }]));

    await (el as unknown as StatusInternals).handleSlashStatus();

    expect(agentListCalls()).toEqual(['/api/v1/agents?projectId=proj-peer']);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('Project agents:\n  coder: running');
    });
  });

  it('in a DM with no peer project, sends no list request', async () => {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = 'dm:agent:unknown-agent:user:u1';
    el.isDM = true;
    // Non-empty, to prove it is never used as a fallback in a DM.
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());

    await (el as unknown as StatusInternals).handleSlashStatus();

    expect(agentListCalls()).toEqual([]);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('No project context available.');
    });
  });

  it('shows the empty-project message when the response has no agents', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce(agentsPage([]));

    await (el as unknown as StatusInternals).handleSlashStatus();

    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('No agents found in this project.');
    });
  });
});

describe('scion-chat-thread gcs-link-click', () => {
  type GcsInternals = {
    filePreview: {
      kind: string;
      messageId: string;
      bucket: string;
      object: string;
      name: string;
    } | null;
    handleGcsLinkClick(
      e: CustomEvent<{ bucket: string; object: string; name: string; messageId: string }>
    ): void;
  };

  it('sets the preview target directly from the event detail, with no project resolution', async () => {
    const el = await mount();
    const internals = el as unknown as GcsInternals;

    internals.handleGcsLinkClick(
      new CustomEvent('gcs-link-click', {
        detail: {
          bucket: 'scion-xproject-exchange',
          object: 'workspace-volumes/dev-brief.md',
          name: 'dev-brief.md',
          messageId: 'msg-1',
        },
      })
    );

    expect(internals.filePreview).toEqual({
      kind: 'gcs',
      messageId: 'msg-1',
      bucket: 'scion-xproject-exchange',
      object: 'workspace-volumes/dev-brief.md',
      name: 'dev-brief.md',
    });
  });
});

describe('scion-chat-thread /spawn slash command', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
    fakeStateManager.clearAgents();
  });

  type SpawnInternals = { handleSlashSpawn(args: string): Promise<void> };

  function systemMessages(el: ScionChatThread): Array<string | null> {
    return Array.from(el.shadowRoot?.querySelectorAll('scion-chat-system-line') ?? []).map((l) =>
      l.getAttribute('message')
    );
  }

  function createCalls(): Array<Record<string, unknown>> {
    return apiFetch.mock.calls
      .filter((c) => c[0] === '/api/v1/agents' && c[1]?.method === 'POST')
      .map((c) => JSON.parse(String(c[1].body)) as Record<string, unknown>);
  }

  function created(agent: { slug: string; name: string }) {
    return { ok: true, status: 201, json: () => Promise.resolve({ agent }) };
  }

  /**
   * The hub's CreateAgentRequest (pkg/hub/handlers_agents_core.go) decodes
   * `projectId` and `name`, and rejects the request when either is empty.
   * A `project_id` key is silently ignored by the decoder.
   */
  it('posts name, projectId and template in the shape the hub decodes', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce(created({ slug: 'my-coder', name: 'my-coder' }));

    await (el as unknown as SpawnInternals).handleSlashSpawn('coder my-coder');

    expect(createCalls()).toEqual([{ name: 'my-coder', projectId: 'proj-1', template: 'coder' }]);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('Agent "my-coder" spawned successfully.');
    });
  });

  it('defaults the name to the template plus a short random suffix', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce(created({ slug: 'coder-ab12', name: 'coder-ab12' }));

    await (el as unknown as SpawnInternals).handleSlashSpawn('coder');

    const calls = createCalls();
    expect(calls).toHaveLength(1);
    expect(calls[0]?.projectId).toBe('proj-1');
    expect(calls[0]?.template).toBe('coder');
    expect(String(calls[0]?.name)).toMatch(/^coder-[a-z0-9]{4}$/);
  });

  /**
   * The hub rejects names over 63 runes. A long template is truncated so
   * the default name fits, without leaving a hyphen before the suffix. A
   * short base36 float still yields a 4-character suffix.
   */
  it('truncates a long template so the default name fits 63 runes', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce(created({ slug: 'x', name: 'x' }));
    const random = vi.spyOn(Math, 'random').mockReturnValue(0.5);
    const template = `${'a'.repeat(57)}-${'b'.repeat(12)}`;

    try {
      await (el as unknown as SpawnInternals).handleSlashSpawn(template);
    } finally {
      random.mockRestore();
    }

    const calls = createCalls();
    expect(calls).toHaveLength(1);
    expect(calls[0]?.template).toBe(template);
    expect(calls[0]?.name).toBe(`${'a'.repeat(57)}-i000`);
  });

  /**
   * The suffix is exactly four base36 characters at the extremes of
   * Math.random: a tiny value pads with leading zeros, a value just
   * below 1 maps to the largest suffix.
   */
  it.each([
    [1e-10, '0000'],
    [0, '0000'],
    [1 - Number.EPSILON, 'zzzz'],
  ])('keeps a 4-char base36 suffix when Math.random is %s', async (value, suffix) => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce(created({ slug: 'x', name: 'x' }));
    const random = vi.spyOn(Math, 'random').mockReturnValue(value);

    try {
      await (el as unknown as SpawnInternals).handleSlashSpawn('coder');
    } finally {
      random.mockRestore();
    }

    const name = String(createCalls()[0]?.name);
    expect(name).toMatch(/^coder-[0-9a-z]{4}$/);
    expect(name).toBe(`coder-${suffix}`);
  });

  /** The hub returns `{ agent }`; the reported name is the agent's slug. */
  it('reports the slug from the wrapped agent in the response', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce(created({ slug: 'hub-slug', name: 'Hub Slug' }));

    await (el as unknown as SpawnInternals).handleSlashSpawn('coder Hub-Slug');

    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('Agent "hub-slug" spawned successfully.');
    });
  });

  it('shows usage and sends nothing for missing or extra arguments', async () => {
    const el = await mount();
    el.projectId = 'proj-1';

    await (el as unknown as SpawnInternals).handleSlashSpawn('  ');
    await (el as unknown as SpawnInternals).handleSlashSpawn('coder a b');

    expect(createCalls()).toEqual([]);
    await vi.waitFor(() => {
      expect(
        systemMessages(el).filter((m) => m === 'Usage: /spawn <template> [name]')
      ).toHaveLength(2);
    });
  });

  it('shows a failure message and no success on a non-2xx response', async () => {
    const el = await mount();
    el.projectId = 'proj-1';
    apiFetch.mockResolvedValueOnce({ ok: false, status: 409, json: () => Promise.resolve({}) });

    await (el as unknown as SpawnInternals).handleSlashSpawn('coder x');

    await vi.waitFor(() => {
      expect(systemMessages(el).some((m) => m?.startsWith('Failed to spawn agent'))).toBe(true);
    });
    expect(systemMessages(el).some((m) => m?.includes('spawned successfully'))).toBe(false);
  });

  /**
   * In a chat-page DM, `projectId` is only the inherited project (whatever
   * the user viewed before opening the DM). Like /stop, /spawn must create
   * the agent in the DM peer agent's own project instead.
   */
  it('in a DM, spawns into the peer agent project, not the inherited projectId', async () => {
    fakeStateManager.setAgent('coder', 'proj-peer');
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = 'dm:agent:coder:user:u1';
    el.isDM = true;
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValueOnce(created({ slug: 'helper', name: 'helper' }));

    await (el as unknown as SpawnInternals).handleSlashSpawn('coder helper');

    expect(createCalls()).toEqual([{ name: 'helper', projectId: 'proj-peer', template: 'coder' }]);
  });

  it('in a DM with no peer project, sends nothing and shows the local message', async () => {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = 'dm:agent:unknown-agent:user:u1';
    el.isDM = true;
    el.projectId = 'proj-inherited';
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());

    await (el as unknown as SpawnInternals).handleSlashSpawn('coder helper');

    expect(createCalls()).toEqual([]);
    await vi.waitFor(() => {
      expect(systemMessages(el)).toContain('No project context available.');
    });
  });
});

describe('scion-chat-thread inter-agent markers', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('fetches inter-agent exchanges without raising the access-denied toast', async () => {
    // Members lack agent.attach on agents they did not create, so this
    // optional fetch 403s for them; it must fail quietly.
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.isDM = true;
    el.conversationKey = 'dm:agent:agent-1:user:user-1';
    document.body.appendChild(el);
    await el.updateComplete;

    await vi.waitFor(() =>
      expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('/interagent?'))).toBe(true)
    );
    const call = apiFetch.mock.calls.find((c) => String(c[0]).includes('/interagent?'))!;
    expect((call[1] as { suppressAccessDeniedToast?: boolean })?.suppressAccessDeniedToast).toBe(
      true
    );
  });
});

describe('export timestamps in the display zone (tz-refactor task 21)', () => {
  afterEach(() => {
    setPreferredTimeZone('');
    vi.useRealTimers();
  });

  it('formats each exported message time 24-hour in the display zone, naming the zone', () => {
    // vitest pins the browser zone to UTC; 15:00Z is midnight in Tokyo.
    setPreferredTimeZone('Asia/Tokyo');
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const el = document.createElement('scion-chat-thread') as any;
    expect(el.formatExportTimestamp('2026-09-23T15:00:00Z')).toBe(
      'Sep 24, 2026, 00:00 (Asia/Tokyo)'
    );
    expect(el.formatExportTimestamp('not-a-date')).toBe('not-a-date');
  });

  it('stamps the export filename with the display-zone date', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-09-23T15:30:00Z'));
    setPreferredTimeZone('Asia/Tokyo');
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const el = document.createElement('scion-chat-thread') as any;
    expect(el.filenameDateStamp()).toBe('2026-09-24');
    setPreferredTimeZone('America/New_York');
    expect(el.filenameDateStamp()).toBe('2026-09-23');
  });
});

/**
 * Switching chat to the dashboard and back builds a fresh thread; the page
 * hands it the previous instance's scroll anchor (`restoreScrollAnchor`) and
 * reads the live one back (`scrollAnchor`) when it is torn down.
 */
describe('scion-chat-thread scroll position hand-over', () => {
  const ROW_HEIGHT = 100;
  const ROWS = 10;
  const VIEWPORT = 300;
  const history = Array.from({ length: ROWS }, (_, i) => ({
    id: `m${i}`,
    sender: 'them@example.com',
    msg: `message ${i}`,
    createdAt: `2026-01-01T00:0${i}:00Z`,
  }));
  const older = {
    id: 'old-1',
    sender: 'them@example.com',
    msg: 'from an older page',
    createdAt: '2025-12-31T00:00:00Z',
  };

  const originalGetBoundingClientRect = HTMLElement.prototype.getBoundingClientRect;
  const originalScrollHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'scrollHeight'
  );
  const originalClientHeight = Object.getOwnPropertyDescriptor(
    HTMLElement.prototype,
    'clientHeight'
  );
  let hidden = false;
  /** Extra px every row is pushed down by (simulates a late layout shift). */
  let shift = 0;
  /** Own read watermark served by the `/read` endpoint ('' = none). */
  let lastRead = '';

  /** The thread's scroller, found from any element inside its shadow root. */
  function scrollerOf(el: Element): HTMLElement | null {
    const root = el.getRootNode() as ShadowRoot;
    return root.querySelector?.('.messages-scroll') ?? null;
  }

  /** happy-dom has no layout: lay rows out at 100px each, list scrolled by scrollTop. */
  function rect(top: number, height: number): DOMRect {
    return {
      top,
      bottom: top + height,
      height,
      left: 0,
      right: 0,
      width: 0,
      x: 0,
      y: top,
      toJSON: () => ({}),
    } as DOMRect;
  }

  beforeEach(() => {
    hidden = false;
    shift = 0;
    lastRead = '';
    apiFetch.mockReset();
    apiFetch.mockImplementation((url: string) =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve(
            String(url).endsWith('/read')
              ? lastRead
                ? { lastReadMessageId: lastRead }
                : {}
              : { items: String(url).includes('around=old-1') ? [older, ...history] : history }
          ),
      } as unknown as Response)
    );
    Object.defineProperty(HTMLElement.prototype, 'getBoundingClientRect', {
      configurable: true,
      value: function (this: HTMLElement) {
        if (hidden) return rect(0, 0);
        if (this.classList.contains('messages-scroll')) return rect(0, VIEWPORT);
        const match = /^msg-(.+)$/.exec(this.id);
        if (match) {
          const scroller = scrollerOf(this);
          const rows = Array.from(scroller?.querySelectorAll('[id^="msg-"]') ?? []);
          const index = rows.indexOf(this);
          return rect(index * ROW_HEIGHT + shift - (scroller?.scrollTop ?? 0), ROW_HEIGHT);
        }
        return originalGetBoundingClientRect.call(this);
      },
    });
    Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
      configurable: true,
      get(this: HTMLElement) {
        return (this.querySelectorAll('[id^="msg-"]').length || ROWS) * ROW_HEIGHT;
      },
    });
    Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
      configurable: true,
      get: () => VIEWPORT,
    });
  });

  afterEach(() => {
    Object.defineProperty(HTMLElement.prototype, 'getBoundingClientRect', {
      configurable: true,
      value: originalGetBoundingClientRect,
    });
    for (const [prop, descriptor] of [
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

  async function mountWith(
    anchor: import('./chat-scroll-anchor.js').ChatScrollAnchor | null
  ): Promise<{ el: ScionChatThread; scroller: () => HTMLElement }> {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.restoreScrollAnchor = anchor;
    document.body.appendChild(el);
    await vi.waitFor(() => expect(el.shadowRoot?.getElementById('msg-m9')).not.toBeNull());
    const scroller = (): HTMLElement =>
      el.shadowRoot!.querySelector('.messages-scroll') as HTMLElement;
    return { el, scroller };
  }

  const nextFrame = (): Promise<void> =>
    new Promise((resolve) => requestAnimationFrame(() => resolve()));

  it('puts the anchor message back at its recorded offset', async () => {
    const { scroller } = await mountWith({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'm4',
      offset: -30,
    });
    // m4's top is 400 - scrollTop; -30 means scrollTop 430.
    await vi.waitFor(() => expect(scroller().scrollTop).toBe(430));
  });

  it('loads the history around an anchor message the latest page lacks', async () => {
    const { el, scroller } = await mountWith({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'old-1',
      offset: 0,
    });
    await vi.waitFor(() => expect(el.shadowRoot?.getElementById('msg-old-1')).not.toBeNull());
    expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('around=old-1'))).toBe(true);
    await vi.waitFor(() => expect(scroller().scrollTop).toBe(0));
  });

  it('follows the bottom when the previous view was following it', async () => {
    const { scroller } = await mountWith({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: true,
      messageId: 'm7',
      offset: 0,
    });
    await vi.waitFor(() => expect(scroller().scrollTop).toBe(ROWS * ROW_HEIGHT));
  });

  it("ignores another conversation's anchor (e.g. a jump to an agent DM)", async () => {
    const { scroller } = await mountWith({
      conversationKey: 'dm:agent:a:user:u',
      pinnedToBottom: false,
      messageId: 'm2',
      offset: 0,
    });
    await vi.waitFor(() => expect(scroller().scrollTop).toBe(ROWS * ROW_HEIGHT));
  });

  it('applies an anchor once, not on a later return to the conversation', async () => {
    const { el, scroller } = await mountWith({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'm4',
      offset: 0,
    });
    await vi.waitFor(() => expect(scroller().scrollTop).toBe(400));
    el.conversationKey = 'topic-2';
    await el.updateComplete;
    el.conversationKey = CONVERSATION_KEY;
    await vi.waitFor(() => expect(el.shadowRoot?.getElementById('msg-m9')).not.toBeNull());
    await vi.waitFor(() => expect(scroller().scrollTop).toBe(ROWS * ROW_HEIGHT));
  });

  it('reports the topmost visible message after a scroll', async () => {
    const { el, scroller } = await mountWith(null);
    scroller().scrollTop = 250;
    scroller().dispatchEvent(new Event('scroll'));
    await nextFrame();
    expect(el.scrollAnchor).toEqual({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'm2',
      offset: -50,
    });
  });

  it('keeps the last good position while hidden, and after detaching', async () => {
    const { el, scroller } = await mountWith(null);
    scroller().scrollTop = 250;
    scroller().dispatchEvent(new Event('scroll'));
    await nextFrame();
    hidden = true;
    scroller().dispatchEvent(new Event('scroll'));
    await nextFrame();
    el.remove();
    expect(el.scrollAnchor?.messageId).toBe('m2');
  });

  it('meets messages that arrived while away at the unread divider, not the bottom', async () => {
    lastRead = 'm5';
    const { el, scroller } = await mountWith({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: true,
      messageId: 'm9',
      offset: 0,
    });
    const internals = el as unknown as { _unreadAnchorActive: boolean };
    await vi.waitFor(() => expect(internals._unreadAnchorActive).toBe(true));
    expect(scroller().scrollTop).not.toBe(ROWS * ROW_HEIGHT);
  });

  it('keeps a mid-thread position even when there are unread messages', async () => {
    lastRead = 'm5';
    const { el, scroller } = await mountWith({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'm4',
      offset: 0,
    });
    await vi.waitFor(() => expect(scroller().scrollTop).toBe(400));
    expect((el as unknown as { _unreadAnchorActive: boolean })._unreadAnchorActive).toBe(false);
  });

  it('lets a #msg- permalink win over the anchor, and uses the anchor up', async () => {
    window.history.replaceState({}, '', '#msg-m2');
    try {
      const anchor = {
        conversationKey: CONVERSATION_KEY,
        pinnedToBottom: false,
        messageId: 'm4',
        offset: 0,
      };
      const el = document.createElement('scion-chat-thread') as ScionChatThread;
      const jump = vi.spyOn(el, 'scrollToMessageById').mockResolvedValue();
      el.conversationKey = CONVERSATION_KEY;
      el.restoreScrollAnchor = anchor;
      document.body.appendChild(el);
      await vi.waitFor(() => expect(jump).toHaveBeenCalledWith('m2', true));
      const scroller = el.shadowRoot!.querySelector('.messages-scroll') as HTMLElement;
      expect(scroller.scrollTop).not.toBe(400);
      expect((el as unknown as { _usedRestoreAnchor: unknown })._usedRestoreAnchor).toBe(anchor);
      expect(el.scrollAnchor?.messageId).not.toBe('m4');
    } finally {
      window.history.replaceState({}, '', window.location.pathname);
    }
  });

  it('announces once that it took the anchor, so the page stops offering it', async () => {
    const anchor = {
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'm4',
      offset: 0,
    };
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    const consumed: unknown[] = [];
    el.addEventListener('scroll-restore-consumed', (e) => consumed.push((e as CustomEvent).detail));
    el.conversationKey = CONVERSATION_KEY;
    el.restoreScrollAnchor = anchor;
    document.body.appendChild(el);
    await vi.waitFor(() => expect(consumed).toEqual([anchor]));
    // A later re-load of the same conversation does not take it again.
    el.conversationKey = 'topic-2';
    await el.updateComplete;
    el.conversationKey = CONVERSATION_KEY;
    await vi.waitFor(() => expect(el.shadowRoot?.getElementById('msg-m9')).not.toBeNull());
    expect(consumed).toHaveLength(1);
  });

  it('a jump made before the load finishes uses the anchor up instead of restoring it', async () => {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    el.restoreScrollAnchor = {
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'm4',
      offset: 0,
    };
    document.body.appendChild(el);
    void el.scrollToMessageById('m2', false);
    await vi.waitFor(() => expect(el.shadowRoot?.getElementById('msg-m9')).not.toBeNull());
    await new Promise((resolve) => setTimeout(resolve, 50));
    const scroller = el.shadowRoot!.querySelector('.messages-scroll') as HTMLElement;
    expect(scroller.scrollTop).not.toBe(400);
    // A position this view never showed is not handed on either.
    el.remove();
    expect(el.scrollAnchor?.messageId).not.toBe('m4');
  });

  it('a jump made while the anchor is still being located stands the restore down', async () => {
    let releaseAround: () => void = () => {};
    apiFetch.mockImplementation(async (url: string) => {
      if (String(url).includes('around=old-1')) {
        await new Promise<void>((resolve) => (releaseAround = resolve));
      }
      return {
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve(
            String(url).endsWith('/read')
              ? {}
              : { items: String(url).includes('around=old-1') ? [older, ...history] : history }
          ),
      } as unknown as Response;
    });
    const { el, scroller } = await mountWith({
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'old-1',
      offset: -50,
    });
    await vi.waitFor(() =>
      expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('around=old-1'))).toBe(true)
    );
    // e.g. a search result in this same conversation
    void el.scrollToMessageById('m2', false);
    scroller().scrollTop = 123;
    releaseAround();
    await new Promise((resolve) => setTimeout(resolve, 50));
    await el.updateComplete;
    // The restore would have written 50 (old-1 at -50px).
    expect(scroller().scrollTop).not.toBe(50);
    // Its late history window is dropped too, so it cannot replace the
    // messages the jump is showing.
    expect(el.shadowRoot?.getElementById('msg-old-1')).toBeNull();
    expect(el.shadowRoot?.getElementById('msg-m2')).not.toBeNull();
  });

  it('hands on the restore target if the user leaves before it lands', async () => {
    let releaseAround: () => void = () => {};
    apiFetch.mockImplementation(async (url: string) => {
      if (String(url).includes('around=old-1')) {
        await new Promise<void>((resolve) => (releaseAround = resolve));
      }
      return {
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve(
            String(url).endsWith('/read')
              ? {}
              : { items: String(url).includes('around=old-1') ? [older, ...history] : history }
          ),
      } as unknown as Response;
    });
    const anchor = {
      conversationKey: CONVERSATION_KEY,
      pinnedToBottom: false,
      messageId: 'old-1',
      offset: -50,
    };
    const { el } = await mountWith(anchor);
    await vi.waitFor(() =>
      expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('around=old-1'))).toBe(true)
    );
    // Slow network: the user switches to the dashboard now.
    el.remove();
    expect(el.scrollAnchor).toEqual(anchor);
    releaseAround();
  });

  describe('holding the restored position against late layout shifts', () => {
    const OriginalResizeObserver = globalThis.ResizeObserver;
    let observers: Array<{ fire: () => void; disconnected: boolean }>;

    beforeEach(() => {
      observers = [];
      globalThis.ResizeObserver = class {
        private entry: { fire: () => void; disconnected: boolean };
        constructor(callback: ResizeObserverCallback) {
          this.entry = {
            fire: () => callback([], this as unknown as ResizeObserver),
            disconnected: false,
          };
          observers.push(this.entry);
        }
        observe(): void {}
        unobserve(): void {}
        disconnect(): void {
          this.entry.disconnected = true;
        }
      } as unknown as typeof ResizeObserver;
    });

    afterEach(() => {
      globalThis.ResizeObserver = OriginalResizeObserver;
    });

    async function restored(): Promise<{
      scroller: () => HTMLElement;
      watch: () => { fire: () => void; disconnected: boolean };
    }> {
      const { scroller } = await mountWith({
        conversationKey: CONVERSATION_KEY,
        pinnedToBottom: false,
        messageId: 'm4',
        offset: -30,
      });
      await vi.waitFor(() => expect(scroller().scrollTop).toBe(430));
      return { scroller, watch: () => observers[observers.length - 1] };
    }

    it('re-applies the anchor when rows above it grow', async () => {
      const { scroller, watch } = await restored();
      shift = 200; // e.g. inter-agent exchanges rendered above the anchor
      watch().fire();
      expect(scroller().scrollTop).toBe(630);
    });

    it('stops at the first user scroll', async () => {
      const { scroller, watch } = await restored();
      scroller().scrollTop = 100; // the user scrolls away
      shift = 200;
      watch().fire();
      expect(scroller().scrollTop).toBe(100);
      expect(watch().disconnected).toBe(true);
    });

    it('rides out sub-pixel scrollTop drift (fractional DPR)', async () => {
      const { scroller, watch } = await restored();
      scroller().scrollTop = 430.5; // rounding, not a user scroll
      shift = 200;
      watch().fire();
      expect(watch().disconnected).toBe(false);
      expect(scroller().scrollTop).toBe(630);
    });

    it('still stops at a real scroll of a few dozen pixels', async () => {
      const { scroller, watch } = await restored();
      scroller().scrollTop = 470; // the user scrolls 40px
      shift = 200;
      watch().fire();
      expect(scroller().scrollTop).toBe(470);
      expect(watch().disconnected).toBe(true);
    });

    it('stops after a short settle window', async () => {
      const { watch } = await restored();
      expect(watch().disconnected).toBe(false);
      await new Promise((resolve) => setTimeout(resolve, 600));
      expect(watch().disconnected).toBe(true);
    });
  });

  it('forgets the position when switching conversations', async () => {
    const { el, scroller } = await mountWith(null);
    scroller().scrollTop = 250;
    scroller().dispatchEvent(new Event('scroll'));
    await nextFrame();
    el.conversationKey = 'topic-2';
    await el.updateComplete;
    expect(el.scrollAnchor).toBeNull();
  });
});

describe('scion-chat-thread work finishing after a conversation switch', () => {
  type Internals = {
    loading: boolean;
    loadOlderMessagesV2(scrollEl: HTMLElement): Promise<void>;
    loadingOlder: boolean;
    error: string | null;
    handleJumpToLatest(): Promise<void>;
    handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
    startStreamV2(): void;
    viewingAroundMessage: boolean;
    pinnedToBottom: boolean;
    composerReplyTo: { messageId: string; senderName: string; content: string } | null;
    sendError: string | null;
  };

  const REPLY_TO = { messageId: 'm-1', senderName: 'Ada', content: 'hello' };

  async function flush(): Promise<void> {
    for (let i = 0; i < 5; i++) await new Promise((r) => setTimeout(r, 0));
  }

  /** The next request answers at once, but its body waits for `release`. */
  function holdNextBody(): { release: (body: unknown) => void } {
    const held = { release: (_body: unknown): void => {} };
    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: () =>
          new Promise((resolve) => {
            held.release = resolve;
          }),
      } as unknown as Response)
    );
    return held;
  }

  function stalePage(id: string): unknown {
    return {
      items: [
        {
          id,
          projectId: '',
          sender: 'agent:coder',
          senderId: 'agent-1',
          recipient: '',
          recipientId: '',
          msg: 'old thread',
          type: 'chat',
          agentId: '',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    };
  }

  function send(el: ScionChatThread): Promise<void> {
    return (el as unknown as Internals).handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: 'hi',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          onError: vi.fn(),
          mentions: [],
          attachmentIds: [],
        },
      })
    );
  }

  async function switchConversation(el: ScionChatThread): Promise<void> {
    el.conversationKey = 'other-thread';
    await el.updateComplete;
  }

  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
    // Tests wait for this mock to be called; a call left over from an
    // earlier test must not satisfy that wait.
    extractApiErrorMock.mockClear();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('a stale initial load leaves the next conversation loading, its anchor and stream alone', async () => {
    const held = holdNextBody();
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    // The next conversation's own history stays pending.
    apiFetch.mockImplementation(() => new Promise(() => {}));
    const consumed = vi.fn();
    el.addEventListener('scroll-restore-consumed', consumed);
    el.restoreScrollAnchor = {
      conversationKey: 'other-thread',
      pinnedToBottom: false,
      messageId: 'x',
      offset: 0,
    };
    await switchConversation(el);
    const internals = el as unknown as Internals;
    const startStream = vi.spyOn(internals, 'startStreamV2');
    expect(internals.loading).toBe(true);

    held.release(stalePage('stale-1'));
    await flush();

    expect(startStream).not.toHaveBeenCalled();
    expect(internals.loading).toBe(true);
    expect(consumed).not.toHaveBeenCalled();
  });

  it('a stale initial load error is not shown on the next conversation', async () => {
    let releaseError: (msg: string) => void = () => {};
    extractApiErrorMock.mockImplementationOnce(
      () => new Promise<string>((resolve) => (releaseError = resolve))
    );
    apiFetch.mockResolvedValueOnce({ ok: false, status: 500 } as unknown as Response);
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await el.updateComplete;
    await vi.waitFor(() => expect(extractApiErrorMock).toHaveBeenCalled());
    // The next conversation's own history stays pending.
    apiFetch.mockImplementation(() => new Promise(() => {}));
    await switchConversation(el);

    releaseError('stale failure');
    await flush();

    expect((el as unknown as { error: string | null }).error).toBeNull();
  });

  it('an initial load error in the same conversation is shown', async () => {
    apiFetch.mockResolvedValueOnce({ ok: false, status: 500 } as unknown as Response);
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    await el.updateComplete;

    await vi.waitFor(() => expect((el as unknown as { error: string | null }).error).toBe('error'));
  });

  it('a stale older page does not shift the new conversation’s scroll position', async () => {
    const el = await mount();
    const held = holdNextBody();
    let height = 100;
    const scrollEl = {
      scrollTop: 50,
      get scrollHeight(): number {
        return height;
      },
    } as unknown as HTMLElement;
    const loading = (el as unknown as Internals).loadOlderMessagesV2(scrollEl);
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    await switchConversation(el);
    height = 400;
    // The new conversation is loading its own older page.
    (el as unknown as Internals).loadingOlder = true;

    held.release(stalePage('stale-older'));
    await loading;

    expect(scrollEl.scrollTop).toBe(50);
    expect((el as unknown as Internals).loadingOlder).toBe(true);
  });

  it('an older page failing after a switch leaves the new conversation’s spinner and scroll', async () => {
    const el = await mount();
    let rejectBody: (err: Error) => void = () => {};
    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: () => new Promise((_, reject) => (rejectBody = reject)),
      } as unknown as Response)
    );
    let height = 100;
    const scrollEl = {
      scrollTop: 50,
      get scrollHeight(): number {
        return height;
      },
    } as unknown as HTMLElement;
    const internals = el as unknown as Internals;
    const loading = internals.loadOlderMessagesV2(scrollEl);
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    await switchConversation(el);
    height = 400;
    internals.loadingOlder = true;

    rejectBody(new Error('stream reset'));
    await loading;

    expect(scrollEl.scrollTop).toBe(50);
    expect(internals.loadingOlder).toBe(true);
  });

  it('a stale jump to latest leaves the new conversation’s view alone', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.viewingAroundMessage = true;
    const held = holdNextBody();
    const jumping = internals.handleJumpToLatest();
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    await switchConversation(el);
    await flush();
    // The new conversation opened on a message further up its history.
    internals.viewingAroundMessage = true;
    internals.pinnedToBottom = false;

    held.release(stalePage('stale-latest'));
    await jumping;

    expect(internals.viewingAroundMessage).toBe(true);
    expect(internals.pinnedToBottom).toBe(false);
  });

  it('a jump to latest whose request fails after a switch leaves the new view alone', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.viewingAroundMessage = true;
    let rejectFetch: (err: Error) => void = () => {};
    apiFetch.mockImplementationOnce(() => new Promise((_, reject) => (rejectFetch = reject)));
    const jumping = internals.handleJumpToLatest();
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    await switchConversation(el);
    await flush();
    internals.viewingAroundMessage = true;

    rejectFetch(new Error('offline'));
    await jumping;

    expect(internals.error).toBeNull();
    expect(internals.pinnedToBottom).toBe(true);
    expect(internals.viewingAroundMessage).toBe(true);
  });

  it('a jump to latest whose error is read after a switch leaves the new view alone', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.viewingAroundMessage = true;
    let releaseError: (msg: string) => void = () => {};
    extractApiErrorMock.mockImplementationOnce(
      () => new Promise<string>((resolve) => (releaseError = resolve))
    );
    apiFetch.mockResolvedValueOnce({ ok: false, status: 500 } as unknown as Response);
    const jumping = internals.handleJumpToLatest();
    await vi.waitFor(() => expect(extractApiErrorMock).toHaveBeenCalled());
    await switchConversation(el);
    await flush();
    internals.viewingAroundMessage = true;

    releaseError('stale failure');
    await jumping;

    expect(internals.error).toBeNull();
    expect(internals.pinnedToBottom).toBe(true);
    expect(internals.viewingAroundMessage).toBe(true);
  });

  it('a send refused after a switch puts no reply bar or error on the new conversation', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.composerReplyTo = REPLY_TO;
    let resolveSend!: (value: unknown) => void;
    apiFetch.mockImplementationOnce(() => new Promise((resolve) => (resolveSend = resolve)));
    const sending = send(el);
    await switchConversation(el);

    resolveSend({ ok: false, status: 500, json: () => Promise.resolve({}) });
    await sending;

    expect(internals.composerReplyTo).toBeNull();
    expect(internals.sendError).toBeNull();
  });

  it('a send that throws after a switch puts no reply bar or error on the new conversation', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.composerReplyTo = REPLY_TO;
    let rejectSend!: (err: Error) => void;
    apiFetch.mockImplementationOnce(() => new Promise((_, reject) => (rejectSend = reject)));
    const sending = send(el);
    await switchConversation(el);

    rejectSend(new Error('offline'));
    await sending;

    expect(internals.composerReplyTo).toBeNull();
    expect(internals.sendError).toBeNull();
  });

  it('a send whose error is read after a switch shows no error on the new conversation', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    let releaseError: (msg: string) => void = () => {};
    extractApiErrorMock.mockImplementationOnce(
      () => new Promise<string>((resolve) => (releaseError = resolve))
    );
    apiFetch.mockResolvedValueOnce({ ok: false, status: 500 } as unknown as Response);
    const sending = send(el);
    await vi.waitFor(() => expect(extractApiErrorMock).toHaveBeenCalled());
    await switchConversation(el);

    releaseError('stale failure');
    await sending;

    expect(internals.sendError).toBeNull();
  });

  it('a reply picked while a refused send reads its error is kept', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.composerReplyTo = REPLY_TO;
    let releaseError: (msg: string) => void = () => {};
    extractApiErrorMock.mockImplementationOnce(
      () => new Promise<string>((resolve) => (releaseError = resolve))
    );
    apiFetch.mockResolvedValueOnce({ ok: false, status: 500 } as unknown as Response);
    const sending = send(el);
    await vi.waitFor(() => expect(extractApiErrorMock).toHaveBeenCalled());
    const picked = { messageId: 'm-2', senderName: 'Bob', content: 'later' };
    internals.composerReplyTo = picked;

    releaseError('refused');
    await sending;

    expect(internals.composerReplyTo).toEqual(picked);
    expect(internals.sendError).toBe('refused');
  });

  it('a send refused in the same conversation restores the reply bar and shows the error', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.composerReplyTo = REPLY_TO;
    apiFetch.mockResolvedValueOnce({ ok: false, status: 500, json: () => Promise.resolve({}) });

    await send(el);

    expect(internals.composerReplyTo).toEqual(REPLY_TO);
    expect(internals.sendError).toBe('error');
  });

  it('a send that throws in the same conversation restores the reply bar and shows the error', async () => {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.composerReplyTo = REPLY_TO;
    apiFetch.mockRejectedValueOnce(new Error('offline'));

    await send(el);

    expect(internals.composerReplyTo).toEqual(REPLY_TO);
    expect(internals.sendError).toBe('offline');
  });
});

/**
 * A jump into the conversation already open (a search result) re-mounts the
 * thread and starts while its initial load is still running. The load must
 * not then fall back to the unread divider or the bottom over the jump, and
 * an earlier, slower jump must not land over a newer one.
 */
describe('scion-chat-thread jump during the initial load', () => {
  type Internals = {
    scrollToUnreadDivider(): void;
    scrollToBottomAfterRender(): void;
  };

  function msg(id: string, minute: number): Message {
    return {
      id,
      sender: 'them@example.com',
      msg: id,
      createdAt: `2026-01-01T00:${String(minute).padStart(2, '0')}:00Z`,
    } as unknown as Message;
  }

  const LATEST = [msg('latest-1', 50), msg('latest-2', 51), msg('latest-3', 52)];

  function page(items: Message[], extra: Record<string, unknown> = {}): Response {
    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve({ items, ...extra }),
    } as unknown as Response;
  }

  function around(id: string, minute: number): Response {
    return page(
      [msg(`${id}-before`, minute - 1), msg(id, minute), msg(`${id}-after`, minute + 1)],
      {
        nextCursor: `${id}-cursor`,
      }
    );
  }

  /** A response promise and the function that settles it. */
  function deferred(): { promise: Promise<Response>; resolve: (r: Response) => void } {
    let resolve: (r: Response) => void = () => {};
    const promise = new Promise<Response>((r) => {
      resolve = r;
    });
    return { promise, resolve };
  }

  async function flush(): Promise<void> {
    for (let i = 0; i < 6; i++) await new Promise((r) => setTimeout(r, 0));
  }

  let scrolledTo: string[];
  let lastReadMessageId: string;
  const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;

  /** Route requests: held ones by key, the rest answered at once. */
  let held: Map<string, Promise<Response>>;
  function routeKey(url: string): string {
    if (url.includes('around=')) return `around:${url.split('around=')[1].split('&')[0]}`;
    if (url.includes('/messages?')) return 'history';
    return url.endsWith('/read') ? 'read' : 'other';
  }

  function hold(key: string): (r: Response) => void {
    const d = deferred();
    held.set(key, d.promise);
    return d.resolve;
  }

  /** Create the thread with spies on its fallback scrolls, then mount it. */
  function create(): {
    el: ScionChatThread;
    unread: ReturnType<typeof vi.fn>;
    bottom: ReturnType<typeof vi.fn>;
  } {
    const el = document.createElement('scion-chat-thread') as ScionChatThread;
    const internals = el as unknown as Internals;
    const unread = vi.spyOn(internals, 'scrollToUnreadDivider') as unknown as ReturnType<
      typeof vi.fn
    >;
    const bottom = vi.spyOn(internals, 'scrollToBottomAfterRender') as unknown as ReturnType<
      typeof vi.fn
    >;
    el.conversationKey = CONVERSATION_KEY;
    document.body.appendChild(el);
    return { el, unread, bottom };
  }

  beforeEach(() => {
    held = new Map();
    scrolledTo = [];
    lastReadMessageId = 'latest-1';
    apiFetch.mockReset();
    apiFetch.mockImplementation((url: string) => {
      const key = routeKey(String(url));
      const pending = held.get(key);
      if (pending) {
        held.delete(key);
        return pending;
      }
      if (key === 'history') return Promise.resolve(page(LATEST));
      if (key === 'read') {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ lastReadMessageId }),
        } as unknown as Response);
      }
      if (key.startsWith('around:')) return Promise.resolve(around(key.slice(7), 10));
      return Promise.resolve(emptyHistory());
    });
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value(this: HTMLElement) {
        scrolledTo.push(this.id);
      },
    });
  });

  afterEach(() => {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
      configurable: true,
      value: originalScrollIntoView,
    });
    document.body.innerHTML = '';
  });

  function rendered(el: ScionChatThread, id: string): boolean {
    return el.shadowRoot?.querySelector(`#msg-${id}`) != null;
  }

  it.each([
    ['the jump page arrives first', true],
    ['the latest page arrives first', false],
  ])(
    'a jump made while the initial load is pending lands on its target (%s)',
    async (_name, jumpFirst) => {
      const releaseHistory = hold('history');
      const releaseAround = hold('around:target');
      const { el, unread, bottom } = create();
      await el.updateComplete;

      const jump = el.scrollToMessageById('target');
      await flush();
      if (jumpFirst) {
        releaseAround(around('target', 10));
        await flush();
        releaseHistory(page(LATEST));
      } else {
        releaseHistory(page(LATEST));
        await flush();
        releaseAround(around('target', 10));
      }
      await jump;
      await flush();
      await el.updateComplete;

      expect(rendered(el, 'target')).toBe(true);
      // The latest page is not spliced in after the jump window.
      expect(rendered(el, 'latest-3')).toBe(false);
      expect(scrolledTo.at(-1)).toBe('msg-target');
      expect(unread).not.toHaveBeenCalled();
      expect(bottom).not.toHaveBeenCalled();
    }
  );

  it.each([
    ['before the load finishes', true],
    ['after the load finishes', false],
  ])('a jump whose target is gone, answered %s, gives the view back', async (_name, jumpFirst) => {
    lastReadMessageId = '';
    const releaseHistory = hold('history');
    const releaseAround = hold('around:gone');
    const { el, unread, bottom } = create();
    await el.updateComplete;

    const jump = el.scrollToMessageById('gone');
    await flush();
    const missing = {
      ok: false,
      status: 404,
      json: () => Promise.resolve({}),
    } as unknown as Response;
    if (jumpFirst) {
      releaseAround(missing);
      await jump;
      releaseHistory(page(LATEST));
    } else {
      releaseHistory(page(LATEST));
      await flush();
      expect(bottom).not.toHaveBeenCalled();
      releaseAround(missing);
      await jump;
    }
    await flush();
    await el.updateComplete;

    expect(rendered(el, 'latest-3')).toBe(true);
    expect(unread).not.toHaveBeenCalled();
    expect(bottom).toHaveBeenCalledTimes(1);
  });

  it('a Retry after a failed load and a failed jump opens at the bottom', async () => {
    lastReadMessageId = '';
    const failed = {
      ok: false,
      status: 500,
      json: () => Promise.resolve({}),
    } as unknown as Response;
    const releaseHistory = hold('history');
    const releaseAround = hold('around:gone');
    const { el, bottom } = create();
    await el.updateComplete;

    const jump = el.scrollToMessageById('gone');
    await flush();
    releaseHistory(failed);
    await flush();
    releaseAround(failed);
    await jump;
    await flush();
    await el.updateComplete;

    const retry = el.shadowRoot?.querySelector('.state-msg sl-button') as HTMLElement | null;
    expect(retry).not.toBeNull();
    bottom.mockClear();
    retry?.click();
    await flush();
    await el.updateComplete;

    expect(rendered(el, 'latest-3')).toBe(true);
    expect(bottom).toHaveBeenCalledTimes(1);
    expect(el.shadowRoot?.querySelector('.permalink-highlight')).toBeNull();
  });

  it.each([
    ['while the load is running', true],
    ['after the load finishes', false],
  ])(
    'a newer jump that finds no target, %s, keeps a landed jump in view',
    async (_name, failDuringLoad) => {
      lastReadMessageId = '';
      const missing = {
        ok: false,
        status: 404,
        json: () => Promise.resolve({}),
      } as unknown as Response;
      const releaseHistory = hold('history');
      const releaseGone = hold('around:gone');
      const { el, unread, bottom } = create();
      await el.updateComplete;

      await el.scrollToMessageById('first');
      await el.updateComplete;
      expect(scrolledTo.at(-1)).toBe('msg-first');
      const gone = el.scrollToMessageById('gone');
      await flush();
      if (failDuringLoad) {
        releaseGone(missing);
        await gone;
        await flush();
        releaseHistory(page(LATEST));
      } else {
        releaseHistory(page(LATEST));
        await flush();
        releaseGone(missing);
        await gone;
      }
      await flush();
      await el.updateComplete;

      expect(rendered(el, 'first')).toBe(true);
      expect(rendered(el, 'latest-3')).toBe(false);
      expect(scrolledTo.at(-1)).toBe('msg-first');
      expect(unread).not.toHaveBeenCalled();
      expect(bottom).not.toHaveBeenCalled();
    }
  );

  it('a jump from before leaving and coming back leaves the reopened view alone', async () => {
    hold('history');
    const releaseAround = hold('around:latest-2');
    const { el, unread } = create();
    await el.updateComplete;

    const jump = el.scrollToMessageById('latest-2');
    await flush();
    // Away and straight back while the jump's page is still loading: the
    // reopened conversation runs its own load and opens at its divider.
    el.conversationKey = 'other-thread';
    await el.updateComplete;
    await flush();
    el.conversationKey = CONVERSATION_KEY;
    await el.updateComplete;
    await flush();
    await el.updateComplete;
    expect(rendered(el, 'latest-2')).toBe(true);
    // Opened at the divider (once for the other thread, once on return).
    expect(unread).toHaveBeenCalledTimes(2);
    scrolledTo = [];

    releaseAround(around('latest-2', 51));
    await jump;
    await flush();
    await el.updateComplete;

    expect(rendered(el, 'latest-3')).toBe(true);
    expect(scrolledTo).not.toContain('msg-latest-2');
    expect(el.shadowRoot?.querySelector('.permalink-highlight')).toBeNull();
  });

  it('a superseded jump that finds no target does not reset the view', async () => {
    lastReadMessageId = '';
    const missing = {
      ok: false,
      status: 404,
      json: () => Promise.resolve({}),
    } as unknown as Response;
    const releaseHistory = hold('history');
    const releaseFirst = hold('around:first');
    const releaseSecond = hold('around:second');
    const { el, bottom } = create();
    await el.updateComplete;

    const first = el.scrollToMessageById('first');
    await flush();
    const second = el.scrollToMessageById('second');
    await flush();
    releaseHistory(page(LATEST));
    await flush();
    releaseFirst(missing);
    await first;
    await flush();
    expect(bottom).not.toHaveBeenCalled();

    releaseSecond(around('second', 30));
    await second;
    await flush();
    await el.updateComplete;

    expect(rendered(el, 'second')).toBe(true);
    expect(scrolledTo.at(-1)).toBe('msg-second');
    expect(bottom).not.toHaveBeenCalled();
  });

  it('two overlapping jumps resolve to the newer one', async () => {
    const { el } = create();
    await flush();
    await el.updateComplete;

    const releaseFirst = hold('around:first');
    const first = el.scrollToMessageById('first');
    await flush();
    await el.scrollToMessageById('second');
    await el.updateComplete;
    expect(rendered(el, 'second')).toBe(true);

    releaseFirst(around('first', 30));
    await first;
    await flush();
    await el.updateComplete;

    expect(rendered(el, 'second')).toBe(true);
    expect(rendered(el, 'first')).toBe(false);
    expect(scrolledTo).not.toContain('msg-first');
    expect(scrolledTo.at(-1)).toBe('msg-second');
  });

  it.each([
    ['the unread divider', 'latest-1', 'unread'],
    ['the bottom', '', 'bottom'],
  ])('a load with no jump still opens at %s', async (_name, lastRead, expected) => {
    lastReadMessageId = lastRead;
    const { el, unread, bottom } = create();
    await flush();
    await el.updateComplete;

    expect(rendered(el, 'latest-3')).toBe(true);
    expect(unread).toHaveBeenCalledTimes(expected === 'unread' ? 1 : 0);
    expect(bottom).toHaveBeenCalledTimes(expected === 'bottom' ? 1 : 0);
  });
});

describe('scion-chat-thread older-page loads and message jumps', () => {
  type Internals = {
    loadOlderMessagesV2(scrollEl: HTMLElement): Promise<void>;
    messages: Array<{ id: string }>;
    nextCursor: string | null;
    _jumpSeq: number;
    _jumpScrollCleanup: (() => void) | null;
    pinnedToBottom: boolean;
  };

  /** The next request answers at once, but its body waits for `release`. */
  function holdNextBody(): { release: (body: unknown) => void } {
    const held = { release: (_body: unknown): void => {} };
    apiFetch.mockImplementationOnce(() =>
      Promise.resolve({
        ok: true,
        status: 200,
        json: () =>
          new Promise((resolve) => {
            held.release = resolve;
          }),
      } as unknown as Response)
    );
    return held;
  }

  function olderPage(id: string): unknown {
    return {
      items: [
        {
          id,
          projectId: '',
          sender: 'agent:coder',
          senderId: 'agent-1',
          recipient: '',
          recipientId: '',
          msg: 'older message',
          type: 'chat',
          agentId: '',
          createdAt: '2026-01-01T00:00:00Z',
        },
      ],
    };
  }

  /** A scroller whose content height grows from 100 to 400 once the page lands. */
  function fakeScroller(): { scrollEl: HTMLElement; grow: () => void } {
    let height = 100;
    const scrollEl = {
      scrollTop: 50,
      get scrollHeight(): number {
        return height;
      },
    } as unknown as HTMLElement;
    return { scrollEl, grow: (): void => void (height = 400) };
  }

  async function startOlderLoad(): Promise<{
    internals: Internals;
    scrollEl: HTMLElement;
    grow: () => void;
    held: { release: (body: unknown) => void };
    loading: Promise<void>;
  }> {
    const el = await mount();
    const internals = el as unknown as Internals;
    internals.nextCursor = 'older-cursor';
    const held = holdNextBody();
    const { scrollEl, grow } = fakeScroller();
    const loading = internals.loadOlderMessagesV2(scrollEl);
    await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
    return { internals, scrollEl, grow, held, loading };
  }

  beforeEach(() => {
    apiFetch.mockReset();
    apiFetch.mockResolvedValue(emptyHistory());
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('merges the older page and keeps the reader in place with no jump', async () => {
    const { internals, scrollEl, grow, held, loading } = await startOlderLoad();
    grow();
    held.release(olderPage('older-1'));
    await loading;

    expect(internals.messages.some((m) => m.id === 'older-1')).toBe(true);
    expect(scrollEl.scrollTop).toBe(350);
  });

  it('drops an older page whose load started before a jump', async () => {
    const { internals, scrollEl, grow, held, loading } = await startOlderLoad();
    // A jump starts while the older page is in flight.
    internals._jumpSeq++;
    grow();
    held.release(olderPage('older-1'));
    await loading;

    expect(internals.messages.some((m) => m.id === 'older-1')).toBe(false);
    expect(scrollEl.scrollTop).toBe(50);
  });

  it('does not write scrollTop while a jump scroll is settling', async () => {
    const { internals, scrollEl, grow, held, loading } = await startOlderLoad();
    // The jump's smooth scroll is in flight (its settle watcher is armed),
    // and the reader is no longer at the bottom.
    internals.pinnedToBottom = false;
    internals._jumpScrollCleanup = (): void => {};
    grow();
    held.release(olderPage('older-1'));
    await loading;

    expect(internals.messages.some((m) => m.id === 'older-1')).toBe(true);
    expect(scrollEl.scrollTop).toBe(50);
    internals._jumpScrollCleanup = null;
  });
});
