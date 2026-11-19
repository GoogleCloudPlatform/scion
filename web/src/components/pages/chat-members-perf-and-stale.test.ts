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
 * Regression tests for ptone/scion#8270:
 *  - `?include=humans` on global `/chat` presence refresh and 4 of 5 space
 *    fallback poll ticks, with a full `loadV2Members` every 5th tick (5 min).
 *  - Stale deleted-agent fixes:
 *    1. In-flight `loadV2Members` racing SSE `agent.deleted` filters out
 *       `stateManager.getDeletedAgentIds()`.
 *    2. Space-view `_handleAgentsUpdated` prunes deleted agent IDs from
 *       `v2Members` (`@mention` roster) without churning `v2Members` identity
 *       on status-only ticks.
 *    3. Viewing a DM with `_sidebarOwner === 'space'` continues refreshing
 *       `_sidebarSpaceId` on the fallback poll and on reconnect/resume.
 *    4. SSE reconnect (`agents-resync`), tab resume (`ResumeRefresh`), and
 *       `rail-refresh` reload `loadV2Members` even when `parseV2Route`
 *       short-circuits on an already-open conversation.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import { apiFetch } from '../../client/api.js';
import { chatDMsLoad, chatSpacesLoad } from '../../client/chat-list-cache.js';
import { RESUME_REFRESH_AFTER_MS } from '../../client/resume-refresh.js';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../client/main.js', async () => {
  const state = await import('../../client/state.js');
  return {
    ...(await import('../../client/__fixtures__/main-stub.js')),
    stateManager: state.stateManager,
  };
});

vi.mock('../../client/api.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../client/api.js')>();
  return {
    ...actual,
    apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
  };
});

import { stateManager } from '../../client/state.js';

class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

function handleUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
}

const POLL_MS = 60_000;

beforeAll(async () => {
  await import('./chat.js');
  await Promise.all([
    import('../shared/chat/chat-space-rail.js'),
    import('../shared/chat/chat-members.js'),
  ]);
});

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
  stateManager.setScope({ type: 'brokers-list' });
  stateManager.setScope({ type: 'chat', spaceIds: ['p1'], userId: 'user-me' });
  chatDMsLoad.invalidate();
  chatSpacesLoad.invalidate();
  vi.mocked(apiFetch).mockReset();
  vi.mocked(apiFetch).mockImplementation(() =>
    Promise.resolve(new Response('{}', { status: 200 }))
  );
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

async function mountStartedPage(path = '/chat'): Promise<any> {
  const el = document.createElement('scion-page-chat') as any;
  el.pageData = { user: { id: 'user-me' } };
  el.shouldUpdate = (): boolean => false;
  const startups: Promise<void>[] = [];
  const origInit = el.initV2;
  el.initV2 = function (this: unknown): Promise<void> {
    const p = origInit.call(this);
    startups.push(p);
    return p;
  };
  window.history.replaceState({}, '', path);
  document.body.appendChild(el);
  await Promise.all(startups);
  return el;
}

function callUrls(): string[] {
  return vi.mocked(apiFetch).mock.calls.map(([u]) => String(u));
}

describe('space members ?include=humans polling and 5-minute full backstop', () => {
  it('refreshHubMemberPresence requests ?include=humans', async () => {
    const page = document.createElement('scion-page-chat') as any;
    page.v2HumanMembers = [{ id: 'u1', kind: 'user', displayName: 'Ada', presenceState: '' }];
    vi.mocked(apiFetch).mockImplementationOnce(() =>
      Promise.resolve(
        new Response(JSON.stringify({ humans: [{ id: 'u1', presenceState: 'active' }] }), {
          status: 200,
        })
      )
    );

    await page.refreshHubMemberPresence('proj-1');

    expect(callUrls()).toEqual(['/api/v1/chat/spaces/proj-1/members?include=humans']);
    expect(page.v2HumanMembers[0].presenceState).toBe('active');
  });

  it('uses ?include=humans on 4 of 5 space poll ticks and full /members on every 5th tick', async () => {
    vi.useFakeTimers({
      toFake: ['setInterval', 'clearInterval', 'setTimeout', 'clearTimeout', 'Date'],
    });
    let serverAgents = [
      { id: 'a1', displayName: 'Agent One', slug: 'agent-1', phase: 'running', canAttach: true },
      { id: 'a2', displayName: 'Agent Two', slug: 'agent-2', phase: 'running', canAttach: false },
    ];
    let serverHumans = [{ id: 'h1', displayName: 'Human One', presenceState: 'idle' }];

    vi.mocked(apiFetch).mockImplementation((url) => {
      const s = String(url);
      if (s === '/api/v1/chat/spaces/p1/members?include=humans') {
        return Promise.resolve(
          new Response(JSON.stringify({ humans: serverHumans }), { status: 200 })
        );
      }
      if (s === '/api/v1/chat/spaces/p1/members') {
        return Promise.resolve(
          new Response(JSON.stringify({ humans: serverHumans, agents: serverAgents }), {
            status: 200,
          })
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = await mountStartedPage('/chat/p1/t1');
    page.v2Conversation = {
      conversationKey: 't1',
      projectId: 'p1',
      projectSlug: 'p1',
      threadName: 'general',
      defaultAgent: '',
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    await page.loadV2Members('p1');
    vi.mocked(apiFetch).mockClear();

    // Update server humans + remove a2 silently (missed SSE delete).
    serverHumans = [
      { id: 'h1', displayName: 'Human One', presenceState: 'active' },
      { id: 'h2', displayName: 'Human Two', presenceState: 'idle' },
    ];
    serverAgents = [
      { id: 'a1', displayName: 'Agent One', slug: 'agent-1', phase: 'running', canAttach: true },
    ];

    // Ticks 1..4: humans-only, agents preserved (including canAttach), human roster updated.
    for (let tick = 1; tick <= 4; tick++) {
      await vi.advanceTimersByTimeAsync(POLL_MS);
    }
    const firstFour = callUrls().filter((u) => u.includes('/members'));
    expect(firstFour).toEqual([
      '/api/v1/chat/spaces/p1/members?include=humans',
      '/api/v1/chat/spaces/p1/members?include=humans',
      '/api/v1/chat/spaces/p1/members?include=humans',
      '/api/v1/chat/spaces/p1/members?include=humans',
    ]);
    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['h1', 'h2']);
    expect(page.v2HumanMembers[0].presenceState).toBe('active');
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a1', 'a2']);
    expect(page.v2AgentMembers[0].canAttach).toBe(true);
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['h1', 'h2', 'a1', 'a2']);

    // Tick 5 (5 min): full loadV2Members runs and reconciles missed delete of a2.
    vi.mocked(apiFetch).mockClear();
    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(callUrls().filter((u) => u.includes('/members'))).toEqual([
      '/api/v1/chat/spaces/p1/members',
    ]);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a1']);
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['h1', 'h2', 'a1']);

    // Tick 6: resets back to ?include=humans.
    vi.mocked(apiFetch).mockClear();
    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(callUrls().filter((u) => u.includes('/members'))).toEqual([
      '/api/v1/chat/spaces/p1/members?include=humans',
    ]);

    page.remove();
  });

  it('drops a stale refreshSpaceHumans response when another view claims the sidebar', async () => {
    const page = document.createElement('scion-page-chat') as any;
    let resolveHumans!: (res: Response) => void;
    vi.mocked(apiFetch)
      .mockImplementationOnce(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              humans: [{ id: 'h-p1', displayName: 'P1 Human' }],
              agents: [{ id: 'a-p1', displayName: 'P1 Agent' }],
            }),
            { status: 200 }
          )
        )
      )
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveHumans = resolve;
          })
      )
      .mockImplementationOnce(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              humans: [{ id: 'h-p2', displayName: 'P2 Human' }],
              agents: [{ id: 'a-p2', displayName: 'P2 Agent' }],
            }),
            { status: 200 }
          )
        )
      );

    await page.loadV2Members('p1');
    const humansRefresh = page.refreshSpaceHumans('p1');
    await page.loadV2Members('p2');

    resolveHumans(
      new Response(JSON.stringify({ humans: [{ id: 'stale-h', displayName: 'Stale' }] }), {
        status: 200,
      })
    );
    await humansRefresh;

    expect(page.v2HumanMembers.map((h: any) => h.id)).toEqual(['h-p2']);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a-p2']);
  });
});

describe('stale deleted-agent fixes', () => {
  it('filters tombstoned agents out of an in-flight loadV2Members response racing SSE deleted', async () => {
    let resolveMembers!: (res: Response) => void;
    vi.mocked(apiFetch).mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          resolveMembers = resolve;
        })
    );

    const page = document.createElement('scion-page-chat') as any;
    page.v2Conversation = {
      conversationKey: 't1',
      projectId: 'p1',
      projectSlug: 'p1',
      threadName: 'general',
      defaultAgent: 'gone-slug',
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };

    const inFlight = page.loadV2Members('p1');

    // SSE delivers agent.deleted while the older REST snapshot is in flight.
    handleUpdate('project.p1.agent.deleted', { agentId: 'a-gone', projectId: 'p1' });
    expect(stateManager.getDeletedAgentIds().has('a-gone')).toBe(true);

    resolveMembers(
      new Response(
        JSON.stringify({
          humans: [{ id: 'h1', displayName: 'Ada' }],
          agents: [
            { id: 'a-gone', displayName: 'Gone Agent', slug: 'gone-slug', canAttach: true },
            { id: 'a-live', displayName: 'Live Agent', slug: 'live-slug', canAttach: false },
          ],
        }),
        { status: 200 }
      )
    );
    await inFlight;

    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a-live']);
    expect(page.v2AgentMembers[0].projectId).toBe('p1');
    expect(page.v2AgentMembers[0].lastSeen).toBe('');
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['h1', 'a-live']);
    expect(page.v2Conversation.defaultAgent).toBe('');
  });

  it('prunes SSE-deleted agents from v2Members (@mention roster) without churning status-only ticks', async () => {
    vi.mocked(apiFetch).mockImplementationOnce(() =>
      Promise.resolve(
        new Response(
          JSON.stringify({
            humans: [{ id: 'h1', displayName: 'Ada' }],
            agents: [
              {
                id: 'a1',
                displayName: 'Agent One',
                slug: 'agent-1',
                phase: 'running',
                canAttach: true,
              },
              {
                id: 'a2',
                displayName: 'Agent Two',
                slug: 'agent-2',
                phase: 'running',
                canAttach: false,
              },
            ],
          }),
          { status: 200 }
        )
      )
    );

    const page = document.createElement('scion-page-chat') as any;
    page.v2Conversation = {
      conversationKey: 't1',
      projectId: 'p1',
      projectSlug: 'p1',
      threadName: 'general',
      defaultAgent: 'agent-2',
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    await page.loadV2Members('p1');
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['h1', 'a1', 'a2']);

    // Status-only SSE update keeps v2Members array reference stable.
    const rosterBeforeStatus = page.v2Members;
    handleUpdate('project.p1.agent.status', {
      agentId: 'a1',
      projectId: 'p1',
      phase: 'running',
      activity: 'thinking',
    });
    page._handleAgentsUpdated();
    expect(page.v2Members).toBe(rosterBeforeStatus);

    // SSE delete prunes a2 from both v2AgentMembers and v2Members immediately.
    handleUpdate('project.p1.agent.deleted', { agentId: 'a2', projectId: 'p1' });
    page._handleAgentsUpdated();
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a1']);
    expect(page.v2Members.map((m: any) => m.id)).toEqual(['h1', 'a1']);
    expect(page.v2Conversation.defaultAgent).toBe('');
  });

  it('refreshes space members while viewing a DM with _sidebarOwner === "space", but skips hub DMs', async () => {
    vi.useFakeTimers({
      toFake: ['setInterval', 'clearInterval', 'setTimeout', 'clearTimeout', 'Date'],
    });
    let serverAgents = [
      { id: 'a1', displayName: 'Agent One', slug: 'agent-1', canAttach: true },
      { id: 'a2', displayName: 'Agent Two', slug: 'agent-2', canAttach: true },
    ];
    vi.mocked(apiFetch).mockImplementation((url) => {
      const s = String(url);
      if (s === '/api/v1/chat/spaces/p1/members?include=humans') {
        return Promise.resolve(
          new Response(JSON.stringify({ humans: [{ id: 'h1', displayName: 'Ada' }] }), {
            status: 200,
          })
        );
      }
      if (s === '/api/v1/chat/spaces/p1/members') {
        return Promise.resolve(
          new Response(
            JSON.stringify({ humans: [{ id: 'h1', displayName: 'Ada' }], agents: serverAgents }),
            { status: 200 }
          )
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = await mountStartedPage('/chat/p1/t1');
    await page.loadV2Members('p1');
    // User switches to a DM while _sidebarOwner === 'space' (p1).
    page.v2Conversation = {
      conversationKey: 'dm:agent:a1:user:user-me',
      projectId: 'p1',
      projectSlug: '',
      threadName: '',
      defaultAgent: '',
      isDM: true,
      peerName: 'Agent One',
      peerId: 'a1',
      peerKind: 'agent',
    };
    // Missed SSE delete of a2 while viewing the DM.
    serverAgents = [{ id: 'a1', displayName: 'Agent One', slug: 'agent-1', canAttach: true }];
    vi.mocked(apiFetch).mockClear();

    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(POLL_MS);
    }
    expect(callUrls().filter((u) => u.includes('/members'))).toEqual([
      '/api/v1/chat/spaces/p1/members?include=humans',
      '/api/v1/chat/spaces/p1/members?include=humans',
      '/api/v1/chat/spaces/p1/members?include=humans',
      '/api/v1/chat/spaces/p1/members?include=humans',
    ]);

    // 5th tick runs full loadV2Members('p1') even while in the DM and drops a2!
    vi.mocked(apiFetch).mockClear();
    await vi.advanceTimersByTimeAsync(POLL_MS);
    expect(callUrls().filter((u) => u.includes('/members'))).toEqual([
      '/api/v1/chat/spaces/p1/members',
    ]);
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a1']);

    // Contrast: a DM opened from global /chat (_sidebarOwner === 'hub') makes 0 /members calls.
    page.v2Conversation = null;
    page.loadHubMembers();
    page.v2Conversation = {
      conversationKey: 'dm:user:u2:user:user-me',
      projectId: '',
      projectSlug: '',
      threadName: '',
      defaultAgent: '',
      isDM: true,
      peerName: 'Bob',
      peerId: 'u2',
      peerKind: 'user',
    };
    vi.mocked(apiFetch).mockClear();
    await vi.advanceTimersByTimeAsync(POLL_MS * 5);
    expect(callUrls().filter((u) => u.includes('/members'))).toEqual([]);

    page.remove();
  });

  it('reloads loadV2Members on SSE reconnect (agents-resync), tab resume, and rail-refresh', async () => {
    let visibility: DocumentVisibilityState = 'visible';
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility);
    let now = 1_000_000;
    vi.spyOn(Date, 'now').mockImplementation(() => now);

    let serverAgents = [
      { id: 'a1', displayName: 'Agent One', slug: 'agent-1', canAttach: true },
      { id: 'a2', displayName: 'Agent Two', slug: 'agent-2', canAttach: true },
    ];
    vi.mocked(apiFetch).mockImplementation((url) => {
      if (String(url) === '/api/v1/chat/spaces/p1/members') {
        return Promise.resolve(
          new Response(JSON.stringify({ humans: [], agents: serverAgents }), { status: 200 })
        );
      }
      return Promise.resolve(new Response('{}', { status: 200 }));
    });

    const page = await mountStartedPage('/chat/p1/t1');
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    page.v2Conversation = {
      conversationKey: 't1',
      projectId: 'p1',
      projectSlug: 'p1',
      threadName: 'general',
      defaultAgent: '',
      isDM: false,
      peerName: '',
      peerId: '',
      peerKind: 'user',
    };
    await page.loadV2Members('p1');
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a1', 'a2']);

    // 1) SSE reconnect (`agents-resync`) while viewing space thread reloads full members.
    serverAgents = [{ id: 'a1', displayName: 'Agent One', slug: 'agent-1', canAttach: true }];
    vi.mocked(apiFetch).mockClear();
    stateManager.dispatchEvent(new CustomEvent('agents-resync'));
    await vi.advanceTimersByTimeAsync(250);
    await vi.waitFor(() =>
      expect(callUrls().filter((u) => u === '/api/v1/chat/spaces/p1/members')).toHaveLength(1)
    );
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a1']);

    // 2) Tab resume (`ResumeRefresh`) while viewing a DM with _sidebarOwner === 'space' reloads p1.
    page.v2Conversation = {
      conversationKey: 'dm:agent:a1:user:user-me',
      projectId: 'p1',
      projectSlug: '',
      threadName: '',
      defaultAgent: '',
      isDM: true,
      peerName: 'Agent One',
      peerId: 'a1',
      peerKind: 'agent',
    };
    serverAgents = [];
    vi.mocked(apiFetch).mockClear();
    visibility = 'hidden';
    document.dispatchEvent(new Event('visibilitychange'));
    now += RESUME_REFRESH_AFTER_MS;
    visibility = 'visible';
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.advanceTimersByTimeAsync(250);
    await vi.waitFor(() =>
      expect(callUrls().filter((u) => u === '/api/v1/chat/spaces/p1/members')).toHaveLength(1)
    );
    expect(page.v2AgentMembers).toEqual([]);

    // 3) Direct `rail-refresh` event (when rail component is mounted) also reloads p1 once.
    serverAgents = [{ id: 'a3', displayName: 'Agent Three', slug: 'agent-3', canAttach: false }];
    vi.mocked(apiFetch).mockClear();
    const waited: Promise<unknown>[] = [];
    page.dispatchEvent(
      new CustomEvent('rail-refresh', {
        detail: { waitUntil: (p: Promise<unknown>) => waited.push(p) },
      })
    );
    await Promise.all(waited);
    await vi.waitFor(() =>
      expect(callUrls().filter((u) => u === '/api/v1/chat/spaces/p1/members')).toHaveLength(1)
    );
    expect(page.v2AgentMembers.map((a: any) => a.id)).toEqual(['a3']);

    page.remove();
  });
});
