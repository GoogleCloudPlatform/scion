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
 * Feature flag utilities.
 *
 * Resolution order, highest first:
 * 1. Values already in `window.__SCION_FEATURES__` before the first
 *    `setServerFlags()` call ("pinned") — only `setFeatureFlag('web.native_chat*')`
 *    in production, and E2E init scripts otherwise.
 * 2. The hub-wide value from `GET /api/v1/experiments`, applied at boot
 *    through `setServerFlags()` (signed-in users only).
 * 3. localStorage override for development (key: `scion:feature:<name>`),
 *    which still applies to names the server did not send and on page loads
 *    where the experiments fetch fails.
 * 4. The compiled `DEFAULT_ON_FLAGS` below, used when the fetch fails.
 *
 * Default is `false` (flag off) when not found in any source.
 */

declare global {
  interface Window {
    __SCION_FEATURES__?: Record<string, boolean>;
  }
}

/**
 * Feature flags that are ON by default (Phase 5+).
 * These can still be disabled via the server (the experiments map or
 * `setFeatureFlag()`) or a localStorage override.
 *
 * Entries stay string literals (not the exported constants below), because a
 * Go-side consistency test extracts this set with a regex that only sees
 * literals.
 */
const DEFAULT_ON_FLAGS = new Set(['web.native_chat', 'web.terminal_workspace']);

/**
 * Snapshot of `window.__SCION_FEATURES__` taken the first time
 * {@link setServerFlags} runs. Keys present here are "pinned" and are never
 * overwritten by a later `setServerFlags()` call, so a value written before
 * boot (an E2E init script, or `setFeatureFlag`) always wins over the server.
 * Taken lazily so tests can set the bag before calling `setServerFlags()`.
 */
let pinned: Record<string, boolean> | null = null;

/** Flag names for which the localStorage-shadowed-by-server notice has already been logged this page load. */
const shadowLogged = new Set<string>();

/**
 * Apply the hub-wide experiment map fetched from `GET /api/v1/experiments`
 * to the feature-flag bag. Call this once at boot, after the fetch resolves.
 *
 * Keys already present in `window.__SCION_FEATURES__` before the first call
 * ("pinned") are left untouched, so E2E-pinned and `setFeatureFlag`-written
 * values keep beating the server.
 */
export function setServerFlags(flags: Record<string, boolean>): void {
  if (typeof window === 'undefined') return;
  if (!flags || typeof flags !== 'object' || Array.isArray(flags)) return;
  const current = window.__SCION_FEATURES__ ?? {};
  if (pinned === null) pinned = { ...current };
  const next = { ...current };
  for (const [name, value] of Object.entries(flags)) {
    if (Object.prototype.hasOwnProperty.call(pinned, name)) continue;
    next[name] = value;
  }
  window.__SCION_FEATURES__ = next;
}

/**
 * @internal test-only: clears the pinned snapshot and the shadowed-override
 * log dedup between tests.
 */
export function resetServerFlagStateForTests(): void {
  pinned = null;
  shadowLogged.clear();
}

/**
 * Check whether a feature flag is enabled.
 *
 * @param name - Dot-separated flag name (e.g. "web.native_chat")
 * @returns true if the flag is enabled, false otherwise
 */
export function isFeatureEnabled(name: string): boolean {
  // 1 & 2. Pinned and server-applied values share one bag: setServerFlags()
  // never overwrites a pinned key, so whichever is present here already
  // reflects the right precedence.
  if (typeof window !== 'undefined' && window.__SCION_FEATURES__) {
    const value = window.__SCION_FEATURES__[name];
    if (typeof value === 'boolean') {
      warnIfShadowed(name);
      return value;
    }
  }

  // 3. Check localStorage override (dev convenience)
  if (typeof localStorage !== 'undefined') {
    try {
      const stored = localStorage.getItem(`scion:feature:${name}`);
      if (stored === 'true') return true;
      if (stored === 'false') return false;
    } catch {
      // localStorage not available (e.g. SSR)
    }
  }

  // 4. Default: on for flags in DEFAULT_ON_FLAGS, off otherwise
  return DEFAULT_ON_FLAGS.has(name);
}

/**
 * Logs one `console.info` per flag per page load when a localStorage dev
 * override exists for a name that the bag (pinned or server) already
 * resolves, so a developer relying on a stale devtools override is not left
 * confused about why it no longer applies.
 */
function warnIfShadowed(name: string): void {
  if (shadowLogged.has(name)) return;
  if (typeof localStorage === 'undefined') return;
  try {
    const stored = localStorage.getItem(`scion:feature:${name}`);
    if (stored !== 'true' && stored !== 'false') return;
  } catch {
    return;
  }
  shadowLogged.add(name);
  console.info(
    `[feature-flags] localStorage override for "${name}" is shadowed by the server/pinned value.`
  );
}

/**
 * Override a feature flag from the server-published settings.
 *
 * Writes into the same `window.__SCION_FEATURES__` bag read by
 * {@link isFeatureEnabled}, so the value takes precedence over both the
 * localStorage dev override and the compiled default. Call this at boot,
 * before any routing decision.
 *
 * @param name - Dot-separated flag name (e.g. "web.native_chat")
 * @param enabled - The server-authoritative value
 */
export function setFeatureFlag(name: string, enabled: boolean): void {
  if (typeof window === 'undefined') return;
  window.__SCION_FEATURES__ = { ...window.__SCION_FEATURES__, [name]: enabled };
}

/**
 * Persistent terminal workspace flag (ptone/scion#1662, ptone/scion#2217).
 * Default ON. Controlled hub-wide from Admin → Server Config →
 * Experiments; a per-browser localStorage opt-out only applies when the
 * experiments fetch fails or on a signed-out page load.
 */
export const TERMINAL_WORKSPACE_FLAG = 'web.terminal_workspace';
