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
 * Chat notifications are published on `user.{id}.notification` so that a DM
 * preview reaches only its recipient. The user-scoped chat branch routes
 * anything that is not typing or read-state to `chat-message-received`, so
 * without an explicit case the notification would masquerade as a chat message
 * and the tray would silently stop refreshing.
 */
describe('StateManager user-scoped notification subject', () => {
  const payload = {
    id: 'notif-1',
    status: 'MENTION',
    message: '@Ada mentioned you in #design: have a look',
    subscriberId: 'b',
  };

  it('routes user.{id}.notification to notification-created with its payload', () => {
    const sm = new StateManager();
    const created = vi.fn();
    const message = vi.fn();
    sm.addEventListener('notification-created', created);
    sm.addEventListener('chat-message-received', message);

    emit(sm, 'user.b.notification', payload);

    expect(message).not.toHaveBeenCalled();
    expect(created).toHaveBeenCalledTimes(1);
    const detail = (created.mock.calls[0]?.[0] as CustomEvent).detail as { data: unknown };
    expect(detail.data).toEqual(payload);
  });

  it('still routes the unscoped notification subject to notification-created', () => {
    const sm = new StateManager();
    const created = vi.fn();
    sm.addEventListener('notification-created', created);

    emit(sm, 'notification.created', { id: 'notif-2', status: 'COMPLETED' });

    expect(created).toHaveBeenCalledTimes(1);
  });

  it('subscribes to the per-user notification subject in a non-chat scope', () => {
    const sm = new StateManager();
    const subjectsFor = (scope: Parameters<StateManager['setScope']>[0]): string[] =>
      (sm as unknown as { subjectsForScope(s: unknown): string[] }).subjectsForScope(scope);

    expect(subjectsFor({ type: 'dashboard' })).not.toContain('user.me.notification');

    sm.setCurrentUserId('me');

    expect(subjectsFor({ type: 'dashboard' })).toContain('user.me.notification');
    expect(subjectsFor({ type: 'project', projectId: 'p1' })).toContain('user.me.notification');
    expect(subjectsFor({ type: 'chat', spaceIds: ['p1'], userId: 'me' })).toContain(
      'user.me.notification'
    );
  });

  it('subscribes to the per-user notification subject in chat scope before the user id is set', () => {
    const sm = new StateManager();
    const subjects = (
      sm as unknown as { subjectsForScope(s: unknown): string[] }
    ).subjectsForScope({ type: 'chat', spaceIds: ['p1'], userId: 'me' });

    // `user.me.chat.>` does not match `user.me.notification` — the chat scope
    // needs the notification subject listed in its own right.
    expect(subjects).toContain('user.me.notification');
  });
});
