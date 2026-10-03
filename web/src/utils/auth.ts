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
 * Shared authentication utilities.
 *
 * Extracted from the individual shell components so all shells use the same
 * logout logic (design doc Section 4.1: "fix in place and share").
 */

/**
 * Custom event dispatched on `window` before the logout POST begins.
 * Listeners must run synchronously or queue microtask-level cleanup;
 * the redirect follows as soon as the POST resolves.
 */
export const ACCOUNT_TEARDOWN_EVENT = 'scion:account-teardown';

export interface AccountTeardownDetail {
  /** Reason the teardown was triggered. */
  readonly reason: 'logout' | 'auth-expired';
}

/**
 * Perform a logout by POSTing to the auth endpoint and redirecting to login.
 * Uses the Vite BASE_URL to construct correct paths behind a reverse proxy.
 *
 * Before the POST, dispatches a `scion:account-teardown` event so that
 * cross-tab terminal coordinators and other subsystems can dispose resources
 * while the document is still alive.
 */
export function performLogout(): void {
  dispatchTeardown('logout');
  const base = (import.meta.env?.BASE_URL || '/').replace(/\/$/, '');
  fetch(`${base}/auth/logout`, {
    method: 'POST',
    credentials: 'include',
  })
    .then(() => {
      window.location.href = `${base}/auth/login`;
    })
    .catch((error) => {
      console.error('Logout failed:', error);
    });
}

/**
 * Dispatch the teardown event for external callers (e.g. auth-expiry detection).
 * Does not trigger logout itself — the caller is responsible for any redirect.
 */
export function dispatchTeardown(reason: AccountTeardownDetail['reason']): void {
  window.dispatchEvent(
    new CustomEvent<AccountTeardownDetail>(ACCOUNT_TEARDOWN_EVENT, {
      detail: { reason },
    })
  );
}
