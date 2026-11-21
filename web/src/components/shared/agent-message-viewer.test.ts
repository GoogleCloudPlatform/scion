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
 * The message viewer lists only message records from the message log:
 * lifecycle entries (log_kind=event, or no sender and no recipient) do
 * not render as "unknown -> unknown" rows.
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

// The user directory's lookups go through apiFetch; the rest of api.js is real.
const { apiFetchMock } = vi.hoisted(() => ({ apiFetchMock: vi.fn() }));
vi.mock('../../client/api.js', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../client/api.js')>()),
  apiFetch: (path: string, options?: RequestInit) => apiFetchMock(path, options),
}));

beforeAll(async () => {
  await import('./agent-message-viewer.js');
});

// The viewer renders ingested entries on the next animation frame
// (webperf C6); run frames synchronously so `updateComplete` covers them.
beforeEach(() => {
  vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
    cb(0);
    return 1;
  });
  vi.stubGlobal('cancelAnimationFrame', () => {});
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.unstubAllGlobals();
});

function entry(insertId: string, labels: Record<string, string>) {
  return {
    timestamp: '2026-09-23T15:00:00.000Z',
    severity: 'INFO',
    message: 'log line',
    labels,
    jsonPayload: {},
    insertId,
  };
}

describe('scion-agent-message-viewer message log filtering', () => {
  it('keeps message records and drops lifecycle entries', async () => {
    const el: AnyEl = document.createElement('scion-agent-message-viewer');
    el.projectId = 'p1';
    document.body.appendChild(el);

    el.mergeEntries([
      entry('record', { sender: 'user:alice', recipient: 'agent:bob', msg_type: 'instruction' }),
      // Lifecycle line tagged as an event, even with sender/recipient.
      entry('event', { sender: 'agent:bob', recipient: 'user:alice', log_kind: 'event' }),
      // Older lifecycle line with neither sender nor recipient.
      entry('bare', { msg_type: 'instruction', recipient_id: 'u1' }),
    ]);
    await el.updateComplete;

    const ids = (el.buffer.items as { insertId: string }[]).map((m) => m.insertId);
    expect(ids).toEqual(['record']);

    const actors = Array.from(
      el.shadowRoot.querySelectorAll('.msg-actor, .msg-target') as NodeListOf<HTMLElement>
    ).map((n) => (n.textContent ?? '').trim());
    expect(actors).toEqual(['user:alice', 'agent:bob']);
    expect(actors).not.toContain('unknown');
  });
});

// A message sent with an agent delegated credential lists the agent as the
// sender and adds "authorized by <issuer> via delegation <grant>"
// (.design/agent-delegation.md §14.5). Ordinary messages show no such line.
describe('scion-agent-message-viewer authorized-by line', () => {
  function hubMessage(id: string, extra: Record<string, string> = {}) {
    return {
      id,
      projectId: 'p1',
      sender: 'agent:builder',
      senderId: 'agent-builder-id',
      recipient: 'agent:reviewer',
      recipientId: 'agent-reviewer-id',
      msg: 'build is green',
      type: 'instruction',
      agentId: 'agent-reviewer-id',
      createdAt: '2026-09-23T15:00:00.000Z',
      ...extra,
    };
  }

  beforeEach(async () => {
    apiFetchMock.mockReset();
    const { resetUserDirectory } = await import('../../client/user-directory.js');
    resetUserDirectory();
  });

  async function mountWith(items: unknown[]): Promise<AnyEl> {
    const { rememberUsers } = await import('../../client/user-directory.js');
    rememberUsers([{ id: 'u-alice', displayName: 'Alice Example', email: 'alice@example.com' }]);
    const el: AnyEl = document.createElement('scion-agent-message-viewer');
    el.projectId = 'p1';
    el.agentId = 'agent-reviewer-id';
    document.body.appendChild(el);
    el.mergeHubMessages(items);
    await el.updateComplete;
    return el;
  }

  function lines(el: AnyEl): string[] {
    return Array.from(
      el.shadowRoot.querySelectorAll('.msg-authorized-by') as NodeListOf<HTMLElement>
    ).map((n) => (n.textContent ?? '').trim());
  }

  it('names the issuer by display name and keeps the agent as sender', async () => {
    const el = await mountWith([
      hubMessage('d1', { authorizingUserId: 'u-alice', delegationGrantId: 'grant-1' }),
    ]);

    expect(lines(el)).toEqual(['authorized by Alice Example via delegation grant-1']);
    const actor = el.shadowRoot.querySelector('.msg-actor') as HTMLElement;
    expect((actor.textContent ?? '').trim()).toBe('agent:builder');
    expect(apiFetchMock).not.toHaveBeenCalled();
  });

  it('resolves an uncached issuer once and re-renders with the name', async () => {
    apiFetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: () => Promise.resolve({ id: 'u-bob', displayName: '', email: 'bob@example.com' }),
    });
    const el = await mountWith([hubMessage('d3', { authorizingUserId: 'u-bob' })]);

    await vi.waitFor(() => expect(lines(el)).toEqual(['authorized by bob@example.com']));
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
    expect(apiFetchMock.mock.calls[0]?.[0]).toBe('/api/v1/users/u-bob');
  });

  it('shows the issuer ID after a failed lookup and does not retry it', async () => {
    apiFetchMock.mockResolvedValue({ ok: false, status: 403, json: () => Promise.resolve({}) });
    const el = await mountWith([
      hubMessage('d2', { authorizingUserId: 'u-unknown', delegationGrantId: 'grant-2' }),
    ]);
    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(1));
    expect(lines(el)).toEqual(['authorized by u-unknown via delegation grant-2']);

    // Later renders (another batch, a forced update) ask the hub no more.
    el.mergeHubMessages([
      hubMessage('d4', { authorizingUserId: 'u-unknown', delegationGrantId: 'grant-2' }),
    ]);
    await el.updateComplete;
    el.requestUpdate();
    await el.updateComplete;
    await Promise.resolve();
    expect(lines(el)).toEqual([
      'authorized by u-unknown via delegation grant-2',
      'authorized by u-unknown via delegation grant-2',
    ]);
    expect(apiFetchMock).toHaveBeenCalledTimes(1);
  });

  it('shows nothing on an ordinary message', async () => {
    const el = await mountWith([hubMessage('o1')]);

    expect(el.shadowRoot.querySelectorAll('.message-row').length).toBe(1);
    expect(lines(el)).toEqual([]);
    expect(apiFetchMock).not.toHaveBeenCalled();
  });
});

describe('scion-agent-message-viewer SSE reconnect and visibility backfill', () => {
  class FakeEventSource {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSED = 2;
    static instances: FakeEventSource[] = [];

    readonly url: string;
    readyState = FakeEventSource.CONNECTING;
    onopen: ((ev: Event) => void) | null = null;
    onerror: ((ev: Event) => void) | null = null;
    closed = false;
    private listeners = new Map<string, Array<(ev: Event) => void>>();

    constructor(url: string) {
      this.url = url;
      FakeEventSource.instances.push(this);
    }

    addEventListener(type: string, cb: (ev: Event) => void): void {
      const list = this.listeners.get(type) ?? [];
      list.push(cb);
      this.listeners.set(type, list);
    }

    emitOpen(): void {
      this.readyState = FakeEventSource.OPEN;
      this.onopen?.(new Event('open'));
    }

    emitError(): void {
      this.onerror?.(new Event('error'));
    }

    emitMessage(payload: unknown): void {
      const ev = new MessageEvent('message', { data: JSON.stringify(payload) });
      for (const cb of this.listeners.get('message') ?? []) cb(ev);
    }

    close(): void {
      this.closed = true;
      this.readyState = FakeEventSource.CLOSED;
    }
  }

  function hubMessage(id: string, createdAt: string) {
    return {
      id,
      projectId: 'p1',
      sender: 'user:alice',
      senderId: 'user-alice',
      recipient: 'agent:bob',
      recipientId: 'agent-bob',
      msg: `message ${id}`,
      type: 'instruction',
      agentId: 'agent-bob',
      createdAt,
    };
  }

  function mockMessagesResponse(items: unknown[]) {
    return {
      ok: true,
      status: 200,
      json: () => Promise.resolve({ items }),
    };
  }

  let hidden = false;

  beforeEach(() => {
    FakeEventSource.instances = [];
    apiFetchMock.mockReset();
    hidden = false;
    vi.stubGlobal('EventSource', FakeEventSource);
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() =>
      hidden ? 'hidden' : 'visible'
    );
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  function setHidden(value: boolean): void {
    hidden = value;
    document.dispatchEvent(new Event('visibilitychange'));
  }

  it('backfills missed messages after EventSource error and reconnect without duplicating entries', async () => {
    const m1 = hubMessage('m1', '2026-09-23T15:00:00.000Z');
    const m2 = hubMessage('m2', '2026-09-23T15:01:00.000Z');
    const m3 = hubMessage('m3', '2026-09-23T15:02:00.000Z');

    apiFetchMock.mockResolvedValueOnce(mockMessagesResponse([m1]));

    const el: AnyEl = document.createElement('scion-agent-message-viewer');
    el.projectId = 'p1';
    el.agentId = 'agent-bob';
    document.body.appendChild(el);

    el.loadMessages();
    await vi.waitFor(() => expect(el.buffer.items).toHaveLength(1));
    expect(apiFetchMock).toHaveBeenCalledTimes(1);

    // Initial stream open should not trigger a redundant fetch.
    el.startStream();
    const es = FakeEventSource.instances[0];
    expect(es).toBeDefined();
    es.emitOpen();
    await el.updateComplete;
    expect(apiFetchMock).toHaveBeenCalledTimes(1);

    // Live message arrives over SSE.
    es.emitMessage(m2);
    await el.updateComplete;
    expect((el.buffer.items as { insertId: string }[]).map((r) => r.insertId)).toEqual([
      'hub:m1',
      'hub:m2',
    ]);

    // Stream errors and later reconnects; m3 was published during the drop.
    apiFetchMock.mockResolvedValueOnce(mockMessagesResponse([m1, m2, m3]));
    es.emitError();
    es.emitOpen();

    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(el.buffer.items).toHaveLength(3));
    expect((el.buffer.items as { insertId: string }[]).map((r) => r.insertId)).toEqual([
      'hub:m1',
      'hub:m2',
      'hub:m3',
    ]);
    await el.updateComplete;
    expect(el.shadowRoot.querySelectorAll('.message-row').length).toBe(3);
  });

  it('backfills when the tab becomes visible again without duplicating existing messages', async () => {
    const m1 = hubMessage('m1', '2026-09-23T15:00:00.000Z');
    const m2 = hubMessage('m2', '2026-09-23T15:01:00.000Z');

    apiFetchMock.mockResolvedValueOnce(mockMessagesResponse([m1]));

    const el: AnyEl = document.createElement('scion-agent-message-viewer');
    el.projectId = 'p1';
    el.agentId = 'agent-bob';
    document.body.appendChild(el);

    // Before loadMessages(), visibility changes must not eagerly fetch.
    setHidden(true);
    setHidden(false);
    expect(apiFetchMock).not.toHaveBeenCalled();

    el.loadMessages();
    await vi.waitFor(() => expect(el.buffer.items).toHaveLength(1));
    expect(apiFetchMock).toHaveBeenCalledTimes(1);

    // Tab hides and returns with m2 published in the interim.
    apiFetchMock.mockResolvedValueOnce(mockMessagesResponse([m1, m2]));
    setHidden(true);
    setHidden(false);

    await vi.waitFor(() => expect(apiFetchMock).toHaveBeenCalledTimes(2));
    await vi.waitFor(() => expect(el.buffer.items).toHaveLength(2));
    expect((el.buffer.items as { insertId: string }[]).map((r) => r.insertId)).toEqual([
      'hub:m1',
      'hub:m2',
    ]);

    // After disconnect, visibility changes must no longer fetch.
    el.remove();
    setHidden(true);
    setHidden(false);
    expect(apiFetchMock).toHaveBeenCalledTimes(2);
  });

  it('restarts a closed EventSource when the tab becomes visible', async () => {
    const m1 = hubMessage('m1', '2026-09-23T15:00:00.000Z');
    apiFetchMock.mockResolvedValue(mockMessagesResponse([m1]));

    const el: AnyEl = document.createElement('scion-agent-message-viewer');
    el.projectId = 'p1';
    el.agentId = 'agent-bob';
    document.body.appendChild(el);

    el.loadMessages();
    await vi.waitFor(() => expect(el.buffer.items).toHaveLength(1));

    el.startStream();
    const firstEs = FakeEventSource.instances[0];
    firstEs.emitOpen();
    // Simulate the browser closing the stream while backgrounded.
    firstEs.readyState = FakeEventSource.CLOSED;

    setHidden(true);
    setHidden(false);

    expect(FakeEventSource.instances).toHaveLength(2);
    expect(firstEs.closed).toBe(true);
  });
});
