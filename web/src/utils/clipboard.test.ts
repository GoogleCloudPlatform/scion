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

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';

vi.mock('./toast.js', () => ({ showToast: vi.fn() }));

const { copyToClipboard, copyRichToClipboard, copyWithToast } = await import('./clipboard.js');
const { showToast } = await import('./toast.js');

const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
const originalClipboardItem = Object.getOwnPropertyDescriptor(globalThis, 'ClipboardItem');

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', { value, configurable: true });
}

/** A minimal ClipboardItem stand-in; jsdom does not provide one. */
class FakeClipboardItem {
  constructor(readonly items: Record<string, Blob>) {}
}

beforeEach(() => {
  vi.mocked(showToast).mockClear();
});

afterEach(() => {
  if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
  if (originalClipboardItem)
    Object.defineProperty(globalThis, 'ClipboardItem', originalClipboardItem);
  else delete (globalThis as unknown as Record<string, unknown>).ClipboardItem;
});

describe('copyToClipboard', () => {
  it('writes the text and resolves true when the Clipboard API is present', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    setClipboard({ writeText });
    await expect(copyToClipboard('hello')).resolves.toBe(true);
    expect(writeText).toHaveBeenCalledWith('hello');
  });

  it('resolves false, without throwing, when the Clipboard API is absent', async () => {
    setClipboard(undefined);
    let result: Promise<boolean> | undefined;
    expect(() => {
      result = copyToClipboard('hello');
    }).not.toThrow();
    await expect(result).resolves.toBe(false);
  });

  it('resolves false when the clipboard has no writeText', async () => {
    setClipboard({});
    await expect(copyToClipboard('hello')).resolves.toBe(false);
  });

  it('resolves false when the write is rejected', async () => {
    setClipboard({ writeText: vi.fn().mockRejectedValue(new Error('denied')) });
    await expect(copyToClipboard('hello')).resolves.toBe(false);
  });

  it('resolves false when writeText throws synchronously', async () => {
    setClipboard({
      writeText: () => {
        throw new TypeError('boom');
      },
    });
    await expect(copyToClipboard('hello')).resolves.toBe(false);
  });
});

describe('copyRichToClipboard', () => {
  const parts = { 'text/html': '<p>hi</p>', 'text/plain': 'hi' };

  it('writes one item holding every MIME type and resolves true', async () => {
    Object.defineProperty(globalThis, 'ClipboardItem', {
      value: FakeClipboardItem,
      configurable: true,
    });
    const write = vi.fn().mockResolvedValue(undefined);
    setClipboard({ write });
    await expect(copyRichToClipboard(parts)).resolves.toBe(true);
    expect(write).toHaveBeenCalledTimes(1);
    const [items] = write.mock.calls[0] as [FakeClipboardItem[]];
    expect(items).toHaveLength(1);
    expect(Object.keys(items[0].items)).toEqual(['text/html', 'text/plain']);
    expect(items[0].items['text/html'].type).toBe('text/html');
  });

  it('resolves false, without throwing, when the Clipboard API is absent', async () => {
    Object.defineProperty(globalThis, 'ClipboardItem', {
      value: FakeClipboardItem,
      configurable: true,
    });
    setClipboard(undefined);
    let result: Promise<boolean> | undefined;
    expect(() => {
      result = copyRichToClipboard(parts);
    }).not.toThrow();
    await expect(result).resolves.toBe(false);
  });

  it('resolves false when ClipboardItem is unavailable', async () => {
    delete (globalThis as unknown as Record<string, unknown>).ClipboardItem;
    const write = vi.fn().mockResolvedValue(undefined);
    setClipboard({ write });
    await expect(copyRichToClipboard(parts)).resolves.toBe(false);
    expect(write).not.toHaveBeenCalled();
  });

  it('resolves false when the clipboard has no write', async () => {
    Object.defineProperty(globalThis, 'ClipboardItem', {
      value: FakeClipboardItem,
      configurable: true,
    });
    setClipboard({ writeText: vi.fn() });
    await expect(copyRichToClipboard(parts)).resolves.toBe(false);
  });

  it('resolves false when the write is rejected', async () => {
    Object.defineProperty(globalThis, 'ClipboardItem', {
      value: FakeClipboardItem,
      configurable: true,
    });
    setClipboard({ write: vi.fn().mockRejectedValue(new Error('denied')) });
    await expect(copyRichToClipboard(parts)).resolves.toBe(false);
  });
});

describe('copyWithToast', () => {
  it('shows "Copied" and resolves true when the write succeeds', async () => {
    setClipboard({ writeText: vi.fn().mockResolvedValue(undefined) });
    await expect(copyWithToast('hello')).resolves.toBe(true);
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith('Copied', 'success');
  });

  it('shows "Could not copy", without throwing, when the Clipboard API is absent', async () => {
    setClipboard(undefined);
    await expect(copyWithToast('hello')).resolves.toBe(false);
    expect(showToast).toHaveBeenCalledTimes(1);
    expect(showToast).toHaveBeenCalledWith('Could not copy', 'danger');
  });

  it('shows "Could not copy" when the write is rejected', async () => {
    setClipboard({ writeText: vi.fn().mockRejectedValue(new Error('denied')) });
    await expect(copyWithToast('hello')).resolves.toBe(false);
    expect(showToast).toHaveBeenCalledWith('Could not copy', 'danger');
  });
});
