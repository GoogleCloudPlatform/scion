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
 * Single-flight loader for the terminal workspace chunk (ptone/scion#5306).
 *
 * Every caller shares one import() of terminal-workspace-entry.ts, so the
 * chunk is fetched once however many /terminals navigations race for it. A
 * failed load (for example a chunk removed by a deploy) is not cached: the
 * next call tries again.
 */

export type TerminalWorkspaceModule = typeof import('./terminal-workspace-entry.js');

/**
 * Wraps `importer` so concurrent and later calls share one in-flight or
 * settled load, except that a rejected load is dropped so the next call
 * imports again. Exported for tests; production code uses
 * loadTerminalWorkspace.
 */
export function singleFlight<T>(importer: () => Promise<T>): () => Promise<T> {
  let pending: Promise<T> | null = null;
  return () => {
    if (!pending) {
      const load = importer();
      pending = load;
      load.catch(() => {
        if (pending === load) pending = null;
      });
    }
    return pending;
  };
}

/** Loads the terminal workspace chunk once (see the file comment). */
export const loadTerminalWorkspace: () => Promise<TerminalWorkspaceModule> = singleFlight(
  () => import('./terminal-workspace-entry.js')
);
