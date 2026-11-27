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

import { describe, it, expect, beforeAll, afterEach, vi } from 'vitest';
import { render, type TemplateResult } from 'lit';

import type { Agent } from '../../shared/types.js';
import type { AgentConversationSummary, ScionPageAgentDetail } from './agent-detail.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

const apiFetchMock = vi.fn();
vi.mock('../../client/api.js', async () => {
  const actual = await vi.importActual<typeof import('../../client/api.js')>('../../client/api.js');
  return {
    ...actual,
    apiFetch: (...args: unknown[]) => apiFetchMock(...args),
  };
});

function makeAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: 'agent-1',
    name: 'worker-alpha',
    projectId: 'proj-1',
    template: 'default',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read', 'message'] },
    ...overrides,
  } as Agent;
}

function createDetail(agent: Agent, nativeChatEnabled = true): ScionPageAgentDetail {
  const el = document.createElement('scion-page-agent-detail') as ScionPageAgentDetail;
  el.agentId = agent.id;
  Object.assign(el, { loading: false, agent });
  Object.defineProperty(el, 'nativeChatEnabled', {
    get: () => nativeChatEnabled,
    configurable: true,
  });
  return el;
}

function renderPanel(el: ScionPageAgentDetail): HTMLDivElement {
  const host = document.createElement('div');
  const tpl = (
    el as unknown as { renderMessagesPanel: () => TemplateResult }
  ).renderMessagesPanel();
  render(tpl, host);
  return host;
}

describe('agent detail — Messages tab shared conversations (ptone/scion#9544)', () => {
  beforeAll(async () => {
    await import('./agent-detail.js');
  }, 30_000);

  afterEach(() => {
    apiFetchMock.mockReset();
  });

  it('fetches shared conversations with agent_id when Messages tab is shown and renders links + non-link rows', async () => {
    const conversations: AgentConversationSummary[] = [
      {
        id: 'conv-dm',
        kind: 'direct',
        surface: 'native',
        externalRef: 'dm:agent:agent-1:user:user-1',
        dmPeer: { kind: 'agent', id: 'agent-1', name: 'worker-alpha' },
        lastActivityAt: '2026-11-27T03:00:00Z',
      },
      {
        id: 'conv-group',
        projectId: 'proj-1',
        kind: 'group',
        surface: 'native',
        externalRef: 'thread:proj-1:topic-77',
        threadName: 'Release Coordination',
        lastActivityAt: '2026-11-27T02:30:00Z',
      },
      {
        id: 'conv-external',
        projectId: 'proj-1',
        kind: 'group',
        surface: 'discord',
        displayName: 'Discord Ops',
        lastActivityAt: '2026-11-27T01:00:00Z',
      },
    ];

    apiFetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ conversations, total: 3, truncated: false }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const el = createDetail(makeAgent());
    (el as unknown as { handleTabShow: (e: CustomEvent<{ name: string }>) => void }).handleTabShow(
      new CustomEvent('sl-tab-show', { detail: { name: 'messages' } })
    );

    await vi.waitFor(() => {
      expect(apiFetchMock).toHaveBeenCalledWith('/api/v1/conversations?agent_id=agent-1');
    });
    await vi.waitFor(() => {
      expect(
        (el as unknown as { sharedConversationsLoaded: boolean }).sharedConversationsLoaded
      ).toBe(true);
    });

    const host = renderPanel(el);
    const rows = Array.from(host.querySelectorAll('[data-testid="shared-conversation-row"]'));
    expect(rows).toHaveLength(3);

    // 1. Direct message links to /chat/dm/...
    expect(rows[0].tagName).toBe('A');
    expect(rows[0].getAttribute('href')).toBe('/chat/dm/dm%3Aagent%3Aagent-1%3Auser%3Auser-1');
    expect(rows[0].textContent).toContain('worker-alpha');
    expect(rows[0].textContent).toContain('Direct');

    // 2. Native group parses topicId from thread:<projectId>:<topicId>
    expect(rows[1].tagName).toBe('A');
    expect(rows[1].getAttribute('href')).toBe('/chat/space/proj-1/thread/topic-77');
    expect(rows[1].textContent).toContain('Release Coordination');
    expect(rows[1].textContent).toContain('Group');

    // 3. External-surface group renders as non-link row with surface badge
    expect(rows[2].tagName).toBe('DIV');
    expect(rows[2].getAttribute('href')).toBeNull();
    expect(rows[2].textContent).toContain('Discord Ops');
    expect(rows[2].textContent).toContain('discord');
  });

  it('defaults to Conversations and toggles cleanly between Conversations and Log', async () => {
    apiFetchMock.mockResolvedValue(
      new Response(JSON.stringify({ conversations: [], total: 0, truncated: false }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    const el = createDetail(makeAgent());
    let host = renderPanel(el);

    const toggle = host.querySelector('sl-radio-group.messages-view-toggle');
    expect(toggle?.getAttribute('value')).toBe('conversations');
    expect(
      (host.querySelector('[data-testid="shared-conversations-panel"]') as HTMLElement).style
        .display
    ).toBe('');
    expect((host.querySelector('scion-agent-message-viewer') as HTMLElement).style.display).toBe(
      'none'
    );

    // Toggle to Log
    (
      el as unknown as { handleMessageViewToggle: (mode: 'conversations' | 'log') => void }
    ).handleMessageViewToggle('log');
    host = renderPanel(el);
    expect(host.querySelector('sl-radio-group.messages-view-toggle')?.getAttribute('value')).toBe(
      'log'
    );
    expect(
      (host.querySelector('[data-testid="shared-conversations-panel"]') as HTMLElement).style
        .display
    ).toBe('none');
    expect((host.querySelector('scion-agent-message-viewer') as HTMLElement).style.display).toBe(
      ''
    );

    // Toggle back to Conversations triggers fetch
    (
      el as unknown as { handleMessageViewToggle: (mode: 'conversations' | 'log') => void }
    ).handleMessageViewToggle('conversations');
    await vi.waitFor(() => {
      expect(apiFetchMock).toHaveBeenCalledWith('/api/v1/conversations?agent_id=agent-1');
    });
  });

  it('shows empty state and error state with retry', async () => {
    const el = createDetail(makeAgent());
    let host = renderPanel(el);
    expect(host.querySelector('[data-testid="shared-conversations-empty"]')).not.toBeNull();

    apiFetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ error: { message: 'backend unavailable' } }), {
        status: 500,
        headers: { 'Content-Type': 'application/json' },
      })
    );
    await (
      el as unknown as { loadSharedConversations: () => Promise<void> }
    ).loadSharedConversations();

    host = renderPanel(el);
    const errEl = host.querySelector('[data-testid="shared-conversations-error"]');
    expect(errEl).not.toBeNull();
    expect(errEl?.textContent).toContain('backend unavailable');
    expect(host.querySelector('[data-testid="shared-conversations-retry"]')).not.toBeNull();
  });

  it('preserves messaging denial and sealed banners above the toggle', () => {
    const denied = createDetail(
      makeAgent({
        messageMode: 'owner',
        _messageability: { canMessage: false, reason: 'not_owner' },
      })
    );
    const deniedHost = renderPanel(denied);
    expect(deniedHost.querySelector('.alert-warning')).not.toBeNull();
    expect(deniedHost.querySelector('[data-testid="shared-conversations-panel"]')).not.toBeNull();

    const sealed = createDetail(makeAgent({ messageMode: 'none' }));
    const sealedHost = renderPanel(sealed);
    expect(sealedHost.querySelector('.alert-danger')).not.toBeNull();
    expect(sealedHost.querySelector('[data-testid="shared-conversations-panel"]')).not.toBeNull();
  });

  it('keeps message viewer only when nativeChatEnabled is false', () => {
    const el = createDetail(makeAgent(), false);
    const host = renderPanel(el);
    expect(host.querySelector('sl-radio-group.messages-view-toggle')).toBeNull();
    expect(host.querySelector('[data-testid="shared-conversations-panel"]')).toBeNull();
    expect(host.querySelector('scion-agent-message-viewer')).not.toBeNull();
  });
});
