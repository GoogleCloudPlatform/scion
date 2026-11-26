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
} from '../../../utils/feature-flags.js';
import { stateManager } from '../../../client/state.js';
import type { CommandRequestDetailResponse } from '../../../client/command-requests.js';

const navigateToMock = vi.hoisted(() => vi.fn());

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  stateManager,
  navigateTo: navigateToMock,
}));

await import('./chat-message.js');
type ScionChatMessage = import('./chat-message.js').ScionChatMessage;

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
      commandSha256: 'b'.repeat(64),
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
    threadId: 'dm:agent:agent-1:user:user-1',
    intro: '**Please approve** [decoy](https://evil.example)',
    approverName: 'Alice',
    canApprove: true,
    ...overrides,
  };
}

describe('native chat command-request wiring', () => {
  beforeEach(() => {
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
    localStorage.clear();
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
  });

  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    resetServerFlagStateForTests();
    delete window.__SCION_FEATURES__;
  });

  it('renders <scion-chat-command-request surface="native"> and keeps intro as plain text (audit L4)', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(makeDetail()));
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-cmd-1';
    msg.messageType = 'command-request';
    msg.body = '**Please approve**\n\n```sh\ngcloud config list\n```';
    msg.fromAgent = true;
    msg.senderIsAgent = true;
    msg.sender = 'agent:deploy-bot';
    msg.senderName = 'deploy-bot';
    msg.currentUserId = 'user-1';
    document.body.appendChild(msg);

    await vi.waitFor(() => {
      expect(msg.shadowRoot?.querySelector('scion-chat-command-request')).not.toBeNull();
    });
    await msg.updateComplete;

    // L4: intro is rendered as plain text, NEVER Markdown
    const intro = msg.shadowRoot?.querySelector('[data-testid="command-request-intro"]');
    expect(intro?.textContent?.trim()).toBe('**Please approve** [decoy](https://evil.example)');
    expect(intro?.querySelector('strong')).toBeNull();
    expect(intro?.querySelector('a')).toBeNull();
    expect(msg.shadowRoot?.querySelector('.md-content')).toBeNull();
  });

  it('does not fetch or render card when hub.command_requests is off', async () => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, false);
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);

    const msg = document.createElement('scion-chat-message') as ScionChatMessage;
    msg.messageId = 'msg-cmd-1';
    msg.messageType = 'command-request';
    msg.body = '**Command request** `deploy-preview`';
    msg.fromAgent = true;
    document.body.appendChild(msg);
    await msg.updateComplete;

    expect(fetchMock).not.toHaveBeenCalled();
    expect(msg.shadowRoot?.querySelector('scion-chat-command-request')).toBeNull();
  });
});
