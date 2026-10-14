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
 * Flow: toggle `web.agent_edit` (default off) on through the Experiments
 * tab (proving the Go<->TS contract end to end), confirm a fresh page load
 * receives it on, reset it through the API, and confirm the next load
 * receives the registry default (off) again.
 *
 * State hygiene: all main-suite specs share one hub with one worker. This
 * spec (1) asserts up front that the flag has no stored override, failing
 * fast otherwise, so a leftover from a previous failed run is never
 * silently built on; and (2) always sends a `null` PUT in `finally`, so a
 * failure here cannot leak an override into later specs.
 */

import { test, expect } from '@playwright/test';
import { getE2EEnv } from './harness/env.js';

const EXPERIMENT_NAME = 'web.agent_edit';
const EXPERIMENT_TITLE = 'Edit agent';

interface AdminExperimentEntry {
  name: string;
  override: boolean | null;
  enabled: boolean;
}

interface AdminExperimentsResponse {
  revision: number;
  experiments: AdminExperimentEntry[];
}

async function getAdminExperiments(
  baseURL: string,
  devToken: string
): Promise<AdminExperimentsResponse> {
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

  test('toggles web.agent_edit on through the tab; a reset through the API restores the default', async ({
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

    // eslint-disable-next-line no-useless-assignment -- the initial value is never read (finally uses latest.revision); kept so this lint-only change leaves the emitted code unchanged
    let currentRevision = before.revision;

    try {
      await page.goto('/admin/server-config', { waitUntil: 'domcontentloaded' });

      await page.getByRole('tab', { name: 'Experiments' }).click();

      // Assert the real, browser-computed accessible name: sl-switch's
      // shadow template wraps its default slot and the input in one
      // <label>, so the name must come from slotted content — an
      // aria-label attribute on the host is not forwarded into the shadow
      // root and is not exposed as the control's accessible name. This
      // queries the accessibility tree directly (role + name), which is
      // where that forwarding gap actually showed up; checking
      // `toHaveAccessibleName` on the host element locator does not, since
      // the host and its shadow <input> are different accessibility nodes.
      await expect(
        page.getByRole('switch', { name: new RegExp(`Enable ${EXPERIMENT_TITLE}`) })
      ).toHaveCount(1);

      // The tab lists every registered experiment, so pick this row's switch
      // by its slotted label. Click the host custom element, not the raw
      // shadow <input> (which Shoelace's visual control span intercepts
      // pointer events for).
      const toggleSwitch = page
        .locator('scion-admin-experiments sl-switch')
        .filter({ hasText: `Enable ${EXPERIMENT_TITLE}` });

      const putResponse = page.waitForResponse(
        (res) => res.url().includes('/api/v1/admin/experiments') && res.request().method() === 'PUT'
      );
      await toggleSwitch.click();
      const res = await putResponse;
      expect(res.status()).toBe(200);
      const body = (await res.json()) as AdminExperimentsResponse;
      currentRevision = body.revision;
      expect(body.experiments.find((e) => e.name === EXPERIMENT_NAME)?.enabled).toBe(true);

      await expect(page.getByText(/Last changed by/)).toBeVisible({ timeout: 15_000 });

      // The flag is now on hub-wide: a fresh page load receives it from
      // GET /api/v1/experiments into the feature-flag bag.
      await page.goto('/', { waitUntil: 'domcontentloaded' });
      await expect
        .poll(() => page.evaluate((name) => window.__SCION_FEATURES__?.[name], EXPERIMENT_NAME))
        .toBe(true);

      // Reset through the API (not the tab): the next load gets the default.
      const reset = await putOverride(env.baseURL, env.devToken, null, currentRevision);
      currentRevision = reset.revision;

      await page.goto('/', { waitUntil: 'domcontentloaded' });
      await expect
        .poll(() => page.evaluate((name) => window.__SCION_FEATURES__?.[name], EXPERIMENT_NAME))
        .toBe(false);
    } finally {
      // Cleanup: always reset to "no override" so later main-suite specs
      // never see this flag enabled, even if an assertion above failed.
      const latest = await getAdminExperiments(env.baseURL, env.devToken);
      const latestEntry = latest.experiments.find((e) => e.name === EXPERIMENT_NAME);
      if (latestEntry?.override !== null) {
        await putOverride(env.baseURL, env.devToken, null, latest.revision);
      }
    }
  });
});
