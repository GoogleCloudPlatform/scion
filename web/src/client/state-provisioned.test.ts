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
 * ptone/scion#2929: status deltas always carry `provisionedOnly`, so the
 * merged value follows each delta, true or false.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import { isProvisionedOnly } from '../shared/agent-state-display.js';

function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
}

class FakeEventSource extends EventTarget {
  readyState = 0;
  close(): void {
    this.readyState = 2;
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('requestAnimationFrame', () => 0);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function managerWith(provisionedOnly: boolean): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  emit(sm, 'agent.a1.created', { phase: 'created', name: 'A1', provisionedOnly });
  vi.advanceTimersByTime(100);
  return sm;
}

function shown(sm: StateManager): boolean {
  const agent = sm.getAgent('a1');
  return agent !== undefined && isProvisionedOnly(agent);
}

describe('provisionedOnly SSE merge', () => {
  // Only the merge is tested here. The hub does not publish a status
  // delta on a failed start, so the web shows that case on the next refetch.
  it('a status delta with provisionedOnly false clears the status', () => {
    const sm = managerWith(true);
    expect(shown(sm)).toBe(true);
    emit(sm, 'agent.a1.status', { phase: 'created', provisionedOnly: false });
    vi.advanceTimersByTime(100);
    expect(shown(sm)).toBe(false);
  });

  it('a stop on a full create in created sets the status', () => {
    const sm = managerWith(false);
    expect(shown(sm)).toBe(false);
    emit(sm, 'agent.a1.status', { phase: 'created', provisionedOnly: true });
    vi.advanceTimersByTime(100);
    expect(shown(sm)).toBe(true);
  });
});
