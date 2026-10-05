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
 * Tests for seeding chat composer drafts from outside the chat page. The
 * composer test pins the storage key to the one the composer restores, so
 * the two cannot drift apart silently.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';
import { chatDraftStorageKey, seedChatDraft } from './chat-drafts.js';

vi.mock('./api.js', () => ({
  apiFetch: vi.fn(() => Promise.resolve(new Response('{}', { status: 200 }))),
}));
vi.mock('../utils/toast.js', () => ({ showToast: vi.fn() }));

const KEY = 'dm:agent:a1:user:u1';

beforeAll(async () => {
  await import('../components/shared/chat/chat-composer.js');
});

beforeEach(() => localStorage.clear());
afterEach(() => {
  localStorage.clear();
  document.body.innerHTML = '';
});

describe('seedChatDraft', () => {
  it('stores text under the conversation draft key', () => {
    expect(seedChatDraft(KEY, 'hello')).toBe(true);
    expect(localStorage.getItem(chatDraftStorageKey(KEY))).toBe('hello');
  });

  it('appends to an existing draft on a new paragraph', () => {
    localStorage.setItem(chatDraftStorageKey(KEY), 'first\n');
    seedChatDraft(KEY, 'second');
    expect(localStorage.getItem(chatDraftStorageKey(KEY))).toBe('first\n\nsecond');
  });

  it('ignores empty text and a missing key', () => {
    expect(seedChatDraft(KEY, '   ')).toBe(false);
    expect(seedChatDraft('', 'hello')).toBe(false);
    expect(localStorage.length).toBe(0);
  });

  it('is restored by the chat composer for that conversation', async () => {
    seedChatDraft(KEY, 'carried over');
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const el = document.createElement('scion-chat-composer') as any;
    el.conversationMode = true;
    el.conversationKey = KEY;
    document.body.appendChild(el);
    await el.updateComplete;
    expect(el.text).toBe('carried over');
  });

  it('survives an empty composer for that conversation unmounting', async () => {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const el = document.createElement('scion-chat-composer') as any;
    el.conversationMode = true;
    el.conversationKey = KEY;
    document.body.appendChild(el);
    await el.updateComplete;
    seedChatDraft(KEY, 'handed over');
    el.remove();
    expect(localStorage.getItem(chatDraftStorageKey(KEY))).toBe('handed over');
  });

  it('is still removed when the user clears the composer before unmount', async () => {
    seedChatDraft(KEY, 'to be cleared');
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    const el = document.createElement('scion-chat-composer') as any;
    el.conversationMode = true;
    el.conversationKey = KEY;
    document.body.appendChild(el);
    await el.updateComplete;
    const textarea = el.shadowRoot.querySelector('sl-textarea') as HTMLTextAreaElement;
    expect(textarea.value).toBe('to be cleared');
    textarea.value = '';
    textarea.dispatchEvent(new Event('sl-input', { bubbles: true, composed: true }));
    el.remove();
    expect(localStorage.getItem(chatDraftStorageKey(KEY))).toBeNull();
  });
});
