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
 * Agent message events (`agent.{id}.message`) carry a chat message payload,
 * not an agent delta, so they must never be merged into the Agent object.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';

/** Feed a subject/data pair through the SSE update path. */
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
  vi.useFakeTimers();
  vi.stubGlobal('requestAnimationFrame', () => 0);
  vi.stubGlobal('cancelAnimationFrame', () => {});
  vi.stubGlobal('EventSource', FakeEventSource);
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function detailManager(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'agent-detail', projectId: 'p1', agentId: 'a1' });
  emit(sm, 'agent.a1.created', {
    id: 'a1',
    name: 'Agent One',
    projectId: 'p1',
    phase: 'running',
    activity: 'working',
  });
  vi.advanceTimersByTime(100);
  return sm;
}

/** A message event payload as the hub publishes it. */
const messagePayload = {
  id: 'msg-1',
  agentId: 'a1',
  projectId: 'p1',
  sender: 'user:someone@example.com',
  recipient: 'agent:agent-one',
  msg: 'hello',
  message: 'hello',
  type: 'instruction',
  createdAt: '2026-01-01T00:00:00Z',
};

describe('agent message events', () => {
  it('leave the Agent object unchanged and do not notify', () => {
    const sm = detailManager();
    const before = sm.getAgent('a1');
    const updated = vi.fn();
    sm.addEventListener('agents-updated', updated);

    emit(sm, 'agent.a1.message', messagePayload);
    vi.advanceTimersByTime(100);

    expect(sm.getAgent('a1')).toBe(before);
    expect(sm.getAgent('a1')?.id).toBe('a1');
    expect(updated).not.toHaveBeenCalled();
  });

  it('for an unknown agent are not buffered or surfaced', () => {
    const sm = detailManager();
    emit(sm, 'agent.a2.message', { ...messagePayload, agentId: 'a2' });
    emit(sm, 'agent.a2.created', { id: 'a2', name: 'Agent Two', phase: 'running' });

    const agent = sm.getAgent('a2');
    expect(agent?.id).toBe('a2');
    expect(agent).not.toHaveProperty('sender');
    expect(agent).not.toHaveProperty('msg');
  });

  it('do not stop a status event from merging', () => {
    const sm = detailManager();
    const updated = vi.fn();
    sm.addEventListener('agents-updated', updated);

    emit(sm, 'agent.a1.message', messagePayload);
    emit(sm, 'agent.a1.status', { phase: 'stopped', activity: 'completed' });
    vi.advanceTimersByTime(100);

    const agent = sm.getAgent('a1');
    expect(agent?.phase).toBe('stopped');
    expect(agent?.activity).toBe('completed');
    expect(agent?.id).toBe('a1');
    expect(agent).not.toHaveProperty('sender');
    expect(updated).toHaveBeenCalledTimes(1);
  });

  it('do not stop an updated event from merging', () => {
    const sm = detailManager();
    emit(sm, 'agent.a1.updated', { name: 'Renamed' });
    expect(sm.getAgent('a1')?.name).toBe('Renamed');
  });
});
