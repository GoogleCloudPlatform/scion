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
 * Writes `text` to the clipboard and resolves to whether it succeeded.
 *
 * Never throws: the Clipboard API is absent outside a secure context (a
 * plain-http host) and in some older browsers, and `writeText` rejects when
 * the browser denies the write. Each of those resolves to `false`.
 */
export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    const clipboard = (navigator as Partial<Navigator>).clipboard;
    if (typeof clipboard?.writeText !== 'function') return false;
    await clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}
