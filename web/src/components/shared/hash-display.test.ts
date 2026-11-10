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
 * The hash copy button: a check icon on success, the shared "Could not copy"
 * toast when the clipboard is unavailable (ptone/scion#7555). It stands for
 * the copy actions that keep their own success indicator and add only the
 * failure toast.
 */

import { describe, it, expect, afterEach, beforeEach, vi } from 'vitest';

vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

await import('./hash-display.js');
const { showToast } = await import('../../utils/toast.js');
type ScionHashDisplay = import('./hash-display.js').ScionHashDisplay;

const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');

function setClipboard(value: unknown): void {
  Object.defineProperty(navigator, 'clipboard', { value, configurable: true });
}

beforeEach(() => {
  vi.mocked(showToast).mockClear();
});

afterEach(() => {
  if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
  else delete (navigator as unknown as Record<string, unknown>).clipboard;
  document.body.innerHTML = '';
});

async function mount(): Promise<ScionHashDisplay> {
  const el = document.createElement('scion-hash-display');
  el.hash = 'sha256:0123456789abcdef';
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function icon(el: ScionHashDisplay): string | null {
  return el.shadowRoot!.querySelector('.copy-btn sl-icon')!.getAttribute('name');
}

describe('hash-display copy', () => {
  it('copies the hash and shows the check icon, with no toast', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    setClipboard({ writeText });
    const el = await mount();
    el.shadowRoot!.querySelector<HTMLButtonElement>('.copy-btn')!.click();
    await vi.waitFor(() => expect(icon(el)).toBe('clipboard-check'));
    expect(writeText).toHaveBeenCalledWith('sha256:0123456789abcdef');
    expect(showToast).not.toHaveBeenCalled();
  });

  it('shows "Could not copy", without throwing, when the clipboard is unavailable', async () => {
    setClipboard(undefined);
    const el = await mount();
    expect(() =>
      el.shadowRoot!.querySelector<HTMLButtonElement>('.copy-btn')!.click()
    ).not.toThrow();
    await vi.waitFor(() => expect(showToast).toHaveBeenCalledWith('Could not copy', 'danger'));
    expect(showToast).toHaveBeenCalledTimes(1);
    await el.updateComplete;
    expect(icon(el)).toBe('clipboard');
  });
});
