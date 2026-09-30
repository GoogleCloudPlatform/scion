// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/**
 * Experiments tab E2E spec (ptone/scion#2217), under the main Playwright
 * config, against the real hub started by global-setup.
 *
 * Flow: toggle `web.terminal_workspace` off through the Experiments tab
 * (proving the Go<->TS contract end to end), confirm the legacy terminal
 * route stays put, re-enable through the API, and confirm the rewrite to
 * the persistent workspace route happens.
 *
 * State hygiene: all main-suite specs share one hub with one worker. This
 * spec (1) asserts up front that the flag has no stored override, failing
 * fast otherwise, so a leftover from a previous failed run is never
 * silently built on; and (2) always sends a `null` PUT in `finally`, so a
 * failure here cannot leak `false` into later specs.
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv } from './harness/env.js';

const EXPERIMENT_NAME = 'web.terminal_workspace';

interface AdminExperimentEntry {
  name: string;
  override: boolean | null;
  enabled: boolean;
}

interface AdminExperimentsResponse {
  revision: number;
  experiments: AdminExperimentEntry[];
}

async function getAdminExperiments(baseURL: string, devToken: string): Promise<AdminExperimentsResponse> {
  const res = await fetch(`${baseURL}/api/v1/admin/experiments`, {
    headers: { Authorization: `Bearer ${devToken}` },
  });
  if (!res.ok) {
    throw new Error(`GET /api/v1/admin/experiments failed (${res.status}): ${await res.text()}`);
  }
  return (await res.json()) as AdminExperimentsResponse;
}

async function putOverride(
  baseURL: string,
  devToken: string,
  value: boolean | null,
  expectedRevision: number
): Promise<AdminExperimentsResponse> {
  const res = await fetch(`${baseURL}/api/v1/admin/experiments`, {
    method: 'PUT',
    headers: {
      Authorization: `Bearer ${devToken}`,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      overrides: { [EXPERIMENT_NAME]: value },
      expected_revision: expectedRevision,
    }),
  });
  if (!res.ok) {
    throw new Error(`PUT /api/v1/admin/experiments failed (${res.status}): ${await res.text()}`);
  }
  return (await res.json()) as AdminExperimentsResponse;
}

test.describe('Experiments tab', () => {
  const env = getE2EEnv();
  test.use({ storageState: env.adminStorageState, baseURL: env.baseURL });

  test('toggles web.terminal_workspace through the tab; legacy route stays until re-enabled via the API', async ({
    page,
  }) => {
    // Precondition: fail fast if a previous run left an override behind.
    const before = await getAdminExperiments(env.baseURL, env.devToken);
    const entry = before.experiments.find((e) => e.name === EXPERIMENT_NAME);
    expect(entry, `${EXPERIMENT_NAME} must be a registered experiment`).toBeTruthy();
    expect(
      entry?.override,
      `${EXPERIMENT_NAME} must have no stored override before this spec runs`
    ).toBeNull();

    let currentRevision = before.revision;

    try {
      await page.goto('/admin/server-config', { waitUntil: 'domcontentloaded' });

      await page.getByRole('tab', { name: 'Experiments' }).click();

      // Only one experiment is registered as of this phase
      // (web.terminal_workspace, ptone/scion#2217 §3.2), so the tab has a
      // single row and a single switch.
      const toggleSwitch = page.locator('scion-admin-experiments sl-switch').first();

      const putResponse = page.waitForResponse(
        (res) => res.url().includes('/api/v1/admin/experiments') && res.request().method() === 'PUT'
      );
      await toggleSwitch.click();
      const res = await putResponse;
      expect(res.status()).toBe(200);
      const body = (await res.json()) as AdminExperimentsResponse;
      currentRevision = body.revision;

      await expect(page.getByText(/Last changed by/)).toBeVisible({ timeout: 15_000 });

      // The flag is now off hub-wide: a fresh page load keeps the legacy
      // terminal route instead of rewriting it to /terminals/{id}. The
      // rewrite happens before any agent lookup, so no agent needs to exist.
      const agentId = crypto.randomUUID();
      await page.goto(`/agents/${agentId}/terminal`, { waitUntil: 'domcontentloaded' });
      await expect(page).toHaveURL(new RegExp(`/agents/${agentId}/terminal$`));

      // Re-enable through the API (not the tab) and confirm the rewrite.
      const reEnabled = await putOverride(env.baseURL, env.devToken, null, currentRevision);
      currentRevision = reEnabled.revision;

      await page.goto(`/agents/${agentId}/terminal`, { waitUntil: 'domcontentloaded' });
      await expect(page).toHaveURL(new RegExp(`/terminals/${agentId}$`));
    } finally {
      // Cleanup: always reset to "no override" so later main-suite specs
      // never see this flag disabled, even if an assertion above failed.
      const latest = await getAdminExperiments(env.baseURL, env.devToken);
      const latestEntry = latest.experiments.find((e) => e.name === EXPERIMENT_NAME);
      if (latestEntry?.override !== null) {
        await putOverride(env.baseURL, env.devToken, null, latest.revision);
      }
    }
  });
});
