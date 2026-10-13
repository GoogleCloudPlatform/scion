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
 * Test-identity gate status for the test-hub banner (ptone/scion#4240).
 *
 * The hub reports, without authentication, whether it runs with test
 * identities enabled (GET /api/v1/test-infra/status). The value is fixed at
 * hub startup, so it is fetched once per page load and shared by every
 * surface that shows the banner (the app, profile and chat shells and the
 * login page).
 *
 * This is the banner's only source. It never reads a browser feature flag,
 * an experiment or local storage: those are user-editable, and the banner
 * must reflect the hub's startup gate, not the browser's state.
 */

export const TEST_INFRA_STATUS_URL = '/api/v1/test-infra/status';

/** Dispatched on window when the status first arrives. */
export const TEST_INFRA_STATUS_EVENT = 'scion:test-infra-status';

export interface TestInfraStatus {
  testIdentities: boolean;
  testHubAdmin: boolean;
  testSuperAdmin: boolean;
}

let current: TestInfraStatus | null = null;
let inflight: Promise<TestInfraStatus | null> | null = null;

/**
 * Reads a status body. Only a literal `true` turns a gate on; anything else
 * (missing, a string, a number) reads as off. A body that is not an object
 * is rejected.
 */
export function parseTestInfraStatus(body: unknown): TestInfraStatus | null {
  if (!body || typeof body !== 'object' || Array.isArray(body)) return null;
  const b = body as Record<string, unknown>;
  return {
    testIdentities: b.testIdentities === true,
    testHubAdmin: b.testHubAdmin === true,
    testSuperAdmin: b.testSuperAdmin === true,
  };
}

/** The status, once it has been fetched; null before then or after a failed fetch. */
export function getTestInfraStatus(): TestInfraStatus | null {
  return current;
}

/**
 * Fetches the status, once per page load. Concurrent callers share one
 * request. A failed request is not cached, so the next call retries.
 */
export function loadTestInfraStatus(): Promise<TestInfraStatus | null> {
  if (current) return Promise.resolve(current);
  if (inflight) return inflight;
  inflight = (async (): Promise<TestInfraStatus | null> => {
    try {
      // Credentials are included: a hub behind an authenticating proxy
      // needs the proxy's cookie on every request. The route itself is
      // public, but the web layer answers 401 (session_expired) for a stale
      // session cookie, and clears that cookie as it does, so one immediate
      // retry gets the status.
      let res = await fetch(TEST_INFRA_STATUS_URL, { credentials: 'include' });
      if (res.status === 401) {
        res = await fetch(TEST_INFRA_STATUS_URL, { credentials: 'include' });
      }
      if (!res.ok) return null;
      const status = parseTestInfraStatus(await res.json());
      if (status) {
        current = status;
        window.dispatchEvent(new CustomEvent(TEST_INFRA_STATUS_EVENT, { detail: status }));
      }
      return status;
    } catch {
      return null;
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

/** Clears the shared state. Tests only. */
export function resetTestInfraStatusForTests(): void {
  current = null;
  inflight = null;
}
