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
 * Shared duration-limit helpers for the agent Edit and Detail pages
 * (ptone/scion#7594).
 *
 * Mirrors Go's `time.ParseDuration` (used by `api.ParseDuration` in
 * `pkg/api/types.go` and `maxDurationLimit` in `cmd/sciontool/commands/init.go`)
 * so every zero-duration spelling the hub treats as zero ("0", "0s", "0m",
 * "0h", "0ms", "0us", "0µs", "0μs", "0ns", composite forms like "0h0m0s", and
 * signed/decimal zero forms) is consistently treated as Unlimited.
 */

/**
 * Matches every valid Go `time.ParseDuration` string whose numeric value is 0:
 * - Optional leading sign (`+` or `-`)
 * - Either bare `"0"` (the only unitless spelling Go accepts)
 * - Or one or more `<zero-number><unit>` segments using Go's supported units
 *   (`ns`, `us`, `µs` U+00B5, `μs` U+03BC, `ms`, `s`, `m`, `h`), where each
 *   number has at least one `0` digit and no non-zero digits.
 */
const ZERO_DURATION_RE = /^[+-]?(?:0|(?:(?:0+(?:\.0*)?|\.0+)(?:ns|us|\u00b5s|\u03bcs|ms|s|m|h))+)$/;

/**
 * Reports whether `value` is a Go duration string representing zero duration
 * (Unlimited for agent duration limits). Empty strings, non-strings, malformed
 * strings, and non-zero durations return `false`.
 */
export function isZeroDurationLimit(value: unknown): boolean {
  return typeof value === 'string' && ZERO_DURATION_RE.test(value);
}
