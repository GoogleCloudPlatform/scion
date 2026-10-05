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
 * End-to-end coverage for the `/agents` page's `agents-changed` consumption
 * via `mergeChanged` (design §7, §11): this replaces the old per-event
 * `onAgentsUpdated` full rebuild. Exercises the real `stateManager`
 * singleton (SSE deltas go through its actual coalescing pipeline), with
 * only `fetch` and `localStorage` faked.
 */

// @vitest-environment happy-dom

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
// Auto-confirm the force-delete prompt (the first confirm is skipped with
// altKey); nothing else in this file opens a dialog.
vi.mock('../shared/confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

import './agents.js';
import type { ScionPageAgents } from './agents.js';
import { stateManager } from '../../client/state.js';
import type { Agent } from '../../shared/types.js';

/** happy-dom has no EventSource; setScope opens one. */
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

beforeAll(() => {
  const store = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
});

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function agent(id: string, overrides: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read'] },
    ...overrides,
  } as Agent;
}

function handleUpdate(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
}

async function flush(): Promise<void> {
  // `state.ts` coalesces into one flush per rAF, or after 100ms with no
  // frame (happy-dom has no rAF loop driving tests), so 150ms is enough
  // to clear the timeout fallback deterministically.
  await new Promise((r) => setTimeout(r, 150));
}

type TestEl = ScionPageAgents & { agents: Agent[] };

describe('scion-page-agents live updates (agents-changed -> mergeChanged)', () => {
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    // `stateManager` is a process-wide singleton and the page always opens
    // the same `{type: 'dashboard'}` scope, which `setScope` treats as a
    // no-op when the scope is already dashboard (no real change, no
    // clear) — without forcing an actual scope change first, agents and
    // capabilities seeded by an earlier test in this file would leak into
    // the next one's "hydrated data" reuse check (agents.ts:462).
    stateManager.setScope({ type: 'brokers-list' });
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-agents').forEach((n) => n.remove());
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it('merges a status delta in place, preserving identity for every untouched agent, and adds an SSE-created agent under scope all with no extra request', async () => {
    const requests: string[] = [];
    const initial = [agent('a1'), agent('a2'), agent('a3')];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        requests.push(typeof input === 'string' ? input : input.toString());
        return Promise.resolve(jsonResponse({ agents: initial, _capabilities: { actions: [] } }));
      })
    );

    const el = document.createElement('scion-page-agents') as TestEl;
    document.body.appendChild(el);
    await el.updateComplete;
    await flush();
    await el.updateComplete;

    expect(requests.length).toBe(1);
    const before = el.agents;
    const a1Before = before.find((a) => a.id === 'a1');
    const a2Before = before.find((a) => a.id === 'a2');
    expect(a1Before?.phase).toBe('running');

    handleUpdate('agent.a1.status', { agentId: 'a1', phase: 'stopped' });
    await flush();
    await el.updateComplete;

    // Updated row reflects the delta; the array identity changed...
    expect(el.agents).not.toBe(before);
    const a1After = el.agents.find((a) => a.id === 'a1');
    expect(a1After?.phase).toBe('stopped');
    // ...but every untouched agent is carried through by reference.
    expect(el.agents.find((a) => a.id === 'a2')).toBe(a2Before);
    expect(el.agents.find((a) => a.id === 'a3')).toBe(before.find((a) => a.id === 'a3'));
    expect(requests.length).toBe(1); // no request for a live update

    // An SSE-created agent is added under scope `all` (today's add rule).
    handleUpdate('agent.a4.created', {
      agentId: 'a4',
      id: 'a4',
      name: 'a4',
      projectId: 'p1',
      template: 't',
      phase: 'running',
      created: '2026-01-02T00:00:00Z',
      updated: '2026-01-02T00:00:00Z',
      messageMode: 'project',
    });
    await flush();
    await el.updateComplete;

    expect(el.agents.some((a) => a.id === 'a4')).toBe(true);
    expect(requests.length).toBe(1); // still no request
  });

  it('an SSE-created agent keeps its inherited capabilities across its next status delta', async () => {
    const initial = [agent('a1')];
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(jsonResponse({ agents: initial, _capabilities: { actions: ['stop'] } }))
      )
    );

    const el = document.createElement('scion-page-agents') as TestEl;
    document.body.appendChild(el);
    await el.updateComplete;
    await flush();
    await el.updateComplete;

    // An SSE-created agent, carrying no `_capabilities` of its own, as a
    // real create event does.
    handleUpdate('agent.a4.created', {
      agentId: 'a4',
      id: 'a4',
      name: 'a4',
      projectId: 'p1',
      template: 't',
      phase: 'running',
      created: '2026-01-02T00:00:00Z',
      updated: '2026-01-02T00:00:00Z',
      messageMode: 'project',
    });
    await flush();
    await el.updateComplete;

    const afterCreate = el.agents.find((a) => a.id === 'a4');
    expect(afterCreate?._capabilities).toBeTruthy();

    // Its next delta carries no `_capabilities` either.
    handleUpdate('agent.a4.status', { agentId: 'a4', phase: 'stopped' });
    await flush();
    await el.updateComplete;

    const afterStatus = el.agents.find((a) => a.id === 'a4');
    expect(afterStatus?.phase).toBe('stopped');
    expect(afterStatus?._capabilities).toBe(afterCreate?._capabilities);
  });

  it('removes an agent on an SSE delete', async () => {
    const initial = [agent('a1'), agent('a2')];
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(jsonResponse({ agents: initial, _capabilities: { actions: [] } }))
      )
    );

    const el = document.createElement('scion-page-agents') as TestEl;
    document.body.appendChild(el);
    await el.updateComplete;
    await flush();
    await el.updateComplete;

    expect(el.agents.map((a) => a.id).sort()).toEqual(['a1', 'a2']);

    handleUpdate('agent.a1.deleted', {});
    await flush();
    await el.updateComplete;

    expect(el.agents.map((a) => a.id)).toEqual(['a2']);
  });

  it('a REST response landing after an SSE delete does not resurrect the deleted agent', async () => {
    const initial = [agent('a1'), agent('a2')];
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(jsonResponse({ agents: initial, _capabilities: { actions: [] } }))
      )
    );

    const el = document.createElement('scion-page-agents') as TestEl;
    document.body.appendChild(el);
    await el.updateComplete;
    await flush();
    await el.updateComplete;

    expect(el.agents.map((a) => a.id).sort()).toEqual(['a1', 'a2']);

    // The hub tells this client 'a1' is gone.
    handleUpdate('agent.a1.deleted', {});
    await flush();
    await el.updateComplete;
    expect(el.agents.some((a) => a.id === 'a1')).toBe(false);

    // A background refresh re-fetches, and the fixture's fetch handler
    // still returns the original fixture list — 'a1' included — because it
    // has no knowledge of the delete (the same shape as a REST response
    // that was already in flight, or served from a stale read replica,
    // when the delete happened). The already-tombstoned ID must not
    // reappear.
    (el as unknown as { backgroundRefresh(): void }).backgroundRefresh();
    await flush();
    await el.updateComplete;

    expect(el.agents.some((a) => a.id === 'a1')).toBe(false);
  });

  it("a scoped (mine) load does not adopt a brand-new SSE-created agent, but still updates one it already holds (today's add rule)", async () => {
    const initial = [agent('a1', { projectId: 'p1' })];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: string | URL | Request) => {
        const url = typeof input === 'string' ? input : input.toString();
        if (url.includes('scope=mine')) {
          return Promise.resolve(jsonResponse({ agents: initial, _capabilities: { actions: [] } }));
        }
        return Promise.resolve(jsonResponse({ agents: [], _capabilities: { actions: [] } }));
      })
    );
    localStorage.setItem('scion-scope-agents', 'mine');

    const el = document.createElement('scion-page-agents') as TestEl;
    el.pageData = {
      path: '/agents',
      title: 'Agents',
      user: { id: 'u', email: 'u@example.com', name: 'U', role: 'member' },
    };
    document.body.appendChild(el);
    await el.updateComplete;
    await flush();
    await el.updateComplete;

    expect(el.agents.map((a) => a.id)).toEqual(['a1']);

    // A brand-new agent created elsewhere must not appear under a scope filter.
    handleUpdate('agent.a2.created', {
      agentId: 'a2',
      id: 'a2',
      name: 'a2',
      projectId: 'p2',
      template: 't',
      phase: 'running',
      created: '2026-01-02T00:00:00Z',
      updated: '2026-01-02T00:00:00Z',
      messageMode: 'project',
    });
    await flush();
    await el.updateComplete;
    expect(el.agents.map((a) => a.id)).toEqual(['a1']); // not added

    // An already-held agent still gets its updates regardless of scope.
    handleUpdate('agent.a1.status', { agentId: 'a1', phase: 'stopped' });
    await flush();
    await el.updateComplete;
    expect(el.agents.find((a) => a.id === 'a1')?.phase).toBe('stopped');
  });

  describe('backend-driven delete (ptone/scion#2483 phase 1b)', () => {
    const deletionView = {
      state: 'deleting',
      soft: false,
      claim: 1,
      startedAt: new Date().toISOString(),
      leaseExpiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
    };

    /** List GET answers from `rows`; DELETE answers with `deleteResponse()`. */
    function stubHub(rows: Agent[], deleteResponse: () => Response): void {
      vi.stubGlobal(
        'fetch',
        vi.fn((_input: string | URL | Request, init?: RequestInit) => {
          if (init?.method === 'DELETE') return Promise.resolve(deleteResponse());
          return Promise.resolve(jsonResponse({ agents: rows, _capabilities: { actions: [] } }));
        })
      );
    }

    async function mountPage(): Promise<TestEl> {
      const el = document.createElement('scion-page-agents') as TestEl;
      document.body.appendChild(el);
      await el.updateComplete;
      await flush();
      await el.updateComplete;
      return el;
    }

    type Internals = {
      handleAgentAction(id: string, action: string, event?: MouseEvent): Promise<void>;
    };
    const altClick = { altKey: true } as MouseEvent; // skips the confirm dialog

    const actionable = (id: string): Agent =>
      agent(id, { _capabilities: { actions: ['read', 'lifecycle', 'delete'] } });

    function trashButtons(el: TestEl): number {
      // The badge's own trash icon lives in its shadow root, so only the
      // Delete buttons are counted here.
      return el.shadowRoot?.querySelectorAll('sl-icon[name="trash"]').length ?? 0;
    }

    async function badgeTexts(el: TestEl): Promise<string[]> {
      const badges = [...(el.shadowRoot?.querySelectorAll('scion-deletion-badge') ?? [])] as Array<
        HTMLElement & { updateComplete: Promise<boolean> }
      >;
      await Promise.all(badges.map((b) => b.updateComplete));
      return badges
        .map((b) => b.shadowRoot?.querySelector('.badge')?.textContent?.trim() ?? '')
        .filter(Boolean);
    }

    it('202 keeps the row with Deleting… and hides its actions; SSE deleted then removes it', async () => {
      const rows = [actionable('a1'), actionable('a2')];
      stubHub(
        rows,
        () =>
          new Response(JSON.stringify({ agentId: 'a1', deletion: deletionView }), {
            status: 202,
            headers: { 'Content-Type': 'application/json' },
          })
      );
      const el = await mountPage();
      expect(trashButtons(el)).toBe(2);

      await (el as unknown as Internals).handleAgentAction('a1', 'delete', altClick);
      await flush();
      await el.updateComplete;

      expect(el.agents.map((a) => a.id).sort()).toEqual(['a1', 'a2']);
      expect(el.agents.find((a) => a.id === 'a1')?.deletion?.state).toBe('deleting');
      expect(await badgeTexts(el)).toEqual(['Deleting…']);
      expect(trashButtons(el)).toBe(1);

      handleUpdate('agent.a1.deleted', {});
      await flush();
      await el.updateComplete;
      expect(el.agents.map((a) => a.id)).toEqual(['a2']);
    });

    it('a failed delta re-enables the row actions; deletion:null clears the badge', async () => {
      stubHub([actionable('a1')], () => new Response(null));
      const el = await mountPage();
      handleUpdate('agent.a1.status', { deletion: deletionView });
      await flush();
      await el.updateComplete;
      expect(trashButtons(el)).toBe(0);

      handleUpdate('agent.a1.status', {
        deletion: { ...deletionView, state: 'failed', code: 'conflict' },
      });
      await flush();
      await el.updateComplete;
      expect(await badgeTexts(el)).toEqual(['Delete failed: conflict']);
      expect(trashButtons(el)).toBe(1);

      handleUpdate('agent.a1.status', { deletion: null });
      await flush();
      await el.updateComplete;
      expect(await badgeTexts(el)).toEqual([]);
      expect(trashButtons(el)).toBe(1);
    });

    it('force DELETE 202 (502, confirm, force 202) keeps the row with Deleting…', async () => {
      const rows = [actionable('a1'), actionable('a2')];
      const deletes: string[] = [];
      vi.stubGlobal(
        'fetch',
        vi.fn((input: string | URL | Request, init?: RequestInit) => {
          if (init?.method === 'DELETE') {
            const url = typeof input === 'string' ? input : input.toString();
            deletes.push(url);
            if (!url.includes('force=true')) {
              return Promise.resolve(new Response('{}', { status: 502 }));
            }
            return Promise.resolve(
              new Response(JSON.stringify({ agentId: 'a1', deletion: deletionView }), {
                status: 202,
                headers: { 'Content-Type': 'application/json' },
              })
            );
          }
          return Promise.resolve(jsonResponse({ agents: rows, _capabilities: { actions: [] } }));
        })
      );
      const el = await mountPage();

      await (el as unknown as Internals).handleAgentAction('a1', 'delete', altClick);
      await flush();
      await el.updateComplete;

      expect(deletes).toEqual(['/api/v1/agents/a1', '/api/v1/agents/a1?force=true']);
      expect(el.agents.map((a) => a.id).sort()).toEqual(['a1', 'a2']);
      expect(await badgeTexts(el)).toEqual(['Deleting…']);
      expect(trashButtons(el)).toBe(1);
    });

    it("table (list) view shows the badge in the row and hides that row's Delete", async () => {
      localStorage.setItem('scion-view-agents', 'list');
      stubHub([actionable('a1'), actionable('a2')], () => new Response(null));
      const el = await mountPage();
      expect(el.shadowRoot?.querySelectorAll('tbody tr').length).toBe(2);

      handleUpdate('agent.a1.status', { deletion: deletionView });
      await flush();
      await el.updateComplete;

      const badgeEls = [
        ...(el.shadowRoot?.querySelectorAll('tbody tr scion-deletion-badge') ?? []),
      ] as Array<HTMLElement & { updateComplete: Promise<boolean> }>;
      await Promise.all(badgeEls.map((b) => b.updateComplete));
      const rowBadges = badgeEls
        .map((b) => b.shadowRoot?.querySelector('.badge')?.textContent?.trim())
        .filter(Boolean);
      expect(rowBadges).toEqual(['Deleting…']);
      expect(el.shadowRoot?.querySelectorAll('tbody tr sl-icon[name="trash"]').length).toBe(1);
    });

    it('204 removes the row immediately, as before', async () => {
      const rows = [actionable('a1'), actionable('a2')];
      stubHub(rows, () => {
        rows.splice(0, 1); // the hub has deleted a1 for the background refresh
        return new Response(null, { status: 204 });
      });
      const el = await mountPage();

      await (el as unknown as Internals).handleAgentAction('a1', 'delete', altClick);
      await el.updateComplete;
      expect(el.agents.map((a) => a.id)).toEqual(['a2']);
    });
  });
});
