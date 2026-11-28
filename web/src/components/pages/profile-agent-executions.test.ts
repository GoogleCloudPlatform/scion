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
 * Tests for `<scion-page-profile-agent-executions>` (`/profile/agent-executions`),
 * `inbox-changed` live refresh, and "Open in chat" links (ptone/scion#9698,
 * AC-10, AC-11, AC-12, AC-14, AC-16).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type {
  CommandRequestDetailResponse,
  CommandRequestView,
} from '../../client/command-requests.js';
import { resetActionInboxProbeCacheForTests } from '../../client/action-inbox.js';
import { stateManager } from '../../client/state.js';
import {
  COMMAND_REQUESTS_FLAG,
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import './profile-agent-executions.js';
import type { ScionPageProfileAgentExecutions } from './profile-agent-executions.js';
import type { ScionActionInboxPage } from '../shared/action-inbox-page.js';
import type { ScionChatCommandRequest } from '../shared/chat/chat-command-request.js';

function makeRequest(overrides: Partial<CommandRequestView> = {}): CommandRequestView {
  return {
    rowId: 'cr-1',
    messageId: 'msg-1',
    projectId: 'proj-1',
    agentId: 'agent-1',
    agentName: 'deploy-bot',
    key: 'restart-staging',
    version: 1,
    description: 'Restart staging workers',
    script: 'echo restarting',
    workdir: '~',
    workdirKind: 'home',
    timeoutS: 600,
    shareOutputRequested: false,
    commandSha256: 'abc123',
    approverUserId: 'user-1',
    status: 'pending',
    createdAt: '2026-11-27T10:00:00Z',
    expiresAt: '2026-11-28T10:00:00Z',
    ...overrides,
  };
}

function makeDetail(
  overrides: Partial<CommandRequestDetailResponse> = {}
): CommandRequestDetailResponse {
  return {
    request: makeRequest(),
    projectName: 'Core API',
    threadId: 'topic-1',
    canApprove: true,
    canDismiss: true,
    promptAvailable: true,
    ...overrides,
  };
}

function jsonResp(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function flush(): Promise<void> {
  await Promise.resolve();
  await new Promise((r) => setTimeout(r, 20));
}

describe('<scion-page-profile-agent-executions>', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    window.history.replaceState({}, '', '/profile/agent-executions');
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    resetActionInboxProbeCacheForTests();
    fetchMock = vi.fn<typeof fetch>();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.useRealTimers();
    vi.unstubAllGlobals();
    localStorage.clear();
    delete window.__SCION_FEATURES__;
    resetServerFlagStateForTests();
    resetActionInboxProbeCacheForTests();
  });

  it('renders 404 and makes zero API calls when hub.command_requests is off (AC-10)', async () => {
    const page = document.createElement(
      'scion-page-profile-agent-executions'
    ) as ScionPageProfileAgentExecutions;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;
    expect(inner.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders 404 when hub returns 404 on /api/v1/users/me/command-requests', async () => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
    fetchMock.mockResolvedValue(jsonResp({ error: { code: 'not_found', message: 'off' } }, 404));

    const page = document.createElement(
      'scion-page-profile-agent-executions'
    ) as ScionPageProfileAgentExecutions;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;
    expect(inner.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
  });

  it('lists open cards with context, passes .detail without per-card fetch, and renders active Inbox tab (AC-10, AC-14)', async () => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const detail = makeDetail();
    fetchMock.mockResolvedValue(
      jsonResp({
        counts: { answerable: 1, open: 1 },
        cards: [detail],
        facets: {
          agents: [{ id: 'agent-1', name: 'deploy-bot', count: 1 }],
          projects: [{ id: 'proj-1', name: 'Core API', count: 1 }],
        },
        truncatedProjects: false,
        capped: false,
      })
    );

    const page = document.createElement(
      'scion-page-profile-agent-executions'
    ) as ScionPageProfileAgentExecutions;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    // Only one list fetch, no per-card /api/v1/command-requests/{id} fetch.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0] as string).toContain('/api/v1/users/me/command-requests?');

    // Inbox tab strip shows Overview | Agent Executions with Agent Executions active.
    const activeTab = inner.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-inbox-tab="agent-executions"]'
    );
    expect(activeTab).not.toBeNull();
    expect(activeTab!.classList.contains('active')).toBe(true);
    expect(activeTab!.getAttribute('aria-current')).toBe('page');

    // Card context shows project, agent, and Open in chat link.
    const cardEl = inner.shadowRoot!.querySelector('[data-command-request-card="cr-1"]')!;
    expect(cardEl).not.toBeNull();
    expect(cardEl.querySelector('[data-card-context]')?.textContent).toContain('Core API');
    expect(cardEl.querySelector('[data-card-context]')?.textContent).toContain('deploy-bot');

    // Child <scion-chat-command-request> received .detail directly.
    const crEl = cardEl.querySelector<ScionChatCommandRequest>('scion-chat-command-request')!;
    await crEl.updateComplete;
    expect(crEl.detail).toEqual(detail);
    expect(crEl.surface).toBe('standalone_page');
  });

  it('refreshes on page 1 when a card dispatches command-request-approved or command-request-dismissed (AC-11)', async () => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonResp({
          counts: { answerable: 1, open: 1 },
          cards: [makeDetail()],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      )
    );

    const page = document.createElement(
      'scion-page-profile-agent-executions'
    ) as ScionPageProfileAgentExecutions;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;
    expect(fetchMock).toHaveBeenCalledTimes(1);

    const crEl = inner.shadowRoot!.querySelector<ScionChatCommandRequest>(
      'scion-chat-command-request'
    )!;
    crEl.dispatchEvent(
      new CustomEvent('command-request-approved', {
        detail: { request: makeRequest({ status: 'approved' }) },
        bubbles: true,
        composed: true,
      })
    );
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    crEl.dispatchEvent(
      new CustomEvent('command-request-dismissed', {
        detail: { request: makeRequest({ status: 'dismissed' }) },
        bubbles: true,
        composed: true,
      })
    );
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('refreshes live on inbox-changed for agent_executions and ignores other categories (AC-12)', async () => {
    vi.useFakeTimers();
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      )
    );

    const page = document.createElement(
      'scion-page-profile-agent-executions'
    ) as ScionPageProfileAgentExecutions;
    document.body.appendChild(page);
    await vi.advanceTimersByTimeAsync(20);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Different inbox category is ignored (both wrapped notifyWithData and flat shapes).
    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { data: { category: 'agent_secrets' } } })
    );
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Matching agent_executions hint (StateManager notifyWithData shape) triggers debounced reload after 750ms.
    stateManager.dispatchEvent(
      new CustomEvent('inbox-changed', { detail: { data: { category: 'agent_executions' } } })
    );
    await vi.advanceTimersByTimeAsync(800);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    // Matching message-action-updated also refreshes.
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'command_request' } } })
    );
    await vi.advanceTimersByTimeAsync(800);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('renders Open in chat for DM and thread cards, purged note when promptAvailable is false, and nothing when threadId is missing (AC-16)', async () => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);

    const threadCard = makeDetail({
      request: makeRequest({ rowId: 'cr-thread', messageId: 'msg-t1', projectId: 'proj-1' }),
      threadId: 'topic-42',
      promptAvailable: true,
    });
    const dmCard = makeDetail({
      request: makeRequest({ rowId: 'cr-dm', messageId: 'msg-d1', projectId: 'proj-1' }),
      threadId: 'dm:agent:agent-1:user:user-1',
      promptAvailable: true,
    });
    const purgedCard = makeDetail({
      request: makeRequest({ rowId: 'cr-purged', messageId: 'msg-p1', projectId: 'proj-1' }),
      threadId: 'topic-42',
      promptAvailable: false,
    });
    const noThreadCard = makeDetail({
      request: makeRequest({ rowId: 'cr-nothread', messageId: 'msg-n1', projectId: 'proj-1' }),
      threadId: '',
      promptAvailable: true,
    });

    fetchMock.mockResolvedValue(
      jsonResp({
        counts: { answerable: 4, open: 4 },
        cards: [threadCard, dmCard, purgedCard, noThreadCard],
        facets: { agents: [], projects: [] },
        truncatedProjects: false,
        capped: false,
      })
    );

    const page = document.createElement(
      'scion-page-profile-agent-executions'
    ) as ScionPageProfileAgentExecutions;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    // Thread card: links to /chat/space/proj-1/thread/topic-42#msg-msg-t1
    const threadLink = inner.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-command-request-card="cr-thread"] [data-open-in-chat]'
    );
    expect(threadLink).not.toBeNull();
    expect(threadLink!.getAttribute('href')).toBe('/chat/space/proj-1/thread/topic-42#msg-msg-t1');
    expect(threadLink!.textContent).toContain('Open in chat');

    // DM card: links to /chat/dm/<encoded>#msg-msg-d1
    const dmLink = inner.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-command-request-card="cr-dm"] [data-open-in-chat]'
    );
    expect(dmLink).not.toBeNull();
    expect(dmLink!.getAttribute('href')).toBe(
      `/chat/dm/${encodeURIComponent('dm:agent:agent-1:user:user-1')}#msg-msg-d1`
    );

    // Purged card: shows "Original message no longer available", no link.
    const purgedEl = inner.shadowRoot!.querySelector('[data-command-request-card="cr-purged"]')!;
    expect(purgedEl.querySelector('[data-open-in-chat]')).toBeNull();
    expect(purgedEl.querySelector('[data-purged-note]')?.textContent).toContain(
      'Original message no longer available'
    );

    // Missing threadId card: shows neither link nor purged note.
    const noThreadEl = inner.shadowRoot!.querySelector(
      '[data-command-request-card="cr-nothread"]'
    )!;
    expect(noThreadEl.querySelector('[data-open-in-chat]')).toBeNull();
    expect(noThreadEl.querySelector('[data-purged-note]')).toBeNull();
  });
});
