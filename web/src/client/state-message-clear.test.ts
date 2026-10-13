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
 * ptone/scion#4415: the hub sends `detail.message: ''` when an agent's
 * status message clears, so both the nested and the top-level copy of the
 * message must clear in the StateManager.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import type { Agent } from '../shared/types.js';
import { lastRuntimeOperationIndicator } from '../components/pages/agent-placement.js';

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

/** A cleared status event as the hub sends it before StartedAt is set. */
const CLEARED = { agentId: 'a1', projectId: 'p1', phase: 'running', detail: { message: '' } };

/** The hub view: a REST row carrying the message at the top level. */
function restSeeded(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  sm.seedAgents([
    { id: 'a1', name: 'A1', projectId: 'p1', phase: 'running', message: 'Cloning' } as Agent,
  ]);
  vi.advanceTimersByTime(100);
  return sm;
}

/** An agent whose message arrived live, nested under `detail`. */
function liveSeeded(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1', projectId: 'p1' });
  emit(sm, 'agent.a1.status', { phase: 'running', detail: { message: 'Cloning' } });
  vi.advanceTimersByTime(100);
  return sm;
}

describe('cleared agent status message', () => {
  it('clears a REST-seeded top-level message', () => {
    const sm = restSeeded();
    expect(sm.getAgent('a1')?.message).toBe('Cloning');

    emit(sm, 'agent.a1.status', CLEARED);
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')?.message).toBe('');
    expect(sm.getAgent('a1')?.detail?.message).toBe('');
  });

  it('clears a live message in both the detail and top-level copies', () => {
    const sm = liveSeeded();
    expect(sm.getAgent('a1')?.message).toBe('Cloning');
    expect(sm.getAgent('a1')?.detail?.message).toBe('Cloning');

    emit(sm, 'agent.a1.status', {
      ...CLEARED,
      detail: { message: '', startedAt: '2026-10-11T00:00:00Z' },
    });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')?.message).toBe('');
    expect(sm.getAgent('a1')?.detail?.message).toBe('');
  });

  it('repeated identical cleared events re-render once', () => {
    for (const sm of [restSeeded(), liveSeeded()]) {
      const updated = vi.fn();
      sm.addEventListener('agents-updated', updated);

      emit(sm, 'agent.a1.status', CLEARED);
      vi.advanceTimersByTime(100);
      expect(updated).toHaveBeenCalledTimes(1);
      const after = sm.getAgent('a1');

      emit(sm, 'agent.a1.status', CLEARED);
      vi.advanceTimersByTime(100);
      emit(sm, 'agent.a1.status', { ...CLEARED, detail: { message: '' } });
      vi.advanceTimersByTime(1000);
      expect(updated).toHaveBeenCalledTimes(1);
      expect(sm.getAgent('a1')).toBe(after);
    }
  });

  it('a delta without detail keeps the message', () => {
    const sm = restSeeded();
    emit(sm, 'agent.a1.status', { phase: 'running', activity: 'thinking' });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.message).toBe('Cloning');
  });

  it('a detail without a message key keeps the top-level message', () => {
    // An older hub omitted an empty message from detail; that delta still
    // means "no message field", not "message cleared".
    const sm = restSeeded();
    emit(sm, 'agent.a1.status', { phase: 'running', detail: { toolName: 'bash' } });
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.message).toBe('Cloning');
  });
});

describe('cleared agent status message on replay', () => {
  it('clears through the buffer before created', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    emit(sm, 'agent.a1.status', { phase: 'running', detail: { message: 'Cloning' } });
    emit(sm, 'agent.a1.status', CLEARED);
    emit(sm, 'agent.a1.created', { phase: 'running', name: 'A1', message: 'Cloning' });
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')?.message).toBe('');
    expect(sm.getAgent('a1')?.detail?.message).toBe('');
  });

  it('clears when a stale row seeds during the epoch (reload race)', () => {
    for (const partial of [false, true]) {
      const sm = liveSeeded();
      const token = sm.beginSeedEpoch();
      emit(sm, 'agent.a1.status', CLEARED);
      sm.seedAgents(
        [{ id: 'a1', name: 'A1', projectId: 'p1', phase: 'running', message: 'Cloning' } as Agent],
        { token, partial }
      );
      vi.advanceTimersByTime(100);

      expect(sm.getAgent('a1')?.message).toBe('');
      expect(sm.getAgent('a1')?.detail?.message).toBe('');
    }
  });

  it('stays clear after a reload seeds the cleared row', () => {
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.seedAgents([
      { id: 'a1', name: 'A1', projectId: 'p1', phase: 'running', message: '' } as Agent,
    ]);
    emit(sm, 'agent.a1.status', CLEARED);
    vi.advanceTimersByTime(100);
    expect(sm.getAgent('a1')?.message).toBe('');
  });

  it('clears the placement view message', () => {
    // The placement view reads only the top-level message.
    const sm = new StateManager();
    sm.setScope({ type: 'dashboard' });
    sm.seedAgents([
      { id: 'a1', name: 'A1', projectId: 'p1', phase: 'stopped', message: 'refused' } as Agent,
    ]);
    vi.advanceTimersByTime(100);
    expect(lastRuntimeOperationIndicator(sm.getAgent('a1')!).label).toBe('see message');

    emit(sm, 'agent.a1.status', { ...CLEARED, phase: 'stopped' });
    vi.advanceTimersByTime(100);
    expect(lastRuntimeOperationIndicator(sm.getAgent('a1')!)).toEqual({
      label: 'none recorded',
      status: 'neutral',
    });
  });
});
