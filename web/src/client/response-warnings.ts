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
 * Shows the `warnings` a hub response carries, such as the create and start
 * warning that an agent's token was issued without the artifact scopes
 * (ptone/scion#4569). The CLI prints these; without this the web dropped
 * them.
 */

import { showToast } from '../utils/toast.js';

/** The non-empty string entries of `body.warnings`, de-duplicated. */
export function responseWarnings(body: unknown): string[] {
  if (typeof body !== 'object' || body === null) return [];
  const list = (body as { warnings?: unknown }).warnings;
  if (!Array.isArray(list)) return [];
  const out: string[] = [];
  for (const w of list) {
    if (typeof w === 'string' && w.trim() !== '' && !out.includes(w)) out.push(w);
  }
  return out;
}

/**
 * Shows each warning of `body` as a warning toast that stays until closed
 * (they are actionable and often long), and returns them.
 */
export function showResponseWarnings(body: unknown): string[] {
  const warnings = responseWarnings(body);
  for (const w of warnings) {
    showToast(w, 'warning', { duration: Infinity });
  }
  return warnings;
}

/**
 * Reads the JSON body of a successful `response` and shows its warnings. A
 * body that is missing or not JSON shows nothing. It consumes the body.
 */
export async function showResponseBodyWarnings(response: Response): Promise<string[]> {
  let body: unknown;
  try {
    body = await response.json();
  } catch {
    return [];
  }
  return showResponseWarnings(body);
}
