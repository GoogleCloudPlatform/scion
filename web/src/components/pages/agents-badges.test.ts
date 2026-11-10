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
 * Agent badges on the /agents page (web.agent_badges, ptone/scion#5503):
 * the grid card and the table row show `scion-agent-badges` in place of the
 * cpu icon while the experiment is on, built from the list row's
 * harnessVendor and modelSize, and an agent that arrives by SSE gets its
 * badges from the event with no request. With the experiment off nothing
 * changes. Exercises the real `stateManager`; only `fetch`, `localStorage`
 * and `EventSource` are faked.
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import './agents.js';
import type { ScionPageAgents } from './agents.js';
import { stateManager } from '../../client/state.js';
import type { Agent } from '../../shared/types.js';
import { requestUrl } from '../../client/__fixtures__/request-url.js';
import { resetServerFlagStateForTests } from '../../utils/feature-flags.js';

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

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
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
  // state.ts flushes after 100ms with no animation frame.
  await new Promise((r) => setTimeout(r, 150));
}

type TestEl = ScionPageAgents & { agents: Agent[] };

const requests: string[] = [];

async function mount(initial: Agent[]): Promise<TestEl> {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: string | URL | Request) => {
      requests.push(requestUrl(input));
      return Promise.resolve(jsonResponse({ agents: initial, _capabilities: { actions: [] } }));
    })
  );
  const el = document.createElement('scion-page-agents') as TestEl;
  document.body.appendChild(el);
  await el.updateComplete;
  await flush();
  await el.updateComplete;
  return el;
}

/** The badges element in the card (grid) or row (list) of the agent named `name`. */
function badgesFor(el: TestEl, name: string): HTMLElement | null {
  const root = el.shadowRoot!;
  const link = Array.from(root.querySelectorAll<HTMLAnchorElement>('a')).find(
    (a) => a.getAttribute('href') === `/agents/${name}`
  );
  const container = link?.closest('h3, .name-cell');
  return (container?.querySelector('scion-agent-badges') as HTMLElement | null) ?? null;
}

function cpuIconFor(el: TestEl, name: string): Element | null {
  const link = Array.from(el.shadowRoot!.querySelectorAll('a')).find(
    (a) => a.getAttribute('href') === `/agents/${name}`
  );
  return link?.closest('h3, .name-cell')?.querySelector('sl-icon[name="cpu"]') ?? null;
}

describe('scion-page-agents agent badges', () => {
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    // Force a real scope change so rows from another test do not leak in.
    stateManager.setScope({ type: 'brokers-list' });
    requests.length = 0;
    resetServerFlagStateForTests();
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-agents').forEach((n) => n.remove());
    delete window.__SCION_FEATURES__;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  for (const view of ['grid', 'list'] as const) {
    it(`${view}: shows the badges from the row's fields in place of the cpu icon when on`, async () => {
      window.__SCION_FEATURES__ = { 'web.agent_badges': true };
      localStorage.setItem('scion-view-agents', view);
      const el = await mount([agent('a1', { harnessVendor: 'anthropic', modelSize: 'large' })]);

      const badges = badgesFor(el, 'a1');
      expect(badges).not.toBeNull();
      expect(badges?.getAttribute('vendor')).toBe('anthropic');
      expect(badges?.getAttribute('size')).toBe('large');
      expect(cpuIconFor(el, 'a1')).toBeNull();
      await (badges as unknown as { updateComplete: Promise<unknown> }).updateComplete;
      expect(badges?.shadowRoot?.querySelector('.size')?.textContent).toBe('L');
    });

    it(`${view}: keeps the cpu icon and creates no badges when off`, async () => {
      window.__SCION_FEATURES__ = { 'web.agent_badges': false };
      localStorage.setItem('scion-view-agents', view);
      const el = await mount([agent('a1', { harnessVendor: 'anthropic', modelSize: 'large' })]);

      expect(el.shadowRoot!.querySelector('scion-agent-badges')).toBeNull();
      expect(cpuIconFor(el, 'a1')).not.toBeNull();
    });
  }

  it('an SSE-created agent shows its badges with no request, and a status event clears its size', async () => {
    window.__SCION_FEATURES__ = { 'web.agent_badges': true };
    const el = await mount([agent('a1')]);
    expect(requests.length).toBe(1);

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
      harnessVendor: 'google',
      modelSize: 'small',
    });
    await flush();
    await el.updateComplete;

    let badges = badgesFor(el, 'a4');
    expect(badges?.getAttribute('vendor')).toBe('google');
    expect(badges?.getAttribute('size')).toBe('small');

    // The hub sends modelSize '' on a status event to clear it.
    handleUpdate('agent.a4.status', { agentId: 'a4', phase: 'stopped', modelSize: '' });
    await flush();
    await el.updateComplete;

    badges = badgesFor(el, 'a4');
    expect(badges?.getAttribute('vendor')).toBe('google');
    expect(badges?.getAttribute('size')).toBe('');
    // The created event's hydration read is the only other request: the
    // badges themselves fetch nothing.
    expect(requests.filter((u) => !u.startsWith('/api/v1/agents/a4')).length).toBe(1);
  });
});
