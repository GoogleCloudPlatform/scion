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

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';
import type { Agent } from '../shared/types.js';

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
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

function mk(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'dashboard' });
  return sm;
}

function makeAgent(
  overrides: Omit<Partial<Agent>, 'activity'> & { activity?: string | undefined }
): Agent {
  return {
    id: 'a1',
    name: 'a1',
    projectId: 'p1',
    template: 'default',
    phase: 'running',
    ...overrides,
  } as Agent;
}

describe('StateManager suspend and resume sticky-activity handling', () => {
  it('keeps completed sticky while running, while allowing idle -> working transitions', () => {
    const sm = mk();
    sm.seedAgents([
      makeAgent({ id: 'a-done', phase: 'running', activity: 'completed' }),
      makeAgent({ id: 'a-idle', phase: 'running', activity: 'idle' }),
    ]);

    emit(sm, 'agent.a-done.status', { phase: 'running', activity: 'working' });
    expect(sm.getAgent('a-done')?.activity).toBe('completed');

    emit(sm, 'agent.a-idle.status', { phase: 'running', activity: 'working' });
    expect(sm.getAgent('a-idle')?.activity).toBe('working');
  });

  it('clears sticky non-retained activity when transitioning to suspended with empty activity', () => {
    const sm = mk();
    sm.seedAgents([makeAgent({ phase: 'running', activity: 'waiting_for_input' })]);

    emit(sm, 'agent.a1.status', { phase: 'suspended', activity: '' });
    expect(sm.getAgent('a1')?.phase).toBe('suspended');
    expect(sm.getAgent('a1')?.activity).toBe('');
  });

  it('retains completed, idle, and harness_error across suspend and clears them on resume', () => {
    for (const retained of ['completed', 'idle', 'harness_error'] as const) {
      const sm = mk();
      sm.seedAgents([makeAgent({ phase: 'running', activity: retained })]);

      // Suspend retains the eligible activity.
      emit(sm, 'agent.a1.status', { phase: 'suspended', activity: retained });
      expect(sm.getAgent('a1')?.phase).toBe('suspended');
      expect(sm.getAgent('a1')?.activity).toBe(retained);

      // Resume starting/running with empty or working activity clears retained activity.
      emit(sm, 'agent.a1.status', { phase: 'running', activity: '' });
      expect(sm.getAgent('a1')?.phase).toBe('running');
      expect(sm.getAgent('a1')?.activity).toBe('');
    }
  });

  it('clears retained activity when resuming through starting -> running -> working', () => {
    const sm = mk();
    sm.seedAgents([makeAgent({ phase: 'suspended', activity: 'completed' })]);

    emit(sm, 'agent.a1.status', { phase: 'starting', activity: '' });
    expect(sm.getAgent('a1')?.phase).toBe('starting');
    expect(sm.getAgent('a1')?.activity).toBe('');

    emit(sm, 'agent.a1.status', { phase: 'running', activity: 'working' });
    expect(sm.getAgent('a1')?.phase).toBe('running');
    expect(sm.getAgent('a1')?.activity).toBe('working');
  });

  it('preserves exact equivalence when suspend/resume deltas arrive during a seed epoch', () => {
    const sm = mk();
    const token = sm.beginSeedEpoch();

    // While REST fetch is in flight returning stale suspended+completed,
    // live SSE delivers resume (running, activity: '') followed by working.
    emit(sm, 'agent.a1.status', { phase: 'running', activity: '' });
    emit(sm, 'agent.a1.status', { phase: 'running', activity: 'working' });

    sm.seedAgents([makeAgent({ phase: 'suspended', activity: 'completed' })], { token });
    sm.endSeedEpoch(token);

    expect(sm.getAgent('a1')?.phase).toBe('running');
    expect(sm.getAgent('a1')?.activity).toBe('working');
  });
});
