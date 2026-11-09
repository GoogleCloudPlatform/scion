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
 * A thread the caller may read but not post in: the history's
 * `_capabilities` without a post action (or, without them, `canPost`
 * false), or a send refused with chat_post_denied, replaces the composer
 * with a read-only notice whose text follows the hub's reason.
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

const stateManager = vi.hoisted(() =>
  Object.assign(new EventTarget(), {
    currentScope: null,
    isConnected: false,
    getAgent: (): undefined => undefined,
    seedAgents: (): void => {},
  })
);

const apiFetch = vi.hoisted(() => vi.fn<(path: string, init?: unknown) => Promise<Response>>());

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  stateManager,
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch,
  extractApiError: (): Promise<string> => Promise.resolve('error'),
}));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

function json(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

/** The history body each test serves for the conversation's messages. */
let history: Record<string, unknown>;
/** The response a send gets. */
let sendResponse: Response;

async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await new Promise((resolve) => setTimeout(resolve, 0));
}

async function open(key: string, isDM = false): Promise<ScionChatThread> {
  const thread = document.createElement('scion-chat-thread');
  thread.conversationKey = key;
  thread.isDM = isDM;
  thread.projectId = 'proj-1';
  document.body.appendChild(thread);
  await thread.updateComplete;
  await vi.waitFor(() =>
    expect(apiFetch.mock.calls.some(([path]) => path.includes('/messages?'))).toBe(true)
  );
  await settle();
  await thread.updateComplete;
  return thread;
}

function composer(el: ScionChatThread): Element | null {
  return el.shadowRoot?.querySelector('scion-chat-composer') ?? null;
}

function notice(el: ScionChatThread): Element | null {
  return el.shadowRoot?.querySelector('.read-only-notice') ?? null;
}

beforeEach(() => {
  history = { messages: [] };
  sendResponse = json({ id: 'm1' }, 201);
  apiFetch.mockReset();
  apiFetch.mockImplementation((path, init) => {
    const method = (init as { method?: string } | undefined)?.method ?? 'GET';
    if (method === 'POST' && /\/messages$/.test(path)) return Promise.resolve(sendResponse);
    if (path.includes('/messages?')) return Promise.resolve(json(history));
    return Promise.resolve(json({ items: [] }));
  });
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('read-only thread', () => {
  it('shows the read-only notice instead of the composer when canPost is false', async () => {
    history = { messages: [], canPost: false };
    const el = await open('topic-read-only');

    expect(composer(el)).toBeNull();
    expect(notice(el)?.textContent).toContain("don't have permission to post");
    expect(notice(el)?.getAttribute('role')).toBe('status');
  });

  it('shows the composer when canPost is true', async () => {
    history = { messages: [], canPost: true };
    const el = await open('topic-writer');

    expect(composer(el)).not.toBeNull();
    expect(notice(el)).toBeNull();
  });

  it('shows the composer when the history carries no canPost', async () => {
    const el = await open('topic-no-flag');

    expect(composer(el)).not.toBeNull();
    expect(notice(el)).toBeNull();
  });

  it('keeps the composer in a DM', async () => {
    history = { messages: [], canPost: false };
    const el = await open('dm:user:u1:user:u2', true);

    expect(composer(el)).not.toBeNull();
    expect(notice(el)).toBeNull();
  });

  it('switches to the notice when a send is refused with chat_post_denied', async () => {
    history = { messages: [], canPost: true };
    sendResponse = json({ error: { code: 'chat_post_denied', message: 'Read only' } }, 403);
    const el = await open('topic-revoked');
    const onError = vi.fn();
    composer(el)?.dispatchEvent(
      new CustomEvent('chat-send', {
        detail: { text: 'hello', onSuccess: vi.fn(), onError },
        bubbles: true,
        composed: true,
      })
    );
    await vi.waitFor(() => expect(onError).toHaveBeenCalled());
    await settle();
    await el.updateComplete;

    expect(composer(el)).toBeNull();
    expect(notice(el)).not.toBeNull();
    expect(el.shadowRoot?.querySelector('.send-error')).toBeNull();
  });

  it('keeps the composer after a 403 with another code', async () => {
    history = { messages: [], canPost: true };
    sendResponse = json({ error: { code: 'forbidden', message: 'Insufficient permissions' } }, 403);
    const el = await open('topic-other-403');
    const onError = vi.fn();
    composer(el)?.dispatchEvent(
      new CustomEvent('chat-send', {
        detail: { text: 'hello', onSuccess: vi.fn(), onError },
        bubbles: true,
        composed: true,
      })
    );
    await vi.waitFor(() => expect(onError).toHaveBeenCalled());
    await settle();
    await el.updateComplete;

    expect(composer(el)).not.toBeNull();
    expect(notice(el)).toBeNull();
  });

  it('shows the notice when _capabilities has no post action, overriding canPost', async () => {
    history = {
      messages: [],
      canPost: true,
      _capabilities: { actions: ['read', 'delete_own'], reason: 'read_only' },
    };
    const el = await open('topic-caps-read-only');

    expect(composer(el)).toBeNull();
    expect(notice(el)?.textContent).toContain("don't have permission to post");
  });

  it('shows the composer when _capabilities lists post', async () => {
    history = { messages: [], canPost: false, _capabilities: { actions: ['read', 'post'] } };
    const el = await open('topic-caps-writer');

    expect(composer(el)).not.toBeNull();
    expect(notice(el)).toBeNull();
  });

  it('names the access token when the reason is token_scope', async () => {
    history = { messages: [], _capabilities: { actions: ['read'], reason: 'token_scope' } };
    const el = await open('topic-caps-token');

    expect(composer(el)).toBeNull();
    expect(notice(el)?.textContent).toContain('access token');
  });

  it('takes the reason from a chat_post_denied refusal', async () => {
    history = { messages: [], _capabilities: { actions: ['read', 'post'] } };
    sendResponse = json(
      {
        error: {
          code: 'chat_post_denied',
          message: 'Read only',
          details: { reason: 'token_scope' },
        },
      },
      403
    );
    const el = await open('topic-caps-revoked');
    const onError = vi.fn();
    composer(el)?.dispatchEvent(
      new CustomEvent('chat-send', {
        detail: { text: 'hello', onSuccess: vi.fn(), onError },
        bubbles: true,
        composed: true,
      })
    );
    await vi.waitFor(() => expect(onError).toHaveBeenCalled());
    await settle();
    await el.updateComplete;

    expect(composer(el)).toBeNull();
    expect(notice(el)?.textContent).toContain('access token');
  });
});
