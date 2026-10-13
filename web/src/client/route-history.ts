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
 * `state` becomes the entry's history state. Resolves once the shell has
 * re-rendered for it.
 */
export function pushRouteEntry(
  shell: RouteShell | undefined,
  appPath: string,
  browserUrl: string,
  state: Record<string, unknown> = {}
): Promise<void> {
  window.history.pushState(state, '', browserUrl);
  if (!shell) return Promise.resolve();
  shell.currentPath = appPath;
  return Promise.resolve(shell.updateComplete).then(() => undefined);
}

/**
 * Key in `history.state` marking an entry a page pushed for its own in-page
 * state (e.g. which chat panel is on screen on a phone) on the URL it is
 * already showing. The page restores that state itself on `popstate`.
 */
export const IN_PAGE_STATE_KEY = 'scionInPage';

/** Does this history state carry an in-page marker? */
export function hasInPageState(state: unknown): boolean {
  return (
    typeof state === 'object' &&
    state !== null &&
    typeof (state as Record<string, unknown>)[IN_PAGE_STATE_KEY] === 'object' &&
    (state as Record<string, unknown>)[IN_PAGE_STATE_KEY] !== null
  );
}

/**
 * Should a `popstate` to `appPath` (path plus query) be left to the page on
 * screen instead of rendering the route again? Only for an in-page entry of
 * the path the shell already shows, with the route outlet visible: coming
 * back from the terminal workspace still goes through a render, which
 * reuses the hidden page. The fragment is ignored, as for the terminal
 * return.
 */
export function isInPagePop(
  state: unknown,
  appPath: string,
  shell: RouteShell | undefined,
  outletHidden: boolean
): boolean {
  return (
    !outletHidden &&
    !!shell &&
    hasInPageState(state) &&
    shell.currentPath.split('#')[0] === appPath.split('#')[0]
  );
}
