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
 * live message whose event names artifact refs (metadata.artifacts,
 * ptone/scion#3758) refreshes its chips from history for this viewer.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { requestBodyText } from '../../../client/__fixtures__/request-url.js';

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

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
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

  it('hands the signed-in user to each message and to its own preview', async () => {
    apiFetch.mockResolvedValue(history({ messages: [message('m1', 'hello')] }));
    const el = await mount();
    el.currentUserId = 'u-self';
    await el.updateComplete;
    expect((messageEl(el, 'm1') as unknown as { currentUserId: string }).currentUserId).toBe(
      'u-self'
    );
    apiFetch.mockImplementation(() => new Promise(() => {}));
    (el as unknown as { filePreview: unknown }).filePreview = {
      kind: 'artifact',
      id: A,
      seq: 0,
      name: 'Artifact',
    };
    await el.updateComplete;
    const preview = el.shadowRoot?.querySelector('scion-chat-file-preview') as
      | (HTMLElement & { currentUserId: string })
      | null;
    expect(preview?.currentUserId).toBe('u-self');
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
    const body = JSON.parse(requestBodyText((post![1] as RequestInit).body)) as {
      metadata: Record<string, string>;
    };
    expect(body.metadata).toEqual({
      'RE-to': 'hello',
      artifacts: JSON.stringify([`scion://artifact/${A}`]),
    });
    expect(internals.getMessageArtifactRefs('sent-1')).toEqual([VIEW]);
    expect(showToastMock).toHaveBeenCalledWith('1 artifact reference(s) not attached', 'warning');
  });

  /** Dispatches a live chat event for message id, with optional metadata.artifacts. */
  function live(id: string, msg: string, artifacts?: string[]): void {
    const data = {
      ...message(id, msg),
      conversationKey: KEY,
      threadId: KEY,
      ...(artifacts ? { metadata: { artifacts: JSON.stringify(artifacts) } } : {}),
    };
    fakeStateManager.dispatchEvent(
      new CustomEvent('chat-message-received', { detail: { state: {}, data } })
    );
  }

  function historyCalls(): string[] {
    return apiFetch.mock.calls.map((c) => String(c[0])).filter((u) => u.includes('/messages?'));
  }

  it('refreshes the chips of a live message with artifacts from history, for this viewer', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    const el = await mount();
    apiFetch.mockClear();
    // History resolves the refs for this viewer; the event named only the ref.
    const unavailable = { ref: `scion://artifact/${A}`, id: A, available: false };
    apiFetch.mockResolvedValue(
      history({
        messages: [message('m9', 'other'), message('m2', 'here it is')],
        messageArtifacts: { m2: [VIEW], m9: [unavailable] },
      })
    );
    live('m2', 'here it is', [`scion://artifact/${A}`]);
    await vi.waitFor(() => expect(historyCalls()).toHaveLength(1));
    // Only the newest messages back to this one; never a full page.
    expect(historyCalls()[0]).toBe(`/api/v1/chat/conversations/${KEY}/messages?limit=1`);
    await vi.waitFor(() => expect(messageEl(el, 'm2')?.artifactRefs).toEqual([VIEW]));
    // Views of messages that were not queued are not taken from the page.
    expect((el as unknown as { v2ArtifactMap: Map<string, unknown> }).v2ArtifactMap.has('m9')).toBe(
      false
    );
  });

  it('coalesces a burst into one narrow refresh covering every queued message', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    const el = await mount();
    apiFetch.mockClear();
    apiFetch.mockResolvedValue(
      history({
        messages: [message('m6', 'second'), message('m5', 'plain'), message('m4', 'first')],
        messageArtifacts: { m4: [VIEW], m6: [{ ...VIEW, title: 'Other' }] },
      })
    );
    live('m4', 'first', [`scion://artifact/${A}`]);
    live('m5', 'plain in between');
    live('m6', 'second', [`scion://artifact/${A}`]);
    await vi.waitFor(() => expect(historyCalls()).toHaveLength(1));
    expect(historyCalls()[0]).toBe(`/api/v1/chat/conversations/${KEY}/messages?limit=3`);
    await vi.waitFor(() =>
      expect(messageEl(el, 'm6')?.artifactRefs).toEqual([{ ...VIEW, title: 'Other' }])
    );
    expect(messageEl(el, 'm4')?.artifactRefs).toEqual([VIEW]);
    await new Promise((r) => setTimeout(r, 200));
    expect(historyCalls()).toHaveLength(1);
  });

  it('asks once more with a wider window when a later message pushed the queued one off the page', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    const el = await mount();
    apiFetch.mockClear();
    apiFetch.mockImplementation((url: string) =>
      Promise.resolve(
        url.endsWith('limit=1')
          ? // A reply stored between the event and the request fills the page.
            history({ messages: [message('m-reply', 'quick reply')], messageArtifacts: {} })
          : history({
              messages: [message('m-reply', 'quick reply'), message('m11', 'with artifact')],
              messageArtifacts: { m11: [VIEW] },
            })
      )
    );
    live('m11', 'with artifact', [`scion://artifact/${A}`]);
    await vi.waitFor(() => expect(messageEl(el, 'm11')?.artifactRefs).toEqual([VIEW]));
    expect(historyCalls()).toEqual([
      `/api/v1/chat/conversations/${KEY}/messages?limit=1`,
      `/api/v1/chat/conversations/${KEY}/messages?limit=20`,
    ]);
    // A message on the page with no views (none recorded) is not asked again.
    apiFetch.mockClear();
    apiFetch.mockResolvedValue(history({ messages: [message('m12', 'x')], messageArtifacts: {} }));
    live('m12', 'x', [`scion://artifact/${A}`]);
    await new Promise((r) => setTimeout(r, 250));
    expect(historyCalls()).toHaveLength(1);
  });

  it('drops queued refreshes when the conversation changes inside the window', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    const el = await mount();
    live('m13', 'with artifact', [`scion://artifact/${A}`]);
    apiFetch.mockClear();
    el.conversationKey = 'topic-2';
    await el.updateComplete;
    await new Promise((r) => setTimeout(r, 250));
    // Only the new conversation's own history load (a full page); no chip
    // refresh for m13 against either key.
    expect(historyCalls().filter((u) => /limit=(1|20)$/.test(u))).toEqual([]);
  });

  it('does not refetch for its own sent message, whose views came with the send response', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    const el = await mount();
    apiFetch.mockClear();
    (el as unknown as { v2ArtifactMap: Map<string, unknown> }).v2ArtifactMap.set('m-own', [VIEW]);
    live('m-own', 'mine', [`scion://artifact/${A}`]);
    await new Promise((r) => setTimeout(r, 250));
    expect(historyCalls()).toEqual([]);
  });

  it('does not refetch for a live message without artifact metadata, even if its body names one', async () => {
    apiFetch.mockResolvedValue(history({ items: [] }));
    await mount();
    apiFetch.mockClear();
    live('m3', 'plain text');
    live('m7', `see scion://artifact/${A}`);
    await new Promise((r) => setTimeout(r, 200));
    expect(historyCalls()).toEqual([]);
  });

  it('does not refresh when the artifacts experiment is off', async () => {
    window.__SCION_FEATURES__ = {};
    apiFetch.mockResolvedValue(history({ items: [] }));
    await mount();
    apiFetch.mockClear();
    live('m8', 'here', [`scion://artifact/${A}`]);
    await new Promise((r) => setTimeout(r, 200));
    expect(historyCalls()).toEqual([]);
  });
});
