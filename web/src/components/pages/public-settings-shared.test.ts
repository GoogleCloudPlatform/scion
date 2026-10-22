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
 * ptone/scion#5311: GET /api/v1/settings/public is read once per page load.
 * The boot fetch (applyServerFeatureFlags) fills the cache that the create
 * form and the project settings fallback then read; an admin save drops it.
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';

import { requestUrl } from '../../client/__fixtures__/request-url.js';
import {
  applyServerFeatureFlags,
  getPublicSettings,
  invalidatePublicSettings,
} from '../../client/server-feature-flags.js';

function json(body: unknown, status = 200): Promise<Response> {
  return Promise.resolve(
    new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  );
}

let settingsFetches = 0;

function stubFetch(): void {
  settingsFetches = 0;
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = requestUrl(input);
      if (url.includes('/api/v1/settings/public')) {
        settingsFetches += 1;
        return json({ telemetryEnabled: true, autoExposePortsEnabled: true });
      }
      // Project settings: an older hub without the resolved endpoint, which
      // is the path that reads the public settings.
      if (url.includes('/settings/resolved')) return json({}, 404);
      if (url.includes('/api/v1/projects?')) return json({ projects: [{ id: 'p1', name: 'P1' }] });
      if (url.match(/\/api\/v1\/projects\/proj-1(\?|$)/)) {
        return json({ id: 'proj-1', name: 'Proj', slug: 'proj' });
      }
      return json({});
    })
  );
}

/** Waits until `ready()` holds, giving the page's async loads time to run. */
async function until(ready: () => boolean): Promise<void> {
  await vi.waitFor(() => expect(ready()).toBe(true), { timeout: 3000 });
}

beforeAll(async () => {
  stubFetch();
  await Promise.all([import('./agent-create.js'), import('./project-settings.js')]);
}, 30_000);

afterEach(() => {
  document.body.innerHTML = '';
  invalidatePublicSettings();
  vi.unstubAllGlobals();
  window.history.replaceState({}, '', '/');
});

describe('shared public settings (ptone/scion#5311)', () => {
  it('public settings fetched once across dialogs', async () => {
    stubFetch();
    await applyServerFeatureFlags();
    expect(settingsFetches).toBe(1);

    const create = document.createElement('scion-page-agent-create') as HTMLElement & {
      hubSettings: { telemetryEnabled?: boolean };
    };
    document.body.appendChild(create);
    await until(() => create.hubSettings.telemetryEnabled === true);

    const settings = document.createElement('scion-page-project-settings') as HTMLElement & {
      projectId: string;
      hubTelemetryDefault: boolean | null;
    };
    settings.projectId = 'proj-1';
    document.body.appendChild(settings);
    await until(() => settings.hubTelemetryDefault === true);

    expect(settingsFetches).toBe(1);
  });

  it('an admin save (invalidatePublicSettings) makes the next read fetch again', async () => {
    stubFetch();
    await getPublicSettings();
    await getPublicSettings();
    expect(settingsFetches).toBe(1);

    invalidatePublicSettings();
    await getPublicSettings();
    expect(settingsFetches).toBe(2);
  });
});
