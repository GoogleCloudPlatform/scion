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
 * Checks for feature flags in this order:
 * 1. Server-injected `window.__SCION_FEATURES__` (set by the Go template)
 * 2. localStorage override for development (key: `scion:feature:<name>`)
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
 * These can still be disabled via server injection or localStorage override.
 */
const DEFAULT_ON_FLAGS = new Set(['web.native_chat', 'web.native_chat_v2']);

/**
 * Check whether a feature flag is enabled.
 *
 * @param name - Dot-separated flag name (e.g. "web.native_chat")
 * @returns true if the flag is enabled, false otherwise
 */
export function isFeatureEnabled(name: string): boolean {
  // 1. Check server-injected features
  if (typeof window !== 'undefined' && window.__SCION_FEATURES__) {
    const value = window.__SCION_FEATURES__[name];
    if (typeof value === 'boolean') return value;
  }

  // 2. Check localStorage override (dev convenience)
  if (typeof localStorage !== 'undefined') {
    try {
      const stored = localStorage.getItem(`scion:feature:${name}`);
      if (stored === 'true') return true;
      if (stored === 'false') return false;
    } catch {
      // localStorage not available (e.g. SSR)
    }
  }

  // Default: on for flags in DEFAULT_ON_FLAGS, off otherwise
  return DEFAULT_ON_FLAGS.has(name);
}

/**
 * Override a feature flag from the server-published settings.
 *
 * Writes into the same `window.__SCION_FEATURES__` bag the Go template uses,
 * so the value takes precedence over both the localStorage dev override and
 * the compiled default. Call this at boot, before any routing decision.
 *
 * @param name - Dot-separated flag name (e.g. "web.native_chat")
 * @param enabled - The server-authoritative value
 */
export function setFeatureFlag(name: string, enabled: boolean): void {
  if (typeof window === 'undefined') return;
  window.__SCION_FEATURES__ = { ...window.__SCION_FEATURES__, [name]: enabled };
}

/**
 * Wave-2 native chat feature flag.
 * Default ON (W9) — added to DEFAULT_ON_FLAGS for general availability.
 * Disable via server injection or localStorage: scion:feature:web.native_chat_v2=false
 * to fall back to wave-1 UI for rollback.
 */
export const NATIVE_CHAT_V2_FLAG = 'web.native_chat_v2';

/**
 * Access Boundaries read flag — gates inventory, detail, and explain pages.
 * Default OFF — not yet in general availability.
 * Enable via server injection or localStorage: scion:feature:web.access_boundaries_read=true
 */
export const ACCESS_BOUNDARIES_READ_FLAG = 'web.access_boundaries_read';

/**
 * Access Boundaries authoring flag — gates create, edit, and delete flows.
 * Default OFF — not yet in general availability.
 * Enable via server injection or localStorage: scion:feature:web.access_boundaries_authoring=true
 */
export const ACCESS_BOUNDARIES_AUTHORING_FLAG = 'web.access_boundaries_authoring';
