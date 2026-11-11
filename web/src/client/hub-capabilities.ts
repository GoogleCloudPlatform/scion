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
 * Hub-scope project capabilities.
 *
 * "Can this user create a project?" is a hub-level question. The Hub answers
 * it in the top-level `_capabilities` of `GET /api/v1/projects`
 * (`ComputeScopeCapabilities(..., "project")`), which the Projects list page
 * already reads. Every create-project entry point gates on
 * `can(caps, 'create')` from that same source, never on the user's role
 * string, so custom roles and bindings are honoured.
 */

import type { Capabilities } from '../shared/types.js';
import { apiFetch } from './api.js';

/** Successful result, kept for the rest of the page load. */
let cached: Capabilities | undefined;

/** In-flight request, shared by concurrent callers. */
let inflight: Promise<Capabilities | undefined> | null = null;

function isCapabilities(value: unknown): value is Capabilities {
  return (
    typeof value === 'object' &&
    value !== null &&
    Array.isArray((value as { actions?: unknown }).actions)
  );
}

/**
 * Hub-scope project capabilities (ComputeScopeCapabilities). Cached per page
 * load; fail-closed.
 *
 * Returns undefined when the request fails or the response carries no
 * capabilities; `can(undefined, ...)` is false, so callers hide the action.
 * Failures are not cached, so a later call retries.
 */
export async function fetchHubProjectCapabilities(): Promise<Capabilities | undefined> {
  if (cached) return cached;
  if (inflight) return inflight;

  inflight = (async (): Promise<Capabilities | undefined> => {
    try {
      const res = await apiFetch('/api/v1/projects?limit=1', {
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) return undefined;
      const data = (await res.json()) as { _capabilities?: unknown } | unknown[];
      if (Array.isArray(data)) return undefined;
      const caps = data?._capabilities;
      if (!isCapabilities(caps)) return undefined;
      cached = caps;
      return caps;
    } catch {
      return undefined;
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

/**
 * Record hub-scope project capabilities that a page already received from its
 * own `GET /api/v1/projects` call, so a later helper call needs no request.
 */
export function seedHubProjectCapabilities(caps: Capabilities | undefined): void {
  if (isCapabilities(caps)) {
    cached = caps;
  }
}

/** Test hook: forget the cached value. */
export function resetHubProjectCapabilitiesCache(): void {
  cached = undefined;
  inflight = null;
}
