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
 * Completeness flag unit tests (design doc §6.3, §7; perf/2385-sse-coalesce,
 * P1a). The flag lands with no callers in P1a — these tests exercise the
 * API directly: `markAgentSetComplete(view)` / `isAgentSetComplete(need)`,
 * upgrade-only semantics, and that only an actual `setScope` scope change
 * clears it (not a resync, a label-shaped reseed, or a partial seed).
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import type { Agent } from '../shared/types.js';

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

describe('completeness flag: set', () => {
  it('is unset before anything marks it complete', () => {
    const sm = new StateManager();
    expect(sm.isAgentSetComplete('compact')).toBe(false);
    expect(sm.isAgentSetComplete('full')).toBe(false);
  });

  it('markAgentSetComplete("compact") satisfies a compact need but not a full one', () => {
    const sm = new StateManager();
    sm.markAgentSetComplete('compact');
    expect(sm.isAgentSetComplete('compact')).toBe(true);
    expect(sm.isAgentSetComplete('full')).toBe(false);
  });

  it('markAgentSetComplete("full") satisfies both a compact and a full need', () => {
    const sm = new StateManager();
    sm.markAgentSetComplete('full');
    expect(sm.isAgentSetComplete('compact')).toBe(true);
    expect(sm.isAgentSetComplete('full')).toBe(true);
  });
});

describe('completeness flag: upgrade / no downgrade', () => {
  it('upgrades compact to full', () => {
    const sm = new StateManager();
    sm.markAgentSetComplete('compact');
    sm.markAgentSetComplete('full');
    expect(sm.isAgentSetComplete('full')).toBe(true);
  });

  it('never downgrades full to compact', () => {
    const sm = new StateManager();
    sm.markAgentSetComplete('full');
    sm.markAgentSetComplete('compact');
    expect(sm.isAgentSetComplete('full')).toBe(true);
    expect(sm.isAgentSetComplete('compact')).toBe(true);
  });
});

describe('completeness flag: cleared only by an actual scope change', () => {
  it('an actual scope change clears the flag', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.markAgentSetComplete('full');

    sm.setScope({ type: 'brokers-list' });

    expect(sm.isAgentSetComplete('compact')).toBe(false);
    expect(sm.isAgentSetComplete('full')).toBe(false);
  });

  it('a no-op setScope call to the same scope does not clear the flag', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.markAgentSetComplete('full');

    sm.setScope({ type: 'dashboard' }); // scopeEquals early-return: not an actual change

    expect(sm.isAgentSetComplete('full')).toBe(true);
  });

  it('a plain (label-shaped) reseed does not clear the flag', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.markAgentSetComplete('full');

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent]);

    expect(sm.isAgentSetComplete('full')).toBe(true);
  });

  it('a partial seed does not clear the flag', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.markAgentSetComplete('full');

    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { partial: true });

    expect(sm.isAgentSetComplete('full')).toBe(true);
  });

  it('a seed epoch (begin/seed/end) does not clear the flag', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.markAgentSetComplete('compact');

    const token = sm.beginSeedEpoch();
    sm.seedAgents([{ id: 'a1', name: 'A1', phase: 'running' } as Agent], { token, partial: true });
    sm.endSeedEpoch(token);

    expect(sm.isAgentSetComplete('compact')).toBe(true);
  });

  it('an SSE resync does not clear the flag', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.markAgentSetComplete('full');

    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected'));
    sm.sseClientInstance.dispatchEvent(new CustomEvent('disconnected'));
    sm.sseClientInstance.dispatchEvent(new CustomEvent('connected')); // triggers agents-resync

    expect(sm.isAgentSetComplete('full')).toBe(true);
  });

  it('an SSE delta merge does not clear the flag', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.markAgentSetComplete('full');

    (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
      subject: 'agent.a1.created',
      data: { phase: 'running', name: 'A1' },
    });

    expect(sm.isAgentSetComplete('full')).toBe(true);
  });
});
