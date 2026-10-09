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

/** The URL string a fetch mock was called with (string, URL or Request input). */
export function requestUrl(input: RequestInfo | URL): string {
  if (typeof input === 'string') return input;
  return input instanceof URL ? input.href : input.url;
}

/**
 * The text of a fetch mock's request body. Matches `String(body)` for the
 * body kinds the app sends (strings and URLSearchParams) and for a missing
 * body; any other kind is not expected in these tests and throws.
 */
export function requestBodyText(body: BodyInit | null | undefined): string {
  if (typeof body === 'string') return body;
  if (body === null || body === undefined) return String(body);
  if (body instanceof URLSearchParams) return body.toString();
  throw new TypeError('unsupported request body kind');
}
