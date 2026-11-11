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
 * Pure decision logic for `client/main.ts`'s recent-files identity/teardown
 * wiring, extracted so it can be unit tested without loading `main.ts`'s
 * full app-bootstrap module graph (which has no test file at all, and
 * self-initializes on import).
 */

import type { AccountTeardownDetail } from '../utils/auth.js';
import type { RecentFilesScope } from './chat-recent-files.js';

/**
 * Explicit logout only: an auth-expiry teardown may resume the same account
 * after re-auth, so it must not clear this account's recent-files index.
 */
export function shouldClearRecentFilesOnTeardown(
  reason: AccountTeardownDetail['reason'] | undefined
): boolean {
  return reason === 'logout';
}

/** The identity chatRecentFiles.setScope() needs, built once the user is known. */
export function buildRecentFilesScope(
  user: { id: string },
  origin: string,
  baseUrl: string
): RecentFilesScope {
  return { origin, baseUrl, userId: user.id };
}
