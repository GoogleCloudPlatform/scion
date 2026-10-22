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
 * Helpers for admin settings forms that send only what the user changed.
 *
 * A settings PUT keeps every key the body omits (ptone/scion#3913), so a
 * form compares its current values with a snapshot taken when it loaded
 * and sends only the difference. Defaults a form shows for keys the GET
 * left out are therefore never written unless the user edits them.
 */

/**
 * Returns the leaves of current that differ from base (plain objects are
 * compared key by key; arrays and scalars as whole values).
 *
 * Only keys present in current are reported: a key that is in base but not
 * in current is not reported as a deletion. Callers pass objects of a fixed
 * shape (every key always present, a cleared value as "", [], false or
 * null), so a clear is a changed value, not a missing key.
 */
export function diffPayload(
  current: Record<string, unknown>,
  base: Record<string, unknown>
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(current)) {
    const b = base[k];
    if (
      v &&
      typeof v === 'object' &&
      !Array.isArray(v) &&
      b &&
      typeof b === 'object' &&
      !Array.isArray(b)
    ) {
      const sub = diffPayload(v as Record<string, unknown>, b as Record<string, unknown>);
      if (Object.keys(sub).length > 0) out[k] = sub;
    } else if (JSON.stringify(v) !== JSON.stringify(b)) {
      out[k] = v;
    }
  }
  return out;
}
