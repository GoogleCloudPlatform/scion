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
 * Tests for artifact references in <scion-chat-thread> (ptone/scion#3224):
 * history `messageArtifacts` reach the message, the composer's refs ride in
 * the send metadata, the send response's views and warning are used, and a
 * live message naming an artifact fetches the viewer's views.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

class FakeStateManager extends EventTarget {
  currentScope: { type: string; userId: string } | null = null;
  getAgent(): undefined {
    return undefined;
  }
  clearAgents(): void {}
  setAgent(): void {}
}
const fakeStateManager = new FakeStateManager();
const apiFetch = vi.fn();

vi.mock('../../../client/main.js', () => ({
  navigateTo: vi.fn(),
  get stateManager() {
    return fakeStateManager;
  },
}));
vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]) => apiFetch(...args) as unknown,
  extractApiError: () => Promise.resolve('error'),
}));
const showToastMock = vi.fn();
vi.mock('../../../utils/toast.js', () => ({
  showToast: (...args: unknown[]) => showToastMock(...args) as unknown,
}));
vi.mock('../confirm-dialog.js', () => ({ showConfirm: vi.fn() }));

await import('./chat-thread.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;
type ChatSendDetail = import('./chat-composer.js').ChatSendDetail;

const KEY = 'topic-1';
const A = '5f1c2d3e-0000-4000-8000-0000000000aa';
const VIEW = {
  ref: `scion://artifact/${A}`,
  id: A,
  available: true,
  title: 'Design notes',
  version: 3,
  ownerName: 'docs-writer',
};

function history(body: Record<string, unknown>): Response {
  return { ok: true, status: 200, json: () => Promise.resolve(body) } as unknown as Response;
}

function message(id: string, msg: string) {
  return {
    id,
    projectId: 'p1',
    sender: 'agent:docs',
    senderId: 'a1',
    recipient: 'user:me',
    recipientId: 'u1',
    msg,
    type: 'chat',
    agentId: 'a1',
    createdAt: '2026-10-07T10:00:00Z',
  };
}

async function mount(): Promise<ScionChatThread> {
  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = KEY;
  document.body.appendChild(el);
  await el.updateComplete;
  await vi.waitFor(() => expect(apiFetch).toHaveBeenCalled());
  for (let i = 0; i < 6; i++) {
    await Promise.resolve();
    await el.updateComplete;
  }
  return el;
}

function messageEl(
  el: ScionChatThread,
  id: string
): (HTMLElement & { artifactRefs: unknown }) | null {
  const all = [...(el.shadowRoot?.querySelectorAll('scion-chat-message') ?? [])] as (HTMLElement & {
    messageId?: string;
    artifactRefs: unknown;
  })[];
  return all.find((m) => m.messageId === id) ?? null;
}

describe('scion-chat-thread artifact references', () => {
  beforeEach(() => {
    apiFetch.mockReset();
    showToastMock.mockReset();
    window.__SCION_FEATURES__ = { 'hub.artifacts': true };
  });
  afterEach(() => {
    document.body.innerHTML = '';
    delete window.__SCION_FEATURES__;
  });

  it('hands history messageArtifacts to the message', async () => {
    apiFetch.mockResolvedValue(
      history({
        items: [message('m1', `see scion://artifact/${A}`)],
        messageArtifacts: { m1: [VIEW] },
      })
    );
    const el = await mount();
    await vi.waitFor(() => expect(messageEl(el, 'm1')?.artifactRefs).toEqual([VIEW]));
  });

  it('sends picked refs in metadata next to RE-to, and uses the response views and warning', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    const el = await mount();
    apiFetch.mockImplementation((url: string, init?: RequestInit) =>
      Promise.resolve(
        init?.method === 'POST'
          ? ({
              ok: true,
              status: 201,
              json: () =>
                Promise.resolve({
                  id: 'sent-1',
                  artifacts: [VIEW],
                  artifactWarning: '1 artifact reference(s) not attached',
                }),
            } as unknown as Response)
          : history({ items: [] })
      )
    );
    const internals = el as unknown as {
      handleChatSendV2(e: CustomEvent<ChatSendDetail>): Promise<void>;
      getMessageArtifactRefs(id: string): unknown;
    };
    await internals.handleChatSendV2(
      new CustomEvent<ChatSendDetail>('chat-send', {
        detail: {
          text: '',
          plain: false,
          interrupt: false,
          onSuccess: vi.fn(),
          mentions: [],
          attachmentIds: [],
          artifactRefs: [`scion://artifact/${A}`],
          replyToId: 'orig-1',
          replyToContent: 'hello',
        },
      })
    );
    const post = apiFetch.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST'
    );
    const body = JSON.parse(String((post![1] as RequestInit).body)) as {
      metadata: Record<string, string>;
    };
    expect(body.metadata).toEqual({
      'RE-to': 'hello',
      artifacts: JSON.stringify([`scion://artifact/${A}`]),
    });
    expect(internals.getMessageArtifactRefs('sent-1')).toEqual([VIEW]);
    expect(showToastMock).toHaveBeenCalledWith('1 artifact reference(s) not attached', 'warning');
  });

  it('fetches the viewer views when a live message names an artifact', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    await mount();
    apiFetch.mockClear();
    apiFetch.mockResolvedValue(history({ items: [], messageArtifacts: {} }));
    const live = {
      ...message('m2', `here scion://artifact/${A}`),
      conversationKey: KEY,
      threadId: KEY,
    };
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-received', { detail: { state: {}, data: live } })
    );
    await vi.waitFor(() =>
      expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('/messages?'))).toBe(true)
    );
  });

  it('does not refetch for a live message without an artifact reference', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    await mount();
    apiFetch.mockClear();
    const live = { ...message('m3', 'plain text'), conversationKey: KEY, threadId: KEY };
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-received', { detail: { state: {}, data: live } })
    );
    await new Promise((r) => setTimeout(r, 20));
    expect(apiFetch.mock.calls.some((c) => String(c[0]).includes('/messages?'))).toBe(false);
  });
});
