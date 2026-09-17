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
 * Dismissing a transient chat affordance has to survive whatever happens next.
 *
 * Both bugs covered here were invisible to a test that renders once and asserts:
 * the reply bar only returns when the PARENT re-renders, and the mention
 * dropdown only returns on the NEXT input event. So each test below drives the
 * thing that comes after the dismissal, which is the whole point.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll } from 'vitest';

/* eslint-disable @typescript-eslint/no-explicit-any */

vi.mock('../../../client/api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));
vi.mock('../../../client/main.js', () => ({ navigateTo: vi.fn() }));

beforeAll(async () => {
  await import('./chat-composer.js');
  await import('./mention-autocomplete.js');
  await import('./slash-autocomplete.js');
});

describe('reply/edit bar dismissal (#89)', () => {
  it('asks the parent to clear instead of clearing its own property', async () => {
    const el = document.createElement('scion-chat-composer') as any;
    el.replyTo = { messageId: 'm1', senderName: 'someone', content: 'hello' };
    document.body.appendChild(el);
    await el.updateComplete;

    const events: string[] = [];
    el.addEventListener('chat-cancel-reply', () => events.push('cancel-reply'));

    el.cancelReply();
    await el.updateComplete;

    // The event is what matters: the parent owns replyTo, so a local
    // assignment would be undone on the parent's next render.
    expect(events).toEqual(['cancel-reply']);
    el.remove();
  });

  it('emits cancel-edit and clears only its own local text state', async () => {
    const el = document.createElement('scion-chat-composer') as any;
    el.editMessage = { messageId: 'm1', content: 'draft' };
    el.text = 'draft';
    document.body.appendChild(el);
    await el.updateComplete;

    const events: string[] = [];
    el.addEventListener('chat-cancel-edit', () => events.push('cancel-edit'));

    el.cancelEdit();
    await el.updateComplete;

    expect(events).toEqual(['cancel-edit']);
    expect(el.text).toBe('');
    el.remove();
  });

  it('survives a parent re-render: the bar does not come back', async () => {
    // Model the real ownership — parent state pushed down as a property.
    const parent = { replyTo: { messageId: 'm1', senderName: 's', content: 'c' } as any };
    const el = document.createElement('scion-chat-composer') as any;
    el.addEventListener('chat-cancel-reply', () => {
      parent.replyTo = null;
    });
    el.replyTo = parent.replyTo;
    document.body.appendChild(el);
    await el.updateComplete;

    el.cancelReply();
    await el.updateComplete;

    // The parent re-renders for an unrelated reason and re-applies the binding.
    el.replyTo = parent.replyTo;
    await el.updateComplete;

    expect(el.replyTo).toBeNull();
    el.remove();
  });
});

describe('autocomplete dismissal (#90)', () => {
  function textarea(value: string): HTMLTextAreaElement {
    const ta = document.createElement('textarea');
    ta.value = value;
    document.body.appendChild(ta);
    return ta;
  }

  it('mention: Escape survives the next keystroke', async () => {
    const el = document.createElement('scion-mention-autocomplete') as any;
    el.agents = [{ id: 'a1', name: 'alpha', slug: 'alpha' }];
    el.members = [];
    document.body.appendChild(el);
    await el.updateComplete;

    const ta = textarea('@al');
    el.handleInput('@al', 3, ta);
    await el.updateComplete;
    expect(el.active).toBe(true);

    el.handleKeydown(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;
    expect(el.active).toBe(false);

    // The keystroke that used to bring it straight back.
    ta.value = '@alp';
    el.handleInput('@alp', 4, ta);
    await el.updateComplete;
    expect(el.active).toBe(false);

    ta.remove();
    el.remove();
  });

  it('mention: a fresh trigger still opens after a dismissal', async () => {
    const el = document.createElement('scion-mention-autocomplete') as any;
    el.agents = [{ id: 'a1', name: 'alpha', slug: 'alpha' }];
    el.members = [];
    document.body.appendChild(el);
    await el.updateComplete;

    const ta = textarea('@al');
    el.handleInput('@al', 3, ta);
    el.handleKeydown(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;

    // Trigger removed, then a new one started further along.
    ta.value = 'hi ';
    el.handleInput('hi ', 3, ta);
    ta.value = 'hi @al';
    el.handleInput('hi @al', 6, ta);
    await el.updateComplete;

    expect(el.active).toBe(true);
    ta.remove();
    el.remove();
  });

  it('mention: backspacing past dismissed query reopens the dropdown', async () => {
    const el = document.createElement('scion-mention-autocomplete') as any;
    el.agents = [{ id: 'a1', name: 'alpha', slug: 'alpha' }];
    el.members = [];
    document.body.appendChild(el);
    await el.updateComplete;

    const ta = textarea('@alp');
    el.handleInput('@alp', 4, ta);
    await el.updateComplete;
    expect(el.active).toBe(true);

    el.handleKeydown(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;
    expect(el.active).toBe(false);

    // Backspace: query is now 'a', which does NOT start with 'alp'.
    ta.value = '@a';
    el.handleInput('@a', 2, ta);
    await el.updateComplete;
    expect(el.active).toBe(true);

    ta.remove();
    el.remove();
  });

  it('slash: Escape survives the next keystroke', async () => {
    const el = document.createElement('scion-slash-autocomplete') as any;
    document.body.appendChild(el);
    await el.updateComplete;

    el.handleInput('/', 1);
    await el.updateComplete;
    expect(el.active).toBe(true);

    el.handleKeydown(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;
    expect(el.active).toBe(false);

    el.handleInput('/h', 2);
    await el.updateComplete;
    expect(el.active).toBe(false);

    el.remove();
  });

  it('slash: typing a different command after dismissal reopens', async () => {
    const el = document.createElement('scion-slash-autocomplete') as any;
    document.body.appendChild(el);
    await el.updateComplete;

    // Dismiss while typing /he (matches /help)
    el.handleInput('/he', 3);
    await el.updateComplete;
    expect(el.active).toBe(true);

    el.handleKeydown(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;
    expect(el.active).toBe(false);

    // Clear and type a different command — should reopen.
    el.handleInput('/st', 3);
    await el.updateComplete;
    expect(el.active).toBe(true);

    el.remove();
  });

  it('slash: backspacing past dismissed prefix reopens', async () => {
    const el = document.createElement('scion-slash-autocomplete') as any;
    document.body.appendChild(el);
    await el.updateComplete;

    el.handleInput('/hel', 4);
    await el.updateComplete;
    expect(el.active).toBe(true);

    el.handleKeydown(new KeyboardEvent('keydown', { key: 'Escape' }));
    await el.updateComplete;
    expect(el.active).toBe(false);

    // Backspace: prefix is now 'h', does NOT start with 'hel'.
    el.handleInput('/h', 2);
    await el.updateComplete;
    expect(el.active).toBe(true);

    el.remove();
  });
});
