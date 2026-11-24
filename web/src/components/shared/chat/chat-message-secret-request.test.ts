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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  AGENT_SECRETS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../../utils/feature-flags.js';
import { stateManager } from '../../../client/state.js';
import type {
  SecretRequestCardResponse,
  SecretRequestView,
} from '../../../client/secret-requests.js';

const navigateToMock = vi.hoisted(() => vi.fn());

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  stateManager,
  navigateTo: navigateToMock,
}));

await import('./chat-message.js');
await import('./chat-thread.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;
type ScionChatSecretRequest = import('./chat-secret-request.js').ScionChatSecretRequest;
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeRow(overrides: Partial<SecretRequestView> = {}): SecretRequestView {
  return {
    rowId: 'row-1',
    messageId: 'msg-sr-1',
    projectId: 'proj-1',
    agentId: 'agent-1',
    agentName: 'release-bot',
    key: 'GITHUB_TOKEN',
    version: 1,
    description: 'GitHub PAT with repo scope',
    secretType: 'environment',
    position: 0,
    status: 'pending',
    createdAt: '2026-11-24T00:00:00Z',
    expiresAt: '2026-12-01T00:00:00Z',
    ...overrides,
  };
}

function makeCard(overrides: Partial<SecretRequestCardResponse> = {}): SecretRequestCardResponse {
  return {
    messageId: 'msg-sr-1',
    projectId: 'proj-1',
    projectName: 'alpha-project',
    agentId: 'agent-1',
    agentName: 'release-bot',
    threadId: 'thread-1',
    intro: 'I need a token to push the release branch.',
    canRespond: true,
    reason: '',
    rows: [makeRow()],
    ...overrides,
  };
}

const FALLBACK_MARKDOWN =
  'I need a token to push the release branch.\n\n**Secret Request** — submit via https://hub.example/secret-requests/msg-sr-1 (do not reply in chat).';

describe('native chat secret-request wiring', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    localStorage.clear();
    sessionStorage.clear();
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
  });

  it('renders <scion-chat-secret-request surface="native"> without duplicating fallback text', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(makeCard()));
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-sr-1';
    msg.messageType = 'secret-request';
    msg.body = FALLBACK_MARKDOWN;
    msg.fromAgent = true;
    msg.senderIsAgent = true;
    msg.sender = 'agent:release-bot';
    msg.senderName = 'release-bot';
    msg.currentUserId = 'user-1';
    document.body.appendChild(msg);

    await vi.waitFor(() => {
      expect(msg.shadowRoot?.querySelector('scion-chat-secret-request')).not.toBeNull();
    });
    await msg.updateComplete;

    const secretReqCalls = fetchMock.mock.calls.filter(([url]) =>
      String(url).startsWith('/api/v1/secret-requests/')
    );
    expect(secretReqCalls).toHaveLength(1);
    expect(secretReqCalls[0][0]).toBe('/api/v1/secret-requests/msg-sr-1');

    // Intro is rendered once, and the fallback standalone link is NOT duplicated in the bubble
    await vi.waitFor(() => {
      expect(msg.shadowRoot?.textContent).toContain('I need a token to push the release branch.');
    });
    expect(msg.shadowRoot?.textContent).not.toContain('https://hub.example/secret-requests/');

    const card = msg.shadowRoot?.querySelector(
      'scion-chat-secret-request'
    ) as ScionChatSecretRequest;
    expect(card.surface).toBe('native');
    expect(card.canRespond).toBe(true);
    expect(card.agentName).toBe('release-bot');
    expect(card.projectName).toBe('alpha-project');
  });

  it('passes real canRespond=false and reason from the card so non-responders see a disabled form', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        makeCard({
          canRespond: false,
          reason: 'you need agent.attach permission on this agent to supply or dismiss secrets',
          intro: '',
        })
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-sr-1';
    msg.messageType = 'secret-request';
    msg.body = FALLBACK_MARKDOWN;
    msg.fromAgent = true;
    msg.senderIsAgent = true;
    document.body.appendChild(msg);

    await vi.waitFor(() => {
      expect(msg.shadowRoot?.querySelector('scion-chat-secret-request')).not.toBeNull();
    });
    await msg.updateComplete;

    const card = msg.shadowRoot?.querySelector(
      'scion-chat-secret-request'
    ) as ScionChatSecretRequest;
    await card.updateComplete;

    expect(card.canRespond).toBe(false);
    expect(card.shadowRoot?.querySelector('.status-alert.blocked')?.textContent).toContain(
      'agent.attach'
    );
    const input = card.shadowRoot?.querySelector('input') as HTMLInputElement;
    expect(input.disabled).toBe(true);
    // No body block rendered when intro is empty
    expect(msg.shadowRoot?.querySelector('.plain-text')).toBeNull();
  });

  it('sends surface="native" on fulfill and updates live through controller events', async () => {
    const fetchMock = vi.fn().mockImplementation((input: string | URL) => {
      const url = String(input);
      if (url === '/api/v1/secret-requests/msg-sr-1') {
        return Promise.resolve(jsonResponse(makeCard()));
      }
      if (url === '/api/v1/secret-requests/msg-sr-1/fulfill') {
        return Promise.resolve(
          jsonResponse({
            rows: [
              makeRow({
                status: 'fulfilled',
                fulfilled: {
                  at: '2026-11-24T00:05:00Z',
                  byUserId: 'user-1',
                  byName: 'Alice',
                  surface: 'native',
                },
              }),
            ],
            responseMessageId: 'resp-1',
            replayed: false,
          })
        );
      }
      return Promise.resolve(new Response('', { status: 404 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-sr-1';
    msg.messageType = 'secret-request';
    msg.body = FALLBACK_MARKDOWN;
    msg.currentUserId = 'user-1';
    document.body.appendChild(msg);

    await vi.waitFor(() => {
      expect(msg.shadowRoot?.querySelector('scion-chat-secret-request')).not.toBeNull();
    });
    const card = msg.shadowRoot?.querySelector(
      'scion-chat-secret-request'
    ) as ScionChatSecretRequest;
    await card.updateComplete;

    // Live transition to submitting via controller event
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'secret_request',
            messageId: 'msg-sr-1',
            secret_request: makeRow({ status: 'pending', submitting: true }),
          },
        },
      })
    );
    await card.updateComplete;
    expect(card.shadowRoot?.textContent).toContain('Submitting…');

    // Roll back to pending and submit natively
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'secret_request',
            messageId: 'msg-sr-1',
            secret_request: makeRow({ status: 'pending', submitting: false }),
          },
        },
      })
    );
    await card.updateComplete;

    const input = card.shadowRoot?.querySelector(
      'input[data-key="GITHUB_TOKEN"]'
    ) as HTMLInputElement;
    input.value = 'ghp_native_secret';
    input.dispatchEvent(new Event('input'));
    await card.updateComplete;

    (card.shadowRoot?.querySelector('.submit-btn') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(card.shadowRoot?.textContent).toContain('Fulfilled by you');
    });

    const fulfillCalls = fetchMock.mock.calls.filter(
      ([url]) => String(url) === '/api/v1/secret-requests/msg-sr-1/fulfill'
    );
    expect(fulfillCalls).toHaveLength(1);
    const [url, init] = fulfillCalls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/secret-requests/msg-sr-1/fulfill');
    const body = JSON.parse(init.body as string) as {
      surface: string;
      values: Record<string, string>;
    };
    expect(body.surface).toBe('native');
    expect(body.values).toEqual({ GITHUB_TOKEN: 'ghp_native_secret' });
    // Form disabled/hidden once no key is pending
    expect(card.shadowRoot?.querySelector('.card-foot')).toBeNull();
  });

  it('does not fetch or render <scion-chat-secret-request> when hub.agent_secrets is off', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, false);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-sr-1';
    msg.messageType = 'secret-request';
    msg.body = FALLBACK_MARKDOWN;
    msg.fromAgent = true;
    document.body.appendChild(msg);
    await msg.updateComplete;

    expect(fetchMock).not.toHaveBeenCalled();
    expect(msg.shadowRoot?.querySelector('scion-chat-secret-request')).toBeNull();
    await vi.waitFor(() => {
      expect(msg.shadowRoot?.textContent).toContain('Secret Request');
    });
  });

  it('renders secret-response messages sensibly (key/version only, compact row when experiment is on)', async () => {
    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-resp-1';
    msg.messageType = 'secret-response';
    msg.body =
      '`GITHUB_TOKEN` v2 is now available (stored by alice@example.com). Read it when needed with `scion secret get --value GITHUB_TOKEN`.';
    msg.senderId = 'user-1';
    msg.currentUserId = 'user-1';
    msg.timestamp = '2026-11-24T00:05:00Z';
    document.body.appendChild(msg);

    await vi.waitFor(() => {
      expect(msg.shadowRoot?.querySelector('[data-testid="secret-response-row"]')).not.toBeNull();
      expect(msg.shadowRoot?.textContent).toContain('GITHUB_TOKEN');
    });
    expect(msg.shadowRoot?.textContent).toContain('v2');
    expect(msg.shadowRoot?.querySelector('.secret-response-row.own')).not.toBeNull();

    // Another user's response renders in the 'other' lane
    msg.senderId = 'user-2';
    await msg.updateComplete;
    expect(msg.shadowRoot?.querySelector('.secret-response-row.other')).not.toBeNull();
  });

  it('jumps to replaced secret-request card within chat when present, else navigates to standalone route', async () => {
    navigateToMock.mockReset();
    const thread = document.createElement('scion-chat-thread') as ScionChatThread;
    document.body.appendChild(thread);
    await thread.updateComplete;

    // Case 1: target message exists in chat -> scrolls within chat, no standalone navigation
    const targetEl = document.createElement('div');
    targetEl.id = 'msg-v2-in-chat';
    thread.shadowRoot!.appendChild(targetEl);
    const scrollSpy = vi.spyOn(thread, 'scrollToMessageById').mockResolvedValue();

    (
      thread as unknown as {
        handleSecretRequestJump: (e: CustomEvent<{ messageId: string }>) => void;
      }
    ).handleSecretRequestJump(
      new CustomEvent('secret-request-jump', { detail: { messageId: 'v2-in-chat' } })
    );

    await vi.waitFor(() => expect(scrollSpy).toHaveBeenCalledWith('v2-in-chat', true, false, ''));
    expect(navigateToMock).not.toHaveBeenCalled();

    // Case 2: target message is NOT in chat -> falls back to standalone route
    (
      thread as unknown as {
        handleSecretRequestJump: (e: CustomEvent<{ messageId: string }>) => void;
      }
    ).handleSecretRequestJump(
      new CustomEvent('secret-request-jump', { detail: { messageId: 'v2-other-conv' } })
    );

    await vi.waitFor(() => {
      expect(navigateToMock).toHaveBeenCalledWith('/secret-requests/v2-other-conv');
    });
  });

  it('skips standalone navigation if the conversation changed while secret-request jump was in flight', async () => {
    navigateToMock.mockReset();
    const thread = document.createElement('scion-chat-thread') as ScionChatThread;
    document.body.appendChild(thread);
    await thread.updateComplete;

    let resolveScroll: (() => void) | null = null;
    vi.spyOn(thread, 'scrollToMessageById').mockImplementation(
      () =>
        new Promise<void>((resolve) => {
          resolveScroll = resolve;
        })
    );

    const t = thread as unknown as {
      fetchId: number;
      handleSecretRequestJump: (e: CustomEvent<{ messageId: string }>) => void;
    };
    t.handleSecretRequestJump(
      new CustomEvent('secret-request-jump', { detail: { messageId: 'v2-stale-jump' } })
    );

    // Simulate switching conversation before scrollToMessageById resolves
    t.fetchId++;
    resolveScroll!();
    await new Promise((r) => setTimeout(r, 0));

    expect(navigateToMock).not.toHaveBeenCalled();
  });

  it('aborts in-flight card fetch on disconnect and refetches on reconnect', async () => {
    const signals: AbortSignal[] = [];
    let callCount = 0;
    const fetchMock = vi.fn().mockImplementation((input: string | URL, init?: RequestInit) => {
      const url = String(input);
      if (url === '/api/v1/secret-requests/msg-sr-1') {
        callCount++;
        if (init?.signal) signals.push(init.signal);
        if (callCount === 1) {
          // Keep first request pending until aborted
          return new Promise<Response>(() => {});
        }
        return Promise.resolve(jsonResponse(makeCard()));
      }
      return Promise.resolve(new Response('', { status: 404 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-sr-1';
    msg.messageType = 'secret-request';
    msg.body = FALLBACK_MARKDOWN;
    document.body.appendChild(msg);
    await msg.updateComplete;

    expect(signals).toHaveLength(1);
    expect(signals[0].aborted).toBe(false);

    // Disconnect aborts the in-flight request and clears loaded id
    msg.remove();
    expect(signals[0].aborted).toBe(true);

    // Reconnect triggers a fresh GET and renders the card
    document.body.appendChild(msg);
    await vi.waitFor(() => {
      expect(msg.shadowRoot?.querySelector('scion-chat-secret-request')).not.toBeNull();
    });
    expect(callCount).toBe(2);
  });

  it('renders the Markdown fallback alongside the error and Retry button when card GET fails', async () => {
    const fetchMock = vi.fn().mockImplementation((input: string | URL) => {
      if (String(input) === '/api/v1/secret-requests/msg-sr-1') {
        return Promise.resolve(
          jsonResponse({ error: { code: 'internal_error', message: 'temporary failure' } }, 500)
        );
      }
      return Promise.resolve(new Response('', { status: 404 }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-sr-1';
    msg.messageType = 'secret-request';
    msg.body = FALLBACK_MARKDOWN;
    msg.fromAgent = true;
    document.body.appendChild(msg);

    await vi.waitFor(() => {
      expect(msg.shadowRoot?.querySelector('.secret-request-status.error')).not.toBeNull();
      expect(msg.shadowRoot?.textContent).toContain('https://hub.example/secret-requests/msg-sr-1');
    });
    expect(msg.shadowRoot?.querySelector('.secret-request-status.error sl-button')).not.toBeNull();
  });
});
