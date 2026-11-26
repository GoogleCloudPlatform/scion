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
  COMMAND_REQUESTS_FLAG,
  resetServerFlagStateForTests,
  setFeatureFlag,
} from '../../utils/feature-flags.js';
import { stateManager } from '../../client/state.js';
import type { CommandRequestDetailResponse } from '../../client/command-requests.js';
import './command-request.js';
import type { ScionPageCommandRequest } from './command-request.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function makeDetail(
  overrides: Partial<CommandRequestDetailResponse> = {}
): CommandRequestDetailResponse {
  return {
    request: {
      rowId: 'row-cmd-1',
      messageId: 'msg-cmd-1',
      projectId: 'proj-1',
      agentId: 'agent-1',
      agentName: 'deploy-bot',
      key: 'deploy-preview',
      version: 1,
      description: 'Deploy PR 42',
      script: 'gcloud config list\n',
      workdir: '',
      workdirKind: 'home',
      timeoutS: 600,
      shareOutputRequested: false,
      commandSha256: 'c'.repeat(64),
      approverUserId: 'user-1',
      status: 'pending',
      createdAt: '2026-11-26T10:00:00Z',
      expiresAt: '2026-11-27T10:00:00Z',
    },
    exec_container: {
      state: 'ready',
      agentId: 'exec-1',
      agentName: 'alice-shell',
      projectName: 'alpha',
      homePersistent: true,
    },
    projectName: 'alpha',
    intro: '**plain text intro**',
    approverName: 'Alice',
    canApprove: true,
    ...overrides,
  };
}

describe('<scion-page-command-request>', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    localStorage.clear();
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
    vi.spyOn(stateManager, 'setScope').mockImplementation(() => {});
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
  });

  it('renders 404 and makes zero API calls when hub.command_requests is off', async () => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, false);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const page = document.createElement('scion-page-command-request') as ScionPageCommandRequest;
    page.requestId = 'row-cmd-1';
    document.body.appendChild(page);
    await page.updateComplete;

    expect(fetchMock).not.toHaveBeenCalled();
    expect(page.shadowRoot?.querySelector('scion-page-404')).not.toBeNull();
  });

  it('loads command request detail and renders plain-text intro and card', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(makeDetail()));
    vi.stubGlobal('fetch', fetchMock);

    const page = document.createElement('scion-page-command-request') as ScionPageCommandRequest;
    page.requestId = 'row-cmd-1';
    document.body.appendChild(page);

    await vi.waitFor(() => {
      expect(page.shadowRoot?.querySelector('scion-chat-command-request')).not.toBeNull();
    });

    const intro = page.shadowRoot?.querySelector('[data-testid="command-request-intro"]');
    expect(intro?.textContent).toBe('**plain text intro**');
    expect(intro?.querySelector('strong')).toBeNull();
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/command-requests/row-cmd-1',
      expect.objectContaining({ method: 'GET' })
    );
  });
});
