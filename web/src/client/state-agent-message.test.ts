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
import { StateManager, type AgentsChangedDetail } from './state.js';

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

const AGENT_CREATED_AT = '2026-01-01T00:00:00Z';

function detailManager(): StateManager {
  const sm = new StateManager();
  sm.setScope({ type: 'agent-detail', projectId: 'p1', agentId: 'a1' });
  emit(sm, 'agent.a1.created', {
    id: 'a1',
    name: 'Agent One',
    projectId: 'p1',
    phase: 'running',
    activity: 'working',
    createdAt: AGENT_CREATED_AT,
  });
  vi.advanceTimersByTime(100);
  return sm;
}

/** A message event payload with the fields of the hub's UserMessageEvent. */
const messagePayload = {
  id: 'msg-1',
  projectId: 'p1',
  sender: 'user:someone@example.com',
  senderId: 'u1',
  recipient: 'agent:agent-one',
  recipientId: 'a1',
  msg: 'hello',
  type: 'instruction',
  agentId: 'a1',
  createdAt: '2026-02-02T12:00:00Z',
  channel: 'web',
  threadId: 'thread-1',
  read: false,
  dispatchState: 'delivered',
};

/** Message-only keys that must never appear on an Agent. */
const MESSAGE_ONLY_KEYS = [
  'sender',
  'senderId',
  'recipient',
  'recipientId',
  'msg',
  'type',
  'channel',
  'threadId',
  'read',
  'dispatchState',
];

function expectNoMessageFields(agent: unknown): void {
  for (const key of MESSAGE_ONLY_KEYS) {
    expect(agent).not.toHaveProperty(key);
  }
}

describe('agent message events', () => {
  it('leave the Agent object unchanged and do not notify', () => {
    const sm = detailManager();
    const before = sm.getAgent('a1');
    const updated = vi.fn();
    sm.addEventListener('agents-updated', updated);

    emit(sm, 'agent.a1.message', messagePayload);
    vi.advanceTimersByTime(100);

    const after = sm.getAgent('a1');
    expect(after).toBe(before);
    expect(after?.createdAt).toBe(AGENT_CREATED_AT);
    expectNoMessageFields(after);
    expect(updated).not.toHaveBeenCalled();
  });

  it('for an unknown agent are not buffered or surfaced', () => {
    const sm = detailManager();
    const changed = vi.fn();
    sm.addEventListener('agents-changed', changed);

    emit(sm, 'agent.a2.message', { ...messagePayload, agentId: 'a2', recipientId: 'a2' });
    vi.advanceTimersByTime(100);

    // A flush before the created event must not report a2 as unknown.
    for (const [event] of changed.mock.calls) {
      const detail = (event as CustomEvent<AgentsChangedDetail>).detail;
      expect(detail.unknown.has('a2')).toBe(false);
    }
    expect(sm.getAgent('a2')).toBeUndefined();

    emit(sm, 'agent.a2.created', {
      id: 'a2',
      name: 'Agent Two',
      phase: 'running',
      createdAt: AGENT_CREATED_AT,
    });

    const agent = sm.getAgent('a2');
    expect(agent?.createdAt).toBe(AGENT_CREATED_AT);
    expectNoMessageFields(agent);
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
    expect(agent?.createdAt).toBe(AGENT_CREATED_AT);
    expectNoMessageFields(agent);
    expect(updated).toHaveBeenCalledTimes(1);
  });

  it('do not stop an updated event from merging', () => {
    const sm = detailManager();
    emit(sm, 'agent.a1.updated', { name: 'Renamed' });
    expect(sm.getAgent('a1')?.name).toBe('Renamed');
  });
});
