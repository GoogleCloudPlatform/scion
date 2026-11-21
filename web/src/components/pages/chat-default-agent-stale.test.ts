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
 * Regression tests for ptone/scion#8642:
 * Setting or clearing a thread's default agent must not revert to the previous
 * value when switching to another thread and straight back.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import type { ScionChatSpaceRail } from '../shared/chat/chat-space-rail.js';
import type { ScionChatThread } from '../shared/chat/chat-thread.js';

vi.mock('../../client/main.js', () => import('../../client/__fixtures__/main-stub.js'));

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

interface ThreadEntry {
  id: string;
  name: string;
  isGeneral: boolean;
  defaultAgent: string;
  hasUnread: boolean;
  muted: boolean;
  pinned: boolean;
}

interface RailInternals extends ScionChatSpaceRail {
  spaces: Array<{ projectId: string; projectName: string; projectSlug: string }>;
  threadsBySpace: Map<string, ThreadEntry[]>;
  handleThreadClick(thread: ThreadEntry, projectId: string): void;
}

interface ChatInternals extends HTMLElement {
  v2Conversation: {
    conversationKey: string;
    projectId: string;
    projectSlug: string;
    threadName: string;
    defaultAgent: string;
    isDM: boolean;
  } | null;
  handleThreadSelect(e: CustomEvent): void;
  handleDefaultAgentChanged(e: CustomEvent): void;
  handleChatTopic(e: Event): void;
  fetchThreadDetails(conversationKey: string): Promise<void>;
}

interface ThreadInternals extends ScionChatThread {
  handleDefaultAgentChange(e: CustomEvent<{ defaultAgent: string }>): Promise<void>;
  handleDefaultCommand(text: string): Promise<void>;
  handleContextMenuSetDefault(): Promise<void>;
  contextMenuMessage: { sender: string; senderId: string } | null;
}

beforeAll(async () => {
  await Promise.all([
    import('./chat.js'),
    import('../shared/chat/chat-space-rail.js'),
    import('../shared/chat/chat-thread.js'),
  ]);
});

beforeEach(() => {
  vi.mocked(apiFetch).mockReset();
  vi.mocked(apiFetch).mockResolvedValue(new Response('{}', { status: 200 }));
});

afterEach(() => {
  document.body.innerHTML = '';
});

function setupPageWithRail(): {
  page: ChatInternals;
  rail: RailInternals;
  clickThread: (id: string) => void;
} {
  const page = document.createElement('scion-page-chat') as ChatInternals;
  const rail = document.createElement('scion-chat-space-rail') as RailInternals;
  rail.spaces = [{ projectId: 'proj-1', projectName: 'Project One', projectSlug: 'proj-1' }];
  rail.threadsBySpace = new Map([
    [
      'proj-1',
      [
        {
          id: 'topic-a',
          name: 'alpha',
          isGeneral: false,
          defaultAgent: 'agent-old',
          hasUnread: false,
          muted: false,
          pinned: false,
        },
        {
          id: 'topic-b',
          name: 'beta',
          isGeneral: false,
          defaultAgent: '',
          hasUnread: false,
          muted: false,
          pinned: false,
        },
      ],
    ],
  ]);

  Object.defineProperty(page, 'shadowRoot', {
    configurable: true,
    value: {
      querySelector: (sel: string) => (sel === 'scion-chat-space-rail' ? rail : null),
    },
  });

  rail.addEventListener('thread-select', (e) => {
    page.handleThreadSelect(e as CustomEvent);
  });

  const clickThread = (id: string): void => {
    const thread = rail.threadsBySpace.get('proj-1')?.find((t) => t.id === id);
    if (!thread) throw new Error(`missing thread ${id}`);
    rail.handleThreadClick(thread, 'proj-1');
  };

  return { page, rail, clickThread };
}

describe('chat thread default agent stays fresh across thread switches (ptone/scion#8642)', () => {
  it('updates the cached rail thread on default-agent-changed so switching away and back shows the new value', () => {
    const { page, clickThread } = setupPageWithRail();

    clickThread('topic-a');
    expect(page.v2Conversation?.defaultAgent).toBe('agent-old');

    page.handleDefaultAgentChanged(
      new CustomEvent('default-agent-changed', { detail: { defaultAgent: 'agent-new' } })
    );
    expect(page.v2Conversation?.defaultAgent).toBe('agent-new');

    // Switch to another thread and straight back.
    clickThread('topic-b');
    expect(page.v2Conversation?.conversationKey).toBe('topic-b');

    clickThread('topic-a');
    expect(page.v2Conversation?.defaultAgent).toBe('agent-new');
  });

  it('clears the cached rail thread when default-agent-changed sets an empty agent', () => {
    const { page, clickThread } = setupPageWithRail();

    clickThread('topic-a');
    expect(page.v2Conversation?.defaultAgent).toBe('agent-old');

    page.handleDefaultAgentChanged(
      new CustomEvent('default-agent-changed', { detail: { defaultAgent: '' } })
    );
    expect(page.v2Conversation?.defaultAgent).toBe('');

    clickThread('topic-b');
    clickThread('topic-a');
    expect(page.v2Conversation?.defaultAgent).toBe('');
  });

  it('updates the cached rail thread on a chat.topic SSE event for the currently open thread', () => {
    const { page, clickThread } = setupPageWithRail();

    clickThread('topic-a');
    expect(page.v2Conversation?.defaultAgent).toBe('agent-old');

    page.handleChatTopic(
      new CustomEvent('chat-topic', {
        detail: {
          data: {
            action: 'updated',
            topic: { id: 'topic-a', projectId: 'proj-1', defaultAgent: 'agent-sse' },
          },
        },
      })
    );
    expect(page.v2Conversation?.defaultAgent).toBe('agent-sse');

    clickThread('topic-b');
    clickThread('topic-a');
    expect(page.v2Conversation?.defaultAgent).toBe('agent-sse');
  });

  it('fetches topic details with cache: no-store and reflects a cleared defaultAgent', async () => {
    const { page, clickThread } = setupPageWithRail();
    clickThread('topic-a');
    expect(page.v2Conversation?.defaultAgent).toBe('agent-old');

    vi.mocked(apiFetch).mockResolvedValueOnce(
      new Response(JSON.stringify({ id: 'topic-a', name: 'alpha' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    await page.fetchThreadDetails('topic-a');

    expect(apiFetch).toHaveBeenCalledWith(
      '/api/v1/chat/topics/topic-a',
      expect.objectContaining({ cache: 'no-store' })
    );
    expect(page.v2Conversation?.defaultAgent).toBe('');
  });
});

describe('scion-chat-thread does not mutate its parent-owned defaultAgent property', () => {
  it('dispatches default-agent-changed from composer dropdown without mutating this.defaultAgent', async () => {
    const el = document.createElement('scion-chat-thread') as ThreadInternals;
    el.conversationKey = 'topic-a';
    el.defaultAgent = 'agent-old';
    const events: string[] = [];
    el.addEventListener('default-agent-changed', (e) => {
      events.push((e as CustomEvent<{ defaultAgent: string }>).detail.defaultAgent);
    });

    await el.handleDefaultAgentChange(
      new CustomEvent('default-agent-change', { detail: { defaultAgent: 'agent-new' } })
    );

    expect(events).toEqual(['agent-new']);
    expect(el.defaultAgent).toBe('agent-old');
  });

  it('dispatches default-agent-changed from /default command without mutating this.defaultAgent', async () => {
    const el = document.createElement('scion-chat-thread') as ThreadInternals;
    el.conversationKey = 'topic-a';
    el.defaultAgent = 'agent-old';
    const events: string[] = [];
    el.addEventListener('default-agent-changed', (e) => {
      events.push((e as CustomEvent<{ defaultAgent: string }>).detail.defaultAgent);
    });

    await el.handleDefaultCommand('/default agent-slash');

    expect(events).toEqual(['agent-slash']);
    expect(el.defaultAgent).toBe('agent-old');
  });

  it('dispatches default-agent-changed from context menu without mutating this.defaultAgent', async () => {
    const el = document.createElement('scion-chat-thread') as ThreadInternals;
    el.conversationKey = 'topic-a';
    el.defaultAgent = 'agent-old';
    el.contextMenuMessage = { sender: 'agent:agent-ctx', senderId: 'id-ctx' };
    const events: string[] = [];
    el.addEventListener('default-agent-changed', (e) => {
      events.push((e as CustomEvent<{ defaultAgent: string }>).detail.defaultAgent);
    });

    await el.handleContextMenuSetDefault();

    expect(events).toEqual(['agent-ctx']);
    expect(el.defaultAgent).toBe('agent-old');
  });
});
