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
 * ptone/scion#4128: restoring a multi-pane layout from the URL must not wait
 * for each slot's focus observation one slot after another. These tests drive
 * the real TerminalCoordinator (no focus adapter, as in main.ts) under fake
 * timers.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TerminalCoordinator, type TerminalCoordinatorAdapter } from './terminal-coordinator.js';
import { TerminalLayoutManager } from './terminal-layout.js';
import { openLayoutSlots, type LayoutOpenOptions } from './terminal-layout-open.js';
import type {
  TerminalResourceInitializer,
  TerminalSession,
  TerminalSessionRegistry,
} from './terminal-sessions.js';

const A = '11111111-1111-4111-8111-111111111111';
const B = '22222222-2222-4222-8222-222222222222';
const C = '33333333-3333-4333-8333-333333333333';
const scope = { hubUrl: 'https://hub.example/team/', accountId: 'account-1' };

class FakeBroadcastChannel {
  onmessage: ((event: { data: unknown }) => void) | null = null;
  postMessage(): void {}
  close(): void {}
}

interface Harness {
  coordinator: TerminalCoordinator;
  layout: TerminalLayoutManager;
  /** Agent IDs in the order the coordinator selected their sessions. */
  selected: string[];
  /** Agent IDs in the order sessions were created. */
  created: string[];
  options(overrides?: Partial<LayoutOpenOptions>): LayoutOpenOptions;
  navigations: Map<string, number>;
}

async function harness(opts: { fail?: Set<string>; owner?: boolean } = {}): Promise<Harness> {
  vi.stubGlobal('BroadcastChannel', FakeBroadcastChannel);
  vi.stubGlobal('isSecureContext', true);
  // Opening an entry starts the registry's metadata subscription.
  vi.stubGlobal(
    'EventSource',
    class extends EventTarget {
      close(): void {}
    }
  );
  vi.stubGlobal(
    'fetch',
    vi.fn(() => new Promise<Response>(() => {}))
  );
  vi.stubGlobal('navigator', {
    ...navigator,
    locks: {
      // Granted asynchronously, as a browser does.
      request: vi.fn(
        async (name: string, _opts: unknown, callback: (lock: unknown) => Promise<void>) => {
          await Promise.resolve();
          await callback({ name, mode: 'exclusive' });
        }
      ),
    },
  });
  const layout = new TerminalLayoutManager();
  const selected: string[] = [];
  const created: string[] = [];
  let registry: TerminalSessionRegistry | null = null;
  const initialize = vi.fn<TerminalResourceInitializer>(() =>
    Promise.reject(new Error('not used'))
  );
  const adapter: TerminalCoordinatorAdapter = {
    initialize,
    create: (reg, agentId): TerminalSession => {
      registry = reg;
      if (opts.fail?.has(agentId)) throw new Error('Agent unavailable.');
      created.push(agentId);
      // Idle entry: these tests are about open/select/focus timing, not the stream.
      return reg.open(agentId, initialize, { deferConnect: true });
    },
    select: (session): void => {
      selected.push(session.state.agentId);
      layout.select(session.state.key);
    },
  };
  const coordinator = new TerminalCoordinator(scope, adapter);
  // main.ts restores the saved terminal list, which claims ownership, before
  // it restores the layout.
  if (opts.owner ?? true) expect(await coordinator.claimOwnership()).toBe(true);
  const navigations = new Map<string, number>();
  const workspace = {
    findSessionKeyByAgentId: (agentId: string): string | null =>
      registry?.list().find((s) => s.state.agentId === agentId)?.state.key ?? null,
  };
  return {
    coordinator,
    layout,
    selected,
    created,
    navigations,
    options: (overrides = {}) => ({
      coordinator,
      workspace,
      slots: [A, B, C],
      navigations,
      navigationId: 1,
      currentNavigationId: () => 1,
      ...overrides,
    }),
  };
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('openLayoutSlots (#4128)', () => {
  it('opens a 3-pane layout with one focus wait, not one per slot', async () => {
    const h = await harness();
    const focus = vi.spyOn(window, 'focus').mockImplementation(() => {});
    let keys: Array<string | null> | null | undefined;
    void openLayoutSlots(h.options()).then((result) => {
      keys = result;
    });

    // Every slot has been selected and is waiting on its own focus
    // observation at the same time.
    await vi.advanceTimersByTimeAsync(99);
    expect(h.selected).toEqual([A, B, C]);
    expect(focus).toHaveBeenCalledTimes(3);
    expect(keys).toBeUndefined();

    // One 100 ms wait settles all three; a serial restore would still be on
    // its first slot here.
    await vi.advanceTimersByTimeAsync(1);
    expect(keys).toHaveLength(3);
    expect(keys!.every((key) => key !== null)).toBe(true);
    expect(h.navigations.size).toBe(0);
  });

  it('ends with focus on the same pane as a serial restore', async () => {
    const h = await harness();
    vi.spyOn(window, 'focus').mockImplementation(() => {});
    const done = openLayoutSlots(h.options());
    await vi.advanceTimersByTimeAsync(100);
    const keys = await done;
    // Sessions are created and selected in slot order, as before.
    expect(h.created).toEqual([A, B, C]);
    expect(h.selected).toEqual([A, B, C]);
    expect(h.layout.getState().single[0]).toBe(keys![2]);
    h.layout.restore('four', keys!);
    const state = h.layout.getState();
    expect(state.active).toBe('four');
    expect(state.four).toEqual([keys![0], keys![1], keys![2], null]);
    // The first occupied slot is the selected (focused) pane.
    expect(state.single[0]).toBe(keys![0]);
  });

  it('keeps slot order when this tab has not claimed ownership yet', async () => {
    const h = await harness({ owner: false });
    vi.spyOn(window, 'focus').mockImplementation(() => {});
    const done = openLayoutSlots(h.options());
    await vi.advanceTimersByTimeAsync(100);
    expect((await done)!.every((key) => key !== null)).toBe(true);
    expect(h.created).toEqual([A, B, C]);
    expect(h.selected).toEqual([A, B, C]);
  });

  it('reuses open sessions, skips empty slots and opens a repeated agent once', async () => {
    const h = await harness();
    vi.spyOn(window, 'focus').mockImplementation(() => {});
    const first = openLayoutSlots(h.options({ slots: [A] }));
    await vi.advanceTimersByTimeAsync(100);
    const [keyA] = (await first)!;
    const open = vi.spyOn(h.coordinator, 'open');

    const done = openLayoutSlots(h.options({ slots: [A, null, B, B] }));
    await vi.advanceTimersByTimeAsync(100);
    const keys = (await done)!;
    expect(open).toHaveBeenCalledTimes(1);
    expect(open.mock.calls[0][0]).toBe(B);
    expect(keys[0]).toBe(keyA);
    expect(keys[1]).toBeNull();
    expect(keys[2]).not.toBeNull();
    expect(keys[3]).toBe(keys[2]);
  });

  it('leaves the slot of an agent that cannot be opened empty', async () => {
    const h = await harness({ fail: new Set([B]) });
    vi.spyOn(window, 'focus').mockImplementation(() => {});
    const done = openLayoutSlots(h.options());
    await vi.advanceTimersByTimeAsync(100);
    const keys = (await done)!;
    expect(keys[0]).not.toBeNull();
    expect(keys[1]).toBeNull();
    expect(keys[2]).not.toBeNull();
  });

  it('returns null when the navigation is superseded during the opens', async () => {
    const h = await harness();
    vi.spyOn(window, 'focus').mockImplementation(() => {});
    let current = 1;
    const done = openLayoutSlots(h.options({ currentNavigationId: () => current }));
    current = 2;
    await vi.advanceTimersByTimeAsync(100);
    expect(await done).toBeNull();
  });

  it('opens nothing when the navigation is already superseded', async () => {
    const h = await harness();
    const open = vi.spyOn(h.coordinator, 'open');
    expect(await openLayoutSlots(h.options({ currentNavigationId: () => 2 }))).toBeNull();
    expect(open).not.toHaveBeenCalled();
    expect(h.navigations.size).toBe(0);
  });
});
