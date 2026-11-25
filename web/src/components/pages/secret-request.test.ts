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
} from '../../utils/feature-flags.js';
import { stateManager } from '../../client/state.js';
import type { SecretRequestCardResponse } from '../../client/secret-requests.js';
import { crumbTuples } from '../../test-utils/crumbs.js';
import './secret-request.js';
import type { ScionPageSecretRequest } from './secret-request.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeCard(overrides: Partial<SecretRequestCardResponse> = {}): SecretRequestCardResponse {
  return {
    messageId: 'msg-100',
    projectId: 'proj-1',
    projectName: 'alpha-project',
    agentId: 'agent-1',
    agentName: 'release-bot',
    intro: 'I need a token to push the release branch.',
    canRespond: true,
    reason: '',
    rows: [
      {
        rowId: 'row-1',
        messageId: 'msg-100',
        projectId: 'proj-1',
        agentId: 'agent-1',
        agentName: 'release-bot',
        key: 'GITHUB_TOKEN',
        version: 1,
        description: 'GitHub PAT',
        secretType: 'environment',
        position: 0,
        status: 'pending',
        createdAt: '2026-11-22T23:50:00Z',
        expiresAt: '2026-11-29T23:50:00Z',
      },
    ],
    ...overrides,
  };
}

describe('scion-page-secret-request', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    localStorage.clear();
    setFeatureFlag(AGENT_SECRETS_FLAG, true);
    vi.spyOn(stateManager, 'setScope').mockImplementation(() => {});
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
  });

  it('renders 404 and makes zero API calls when hub.agent_secrets is off', async () => {
    setFeatureFlag(AGENT_SECRETS_FLAG, false);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const page = document.createElement('scion-page-secret-request') as ScionPageSecretRequest;
    page.pageData = { path: '/secret-requests/msg-100', title: 'Scion' };
    document.body.appendChild(page);
    await page.updateComplete;

    expect(fetchMock).not.toHaveBeenCalled();
    expect(page.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
  });

  it('loads the card from GET /api/v1/secret-requests/{messageId} and renders the component with surface=standalone_page', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(makeCard()));
    vi.stubGlobal('fetch', fetchMock);

    const page = document.createElement('scion-page-secret-request') as ScionPageSecretRequest;
    page.pageData = {
      path: '/secret-requests/msg-100',
      title: 'Scion',
      user: { id: 'u-1', email: 'alice@example.com', name: 'Alice' },
    };
    document.body.appendChild(page);

    await vi.waitFor(() => {
      expect(page.shadowRoot?.querySelector('scion-chat-secret-request')).not.toBeNull();
    });
    await page.updateComplete;

    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe('/api/v1/secret-requests/msg-100');
    expect(page.shadowRoot?.textContent).toContain('release-bot');
    expect(page.shadowRoot?.textContent).toContain('alpha-project');
    expect(page.shadowRoot?.textContent).toContain('I need a token to push the release branch.');

    const card = page.shadowRoot?.querySelector('scion-chat-secret-request');
    expect(card).not.toBeNull();
    expect(card?.getAttribute('surface')).toBe('standalone_page');

    expect(crumbTuples(page.shadowRoot)).toEqual([
      ['Projects', '/projects'],
      ['alpha-project', '/projects/proj-1'],
      ['Secret Request', undefined],
    ]);
    expect(page.shadowRoot?.querySelector('a.back-link')).toBeNull();
  });

  it('renders ancestor-only Dashboard breadcrumb in loading and error states and falls back when projectId is absent', async () => {
    // Loading state: pending fetch keeps page in loading=true
    let resolveFetch!: (res: Response) => void;
    const pendingFetch = new Promise<Response>((resolve) => {
      resolveFetch = resolve;
    });
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(pendingFetch));

    const loadingPage = document.createElement(
      'scion-page-secret-request'
    ) as ScionPageSecretRequest;
    loadingPage.pageData = { path: '/secret-requests/msg-100', title: 'Scion' };
    document.body.appendChild(loadingPage);
    await loadingPage.updateComplete;

    expect(crumbTuples(loadingPage.shadowRoot)).toEqual([['Dashboard', '/']]);
    expect(loadingPage.shadowRoot?.querySelector('a.back-link')).toBeNull();

    // Error state: 404 response
    resolveFetch(jsonResponse({ error: { code: 'not_found', message: 'Not found' } }, 404));
    await vi.waitFor(() => {
      expect(loadingPage.shadowRoot?.querySelector('.state-card.error')).not.toBeNull();
    });
    await loadingPage.updateComplete;

    expect(crumbTuples(loadingPage.shadowRoot)).toEqual([['Dashboard', '/']]);
    expect(loadingPage.shadowRoot?.querySelector('a.back-link')).toBeNull();

    // Loaded state with no projectId falls back to Dashboard / Secret Request
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(makeCard({ projectId: '', projectName: '' })))
    );
    const noProjectPage = document.createElement(
      'scion-page-secret-request'
    ) as ScionPageSecretRequest;
    noProjectPage.pageData = { path: '/secret-requests/msg-100', title: 'Scion' };
    document.body.appendChild(noProjectPage);

    await vi.waitFor(() => {
      expect(noProjectPage.shadowRoot?.querySelector('scion-chat-secret-request')).not.toBeNull();
    });
    await noProjectPage.updateComplete;

    expect(crumbTuples(noProjectPage.shadowRoot)).toEqual([
      ['Dashboard', '/'],
      ['Secret Request', undefined],
    ]);
  });

  it('shows replacement banner with link to the current version for an old replaced link', async () => {
    const replacedCard = makeCard({
      canRespond: false,
      reason: 'This request has been replaced.',
      rows: [
        {
          rowId: 'row-1',
          messageId: 'msg-100',
          projectId: 'proj-1',
          agentId: 'agent-1',
          agentName: 'release-bot',
          key: 'GITHUB_TOKEN',
          version: 1,
          secretType: 'environment',
          position: 0,
          status: 'replaced',
          createdAt: '2026-11-22T23:50:00Z',
          expiresAt: '2026-11-29T23:50:00Z',
          replacedBy: {
            rowId: 'row-2',
            messageId: 'msg-200',
            version: 2,
            at: '2026-11-22T23:55:00Z',
          },
        },
      ],
    });
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(replacedCard));
    vi.stubGlobal('fetch', fetchMock);

    const page = document.createElement('scion-page-secret-request') as ScionPageSecretRequest;
    page.pageData = { path: '/secret-requests/msg-100', title: 'Scion' };
    document.body.appendChild(page);

    await vi.waitFor(() => {
      expect(page.shadowRoot?.querySelector('.replacement-banner')).not.toBeNull();
    });
    await page.updateComplete;

    const banner = page.shadowRoot?.querySelector('.replacement-banner');
    expect(banner).not.toBeNull();
    expect(banner?.textContent).toContain('replaced by a newer version');
    const link = banner?.querySelector('a.replacement-link') as HTMLAnchorElement;
    expect(link).not.toBeNull();
    expect(link.getAttribute('href')).toBe('/secret-requests/msg-200');
    expect(link.textContent).toContain('v2');
  });

  it('updates replacement banner live via the embedded card controller without duplicate stateManager subscriptions', async () => {
    const addListenerSpy = vi.spyOn(stateManager, 'addEventListener');
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(makeCard()));
    vi.stubGlobal('fetch', fetchMock);

    const page = document.createElement('scion-page-secret-request') as ScionPageSecretRequest;
    page.pageData = { path: '/secret-requests/msg-100', title: 'Scion' };
    document.body.appendChild(page);

    await vi.waitFor(() => {
      expect(page.shadowRoot?.querySelector('scion-chat-secret-request')).not.toBeNull();
    });
    await page.updateComplete;

    const actionSubscriptions = addListenerSpy.mock.calls.filter(
      ([eventName]) => eventName === 'message-action-updated'
    );
    expect(actionSubscriptions).toHaveLength(1);

    stateManager.dispatchEvent(
      new CustomEvent('message-action-updated', {
        detail: {
          data: {
            kind: 'secret_request',
            messageId: 'msg-100',
            secret_request: {
              ...makeCard().rows[0],
              status: 'replaced',
              replacedBy: {
                rowId: 'row-2',
                messageId: 'msg-201',
                version: 2,
                at: '2026-11-22T23:59:00Z',
              },
            },
          },
        },
      })
    );

    await vi.waitFor(() => {
      expect(page.shadowRoot?.querySelector('.replacement-banner')).not.toBeNull();
    });
    const link = page.shadowRoot?.querySelector('a.replacement-link') as HTMLAnchorElement;
    expect(link.getAttribute('href')).toBe('/secret-requests/msg-201');
  });
});
