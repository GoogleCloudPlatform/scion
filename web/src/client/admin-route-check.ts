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
 * Lightweight registry for checking whether a pathname maps to an admin route.
 *
 * Kept separate from `view-as-member.ts` so `main.ts` can register its
 * `ADMIN_ROUTES` predicate at boot without pulling the toggle client into the
 * initial entry bundle.
 */

export type AdminPathChecker = (pathname: string) => boolean;

let adminPathChecker: AdminPathChecker | null = null;

/** Register the router-backed predicate for `ADMIN_ROUTES` paths. */
export function setAdminPathChecker(checker: AdminPathChecker | null): void {
  adminPathChecker = checker;
}

/**
 * Whether an application pathname (base-path stripped) maps to a route in
 * `ADMIN_ROUTES`. Delegates to the router predicate registered by `main.ts`.
 */
export function isAdminRoutePath(pathname: string): boolean {
  const clean = pathname.split(/[?#]/, 1)[0] || '/';
  return adminPathChecker ? adminPathChecker(clean) : false;
}
