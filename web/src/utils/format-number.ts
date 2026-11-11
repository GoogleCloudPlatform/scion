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
 * Locale-aware number formatting (tz-refactor task 11, design.md §2.4
 * "Enforcement").
 *
 * The format-scan test cannot tell `Number.prototype`'s locale-string method
 * from the unrelated `Date.prototype` one it bans, so every numeric call
 * site routes through here instead, pinned to the same `FORMAT_LOCALE` as
 * `web/src/utils/time.ts`. `Intl.NumberFormat` itself is not a banned token,
 * so this file needs no scan exemption.
 */

import { FORMAT_LOCALE } from './time.js';

/** Formats `n` with `FORMAT_LOCALE`'s grouping and decimal conventions. */
export function formatNumber(n: number): string {
  return new Intl.NumberFormat(FORMAT_LOCALE).format(n);
}
