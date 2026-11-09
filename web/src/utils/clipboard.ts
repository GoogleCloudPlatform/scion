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
 * Guarded Clipboard API helpers shared by every copy action in the web UI.
 *
 * None of them throws: the Clipboard API is absent outside a secure context (a
 * plain-http host that is not localhost) and in some older browsers, and its
 * writes reject when the browser denies them. Each of those resolves to
 * `false`, so a caller can show feedback instead of failing silently.
 */

import { showToast } from './toast.js';

function clipboardApi(): Partial<Clipboard> | undefined {
  return (navigator as Partial<Navigator>).clipboard;
}

/** Writes `text` to the clipboard and resolves to whether it succeeded. */
export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    const clipboard = clipboardApi();
    if (typeof clipboard?.writeText !== 'function') return false;
    await clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

/**
 * Writes one clipboard item holding each MIME type in `parts` (for example
 * `text/html` and `text/plain`) and resolves to whether it succeeded. Resolves
 * `false` when `clipboard.write` or `ClipboardItem` is unavailable.
 */
export async function copyRichToClipboard(parts: Record<string, string>): Promise<boolean> {
  try {
    const clipboard = clipboardApi();
    if (typeof clipboard?.write !== 'function' || typeof ClipboardItem !== 'function') {
      return false;
    }
    const blobs: Record<string, Blob> = {};
    for (const [type, value] of Object.entries(parts)) {
      blobs[type] = new Blob([value], { type });
    }
    await clipboard.write([new ClipboardItem(blobs)]);
    return true;
  } catch {
    return false;
  }
}

/**
 * Copies `text` and reports the outcome in a toast: "Copied" on success,
 * "Could not copy" on failure. Resolves to whether the copy succeeded.
 */
export async function copyWithToast(text: string): Promise<boolean> {
  const ok = await copyToClipboard(text);
  if (ok) showToast('Copied', 'success');
  else showCopyFailedToast();
  return ok;
}

/**
 * Shows the shared "Could not copy" toast. For copy actions that already show
 * their own success feedback (a "Copied" label or a check icon) and only need
 * to report a failure.
 */
export function showCopyFailedToast(): void {
  showToast('Could not copy', 'danger');
}
