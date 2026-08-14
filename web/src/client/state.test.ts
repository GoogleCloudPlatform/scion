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
