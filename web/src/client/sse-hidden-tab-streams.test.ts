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
 * Hidden-tab stream close (ptone/scion#5235) through the three events stream
 * owners: the page's view stream, the agent store's feed and terminal
 * metadata. Each closes after 60 s hidden with web.sse_hidden_tab_close on,
 * and on return reopens and resyncs through its existing recovery path.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  agent,
  createHarness,
  FakeEventSource,
  settle,
} from './__fixtures__/agent-store-harness.js';
import { StateManager } from './state.js';
import { TerminalMetadata } from './terminal-metadata.js';

const HUB = { scope: 'hub' } as const;

let visibility: DocumentVisibilityState = 'visible';

/** Move the tab between foreground and background, as the browser would. */
function setVisibility(state: DocumentVisibilityState): void {
  visibility = state;
  document.dispatchEvent(new Event('visibilitychange'));
}

/**
 * The harness's fake with the EventSource readyState constants, which the
 * client's resume path compares against.
 */
class Source extends FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
}

/** Streams not yet closed. */
function openStreams(): FakeEventSource[] {
  return FakeEventSource.instances.filter((es) => !es.closed);
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.spyOn(console, 'info').mockImplementation(() => {});
  FakeEventSource.instances = [];
  vi.stubGlobal('EventSource', Source);
  visibility = 'visible';
  Object.defineProperty(document, 'visibilityState', {
    configurable: true,
    get: () => visibility,
  });
  window.__SCION_FEATURES__ = { 'web.sse_hidden_tab_close': true };
});

afterEach(() => {
  delete window.__SCION_FEATURES__;
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('view stream (StateManager)', () => {
  it('closes after 60 s hidden and emits agents-resync once it reopens on return', async () => {
    const state = new StateManager();
    const resync = vi.fn();
    state.addEventListener('agents-resync', resync);
    state.setScope({ type: 'dashboard' });
    openStreams()[0]?.open();
    expect(state.isConnected).toBe(true);

    setVisibility('hidden');
    vi.advanceTimersByTime(60_000);
    expect(openStreams()).toHaveLength(0);
    expect(state.isConnected).toBe(false);

    setVisibility('visible');
    expect(openStreams()).toHaveLength(1);
    openStreams()[0]?.open();
    await settle();
    expect(resync).toHaveBeenCalledTimes(1);
    expect(state.isConnected).toBe(true);
    state.disconnect();
  });

  it('stays open while hidden with the experiment off', () => {
    window.__SCION_FEATURES__ = { 'web.sse_hidden_tab_close': false };
    const state = new StateManager();
    state.setScope({ type: 'dashboard' });
    openStreams()[0]?.open();

    setVisibility('hidden');
    vi.advanceTimersByTime(10 * 60_000);
    expect(openStreams()).toHaveLength(1);
    state.disconnect();
  });
});

describe('agent store feed', () => {
  it('closes after 60 s hidden, and on return reopens and walks the retained list again', async () => {
    const h = createHarness([agent('a1')]);
    // createHarness stubs its own fake; put the one with constants back.
    vi.stubGlobal('EventSource', Source);
    const release = h.store.retain(HUB, () => {});
    const first = h.store.ensure(HUB);
    await h.connect();
    await first;
    expect(h.server.walks()).toBe(1);

    h.visibility.set('hidden');
    setVisibility('hidden');
    await vi.advanceTimersByTimeAsync(60_000);
    expect(openStreams()).toHaveLength(0);
    expect(h.server.walks()).toBe(1);

    h.server.agents.push(agent('a2'));
    setVisibility('visible');
    h.visibility.set('visible');
    expect(openStreams()).toHaveLength(1);
    await h.connect();
    expect(h.server.walks()).toBe(2);
    expect(h.store.peek(HUB)?.agents.map((a) => a.id)).toEqual(['a1', 'a2']);
    release();
    h.store.destroy();
  });
});

describe('terminal metadata', () => {
  const id = '11111111-1111-4111-8111-000000000001';
  const flush = async (): Promise<void> => {
    for (let i = 0; i < 12; i++) await Promise.resolve();
  };

  it('closes after 60 s hidden, and on return reopens and refetches the agent', async () => {
    const fetcher = vi
      .fn<typeof fetch>()
      .mockImplementation(() =>
        Promise.resolve(
          new Response(JSON.stringify({ id, name: 'a1', phase: 'running', activity: 'idle' }))
        )
      );
    vi.stubGlobal('fetch', fetcher);
    const metadata = new TerminalMetadata('https://hub.example/team/');
    metadata.retain(id);
    await flush();
    openStreams()[0]?.open();
    await flush();
    expect(fetcher).toHaveBeenCalledTimes(1);
    expect(metadata.get(id)?.availability).toBe('ready');

    setVisibility('hidden');
    vi.advanceTimersByTime(60_000);
    expect(openStreams()).toHaveLength(0);

    setVisibility('visible');
    expect(openStreams()).toHaveLength(1);
    openStreams()[0]?.open();
    await flush();
    expect(fetcher).toHaveBeenCalledTimes(2);
    expect(metadata.get(id)?.availability).toBe('ready');
    metadata.dispose();
  });
});
