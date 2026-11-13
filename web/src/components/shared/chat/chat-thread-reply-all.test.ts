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
 * Tests for "Reply all" in <scion-chat-thread> (ptone/scion#7817).
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';

class FakeStateManager extends EventTarget {
  currentScope: { type: string; userId: string } | null = null;
  isConnected = false;
  getAgent(): undefined {
    return undefined;
  }
  seedAgents(): void {}
}
const fakeStateManager = new FakeStateManager();

const apiFetch = vi.fn();
const showToastMock = vi.fn();

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  navigateTo: vi.fn(),
  get stateManager(): FakeStateManager {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]): unknown => apiFetch(...args) as unknown,
  extractApiError: (_res: unknown, fallback: string): Promise<string> => Promise.resolve(fallback),
}));

vi.mock('../../../utils/toast.js', () => ({
  showToast: (...args: unknown[]): unknown => showToastMock(...args) as unknown,
}));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;
type ScionChatComposer = import('./chat-composer.js').ScionChatComposer;
type Message = import('../../../shared/types.js').Message;

interface MenuActionLike {
  id: string;
  label: string;
  icon?: string;
  disabled?: boolean;
  run: () => void;
}

interface ThreadInternals {
  messageMenuActions(msg: Message): MenuActionLike[];
  handleMessageContextMenu(e: MouseEvent, msg: Message): void;
  composerReplyTo: { messageId: string; senderName: string; content: string } | null;
}

const MEMBERS: ScionChatThread['members'] = [
  { id: 'u-me', name: 'Me User', email: 'me@example.com', kind: 'user' },
  { id: 'u-alice', name: 'Alice Smith', email: 'alice@example.com', kind: 'user' },
  { id: 'u-carol', name: 'Carol', email: 'carol@example.com', kind: 'user' },
];

const AGENT_MEMBERS: ScionChatThread['agentMembers'] = [
  {
    id: 'a-alpha',
    slug: 'agent-a',
    displayName: 'Agent A',
    status: 'running',
    projectId: 'p1',
    canAttach: true,
  },
  {
    id: 'a-beta',
    slug: 'agent-b',
    displayName: 'Agent B',
    status: 'running',
    projectId: 'p1',
    canAttach: true,
  },
];

function makeMsg(overrides: Partial<Message> & { id: string; msg: string }): Message {
  return {
    projectId: 'p1',
    sender: 'agent:agent-a',
    senderId: 'a-alpha',
    recipient: '',
    recipientId: '',
    type: 'chat',
    createdAt: '2026-11-13T12:00:00Z',
    ...overrides,
  } as Message;
}

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 6; i++) {
    await new Promise((r) => setTimeout(r, 0));
  }
}

async function mountThread(opts: {
  messages: Message[];
  canPost?: boolean;
  isDM?: boolean;
  conversationKey?: string;
}): Promise<ScionChatThread> {
  apiFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (init?.method === 'POST' && url.endsWith('/messages')) {
      return Promise.resolve(jsonResponse({ message: { id: 'sent-1' } }, 201));
    }
    if (url.includes('/messages?')) {
      return Promise.resolve(
        jsonResponse({
          messages: opts.messages,
          ...(typeof opts.canPost === 'boolean' ? { canPost: opts.canPost } : {}),
        })
      );
    }
    return Promise.resolve(jsonResponse({}));
  });

  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = opts.conversationKey ?? 'topic-1';
  el.projectId = 'p1';
  el.currentUserId = 'u-me';
  el.isDM = opts.isDM ?? false;
  el.members = MEMBERS;
  el.agentMembers = AGENT_MEMBERS;
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() =>
    expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('/messages?'))).toBe(true)
  );
  await settle();
  await el.updateComplete;
  return el;
}

describe('scion-chat-thread Reply all', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    showToastMock.mockReset();
  });

  afterEach(() => {
    document.body.innerHTML = '';
  });

  it('hides Reply all when no other participant is mentioned in prose', async () => {
    const cases: Message[] = [
      makeMsg({ id: 'm-none', msg: 'plain message without mentions' }),
      makeMsg({ id: 'm-self', msg: 'hey @me-user check this' }),
      makeMsg({ id: 'm-sender', msg: 'note to @agent-a only' }),
      makeMsg({ id: 'm-code', msg: '```ts\n@agent-b\n```\n`@alice-smith`\n> @carol' }),
      makeMsg({ id: 'm-unknown', msg: '@Override @departed-user nothing here' }),
    ];
    const el = await mountThread({ messages: cases });
    const internals = el as unknown as ThreadInternals;

    for (const msg of cases) {
      const ids = internals.messageMenuActions(msg).map((a) => a.id);
      expect(ids, `menu for ${msg.id}`).not.toContain('reply-all');
    }
  });

  it('hides Reply all on a read-only thread (canPost false)', async () => {
    const msg = makeMsg({ id: 'm-1', msg: '@agent-b @alice-smith look' });
    const el = await mountThread({ messages: [msg], canPost: false });
    const ids = (el as unknown as ThreadInternals).messageMenuActions(msg).map((a) => a.id);

    expect(ids).not.toContain('reply-all');
  });

  it('shows Reply all right after Reply, including on own message and in agent DMs', async () => {
    const agentMsg = makeMsg({ id: 'm-agent', msg: '@agent-b @alice-smith look' });
    const ownMsg = makeMsg({
      id: 'm-own',
      sender: 'user:me@example.com',
      senderId: 'u-me',
      msg: '@agent-b thoughts?',
    });

    const el = await mountThread({
      messages: [agentMsg, ownMsg],
      isDM: true,
      conversationKey: 'agent:agent-a',
    });
    const internals = el as unknown as ThreadInternals;

    const agentActions = internals.messageMenuActions(agentMsg);
    expect(agentActions[0].id).toBe('reply');
    expect(agentActions[1]).toMatchObject({
      id: 'reply-all',
      label: 'Reply all',
      icon: 'reply-all',
    });

    const ownActions = internals.messageMenuActions(ownMsg);
    expect(ownActions.slice(0, 3).map((a) => a.id)).toEqual(['reply', 'reply-all', 'edit']);
  });

  it('desktop popup click on agent message seeds reply banner + "cc @..." and sends mentions', async () => {
    const msg = makeMsg({ id: 'm-agent', msg: '@agent-b @alice-smith look' });
    const el = await mountThread({ messages: [msg] });
    const internals = el as unknown as ThreadInternals;

    internals.handleMessageContextMenu(
      new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 80, clientY: 80 }),
      msg
    );
    await el.updateComplete;

    const menuItems = [
      ...(el.shadowRoot?.querySelectorAll<HTMLElement>('.context-menu .context-menu-item') ?? []),
    ];
    expect(menuItems[0]?.textContent?.trim()).toBe('Reply');
    expect(menuItems[1]?.textContent?.trim()).toBe('Reply all');
    expect(menuItems[1]?.querySelector('sl-icon')?.getAttribute('name')).toBe('reply-all');

    menuItems[1].click();
    await el.updateComplete;
    await settle();

    expect(el.shadowRoot?.querySelector('.context-menu')).toBeNull();
    expect(internals.composerReplyTo?.messageId).toBe('m-agent');

    const composer = el.shadowRoot?.querySelector('scion-chat-composer') as ScionChatComposer;
    expect(composer).toBeTruthy();
    await composer.updateComplete;
    const sl = composer.shadowRoot?.querySelector('sl-textarea') as {
      updateComplete: Promise<unknown>;
      shadowRoot: ShadowRoot;
    };
    await sl.updateComplete;
    const ta = sl.shadowRoot.querySelector('textarea') as HTMLTextAreaElement;
    expect(ta.value).toBe('cc @agent-b @alice-smith ');

    // Append reply text and trigger send to verify POST payload.
    ta.value = 'cc @agent-b @alice-smith sounds good';
    ta.selectionStart = ta.selectionEnd = ta.value.length;
    ta.dispatchEvent(new Event('input', { bubbles: true, composed: true }));
    await composer.updateComplete;

    (composer as unknown as { handleSend(): void }).handleSend();
    await vi.waitFor(() =>
      expect(
        apiFetch.mock.calls.some(
          ([u, init]) =>
            (init as RequestInit | undefined)?.method === 'POST' && String(u).endsWith('/messages')
        )
      ).toBe(true)
    );

    const postCall = apiFetch.mock.calls.find(
      ([u, init]) =>
        (init as RequestInit | undefined)?.method === 'POST' && String(u).endsWith('/messages')
    );
    const sentBody = JSON.parse((postCall?.[1] as { body: string }).body) as {
      content: string;
      reply_to_id: string;
      mentions: string[];
    };
    expect(sentBody.reply_to_id).toBe('m-agent');
    expect(sentBody.content).toBe('cc @agent-b @alice-smith sounds good');
    expect(sentBody.mentions).toEqual(['agent-b', 'alice-smith']);
  });

  it('seeds "@..." without "cc " lead-in when replying all to a human message', async () => {
    const humanMsg = makeMsg({
      id: 'm-human',
      sender: 'user:alice@example.com',
      senderId: 'u-alice',
      msg: '@agent-b @carol thoughts?',
    });
    const el = await mountThread({ messages: [humanMsg] });
    const internals = el as unknown as ThreadInternals;

    const replyAll = internals.messageMenuActions(humanMsg).find((a) => a.id === 'reply-all');
    expect(replyAll).toBeTruthy();
    replyAll!.run();

    await el.updateComplete;
    await settle();

    const composer = el.shadowRoot?.querySelector('scion-chat-composer') as ScionChatComposer;
    await composer.updateComplete;
    const sl = composer.shadowRoot?.querySelector('sl-textarea') as {
      updateComplete: Promise<unknown>;
      shadowRoot: ShadowRoot;
    };
    await sl.updateComplete;
    const ta = sl.shadowRoot.querySelector('textarea') as HTMLTextAreaElement;
    expect(ta.value).toBe('@agent-b @carol ');
  });
});
