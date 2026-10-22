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
 * ptone/scion#5349 (webperf C1): nothing wired up by the app may build a full
 * state snapshot per agent update. The removed debug log listened to
 * `agents-updated` and called `getStateSnapshot()` (an O(N) copy of every
 * agent, project and broker) on each flush.
 *
 * The test boots the real entry module, so it covers every listener main.ts
 * attaches to the shared `stateManager`, not just StateManager itself.
 */

import { afterAll, afterEach, describe, expect, it, vi } from 'vitest';
import { BOOT_WAIT, bootMainAt } from './__fixtures__/main-boot.js';
import { stateManager } from './state.js';

vi.mock('../components/pages/home.js', () => {
  if (!customElements.get('scion-page-home')) {
    customElements.define('scion-page-home', class extends HTMLElement {});
  }
  return {};
});

/** Feed a subject/data pair through the SSE update path. */
function emit(subject: string, data: unknown): void {
  (
    stateManager as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }
  ).handleUpdate({ subject, data });
}

/** Run the pending coalesced flush now instead of waiting for rAF or the 100ms fallback. */
function flush(): void {
  (stateManager as unknown as { flush(): void }).flush();
}

/** Listeners added to the shared stateManager; removed after each test even if an assertion fails. */
const cleanups: Array<() => void> = [];

function listen(type: string, listener: () => void): void {
  stateManager.addEventListener(type, listener);
  cleanups.push(() => stateManager.removeEventListener(type, listener));
}

describe('StateManager snapshot cost', () => {
  afterEach(() => {
    for (const cleanup of cleanups.splice(0)) cleanup();
  });

  afterAll(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('stateManager agents-updated does not call getStateSnapshot', async () => {
    await bootMainAt('/');
    await vi.waitFor(
      () => expect(document.querySelector('scion-page-home')).not.toBeNull(),
      BOOT_WAIT
    );

    const snapshot = vi.spyOn(stateManager, 'getStateSnapshot');
    const updated = vi.fn();
    listen('agents-updated', updated);

    emit('agent.a1.created', { name: 'A1', phase: 'running' });
    flush();
    updated.mockClear();

    // 100 synthetic updates, each a real change (alternating activity) and
    // each flushed on its own, so agents-updated fires once per update.
    for (let i = 0; i < 100; i++) {
      emit('agent.a1.status', { activity: i % 2 === 0 ? 'working' : 'thinking' });
      flush();
    }

    expect(updated).toHaveBeenCalledTimes(100);
    expect(snapshot).toHaveBeenCalledTimes(0);

    // The removed listener also snapshotted on project and broker updates.
    const projects = vi.fn();
    const brokers = vi.fn();
    listen('projects-updated', projects);
    listen('brokers-updated', brokers);
    emit('project.p1.updated', { name: 'P1' });
    emit('broker.b1.updated', { name: 'B1' });
    expect(projects).toHaveBeenCalledTimes(1);
    expect(brokers).toHaveBeenCalledTimes(1);
    expect(snapshot).toHaveBeenCalledTimes(0);
  }, 15_000);
});
