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
 * API mocks for the project-files-tabs E2E fixture.
 *
 * Mounts the real `scion-page-project-detail` component in a real browser
 * (see fixture.ts) with the hub API replaced by page.route() handlers, so
 * the test exercises real Shoelace `<sl-tab-group>` semantics (click
 * activation, the `active` attribute, its internal fallback to tabs[0])
 * against the actual template wiring — which the Vitest/happy-dom component
 * tests in src/components/pages/project-detail-files.test.ts cannot, since
 * Shoelace elements are not upgraded there.
 */

import type { Page, Route } from '@playwright/test';

export const PROJECT_ID = 'e2e-project';
export const SHARED_DIR_A = 'shared-a';
export const SHARED_DIR_B = 'shared-b';

export interface MockCounts {
  workspaceListings: number;
  sharedDirListings: Record<string, number>;
}

function jsonBody(body: unknown): { status: number; contentType: string; body: string } {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) };
}

function fileListResponse(paths: string[]): unknown {
  return {
    files: paths.map((path) => ({
      path,
      size: 12,
      modTime: '2026-01-01T00:00:00Z',
      mode: '-rw-r--r--',
    })),
    totalSize: paths.length * 12,
    totalCount: paths.length,
  };
}

/**
 * Installs route handlers for every API call the project detail page makes,
 * and a stub for the SSE /events endpoint (a real connection is not needed
 * for this test, but leaving it unhandled would 404 and spam reconnect
 * attempts). Returns live counters for the two shared-dir listing endpoints.
 */
export async function installApiMocks(page: Page): Promise<MockCounts> {
  const counts: MockCounts = {
    workspaceListings: 0,
    sharedDirListings: { [SHARED_DIR_A]: 0, [SHARED_DIR_B]: 0 },
  };

  await page.route('**/events*', async (route: Route) => {
    // A minimal, immediately-closed SSE response. The page will keep
    // retrying on a timer; that's harmless background noise for this test.
    await route.fulfill({ status: 200, contentType: 'text/event-stream', body: '' });
  });

  await page.route('**/api/v1/**', async (route: Route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;

    if (path === '/api/v1/projects' && url.searchParams.get('limit') === '1') {
      await route.fulfill(jsonBody({ projects: [] }));
      return;
    }

    if (path === `/api/v1/projects/${PROJECT_ID}/agents`) {
      await route.fulfill(jsonBody({ agents: [], _capabilities: { actions: ['read'] } }));
      return;
    }

    // Workspace file content (editor open): .../workspace/files/<name>?format=json
    const workspaceContentMatch = path.match(
      new RegExp(`^/api/v1/projects/${PROJECT_ID}/workspace/files/(.+)$`)
    );
    if (workspaceContentMatch && url.searchParams.get('format') === 'json') {
      await route.fulfill(
        jsonBody({ content: 'hello from workspace', modTime: '2026-01-01T00:00:00Z' })
      );
      return;
    }

    // Workspace listing: .../workspace/files
    if (path === `/api/v1/projects/${PROJECT_ID}/workspace/files`) {
      counts.workspaceListings++;
      await route.fulfill(jsonBody(fileListResponse(['workspace-file.txt'])));
      return;
    }

    // File content (editor open): .../files/<name>?format=json
    const contentMatch = path.match(
      new RegExp(`^/api/v1/projects/${PROJECT_ID}/shared-dirs/([^/]+)/files/(.+)$`)
    );
    if (contentMatch && url.searchParams.get('format') === 'json') {
      const dir = decodeURIComponent(contentMatch[1]);
      await route.fulfill(
        jsonBody({ content: `hello from ${dir}`, modTime: '2026-01-01T00:00:00Z' })
      );
      return;
    }

    // Shared-dir listing: .../shared-dirs/<dir>/files
    const listingMatch = path.match(
      new RegExp(`^/api/v1/projects/${PROJECT_ID}/shared-dirs/([^/]+)/files$`)
    );
    if (listingMatch) {
      const dir = decodeURIComponent(listingMatch[1]);
      counts.sharedDirListings[dir] = (counts.sharedDirListings[dir] ?? 0) + 1;
      await route.fulfill(jsonBody(fileListResponse([`${dir}-file.txt`])));
      return;
    }

    if (path === `/api/v1/projects/${PROJECT_ID}`) {
      await route.fulfill(
        jsonBody({
          id: PROJECT_ID,
          name: 'E2E Project',
          slug: 'e2e-project',
          sharedDirs: [{ name: SHARED_DIR_A }, { name: SHARED_DIR_B }],
          _capabilities: { actions: ['read', 'update'] },
        })
      );
      return;
    }

    // Metrics/session-summary endpoints degrade gracefully on non-2xx —
    // the page just shows no summary. Not needed for this test.
    if (
      path === `/api/v1/projects/${PROJECT_ID}/metrics-summary` ||
      path === `/api/v1/projects/${PROJECT_ID}/metrics/summary`
    ) {
      await route.fulfill({ status: 404, body: 'not found' });
      return;
    }

    await route.fulfill({ status: 404, body: 'not found' });
  });

  return counts;
}
