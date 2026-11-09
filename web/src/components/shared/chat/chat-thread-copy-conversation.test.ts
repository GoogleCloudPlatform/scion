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
 * The thread's "Copy conversation" action (copyAsFormattedText): a formatted
 * HTML + plain-text copy, falling back to plain text (ptone/scion#7554), and
 * guarded when the Clipboard API is unavailable (ptone/scion#7555).
 */

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';

const fakeStateManager = new (class extends EventTarget {
  currentScope = null;
  getAgent(): undefined {
    return undefined;
  }
})();
const apiFetch = vi.fn();

vi.mock('../../../client/main.js', async () => ({
  ...(await import('../../../client/__fixtures__/main-stub.js')),
  get navigateTo(): ReturnType<typeof vi.fn> {
    return vi.fn();
  },
  get stateManager(): typeof fakeStateManager {
    return fakeStateManager;
  },
}));

vi.mock('../../../client/api.js', () => ({
  apiFetch: (...args: unknown[]): unknown => apiFetch(...args) as unknown,
  extractApiError: (_res: unknown, fallback: string): Promise<string> => Promise.resolve(fallback),
}));

vi.mock('../../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./chat-thread.js');
const { showToast } = await import('../../../utils/toast.js');
type ScionChatThread = import('./chat-thread.js').ScionChatThread;

const MESSAGES = [
  { id: 'm1', sender: 'agent:fixture-bot', msg: 'first fixture message' },
  { id: 'm2', sender: 'user:fixture-user', msg: 'second fixture message' },
].map((m, i) => ({ ...m, createdAt: `2026-01-01T00:0${i}:00Z` }));

/** A minimal ClipboardItem stand-in; jsdom does not provide one. */
class FakeClipboardItem {
  constructor(readonly items: Record<string, Blob>) {}
}

const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
const originalClipboardItem = Object.getOwnPropertyDescriptor(globalThis, 'ClipboardItem');

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', { value, configurable: true });
}

beforeEach(() => {
  vi.mocked(showToast).mockClear();
  apiFetch.mockReset();
  apiFetch.mockResolvedValue({
    ok: true,
    status: 200,
    json: () => Promise.resolve({ messages: MESSAGES }),
  } as unknown as Response);
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (query: string) =>
      ({
        matches: false,
        media: query,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
      }) as unknown as MediaQueryList
  );
  Object.defineProperty(globalThis, 'ClipboardItem', {
    value: FakeClipboardItem,
    configurable: true,
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
  if (originalClipboardItem)
    Object.defineProperty(globalThis, 'ClipboardItem', originalClipboardItem);
  else delete (globalThis as unknown as Record<string, unknown>).ClipboardItem;
  document.body.innerHTML = '';
});

async function mount(): Promise<ScionChatThread> {
  const el = document.createElement('scion-chat-thread') as ScionChatThread;
  el.conversationKey = 'topic-1';
  document.body.appendChild(el);
  await vi.waitFor(() => {
    expect(el.shadowRoot?.querySelector('#msg-m2')).not.toBeNull();
  });
  vi.mocked(showToast).mockClear();
  return el;
}

describe('Copy conversation', () => {
  it('writes HTML and plain text in one item and shows the formatted success toast', async () => {
    const write = vi.fn().mockResolvedValue(undefined);
    const writeText = vi.fn().mockResolvedValue(undefined);
    setClipboard({ write, writeText });
    const el = await mount();
    await el.copyAsFormattedText();
    expect(write).toHaveBeenCalledTimes(1);
    const [items] = write.mock.calls[0] as [FakeClipboardItem[]];
    expect(Object.keys(items[0].items)).toEqual(['text/html', 'text/plain']);
    expect(writeText).not.toHaveBeenCalled();
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith('Conversation copied to clipboard.', 'success');
  });

  it('falls back to plain text when the formatted copy is rejected, and shows the plain-text success toast', async () => {
    const write = vi.fn().mockRejectedValue(new Error('denied'));
    const writeText = vi.fn().mockResolvedValue(undefined);
    setClipboard({ write, writeText });
    const el = await mount();
    await el.copyAsFormattedText();
    expect(write).toHaveBeenCalledTimes(1);
    expect(writeText).toHaveBeenCalledTimes(1);
    const plain = writeText.mock.calls[0][0] as string;
    expect(plain).toContain('agent:fixture-bot');
    expect(plain).toContain('second fixture message');
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith(
      'Conversation copied to clipboard (plain text).',
      'success'
    );
  });

  it('shows exactly one "Failed to copy to clipboard." toast, without throwing, when the Clipboard API is unavailable', async () => {
    setClipboard(undefined);
    const el = await mount();
    await expect(el.copyAsFormattedText()).resolves.toBeUndefined();
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith('Failed to copy to clipboard.', 'danger');
  });
});
