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

import { describe, it, expect, afterEach, vi } from 'vitest';
import { copyToClipboard } from './clipboard.js';

const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', { value, configurable: true });
}

afterEach(() => {
  if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
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
