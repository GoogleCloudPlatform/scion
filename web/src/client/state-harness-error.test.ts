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

// harness_error is kept against end-of-turn working by the Hub, not the
// client: a "working" delta the Hub sends for it is a real clear (for
// example after OpenCode's follow-up model step) and replaces it, as a
// turn-progress activity does (ptone/scion#7858).

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import type { Agent } from '../shared/types.js';

function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
}

/** Stand-in for EventSource, which happy-dom does not implement; setScope opens one. */
class FakeEventSource extends EventTarget {
  readyState = 0;
  close(): void {
    this.readyState = 2;
  }
}

beforeEach(() => {
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function seeded(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running', activity: 'harness_error' } as Agent]);
  return sm;
}

describe('harness_error activity', () => {
  it('is replaced by a working delta (the Hub cleared it)', () => {
    const sm = seeded();
    emit(sm, 'agent.a1.status', { activity: 'working' });
    expect(sm.getAgent('a1')?.activity).toBe('working');
  });

  it('is replaced by a turn-progress activity', () => {
    const sm = seeded();
    emit(sm, 'agent.a1.status', { activity: 'thinking' });
    expect(sm.getAgent('a1')?.activity).toBe('thinking');
  });
});
