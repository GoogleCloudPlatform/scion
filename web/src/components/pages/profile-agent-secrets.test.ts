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
 * Tests for `<scion-page-profile-agent-secrets>` (`/profile/agent-secrets`),
 * `<scion-action-inbox-page>`, and `mergeSecretRequestCardFragments`
 * (ptone/scion#9299, design §5.5, AC-5, AC-6, AC-12, AC-13).
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { SecretRequestCardResponse, SecretRequestView } from '../../client/secret-requests.js';
import { resetActionInboxProbeCacheForTests } from '../../client/action-inbox.js';
import { stateManager } from '../../client/state.js';
import {
  AGENT_SECRETS_FLAG,
  INBOX_DASHBOARD_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import './profile-agent-secrets.js';
import {
  mergeSecretRequestCardFragments,
  type ScionPageProfileAgentSecrets,
} from './profile-agent-secrets.js';
import type { ScionActionInboxPage } from '../shared/action-inbox-page.js';
import type { ScionChatSecretRequest } from '../shared/chat/chat-secret-request.js';

function makeRow(overrides: Partial<SecretRequestView> = {}): SecretRequestView {
  return {
    rowId: 'row-1',
    messageId: 'msg-1',
    projectId: 'proj-1',
    agentId: 'agent-1',
    agentName: 'builder-1',
    key: 'GITHUB_TOKEN',
    version: 1,
    description: 'Token for CI',
    secretType: 'environment',
    target: '',
    position: 0,
    status: 'pending',
    submitting: false,
    createdAt: '2026-11-26T10:00:00Z',
    expiresAt: '2026-12-03T10:00:00Z',
    ...overrides,
  };
}

function makeCard(overrides: Partial<SecretRequestCardResponse> = {}): SecretRequestCardResponse {
  return {
    messageId: 'msg-1',
    projectId: 'proj-1',
    projectName: 'Core API',
    agentId: 'agent-1',
    agentName: 'builder-1',
    rows: [makeRow()],
    canRespond: true,
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

describe('<scion-page-profile-agent-secrets>', () => {
  let fetchMock: ReturnType<typeof vi.fn<typeof fetch>>;

  beforeEach(() => {
    window.history.replaceState({}, '', '/profile/agent-secrets');
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

  it('renders 404 and makes zero API calls when hub.agent_secrets is off (AC-13)', async () => {
    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await flush();
    await page.updateComplete;

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;
    expect(inner.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('renders 404 when hub returns 404 on /api/v1/users/me/secret-requests', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
    fetchMock.mockResolvedValue(jsonResp({ error: { code: 'not_found', message: 'off' } }, 404));

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;
    expect(inner.shadowRoot!.querySelector('scion-page-404')).not.toBeNull();
  });

  it('lists open cards with context, marks waiting-on-someone-else, passes .card without per-card fetch, and renders active Inbox tab (AC-5, AC-6)', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
    setFeatureFlag(INBOX_DASHBOARD_FLAG, true);

    const answerableCard = makeCard({ messageId: 'msg-1', canRespond: true });
    const waitingOthersCard = makeCard({
      messageId: 'msg-2',
      projectId: 'proj-2',
      projectName: 'Web UI',
      agentId: 'agent-2',
      agentName: 'fe-bot',
      canRespond: false,
      reason: 'you need agent.attach permission on this agent',
      rows: [makeRow({ rowId: 'row-2', messageId: 'msg-2', key: 'NPM_TOKEN' })],
    });

    fetchMock.mockResolvedValue(
      jsonResp({
        counts: { answerable: 1, open: 2 },
        cards: [answerableCard, waitingOthersCard],
        facets: {
          agents: [{ id: 'agent-1', name: 'builder-1', count: 1 }],
          projects: [{ id: 'proj-1', name: 'Core API', count: 1 }],
        },
        truncatedProjects: false,
        capped: false,
      })
    );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    // Only one list fetch, no per-card /api/v1/secret-requests/{id} fetch.
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0] as string).toContain('/api/v1/users/me/secret-requests?');

    // Inbox tab strip shows Overview | Agent Secrets with Agent Secrets active.
    const activeTab = inner.shadowRoot!.querySelector<HTMLAnchorElement>(
      '[data-inbox-tab="agent-secrets"]'
    );
    expect(activeTab).not.toBeNull();
    expect(activeTab!.classList.contains('active')).toBe(true);
    expect(activeTab!.getAttribute('aria-current')).toBe('page');

    // Both cards rendered; second is marked waiting on someone else.
    const card1 = inner.shadowRoot!.querySelector('[data-secret-request-card="msg-1"]')!;
    const card2 = inner.shadowRoot!.querySelector('[data-secret-request-card="msg-2"]')!;
    expect(card1.querySelector('[data-waiting-on-others]')).toBeNull();
    expect(card2.querySelector('[data-waiting-on-others]')?.textContent).toContain(
      'Waiting on someone else'
    );

    // Child <scion-chat-secret-request> received .card directly.
    const srEl = card1.querySelector<ScionChatSecretRequest>('scion-chat-secret-request')!;
    await srEl.updateComplete;
    expect(srEl.messageId).toBe('msg-1');
    expect(srEl.rows).toHaveLength(1);
    expect(srEl.canRespond).toBe(true);
    expect(srEl.surface).toBe('standalone_page');
  });

  it('switches to Resolved tab and merges paginated card fragments by messageId', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, true);

    fetchMock
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      )
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [
            makeCard({
              messageId: 'msg-multi',
              canRespond: false,
              rows: [makeRow({ rowId: 'r1', key: 'KEY_A', position: 0, status: 'fulfilled' })],
            }),
          ],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'cursor-page-2',
        })
      )
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [
            makeCard({
              messageId: 'msg-multi',
              canRespond: false,
              rows: [makeRow({ rowId: 'r2', key: 'KEY_B', position: 1, status: 'dismissed' })],
            }),
          ],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;
    expect(inner.shadowRoot!.querySelector('[data-action-empty]')?.textContent).toContain(
      'Nothing Waiting'
    );

    // Switch to Resolved tab.
    const resolvedBtn = inner.shadowRoot!.querySelector<HTMLButtonElement>(
      '[data-action-tab="resolved"]'
    )!;
    resolvedBtn.click();
    await flush();
    await inner.updateComplete;

    expect(fetchMock.mock.calls[1][0] as string).toContain('tab=resolved');
    const loadMoreBtn = inner.shadowRoot!.querySelector<HTMLElement>('[data-action-load-more]')!;
    expect(loadMoreBtn).not.toBeNull();

    // Load page 2; fragment for msg-multi merges into the existing card.
    loadMoreBtn.click();
    await flush();
    await inner.updateComplete;

    expect(inner.shadowRoot!.querySelectorAll('[data-secret-request-card]')).toHaveLength(1);
    const srEl = inner.shadowRoot!.querySelector<ScionChatSecretRequest>(
      'scion-chat-secret-request'
    )!;
    await srEl.updateComplete;
    expect(srEl.rows.map((r) => r.key)).toEqual(['KEY_A', 'KEY_B']);
  });

  it('refreshes live on message-action-updated for secret_request (debounced)', async () => {
    vi.useFakeTimers();
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
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
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await vi.advanceTimersByTimeAsync(20);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // Different kind is ignored.
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'other_kind' } } })
    );
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // secret_request event triggers debounced reload after 750ms.
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'secret_request' } } })
    );
    await vi.advanceTimersByTimeAsync(800);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('mergeSecretRequestCardFragments deduplicates rows and preserves position order', () => {
    const a = makeCard({
      messageId: 'm1',
      rows: [makeRow({ rowId: 'r2', position: 1, key: 'B', status: 'pending' })],
    });
    const b = makeCard({
      messageId: 'm1',
      rows: [
        makeRow({ rowId: 'r1', position: 0, key: 'A', status: 'fulfilled' }),
        makeRow({ rowId: 'r2', position: 1, key: 'B', status: 'fulfilled' }),
      ],
    });
    const merged = mergeSecretRequestCardFragments(a, b);
    expect(merged.rows.map((r) => `${r.key}:${r.status}`)).toEqual(['A:fulfilled', 'B:fulfilled']);
  });

  it('resets loadingMore and ignores stale page when an SSE refresh supersedes in-flight loadMore', async () => {
    vi.useFakeTimers();
    setFeatureFlag(AGENT_SECRETS_FLAG, true);

    let resolveStaleMore!: (resp: Response) => void;
    const staleMorePromise = new Promise<Response>((resolve) => {
      resolveStaleMore = resolve;
    });

    fetchMock
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'msg-1' })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'c2',
        })
      )
      .mockReturnValueOnce(staleMorePromise)
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'msg-1-refreshed' })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'c2-fresh',
        })
      )
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'msg-2-fresh' })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await vi.advanceTimersByTimeAsync(20);

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    // Start loadMore (held open), then trigger SSE live refresh mid-flight.
    const moreCall = inner.ctrl.loadMore();
    expect(inner.ctrl.loadingMore).toBe(true);

    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'secret_request' } } })
    );
    await vi.advanceTimersByTimeAsync(800);

    // Resolve the superseded loadMore response late; it must be ignored.
    resolveStaleMore(
      jsonResp({
        counts: { answerable: 2, open: 2 },
        cards: [makeCard({ messageId: 'msg-stale' })],
        facets: { agents: [], projects: [] },
        truncatedProjects: false,
        capped: false,
      })
    );
    await moreCall;
    await inner.updateComplete;

    expect(inner.ctrl.loadingMore).toBe(false);
    expect(inner.ctrl.cards.map((c) => (c as SecretRequestCardResponse).messageId)).toEqual([
      'msg-1-refreshed',
    ]);

    // A subsequent loadMore must still work and fetch page 2.
    await inner.ctrl.loadMore();
    await inner.updateComplete;
    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(inner.ctrl.cards.map((c) => (c as SecretRequestCardResponse).messageId)).toEqual([
      'msg-1-refreshed',
      'msg-2-fresh',
    ]);
  });

  it('resets loadingMore and ignores stale page when a filter change supersedes in-flight loadMore', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, true);

    let resolveStaleMore!: (resp: Response) => void;
    const staleMorePromise = new Promise<Response>((resolve) => {
      resolveStaleMore = resolve;
    });

    fetchMock
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'msg-1' })],
          facets: {
            agents: [],
            projects: [{ id: 'proj-2', name: 'Web UI', count: 1 }],
          },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'c2',
        })
      )
      .mockReturnValueOnce(staleMorePromise)
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'msg-proj2-p1', projectId: 'proj-2' })],
          facets: {
            agents: [],
            projects: [{ id: 'proj-2', name: 'Web UI', count: 1 }],
          },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'c2-proj2',
        })
      )
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'msg-proj2-p2', projectId: 'proj-2' })],
          facets: {
            agents: [],
            projects: [{ id: 'proj-2', name: 'Web UI', count: 1 }],
          },
          truncatedProjects: false,
          capped: false,
        })
      );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    const moreCall = inner.ctrl.loadMore();
    expect(inner.ctrl.loadingMore).toBe(true);

    // Change project filter while loadMore is in flight.
    inner.ctrl.setProject('proj-2');
    await flush();

    resolveStaleMore(
      jsonResp({
        counts: { answerable: 2, open: 2 },
        cards: [makeCard({ messageId: 'msg-stale' })],
        facets: { agents: [], projects: [] },
        truncatedProjects: false,
        capped: false,
      })
    );
    await moreCall;
    await inner.updateComplete;

    expect(inner.ctrl.loadingMore).toBe(false);
    expect(inner.ctrl.cards.map((c) => (c as SecretRequestCardResponse).messageId)).toEqual([
      'msg-proj2-p1',
    ]);

    // Pagination remains functional after the filter change.
    await inner.ctrl.loadMore();
    await inner.updateComplete;
    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(inner.ctrl.cards.map((c) => (c as SecretRequestCardResponse).messageId)).toEqual([
      'msg-proj2-p1',
      'msg-proj2-p2',
    ]);
  });

  it('preserves multi-page Resolved list on message-action-updated and shows a Refresh affordance', async () => {
    vi.useFakeTimers();
    setFeatureFlag(AGENT_SECRETS_FLAG, true);

    fetchMock
      // Initial Open load
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      )
      // Resolved page 1
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [makeCard({ messageId: 'res-1', canRespond: false })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'cur-2',
        })
      )
      // Resolved page 2
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [makeCard({ messageId: 'res-2', canRespond: false })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'cur-3',
        })
      )
      // Resolved page 3
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [makeCard({ messageId: 'res-3', canRespond: false })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      )
      // Manual refresh after clicking the affordance
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 0, open: 0 },
          cards: [makeCard({ messageId: 'res-new', canRespond: false })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await vi.advanceTimersByTimeAsync(20);

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    // Load 3 Resolved pages.
    inner.ctrl.setTab('resolved');
    await vi.advanceTimersByTimeAsync(20);
    await inner.ctrl.loadMore();
    await inner.ctrl.loadMore();
    await inner.updateComplete;

    expect(inner.ctrl.cards.map((c) => (c as SecretRequestCardResponse).messageId)).toEqual([
      'res-1',
      'res-2',
      'res-3',
    ]);
    expect(fetchMock).toHaveBeenCalledTimes(4);

    // Fire SSE update: list must NOT collapse and the affordance must appear.
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'secret_request' } } })
    );
    await vi.advanceTimersByTimeAsync(1000);
    await inner.updateComplete;

    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect(inner.ctrl.cards.map((c) => (c as SecretRequestCardResponse).messageId)).toEqual([
      'res-1',
      'res-2',
      'res-3',
    ]);
    const bar = inner.shadowRoot!.querySelector('[data-action-new-activity]');
    expect(bar).not.toBeNull();

    // Clicking Refresh reloads and clears the affordance.
    const refreshBtn = inner.shadowRoot!.querySelector<HTMLElement>(
      '[data-action-new-activity-refresh]'
    )!;
    refreshBtn.click();
    await vi.advanceTimersByTimeAsync(20);
    await inner.updateComplete;

    expect(fetchMock).toHaveBeenCalledTimes(5);
    expect(inner.shadowRoot!.querySelector('[data-action-new-activity]')).toBeNull();
    expect(inner.ctrl.cards.map((c) => (c as SecretRequestCardResponse).messageId)).toEqual([
      'res-new',
    ]);
  });

  it('uses aria-pressed toggle buttons for Open/Resolved and kind-supplied searchPlaceholder', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
    fetchMock.mockResolvedValue(
      jsonResp({
        counts: { answerable: 0, open: 0 },
        cards: [],
        facets: { agents: [], projects: [] },
        truncatedProjects: false,
        capped: false,
      })
    );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    expect(inner.shadowRoot!.querySelector('[role="tablist"]')).toBeNull();
    expect(inner.shadowRoot!.querySelector('[role="tab"]')).toBeNull();
    const openBtn = inner.shadowRoot!.querySelector<HTMLButtonElement>('[data-action-tab="open"]')!;
    const resolvedBtn = inner.shadowRoot!.querySelector<HTMLButtonElement>(
      '[data-action-tab="resolved"]'
    )!;
    expect(openBtn.getAttribute('aria-pressed')).toBe('true');
    expect(resolvedBtn.getAttribute('aria-pressed')).toBe('false');

    const searchInput = inner.shadowRoot!.querySelector<HTMLInputElement>('[data-filter-search]')!;
    expect(searchInput.placeholder).toBe('Filter by key, description, or agent…');
  });

  it('refreshes on page 1 when a rendered card dispatches secret-request-fulfilled or secret-request-dismissed', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
    fetchMock.mockImplementation(() =>
      Promise.resolve(
        jsonResp({
          counts: { answerable: 1, open: 1 },
          cards: [makeCard({ messageId: 'msg-wire-1', canRespond: true })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      )
    );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await flush();

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;
    expect(fetchMock).toHaveBeenCalledTimes(1);

    const cardEl = inner.shadowRoot!.querySelector<ScionChatSecretRequest>(
      'scion-chat-secret-request'
    )!;
    expect(cardEl).not.toBeNull();

    cardEl.dispatchEvent(
      new CustomEvent('secret-request-fulfilled', {
        detail: { messageId: 'msg-wire-1', rows: [], responseMessageId: 'r1', replayed: false },
        bubbles: true,
        composed: true,
      })
    );
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(2);

    cardEl.dispatchEvent(
      new CustomEvent('secret-request-dismissed', {
        detail: { messageId: 'msg-wire-1', rows: [], responseMessageId: 'r2', replayed: false },
        bubbles: true,
        composed: true,
      })
    );
    await flush();
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('preserves multi-page Open tab on SSE update and page-2 fulfill, showing the Refresh affordance', async () => {
    vi.useFakeTimers();
    setFeatureFlag(AGENT_SECRETS_FLAG, true);

    fetchMock
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'open-p1', canRespond: true })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
          nextCursor: 'open-cur-2',
        })
      )
      .mockResolvedValueOnce(
        jsonResp({
          counts: { answerable: 2, open: 2 },
          cards: [makeCard({ messageId: 'open-p2', canRespond: true })],
          facets: { agents: [], projects: [] },
          truncatedProjects: false,
          capped: false,
        })
      );

    const page = document.createElement(
      'scion-page-profile-agent-secrets'
    ) as ScionPageProfileAgentSecrets;
    document.body.appendChild(page);
    await vi.advanceTimersByTimeAsync(20);

    const inner = page.shadowRoot!.querySelector<ScionActionInboxPage>('scion-action-inbox-page')!;
    await inner.updateComplete;

    // Load page 2 on the Open tab.
    await inner.ctrl.loadMore();
    await inner.updateComplete;
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(inner.shadowRoot!.querySelector('[data-secret-request-card="open-p2"]')).not.toBeNull();

    // 1. SSE event arrives while 2 pages are loaded: page-2 card stays mounted and affordance shows.
    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', { detail: { data: { kind: 'secret_request' } } })
    );
    await vi.advanceTimersByTimeAsync(1000);
    await inner.updateComplete;

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(inner.shadowRoot!.querySelector('[data-secret-request-card="open-p2"]')).not.toBeNull();
    expect(inner.shadowRoot!.querySelector('[data-action-new-activity]')).not.toBeNull();

    // Clear affordance state directly to verify page-2 Fulfill also triggers it without unmounting.
    inner.ctrl.hasNewActivity = false;
    inner.requestUpdate();
    await inner.updateComplete;
    expect(inner.shadowRoot!.querySelector('[data-action-new-activity]')).toBeNull();

    // 2. Fulfill on page-2 card: page-2 card stays mounted, no auto-fetch, and affordance shows.
    const page2CardEl = inner.shadowRoot!.querySelector<ScionChatSecretRequest>(
      '[data-secret-request-card="open-p2"] scion-chat-secret-request'
    )!;
    page2CardEl.dispatchEvent(
      new CustomEvent('secret-request-fulfilled', {
        detail: { messageId: 'open-p2', rows: [], responseMessageId: 'r-p2', replayed: false },
        bubbles: true,
        composed: true,
      })
    );
    await vi.advanceTimersByTimeAsync(20);
    await inner.updateComplete;

    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(inner.shadowRoot!.querySelector('[data-secret-request-card="open-p2"]')).not.toBeNull();
    expect(inner.shadowRoot!.querySelector('[data-action-new-activity]')).not.toBeNull();
  });
});
