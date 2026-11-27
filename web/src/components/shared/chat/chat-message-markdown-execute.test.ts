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
import { COMMAND_REQUESTS_FLAG, setFeatureFlag } from '../../../utils/feature-flags.js';
import {
  computeCommandRequestCanonicalHash,
  type CommandRequestExecContainerView,
} from '../../../client/command-requests.js';

vi.mock('../../../client/main.js', async () => {
  const { stateManager } = await import('../../../client/state.js');
  return {
    navigateTo: vi.fn(),
    stateManager,
  };
});

import './chat-message.js';
import './chat-thread.js';
import type { ScionChatMessage } from './chat-message.js';
import type { ScionChatThread } from './chat-thread.js';
import type { ScionCommandConfirmDialog } from './command-confirm-dialog.js';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const READY_CONTAINER: CommandRequestExecContainerView = {
  state: 'ready',
  agentId: 'exec-1',
  agentName: 'alice-shell',
  projectId: 'proj-1',
  projectName: 'alpha',
  homePersistent: true,
};

function createMessageEl(overrides: Partial<ScionChatMessage> = {}): ScionChatMessage {
  const el = document.createElement('scion-chat-message') as ScionChatMessage;
  el.messageId = 'msg-md-1';
  el.sender = 'agent:deploy-bot';
  el.senderId = 'agent-1';
  el.senderIsAgent = true;
  el.fromAgent = true;
  el.currentUserId = 'user-alice';
  el.isDirectConversation = true;
  el.timestamp = new Date().toISOString();
  el.execContainer = READY_CONTAINER;
  Object.assign(el, overrides);
  document.body.appendChild(el);
  return el;
}

describe('Markdown Execute on fenced shell blocks (§4.14, AC-F9)', () => {
  beforeEach(() => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, true);
  });

  afterEach(() => {
    setFeatureFlag(COMMAND_REQUESTS_FLAG, false);
    document.body.innerHTML = '';
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders Copy + Execute in a DM agent sh/bash/shell/console block and runs via confirm dialog', async () => {
    const script = 'gcloud config list\n';
    const expectedHash = await computeCommandRequestCanonicalHash(script, '', 600);
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          request: {
            rowId: 'row-md-1',
            messageId: 'msg-md-1',
            projectId: 'proj-1',
            agentId: 'agent-1',
            agentName: 'deploy-bot',
            key: `md-msg-md-1-${expectedHash.slice(0, 16)}`,
            version: 1,
            script,
            workdir: '',
            workdirKind: 'home',
            timeoutS: 600,
            shareOutputRequested: false,
            commandSha256: expectedHash,
            approverUserId: 'user-alice',
            status: 'approved',
            createdAt: new Date().toISOString(),
            expiresAt: new Date(Date.now() + 86400000).toISOString(),
          },
          run: {
            runId: 'run-md-1',
            requestId: 'row-md-1',
            status: 'queued',
            shareOutput: false,
            timeoutS: 600,
            createdAt: new Date().toISOString(),
          },
        },
        202
      )
    );
    vi.stubGlobal('fetch', fetchMock);

    const el = createMessageEl({
      body: 'Run this:\n\n```sh\ngcloud config list\n```\n',
    });

    await vi.waitFor(() => {
      const wrapper = el.shadowRoot?.querySelector('.code-block[data-shell-block="true"]');
      expect(wrapper).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-copy-btn"]')).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-execute-btn"]')).not.toBeNull();
    });

    const wrapper = el.shadowRoot?.querySelector(
      '.code-block[data-shell-block="true"]'
    ) as HTMLElement;
    expect(wrapper.dataset.rawScript).toBe(script);

    // Click Execute -> opens shared confirm dialog with share output OFF by default.
    (wrapper.querySelector('[data-testid="code-block-execute-btn"]') as HTMLButtonElement).click();
    await el.updateComplete;

    const dialog = el.shadowRoot?.querySelector(
      'scion-command-confirm-dialog'
    ) as ScionCommandConfirmDialog | null;
    expect(dialog).not.toBeNull();
    await dialog!.updateComplete;

    await vi.waitFor(() => {
      const sha = dialog!.shadowRoot?.querySelector('[data-testid="confirm-sha256"]')?.textContent;
      expect(sha).toContain(expectedHash);
    });

    const shareCheckbox = dialog!.shadowRoot?.querySelector(
      '[data-testid="confirm-share-output-checkbox"]'
    ) as HTMLElement & { checked?: boolean };
    expect(shareCheckbox?.hasAttribute('checked')).toBe(false);

    // Confirm -> POSTs /api/v1/command-requests/from-message with verified hash.
    (
      dialog!.shadowRoot?.querySelector('[data-testid="confirm-approve-btn"]') as HTMLElement
    ).click();

    await vi.waitFor(() => {
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('/api/v1/command-requests/from-message');
    const body = JSON.parse(init.body as string) as {
      message_id: string;
      script: string;
      command_sha256: string;
      share_output: boolean;
    };
    expect(body.message_id).toBe('msg-md-1');
    expect(body.script).toBe(script);
    expect(body.command_sha256).toBe(expectedHash);
    expect(body.share_output).toBe(false);

    // Wrapper now displays run-status chip linking to /command-requests/:id.
    await vi.waitFor(() => {
      const chip = wrapper.querySelector(
        '[data-testid="code-block-run-chip"]'
      ) as HTMLAnchorElement;
      expect(chip).not.toBeNull();
      expect(chip.getAttribute('href')).toBe('/command-requests/row-md-1');
      expect(chip.textContent).toContain('Queued');
    });
  });

  it('hides Execute but keeps Copy for no-execute, >24h, python, human-authored, and unaddressed group messages', async () => {
    // 1. no-execute shell block -> Copy present, no Execute.
    const noExecEl = createMessageEl({
      body: '```bash no-execute\nrm -rf /tmp/example\n```\n',
    });
    await vi.waitFor(() => {
      const wrapper = noExecEl.shadowRoot?.querySelector('.code-block[data-shell-block="true"]');
      expect(wrapper).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-copy-btn"]')).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-execute-btn"]')).toBeNull();
    });

    // 2. Message older than 24 h -> Copy present, no Execute.
    const oldEl = createMessageEl({
      timestamp: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString(),
      body: '```sh\necho old\n```\n',
    });
    await vi.waitFor(() => {
      const wrapper = oldEl.shadowRoot?.querySelector('.code-block[data-shell-block="true"]');
      expect(wrapper).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-copy-btn"]')).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-execute-btn"]')).toBeNull();
    });

    // 3. Python block -> not a shell block wrapper, no Execute.
    const pyEl = createMessageEl({
      body: '```python\nprint("hi")\n```\n',
    });
    await vi.waitFor(() => {
      expect(pyEl.shadowRoot?.querySelector('scion-code-editor')).not.toBeNull();
    });
    expect(pyEl.shadowRoot?.querySelector('[data-testid="code-block-execute-btn"]')).toBeNull();

    // 4. Group message not addressed to viewer alone -> Copy present, no Execute.
    const groupUnaddressed = createMessageEl({
      isDirectConversation: false,
      body: 'Anyone run:\n```sh\necho hi\n```\n',
      members: [
        { id: 'user-alice', kind: 'user', email: 'alice@example.com', displayName: 'Alice' },
        { id: 'user-bob', kind: 'user', email: 'bob@example.com', displayName: 'Bob' },
      ],
    });
    await vi.waitFor(() => {
      const wrapper = groupUnaddressed.shadowRoot?.querySelector(
        '.code-block[data-shell-block="true"]'
      );
      expect(wrapper).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-copy-btn"]')).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-execute-btn"]')).toBeNull();
    });

    // 5. Group message addressed to viewer alone via @-mention -> Copy + Execute.
    const groupSingle = createMessageEl({
      isDirectConversation: false,
      body: '@alice@example.com please run:\n```console\ngcloud config list\n```\n',
      members: [
        { id: 'user-alice', kind: 'user', email: 'alice@example.com', displayName: 'Alice' },
        { id: 'user-bob', kind: 'user', email: 'bob@example.com', displayName: 'Bob' },
      ],
    });
    await vi.waitFor(() => {
      const wrapper = groupSingle.shadowRoot?.querySelector('.code-block[data-shell-block="true"]');
      expect(wrapper).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-copy-btn"]')).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-execute-btn"]')).not.toBeNull();
    });
  });

  it('shows set-up link when unconfigured and disabled Execute when stopped', async () => {
    const unconfEl = createMessageEl({
      execContainer: { state: 'unconfigured' },
      body: '```sh\necho hi\n```\n',
    });
    await vi.waitFor(() => {
      const link = unconfEl.shadowRoot?.querySelector(
        '[data-testid="code-block-setup-link"]'
      ) as HTMLAnchorElement | null;
      expect(link).not.toBeNull();
      expect(link?.getAttribute('href')).toBe('/profile/settings');
      expect(
        unconfEl.shadowRoot?.querySelector('[data-testid="code-block-execute-btn"]')
      ).toBeNull();
    });

    const stoppedEl = createMessageEl({
      execContainer: { ...READY_CONTAINER, state: 'stopped' },
      body: '```sh\necho hi\n```\n',
    });
    await vi.waitFor(() => {
      const btn = stoppedEl.shadowRoot?.querySelector(
        '[data-testid="code-block-execute-btn"]'
      ) as HTMLButtonElement | null;
      expect(btn).not.toBeNull();
      expect(btn?.disabled).toBe(true);
      expect(btn?.title).toContain('Start your execution container');
    });
  });

  it('O1: hides Execute (keeps Copy) on shell fences nested inside a list item or blockquote', async () => {
    const listEl = createMessageEl({
      body: '1. Run this:\n\n   ```sh\n   echo nested\n   ```\n',
    });
    await vi.waitFor(() => {
      const wrapper = listEl.shadowRoot?.querySelector('.code-block[data-shell-block="true"]');
      expect(wrapper).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-copy-btn"]')).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-execute-btn"]')).toBeNull();
    });

    const quoteEl = createMessageEl({
      body: '> ```sh\n> echo quoted\n> ```\n',
    });
    await vi.waitFor(() => {
      const wrapper = quoteEl.shadowRoot?.querySelector('.code-block[data-shell-block="true"]');
      expect(wrapper).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-copy-btn"]')).not.toBeNull();
      expect(wrapper?.querySelector('[data-testid="code-block-execute-btn"]')).toBeNull();
    });
  });

  it('R1 + R2: wires members from <scion-chat-thread> for group single-@ Execute and hides on 2-human local-part or display-name mentions', async () => {
    const nowIso = new Date().toISOString();
    const fetchMock = vi.fn().mockImplementation((input: string | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url.includes('/api/v1/users/me/exec-container')) {
        return Promise.resolve(
          jsonResponse({
            configured: true,
            state: 'ready',
            agent: {
              id: 'exec-1',
              slug: 'alice-shell',
              name: 'alice-shell',
              projectId: 'proj-1',
              projectName: 'alpha',
              homePersistent: true,
            },
          })
        );
      }
      return Promise.resolve(jsonResponse({ messages: [] }));
    });
    vi.stubGlobal('fetch', fetchMock);

    const thread = document.createElement('scion-chat-thread') as ScionChatThread;
    thread.currentUserId = 'user-alice';
    thread.isDM = false;
    thread.members = [
      { id: 'user-alice', name: 'Alice Smith', email: 'alice@example.com', kind: 'user' },
      { id: 'user-bob', name: 'Robert Jones', email: 'bob@example.com', kind: 'user' },
    ];
    thread.messages = [
      {
        id: 'msg-grp-single',
        sender: 'agent:deploy-bot',
        senderId: 'agent-1',
        recipient: 'conv:conv-1',
        recipientId: '',
        msg: '@alice@example.com please run:\n\n```sh\necho solo\n```\n',
        type: 'instruction',
        createdAt: nowIso,
      },
      {
        id: 'msg-grp-two-slug',
        sender: 'agent:deploy-bot',
        senderId: 'agent-1',
        recipient: 'conv:conv-1',
        recipientId: '',
        msg: '@alice-smith @robert-jones please check:\n\n```sh\necho two-slug\n```\n',
        type: 'instruction',
        createdAt: nowIso,
      },
      {
        id: 'msg-grp-two-local',
        sender: 'agent:deploy-bot',
        senderId: 'agent-1',
        recipient: 'conv:conv-1',
        recipientId: '',
        msg: '@alice-smith @bob please check:\n\n```sh\necho two-local\n```\n',
        type: 'instruction',
        createdAt: nowIso,
      },
    ];
    document.body.appendChild(thread);
    await thread.updateComplete;

    await vi.waitFor(() => {
      const soloMsg = thread.shadowRoot?.querySelector(
        'scion-chat-message#msg-msg-grp-single'
      ) as ScionChatMessage | null;
      expect(soloMsg).not.toBeNull();
      expect(
        soloMsg?.shadowRoot?.querySelector('[data-testid="code-block-execute-btn"]')
      ).not.toBeNull();
    });

    await vi.waitFor(() => {
      const slugMsg = thread.shadowRoot?.querySelector(
        'scion-chat-message#msg-msg-grp-two-slug'
      ) as ScionChatMessage | null;
      const localMsg = thread.shadowRoot?.querySelector(
        'scion-chat-message#msg-msg-grp-two-local'
      ) as ScionChatMessage | null;
      expect(
        slugMsg?.shadowRoot?.querySelector('[data-testid="code-block-copy-btn"]')
      ).not.toBeNull();
      expect(
        slugMsg?.shadowRoot?.querySelector('[data-testid="code-block-execute-btn"]')
      ).toBeNull();
      expect(
        localMsg?.shadowRoot?.querySelector('[data-testid="code-block-copy-btn"]')
      ).not.toBeNull();
      expect(
        localMsg?.shadowRoot?.querySelector('[data-testid="code-block-execute-btn"]')
      ).toBeNull();
    });
  });
});
