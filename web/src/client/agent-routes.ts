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

/** Retired agent page URLs and where they now point. */

/** The retired Configure page (ptone/scion#3975); the Edit page replaced it. */
const legacyConfigureRoute = /^\/agents\/([^/]+)\/configure$/;

/**
 * The path a retired agent URL redirects to, or null for any other path:
 * /agents/{id}/configure goes to /agents/{id}/edit.
 */
export function legacyAgentRedirect(pathname: string): string | null {
  const id = pathname.match(legacyConfigureRoute)?.[1];
  return id ? `/agents/${id}/edit` : null;
}
