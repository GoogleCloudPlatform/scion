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
 * Boot-time fetch of server-published settings into the client feature-flag
 * layer. Split out of main.ts so it can be unit tested directly, without
 * importing the whole app entry module (every page component) just to reach
 * one function.
 */

import { setFeatureFlag, setServerFlags } from '../utils/feature-flags.js';

/**
 * Parses `res` as JSON, returning `null` for a non-OK status or a body that
 * isn't valid JSON. A network error (a rejected `fetch()`) is not this
 * function's concern — it surfaces as a rejected promise and is handled by
 * the caller's `Promise.allSettled`.
 */
async function okJsonOrNull(res: Response): Promise<unknown> {
  if (!res.ok) return null;
  try {
    return (await res.json()) as unknown;
  } catch {
    return null;
  }
}

/**
 * The fields of `GET /api/v1/settings/public` the client reads
 * (`PublicSettingsResponse` in pkg/hub/handlers_public_settings.go).
 */
export interface PublicSettings {
  telemetryEnabled?: boolean;
  autoExposePortsEnabled?: boolean;
  nativeChatEnabled?: boolean;
  defaultRuntimeBroker?: string;
  defaultHarnessConfig?: string;
  defaultTemplate?: string;
  defaultModel?: string;
  agentSecretsUserScopeOnly?: boolean;
}

/** Successful result, kept until invalidatePublicSettings(). */
let publicSettings: PublicSettings | null = null;

/** In-flight request, shared by concurrent callers. */
let publicSettingsInflight: Promise<Readonly<PublicSettings> | null> | null = null;

/**
 * Bumped by invalidatePublicSettings(), so a request that was already in
 * flight when an admin saved cannot repopulate the cache with the old value.
 */
let publicSettingsGeneration = 0;

/**
 * `GET /api/v1/settings/public`, fetched once per page load and shared by
 * boot (applyServerFeatureFlags) and every dialog or page that reads hub
 * defaults (ptone/scion#5311).
 *
 * Resolves to null for a failed request, a non-OK status or a non-JSON body;
 * failures are not cached, so the next call retries. The response is
 * hub-global (it carries no per-user data, and the hub serves the same body
 * to every caller), so the cache cannot leak one user's settings to another.
 * Signing out or switching hubs is a full page load, which starts with an
 * empty cache. Admin edits in this tab call invalidatePublicSettings(); an
 * edit made elsewhere is picked up on the next page load.
 */
export function getPublicSettings(): Promise<Readonly<PublicSettings> | null> {
  if (publicSettings) return Promise.resolve(publicSettings);
  if (publicSettingsInflight) return publicSettingsInflight;

  const generation = publicSettingsGeneration;
  const request = (async (): Promise<PublicSettings | null> => {
    try {
      const res = await fetch('/api/v1/settings/public', { credentials: 'include' });
      const body = await okJsonOrNull(res);
      // Frozen: every caller shares this one object.
      const settings =
        body && typeof body === 'object' && !Array.isArray(body)
          ? Object.freeze(body as PublicSettings)
          : null;
      if (settings && generation === publicSettingsGeneration) {
        publicSettings = settings;
      }
      return settings;
    } catch {
      return null;
    }
  })();
  publicSettingsInflight = request;
  // Cleared once settled (never synchronously, even if fetch throws), and only
  // while the slot still holds this request: after an invalidation it belongs
  // to a newer one, if any.
  void request.then(() => {
    if (publicSettingsInflight === request) publicSettingsInflight = null;
  });
  return request;
}

/**
 * Drop the cached public settings so the next getPublicSettings() call
 * fetches them again. Called after an admin saves hub settings, and by tests.
 */
export function invalidatePublicSettings(): void {
  publicSettings = null;
  publicSettingsInflight = null;
  publicSettingsGeneration++;
}

/**
 * Apply server-published settings to the client feature-flag layer.
 *
 * Two independent sources are fetched in parallel, with `Promise.allSettled`
 * so that either one failing does not block the other:
 *
 * - `/api/v1/settings/public`: anonymous. The hub owns the native chat
 *   toggle (server.native_chat.enabled); when it is off the chat API
 *   endpoints are not even registered, so the UI must not offer chat.
 *   Read through getPublicSettings(), so this boot request also fills the
 *   cache the create/configure dialogs and the terminal read later.
 * - `/api/v1/experiments`: the hub-wide, admin-controlled experiment map
 *   (ptone/scion#2217), signed-in callers only. A failure, 401, 404 or a
 *   non-JSON 200 (e.g. a dev server with no hub) leaves the compiled
 *   defaults and any localStorage override in place for this page load.
 *   gs:// link rendering is gated by the registered `web.gcs_links`
 *   experiment (ptone/scion#2545), read through this map the same way as
 *   any other registered experiment. If this fetch fails, `web.gcs_links`
 *   resolves from a localStorage override if one is set, else from the
 *   compiled default, which is off (it is deliberately absent from
 *   `DEFAULT_ON_FLAGS`); either way the hub's own experiment check still
 *   refuses the fetch.
 *
 * Resolving both before the first render keeps the /chat route gate in
 * renderRoute() honest.
 */
export async function applyServerFeatureFlags(): Promise<void> {
  const [pub, exp] = await Promise.allSettled([
    getPublicSettings(),
    fetch('/api/v1/experiments', { credentials: 'include' }).then(okJsonOrNull),
  ]);

  if (pub.status === 'fulfilled' && pub.value) {
    if (pub.value.nativeChatEnabled === false) {
      setFeatureFlag('web.native_chat', false);
    }
  }

  if (exp.status === 'fulfilled' && exp.value) {
    const body = exp.value as { experiments?: Record<string, boolean> };
    // setServerFlags() itself rejects a null, non-object, or array argument.
    if (body.experiments && typeof body.experiments === 'object') {
      setServerFlags(body.experiments);
    }
  }
}
