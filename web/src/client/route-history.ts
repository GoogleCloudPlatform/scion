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
 * History helpers for the router in `main.ts`, kept apart from it because
 * importing `main.ts` boots the whole app — this part can be unit tested.
 */

/** The active shell element as the router sees it. */
export type RouteShell = HTMLElement & {
  currentPath: string;
  updateComplete?: Promise<unknown>;
};

/**
 * Push a history entry for `appPath` (shown as `browserUrl`) without
 * rendering, and record it as the shell's rendered path, as a render would.
 * Resolves once the shell has re-rendered for it.
 */
export function pushRouteEntry(
  shell: RouteShell | undefined,
  appPath: string,
  browserUrl: string
): Promise<void> {
  window.history.pushState({}, '', browserUrl);
  if (!shell) return Promise.resolve();
  shell.currentPath = appPath;
  return Promise.resolve(shell.updateComplete).then(() => undefined);
}
