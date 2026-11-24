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
 * Client helper for the "View as member" browser-session toggle
 * (ptone/scion#8648, design §4.8).
 *
 * Posts to POST /auth/view-as-member, clears the cached admin-status, and
 * triggers a full page navigation so SSR data, feature flags, admin nav
 * guards, and the SSE stream all reset cleanly in one step.
 */

import { apiFetch, extractApiError } from './api.js';
import { clearAdminStatus } from './admin-status.js';
import { loadPage, stripBasePath } from './navigation.js';
import {
  isAdminRoutePath,
  setAdminPathChecker,
  type AdminPathChecker,
} from './admin-route-check.js';

export { isAdminRoutePath, setAdminPathChecker, type AdminPathChecker };

export interface ToggleViewAsMemberOptions {
  /** Override for full-page navigation (e.g. to `/` when leaving an admin route). */
  navigate?: (target: string) => void;
  /** Override for full-page reload of the current URL. */
  reload?: () => void;
  /** Override for current pathname (base-path stripped) in tests. */
  currentPathname?: string;
  /** Override for the admin-route predicate in tests. */
  isAdminPath?: AdminPathChecker;
}

/**
 * Enter or exit "View as member" mode for the current browser session.
 *
 * On success, clears the shared admin-status cache and performs a full
 * navigation:
 * - entering from an `ADMIN_ROUTES` page -> `/`
 * - entering from any other page, or exiting -> reload current URL
 */
export async function toggleViewAsMember(
  enabled: boolean,
  options: ToggleViewAsMemberOptions = {}
): Promise<boolean> {
  const res = await apiFetch('/auth/view-as-member', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled }),
    authErrors: 'caller',
  });

  if (!res.ok) {
    throw new Error(
      await extractApiError(
        res,
        enabled
          ? `Failed to enter "View as member" mode (HTTP ${res.status})`
          : `Failed to exit "View as member" mode (HTTP ${res.status})`
      )
    );
  }

  clearAdminStatus();

  const navigate = options.navigate ?? loadPage;
  const reload = options.reload ?? ((): void => window.location.reload());
  const pathname =
    options.currentPathname ??
    (typeof window !== 'undefined' ? stripBasePath(window.location.pathname) : '/');
  const checkAdminPath = options.isAdminPath ?? isAdminRoutePath;

  if (enabled && checkAdminPath(pathname)) {
    navigate('/');
  } else {
    reload();
  }

  return true;
}
