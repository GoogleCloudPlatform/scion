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

// @vitest-environment happy-dom

import { describe, it, expect, vi } from 'vitest';
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

  it('no longer routes the retired per-user notification subject', () => {
    const sm = new StateManager();
    const created = vi.fn();
    const message = vi.fn();
    sm.addEventListener('notification-created', created);
    sm.addEventListener('chat-message-received', message);

    emit(sm, 'user.b.notification', { id: 'n1', status: 'MENTION' });

    expect(created).not.toHaveBeenCalled();
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

  it('subscribes to the own chat subject in every view scope', () => {
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
      expect(subjects).not.toContain('user.me.notification');
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
