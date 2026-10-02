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
 * Apply server-published settings to the client feature-flag layer.
 *
 * Two independent sources are fetched in parallel, with `Promise.allSettled`
 * so that either one failing does not block the other:
 *
 * - `/api/v1/settings/public`: anonymous. The hub owns the native chat
 *   toggle (server.native_chat.enabled); when it is off the chat API
 *   endpoints are not even registered, so the UI must not offer chat.
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
 * renderRoute() honest and settles `terminalWorkspaceEnabled` before it is
 * read.
 */
export async function applyServerFeatureFlags(): Promise<void> {
  const [pub, exp] = await Promise.allSettled([
    fetch('/api/v1/settings/public', { credentials: 'include' }).then(okJsonOrNull),
    fetch('/api/v1/experiments', { credentials: 'include' }).then(okJsonOrNull),
  ]);

  if (pub.status === 'fulfilled' && pub.value) {
    const settings = pub.value as { nativeChatEnabled?: boolean };
    if (settings.nativeChatEnabled === false) {
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
