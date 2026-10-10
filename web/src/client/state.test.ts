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
 * Tests for StateManager SSE subject routing.
 *
 * Human-to-human DMs have no project, so the hub fans their typing events out
 * on `user.{userId}.chat.typing`. Routing that subject to
 * `chat-message-received` (as every user-scoped chat subject once was) makes
 * the thread refetch history and never show a typing indicator.
 */

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StateManager } from './state.js';

/** Feed a subject/data pair through the SSE update path. */
function emit(sm: StateManager, subject: string, data: unknown): void {
  (sm as unknown as { handleUpdate(u: { subject: string; data: unknown }): void }).handleUpdate({
    subject,
    data,
  });
}

describe('StateManager user-scoped chat subjects', () => {
  it('routes user.{id}.chat.typing to chat-typing-received', () => {
    const sm = new StateManager();
    const typing = vi.fn();
    const message = vi.fn();
    sm.addEventListener('chat-typing-received', typing);
    sm.addEventListener('chat-message-received', message);

    const data = { threadId: 'dm:user:a:user:b', userId: 'a', displayName: 'Ada' };
    emit(sm, 'user.b.chat.typing', data);

    expect(message).not.toHaveBeenCalled();
    expect(typing).toHaveBeenCalledTimes(1);
    const detail = (typing.mock.calls[0]?.[0] as CustomEvent).detail as { data: unknown };
    expect(detail.data).toEqual(data);
  });

  it('still routes user.{id}.chat.dm to chat-message-received', () => {
    const sm = new StateManager();
    const typing = vi.fn();
    const message = vi.fn();
    sm.addEventListener('chat-typing-received', typing);
    sm.addEventListener('chat-message-received', message);

    emit(sm, 'user.b.chat.dm', { threadId: 'dm:user:a:user:b', id: 'm1' });

    expect(typing).not.toHaveBeenCalled();
    expect(message).toHaveBeenCalledTimes(1);
  });

  it('routes user.{id}.chat.scheduled to chat-scheduled-updated, never as a message', () => {
    const sm = new StateManager();
    const scheduled = vi.fn();
    const message = vi.fn();
    sm.addEventListener('chat-scheduled-updated', scheduled);
    sm.addEventListener('chat-message-received', message);

    const data = { action: 'created', scheduledMessage: { id: 's1', conversationKey: 't1' } };
    emit(sm, 'user.b.chat.scheduled', data);

    expect(message).not.toHaveBeenCalled();
    expect(scheduled).toHaveBeenCalledTimes(1);
    const detail = (scheduled.mock.calls[0]?.[0] as CustomEvent).detail as { data: unknown };
    expect(detail.data).toEqual(data);
  });
});

/**
 * DM messages and read-state changes are published on the caller's own
 * `user.{id}.chat.*` subjects. They must be subscribed in every scope, so a
 * DM raises a popup and moves the unread badge on any page.
 */
describe('StateManager own chat subject', () => {
  const subjectsFor = (
    sm: StateManager,
    scope: Parameters<StateManager['setScope']>[0]
  ): string[] =>
    (sm as unknown as { subjectsForScope(s: unknown): string[] }).subjectsForScope(scope);

  it('marks a message on the own subject as delivered to the user', () => {
    const sm = new StateManager();
    const message = vi.fn();
    sm.addEventListener('chat-message-received', message);

    emit(sm, 'user.b.chat.dm', { threadId: 'dm:user:a:user:b', id: 'm1' });

    const detail = (message.mock.calls[0]?.[0] as CustomEvent).detail as { data: unknown };
    expect(detail.data).toEqual({
      threadId: 'dm:user:a:user:b',
      id: 'm1',
      deliveredToUser: true,
    });
  });

  it('routes the per-user notification subject to notification-created only', () => {
    const sm = new StateManager();
    const created = vi.fn();
    const message = vi.fn();
    sm.addEventListener('notification-created', created);
    sm.addEventListener('chat-message-received', message);

    const data = { id: 'n1', status: 'SCHEDULE_BLOCKED' };
    emit(sm, 'user.b.notification', data);

    expect(created).toHaveBeenCalledTimes(1);
    const detail = (created.mock.calls[0]?.[0] as CustomEvent).detail as { data: unknown };
    expect(detail.data).toEqual(data);
    expect(message).not.toHaveBeenCalled();
  });

  it('still routes the unscoped notification subject to notification-created', () => {
    const sm = new StateManager();
    const created = vi.fn();
    sm.addEventListener('notification-created', created);

    emit(sm, 'notification.created', { id: 'notif-2', status: 'COMPLETED' });

    expect(created).toHaveBeenCalledTimes(1);
  });

  it('raises no legacy user-message-created event for user-message subjects', () => {
    const sm = new StateManager();
    const legacy = vi.fn();
    const message = vi.fn();
    sm.addEventListener('user-message-created', legacy);
    sm.addEventListener('chat-message-received', message);

    emit(sm, 'project.p1.user.message', { id: 'm1' });
    emit(sm, 'project.p1.chat.message', { id: 'm2', threadId: 't1' });

    expect(legacy).not.toHaveBeenCalled();
    expect(message).toHaveBeenCalledTimes(1);
  });

  it('subscribes to the own chat and notification subjects in every view scope', () => {
    const sm = new StateManager();
    expect(subjectsFor(sm, { type: 'dashboard' })).not.toContain('user.me.chat.>');

    sm.setCurrentUserId('me');

    for (const scope of [
      { type: 'dashboard' },
      { type: 'project', projectId: 'p1' },
      { type: 'agent-detail', projectId: 'p1', agentId: 'a1' },
      { type: 'brokers-list' },
    ] as const) {
      const subjects = subjectsFor(sm, scope);
      expect(subjects).toContain('user.me.chat.>');
      expect(subjects).toContain('user.me.notification');
    }
  });

  it('lists the own chat subject once in chat scope', () => {
    const sm = new StateManager();
    sm.setCurrentUserId('me');
    const subjects = subjectsFor(sm, { type: 'chat', spaceIds: ['p1'], userId: 'me' });
    expect(subjects.filter((s) => s === 'user.me.chat.>')).toHaveLength(1);
  });
});

describe('StateManager agent-feed scope', () => {
  const subjectsFor = (
    sm: StateManager,
    scope: Parameters<StateManager['setScope']>[0]
  ): string[] =>
    (sm as unknown as { subjectsForScope(s: unknown): string[] }).subjectsForScope(scope);

  it('subscribes the agent-feed scope to exactly the cross-project agent subject', () => {
    const sm = new StateManager();
    expect(subjectsFor(sm, { type: 'agent-feed' })).toEqual(['project.*.agent.>']);

    sm.setCurrentUserId('me');

    // No own chat subject: the view connection already holds it.
    expect(subjectsFor(sm, { type: 'agent-feed' })).toEqual(['project.*.agent.>']);
  });

  it('treats a repeated agent-feed scope as unchanged', () => {
    const sm = new StateManager();
    const connect = vi.fn();
    (sm as unknown as { sseClient: { connect: (s: string[]) => void } }).sseClient.connect =
      connect;

    sm.setScope({ type: 'agent-feed' });
    const generation = sm.scopeGeneration;
    sm.setScope({ type: 'agent-feed' });

    expect(sm.scopeGeneration).toBe(generation);
    expect(connect).toHaveBeenCalledTimes(1);
    expect(connect).toHaveBeenCalledWith(['project.*.agent.>']);
  });
});

describe('StateManager resync on a stale SSE reconnect', () => {
  /** Minimal EventSource stand-in; happy-dom does not implement it. */
  class FakeEventSource extends EventTarget {
    static readonly CONNECTING = 0;
    static readonly OPEN = 1;
    static readonly CLOSED = 2;
    static instances: FakeEventSource[] = [];
    readyState = FakeEventSource.CONNECTING;
    onopen: ((ev: Event) => void) | null = null;
    onerror: ((ev: Event) => void) | null = null;
    constructor(readonly url: string) {
      super();
      FakeEventSource.instances.push(this);
    }
    close(): void {
      this.readyState = FakeEventSource.CLOSED;
    }
    open(): void {
      this.readyState = FakeEventSource.OPEN;
      this.onopen?.(new Event('open'));
    }
  }

  const latest = (): FakeEventSource =>
    FakeEventSource.instances[FakeEventSource.instances.length - 1]!;

  let visibility: DocumentVisibilityState = 'visible';
  const setVisibility = (state: DocumentVisibilityState): void => {
    visibility = state;
    document.dispatchEvent(new Event('visibilitychange'));
  };

  beforeEach(() => {
    vi.useFakeTimers();
    FakeEventSource.instances = [];
    vi.stubGlobal('EventSource', FakeEventSource);
    visibility = 'visible';
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibility,
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
    // Drop the own property so document falls back to its real getter.
    Reflect.deleteProperty(document, 'visibilityState');
  });

  it('raises one agents-resync per stale reconnect and none on first connect', () => {
    const sm = new StateManager();
    const resync = vi.fn();
    sm.addEventListener('agents-resync', resync);

    sm.setScope({ type: 'dashboard' });
    latest().open();
    expect(resync).not.toHaveBeenCalled();

    // Back from a long absence on a silent stream that still reports OPEN.
    setVisibility('hidden');
    vi.advanceTimersByTime(120_000);
    setVisibility('visible');
    expect(FakeEventSource.instances).toHaveLength(2);
    latest().open();
    expect(resync).toHaveBeenCalledTimes(1);

    // The server's own connected event on the same connection adds none.
    latest().dispatchEvent(
      new MessageEvent('connected', { data: JSON.stringify({ connectionId: 'c', subjects: [] }) })
    );
    expect(resync).toHaveBeenCalledTimes(1);

    // A second stale reconnect raises exactly one more.
    setVisibility('hidden');
    vi.advanceTimersByTime(120_000);
    setVisibility('visible');
    latest().open();
    expect(resync).toHaveBeenCalledTimes(2);

    // StateManager has no public teardown; close its client directly so its
    // listeners and timers do not outlive the test.
    (sm as unknown as { sseClient: { disconnect(): void } }).sseClient.disconnect();
  });
});
