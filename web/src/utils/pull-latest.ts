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

/** Successful response body of POST /api/v1/projects/{id}/workspace/pull. */
export interface PullLatestResponse {
  updated?: boolean;
  commits?: { hash: string; subject: string }[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function nonEmptyString(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/**
 * Builds the readable failure text for a failed Pull latest request.
 *
 * Accepts the structured APIError shape ({ error: { message, details } })
 * and the legacy shapes ({ detail } or { error: string }). The result is
 * always a string: the error object's message, then detail, then a
 * string error, then "Pull failed". A guidance hint from the structured
 * error is appended when present.
 */
export function pullLatestErrorMessage(body: unknown): string {
  const result = isRecord(body) ? body : {};
  const apiErr = isRecord(result.error) ? result.error : undefined;

  let message =
    nonEmptyString(apiErr?.message) ??
    nonEmptyString(result.detail) ??
    nonEmptyString(result.error) ??
    'Pull failed';

  const details = isRecord(apiErr?.details) ? apiErr.details : undefined;
  const guidance = nonEmptyString(details?.guidance);
  if (guidance) {
    message += ` — ${guidance}`;
  }
  return message;
}
